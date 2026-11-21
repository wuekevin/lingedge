// Package release 是 Story 1.34 发行流水线的端到端测试（真实 go build、真实离线签名工具、真实 Tools 校验链）。
//
// 测试密钥一律在运行时随机生成，写入 t.TempDir()，不入库（G-6/G-7 不设豁免）；
// 被测对象全是真实实现：scripts/release（在线流水线）、scripts/release-sign（离线签名，独立 module，作为子进程运行）、
// internal/tools/manifest 与 internal/tools/downloader（Tools 的验签与取包链）。
package release_test

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	repoOnce sync.Once
	repoDir  string

	binOnce             sync.Once
	releaseBin, signBin string
	binErr              error
)

func repoRoot(t testing.TB) string {
	t.Helper()
	repoOnce.Do(func() {
		wd, _ := os.Getwd()
		for d := wd; ; d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
				repoDir = d
				return
			}
			if d == filepath.Dir(d) {
				return
			}
		}
	})
	if repoDir == "" {
		t.Fatal("找不到仓库根")
	}
	return repoDir
}

// tools 构建一次在线流水线与离线签名工具的二进制（离线签名工具是独立 module，需在其目录下构建）。
func tools(t testing.TB) (release, sign string) {
	t.Helper()
	root := repoRoot(t)
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lingedge-release-tools-")
		if err != nil {
			binErr = err
			return
		}
		releaseBin = filepath.Join(dir, "release")
		signBin = filepath.Join(dir, "release-sign")
		if out, err := runIn(root, nil, "go", "build", "-o", releaseBin, "./scripts/release"); err != nil {
			binErr = errors.New(string(out) + err.Error())
			return
		}
		if out, err := runIn(filepath.Join(root, "scripts", "release-sign"), nil, "go", "build", "-o", signBin, "."); err != nil {
			binErr = errors.New(string(out) + err.Error())
		}
	})
	if binErr != nil {
		t.Fatalf("构建流水线二进制失败: %v", binErr)
	}
	return releaseBin, signBin
}

func runIn(dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

func mustRun(t testing.TB, dir string, name string, args ...string) string {
	t.Helper()
	out, err := runIn(dir, nil, name, args...)
	if err != nil {
		t.Fatalf("%s %s 失败: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// srcCopy 把当前工作树（已跟踪 + 未忽略的新文件）复制到临时目录并提交，得到一个干净的 git 仓库（发行构建要求可追溯到提交）。
// keysJSON 非空则注入为内置发行公钥 keys.json（gitignored，不进提交）。
func srcCopy(t testing.TB, keysJSON []byte) string { return srcCopyIn(t, t.TempDir(), keysJSON) }

// srcCopyIn 同 srcCopy，但复制到调用方指定的目录（共享演练需要比单个测试更长的生命周期）。
func srcCopyIn(t testing.TB, dst string, keysJSON []byte) string {
	t.Helper()
	root := repoRoot(t)
	out, err := runIn(root, nil, "git", "ls-files", "-z", "-c", "-o", "--exclude-standard")
	if err != nil {
		t.Fatalf("git ls-files: %v\n%s", err, out)
	}
	for _, n := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		src := filepath.Join(root, n)
		fi, err := os.Lstat(src)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		copyFile(t, src, filepath.Join(dst, n), fi.Mode().Perm())
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		mustRun(t, dst, "git", args...)
	}
	mustRun(t, dst, "git", "-c", "user.name=release-test", "-c", "user.email=release-test@example.invalid", "commit", "-q", "-m", "release e2e source")
	if keysJSON != nil {
		if err := os.WriteFile(filepath.Join(dst, "internal/tools/manifest/builtin/keys.json"), keysJSON, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func copyFile(t testing.TB, src, dst string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		t.Fatal(err)
	}
}

func copyDir(t testing.TB, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o755)
		case d.Type().IsRegular():
			fi, _ := d.Info()
			copyFile(t, p, to, fi.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// keypair 用离线签名工具生成运行时随机密钥：返回私钥文件、公钥集 keys.json 路径与内容。
func keypair(t testing.TB, id string) (keyPath, keysPath string, keys []byte) {
	return keypairIn(t, id, t.TempDir())
}

func keypairIn(t testing.TB, id, d string) (keyPath, keysPath string, keys []byte) {
	t.Helper()
	_, sign := tools(t)
	keyPath, keysPath = filepath.Join(d, id+".key.json"), filepath.Join(d, "keys.json")
	mustRun(t, d, sign, "keygen", "-key-id", id, "-key-out", keyPath, "-pub-out", keysPath)
	b, err := os.ReadFile(keysPath)
	if err != nil {
		t.Fatal(err)
	}
	return keyPath, keysPath, b
}

func b64d(t testing.TB, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pubOf 取 keys.json 中第一把公钥。
func pubOf(t testing.TB, keys []byte) (id string, pub ed25519.PublicKey) {
	t.Helper()
	var d struct {
		Keys []struct {
			KeyID     string `json:"key_id"`
			PublicKey string `json:"public_key"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(keys, &d); err != nil || len(d.Keys) == 0 {
		t.Fatal("keys.json 非法")
	}
	return d.Keys[0].KeyID, b64d(t, d.Keys[0].PublicKey)
}

// rewriteBundle 读取离线 bundle，经 mod 改写某个条目后写出新 bundle（mod 返回 nil 表示删除该条目）。
func rewriteBundle(t testing.TB, src, dst string, mod func(name string, body []byte) []byte) {
	t.Helper()
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		nb := mod(h.Name, body)
		if nb == nil {
			continue
		}
		nh := *h
		nh.Size = int64(len(nb))
		if err := tw.WriteHeader(&nh); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(nb)
	}
	_ = tw.Close()
	if err := os.WriteFile(dst, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
