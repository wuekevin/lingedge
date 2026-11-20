// Package idempotency 定义 REST 幂等键（Idempotency-Key）的存储抽象（Story 1.12 AC5、ADR-017 §4）。
//
// 真相源是 MySQL 的 idempotency_keys 表（实现见 idempotency/repository）；Redis 只允许作加速、丢失只降级，当前实现不依赖 Redis，
// 因此 Redis 不可用对幂等语义没有任何影响。
//
// 协议（由 httpx 的幂等中间件驱动）：
//
//	Begin    先占位（INSERT）；占位成功即 Acquired，由调用方执行请求；唯一键冲突时读取既有记录并判定：
//	         同键同请求 + 已有响应 → Replay；同键不同请求 → Mismatch（422）；同键同请求但仍在执行 → InFlight（409）；
//	         记录已过期 → 清除后重新占位；执行方崩溃留下的陈旧占位（超过 InFlightLease 仍无响应）可被接管。
//	Complete 写入首次响应（状态 + 响应封装）；Abandon 释放占位（执行失败时，让客户端可以用同一个键重试）。
package idempotency

import (
	"context"
	"errors"
	"time"
)

const (
	// MaxKeyLen 是 Idempotency-Key 的最大长度（idempotency_keys.idem_key VARCHAR(128)）。
	MaxKeyLen = 128
	// DefaultTTL 是记录保留时长（契约 §9.2：24 小时）。
	DefaultTTL = 24 * time.Hour
	// DefaultInFlightLease 是「执行中」占位的租约：超过它仍无响应的占位视为执行方已崩溃，可被同键请求接管。
	DefaultInFlightLease = 5 * time.Minute
	// MaxPathLen 是可入库的请求路径长度（idempotency_keys.path VARCHAR(255)）。
	MaxPathLen = 255
)

// 错误。
var (
	// ErrLost：Complete 时占位已不属于调用方（被接管或已被清理）；响应不会被缓存，调用方只记日志。
	ErrLost = errors.New("idempotency: 占位已丢失（被接管或已清理）")
)

// Request 是一次带幂等键的请求的指纹。
type Request struct {
	// PrincipalKey 隔离主体：`<platform|tenant>:<actor_id>:<tenant_id>`（≤64 字符，database/platform-identity §4.4）。
	PrincipalKey string
	// Key 是客户端提供的 Idempotency-Key（已校验长度与字符集）。
	Key string
	// Method / Path 是 HTTP 方法与实际请求路径（不含查询串）。
	Method, Path string
	// Hash 是方法、路径、规范化查询与请求体的 SHA-256（小写十六进制，64 字符）。
	Hash string
}

// Result 是 Begin 的判定。
type Result int

const (
	// Acquired：占位成功，调用方负责执行并调用 Complete/Abandon。
	Acquired Result = iota + 1
	// Replay：同键同请求且已有首次响应，调用方原样返回 Outcome 里的响应。
	Replay
	// Mismatch：同键不同请求 → 422 idempotency.key_reuse。
	Mismatch
	// InFlight：同键同请求仍在执行 → 409（请稍后重试）。
	InFlight
)

// Outcome 是 Begin 的结果。
type Outcome struct {
	Result Result
	// ID 是占位记录 id（Acquired 时有效）。
	ID uint64
	// Status/Body/TaskID 是首次响应（Replay 时有效；Body 是 httpx 的响应封装，对存储层不透明）。
	Status int
	Body   []byte
	TaskID uint64
}

// Store 是幂等键存储。实现必须并发安全。
type Store interface {
	Begin(ctx context.Context, r Request) (Outcome, error)
	// Complete 记录首次响应；taskID 为 0 表示没有关联 Task。
	Complete(ctx context.Context, id uint64, status int, body []byte, taskID uint64) error
	// Abandon 释放仍未完成的占位。
	Abandon(ctx context.Context, id uint64) error
	// DeleteExpired 删除已过期记录（最多 limit 条），返回删除条数；由 Scheduler 的内部周期作业 idempotency.cleanup 调用（Story 1.15，internal/platform/schedrt/builtin）。
	DeleteExpired(ctx context.Context, limit int) (int64, error)
}
