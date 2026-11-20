// Package builtin 是 Scheduler 内置的内部周期作业（Story 1.15 AC2；不属 OpenAPI TaskType，AA-8）：
//
//	tasks.reap          每 10s     回收租约过期的 running 任务（复用 Story 1.14 的 taskrt.Store.Reap；Worker 自身也会回收，二者并发安全）
//	idempotency.cleanup 每 15 分钟 删除已过期的幂等键（idempotency.Store.DeleteExpired；记录自创建起保留 24h，契约 §9.2）
//	sessions.cleanup    每小时     删除「过期或吊销」满 30 天的会话（database/README §7）
//	partition.maintain  每日 03:17 UTC 以 lingedge_maint 对实际存在的分区表 REORGANIZE PARTITION，滚动预建未来分区并保留 pmax；
//	                    maint 账号未配置时明确跳过并告警，绝不假装成功
//
// 所有作业幂等：同一窗口重复执行（窗口租约超时/停顿重叠）不产生重复副作用。
package builtin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/idempotency"
	"github.com/wuekevin/lingedge/internal/platform/schedrt"
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
)

// 内部作业名（不属 TaskType）。
const (
	JobTasksReap           = "tasks.reap"
	JobIdempotencyCleanup  = "idempotency.cleanup"
	JobSessionsCleanup     = "sessions.cleanup"
	JobPartitionMaintain   = "partition.maintain"
	partitionMaintainCron  = "17 3 * * *"
	tasksReapEvery         = 10 * time.Second
	idempotencyCleanupEach = 15 * time.Minute
	sessionsCleanupEach    = time.Hour
)

// 保留期与批量。
const (
	// SessionRetention：会话过期/吊销后保留 30 天（database/README §7）。
	SessionRetention = 30 * 24 * time.Hour
	batchSize        = 1000
	maxBatches       = 200 // 单次执行的批数上限（防止一次清理占用过久；剩余留给下一窗口）
	reapLimit        = 500
)

// SessionStore 是会话保留期清理的持久层契约（实现：schedrt/repository.Sessions）。
type SessionStore interface {
	// DeleteEnded 删除「过期或吊销」已满 retention 的会话（最多 limit 条），返回删除条数。要求平台 Scope。
	DeleteEnded(ctx context.Context, sc scope.Scope, retention time.Duration, limit int) (int64, error)
}

// Deps 是内置作业的依赖。
type Deps struct {
	Scope  scope.Scope // 平台 Scope（调用方铸造）
	Clock  clock.Clock
	Logger *zap.Logger

	Tasks       taskrt.Store
	Backoff     taskrt.BackoffConfig // 零值取 taskrt.DefaultBackoff
	Idempotency idempotency.Store
	Sessions    SessionStore
	// Partitions 为 nil 表示 maint 账号未配置：partition.maintain 每次触发都明确跳过并告警。
	Partitions PartitionStore
	Policy     PartitionPolicy // 零值取 DefaultPartitionPolicy
}

// Register 把四个内部作业登记到 reg。
func Register(reg *schedrt.Registry, d Deps) error {
	if d.Tasks == nil || d.Idempotency == nil || d.Sessions == nil || d.Logger == nil || d.Clock == nil || d.Scope.IsZero() {
		return errors.New("schedrt/builtin: 依赖不完整（需要 Scope、Clock、Logger、Tasks、Idempotency、Sessions）")
	}
	if d.Backoff == (taskrt.BackoffConfig{}) {
		d.Backoff = taskrt.DefaultBackoff
	}
	if d.Policy == (PartitionPolicy{}) {
		d.Policy = DefaultPartitionPolicy
	}
	every := func(dur time.Duration) schedrt.Schedule {
		s, err := schedrt.Every(dur)
		if err != nil {
			panic(err) // 编译期常量，不可能失败
		}
		return s
	}
	cron, err := schedrt.ParseCron(partitionMaintainCron)
	if err != nil {
		return err
	}
	for _, j := range []struct {
		name string
		s    schedrt.Schedule
		fn   schedrt.JobFunc
	}{
		{JobTasksReap, every(tasksReapEvery), TasksReap(d)},
		{JobIdempotencyCleanup, every(idempotencyCleanupEach), IdempotencyCleanup(d)},
		{JobSessionsCleanup, every(sessionsCleanupEach), SessionsCleanup(d)},
		{JobPartitionMaintain, cron, PartitionMaintain(d)},
	} {
		if err := reg.Internal(j.name, j.s, 0, j.fn); err != nil {
			return err
		}
	}
	return nil
}

// TasksReap 回收租约过期的 running 任务（pending+退避 / 超限 failed / 已请求取消则 canceled）。
func TasksReap(d Deps) schedrt.JobFunc {
	return func(ctx context.Context, _ time.Time) error {
		var total taskrt.ReapResult
		for i := 0; i < maxBatches; i++ {
			r, err := d.Tasks.Reap(ctx, d.Scope, reapLimit, d.Backoff)
			if err != nil {
				return fmt.Errorf("回收过期租约: %w", err)
			}
			total.Requeued += r.Requeued
			total.Failed += r.Failed
			total.Canceled += r.Canceled
			if r.Requeued+r.Failed+r.Canceled < reapLimit {
				break
			}
		}
		if n := total.Requeued + total.Failed + total.Canceled; n > 0 {
			d.Logger.Info("已回收租约过期的任务", zap.Int("requeued", total.Requeued), zap.Int("failed", total.Failed), zap.Int("canceled", total.Canceled))
		}
		return nil
	}
}

// IdempotencyCleanup 删除已过期的幂等键（记录创建时 expires_at = 创建时刻 + 24h；到期后才删除，未到期的一律保留）。
func IdempotencyCleanup(d Deps) schedrt.JobFunc {
	return func(ctx context.Context, _ time.Time) error {
		var total int64
		for i := 0; i < maxBatches; i++ {
			n, err := d.Idempotency.DeleteExpired(ctx, batchSize)
			if err != nil {
				return err
			}
			total += n
			if n < batchSize {
				break
			}
		}
		if total > 0 {
			d.Logger.Info("已清理过期幂等键", zap.Int64("deleted", total))
		}
		return nil
	}
}

// SessionsCleanup 删除过期/吊销满 30 天的会话。
func SessionsCleanup(d Deps) schedrt.JobFunc {
	return func(ctx context.Context, _ time.Time) error {
		var total int64
		for i := 0; i < maxBatches; i++ {
			n, err := d.Sessions.DeleteEnded(ctx, d.Scope, SessionRetention, batchSize)
			if err != nil {
				return err
			}
			total += n
			if n < batchSize {
				break
			}
		}
		if total > 0 {
			d.Logger.Info("已清理过期/吊销满 30 天的会话", zap.Int64("deleted", total))
		}
		return nil
	}
}
