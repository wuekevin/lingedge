package redisx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

func ks(t *testing.T) Keyspace {
	t.Helper()
	k, err := NewKeyspace("prod")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeyNaming(t *testing.T) {
	k := ks(t)
	got, err := k.Key("auth", "rl", "login", "203.0.113.7")
	if err != nil || got.String() != "le:prod:auth:rl:login:203.0.113.7" {
		t.Fatalf("got %q %v", got, err)
	}
	u := uid.NewGenerator(clock.System()).MustNew()
	sc, err := tenantscope.FromTask(7, 0)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := k.TenantKey(sc, u, "auth", "rl", "tok1")
	if err != nil || tk.String() != "le:prod:auth:rl:t"+u+":tok1" {
		t.Fatalf("got %q %v", tk, err)
	}
	if tk2, err := k.TenantKey(sc, u, "perm", "set"); err != nil || !strings.HasSuffix(tk2.String(), ":t"+u) {
		t.Fatalf("仅租户段的键应合法: %q %v", tk2, err)
	}
}

func TestKeyNegatives(t *testing.T) {
	k := ks(t)
	u := uid.NewGenerator(clock.System()).MustNew()
	tenant, _ := tenantscope.FromTask(7, 0)
	platform := tenantscope.PlatformForJob(0)

	// 花括号旧写法与其他不合规键（AC7 负向用例）。
	for _, raw := range []string{
		"le:{prod}:auth:rl:x", "le:{env}:auth:rl:x", "t:" + u + ":x", "le:prod:auth:rl:t{" + u + "}",
		"le:prod:auth:rl:", "le:prod:auth:rl", "le:prod:Auth:rl:x", "le:prod:auth:rl:a b", "le::auth:rl:x",
		"le:prod:auth:rl:x{", "LE:prod:auth:rl:x", "le:prod:auth:rl:x\n", "le:prod:auth:rl:x:", "le:prod:auth:rl:" + strings.Repeat("a", 300),
		"le:staging:auth:rl:x", // 环境不符
	} {
		if _, err := k.Parse(raw); !errors.Is(err, ErrBadKey) {
			t.Errorf("Parse(%q) 应被拒绝, got %v", raw, err)
		}
	}
	if _, err := k.Parse("le:prod:auth:rl:x"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		m, p string
		ids  []string
	}{
		{"auth", "rl", []string{"{x}"}}, {"auth", "rl", []string{"a:b"}}, {"auth", "rl", nil}, {"{auth}", "rl", []string{"x"}},
		{"auth", "", []string{"x"}}, {"a-b", "rl", []string{"x"}}, {"auth", "rl", []string{""}}, {"auth", "rl", []string{strings.Repeat("x", 129)}},
	} {
		if _, err := k.Key(c.m, c.p, c.ids...); !errors.Is(err, ErrBadKey) {
			t.Errorf("Key(%q,%q,%q) 应被拒绝, got %v", c.m, c.p, c.ids, err)
		}
	}
	// 伪造租户键：非租户键的标识段不得长成 t<ULID>，租户键只能经 TenantKey 构造。
	if _, err := k.Key("auth", "rl", "t"+u); !errors.Is(err, ErrBadKey) {
		t.Errorf("伪造租户段应被拒绝: %v", err)
	}
	if _, err := k.Key("auth", "rl", "x", "t"+u); !errors.Is(err, ErrBadKey) {
		t.Errorf("伪造租户段（非首段）应被拒绝: %v", err)
	}
	// 租户键：零 Scope、平台 Scope、非法 uid 一律拒绝。
	if _, err := k.TenantKey(tenantscope.Scope{}, u, "auth", "rl", "x"); !errors.Is(err, ErrNoScope) {
		t.Errorf("零 Scope: %v", err)
	}
	if _, err := k.TenantKey(platform, u, "auth", "rl", "x"); !errors.Is(err, ErrNoScope) {
		t.Errorf("平台 Scope: %v", err)
	}
	if _, err := k.TenantKey(tenant, "{"+u+"}", "auth", "rl", "x"); !errors.Is(err, ErrNoScope) {
		t.Errorf("非法 uid: %v", err)
	}
	if _, err := (Keyspace{}).Key("auth", "rl", "x"); !errors.Is(err, ErrBadKey) {
		t.Errorf("零值 Keyspace: %v", err)
	}
	for _, env := range []string{"", "{prod}", "Prod", "1prod", "a_b", strings.Repeat("a", 33)} {
		if _, err := NewKeyspace(env); err == nil {
			t.Errorf("env %q 应被拒绝", env)
		}
	}
	if (Key{}).Valid() {
		t.Error("零值 Key 不应有效")
	}
}

func TestChannelPurposes(t *testing.T) {
	k := ks(t)
	for _, p := range []Purpose{PurposeCertRevoke, PurposePermVersion, PurposeTaskWakeup, PurposeSettingChange} {
		ch, err := k.Channel("node", p)
		if err != nil || ch.String() != "le:prod:node:"+string(p)+":all" {
			t.Errorf("%s: %q %v", p, ch, err)
		}
	}
	if _, err := k.Channel("node", "whatever"); !errors.Is(err, ErrBadPurpose) {
		t.Errorf("清单外用途应被拒绝: %v", err)
	}
}

func TestConfigValidateAndRedaction(t *testing.T) {
	good := Config{Addr: "127.0.0.1:6379", Env: "test", Password: config.NewSecret("S3cretPw-xyz")}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]Config{
		"env 空": {Addr: "127.0.0.1:6379"}, "addr 缺端口": {Addr: "127.0.0.1", Env: "t"}, "db 越界": {Addr: "h:1", Env: "t", DB: 16},
		"tls 子项缺 tls": {Addr: "h:1", Env: "t", TLSServerName: "x"},
	} {
		if err := c.validate(); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
	for _, s := range []string{good.String(), fmt.Sprintf("%v", good), fmt.Sprintf("%+v", good), fmt.Sprintf("%#v", good), fmt.Sprint(good)} {
		if strings.Contains(s, "S3cretPw") {
			t.Fatalf("口令泄漏: %s", s)
		}
	}
	bad := Config{Addr: "127.0.0.1:1", Env: "t", TLS: true, TLSCAFile: "/nonexistent/ca.pem"}
	if _, err := New(context.Background(), bad, Options{}); err == nil {
		t.Error("不存在的 CA 文件应是配置错误")
	}
}

// 本机一个「必定拒绝连接」的回环地址（先占用再释放端口）。
func deadAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	_ = l.Close()
	return a
}

