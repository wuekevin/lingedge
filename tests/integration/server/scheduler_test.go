package serverintegration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/schedrt"
	schedrepo "github.com/wuekevin/lingedge/internal/platform/schedrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	taskrepo "github.com/wuekevin/lingedge/internal/platform/taskrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/tests/integration/server/schedkit"
	"github.com/wuekevin/lingedge/tests/integration/taskrt/handlers"
)

// Story 1.15 集成测试：Scheduler 的选主、租约重叠、kill -9 接管、重启恢复，全部跑在测试自拉起的真实 MySQL 上，无 Mock。
// 唯一的故障注入是 overlapLocks（把 scheduler.leader 租约永远判给调用方，模拟「旧 leader 停顿未察觉、新 leader 已接管」的租约重叠）；
// 其余（窗口租约、uk_tasks_idem 幂等键、Worker 执行）都是真实的数据库语义。

func (e *env) schedTables(t testing.TB) {
	t.Helper()
	e.root(t, schedkit.CronExecDDL)
	e.root(t, handlers.ExecLogDDL)
	for _, tb := range []string{"cron_exec", "exec_log"} {
		e.root(t, "GRANT SELECT, INSERT ON §."+tb+" TO '"+migrate.AppUser+"'@'%'")
	}
}

type results struct {
	mu   sync.Mutex
	list []schedrt.Result
}

func (r *results) add(x schedrt.Result) { r.mu.Lock(); r.list = append(r.list, x); r.mu.Unlock() }

func (r *results) count(job string, o schedrt.Outcome) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.list {
		if (job == "" || x.Job == job) && x.Outcome == o {
			n++
		}
	}
	return n
}

func (r *results) total() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.list) }

type runnerHandle struct {
	r      *schedrt.Runner
	res    *results
	cancel context.CancelFunc
	done   chan struct{}
}

func (h *runnerHandle) stop(t testing.TB) {
	t.Helper()
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(20 * time.Second):
		t.Fatal("Scheduler 未在 20s 内停止")
	}
}

func discardLogger(t testing.TB) *zap.Logger {
	t.Helper()
	l, err := logx.New(logx.Options{Level: "warn", Output: &syncBuf{}})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (e *env) startRunner(t *testing.T, instance string, locks schedrt.LockStore, ttl time.Duration) *runnerHandle {
	t.Helper()
	h := &runnerHandle{res: &results{}, done: make(chan struct{})}
	var err error
	h.r, err = schedkit.Build(schedkit.Cfg{App: e.app, Instance: instance, Locks: locks, TTL: ttl, Tick: 100 * time.Millisecond, Logger: discardLogger(t), OnResult: h.res.add})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { defer close(h.done); _ = h.r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-h.done })
	return h
}

