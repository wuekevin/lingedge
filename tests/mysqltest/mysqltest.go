//go:build !windows

// Package mysqltest 为集成测试提供「独立临时 mysqld 实例」（Story 1.5 环境要求、ADR-023）。
//
// 铁律：绝不触碰本机已在运行的 MySQL 实例（其数据、账号、端口）与其他项目的数据。做法：
//   - 每个测试进程用 `mysqld --no-defaults --initialize-insecure` 初始化一个全新的临时 datadir，
//     在空闲的回环端口与 datadir 内的独立 socket 上启动（--no-defaults：不读 /etc/my.cnf 与 ~/.my.cnf；--mysqlx=OFF：不占 33060）；
//   - 默认开启 --log-bin 且 log_bin_trust_function_creators=0（生产形态；账号/触发器权限类用例依赖，database/README §5.1）；
//   - 测试结束（Main 返回前）SIGTERM 关闭该实例并删除 datadir；上次异常退出遗留的实例（属主测试进程已不存在）会在下次启动时按
//     「命令行含该 datadir」精确核对后清理，不会误杀其他进程。
//
// 可用性与开关（CI 无 MySQL 时必须干净地跳过，且不得假装通过）：
//   - 找不到 8.0 版 mysqld → t.Skip，消息以「等待环境验证」开头；
//   - LINGEDGE_MYSQL_TESTS=off（或 0/false）→ 显式跳过；
//   - LINGEDGE_REQUIRE_MYSQL=1 → 找不到 mysqld 或启动失败视为测试失败（有 MySQL 的 CI 不得静默跳过）；
//   - LINGEDGE_MYSQLD=<路径> 指定 mysqld（缺省按 PATH 与 Homebrew 路径查找）。
//
// 用法：测试包的 TestMain 调用 os.Exit(mysqltest.Main(m))；用例里 inst := mysqltest.Start(t) 取得共享实例，
// dbName := inst.NewDatabase(t) 得到用后即删的随机库。
package mysqltest

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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

	"github.com/go-sql-driver/mysql"
)

// 环境变量开关。
const (
	EnvTests   = "LINGEDGE_MYSQL_TESTS"
	EnvRequire = "LINGEDGE_REQUIRE_MYSQL"
	EnvMysqld  = "LINGEDGE_MYSQLD"
)

// SkipPrefix 是「环境缺失而跳过」的统一消息前缀（便于在 CI 日志里检索「等待环境验证」项）。
const SkipPrefix = "等待环境验证: "

const dirPrefix = "lemy" // 短前缀：Unix socket 路径上限约 104 字节（macOS）

// Options 是实例形态。零值即默认：开启 binlog、log_bin_trust_function_creators=0。
type Options struct {
	NoBinlog              bool // 关闭 binlog（此时建触发器不需要 SET_USER_ID）
	TrustFunctionCreators bool // log_bin_trust_function_creators=1
}

func (o Options) key() string {
	return fmt.Sprintf("nobinlog=%t,trust=%t", o.NoBinlog, o.TrustFunctionCreators)
}

// Instance 是一个独立临时 mysqld。
type Instance struct {
	Dir    string // 临时根目录（含 datadir、socket、日志）
	Socket string
	Port   int
	Opts   Options

	mysqld string
	cmd    *exec.Cmd
	exited chan struct{}
	waitEr error
}

var (
	mu        sync.Mutex
	instances = map[string]*Instance{}
	mainUsed  bool
	envErr    = map[string]error{} // 环境缺失（找不到 mysqld）：跳过或按 EnvRequire 失败
	startErr  = map[string]error{} // 启动失败：一律失败，不得被当作「环境缺失」而跳过
)

// Main 运行测试并在结束时关闭、清理所有由本包启动的实例；返回值交给 os.Exit。
func Main(m *testing.M) int {
	mu.Lock()
	mainUsed = true
	mu.Unlock()
	code := m.Run()
	StopAll()
	return code
}

// StopAll 关闭并清理本进程启动的全部实例（Main 已调用；仅在不使用 Main 的特殊场景下手动调用）。
func StopAll() {
	mu.Lock()
	defer mu.Unlock()
	for k, in := range instances {
		in.stop()
		delete(instances, k)
	}
}

// Start 返回共享实例（首次调用时初始化并启动，耗时数秒到数十秒）。环境不满足时 t.Skip（见包说明）。
func Start(t testing.TB, opts ...Options) *Instance {
	t.Helper()
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if v := strings.ToLower(os.Getenv(EnvTests)); v == "off" || v == "0" || v == "false" {
		t.Skip(SkipPrefix + EnvTests + "=" + v + "，已显式关闭 MySQL 集成测试；本测试未执行，不代表通过")
	}
	mu.Lock()
	defer mu.Unlock()
	if !mainUsed {
		t.Fatal("mysqltest: 测试包必须在 TestMain 中调用 os.Exit(mysqltest.Main(m))，否则无法保证临时实例被关闭并清理")
	}
	if in, ok := instances[o.key()]; ok {
		return in
	}
	if err, ok := startErr[o.key()]; ok {
		t.Fatalf("mysqltest: 临时 mysqld 此前已启动失败: %v", err)
	}
	if err, ok := envErr[o.key()]; ok {
		failOrSkip(t, err)
	}
	bin, err := findMysqld()
	if err != nil {
		envErr[o.key()] = err
		failOrSkip(t, err)
	}
	reapStale()
	in, err := startInstance(bin, o)
	if err != nil {
		startErr[o.key()] = err
		t.Fatalf("mysqltest: 启动临时 mysqld 失败: %v", err)
	}
	instances[o.key()] = in
	return in
}

