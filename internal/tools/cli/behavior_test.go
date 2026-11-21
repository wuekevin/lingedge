package cli

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const nonInteractiveNotice = "已自动启用 --non-interactive"

// AC4：无 TTY、TERM=dumb、CI=true 自动等价 --non-interactive，并只提示一次；显式标志不提示。
func TestAutoNonInteractiveNoticeOnce(t *testing.T) {
	cases := []struct {
		name string
		opts []hopt
		args []string
		want bool
	}{
		{"无 TTY", nil, []string{"version"}, true},
		{"TERM=dumb", []hopt{withTTY(), withEnv(map[string]string{"TERM": "dumb"})}, []string{"version"}, true},
		{"CI=true", []hopt{withTTY(), withEnv(map[string]string{"CI": "true"})}, []string{"version"}, true},
		{"有 TTY 无提示", []hopt{withTTY()}, []string{"version"}, false},
		{"显式 --non-interactive 不提示", nil, []string{"version", "--non-interactive"}, false},
		{"显式 --json 不提示", nil, []string{"version", "--json"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.opts...)
			if code := h.run(c.args...); code != 0 {
				t.Fatalf("退出码 %d: %s", code, h.stderr)
			}
			if n := strings.Count(h.stderr.String(), nonInteractiveNotice); (n == 1) != c.want || n > 1 {
				t.Errorf("提示出现 %d 次，期望 %v\nstderr=%q", n, c.want, h.stderr.String())
			}
			if strings.Contains(h.stdout.String(), nonInteractiveNotice) {
				t.Error("提示必须走 stderr")
			}
		})
	}
}

// --json 下自动判定的提示是 stderr 上的 NDJSON 事件；--progress=none 可关闭；stdout 仍只有一个 envelope。
func TestAutoNoticeIsNDJSONUnderJSON(t *testing.T) {
	// 显式 --json 不触发「自动」提示（见上），故这里用 CI=true + TTY 的组合之外的路径：直接断言 notice 的 NDJSON 形态。
	h := newHarness(t)
	r := &Run{env: h.env.withDefaults(), ctx: context.Background(), runID: testRunID}
	r.opts.JSON = true
	r.notice("cli.non_interactive_auto", "测试提示 password=hunter2")
	line := strings.TrimSpace(h.stderr.String())
	m := decodeAny(t, []byte(line)).(map[string]any)
	if m["type"] != "notice" || m["code"] != "cli.non_interactive_auto" || strings.Contains(line, "hunter2") {
		t.Errorf("NDJSON 事件不符: %s", line)
	}
	h.stderr.Reset()
	r.opts.Progress = "none"
	r.notice("x.y", "m")
	if h.stderr.Len() != 0 {
		t.Errorf("--progress=none 应关闭事件: %q", h.stderr.String())
	}
}

