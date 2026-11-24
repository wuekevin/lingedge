package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

func sock(port int, proc, user, unit string, pid int) probe.Socket {
	return probe.Socket{Listener: resolver.Listener{Proto: "tcp", Addr: "0.0.0.0", Port: port, PID: pid, Process: proc, User: user, Unit: unit}, Source: "ss"}
}

func TestPortCheckTable(t *testing.T) {
	srv := Component{Name: "server", Version: "1.0.0", State: "running", Unit: "lingedge-server.service", RunUser: "lingedge", Ports: []int{8080}}
	cases := []struct {
		name  string
		comp  Component
		socks []probe.Socket
		want  Level
		sub   string
	}{
		{"自己的监听（按单元）", srv, []probe.Socket{sock(8080, "lingedge-serve", "lingedge", "lingedge-server.service", 10)}, LevelPass, "自己监听"},
		{"自己的监听（按运行用户）", srv, []probe.Socket{sock(8080, "lingedge-serve", "lingedge", "", 10)}, LevelPass, ""},
		{"被他人占用", srv, []probe.Socket{sock(8080, "nginx", "www", "nginx.service", 812)}, LevelFail, "nginx(pid 812"},
		{"记录运行中但没监听", srv, nil, LevelWarn, "没有被监听"},
		{"已停止的组件端口空闲", Component{Name: "server", Version: "1.0.0", State: "stopped", Ports: []int{8080}}, nil, LevelPass, "未被占用"},
		{"占用者不可见", srv, []probe.Socket{{Listener: resolver.Listener{Proto: "tcp", Addr: "0.0.0.0", Port: 8080}, Hidden: true}}, LevelFail, "root"},
	}
	for _, c := range cases {
		w := newWorld(t)
		w.Components = []Component{c.comp}
		w.Probe = fakeProbe(goodSystem(), c.socks...)
		f := find(t, runAll(t, w, Filter{}), "port.server.8080")
		want(t, f, c.want, c.sub)
	}
	// DNS 53：占用者是 systemd-resolved 时给出 D-9 的专门提示。
	w := newWorld(t)
	w.Components = []Component{{Name: "dns", Version: "1.0.0", State: "installed", RunUser: "lingedge-dns", Ports: []int{53}}}
	w.Probe = fakeProbe(goodSystem(), sock(53, "systemd-resolve", "systemd-resolve", "systemd-resolved.service", 700))
	f := find(t, runAll(t, w, Filter{}), "port.dns.53")
	want(t, f, LevelFail, "systemd-resolve")
	if !strings.Contains(f.Hint, "systemd-resolved") || !strings.Contains(f.Hint, "D-9") {
		t.Errorf("53 端口提示：%q", f.Hint)
	}
	// ss 与 /proc 都不可用：不能当作无冲突。
	w = newWorld(t)
	w.Components = []Component{srv}
	w.Probe = func(context.Context) (probe.Result, error) {
		return probe.Result{System: goodSystem(), ListenersKnown: false, ListenersSource: "none"}, nil
	}
	want(t, find(t, runAll(t, w, Filter{}), "port.server.8080"), LevelWarn, "无法检测")
}

func TestBaotaNginxConflict(t *testing.T) {
	node := Component{Name: "node", Version: "1.0.0", State: "running", Unit: "lingedge-node.service", RunUser: "lingedge-node", Ports: []int{80, 443}}
	mk := func(comps []Component, rep baota.Report, socks ...probe.Socket) Finding {
		w := newWorld(t)
		w.Components = comps
		w.Probe = func(context.Context) (probe.Result, error) {
			return probe.Result{System: goodSystem(), Sockets: socks, ListenersKnown: true, Baota: rep}, nil
		}
		return find(t, runAll(t, w, Filter{}), "baota.nginx_conflict")
	}
	f := mk([]Component{node}, baota.Report{Detected: true, Confidence: "high"}, sock(80, "nginx", "root", "nginx.service", 900), sock(443, "nginx", "root", "nginx.service", 900))
	want(t, f, LevelFail, "宝塔")
	want(t, mk([]Component{node}, baota.Report{}, sock(80, "nginx", "root", "nginx.service", 900)), LevelFail, "80")
	want(t, mk([]Component{node}, baota.Report{}), LevelPass, "")
	want(t, mk([]Component{node}, baota.Report{}, sock(80, "openresty", "lingedge-node", "lingedge-openresty.service", 50)), LevelPass, "")
	want(t, mk(nil, baota.Report{Detected: true, Confidence: "high"}), LevelSkip, "宝塔")
}

