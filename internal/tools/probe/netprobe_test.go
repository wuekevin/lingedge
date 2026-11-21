package probe

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

func greetingPacket(version string) []byte {
	payload := append([]byte{0x0a}, []byte(version)...)
	payload = append(payload, 0, 1, 2, 3, 4)
	n := len(payload)
	return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), 0}, payload...)
}

func TestParseMySQLGreeting(t *testing.T) {
	real, err := hex.DecodeString(strings.Join(strings.Fields(string(fixture(t, "real/mysql-greeting-8.0.46.hex"))), ""))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := ParseMySQLGreeting(bytes.NewReader(real)); err != nil || v != "8.0.46" {
		t.Fatalf("真实握手包: %q %v", v, err)
	}
	ok := map[string]string{
		"5.5.5-10.11.6-MariaDB-0+deb12u1": "10.11.6-MariaDB-0+deb12u1", // MariaDB 的 5.5.5- 兼容前缀被去掉
		"8.0.36-0ubuntu0.22.04.1":         "8.0.36-0ubuntu0.22.04.1",
		"5.7.44-log":                      "5.7.44-log",
	}
	for in, want := range ok {
		if v, err := ParseMySQLGreeting(bytes.NewReader(greetingPacket(in))); err != nil || v != want {
			t.Errorf("%q → %q %v，期望 %q", in, v, err, want)
		}
	}
	bad := map[string][]byte{
		"空":       nil,
		"只有头":     {1, 0, 0, 0},
		"长度为 0":   {0, 0, 0, 0},
		"长度过大":    {0xff, 0xff, 0x7f, 0},
		"错误包":     append([]byte{9, 0, 0, 0, 0xff}, []byte("\x15\x04#HY000")...),
		"协议版本 9":  append([]byte{7, 0, 0, 0, 9}, '8', '.', '0', '.', '1', 0),
		"无版本串":    {2, 0, 0, 0, 0x0a, 0},
		"版本串没有结尾": append([]byte{4, 0, 0, 0, 0x0a}, 'a', 'b', 'c'),
		"版本格式异常":  greetingPacket("not-a-version"),
		"HTTP 响应": []byte("HTTP/1.1 400 Bad Request\r\n\r\n"),
		"载荷被截断":   {9, 0, 0, 0, 0x0a, '8'},
	}
	for name, b := range bad {
		if v, err := ParseMySQLGreeting(bytes.NewReader(b)); err == nil {
			t.Errorf("%s: 应失败，得到 %q", name, v)
		}
	}
}

func TestRespAndRedisAllowed(t *testing.T) {
	if got := string(respEncode([]string{"AUTH", "u", "p"})); got != "*3\r\n$4\r\nAUTH\r\n$1\r\nu\r\n$1\r\np\r\n" {
		t.Errorf("RESP 编码: %q", got)
	}
	allowed := [][]string{{"PING"}, {"ping"}, {"AUTH", "p"}, {"AUTH", "u", "p"}, {"INFO", "server"}, {"info", "SERVER"}}
	for _, a := range allowed {
		if !redisAllowed(a) {
			t.Errorf("应允许 %v", a)
		}
	}
	denied := [][]string{nil, {}, {"SET", "k", "v"}, {"FLUSHALL"}, {"CONFIG", "SET", "x", "y"}, {"INFO"}, {"INFO", "all"}, {"PING", "x"}, {"AUTH"}, {"AUTH", "a", "b", "c"}, {"CLIENT", "SETNAME", "x"}, {"SHUTDOWN"}, {"DEBUG", "SLEEP", "0"}}
	for _, a := range denied {
		if redisAllowed(a) {
			t.Errorf("应拒绝 %v", a)
		}
	}
	r, err := respRead(bufioReader("$-1\r\n"))
	if err != nil || r.kind != '$' || r.text != "" {
		t.Errorf("空 bulk: %+v %v", r, err)
	}
	for _, bad := range []string{"", "\r\n", "?x\r\n", "$99999999\r\n", "$abc\r\n", "+OK\n", "$5\r\nab"} {
		if _, err := respRead(bufioReader(bad)); err == nil {
			t.Errorf("%q 应失败", bad)
		}
	}
}

func TestParseRedisInfoAndVersions(t *testing.T) {
	info := string(fixture(t, "real/redis-info-server.txt"))
	if v := ParseRedisInfoVersion(info); v != "8.10.1" {
		t.Errorf("INFO 版本=%q", v)
	}
	if v := ParseRedisInfoVersion("# Server\r\nredis_mode:standalone\r\n"); v != "" {
		t.Errorf("无版本: %q", v)
	}
	if f := redisFlavor("server_name:valkey\r\n"); f != "valkey" {
		t.Errorf("flavor=%q", f)
	}
	if v := ParseRedisServerVersion(string(fixture(t, "real/redis-server-version.txt"))); v != "8.10.1" {
		t.Errorf("redis-server 版本=%q", v)
	}
	if v := ParseRedisServerVersion("Redis server v=7.0.15 sha=00000000:0 malloc=jemalloc-5.3.0 bits=64 build=abc"); v != "7.0.15" {
		t.Errorf("v=%q", v)
	}
}

