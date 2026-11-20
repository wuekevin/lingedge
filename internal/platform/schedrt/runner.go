package schedrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
)

// 缺省参数。[ASSUMPTION] 工程缺省，基线未规定（UD-27）：租约 30s 与 Worker 租约同量级——leader 被 kill -9 后最坏接管延迟
// ≈ LeaseTTL + LeaseTTL/3 ≈ 40s，期间周期作业只是延后（不丢：接管后补偿最近一个到期窗口）；探测间隔 1s 仅读内存，不访问数据库。
const (
	DefaultLeaseTTL = 30 * time.Second
	DefaultTick     = time.Second
	// shutdownWait 是停机时等待在途作业结束的上限。
	shutdownWait = 10 * time.Second
)

// Outcome 是一次作业触发的结果。
type Outcome string

// 触发结果。
const (
	OutcomeDone        Outcome = "done"         // 内部型：执行成功
	OutcomeEnqueued    Outcome = "enqueued"     // 任务型：本实例入队了任务
	OutcomeDuplicate   Outcome = "duplicate"    // 任务型：幂等键命中（另一实例已入队同一窗口）
	OutcomeWindowTaken Outcome = "window_taken" // 内部型：窗口租约已被占用（本窗口已由其他实例执行或正在执行）
	OutcomeSkipped     Outcome = "skipped"      // 内部型：作业明确跳过（缺少所需配置等），不是成功
	OutcomeFailed      Outcome = "failed"
)

// Result 是 OnResult 回调的内容。
type Result struct {
	Job     string
	Fire    time.Time
	Outcome Outcome
	Err     error
	TaskUID string // 任务型入队/命中时的任务 uid
}

// Config 是 Runner 的参数。
type Config struct {
	Locks    LockStore
	Registry *Registry
	// Enqueuer 仅任务型作业需要；注册表里有任务型作业而它为 nil 时 NewRunner 拒绝。
	Enqueuer *taskrt.Enqueuer
	// Scope 是入队使用的平台 Scope（调用方铸造：tenantscope.PlatformForJob）。
	Scope  scope.Scope
	Logger *zap.Logger
	Clock  clock.Clock
	// Instance 是本实例的租约持有者标识（须在实例间唯一，如 taskrt.NewOwner）。
	Instance string
	LeaseTTL time.Duration // 缺省 DefaultLeaseTTL
	Tick     time.Duration // 缺省 DefaultTick
	// OnResult 在每次触发有结果时同步回调（指标/测试）；不得阻塞。
	OnResult func(Result)
}

type jobState struct {
	job     Job
	due     time.Time // 下一次尝试时刻（零值 = 未初始化）
	fire    time.Time // 下一次尝试所属窗口起点
	running atomic.Bool
}

// Runner 是 Scheduler 运行时。
type Runner struct {
	cfg  Config
	log  *zap.Logger
	jobs []*jobState

	mu         sync.Mutex // 保护 leader 相关状态与各 jobState 的 due/fire
	leader     bool
	fence      uint64
	epoch      uint64
	lastLease  time.Time
	lastOK     time.Time
	leaderCtx  context.Context
	leaderStop context.CancelFunc

	wg sync.WaitGroup

	isLeader atomic.Bool
	failures atomic.Int64
	skips    atomic.Int64
	runs     atomic.Int64
}

// NewRunner 校验参数并创建 Runner。
func NewRunner(cfg Config) (*Runner, error) {
	if cfg.Locks == nil || cfg.Registry == nil || cfg.Logger == nil {
		return nil, errors.New("schedrt: Runner 需要 Locks、Registry 与 Logger")
	}
	if cfg.Instance == "" || len(cfg.Instance) > 128 {
		return nil, errors.New("schedrt: Instance 必须是 1–128 字符的实例标识")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System()
	}
	if cfg.LeaseTTL == 0 {
		cfg.LeaseTTL = DefaultLeaseTTL
	}
	if cfg.Tick == 0 {
		cfg.Tick = DefaultTick
	}
	if cfg.LeaseTTL < 500*time.Millisecond || cfg.Tick <= 0 || cfg.Tick > cfg.LeaseTTL/3 {
		return nil, errors.New("schedrt: LeaseTTL 须 >= 500ms，且 Tick 须在 (0, LeaseTTL/3]")
	}
	r := &Runner{cfg: cfg, log: cfg.Logger}
	for _, j := range cfg.Registry.Jobs() {
		if j.Task != nil && (cfg.Enqueuer == nil || cfg.Scope.IsZero()) {
			return nil, fmt.Errorf("schedrt: 任务型作业 %s 需要 Enqueuer 与平台 Scope", j.Name)
		}
		r.jobs = append(r.jobs, &jobState{job: j})
	}
	return r, nil
}

