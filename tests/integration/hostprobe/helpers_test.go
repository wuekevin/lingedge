package hostprobeit

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

const fixtureRoot = "../../fixtures/hostprobe"

func realBinary(t *testing.T, name string) string {
	t.Helper()
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if p := "/opt/homebrew/opt/mysql@8.0/bin/" + name; fileExists(p) {
		return p
	}
	t.Skipf("等待环境验证: 本机缺少 %s", name)
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func runnerFor(t *testing.T, bins ...string) *safeexec.Runner {
	t.Helper()
	var dirs []string
	for _, b := range bins {
		dirs = append(dirs, filepath.Dir(b))
	}
	r, err := probe.NewRunner(dirs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func addr(ep probe.Endpoint) string { return net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port)) }

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

// listenAnd 起一个只在回环上监听的服务器，对每个连接执行 h：用于「协议不对/沉默」这类真实服务器不会出现的对端。
func listenAnd(t *testing.T, h func(net.Conn)) probe.Endpoint {
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
			go func() { defer func() { _ = c.Close() }(); h(c) }()
		}
	}()
	return probe.Endpoint{Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port}
}

// ---- 测试用的最小 RESP 客户端（只用于读 INFO commandstats 做「只读证明」）----

func respEncode(args ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return []byte(b.String())
}

func respBulk(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil || len(line) < 3 {
		t.Fatalf("RESP: %q %v", line, err)
	}
	if line[0] != '$' && line[0] != '+' && line[0] != '-' {
		t.Fatalf("RESP: %q", line)
	}
	if line[0] != '$' {
		return strings.TrimSpace(line[1:])
	}
	n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	buf := make([]byte, n+2)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func redisCommandstats(t *testing.T, ep probe.Endpoint, password string) map[string]int {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr(ep), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	if password != "" {
		_, _ = c.Write(respEncode("AUTH", password))
		respBulk(t, r)
	}
	_, _ = c.Write(respEncode("INFO", "commandstats"))
	out := map[string]int{}
	for _, ln := range strings.Split(respBulk(t, r), "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "cmdstat_") {
			continue
		}
		name, rest, _ := strings.Cut(ln[len("cmdstat_"):], ":")
		for _, kv := range strings.Split(rest, ",") {
			if v, ok := strings.CutPrefix(kv, "calls="); ok {
				n, _ := strconv.Atoi(v)
				out[name] = n
			}
		}
	}
	return out
}

// ---- 外部程序 ss/systemctl 的替身（macOS 没有它们）：记录 argv 并输出夹具内容 ----

type scripts struct{ dir, ssLog, sysLog string }

func newScripts(t *testing.T) scripts {
	t.Helper()
	dir := t.TempDir()
	abs, _ := filepath.Abs(fixtureRoot)
	s := scripts{dir: dir, ssLog: filepath.Join(dir, "ss.log"), sysLog: filepath.Join(dir, "systemctl.log")}
	for name, spec := range map[string][2]string{"ss": {s.ssLog, "ss/ss-lntup-root.txt"}, "systemctl": {s.sysLog, "systemctl/list-units.txt"}} {
		body := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\ncat %q\n", spec[0], filepath.Join(abs, spec[1]))
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
