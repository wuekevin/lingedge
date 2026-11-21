package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/pkg/setuptoken"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.35 的命令层测试：server 的生产 ComponentSpec + toolsworld 的伪操作系统，经真实 cli.Execute。

func serverHarness(t *testing.T, opts ...hopt) (*harness, *toolsworld.World) {
	t.Helper()
	w, err := toolsworld.NewWorldWith(t.TempDir(), toolsworld.Options{ProductionServerSpec: true})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, append([]hopt{asRoot()}, opts...)...)
	applyWorld(t, h, w)
	return h, w
}

// AC4：8080/9180（metrics，127.0.0.1）/9443/9444 任一被占用 → 退出码 4（dry-run 与真实安装一致），且没有任何副作用。
func TestServerInstallPortOccupiedExit4(t *testing.T) {
	for _, c := range []struct {
		port int
		addr string
	}{{8080, "127.0.0.1"}, {9180, "127.0.0.1"}, {9443, "0.0.0.0"}, {9444, "0.0.0.0"}, {8080, "0.0.0.0"}} {
		for _, dry := range []bool{true, false} {
			h, w := serverHarness(t)
			w.Probe.System.Listeners = []resolver.Listener{{Proto: "tcp", Addr: c.addr, Port: c.port, Process: "nginx", PID: 4242}}
			args := []string{"install", "server", "--offline", w.Bundle, "--json"}
			if dry {
				args = append(args, "--dry-run")
			}
			if code := h.run(args...); code != 4 {
				t.Fatalf("端口 %s:%d 被占用（dry=%v）应退出码 4，实际 %d\n%s", c.addr, c.port, dry, code, h.stdout)
			}
			env := h.envelope()
			if got := errCode(env); !strings.HasPrefix(got, "port.") {
				t.Errorf("错误码应是 port.*: %q", got)
			}
			if !strings.Contains(h.stdout.String(), strconv.Itoa(c.port)) {
				t.Errorf("错误信息应指出端口 %d: %s", c.port, h.stdout)
			}
			for _, rel := range []string{"etc/lingedge/server", "opt/lingedge/server", "var/lib/lingedge/server", "etc/systemd/system/lingedge-server.service"} {
				if w.Has(rel) {
					t.Errorf("端口冲突不得留下 %s", rel)
				}
			}
		}
	}
	// 换端口避开占用后可以通过预检（--set port.server.tcp.9180=19180）。
	h, w := serverHarness(t)
	w.Probe.System.Listeners = []resolver.Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: 9180, Process: "prometheus"}}
	if code := h.run("install", "server", "--offline", w.Bundle, "--dry-run", "--set", "port.server.tcp.9180=19180", "--json"); code != 0 {
		t.Fatalf("换端口后应通过预检: %d\n%s", code, h.stdout)
	}
}

// AC4：非 root → 退出码 3，且没有任何副作用（不创建状态目录）。
func TestServerInstallRequiresRoot(t *testing.T) {
	h, w := serverHarness(t)
	h.env.Geteuid = func() int { return 1000 }
	if code := h.run("install", "server", "--offline", w.Bundle, "--json"); code != 3 || errCode(h.envelope()) != "precondition.root_required" {
		t.Fatalf("非 root 应退出码 3: %d\n%s", code, h.stdout)
	}
	if w.Has("var/lib/lingedge/tools") || w.Has("etc/lingedge") {
		t.Error("非 root 不得创建任何目录")
	}
}

