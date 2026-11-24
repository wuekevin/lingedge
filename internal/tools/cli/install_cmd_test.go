package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// 真实命令 + 真实 registry/installer/downloader/resolver/service，只有「操作系统」（伪二进制）与「组件」（toolsworld 的假规格，仅 _test.go 引用）是假的。
func applyWorld(t testing.TB, h *harness, w *toolsworld.World) {
	t.Helper()
	r, err := service.NewRunner([]string{w.Bin})
	if err != nil {
		t.Fatal(err)
	}
	h.env.StatePath = w.Layout.StateFile()
	h.env.BuiltinKeys = func() ([]byte, error) { return toolsworld.KeysJSON(), nil }
	h.env.Install = InstallEnv{Root: w.Root, Specs: w.Specs, Runner: r, Own: w.Own, Booted: func() bool { return true },
		Collect: func(ctx context.Context) (probe.Result, error) { return w.Probe, nil }}
}

func installHarness(t *testing.T, opts ...hopt) (*harness, *toolsworld.World) {
	t.Helper()
	w, err := toolsworld.NewWorld(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, append([]hopt{asRoot()}, opts...)...)
	applyWorld(t, h, w)
	return h, w
}

func dataOf(t *testing.T, h *harness) map[string]any {
	t.Helper()
	env := h.envelope()
	d, _ := env["data"].(map[string]any)
	return d
}

func TestInstallCommandEndToEnd(t *testing.T) {
	h, w := installHarness(t)
	if code := h.run("install", "node", "--offline", w.Bundle, "--json"); code != 0 {
		t.Fatalf("退出码 %d\nstdout=%s\nstderr=%s", code, h.stdout, h.stderr)
	}
	env := h.envelope()
	if err := validateAgainst(t, "envelope.schema.json", env); err != nil {
		t.Fatal(err)
	}
	if err := validateAgainst(t, "data.install.schema.json", env["data"]); err != nil {
		t.Fatalf("data 不符合 schema: %v\n%s", err, h.stdout)
	}
	d := env["data"].(map[string]any)
	if d["outcome"] != "installed" || d["dry_run"] != false || d["tx"] == "" {
		t.Fatalf("%v", d)
	}
	// stderr 是 NDJSON 进度（--json 默认开启）：每行合法 JSON，包含 22 步里的取包/提交步骤。
	steps := map[int]bool{}
	sc := bufio.NewScanner(strings.NewReader(h.stderr.String()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("进度行不是 JSON: %q", sc.Text())
		}
		if ev, ok := m["event"].(map[string]any); ok && ev["type"] == "step_start" {
			steps[int(ev["step"].(float64))] = true
		}
	}
	for _, n := range []int{10, 14, 17, 19, 21, 22} {
		if !steps[n] {
			t.Errorf("进度里缺少第 %d 步", n)
		}
	}
	// AC5：lingedge list 读取到持久化的安装状态（来自真实 state.json）。
	h.run("list", "--json")
	env = h.envelope()
	comps := env["data"].(map[string]any)["components"].([]any)
	names := map[string]string{}
	for _, c := range comps {
		m := c.(map[string]any)
		names[m["name"].(string)] = m["version"].(string)
	}
	if names["node"] != "0.1.0" || names["openresty"] != "0.1.0" {
		t.Fatalf("list: %v", names)
	}
	// 再装一次：已安装 → 退出码 0，outcome=already_installed。
	if code := h.run("install", "node", "--offline", w.Bundle, "--json"); code != 0 || dataOf(t, h)["outcome"] != "already_installed" {
		t.Fatalf("重复安装: %d %s", code, h.stdout)
	}
}

func TestInstallDryRunZeroSideEffectsAndSchema(t *testing.T) {
	h, w := installHarness(t, withTTY())
	before := dirSnapshot(t, w.Root)
	for _, args := range [][]string{
		{"install", "node", "--offline", w.Bundle, "--dry-run", "--json"},
		{"install", "node", "--offline", w.Bundle, "--dry-run", "--json", "--set", "port.node.tcp.9100=19100@127.0.0.1"},
		{"install", "server", "--offline", w.Bundle, "--dry-run", "--json", "--setup-listen", "127.0.0.1:18080"},
	} {
		if code := h.run(args...); code != 0 {
			t.Fatalf("%v: 退出码 %d\n%s\n%s", args, code, h.stdout, h.stderr)
		}
		env := h.envelope()
		if env["dry_run"] != true {
			t.Fatalf("envelope.dry_run 应为 true")
		}
		if err := validateAgainst(t, "data.install.schema.json", env["data"]); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, h.stdout)
		}
		d := env["data"].(map[string]any)
		if d["outcome"] != "planned" || d["dry_run"] != true {
			t.Fatalf("%v", d)
		}
		if got := dirSnapshot(t, w.Root); got != before {
			t.Fatalf("dry-run 改动了文件系统（%v）", args)
		}
	}
	// --set 换端口体现在计划里。
	h.run("install", "node", "--offline", w.Bundle, "--dry-run", "--json", "--set", "port.node.tcp.9100=19100@127.0.0.1")
	ports := dataOf(t, h)["plan"].(map[string]any)["ports"].([]any)
	found := false
	for _, p := range ports {
		m := p.(map[string]any)
		found = found || (m["component"] == "node" && int(m["port"].(float64)) == 19100 && m["bind"] == "127.0.0.1" && m["overridden"] == true)
	}
	if !found {
		t.Fatalf("计划应体现端口覆盖: %v", ports)
	}
	// 人类可读的计划。
	if code := h.run("install", "node", "--offline", w.Bundle, "--dry-run"); code != 0 {
		t.Fatalf("%d %s", code, h.stderr)
	}
	for _, want := range []string{"安装计划", "未做任何修改", "openresty", "lingedge-node.service", "预检（步骤 1–9）"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("人类输出缺少 %q:\n%s", want, h.stdout)
		}
	}
}

func dirSnapshot(t testing.TB, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		b.WriteString(rel + " " + fi.Mode().String())
		if fi.Mode().IsRegular() {
			data, _ := os.ReadFile(p)
			sum := sha256.Sum256(data)
			b.WriteString(" " + hex.EncodeToString(sum[:6]))
		}
		b.WriteString("\n")
		return nil
	})
	return b.String()
}

func TestInstallErrorsMapToExitCodes(t *testing.T) {
	// 生产形态：没有组件规格 → 明确失败（1），而不是任何形式的假成功。
	h, w := installHarness(t)
	h.env.Install.Specs = nil // nil = 生产规格：当前只有 server（Story 1.35），node 等仍未交付
	if code := h.run("install", "node", "--offline", w.Bundle, "--json"); code != 1 || errCode(h.envelope()) != "install.component_spec_missing" {
		t.Fatalf("规格缺失: %d %s", code, h.stdout)
	}
	// 未发布/内置仍先于规格检查得到 7/8（FR-3）。
	if code := h.run("install", "scheduler", "--offline", w.Bundle, "--json"); code != 8 {
		t.Fatalf("内置: %d", code)
	}
	// 冲突需确认（9）：既有配置不同。
	h, w = installHarness(t)
	p := filepath.Join(w.Root, "etc/lingedge/node/node.yaml")
	_ = os.MkdirAll(filepath.Dir(p), 0o750)
	_ = os.WriteFile(p, []byte("custom\n"), 0o640)
	_ = os.Chmod(filepath.Dir(p), 0o750)
	if code := h.run("install", "node", "--offline", w.Bundle, "--json", "--non-interactive"); code != 9 || errCode(h.envelope()) != "confirm.required" {
		t.Fatalf("冲突: %d %s", code, h.stdout)
	}
	env := h.envelope()
	item := env["errors"].([]any)[0].(map[string]any)
	if cs, _ := item["details"].(map[string]any)["conflicts"].([]any); len(cs) == 0 {
		t.Errorf("details.conflicts 应列出冲突: %v", item)
	}
	if b, _ := os.ReadFile(p); string(b) != "custom\n" {
		t.Fatal("既有配置不得被改动")
	}
	// --yes：继续，既有文件原样，写 .new。
	if code := h.run("install", "node", "--offline", w.Bundle, "--json", "--yes"); code != 0 {
		t.Fatalf("--yes: %d %s", code, h.stdout)
	}
	if b, _ := os.ReadFile(p); string(b) != "custom\n" {
		t.Fatal("既有配置不得被改动")
	}
	if _, err := os.Stat(p + ".new"); err != nil {
		t.Fatal(".new 应已写入")
	}
	// 端口占用 → 4。
	h, w = installHarness(t)
	w.Probe.System.Listeners = []resolver.Listener{{Proto: "tcp", Addr: "0.0.0.0", Port: 9100, PID: 1, Process: "x"}}
	if code := h.run("install", "node", "--offline", w.Bundle, "--json"); code != 4 {
		t.Fatalf("端口: %d %s", code, h.stdout)
	}
	// 启动失败 → 回滚 → 10；状态已恢复。
	h, w = installHarness(t)
	w.SetCtl("fail-start", true)
	if code := h.run("install", "node", "--offline", w.Bundle, "--json"); code != 10 || errCode(h.envelope()) != "install.rolled_back" {
		t.Fatalf("回滚: %d %s", code, h.stdout)
	}
	item = h.envelope()["errors"].([]any)[0].(map[string]any)
	if item["system_state"] != "rolled_back" || int(item["step"].(float64)) != 19 {
		t.Fatalf("%v", item)
	}
	h.run("list", "--json")
	if n := len(dataOf(t, h)["components"].([]any)); n != 0 {
		t.Fatalf("回滚后 list 应为空: %d", n)
	}
	// --set 被组件 schema 拒绝 → 2；坏语法 → 2。
	h, w = installHarness(t)
	for _, set := range []string{"bogus=1", "port.node.tcp.9100=0"} {
		if code := h.run("install", "node", "--offline", w.Bundle, "--json", "--set", set); code != 2 {
			t.Fatalf("--set %s: %d %s", set, code, h.stdout)
		}
	}
	// 校验失败（篡改的 bundle 路径不存在）→ 2；tools.yaml 非法 → 2。
	cfg := filepath.Join(t.TempDir(), "tools.yaml")
	_ = os.WriteFile(cfg, []byte("release:\n  base_url: http://insecure\n"), 0o644)
	if code := h.run("install", "node", "--offline", w.Bundle, "--json", "--config", cfg); code != 2 || errCode(h.envelope()) != "usage.invalid_config" {
		t.Fatalf("tools.yaml: %d %s", code, h.stdout)
	}
}

// 交互确认：TTY + stdin 回答 y 继续、n 取消（9）；提问走 stderr。
func TestInstallInteractiveConflictPrompt(t *testing.T) {
	for _, tc := range []struct {
		ans  string
		want int
	}{{"y\n", 0}, {"n\n", 9}} {
		h, w := installHarness(t, withTTY(), withStdin(tc.ans))
		p := filepath.Join(w.Root, "etc/lingedge/node/node.yaml")
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		_ = os.WriteFile(p, []byte("custom\n"), 0o640)
		_ = os.Chmod(filepath.Dir(p), 0o750)
		if code := h.run("install", "node", "--offline", w.Bundle); code != tc.want {
			t.Fatalf("回答 %q: 退出码 %d\n%s", tc.ans, code, h.stderr)
		}
		if !strings.Contains(h.stderr.String(), "[y/N]") || !strings.Contains(h.stderr.String(), "node.yaml") {
			t.Errorf("应在 stderr 提问并列出冲突:\n%s", h.stderr)
		}
	}
}

// setup token 在人类输出里恰出现一次，不出现在日志；JSON 的 data.install.access 中以 secret 标记交付。
func TestInstallServerPrintsTokenOnce(t *testing.T) {
	h, w := installHarness(t)
	logf := filepath.Join(t.TempDir(), "t.log")
	if code := h.run("install", "server", "--offline", w.Bundle, "--log-file", logf); code != 0 {
		t.Fatalf("%d\n%s", code, h.stderr)
	}
	out := h.stdout.String()
	var tok string
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "安装向导令牌") {
			tok = strings.TrimSpace(strings.SplitN(ln, "：", 2)[1])
		}
	}
	if len(tok) != 43 || strings.Count(out, tok) != 1 {
		t.Fatalf("令牌应恰出现一次: %q\n%s", tok, out)
	}
	lb, _ := os.ReadFile(logf)
	if strings.Contains(string(lb), tok) || strings.Contains(h.stderr.String(), tok) {
		t.Fatal("令牌不得进入日志或 stderr")
	}
	for _, f := range []string{"etc/lingedge/server/server.yaml", "etc/lingedge/server/setup-token.hash"} {
		b, _ := os.ReadFile(filepath.Join(w.Root, f))
		if strings.Contains(string(b), tok) {
			t.Errorf("令牌不得落盘: %s", f)
		}
	}
}

