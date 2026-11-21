package resolver

import (
	"strings"
	"testing"
)

// 地址冲突矩阵（§10.4）：同地址冲突与通配冲突，IPv4/IPv6 各自及交叉。
func TestOverlapMatrix(t *testing.T) {
	type row struct {
		a, b string
		want bool
		kind string
	}
	for _, r := range []row{
		{"127.0.0.1", "127.0.0.1", true, "exact"},
		{"127.0.0.1", "127.0.0.2", false, ""},
		{"10.0.0.5", "10.0.0.5", true, "exact"},
		{"10.0.0.5", "10.0.0.6", false, ""},
		{"0.0.0.0", "127.0.0.1", true, "wildcard"}, // 我方通配，占用者 loopback
		{"127.0.0.1", "0.0.0.0", true, "wildcard"}, // 我方 loopback，占用者通配
		{"0.0.0.0", "0.0.0.0", true, "wildcard"},
		{"0.0.0.0", "10.1.2.3", true, "wildcard"},
		{"::", "::", true, "wildcard"},
		{"::", "::1", true, "wildcard"},
		{"::1", "::", true, "wildcard"},
		{"::1", "::1", true, "exact"},
		{"::1", "::2", false, ""},
		{"fe80::1", "fe80::1", true, "exact"},
		{"2001:db8::1", "2001:db8::2", false, ""},
		{"::", "127.0.0.1", true, "wildcard"}, // Linux 双栈：:: 同时占用 IPv4
		{"127.0.0.1", "::", true, "wildcard"},
		{"::", "0.0.0.0", true, "wildcard"},
		{"0.0.0.0", "::", true, "wildcard"},
		{"0.0.0.0", "::1", false, ""}, // IPv4 通配不占 IPv6 具体地址
		{"::1", "0.0.0.0", false, ""},
		{"::1", "127.0.0.1", false, ""}, // 不同族的具体地址互不相干
		{"::ffff:127.0.0.1", "127.0.0.1", true, "exact"},
		{"::ffff:10.0.0.1", "0.0.0.0", true, "wildcard"},
		{"[::1]", "::1", true, "exact"},
		{"*", "127.0.0.1", true, "wildcard"},
		{"", "::1", true, "wildcard"},
	} {
		a, ok1 := parseBind(r.a)
		b, ok2 := parseBind(r.b)
		if !ok1 || !ok2 {
			t.Fatalf("解析失败 %q %q", r.a, r.b)
		}
		got, kind := overlap(a, b)
		if got != r.want || (got && kind != r.kind) {
			t.Errorf("overlap(%q,%q) = %v/%s，期望 %v/%s", r.a, r.b, got, kind, r.want, r.kind)
		}
		// 对称
		if g2, _ := overlap(b, a); g2 != got {
			t.Errorf("overlap 不对称: %q %q", r.a, r.b)
		}
	}
	if _, ok := parseBind("not-an-ip"); ok {
		t.Error("非法地址不应解析成功")
	}
}

func svcManifest(t testing.TB, comps map[string][]pt) Input {
	var list []cs
	for n, ps := range comps {
		list = append(list, cs{name: n, vers: []vs{{ver: "1.0.0", ports: ps}}})
	}
	return Input{Manifest: buildManifest(t, nil, list...)}
}

// AC4：与本机监听冲突的矩阵，通过完整求解（含 0.0.0.0 对 127.0.0.1、IPv6 ::/::1）。
func TestPortsAgainstListeners(t *testing.T) {
	type row struct {
		name     string
		ours     string
		proto    string
		theirs   Listener
		conflict bool
	}
	rows := []row{
		{"0.0.0.0 对 127.0.0.1", "0.0.0.0", "tcp", Listener{Proto: "tcp", Addr: "127.0.0.1", Port: 7000}, true},
		{"127.0.0.1 对 0.0.0.0", "127.0.0.1", "tcp", Listener{Proto: "tcp", Addr: "0.0.0.0", Port: 7000}, true},
		{"同地址", "127.0.0.1", "tcp", Listener{Proto: "tcp", Addr: "127.0.0.1", Port: 7000}, true},
		{"不同 loopback", "127.0.0.1", "tcp", Listener{Proto: "tcp", Addr: "127.0.0.2", Port: 7000}, false},
		{"IPv6 :: 对 ::1", "::", "tcp", Listener{Proto: "tcp", Addr: "::1", Port: 7000}, true},
		{"IPv6 ::1 对 ::", "::1", "tcp", Listener{Proto: "tcp6", Addr: "::", Port: 7000}, true},
		{"IPv6 ::1 对 ::1", "::1", "tcp", Listener{Proto: "tcp", Addr: "::1", Port: 7000}, true},
		{"0.0.0.0 对 ::1 无关", "0.0.0.0", "tcp", Listener{Proto: "tcp", Addr: "::1", Port: 7000}, false},
		{"协议不同", "0.0.0.0", "tcp", Listener{Proto: "udp", Addr: "0.0.0.0", Port: 7000}, false},
		{"udp6 归一", "0.0.0.0", "udp", Listener{Proto: "udp6", Addr: "::", Port: 7000}, true},
		{"端口不同", "0.0.0.0", "tcp", Listener{Proto: "tcp", Addr: "0.0.0.0", Port: 7001}, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			in := svcManifest(t, map[string][]pt{"svc": {{r.proto, 7000, r.ours, false}}})
			in.Want = want("svc")
			sys := okSystem()
			sys.Listeners = []Listener{r.theirs}
			in.System = sys
			p := Resolve(in)
			b := p.hasBlocker("port.conflict.listener")
			if (b != nil) != r.conflict {
				t.Fatalf("冲突 = %v，期望 %v：%+v", b != nil, r.conflict, p.Blockers)
			}
			if b != nil {
				if exitOf(t, p) != 4 || p.Executable() {
					t.Fatal("端口冲突应为退出码 4 且不可执行")
				}
				occ := b.Details["occupant"].(map[string]any)
				if occ["addr"] != r.theirs.Addr || !strings.Contains(b.Message, "7000") {
					t.Fatalf("应说明占用者: %+v", b)
				}
				// 阻断时 Plan 仍展示组件与端口（--dry-run 同样展示）
				if len(p.Components) != 1 || len(p.Ports) != 1 {
					t.Fatalf("阻断时仍须展示 Plan: %+v", p)
				}
			}
		})
	}
}

