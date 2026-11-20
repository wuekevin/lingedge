package redisx

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
)

// Loader 从真相源（MySQL 等）读取值。它的错误原样返回给调用方：缓存层从不吞掉真相源的错误。
type Loader func(ctx context.Context) ([]byte, error)

// Cache 是读缓存：命中返回缓存值；miss 或 Redis 故障一律直读真相源（结果正确，只是更慢）；
// 读到后尽力回填（失败不影响结果）。Redis 恢复或被清空后，下一次 miss 自动重建缓存，无需人工干预。
//
// 一致性边界：Invalidate 在 Redis 不可用时无法删除缓存项；若 Redis 随后在未清空的情况下恢复，旧值最多再存活到其 TTL。
// 因此缓存 TTL 必须取业务能容忍的陈旧上限（会话/权限 ≤60s，ADR-025 §1），不得把 Redis 当作失效的唯一手段。
type Cache struct{ c *Client }

// NewCache 创建缓存。
func NewCache(c *Client) *Cache { return &Cache{c: c} }

// GetOrLoad 见 Cache 说明。ttl 必须为正（编程错误，先于 load 失败）。
func (ca *Cache) GetOrLoad(ctx context.Context, k Key, ttl time.Duration, load Loader) ([]byte, error) {
	if !k.Valid() {
		return nil, ErrBadKey
	}
	if ttl < time.Millisecond {
		return nil, ErrNoTTL
	}
	v, ok, err := ca.c.Get(ctx, k)
	switch {
	case err == nil && ok:
		return []byte(v), nil
	case err != nil && !errors.Is(err, ErrUnavailable):
		return nil, err // ctx 取消 / client 已关闭
	case err != nil:
		ca.c.noteFallback()
	}
	val, lerr := load(ctx)
	if lerr != nil {
		return nil, lerr
	}
	if err := ca.c.Set(ctx, k, string(val), ttl); err != nil {
		switch {
		case errors.Is(err, ErrSensitiveValue):
			ca.c.log.Warn("缓存值含敏感信息，已跳过回填（编程错误）", zap.String("key", k.s))
		case errors.Is(err, ErrUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// 尽力回填：Redis 故障时直接返回真相源的值。
		default:
			ca.c.log.Warn("缓存回填失败", zap.String("key", k.s), zap.String("error", ca.c.scrub(err)))
		}
	}
	return val, nil
}

// Invalidate 尽力删除缓存项；Redis 不可用时返回 ErrUnavailable（调用方可忽略，缓存靠 TTL 自然过期）。
func (ca *Cache) Invalidate(ctx context.Context, k Key) error { return ca.c.Del(ctx, k) }