// AC4：需要确认而未给显式标志 → 退出码 9；--yes 放行（继续走到该命令自己的结果）。
func TestConfirmationRules(t *testing.T) {
	t.Run("非交互无 --yes → 9", func(t *testing.T) {
		h := newHarness(t, asRoot())
		if code := h.run("rollback", "node"); code != 9 {
			t.Fatalf("退出码 %d\n%s", code, h.stderr)
		}
		if !strings.Contains(h.stderr.String(), "--yes") {
			t.Errorf("应提示显式标志: %s", h.stderr)
		}
	})
	t.Run("--yes 通过确认，落到未实现 → 1", func(t *testing.T) {
		h := newHarness(t, asRoot())
		if code := h.run("rollback", "node", "--yes"); code != 1 {
			t.Fatalf("退出码 %d\n%s", code, h.stderr)
		}
	})
	t.Run("--json 隐含非交互：即使有 TTY 也不提问", func(t *testing.T) {
		h := newHarness(t, asRoot(), withTTY(), withStdin("y\n"))
		if code := h.run("rollback", "node", "--json"); code != 9 {
			t.Fatalf("退出码 %d\n%s", code, h.stdout)
		}
	})
	t.Run("交互：y 通过 / n 拒绝（提问走 stderr）", func(t *testing.T) {
		h := newHarness(t, asRoot(), withTTY(), withStdin("y\n"))
		if code := h.run("update", "node"); code != 1 { // update node 无确认，作对照：交互模式不提问（Story 1.33 起 recover 已实现，改用仍未实现的 update）
			t.Fatalf("update 退出码 %d", code)
		}
		h = newHarness(t, asRoot(), withTTY(), withStdin("y\n"))
		if code := h.run("backup", "restore", "x.tar", "--non-interactive=false"); code != 1 {
			t.Fatalf("交互 y 后应落到未实现(1)，得 %d\n%s", code, h.stderr)
		}
		h = newHarness(t, asRoot(), withTTY(), withStdin("n\n"))
		// 交互模式下未实现命令不征求确认（无意义的提问），直接说明尚未实现。
		if code := h.run("backup", "restore", "x.tar"); code != 1 || strings.Contains(h.stderr.String(), "[y/N]") {
			t.Fatalf("交互未实现命令不应提问，得 %d\n%s", code, h.stderr)
		}
	})
	t.Run("--purge 需要主机名", func(t *testing.T) {
		cases := []struct {
			name string
			opts []hopt
			args []string
			want int
		}{
			{"非交互无 confirm-name", []hopt{asRoot()}, []string{"uninstall", "node", "--purge", "--yes"}, 9},
			{"非交互名字不符", []hopt{asRoot()}, []string{"uninstall", "node", "--purge", "--yes", "--confirm-name", "other"}, 9},
			{"非交互仅 confirm-name 无 --yes", []hopt{asRoot()}, []string{"uninstall", "node", "--purge", "--confirm-name", "edge-host-1"}, 9},
			{"非交互 --yes + 正确主机名", []hopt{asRoot()}, []string{"uninstall", "node", "--purge", "--yes", "--confirm-name", "edge-host-1"}, 1},
			{"--confirm-name 无 --purge", []hopt{asRoot()}, []string{"uninstall", "node", "--confirm-name", "edge-host-1"}, 2},
		}
		for _, c := range cases {
			h := newHarness(t, c.opts...)
			if code := h.run(c.args...); code != c.want {
				t.Errorf("%s: 退出码 %d，期望 %d\n%s", c.name, code, c.want, h.stderr)
			}
		}
	})
}

// confirmYes/confirmHostname 的交互分支（stdin 真实读取）。
func TestInteractivePrompts(t *testing.T) {
	mk := func(in string) *Run {
		h := newHarness(t, withTTY(), withStdin(in), withHostname("edge-host-1"))
		r := &Run{env: h.env.withDefaults(), ctx: context.Background()}
		r.stdin = bufioReader(r.env.Stdin)
		r.nonInter = false
		return r
	}
	if e := mk("y\n").confirmYes("继续？"); e != nil {
		t.Errorf("y 应通过: %v", e)
	}
	if e := mk("YES\n").confirmYes("继续？"); e != nil {
		t.Errorf("YES 应通过: %v", e)
	}
	if e := mk("n\n").confirmYes("继续？"); e == nil || e.Exit != ExitConfirmRequired {
		t.Errorf("n 应拒绝(9): %v", e)
	}
	if e := mk("").confirmYes("继续？"); e == nil || e.Exit != ExitConfirmRequired {
		t.Errorf("EOF 应拒绝(9): %v", e)
	}
	if e := mk("edge-host-1\n").confirmHostname(""); e != nil {
		t.Errorf("键入正确主机名应通过: %v", e)
	}
	if e := mk("edge-host-2\n").confirmHostname(""); e == nil || e.Exit != ExitConfirmRequired {
		t.Errorf("键入错误主机名应拒绝: %v", e)
	}
	if e := mk("").confirmHostname("edge-host-1"); e != nil {
		t.Errorf("交互 + 正确 --confirm-name 应通过: %v", e)
	}
}

