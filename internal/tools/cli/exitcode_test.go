package cli

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// tableFromDoc 解析 Markdown 中「| 码 | 名称 | … |」形态的退出码表（限定在 heading 之后、下一个同级或更高级 heading 之前）。
func tableFromDoc(t *testing.T, rel, heading string) map[int]string {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	level := len(heading) - len(strings.TrimLeft(heading, "#"))
	in := false
	rows := map[int]string{}
	re := regexp.MustCompile("^\\|\\s*(\\d+)\\s*\\|\\s*`?([A-Z_—-]+)`?\\s*\\|")
	for _, l := range lines {
		if !in {
			if strings.HasPrefix(l, heading) {
				in = true
			}
			continue
		}
		if strings.HasPrefix(l, "#") {
			lv := len(l) - len(strings.TrimLeft(l, "#"))
			if lv <= level {
				break
			}
		}
		if m := re.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			rows[n] = m[2]
		}
	}
	if len(rows) == 0 {
		t.Fatalf("%s: 未在 %q 下解析到退出码表", rel, heading)
	}
	return rows
}

// AC2/AC7：Go 常量表与 tools-design §4、ux/cli-tui §4.2 逐项一致（码与名称）。
func TestExitTableMatchesDocs(t *testing.T) {
	for _, d := range []struct{ file, heading string }{
		{"docs/architecture/tools-design.md", "## 4. 退出码"},
		{"docs/ux/cli-tui.md", "### 4.2 退出码"},
	} {
		got := tableFromDoc(t, d.file, d.heading)
		if len(got) != len(exitTable) {
			t.Errorf("%s: 文档有 %d 项，常量表 %d 项", d.file, len(got), len(exitTable))
		}
		for _, e := range exitTable {
			if got[int(e.Code)] != e.Name {
				t.Errorf("%s: 码 %d 文档名称 %q，常量表 %q", d.file, e.Code, got[int(e.Code)], e.Name)
			}
		}
	}
}

func TestExitTableShape(t *testing.T) {
	want := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 130}
	if len(exitTable) != len(want) {
		t.Fatalf("退出码表项数 %d，期望 %d", len(exitTable), len(want))
	}
	seen := map[string]bool{}
	for i, e := range exitTable {
		if int(e.Code) != want[i] {
			t.Errorf("第 %d 项码 %d，期望 %d", i, e.Code, want[i])
		}
		if seen[e.Name] || e.Name == "" || e.Name == "UNKNOWN" {
			t.Errorf("名称非法或重复: %q", e.Name)
		}
		seen[e.Name] = true
		if e.Code.Name() != e.Name {
			t.Errorf("Name() 不一致: %d", e.Code)
		}
	}
	if ExitCode(99).Name() != "UNKNOWN" {
		t.Error("表外退出码应为 UNKNOWN")
	}
}

// envelope schema 里的 exit_code/exit_name 枚举必须与退出码表一致（同一契约的两个载体不得漂移）。
func TestEnvelopeSchemaEnumsMatchExitTable(t *testing.T) {
	s := readSchema(t, "envelope.schema.json")
	codes := s.Properties["exit_code"].Value.Enum
	names := s.Properties["exit_name"].Value.Enum
	if len(codes) != len(exitTable) || len(names) != len(exitTable) {
		t.Fatalf("schema 枚举项数 %d/%d，退出码表 %d", len(codes), len(names), len(exitTable))
	}
	for i, e := range exitTable {
		if int(codes[i].(float64)) != int(e.Code) || names[i].(string) != e.Name {
			t.Errorf("第 %d 项 schema=(%v,%v)，表=(%d,%s)", i, codes[i], names[i], e.Code, e.Name)
		}
	}
}

