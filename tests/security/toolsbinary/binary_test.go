// Package toolsbinary 是 lingedge 二进制层的「生产不含假组件」检查（Story 1.33、G-11）：对真实编译出的 lingedge（不带 lingedge_testdouble 标签）做
// `go tool nm`，断言符号表里没有 tests/toolsworld（测试世界与假组件规格）或任何 testdouble 符号；同时断言 installer.ProductionSpecs() 只含已交付的 server（Story 1.35）。
// 扫描器带负向样本：一个故意 import tests/toolsworld 的临时程序编译后必须被同一个扫描器抓到（证明扫描不是恒真）。
// 源码层的同类检查是 `gates testdouble`（依赖图与 import 检查）。
package toolsbinary

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
)

var reFake = regexp.MustCompile(`(?i)(toolsworld|testdouble|fakeSpec|fake_spec)`)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func build(t *testing.T, dir, pkg, out string, extra ...string) {
	t.Helper()
	args := append([]string{"build"}, extra...)
	args = append(args, "-o", out, pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, stderr.String())
	}
}

func symbolHits(t *testing.T, bin string) (hits []string, total int) {
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
		if name := strings.Join(f[2:], " "); reFake.MatchString(name) {
			hits = append(hits, name)
		}
	}
	return hits, total
}

// 生产规格只登记已交付的组件（Story 1.35：server；Story 1.40：静态前端 admin-web/console/docs/status/website）；多出的名字意味着有人把未交付/测试用的规格混进了生产路径。
func TestProductionSpecsAreOnlyDeliveredComponents(t *testing.T) {
	got := strings.Join(installer.ProductionSpecs().Names(), ",")
	if got != "admin-web,console,docs,server,status,website" {
		t.Fatalf("生产构建只应登记已交付组件（server 与静态前端；其余由各自 Story 交付后在此追加）的规格，实际：%q", got)
	}
	for name, spec := range installer.ProductionSpecs() {
		if reFake.MatchString(fmt.Sprintf("%T", spec)) {
			t.Errorf("生产规格 %s 的类型 %T 看起来是测试替身", name, spec)
		}
	}
}

func TestLingedgeBinaryContainsNoFakeComponents(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过编译 lingedge")
	}
	bin := filepath.Join(t.TempDir(), "lingedge")
	build(t, repoRoot(t), "./cmd/lingedge", bin) // 不传 -tags：与 make build 同样的生产构建
	hits, total := symbolHits(t, bin)
	if total < 3000 {
		t.Fatalf("符号表只有 %d 项，扫描无效", total)
	}
	if len(hits) > 0 {
		t.Fatalf("lingedge 的符号表含测试世界/假组件符号（G-11）：\n%s", strings.Join(hits, "\n"))
	}
	// 同时断言扫描的确是装配了安装编排的二进制（否则「没有假组件」是空话）。
	out, err := exec.Command("go", "tool", "nm", bin).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"installer.(*Installer).Install", "installer.(*Installer).Recover", "cli.runInstall"} {
		if !bytes.Contains(out, []byte(must)) {
			t.Errorf("符号表里应有 %s", must)
		}
	}
	// 字符串层：假组件在测试世界里的独有标记不得出现在二进制字节里。
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"LingEdge unit test key v1", "fail-health-", "block-start-"} {
		if bytes.Contains(raw, []byte(marker)) {
			t.Errorf("二进制含测试世界字符串 %q", marker)
		}
	}
}

// 负向自测：一个 import tests/toolsworld 的程序，同一个扫描器必须抓到。
func TestScannerDetectsToolsWorld(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过编译")
	}
	root := repoRoot(t)
	dir := filepath.Join(root, "tests", "security", "toolsbinary", "negsample")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	src := "package main\n\nimport (\n\t\"os\"\n\n\t\"github.com/wuekevin/lingedge/tests/toolsworld\"\n)\n\nfunc main() { w, _ := toolsworld.NewWorld(os.Args[0]); _ = w }\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "neg")
	build(t, root, "./tests/security/toolsbinary/negsample", bin)
	hits, _ := symbolHits(t, bin)
	found := false
	for _, h := range hits {
		found = found || strings.Contains(strings.ToLower(h), "toolsworld")
	}
	if !found {
		t.Fatalf("扫描器应抓到 toolsworld 符号，实际命中: %v", hits)
	}
}
