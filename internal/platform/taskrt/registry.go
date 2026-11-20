package taskrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// Result 是处理函数成功返回的结果。
type Result struct {
	// Partial=true 表示扇出任务部分成功（终态 partial）；否则 success。
	Partial bool
	// Data 写入 tasks.result_json（须可 JSON 序列化；不得含敏感明文，输出时会再脱敏）。
	Data any
}

// Handler 是任务处理函数。契约：
//
//   - 必须幂等：同一任务可能被至少执行一次（租约过期回收、Worker 崩溃后重跑），效果必须恰好一次（以业务键/幂等键去重，ADR-019 §5）；
//   - 以 run.Scope 访问数据（租户任务为租户 Scope，平台任务为平台 Scope）；
//   - 长任务须周期性调用 run.Checkpoint()（取消与租约丢失都从这里或 ctx 取消得知）；
//   - 返回 nil/Result 成功；ErrCanceled → canceled；Permanent(...) → failed（不重试）；其他 error → 退避重试，超过 max_attempts 则 failed。
type Handler func(ctx context.Context, run *Run) (Result, error)

// Spec 是一个已登记任务类型。
type Spec struct {
	Type        Type
	Handler     Handler
	MaxAttempts uint32        // 0 = DefaultMaxAttempts
	Priority    int           // 0 = DefaultPriority（数值小者优先）
	Timeout     time.Duration // 单次执行上限；0 = 不限（仍受租约续约约束）
}

// 默认值（与 tasks 表列默认一致）。
const (
	DefaultMaxAttempts uint32 = 5
	DefaultPriority    int    = 100
)

// Registry 是任务类型注册表（白名单）。并发安全。
type Registry struct {
	mu    sync.RWMutex
	specs map[Type]Spec
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry { return &Registry{specs: map[Type]Spec{}} }

// Register 登记任务类型。拒绝：不在 OpenAPI TaskType 白名单内、名称带执行命令语义、重复登记、处理函数为空。
func (r *Registry) Register(s Spec) error {
	if !IsKnown(s.Type) {
		return fmt.Errorf("%w: %q 不在 OpenAPI TaskType 白名单内（新增类型须先改契约，FR-35）", ErrTypeNotRegistered, s.Type)
	}
	if why := ForbiddenTypeReason(s.Type); why != "" {
		return fmt.Errorf("%w: %q: %s", ErrTypeNotRegistered, s.Type, why)
	}
	if s.Handler == nil {
		return fmt.Errorf("taskrt: 任务类型 %q 的处理函数为空", s.Type)
	}
	if s.Timeout < 0 {
		return fmt.Errorf("taskrt: 任务类型 %q 的 Timeout 不得为负", s.Type)
	}
	if s.MaxAttempts == 0 {
		s.MaxAttempts = DefaultMaxAttempts
	}
	if s.Priority == 0 {
		s.Priority = DefaultPriority
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.specs[s.Type]; dup {
		return fmt.Errorf("taskrt: 任务类型 %q 重复登记", s.Type)
	}
	r.specs[s.Type] = s
	return nil
}

// Lookup 返回已登记类型的 Spec。
func (r *Registry) Lookup(t Type) (Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[t]
	return s, ok
}

// Types 返回已登记类型（升序）。
func (r *Registry) Types() []Type {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Type, 0, len(r.specs))
	for t := range r.specs {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Run 是处理函数拿到的执行上下文。
type Run struct {
	Task  Task
	Scope tenantscope.Scope

	rt     *Worker
	fence  uint64
	cancel atomic.Bool
	mem    *MemRecorder // 非 nil：内存运行（NewRun，测试用），Progress/Event 记录到内存而不写库
}

// MemRecorder 记录内存运行（NewRun）期间处理函数写出的进度与事件。
type MemRecorder struct {
	mu       sync.Mutex
	Progress []int
	Events   []Event
}

// NewRun 创建不连接数据库的 Run（供处理函数单测与 tasktest.AssertIdempotent）：Progress/Event 记录到返回的 MemRecorder。
// 生产路径只由 Worker 创建 Run。
func NewRun(t Task, sc tenantscope.Scope) (*Run, *MemRecorder) {
	rec := &MemRecorder{}
	r := &Run{Task: t, Scope: sc, mem: rec}
	r.cancel.Store(t.CancelRequested)
	return r, rec
}

// SetCancelRequested 模拟取消请求（仅内存运行有意义）。
func (r *Run) SetCancelRequested() { r.cancel.Store(true) }

// Progress 写回进度（0–100，围栏保护；租约已丢失返回 ErrFenceLost）。
func (r *Run) Progress(ctx context.Context, pct int) error {
	if pct < 0 || pct > 100 {
		return fmt.Errorf("taskrt: 进度 %d 超出 0–100", pct)
	}
	if r.mem != nil {
		r.mem.mu.Lock()
		r.mem.Progress = append(r.mem.Progress, pct)
		r.mem.mu.Unlock()
		return nil
	}
	return r.rt.store.SetProgress(ctx, r.rt.scope, r.Task.ID, r.fence, r.rt.owner, pct)
}

// Event 追加一条任务事件（围栏保护；data 会被脱敏校验——含敏感内容的事件被拒绝）。
func (r *Run) Event(ctx context.Context, level, message string, data any) error {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if raw, err = ValidatePayloadAny(b); err != nil {
			return err
		}
	}
	if r.mem != nil {
		r.mem.mu.Lock()
		r.mem.Events = append(r.mem.Events, Event{Level: level, Message: message, Data: raw})
		r.mem.mu.Unlock()
		return nil
	}
	return r.rt.store.AddEvent(ctx, r.rt.scope, r.Task.ID, r.fence, r.rt.owner, level, truncate(message, 1024), raw)
}

// CancelRequested 报告是否已收到取消请求（来自最近一次续约/检查点，无额外查询）。
func (r *Run) CancelRequested() bool { return r.cancel.Load() }

// Checkpoint 是协作式取消的检查点：已请求取消则返回 ErrCanceled；ctx 已取消（租约丢失/停机）则返回 ctx 错误。
func (r *Run) Checkpoint(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if c := context.Cause(ctx); c != nil {
			return c
		}
		return err
	}
	if r.cancel.Load() {
		return ErrCanceled
	}
	return nil
}

// ValidatePayloadAny 与 ValidatePayload 相同，但允许任意 JSON 值（事件 data 可以是数组/标量）。
func ValidatePayloadAny(raw json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if w := findSensitive(v, ""); w != "" {
		return nil, errors.Join(ErrSensitivePayload, errors.New(w))
	}
	return raw, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func marshalResult(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return RedactJSON(b), nil
}