// IsLeader 报告本实例当前是否持有 scheduler.leader。
func (r *Runner) IsLeader() bool { return r.isLeader.Load() }

// Fence 返回最近一次成功获取/续约得到的 leader fence（非 leader 时为 0）。
func (r *Runner) Fence() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.leader {
		return 0
	}
	return r.fence
}

// Failures/Skips/Runs 是累计计数（指标）。
func (r *Runner) Failures() int64 { return r.failures.Load() }

// Skips 是作业明确跳过的累计次数。
func (r *Runner) Skips() int64 { return r.skips.Load() }

// Runs 是作业触发后得到结果的累计次数（含各类结果）。
func (r *Runner) Runs() int64 { return r.runs.Load() }

// Run 阻塞运行到 ctx 取消：周期探测选主并触发到期作业。停机时停止触发、等待在途作业（至多 shutdownWait）并释放 leader 租约。
func (r *Runner) Run(ctx context.Context) error {
	r.log.Info("Scheduler 已启动", zap.String("instance", r.cfg.Instance), zap.Duration("lease_ttl", r.cfg.LeaseTTL), zap.Int("jobs", len(r.jobs)))
	for _, st := range r.jobs {
		r.log.Info("周期作业已登记", zap.String("job", st.job.Name), zap.String("kind", st.job.Kind()), zap.String("schedule", st.job.Schedule.String()))
	}
	t := time.NewTicker(r.cfg.Tick)
	defer t.Stop()
	r.tick(ctx) // 立即尝试一次，不等第一个 Tick
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-t.C:
			r.tick(ctx)
		}
	}
	r.stopLeading()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownWait):
		r.log.Warn("停机时仍有周期作业未结束，放弃等待", zap.Duration("waited", shutdownWait))
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.cfg.Locks.Release(rctx, LeaderLock, r.cfg.Instance); err != nil {
		r.log.Warn("释放 scheduler.leader 租约失败（将在租约过期后被接管）", zap.Error(err))
	}
	r.log.Info("Scheduler 已停止", zap.String("instance", r.cfg.Instance))
	return nil
}

func (r *Runner) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	now := r.cfg.Clock.Now()
	r.mu.Lock()
	needLease := now.Sub(r.lastLease) >= r.cfg.LeaseTTL/3 // 非 leader 同样按 TTL/3 探测（kill -9 后接管延迟 ≤ TTL + TTL/3）
	r.mu.Unlock()
	if needLease {
		r.electionStep(ctx, now)
	}
	r.mu.Lock()
	if !r.leader {
		r.mu.Unlock()
		return
	}
	var launch []*jobState
	for _, st := range r.jobs {
		if !st.due.IsZero() && !now.Before(st.due) && !st.running.Load() {
			st.running.Store(true)
			launch = append(launch, st)
		}
	}
	epoch, lctx := r.epoch, r.leaderCtx
	r.mu.Unlock()
	for _, st := range launch {
		fire := st.fire
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer st.running.Store(false)
			r.runJob(lctx, epoch, st, fire)
		}()
	}
}

// electionStep 尝试获取/续约 scheduler.leader，并处理角色变化。
func (r *Runner) electionStep(ctx context.Context, now time.Time) {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.LeaseTTL/2)
	fence, ok, err := r.cfg.Locks.Acquire(cctx, LeaderLock, r.cfg.Instance, r.cfg.LeaseTTL, true)
	cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastLease = now
	switch {
	case err != nil:
		r.log.Warn("scheduler.leader 租约获取/续约失败", zap.Error(err))
		// 无法确认租约：超过一个 TTL 没有成功续约就必须自认失去领导权（此时他人可能已接管）。
		if r.leader && now.Sub(r.lastOK) >= r.cfg.LeaseTTL {
			r.demoteLocked("连续 " + r.cfg.LeaseTTL.String() + " 无法续约")
		}
	case ok:
		r.lastOK = now
		if !r.leader {
			r.promoteLocked(ctx, fence, now)
		} else {
			r.fence = fence
		}
	case r.leader:
		r.demoteLocked("租约已被其他实例接管")
	}
}