func TestStartsDegradedWhenUnreachableAndFailsFast(t *testing.T) {
	c, err := New(context.Background(), Config{Addr: deadAddr(t), Env: "test", ProbeInterval: time.Hour}, Options{})
	if err != nil {
		t.Fatalf("Redis 不可达不得阻止启动: %v", err)
	}
	defer func() { _ = c.Close() }()
	if c.State() != StateDegraded || c.Status().LastError == "" {
		t.Fatalf("应为降级: %+v", c.Status())
	}
	k, _ := c.Keyspace().Key("auth", "rl", "x")
	start := time.Now()
	if err := c.Set(context.Background(), k, "v", time.Minute); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := c.Get(context.Background(), k); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Error("降级期间应快速失败")
	}
	// 写入前的参数检查先于可用性：无 TTL 在降级时同样报 ErrNoTTL。
	if err := c.Set(context.Background(), k, "v", 0); !errors.Is(err, ErrNoTTL) {
		t.Errorf("got %v", err)
	}
	if err := c.Set(context.Background(), Key{}, "v", time.Minute); !errors.Is(err, ErrBadKey) {
		t.Errorf("got %v", err)
	}
	if err := c.Set(context.Background(), k, "password=hunter2hunter2", time.Minute); !errors.Is(err, ErrSensitiveValue) {
		t.Errorf("got %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil { // 可重复关闭
		t.Fatal(err)
	}
	if err := c.Set(context.Background(), k, "v", time.Minute); !errors.Is(err, ErrClosed) {
		t.Errorf("got %v", err)
	}
}

