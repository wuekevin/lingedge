package setup

import (
	"context"
	"sync"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/pkg/setuptoken"
)

// 缺省节流参数 [ASSUMPTION]（基线未规定）：同一来源 5 分钟内 token 失败 10 次即 429（含此后携带正确 token 的请求，直到窗口结束）；
// 每个来源每分钟最多 120 个向导请求。
const (
	defaultFailMax    = 10
	defaultFailWindow = 5 * time.Minute
	defaultRateMax    = 120
)

// guard 实现 token 校验与节流：Redis 可用时经 redisx.Limiter（全局精确），不可用/未配置时本地计数（tools-design §13、AC2）。
type guard struct {
	hashPath   string
	failMax    int
	failWindow time.Duration
	rateMax    int
	clk        clock.Clock
	rc         *redisx.Client
	lim        *redisx.Limiter

	mu    sync.Mutex
	fails map[string]*win
	rates map[string]*win
}

type win struct {
	n   int
	end time.Time
}

func newGuard(hashPath string, failMax int, failWindow time.Duration, rateMax int, clk clock.Clock, rc *redisx.Client) *guard {
	if failMax <= 0 {
		failMax = defaultFailMax
	}
	if failWindow <= 0 {
		failWindow = defaultFailWindow
	}
	if rateMax <= 0 {
		rateMax = defaultRateMax
	}
	g := &guard{hashPath: hashPath, failMax: failMax, failWindow: failWindow, rateMax: rateMax, clk: clk, rc: rc, fails: map[string]*win{}, rates: map[string]*win{}}
	if rc != nil {
		g.lim = redisx.NewLimiter(rc, nil)
	}
	return g
}

func sanitizeIP(ip string) string {
	b := make([]byte, 0, len(ip))
	for _, c := range []byte(ip) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '=':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	if len(b) == 0 {
		return "unknown"
	}
	return string(b)
}

func (g *guard) localHit(m map[string]*win, key string, window time.Duration, inc bool) (n int, retry time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clk.Now()
	w := m[key]
	if w == nil || !now.Before(w.end) {
		w = &win{end: now.Add(window)}
		m[key] = w
		if len(m) > 4096 { // 防止被来源枚举撑爆：清掉过期项
			for k, v := range m {
				if !now.Before(v.end) {
					delete(m, k)
				}
			}
		}
	}
	if inc {
		w.n++
	}
	return w.n, w.end.Sub(now)
}

// allowRequest 是每来源每分钟的请求上限。
func (g *guard) allowRequest(ctx context.Context, ip string) (bool, time.Duration) {
	id := sanitizeIP(ip)
	if g.lim != nil {
		if k, err := g.rc.Keyspace().Key("rl", "setup", id); err == nil {
			if d, err := g.lim.Allow(ctx, k, g.rateMax, time.Minute); err == nil {
				return d.Allowed, d.RetryAfter
			}
		}
	}
	n, retry := g.localHit(g.rates, id, time.Minute, true)
	return n <= g.rateMax, retry
}

// blocked 报告该来源的 token 失败次数是否已达上限（不计数）。
func (g *guard) blocked(ctx context.Context, ip string) (bool, time.Duration) {
	id := sanitizeIP(ip)
	if g.lim != nil {
		if k, err := g.rc.Keyspace().Key("rl", "setupfail", id); err == nil {
			if d, err := g.lim.Peek(ctx, k, g.failMax, g.failWindow); err == nil {
				return !d.Allowed, d.RetryAfter
			}
		}
	}
	n, retry := g.localHit(g.fails, id, g.failWindow, false)
	return n >= g.failMax, retry
}

// noteFailure 记一次 token 失败。
func (g *guard) noteFailure(ctx context.Context, ip string) {
	id := sanitizeIP(ip)
	if g.lim != nil {
		if k, err := g.rc.Keyspace().Key("rl", "setupfail", id); err == nil {
			if _, err := g.lim.Allow(ctx, k, g.failMax, g.failWindow); err == nil {
				return
			}
		}
	}
	g.localHit(g.fails, id, g.failWindow, true)
}

// verify 校验 X-Setup-Token：与 setup-token.hash 的 SHA-256 常量时间比较；哈希文件缺失/损坏一律失败（fail-closed）。
func (g *guard) verify(token string) bool {
	h, err := setuptoken.ReadHashFile(g.hashPath)
	if err != nil {
		return false
	}
	return setuptoken.Verify(token, h)
}
