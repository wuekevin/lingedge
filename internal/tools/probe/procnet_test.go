package probe

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"testing"
)

func TestParseProcNetTCPAndByteOrder(t *testing.T) {
	es, skipped := ParseProcNet(fixture(t, "proc/net/tcp"), "tcp", false, true)
	if skipped != 0 {
		t.Fatalf("skipped=%d", skipped)
	}
	// 10 行里 2 行不是监听（ESTABLISHED、TIME_WAIT）
	want := []ProcNetEntry{
		{"tcp", "0.0.0.0", 80, 0, 20001}, {"tcp", "0.0.0.0", 443, 0, 20002}, {"tcp", "127.0.0.1", 3306, 111, 20003},
		{"tcp", "0.0.0.0", 22, 0, 20004}, {"tcp", "127.0.0.53", 53, 101, 20005}, {"tcp", "0.0.0.0", 8888, 0, 20006},
		{"tcp", "127.0.0.1", 6379, 112, 20007}, {"tcp", "0.0.0.0", 9999, 1000, 20020},
	}
	if !reflect.DeepEqual(es, want) {
		t.Fatalf("tcp:\n got %+v\nwant %+v", es, want)
	}
	// 字节序：同一段文本在大端主机上含义不同——0100007F 在小端是 127.0.0.1，在大端是 1.0.0.127
	be, _ := ParseProcNet([]byte("  0: 0100007F:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0 100 0 0 10 0\n"), "tcp", false, false)
	le, _ := ParseProcNet([]byte("  0: 0100007F:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0 100 0 0 10 0\n"), "tcp", false, true)
	if len(be) != 1 || be[0].Addr != "1.0.0.127" || len(le) != 1 || le[0].Addr != "127.0.0.1" {
		t.Errorf("字节序: be=%+v le=%+v", be, le)
	}
	// 端口始终是大端十六进制：0050=80，1F90=8080，不随主机字节序变化
	if be[0].Port != 80 || le[0].Port != 80 {
		t.Errorf("端口: %d %d", be[0].Port, le[0].Port)
	}
	beBE, _ := ParseProcNet([]byte("  0: 7F000001:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0 100 0 0 10 0\n"), "tcp", false, false)
	if len(beBE) != 1 || beBE[0].Addr != "127.0.0.1" || beBE[0].Port != 8080 {
		t.Errorf("大端主机 127.0.0.1:8080 = %+v", beBE)
	}
}

func TestParseProcNetIPv6AndUDP(t *testing.T) {
	es, _ := ParseProcNet(fixture(t, "proc/net/tcp6"), "tcp", true, true)
	got := map[string]int{}
	for _, e := range es {
		got[e.Addr+"/"+strconv.Itoa(e.Port)] = int(e.Inode)
	}
	want := map[string]int{"::/22": 20008, "::1/6379": 20009, "::/80": 20010, "::/443": 20012, "::/8080": 20013}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tcp6=%v", got)
	}
	// IPv4 映射地址：::ffff:127.0.0.1 在小端主机上写成 0000000000000000FFFF00000100007F
	mapped, _ := ParseProcNet([]byte("  0: 0000000000000000FFFF00000100007F:1F95 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 9 1 0 100 0 0 10 0\n"), "tcp", true, true)
	if len(mapped) != 1 || mapped[0].Addr != "::ffff:127.0.0.1" || mapped[0].Port != 8085 {
		t.Errorf("IPv4 映射地址: %+v", mapped)
	}
	// IPv6 的四个 32 位字各自按主机序：::1 在小端是 00000000000000000000000001000000
	be, _ := ParseProcNet([]byte("  0: 00000000000000000000000000000001:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0 100 0 0 10 0\n"), "tcp", true, false)
	if len(be) != 1 || be[0].Addr != "::1" {
		t.Errorf("大端主机的 ::1: %+v", be)
	}
	// UDP：只收「已绑定未连接」（状态 07、远端全零）；已连接的 UDP 套接字不是监听
	u, _ := ParseProcNet(fixture(t, "proc/net/udp"), "udp", false, true)
	var ports []int
	for _, e := range u {
		ports = append(ports, e.Port)
		if e.Proto != "udp" {
			t.Errorf("proto=%s", e.Proto)
		}
	}
	if !reflect.DeepEqual(ports, []int{53, 68, 123}) {
		t.Errorf("udp ports=%v（40000 是已连接套接字，应排除）", ports)
	}
	// 防御：状态 07 但远端非零的行（内核不会产生）不算监听
	if got, _ := ParseProcNet([]byte("  0: 0100007F:0035 0200007F:1234 07 00000000:00000000 00:00000000 00000000     0        0 5 2 0 0\n"), "udp", false, true); len(got) != 0 {
		t.Errorf("远端非零的 UDP 行不应算监听: %+v", got)
	}
	u6, _ := ParseProcNet(fixture(t, "proc/net/udp6"), "udp", true, true)
	if len(u6) != 2 || u6[0].Addr != "::1" || u6[0].Port != 323 || u6[1].Addr != "::" {
		t.Errorf("udp6=%+v", u6)
	}
}

