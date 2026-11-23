package redisx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/pkg/redact"
)

// 调用方应把这两类错误都当作「Redis 没给出答案」并走降级路径，而不是向用户返回失败。
var (
	// ErrUnavailable：Redis 不可用（连接/超时/认证/加载中等）。降级期间调用快速返回它，不发出网络请求。
	ErrUnavailable = errors.New("redisx: Redis 不可用（降级模式）")
	// ErrClosed：Client 已关闭。
	ErrClosed = errors.New("redisx: client 已关闭")
)

// State 是 Redis 的健康状态。
type State int32

const (
	// StateUp：最近一次探测/调用成功。
	StateUp State = iota
	// StateDegraded：Redis 不可用，系统按降级矩阵运行；/healthz/ready 应报告 degraded 而非失败（ADR-025 §8）。
	StateDegraded
)

func (s State) String() string {
	if s == StateUp {
		return "up"
	}
	return "degraded"
}

// Status 是供 /healthz/ready（Story 1.12 接线）使用的快照。LastError 已脱敏。
type Status struct {
	State     State
	Since     time.Time
	LastError string
	Fallbacks uint64 // 累计降级次数（限流/缓存回退真相源）
}

// Options 是 New 的可选依赖。
type Options struct {
	Logger *zap.Logger
	Clock  clock.Clock
}

// Client 是对 go-redis 的受控封装：所有 Redis 访问都经它，因而 TTL、键名、脱敏、降级策略无法被绕开。
type Client struct {
	rdb  *redis.Client
	cfg  Config
	ks   Keyspace
	log  *zap.Logger
	clk  clock.Clock
	addr string

	state     atomic.Int32
	fallbacks atomic.Uint64

	mu        sync.Mutex
	since     time.Time
	lastErr   string
	recovered chan struct{} // 每次恢复时关闭并重建（广播）

	stop      chan struct{}
	lifeMu    sync.Mutex // 保护 closed 与 wg.Add 的先后（Subscribe 与 Close 并发）
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool
}

// New 创建 Client。配置错误（环境名/地址/TLS 材料）返回 error；**Redis 不可达不是错误**：
// 以 StateDegraded 返回并记录告警，后台探测在 Redis 恢复后自动转为 Up（ADR-025 §8：不阻止启动）。
func New(ctx context.Context, cfg Config, opt Options) (*Client, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	tc, err := cfg.tlsConfig()
	if err != nil {
		return nil, err
	}
	ks, _ := NewKeyspace(cfg.Env)
	log := opt.Logger
	if log == nil {
		log = zap.NewNop()
	}
	clk := opt.Clock
	if clk == nil {
		clk = clock.System()
	}
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.Addr, DB: cfg.DB, Password: cfg.Password.Reveal(),
		TLSConfig:   tc,
		DialTimeout: cfg.DialTimeout, ReadTimeout: cfg.ReadTimeout, WriteTimeout: cfg.WriteTimeout,
		PoolSize: cfg.PoolSize, MaxRetries: -1, DisableIdentity: true,
	})
	c := &Client{rdb: rdb, cfg: cfg, ks: ks, log: log.Named("redisx"), clk: clk, addr: cfg.Addr,
		since: clk.Now(), recovered: make(chan struct{}), stop: make(chan struct{})}

	pctx, cancel := context.WithTimeout(ctx, cfg.DialTimeout+cfg.ReadTimeout)
	err = rdb.Ping(pctx).Err()
	cancel()
	if err != nil {
		c.state.Store(int32(StateDegraded))
		c.lastErr = c.scrub(err)
		c.log.Warn("Redis 不可达：以降级模式启动（限流回退进程内/DB，缓存直读真相源，广播依赖轮询兜底）",
			zap.String("addr", cfg.Addr), zap.String("error", c.lastErr))
	}
	c.wg.Add(1)
	go c.probeLoop()
	return c, nil
}