// AC5：非 root 执行需 root 的命令 → 3 + `sudo lingedge …` 提示；--token 在提示中被打码。
func TestNonRootHintsSudo(t *testing.T) {
	h := newHarness(t)
	if code := h.run("service", "restart", "node", "--non-interactive"); code != 3 {
		t.Fatalf("退出码 %d", code)
	}
	if !strings.Contains(h.stderr.String(), "sudo lingedge service restart node") {
		t.Errorf("缺少 sudo 提示: %s", h.stderr)
	}
	h = newHarness(t)
	if code := h.run("node", "enroll", "--token", "lenr1.SUPERSECRETVALUE", "--json"); code != 3 {
		t.Fatalf("退出码 %d", code)
	}
	if strings.Contains(h.stdout.String(), "SUPERSECRETVALUE") || !strings.Contains(h.stdout.String(), "sudo lingedge node enroll --token ***") {
		t.Errorf("sudo 提示应打码令牌: %s", h.stdout)
	}
	// 需 root 的全集：全部返回 3（而不是 1 假装可用）。
	for _, args := range [][]string{
		{"update", "node"}, {"update", "self"}, {"service", "start", "node"}, {"service", "stop", "node"},
		{"backup", "create"}, {"backup", "restore", "f.tar"}, {"node", "enroll"}, {"rollback", "node"}, {"uninstall", "node"},
		{"recover"}, {"config", "edit", "server"},
	} {
		h := newHarness(t)
		if code := h.run(append(args, "--non-interactive")...); code != 3 {
			t.Errorf("%v 非 root 退出码 %d，期望 3", args, code)
		}
	}
}

// AC5：version/list/status 对无 state.json 返回空而非报错，且普通用户可运行。
func TestReadOnlyCommandsEmptyWithoutState(t *testing.T) {
	for _, c := range []string{"version", "list", "status"} {
		h := newHarness(t)
		if code := h.run(c, "--json"); code != 0 {
			t.Fatalf("%s 退出码 %d\n%s", c, code, h.stdout)
		}
		data := h.envelope()["data"].(map[string]any)
		if comps, ok := data["components"].([]any); !ok || len(comps) != 0 {
			t.Errorf("%s: components 应为空数组: %v", c, data)
		}
		h = newHarness(t)
		if code := h.run(c, "--non-interactive"); code != 0 {
			t.Errorf("%s 人类输出退出码 %d", c, code)
		}
	}
}

