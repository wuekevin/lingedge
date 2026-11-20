package redisx

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
)

// Source 说明一次限流判定由哪一层给出（降级矩阵：Redis → 进程内 → DB 兜底）。
type Source string

const (
	SourceRedis Source = "redis"
	SourceLocal Source = "local"
	SourceDB    Source = "db"
)

// Decision 是限流判定。Degraded 为 true 表示 Redis 不可用，判定来自回退层（可能略宽松，已告警）。
type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration // 被拒绝时建议的等待时间
	Source     Source
	Degraded   bool
}

// DBFallback 是 DB 兜底层（如 users.failed_login_count/locked_until，ADR-025 §5）。由调用方的 Repository 实现，
// 本包不依赖数据库。返回 false 表示按真相源应拒绝；返回 error 时仅采用进程内判定（并告警），不因兜底层故障而误锁用户。
type DBFallback interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error)
}

// Limiter 是固定窗口限流：Redis 可用时全局精确；不可用时回退到进程内计数 + 可选 DB 兜底。
type Limiter struct {
	c     *Client
	fb    DBFallback
	local *localWindows
	log   *zap.Logger
}

// NewLimiter 创建限流器。fb 可为 nil（无 DB 兜底，仅进程内回退）。
func NewLimiter(c *Client, fb DBFallback) *Limiter {
	return &Limiter{c: c, fb: fb, local: newLocalWindows(c.clk), log: c.log}
}

var incrScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
local t = redis.call('PTTL', KEYS[1])
-- 首次计数，或键因外部原因（PERSIST/覆盖写）丢了 TTL：重设窗口，保证计数键永不无 TTL。
if n == 1 or t < 0 then redis.call('PEXPIRE', KEYS[1], ARGV[1]); t = tonumber(ARGV[1]) end
return {n, t}
`)

// Allow 对 key 在 window 内最多放行 limit 次。参数非法返回 error（编程错误）；Redis 故障不会返回 error，而是降级判定。
func (l *Limiter) Allow(ctx context.Context, key Key, limit int, window time.Duration) (Decision, error) {
	if !key.Valid() {
		return Decision{}, ErrBadKey
	}
	if limit <= 0 || window < time.Millisecond {
		return Decision{}, fmt.Errorf("redisx: 限流参数非法（limit=%d window=%s）", limit, window)
	}
	var n, pttl int64
	badReply := false
	err := l.c.run(ctx, func(r *redis.Client) error {
		res, err := incrScript.Run(ctx, r, []string{key.s}, window.Milliseconds()).Int64Slice()
		if err != nil {
			return err
		}
		if len(res) != 2 {
			badReply = true // 协议层异常不代表 Redis 不可用：不触发降级状态，仅本次回退
			return nil
		}
		n, pttl = res[0], res[1]
		return nil
	})
	if err == nil && badReply {
		err = ErrUnavailable
	}
	if err == nil {
		d := Decision{Allowed: n <= int64(limit), Remaining: max(0, limit-int(n)), Source: SourceRedis}
		if !d.Allowed && pttl > 0 {
			d.RetryAfter = time.Duration(pttl) * time.Millisecond
		}
		return d, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Decision{}, ctxErr
	}
	// 降级：进程内 + DB 兜底。
	l.c.noteFallback()
	d := l.local.allow(key.s, limit, window)
	d.Degraded = true
	if l.fb != nil {
		ok, ferr := l.fb.Allow(ctx, key.s, limit, window)
		switch {
		case ferr != nil:
			l.log.Warn("限流 DB 兜底失败：仅采用进程内判定", zap.String("key", key.s), zap.String("error", l.c.scrub(ferr)))
		default:
			d.Source = SourceDB
			if !ok {
				d.Allowed = false
				d.Remaining = 0
				if d.RetryAfter <= 0 {
					d.RetryAfter = window
				}
			}
		}
	}
	return d, nil
}

// localWindows 是进程内固定窗口计数（仅降级时使用；多进程部署时每进程各算各的，故「可能略宽松」）。
type localWindows struct {
	clk clock.Clock
	mu  sync.Mutex
	w   map[string]*window
}

type window struct {
	n   int
	end time.Time
}

const localSweepThreshold = 4096

func newLocalWindows(clk clock.Clock) *localWindows {
	return &localWindows{clk: clk, w: map[string]*window{}}
}

func (l *localWindows) allow(key string, limit int, d time.Duration) Decision {
	now := l.clk.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.w) > localSweepThreshold { // 摊销清理，防止长期降级时内存无界增长
		for k, v := range l.w {
			if !v.end.After(now) {
				delete(l.w, k)
			}
		}
	}
	w := l.w[key]
	if w == nil || !w.end.After(now) {
		w = &window{end: now.Add(d)}
		l.w[key] = w
	}
	w.n++
	dec := Decision{Allowed: w.n <= limit, Remaining: max(0, limit-w.n), Source: SourceLocal}
	if !dec.Allowed {
		dec.RetryAfter = w.end.Sub(now)
	}
	return dec
}
