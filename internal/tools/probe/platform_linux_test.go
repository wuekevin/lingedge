//go:build linux

package probe

import (
	"net"
	"os"
	"strings"
	"testing"
)

// ENV-LNX：真实 Linux 上的采集。本文件在 macOS 开发机上只参与编译（go vet GOOS=linux），不执行——执行与复验属 ENV-LNX（Story 1.45）。

func TestLinuxRealMemoryAndDisks(t *testing.T) {
	total, avail, _, err := osMemory()
	if err != nil || total <= 0 || avail < 0 || avail > total {
		t.Fatalf("memory: %d %d %v", total, avail, err)
	}
	b, _ := os.ReadFile("/proc/meminfo")
	if !strings.Contains(string(b), "MemTotal") {
		t.Fatal("/proc/meminfo 异常")
	}
	ds, _, err := osDisks()
	if err != nil || len(ds) == 0 {
		t.Fatalf("disks: %v %v", ds, err)
	}
	hasRoot := false
	for _, d := range ds {
		if d.Mount == "/" && d.TotalMB >= d.FreeMB {
			hasRoot = true
		}
	}
	if !hasRoot {
		t.Errorf("缺少 /: %+v", ds)
	}
}

// 真实 /proc：本进程自己的监听必须被 /proc/net + /proc/<pid>/fd 找到并识别为本进程。
func TestLinuxRealProcListeners(t *testing.T) {
	p := ProcFS{Root: "/proc", UserDB: "/etc/passwd", LE: hostLittleEndian()}
	ln, port := listenLoopback(t)
	defer func() { _ = ln.Close() }()
	socks, _, err := p.ListenersFromProc()
	if err != nil {
		t.Fatal(err)
	}
	s, ok := findSock(socks, "tcp", "127.0.0.1", port)
	if !ok || s.Listener.PID != os.Getpid() || s.Hidden {
		t.Fatalf("应找到本进程的监听: %+v", s)
	}
}

func listenLoopback(t *testing.T) (net.Listener, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l, l.Addr().(*net.TCPAddr).Port
}