func TestSubscribeRequiresPollFallback(t *testing.T) {
	c, _ := New(context.Background(), Config{Addr: deadAddr(t), Env: "test", ProbeInterval: time.Hour}, Options{})
	defer func() { _ = c.Close() }()
	ch, _ := c.Keyspace().Channel("node", PurposeTaskWakeup)
	h := func(context.Context, string) {}
	p := func(context.Context) error { return nil }
	for name, o := range map[string]SubscribeOptions{
		"无 Poll": {Handler: h, PollInterval: time.Second}, "无间隔": {Handler: h, Poll: p}, "负间隔": {Handler: h, Poll: p, PollInterval: -1},
	} {
		if _, err := c.Subscribe(context.Background(), ch, o); !errors.Is(err, ErrNoFallback) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestLocalLimiterWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := clock.Func(func() time.Time { return now })
	l := newLocalWindows(clk)
	for i := 1; i <= 3; i++ {
		if d := l.allow("k", 3, time.Minute); !d.Allowed || d.Remaining != 3-i {
			t.Fatalf("第 %d 次: %+v", i, d)
		}
	}
	d := l.allow("k", 3, time.Minute)
	if d.Allowed || d.RetryAfter != time.Minute || d.Source != SourceLocal {
		t.Fatalf("第 4 次应拒绝: %+v", d)
	}
	if !l.allow("other", 3, time.Minute).Allowed {
		t.Error("不同键互不影响")
	}
	now = now.Add(time.Minute) // 窗口结束
	if !l.allow("k", 3, time.Minute).Allowed {
		t.Error("新窗口应放行")
	}
	// 摊销清理：大量过期窗口被回收。
	for i := 0; i < localSweepThreshold+10; i++ {
		l.allow(fmt.Sprintf("x%d", i), 1, time.Second)
	}
	now = now.Add(time.Hour)
	l.allow("trigger", 1, time.Second)
	if len(l.w) > 2 {
		t.Errorf("过期窗口未被清理: %d", len(l.w))
	}
}

type fakeDB struct {
	ok  bool
	err error
	n   int
}

func (f *fakeDB) Allow(context.Context, string, int, time.Duration) (bool, error) {
	f.n++
	return f.ok, f.err
}

func TestLimiterDegradedLayers(t *testing.T) {
	c, _ := New(context.Background(), Config{Addr: deadAddr(t), Env: "test", ProbeInterval: time.Hour}, Options{})
	defer func() { _ = c.Close() }()
	k, _ := c.Keyspace().Key("auth", "rl", "login", "u1")
	ctx := context.Background()

	l := NewLimiter(c, nil)
	for i := 0; i < 2; i++ {
		if d, err := l.Allow(ctx, k, 2, time.Minute); err != nil || !d.Allowed || !d.Degraded || d.Source != SourceLocal {
			t.Fatalf("无 DB 兜底: %+v %v", d, err)
		}
	}
	if d, _ := l.Allow(ctx, k, 2, time.Minute); d.Allowed {
		t.Fatalf("进程内应拒绝: %+v", d)
	}

	db := &fakeDB{ok: false}
	l = NewLimiter(c, db)
	if d, _ := l.Allow(ctx, k, 100, time.Minute); d.Allowed || d.Source != SourceDB || !d.Degraded {
		t.Fatalf("DB 兜底拒绝应生效: %+v", d)
	}
	db.err = errors.New("db down")
	if d, err := l.Allow(ctx, k, 100, time.Minute); err != nil || !d.Allowed || d.Source != SourceLocal {
		t.Fatalf("DB 兜底故障时仅采用进程内判定: %+v %v", d, err)
	}
	if _, err := l.Allow(ctx, k, 0, time.Minute); err == nil {
		t.Error("limit<=0 应报错")
	}
	if _, err := l.Allow(ctx, k, 1, 0); err == nil {
		t.Error("window<=0 应报错")
	}
	if _, err := l.Allow(ctx, Key{}, 1, time.Second); !errors.Is(err, ErrBadKey) {
		t.Error("零值键应被拒绝")
	}
	if c.Status().Fallbacks == 0 {
		t.Error("应累计降级次数")
	}
}

func TestCacheDegradedReadsSourceOfTruth(t *testing.T) {
	c, _ := New(context.Background(), Config{Addr: deadAddr(t), Env: "test", ProbeInterval: time.Hour}, Options{})
	defer func() { _ = c.Close() }()
	k, _ := c.Keyspace().Key("auth", "session", "s1")
	ca := NewCache(c)
	calls := 0
	load := func(context.Context) ([]byte, error) { calls++; return []byte("truth"), nil }
	for i := 0; i < 3; i++ {
		v, err := ca.GetOrLoad(context.Background(), k, time.Minute, load)
		if err != nil || string(v) != "truth" {
			t.Fatalf("%q %v", v, err)
		}
	}
	if calls != 3 {
		t.Errorf("降级时每次直读真相源: %d", calls)
	}
	boom := errors.New("db error")
	if _, err := ca.GetOrLoad(context.Background(), k, time.Minute, func(context.Context) ([]byte, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("真相源错误必须原样返回: %v", err)
	}
	if _, err := ca.GetOrLoad(context.Background(), k, 0, load); !errors.Is(err, ErrNoTTL) {
		t.Errorf("无 TTL 应报错: %v", err)
	}
	if err := ca.Invalidate(context.Background(), k); !errors.Is(err, ErrUnavailable) {
		t.Errorf("降级时 Invalidate 返回 ErrUnavailable: %v", err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ca.GetOrLoad(cctx, k, time.Minute, load); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 取消应原样返回: %v", err)
	}
}