func failOrSkip(t testing.TB, err error) {
	t.Helper()
	if os.Getenv(EnvRequire) == "1" {
		t.Fatalf("mysqltest: %s=1 要求 MySQL 可用，但: %v", EnvRequire, err)
	}
	t.Skip(SkipPrefix + err.Error() + "；本测试未执行，不代表通过（设置 " + EnvRequire + "=1 可把此情形视为失败）")
}

func findMysqld() (string, error) {
	var candidates []string
	if p := os.Getenv(EnvMysqld); p != "" {
		candidates = append(candidates, p)
	}
	if p, err := exec.LookPath("mysqld"); err == nil {
		candidates = append(candidates, p)
	}
	candidates = append(candidates,
		"/opt/homebrew/opt/mysql@8.0/bin/mysqld",
		"/usr/local/opt/mysql@8.0/bin/mysqld",
		"/usr/sbin/mysqld", "/usr/libexec/mysqld", "/usr/local/mysql/bin/mysqld")
	var why []string
	for _, c := range candidates {
		out, err := exec.Command(c, "--version").CombinedOutput()
		if err != nil {
			why = append(why, c+": 无法执行")
			continue
		}
		if !bytes.Contains(out, []byte("Ver 8.0.")) {
			why = append(why, c+": 版本不是 8.0.x")
			continue
		}
		return c, nil
	}
	if len(why) == 0 {
		why = []string{"PATH 与常见位置均无 mysqld"}
	}
	return "", fmt.Errorf("未找到可用的 mysqld 8.0（%s；可用 %s 指定）", strings.Join(why, "；"), EnvMysqld)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func startInstance(mysqld string, o Options) (*Instance, error) {
	dir, err := os.MkdirTemp("", dirPrefix)
	if err != nil {
		return nil, err
	}
	in := &Instance{Dir: dir, Socket: filepath.Join(dir, "m.sock"), Opts: o, mysqld: mysqld, exited: make(chan struct{})}
	if err := os.WriteFile(filepath.Join(dir, "owner"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	fail := func(err error) (*Instance, error) {
		tail := in.logTail() // 先读日志再删目录
		in.stop()
		return nil, fmt.Errorf("%w%s", err, tail)
	}

	datadir := filepath.Join(dir, "data")
	real, err := filepath.EvalSymlinks(mysqld) // 经符号链接（如 /opt/homebrew/bin/mysqld）找到时，basedir 以真实路径为准
	if err != nil {
		real = mysqld
	}
	base := filepath.Dir(filepath.Dir(real))
	common := []string{"--no-defaults", "--basedir=" + base, "--datadir=" + datadir}
	if os.Geteuid() == 0 {
		common = append(common, "--user=root")
	}
	initArgs := append(append([]string{}, common...), "--initialize-insecure", "--log-error="+filepath.Join(dir, "init.err"))
	if out, err := exec.Command(mysqld, initArgs...).CombinedOutput(); err != nil {
		return fail(fmt.Errorf("初始化 datadir 失败: %v: %s", err, bytes.TrimSpace(out)))
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ { // 端口在探测与绑定之间可能被抢占，重试
		port, err := freePort()
		if err != nil {
			return fail(err)
		}
		in.Port = port
		args := append(append([]string{}, common...),
			"--port="+strconv.Itoa(port), "--bind-address=127.0.0.1", "--socket="+in.Socket,
			"--mysqlx=OFF", "--log-error="+filepath.Join(dir, "err.log"), "--pid-file="+filepath.Join(dir, "m.pid"),
			"--performance-schema=OFF", "--innodb-buffer-pool-size=64M", "--innodb-flush-log-at-trx-commit=2",
			"--max-connections=100", "--server-id=1")
		if !o.NoBinlog {
			args = append(args, "--log-bin="+filepath.Join(dir, "binlog"))
		} else {
			args = append(args, "--skip-log-bin")
		}
		if o.TrustFunctionCreators {
			args = append(args, "--log-bin-trust-function-creators=1")
		} else {
			args = append(args, "--log-bin-trust-function-creators=0")
		}
		cmd := exec.Command(mysqld, args...)
		in.exited = make(chan struct{})
		if err := cmd.Start(); err != nil {
			return fail(err)
		}
		in.cmd = cmd
		go func(c *exec.Cmd, ch chan struct{}) { in.waitEr = c.Wait(); close(ch) }(cmd, in.exited)
		if lastErr = in.waitReady(90 * time.Second); lastErr == nil {
			return in, nil
		}
		in.terminate()
	}
	return fail(lastErr)
}

func (in *Instance) rootDSN() *mysql.Config {
	c := mysql.NewConfig()
	c.User, c.Net, c.Addr = "root", "unix", in.Socket
	c.AllowNativePasswords = true
	return c
}

func (in *Instance) waitReady(d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		select {
		case <-in.exited:
			return fmt.Errorf("mysqld 提前退出: %v", in.waitEr)
		default:
		}
		if _, err := os.Stat(in.Socket); err == nil {
			if db, err := sql.Open("mysql", in.rootDSN().FormatDSN()); err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				err = db.PingContext(ctx)
				cancel()
				_ = db.Close()
				if err == nil {
					return nil
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("等待 mysqld 就绪超时")
}

// terminate 优雅关闭 mysqld（SIGTERM，超时 SIGKILL）并等待其退出。
func (in *Instance) terminate() {
	if in.cmd == nil || in.cmd.Process == nil {
		return
	}
	select {
	case <-in.exited:
		return
	default:
	}
	_ = in.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-in.exited:
	case <-time.After(60 * time.Second):
		_ = in.cmd.Process.Kill()
		<-in.exited
	}
}

func (in *Instance) stop() {
	in.terminate()
	_ = os.RemoveAll(in.Dir)
}

func (in *Instance) logTail() string {
	b, err := os.ReadFile(filepath.Join(in.Dir, "err.log"))
	if err != nil {
		return ""
	}
	if len(b) > 2000 {
		b = b[len(b)-2000:]
	}
	return "\nmysqld 日志尾部:\n" + string(b)
}

// reapStale 清理上次异常退出遗留的临时实例：属主测试进程已不存在，且 mysqld 命令行确含该 datadir 才会终止。
func reapStale() {
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), dirPrefix+"*"))
	for _, d := range dirs {
		ob, err := os.ReadFile(filepath.Join(d, "owner"))
		if err != nil {
			continue // 不是本包创建的目录
		}
		owner, err := strconv.Atoi(strings.TrimSpace(string(ob)))
		if err != nil || owner == os.Getpid() {
			continue
		}
		if err := syscall.Kill(owner, 0); err == nil || errors.Is(err, syscall.EPERM) {
			continue // 属主仍在运行（可能是并行的另一个测试包）
		}
		for _, pid := range mysqldPIDsFor(d) {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			for i := 0; i < 100 && syscall.Kill(pid, 0) == nil; i++ {
				time.Sleep(100 * time.Millisecond)
			}
			if syscall.Kill(pid, 0) == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = os.RemoveAll(d)
	}
}

// mysqldPIDsFor 返回命令行含「--datadir=<d>/data」的进程 pid（精确核对，不误伤其他进程）。
// 不依赖 m.pid：初始化阶段崩溃或 pid 文件尚未写出时，遗留的 mysqld 同样能被找到。
func mysqldPIDsFor(d string) []int {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	needle := "--datadir=" + filepath.Join(d, "data")
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		i := strings.IndexByte(line, ' ')
		if i < 0 || !strings.Contains(line[i:], needle) {
			continue
		}
		if pid, err := strconv.Atoi(line[:i]); err == nil && pid > 1 && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids
}

// ---- 使用方便函数 ----

// Host 返回 TCP 连接目标主机（回环）；端口见 Port 字段。
func (in *Instance) Host() string { return "127.0.0.1" }

// Admin 返回以 root（无口令，仅限本临时实例的 Unix socket）连接的 *sql.DB，测试结束自动关闭。
func (in *Instance) Admin(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", in.rootDSN().FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Exec 以 root 执行语句（DDL/授权），失败即终止测试。
func (in *Instance) Exec(t testing.TB, stmt string, args ...any) {
	t.Helper()
	if _, err := in.Admin(t).ExecContext(context.Background(), stmt, args...); err != nil {
		t.Fatalf("执行失败: %v\n%s", err, firstLine(stmt))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// RandomName 返回带前缀的随机小写标识符（<=32 字符），用于库名/账号名。
func RandomName(prefix string) string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// RandomSecret 返回随机口令（运行期生成，源码与仓库里不存在任何固定口令，NFR-6）。
func RandomSecret(t testing.TB) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// NewDatabase 创建随机名的库（utf8mb4），测试结束后删除；返回库名。
func (in *Instance) NewDatabase(t testing.TB) string {
	t.Helper()
	name := RandomName("lingedge_test")
	in.Exec(t, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	t.Cleanup(func() {
		if db, err := sql.Open("mysql", in.rootDSN().FormatDSN()); err == nil {
			_, _ = db.Exec("DROP DATABASE IF EXISTS `" + name + "`")
			_ = db.Close()
		}
	})
	return name
}

// DropUserOnCleanup 在测试结束时删除账号（账号是全局对象，不随库删除）。
func (in *Instance) DropUserOnCleanup(t testing.TB, user string) {
	t.Helper()
	t.Cleanup(func() {
		if db, err := sql.Open("mysql", in.rootDSN().FormatDSN()); err == nil {
			_, _ = db.Exec("DROP USER IF EXISTS '" + user + "'@'%'")
			_ = db.Close()
		}
	})
}