// AC2：--help exit-codes 输出整张表（人类与 JSON 两种形态）。
func TestHelpExitCodes(t *testing.T) {
	for _, args := range [][]string{{"--help", "exit-codes"}, {"help", "exit-codes"}, {"-h", "exit-codes"}} {
		h := newHarness(t)
		if code := h.run(args...); code != 0 {
			t.Fatalf("%v 退出码 %d", args, code)
		}
		out := h.stdout.String()
		for _, e := range exitTable {
			if !regexp.MustCompile(`(?m)^` + strconv.Itoa(int(e.Code)) + `\s+` + e.Name + `\s`).MatchString(out) {
				t.Errorf("%v: 输出缺少 %d %s\n%s", args, e.Code, e.Name, out)
			}
		}
	}
	h := newHarness(t)
	if code := h.run("--help", "exit-codes", "--json"); code != 0 {
		t.Fatalf("退出码 %d", code)
	}
	env := h.envelope()
	data := env["data"].(map[string]any)
	if len(data["exit_codes"].([]any)) != len(exitTable) {
		t.Errorf("JSON exit_codes 项数不符: %v", data)
	}
	if err := validateAgainst(t, "data.exit-codes.schema.json", data); err != nil {
		t.Error(err)
	}
}

// 退出码映射（表驱动）：每个「当前有生产者」的退出码都由真实命令路径产生，并同时体现在进程返回值与 envelope 中。
// 4–7（7 除外之外）、10–14 的生产者在后续 Story（端口探测/验签/任务锁/清单/回滚/journal/registry）；
// 它们的名称与取值由 TestExitTableShape 与 schema 枚举测试锁定。
func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		name     string
		opts     []hopt
		args     []string
		want     ExitCode
		wantCode string
	}{
		{"成功 version", nil, []string{"version"}, ExitOK, ""},
		{"未实现命令 → 1", []hopt{asRoot()}, []string{"update", "server"}, ExitGeneral, "cli.not_implemented"},
		{"未知组件 → 2", nil, []string{"install", "nope"}, ExitUsage, "usage.invalid_component"},
		{"未知命令 → 2", nil, []string{"frobnicate"}, ExitUsage, "usage.invalid"},
		{"未知标志 → 2", nil, []string{"version", "--nope"}, ExitUsage, "usage.invalid_flag"},
		{"非 root → 3", nil, []string{"install", "server"}, ExitPrecondition, "precondition.root_required"},
		{"内置组件 → 8", nil, []string{"install", "worker"}, ExitBuiltin, "component.builtin"},
		{"非交互缺确认 → 9", []hopt{asRoot()}, []string{"uninstall", "server"}, ExitConfirmRequired, "confirm.required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.opts...)
			args := append(append([]string{}, c.args...), "--json")
			code := h.run(args...)
			if ExitCode(code) != c.want {
				t.Fatalf("退出码 %d，期望 %d\nstdout=%s\nstderr=%s", code, c.want, h.stdout, h.stderr)
			}
			env := h.envelope()
			if int(env["exit_code"].(float64)) != code || env["exit_name"] != c.want.Name() || env["ok"] != (code == 0) {
				t.Errorf("envelope 与退出码不一致: %v", env)
			}
			if got := errCode(env); got != c.wantCode {
				t.Errorf("errors[0].code = %q，期望 %q", got, c.wantCode)
			}
			if err := validateAgainst(t, "envelope.schema.json", env); err != nil {
				t.Errorf("envelope 不符合 schema: %v", err)
			}
		})
	}
}

// 130：SIGINT 被取消的 context（Main 把 SIGINT 绑定到它）。
func TestInterruptExit130(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := newHarness(t)
	if code := h.runCtx(ctx, "list", "--json"); code != 130 {
		t.Fatalf("退出码 %d，期望 130", code)
	}
	env := h.envelope()
	if env["exit_name"] != "SIGINT" || errCode(env) != "interrupt.sigint" {
		t.Errorf("envelope: %v", env)
	}
	if err := validateAgainst(t, "envelope.schema.json", env); err != nil {
		t.Error(err)
	}
}