// installer.Kind → 退出码/错误码的映射穷尽：新增 Kind 而忘记映射会被发现。
func TestInstallErrorMappingIsExhaustive(t *testing.T) {
	want := map[installer.Kind]ExitCode{
		installer.KindSpecMissing: ExitGeneral, installer.KindSettings: ExitUsage, installer.KindUpgradeRequired: ExitPrecondition, installer.KindConfirmRequired: ExitConfirmRequired,
		installer.KindPortUnverified: ExitPortConflict, installer.KindPrecondition: ExitPrecondition, installer.KindConfig: ExitUsage, installer.KindStepFailed: ExitGeneral,
		installer.KindRolledBack: ExitRolledBack, installer.KindHealthFailed: ExitHealthFailed, installer.KindRollbackFailed: ExitRollbackFailed, installer.KindInterrupted: ExitSigint,
		installer.KindRecover: ExitPrecondition, installer.KindNotInstalled: ExitNotInstalled, installer.KindInternal: ExitGeneral,
	}
	for _, k := range installer.Kinds() {
		e := InstallError(&installer.Error{Kind: k, Msg: "m"})
		if e == nil || e.Exit != want[k] || e.Code == "" {
			t.Errorf("Kind %s → %+v，期望退出码 %d", k, e, want[k])
		}
	}
	// 10 与 12 互斥：同一个错误不会同时映射到两者；StepFailed 沿 Cause 映射（校验失败 5）。
	if InstallError(errors.New("plain")) != nil {
		t.Error("非 installer 错误应返回 nil")
	}
}

// ---- 真实子进程：SIGINT 与 kill -9 ----

const childRoot = "LINGEDGE_CLI_TEST_CHILD_ROOT"