// dry-run：真实 server 规格的计划展示端口、单元、文件、冲突、警告，不产生任何副作用；--set 换端口反映在计划与 SSH 提示之外的 server.yaml 里。
func TestServerInstallDryRunPlan(t *testing.T) {
	h, w := serverHarness(t)
	before := snapshotTree(t, w.Root)
	if code := h.run("install", "server", "--offline", w.Bundle, "--dry-run", "--setup-listen", "0.0.0.0:8080", "--json"); code != 0 {
		t.Fatalf("dry-run: %d\n%s", code, h.stdout)
	}
	d := dataOf(t, h)
	if err := validateAgainst(t, "data.install.schema.json", d); err != nil {
		t.Fatal(err)
	}
	if after := snapshotTree(t, w.Root); after != before {
		t.Error("dry-run 不得有任何副作用")
	}
	out := h.stdout.String()
	for _, want := range []string{"setup.listen_exposed", "external.remote_assumed", "lingedge-server.service", "master.key", "setup-token.hash"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run 输出应含 %q", want)
		}
	}
	if strings.Contains(out, `"kind":"setup_token"`) {
		t.Error("dry-run 不得生成/输出 token")
	}
}

func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			rel, _ := filepath.Rel(root, p)
			b.WriteString(rel + " " + fi.Mode().String() + " " + strconv.FormatInt(fi.Size(), 10) + "\n")
		}
		return nil
	})
	return b.String()
}

