package taskrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// EnqueueRequest 是业务模块的入队请求。
type EnqueueRequest struct {
	Type Type
	// Payload 是 JSON 对象，只放引用（ID/uid）；含敏感明文即被拒绝（ValidatePayload）。
	Payload json.RawMessage
	// TenantID 仅在平台 Scope 下有效（平台任务可不带）；租户 Scope 下被 Scope 的租户覆盖。
	TenantID *uint64
	// IdempotencyKey 可选：同 (type, key) 只会有一条任务（周期任务用确定性键，如 rollup.aggregate:<窗口>）。
	IdempotencyKey string
	// RunAfter 延迟执行；零值 = 立即。
	RunAfter time.Time
	// Priority/MaxAttempts 零值取注册表里该类型的默认。数值小者优先。
	Priority    int
	MaxAttempts uint32
	ParentTask  uint64
	// RequestID 缺省取 ctx 的 request-id，再缺省新生成。
	RequestID string
}

// Enqueuer 是入队服务：白名单校验 → payload 校验 → 持久化 → 唤醒。
type Enqueuer struct {
	store Store
	reg   *Registry
	ids   *uid.Generator
	waker Waker
	// InTx 报告 ctx 是否处于调用方事务内（事务内入队在提交前唤醒无意义，由调用方提交后调用 Wake）；nil 视为不在事务内。
	InTx func(ctx context.Context) bool
}

// NewEnqueuer 创建入队服务。waker 可为 nil（等同 NopWaker）。
func NewEnqueuer(store Store, reg *Registry, ids *uid.Generator, waker Waker) (*Enqueuer, error) {
	if store == nil || reg == nil || ids == nil {
		return nil, errors.New("taskrt: Enqueuer 需要 Store、Registry 与 uid 生成器")
	}
	if waker == nil {
		waker = NopWaker{}
	}
	return &Enqueuer{store: store, reg: reg, ids: ids, waker: waker}, nil
}

// Wake 手动唤醒（事务提交之后调用）。
func (e *Enqueuer) Wake(ctx context.Context) { e.waker.Wake(ctx) }

// Enqueue 入队。created=false 表示幂等键命中，返回的是既有任务。
// 未登记类型返回 ErrTypeNotRegistered（含任何「执行命令」类——它们根本无法登记）。
func (e *Enqueuer) Enqueue(ctx context.Context, sc scope.Scope, req EnqueueRequest) (Task, bool, error) {
	spec, ok := e.reg.Lookup(req.Type)
	if !ok {
		return Task{}, false, fmt.Errorf("%w: %q", ErrTypeNotRegistered, req.Type)
	}
	payload, err := ValidatePayload(req.Payload)
	if err != nil {
		return Task{}, false, err
	}
	n := NewTask{Type: req.Type, Payload: payload, Priority: req.Priority, MaxAttempts: req.MaxAttempts, RequestID: req.RequestID}
	if n.Priority == 0 {
		n.Priority = spec.Priority
	}
	if n.MaxAttempts == 0 {
		n.MaxAttempts = spec.MaxAttempts
	}
	if req.IdempotencyKey != "" {
		if len(req.IdempotencyKey) > 128 {
			return Task{}, false, errors.New("taskrt: 幂等键超过 128 字符")
		}
		k := req.IdempotencyKey
		n.IdempotencyKey = &k
	}
	if !req.RunAfter.IsZero() {
		t := req.RunAfter.UTC()
		n.RunAfter = &t
	}
	if req.ParentTask != 0 {
		p := req.ParentTask
		n.ParentTaskID = &p
	}
	if req.TenantID != nil && sc.IsPlatform() {
		t := *req.TenantID
		n.TenantID = &t
	}
	if a := sc.ActorID(); a != 0 {
		n.CreatedByUserID = &a
	}
	if n.RequestID == "" {
		if id, ok := requestid.FromContext(ctx); ok {
			n.RequestID = id
		}
	}
	if n.RequestID == "" {
		if n.RequestID, err = e.ids.New(); err != nil {
			return Task{}, false, err
		}
	}
	if n.UID, err = e.ids.New(); err != nil {
		return Task{}, false, err
	}
	t, created, err := e.store.Insert(ctx, sc, n)
	if err != nil {
		return Task{}, false, err
	}
	if created && (e.InTx == nil || !e.InTx(ctx)) {
		e.waker.Wake(ctx)
	}
	return t, created, nil
}
