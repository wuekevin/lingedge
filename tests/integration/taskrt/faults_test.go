package taskrtintegration

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/taskrt"
)

// AC4（故障注入）：kill -9 真实 Worker 子进程 → 租约过期后任务被回收（pending+退避），由另一个 Worker 重新领取并完成：
// attempts=2、fence=2；至少一次执行（exec_log 两条，分属两个 owner）。
func TestKill9WorkerTaskIsReclaimed(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	task := enqueue(t, enq, platformScope(), `{"sleep_ms":60000}`)
	proc := e.startWorkerProc(t, "proc-A", 800*time.Millisecond, 100*time.Millisecond, "")
	eventually(t, "子进程领取并开始执行", 30*time.Second, func() bool { return e.execCount(t, task.UID) == 1 })
	proc.kill9(t) // 真实 kill -9：没有任何清理机会
	if st := e.status(t, task.UID); st != "running" {
		t.Fatalf("kill -9 后任务仍应是 running（尚未被回收），实际 %s", st)
	}

	// 把任务内容改成快速完成的形态以便第二次执行结束（payload 是引用型开关，这里用 root 旁路改写，模拟「修复后的重跑」）。
	e.root(t, "UPDATE §.tasks SET payload_json = '{}' WHERE uid = ?", task.UID)
	e.startWorker(t, e.registry(t, "B"), workerOpts{owner: "B", lease: 800 * time.Millisecond})
	eventually(t, "任务被回收并由 B 完成", 30*time.Second, func() bool { return e.status(t, task.UID) == "success" })

	tk, _ := e.store.Get(context.Background(), platformScope(), task.UID)
	if tk.Attempts != 2 || tk.Fence != 2 {
		t.Fatalf("回收重跑后应 attempts=2, fence=2，实际 %d/%d", tk.Attempts, tk.Fence)
	}
	if n := e.execCount(t, task.UID); n != 2 {
		t.Fatalf("至少一次执行：应有 2 次执行记录，实际 %d", n)
	}
	if owners := e.rootInt(t, "SELECT COUNT(DISTINCT owner) FROM §.exec_log WHERE task_uid = ?", task.UID); owners != 2 {
		t.Fatalf("两次执行应分属两个 Worker，实际 %d", owners)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.task_events WHERE task_id = ? AND message LIKE 'lease expired%'", tk.ID); n != 1 {
		t.Fatalf("应记录一次租约过期回收事件，实际 %d", n)
	}
	if tk.ErrorCode != nil {
		t.Fatalf("成功后应清除 error_code: %v", *tk.ErrorCode)
	}
}

// kill -9 + 已达 max_attempts：回收为 failed，原因 task.lease_expired（超限不再重试）。
func TestKill9ExhaustedAttemptsFail(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	task := enqueue(t, enq, platformScope(), `{"sleep_ms":60000}`, func(r *taskrt.EnqueueRequest) { r.MaxAttempts = 1 })
	proc := e.startWorkerProc(t, "proc-A", 800*time.Millisecond, 100*time.Millisecond, "")
	eventually(t, "子进程开始执行", 30*time.Second, func() bool { return e.execCount(t, task.UID) == 1 })
	proc.kill9(t)
	e.startWorker(t, e.registry(t, "B"), workerOpts{owner: "B", lease: 800 * time.Millisecond})
	eventually(t, "超限回收为 failed", 30*time.Second, func() bool { return e.status(t, task.UID) == "failed" })
	tk, _ := e.store.Get(context.Background(), platformScope(), task.UID)
	if tk.ErrorCode == nil || *tk.ErrorCode != "task.lease_expired" || tk.ErrorMessage == nil || tk.FinishedAt == nil || tk.LeaseOwner != nil {
		t.Fatalf("超限失败应含原因: %+v", tk)
	}
	time.Sleep(600 * time.Millisecond)
	if n := e.execCount(t, task.UID); n != 1 {
		t.Fatalf("超限后不得再执行，实际 %d", n)
	}
}

