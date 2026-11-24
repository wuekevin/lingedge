package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

func TestSystemctlArgvShapes(t *testing.T) {
	e := newFakeEnv(t)
	s := e.systemctl(t)
	ctx := context.Background()
	if err := s.DaemonReload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enable(ctx, UnitNode, UnitOpenrestyPath, UnitUpgradePath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, UnitNode); err != nil {
		t.Fatal(err)
	}
	if err := s.Restart(ctx, UnitNode, UnitOpenresty); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, UnitNode); err != nil {
		t.Fatal(err)
	}
	if err := s.Disable(ctx, UnitUpgradePath); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl [daemon-reload]",
		"systemctl [is-enabled] [lingedge-node.service]",
		"systemctl [is-enabled] [lingedge-openresty.path]",
		"systemctl [is-enabled] [lingedge-node-upgrade.path]",
		"systemctl [enable] [lingedge-node.service] [lingedge-openresty.path] [lingedge-node-upgrade.path]",
		"systemctl [is-active] [lingedge-node.service]",
		"systemctl [start] [lingedge-node.service]",
		"systemctl [restart] [lingedge-node.service] [lingedge-openresty.service]",
		"systemctl [stop] [lingedge-node.service]",
		"systemctl [disable] [lingedge-node-upgrade.path]",
	}
	if got := e.calls(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv 不符\n得到 %q\n期望 %q", got, want)
	}
}

func TestSystemctlQueries(t *testing.T) {
	e := newFakeEnv(t)
	s := e.systemctl(t)
	ctx := context.Background()
	if st, err := s.IsActive(ctx, UnitNode); err != nil || st != "inactive" { // 非 0 退出码是状态而不是故障
		t.Fatalf("is-active：%q %v", st, err)
	}
	if st, err := s.IsEnabled(ctx, UnitNode); err != nil || st != "disabled" {
		t.Fatalf("is-enabled：%q %v", st, err)
	}
	e.set(t, "active-lingedge-node.service", "")
	e.set(t, "enabled-lingedge-node.service", "")
	if st, _ := s.IsActive(ctx, UnitNode); st != "active" {
		t.Errorf("is-active：%q", st)
	}
	if st, _ := s.IsEnabled(ctx, UnitNode); st != "enabled" {
		t.Errorf("is-enabled：%q", st)
	}
}

func TestSystemctlVersionParsesOutput(t *testing.T) {
	e := newFakeEnv(t)
	e.set(t, "version", readFile(t, "testdata/systemd/rhel8-239.txt"))
	v, err := e.systemctl(t).Version(context.Background())
	if err != nil || v.Major != 239 {
		t.Fatalf("%+v %v", v, err)
	}
	if got := e.calls(t); !reflect.DeepEqual(got, []string{"systemctl [--version]"}) {
		t.Errorf("argv：%q", got)
	}
	e.set(t, "version", "not systemd output\n")
	if _, err := e.systemctl(t).Version(context.Background()); err == nil {
		t.Error("无法识别的版本输出应失败")
	}
}

func TestSystemctlRejectsUnknownUnitsBeforeExec(t *testing.T) {
	e := newFakeEnv(t)
	s := e.systemctl(t)
	ctx := context.Background()
	for _, bad := range []UnitName{"", "ssh.service", "--now", "lingedge-node.service --now", "lingedge-node.service\nstart", "../x.service", "lingedge-server.service.new", "-a"} {
		if _, err := s.Enable(ctx, bad); KindOf(err) != KindInvalidParam {
			t.Errorf("Enable(%q) 应被拒绝：%v", bad, err)
		}
		if err := s.Stop(ctx, bad); KindOf(err) != KindInvalidParam {
			t.Errorf("Stop(%q) 应被拒绝：%v", bad, err)
		}
		if _, err := s.IsActive(ctx, bad); KindOf(err) != KindInvalidParam {
			t.Errorf("IsActive(%q) 应被拒绝：%v", bad, err)
		}
	}
	if _, err := s.Start(ctx); KindOf(err) != KindInvalidParam { // 空列表
		t.Errorf("无单元应被拒绝：%v", err)
	}
	if got := e.calls(t); len(got) != 0 {
		t.Errorf("被拒绝的请求不得触发任何进程：%q", got)
	}
}

func TestSystemctlRefusesOnNonSystemd(t *testing.T) {
	e := newFakeEnv(t)
	s := &Systemctl{Run: e.runner(t), Booted: func() bool { return false }}
	for name, err := range map[string]error{
		"reload":  s.DaemonReload(context.Background()),
		"stop":    s.Stop(context.Background(), UnitNode),
		"disable": s.Disable(context.Background(), UnitNode),
	} {
		if KindOf(err) != KindNotSystemd {
			t.Errorf("%s：%v", name, err)
		}
	}
	if _, err := s.Version(context.Background()); KindOf(err) != KindNotSystemd {
		t.Errorf("version：%v", err)
	}
	if len(e.calls(t)) != 0 {
		t.Error("非 systemd 环境不得执行任何进程")
	}
}

