package taskrt

import (
	"context"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/scope"
)

// Store 是 tasks/task_events 的持久层契约（实现：internal/platform/taskrt/repository，真实 MySQL）。
//
// Scope 约定（ADR-009）：入队/读取/取消接受租户 Scope（只见本租户行，越权表现为 db.ErrNotFound → 404）或平台 Scope（跨租户）；
// Worker 侧方法（Claim/Renew/SetProgress/AddEvent/Complete/Retry/Release/Reap）要求平台 Scope（Worker 是跨租户的平台进程），否则返回 ErrScope。
// Worker 侧的写回方法全部带围栏：`id=? AND fence=? AND lease_owner=? AND status='running'`，不满足返回 ErrFenceLost 且不改变任何数据。
type Store interface {
	// Insert 入队。幂等键冲突（同 type + idempotency_key）时返回既有任务且 created=false（确定性入队，ADR-019 §6）。
	Insert(ctx context.Context, sc scope.Scope, n NewTask) (t Task, created bool, err error)
	// Get 按 uid 读取（租户 Scope 下他租户行表现为 db.ErrNotFound）。
	Get(ctx context.Context, sc scope.Scope, uid string) (Task, error)
	// List 列表（含 total）。
	List(ctx context.Context, sc scope.Scope, f Filter) ([]Task, int64, error)
	// ListEvents 任务事件（按 id 升序）；任务不可见时 db.ErrNotFound。
	ListEvents(ctx context.Context, sc scope.Scope, taskUID string, offset, limit int) ([]Event, int64, error)
	// RequestCancel 协作式取消：pending → canceled（无人持有）；running → cancel_requested=1；终态 → ErrNotCancelable。
	RequestCancel(ctx context.Context, sc scope.Scope, uid string) (Task, error)

	// Claim 领取至多 n 个已登记类型、到期的 pending 任务（FOR UPDATE SKIP LOCKED，fence+1、attempts+1）。
	Claim(ctx context.Context, sc scope.Scope, owner string, types []Type, n int, lease time.Duration) ([]Task, error)
	// Renew 续约；返回当前 cancel_requested。
	Renew(ctx context.Context, sc scope.Scope, id, fence uint64, owner string, lease time.Duration) (cancelRequested bool, err error)
	// SetProgress 写进度。
	SetProgress(ctx context.Context, sc scope.Scope, id, fence uint64, owner string, pct int) error
	// AddEvent 追加事件（围栏保护）。
	AddEvent(ctx context.Context, sc scope.Scope, id, fence uint64, owner, level, message string, data []byte) error
	// Complete 写终态（success|partial|failed|canceled）。
	Complete(ctx context.Context, sc scope.Scope, id, fence uint64, owner string, f Finish) error
	// Retry 把失败的执行放回 pending（退避 run_after），保留 attempts。
	Retry(ctx context.Context, sc scope.Scope, id, fence uint64, owner, errCode, errMsg string, b BackoffConfig) error
	// Release 停机时归还：回到 pending 并退还本次 attempts。
	Release(ctx context.Context, sc scope.Scope, id, fence uint64, owner string) error
	// Reap 回收租约过期且仍 running 的任务（pending+退避 / 超限 failed / 已请求取消则 canceled）。
	Reap(ctx context.Context, sc scope.Scope, limit int, b BackoffConfig) (ReapResult, error)
}
