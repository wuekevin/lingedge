// Package scaffold 校验 Story 1.1 的工程骨架约定（目录、go.mod、Makefile、cmd 入口、Docker 禁令）。
package scaffold

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

var cmds = []string{
	"lingedge", "lingedge-server", "lingedge-node", "lingedge-dns",
	"lingedge-scheduler", "lingedge-reporter", "lingedge-worker",
}

// 基线 §20 目录清单。
var requiredDirs = func() []string {
	dirs := []string{
		"internal/tools", "pkg", "proto",
		"frontend/website", "frontend/admin", "frontend/console", "frontend/shared",
		"docs-site", "status-site",
		"edge/templates", "edge/lua", "edge/tests",
		"migrations", "manifests", "configs",
		"deploy/baota", "deploy/systemd", "deploy/nginx", "deploy/openresty", "deploy/install",
		"scripts",
		"tests/integration", "tests/e2e", "tests/performance", "tests/security",
		"docs/prd", "docs/architecture", "docs/database", "docs/api", "docs/operations", "docs/security", "docs/bmad",
	}
	for _, c := range cmds {
		dirs = append(dirs, "cmd/"+c)
	}
	for _, t := range strings.Fields("cli tui installer manifest resolver downloader registry updater rollback backup doctor service baota security") {
		dirs = append(dirs, "internal/tools/"+t)
	}
	for _, m := range strings.Fields("platform auth tenant website origin cache ruleengine node cluster scheduler dns certificate waf security l4proxy monitoring reporter billing notification cms seo media plugin") {
		dirs = append(dirs, "internal/"+m)
	}
	return dirs
}()

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到 go.mod")
		}
		dir = parent
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// walkSource 遍历受检的源码树，跳过文档、工具缓存、构建产物与本测试包。
func walkSource(t *testing.T, root string, fn func(rel string, d fs.DirEntry)) {
	t.Helper()
	skipRel := map[string]bool{"_bmad": true, "_bmad-output": true, "docs": true, "bin": true, "tests/scaffold": true}
	skipName := map[string]bool{".git": true, "dist": true, "node_modules": true, ".pnpm-store": true, ".nuxt": true, ".output": true}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() && (skipRel[rel] || skipName[d.Name()]) {
			return fs.SkipDir
		}
		fn(rel, d)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRequiredDirsExist(t *testing.T) {
	root := repoRoot(t)
	for _, d := range requiredDirs {
		fi, err := os.Stat(filepath.Join(root, d))
		if err != nil || !fi.IsDir() {
			t.Errorf("基线 §20 目录缺失: %s", d)
			continue
		}
		entries, _ := os.ReadDir(filepath.Join(root, d))
		if len(entries) == 0 {
			t.Errorf("目录为空（应至少含 doc.go/.gitkeep）: %s", d)
		}
	}
}

func TestGoMod(t *testing.T) {
	root := repoRoot(t)
	mod := readFile(t, filepath.Join(root, "go.mod"))
	if !regexp.MustCompile(`(?m)^module github\.com/wuekevin/lingedge$`).MatchString(mod) {
		t.Error("模块路径应为 github.com/wuekevin/lingedge（UD-1）")
	}
	if !regexp.MustCompile(`(?m)^go 1\.27\.0$`).MatchString(mod) {
		t.Error("go 行应固定为 go 1.27.0（语言基线）")
	}
	if !regexp.MustCompile(`(?m)^toolchain go1\.27\.4$`).MatchString(mod) {
		t.Error("toolchain 行应固定为 go1.27.4")
	}
	for _, line := range strings.Split(mod, "\n") {
		if strings.Contains(line, "=>") && regexp.MustCompile(`=>\s*["]?(\.|/|~)`).MatchString(line) {
			t.Errorf("go.mod 含指向本地路径的 replace: %q", line)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "go.sum")); err != nil {
		t.Errorf("go.sum 必须入库: %v", err)
	}
	ignore := readFile(t, filepath.Join(root, ".gitignore"))
	if regexp.MustCompile(`(?m)^/?go\.(sum|mod)\s*$`).MatchString(ignore) {
		t.Error(".gitignore 不得忽略 go.mod/go.sum")
	}
}

func TestCmdEntriesAreVersionOnly(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		if e.IsDir() {
			got = append(got, e.Name())
		}
	}
	if strings.Join(got, ",") != strings.Join(sorted(cmds), ",") {
		t.Errorf("cmd/ 子目录 = %v，期望恰为 %v", got, sorted(cmds))
	}
	for _, c := range cmds {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "cmd", c, "main.go"), nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("%s: %v", c, err)
			continue
		}
		var imports []string
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			imports = append(imports, p)
		}
		want := "os,github.com/wuekevin/lingedge/pkg/version"
		if c == "lingedge-server" {
			// Story 1.6：`lingedge-server migrate` 子命令的入口（迁移框架与嵌入的迁移文件）；其余子命令随 1.13 装配。
			want = "os,github.com/wuekevin/lingedge/internal/platform/migrate,github.com/wuekevin/lingedge/migrations,github.com/wuekevin/lingedge/pkg/version"
		}
		if strings.Join(imports, ",") != want {
			t.Errorf("%s: main 的依赖应恰为 %s，实际 %v", c, want, imports)
		}
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestCmdsPrintRealVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过真实编译")
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	const ldflags = "-X github.com/wuekevin/lingedge/pkg/version.Version=9.9.9-test -X github.com/wuekevin/lingedge/pkg/version.Commit=deadbeef"
	for _, c := range cmds {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			bin := filepath.Join(tmp, c)
			build := exec.Command("go", "build", "-buildvcs=false", "-ldflags", ldflags, "-o", bin, "./cmd/"+c)
			build.Dir = root
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("编译失败: %v\n%s", err, out)
			}
			out, err := exec.Command(bin, "--version").Output()
			if err != nil {
				t.Fatalf("--version 失败: %v", err)
			}
			want := c + " 9.9.9-test (commit deadbeef, " + runtime.Version() + ", " + runtime.GOOS + "/" + runtime.GOARCH + ")\n"
			if string(out) != want {
				t.Errorf("输出 %q，期望 %q", out, want)
			}
			var stderr bytes.Buffer
			bad := exec.Command(bin)
			bad.Stderr = &stderr
			var ee *exec.ExitError
			if err := bad.Run(); !errors.As(err, &ee) || ee.ExitCode() != 2 {
				t.Errorf("无参数应以退出码 2 退出（无伪业务行为），实际 %v", err)
			}
			if !strings.Contains(stderr.String(), "尚未交付") {
				t.Errorf("无参数 stderr = %q", stderr.String())
			}
		})
	}
}