func TestBaotaFirewall(t *testing.T) {
	setup := func(t *testing.T, fws []baota.Firewall, files map[string]string) Finding {
		w := newWorld(t)
		w.Components = []Component{{Name: "server", Version: "1.0.0", State: "running"}}
		w.ServerListens = func() (string, string) { return "127.0.0.1:8080", "0.0.0.0:9443" }
		for p, c := range files {
			put(t, w, p, c, 0o644)
		}
		w.Probe = func(context.Context) (probe.Result, error) {
			return probe.Result{System: goodSystem(), ListenersKnown: true, Baota: baota.Report{Firewalls: fws}}, nil
		}
		return find(t, runAll(t, w, Filter{}), "baota.firewall")
	}
	ufwBoth := "-A ufw-user-input -p tcp --dport 9443 -j ACCEPT\n-A ufw-user-input -p tcp --dport 9444 -j ACCEPT\n"
	ufwOne := "-A ufw-user-input -p tcp --dport 9443 -j ACCEPT\n"
	fwdBoth := `<zone><port protocol="tcp" port="9443"/><port protocol="tcp" port="9444"/></zone>`
	fwdRange := `<zone><port protocol="tcp" port="9440-9450"/></zone>`
	fwdOne := `<zone><port protocol="udp" port="9443"/><port protocol="tcp" port="9443"/></zone>`
	ufw := []baota.Firewall{{Kind: "ufw", Active: "yes"}}
	fwd := []baota.Firewall{{Kind: "firewalld", Active: "yes"}}
	want(t, setup(t, nil, nil), LevelPass, "未检测到启用")
	want(t, setup(t, []baota.Firewall{{Kind: "ufw", Active: "no"}}, nil), LevelPass, "")
	want(t, setup(t, ufw, map[string]string{"etc/ufw/user.rules": ufwBoth}), LevelPass, "已放行")
	f := setup(t, ufw, map[string]string{"etc/ufw/user.rules": ufwOne})
	want(t, f, LevelWarn, "9444")
	want(t, setup(t, ufw, nil), LevelWarn, "无法只读判定")
	want(t, setup(t, fwd, map[string]string{"etc/firewalld/zones/public.xml": fwdBoth}), LevelPass, "")
	want(t, setup(t, fwd, map[string]string{"etc/firewalld/zones/public.xml": fwdRange}), LevelPass, "")
	want(t, setup(t, fwd, map[string]string{"etc/firewalld/zones/public.xml": fwdOne}), LevelWarn, "9444")
	want(t, setup(t, []baota.Firewall{{Kind: "iptables", Active: "yes"}}, nil), LevelWarn, "无法只读判定")
	// 仅监听回环 + 没有 node：没有需要从外部可达的端口。
	w := newWorld(t)
	w.Components = []Component{{Name: "server", Version: "1.0.0"}}
	w.ServerListens = func() (string, string) { return "127.0.0.1:8080", "127.0.0.1:9443" }
	w.Probe = fakeProbe(goodSystem())
	want(t, find(t, runAll(t, w, Filter{}), "baota.firewall"), LevelSkip, "")
}

