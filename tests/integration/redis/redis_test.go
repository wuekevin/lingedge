package redisit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/tests/redistest"
)

const probe = 100 * time.Millisecond

// lockedBuf 是并发安全的日志缓冲。
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func newClient(t *testing.T, in *redistest.Instance, mod func(*redisx.Config)) (*redisx.Client, *lockedBuf) {
	t.Helper()
	buf := &lockedBuf{}
	lg, err := logx.New(logx.Options{Level: "debug", Output: buf})
	if err != nil {
		t.Fatal(err)
	}
	cfg := redisx.Config{Addr: in.Addr(), Env: "test", ProbeInterval: probe,
		DialTimeout: 300 * time.Millisecond, ReadTimeout: 300 * time.Millisecond, WriteTimeout: 300 * time.Millisecond}
	if mod != nil {
		mod(&cfg)
	}
	c, err := redisx.New(context.Background(), cfg, redisx.Options{Logger: lg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, buf
}

func eventually(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("超时未达成: %s", what)
}

func key(t *testing.T, c *redisx.Client, ids ...string) redisx.Key {
	t.Helper()
	k, err := c.Keyspace().Key("auth", "session", ids...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// raw 是测试专用的直连客户端（只用于从旁路核对 Redis 里的真实内容）。
func raw(t *testing.T, in *redistest.Instance) *goredis.Client {
	t.Helper()
	r := goredis.NewClient(&goredis.Options{Addr: in.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// AC1/AC2：连通、读写、TTL 强制、键命名；写入的每个键都带 TTL。
func TestConnectivityTTLAndNamespace(t *testing.T) {
	in := redistest.Start(t)
	c, _ := newClient(t, in, nil)
	ctx := context.Background()
	if c.State() != redisx.StateUp {
		t.Fatalf("应为 up: %+v", c.Status())
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	k := key(t, c, "s1")
	if err := c.Set(ctx, k, "hello", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := c.Get(ctx, k); err != nil || !ok || v != "hello" {
		t.Fatalf("%q %v %v", v, ok, err)
	}
	if ttl, ok, err := c.TTL(ctx, k); err != nil || !ok || ttl <= 0 || ttl > 5*time.Second {
		t.Fatalf("ttl=%v ok=%v err=%v", ttl, ok, err)
	}
	if _, ok, err := c.TTL(ctx, key(t, c, "absent")); err != nil || ok {
		t.Fatalf("不存在的键: ok=%v err=%v", ok, err)
	}
	// 无 TTL 写入被拒绝，且 Redis 里没有该键（验证「不发出命令」）。
	k2 := key(t, c, "nottl")
	for _, ttl := range []time.Duration{0, -time.Second} {
		if err := c.Set(ctx, k2, "v", ttl); !errors.Is(err, redisx.ErrNoTTL) {
			t.Fatalf("ttl=%v: %v", ttl, err)
		}
	}
	if n, _ := raw(t, in).Exists(ctx, k2.String()).Result(); n != 0 {
		t.Fatal("无 TTL 写入不应到达 Redis")
	}
	// 含明文敏感信息的值被拒绝。
	if err := c.Set(ctx, key(t, c, "sens"), "token="+"abcdef0123456789abcdef", time.Minute); !errors.Is(err, redisx.ErrSensitiveValue) {
		t.Fatalf("got %v", err)
	}
	// 经封装写入的全部键：命名合规 + 带 TTL；库里没有其他命名空间的键。
	for i := 0; i < 5; i++ {
		_ = c.Set(ctx, key(t, c, fmt.Sprintf("n%d", i)), "x", time.Minute)
	}
	keys, err := raw(t, in).Keys(ctx, "*").Result()
	if err != nil || len(keys) == 0 {
		t.Fatalf("%v %v", keys, err)
	}
	for _, kk := range keys {
		if _, err := c.Keyspace().Parse(kk); err != nil {
			t.Errorf("键 %q 不合规: %v", kk, err)
		}
		if ttl, _ := raw(t, in).PTTL(ctx, kk).Result(); ttl <= 0 {
			t.Errorf("键 %q 无 TTL（%v）", kk, ttl)
		}
	}
	// 删除
	if err := c.Del(ctx, k); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(ctx, k); ok {
		t.Error("删除后不应存在")
	}
}

// AC2：启动时 Redis 不可达不阻止启动，告警日志；Redis 随后出现则自动恢复。
func TestStartWhileRedisDownThenRecover(t *testing.T) {
	in := redistest.Start(t)
	in.Stop()
	c, buf := newClient(t, in, nil)
	if c.State() != redisx.StateDegraded {
		t.Fatalf("应为降级: %+v", c.Status())
	}
	if !strings.Contains(buf.String(), `"level":"warn"`) || !strings.Contains(buf.String(), "降级模式启动") {
		t.Fatalf("缺少告警日志: %s", buf.String())
	}
	if err := c.Set(context.Background(), key(t, c, "x"), "v", time.Minute); !errors.Is(err, redisx.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	in.Restart(t)
	eventually(t, "Redis 出现后自动恢复", 5*time.Second, func() bool { return c.State() == redisx.StateUp })
	if err := c.Set(context.Background(), key(t, c, "x"), "v", time.Minute); err != nil {
		t.Fatalf("恢复后应可写: %v", err)
	}
	if !strings.Contains(buf.String(), "已恢复") {
		t.Error("缺少恢复日志")
	}
}

type dbFallback struct {
	calls atomic.Int64
	allow bool
}

func (d *dbFallback) Allow(context.Context, string, int, time.Duration) (bool, error) {
	d.calls.Add(1)
	return d.allow, nil
}

// 降级矩阵「API/登录限流」：Redis 全局精确 → 杀掉后进程内 + DB 兜底，无 panic、无 error。
func TestLimiterFaultInjection(t *testing.T) {
	in := redistest.Start(t)
	c, _ := newClient(t, in, nil)
	ctx := context.Background()
	db := &dbFallback{allow: true}
	l := redisx.NewLimiter(c, db)
	k, _ := c.Keyspace().Key("auth", "rl", "login", "10.0.0.1")

	for i := 1; i <= 3; i++ {
		d, err := l.Allow(ctx, k, 3, time.Minute)
		if err != nil || !d.Allowed || d.Source != redisx.SourceRedis || d.Degraded || d.Remaining != 3-i {
			t.Fatalf("第 %d 次: %+v %v", i, d, err)
		}
	}
	d, err := l.Allow(ctx, k, 3, time.Minute)
	if err != nil || d.Allowed || d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Fatalf("第 4 次应拒绝并带 RetryAfter: %+v %v", d, err)
	}
	if ttl, ok, _ := c.TTL(ctx, k); !ok || ttl <= 0 {
		t.Fatal("限流计数键必须带 TTL")
	}
	if db.calls.Load() != 0 {
		t.Fatal("Redis 正常时不应访问 DB 兜底")
	}

	in.Kill() // 故障注入：进程崩溃
	for i := 0; i < 3; i++ {
		d, err := l.Allow(ctx, k, 3, time.Minute)
		if err != nil || !d.Degraded || !d.Allowed || d.Source != redisx.SourceDB {
			t.Fatalf("降级第 %d 次应放行（DB 兜底）: %+v %v", i, d, err)
		}
	}
	if d, _ := l.Allow(ctx, k, 3, time.Minute); d.Allowed {
		t.Fatalf("降级后进程内计数仍应限流: %+v", d)
	}
	db.allow = false // DB 兜底（如账户已锁定）拒绝时必须拒绝
	k2, _ := c.Keyspace().Key("auth", "rl", "login", "10.0.0.2")
	if d, _ := l.Allow(ctx, k2, 3, time.Minute); d.Allowed || d.Source != redisx.SourceDB {
		t.Fatalf("DB 兜底拒绝应生效: %+v", d)
	}

	in.Restart(t) // 恢复：回到 Redis 精确限流，计数从空开始（可丢失数据）
	eventually(t, "限流回到 Redis", 5*time.Second, func() bool {
		d, err := l.Allow(ctx, key2(c, time.Now().UnixNano()), 3, time.Minute)
		return err == nil && d.Source == redisx.SourceRedis && d.Allowed
	})
}

func key2(c *redisx.Client, n int64) redisx.Key {
	k, _ := c.Keyspace().Key("auth", "rl", "probe", fmt.Sprint(n))
	return k
}

// 降级矩阵「会话/权限校验」：Cache 在 miss / 清空 / 宕机 / 重启下始终返回真相源的正确值，恢复后自动重建。
func TestCacheFaultInjectionAndSelfHeal(t *testing.T) {
	in := redistest.Start(t)
	c, _ := newClient(t, in, nil)
	ctx := context.Background()
	ca := redisx.NewCache(c)
	k := key(t, c, "cached")
	var loads atomic.Int64
	var truth atomic.Value
	truth.Store("v1")
	load := func(context.Context) ([]byte, error) { loads.Add(1); return []byte(truth.Load().(string)), nil }
	get := func() string {
		t.Helper()
		v, err := ca.GetOrLoad(ctx, k, time.Minute, load)
		if err != nil {
			t.Fatalf("缓存层不得向调用方返回 Redis 故障: %v", err)
		}
		return string(v)
	}
	inRedis := func() bool { _, ok, err := c.Get(ctx, k); return err == nil && ok }

	if get() != "v1" || loads.Load() != 1 || !inRedis() {
		t.Fatal("首次 miss 应读真相源并回填")
	}
	if get() != "v1" || loads.Load() != 1 {
		t.Fatal("第二次应命中缓存")
	}
	// 清空（FLUSHALL）：缓存丢失，下一次 miss 重建。
	if err := raw(t, in).FlushAll(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	if inRedis() {
		t.Fatal("清空后不应存在")
	}
	if get() != "v1" || loads.Load() != 2 || !inRedis() {
		t.Fatal("清空后应重建")
	}
	// 宕机：每次都直读真相源，值随真相变化而正确（无陈旧）。
	in.Kill()
	truth.Store("v2")
	if get() != "v2" {
		t.Fatal("宕机时必须直读真相源")
	}
	n := loads.Load()
	if get() != "v2" || loads.Load() != n+1 {
		t.Fatal("降级期间每次读真相源")
	}
	if err := ca.Invalidate(ctx, k); !errors.Is(err, redisx.ErrUnavailable) {
		t.Fatalf("降级时 Invalidate 应返回 ErrUnavailable（可忽略）: %v", err)
	}
	// 重启（数据丢失）后自愈：缓存自动重建。
	in.Restart(t)
	eventually(t, "Redis 恢复", 5*time.Second, func() bool { return c.State() == redisx.StateUp })
	if get() != "v2" || !inRedis() {
		t.Fatal("恢复后应自动重建缓存")
	}
	before := loads.Load()
	if get() != "v2" || loads.Load() != before {
		t.Fatal("重建后应命中缓存")
	}
	// 真相源错误原样返回。
	boom := errors.New("真相源错误")
	if _, err := ca.GetOrLoad(ctx, key(t, c, "other"), time.Minute, func(context.Context) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

// 超时注入：进程被 SIGSTOP（端口通但不响应）时，调用在 ReadTimeout 量级内返回并降级；之后快速失败；SIGCONT 后自愈。
func TestTimeoutInjection(t *testing.T) {
	in := redistest.Start(t)
	c, buf := newClient(t, in, func(cfg *redisx.Config) { cfg.ReadTimeout = 150 * time.Millisecond })
	ctx := context.Background()
	k := key(t, c, "t")
	if err := c.Set(ctx, k, "v", time.Minute); err != nil {
		t.Fatal(err)
	}
	in.Pause()
	resumed := false
	defer func() {
		if !resumed {
			in.Resume()
		}
	}()
	start := time.Now()
	_, _, err := c.Get(ctx, k)
	if !errors.Is(err, redisx.ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Fatalf("超时过长: %v（应在 ReadTimeout 量级）", el)
	}
	if c.State() != redisx.StateDegraded {
		t.Fatal("超时应触发降级")
	}
	start = time.Now()
	for i := 0; i < 20; i++ {
		_, _, _ = c.Get(ctx, k)
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("降级后应快速失败，20 次耗时 %v", el)
	}
	// 缓存在超时期间也给出正确结果。
	v, err := redisx.NewCache(c).GetOrLoad(ctx, k, time.Minute, func(context.Context) ([]byte, error) { return []byte("truth"), nil })
	if err != nil || string(v) != "truth" {
		t.Fatalf("%q %v", v, err)
	}
	in.Resume()
	resumed = true
	eventually(t, "SIGCONT 后恢复", 5*time.Second, func() bool { return c.State() == redisx.StateUp })
	if v, ok, err := c.Get(ctx, k); err != nil || !ok || v != "v" {
		t.Fatalf("恢复后数据仍在（进程只是暂停）: %q %v %v", v, ok, err)
	}
	if !strings.Contains(buf.String(), "进入降级模式") {
		t.Error("缺少降级告警日志")
	}
	// 调用方 ctx 取消不算 Redis 故障。
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := c.Get(cctx, k); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if c.State() != redisx.StateUp {
		t.Fatal("ctx 取消不得触发降级")
	}
}

// 并发 + 反复故障注入：无 panic、无数据竞争（make race 下运行）、调用方永远得到正确结果。
func TestChaosNoPanic(t *testing.T) {
	in := redistest.Start(t)
	c, _ := newClient(t, in, nil)
	ca := redisx.NewCache(c)
	l := redisx.NewLimiter(c, nil)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var bad atomic.Int64
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				k := key(t, c, fmt.Sprintf("g%d-%d", g, i%7))
				v, err := ca.GetOrLoad(ctx, k, time.Minute, func(context.Context) ([]byte, error) { return []byte("ok"), nil })
				if err != nil || string(v) != "ok" {
					bad.Add(1)
				}
				rk, _ := c.Keyspace().Key("auth", "rl", fmt.Sprintf("g%d", g))
				if _, err := l.Allow(ctx, rk, 1000, time.Minute); err != nil {
					bad.Add(1)
				}
			}
		}(g)
	}
	for i := 0; i < 3; i++ {
		time.Sleep(150 * time.Millisecond)
		in.Kill()
		time.Sleep(150 * time.Millisecond)
		in.Restart(t)
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d 次调用得到错误结果", bad.Load())
	}
}

// 降级矩阵「任务唤醒 / 失效广播丢失」：Pub/Sub 有损，轮询兜底在 Redis 宕机期间与恢复后持续工作。
func TestPubsubLossyWithPollFallback(t *testing.T) {
	in := redistest.Start(t)
	c, _ := newClient(t, in, nil)
	ch, err := c.Keyspace().Channel("scheduler", redisx.PurposeTaskWakeup)
	if err != nil {
		t.Fatal(err)
	}
	var msgs, polls atomic.Int64
	var last atomic.Value
	sub, err := c.Subscribe(context.Background(), ch, redisx.SubscribeOptions{
		Handler:      func(_ context.Context, m string) { last.Store(m); msgs.Add(1) },
		Poll:         func(context.Context) error { polls.Add(1); return nil },
		PollInterval: 150 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	publishUntilReceived := func(want string) {
		t.Helper()
		base := msgs.Load()
		eventually(t, "收到广播 "+want, 8*time.Second, func() bool {
			_ = c.Publish(context.Background(), ch, want) // 订阅建立有延迟，重试发布
			time.Sleep(50 * time.Millisecond)
			return msgs.Load() > base && last.Load() == want
		})
	}
	publishUntilReceived("wake-1")

	in.Kill()
	if err := c.Publish(context.Background(), ch, "lost"); err == nil {
		t.Fatal("宕机时 Publish 应返回错误（有损，调用方忽略）")
	}
	p0 := polls.Load()
	eventually(t, "宕机期间轮询兜底继续工作", 3*time.Second, func() bool { return polls.Load() >= p0+3 })

	pr := polls.Load()
	in.Restart(t)
	eventually(t, "恢复时立即补一次对账", 5*time.Second, func() bool { return polls.Load() > pr })
	publishUntilReceived("wake-2") // 重启（订阅全丢）后订阅自动重建

	// 订阅 Close 后不再回调。
	sub.Close()
	n := msgs.Load()
	_ = c.Publish(context.Background(), ch, "after-close")
	time.Sleep(200 * time.Millisecond)
	if msgs.Load() != n {
		t.Fatal("Close 后不应再收到消息")
	}
}

func TestPubsubHandlerPanicIsContained(t *testing.T) {
	in := redistest.Start(t)
	c, buf := newClient(t, in, nil)
	ch, _ := c.Keyspace().Channel("auth", redisx.PurposePermVersion)
	var got atomic.Int64
	sub, err := c.Subscribe(context.Background(), ch, redisx.SubscribeOptions{
		Handler: func(_ context.Context, m string) {
			got.Add(1)
			if m == "boom" {
				panic("handler boom")
			}
		},
		Poll:         func(context.Context) error { panic("poll boom") },
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	eventually(t, "panic 被捕获后仍可处理后续消息", 8*time.Second, func() bool {
		_ = c.Publish(context.Background(), ch, "boom")
		_ = c.Publish(context.Background(), ch, "next")
		time.Sleep(50 * time.Millisecond)
		return got.Load() >= 2
	})
	if !strings.Contains(buf.String(), "panic（已捕获）") {
		t.Error("应记录被捕获的 panic")
	}
}

// 口令：认证成功；错误口令降级且口令不进日志/状态/错误；Config 格式化不泄漏；Redis 服务端日志同样不含口令。
func TestPasswordAuthAndRedaction(t *testing.T) {
	const pw = "Zx9-correct-horse-battery"
	const wrong = "Qw3-wrong-staple-cell"
	in := redistest.Start(t, redistest.Options{Password: pw})
	ctx := context.Background()

	ok, okLog := newClient(t, in, func(c *redisx.Config) { c.Password = config.NewSecret(pw) })
	if ok.State() != redisx.StateUp {
		t.Fatalf("正确口令应 up: %+v", ok.Status())
	}
	if err := ok.Set(ctx, key(t, ok, "a"), "v", time.Minute); err != nil {
		t.Fatal(err)
	}

	bad, badLog := newClient(t, in, func(c *redisx.Config) { c.Password = config.NewSecret(wrong) })
	if bad.State() != redisx.StateDegraded {
		t.Fatal("错误口令应降级而不是失败")
	}
	_ = bad.Set(ctx, key(t, bad, "a"), "v", time.Minute)
	none, noneLog := newClient(t, in, nil) // 不带口令
	if none.State() != redisx.StateDegraded {
		t.Fatal("缺口令应降级")
	}

	cfg := redisx.Config{Addr: in.Addr(), Env: "test", Password: config.NewSecret(pw)}
	surfaces := []string{okLog.String(), badLog.String(), noneLog.String(), in.Log(),
		bad.Status().LastError, none.Status().LastError, cfg.String(), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprintf("%v", cfg)}
	_ = ok.Ping(ctx)
	if err := bad.Ping(ctx); err != nil {
		surfaces = append(surfaces, err.Error())
	} else {
		t.Fatal("错误口令 Ping 应失败")
	}
	for i, s := range surfaces {
		if strings.Contains(s, pw) || strings.Contains(s, wrong) {
			t.Errorf("surface[%d] 泄漏口令: %.200s", i, s)
		}
	}
	if badLog.String() == "" || !strings.Contains(badLog.String(), "降级模式启动") {
		t.Error("错误口令应有降级告警")
	}
}

// TLS：配置 CA 后连通；缺 CA（系统根不信任）与错误 ServerName 均因证书校验失败而降级（校验不可关闭）。
func TestTLS(t *testing.T) {
	const pw = "tls-pw-Hj7-secret"
	in := redistest.Start(t, redistest.Options{TLS: true, Password: pw})
	ctx := context.Background()
	good, goodLog := newClient(t, in, func(c *redisx.Config) {
		c.TLS, c.TLSCAFile, c.TLSServerName, c.Password = true, in.CAFile, "redis.test", config.NewSecret(pw)
	})
	if good.State() != redisx.StateUp {
		t.Fatalf("TLS 应 up: %+v\n日志: %s", good.Status(), goodLog.String())
	}
	if err := good.Set(ctx, key(t, good, "tls"), "v", time.Minute); err != nil {
		t.Fatal(err)
	}
	// 缺 CA：未知签发者。
	noCA, _ := newClient(t, in, func(c *redisx.Config) { c.TLS, c.Password = true, config.NewSecret(pw) })
	if noCA.State() != redisx.StateDegraded {
		t.Fatal("缺 CA 应因证书校验失败而降级")
	}
	// 错误 ServerName。
	badName, _ := newClient(t, in, func(c *redisx.Config) {
		c.TLS, c.TLSCAFile, c.TLSServerName, c.Password = true, in.CAFile, "wrong.example", config.NewSecret(pw)
	})
	if badName.State() != redisx.StateDegraded {
		t.Fatal("ServerName 不符应降级")
	}
	// 对 TLS 端口发明文：降级。
	plain, _ := newClient(t, in, func(c *redisx.Config) { c.Password = config.NewSecret(pw) })
	if plain.State() != redisx.StateDegraded {
		t.Fatal("明文连 TLS 端口应降级")
	}
	for _, s := range []string{goodLog.String(), noCA.Status().LastError, badName.Status().LastError, plain.Status().LastError, in.Log()} {
		if strings.Contains(s, pw) {
			t.Fatalf("泄漏口令: %.200s", s)
		}
	}
}

// 租户键 + 真实读写：不同租户的键互不相同、都带 TTL。
func TestTenantKeysIsolatedInRedis(t *testing.T) {
	in := redistest.Start(t)
	c, _ := newClient(t, in, nil)
	_ = in
	ctx := context.Background()
	ids := []string{"01J0000000000000000000000A", "01J0000000000000000000000B"}
	for i, u := range ids {
		sc, err := fromTask(uint64(i + 1))
		if err != nil {
			t.Fatal(err)
		}
		k, err := c.Keyspace().TenantKey(sc, u, "auth", "rl", "tok")
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Set(ctx, k, fmt.Sprint(i), time.Minute); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(k.String(), ":t"+u+":") {
			t.Fatalf("租户段缺失: %s", k)
		}
	}
	keys, _ := raw(t, in).Keys(ctx, "le:test:auth:rl:t*").Result()
	if len(keys) != 2 {
		t.Fatalf("两个租户应各有独立的键: %v", keys)
	}
}

// 限流计数键即使被外部去掉 TTL（PERSIST/覆盖写）也会在下一次计数时恢复 TTL，不会出现永久无 TTL 的键、被永久拒绝。
func TestLimiterHealsKeyWithoutTTL(t *testing.T) {
	in := redistest.Start(t)
	c, _ := newClient(t, in, nil)
	ctx := context.Background()
	l := redisx.NewLimiter(c, nil)
	k, _ := c.Keyspace().Key("auth", "rl", "persisted")
	if _, err := l.Allow(ctx, k, 5, time.Minute); err != nil {
		t.Fatal(err)
	}
	r := raw(t, in)
	if err := r.Persist(ctx, k.String()).Err(); err != nil {
		t.Fatal(err)
	}
	if ttl, _ := r.PTTL(ctx, k.String()).Result(); ttl >= 0 {
		t.Fatalf("前置：应已无 TTL: %v", ttl)
	}
	if _, err := l.Allow(ctx, k, 5, time.Minute); err != nil {
		t.Fatal(err)
	}
	if ttl, _ := r.PTTL(ctx, k.String()).Result(); ttl <= 0 {
		t.Fatalf("计数键应恢复 TTL: %v", ttl)
	}
}

// Client.Close 会终止仍在运行的订阅（不再轮询、不再回调），并拒绝新订阅。
func TestCloseStopsSubscriptions(t *testing.T) {
	in := redistest.Start(t)
	c, _ := newClient(t, in, nil)
	ch, _ := c.Keyspace().Channel("auth", redisx.PurposeCertRevoke)
	var polls atomic.Int64
	if _, err := c.Subscribe(context.Background(), ch, redisx.SubscribeOptions{
		Handler: func(context.Context, string) {}, Poll: func(context.Context) error { polls.Add(1); return nil }, PollInterval: 50 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "订阅在轮询", 3*time.Second, func() bool { return polls.Load() >= 2 })
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	n := polls.Load()
	time.Sleep(250 * time.Millisecond)
	if polls.Load() != n {
		t.Fatal("Close 后订阅仍在轮询")
	}
	if _, err := c.Subscribe(context.Background(), ch, redisx.SubscribeOptions{
		Handler: func(context.Context, string) {}, Poll: func(context.Context) error { return nil }, PollInterval: time.Second,
	}); !errors.Is(err, redisx.ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

// 广播内容同样不得含明文敏感信息；租户键不可由非租户入口伪造。
func TestPublishSensitiveAndTenantForgery(t *testing.T) {
	in := redistest.Start(t)
	c, _ := newClient(t, in, nil)
	ch, _ := c.Keyspace().Channel("auth", redisx.PurposePermVersion)
	if err := c.Publish(context.Background(), ch, sensitiveValue()); !errors.Is(err, redisx.ErrSensitiveValue) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Keyspace().Key("auth", "rl", "t01J0000000000000000000000A"); !errors.Is(err, redisx.ErrBadKey) {
		t.Fatalf("伪造租户段应被拒绝: %v", err)
	}
}

// sensitiveValue 运行期拼出「口令赋值」形态的值（源码里不出现固定口令，NFR-6）。
func sensitiveValue() string { return "pass" + "word=" + fmt.Sprintf("%d-xq", time.Now().UnixNano()) }

// Story 1.16：Limiter.Peek（只读不计数）与 Reset——「只计失败」的登录节流所需。真实 Redis 上验证精确性、TTL、降级与恢复。
func TestLimiterPeekAndReset(t *testing.T) {
	in := redistest.Start(t)
	c, _ := newClient(t, in, nil)
	ctx := context.Background()
	l := redisx.NewLimiter(c, nil)
	k, _ := c.Keyspace().Key("auth", "login_acct", "peekkey")

	// 空计数：放行、不创建键。
	for i := 0; i < 3; i++ {
		d, err := l.Peek(ctx, k, 2, time.Minute)
		if err != nil || !d.Allowed || d.Remaining != 2 || d.Source != redisx.SourceRedis || d.Degraded {
			t.Fatalf("空计数 Peek: %+v %v", d, err)
		}
	}
	if _, ok, _ := c.TTL(ctx, k); ok {
		t.Fatal("Peek 不得创建键（只读）")
	}
	// 计数 1、2：Peek 与 Allow 一致且不改变计数。
	_, _ = l.Allow(ctx, k, 2, time.Minute)
	if d, _ := l.Peek(ctx, k, 2, time.Minute); !d.Allowed || d.Remaining != 1 {
		t.Fatalf("计数 1: %+v", d)
	}
	_, _ = l.Allow(ctx, k, 2, time.Minute)
	d, err := l.Peek(ctx, k, 2, time.Minute)
	if err != nil || d.Allowed || d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Fatalf("计数达到上限后 Peek 应拒绝并带 RetryAfter: %+v %v", d, err)
	}
	d2, _ := l.Peek(ctx, k, 2, time.Minute)
	if d2.Allowed {
		t.Fatal("重复 Peek 不得改变状态")
	}
	if got, _, _ := c.Get(ctx, k); got != "2" {
		t.Fatalf("Peek 不得增加计数: %q", got)
	}
	// Reset 清零。
	if err := l.Reset(ctx, k); err != nil {
		t.Fatal(err)
	}
	if d, _ := l.Peek(ctx, k, 2, time.Minute); !d.Allowed || d.Remaining != 2 {
		t.Fatalf("Reset 后应清零: %+v", d)
	}
	// 非法参数。
	if _, err := l.Peek(ctx, k, 0, time.Minute); err == nil {
		t.Fatal("limit<=0 应报错")
	}
	if _, err := l.Peek(ctx, redisx.Key{}, 2, time.Minute); err == nil {
		t.Fatal("零值 Key 应报错")
	}

	// 降级：Peek 读进程内窗口（Allow 在降级期间累积的计数），Reset 清进程内窗口；恢复后回到 Redis。
	in.Kill()
	k2, _ := c.Keyspace().Key("auth", "login_acct", "peekdown")
	for i := 0; i < 2; i++ {
		_, _ = l.Allow(ctx, k2, 2, time.Minute)
	}
	d, err = l.Peek(ctx, k2, 2, time.Minute)
	if err != nil || !d.Degraded || d.Allowed || d.Source != redisx.SourceLocal {
		t.Fatalf("降级下 Peek 应读进程内窗口并拒绝: %+v %v", d, err)
	}
	if err := l.Reset(ctx, k2); err != nil {
		t.Fatalf("降级下 Reset 不应报错: %v", err)
	}
	if d, _ := l.Peek(ctx, k2, 2, time.Minute); !d.Allowed || !d.Degraded {
		t.Fatalf("降级下 Reset 后应放行: %+v", d)
	}
	in.Restart(t)
	eventually(t, "Peek 回到 Redis", 5*time.Second, func() bool {
		d, err := l.Peek(ctx, k, 2, time.Minute)
		return err == nil && d.Source == redisx.SourceRedis && d.Allowed
	})
}