func (r *Runner) promoteLocked(ctx context.Context, fence uint64, now time.Time) {
	r.leader, r.fence = true, fence
	r.epoch++
	r.leaderCtx, r.leaderStop = context.WithCancel(ctx)
	r.isLeader.Store(true)
	for _, st := range r.jobs {
		// 补偿最近一个已到期窗口一次（窗口租约/幂等键保证它不会与其他实例重复）；没有已到期窗口则等下一个。
		if p := st.job.Schedule.Prev(now); !p.IsZero() {
			st.fire, st.due = p, now
		} else {
			st.fire = st.job.Schedule.Next(now)
			st.due = st.fire
		}
	}
	r.log.Info("成为 Scheduler leader", zap.String("instance", r.cfg.Instance), zap.Uint64("fence", fence))
}

func (r *Runner) demoteLocked(reason string) {
	r.leader, r.fence = false, 0
	r.isLeader.Store(false)
	if r.leaderStop != nil {
		r.leaderStop()
	}
	for _, st := range r.jobs {
		st.due, st.fire = time.Time{}, time.Time{}
	}
	r.log.Warn("失去 Scheduler leader", zap.String("instance", r.cfg.Instance), zap.String("reason", reason))
}

func (r *Runner) stopLeading() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.leader {
		r.leader = false
		r.isLeader.Store(false)
		if r.leaderStop != nil {
			r.leaderStop()
		}
	}
}

// runJob 触发一个窗口并据结果推进下一次触发。
func (r *Runner) runJob(ctx context.Context, epoch uint64, st *jobState, fire time.Time) {
	j := st.job
	var res Result
	if j.Task != nil {
		res = r.enqueueTask(ctx, j, fire)
	} else {
		res = r.runInternal(ctx, j, fire)
	}
	if ctx.Err() != nil && res.Outcome == OutcomeFailed { // 失去领导权/停机导致的取消不是作业故障
		return
	}
	r.runs.Add(1)
	r.report(res)
	r.advance(epoch, st, fire, res)
}

func (r *Runner) report(res Result) {
	switch res.Outcome {
	case OutcomeFailed:
		r.failures.Add(1)
		r.log.Error("周期作业失败（告警）", zap.Bool("alert", true), zap.String("job", res.Job), zap.Time("fire", res.Fire), zap.Error(res.Err))
	case OutcomeSkipped:
		r.skips.Add(1)
		r.log.Warn("周期作业已跳过，本次没有执行（告警）", zap.Bool("alert", true), zap.String("job", res.Job), zap.Time("fire", res.Fire), zap.Error(res.Err))
	case OutcomeEnqueued:
		r.log.Info("周期作业已入队", zap.String("job", res.Job), zap.Time("fire", res.Fire), zap.String("task", res.TaskUID))
	case OutcomeDuplicate:
		r.log.Info("周期作业窗口已由其他 Scheduler 入队（幂等键命中）", zap.String("job", res.Job), zap.Time("fire", res.Fire), zap.String("task", res.TaskUID))
	case OutcomeWindowTaken:
		r.log.Debug("周期作业窗口已被占用，本实例不执行", zap.String("job", res.Job), zap.Time("fire", res.Fire))
	default:
		r.log.Debug("周期作业完成", zap.String("job", res.Job), zap.Time("fire", res.Fire))
	}
	if r.cfg.OnResult != nil {
		r.cfg.OnResult(res)
	}
}

// advance 计算下一次触发：失败时在窗口内短退避重试，否则取下一个窗口（若执行期间又错过了窗口，补偿最近一个）。
func (r *Runner) advance(epoch uint64, st *jobState, fire time.Time, res Result) {
	now := r.cfg.Clock.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.leader || r.epoch != epoch {
		return
	}
	s := st.job.Schedule
	next := s.Next(fire)
	if res.Outcome == OutcomeFailed {
		if d := retryDelay(fire, next); d > 0 {
			st.fire, st.due = fire, now.Add(d)
			return
		}
	}
	if p := s.Prev(now); p.After(fire) {
		st.fire, st.due = p, now
		return
	}
	st.fire, st.due = next, next
}

