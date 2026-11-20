package sdnotify

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func listen(t *testing.T, name string) *net.UnixConn {
	t.Helper()
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func recv(t *testing.T, c *net.UnixConn) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1024)
	n, _, err := c.ReadFromUnix(buf)
	if err != nil {
		t.Fatalf("未收到通知数据报: %v", err)
	}
	return string(buf[:n])
}

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "sdn") // sun_path 上限约 104 字节
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// 测试自建 Unix 数据报 socket 验证 READY=1（经环境变量 NOTIFY_SOCKET，真实读取路径）。
func TestNotifyReadyViaEnvSocket(t *testing.T) {
	p := filepath.Join(shortDir(t), "n.sock")
	c := listen(t, p)
	t.Setenv(EnvSocket, p)
	sent, err := Notify(Ready)
	if err != nil || !sent {
		t.Fatalf("Notify: %v %v", sent, err)
	}
	if got := recv(t, c); got != "READY=1" {
		t.Fatalf("收到 %q", got)
	}
	if sent, err := Notify(Stopping); err != nil || !sent {
		t.Fatal(err)
	}
	if got := recv(t, c); got != "STOPPING=1" {
		t.Fatalf("收到 %q", got)
	}
}

// 非 systemd 环境（未设置/为空）：no-op，不报错、不创建任何东西。
func TestNotifyNoopWithoutSocket(t *testing.T) {
	t.Setenv(EnvSocket, "")
	if sent, err := Notify(Ready); sent || err != nil {
		t.Fatalf("应为 no-op: %v %v", sent, err)
	}
	if err := os.Unsetenv(EnvSocket); err != nil {
		t.Fatal(err)
	}
	if sent, err := Notify(Ready); sent || err != nil {
		t.Fatalf("未设置应为 no-op: %v %v", sent, err)
	}
}

// 抽象 socket（'@' 前缀）：Linux 支持；其他系统（如 macOS）没有抽象命名空间，只验证前缀被翻译为 NUL。
func TestAbstractSocketPrefix(t *testing.T) {
	a, err := resolve("@lingedge-test")
	if err != nil || a.Name != "\x00lingedge-test" {
		t.Fatalf("'@' 应翻译为 NUL: %+v %v", a, err)
	}
	if runtime.GOOS != "linux" {
		t.Skip("等待环境验证: 抽象 socket 的端到端收发仅 Linux 支持，本机非 Linux；前缀翻译已验证")
	}
	name := "@lingedge-sdnotify-" + strings.ReplaceAll(t.Name(), "/", "_") + time.Now().Format("150405.000000")
	c := listen(t, "\x00"+name[1:])
	sent, err := NotifyTo(name, Ready)
	if err != nil || !sent {
		t.Fatalf("NotifyTo: %v %v", sent, err)
	}
	if got := recv(t, c); got != "READY=1" {
		t.Fatalf("收到 %q", got)
	}
}

func TestNotifyRejectsBadInput(t *testing.T) {
	for _, s := range []string{"relative/path.sock", "vsock:1:2", "@"} {
		if sent, err := NotifyTo(s, Ready); sent || err == nil {
			t.Errorf("%q 应报错: %v %v", s, sent, err)
		}
	}
	if sent, err := NotifyTo("/nonexistent/n.sock", Ready); sent || err == nil {
		t.Errorf("连接失败应返回错误: %v %v", sent, err)
	}
	if sent, err := NotifyTo("/tmp/x", ""); sent || err == nil {
		t.Error("空状态应报错")
	}
	if sent, err := NotifyTo("/tmp/x", "A\x00B"); sent || err == nil {
		t.Error("含 NUL 的状态应报错")
	}
}
