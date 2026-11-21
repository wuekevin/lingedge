package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// cmdVerify 是发行前的最后一道关：用 Tools 的真实校验链验证「将要发布的字节」，通过才生成 publish/。
//
// 验证对象不是流水线自己的内存状态，而是磁盘上的 manifest.json + manifest.json.sig + packages/*.tar.gz，
// 校验链是 Tools 安装路径的同一份代码：manifest.Verifier.Verify（验签/schema/channel/sequence/valid_until）→
// downloader.Acquire 的离线 bundle 路径（外层 sha256+size → 包内 SHA256SUMS 签名 → SafeExtract 逐文件校验 → META 与清单一致）。
// 任何一步失败整个命令失败，不产出 publish/。
func cmdVerify(args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("verify", flag.ContinueOnError)
	fl.SetOutput(stderr)
	out := fl.String("out", "", "build 的输出目录（必填）")
	keys := fl.String("keys", "", "发行公钥集 keys.json（必填）")
	nowFlag := fl.String("now", "", "验证时刻（RFC 3339；缺省当前时间）")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *out == "" || *keys == "" {
		return usageError{"verify 需要 -out 与 -keys"}
	}
	now := time.Now().UTC()
	if *nowFlag != "" {
		t, err := parseTime("-now", *nowFlag)
		if err != nil {
			return err
		}
		now = t
	}
	o, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	plan, err := loadPlan(o)
	if err != nil {
		return err
	}
	kb, err := os.ReadFile(*keys)
	if err != nil {
		return err
	}
	mb, err := os.ReadFile(filepath.Join(o, "manifest.json"))
	if err != nil {
		return err
	}
	sb, err := os.ReadFile(filepath.Join(o, "manifest.json.sig"))
	if err != nil {
		return fmt.Errorf("缺少 manifest.json.sig（第 2 轮离线签名未完成？）: %w", err)
	}
	m, err := manifest.ParseManifest(mb)
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "lingedge-release-verify-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	bundle := filepath.Join(tmp, "bundle.tar")
	if err := writeBundle(bundle, o, mb, sb, plan); err != nil {
		return err
	}
	newVerifier := func(dir string) (*manifest.Verifier, error) {
		trust, err := manifest.LoadTrust(kb, "")
		if err != nil {
			return nil, err
		}
		return &manifest.Verifier{Trust: trust, Seen: manifest.FileSequenceStore{Path: filepath.Join(dir, "trust", "state")}, Now: func() time.Time { return now }}, nil
	}
	nPkg := 0
	for i, p := range plan.Packages {
		dir := filepath.Join(tmp, fmt.Sprintf("tx%d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		v, err := newVerifier(dir)
		if err != nil {
			return err
		}
		acq, err := downloader.Acquire(context.Background(), downloader.Params{
			Verifier: v, KeysDir: filepath.Join(dir, "trust", "keys.d"), BundlePath: bundle, Channel: m.Channel,
			Component: p.Component, Version: p.Version, Variant: p.Variant, Platform: p.Platform,
			StagingRoot: filepath.Join(dir, "staging"), DeferCommit: true})
		if err != nil {
			return fmt.Errorf("包 %s 未通过 Tools 的校验链: %w", p.ID, err)
		}
		if acq.Meta.FilesCount != p.FilesCount || acq.Meta.UnpackedSize != p.UnpackedSize {
			_ = acq.Discard()
			return fmt.Errorf("包 %s 的 META 与计划不符", p.ID)
		}
		if err := acq.Discard(); err != nil {
			return err
		}
		nPkg++
	}
	// 清单里的每个 artifact 都必须对应一个已验证的包；清单也不得夸大。
	want := 0
	for _, c := range m.Components {
		for _, ve := range c.Versions {
			for _, vr := range ve.Variants {
				want += len(vr.Artifacts)
			}
		}
	}
	if want != nPkg {
		return fmt.Errorf("清单声明 %d 个 artifact，但实际验证了 %d 个包", want, nPkg)
	}

	// 通过：产出 publish/。
	pub := filepath.Join(o, "publish")
	if err := os.RemoveAll(pub); err != nil {
		return err
	}
	rec := map[string]any{"release": plan.Release, "commit": plan.Commit, "channel": m.Channel, "sequence": m.Sequence,
		"generated_at": m.GeneratedAt.Format(time.RFC3339), "valid_until": m.ValidUntil.Format(time.RFC3339), "verified_at": now.Format(time.RFC3339)}
	files := map[string]string{}
	put := func(rel string, b []byte) error {
		files[rel] = sha256Hex(b)
		return writeFileMkdir(filepath.Join(pub, filepath.FromSlash(rel)), b)
	}
	if err := put(m.Channel+"/manifest.json", mb); err != nil {
		return err
	}
	if err := put(m.Channel+"/manifest.json.sig", sb); err != nil {
		return err
	}
	for _, p := range plan.Packages {
		b, err := os.ReadFile(filepath.Join(o, "packages", p.Component, p.Version, p.Filename))
		if err != nil {
			return err
		}
		if err := put(p.Component+"/"+p.Version+"/"+p.Filename, b); err != nil {
			return err
		}
	}
	for _, name := range []string{"SBOM.cdx.json", "build-record.json", "repro.json"} {
		b, err := os.ReadFile(filepath.Join(o, name))
		if errors.Is(err, fs.ErrNotExist) && name == "repro.json" {
			continue // -repro=false 的演练构建没有可复现性记录；发行记录里如实不含
		}
		if err != nil {
			return err
		}
		if err := put(name, b); err != nil {
			return err
		}
	}
	bb, err := os.ReadFile(bundle)
	if err != nil {
		return err
	}
	if err := put("lingedge-offline-"+plan.Release+".tar", bb); err != nil {
		return err
	}
	rec["files"] = files
	if _, err := writeJSON(filepath.Join(pub, "RELEASE-RECORD.json"), rec); err != nil {
		return err
	}
	if err := scanTreeForPrivateKeys(pub); err != nil {
		_ = os.RemoveAll(pub)
		return err
	}
	_, _ = fmt.Fprintf(stdout, "发行验证通过：%d 个包经 Tools 同一条校验链验证；发行目录 %s 已生成。\n  把 publish/ 下内容发布到 release.base_url（清单在 <base_url>/%s/manifest.json）。\n", nPkg, pub, m.Channel)
	return nil
}

func writeFileMkdir(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// writeBundle 按 tools-design §9.4 写离线 bundle：offline-bundle/{manifest.json,manifest.json.sig,packages/<文件名>}。
// 文件名 = 清单 URL 末段 = 包文件名。
func writeBundle(path, out string, mb, sb []byte, plan *releasePlan) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name string, body []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatPAX, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	if err := add("offline-bundle/manifest.json", mb); err != nil {
		return err
	}
	if err := add("offline-bundle/manifest.json.sig", sb); err != nil {
		return err
	}
	pk := append([]planPackage(nil), plan.Packages...)
	sort.Slice(pk, func(i, j int) bool { return pk[i].Filename < pk[j].Filename })
	for _, p := range pk {
		b, err := os.ReadFile(filepath.Join(out, "packages", p.Component, p.Version, p.Filename))
		if err != nil {
			return err
		}
		if err := add("offline-bundle/packages/"+p.Filename, b); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// 发行目录里不得出现私钥材料（ADR-012：CI 中不存在发行私钥）：PEM 私钥块、离线签名工具的私钥文件格式。
var privateKeyShapes = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?s)lingedge-release-key-v1.{0,200}"seed"\s*:\s*"[A-Za-z0-9+/]{43}=?"`),
}

func scanTreeForPrivateKeys(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("发行目录含符号链接 %s", p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tar") {
			return nil // 归档内容已由 SHA256SUMS 签名链覆盖；私钥不会以明文出现在已验证的包里（包 payload 由流水线逐文件产出）
		}
		for _, re := range privateKeyShapes {
			if re.Match(b) {
				return fmt.Errorf("发行目录文件 %s 含私钥材料，拒绝发布", p)
			}
		}
		return nil
	})
}
