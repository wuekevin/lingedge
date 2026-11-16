// Package gates 对 Story 1.2 的静态质量门禁做端到端自测：每条规则都有「正向通过 + 负向必败」两类用例。
//
// 做法：把仓库的最小子集复制到临时目录，在副本里写入故意违规的文件，运行真实的门禁（golangci-lint/depguard/forbidigo、
// go build、scripts/gates 各子命令），断言违规被拦截、合法写法不被误伤；副本随测试结束删除，仓库本身不会残留任何负向样例。
//
// golangci-lint 相关用例需要环境变量 GOLANGCI_LINT（含固定版本的完整命令，如 `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0`），
// 未设置时跳过并明确提示（`make gate-selftest` 会设置）；不得把跳过当作通过。
package gates

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const mod = "github.com/wuekevin/lingedge"

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

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyRepo 复制构建与门禁所需的最小子集（不含 .git、dist、docs），返回副本根目录。
func copyRepo(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	dst := t.TempDir()
	for _, f := range []string{"go.mod", "go.sum", ".golangci.yml"} {
		if err := copyFile(filepath.Join(root, f), filepath.Join(dst, f)); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"cmd", "internal", "pkg", "scripts"} {
		err := filepath.WalkDir(filepath.Join(root, d), func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			return copyFile(p, filepath.Join(dst, rel))
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func put(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func imp(pkgs ...string) string {
	var b strings.Builder
	for _, p := range pkgs {
		b.WriteString("import _ \"" + mod + "/" + p + "\"\n")
	}
	return b.String()
}

// pkgFile 生成一个只含若干空白导入的 Go 文件。
func pkgFile(pkg string, imports ...string) string {
	return "package " + pkg + "\n\n" + imp(imports...)
}

var (
	gatesBinOnce sync.Once
	gatesBin     string
	gatesBinErr  error
)

// gatesBinary 编译一次 scripts/gates，供各副本以 -root 调用。
func gatesBinary(t *testing.T) string {
	t.Helper()
	gatesBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lingedge-gates-bin-")
		if err != nil {
			gatesBinErr = err
			return
		}
		gatesBin = filepath.Join(dir, "gates")
		c := exec.Command("go", "build", "-o", gatesBin, "./scripts/gates")
		c.Dir = repoRoot(t)
		if out, err := c.CombinedOutput(); err != nil {
			gatesBinErr = &buildErr{err, string(out)}
		}
	})
	if gatesBinErr != nil {
		t.Fatal(gatesBinErr)
	}
	return gatesBin
}

func TestMain(m *testing.M) {
	code := m.Run()
	if gatesBin != "" {
		_ = os.RemoveAll(filepath.Dir(gatesBin))
	}
	os.Exit(code)
}

type buildErr struct {
	err error
	out string
}

func (e *buildErr) Error() string { return e.err.Error() + "\n" + e.out }

// runGate 在 root 上运行门禁子命令，返回退出码与合并输出。
func runGate(t *testing.T, root string, args ...string) (int, string) {
	t.Helper()
	c := exec.Command(gatesBinary(t), append(args, "-root", root)...)
	c.Dir = root
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()
	if err == nil {
		return 0, buf.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), buf.String()
	}
	t.Fatal(err)
	return -1, ""
}

func goIn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	c := exec.Command("go", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	out, err := c.CombinedOutput()
	return string(out), err
}

// ---- 依赖方向 / 测试设施 / exec：golangci-lint + depguard + forbidigo（AC1、AC3、AC6）----

func TestLintGatesBlockViolationsAndAllowLegitimate(t *testing.T) {
	lint := os.Getenv("GOLANGCI_LINT")
	if lint == "" {
		t.Skip("未设置 GOLANGCI_LINT：跳过 golangci-lint 门禁自测（请运行 `make gate-selftest`）；跳过不代表通过")
	}
	dst := copyRepo(t)

	// 必须被拦截的违规（文件 → 期望出现在该文件诊断中的关键字）。
	violations := map[string]struct{ content, linter, mention string }{
		"internal/node/internal/service/l2.go":              {pkgFile("service", "internal/certificate"), "depguard", "internal/node 不得 import internal/certificate"},
		"cmd/lingedge-node/extra.go":                        {pkgFile("main", "internal/tenant"), "depguard", "cmd/lingedge-node"},
		"internal/tools/cli/server_dep.go":                  {pkgFile("cli", "internal/auth"), "depguard", "internal/tools 不得 import"},
		"internal/origin/internal/service/l2l2.go":          {pkgFile("service", "internal/cache"), "depguard", "internal/origin 不得 import internal/cache"},
		"internal/cache/internal/service/to_node.go":        {pkgFile("service", "internal/node"), "depguard", "internal/cache 不得 import internal/node"},
		"internal/website/internal/service/upward.go":       {pkgFile("service", "internal/origin"), "depguard", "internal/website 不得 import internal/origin"},
		"internal/platform/ports/to_module.go":              {pkgFile("ports", "internal/website"), "depguard", "internal/platform 不得 import internal/website"},
		"pkg/version/to_internal.go":                        {pkgFile("version", "internal/platform"), "depguard", "pkg/"},
		"cmd/lingedge-server/uses_tools.go":                 {pkgFile("main", "internal/tools/cli"), "depguard", "internal/tools"},
		"cmd/lingedge/uses_server.go":                       {pkgFile("main", "internal/node"), "depguard", "cmd/lingedge（Tools）"},
		"internal/auth/internal/service/prod_test_infra.go": {"package service\n\nimport \"testing\"\n\nvar _ = testing.Short\n", "depguard", "非测试代码不得 import testing"},
		"internal/auth/internal/service/prod_exec.go":       {"package service\n\nimport \"os/exec\"\n\nvar Run = func(a string) error {\n\treturn exec.Command(\"ls\", a).Run()\n}\n", "forbidigo", "os/exec"},
		"internal/auth/internal/service/prod_syscall.go":    {"package service\n\nimport \"syscall\"\n\nvar Replace = func(a string) error {\n\treturn syscall.Exec(a, nil, nil)\n}\n", "forbidigo", "syscall.Exec"},
		"internal/auth/internal/service/prod_startproc.go":  {"package service\n\nimport \"os\"\n\nvar Spawn = func(a string) (*os.Process, error) {\n\treturn os.StartProcess(a, nil, nil)\n}\n", "forbidigo", "os.StartProcess"},
		"internal/auth/internal/service/prod_time_now.go":   {"package service\n\nimport \"time\"\n\nvar Stamp = func() time.Time {\n\treturn time.Now()\n}\n", "forbidigo", "[no-direct-clock]"},
		"internal/billing/internal/service/prod_since.go":   {"package service\n\nimport \"time\"\n\nvar Age = func(t time.Time) (time.Duration, time.Duration) {\n\treturn time.Since(t), time.Until(t)\n}\n", "forbidigo", "[no-direct-clock]"},
		"internal/cache/internal/service/prod_now_alias.go": {"package service\n\nimport \"time\"\n\nvar Now = time.Now\n", "forbidigo", "[no-direct-clock]"},
		"internal/platform/other/prod_now.go":               {"package other\n\nimport \"time\"\n\nvar Stamp = func() time.Time {\n\treturn time.Now()\n}\n", "forbidigo", "[no-direct-clock]"},
		"internal/auth/internal/service/prod_exec_alias.go": {"package service\n\nimport (\n\t\"context\"\n\tx \"os/exec\"\n)\n\nvar Run2 = func(ctx context.Context, a string) error {\n\treturn x.CommandContext(ctx, \"ls\", a).Run()\n}\n", "forbidigo", "os/exec"},
	}
	// 必须通过（合法依赖与白名单用法）。
	legit := map[string]string{
		"internal/origin/internal/service/ok_site_ports.go": pkgFile("service", "internal/website", "internal/platform/ports", "internal/platform"),
		"internal/website/internal/service/ok_tenant.go":    pkgFile("service", "internal/tenant", "internal/platform"),
		"internal/reporter/internal/service/ok_monitor.go":  pkgFile("service", "internal/monitoring", "internal/platform/ports"),
		"internal/node/internal/service/ok_ports.go":        pkgFile("service", "internal/platform/ports", "pkg/version"),
		"internal/cache/internal/service/ok_own_impl.go":    pkgFile("service", "internal/cache/internal/domain", "internal/website"),
		"internal/tools/cli/ok_tools.go":                    pkgFile("cli", "internal/tools/security", "pkg/version"),
		"cmd/lingedge/ok_tools.go":                          pkgFile("main", "internal/tools/cli"),
		"cmd/lingedge-server/ok_assembly.go":                pkgFile("main", "internal/node", "internal/origin", "internal/cache", "internal/platform/ports"),
		"cmd/lingedge-node/ok_pkg.go":                       pkgFile("main", "pkg/version"),
		"internal/tools/security/ok_exec.go":                "package security\n\nimport \"os/exec\"\n\nvar Status = func() error {\n\treturn exec.Command(\"systemctl\", \"status\", \"lingedge-node\").Run()\n}\n",
		"internal/tools/security/ok_syscall.go":             "package security\n\nimport \"syscall\"\n\nvar Replace = func(a string) error {\n\treturn syscall.Exec(a, nil, nil)\n}\n",
		"internal/origin/internal/testdouble/ok_td.go":      "//go:build lingedge_testdouble\n\npackage testdouble\n\nimport \"testing\"\n\nvar _ = testing.Short\n",
		"internal/origin/internal/service/ok_exec_test.go":  "package service\n\nimport \"os/exec\"\n\nvar _ = exec.Command\n",
		// 时钟包是唯一出口；测试、Tools（不能依赖 platform）、cmd、pkg 不受「禁止直接读时钟」约束。
		"internal/platform/clock/ok_now.go":             "package clock\n\nimport \"time\"\n\nvar Stamp = func() time.Time {\n\treturn time.Now()\n}\n",
		"internal/auth/internal/service/ok_now_test.go": "package service\n\nimport \"time\"\n\nvar _ = time.Now\n",
		"internal/tools/cli/ok_now.go":                  "package cli\n\nimport \"time\"\n\nvar Stamp = func() time.Time {\n\treturn time.Now()\n}\n",
		"cmd/lingedge-node/ok_now.go":                   "package main\n\nimport \"time\"\n\nvar Stamp = func() time.Time {\n\treturn time.Now()\n}\n",
		"pkg/version/ok_now.go":                         "package version\n\nimport \"time\"\n\nvar Stamp = func() time.Time {\n\treturn time.Now()\n}\n",
		"internal/auth/internal/service/ok_injected.go": "package service\n\nimport (\n\t\"time\"\n\n\t\"github.com/wuekevin/lingedge/internal/platform/clock\"\n)\n\nvar StampInjected = func(c clock.Clock) time.Time {\n\treturn c.Now()\n}\n",
	}
	put(t, dst, "internal/cache/internal/domain/d.go", "package domain\n") // ok_own_impl.go 引用的自有实现包
	for rel, v := range violations {
		put(t, dst, rel, v.content)
	}
	for rel, c := range legit {
		put(t, dst, rel, c)
	}

	fields := strings.Fields(lint)
	c := exec.Command(fields[0], append(fields[1:], "run", "./...")...)
	c.Dir = dst
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()
	out := buf.String()
	if err == nil {
		t.Fatalf("存在故意违规，make lint 必须失败，但 golangci-lint 通过了:\n%s", out)
	}

	diag := func(rel string) []string {
		var ls []string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, rel+":") {
				ls = append(ls, l)
			}
		}
		return ls
	}
	for rel, v := range violations {
		ls := diag(rel)
		if len(ls) == 0 {
			t.Errorf("违规未被拦截: %s", rel)
			continue
		}
		joined := strings.Join(ls, "\n")
		if !strings.Contains(joined, "("+v.linter+")") || !strings.Contains(joined, v.mention) {
			t.Errorf("%s 的诊断应来自 %s 且包含 %q，实际:\n%s", rel, v.linter, v.mention, joined)
		}
	}
	for rel := range legit {
		if ls := diag(rel); len(ls) != 0 {
			t.Errorf("合法写法被误伤: %s\n%s", rel, strings.Join(ls, "\n"))
		}
	}
	if t.Failed() {
		t.Logf("golangci-lint 完整输出:\n%s", out)
	}
}

