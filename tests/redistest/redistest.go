//go:build !windows

// Package redistest 为集成测试提供「独立临时 redis-server 实例」（Story 1.10、ADR-025 验证节）。
//
// 铁律：绝不触碰本机已在运行的 Redis（若有）及其数据/端口。每个实例：独立回环端口、独立临时目录、
// --save "" --appendonly no（无持久化）、--protected-mode yes 且仅绑定 127.0.0.1；测试结束 SIGTERM 关闭并删除目录；
// 上次异常退出遗留的实例（属主测试进程已不存在，且命令行确含该目录）会在下次启动时清理。
//
// 提供故障注入：Kill（SIGKILL，模拟崩溃）、Stop/Restart（优雅停止与同端口重启，数据丢失）、
// Pause/Resume（SIGSTOP/SIGCONT，模拟「连得上但不响应」的超时故障）、FlushAll。
//
// 可用性开关：找不到 redis-server → t.Skip，消息以「等待环境验证」开头；LINGEDGE_REQUIRE_REDIS=1 时视为失败；
// LINGEDGE_REDIS_TESTS=off 显式跳过；LINGEDGE_REDIS_SERVER=<路径> 指定可执行文件。
package redistest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// 环境变量开关。
const (
	EnvTests   = "LINGEDGE_REDIS_TESTS"
	EnvRequire = "LINGEDGE_REQUIRE_REDIS"
	EnvServer  = "LINGEDGE_REDIS_SERVER"
)

// SkipPrefix 是「环境缺失而跳过」的统一消息前缀。
const SkipPrefix = "等待环境验证: "

const dirPrefix = "leredis"

// Options 是实例形态。
type Options struct {
	Password string // 非空则 requirepass
	TLS      bool   // 仅 TLS 端口（--port 0）；CA 证书见 Instance.CAFile
}

// Instance 是一个独立临时 redis-server。
type Instance struct {
	Dir    string
	Port   int
	Opts   Options
	CAFile string // TLS 时的 CA（PEM）

	bin    string
	cmd    *exec.Cmd
	exited chan struct{}
	mu     sync.Mutex
}

var (
	mu       sync.Mutex
	live     []*Instance
	reaped   bool
	mainUsed bool
)

// Main 运行测试并在结束时关闭、清理全部实例。
func Main(m *testing.M) int {
	mu.Lock()
	mainUsed = true
	mu.Unlock()
	code := m.Run()
	mu.Lock()
	for _, in := range live {
		in.destroy()
	}
	live = nil
	mu.Unlock()
	return code
}

// Version 返回 redis-server --version 的原文（供开发记录，版本只写能确认的）。
func Version() string {
	bin, err := find()
	if err != nil {
		return ""
	}
	out, _ := exec.Command(bin, "--version").Output()
	return strings.TrimSpace(string(out))
}

// Start 启动一个新实例（每个测试一个，互不影响）。环境不满足时 t.Skip（见包说明）。
func Start(t testing.TB, opts ...Options) *Instance {
	t.Helper()
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if v := strings.ToLower(os.Getenv(EnvTests)); v == "off" || v == "0" || v == "false" {
		t.Skip(SkipPrefix + EnvTests + "=" + v + "，已显式关闭 Redis 集成测试；本测试未执行，不代表通过")
	}
	mu.Lock()
	if !mainUsed {
		mu.Unlock()
		t.Fatal("redistest: 测试包必须在 TestMain 中调用 os.Exit(redistest.Main(m))，否则无法保证实例被关闭并清理")
	}
	if !reaped {
		reapStale()
		reaped = true
	}
	mu.Unlock()
	bin, err := find()
	if err != nil {
		if os.Getenv(EnvRequire) == "1" {
			t.Fatalf("redistest: %s=1 要求 Redis 可用，但: %v", EnvRequire, err)
		}
		t.Skip(SkipPrefix + err.Error() + "；本测试未执行，不代表通过（设置 " + EnvRequire + "=1 可把此情形视为失败）")
	}
	dir, err := os.MkdirTemp("", dirPrefix)
	if err != nil {
		t.Fatal(err)
	}
	in := &Instance{Dir: dir, Opts: o, bin: bin}
	if err := os.WriteFile(filepath.Join(dir, "owner"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	if o.TLS {
		if err := in.writeTLS(); err != nil {
			_ = os.RemoveAll(dir)
			t.Fatal(err)
		}
	}
	mu.Lock()
	live = append(live, in)
	mu.Unlock()
	t.Cleanup(in.destroy)
	if err := in.launch(0); err != nil {
		t.Fatalf("redistest: 启动 redis-server 失败: %v", err)
	}
	return in
}

// Addr 返回 host:port。
func (in *Instance) Addr() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(in.Port)) }