// startWorker 在测试进程内起一个真实 taskrt.Worker（retention.cleanup 处理函数写 exec_log），测试结束自动停止。
func (e *env) startWorker(t *testing.T, owner string) {
	t.Helper()
	ids := uid.NewGenerator(clock.System())
	store, err := taskrepo.New(e.app, ids)
	if err != nil {
		t.Fatal(err)
	}
	reg := taskrt.NewRegistry()
	if err := reg.Register(taskrt.Spec{Type: taskrt.TypeRetentionCleanup, Handler: handlers.Exec(e.app, owner)}); err != nil {
		t.Fatal(err)
	}
	w, err := taskrt.NewWorker(taskrt.WorkerConfig{Store: store, Registry: reg, Logger: discardLogger(t), Owner: owner, PollInterval: 100 * time.Millisecond,
		Lease: 5 * time.Second, Scope: tenantscope.PlatformForJob(0)}) // [tenantscope-mint] 测试 Worker
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func (e *env) leaderRow(t testing.TB) (holder string, fence int64, expired bool) {
	t.Helper()
	locks, err := schedrepo.NewLocks(e.app)
	if err != nil {
		t.Fatal(err)
	}
	li, err := locks.Get(ctxT(t), schedrt.LeaderLock)
	if err != nil {
		return "", 0, true
	}
	return li.Holder, int64(li.Fence), li.Expired
}

// ---- AC1：两个实例并存，仅持有者触发 ----

func TestSchedulerOnlyLeaderFires(t *testing.T) {
	e := newEnv(t, all)
	e.schedTables(t)
	a := e.startRunner(t, "inst-A", nil, 1500*time.Millisecond)
	b := e.startRunner(t, "inst-B", nil, 1500*time.Millisecond)

	both := 0
	for i := 0; i < 100; i++ { // 5s：任意时刻至多一个 leader
		if a.r.IsLeader() && b.r.IsLeader() {
			both++
		}
		time.Sleep(50 * time.Millisecond)
	}
	if both != 0 {
		t.Fatalf("两个实例同时自认 leader %d 次", both)
	}
	var leader, follower *runnerHandle
	switch {
	case a.r.IsLeader() && !b.r.IsLeader():
		leader, follower = a, b
	case b.r.IsLeader() && !a.r.IsLeader():
		leader, follower = b, a
	default:
		t.Fatalf("应恰有一个 leader: A=%v B=%v", a.r.IsLeader(), b.r.IsLeader())
	}
	if follower.res.total() != 0 {
		t.Fatalf("非 leader 不得触发任何作业: %d 次", follower.res.total())
	}
	if leader.res.count(schedkit.ProbeInternal, schedrt.OutcomeDone) < 3 || leader.res.count(schedkit.ProbeTask, schedrt.OutcomeEnqueued) < 3 {
		t.Fatalf("leader 应已触发多个窗口: internal=%d task=%d", leader.res.count(schedkit.ProbeInternal, schedrt.OutcomeDone), leader.res.count(schedkit.ProbeTask, schedrt.OutcomeEnqueued))
	}
	if h, f, exp := e.leaderRow(t); h == "" || f < 1 || exp {
		t.Fatalf("job_locks 的 scheduler.leader 行不符: holder=%q fence=%d expired=%v", h, f, exp)
	}
	// 数据库事实：每个窗口恰好一次执行、一条任务。
	if dup := e.rootInt(t, "SELECT COALESCE(MAX(c), 0) FROM (SELECT COUNT(*) c FROM §.cron_exec GROUP BY job, fire) x"); dup != 1 {
		t.Fatalf("内部作业同一窗口的最大执行次数 = %d，应为 1", dup)
	}
	if n, enq := e.rootInt(t, "SELECT COUNT(*) FROM §.tasks WHERE idempotency_key LIKE 'cron:probe.task:%'"), leader.res.count(schedkit.ProbeTask, schedrt.OutcomeEnqueued); int(n) != enq {
		t.Fatalf("tasks 行数 %d 应等于 leader 入队次数 %d", n, enq)
	}

	// 优雅停机：leader 主动释放租约，另一实例在不到一个 TTL 内接管（fence 递增）。
	_, f0, _ := e.leaderRow(t)
	leader.stop(t)
	start := time.Now()
	eventually(t, "跟随者接管 leader", 3*time.Second, func() bool { return follower.r.IsLeader() })
	if d := time.Since(start); d > 1400*time.Millisecond {
		t.Errorf("优雅停机后接管耗时 %v，应远小于租约 TTL（已主动释放）", d)
	}
	if _, f1, _ := e.leaderRow(t); f1 <= f0 {
		t.Errorf("接管后 fence 应递增: %d → %d", f0, f1)
	}
}

// ---- AC1：租约重叠时，确定性幂等键 / 窗口租约保证同窗口只产生一次副作用 ----

// overlapLocks 把 scheduler.leader 租约永远判给调用方（故障注入：两个 Scheduler 同时自认 leader，即租约重叠）；其余租约（窗口租约）走真实 job_locks。
type overlapLocks struct{ real schedrt.LockStore }

func (o overlapLocks) Acquire(ctx context.Context, name, holder string, ttl time.Duration, allowSelf bool) (uint64, bool, error) {
	if name == schedrt.LeaderLock {
		return 1, true, nil
	}
	return o.real.Acquire(ctx, name, holder, ttl, allowSelf)
}

func (o overlapLocks) Hold(ctx context.Context, name, holder string, d time.Duration) (bool, error) {
	return o.real.Hold(ctx, name, holder, d)
}

func (o overlapLocks) Release(ctx context.Context, name, holder string) error {
	if name == schedrt.LeaderLock {
		return nil
	}
	return o.real.Release(ctx, name, holder)
}

func TestOverlappingLeasesYieldSingleEffectPerWindow(t *testing.T) {
	e := newEnv(t, all)
	e.schedTables(t)
	realLocks, err := schedrepo.NewLocks(e.app)
	if err != nil {
		t.Fatal(err)
	}
	ol := overlapLocks{realLocks}
	a := e.startRunner(t, "overlap-A", ol, 3*time.Second)
	b := e.startRunner(t, "overlap-B", ol, 3*time.Second)

	e.startWorker(t, "w")

	eventually(t, "两个实例都自认 leader（重叠已构造）", 5*time.Second, func() bool { return a.r.IsLeader() && b.r.IsLeader() })
	time.Sleep(7 * time.Second) // 约 7 个窗口
	a.stop(t)
	b.stop(t)

	enq := a.res.count(schedkit.ProbeTask, schedrt.OutcomeEnqueued) + b.res.count(schedkit.ProbeTask, schedrt.OutcomeEnqueued)
	dupHit := a.res.count(schedkit.ProbeTask, schedrt.OutcomeDuplicate) + b.res.count(schedkit.ProbeTask, schedrt.OutcomeDuplicate)
	tasks := int(e.rootInt(t, "SELECT COUNT(*) FROM §.tasks WHERE idempotency_key LIKE 'cron:probe.task:%'"))
	if tasks < 5 || enq != tasks {
		t.Fatalf("入队次数 %d 应等于 tasks 行数 %d（且 >= 5 个窗口）", enq, tasks)
	}
	if dupHit < 3 {
		t.Fatalf("重叠的 Scheduler 应多次命中幂等键（%d）：说明两个实例确实触发了同一窗口", dupHit)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM (SELECT idempotency_key FROM §.tasks WHERE idempotency_key LIKE 'cron:probe.task:%' GROUP BY idempotency_key HAVING COUNT(*) > 1) x"); n != 0 {
		t.Fatalf("存在重复窗口的任务: %d", n)
	}

	// 内部型作业：窗口租约保证同一窗口只执行一次。
	done := a.res.count(schedkit.ProbeInternal, schedrt.OutcomeDone) + b.res.count(schedkit.ProbeInternal, schedrt.OutcomeDone)
	taken := a.res.count(schedkit.ProbeInternal, schedrt.OutcomeWindowTaken) + b.res.count(schedkit.ProbeInternal, schedrt.OutcomeWindowTaken)
	rows := int(e.rootInt(t, "SELECT COUNT(*) FROM §.cron_exec"))
	if rows < 5 || rows != done || taken < 3 {
		t.Fatalf("cron_exec=%d done=%d window_taken=%d：应每窗口一次执行，另一实例被窗口租约挡下", rows, done, taken)
	}
	if mx := e.rootInt(t, "SELECT MAX(c) FROM (SELECT COUNT(*) c FROM §.cron_exec GROUP BY job, fire) x"); mx != 1 {
		t.Fatalf("同一窗口最大执行次数 = %d，应为 1", mx)
	}

	// FR-29：入队的任务被 Worker 各执行恰好一次（没有重复副作用）。
	eventually(t, "任务全部执行完", 15*time.Second, func() bool {
		return e.rootInt(t, "SELECT COUNT(*) FROM §.tasks WHERE idempotency_key LIKE 'cron:probe.task:%' AND status = 'success'") == int64(tasks)
	})
	if mx, n := e.rootInt(t, "SELECT MAX(c) FROM (SELECT COUNT(*) c FROM §.exec_log GROUP BY task_uid) x"), e.rootInt(t, "SELECT COUNT(DISTINCT task_uid) FROM §.exec_log"); mx != 1 || int(n) != tasks {
		t.Fatalf("exec_log：每任务最大执行次数=%d（应 1），被执行任务数=%d（应 %d）", mx, n, tasks)
	}
}

// ---- AC1/AC3：leader 被 kill -9 后另一实例接管 ----

type schedProc struct {
	cmd  *exec.Cmd
	out  *syncBuf
	done chan struct{}
}

var schedBin string

func buildSchedProc(t testing.TB) string {
	t.Helper()
	if schedBin != "" {
		return schedBin
	}
	dir, err := os.MkdirTemp("", "lesp")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "schedproc")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", bin, "./tests/integration/server/schedproc")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建 schedproc 失败: %v\n%s", err, out)
	}
	schedBin = bin
	return bin
}