func TestPortConflictShowsOccupantAndBaotaHint(t *testing.T) {
	sys := okSystem()
	sys.Listeners = []Listener{{Proto: "tcp", Addr: "0.0.0.0", Port: 80, PID: 4321, Process: "nginx", User: "www"}, {Proto: "tcp", Addr: "0.0.0.0", Port: 443, PID: 4321, Process: "nginx", User: "www"}}
	sys.Baota = Baota{Detected: true, Confidence: "high", PanelPort: 8888}
	p := plan(t, Input{Manifest: stdManifest(t), System: sys, Want: want("node")})
	if exitOf(t, p) != 4 {
		t.Fatalf("%+v", p.Blockers)
	}
	n := 0
	for _, b := range p.Blockers {
		if b.Code == "port.conflict.listener" {
			n++
			if !strings.Contains(b.Message, "nginx") || !strings.Contains(b.Message, "pid 4321") || !strings.Contains(b.Message, "www") || !strings.Contains(b.Hint, "R-10") {
				t.Errorf("%+v", b)
			}
		}
	}
	if n != 2 || !p.hasWarning("baota.detected") {
		t.Fatalf("n=%d %+v", n, p.Warnings)
	}
	// 宝塔存在但无冲突 → 只有提示，不阻止（FR-7）
	sys.Listeners = nil
	p = plan(t, Input{Manifest: stdManifest(t), System: sys, Want: want("node")})
	mustOK(t, p)
	if !p.hasWarning("baota.detected") {
		t.Fatal("应提示宝塔存在")
	}
}

// 已装组件自己的监听（升级时）不是冲突；别的进程占着同一个端口才是。
func TestOwnListenerIsNotAConflict(t *testing.T) {
	m := stdManifest(t)
	sys := okSystem()
	sys.Listeners = []Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: 8080, PID: 100, Process: "lingedge-server", Unit: "lingedge-server.service"}}
	p := plan(t, Input{Manifest: m, System: sys, Want: []Want{{Component: "server", Version: "0.2.0"}}, Installed: inst("server@0.1.0")})
	mustOK(t, p)
	// 单元不符且进程名不符 → 外来占用
	sys.Listeners = []Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: 8080, PID: 7, Process: "python3", Unit: "other.service"}}
	p = plan(t, Input{Manifest: m, System: sys, Want: []Want{{Component: "server", Version: "0.2.0"}}, Installed: inst("server@0.1.0")})
	if p.hasBlocker("port.conflict.listener") == nil {
		t.Fatalf("%+v", p.Blockers)
	}
	// 全新安装时，哪怕进程叫 lingedge-server（残留），也是冲突
	sys.Listeners = []Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: 8080, PID: 7, Process: "lingedge-server"}}
	p = plan(t, Input{Manifest: m, System: sys, Want: want("server")})
	if p.hasBlocker("port.conflict.listener") == nil {
		t.Fatalf("%+v", p.Blockers)
	}
	// 保持不动的组件不查本机监听
	sys.Listeners = []Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: 8080, PID: 7, Process: "python3"}}
	p = plan(t, Input{Manifest: m, System: sys, Want: want("server"), Installed: inst("server@0.2.0")})
	mustOK(t, p)
}

