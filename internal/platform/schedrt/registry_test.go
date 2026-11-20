package schedrt

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/taskrt"
)

func noop(context.Context, time.Time) error { return nil }

func TestRegistryValidation(t *testing.T) {
	every, _ := Every(time.Minute)
	r := NewRegistry()
	if err := r.Internal("tasks.reap", every, 0, noop); err != nil {
		t.Fatal(err)
	}
	if err := r.Internal("tasks.reap", every, 0, noop); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Errorf("重复登记应被拒绝: %v", err)
	}
	// AA-8：内部作业不得与 TaskType 同名（不属 TaskType，也不能借名入 tasks 表）。
	for _, ty := range taskrt.KnownTypes() {
		if err := r.Internal(string(ty), every, 0, noop); err == nil || !strings.Contains(err.Error(), "AA-8") {
			t.Errorf("内部作业名 %q 与 TaskType 同名应被拒绝: %v", ty, err)
		}
	}
	for _, bad := range []string{"", "Tasks.Reap", "1abc", "a b", "a..b", "a-b", strings.Repeat("a", 65)} {
		if err := r.Internal(bad, every, 0, noop); err == nil {
			t.Errorf("非法作业名 %q 应被拒绝", bad)
		}
	}
	if err := r.Internal("x.y", nil, 0, noop); err == nil {
		t.Error("缺少调度规则应被拒绝")
	}
	if err := r.Internal("x.y", every, 0, nil); err == nil {
		t.Error("Run 与 Task 都缺失应被拒绝")
	}
	if err := r.add(Job{Name: "x.y", Schedule: every, Run: noop, Task: &TaskSpec{Type: taskrt.TypeHeartbeatSweep}}); err == nil {
		t.Error("Run 与 Task 同时设置应被拒绝")
	}
	// 任务型：类型必须在白名单内。
	if err := r.Task("heartbeat.sweep", every, TaskSpec{Type: "not.a.type"}); err == nil {
		t.Error("非白名单任务类型应被拒绝")
	}
	if err := r.Task("heartbeat.sweep", every, TaskSpec{Type: taskrt.TypeHeartbeatSweep}); err != nil {
		t.Fatal(err)
	}
	if got := len(r.Jobs()); got != 2 {
		t.Fatalf("应有 2 个作业: %d", got)
	}
	if k := IdempotencyKey("heartbeat.sweep", ts("2026-11-21 10:00:00")); k != "cron:heartbeat.sweep:1795255200" {
		t.Errorf("幂等键: %s", k)
	}
}

func TestRetryDelayAndHold(t *testing.T) {
	f := ts("2026-11-21 03:17:00")
	if d := retryDelay(f, f.Add(24*time.Hour)); d != 5*time.Minute {
		t.Errorf("日作业重试延迟: %v", d)
	}
	if d := retryDelay(f, f.Add(10*time.Second)); d != 0 {
		t.Errorf("10s 作业不单独重试: %v", d)
	}
	if h := windowHold(f.Add(time.Second), f, f.Add(10*time.Second)); h != 8*time.Second {
		t.Errorf("窗口保持: %v", h)
	}
	if h := windowHold(f.Add(time.Hour), f, f.Add(time.Minute)); h != time.Millisecond {
		t.Errorf("执行超时后保持下限: %v", h)
	}
}