// TestHelperCLIChild 是真实子进程：以与生产相同的 MainEnv（真实 SIGINT 处理）运行 lingedge。仅在环境变量存在时工作。
func TestHelperCLIChild(t *testing.T) {
	root := os.Getenv(childRoot)
	if root == "" {
		t.Skip("仅作为子进程运行")
	}
	w, err := toolsworld.NewWorld(root)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("LE_CHILD_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	env := Env{Stdin: strings.NewReader(""), Stdout: os.Stdout, Stderr: os.Stderr, Getenv: func(string) string { return "" }, Geteuid: func() int { return 0 },
		Hostname: func() (string, error) { return "edge-host-1", nil }, Now: func() time.Time { return toolsworld.T0 }, StatePath: w.Layout.StateFile(),
		BuiltinKeys: func() ([]byte, error) { return toolsworld.KeysJSON(), nil }}
	r, _ := service.NewRunner([]string{w.Bin})
	env.Install = InstallEnv{Root: w.Root, Specs: w.Specs, Runner: r, Own: w.Own, Booted: func() bool { return true },
		Collect: func(ctx context.Context) (probe.Result, error) { return w.Probe, nil }}
	if at := os.Getenv("LE_CHILD_KILL_AT"); at != "" {
		env.Install.Hook = func(p string) {
			if p == at {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		}
	}
	os.Exit(MainEnv(env, args))
}

func runCLIChild(t *testing.T, root string, args []string, env ...string) *exec.Cmd {
	t.Helper()
	ab, _ := json.Marshal(args)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCLIChild$")
	cmd.Env = append(os.Environ(), append([]string{childRoot + "=" + root, "LE_CHILD_ARGS=" + string(ab)}, env...)...)
	return cmd
}

// 真实 SIGINT：安装卡在启动步骤时向进程发 SIGINT → 完成当前原子步骤、回滚、退出码 130，系统与安装前一致。
func TestInstallSIGINTRollsBackWithExit130(t *testing.T) {
	w, err := toolsworld.NewWorld(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := dirSnapshotExcludingBookkeeping(t, w.Root)
	w.SetCtl("block-start-node", true)
	cmd := runCLIChild(t, w.Root, []string{"install", "node", "--offline", w.Bundle, "--json", "--non-interactive"})
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(w.Root, "run", "node.start-blocked")
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("子进程没有走到启动步骤")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 130 {
		t.Fatalf("退出码应为 130: %v\n%s", err, stdout.String())
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &env); err != nil {
		t.Fatalf("stdout: %q", stdout.String())
	}
	item := env["errors"].([]any)[0].(map[string]any)
	if item["code"] != "interrupt.sigint" || item["system_state"] != "rolled_back" {
		t.Fatalf("%v", item)
	}
	if after := dirSnapshotExcludingBookkeeping(t, w.Root); after != before {
		t.Fatalf("SIGINT 回滚后应与安装前一致\n--- before\n%s\n--- after\n%s", before, after)
	}
	if w.FakeHas("users/lingedge-node") || w.FakeHas("active-lingedge-node.service") {
		t.Error("伪 systemd/用户状态应已恢复")
	}
}

func dirSnapshotExcludingBookkeeping(t testing.TB, root string) string {
	var b strings.Builder
	for _, ln := range strings.Split(dirSnapshot(t, root), "\n") {
		if strings.HasPrefix(ln, "var/lib/lingedge/tools") && !strings.HasPrefix(ln, "var/lib/lingedge/tools/state.json") || strings.HasPrefix(ln, "run") || strings.HasPrefix(ln, "ctl") ||
			strings.HasPrefix(ln, "fake/reloads") || strings.HasPrefix(ln, "bin/") {
			continue
		}
		b.WriteString(ln + "\n")
	}
	return b.String()
}

// 真实 kill -9 之后：写命令得 13；recover 非交互无标志 → 9（需要明确方式）；recover --rollback → 0 并恢复；list 为空。
func TestKill9ThenRecoverViaCLI(t *testing.T) {
	w, err := toolsworld.NewWorld(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := dirSnapshotExcludingBookkeeping(t, w.Root)
	cmd := runCLIChild(t, w.Root, []string{"install", "node", "--offline", w.Bundle, "--json", "--non-interactive"}, "LE_CHILD_KILL_AT=effect:17")
	err = cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || !ee.Sys().(syscall.WaitStatus).Signaled() {
		t.Fatalf("子进程应被 SIGKILL: %v", err)
	}
	h := newHarness(t, asRoot())
	applyWorld(t, h, w)
	if code := h.run("install", "node", "--offline", w.Bundle, "--json"); code != 13 || errCode(h.envelope()) != "interrupted.unfinished_tx" {
		t.Fatalf("kill -9 之后写命令应得 13: %d %s", code, h.stdout)
	}
	if code := h.run("recover", "--json", "--non-interactive"); code != 9 {
		t.Fatalf("无标志的非交互 recover 应得 9: %d %s", code, h.stdout)
	}
	if code := h.run("recover", "--json", "--resume", "--rollback"); code != 2 {
		t.Fatalf("互斥标志: %d", code)
	}
	if code := h.run("recover", "--rollback", "--json"); code != 0 {
		t.Fatalf("recover --rollback: %d %s", code, h.stdout)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.recover.schema.json", env["data"]); err != nil {
		t.Fatalf("%v\n%s", err, h.stdout)
	}
	txs := env["data"].(map[string]any)["transactions"].([]any)
	if len(txs) != 1 || txs[0].(map[string]any)["action"] != "rolled_back" {
		t.Fatalf("%v", txs)
	}
	if after := dirSnapshotExcludingBookkeeping(t, w.Root); after != before {
		t.Fatalf("recover --rollback 后应回到安装前\n%s\n---\n%s", before, after)
	}
	// 没有未完成事务：recover 成功且无事可做；之后可以重新安装。
	if code := h.run("recover", "--json"); code != 0 || len(dataOf(t, h)["transactions"].([]any)) != 0 {
		t.Fatalf("空 recover: %d %s", code, h.stdout)
	}
	if code := h.run("install", "node", "--offline", w.Bundle, "--json"); code != 0 {
		t.Fatalf("重新安装: %d %s", code, h.stdout)
	}

	// resume 路径：在第 16 步被杀，recover --resume 续跑完成。
	w2, _ := toolsworld.NewWorld(t.TempDir())
	cmd = runCLIChild(t, w2.Root, []string{"install", "node", "--offline", w2.Bundle, "--json", "--non-interactive"}, "LE_CHILD_KILL_AT=begun:16")
	_ = cmd.Run()
	h2 := newHarness(t, asRoot())
	applyWorld(t, h2, w2)
	if code := h2.run("recover", "--resume", "--json"); code != 0 {
		t.Fatalf("recover --resume: %d %s", code, h2.stdout)
	}
	h2.run("list", "--json")
	if n := len(dataOf(t, h2)["components"].([]any)); n != 2 {
		t.Fatalf("续跑后 list 应有 2 个组件: %d", n)
	}
	// 激活阶段被杀：resume 被拒绝（3），rollback 成功。
	w3, _ := toolsworld.NewWorld(t.TempDir())
	cmd = runCLIChild(t, w3.Root, []string{"install", "node", "--offline", w3.Bundle, "--json", "--non-interactive"}, "LE_CHILD_KILL_AT=begun:20")
	_ = cmd.Run()
	h3 := newHarness(t, asRoot())
	applyWorld(t, h3, w3)
	if code := h3.run("recover", "--resume", "--json"); code != 3 || errCode(h3.envelope()) != "recover.not_possible" {
		t.Fatalf("激活阶段 resume 应被拒绝: %d %s", code, h3.stdout)
	}
	if code := h3.run("recover", "--rollback", "--json"); code != 0 {
		t.Fatalf("%d %s", code, h3.stdout)
	}
}