func find() (string, error) {
	var cands []string
	if p := os.Getenv(EnvServer); p != "" {
		cands = append(cands, p)
	}
	if p, err := exec.LookPath("redis-server"); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, "/opt/homebrew/bin/redis-server", "/usr/local/bin/redis-server", "/usr/bin/redis-server")
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("未找到 redis-server（PATH 与常见位置均无；可用 " + EnvServer + " 指定）")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// launch 启动进程；port==0 表示新选端口。
func (in *Instance) launch(port int) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		p := port
		if p == 0 {
			var err error
			if p, err = freePort(); err != nil {
				return err
			}
		}
		args := []string{"--bind", "127.0.0.1", "--dir", in.Dir, "--save", "", "--appendonly", "no",
			"--daemonize", "no", "--protected-mode", "yes", "--logfile", filepath.Join(in.Dir, "redis.log"),
			"--maxmemory", "64mb", "--maxmemory-policy", "volatile-lru"}
		if in.Opts.Password != "" {
			args = append(args, "--requirepass", in.Opts.Password)
		}
		if in.Opts.TLS {
			args = append(args, "--port", "0", "--tls-port", strconv.Itoa(p),
				"--tls-cert-file", filepath.Join(in.Dir, "server.crt"), "--tls-key-file", filepath.Join(in.Dir, "server.key"),
				"--tls-ca-cert-file", in.CAFile, "--tls-auth-clients", "no")
		} else {
			args = append(args, "--port", strconv.Itoa(p))
		}
		cmd := exec.Command(in.bin, args...)
		exited := make(chan struct{})
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { _ = cmd.Wait(); close(exited) }()
		in.mu.Lock()
		in.cmd, in.exited, in.Port = cmd, exited, p
		in.mu.Unlock()
		if lastErr = in.waitListening(10 * time.Second); lastErr == nil {
			return nil
		}
		in.terminate(syscall.SIGKILL)
		if port != 0 { // 重启必须同端口，不换端口重试
			break
		}
	}
	return fmt.Errorf("%w%s", lastErr, in.logTail())
}

func (in *Instance) waitListening(d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		select {
		case <-in.exited:
			return errors.New("redis-server 提前退出")
		default:
		}
		if c, err := net.DialTimeout("tcp", in.Addr(), 200*time.Millisecond); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("等待 redis-server 监听超时")
}

func (in *Instance) terminate(sig syscall.Signal) {
	in.mu.Lock()
	cmd, exited := in.cmd, in.exited
	in.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	select {
	case <-exited:
		return
	default:
	}
	_ = cmd.Process.Signal(syscall.SIGCONT) // 若被 Pause，先唤醒才能响应信号
	_ = cmd.Process.Signal(sig)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
}

// Stop 优雅停止（SIGTERM），端口随即关闭。
func (in *Instance) Stop() { in.terminate(syscall.SIGTERM) }

// Kill 模拟崩溃（SIGKILL）。
func (in *Instance) Kill() { in.terminate(syscall.SIGKILL) }

// Restart 在同一端口重启（无持久化，数据全部丢失）；若仍在运行先停止。
func (in *Instance) Restart(t testing.TB) {
	t.Helper()
	in.Stop()
	if err := in.launch(in.Port); err != nil {
		t.Fatalf("redistest: 重启失败: %v", err)
	}
}

// Pause 暂停进程（SIGSTOP）：端口仍接受连接但不响应，用于超时注入。
func (in *Instance) Pause() {
	in.mu.Lock()
	defer in.mu.Unlock()
	_ = in.cmd.Process.Signal(syscall.SIGSTOP)
}

// Resume 继续进程（SIGCONT）。
func (in *Instance) Resume() {
	in.mu.Lock()
	defer in.mu.Unlock()
	_ = in.cmd.Process.Signal(syscall.SIGCONT)
}

// PID 返回进程号（供清理核对）。
func (in *Instance) PID() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.cmd.Process.Pid
}

// Log 返回服务端日志全文（供「口令不进日志」之类的断言）。
func (in *Instance) Log() string {
	b, _ := os.ReadFile(filepath.Join(in.Dir, "redis.log"))
	return string(b)
}

func (in *Instance) logTail() string {
	s := in.Log()
	if len(s) > 1500 {
		s = s[len(s)-1500:]
	}
	if s == "" {
		return ""
	}
	return "\nredis 日志尾部:\n" + s
}

func (in *Instance) destroy() {
	in.terminate(syscall.SIGTERM)
	_ = os.RemoveAll(in.Dir)
}

func (in *Instance) writeTLS() error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "lingedge-redistest-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, _ := x509.ParseCertificate(caDER)
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	srvTpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "redis.test"},
		DNSNames: []string{"redis.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		return err
	}
	in.CAFile = filepath.Join(in.Dir, "ca.crt")
	for path, blk := range map[string]*pem.Block{
		in.CAFile:                           {Type: "CERTIFICATE", Bytes: caDER},
		filepath.Join(in.Dir, "server.crt"): {Type: "CERTIFICATE", Bytes: srvDER},
		filepath.Join(in.Dir, "server.key"): {Type: "EC PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// reapStale 清理上次异常退出遗留的实例：属主测试进程已不存在，且 redis-server 命令行确含该目录才会终止。
func reapStale() {
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), dirPrefix+"*"))
	for _, d := range dirs {
		ob, err := os.ReadFile(filepath.Join(d, "owner"))
		if err != nil {
			continue
		}
		owner, err := strconv.Atoi(strings.TrimSpace(string(ob)))
		if err != nil || owner == os.Getpid() {
			continue
		}
		if err := syscall.Kill(owner, 0); err == nil || errors.Is(err, syscall.EPERM) {
			continue
		}
		for _, pid := range pidsFor(d) {
			_ = syscall.Kill(pid, syscall.SIGCONT)
			_ = syscall.Kill(pid, syscall.SIGTERM)
			time.Sleep(200 * time.Millisecond)
			if syscall.Kill(pid, 0) == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = os.RemoveAll(d)
	}
}

func pidsFor(d string) []int {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	needle := "--dir " + d + " "
	var pids []int
	for _, line := range bytes.Split(out, []byte("\n")) {
		s := strings.TrimSpace(string(line))
		i := strings.IndexByte(s, ' ')
		if i < 0 || !strings.Contains(s[i:]+" ", needle) || !strings.Contains(s[i:], "redis-server") {
			continue
		}
		if pid, err := strconv.Atoi(s[:i]); err == nil && pid > 1 && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids
}