func TestDepChecks(t *testing.T) {
	srv := Component{Name: "server", Version: "1.0.0", State: "running", DataDir: "/var/lib/lingedge/server"}
	mysqlUp := probe.DepStatus{Kind: "mysql", Endpoint: probe.Endpoint{Host: "127.0.0.1", Port: 3306}, Reachable: true, Version: "8.0.36", Flavor: "mysql"}
	redisUp := probe.DepStatus{Kind: "redis", Endpoint: probe.Endpoint{Host: "127.0.0.1", Port: 6379}, Reachable: true, Version: "7.2.4"}
	mk := func(deps []probe.DepStatus, mut func(w *World)) *World {
		w := newWorld(t)
		w.Components = []Component{srv}
		w.Probe = func(context.Context) (probe.Result, error) {
			return probe.Result{System: goodSystem(), ListenersKnown: true, Deps: deps}, nil
		}
		w.ServerListens = func() (string, string) { return "127.0.0.1:8080", "0.0.0.0:9443" }
		if mut != nil {
			mut(w)
		}
		return w
	}
	rep := runAll(t, mk([]probe.DepStatus{mysqlUp, redisUp}, nil), Filter{})
	want(t, find(t, rep, "dep.mysql"), LevelPass, "8.0.36")
	want(t, find(t, rep, "dep.redis"), LevelPass, "7.2.4")
	// MySQL 不可达：向导阶段是告警；向导完成且就绪失败是 fail；就绪通过说明用的是远端实例。
	put(t, mk(nil, nil), "var/lib/lingedge/server/x", "", 0o644) // 仅为 helper 自检
	w := mk(nil, nil)
	want(t, find(t, runAll(t, w, Filter{}), "dep.mysql"), LevelWarn, "")
	w = mk(nil, func(w *World) {
		put(t, w, "var/lib/lingedge/server/install.lock", "", 0o600)
		w.HTTPGet = func(_ context.Context, u string) (int, error) {
			if strings.HasSuffix(u, "/ready") {
				return 503, nil
			}
			return 200, nil
		}
	})
	want(t, find(t, runAll(t, w, Filter{}), "dep.mysql"), LevelFail, "503")
	w = mk(nil, func(w *World) {
		put(t, w, "var/lib/lingedge/server/install.lock", "", 0o600)
		w.HTTPGet = func(context.Context, string) (int, error) { return 200, nil }
	})
	want(t, find(t, runAll(t, w, Filter{}), "dep.mysql"), LevelPass, "远端")
	want(t, find(t, runAll(t, w, Filter{}), "dep.redis"), LevelPass, "")
	// Redis 不可达：降级运行，告警而非失败。
	w = mk([]probe.DepStatus{mysqlUp}, nil)
	want(t, find(t, runAll(t, w, Filter{}), "dep.redis"), LevelWarn, "")
	// 未安装 server：不检查。
	w = mk(nil, func(w *World) { w.Components = nil })
	rep = runAll(t, w, Filter{})
	for _, f := range rep.Findings {
		if strings.HasPrefix(f.ID, "dep.") && f.Level != LevelSkip {
			t.Errorf("未安装 server 不应检查 %s：%+v", f.ID, f)
		}
	}
}

func TestMySQLSettings(t *testing.T) {
	mysqlUp := probe.DepStatus{Kind: "mysql", Reachable: true, Version: "8.0.36", Flavor: "mysql", Endpoint: probe.Endpoint{Host: "127.0.0.1", Port: 3306}}
	mk := func(s *MySQLSettings, err error) Finding {
		w := newWorld(t)
		w.Components = []Component{{Name: "server", Version: "1.0.0"}}
		w.Probe = func(context.Context) (probe.Result, error) {
			return probe.Result{System: goodSystem(), ListenersKnown: true, Deps: []probe.DepStatus{mysqlUp}}, nil
		}
		if s != nil || err != nil {
			w.MySQLSettings = func(context.Context) (MySQLSettings, error) {
				if err != nil {
					return MySQLSettings{}, err
				}
				return *s, nil
			}
		}
		return find(t, runAll(t, w, Filter{}), "dep.mysql.settings")
	}
	want(t, mk(&MySQLSettings{CharacterSet: "utf8mb4", SQLMode: "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION"}, nil), LevelPass, "utf8mb4")
	want(t, mk(&MySQLSettings{CharacterSet: "latin1", SQLMode: "STRICT_TRANS_TABLES"}, nil), LevelWarn, "latin1")
	want(t, mk(&MySQLSettings{CharacterSet: "utf8mb4", SQLMode: "NO_ENGINE_SUBSTITUTION"}, nil), LevelWarn, "严格")
	want(t, mk(&MySQLSettings{CharacterSet: "utf8mb4", SQLMode: "strict_all_tables"}, nil), LevelPass, "")
	want(t, mk(nil, errors.New("缺少迁移账号口令")), LevelSkip, "缺少迁移账号口令")
	want(t, mk(nil, nil), LevelSkip, "没有可用")
}

