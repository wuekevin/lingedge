// Package schedkit 是 Story 1.15 集成测试共用的 Scheduler 装配：与生产同一份 schedrt.Runner，另登记两个探针作业，
// 把「每个窗口谁执行了几次」落到真实 MySQL 的表里，使「恰好一次」类断言基于数据库事实而不是进程内计数。
// 测试进程内的 Runner 与被 kill -9 的子进程（../schedproc）都用它。只在 tests/ 下构建。
package schedkit

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	idemrepo "github.com/wuekevin/lingedge/internal/platform/idempotency/repository"
	"github.com/wuekevin/lingedge/internal/platform/schedrt"
	"github.com/wuekevin/lingedge/internal/platform/schedrt/builtin"
	schedrepo "github.com/wuekevin/lingedge/internal/platform/schedrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	taskrepo "github.com/wuekevin/lingedge/internal/platform/taskrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/tests/integration/taskrt/handlers"
)

// 探针作业名与测试表。
const (
	ProbeInternal = "probe.exec" // 内部型：每次执行向 cron_exec 插一行
	ProbeTask     = "probe.task" // 任务型：入队 retention.cleanup（处理函数写 exec_log）

	// CronExecDDL 记录内部探针作业的每次执行（§ 由测试替换为库名）。
	CronExecDDL = "CREATE TABLE §.cron_exec (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, job VARCHAR(64) NOT NULL, fire BIGINT NOT NULL, " +
		"inst VARCHAR(128) NOT NULL, at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3), KEY idx_cron_job_fire (job, fire)) ENGINE=InnoDB"
)

// Cfg 是 Build 的参数。
type Cfg struct {
	App      *db.AppPool
	Instance string
	Locks    schedrt.LockStore // nil = 真实 MySQL 的 job_locks
	TTL      time.Duration
	Tick     time.Duration
	Logger   *zap.Logger
	OnResult func(schedrt.Result)
	// Builtin 为 true 时另登记生产的四个内部作业（partition.maintain 无 maint 池 → 明确跳过）。
	Builtin bool
	// ProbeEvery 是两个探针作业的间隔（缺省 1s）。
	ProbeEvery time.Duration
}

// Build 创建 Runner。
func Build(c Cfg) (*schedrt.Runner, error) {
	ids := uid.NewGenerator(clock.System())
	tasks, err := taskrepo.New(c.App, ids)
	if err != nil {
		return nil, err
	}
	locks := c.Locks
	if locks == nil {
		if locks, err = schedrepo.NewLocks(c.App); err != nil {
			return nil, err
		}
	}
	reg := taskrt.NewRegistry()
	if err := reg.Register(taskrt.Spec{Type: taskrt.TypeRetentionCleanup, Handler: handlers.Exec(c.App, c.Instance)}); err != nil {
		return nil, err
	}
	enq, err := taskrt.NewEnqueuer(tasks, reg, ids, nil)
	if err != nil {
		return nil, err
	}
	sc := tenantscope.PlatformForJob(0) // [tenantscope-mint] 测试装配：Scheduler 是平台进程
	jobs := schedrt.NewRegistry()
	every := c.ProbeEvery
	if every == 0 {
		every = time.Second
	}
	sched, err := schedrt.Every(every)
	if err != nil {
		return nil, err
	}
	if err := jobs.Internal(ProbeInternal, sched, 0, func(ctx context.Context, fire time.Time) error {
		_, err := c.App.Executor(ctx).Exec(ctx, db.Q("INSERT INTO cron_exec (job, fire, inst) VALUES (?, ?, ?)"), ProbeInternal, fire.Unix(), c.Instance)
		return err
	}); err != nil {
		return nil, err
	}
	if err := jobs.Task(ProbeTask, sched, schedrt.TaskSpec{Type: taskrt.TypeRetentionCleanup}); err != nil {
		return nil, err
	}
	if c.Builtin {
		idem, err := idemrepo.New(c.App, idemrepo.Options{})
		if err != nil {
			return nil, err
		}
		sessions, err := schedrepo.NewSessions(c.App)
		if err != nil {
			return nil, err
		}
		if err := builtin.Register(jobs, builtin.Deps{Scope: sc, Clock: clock.System(), Logger: c.Logger, Tasks: tasks, Idempotency: idem, Sessions: sessions}); err != nil {
			return nil, err
		}
	}
	return schedrt.NewRunner(schedrt.Config{
		Locks: locks, Registry: jobs, Enqueuer: enq, Scope: sc, Logger: c.Logger, Instance: c.Instance, LeaseTTL: c.TTL, Tick: c.Tick, OnResult: c.OnResult,
	})
}
