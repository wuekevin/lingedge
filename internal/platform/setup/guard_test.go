package setup

import (
	"context"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
)

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }

var _ clock.Clock = (*fakeClock)(nil)

func TestGuardLocalCountingWindows(t *testing.T) {
	fc := &fakeClock{now: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	g := newGuard("/nonexistent", 3, time.Minute, 5, fc, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if b, _ := g.blocked(ctx, "1.2.3.4"); b {
			t.Fatalf("第 %d 次失败前不应被阻断", i)
		}
		g.noteFailure(ctx, "1.2.3.4")
	}
	if b, retry := g.blocked(ctx, "1.2.3.4"); !b || retry <= 0 || retry > time.Minute {
		t.Errorf("3 次失败后应阻断并给出等待: %v %v", b, retry)
	}
	if b, _ := g.blocked(ctx, "5.6.7.8"); b {
		t.Error("按来源隔离")
	}
	fc.now = fc.now.Add(time.Minute + time.Second)
	if b, _ := g.blocked(ctx, "1.2.3.4"); b {
		t.Error("窗口结束后应解除")
	}
	// 请求限流。
	for i := 0; i < 5; i++ {
		if ok, _ := g.allowRequest(ctx, "9.9.9.9"); !ok {
			t.Fatalf("第 %d 次请求应放行", i+1)
		}
	}
	if ok, retry := g.allowRequest(ctx, "9.9.9.9"); ok || retry <= 0 {
		t.Error("第 6 次应被限流")
	}
	// 哈希文件缺失：verify fail-closed。
	if g.verify("anything") {
		t.Error("哈希文件不存在时任何 token 都不应通过")
	}
	for _, ip := range []string{"", "::1", "2001:db8::1", "a b"} {
		if s := sanitizeIP(ip); s == "" {
			t.Errorf("sanitizeIP(%q) 不得为空", ip)
		}
	}
}