// Keyspace 返回本环境的键构造器。
func (c *Client) Keyspace() Keyspace { return c.ks }

// State 返回当前状态（无网络请求）。
func (c *Client) State() State { return State(c.state.Load()) }

// Status 返回状态快照（无网络请求）。
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{State: c.State(), Since: c.since, LastError: c.lastErr, Fallbacks: c.fallbacks.Load()}
}

// Close 停止后台探测并关闭连接池。可重复调用。
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.lifeMu.Lock()
		c.closed.Store(true)
		close(c.stop)
		c.lifeMu.Unlock()
		c.wg.Wait()
		err = c.rdb.Close()
	})
	return err
}

func (c *Client) scrub(err error) string {
	if err == nil {
		return ""
	}
	return redact.String(err.Error())
}

func (c *Client) noteFallback() { c.fallbacks.Add(1) }

func (c *Client) markDown(err error) {
	msg := c.scrub(err)
	c.mu.Lock()
	c.lastErr = msg
	if State(c.state.Swap(int32(StateDegraded))) == StateDegraded {
		c.mu.Unlock()
		return
	}
	c.since = c.clk.Now()
	c.mu.Unlock()
	c.log.Warn("Redis 变为不可用：进入降级模式", zap.String("addr", c.addr), zap.String("error", msg))
}

func (c *Client) markUp() {
	c.mu.Lock()
	if State(c.state.Swap(int32(StateUp))) == StateUp {
		c.mu.Unlock()
		return
	}
	c.since = c.clk.Now()
	c.lastErr = ""
	old := c.recovered
	c.recovered = make(chan struct{})
	c.mu.Unlock()
	close(old)
	c.log.Info("Redis 已恢复：退出降级模式", zap.String("addr", c.addr))
}

// recoveredCh 返回「下一次恢复」时被关闭的 channel（订阅者据此补一次轮询，弥补断线期间丢失的广播）。
func (c *Client) recoveredCh() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recovered
}

func (c *Client) probeLoop() {
	defer c.wg.Done()
	t := time.NewTicker(c.cfg.ProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), c.cfg.DialTimeout+c.cfg.ReadTimeout)
			err := c.rdb.Ping(ctx).Err()
			cancel()
			if err != nil {
				c.markDown(err)
			} else {
				c.markUp()
			}
		}
	}
}

// Ping 主动探测（会更新状态）；降级期间也会真实发出请求，供启动/诊断使用。
func (c *Client) Ping(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClosed
	}
	err := c.rdb.Ping(ctx).Err()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.markDown(err)
		return fmt.Errorf("%w: %s", ErrUnavailable, c.scrub(err))
	}
	c.markUp()
	return nil
}

// run 是所有命令的唯一出口：降级时快速失败；调用方 ctx 取消不算故障；其余错误一律降级并折叠为 ErrUnavailable。
// redis.Nil（键不存在）原样返回，由具体方法转换。
func (c *Client) run(ctx context.Context, fn func(*redis.Client) error) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.State() == StateDegraded {
		return ErrUnavailable
	}
	err := fn(c.rdb)
	switch {
	case err == nil, errors.Is(err, redis.Nil):
		return err
	case ctx.Err() != nil:
		return ctx.Err()
	}
	if isCommandError(err) {
		return fmt.Errorf("%w: 命令被拒绝: %s", ErrUnavailable, c.scrub(err))
	}
	c.markDown(err)
	return fmt.Errorf("%w: %s", ErrUnavailable, c.scrub(err))
}

// isCommandError：服务端明确拒绝某条命令（如 WRONGTYPE），Redis 本身仍可用，不触发降级。
func isCommandError(err error) bool {
	var re redis.Error
	if !errors.As(err, &re) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, io.EOF) {
		return false
	}
	msg := re.Error()
	for _, p := range []string{"LOADING", "READONLY", "NOAUTH", "WRONGPASS", "MASTERDOWN", "CLUSTERDOWN", "ERR max number of clients", "OOM", "BUSY", "TRYAGAIN"} {
		if strings.HasPrefix(msg, p) {
			return false
		}
	}
	return true
}