// 租约过期（Worker 仍在、但不再续约，例如被冻结）→ Reap 回收；回收后的退避期内不会被立即领取。
func TestLeaseExpiryReapRequeuesWithBackoff(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	task := enqueue(t, enq, platformScope(), `{}`)
	ctx := context.Background()
	got, err := e.store.Claim(ctx, platformScope(), "A", []taskrt.Type{taskrt.TypeRetentionCleanup}, 1, 300*time.Millisecond)
	if err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	if r, err := e.store.Reap(ctx, platformScope(), 10, taskrt.BackoffConfig{Base: time.Second, Max: time.Minute}); err != nil || r.Requeued != 0 {
		t.Fatalf("租约未过期不得回收: %+v %v", r, err)
	}
	time.Sleep(400 * time.Millisecond)
	r, err := e.store.Reap(ctx, platformScope(), 10, taskrt.BackoffConfig{Base: time.Second, Max: time.Minute})
	if err != nil || r.Requeued != 1 || r.Failed != 0 {
		t.Fatalf("过期应回收为 pending: %+v %v", r, err)
	}
	tk, _ := e.store.Get(ctx, platformScope(), task.UID)
	if tk.Status != taskrt.StatusPending || tk.LeaseOwner != nil || tk.Attempts != 1 || tk.Fence != 1 || tk.ErrorCode == nil || *tk.ErrorCode != "task.lease_expired" {
		t.Fatalf("回收后状态: %+v", tk)
	}
	if !tk.RunAfter.After(time.Now().Add(500 * time.Millisecond)) {
		t.Fatalf("回收应退避 run_after（≥ Base≈1s 之后），实际 %v", time.Until(tk.RunAfter))
	}
	if again, _ := e.store.Claim(ctx, platformScope(), "B", []taskrt.Type{taskrt.TypeRetentionCleanup}, 1, time.Minute); len(again) != 0 {
		t.Fatal("退避期内不应被领取")
	}
}

// AC4：僵尸 Worker 的写回因 fence 不符被拒绝——存储层逐一验证全部写回方法，且被拒绝的写回不改变任何数据。
func TestZombieWritebackRejectedByFence(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	task := enqueue(t, enq, platformScope(), `{}`)
	ctx := context.Background()
	types := []taskrt.Type{taskrt.TypeRetentionCleanup}
	a, _ := e.store.Claim(ctx, platformScope(), "A", types, 1, 200*time.Millisecond) // A：fence=1
	if len(a) != 1 {
		t.Fatal("A 应领取到任务")
	}
	time.Sleep(300 * time.Millisecond)
	if r, _ := e.store.Reap(ctx, platformScope(), 10, taskrt.BackoffConfig{Base: 10 * time.Millisecond, Max: 20 * time.Millisecond}); r.Requeued != 1 {
		t.Fatalf("应回收: %+v", r)
	}
	time.Sleep(60 * time.Millisecond)
	b, _ := e.store.Claim(ctx, platformScope(), "B", types, 1, time.Minute) // B：fence=2
	if len(b) != 1 || b[0].Fence != 2 || b[0].Attempts != 2 {
		t.Fatalf("B 应以 fence=2 重新领取: %+v", b)
	}
	snap := func() string {
		return e.rootString(t, "SELECT CONCAT_WS('|', status, progress, IFNULL(result_json,''), IFNULL(error_code,''), lease_owner, fence, attempts, (SELECT COUNT(*) FROM §.task_events WHERE task_id = tasks.id)) FROM §.tasks WHERE uid = ?", task.UID)
	}
	before := snap()
	id := a[0].ID
	// A 用旧 fence=1（以及即使冒用 B 的 owner 名也不行：fence 不符）依次尝试每种写回。
	writes := map[string]func() error{
		"Renew":       func() error { _, err := e.store.Renew(ctx, platformScope(), id, 1, "A", time.Minute); return err },
		"SetProgress": func() error { return e.store.SetProgress(ctx, platformScope(), id, 1, "A", 77) },
		"AddEvent": func() error {
			return e.store.AddEvent(ctx, platformScope(), id, 1, "A", taskrt.LevelInfo, "zombie event", nil)
		},
		"Complete": func() error {
			return e.store.Complete(ctx, platformScope(), id, 1, "A", taskrt.Finish{Status: taskrt.StatusSuccess, Result: []byte(`{"by":"A"}`)})
		},
		"Retry": func() error {
			return e.store.Retry(ctx, platformScope(), id, 1, "A", "x", "y", taskrt.BackoffConfig{Base: time.Second, Max: time.Second})
		},
		"Release": func() error { return e.store.Release(ctx, platformScope(), id, 1, "A") },
		"Complete(冒用 B 名)": func() error {
			return e.store.Complete(ctx, platformScope(), id, 1, "B", taskrt.Finish{Status: taskrt.StatusFailed})
		},
	}
	for name, w := range writes {
		if err := w(); !errors.Is(err, taskrt.ErrFenceLost) {
			t.Errorf("僵尸 %s 应被围栏拒绝（ErrFenceLost），得到 %v", name, err)
		}
	}
	if after := snap(); after != before {
		t.Fatalf("被拒绝的僵尸写回不得改变任何数据\n前: %s\n后: %s", before, after)
	}
	// 合法持有者 B 的写回正常。
	if err := e.store.Complete(ctx, platformScope(), id, 2, "B", taskrt.Finish{Status: taskrt.StatusSuccess, Result: []byte(`{"by":"B"}`)}); err != nil {
		t.Fatalf("B 的写回应成功: %v", err)
	}
	if got := e.rootString(t, "SELECT result_json FROM §.tasks WHERE uid = ?", task.UID); !strings.Contains(got, `"B"`) {
		t.Fatalf("最终结果应来自 B: %s", got)
	}
	// 终态之后 B 自己再写也被拒绝（status 不再是 running）。
	if err := e.store.SetProgress(ctx, platformScope(), id, 2, "B", 1); !errors.Is(err, taskrt.ErrFenceLost) {
		t.Fatalf("终态后的写回应被拒绝: %v", err)
	}
}

