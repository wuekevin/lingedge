package probe

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortDir 给出一个路径很短的临时目录（unix socket 路径上限约 104 字节，t.TempDir() 带测试名会超长）。
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "lp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// unixListen 在 p 上监听并对每个连接执行 h；返回收到的全部字节（用于「没有发送凭据」的断言）。
func unixListen(t *testing.T, p string, h func(net.Conn)) {
	t.Helper()
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = c.Close() }(); h(c) }()
		}
	}()
}

func TestValidateSocketPath(t *testing.T) {
	for _, ok := range []string{"/tmp/mysql.sock", "/www/server/mysql/mysql.sock", "/run/mysqld/mysqld.sock", "/var/lib/mysql/mysql.sock", "/a"} {
		if err := ValidateSocketPath(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "mysql.sock", "./a.sock", "/tmp/../etc/x.sock", "/tmp//a.sock", "/tmp/a b.sock", "/tmp/a;rm.sock", "/tmp/a$(x).sock", "/tmp/a\nb", "/tmp/a`b`", "-e", "/" + strings.Repeat("a", 120), "/tmp/a/", "/"} {
		if ValidateSocketPath(bad) == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

func TestCheckSocketKinds(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "s.sock")
	unixListen(t, sock, func(net.Conn) {})
	reg := filepath.Join(d, "file.sock")
	if err := os.WriteFile(reg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(d, "link.sock")
	badLink := filepath.Join(d, "badlink.sock")
	dangling := filepath.Join(d, "dangling.sock")
	dirLink := filepath.Join(d, "dirlink.sock")
	for l, tgt := range map[string]string{link: sock, badLink: reg, dangling: filepath.Join(d, "nonexistent"), dirLink: d} {
		if err := os.Symlink(tgt, l); err != nil {
			t.Fatal(err)
		}
	}
	info, err := CheckSocket(sock)
	if err != nil || info.Resolved != sock || !info.Trusted || info.UID != os.Geteuid() {
		t.Fatalf("真实 socket: %+v %v", info, err)
	}
	if info, err := CheckSocket(link); err != nil || info.Path != link || filepath.Base(info.Resolved) != "s.sock" || !info.Trusted {
		t.Fatalf("指向 socket 的符号链接（如 /tmp/mysql.sock → 真实 socket）应通过: %+v %v", info, err)
	}
	for name, p := range map[string]string{"普通文件": reg, "链接到普通文件（不跟随到非 socket）": badLink, "悬空链接": dangling, "链接到目录": dirLink, "目录": d, "不存在": filepath.Join(d, "none.sock")} {
		if _, err := CheckSocket(p); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
	if _, err := CheckSocket(reg); err == nil || !strings.Contains(err.Error(), "不是 socket") {
		t.Errorf("普通文件的错误应说明不是 socket: %v", err)
	}
}

func TestSocketOwnerTrust(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下所有 socket 的属主判定无法构造出不受信的情形")
	}
	d := shortDir(t)
	sock := filepath.Join(d, "s.sock")
	unixListen(t, sock, func(net.Conn) {})
	me := os.Geteuid()
	// 属主是本进程用户：受信
	if info, _ := CheckSocket(sock); !info.Trusted {
		t.Fatal("属主是当前用户应受信")
	}
	// 当前用户 ≠ socket 属主，属主账号名是 mysql：受信；是别的账号：不受信
	oldEuid, oldDB := euidFunc, socketUserDB
	t.Cleanup(func() { euidFunc, socketUserDB = oldEuid, oldDB })
	euidFunc = func() int { return me + 4242 }
	pw := filepath.Join(d, "passwd")
	for name, want := range map[string]bool{"mysql": true, "mariadb": true, "redis": true, "www": false, "nobody": false} {
		if err := os.WriteFile(pw, []byte(name+":x:"+itoa(me)+":"+itoa(me)+"::/:/bin/false\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		socketUserDB = pw
		if info, err := CheckSocket(sock); err != nil || info.Trusted != want {
			t.Errorf("属主账号 %s: trusted=%v，期望 %v（%v）", name, info.Trusted, want, err)
		}
	}
	socketUserDB = filepath.Join(d, "missing-passwd")
	if info, _ := CheckSocket(sock); info.Trusted {
		t.Error("取不到用户库时不应受信")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// 凭据只发给受信属主的 socket：属主不受信时 MySQL/Redis 都不发送任何凭据（服务器端收到的字节里没有口令）。
func TestCredentialsNeverSentToUntrustedSocket(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 下无法构造不受信属主")
	}
	oldEuid, oldDB := euidFunc, socketUserDB
	t.Cleanup(func() { euidFunc, socketUserDB = oldEuid, oldDB })
	euidFunc = func() int { return os.Geteuid() + 4242 }
	socketUserDB = filepath.Join(shortDir(t), "none")
	d := shortDir(t)
	var mu sync.Mutex
	var got []string
	record := func(s string) { mu.Lock(); got = append(got, s); mu.Unlock() }

	// MySQL：服务器发握手包（真实 mysqld 抓取的），记录客户端发来的任何字节
	greeting, err := hex.DecodeString(strings.Join(strings.Fields(string(fixture(t, "real/mysql-greeting-8.0.46.hex"))), ""))
	if err != nil {
		t.Fatal(err)
	}
	my := filepath.Join(d, "m.sock")
	unixListen(t, my, func(c net.Conn) {
		_, _ = c.Write(greeting)
		buf := make([]byte, 256)
		_ = c.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
		if n, _ := c.Read(buf); n > 0 {
			record(string(buf[:n]))
		}
	})
	marker := "never-sent-" + itoa(os.Getpid())
	st := ProbeMySQL(context.Background(), mustRunner(t), UnixEndpoint(my), Creds{MySQLUser: "u", MySQLPassword: marker}, time.Second)
	if !st.Reachable || st.Authenticated || st.Version == "" || !strings.Contains(st.Error, "拒绝向属主") {
		t.Fatalf("MySQL: %+v", st)
	}

	// Redis：对 PING 回 NOAUTH，记录收到的命令
	rd := filepath.Join(d, "r.sock")
	unixListen(t, rd, func(c net.Conn) {
		r := bufio.NewReader(c)
		for {
			_ = c.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			record(line)
			if strings.Contains(strings.ToUpper(line), "PING") {
				_, _ = c.Write([]byte("-NOAUTH Authentication required.\r\n"))
			}
		}
	})
	rs := ProbeRedis(context.Background(), UnixEndpoint(rd), Creds{RedisPassword: marker}, time.Second)
	if !rs.Reachable || !rs.AuthRequired || rs.Authenticated || !strings.Contains(rs.Error, "不受信") {
		t.Fatalf("Redis: %+v", rs)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(got, "|")
	if strings.Contains(all, marker) || strings.Contains(strings.ToUpper(all), "AUTH") {
		t.Fatalf("向不受信的 socket 发送了凭据: %q", all)
	}
	for _, b := range []any{st, rs} {
		if j, _ := json.Marshal(b); strings.Contains(string(j), marker) {
			t.Fatalf("口令出现在结果里: %s", j)
		}
	}
}

// 免凭据握手经 unix socket：不存在 / 不是 socket / 沉默服务器都明确失败。
func TestProbeUnixFailureModes(t *testing.T) {
	d := shortDir(t)
	ctx := context.Background()
	if st := ProbeMySQL(ctx, mustRunner(t), UnixEndpoint(filepath.Join(d, "none.sock")), Creds{}, time.Second); st.Reachable || st.Error == "" {
		t.Errorf("不存在: %+v", st)
	}
	reg := filepath.Join(d, "f.sock")
	_ = os.WriteFile(reg, nil, 0o600)
	if st := ProbeRedis(ctx, UnixEndpoint(reg), Creds{}, time.Second); st.Reachable || !strings.Contains(st.Error, "不是 socket") {
		t.Errorf("普通文件: %+v", st)
	}
	silent := filepath.Join(d, "silent.sock")
	unixListen(t, silent, func(c net.Conn) { time.Sleep(3 * time.Second) })
	start := time.Now()
	if st := ProbeMySQL(ctx, mustRunner(t), UnixEndpoint(silent), Creds{}, 500*time.Millisecond); st.Reachable || time.Since(start) > 2*time.Second {
		t.Errorf("沉默服务器应在超时内失败: %+v %v", st, time.Since(start))
	}
	http := filepath.Join(d, "http.sock")
	unixListen(t, http, func(c net.Conn) { _, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n")) })
	if st := ProbeMySQL(ctx, mustRunner(t), UnixEndpoint(http), Creds{}, time.Second); st.Reachable {
		t.Errorf("非 MySQL 协议不应可达: %+v", st)
	}
	if st := ProbeRedis(ctx, UnixEndpoint(http), Creds{}, time.Second); st.Reachable {
		t.Errorf("非 Redis 协议不应可达: %+v", st)
	}
	// 路径非法：不连接
	if st := ProbeMySQL(ctx, mustRunner(t), UnixEndpoint("/tmp/a b.sock"), Creds{}, time.Second); st.Reachable || st.Error == "" {
		t.Errorf("非法路径: %+v", st)
	}
}

func TestEndpointTypeCompatibleJSON(t *testing.T) {
	tcp := Endpoint{Host: "127.0.0.1", Port: 3306}
	b, _ := json.Marshal(tcp)
	if string(b) != `{"host":"127.0.0.1","port":3306}` {
		t.Fatalf("TCP 端点的 JSON 必须与 1.31 完全一致: %s", b)
	}
	var old Endpoint
	if err := json.Unmarshal([]byte(`{"host":"10.0.0.1","port":6379}`), &old); err != nil || old.IsUnix() || old.source() != "tcp 10.0.0.1:6379" {
		t.Fatalf("旧 JSON 应解析为 TCP: %+v %v", old, err)
	}
	u := UnixEndpoint("/run/mysqld/mysqld.sock")
	b, _ = json.Marshal(u)
	if string(b) != `{"type":"unix","socket":"/run/mysqld/mysqld.sock"}` || !u.IsUnix() || !u.loopback() || u.source() != "unix /run/mysqld/mysqld.sock" {
		t.Fatalf("unix 端点: %s %+v", b, u)
	}
}

func TestMySQLArgvUnixShape(t *testing.T) {
	ep := UnixEndpoint("/www/server/mysql/mysql.sock")
	args, err := mysqlArgs(ep, "lingedge_probe", 3*time.Second)
	if err != nil || !mysqlArgvAllowed(args) {
		t.Fatalf("%v %v", args, err)
	}
	want := []string{"--no-defaults", "--protocol=socket", "--socket=/www/server/mysql/mysql.sock", "-u", "lingedge_probe", "--connect-timeout=3", "-N", "-B", "-e", "SELECT VERSION()"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("argv=%v", args)
	}
	for _, a := range args {
		if strings.Contains(a, "-p") && strings.HasPrefix(a, "-p") || strings.Contains(a, "pass") {
			t.Errorf("argv 含口令选项: %q", a)
		}
	}
	for _, bad := range []string{"/tmp/a b.sock", "relative.sock", "/tmp/../x.sock", "/tmp/a;b.sock", ""} {
		if _, err := mysqlArgs(UnixEndpoint(bad), "u", time.Second); err == nil {
			t.Errorf("socket %q 应被拒绝", bad)
		}
	}
	if _, err := mysqlArgs(ep, "u; DROP", time.Second); err == nil {
		t.Error("用户名非法应被拒绝")
	}
	for _, bad := range [][]string{
		append(append([]string{}, args[:9]...), "DROP DATABASE x"),
		append(append([]string{}, args...), "--execute=DROP"),
		{"--no-defaults", "--protocol=socket", "--socket=/tmp/a b.sock", "-u", "u", "--connect-timeout=3", "-N", "-B", "-e", "SELECT VERSION()"},
		{"--no-defaults", "--protocol=socket", "--socket=/tmp/s.sock", "-u", "-evil", "--connect-timeout=3", "-N", "-B", "-e", "SELECT VERSION()"},
		{"--no-defaults", "--protocol=socket", "--socket=/tmp/s.sock", "-u", "u", "--connect-timeout=3", "-N", "-B", "-psecret", "SELECT VERSION()"},
		{"--no-defaults", "--protocol=tcp", "--socket=/tmp/s.sock", "-u", "u", "--connect-timeout=3", "-N", "-B", "-e", "SELECT VERSION()"},
		{"--protocol=socket", "--socket=/tmp/s.sock", "-u", "u", "--connect-timeout=3", "-N", "-B", "-e", "SELECT VERSION()", "x"},
	} {
		if mysqlArgvAllowed(bad) {
			t.Errorf("%v 不应被允许", bad)
		}
	}
}

func TestParseProcNetUnixAndDiscovery(t *testing.T) {
	got := ParseProcNetUnix(fixture(t, "proc/net/unix"))
	want := []string{"/run/mysqld/mysqld.sock", "/var/run/redis/redis-server.sock", "/www/server/mysql/mysql.sock", "/run/systemd/private", "/tmp/not-a-db.sock", "/tmp/mysql-evil name.sock", "/tmp/evil dir/mysqld.sock"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got=%q", got)
	}
	if ParseProcNetUnix([]byte("garbage\n\x00\xff\nNum RefCount\n1 2 3\n")) != nil {
		t.Error("垃圾输入应得到空")
	}
	proc := t.TempDir()
	_ = os.MkdirAll(filepath.Join(proc, "net"), 0o755)
	_ = os.WriteFile(filepath.Join(proc, "net", "unix"), fixture(t, "proc/net/unix"), 0o644)
	my := discoverSockets([]string{"/tmp/mysql.sock", "/run/mysqld/mysqld.sock"}, proc, reMySQLSockName)
	// 默认位置在前；/proc 发现的只保留名字像 MySQL socket 且路径合法的（含空格的、/tmp/not-a-db.sock 都不要）；去重
	if strings.Join(my, "|") != "/tmp/mysql.sock|/run/mysqld/mysqld.sock|/www/server/mysql/mysql.sock" {
		t.Fatalf("mysql 候选=%q", my)
	}
	rd := discoverSockets(DefaultRedisSockets, proc, reRedisSockName)
	if len(rd) != len(DefaultRedisSockets) || rd[len(rd)-1] != "/www/server/redis/redis.sock" && !contains(rd, "/var/run/redis/redis-server.sock") {
		t.Fatalf("redis 候选=%q", rd)
	}
	if got := discoverSockets(nil, "", reMySQLSockName); len(got) != 0 {
		t.Fatalf("无 proc 无默认应为空: %q", got)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Collect：声明的 socket 一定被探测（不存在则记为不可达并告警）；TCP 不可达时发现的 socket 才探测，且不存在的候选静默跳过。
func TestCollectUnixSocketEndpoints(t *testing.T) {
	d := shortDir(t)
	greeting, _ := hex.DecodeString(strings.Join(strings.Fields(string(fixture(t, "real/mysql-greeting-8.0.46.hex"))), ""))
	live := filepath.Join(d, "mysql.sock")
	unixListen(t, live, func(c net.Conn) { _, _ = c.Write(greeting) })

	base := func() Options {
		o := collectOpts(t, newExternalScripts(t, "ss/ss-lntup-root.txt", 0), "empty")
		o.MySQL, o.Redis = []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}, []Endpoint{{Host: "127.0.0.1", Port: closedPort(t)}}
		return o
	}
	// 声明 socket（宝塔常见）：TCP 不可达，socket 可达 → 依赖可达，来源 unix
	o := base()
	o.MySQLSockets = []string{filepath.Join(d, "missing.sock"), live}
	res, _ := Collect(context.Background(), o)
	if dep := res.System.Deps["mysql"]; !dep.Detected || dep.Version != "8.0.46" || dep.Source != "unix "+live || dep.Flavor != "mysql" {
		t.Fatalf("mysql dep=%+v warnings=%v", dep, res.Warnings)
	}
	var seenMissing bool
	for _, st := range res.Deps {
		if st.Kind == "mysql" && st.Endpoint.Socket == filepath.Join(d, "missing.sock") {
			seenMissing = !st.Reachable && st.Error != "" && st.Endpoint.Type == "unix"
		}
	}
	if !seenMissing || hasWarning(res, "dep.mysql.unreachable") || hasWarning(res, "dep.mysql.absent") {
		t.Fatalf("声明但不存在的 socket 应记为不可达；已可达时不应有依赖警告: %+v %v", res.Deps, res.Warnings)
	}
	if b, _ := json.Marshal(res); !strings.Contains(string(b), `"type":"unix"`) {
		t.Error("结果 JSON 应带 unix 端点")
	}
	var back Result
	b, _ := json.Marshal(res)
	if err := json.Unmarshal(b, &back); err != nil || len(back.Deps) != len(res.Deps) {
		t.Fatalf("JSON 往返: %v", err)
	}

	// 自动发现：TCP 不可达 + 默认位置/ /proc/net/unix 里的 socket
	oldDefs := DefaultMySQLSockets
	t.Cleanup(func() { DefaultMySQLSockets = oldDefs })
	DefaultMySQLSockets = []string{filepath.Join(d, "nope.sock"), live}
	o = base()
	o.MySQLSockets = nil
	res, _ = Collect(context.Background(), o)
	if dep := res.System.Deps["mysql"]; !dep.Detected || dep.Source != "unix "+live {
		t.Fatalf("自动发现: %+v %v", dep, res.Warnings)
	}
	for _, st := range res.Deps {
		if st.Endpoint.Socket == filepath.Join(d, "nope.sock") {
			t.Errorf("自动发现里不存在的候选应被静默跳过，不进入 Deps: %+v", st)
		}
	}
	// TCP 可达时不探测 socket（不惊动常驻服务）
	tcp := listenAndTCP(t, greeting)
	o = base()
	o.MySQL, o.MySQLSockets = []Endpoint{tcp}, nil
	res, _ = Collect(context.Background(), o)
	for _, st := range res.Deps {
		if st.Kind == "mysql" && st.Endpoint.IsUnix() {
			t.Errorf("TCP 已可达，不应再探测 socket: %+v", st)
		}
	}
	if dep := res.System.Deps["mysql"]; !strings.HasPrefix(dep.Source, "tcp ") {
		t.Errorf("dep=%+v", dep)
	}
	// 非 nil 空切片 = 不探测 socket
	o = base()
	o.MySQLSockets = []string{}
	res, _ = Collect(context.Background(), o)
	for _, st := range res.Deps {
		if st.Endpoint.IsUnix() {
			t.Errorf("MySQLSockets=[] 不应探测 socket: %+v", st)
		}
	}
}

func listenAndTCP(t *testing.T, payload []byte) Endpoint {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = c.Close() }(); _, _ = c.Write(payload) }()
		}
	}()
	return Endpoint{Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port}
}

func FuzzParseProcNetUnix(f *testing.F) {
	f.Add(fixture(f, "proc/net/unix"))
	f.Add([]byte("Num RefCount Protocol Flags Type St Inode Path\nffff: 00000002 00000000 00010000 0001 01 1 /run/mysqld/mysqld.sock\n"))
	f.Add([]byte("a b c d e f g @abstract\n\x00\xff"))
	f.Fuzz(func(t *testing.T, b []byte) {
		got := ParseProcNetUnix(b)
		if len(got) > 4096 {
			t.Fatal("结果无上限")
		}
		seen := map[string]bool{}
		for _, p := range got {
			if !strings.HasPrefix(p, "/") || seen[p] {
				t.Fatalf("非法或重复路径 %q", p)
			}
			seen[p] = true
		}
		_ = discoverSockets(DefaultMySQLSockets, "", reMySQLSockName)
	})
}

func FuzzValidateSocketPath(f *testing.F) {
	for _, s := range []string{"/tmp/mysql.sock", "", "/a b", "/../x", "-e", "/tmp/\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if ValidateSocketPath(p) == nil {
			if len(p) > maxSocketPath || !strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t\n\x00;$`\"'\\|&<>(){}*?[]!#~") {
				t.Fatalf("放行了不安全的路径 %q", p)
			}
		}
	})
}
