package taskrtintegration

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func (e *env) waitRunningOrDone(t testing.TB, uid string, d time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	eventually(t, "任务被领取（离开 pending）", d, func() bool { return e.status(t, uid) != "pending" })
	return time.Since(start)
}

func newWaker(t *testing.T, e *env, rc *redisx.Client) *taskrt.RedisWaker {
	t.Helper()
	w, err := taskrt.NewRedisWaker(rc, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// AC5：Redis 唤醒只是加速。轮询间隔设得很大（20s）时，只有唤醒能让任务在秒级内被领取；
// 对照组（无唤醒 + 同样大的轮询间隔）证明测试能区分「靠唤醒」与「靠轮询」。
func TestRedisWakeAcceleratesClaim(t *testing.T) {
	e := newEnv(t)
	rin := redistest.Start(t)
	reg := e.registry(t, "W")

	_, stop := e.startWorker(t, reg, workerOpts{owner: "W", lease: 5 * time.Second, poll: 20 * time.Second, rc: e.redisClient(t, rin)})
	time.Sleep(700 * time.Millisecond) // 订阅建立；Worker 的首个周期已过
	enq := e.enqueuer(t, reg, newWaker(t, e, e.redisClient(t, rin)))
	task := enqueue(t, enq, platformScope(), `{}`)
	d := e.waitRunningOrDone(t, task.UID, 5*time.Second)
	t.Logf("基线：Redis 唤醒 → 领取延迟 %v（poll=20s）", d)
	stop()

	// 对照：同样的大轮询间隔、没有任何唤醒 → 任务在 2s 内仍是 pending。
	_, stop2 := e.startWorker(t, reg, workerOpts{owner: "W2", lease: 5 * time.Second, poll: 20 * time.Second})
	defer stop2()
	time.Sleep(300 * time.Millisecond)
	ctrl := enqueue(t, e.enqueuer(t, reg, nil), platformScope(), `{}`)
	time.Sleep(2 * time.Second)
	if st := e.status(t, ctrl.UID); st != "pending" {
		t.Fatalf("对照组（无唤醒、poll=20s）不应已被领取，实际 %s", st)
	}
}

// AC5：唤醒丢失（发布被丢弃/没有 Redis）→ 轮询兜底，任务不丢。
func TestLostWakeupFallsBackToPolling(t *testing.T) {
	e := newEnv(t)
	reg := e.registry(t, "W")
	e.startWorker(t, reg, workerOpts{owner: "W", lease: 5 * time.Second, poll: 300 * time.Millisecond})
	enq := e.enqueuer(t, reg, taskrt.NopWaker{}) // 唤醒被丢弃
	var tasks []taskrt.Task
	for i := 0; i < 10; i++ {
		tasks = append(tasks, enqueue(t, enq, platformScope(), `{}`))
	}
	eventually(t, "全部任务被轮询领取并完成", 20*time.Second, func() bool {
		return e.rootInt(t, "SELECT COUNT(*) FROM §.tasks WHERE status = 'success'") == int64(len(tasks))
	})
}

// AC5：Redis 进程被杀（唤醒不可用，客户端降级）后，Worker 仍按轮询间隔领取；Redis 恢复后唤醒自愈。
func TestRedisDownPollingStillClaims(t *testing.T) {
	e := newEnv(t)
	rin := redistest.Start(t)
	reg := e.registry(t, "W")
	rc := e.redisClient(t, rin)
	e.startWorker(t, reg, workerOpts{owner: "W", lease: 5 * time.Second, poll: 400 * time.Millisecond, rc: rc})
	enq := e.enqueuer(t, reg, newWaker(t, e, rc))
	time.Sleep(500 * time.Millisecond)

	rin.Kill()
	eventually(t, "Redis 客户端降级", 10*time.Second, func() bool { return rc.State() != redisx.StateUp })
	var uids []string
	for i := 0; i < 5; i++ {
		uids = append(uids, enqueue(t, enq, platformScope(), `{}`).UID) // 入队不依赖 Redis（唤醒发布失败被静默降级）
	}
	eventually(t, "Redis 宕机期间任务仍被轮询领取并完成", 20*time.Second, func() bool {
		return e.rootInt(t, "SELECT COUNT(*) FROM §.tasks WHERE status = 'success'") == int64(len(uids))
	})

	rin.Restart(t)
	eventually(t, "Redis 恢复", 20*time.Second, func() bool { return rc.State() == redisx.StateUp })
	late := enqueue(t, enq, platformScope(), `{}`)
	eventually(t, "恢复后任务照常完成", 20*time.Second, func() bool { return e.status(t, late.UID) == "success" })
}

// 基线测量（给默认轮询间隔提供数值依据，写入 Story 文档）：
//   - 空队列领取（索引 idx_tasks_claim，表里已有 3000 条终态任务）与回收查询的延迟分布；
//   - Redis 唤醒到领取的端到端延迟。
//
// 断言只设很宽的上界（防止性能回退），真实数值通过 t.Logf 输出。
func TestPollingBaselineMeasurements(t *testing.T) {
	e := newEnv(t)
	for _, pfx := range []string{"01HZZZZZZA", "01HZZZZZZB", "01HZZZZZZC"} { // 递归 CTE 默认深度上限 1000
		e.root(t, "INSERT INTO §.tasks (uid, type, status, payload_json, request_id, finished_at) "+
			"WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 1000) "+
			"SELECT CONCAT(?, LPAD(n, 16, '0')), 'retention.cleanup', 'success', '{}', '01HZZZZZZZZZZZZZZZZZZZZZZZ', NOW(3) FROM seq", pfx)
	}
	ctx := context.Background()
	types := []taskrt.Type{taskrt.TypeRetentionCleanup}
	measure := func(f func()) (p50, p95, p99 time.Duration) {
		const n = 300
		ds := make([]time.Duration, n)
		for i := range ds {
			s := time.Now()
			f()
			ds[i] = time.Since(s)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[n/2], ds[n*95/100], ds[n*99/100]
	}
	p50, p95, p99 := measure(func() {
		if got, err := e.store.Claim(ctx, platformScope(), "w", types, 4, time.Minute); err != nil || len(got) != 0 {
			t.Fatalf("空队列领取: %v %v", got, err)
		}
	})
	t.Logf("基线：空队列 Claim（含 BEGIN/SELECT…SKIP LOCKED/COMMIT，库内 3000 终态行）p50=%v p95=%v p99=%v", p50, p95, p99)
	if p99 > 250*time.Millisecond {
		t.Errorf("空领取 p99=%v 过慢", p99)
	}
	r50, r95, r99 := measure(func() {
		if _, err := e.store.Reap(ctx, platformScope(), 100, taskrt.DefaultBackoff); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("基线：空回收 Reap p50=%v p95=%v p99=%v", r50, r95, r99)

	rin := redistest.Start(t)
	reg := e.registry(t, "W")
	e.startWorker(t, reg, workerOpts{owner: "W", lease: 5 * time.Second, poll: 30 * time.Second, rc: e.redisClient(t, rin), concurrency: 4})
	enq := e.enqueuer(t, reg, newWaker(t, e, e.redisClient(t, rin)))
	time.Sleep(700 * time.Millisecond)
	var ds []time.Duration
	for i := 0; i < 15; i++ {
		task := enqueue(t, enq, platformScope(), `{}`)
		ds = append(ds, e.waitRunningOrDone(t, task.UID, 5*time.Second))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	t.Logf("基线：Redis 唤醒 → 领取（含 25ms 轮询观测粒度）p50=%v max=%v", ds[len(ds)/2], ds[len(ds)-1])
}
