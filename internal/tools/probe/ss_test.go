package probe

import (
	"reflect"
	"testing"
)

func TestParseSSRoot(t *testing.T) {
	socks, skipped := ParseSS(fixture(t, "ss/ss-lntup-root.txt"))
	if skipped != 0 || len(socks) != 18 {
		t.Fatalf("sockets=%d skipped=%d", len(socks), skipped)
	}
	get := func(proto, addr string, port int) Socket {
		s, ok := findSock(socks, proto, addr, port)
		if !ok {
			t.Fatalf("缺少 %s %s:%d", proto, addr, port)
		}
		return s
	}
	// 多进程：nginx 三个 pid，Listener 取最小 pid（master）
	n := get("tcp", "0.0.0.0", 80)
	if n.Listener.PID != 1201 || n.Listener.Process != "nginx" || len(n.Procs) != 3 || n.Hidden || n.Source != "ss" {
		t.Errorf("nginx:80 = %+v", n)
	}
	if !reflect.DeepEqual(n.Procs, []ProcRef{{1203, "nginx"}, {1202, "nginx"}, {1201, "nginx"}}) {
		t.Errorf("procs=%+v", n.Procs)
	}
	// 接口后缀
	r := get("tcp", "127.0.0.53", 53)
	if r.Iface != "lo" || r.Listener.Process != "systemd-resolve" || r.Listener.PID != 662 {
		t.Errorf("resolved: %+v", r)
	}
	// IPv6 与通配
	if s := get("tcp", "::", 22); s.Listener.Process != "sshd" {
		t.Errorf("[::]:22 = %+v", s)
	}
	if s := get("tcp", "::1", 6379); s.Listener.Process != "redis-server" {
		t.Errorf("[::1]:6379 = %+v", s)
	}
	if s := get("tcp", "*", 8080); s.Listener.Process != "node" || s.Listener.PID != 4000 {
		t.Errorf("*:8080 = %+v", s)
	}
	if s := get("udp", "*", 5353); s.Listener.Process != "avahi-daemon" {
		t.Errorf("udp *:5353 = %+v", s)
	}
	if s := get("udp", "::1", 323); s.Listener.Process != "chronyd" {
		t.Errorf("udp [::1]:323 = %+v", s)
	}
	if s := get("tcp", "127.0.0.1", 3306); s.Listener.Process != "mysqld" || s.Listener.PID != 980 {
		t.Errorf("mysqld = %+v", s)
	}
}

func TestParseSSNoPermission(t *testing.T) {
	socks, skipped := ParseSS(fixture(t, "ss/ss-lntup-nonroot.txt"))
	if skipped != 0 || len(socks) != 7 {
		t.Fatalf("sockets=%d skipped=%d", len(socks), skipped)
	}
	hidden := 0
	for _, s := range socks {
		if s.Hidden {
			hidden++
			if s.Listener.PID != 0 || s.Listener.Process != "" || len(s.Procs) != 0 {
				t.Errorf("hidden 的监听不应有占用者: %+v", s)
			}
		}
	}
	if hidden != 6 {
		t.Errorf("hidden=%d，期望 6（只有自己的 node 可见）", hidden)
	}
	if s, _ := findSock(socks, "tcp", "0.0.0.0", 9999); s.Hidden || s.Listener.Process != "node" {
		t.Errorf("自己的进程应可见: %+v", s)
	}
	// 即使看不到进程，端口与地址仍然完整——冲突判定不依赖占用者信息
	if s, ok := findSock(socks, "tcp", "0.0.0.0", 80); !ok || !s.Hidden {
		t.Errorf("80 应存在且 hidden: %+v", s)
	}
}

func TestParseSSEdgeCases(t *testing.T) {
	socks, skipped := ParseSS(fixture(t, "ss/ss-lntup-edge.txt"))
	if skipped != 0 || len(socks) != 5 {
		t.Fatalf("sockets=%d skipped=%d", len(socks), skipped)
	}
	if s, _ := findSock(socks, "tcp", "0.0.0.0", 7000); s.Listener.Process != "Plex Media Serv" {
		t.Errorf("含空格的进程名: %+v", s)
	}
	s, _ := findSock(socks, "tcp", "0.0.0.0", 7001)
	if !reflect.DeepEqual(s.Procs, []ProcRef{{2101, `a"b,c)d`}, {2102, "x"}}) || s.Listener.PID != 2101 {
		t.Errorf("引号/逗号/括号的进程名: %+v", s.Procs)
	}
	if s, _ := findSock(socks, "tcp", "0.0.0.0", 7002); s.Listener.Process != "中文" {
		t.Errorf("\\xNN 转义: %+v", s)
	}
	if s, ok := findSock(socks, "tcp", "fe80::1", 7003); !ok || s.Iface != "eth0" {
		t.Errorf("IPv6 接口后缀: %+v", s)
	}
	if _, ok := findSock(socks, "tcp", "::ffff:127.0.0.1", 7004); !ok {
		t.Error("IPv4 映射地址保留原文")
	}
}

func TestParseSSGarbageTolerance(t *testing.T) {
	in := []byte("" +
		"tcp   LISTEN 0 128 0.0.0.0:80 0.0.0.0:* users:((\"nginx\",pid=1,fd=6))\n" +
		"garbage line\n" +
		"tcp   ESTAB  0 0 10.0.0.5:22 10.0.0.9:51234\n" + // 不是监听
		"tcp   LISTEN x 128 0.0.0.0:81 0.0.0.0:*\n" + // Recv-Q 非数字
		"tcp   LISTEN 0 128 0.0.0.0:99999 0.0.0.0:*\n" + // 端口越界
		"tcp   LISTEN 0 128 not-an-ip:82 0.0.0.0:*\n" +
		"udp   UNCONN 0 0 0.0.0.0:53 0.0.0.0:* users:((\"x\",pid=abc,fd=1))\n" + // pid 非数字 → 无占用者但行有效
		"\n   \n" +
		"tcp   LISTEN 0 128 0.0.0.0:83 0.0.0.0:* users:((\"unterminated\n")
	socks, skipped := ParseSS(in)
	if len(socks) != 3 || skipped != 5 {
		t.Fatalf("sockets=%d skipped=%d: %+v", len(socks), skipped, socks)
	}
	if !socks[1].Hidden || !socks[2].Hidden {
		t.Errorf("无法解析出进程的行应标 Hidden: %+v", socks)
	}
}

func FuzzParseSS(f *testing.F) {
	for _, n := range []string{"ss-lntup-root.txt", "ss-lntup-nonroot.txt", "ss-lntup-edge.txt"} {
		f.Add(fixture(f, "ss/"+n))
	}
	f.Add([]byte(`tcp LISTEN 0 1 [::]:1 [::]:* users:(("\`))
	f.Fuzz(func(t *testing.T, b []byte) {
		socks, _ := ParseSS(b)
		for _, s := range socks {
			if s.Listener.Port < 0 || s.Listener.Port > 65535 || (s.Listener.Proto != "tcp" && s.Listener.Proto != "udp") {
				t.Fatalf("非法输出: %+v", s)
			}
			if s.Hidden != (len(s.Procs) == 0) {
				t.Fatalf("Hidden 与 Procs 不一致: %+v", s)
			}
			for _, p := range s.Procs {
				if p.PID <= 0 {
					t.Fatalf("pid 非法: %+v", p)
				}
			}
		}
	})
}
