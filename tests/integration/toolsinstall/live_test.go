package toolsinstall

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/cli"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/pkg/setuptoken"
	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// ---- 构建真实二进制（整个包只构建一次）----

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

func serverBin(t testing.TB) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "letoolsbin")
		if err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "lingedge-server")
		out, err := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binPath, "github.com/wuekevin/lingedge/cmd/lingedge-server").CombinedOutput()
		if err != nil {
			binErr = fmt.Errorf("构建 lingedge-server 失败: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

func rnd(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func freePorts(t testing.TB, n int) []int {
	t.Helper()
	var ls []net.Listener
	var out []int
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		out = append(out, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range ls {
		_ = l.Close()
	}
	return out
}

// ---- 世界与命令 ----

type env struct {
	t     *testing.T
	w     *toolsworld.World
	spec  *liveServer
	stdin string
	ports []int // rest, grpc, enroll, metrics
	cfg   string
	log   string // Tools 日志文件（失败时转储）
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newEnv(t *testing.T, crash bool) *env {
	t.Helper()
	w, err := toolsworld.NewWorldWith(t.TempDir(), toolsworld.Options{ServerBinary: serverBin(t), ProductionServerSpec: true})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, w: w, ports: freePorts(t, 4)}
	e.spec = &liveServer{ComponentSpec: installer.ProductionSpecs()["server"], t: t, w: w, crash: crash, logs: &syncBuf{}}
	w.Specs["server"] = e.spec
	t.Cleanup(e.spec.stop)
	e.cfg = filepath.Join(t.TempDir(), "tools.yaml")
	if err := os.WriteFile(e.cfg, []byte("install:\n  health_timeout: 40s\n  health_interval: 200ms\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

// run 经真实 cli.Execute 运行 lingedge 命令；uid 0 = 伪 root（测试进程自身非 root：chown 是记录型伪实现，其余文件操作都是真实的）。
func (e *env) run(uid int, args ...string) (code int, stdout, stderr string) {
	e.t.Helper()
	r, err := service.NewRunner([]string{e.w.Bin})
	if err != nil {
		e.t.Fatal(err)
	}
	var so, se syncBuf
	ev := cli.Env{
		Stdin: strings.NewReader(e.stdin), Stdout: &so, Stderr: &se, Getenv: func(string) string { return "" },
		Geteuid: func() int { return uid }, Hostname: func() (string, error) { return "edge-host-1", nil },
		Now: func() time.Time { return toolsworld.T0 }, NewRunID: func(time.Time) string { return "01JD5Y3C8Q5G7M0V1N2P3R4S5T" },
		StatePath: e.w.Layout.StateFile(), BuiltinKeys: func() ([]byte, error) { return toolsworld.KeysJSON(), nil },
		Install: cli.InstallEnv{Root: e.w.Root, Specs: e.w.Specs, Runner: r, Own: e.w.Own, Booted: func() bool { return true },
			Collect: func(context.Context) (probe.Result, error) { return e.w.Probe, nil }},
	}
	code = cli.Execute(context.Background(), ev, args)
	return code, so.String(), se.String()
}

func (e *env) installArgs(extra ...string) []string {
	e.log = filepath.Join(e.t.TempDir(), "tools.log")
	a := []string{"install", "server", "--offline", e.w.Bundle, "--config", e.cfg, "--json", "--non-interactive", "--log-file", e.log,
		"--set", "port.server.tcp.8080=" + strconv.Itoa(e.ports[0]),
		"--set", "port.server.tcp.9443=" + strconv.Itoa(e.ports[1]),
		"--set", "port.server.tcp.9444=" + strconv.Itoa(e.ports[2]),
		"--set", "port.server.tcp.9180=" + strconv.Itoa(e.ports[3])}
	return append(a, extra...)
}

func (e *env) host(virtual string) string { return filepath.Join(e.w.Root, virtual) }

// ---- 真实进程：按已安装的单元文件启动 lingedge-server ----

type liveServer struct {
	installer.ComponentSpec // 生产规格（Name/Executables/ValidateSettings/InitConfig/Health 全部是生产实现）
	t                       *testing.T
	w                       *toolsworld.World
	crash                   bool
	logs                    *syncBuf
	// external 为 true：进程由伪 systemd（sysd 包装的 Runner）在 systemctl start/stop 时启停；Start 不再自己拉起进程（升级/回滚测试）。
	external bool
	// failVersions：current 指向这些 release 版本时，真实进程以读不到配置而启动失败（构造「健康检查失败的新版本」）。
	failVersions map[string]bool

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan error
}

var (
	reExec = regexp.MustCompile(`(?m)^ExecStart=(\S+)\s+--config\s+(\S+)\s+(.*)$`)
	reCred = regexp.MustCompile(`(?m)^LoadCredential=master\.key:(\S+)$`)
)

// Start = 生产 Start（伪 systemctl start，标记单元 active）+ 真实进程。进程的启动方式取自**已安装的单元文件**（ExecStart、LoadCredential），
// 路径前缀到临时根；server.yaml 取自已安装的文件，只做测试环境必需的改写（数据目录、令牌哈希路径、argon2 下限），不改监听端口。
func (s *liveServer) Start(ctx context.Context, rt *installer.Runtime) error {
	if err := s.ComponentSpec.Start(ctx, rt); err != nil {
		return err
	}
	if s.external {
		return nil
	}
	return s.launch()
}

// launch 按已安装的单元文件拉起真实 lingedge-server 进程。
func (s *liveServer) launch() error {
	// 与 systemctl start 一致：单元已在运行则什么都不做（幂等；绝不重复拉起第二个进程去抢端口）。
	if s.cmd != nil {
		if ex, _ := s.exited(); !ex {
			return nil
		}
	}
	unit, err := os.ReadFile(filepath.Join(s.w.Root, "etc/systemd/system/lingedge-server.service"))
	if err != nil {
		return fmt.Errorf("读取已安装的单元: %w", err)
	}
	m := reExec.FindSubmatch(unit)
	if m == nil {
		return fmt.Errorf("单元里没有 ExecStart:\n%s", unit)
	}
	execPath, cfgPath, rest := string(m[1]), string(m[2]), strings.Fields(string(m[3]))
	hostExec := filepath.Join(s.w.Root, execPath)
	if fi, err := os.Stat(hostExec); err != nil || fi.Mode().Perm()&0o100 == 0 {
		return fmt.Errorf("单元的 ExecStart %s 在已安装的 release 里不存在或不可执行: %v", execPath, err)
	}
	yml, err := os.ReadFile(filepath.Join(s.w.Root, cfgPath))
	if err != nil {
		return err
	}
	conf := strings.ReplaceAll(string(yml), service.ServerDataDir, filepath.Join(s.w.Root, service.ServerDataDir))
	conf += "auth:\n  argon2:\n    memory_kib: 19456\n    iterations: 2\n    parallelism: 1\n"
	conf = strings.Replace(conf, "secrets:\n", "secrets:\n  setup_token_hash_file: "+filepath.Join(s.w.Root, "etc/lingedge/server/setup-token.hash")+"\n", 1)
	live := filepath.Join(s.w.Root, "run", "server-live.yaml")
	if err := os.WriteFile(live, []byte(conf), 0o600); err != nil {
		return err
	}
	env := cleanEnv()
	if cm := reCred.FindSubmatch(unit); cm != nil { // LoadCredential 的语义：把 master.key 放进 $CREDENTIALS_DIRECTORY（0400）
		dir := filepath.Join(s.w.Root, "run", "creds")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		key, err := os.ReadFile(filepath.Join(s.w.Root, string(cm[1])))
		if err != nil {
			return err
		}
		_ = os.Remove(filepath.Join(dir, "master.key")) // 重新拉起（升级/回滚）时覆盖上一次的 0400 文件
		if err := os.WriteFile(filepath.Join(dir, "master.key"), key, 0o400); err != nil {
			return err
		}
		env = append(env, "CREDENTIALS_DIRECTORY="+dir)
	}
	args := append([]string{"--config", live}, rest...)
	if tg, _ := os.Readlink(filepath.Join(s.w.Root, "opt/lingedge/server/current")); s.crash || s.failVersions[strings.TrimPrefix(tg, "releases/")] {
		args[1] = filepath.Join(s.w.Root, "run", "does-not-exist.yaml") // 真实的启动失败：进程读不到配置，立即以非零退出
	}
	cmd := exec.Command(hostExec, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = s.logs, s.logs
	if err := cmd.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	s.cmd, s.done = cmd, make(chan error, 1)
	done := s.done
	s.mu.Unlock()
	go func() { done <- cmd.Wait() }()
	return nil
}

func (s *liveServer) exited() (bool, error) {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return false, nil
	}
	select {
	case err := <-done:
		done <- err
		return true, err
	default:
		return false, nil
	}
}

func (s *liveServer) stop() {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.mu.Unlock()
	if cmd == nil {
		return
	}
	if ex, _ := s.exited(); ex {
		return
	}
	_ = cmd.Process.Kill()
	select {
	case err := <-done:
		_ = err // 被 SIGKILL 终止的退出错误是预期的
	case <-time.After(10 * time.Second):
	}
}

// term 发送 SIGTERM 并返回退出码（正常模式优雅停机应为 0）。
func (s *liveServer) term() int {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.mu.Unlock()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		done <- err
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 0
	case <-time.After(30 * time.Second):
		s.t.Fatal("进程未在时限内退出")
		return -1
	}
}

func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LINGEDGE_") && !strings.HasPrefix(kv, "CREDENTIALS_DIRECTORY=") {
			out = append(out, kv)
		}
	}
	return out
}

// ---- envelope 读取 ----

func envelopeOf(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&m); err != nil || dec.More() {
		t.Fatalf("stdout 应恰为一个 JSON 文档: %v\n%s", err, stdout)
	}
	return m
}

func accessOf(t *testing.T, env map[string]any) map[string]string {
	t.Helper()
	d, _ := env["data"].(map[string]any)
	out := map[string]string{}
	for _, a := range d["access"].([]any) {
		m := a.(map[string]any)
		out[m["kind"].(string)] = m["value"].(string)
	}
	return out
}

// ---- 向导 HTTP ----

const wizAPI = "/api/v1/public/setup/"

type wizard struct {
	t     *testing.T
	base  string
	token string
	log   []string // 全部响应体（泄漏扫描）
}

func (w *wizard) do(method, path, token string, body any) (int, map[string]any) {
	w.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, w.base+path, rd)
	if err != nil {
		w.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Setup-Token", token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	w.log = append(w.log, string(raw))
	return res.StatusCode, m
}

func (w *wizard) step(method, name string, body any, what string) map[string]any {
	w.t.Helper()
	code, m := w.do(method, wizAPI+name, w.token, body)
	if code != 200 {
		w.t.Fatalf("向导步骤 %s: 期望 200，实际 %d %v", what, code, m)
	}
	return m
}

// 临时 MySQL/Redis 环境（与 tests/integration/setup 同一套约定）。
type backends struct {
	inst                           *mysqltest.Instance
	rds                            *redistest.Instance
	adm                            *sql.DB
	dbName, adminUser, adminPW     string
	appPW, maintPW, migPW, adminWW string
	admEmail                       string
}

func startBackends(t *testing.T) *backends {
	t.Helper()
	b := &backends{inst: mysqltest.Start(t), rds: redistest.Start(t)}
	b.adm = b.inst.Admin(t)
	clean := func() {
		for _, u := range []string{"lingedge_app", "lingedge_maint", "lingedge_migrate"} {
			_, _ = b.adm.Exec("DROP USER IF EXISTS '" + u + "'@'%'")
		}
	}
	clean()
	t.Cleanup(clean)
	b.dbName = "lingedge_ti_" + rnd(4)
	t.Cleanup(func() { _, _ = b.adm.Exec("DROP DATABASE IF EXISTS `" + b.dbName + "`") })
	b.adminUser, b.adminPW = "le_adm_"+rnd(4), rnd(12)
	for _, q := range []string{
		fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", b.adminUser, b.adminPW),
		fmt.Sprintf("GRANT ALL ON *.* TO '%s'@'%%' WITH GRANT OPTION", b.adminUser),
	} {
		if _, err := b.adm.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = b.adm.Exec("DROP USER IF EXISTS '" + b.adminUser + "'@'%'") })
	b.appPW, b.maintPW, b.migPW = rnd(16), rnd(16), rnd(16)
	b.admEmail, b.adminWW = "admin"+rnd(3)+"@example.test", "Zq"+rnd(10)+"!x"
	return b
}

func (b *backends) secrets() []string {
	return []string{b.adminPW, b.appPW, b.maintPW, b.migPW, b.adminWW}
}

func (b *backends) runWizard(w *wizard) map[string]any {
	w.t.Helper()
	w.step("POST", "checks", nil, "checks")
	w.step("PUT", "database", map[string]any{"host": b.inst.Host(), "port": b.inst.Port, "database": b.dbName, "username": "lingedge_app", "password": b.appPW,
		"create_if_missing": true, "admin_username": b.adminUser, "admin_password": b.adminPW, "migrate_password": b.migPW, "maint_password": b.maintPW}, "database")
	w.step("PUT", "redis", map[string]any{"address": b.rds.Addr(), "db": 0}, "redis")
	w.step("POST", "initialize", nil, "initialize")
	w.step("POST", "admin", map[string]any{"email": b.admEmail, "display_name": "Root Admin", "password": b.adminWW}, "admin")
	w.step("PUT", "system", map[string]any{"run_mode": "self_hosted", "base_url": "https://edge.example.test", "timezone": "Asia/Shanghai", "default_locale": "zh-CN",
		"brand": map[string]any{"name": "测试云", "name_en": "TestCloud"}}, "system")
	w.step("POST", "keys", nil, "keys")
	return w.step("POST", "complete", nil, "complete")
}

func walkFiles(t *testing.T, root string, fn func(rel string, b []byte)) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if strings.HasPrefix(rel, "opt/") || strings.HasPrefix(rel, "bin/") { // 包内二进制与伪二进制脚本不扫
			return nil
		}
		b, _ := os.ReadFile(p)
		fn(rel, b)
		return nil
	})
}