// state.json 存在但读不懂时不假装为空，如实报错（退出码 1）。
// Story 1.29 起读取器真实存在（registry）：1.25 时期这里期望的占位错误 state.reader_unavailable 随之消失，
// 同一份内容（缺 schema_version 的 `{"components":{}}`）现在被严格解析为 state.corrupt——「绝不当作空」的语义不变。
func TestStateFilePresentIsNotSilentlyEmpty(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(filepath.Dir(h.env.StatePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.env.StatePath, []byte(`{"components":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"version", "list", "status"} {
		if code := h.run(c, "--json"); code != 1 {
			t.Fatalf("%s: 退出码 %d，期望 1", c, code)
		}
		if got := errCode(h.envelope()); got != "state.corrupt" {
			t.Errorf("%s: errors.code=%q", c, got)
		}
	}
	// 目录不可达（父路径是文件）→ 3，而非空。
	h2 := newHarness(t)
	blocker := filepath.Join(h2.dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h2.env.State = FileState{Path: filepath.Join(blocker, "state.json")}
	if code := h2.run("list", "--json"); code != 3 {
		t.Errorf("父路径为文件应得 3，得 %d", code)
	}
}

// 非空状态来源下的输出形状（来源是 registry 的接口；此处用消费方侧的实现验证渲染与 schema，不代表 state.json 读取已实现）。
type fixedState []ComponentInfo

func (f fixedState) Components(context.Context) ([]ComponentInfo, error) { return f, nil }

func TestNonEmptyStateRendering(t *testing.T) {
	src := fixedState{{Name: "node", Version: "1.0.0", State: "running", Unit: "lingedge-node.service", RunUser: "lingedge-node",
		Ports: []int{80, 443}, ReleaseDir: "/opt/lingedge/node/releases/1.0.0", ConfigDir: "/etc/lingedge/node", DataDir: "/var/lib/lingedge-node",
		InstalledAt: "2026-11-21T12:00:00.000Z"}, {Name: "docs", Version: "1.0.0", State: "installed"}}
	for _, c := range []string{"version", "list", "status"} {
		h := newHarness(t, withState(src))
		if code := h.run(c, "--json"); code != 0 {
			t.Fatalf("%s 退出码 %d", c, code)
		}
		env := h.envelope()
		if err := validateAgainst(t, "data."+c+".schema.json", env["data"]); err != nil {
			t.Errorf("%s: %v", c, err)
		}
		if n := len(env["data"].(map[string]any)["components"].([]any)); n != 2 {
			t.Errorf("%s: 组件数 %d", c, n)
		}
		h = newHarness(t, withState(src))
		h.run(c)
		if !strings.Contains(h.stdout.String(), "node") || !strings.Contains(h.stdout.String(), "1.0.0") {
			t.Errorf("%s 人类输出缺内容: %s", c, h.stdout)
		}
	}
}

// AC1：未实现命令保持 Hidden（--help 不列出），被调用也不返回假成功；已交付命令可见。
func TestHiddenCommandsAndNoFakeSuccess(t *testing.T) {
	h := newHarness(t)
	if code := h.run("--help"); code != 0 {
		t.Fatalf("退出码 %d", code)
	}
	out := h.stdout.String()
	for _, vis := range []string{"version", "list", "status", "install", "recover"} { // install/recover 自 Story 1.33 起已交付
		if !regexp.MustCompile(`(?m)^\s+` + vis + `\s`).MatchString(out) {
			t.Errorf("--help 应列出已交付命令 %s:\n%s", vis, out)
		}
	}
	for _, hid := range []string{"update", "service", "logs", "doctor", "config", "node", "backup", "rollback", "uninstall"} {
		if regexp.MustCompile(`(?m)^\s+` + hid + `\s`).MatchString(out) {
			t.Errorf("未实现命令 %s 不应出现在 --help", hid)
		}
	}
	// 全部未实现的叶子命令（root 身份、满足其校验与确认）都不得以 0 退出。
	for _, args := range [][]string{
		{"update", "node"}, {"update", "check"}, {"update", "self"}, {"service", "status"}, {"service", "start", "node"},
		{"logs", "node"}, {"doctor"}, {"config", "validate"}, {"config", "show", "server"}, {"config", "edit", "server"}, {"config", "setup-token"},
		{"node", "enroll"}, {"backup", "create"}, {"backup", "list"}, {"backup", "verify", "f"}, {"backup", "restore", "f", "--yes"},
		{"rollback", "node", "--yes"}, {"uninstall", "node", "--yes"},
		{"version", "--check"}, {"list", "--available"},
	} {
		h := newHarness(t, asRoot())
		code := h.run(append(args, "--json", "--yes")...)
		if code == 0 {
			t.Errorf("%v 未实现却以 0 退出（假成功）", args)
			continue
		}
		if got := errCode(h.envelope()); got != "cli.not_implemented" {
			t.Errorf("%v: errors.code=%q，期望 cli.not_implemented\n%s", args, got, h.stdout)
		}
	}
	// 纯分组命令：缺子命令/未知子命令是用法错误。
	for _, args := range [][]string{{"service"}, {"service", "bogus"}, {"backup"}, {"config"}, {"node"}} {
		h := newHarness(t)
		if code := h.run(append(args, "--json")...); code != 2 {
			t.Errorf("%v 退出码 %d，期望 2", args, code)
		}
	}
	// 已隐藏的命令仍可 --help 查看用法。
	h = newHarness(t)
	if code := h.run("install", "--help"); code != 0 || !strings.Contains(h.stdout.String(), "--offline") {
		t.Errorf("install --help 应可用: %d %s", code, h.stdout)
	}
}

// 根命令无参数：非 TTY 打印帮助且退出码 0（AC1/§3）；TTY 下菜单未交付，给出明确警告而非假装菜单。
func TestRootWithoutArgs(t *testing.T) {
	h := newHarness(t)
	if code := h.run(); code != 0 || !strings.Contains(h.stdout.String(), "Available Commands") {
		t.Fatalf("非 TTY 应打印帮助且退出 0: %d\n%s", code, h.stdout)
	}
	h = newHarness(t, withTTY())
	if code := h.run(); code != 0 || !strings.Contains(h.stderr.String(), "Story 1.44") {
		t.Fatalf("TTY：应提示菜单尚未交付: %d\n%s", code, h.stderr)
	}
}

// 全局标志：枚举校验、--config 路径、--log-level。
func TestGlobalFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"list", "--log-level", "trace"},
		{"list", "--progress", "xml"},
		{"list", "--config", "/definitely/not/here.yaml"},
		{"list", "--log-file"}, // 缺值
	} {
		h := newHarness(t)
		if code := h.run(args...); code != 2 {
			t.Errorf("%v 退出码 %d，期望 2\n%s", args, code, h.stderr)
		}
	}
	h := newHarness(t)
	cfg := filepath.Join(h.dir, "tools.yaml")
	if err := os.WriteFile(cfg, []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run("list", "--config", cfg); code != 0 {
		t.Errorf("存在的 --config 应通过: %d %s", code, h.stderr)
	}
}

// 颜色：--no-color / NO_COLOR / TERM=dumb / 非 TTY 一律不含 ANSI；TTY 下错误符号着色。
func TestColorRules(t *testing.T) {
	ansi := "\x1b["
	h := newHarness(t, withTTY())
	h.run("install", "nope")
	if !strings.Contains(h.stderr.String(), ansi) {
		t.Errorf("TTY 下应着色: %q", h.stderr.String())
	}
	for name, c := range map[string]struct {
		opts []hopt
		args []string
	}{
		"--no-color":   {[]hopt{withTTY()}, []string{"install", "nope", "--no-color"}},
		"NO_COLOR":     {[]hopt{withTTY(), withEnv(map[string]string{"NO_COLOR": "1"})}, []string{"install", "nope"}},
		"TERM=dumb":    {[]hopt{withTTY(), withEnv(map[string]string{"TERM": "dumb"})}, []string{"install", "nope"}},
		"stderr 非 TTY": {nil, []string{"install", "nope"}},
	} {
		h := newHarness(t, c.opts...)
		h.run(c.args...)
		if strings.Contains(h.stderr.String(), ansi) || !strings.Contains(h.stderr.String(), "✗") {
			t.Errorf("%s: 不应着色但保留符号: %q", name, h.stderr.String())
		}
	}
}

// 人类错误版式（cli-tui §4.1）：发生了什么/怎么办/系统状态/退出码+错误码/运行 ID。
func TestHumanErrorLayout(t *testing.T) {
	h := newHarness(t)
	h.run("install", "nope", "--log-file", filepath.Join(h.dir, "t.log"))
	out := h.stderr.String()
	for _, want := range []string{"✗ install 失败", "发生了什么", "怎么办", "系统状态", "未做任何修改", "退出码 2", "usage.invalid_component", "USAGE", "运行 ID: " + testRunID, "t.log"} {
		if !strings.Contains(out, want) {
			t.Errorf("人类错误输出缺少 %q:\n%s", want, out)
		}
	}
}

func bufioReader(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }
