package probe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

func resWith(socks ...Socket) Result {
	r := Result{ListenersKnown: true, ListenersSource: "ss", Sockets: socks}
	return r
}

func sk(proto, addr string, port, pid int, name, user string) Socket {
	s := Socket{Source: "ss", UID: -1, Listener: resolver.Listener{Proto: proto, Addr: addr, Port: port, PID: pid, Process: name, User: user}}
	if pid > 0 {
		s.Procs = []ProcRef{{pid, name}}
	} else {
		s.Hidden = true
	}
	return s
}

func TestAssessPortMatrix(t *testing.T) {
	cases := []struct {
		name     string
		listener Socket
		want     PortWant
		conflict bool
		match    string
	}{
		{"同地址", sk("tcp", "127.0.0.1", 8080, 10, "app", "u"), PortWant{"server", "tcp", 8080, "127.0.0.1"}, true, "exact"},
		{"占用者通配 vs 我方回环", sk("tcp", "0.0.0.0", 8080, 10, "app", "u"), PortWant{"server", "tcp", 8080, "127.0.0.1"}, true, "wildcard"},
		{"占用者回环 vs 我方通配", sk("tcp", "127.0.0.1", 80, 10, "app", "u"), PortWant{"node", "tcp", 80, "0.0.0.0"}, true, "wildcard"},
		{"* 通配", sk("tcp", "*", 80, 10, "app", "u"), PortWant{"node", "tcp", 80, ""}, true, "wildcard"},
		{"不同回环地址不冲突", sk("tcp", "127.0.0.2", 8080, 10, "app", "u"), PortWant{"server", "tcp", 8080, "127.0.0.1"}, false, ""},
		{"端口不同", sk("tcp", "0.0.0.0", 8081, 10, "app", "u"), PortWant{"server", "tcp", 8080, "127.0.0.1"}, false, ""},
		{"协议不同", sk("udp", "0.0.0.0", 53, 10, "app", "u"), PortWant{"dns", "tcp", 53, "0.0.0.0"}, false, ""},
		{"udp 冲突", sk("udp", "127.0.0.53", 53, 662, "systemd-resolve", "systemd-resolve"), PortWant{"dns", "udp", 53, "0.0.0.0"}, true, "wildcard"},
		{"tcp6 归一", sk("tcp6", "::", 443, 10, "app", "u"), PortWant{"node", "tcp", 443, "0.0.0.0"}, true, "wildcard"},
		{"我方写 tcp6 也归一", sk("tcp", "0.0.0.0", 443, 10, "app", "u"), PortWant{"node", "tcp6", 443, "0.0.0.0"}, true, "wildcard"},
		{"IPv6 具体地址 vs 0.0.0.0 不冲突", sk("tcp", "::1", 6379, 10, "app", "u"), PortWant{"x", "tcp", 6379, "0.0.0.0"}, false, ""},
		{"无权限看不到占用者仍冲突", sk("tcp", "0.0.0.0", 80, 0, "", ""), PortWant{"node", "tcp", 80, "0.0.0.0"}, true, "wildcard"},
	}
	for _, c := range cases {
		got := Assess(resWith(c.listener), nil, Intent{Ports: []PortWant{c.want}})
		if (len(got) == 1) != c.conflict || len(got) > 1 {
			t.Errorf("%s: 冲突=%v，期望 %v", c.name, got, c.conflict)
			continue
		}
		if c.conflict && got[0].Details["match"] != c.match {
			t.Errorf("%s: match=%v 期望 %s", c.name, got[0].Details["match"], c.match)
		}
	}
}

func TestAssessReportsOccupantPidNameOwner(t *testing.T) {
	nginx := sk("tcp", "0.0.0.0", 80, 1201, "nginx", "root")
	nginx.Procs = []ProcRef{{1201, "nginx"}, {1202, "nginx"}}
	nginx6 := sk("tcp", "::", 80, 1201, "nginx", "root")
	r := resWith(nginx, nginx6)
	r.Baota = baota.Report{Detected: true, Confidence: "high"}
	got := Assess(r, nil, Intent{Ports: []PortWant{{"node", "tcp", 80, "0.0.0.0"}}})
	if len(got) != 1 {
		t.Fatalf("同一端口的多个监听应合并为一条: %+v", got)
	}
	c := got[0]
	occ := c.Details["occupant"].(map[string]any)
	if occ["pid"] != 1201 || occ["process"] != "nginx" || occ["user"] != "root" {
		t.Errorf("occupant=%v", occ)
	}
	if len(c.Details["occupants"].([]map[string]any)) != 2 || !strings.Contains(c.Message, "pid 1201") || !strings.Contains(c.Message, "属主 root") || !strings.Contains(c.Message, "共 2 个监听") {
		t.Errorf("message=%s", c.Message)
	}
	if !strings.Contains(c.Hint, "R-10") || !strings.Contains(c.Hint, "不修改宝塔站点") {
		t.Errorf("宝塔 80/443 的处置建议: %s", c.Hint)
	}
	// 不自动停用：建议里只有人工选项
	if !strings.Contains(got[0].Hint, "取消") {
		t.Errorf("hint=%s", got[0].Hint)
	}
	// 非宝塔：通用建议
	r.Baota = baota.Report{}
	if got := Assess(r, nil, Intent{Ports: []PortWant{{"node", "tcp", 80, ""}}}); !strings.Contains(got[0].Hint, "--set") {
		t.Errorf("通用建议: %s", got[0].Hint)
	}
}