// ---- 用例 ----

// AC1 + AC6：真实 lingedge install server → 真实 lingedge-server 以 setup mode 启动并通过 /healthz → Tools 生成的 token 驱动真实向导（MySQL/Redis 临时实例）→
// 向导 complete 写入 install.lock 并在同一进程内切换为正常模式 → lingedge list 显示 server 已初始化。全程 Tools 不写 migrate.yaml、不接触数据库口令。
func TestInstallServerThenRealWizard(t *testing.T) {
	be := startBackends(t)
	e := newEnv(t, false)

	code, so, se := e.run(0, e.installArgs()...)
	if code != 0 {
		t.Fatalf("install server 退出码 %d\nstdout=%s\nstderr=%s\nserver 日志:\n%s", code, so, se, e.spec.logs.String())
	}
	installOut := so + se
	env := envelopeOf(t, so)
	acc := accessOf(t, env)
	token := acc["setup_token"]
	if want := "http://127.0.0.1:" + strconv.Itoa(e.ports[0]) + "/"; acc["setup_url"] != want {
		t.Errorf("向导地址 %q，期望 %q", acc["setup_url"], want)
	}
	if !strings.Contains(acc["ssh_tunnel"], "ssh -L") || !strings.Contains(acc["master_key_warning"], "分开保管") || acc["master_key_fingerprint"] == "" {
		t.Errorf("SSH 隧道/主密钥提示/指纹缺失: %v", acc)
	}
	if len(token) != 43 {
		t.Fatalf("--json 交付的 token 应是真实 43 字符令牌，实际 %q", token)
	}
	hash, err := os.ReadFile(e.host("etc/lingedge/server/setup-token.hash"))
	if err != nil || !setuptoken.Verify(token, string(hash)) {
		t.Fatalf("setup-token.hash 与交付的 token 不对应: %v", err)
	}
	if strings.Contains(so+se, hex.EncodeToString([]byte(token))) || bytes.Contains(hash, []byte(token)) {
		t.Error("token 不得落盘")
	}

	// 安装步骤本身：不写 migrate.yaml、不含任何口令（AC6 ①）；文件权限。
	walkFiles(t, e.w.Root, func(rel string, b []byte) {
		if strings.Contains(rel, "migrate.yaml") || strings.Contains(rel, "runtime-credentials") {
			t.Errorf("安装不得写入 %s", rel)
		}
		if strings.HasPrefix(rel, "etc/") && strings.Contains(strings.ToLower(string(b)), "password") {
			t.Errorf("%s 不得含口令字样", rel)
		}
	})
	for rel, mode := range map[string]os.FileMode{"etc/lingedge/server/master.key": 0o400, "etc/lingedge/server/server.yaml": 0o640, "etc/lingedge/server/setup-token.hash": 0o600} {
		if fi, err := os.Stat(e.host(rel)); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v，期望 %04o", rel, fi, err, mode)
		}
	}

	// 真实 /healthz（生产 Health 已在步骤 20 通过；这里再独立确认，并确认 setup mode 的形态）。
	base := "http://127.0.0.1:" + strconv.Itoa(e.ports[0])
	for _, p := range []struct {
		path string
		want int
	}{{"/healthz", 200}, {"/healthz/ready", 503}} {
		res, err := http.Get(base + p.path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != p.want {
			t.Errorf("GET %s = %d，期望 %d（setup mode）", p.path, res.StatusCode, p.want)
		}
	}
	if ex, _ := e.spec.exited(); ex {
		t.Fatalf("server 进程不应已退出\n%s", e.spec.logs.String())
	}

	// lingedge list：向导未完成。
	listPhase := func() string {
		code, so, se := e.run(1000, "list", "--json")
		if code != 0 {
			t.Fatalf("list: %d %s %s", code, so, se)
		}
		d := envelopeOf(t, so)["data"].(map[string]any)
		c := d["components"].([]any)[0].(map[string]any)
		if c["name"] != "server" || c["state"] != "running" {
			t.Errorf("list 应显示 server running: %v", c)
		}
		return c["setup"].(map[string]any)["phase"].(string)
	}
	if ph := listPhase(); ph != "wizard_pending" {
		t.Fatalf("向导完成前 list 的 phase=%q", ph)
	}

	// F-8：重置令牌（真实 config setup-token --reset）→ 旧令牌被真实 Server 拒绝（401），新令牌可用；Server 不需要重启。
	w := &wizard{t: t, base: base, token: token}
	if code, _ := w.do("GET", wizAPI+"status", token, nil); code != 200 {
		t.Fatalf("Tools 生成的 token 应被真实 Server 接受: %d", code)
	}
	code, so, se = e.run(0, "config", "setup-token", "--reset", "--yes", "--json", "--config", e.cfg)
	if code != 0 {
		t.Fatalf("setup-token --reset: %d %s %s", code, so, se)
	}
	newTok := accessOf(t, envelopeOf(t, so))["setup_token"]
	if len(newTok) != 43 || newTok == token {
		t.Fatalf("新 token: %q", newTok)
	}
	if code, _ := w.do("GET", wizAPI+"status", token, nil); code != 401 {
		t.Errorf("旧 token 应被拒绝（401）: %d", code)
	}
	w.token = newTok
	if code, _ := w.do("GET", wizAPI+"status", newTok, nil); code != 200 {
		t.Fatalf("新 token 应可用: %d", code)
	}

	// 真实向导（MySQL/Redis 临时实例）。
	complete := be.runWizard(w)
	_ = complete
	lock := e.host("var/lib/lingedge/server/install.lock")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("向导 complete 后应写入 install.lock: %v", err)
	}
	if installer.InstallLockName != "install.lock" {
		t.Errorf("installer.InstallLockName 与 Server 向导的 LockFileName 不一致: %q", installer.InstallLockName)
	}
	// 向导 complete 后 setup token 永久失效：哈希文件被删除（或在无权删除时被清空，Story 1.24）。
	if b, err := os.ReadFile(e.host("etc/lingedge/server/setup-token.hash")); err == nil && len(bytes.TrimSpace(b)) != 0 {
		t.Errorf("complete 后 setup-token.hash 应被删除或清空，实际仍有内容")
	}
	cred := e.host("var/lib/lingedge/server/secrets/runtime-credentials.json")
	if fi, err := os.Stat(cred); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("runtime-credentials.json 应存在且 0600: %v %v", fi, err)
	}
	if _, err := os.Stat(e.host("etc/lingedge/server/migrate.yaml")); err == nil {
		t.Error("向导与安装都不写 migrate.yaml（由 sudo lingedge-server migrate credential init 写出）")
	}

	// 同一进程切换为正常模式。
	deadline := time.Now().Add(30 * time.Second)
	ready := 0
	for time.Now().Before(deadline) {
		if res, err := http.Get(base + "/healthz/ready"); err == nil {
			ready = res.StatusCode
			_ = res.Body.Close()
			if ready == 200 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ready != 200 {
		t.Fatalf("complete 后 /healthz/ready 应 200: %d\n%s", ready, e.spec.logs.String())
	}
	if code, _ := w.do("GET", wizAPI+"status", newTok, nil); code != 410 {
		t.Errorf("向导完成后其路由应 410: %d", code)
	}

	// lingedge list：已初始化；人类可读输出同样显示。
	if ph := listPhase(); ph != "initialized" {
		t.Errorf("向导完成后 list 的 phase=%q", ph)
	}
	if code, so, _ := e.run(1000, "list"); code != 0 || !strings.Contains(so, "已初始化") {
		t.Errorf("list 的人类可读输出应显示已初始化:\n%s", so)
	}
	// 令牌重置在向导完成后被拒绝（3）。
	if code, _, _ := e.run(0, "config", "setup-token", "--reset", "--yes", "--json", "--config", e.cfg); code != 3 {
		t.Errorf("向导完成后 setup-token --reset 应退出码 3: %d", code)
	}

	// 泄漏扫描：数据库/管理员口令、token、迁移口令不得出现在 Tools 的输出、配置/状态文件、向导响应与 Server 日志里（向导响应按设计一次性展示的迁移口令除外）。
	secrets := append(be.secrets(), token, newTok)
	scan := func(where, text string, skip ...string) {
		for _, s := range secrets {
			if s == "" {
				continue
			}
			skipped := false
			for _, k := range skip {
				skipped = skipped || s == k
			}
			if !skipped && strings.Contains(text, s) {
				t.Errorf("%s 含机密 %.4s…", where, s)
			}
		}
	}
	scan("server 日志", e.spec.logs.String())
	scan("向导响应", strings.Join(w.log, "\n"), be.migPW, newTok) // complete 的响应按设计一次性展示迁移口令；newTok 是请求头不是响应
	scan("install 输出", installOut, token)                     // token 按设计只在这一次输出里出现；数据库/管理员口令绝不会出现
	walkFiles(t, e.w.Root, func(rel string, b []byte) {
		if strings.HasPrefix(rel, "var/lib/lingedge/server/secrets/") || rel == "run/creds/master.key" || strings.HasPrefix(rel, "run/server-live") {
			return // 凭据文件本身是信封加密存放（单独扫明文口令）
		}
		scan(rel, string(b))
	})
	if b, _ := os.ReadFile(cred); b != nil {
		for _, s := range be.secrets() {
			if strings.Contains(string(b), s) {
				t.Error("runtime-credentials.json 不得含明文口令（应信封加密）")
			}
		}
	}

	// 正常模式优雅停机退出码 0。
	if rc := e.spec.term(); rc != 0 {
		t.Errorf("优雅停机退出码 %d\n%s", rc, e.spec.logs.String())
	}
}

// AC5：激活后真实的健康检查失败（server 进程真的启动失败并退出）→ 自动回滚、退出码 10、不残留用户/单元/目录/配置/release。
func TestInstallServerHealthFailureRollsBack(t *testing.T) {
	e := newEnv(t, true)
	e.cfg = filepath.Join(t.TempDir(), "tools.yaml")
	if err := os.WriteFile(e.cfg, []byte("install:\n  health_timeout: 2s\n  health_interval: 100ms\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := tree(t, e.w)
	code, so, se := e.run(0, e.installArgs()...)
	if code != 10 {
		t.Fatalf("健康失败应退出码 10，实际 %d\nstdout=%s\nstderr=%s", code, so, se)
	}
	env := envelopeOf(t, so)
	if env["exit_name"] != "ROLLED_BACK" {
		t.Errorf("exit_name=%v", env["exit_name"])
	}
	lb, _ := os.ReadFile(e.log)
	t.Logf("Tools 日志:\n%s", lb)
	if ex, exErr := e.spec.exited(); !ex || exErr == nil {
		t.Errorf("真实 server 进程应已因启动失败而非零退出: exited=%v err=%v", ex, exErr)
	}
	if !strings.Contains(e.spec.logs.String(), "does-not-exist") && !strings.Contains(strings.ToLower(e.spec.logs.String()), "config") {
		t.Errorf("进程日志应说明配置读取失败:\n%s", e.spec.logs.String())
	}
	if d := diffTrees(before, tree(t, e.w)); d != "" {
		t.Errorf("回滚后残留（相对安装前）:\n%s", d)
	}
	for _, f := range []string{"users/lingedge", "active-lingedge-server.service", "enabled-lingedge-server.service"} {
		if e.w.FakeHas(f) {
			t.Errorf("回滚后不应残留伪 systemd/用户状态 %s", f)
		}
	}
	if strings.Contains(so, `"kind":"setup_token"`) && !strings.Contains(so, "error") {
		t.Error("失败的安装不应交付令牌")
	}
}

// 真实 lingedge-server 在 setup mode 下的 /healthz 探测对「安装期换端口」同样成立，且端口被占用时 Tools 在预检阶段就以退出码 4 失败（8080/9180/9443/9444 各一次，真实监听占用）。
func TestInstallServerRealPortOccupationExit4(t *testing.T) {
	for i, name := range []string{"rest", "grpc", "enroll", "metrics"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, false)
			l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(e.ports[i]))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l.Close() }()
			// 真实占用 → 用真实的监听表（本测试进程的 socket）喂给预检：只提供「谁在监听」，决策仍是生产的 resolver。
			e.w.Probe.System.Listeners = realListeners(t, e.ports[i], l)
			before := tree(t, e.w)
			code, so, _ := e.run(0, e.installArgs()...)
			if code != 4 {
				t.Fatalf("端口 %d 被占用应退出码 4，实际 %d\n%s", e.ports[i], code, so)
			}
			if d := diffTrees(before, tree(t, e.w)); d != "" {
				t.Errorf("预检失败不得有副作用:\n%s", d)
			}
		})
	}
}
