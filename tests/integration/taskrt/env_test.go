package taskrtintegration

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	taskrepo "github.com/wuekevin/lingedge/internal/platform/taskrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/tests/integration/taskrt/handlers"
	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func ctxT(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// env 是一个用例的独立环境：随机库 + 全部迁移 + 真实 lingedge_app 账号 + exec_log 测试表。
type env struct {
	inst   *mysqltest.Instance
	adm    *sql.DB
	dbName string
	appPW  string
	app    *db.AppPool
	ids    *uid.Generator
	store  *taskrepo.Repo
	cfgDB  db.Config
}

func newEnv(t *testing.T) *env {
	t.Helper()
	inst := mysqltest.Start(t)
	e := &env{inst: inst, dbName: inst.NewDatabase(t), ids: uid.NewGenerator(clock.System())}
	e.appPW = mysqltest.RandomSecret(t)
	maintPW, migPW := mysqltest.RandomSecret(t), mysqltest.RandomSecret(t)
	migUser := mysqltest.RandomName("mig")
	for _, a := range []struct{ u, p string }{{migrate.AppUser, e.appPW}, {migrate.MaintUser, maintPW}, {migUser, migPW}} {
		inst.Exec(t, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", a.u, a.p))
		inst.DropUserOnCleanup(t, a.u)
	}
	inst.Exec(t, "GRANT ALL ON `"+e.dbName+"`.* TO '"+migUser+"'@'%' WITH GRANT OPTION")
	inst.Exec(t, "GRANT SET_USER_ID ON *.* TO '"+migUser+"'@'%'")
	cfg := func(user, pw string) db.Config {
		return db.Config{Host: inst.Host(), Port: inst.Port, Name: e.dbName, User: user, Password: config.NewSecret(pw), MaxOpenConns: 32, MaxIdleConns: 16}
	}
	mig, err := db.OpenMigrate(ctxT(t), cfg(migUser, migPW))
	if err != nil {
		t.Fatalf("OpenMigrate: %v", err)
	}
	t.Cleanup(func() { _ = mig.Close() })
	runner, err := migrate.NewRunner(mig, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Up(ctxT(t), migrate.UpOptions{}); err != nil {
		t.Fatalf("迁移 up: %v", err)
	}
	e.cfgDB = cfg(migrate.AppUser, e.appPW)
	if e.app, err = db.OpenApp(ctxT(t), e.cfgDB); err != nil {
		t.Fatalf("OpenApp: %v", err)
	}
	t.Cleanup(func() { _ = e.app.Close() })
	e.adm = inst.Admin(t)
	e.adm.SetMaxOpenConns(3)
	e.root(t, handlers.ExecLogDDL)
	e.root(t, "GRANT SELECT, INSERT ON §.exec_log TO '"+migrate.AppUser+"'@'%'")
	if e.store, err = taskrepo.New(e.app, e.ids); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) root(t testing.TB, q string, args ...any) {
	t.Helper()
	if _, err := e.adm.ExecContext(ctxT(t), strings.ReplaceAll(q, "§", "`"+e.dbName+"`"), args...); err != nil {
		t.Fatalf("root 执行失败: %v\n%s", err, q)
	}
}

func (e *env) rootInt(t testing.TB, q string, args ...any) int64 {
	t.Helper()
	var n sql.NullInt64
	if err := e.adm.QueryRowContext(ctxT(t), strings.ReplaceAll(q, "§", "`"+e.dbName+"`"), args...).Scan(&n); err != nil {
		t.Fatalf("root 查询失败: %v\n%s", err, q)
	}
	return n.Int64
}

func (e *env) rootString(t testing.TB, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := e.adm.QueryRowContext(ctxT(t), strings.ReplaceAll(q, "§", "`"+e.dbName+"`"), args...).Scan(&s); err != nil {
		t.Fatalf("root 查询失败: %v\n%s", err, q)
	}
	return s.String
}

// tenant 以 root 造一个租户，返回 (id, uid)。
func (e *env) tenant(t testing.TB, slug string) (uint64, string) {
	t.Helper()
	u := e.ids.MustNew()
	e.root(t, "INSERT INTO §.tenants (uid, name, slug) VALUES (?, ?, ?)", u, slug, slug)
	return uint64(e.rootInt(t, "SELECT id FROM §.tenants WHERE uid = ?", u)), u
}

// user 以 root 造一个用户，返回 (id, uid)。
func (e *env) user(t testing.TB, userType, email string) (uint64, string) {
	t.Helper()
	u := e.ids.MustNew()
	e.root(t, "INSERT INTO §.users (uid, user_type, email, display_name, password_hash) VALUES (?, ?, ?, 'x', 'x')", u, userType, email)
	return uint64(e.rootInt(t, "SELECT id FROM §.users WHERE uid = ?", u)), u
}

func platformScope() tenantscope.Scope { return tenantscope.PlatformForJob(0) }

func tenantScope(t testing.TB, tenantID, actor uint64) tenantscope.Scope {
	t.Helper()
	s, err := tenantscope.FromTask(tenantID, actor)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// registry 登记 retention.cleanup 的测试处理函数（真实写 exec_log）；也登记 cache.purge_url 以便测 202 路径。
func (e *env) registry(t testing.TB, owner string) *taskrt.Registry {
	t.Helper()
	reg := taskrt.NewRegistry()
	for _, ty := range []taskrt.Type{taskrt.TypeRetentionCleanup, taskrt.TypeCachePurgeURL} {
		if err := reg.Register(taskrt.Spec{Type: ty, Handler: handlers.Exec(e.app, owner)}); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func (e *env) enqueuer(t testing.TB, reg *taskrt.Registry, w taskrt.Waker) *taskrt.Enqueuer {
	t.Helper()
	enq, err := taskrt.NewEnqueuer(e.store, reg, e.ids, w)
	if err != nil {
		t.Fatal(err)
	}
	return enq
}

// workerOpts 是测试 Worker 的参数（短租约/短轮询/短退避，真实时间）。
type workerOpts struct {
	owner       string
	lease       time.Duration
	poll        time.Duration
	concurrency int
	rc          *redisx.Client
	drain       time.Duration
	reapEvery   time.Duration
}

func discardLogger(t testing.TB) (*zap.Logger, *syncBuf) {
	t.Helper()
	buf := &syncBuf{}
	l, err := logx.New(logx.Options{Level: "info", Output: buf})
	if err != nil {
		t.Fatal(err)
	}
	return l, buf
}

// startWorker 在测试进程内启动一个真实 Worker（真实 MySQL），返回停止函数（等待排空）。
func (e *env) startWorker(t *testing.T, reg *taskrt.Registry, o workerOpts) (*taskrt.Worker, func()) {
	t.Helper()
	if o.lease == 0 {
		o.lease = 600 * time.Millisecond
	}
	if o.poll == 0 {
		o.poll = 100 * time.Millisecond
	}
	if o.concurrency == 0 {
		o.concurrency = 2
	}
	lg, _ := discardLogger(t)
	w, err := taskrt.NewWorker(taskrt.WorkerConfig{
		Store: e.store, Registry: reg, Logger: lg, Redis: o.rc, Owner: o.owner, Lease: o.lease, PollInterval: o.poll, Concurrency: o.concurrency,
		DrainTimeout: o.drain, ReapEvery: o.reapEvery, Backoff: taskrt.BackoffConfig{Base: 100 * time.Millisecond, Max: time.Second}, Scope: platformScope(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return w, stop
}

func (e *env) redisClient(t *testing.T, in *redistest.Instance) *redisx.Client {
	t.Helper()
	rc, err := redisx.New(ctxT(t), redisx.Config{Addr: in.Addr(), Env: "test", ProbeInterval: 200 * time.Millisecond}, redisx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func eventually(t testing.TB, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("超时未满足: %s", what)
}

// enqueue 入队一个 retention.cleanup 任务（payload 为 handlers.Spec 的 JSON）。
func enqueue(t testing.TB, enq *taskrt.Enqueuer, sc tenantscope.Scope, payload string, mod ...func(*taskrt.EnqueueRequest)) taskrt.Task {
	t.Helper()
	req := taskrt.EnqueueRequest{Type: taskrt.TypeRetentionCleanup, Payload: []byte(payload)}
	for _, m := range mod {
		m(&req)
	}
	task, _, err := enq.Enqueue(context.Background(), sc, req)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	return task
}

func (e *env) status(t testing.TB, uid string) string {
	t.Helper()
	return e.rootString(t, "SELECT status FROM §.tasks WHERE uid = ?", uid)
}

func (e *env) execCount(t testing.TB, uid string) int64 {
	t.Helper()
	return e.rootInt(t, "SELECT COUNT(*) FROM §.exec_log WHERE task_uid = ?", uid)
}

// ---- 真实 Worker 子进程 ----

type workerProc struct {
	cmd  *exec.Cmd
	out  *syncBuf
	done chan struct{}
}

func (e *env) startWorkerProc(t *testing.T, owner string, lease, poll time.Duration, redisAddr string) *workerProc {
	t.Helper()
	cmd := exec.Command(buildWorker(t))
	var envv []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LINGEDGE_") {
			envv = append(envv, kv)
		}
	}
	cmd.Env = append(envv, "WP_DB_HOST="+e.inst.Host(), fmt.Sprintf("WP_DB_PORT=%d", e.inst.Port), "WP_DB_NAME="+e.dbName,
		"WP_DB_USER="+migrate.AppUser, "WP_DB_PASS="+e.appPW, "WP_OWNER="+owner,
		fmt.Sprintf("WP_LEASE_MS=%d", lease.Milliseconds()), fmt.Sprintf("WP_POLL_MS=%d", poll.Milliseconds()), "WP_REDIS="+redisAddr)
	out := &syncBuf{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &workerProc{cmd: cmd, out: out, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = cmd.Process.Kill()
			<-p.done
		}
	})
	eventually(t, "workerproc 就绪", 60*time.Second, func() bool { return strings.Contains(out.String(), "workerproc ready") })
	return p
}

func (p *workerProc) signal(t testing.TB, s syscall.Signal) {
	t.Helper()
	if err := p.cmd.Process.Signal(s); err != nil {
		t.Fatalf("发送 %v: %v", s, err)
	}
}

// kill9 发送 SIGKILL 并等待进程消失（真实的 kill -9）。
func (p *workerProc) kill9(t testing.TB) {
	t.Helper()
	p.signal(t, syscall.SIGKILL)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("SIGKILL 后进程未退出")
	}
}
