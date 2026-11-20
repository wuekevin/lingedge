package schedrt

import (
	"context"
	"time"
)

// 租约名（job_locks.name）。
const (
	// LeaderLock 是 Scheduler 选主租约。
	LeaderLock = "scheduler.leader"
	// JobLockPrefix 是内部型作业窗口租约的名称前缀：job.<作业名>。
	JobLockPrefix = "job."
)

// LockStore 是 job_locks 的持久层契约（实现：schedrt/repository，真实 MySQL；时间以数据库 NOW(3) 为准）。
type LockStore interface {
	// Acquire 原子获取租约：行不存在、已过期，或（allowSelf 时）已由 holder 持有，则成为持有者、fence+1、lease_until=NOW+ttl。
	// 其他持有者的未过期租约不可获取（ok=false）。allowSelf 用于选主续约；窗口租约用 false（同一实例也不得在窗口内重复获取）。
	Acquire(ctx context.Context, name, holder string, ttl time.Duration, allowSelf bool) (fence uint64, ok bool, err error)
	// Hold 把 holder 持有的租约设为 NOW+d（可缩短或延长）；不是持有者时 ok=false。
	Hold(ctx context.Context, name, holder string, d time.Duration) (ok bool, err error)
	// Release 立即释放（仅当 holder 仍持有）：把 lease_until 置为当前时刻，使他人可立刻接管。
	Release(ctx context.Context, name, holder string) error
}

// LeaseInfo 是租约行的只读快照（测试与 doctor 观察用）。
type LeaseInfo struct {
	Name       string
	Holder     string
	Fence      uint64
	LeaseUntil time.Time
	Expired    bool
}
