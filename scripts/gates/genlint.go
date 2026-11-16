package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const lintConfigName = ".golangci.yml"

type denyEntry struct{ pkg, desc string }

type depRule struct {
	name  string
	files []string
	deny  []denyEntry
}

func q(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func internalPkg(parts ...string) string {
	return modulePath + "/" + strings.Join(parts, "/")
}

// buildDepRules 据 spec.go 机械生成 depguard 规则（list-mode lax：仅列出禁止项）。
func buildDepRules() []depRule {
	var rules []depRule
	base := "${base-path}"

	// 1. Server 内各模块：只允许图中的箭头（+ platform），其余全部拒绝；允许的依赖只能 import 其根包。
	for _, m := range serverModules {
		allowed := append([]string{"platform"}, allowedDeps[m]...)
		if m == "platform" {
			allowed = nil
		}
		r := depRule{name: "module-" + m, files: []string{base + "/internal/" + m + "/**"}}
		for _, x := range append(append([]string(nil), serverModules...), "tools") {
			if x == m {
				continue
			}
			if contains(allowed, x) {
				r.deny = append(r.deny, denyEntry{
					internalPkg("internal", x, "internal"),
					fmt.Sprintf("模块 %s 只能 import %s 的根包（ADR-001 §3：契约=根包，实现位于 internal/%s/internal/…）", m, x, x),
				})
				continue
			}
			reason := fmt.Sprintf("依赖方向违规：internal/%s 不得 import internal/%s（architecture §5.3 / ADR-001 §4；必需的跨模块调用经 internal/platform/ports，实现在 cmd/lingedge-server 装配层注入）", m, x)
			if x == "tools" {
				reason = fmt.Sprintf("internal/%s 不得 import internal/tools（Tools 与 Server 无编译依赖，architecture §5.3 规则 5）", m)
			}
			r.deny = append(r.deny, denyEntry{internalPkg("internal", x), reason})
		}
		rules = append(rules, r)
	}

	// 2. internal/tools：与 Server 无编译依赖（含 platform）。
	tools := depRule{name: "tools-no-server", files: []string{base + "/internal/tools/**"}}
	for _, x := range serverModules {
		tools.deny = append(tools.deny, denyEntry{internalPkg("internal", x),
			fmt.Sprintf("internal/tools 不得 import Server 业务/平台模块 internal/%s（architecture §5.3 规则 5；仅经发行清单/状态文件/子命令协作）", x)})
	}
	rules = append(rules, tools)

	// 3. pkg：无业务共享库，被独立二进制导入，不得反向依赖 internal。
	rules = append(rules, depRule{name: "pkg-no-internal", files: []string{base + "/pkg/**"},
		deny: []denyEntry{{internalPkg("internal"), "pkg/ 为所有二进制共享的无业务库，不得 import internal/*（architecture §5.2）"}}})

	// 4. 独立二进制：node/dns/reporter 只依赖 pkg、proto、edge。
	for _, b := range standaloneBinaries {
		rules = append(rules, depRule{name: "cmd-" + b, files: []string{base + "/cmd/" + b + "/**"},
			deny: []denyEntry{{internalPkg("internal"), fmt.Sprintf("cmd/%s 只依赖 pkg/、proto/、edge/，不得 import internal/*（ADR-001 §7、architecture §5.3 规则 4）", b)}}})
	}

	// 5. Tools CLI 入口：不得 import Server 模块。
	cli := depRule{name: "cmd-" + toolsBinary, files: []string{base + "/cmd/" + toolsBinary + "/**"}}
	for _, x := range serverModules {
		cli.deny = append(cli.deny, denyEntry{internalPkg("internal", x),
			fmt.Sprintf("cmd/%s（Tools）不得 import Server 模块 internal/%s（architecture §5.3 规则 5）", toolsBinary, x)})
	}
	rules = append(rules, cli)

	// 6. Server 装配入口：不得 import internal/tools。
	for _, b := range serverBinaries {
		rules = append(rules, depRule{name: "cmd-" + b, files: []string{base + "/cmd/" + b + "/**"},
			deny: []denyEntry{{internalPkg("internal", "tools"), fmt.Sprintf("cmd/%s 不得 import internal/tools（Tools 与 Server 无编译依赖）", b)}}})
	}

	// 7. 生产代码不得依赖测试设施（NFR-1、ADR-023 §A.6：生产路径中没有「检测到测试环境」的分支）。
	rules = append(rules, depRule{
		name:  "no-test-infra-in-production",
		files: []string{"!$test", "!" + base + "/tests/**", "!**/testdouble/**"},
		deny: []denyEntry{
			{"testing", "非测试代码不得 import testing（ADR-023 §A.6）；测试替身只允许位于 _test.go、tests/、internal/**/testdouble/"},
			{"net/http/httptest", "非测试代码不得 import net/http/httptest（NFR-1）"},
			{"github.com/stretchr/testify", "非测试代码不得 import 测试库 testify（NFR-1）"},
			{"go.uber.org/mock", "非测试代码不得 import 测试替身库 gomock（NFR-1）"},
			{"github.com/DATA-DOG/go-sqlmock", "非测试代码不得 import sqlmock（NFR-1）"},
			{"github.com/alicebob/miniredis", "非测试代码不得 import miniredis（NFR-1；Redis 相关验证用真实实例）"},
		},
	})

	sort.SliceStable(rules, func(i, j int) bool { return rules[i].name < rules[j].name })
	return rules
}

// forbidigo：禁止 shell/任意命令执行的入口（G-12、NFR-9）。internal/tools/security 是唯一白名单入口。
var forbidPatterns = []struct{ pattern, msg string }{
	{`^exec\.(Command|CommandContext|Cmd)$`, "禁止直接使用 os/exec（G-12/NFR-9）：外部程序只能经 internal/tools/security 的白名单入口以参数数组调用"},
	{`^syscall\.(Exec|ForkExec|StartProcess)$`, "禁止 syscall 级进程执行（G-12/NFR-9）：只能经 internal/tools/security 白名单入口"},
	{`^os\.StartProcess$`, "禁止 os.StartProcess（G-12/NFR-9）：只能经 internal/tools/security 白名单入口"},
	{`^unix\.(Exec|ForkExec)$`, "禁止 x/sys/unix 进程执行（G-12/NFR-9）：只能经 internal/tools/security 白名单入口"},
	{`^(syscall|windows)\.CreateProcess[AW]?$`, "禁止直接创建进程（G-12/NFR-9）：只能经 internal/tools/security 白名单入口"},
}

// noDirectClockMarker 是 forbidigo 诊断里的固定标记，exclusions 据此把规则限定在 Server 业务代码（internal/<server 模块>/）内。
const noDirectClockMarker = "[no-direct-clock]"

// clockPackage 是唯一允许读取系统时钟的包（Story 1.4 AC5）。
const clockPackage = "internal/platform/clock"

// clockForbid：业务代码不得直接读系统时钟（含把 time.Now 当函数值别名），一律经注入的 platform/clock.Clock，使时间可被测试注入。
// 适用范围见 renderLintConfig 的 exclusions：仅 internal/<Server 模块>/；Tools（internal/tools，不能依赖 platform）、cmd、pkg、scripts、tests 不受此条约束。
var clockForbid = struct{ pattern, msg string }{
	`^time\.(Now|Since|Until)$`,
	noDirectClockMarker + " 禁止直接读取系统时钟（Story 1.4 AC5）：经注入的 internal/platform/clock.Clock 取时间（clock.Since/clock.Until 代替 time.Since/time.Until）；仅 " + clockPackage + " 可调用",
}

func renderLintConfig() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("# 本文件由 `go run ./scripts/gates gen-lint` 生成，请勿手改。\n")
	w("# 依赖方向规则据 docs/architecture/adr/ADR-001-modular-monolith.md §3–§7 与 architecture §5.3 的依赖图机械生成（来源：scripts/gates/spec.go）；\n")
	w("# `make arch-check` 校验本文件与规格一致。golangci-lint 版本固定在 Makefile 的 GOLANGCI_LINT_VERSION。\n")
	w("version: \"2\"\n\n")
	w("run:\n  timeout: 10m\n  # 含 testdouble 构建标签，使 internal/**/testdouble 包同样受静态规则约束（生产构建仍不带该标签）。\n  build-tags:\n    - lingedge_testdouble\n\n")
	w("linters:\n  default: none\n  enable:\n")
	for _, l := range []string{"depguard", "errcheck", "forbidigo", "govet", "ineffassign", "staticcheck", "unused"} {
		w("    - %s\n", l)
	}
	w("  settings:\n    depguard:\n      rules:\n")
	for _, r := range buildDepRules() {
		w("        %s:\n          list-mode: lax\n          files:\n", r.name)
		for _, f := range r.files {
			w("            - %s\n", q(f))
		}
		w("          deny:\n")
		for _, d := range r.deny {
			w("            - pkg: %s\n              desc: %s\n", q(d.pkg), q(d.desc))
		}
	}
	w("    forbidigo:\n      analyze-types: true\n      forbid:\n")
	for _, p := range forbidPatterns {
		w("        - pattern: %s\n          msg: %s\n", q(p.pattern), q(p.msg))
	}
	w("        - pattern: %s\n          msg: %s\n", q(clockForbid.pattern), q(clockForbid.msg))
	w("  exclusions:\n    generated: lax\n    rules:\n")
	w("      # 唯一白名单入口：internal/tools/security（其内部仍受 `gates shell` 的参数数组/无 shell 检查约束）。\n")
	w("      - path: %s\n        linters:\n          - forbidigo\n", q(`^internal/tools/security/`))
	w("      # 仓库开发期脚本与测试代码不进入任何发行产物；其 shell 用法仍由 `gates shell` 检查。\n")
	w("      - path: %s\n        linters:\n          - forbidigo\n", q(`^scripts/`))
	w("      - path: %s\n        linters:\n          - forbidigo\n", q(`_test\.go$`))
	w("      - path: %s\n        linters:\n          - forbidigo\n", q(`^tests/`))
	w("      # 直接读时钟的禁令（%s）只约束 Server 业务代码 internal/<Server 模块>/，其余路径整体豁免；时钟包自身是唯一出口。\n", noDirectClockMarker)
	w("      - path-except: %s\n        linters:\n          - forbidigo\n        text: %s\n", q(`^internal/(`+strings.Join(serverModules, "|")+`)/`), q(regexpQuote(noDirectClockMarker)))
	w("      - path: %s\n        linters:\n          - forbidigo\n        text: %s\n", q(`^`+clockPackage+`/`), q(regexpQuote(noDirectClockMarker)))
	return b.String()
}

func regexpQuote(s string) string { return regexp.QuoteMeta(s) }

func runGenLint(root string, opts options) ([]Finding, error) {
	want := renderLintConfig()
	path := filepath.Join(root, lintConfigName)
	if !opts.check {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			return nil, err
		}
		return nil, nil
	}
	got, err := os.ReadFile(path)
	if err != nil {
		return []Finding{{File: lintConfigName, Rule: "gen-lint", Msg: "缺少 .golangci.yml：运行 `make gen-lint`"}}, nil
	}
	if strings.ReplaceAll(string(got), "\r\n", "\n") != want {
		return []Finding{{File: lintConfigName, Rule: "gen-lint", Msg: "与依赖图规格不一致（被手改或规格已变）：运行 `make gen-lint` 重新生成并提交"}}, nil
	}
	return nil, nil
}