func checkWrite(k Key, ttl time.Duration, value string) error {
	if !k.Valid() {
		return ErrBadKey
	}
	if ttl < time.Millisecond {
		return ErrNoTTL
	}
	if redact.String(value) != value {
		return ErrSensitiveValue
	}
	return nil
}

// StructuredGuard 是「结构化缓存值」的写入守卫（Story 1.39b）：调用方为自己的缓存值提供严格的 schema 校验，返回 nil 才允许写入。
// 它**替换**（不是叠加）redact.String 的自由文本启发式——启发式会把形如 `token:manage` 的权限码当成「token: 值」而误伤。
// 守卫必须是白名单式的（只放行已知的、非机密的形态）；不能写成「找不到敏感词就放行」。nil 守卫等价于拒绝（fail-closed）。
type StructuredGuard func(value string) error

func checkStructuredWrite(k Key, ttl time.Duration, value string, guard StructuredGuard) error {
	if !k.Valid() {
		return ErrBadKey
	}
	if ttl < time.Millisecond {
		return ErrNoTTL
	}
	if guard == nil {
		return ErrSensitiveValue
	}
	if err := guard(value); err != nil {
		return fmt.Errorf("%w: %v", ErrSensitiveValue, err)
	}
	return nil
}

// SetStructured 与 Set 相同，但值由调用方的 StructuredGuard（严格 schema 白名单）校验，而非 redact.String 启发式。
// 值不通过守卫返回 ErrSensitiveValue（errors.Is 可判），不发出任何命令。
func (c *Client) SetStructured(ctx context.Context, k Key, value string, ttl time.Duration, guard StructuredGuard) error {
	if err := checkStructuredWrite(k, ttl, value, guard); err != nil {
		return err
	}
	return c.run(ctx, func(r *redis.Client) error { return r.Set(ctx, k.s, value, ttl).Err() })
}

// Set 写入字符串值。ttl 必须为正（否则 ErrNoTTL，不发出任何命令）；值含明文敏感信息返回 ErrSensitiveValue。
func (c *Client) Set(ctx context.Context, k Key, value string, ttl time.Duration) error {
	if err := checkWrite(k, ttl, value); err != nil {
		return err
	}
	return c.run(ctx, func(r *redis.Client) error { return r.Set(ctx, k.s, value, ttl).Err() })
}

// Get 读取字符串值；键不存在返回 ("", false, nil)；Redis 不可用返回 ErrUnavailable。
func (c *Client) Get(ctx context.Context, k Key) (string, bool, error) {
	if !k.Valid() {
		return "", false, ErrBadKey
	}
	var v string
	err := c.run(ctx, func(r *redis.Client) (e error) { v, e = r.Get(ctx, k.s).Result(); return })
	switch {
	case errors.Is(err, redis.Nil):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return v, true, nil
}

// Del 删除键（不存在不是错误）。
func (c *Client) Del(ctx context.Context, k Key) error {
	if !k.Valid() {
		return ErrBadKey
	}
	return c.run(ctx, func(r *redis.Client) error { return r.Del(ctx, k.s).Err() })
}

// TTL 返回键的剩余 TTL；键不存在返回 (0, false, nil)。主要供测试与诊断：断言「所有键都带 TTL」。
func (c *Client) TTL(ctx context.Context, k Key) (time.Duration, bool, error) {
	if !k.Valid() {
		return 0, false, ErrBadKey
	}
	var d time.Duration
	err := c.run(ctx, func(r *redis.Client) (e error) { d, e = r.PTTL(ctx, k.s).Result(); return })
	if err != nil {
		return 0, false, err
	}
	if d == -2 { // 键不存在
		return 0, false, nil
	}
	return d, true, nil
}
