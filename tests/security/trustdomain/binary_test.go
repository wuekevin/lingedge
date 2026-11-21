// Package trustdomain 是三个信任域的二进制层检查（Story 1.23 AC5、ADR-012 §2）：对真实编译出的 lingedge-server 做 `go tool nm`，
// 断言符号表里不存在「加载发行清单私钥」的函数/方法/变量（发行签名私钥离线，Server 不持有也不读取）。源码层的同类检查是 `gates trustdomain`。
// 检查器带负向样本：一个故意定义了 loadReleasePrivateKey 的临时程序编译后必须被同一个扫描器抓到（证明扫描不是恒真）。
package trustdomain

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 与 scripts/gates/trustdomain.go 的 reTrustName 同口径（那里是标识符名；这里是符号名，符号名是 包路径.标识符，大小写不敏感匹配同一组词）。
var reSymbol = regexp.MustCompile(`(?i)((release|manifest)[a-z0-9_]*(priv(ate)?_?key|secret_?key|signing_?key|signer|privkey)|(priv(ate)?_?key|secret_?key|signing_?key|privkey)[a-z0-9_]*(release|manifest))`)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func build(t *testing.T, dir, pkg, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, stderr.String())
	}
}

// symbols 返回二进制中名称命中规则的符号（只取 `go tool nm` 的符号名列）。
func symbols(t *testing.T, bin string) (hits []string, total int) {
	t.Helper()
	out, err := exec.Command("go", "tool", "nm", bin).Output()
	if err != nil {
		t.Fatalf("go tool nm: %v", err)
	}
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) < 3 {
			continue
		}
		total++
		name := strings.Join(f[2:], " ")
		if reSymbol.MatchString(name) {
			hits = append(hits, name)
		}
	}
	return hits, total
}

func TestServerBinaryHasNoReleaseKeyLoader(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过编译 lingedge-server")
	}
	bin := filepath.Join(t.TempDir(), "lingedge-server")
	build(t, repoRoot(t), "./cmd/lingedge-server", bin)
	hits, total := symbols(t, bin)
	if total < 5000 {
		t.Fatalf("符号表只有 %d 项，扫描无效", total)
	}
	if len(hits) > 0 {
		t.Fatalf("lingedge-server 的符号表中出现加载发行清单私钥的符号（发行私钥离线，ADR-012 §2）:\n%s", strings.Join(hits, "\n"))
	}
	// 同时断言二进制里确有信任域②③的密钥服务（扫描对象是预期的二进制；Sign 尚无调用者会被链接器裁掉，路由处理函数 list 一定在）。
	out, err := exec.Command("go", "tool", "nm", bin).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("signingkeys.(*Service).list")) {
		t.Error("符号表里应有 signingkeys.(*Service).list，否则扫描的不是预期的二进制")
	}
}

// 负向自测：加入一个加载发行私钥的函数，同一个扫描器必须失败。
func TestScannerDetectsReleaseKeyLoader(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过编译")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module negsample\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package main\n\nimport \"os\"\n\n//go:noinline\nfunc loadReleasePrivateKey(p string) ([]byte, error) { return os.ReadFile(p) }\n\nfunc main() { _, _ = loadReleasePrivateKey(os.Args[0]) }\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "neg")
	build(t, dir, ".", bin)
	hits, _ := symbols(t, bin)
	found := false
	for _, h := range hits {
		if strings.Contains(h, "loadReleasePrivateKey") {
			found = true
		}
	}
	if !found {
		t.Fatalf("扫描器应抓到 loadReleasePrivateKey，实际命中: %v", hits)
	}
}