// ---- 模块对外契约 = 根包：跨模块 import 对方 internal 子包编译失败（AC6）----

func TestCrossModuleInternalImportFailsToCompile(t *testing.T) {
	dst := copyRepo(t)
	put(t, dst, "internal/cache/internal/service/svc.go", "package service\n")
	put(t, dst, "internal/cache/internal/domain/d.go", "package domain\n")
	// 合法：模块自己的实现包互相引用、他人引用根包。
	put(t, dst, "internal/cache/internal/service/own.go", pkgFile("service", "internal/cache/internal/domain", "internal/website"))
	if out, err := goIn(t, dst, "build", "./internal/..."); err != nil {
		t.Fatalf("合法依赖应编译通过: %v\n%s", err, out)
	}
	// 违规：origin 直接 import cache 的实现包。
	put(t, dst, "internal/origin/peek.go", pkgFile("origin", "internal/cache/internal/service"))
	out, err := goIn(t, dst, "build", "./internal/...")
	if err == nil || !strings.Contains(out, "use of internal package") {
		t.Fatalf("跨模块 import internal/cache/internal/... 必须编译失败（use of internal package），实际 err=%v\n%s", err, out)
	}
}

// ---- scripts/gates 各子命令的端到端（含构建层的测试替身反向检查，AC2）----

func TestGateCommandsNegativeAndCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过（含真实编译）")
	}
	dst := copyRepo(t)
	// 门禁在 gates 目录自身被跳过；副本里的 docs/ 不存在，品牌与口令门禁只看代码与配置。
	for _, g := range []string{"arch", "testdouble", "passwords", "brand", "shell"} {
		if code, out := runGate(t, dst, g); code != 0 {
			t.Fatalf("干净副本的 %s 门禁应通过，退出码 %d:\n%s", g, code, out)
		}
	}
	if code, out := runGate(t, dst, "gen-lint", "-check"); code != 0 {
		t.Fatalf("gen-lint -check 应通过: %d\n%s", code, out)
	}

	cases := []struct {
		name, gate string
		files      map[string]string
		wantRule   string
		wantText   string
	}{
		{
			// 生产依赖图含测试替身包：替身漏写构建标签且被 cmd 引用 → go list -deps 命中。
			name: "测试替身进入生产依赖图", gate: "testdouble", wantRule: "testdouble-deps", wantText: "生产依赖图（",
			files: map[string]string{
				// 位于 internal/testdouble（cmd 可合法 import），但漏写构建标签 → 进入生产依赖图。
				"internal/testdouble/td.go": "package testdouble\n",
				"cmd/lingedge-worker/td.go": pkgFile("main", "internal/testdouble"),
			},
		},
		{
			// 常量折叠后才形成假数据标记：源码字面量扫描看不到，只有产物字符串扫描能拦截。
			name: "产物字符串含假数据标记", gate: "testdouble", wantRule: "testdouble-artifact",
			files: map[string]string{
				"cmd/lingedge-dns/marker.go": "package main\n\nimport \"os\"\n\nconst a, b = \"fake-\", \"traffic\"\n\nfunc init() { _ = os.Getenv(a + b) }\n",
			},
		},
		{
			name: "源码含假数据标记", gate: "testdouble", wantRule: "testdouble-fake-data",
			files: map[string]string{"internal/monitoring/seed.go": "package monitoring\n\nconst n = \"sample-node-01\"\n"},
		},
		{
			name: "模块根包含实现", gate: "arch", wantRule: "arch-module-root",
			files: map[string]string{"internal/origin/impl.go": "package origin\n"},
		},
		{
			name: "ports 含实现", gate: "arch", wantRule: "arch-ports",
			files: map[string]string{"internal/platform/ports/impl.go": "package ports\n\nfunc New() int { return 1 }\n"},
		},
		{
			name: "固定口令（代码）", gate: "passwords", wantRule: "password",
			files: map[string]string{"internal/auth/seed.go": "package auth\n\nvar seed = map[string]string{\"" + "pass" + "word\": \"" + "hun" + "ter2\"}\n"},
		},
		{
			name: "固定口令（配置模板）", gate: "passwords", wantRule: "password",
			files: map[string]string{"configs/server.example.yaml": "db:\n  " + "pass" + "word: \"" + "Welc" + "ome1\"\n"},
		},
		{
			name: "固定口令（迁移）", gate: "passwords", wantRule: "password",
			files: map[string]string{"migrations/000001_seed.sql": "CREATE USER 'app'@'%' IDENTI" + "FIED BY 'li" + "ngedge';\n"},
		},
		{
			name: "其他产品名残留", gate: "brand", wantRule: "brand",
			files: map[string]string{"configs/brand.yaml": "name: " + "Smart" + "CDN\n"},
		},
		{
			name: "shell -c", gate: "shell", wantRule: "shell-c",
			files: map[string]string{"deploy/systemd/x.service": "ExecStart=/bin/" + "sh -" + "c 'run'\n"},
		},
		{
			name: "字符串拼接的 exec", gate: "shell", wantRule: "shell-exec",
			files: map[string]string{"internal/tools/security/cat.go": "package security\n\nimport \"os/exec\"\n\nfunc f(s string) { _ = exec.Command(\"systemctl \" + s) }\n"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for rel, content := range c.files {
				put(t, dst, rel, content)
			}
			code, out := runGate(t, dst, c.gate)
			if code != 1 || !strings.Contains(out, "["+c.wantRule+"]") || !strings.Contains(out, c.wantText) {
				t.Errorf("%s 门禁应以退出码 1 失败并报告 %s，实际 %d:\n%s", c.gate, c.wantRule, code, out)
			}
			// 清理负向样例后必须恢复通过（证明拦截来自该样例，且不会残留）。
			for rel := range c.files {
				if err := os.Remove(filepath.Join(dst, filepath.FromSlash(rel))); err != nil {
					t.Fatal(err)
				}
			}
			if code, out := runGate(t, dst, c.gate); code != 0 {
				t.Errorf("清理样例后 %s 门禁应恢复通过，退出码 %d:\n%s", c.gate, code, out)
			}
		})
	}
}