func TestEndpointLoopback(t *testing.T) {
	for h, want := range map[string]bool{"127.0.0.1": true, "::1": true, "localhost": true, "127.5.5.5": true, "::ffff:127.0.0.1": true, "10.0.0.5": false, "example.com": false, "0.0.0.0": false} {
		if got := (Endpoint{Host: h, Port: 1}).loopback(); got != want {
			t.Errorf("%s loopback=%v", h, got)
		}
	}
}

// 口令只发往环回：对非环回地址，即使服务器要求 AUTH 也绝不发送口令（明文 AUTH 发往他机等于泄露）。
func TestRedisNeverSendsPasswordToNonLoopback(t *testing.T) {
	ip := nonLoopbackIPv4(t)
	received := make(chan string, 4)
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("无法在 %s 上监听: %v", ip, err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				r := bufioReader2(c)
				for {
					_ = c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					received <- strings.TrimSpace(line)
					if strings.Contains(strings.ToUpper(line), "PING") {
						_, _ = c.Write([]byte("-NOAUTH Authentication required.\r\n"))
					}
				}
			}(c)
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	marker := "never-sent-marker-" + strconv.Itoa(port)
	st := ProbeRedis(context.Background(), Endpoint{Host: ip, Port: port}, Creds{RedisPassword: marker}, time.Second)
	time.Sleep(300 * time.Millisecond)
	close(received)
	var all []string
	for l := range received {
		all = append(all, l)
	}
	if strings.Contains(strings.Join(all, "|"), marker) || strings.Contains(strings.ToUpper(strings.Join(all, "|")), "AUTH") {
		t.Fatalf("向非环回地址发送了 AUTH/口令: %v", all)
	}
	if !st.Reachable || !st.AuthRequired || st.Authenticated || !strings.Contains(st.Error, "非环回") {
		t.Fatalf("结果: %+v", st)
	}
}

func TestMySQLArgvShapeAndValidation(t *testing.T) {
	args, err := mysqlArgs(Endpoint{Host: "127.0.0.1", Port: 3306}, "lingedge_probe", 3*time.Second)
	if err != nil || !mysqlArgvAllowed(args) {
		t.Fatalf("合法 argv: %v %v", args, err)
	}
	for _, a := range args {
		if strings.Contains(a, "pass") || strings.HasPrefix(a, "-p") {
			t.Errorf("argv 不得含口令选项: %q", a)
		}
	}
	for _, bad := range []struct {
		ep   Endpoint
		user string
	}{
		{Endpoint{"127.0.0.1", 3306}, "u; DROP"}, {Endpoint{"127.0.0.1", 3306}, "-e"}, {Endpoint{"127.0.0.1", 3306}, ""},
		{Endpoint{"--host", 3306}, "u"}, {Endpoint{"a b", 3306}, "u"}, {Endpoint{"127.0.0.1", 0}, "u"}, {Endpoint{"127.0.0.1", 70000}, "u"},
		{Endpoint{"h$(x)", 3306}, "u"}, {Endpoint{"127.0.0.1", 3306}, "u`x`"}, {Endpoint{"127.0.0.1", 3306}, "u\nx"},
	} {
		if _, err := mysqlArgs(bad.ep, bad.user, time.Second); err == nil {
			t.Errorf("%+v 应被拒绝", bad)
		}
	}
	// 只读表：任何偏离形状的 mysql argv（更换语句、追加选项、-p 口令）都被拒绝
	for _, bad := range [][]string{
		append(append([]string{}, args[:12]...), "DROP DATABASE x"),
		append(append([]string{}, args...), "--execute=DROP"),
		{"-e", "SELECT 1"},
		append(append([]string{}, args[:12]...), "SELECT VERSION(); DROP TABLE x"),
		{"--no-defaults", "--protocol=tcp", "-h", "127.0.0.1", "-P", "3306", "-u", "u", "--connect-timeout=3", "-N", "-B", "-psecret", "SELECT VERSION()"},
	} {
		if mysqlArgvAllowed(bad) {
			t.Errorf("%v 不应被允许", bad)
		}
	}
	if _, _, err := readOnlyRun(context.Background(), mustRunner(t), "mysql", []string{"-e", "DROP DATABASE x"}); !errors.Is(err, ErrNotReadOnly) {
		t.Errorf("readOnlyRun 应拒绝: %v", err)
	}
}

func mustRunner(t *testing.T) *safeexec.Runner {
	t.Helper()
	r, err := NewRunner(nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSanitizeRemovesSecrets(t *testing.T) {
	err := fmt.Errorf("dial failed: pw=hunter2-secret-value token=abc user=x")
	s := sanitize(err, "hunter2-secret-value")
	if strings.Contains(s, "hunter2-secret-value") {
		t.Errorf("口令未被剔除: %s", s)
	}
	if sanitize(nil) != "" {
		t.Error("nil")
	}
	if long := sanitize(fmt.Errorf("%s", strings.Repeat("x", 1000))); len(long) > 310 {
		t.Errorf("过长: %d", len(long))
	}
}

// ---- 小工具 ----

func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	t.Skip("本机没有非环回 IPv4 地址")
	return ""
}