// 默认 Booted（平台实现）：非 Linux 上恒为 false → 明确的 not_systemd（macOS 上的真实降级行为）。
func TestSystemctlDefaultBootedPlatform(t *testing.T) {
	e := newFakeEnv(t)
	s := &Systemctl{Run: e.runner(t)}
	err := s.DaemonReload(context.Background())
	if systemdBooted() {
		t.Skip("本机由 systemd 启动：默认 Booted 为 true，真实行为属 ENV-LNX")
	}
	if KindOf(err) != KindNotSystemd {
		t.Fatalf("非 systemd 平台应明确失败：%v", err)
	}
}

func TestSystemctlExecFailureIsStructuredAndRedacted(t *testing.T) {
	e := newFakeEnv(t)
	e.set(t, "fail-enable", "")
	_, err := e.systemctl(t).Enable(context.Background(), UnitNode)
	if KindOf(err) != KindExec {
		t.Fatalf("%v", err)
	}
	if e.has("enabled-lingedge-node.service") {
		t.Error("失败的 enable 不应留下状态")
	}
	e.set(t, "fail-start", "")
	if _, err := e.systemctl(t).Start(context.Background(), UnitNode); KindOf(err) != KindExec {
		t.Fatalf("%v", err)
	}
}

func TestEnableAndStartUndoRecordCreated(t *testing.T) {
	e := newFakeEnv(t)
	s := e.systemctl(t)
	ctx := context.Background()
	e.set(t, "enabled-lingedge-node.service", "") // 用户此前就启用了 agent
	e.set(t, "active-lingedge-node.service", "")  // 且在运行
	en, err := s.Enable(ctx, UnitNode, UnitOpenrestyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(en) != 2 || en[0].Created || !en[1].Created || en[0].Action != ActSystemctlDisable || en[1].Path != "lingedge-openresty.path" {
		t.Errorf("enable 的 undo：%+v", en)
	}
	st, err := s.Start(ctx, UnitNode, UnitOpenresty)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 2 || st[0].Created || !st[1].Created || st[1].Action != ActSystemctlStop {
		t.Errorf("start 的 undo：%+v", st)
	}
	// 经 Undoer 回放：只撤销本事务做的。
	u := &Undoer{Sys: s}
	for _, a := range append(en, st...) {
		if _, err := u.Apply(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if !e.has("enabled-lingedge-node.service") || !e.has("active-lingedge-node.service") {
		t.Error("用户原有的启用/运行状态被撤销误伤")
	}
	if e.has("enabled-lingedge-openresty.path") || e.has("active-lingedge-openresty.service") {
		t.Error("本事务启用/启动的单元应被撤销")
	}
}

func TestAlreadyEnabledStates(t *testing.T) {
	for st, want := range map[string]bool{"enabled": true, "enabled-runtime": true, "static": true, "indirect": true, "disabled": false, "masked": false, "not-found": false, "linked": false} {
		if alreadyEnabled(st) != want {
			t.Errorf("alreadyEnabled(%q)", st)
		}
	}
}

func TestVerifyUnitsWiring(t *testing.T) {
	e := newFakeEnv(t)
	ctx := context.Background()
	r, err := VerifyUnits(ctx, e.runner(t), "/etc/systemd/system/lingedge-node.service", "/etc/systemd/system/lingedge-openresty.service")
	if err != nil || !r.Available || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	want := []string{"systemd-analyze [verify] [/etc/systemd/system/lingedge-node.service] [/etc/systemd/system/lingedge-openresty.service]"}
	if got := e.calls(t); !reflect.DeepEqual(got, want) {
		t.Errorf("argv：%q", got)
	}
	e.set(t, "verify-fail", "")
	r, err = VerifyUnits(ctx, e.runner(t), "/etc/systemd/system/lingedge-node.service")
	if err != nil || !r.Available || r.OK || r.Output == "" {
		t.Errorf("失败应带输出：%+v %v", r, err)
	}
	for _, bad := range []string{"relative.service", "/etc/x y.service", "/etc/../x", "/etc/a\nb", "--help"} {
		if _, err := VerifyUnits(ctx, e.runner(t), bad); KindOf(err) != KindInvalidParam {
			t.Errorf("路径 %q 应被拒绝：%v", bad, err)
		}
	}
	if _, err := VerifyUnits(ctx, e.runner(t)); err == nil {
		t.Error("无路径应失败")
	}
}

func TestVerifyUnitsUnavailable(t *testing.T) {
	r, err := NewRunner([]string{t.TempDir()}) // 空目录：没有 systemd-analyze
	if err != nil {
		t.Fatal(err)
	}
	res, err := VerifyUnits(context.Background(), r, "/etc/systemd/system/lingedge-node.service")
	if err != nil || res.Available {
		t.Fatalf("缺少 systemd-analyze 应表现为不可用（跳过）而不是错误：%+v %v", res, err)
	}
}

// Runner 只接受白名单内的程序：本包 Runner 无法被用来执行白名单外的命令。
func TestRunnerAllowlist(t *testing.T) {
	e := newFakeEnv(t)
	e.script(t, "rm", "#!/bin/sh\nexit 0\n")
	_, err := e.runner(t).Run(context.Background(), safeexec.Cmd{Path: "rm", Args: []string{"-rf", "/"}})
	if err == nil || !errors.Is(err, safeexec.ErrNotAllowed) {
		t.Fatalf("白名单外程序应被拒绝：%v", err)
	}
	if len(Allowed) != 6 {
		t.Errorf("白名单应恰为 6 项：%v", Allowed)
	}
}

// Show：固定属性集（--property=…），解析为 UnitProps；argv 里没有调用方可控的属性名。
func TestSystemctlShow(t *testing.T) {
	e := newFakeEnv(t)
	s := e.systemctl(t)
	ctx := context.Background()
	p, err := s.Show(ctx, UnitServer)
	if err != nil || p.ActiveState != "inactive" || p.LoadState != "loaded" || p.NRestarts != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	e.set(t, "show-lingedge-server.service", "LoadState=loaded\nActiveState=active\nSubState=running\nUnitFileState=enabled\nResult=success\nMainPID=4242\nNRestarts=3\nActiveEnterTimestamp=Tue 2026-11-24 12:00:00 UTC\nExtra=ignored\n")
	p, err = s.Show(ctx, UnitServer)
	if err != nil || p.ActiveState != "active" || p.MainPID != 4242 || p.NRestarts != 3 || p.UnitFileState != "enabled" || p.ActiveEnterTimestamp == "" {
		t.Fatalf("%+v %v", p, err)
	}
	got := e.calls(t)
	want := "systemctl [show] [--no-pager] [--property=" + showProps + "] [lingedge-server.service]"
	if got[len(got)-1] != want {
		t.Errorf("argv：%q", got[len(got)-1])
	}
	if _, err := s.Show(ctx, UnitName("evil.service")); err == nil {
		t.Error("未知单元必须在执行前被拒绝")
	}
	if _, err := (&Systemctl{Run: s.Run, Booted: func() bool { return false }}).Show(ctx, UnitServer); err == nil {
		t.Error("非 systemd 主机应拒绝")
	}
}

func TestParseShowRejectsGarbage(t *testing.T) {
	good := "LoadState=loaded\nActiveState=active\nSubState=running\nUnitFileState=enabled\nMainPID=1\nNRestarts=0\n"
	if _, err := parseShow(good); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"空输出":            "",
		"没有 ActiveState": "LoadState=loaded\n",
		"状态词含空白与分号":      "ActiveState=active; rm -rf /\n",
		"MainPID 非数字":    good + "MainPID=abc\n",
		"MainPID 为负":     good + "MainPID=-1\n",
		"NRestarts 巨大":   good + "NRestarts=99999999999\n",
		"时间戳含控制字符":       good + "ActiveEnterTimestamp=a\x01b\n",
		"状态词过长":          "ActiveState=" + strings.Repeat("a", 200) + "\n",
	} {
		if _, err := parseShow(in); err == nil {
			t.Errorf("%s：应被拒绝", name)
		}
	}
}

func TestRunUnitsMapping(t *testing.T) {
	if got := RunUnits("server"); len(got) != 1 || got[0] != UnitServer {
		t.Errorf("server：%v", got)
	}
	if got := RunUnits("node"); len(got) != 1 || got[0] != UnitNode {
		t.Errorf("node 的启停只作用于 Agent（数据面自治，ADR-020）：%v", got)
	}
	if len(StatusUnits("node")) != 4 || len(LogUnits("node")) != 2 {
		t.Errorf("node 的观察/日志单元：%v %v", StatusUnits("node"), LogUnits("node"))
	}
	for _, c := range []string{"admin-web", "console", "docs", "status", "website", "dns", "reporter", "nope"} {
		if RunUnits(c) != nil || StatusUnits(c) != nil || LogUnits(c) != nil {
			t.Errorf("%s 没有 Tools 管理的服务单元", c)
		}
	}
}
