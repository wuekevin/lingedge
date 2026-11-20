package server

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/schedrt"
	"github.com/wuekevin/lingedge/internal/platform/schedrt/builtin"
	schedrepo "github.com/wuekevin/lingedge/internal/platform/schedrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/scope"
)

// schedulerComponent 是 scheduler 角色的真实运行时（Story 1.15）：schedrt.Runner 的生命周期封装。
// 与 worker 一样，其 ctx 与进程 ctx 解耦：SIGTERM 时由 Shutdown 触发停止（等待在途作业并释放 leader 租约）。
type schedulerComponent struct {
	r      *schedrt.Runner
	log    *zap.Logger
	maint  *lazyPartitions
	cancel context.CancelFunc
	done   chan struct{}
}

func (c *schedulerComponent) Name() string { return string(RoleScheduler) }

func (c *schedulerComponent) Start(ctx context.Context) {
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.cancel = cancel
	c.log.Info("角色已装配（Scheduler 运行时）", zap.String("role", c.Name()))
	go func() {
		defer close(c.done)
		if err := c.r.Run(sctx); err != nil {
			c.log.Error("Scheduler 异常退出", zap.Error(err))
		}
	}()
}

func (c *schedulerComponent) Shutdown(ctx context.Context) error {
	if c.cancel == nil {
		return nil
	}
	c.cancel()
	var err error
	select {
	case <-c.done:
	case <-ctx.Done():
		err = fmt.Errorf("调度器未在停机预算内退出: %w", ctx.Err())
	}
	if c.maint != nil {
		err = errors.Join(err, c.maint.Close())
	}
	return err
}

// lazyPartitions 是 builtin.PartitionStore：首次使用时才以 maint 账号打开独立连接池，打开失败只让本次作业失败（告警并按窗口内退避重试），
// 绝不影响服务启动与写入（database/README §5.1「失败告警但不影响写入」）。maint 池不用于请求路径。
type lazyPartitions struct {
	open func(ctx context.Context) (*db.MaintPool, error)
	mu   sync.Mutex
	pool *db.MaintPool
	repo *schedrepo.Partitions
}

var _ builtin.PartitionStore = (*lazyPartitions)(nil)

func (l *lazyPartitions) get(ctx context.Context) (*schedrepo.Partitions, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.repo != nil {
		return l.repo, nil
	}
	p, err := l.open(ctx)
	if err != nil {
		return nil, fmt.Errorf("无法以 maint 账号连接数据库: %w", err)
	}
	if p == nil {
		return nil, errors.New("maint 连接池工厂返回空池")
	}
	repo, err := schedrepo.NewPartitions(p)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	l.pool, l.repo = p, repo
	return repo, nil
}

func (l *lazyPartitions) ListPartitioned(ctx context.Context, sc scope.Scope) ([]builtin.PartitionedTable, error) {
	r, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	return r.ListPartitioned(ctx, sc)
}

func (l *lazyPartitions) ReorganizePmax(ctx context.Context, sc scope.Scope, table string, add []db.PartitionSpec) error {
	r, err := l.get(ctx)
	if err != nil {
		return err
	}
	return r.ReorganizePmax(ctx, sc, table, add)
}

// Close 关闭已打开的 maint 池。
func (l *lazyPartitions) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pool == nil {
		return nil
	}
	err := l.pool.Close()
	l.pool, l.repo = nil, nil
	return err
}

// maintOpener 把 Options.OpenMaint 绑定到配置。OpenMaint 为 nil 或配置未提供 maint 账号时返回 nil（作业将明确跳过并告警）。
func maintOpener(o Options, d config.Database) func(ctx context.Context) (*db.MaintPool, error) {
	if o.OpenMaint == nil || !d.Maint.Configured() {
		return nil
	}
	return func(ctx context.Context) (*db.MaintPool, error) { return o.OpenMaint(ctx, d) }
}
