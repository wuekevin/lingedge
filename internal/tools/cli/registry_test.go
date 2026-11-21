package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/registry"
)

const holdEnv = "LINGEDGE_CLI_TEST_HOLD_ROOT"

// TestHelperHoldLock 是真实子进程：以 registry.Begin 持有组件锁（可选再打开事务日志），等待被 kill -9。仅在环境变量存在时工作。
func TestHelperHoldLock(t *testing.T) {
	root := os.Getenv(holdEnv)
	if root == "" {
		t.Skip("仅作为子进程运行")
	}
	reg := registry.New(registry.Layout{Root: root}, registry.Options{RunID: "run-holder-1", Command: "update server"})
	task, err := reg.Begin(context.Background(), registry.TaskRequest{Components: strings.Split(os.Getenv("HOLD_COMPS"), ",")})
	if err != nil {
		_, _ = os.Stdout.WriteString("ERR " + err.Error() + "\n")
		os.Exit(2)
	}
	if os.Getenv("HOLD_TX") == "1" {
		if _, err := task.OpenTx(context.Background()); err != nil {
			_, _ = os.Stdout.WriteString("ERR " + err.Error() + "\n")
			os.Exit(2)
		}
	}
	_, _ = os.Stdout.WriteString("READY\n")
	for {
		time.Sleep(time.Hour)
	}
}

func startHolder(t *testing.T, root, comps string, tx bool) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")
	cmd.Env = append(os.Environ(), holdEnv+"="+root, "HOLD_COMPS="+comps)
	if tx {
		cmd.Env = append(cmd.Env, "HOLD_TX=1")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	sc := bufio.NewScanner(out)
	if !sc.Scan() || sc.Text() != "READY" {
		t.Fatalf("子进程未就绪: %q", sc.Text())
	}
	return cmd
}

func toolsRoot(h *harness) string { return filepath.Dir(h.env.StatePath) }

// AC3：同目标并发第二个任务 → 退出码 6 并输出持有者（pid/run_id/command）；kill -9 后立即可用（无陈旧锁）。
func TestWriteCommandLockedExit6AndKill9Releases(t *testing.T) {
	h := newHarness(t, asRoot())
	holder := startHolder(t, toolsRoot(h), "server", false)

	for _, args := range [][]string{{"install", "server"}, {"service", "restart", "server"}, {"update", "server"}, {"rollback", "server"}, {"uninstall", "server"}} {
		if code := h.run(append(args, "--json", "--non-interactive", "--yes")...); code != 6 {
			t.Fatalf("%v: 退出码 %d，期望 6\n%s", args, code, h.stdout)
		}
		env := h.envelope()
		if errCode(env) != "locked.task" || env["exit_name"] != "LOCKED" {
			t.Fatalf("%v: %v", args, env)
		}
		d := env["errors"].([]any)[0].(map[string]any)["details"].(map[string]any)
		holderInfo := d["holder"].(map[string]any)
		if int(holderInfo["pid"].(float64)) != holder.Process.Pid || holderInfo["run_id"] != "run-holder-1" || holderInfo["command"] != "update server" || d["lock"] != "server" {
			t.Fatalf("%v: 持有者信息 %v，期望 pid=%d", args, d, holder.Process.Pid)
		}
	}
	// 人类输出同样给出持有者与错误码。
	if code := h.run("install", "server", "--non-interactive"); code != 6 || !strings.Contains(h.stderr.String(), "run-holder-1") || !strings.Contains(h.stderr.String(), "LOCKED") {
		t.Fatalf("人类输出: %d %s", code, h.stderr)
	}
	// 不同组件不受影响（走到「尚未实现」= 1，而不是 6）。Story 1.33 起 install 已实现，改用仍是桩的 update 作为「写命令桩」。
	if code := h.run("update", "node", "--non-interactive"); code != 1 {
		t.Fatalf("不同组件应不被锁拦：%d", code)
	}
	// 读命令永不取锁。
	for _, c := range []string{"list", "status", "version"} {
		if code := h.run(c, "--json"); code != 0 {
			t.Errorf("%s 被锁拦住了: %d", c, code)
		}
	}
	// --dry-run 不写任何持久状态，不取锁。
	if code := h.run("update", "server", "--plan-only", "--non-interactive"); code == 6 {
		t.Errorf("--plan-only 不应取锁")
	}

	if err := holder.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	if code := h.run("update", "server", "--non-interactive"); code != 1 {
		t.Fatalf("kill -9 之后锁应已被内核释放（期望走到尚未实现=1），得到 %d\n%s", code, h.stderr)
	}
}