func (e *env) startSched(t *testing.T, instance string, ttlMS int) *schedProc {
	t.Helper()
	cmd := exec.Command(buildSchedProc(t))
	cmd.Env = append(os.Environ(), "SP_DB_HOST="+e.inst.Host(), "SP_DB_PORT="+strconv.Itoa(e.inst.Port), "SP_DB_NAME="+e.dbName, "SP_DB_USER="+migrate.AppUser,
		"SP_DB_PASS="+e.appPW, "SP_INSTANCE="+instance, "SP_TTL_MS="+strconv.Itoa(ttlMS))
	out := &syncBuf{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &schedProc{cmd: cmd, out: out, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

func TestSchedulerLeaderKill9Takeover(t *testing.T) {
	e := newEnv(t, all)
	e.schedTables(t)
	a := e.startSched(t, "proc-A", 2000)
	eventually(t, "A 成为 leader", 20*time.Second, func() bool { h, _, exp := e.leaderRow(t); return h == "proc-A" && !exp })
	b := e.startSched(t, "proc-B", 2000)
	eventually(t, "A 已执行多个窗口", 20*time.Second, func() bool {
		return e.rootInt(t, "SELECT COUNT(*) FROM §.cron_exec WHERE inst = 'proc-A'") >= 3
	})
	time.Sleep(500 * time.Millisecond)
	if h, _, _ := e.leaderRow(t); h != "proc-A" {
		t.Fatalf("B 在 A 存活期间不得抢占 leader: holder=%s", h)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.cron_exec WHERE inst = 'proc-B'"); n != 0 {
		t.Fatalf("B 是跟随者，不得执行作业: %d", n)
	}
	_, f0, _ := e.leaderRow(t)

	killedAt := time.Now()
	if err := a.cmd.Process.Signal(syscall.SIGKILL); err != nil { // kill -9：没有机会释放租约
		t.Fatal(err)
	}
	<-a.done
	lastA := e.rootInt(t, "SELECT COALESCE(MAX(fire), 0) FROM §.cron_exec WHERE inst = 'proc-A'")

	eventually(t, "B 接管 leader", 20*time.Second, func() bool { h, _, exp := e.leaderRow(t); return h == "proc-B" && !exp })
	if d := time.Since(killedAt); d > 8*time.Second {
		t.Errorf("接管耗时 %v，应约为 租约 TTL(2s) + 探测间隔(≈0.7s)", d)
	}
	if _, f1, _ := e.leaderRow(t); f1 <= f0 {
		t.Errorf("接管后 fence 应递增: %d → %d", f0, f1)
	}
	eventually(t, "B 开始执行窗口", 15*time.Second, func() bool {
		return e.rootInt(t, "SELECT COUNT(*) FROM §.cron_exec WHERE inst = 'proc-B' AND fire > ?", lastA) >= 3
	})
	if mx := e.rootInt(t, "SELECT MAX(c) FROM (SELECT COUNT(*) c FROM §.cron_exec GROUP BY job, fire) x"); mx != 1 {
		t.Fatalf("接管前后同一窗口最大执行次数 = %d，应为 1\nA:\n%s\nB:\n%s", mx, a.out.String(), b.out.String())
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM (SELECT idempotency_key FROM §.tasks WHERE idempotency_key LIKE 'cron:probe.task:%' GROUP BY idempotency_key HAVING COUNT(*) > 1) x"); n != 0 {
		t.Fatalf("接管前后存在重复窗口的任务: %d", n)
	}
	if !strings.Contains(b.out.String(), "成为 Scheduler leader") {
		t.Errorf("B 的日志应记录接管:\n%s", b.out.String())
	}
	// 优雅停机的 B：退出码 0，释放租约。
	_ = b.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-b.done:
	case <-time.After(20 * time.Second):
		t.Fatal("B 未在 20s 内停止")
	}
	if _, _, exp := e.leaderRow(t); !exp {
		t.Error("优雅停机后 leader 租约应已释放（过期）")
	}
}

// ---- AC3：Server 重启后未完成任务恢复或安全重试；重复触发不产生重复副作用（真实 lingedge-server 二进制，scheduler 角色）----

func TestServerRestartRecoversAbandonedTasks(t *testing.T) {
	e := newEnv(t, all)
	e.schedTables(t)
	seed := func(uidv string, attempts, maxAttempts int, cancel int) {
		e.root(t, "INSERT INTO §.tasks (uid, type, status, payload_json, attempts, max_attempts, lease_owner, lease_until, fence, request_id, cancel_requested, started_at) "+
			"VALUES (?, 'retention.cleanup', 'running', '{}', ?, ?, 'dead-host:1:x', UTC_TIMESTAMP(3) - INTERVAL 1 MINUTE, ?, ?, ?, UTC_TIMESTAMP(3) - INTERVAL 2 MINUTE)",
			uidv, attempts, maxAttempts, attempts, uidv, cancel)
	}
	ids := uid.NewGenerator(clock.System())
	retry, exhausted, canceled := ids.MustNew(), ids.MustNew(), ids.MustNew()
	seed(retry, 1, 3, 0)     // 还有重试次数：应回到 pending（退避）
	seed(exhausted, 3, 3, 0) // 次数已尽：应 failed
	seed(canceled, 1, 3, 1)  // 已请求取消：应 canceled
	status := func(u string) string { return e.rootString(t, "SELECT status FROM §.tasks WHERE uid = ?", u) }

	metricsPort := freePort(t)
	p := startProc(t, e, []string{"LINGEDGE_METRICS__LISTEN=127.0.0.1:" + strconv.Itoa(metricsPort)}, "--config", e.cfgPath, "--roles", "scheduler")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server 日志:\n%s", p.out.String())
		}
	})
	eventually(t, "三个遗留任务全部被 tasks.reap 处理", 30*time.Second, func() bool {
		return status(retry) == "pending" && status(exhausted) == "failed" && status(canceled) == "canceled"
	})
	if a := e.rootInt(t, "SELECT attempts FROM §.tasks WHERE uid = ?", retry); a != 1 {
		t.Errorf("回收不得改 attempts（安全重试由下一次领取 +1）: %d", a)
	}
	if ec := e.rootString(t, "SELECT error_code FROM §.tasks WHERE uid = ?", retry); ec != "task.lease_expired" {
		t.Errorf("error_code = %q", ec)
	}
	if lo := e.rootString(t, "SELECT COALESCE(lease_owner, 'NULL') FROM §.tasks WHERE uid = ?", retry); lo != "NULL" {
		t.Errorf("回收后租约应清空: %s", lo)
	}
	if ra := e.rootInt(t, "SELECT run_after > UTC_TIMESTAMP(3) FROM §.tasks WHERE uid = ?", retry); ra != 1 {
		t.Error("重试任务应带退避 run_after")
	}
	events := func() int64 {
		return e.rootInt(t, "SELECT COUNT(*) FROM §.task_events ev JOIN §.tasks t ON t.id = ev.task_id WHERE t.uid IN (?, ?, ?)", retry, exhausted, canceled)
	}
	if n := events(); n != 3 {
		t.Fatalf("每个遗留任务应恰有 1 条回收事件: %d", n)
	}
	// 重复触发（tasks.reap 每 10s 一次）：再等一个窗口，状态与事件数都不得变化。
	time.Sleep(12 * time.Second)
	if n := events(); n != 3 || status(retry) != "pending" || status(exhausted) != "failed" || status(canceled) != "canceled" {
		t.Fatalf("重复回收产生了副作用: events=%d", n)
	}
	if !strings.Contains(p.out.String(), "已回收租约过期的任务") {
		t.Errorf("日志应记录回收:\n%s", p.out.String())
	}

	// maint 账号未配置：partition.maintain 明确跳过并告警，不是成功。
	var skipLine string
	for _, ln := range strings.Split(p.out.String(), "\n") {
		if strings.Contains(ln, "周期作业已跳过") && strings.Contains(ln, `"job":"partition.maintain"`) {
			skipLine = ln
		}
	}
	if skipLine == "" || !strings.Contains(skipLine, `"alert":true`) || !strings.Contains(skipLine, "未配置 lingedge_maint") || !strings.Contains(skipLine, `"level":"warn"`) {
		t.Fatalf("maint 缺失时应有「已跳过」告警日志:\n%s", p.out.String())
	}
	if strings.Contains(p.out.String(), "已滚动预建分区") {
		t.Error("maint 缺失时不得出现预建分区的成功日志")
	}
	code, body, _ := httpGet(t, "http://127.0.0.1:"+strconv.Itoa(metricsPort)+"/metrics")
	if code != 200 || !strings.Contains(string(body), "lingedge_scheduler_job_skips 1") || !strings.Contains(string(body), "lingedge_scheduler_is_leader 1") {
		t.Errorf("指标应含 skips=1 与 is_leader=1: %d\n%s", code, body)
	}

	// 优雅停机（SIGTERM）：退出码 0，并释放 leader 租约（重启后的新进程立即接管，不必等租约过期）。
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	if c := p.wait(t, 30*time.Second); c != 0 {
		t.Fatalf("退出码 %d\n%s", c, p.out.String())
	}
	if _, _, exp := e.leaderRow(t); !exp {
		t.Error("优雅停机后 scheduler.leader 租约应已释放")
	}
	// 重启后「安全重试」：新进程接管 leader；被回收的任务由 Worker 重新领取并执行恰好一次（attempts 1→2、fence 递增、exec_log 1 条）。
	e.root(t, "UPDATE §.tasks SET run_after = UTC_TIMESTAMP(3) WHERE uid = ?", retry)
	p2 := startProc(t, e, []string{"LINGEDGE_METRICS__LISTEN=127.0.0.1:" + strconv.Itoa(freePort(t))}, "--config", e.cfgPath, "--roles", "scheduler")
	eventually(t, "重启后的进程成为 leader", 20*time.Second, func() bool { return strings.Contains(p2.out.String(), "成为 Scheduler leader") })
	e.startWorker(t, "recovery-worker")
	eventually(t, "被回收的任务重试成功", 20*time.Second, func() bool { return status(retry) == "success" })
	if a, f := e.rootInt(t, "SELECT attempts FROM §.tasks WHERE uid = ?", retry), e.rootInt(t, "SELECT fence FROM §.tasks WHERE uid = ?", retry); a != 2 || f != 2 {
		t.Errorf("重试后 attempts=%d fence=%d，应为 2/2", a, f)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.exec_log WHERE task_uid = ?", retry); n != 1 {
		t.Errorf("重试任务应恰好执行一次: %d", n)
	}
	_ = p2.cmd.Process.Signal(syscall.SIGTERM)
	if c := p2.wait(t, 30*time.Second); c != 0 {
		t.Fatalf("重启进程退出码 %d\n%s", c, p2.out.String())
	}
	if status(exhausted) != "failed" || status(canceled) != "canceled" {
		t.Error("终态任务不得被重启改变")
	}
}
