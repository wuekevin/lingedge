package taskrt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/pkg/redact"
)

// 默认值与依据（ADR-019 把租约/轮询默认值留给基线测试；本 Story 的实测与取舍见 docs/bmad/stories/1-14-*.md「基线数值」）。
//
//   - DefaultLease 30s、续约 lease/3=10s：容忍连续丢失 2 次续约（DB 抖动/GC 停顿）仍不丢租约；kill -9 后最坏恢复延迟 ≈ lease + 一个轮询周期。
//   - DefaultPollInterval 2s：Redis 唤醒丢失/不可用时，任务入队到被领取的最坏额外延迟 ≈ 2s；每个 Worker 每 2s 一次走 idx_tasks_claim 的空领取查询
//     （实测亚毫秒～毫秒级），Worker 数十个时也只有个位数 QPS，远低于 MySQL 队列的能力（ADR-019 后果）。
//   - DefaultDrainTimeout 25s：小于 server 的 ShutdownTimeout 30s，留出归还任务的时间。
const (
	DefaultLease        = 30 * time.Second
	DefaultPollInterval = 2 * time.Second
	DefaultConcurrency  = 4
	DefaultDrainTimeout = 25 * time.Second
)

// ErrShutdown 是 Worker 强制停机时取消处理函数 ctx 的原因（任务随后被 Release 回 pending）。
var ErrShutdown = errors.New("taskrt: worker 停机")

// WorkerConfig 是 Worker 参数（零值取默认）。
type WorkerConfig struct {
	Store    Store
	Registry *Registry
	Logger   *zap.Logger
	// Redis 可为 nil（无唤醒，纯轮询）。
	Redis *redisx.Client

	// Owner 是 lease_owner 标识（须全局唯一：host:pid:随机后缀）；空则自动生成。
	Owner        string
	Lease        time.Duration
	PollInterval time.Duration
	Concurrency  int
	DrainTimeout time.Duration
	Backoff      BackoffConfig
	// ReapEvery 是回收周期；0 = 每个轮询周期都回收（回收是一条走 idx_tasks_lease 的 UPDATE，空转成本很低）。
	ReapEvery time.Duration
	// Scope 是 Worker 持有的平台 Scope（由装配层经 tenantscope.PlatformForJob 构造并传入）。
	Scope tenantscope.Scope
	// Clock 仅用于回收节流（租约时间以数据库 NOW(3) 为准）；nil = 系统时钟。
	Clock clock.Clock
}

// Worker 是任务运行时的消费端：领取 → 执行 → 续约 → 写回；同时承担租约过期回收。
type Worker struct {
	store   Store
	reg     *Registry
	log     *zap.Logger
	rc      *redisx.Client
	owner   string
	lease   time.Duration
	poll    time.Duration
	conc    int
	drain   time.Duration
	backoff BackoffConfig
	reapGap time.Duration
	scope   tenantscope.Scope

	clk      clock.Clock
	wake     chan struct{}
	lastReap time.Time
}

// NewOwner 生成 lease_owner：host:pid:随机后缀。
func NewOwner(ids *uid.Generator) string {
	h, _ := os.Hostname()
	if h == "" {
		h = "unknown"
	}
	suffix := "x"
	if ids != nil {
		if id, err := ids.New(); err == nil {
			suffix = id[len(id)-8:]
		}
	}
	o := h + ":" + strconv.Itoa(os.Getpid()) + ":" + suffix
	if len(o) > 128 {
		o = o[len(o)-128:]
	}
	return o
}