// retryDelay 返回失败后的重试延迟：窗口长度的 1/4，限制在 [5s, 5min]；窗口太短（重试不会早于下一个窗口的一半）则不单独重试。
func retryDelay(fire, next time.Time) time.Duration {
	gap := next.Sub(fire)
	d := gap / 4
	if d < 5*time.Second {
		d = 5 * time.Second
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	if next.IsZero() || d*2 >= gap {
		return 0
	}
	return d
}

// windowHold 是内部型作业完成后窗口租约的保持时长：到下一个触发点前一刻（留出 min(1s, gap/4) 余量，吸收时钟偏差，避免下一个窗口被误判为已占用）。
func windowHold(now, fire, next time.Time) time.Duration {
	slack := next.Sub(fire) / 4
	if slack > time.Second {
		slack = time.Second
	}
	h := next.Sub(now) - slack
	if h < time.Millisecond {
		h = time.Millisecond
	}
	return h
}

func (r *Runner) enqueueTask(ctx context.Context, j Job, fire time.Time) Result {
	res := Result{Job: j.Name, Fire: fire}
	spec := j.Task
	var payload json.RawMessage
	var err error
	if spec.Payload != nil {
		payload, err = spec.Payload(fire)
	} else {
		payload, err = json.Marshal(map[string]string{"scheduled_for": fire.UTC().Format(time.RFC3339)})
	}
	if err != nil {
		res.Outcome, res.Err = OutcomeFailed, fmt.Errorf("生成任务 payload: %w", err)
		return res
	}
	t, created, err := r.cfg.Enqueuer.Enqueue(ctx, r.cfg.Scope, taskrt.EnqueueRequest{
		Type: spec.Type, Payload: payload, IdempotencyKey: IdempotencyKey(j.Name, fire), Priority: spec.Priority, MaxAttempts: spec.MaxAttempts,
	})
	if err != nil {
		res.Outcome, res.Err = OutcomeFailed, fmt.Errorf("入队 %s: %w", spec.Type, err)
		return res
	}
	res.TaskUID = t.UID
	res.Outcome = OutcomeDuplicate
	if created {
		res.Outcome = OutcomeEnqueued
	}
	return res
}

func (r *Runner) runInternal(ctx context.Context, j Job, fire time.Time) Result {
	res := Result{Job: j.Name, Fire: fire}
	lock := JobLockPrefix + j.Name
	_, ok, err := r.cfg.Locks.Acquire(ctx, lock, r.cfg.Instance, j.Timeout, false)
	if err != nil {
		res.Outcome, res.Err = OutcomeFailed, fmt.Errorf("获取窗口租约: %w", err)
		return res
	}
	if !ok {
		res.Outcome = OutcomeWindowTaken
		return res
	}
	rctx, cancel := context.WithTimeout(ctx, j.Timeout)
	runErr := safeRun(rctx, j.Run, fire)
	cancel()

	next := j.Schedule.Next(fire)
	hold := windowHold(r.cfg.Clock.Now(), fire, next)
	switch {
	case runErr == nil:
		res.Outcome = OutcomeDone
	case errors.Is(runErr, ErrSkipped):
		// 跳过不是「本窗口已完成」：立即释放窗口租约，使补上所缺条件（如配置 maint 账号）并重启后无需等到下一个窗口；
		// 本进程内仍按正常节奏推进到下一个窗口，所以每个窗口只告警一次，不会刷屏。
		res.Outcome, res.Err = OutcomeSkipped, runErr
		hold = time.Millisecond
	default:
		res.Outcome, res.Err = OutcomeFailed, runErr
		if d := retryDelay(fire, next); d > 0 {
			hold = d // 窗口租约只保持到重试时刻，之后（本实例或接管者）可以重试
		} else {
			hold = time.Millisecond
		}
	}
	hctx, hcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer hcancel()
	if ok, err := r.cfg.Locks.Hold(hctx, lock, r.cfg.Instance, hold); err != nil || !ok {
		r.log.Warn("无法更新窗口租约（执行期间租约已过期或数据库故障）", zap.String("job", j.Name), zap.Bool("held", ok), zap.Error(err))
	}
	return res
}

// safeRun 把 panic 转成错误（周期作业的缺陷不应带崩整个进程）。
func safeRun(ctx context.Context, fn JobFunc, fire time.Time) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("作业 panic: %v", p)
		}
	}()
	return fn(ctx, fire)
}