func TestParseProcNetMalformed(t *testing.T) {
	in := []byte("  sl  local_address rem_address   st\n" +
		"  0: ZZZZZZZZ:0050 00000000:0000 0A 0 0 0 0 0 0 5\n" + // 十六进制非法
		"  1: 0100007F:00 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 5\n" + // 端口宽度不对
		"  2: 0100007F:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000 xx 0 5\n" + // uid 非数字
		"  3: 0100007F:0050\n" + // 列太少
		"  4: 0100007F:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 77 1 0 100 0 0 10 0\n")
	es, skipped := ParseProcNet(in, "tcp", false, true)
	if len(es) != 1 || es[0].Inode != 77 || skipped != 4 {
		t.Fatalf("es=%+v skipped=%d", es, skipped)
	}
	if es, _ := ParseProcNet(nil, "tcp", false, true); len(es) != 0 {
		t.Error("空输入")
	}
	if es, _ := ParseProcNet(in, "sctp", false, true); len(es) != 0 {
		t.Error("未知协议应无输出")
	}
}

func FuzzParseProcNet(f *testing.F) {
	for _, n := range []string{"tcp", "tcp6", "udp", "udp6"} {
		f.Add(fixture(f, "proc/net/"+n), n[:3], n == "tcp6" || n == "udp6", true)
	}
	f.Fuzz(func(t *testing.T, b []byte, proto string, v6, le bool) {
		es, _ := ParseProcNet(b, proto, v6, le)
		for _, e := range es {
			if e.Port < 0 || e.Port > 65535 || e.Addr == "" {
				t.Fatalf("非法输出: %+v", e)
			}
		}
	})
}

func TestListenersFromProcFixture(t *testing.T) {
	root := materializeProc(t)
	p := ProcFS{Root: root, UserDB: filepath.Join(fixtureRoot, "passwd"), LE: true}
	socks, notes, err := p.ListenersFromProc()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Errorf("notes=%v", notes)
	}
	// 8 + 5 + 3 + 2 = 18 个监听（连接态/已连接 UDP 被排除）
	if len(socks) != 18 {
		t.Fatalf("sockets=%d", len(socks))
	}
	n, _ := findSock(socks, "tcp", "0.0.0.0", 80)
	if n.Hidden || n.Listener.PID != 1201 || n.Listener.Process != "nginx" || len(n.Procs) != 3 || n.Listener.User != "root" || n.Source != "proc" {
		t.Errorf("nginx:80 = %+v", n)
	}
	// 同一 inode 的 master/worker：全部 pid 都应出现
	if !reflect.DeepEqual(n.Procs, []ProcRef{{1201, "nginx"}, {1202, "nginx"}, {1203, "nginx"}}) {
		t.Errorf("procs=%+v", n.Procs)
	}
	m, _ := findSock(socks, "tcp", "127.0.0.1", 3306)
	if m.Listener.User != "mysql" || m.Listener.Process != "mysqld" || m.Listener.PID != 980 {
		t.Errorf("mysqld: %+v", m)
	}
	// 同一进程持有两个 socket（IPv4 与 IPv6）
	s6, _ := findSock(socks, "tcp", "::", 22)
	if s6.Listener.PID != 800 || s6.Listener.User != "root" {
		t.Errorf("sshd v6: %+v", s6)
	}
	// uid 无 passwd 记录时回落为数字
	// （夹具里 uid 1001 有记录；用不存在的 passwd 路径验证回落）
	p2 := ProcFS{Root: root, UserDB: filepath.Join(root, "no-such-passwd"), LE: true}
	socks2, _, _ := p2.ListenersFromProc()
	if m2, _ := findSock(socks2, "tcp", "127.0.0.1", 3306); m2.Listener.User != "111" {
		t.Errorf("无 passwd 时属主应为数字 uid: %+v", m2)
	}
}