// AC4：存在已死亡且未结束的事务 → 写命令退出码 13，提示 lingedge recover；recover 本身与读命令不受影响。
func TestWriteCommandUnfinishedExit13(t *testing.T) {
	h := newHarness(t, asRoot())
	holder := startHolder(t, toolsRoot(h), "node", true) // 持有 node 并打开了 journal
	// 事务仍活着：另一个组件的写命令不被拦（它是合法的并行操作）。
	if code := h.run("update", "server", "--non-interactive"); code != 1 {
		t.Fatalf("活事务不应让无关组件的写命令得 13: %d", code)
	}
	if err := holder.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()

	for _, args := range [][]string{{"install", "server"}, {"update", "--all"}, {"service", "stop", "docs"}, {"backup", "create"}} {
		if code := h.run(append(args, "--json", "--non-interactive", "--yes")...); code != 13 {
			t.Fatalf("%v: 退出码 %d，期望 13\n%s", args, code, h.stdout)
		}
		env := h.envelope()
		item := env["errors"].([]any)[0].(map[string]any)
		if errCode(env) != "interrupted.unfinished_tx" || env["exit_name"] != "INTERRUPTED" || !strings.Contains(item["hint"].(string), "lingedge recover") || item["system_state"] != "needs_manual" {
			t.Fatalf("%v: %v", args, env)
		}
		txs := item["details"].(map[string]any)["transactions"].([]any)
		if len(txs) != 1 || txs[0].(map[string]any)["status"] != "active" {
			t.Fatalf("%v: %v", args, txs)
		}
	}
	h.run("update", "server", "--non-interactive")
	if !strings.Contains(h.stderr.String(), "lingedge recover") {
		t.Errorf("人类输出应提示 lingedge recover: %s", h.stderr)
	}
	// recover 不被自己拦截（它是处理者）；Story 1.33 起它是真实命令：非交互且未给 --rollback/--resume → 9（需要明确的处理方式），而不是被 13 拦截。
	if code := h.run("recover", "--non-interactive"); code != 9 || errCode(func() map[string]any { h.run("recover", "--json", "--non-interactive"); return h.envelope() }()) != "confirm.required" {
		t.Errorf("recover 应走到其自身（9）而不是被 13 拦截")
	}
	for _, c := range []string{"list", "status", "version"} {
		if code := h.run(c); code != 0 {
			t.Errorf("%s: %d", c, code)
		}
	}
	// dry-run 不写状态，不拦。
	if code := h.run("update", "node", "--plan-only", "--non-interactive"); code == 13 {
		t.Errorf("--plan-only 不应被 13 拦")
	}
	// 处理（abort）之后放行。
	reg := registry.New(registry.Layout{Root: toolsRoot(h)}, registry.Options{})
	un, err := reg.Unfinished(context.Background())
	if err != nil || len(un) != 1 {
		t.Fatalf("%v %v", un, err)
	}
	rt, _, err := reg.BeginRecovery(context.Background(), un[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Journal().Abort("test"); err != nil {
		t.Fatal(err)
	}
	rt.Release()
	if code := h.run("update", "server", "--non-interactive"); code != 1 {
		t.Fatalf("事务结束后应放行（走到尚未实现=1）: %d", code)
	}
}

// 尚无状态目录（从未安装过）的机器：未实现的写命令保持 1.25 的行为，且不会为了「假装检查」创建目录。
func TestWriteCommandStubDoesNotCreateStateDir(t *testing.T) {
	h := newHarness(t, asRoot())
	if code := h.run("update", "server", "--non-interactive"); code != 1 {
		t.Fatalf("退出码 %d", code)
	}
	if _, err := os.Stat(toolsRoot(h)); !os.IsNotExist(err) {
		t.Fatalf("未实现的命令不应创建状态目录: %v", err)
	}
}

// 非 root 先得 3，不会越过 root 检查去碰锁目录。
func TestNonRootNeverTouchesLocks(t *testing.T) {
	h := newHarness(t)
	if code := h.run("install", "server", "--non-interactive"); code != 3 {
		t.Fatalf("退出码 %d", code)
	}
	if _, err := os.Stat(toolsRoot(h)); !os.IsNotExist(err) {
		t.Fatal("非 root 不应创建状态目录")
	}
}

// 真实 registry 读取 state.json：list/status/version 的输出来自文件；形状符合 schema。
func TestListStatusFromRealRegistry(t *testing.T) {
	h := newHarness(t)
	reg := registry.New(registry.LayoutForStatePath(h.env.StatePath), registry.Options{})
	err := reg.UpdateState(context.Background(), func(s *registry.State) error {
		s.Components["server"] = registry.Component{Version: "1.2.3", State: "running", Unit: "lingedge-server.service", RunUser: "lingedge", Ports: []int{8080},
			ReleaseDir: "/opt/lingedge/server/releases/1.2.3", ConfigDir: "/etc/lingedge/server", DataDir: "/var/lib/lingedge/server", InstalledAt: "2026-11-21T12:00:00.000Z", PrevVersion: "1.2.2"}
		s.Components["docs"] = registry.Component{Version: "1.0.0", State: "installed"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"version", "list", "status"} {
		if code := h.run(c, "--json"); code != 0 {
			t.Fatalf("%s 退出码 %d\n%s", c, code, h.stdout)
		}
		env := h.envelope()
		if err := validateAgainst(t, "data."+c+".schema.json", env["data"]); err != nil {
			t.Errorf("%s: %v", c, err)
		}
		comps := env["data"].(map[string]any)["components"].([]any)
		if len(comps) != 2 || comps[0].(map[string]any)["name"] != "docs" || comps[1].(map[string]any)["name"] != "server" || comps[1].(map[string]any)["version"] != "1.2.3" {
			t.Errorf("%s: %v", c, comps)
		}
	}
	h.run("list", "--json")
	d := h.envelope()["data"].(map[string]any)["components"].([]any)[0].(map[string]any)
	if ports, ok := d["ports"].([]any); !ok || len(ports) != 0 {
		t.Errorf("docs 的 ports 应是空数组: %v", d["ports"])
	}
	// 普通用户（本测试进程即非 root）可读：state.json 为 0644。
	if fi, err := os.Stat(h.env.StatePath); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("%v %v", fi, err)
	}
}

func TestStateErrorsMapToExitCodes(t *testing.T) {
	cases := []struct {
		name, content string
		perm          os.FileMode
		exit          int
		code          string
	}{
		{"未来 schema", `{"schema_version":3,"components":{}}`, 0o644, 3, "state.unsupported_schema"},
		{"损坏", `{"schema_version":1,`, 0o644, 1, "state.corrupt"},
		{"内容非法", `{"schema_version":1,"components":{"server":{"version":"x","state":"running","ports":[]}}}`, 0o644, 1, "state.invalid"},
		{"他人可写", `{"schema_version":1,"components":{}}`, 0o666, 3, "state.unsafe_permissions"},
	}
	for _, c := range cases {
		h := newHarness(t)
		if err := os.MkdirAll(filepath.Dir(h.env.StatePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.env.StatePath, []byte(c.content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(h.env.StatePath, c.perm); err != nil {
			t.Fatal(err)
		}
		for _, cmd := range []string{"list", "status", "version"} {
			if code := h.run(cmd, "--json"); code != c.exit {
				t.Fatalf("%s/%s: 退出码 %d，期望 %d", c.name, cmd, code, c.exit)
			}
			if got := errCode(h.envelope()); got != c.code {
				t.Errorf("%s/%s: errors.code=%q，期望 %q", c.name, cmd, got, c.code)
			}
		}
	}
}

// 错误 envelope 即使带 details 也是合法的单个 JSON 文档，且不泄漏口令（命令行里的口令在持有者信息里被脱敏）。
func TestHolderCommandRedactedInEnvelope(t *testing.T) {
	h := newHarness(t, asRoot())
	_ = startHolder(t, toolsRoot(h), "server", false)
	h.run("install", "server", "--set", "database.password=hunter2hunter2", "--json", "--non-interactive")
	var m map[string]any
	if err := json.Unmarshal(h.stdout.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.stdout.String(), "hunter2hunter2") {
		t.Fatalf("envelope 含口令: %s", h.stdout)
	}
}