// NewWorker 校验并创建 Worker。
func NewWorker(c WorkerConfig) (*Worker, error) {
	if c.Store == nil || c.Registry == nil {
		return nil, errors.New("taskrt: Worker 需要 Store 与 Registry")
	}
	if c.Scope.IsZero() || !c.Scope.IsPlatform() {
		return nil, fmt.Errorf("%w: Worker 需要平台 Scope", ErrScope)
	}
	if c.Logger == nil {
		c.Logger = zap.NewNop()
	}
	if c.Lease <= 0 {
		c.Lease = DefaultLease
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
	if c.Backoff.Base <= 0 || c.Backoff.Max <= 0 {
		c.Backoff = DefaultBackoff
	}
	if c.Lease < 3*time.Millisecond {
		return nil, errors.New("taskrt: Lease 过小")
	}
	if c.Clock == nil {
		c.Clock = clock.System()
	}
	if c.Owner == "" {
		c.Owner = NewOwner(nil)
	}
	return &Worker{store: c.Store, reg: c.Registry, log: c.Logger, rc: c.Redis, owner: c.Owner, lease: c.Lease, poll: c.PollInterval,
		conc: c.Concurrency, drain: c.DrainTimeout, backoff: c.Backoff, reapGap: c.ReapEvery, scope: c.Scope, clk: c.Clock, wake: make(chan struct{}, 1)}, nil
}

// Owner 返回 lease_owner 标识。
func (w *Worker) Owner() string { return w.owner }

// Nudge 请求立即进行一次领取（唤醒；非阻塞，多次合并）。
func (w *Worker) Nudge() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run 阻塞运行到 ctx 取消：之后停止领取，等待在途任务至多 DrainTimeout，超时则取消处理函数并把任务归还（Release）。
// 注册表为空时不领取、不回收（没有可执行的类型），只等待停机。
func (w *Worker) Run(ctx context.Context) error {
	types := w.reg.Types()
	if len(types) == 0 {
		w.log.Info("任务运行时：未登记任何任务类型，Worker 空闲（不领取、不回收）")
		<-ctx.Done()
		return nil
	}
	w.log.Info("任务运行时已启动", zap.String("owner", w.owner), zap.Strings("types", typeStrings(types)),
		zap.Duration("lease", w.lease), zap.Duration("poll_interval", w.poll), zap.Int("concurrency", w.conc), zap.Bool("redis_wakeup", w.rc != nil))

	if w.rc != nil {
		ch, err := w.rc.Keyspace().Channel(WakeModule, redisx.PurposeTaskWakeup)
		if err != nil {
			return err
		}
		sub, err := w.rc.Subscribe(ctx, ch, redisx.SubscribeOptions{
			Handler:      func(context.Context, string) { w.Nudge() },
			Poll:         func(context.Context) error { w.Nudge(); return nil },
			PollInterval: w.poll,
		})
		if err != nil {
			w.log.Warn("订阅任务唤醒频道失败，仅轮询", zap.Error(err))
		} else {
			defer sub.Close()
		}
	}

	forced, forceCancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer forceCancel(nil)
	sem := make(chan struct{}, w.conc)
	var wg sync.WaitGroup

	tick := time.NewTicker(w.poll)
	defer tick.Stop()
	w.cycle(ctx, forced, types, sem, &wg)
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
		case <-w.wake:
		}
		w.cycle(ctx, forced, types, sem, &wg)
	}

	// 停机：不再领取；等待在途任务，超时则强制取消并归还。
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(w.drain):
		w.log.Warn("排空超时，取消在途任务并归还", zap.Duration("drain_timeout", w.drain))
		forceCancel(ErrShutdown)
		<-done
	}
	w.log.Info("任务运行时已停止", zap.String("owner", w.owner))
	return nil
}

func typeStrings(ts []Type) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

// cycle 是一个轮询周期：回收过期租约 → 尽可能多地领取并执行。
func (w *Worker) cycle(ctx, forced context.Context, types []Type, sem chan struct{}, wg *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}
	if now := w.clk.Now(); w.reapGap <= 0 || now.Sub(w.lastReap) >= w.reapGap {
		w.lastReap = now
		if _, err := w.Reap(ctx); err != nil && ctx.Err() == nil {
			w.log.Warn("回收过期租约失败", zap.Error(err))
		}
	}
	for ctx.Err() == nil {
		free := cap(sem) - len(sem)
		if free <= 0 {
			return
		}
		tasks, err := w.store.Claim(ctx, w.scope, w.owner, types, free, w.lease)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("领取任务失败", zap.Error(err))
			}
			return
		}
		for _, t := range tasks {
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()
				w.execute(forced, t)
			}()
		}
		if len(tasks) < free {
			return
		}
	}
}

// Reap 回收租约过期且仍 running 的任务（Worker 每个周期自动调用；Scheduler 的 tasks.reap 内部周期作业亦调用同一逻辑）。
func (w *Worker) Reap(ctx context.Context) (ReapResult, error) {
	r, err := w.store.Reap(ctx, w.scope, 100, w.backoff)
	if err == nil && (r.Requeued+r.Failed+r.Canceled) > 0 {
		w.log.Warn("已回收租约过期的任务", zap.Int("requeued", r.Requeued), zap.Int("failed", r.Failed), zap.Int("canceled", r.Canceled))
	}
	return r, err
}

