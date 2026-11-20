package taskrt

import (
	"encoding/json"
	"errors"
	"time"
)

// 运行时错误（哨兵）。
var (
	// ErrTypeNotRegistered：入队的任务类型不在注册表内（白名单拒绝，FR-35/G-12）。
	ErrTypeNotRegistered = errors.New("taskrt: 任务类型未登记（白名单拒绝）")
	// ErrSensitivePayload：payload 含敏感明文或敏感键名。
	ErrSensitivePayload = errors.New("taskrt: payload 含敏感信息（只允许引用 ID/uid）")
	// ErrFenceLost：围栏/租约不符——任务已被回收或重新领取，本持有者的写回被拒绝（僵尸 Worker）。
	ErrFenceLost = errors.New("taskrt: 围栏令牌不符，租约已丢失")
	// ErrNotCancelable：任务已终态，不可取消（409 task.not_cancelable）。
	ErrNotCancelable = errors.New("taskrt: 任务已终态，不可取消")
	// ErrCanceled：处理函数在检查点发现取消请求时返回它，任务以 canceled 终止。
	ErrCanceled = errors.New("taskrt: 任务已取消")
	// ErrScope：Scope 不满足该操作的要求（如 Worker 操作需要平台范围）。
	ErrScope = errors.New("taskrt: Scope 不满足该操作")
)

// permanentError 标记「重试无意义」的失败：任务直接 failed，不再退避重试。
type permanentError struct {
	code string
	err  error
}

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent 把 err 标记为永久失败（不重试）；code 是写入 tasks.error_code 的稳定错误码（空则用 task.failed）。
func Permanent(code string, err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{code: code, err: err}
}

// Task 是 tasks 表的一行（含联查出的 uid 引用）。
type Task struct {
	ID              uint64
	UID             string
	Type            Type
	Status          Status
	Priority        int
	TenantID        *uint64
	TenantUID       *string
	Payload         json.RawMessage
	Result          json.RawMessage
	ErrorCode       *string
	ErrorMessage    *string
	Progress        int
	Attempts        uint32
	MaxAttempts     uint32
	RunAfter        time.Time
	LeaseOwner      *string
	LeaseUntil      *time.Time
	Fence           uint64
	IdempotencyKey  *string
	ParentTaskID    *uint64
	ParentTaskUID   *string
	RequestID       string
	CreatedByUserID *uint64
	CreatedByUID    *string
	CancelRequested bool
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

// Event 是 task_events 的一行。
type Event struct {
	UID       string
	Level     string // info|warn|error
	Message   string
	Data      json.RawMessage
	CreatedAt time.Time
}

// 事件级别。
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// NewTask 是入队参数（Repository 层）。
type NewTask struct {
	UID             string
	TenantID        *uint64 // 平台任务为 nil；租户 Scope 下由 Repository 强制为 Scope 的租户
	Type            Type
	Priority        int
	Payload         json.RawMessage
	MaxAttempts     uint32
	RunAfter        *time.Time // nil = 立即
	IdempotencyKey  *string
	ParentTaskID    *uint64
	RequestID       string
	CreatedByUserID *uint64
}

// Filter 是列表过滤条件（零值 = 不过滤）。
type Filter struct {
	Type        Type
	Status      Status
	ResourceUID string
	RequestID   string
	Desc        bool // 按 created_at 排序方向
	Offset      int
	Limit       int
}

// Finish 是终态写回。
type Finish struct {
	Status       Status // success|partial|failed|canceled
	Result       json.RawMessage
	ErrorCode    string
	ErrorMessage string
}

// BackoffConfig 是重试/回收的退避：run_after = now + min(Base * 2^(attempts-1), Max)。
type BackoffConfig struct {
	Base time.Duration
	Max  time.Duration
}

// DefaultBackoff：首次重试 5s，指数增长，上限 5min（[ASSUMPTION] 工程缺省，基线未规定）。
var DefaultBackoff = BackoffConfig{Base: 5 * time.Second, Max: 5 * time.Minute}

// ReapResult 是一次回收的统计。
type ReapResult struct {
	Requeued int // 回到 pending（退避）
	Failed   int // 超限 failed
	Canceled int // 已请求取消 → canceled
}

// ErrIdempotencyKeyTaken：幂等键已被不可见（他租户）的任务占用；不泄漏该任务。
var ErrIdempotencyKeyTaken = errors.New("taskrt: 幂等键已被占用")