// AC4：组件之间的端口冲突（同 Plan 内；也含与已装组件的冲突）。
func TestPortsBetweenComponents(t *testing.T) {
	in := svcManifest(t, map[string][]pt{"alpha": {{"tcp", 443, "0.0.0.0", false}}, "beta": {{"tcp", 443, "127.0.0.1", false}}, "gamma": {{"udp", 443, "0.0.0.0", false}}, "delta": {{"tcp", 443, "127.0.0.2", false}}})
	in.System = okSystem()
	in.Want = want("alpha", "beta")
	p := Resolve(in)
	b := p.hasBlocker("port.conflict.component")
	if b == nil || exitOf(t, p) != 4 || b.Component != "alpha" || b.Details["match"] != "wildcard" {
		t.Fatalf("%+v", p.Blockers)
	}
	for _, c := range [][]string{{"alpha", "gamma"}, {"beta", "delta"}} { // 协议不同 / 不同 loopback：不冲突
		in.Want = want(c...)
		mustOK(t, Resolve(in))
	}
	in.Want = want("alpha", "delta")
	if Resolve(in).hasBlocker("port.conflict.component") == nil {
		t.Fatal("0.0.0.0 对 127.0.0.2 应冲突")
	}
	// 与已装（保持不动）组件冲突
	in.Want = want("alpha")
	in.Installed = inst("beta@1.0.0")
	if p := Resolve(in); p.hasBlocker("port.conflict.component") == nil || p.comp("beta") != nil {
		t.Fatalf("%+v", p.Blockers)
	}
	// 两个都只是已装且不动：不是本次的问题
	in.Want = want("gamma")
	in.Installed = inst("alpha@1.0.0", "beta@1.0.0")
	mustOK(t, Resolve(in))
	// 清单未写 bind：按通配处理（最保守）
	in = svcManifest(t, map[string][]pt{"alpha": {{"tcp", 53, "", false}}})
	in.System = okSystem()
	in.System.Listeners = []Listener{{Proto: "tcp", Addr: "127.0.0.53", Port: 53, Process: "systemd-resolve"}}
	in.Want = want("alpha")
	p = Resolve(in)
	if p.hasBlocker("port.conflict.listener") == nil || p.Ports[0].Bind != "0.0.0.0" {
		t.Fatalf("%+v", p)
	}
}

// --set 换端口解决冲突；无效/不匹配的覆盖是用法错误。
func TestPortOverrides(t *testing.T) {
	m := stdManifest(t)
	sys := okSystem()
	sys.Listeners = []Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: 8080, Process: "other"}}
	base := Input{Manifest: m, System: sys, Want: want("server")}
	if plan(t, base).hasBlocker("port.conflict.listener") == nil {
		t.Fatal("前提：应冲突")
	}
	base.PortOverrides = []PortOverride{{Component: "server", Proto: "tcp", Port: 8080, To: 18080}}
	p := plan(t, base)
	mustOK(t, p)
	var found bool
	for _, q := range p.Ports {
		if q.Port == 18080 && q.Overridden && q.Purpose == "" {
			found = true
		}
		if q.Port == 8080 {
			t.Fatal("8080 应已被替换")
		}
	}
	if !found {
		t.Fatalf("%+v", p.Ports)
	}
	for name, ov := range map[string]PortOverride{
		"目标越界": {Component: "server", Proto: "tcp", Port: 8080, To: 70000},
		"无匹配":  {Component: "server", Proto: "tcp", Port: 1234, To: 5000},
		"坏地址":  {Component: "server", Proto: "tcp", Port: 8080, To: 5000, Bind: "nope"},
	} {
		base.PortOverrides = []PortOverride{ov}
		p := plan(t, base)
		if exitOf(t, p) != 2 {
			t.Errorf("%s: %+v", name, p.Blockers)
		}
	}
	// 覆盖后与别的组件冲突仍被发现
	in := svcManifest(t, map[string][]pt{"alpha": {{"tcp", 1000, "0.0.0.0", false}}, "beta": {{"tcp", 2000, "0.0.0.0", false}}})
	in.System = okSystem()
	in.Want = want("alpha", "beta")
	in.PortOverrides = []PortOverride{{Component: "alpha", Proto: "tcp", Port: 1000, To: 2000, Bind: "127.0.0.1"}}
	if Resolve(in).hasBlocker("port.conflict.component") == nil {
		t.Fatal("覆盖后的端口仍应参与矩阵")
	}
}

func TestListenerAddrInvalidTreatedAsWildcard(t *testing.T) {
	in := svcManifest(t, map[string][]pt{"alpha": {{"tcp", 9000, "127.0.0.1", false}}})
	in.System = okSystem()
	in.System.Listeners = []Listener{{Proto: "tcp", Addr: "garbage", Port: 9000}}
	in.Want = want("alpha")
	p := Resolve(in)
	if p.hasBlocker("port.conflict.listener") == nil || !p.hasWarning("listener.addr_invalid") {
		t.Fatalf("%+v %+v", p.Blockers, p.Warnings)
	}
	in = svcManifest(t, map[string][]pt{"alpha": {{"tcp", 9000, "bogus", false}}})
	in.System = okSystem()
	in.Want = want("alpha")
	if p := Resolve(in); p.hasBlocker("port.bind_invalid") == nil || exitOf(t, p) != 2 {
		t.Fatalf("%+v", p.Blockers)
	}
}