func (w *Worker) execute(forced context.Context, t Task) {
	log := w.log.With(zap.String("task_uid", t.UID), zap.String("task_type", string(t.Type)), zap.String("request_id", t.RequestID),
		zap.Uint64("fence", t.Fence), zap.Uint32("attempt", t.Attempts))
	spec, ok := w.reg.Lookup(t.Type)
	// 终态写回独立于处理函数的 ctx：即使被取消也要尽力把结果/归还写回。
	wctx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(forced), 10*time.Second)
	}
	if !ok { // 不应发生（领取已按已登记类型过滤）；保守地归还而不是失败
		c, cancel := wctx()
		defer cancel()
		_ = w.store.Release(c, w.scope, t.ID, t.Fence, w.owner)
		log.Error("领取到未登记类型的任务，已归还")
		return
	}
	sc, err := executionScope(t)
	if err != nil {
		c, cancel := wctx()
		defer cancel()
		w.finish(c, log, t, Finish{Status: StatusFailed, ErrorCode: "task.failed", ErrorMessage: "invalid task scope"})
		return
	}

	hctx, hcancel := context.WithCancelCause(forced)
	defer hcancel(nil)
	if spec.Timeout > 0 {
		var tcancel context.CancelFunc
		hctx, tcancel = context.WithTimeout(hctx, spec.Timeout)
		defer tcancel()
	}
	run := &Run{Task: t, Scope: sc, rt: w, fence: t.Fence}
	run.cancel.Store(t.CancelRequested)

	var lost sync.Once
	var fenceLost bool
	stopRenew := make(chan struct{})
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		tk := time.NewTicker(w.lease / 3)
		defer tk.Stop()
		for {
			select {
			case <-stopRenew:
				return
			case <-tk.C:
				c, cancel := context.WithTimeout(context.WithoutCancel(forced), w.lease/3)
				cr, err := w.store.Renew(c, w.scope, t.ID, t.Fence, w.owner, w.lease)
				cancel()
				switch {
				case errors.Is(err, ErrFenceLost):
					lost.Do(func() { fenceLost = true; hcancel(ErrFenceLost) })
					log.Warn("续约被围栏拒绝：租约已丢失，取消处理函数")
					return
				case err != nil:
					log.Warn("续约失败（将在下个周期重试）", zap.Error(err))
				case cr:
					run.cancel.Store(true)
				}
			}
		}
	}()

	res, herr := w.invoke(hctx, spec, run, log)
	close(stopRenew)
	<-renewDone

	c, cancel := wctx()
	defer cancel()
	switch {
	case fenceLost:
		log.Warn("租约已丢失，放弃写回（任务由新持有者处理）")
	case herr == nil:
		w.finishOK(c, log, t, res)
	case errors.Is(herr, ErrCanceled):
		w.finish(c, log, t, Finish{Status: StatusCanceled})
	case errors.Is(context.Cause(hctx), ErrShutdown):
		if err := w.store.Release(c, w.scope, t.ID, t.Fence, w.owner); err != nil {
			log.Warn("停机归还任务失败（将由租约过期回收）", zap.Error(err))
		} else {
			log.Info("停机：任务已归还")
		}
	default:
		w.fail(c, log, t, herr)
	}
}

func (w *Worker) invoke(ctx context.Context, spec Spec, run *Run, log *zap.Logger) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %s", redact.Panic(r))
			log.Error("处理函数 panic（已捕获，按失败处理）", zap.String("panic", redact.Panic(r)))
		}
	}()
	return spec.Handler(ctx, run)
}

func (w *Worker) finishOK(ctx context.Context, log *zap.Logger, t Task, res Result) {
	f := Finish{Status: StatusSuccess}
	if res.Partial {
		f.Status = StatusPartial
	}
	if res.Data != nil {
		b, err := marshalResult(res.Data)
		if err != nil {
			w.finish(ctx, log, t, Finish{Status: StatusFailed, ErrorCode: "task.failed", ErrorMessage: "result is not serializable"})
			return
		}
		f.Result = b
	}
	w.finish(ctx, log, t, f)
}

func (w *Worker) fail(ctx context.Context, log *zap.Logger, t Task, herr error) {
	msg := truncate(redact.String(herr.Error()), 1024)
	var pe *permanentError
	permanent := errors.As(herr, &pe)
	code := "task.failed"
	if permanent && pe.code != "" {
		code = pe.code
	}
	if permanent || t.Attempts >= t.MaxAttempts {
		log.Warn("任务失败", zap.Bool("permanent", permanent), zap.String("error", msg))
		w.finish(ctx, log, t, Finish{Status: StatusFailed, ErrorCode: code, ErrorMessage: msg})
		return
	}
	if err := w.store.Retry(ctx, w.scope, t.ID, t.Fence, w.owner, code, msg, w.backoff); err != nil {
		if errors.Is(err, ErrFenceLost) {
			log.Warn("重试写回被围栏拒绝")
			return
		}
		log.Error("重试写回失败（将由租约过期回收）", zap.Error(err))
		return
	}
	log.Info("任务失败，已退避重试", zap.String("error", msg))
}

func (w *Worker) finish(ctx context.Context, log *zap.Logger, t Task, f Finish) {
	if err := w.store.Complete(ctx, w.scope, t.ID, t.Fence, w.owner, f); err != nil {
		if errors.Is(err, ErrFenceLost) {
			log.Warn("终态写回被围栏拒绝（僵尸写回）", zap.String("status", string(f.Status)))
			return
		}
		log.Error("终态写回失败（将由租约过期回收）", zap.Error(err))
		return
	}
	log.Info("任务结束", zap.String("status", string(f.Status)))
}

// executionScope 构造处理函数使用的 Scope：租户任务 → 租户 Scope（ADR-009 §8），平台任务 → 平台 Scope。
func executionScope(t Task) (tenantscope.Scope, error) {
	actor := uint64(0)
	if t.CreatedByUserID != nil {
		actor = *t.CreatedByUserID
	}
	if t.TenantID != nil {
		return tenantscope.FromTask(*t.TenantID, actor)
	}
	return tenantscope.PlatformForJob(actor), nil
}