func TestAssessBaotaAloneIsNotAConflict(t *testing.T) {
	r := Result{ListenersKnown: true, Baota: baota.Report{Detected: true, Confidence: "high",
		Sites: []baota.Site{{File: "/www/server/panel/vhost/nginx/a.conf", Source: "baota", ServerNames: []string{"a.example.com"}}}}}
	got := Assess(r, baota.OSFS{Root: t.TempDir()}, Intent{
		Ports: []PortWant{{"server", "tcp", 8080, "127.0.0.1"}},
		Paths: []PathWant{{Component: "server", Path: "/opt/lingedge/server"}, {Component: "server", Path: "/etc/lingedge"}},
		Names: []NameWant{{"website", "site.example.com"}},
	})
	if len(got) != 0 {
		t.Fatalf("宝塔存在本身不应产生冲突: %+v", got)
	}
}

func TestAssessPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "opt/lingedge/openresty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "opt/lingedge/server"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc-lingedge-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := Result{ListenersKnown: true, Baota: baota.Report{Detected: true, Confidence: "medium"}}
	got := Assess(r, baota.OSFS{Root: root}, Intent{Paths: []PathWant{
		{Component: "node", Path: "/opt/lingedge/openresty"},                 // 已存在且不属于我们 → 冲突
		{Component: "server", Path: "/opt/lingedge/server", OwnedByUs: true}, // 已安装记录内 → 正常
		{Component: "node", Path: "/opt/lingedge/node"},                      // 不存在 → 正常
		{Component: "node", Path: "/etc-lingedge-file"},                      // 文件
		{Component: "x", Path: "/www/server/nginx/conf/lingedge.conf"},       // 宝塔管理目录
		{Component: "x", Path: "/wwwroot/x"},                                 // 不是 /www 前缀边界
		{Component: "x", Path: "relative/path"},                              // 相对路径忽略
	}})
	var paths []string
	for _, c := range got {
		paths = append(paths, c.Details["path"].(string))
	}
	want := []string{"/etc-lingedge-file", "/opt/lingedge/openresty", "/www/server/nginx/conf/lingedge.conf"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("路径冲突=%v 期望 %v", paths, want)
	}
	// 非宝塔环境下 /www 不是特殊目录
	r.Baota = baota.Report{}
	if got := Assess(r, baota.OSFS{Root: root}, Intent{Paths: []PathWant{{Component: "x", Path: "/www/x"}}}); len(got) != 0 {
		t.Errorf("非宝塔环境: %+v", got)
	}
}

func TestAssessNames(t *testing.T) {
	r := Result{ListenersKnown: true, Baota: baota.Report{Sites: []baota.Site{
		{File: "/www/server/panel/vhost/nginx/a.conf", Source: "baota", ServerNames: []string{"example.com", "www.example.com"}},
		{File: "/etc/nginx/conf.d/w.conf", Source: "system", ServerNames: []string{"*.wild.example.org", ".dot.example.net", "_", `~^(?<s>.+)\.re\.example\.org$`}},
	}}}
	cases := map[string]bool{
		"example.com": true, "EXAMPLE.com": true, "www.example.com": true, "other.example.com": false,
		"a.wild.example.org": true, "wild.example.org": false, "x.y.wild.example.org": true,
		"dot.example.net": true, "a.dot.example.net": true, "notdot.example.net": false,
		"_": false, "x.re.example.org": false, "": false,
		`~^(?<s>.+)\.re\.example\.org$`: false, // 正则无法静态判定：即使字面相同也不报（避免误报）
	}
	for name, want := range cases {
		got := Assess(r, nil, Intent{Names: []NameWant{{"website", name}}})
		if (len(got) > 0) != want {
			t.Errorf("%q: 冲突=%v 期望 %v", name, got, want)
		}
		for _, c := range got {
			if c.Kind != "name" || c.Details["file"] == "" {
				t.Errorf("%q: %+v", name, c)
			}
		}
	}
}

func TestMergeSockets(t *testing.T) {
	a := sk("tcp", "0.0.0.0", 80, 1203, "nginx", "")
	b := sk("tcp", "0.0.0.0", 80, 1201, "nginx", "root")
	b.Procs = []ProcRef{{1201, "nginx"}}
	h := sk("tcp", "0.0.0.0", 81, 0, "", "")
	h2 := sk("tcp", "0.0.0.0", 81, 5, "x", "u")
	out := mergeSockets([]Socket{h, a, b, h2, sk("tcp", "::", 80, 1, "nginx", "root")})
	if len(out) != 3 {
		t.Fatalf("merged=%d: %+v", len(out), out)
	}
	m := out[0]
	if m.Listener.Port != 80 || m.Listener.Addr != "0.0.0.0" || m.Listener.PID != 1201 || m.Listener.User != "root" || len(m.Procs) != 2 || m.Hidden {
		t.Errorf("合并后的 nginx:80: %+v", m)
	}
	if out[2].Listener.Port != 81 || out[2].Hidden || out[2].Listener.PID != 5 {
		t.Errorf("hidden 与可见合并后应可见: %+v", out[2])
	}
}