// 非 root 看不到他人进程的 fd：占用者 pid/name 不可见（Hidden），但属主 uid 仍取自 /proc/net。
func TestListenersFromProcPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("需要非 root 才能构造无权限目录")
	}
	root := materializeProc(t)
	deny := filepath.Join(root, "4000", "fd")
	if err := os.Chmod(deny, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(deny, 0o755) })
	p := ProcFS{Root: root, UserDB: filepath.Join(fixtureRoot, "passwd"), LE: true}
	socks, notes, err := p.ListenersFromProc()
	if err != nil {
		t.Fatal(err)
	}
	s, ok := findSock(socks, "tcp", "0.0.0.0", 9999)
	if !ok || !s.Hidden || s.Listener.PID != 0 || s.Listener.Process != "" || s.Listener.User != "ubuntu" {
		t.Errorf("无权限时应 Hidden 且属主取自 uid: %+v", s)
	}
	if len(notes) != 1 {
		t.Errorf("应有一条降级说明: %v", notes)
	}
	// 其余监听不受影响
	if n, _ := findSock(socks, "tcp", "0.0.0.0", 80); n.Hidden {
		t.Error("nginx 不应 hidden")
	}
}

func TestListenersFromProcUnreadable(t *testing.T) {
	if _, _, err := (ProcFS{Root: filepath.Join(t.TempDir(), "nope")}).ListenersFromProc(); err == nil {
		t.Fatal("proc 根不存在应报错")
	}
	// 只有 tcp 存在（内核禁用 IPv6 与 UDP 文件缺失）：成功且无噪声
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "net", "tcp"), fixture(t, "proc/net/tcp"), 0o644); err != nil {
		t.Fatal(err)
	}
	socks, notes, err := (ProcFS{Root: dir, LE: true}).ListenersFromProc()
	if err != nil || len(notes) != 0 || len(socks) != 8 {
		t.Errorf("socks=%d notes=%v err=%v", len(socks), notes, err)
	}
}

func TestEnrichFromSS(t *testing.T) {
	root := materializeProc(t)
	socks, _ := ParseSS(fixture(t, "ss/ss-lntup-root.txt"))
	ProcFS{Root: root, UserDB: filepath.Join(fixtureRoot, "passwd"), LE: true}.Enrich(socks)
	n, _ := findSock(socks, "tcp", "0.0.0.0", 80)
	if n.Listener.User != "root" || n.Listener.Unit != "nginx.service" {
		t.Errorf("nginx: %+v", n.Listener)
	}
	m, _ := findSock(socks, "tcp", "127.0.0.1", 3306)
	if m.Listener.User != "mysql" || m.Listener.Unit != "mysql.service" {
		t.Errorf("mysqld: %+v", m.Listener)
	}
	node, _ := findSock(socks, "tcp", "0.0.0.0", 9999)
	if node.Listener.User != "ubuntu" || node.Listener.Unit != "" { // session scope 不是 .service
		t.Errorf("node: %+v", node.Listener)
	}
}

func TestUnitFromCgroup(t *testing.T) {
	cases := map[string]string{
		"0::/system.slice/nginx.service\n":                                              "nginx.service",
		"0::/system.slice/run-abc.scope\n":                                              "",
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/x.scope":            "user@1000.service",
		"12:name=systemd:/system.slice/ssh.service\n11:cpu:/system.slice/ssh.service\n": "ssh.service",
		"0::/\n":       "",
		"garbage":      "",
		"":             "",
		"0::/.service": "",
	}
	for in, want := range cases {
		if got := UnitFromCgroup(in); got != want {
			t.Errorf("%q → %q，期望 %q", in, got, want)
		}
	}
}

func TestParsePasswdAndStatus(t *testing.T) {
	m := ParsePasswd([]byte("root:x:0:0::/root:/bin/sh\n# c\nbad\ndup:x:0:0::/:/bin/sh\nx:x:abc:0\n:x:5:5\nmysql:x:111:116::/n:/bin/false\n"))
	if !reflect.DeepEqual(m, map[int]string{0: "root", 111: "mysql"}) {
		t.Errorf("passwd=%v", m)
	}
	if uid, ok := parseStatusUID([]byte("Name:\tx\nUid:\t1000\t1001\t1000\t1000\n")); !ok || uid != 1001 {
		t.Errorf("uid=%d %v", uid, ok)
	}
	if _, ok := parseStatusUID([]byte("Name:\tx\n")); ok {
		t.Error("无 Uid 行")
	}
}