func TestDepNodeAndOpenresty(t *testing.T) {
	w := newWorld(t)
	w.Components = []Component{{Name: "website", Version: "1.0.0", Unit: "lingedge-website.service"}, {Name: "node", Version: "1.0.0"}}
	w.Probe = func(context.Context) (probe.Result, error) {
		return probe.Result{System: goodSystem(), ListenersKnown: true}, nil
	}
	w.OpenrestyV = func(context.Context) (string, error) { return "configure arguments: --with-http_ssl_module", nil }
	rep := runAll(t, w, Filter{})
	want(t, find(t, rep, "dep.nodejs"), LevelFail, "Node.js")
	want(t, find(t, rep, "dep.openresty"), LevelFail, "lua")
	w = newWorld(t)
	w.Components = []Component{{Name: "website", Version: "1.0.0", Unit: "lingedge-website.service"}, {Name: "node", Version: "1.0.0"}}
	s := goodSystem()
	s.Deps = map[string]resolver.Dep{"nodejs": {Detected: true, Version: "20.11.0"}}
	w.Probe = fakeProbe(s)
	w.OpenrestyV = func(context.Context) (string, error) {
		return "nginx version: openresty/1.21\nconfigure arguments: --add-module=../ngx_lua-0.10", nil
	}
	rep = runAll(t, w, Filter{})
	want(t, find(t, rep, "dep.nodejs"), LevelPass, "20.11.0")
	want(t, find(t, rep, "dep.openresty"), LevelPass, "")
	w.OpenrestyV = func(context.Context) (string, error) { return "", errors.New("找不到") }
	want(t, find(t, runAll(t, newWorldWith2(w), Filter{}), "dep.openresty"), LevelFail, "找不到")
	// website 静态变体（无单元）不需要 Node.js。
	w2 := newWorld(t)
	w2.Components = []Component{{Name: "website", Version: "1.0.0"}}
	w2.Probe = fakeProbe(goodSystem())
	want(t, find(t, runAll(t, w2, Filter{}), "dep.nodejs"), LevelSkip, "静态")
}

func newWorldWith2(w *World) *World {
	n := newWorldWith(w)
	n.OpenrestyV = w.OpenrestyV
	return n
}

func TestMySQLPmax(t *testing.T) {
	mysqlUp := probe.DepStatus{Kind: "mysql", Reachable: true, Version: "8.0.36", Flavor: "mysql", Endpoint: probe.Endpoint{Host: "127.0.0.1", Port: 3306}}
	mk := func(rows []PartitionRow, err error, set bool) Finding {
		w := newWorld(t)
		w.Components = []Component{{Name: "server", Version: "1.0.0"}}
		w.Probe = func(context.Context) (probe.Result, error) {
			return probe.Result{System: goodSystem(), ListenersKnown: true, Deps: []probe.DepStatus{mysqlUp}}, nil
		}
		if set {
			w.PmaxRows = func(context.Context) ([]PartitionRow, error) { return rows, err }
		}
		return find(t, runAll(t, w, Filter{}), "dep.mysql.pmax")
	}
	want(t, mk([]PartitionRow{{"audit_logs", 0}, {"login_logs", 0}}, nil, true), LevelPass, "2 张分区表")
	f := mk([]PartitionRow{{"audit_logs", 0}, {"login_logs", 12}}, nil, true)
	want(t, f, LevelWarn, "login_logs（12 行）")
	if strings.Contains(f.Message, "audit_logs") {
		t.Errorf("空的 pmax 不应列入：%q", f.Message)
	}
	want(t, mk(nil, nil, true), LevelPass, "没有分区表")
	want(t, mk(nil, errors.New("缺少迁移账号口令"), true), LevelSkip, "缺少迁移账号口令")
	want(t, mk(nil, nil, false), LevelSkip, "没有可用")
}