func TestNoUpdaterTarget(t *testing.T) {
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "cmd", "lingedge-updater")); err == nil {
		t.Error("不得存在 cmd/lingedge-updater（FR-1）")
	}
	walkSource(t, root, func(rel string, d fs.DirEntry) {
		if strings.Contains(rel, "lingedge-updater") {
			t.Errorf("路径含 lingedge-updater: %s", rel)
			return
		}
		if d.IsDir() {
			return
		}
		if b, err := os.ReadFile(filepath.Join(root, rel)); err == nil && bytes.Contains(b, []byte("lingedge-updater")) {
			t.Errorf("文件含 lingedge-updater: %s", rel)
		}
	})
}

func TestNoDockerArtifacts(t *testing.T) {
	root := repoRoot(t)
	nameRe := regexp.MustCompile(`(?i)(^|/)(dockerfile[^/]*|docker-compose[^/]*|compose\.ya?ml|\.dockerignore|containerfile)$`)
	contentRe := regexp.MustCompile(`(?i)\b(docker|docker-compose|podman|kubectl|kubernetes|k8s)\b`)
	walkSource(t, root, func(rel string, d fs.DirEntry) {
		if nameRe.MatchString(rel) {
			t.Errorf("出现容器化文件（NFR-7）: %s", rel)
		}
		base := filepath.Base(rel)
		if d.IsDir() || strings.HasSuffix(rel, ".md") || strings.HasSuffix(rel, ".proto") ||
			base == "go.mod" || base == "go.sum" || base == "pnpm-lock.yaml" || base == "package.json" {
			return
		}
		if b, err := os.ReadFile(filepath.Join(root, rel)); err == nil && contentRe.Match(b) {
			t.Errorf("文件依赖/提及容器运行时（NFR-7）: %s", rel)
		}
	})
}