// 真实僵尸进程：SIGSTOP 冻结 Worker 子进程（无法续约）→ 租约过期、另一个 Worker 接手并完成 → SIGCONT 后僵尸醒来，
// 它的续约/终态写回被围栏拒绝，最终结果仍来自接手者。
func TestSIGSTOPZombieProcessCannotOverwrite(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	task := enqueue(t, enq, platformScope(), `{"sleep_ms":1500,"ignore_ctx":true}`)
	zombie := e.startWorkerProc(t, "zombie", 600*time.Millisecond, 100*time.Millisecond, "")
	eventually(t, "僵尸开始执行", 30*time.Second, func() bool { return e.execCount(t, task.UID) == 1 })
	zombie.signal(t, syscall.SIGSTOP)
	defer func() { _ = zombie.cmd.Process.Signal(syscall.SIGCONT) }()

	e.root(t, "UPDATE §.tasks SET payload_json = '{}' WHERE uid = ?", task.UID) // 接手者快速完成
	e.startWorker(t, e.registry(t, "taker"), workerOpts{owner: "taker", lease: 600 * time.Millisecond})
	eventually(t, "接手者完成任务", 30*time.Second, func() bool { return e.status(t, task.UID) == "success" })
	final := e.rootString(t, "SELECT CONCAT_WS('|', result_json, fence, attempts, finished_at) FROM §.tasks WHERE uid = ?", task.UID)
	if !strings.Contains(final, `"by": "taker"`) {
		t.Fatalf("结果应来自接手者: %s", final)
	}

	zombie.signal(t, syscall.SIGCONT)
	eventually(t, "僵尸醒来后其写回被围栏拒绝", 30*time.Second, func() bool {
		out := zombie.out.String()
		return strings.Contains(out, "围栏") // 续约被拒（取消处理函数）或终态写回被拒（僵尸写回）
	})
	time.Sleep(2 * time.Second) // 给僵尸足够时间结束 sleep 并尝试写回
	if now := e.rootString(t, "SELECT CONCAT_WS('|', result_json, fence, attempts, finished_at) FROM §.tasks WHERE uid = ?", task.UID); now != final {
		t.Fatalf("僵尸苏醒后任务数据被改变\n前: %s\n后: %s", final, now)
	}
	if st := e.status(t, task.UID); st != "success" {
		t.Fatalf("状态应保持 success，实际 %s", st)
	}
}

// 优雅停机：SIGTERM 子进程 → 停止领取，排空超时后取消处理函数并归还（pending，退还本次 attempts），不丢任务、不留租约。
func TestGracefulShutdownReleasesTask(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	task := enqueue(t, enq, platformScope(), `{"sleep_ms":60000}`)
	proc := e.startWorkerProc(t, "proc-A", 30*time.Second, 100*time.Millisecond, "")
	eventually(t, "开始执行", 30*time.Second, func() bool { return e.execCount(t, task.UID) == 1 })
	proc.signal(t, syscall.SIGTERM)
	select {
	case <-proc.done:
	case <-time.After(30 * time.Second):
		t.Fatalf("SIGTERM 后进程未退出\n%s", proc.out.String())
	}
	tk, _ := e.store.Get(context.Background(), platformScope(), task.UID)
	if tk.Status != taskrt.StatusPending || tk.LeaseOwner != nil || tk.Attempts != 0 {
		t.Fatalf("停机应归还任务（pending、无租约、退还 attempts）: %+v", tk)
	}
	if !strings.Contains(proc.out.String(), "任务已归还") {
		t.Fatalf("应记录归还日志:\n%s", proc.out.String())
	}
}