// 安装（--skip-start：只物化；健康检查/启动由 installer 的测试与 tests/integration/toolsinstall 覆盖）后：list 显示 server 的向导进展（AC6），
// config setup-token --reset 重置令牌（F-8），install.lock 出现后 list 显示已初始化、setup-token 拒绝。
func TestServerListAndSetupTokenReset(t *testing.T) {
	h, w := serverHarness(t)
	if code := h.run("install", "server", "--offline", w.Bundle, "--skip-start", "--json"); code != 0 {
		t.Fatalf("install: %d\n%s", code, h.stdout)
	}
	var tok1 string
	for _, a := range dataOf(t, h)["access"].([]any) {
		if m := a.(map[string]any); m["kind"] == "setup_token" {
			tok1 = m["value"].(string)
		}
	}
	hp := filepath.Join(w.Root, "etc/lingedge/server/setup-token.hash")
	if !setuptoken.Verify(tok1, string(mustRead(t, hp))) {
		t.Fatalf("--json 交付的 token 必须是真实令牌（与 setup-token.hash 对应），而不是被脱敏的 %q", tok1)
	}

	// list：向导未完成。
	listPhase := func() (string, string) {
		if code := h.run("list", "--json"); code != 0 {
			t.Fatalf("list: %d %s", code, h.stdout)
		}
		env := h.envelope()
		d := env["data"].(map[string]any)
		if err := validateAgainst(t, "data.list.schema.json", d); err != nil {
			t.Fatalf("list 输出不符合 schema: %v", err)
		}
		c := d["components"].([]any)[0].(map[string]any)
		s := c["setup"].(map[string]any)
		return s["phase"].(string), c["state"].(string)
	}
	if ph, st := listPhase(); ph != "wizard_pending" || st != "installed" {
		t.Fatalf("list: phase=%q state=%q", ph, st)
	}
	if code := h.run("list"); code != 0 || !strings.Contains(h.stdout.String(), "setup mode：安装向导未完成") {
		t.Errorf("list 的人类可读输出应说明向导未完成:\n%s", h.stdout)
	}

	// setup-token：必须 --reset；非 root → 3；无 --yes 的非交互 → 9。
	if code := h.run("config", "setup-token", "--json"); code != 2 {
		t.Errorf("缺 --reset 应 2: %d", code)
	}
	h.env.Geteuid = func() int { return 1000 }
	if code := h.run("config", "setup-token", "--reset", "--yes", "--json"); code != 3 {
		t.Errorf("非 root 应 3: %d", code)
	}
	h.env.Geteuid = func() int { return 0 }
	if code := h.run("config", "setup-token", "--reset", "--json"); code != 9 {
		t.Errorf("非交互未 --yes 应 9: %d\n%s", code, h.stdout)
	}
	h1 := mustRead(t, hp)
	logf := filepath.Join(t.TempDir(), "tools.log")
	if code := h.run("config", "setup-token", "--reset", "--yes", "--json", "--log-file", logf); code != 0 {
		t.Fatalf("reset: %d\n%s", code, h.stdout)
	}
	env := h.envelope()
	d := env["data"].(map[string]any)
	if err := validateAgainst(t, "data.config-setup-token.schema.json", d); err != nil {
		t.Fatalf("schema: %v", err)
	}
	acc := d["access"].([]any)[0].(map[string]any)
	tok2, _ := acc["value"].(string)
	if len(tok2) != 43 || tok2 == tok1 || acc["one_time"] != true {
		t.Fatalf("新 token 不符: %v", acc)
	}
	if string(mustRead(t, hp)) == string(h1) || !setuptoken.Verify(tok2, string(mustRead(t, hp))) || setuptoken.Verify(tok1, string(mustRead(t, hp))) {
		t.Error("哈希文件应被替换：新 token 通过、旧 token 失效")
	}
	if lb, _ := os.ReadFile(logf); strings.Contains(string(lb), tok2) || strings.Contains(h.stderr.String(), tok2) {
		t.Error("token 不得进入日志或 stderr")
	}
	// 人类可读输出：显示一次。
	if code := h.run("config", "setup-token", "--reset", "--yes"); code != 0 || !strings.Contains(h.stdout.String(), "仅显示这一次") {
		t.Errorf("human 输出: %d\n%s", code, h.stdout)
	}

	// 向导完成（install.lock）后：list 显示已初始化；setup-token 拒绝（3）。
	if err := os.WriteFile(filepath.Join(w.Root, "var/lib/lingedge/server/install.lock"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ph, _ := listPhase(); ph != "initialized" {
		t.Errorf("install.lock 存在应为 initialized: %q", ph)
	}
	if code := h.run("list"); code != 0 || !strings.Contains(h.stdout.String(), "已初始化") {
		t.Errorf("list 的人类可读输出应显示已初始化:\n%s", h.stdout)
	}
	if code := h.run("config", "setup-token", "--reset", "--yes", "--json"); code != 3 {
		t.Errorf("向导已完成应拒绝（3）: %d\n%s", code, h.stdout)
	}
}

// 没有安装 server 时 setup-token 是前置条件失败（3），不创建任何文件。
func TestSetupTokenWithoutServer(t *testing.T) {
	h, w := serverHarness(t)
	if code := h.run("config", "setup-token", "--reset", "--yes", "--json"); code != 3 || errCode(h.envelope()) != "precondition.install" {
		t.Fatalf("%d %s", code, h.stdout)
	}
	if w.Has("etc/lingedge") {
		t.Error("不得创建配置目录")
	}
}

// 一次性 token 的还原只开在唯一的交付通道上：同形态的字符串出现在别处（kind 不符、缺 one_time、嵌套深处的普通字段、错误细节）仍被遮蔽。
func TestOneTimeTokenRestoreIsNarrow(t *testing.T) {
	tok, _, err := setuptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"access": []any{
		map[string]any{"kind": "setup_token", "one_time": true, "label": "x", "value": tok},
		map[string]any{"kind": "setup_token", "label": "x", "value": tok},
		map[string]any{"kind": "note", "one_time": true, "label": "x", "value": tok},
		map[string]any{"kind": "setup_token", "one_time": "true", "label": "x", "value": tok},
	}, "other": map[string]any{"value": tok, "kind": "setup_token"}}
	e := &Envelope{Data: data}
	e.sanitize()
	d := e.Data.(map[string]any)
	acc := d["access"].([]any)
	if acc[0].(map[string]any)["value"] != tok {
		t.Error("唯一的交付通道应还原真实 token")
	}
	for i := 1; i < 4; i++ {
		if acc[i].(map[string]any)["value"] == tok {
			t.Errorf("access[%d] 不是交付通道，token 必须被遮蔽", i)
		}
	}
	if d["other"].(map[string]any)["value"] == tok {
		t.Error("其他位置的 token 必须被遮蔽")
	}
}