func TestMakefileTargets(t *testing.T) {
	root := repoRoot(t)
	notImplemented := map[string]string{
		"frontend": "1.36", "e2e": "1.37", "edge-test": "3.22",
		"release": "1.34",
	}
	// security 自 Story 1.2 起为真实目标（运行静态门禁，不再是占位）；其余新增门禁目标同理。
	implemented := []string{"build", "verify-build", "test", "race", "lint", "security", "gen-lint", "arch-check", "golangci", "shell-check", "gate-selftest", "vuln", "verify-gates",
		// Story 1.3 起为真实目标（契约工具链）。
		"proto", "proto-check", "openapi-check", "openapi-breaking", "migrations-check", "migrations-seal", "freeze-check", "contract-selftest", "contract-check", "contract-bin",
		// Story 1.6 起为真实目标（迁移框架与迁移测试台）。
		"migrate", "migrate-test"}

	for _, tgt := range implemented {
		cmd := exec.Command("make", "-n", "--no-print-directory", tgt)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("目标 %s 不存在或无法解析: %v\n%s", tgt, err, out)
		}
	}
	for tgt, story := range notImplemented {
		cmd := exec.Command("make", "-s", "--no-print-directory", tgt)
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Errorf("目标 %s 未实现却以 0 退出（空目标假通过）", tgt)
		}
		if want := "NOT_IMPLEMENTED(story " + story + ")"; !strings.Contains(stderr.String(), want) {
			t.Errorf("目标 %s: stderr %q 缺少 %q", tgt, stderr.String(), want)
		}
	}
}

// lint/security 必须真实接入各项门禁：任一子命令从配方中消失，CI 仍会全绿，所以逐项断言 make -n 的展开结果。
func TestMakefileWiresGates(t *testing.T) {
	root := repoRoot(t)
	expand := func(target string) string {
		cmd := exec.Command("make", "-n", "--no-print-directory", target)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("make -n %s: %v\n%s", target, err, out)
		}
		return string(out)
	}
	want := map[string][]string{
		"lint":          {"go vet", "gofmt -l", "tests/scaffold", "arch-check", "golangci", "shell-check"},
		"arch-check":    {"scripts/gates arch"},
		"golangci":      {"golangci-lint", "@v2.15.0", "run ./..."},
		"shell-check":   {"scripts/gates shell"},
		"security":      {"scripts/gates testdouble", "scripts/gates passwords", "scripts/gates brand", "scripts/gates shell"},
		"gate-selftest": {"./scripts/gates/...", "GOLANGCI_LINT=", "./tests/gates/..."},
		"verify-gates":  {"golangci-lint", "scripts/gates testdouble", "./tests/gates/..."},
		// Story 1.3：契约工具链必须真实接入（任一子命令从配方消失即失败）。
		"openapi-check":     {"bin/contract openapi-check", "bin/contract openapi-breaking", "-base-ref 'contract-v1'", "oasdiff@v1.38.0"},
		"proto-check":       {"buf@v1.75.0", " lint", " breaking proto --against '.git#ref=contract-v1,subdir=proto'", "scripts/proto-check-gen.sh"},
		"proto":             {"buf@v1.75.0", "generate"},
		"migrations-check":  {"bin/contract migrations-check", "-base-ref 'contract-v1'"},
		"freeze-check":      {"bin/contract freeze-check"},
		"contract-selftest": {"scripts/contract", "go vet ./...", "go test -count=1 ./..."},
		"contract-check":    {"bin/contract openapi-check", "scripts/proto-check-gen.sh", "bin/contract migrations-check", "bin/contract freeze-check", "go test -count=1 ./..."},
		// Story 1.6：迁移测试台必须真实接入内嵌迁移 lint、迁移包单测与真实 MySQL 的 tests/migrations（任一项从配方消失即失败）。
		"migrate-test": {"cmd/lingedge-server migrate lint", "./internal/platform/migrate/...", "LINGEDGE_REQUIRE_MYSQL=1", "./tests/migrations/..."},
		"migrate":      {"cmd/lingedge-server", "bin/lingedge-server migrate"},
	}
	for target, parts := range want {
		out := expand(target)
		for _, p := range parts {
			if !strings.Contains(out, p) {
				t.Errorf("make %s 的展开结果缺少 %q:\n%s", target, p, out)
			}
		}
	}
	if strings.Contains(expand("security"), "NOT_IMPLEMENTED") {
		t.Error("security 不应再是未实现占位")
	}
}

func TestMakefileHasNoUpdaterOrDocker(t *testing.T) {
	root := repoRoot(t)
	mk := readFile(t, filepath.Join(root, "Makefile"))
	if regexp.MustCompile(`(?i)updater|docker`).MatchString(mk) {
		t.Error("Makefile 不得含 updater/docker")
	}
	if regexp.MustCompile(`(?m)^GOBUILD\s*:?=.*-tags`).MatchString(mk) {
		t.Error("生产构建（GOBUILD）不得传入 -tags（尤其 lingedge_testdouble）")
	}
}

func TestReadmeBranding(t *testing.T) {
	readme := readFile(t, filepath.Join(repoRoot(t), "README.md"))
	for _, want := range []string{"LingEdge", "凌界云", "不使用 Docker"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README 缺少 %q", want)
		}
	}
}

// 真实执行 make build / verify-build，并验证 Makefile 的 ldflags 注入与校验脚本的负向行为。
func TestBuildMatrixAndVerifier(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过完整交叉编译矩阵")
	}
	root := repoRoot(t)
	dist := filepath.Join(t.TempDir(), "dist")
	run := func(target string) (string, error) {
		cmd := exec.Command("make", "-s", "--no-print-directory", target, "DIST="+dist, "VERSION=9.9.9-matrix", "COMMIT=cafe1234")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("build"); err != nil {
		t.Fatalf("make build 失败: %v\n%s", err, out)
	}
	if out, err := run("verify-build"); err != nil {
		t.Fatalf("make verify-build 失败: %v\n%s", err, out)
	}

	// Makefile 注入的 ldflags 符号必须真实命中 pkg/version（链接器对不存在的 -X 符号静默忽略）。
	ldCmd := exec.Command("make", "-s", "--no-print-directory", "print-ldflags", "VERSION=9.9.9-matrix", "COMMIT=cafe1234")
	ldCmd.Dir = root
	ld, err := ldCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	hostBin := filepath.Join(t.TempDir(), "lingedge")
	build := exec.Command("go", "build", "-buildvcs=false", "-ldflags", strings.TrimSpace(string(ld)), "-o", hostBin, "./cmd/lingedge")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("编译失败: %v\n%s", err, out)
	}
	out, err := exec.Command(hostBin, "--version").Output()
	if err != nil || !strings.Contains(string(out), "9.9.9-matrix") || !strings.Contains(string(out), "cafe1234") {
		t.Errorf("版本输出 %q（err=%v）未含 Makefile 注入值", out, err)
	}
	info, err := exec.Command("go", "version", "-m", filepath.Join(dist, "linux-arm64", "lingedge-server")).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CGO_ENABLED=0", "GOARCH=arm64", "GOOS=linux"} {
		if !strings.Contains(string(info), want) {
			t.Errorf("go version -m 缺少 %q", want)
		}
	}
	if strings.Contains(string(info), "-tags") {
		t.Error("生产构建产物不得带 -tags")
	}

	// 负向：缺失产物与多余产物必须使校验失败。
	victim := filepath.Join(dist, "linux-arm64", "lingedge-dns")
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	if out, err := run("verify-build"); err == nil || !strings.Contains(out, "缺失") {
		t.Errorf("缺失产物应使 verify-build 失败，输出 %q", out)
	}
	src, _ := os.ReadFile(filepath.Join(dist, "linux-amd64", "lingedge-dns"))
	if err := os.WriteFile(victim, src, 0o755); err != nil { // amd64 二进制冒充 arm64
		t.Fatal(err)
	}
	if out, err := run("verify-build"); err == nil || !strings.Contains(out, "GOOS/GOARCH 不符") {
		t.Errorf("架构不符应使 verify-build 失败，输出 %q", out)
	}
	if out, err := exec.Command("bash", filepath.Join(root, "scripts", "verify-build.sh"), filepath.Join(dist, "nope"), "lingedge").CombinedOutput(); err == nil {
		t.Errorf("dist 目录不存在应失败，输出 %q", out)
	}
}
