package sysconfigit

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// Story 1.22b（1.22 遗留；ADR-025 §1 失效广播）：设置变更经 Pub/Sub 广播，其他实例立即丢弃快照——运行模式的跨实例感知从「最长 CacheTTL（5 秒）」缩短到毫秒级；
// 广播有损，Redis 不可用时仍由快照 TTL 兜底。这里用两个独立的 sysconfig 实例（各自的 Redis 客户端与快照缓存、CacheTTL 设为 1 小时，使 TTL 无法掩盖结果）。
func newPeerInstance(t *testing.T, w *apistack.World, redisAddr string, ttl time.Duration) *sysconfig.Service {
	t.Helper()
	rc, err := redisx.New(context.Background(), redisx.Config{Addr: redisAddr, Env: "test", ProbeInterval: 100 * time.Millisecond,
		DialTimeout: 300 * time.Millisecond, ReadTimeout: 300 * time.Millisecond, WriteTimeout: 300 * time.Millisecond}, redisx.Options{Logger: zap.NewNop()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	m, err := sysconfig.New(sysconfig.Options{Pool: w.App, Clock: clock.System(), IDs: apistack.IDs, Log: zap.NewNop(), Box: apistack.NewBox(t), Audit: w.Audit, Redis: rc, CacheTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	return m.Service()
}

func waitMode(t *testing.T, s *sysconfig.Service, want string, d time.Duration) (took time.Duration, ok bool) {
	t.Helper()
	start := time.Now()
	for time.Since(start) < d {
		if m, err := s.RunMode(context.Background()); err == nil && m == want {
			return time.Since(start), true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return time.Since(start), false
}

func TestRunModeChangeBroadcastReachesOtherInstances(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	adm.Reauth()
	peer := newPeerInstance(t, w, w.Redis.Addr(), time.Hour)
	if m, err := peer.RunMode(context.Background()); err != nil || m != "self_hosted" {
		t.Fatalf("对端初始应为 self_hosted: %q %v", m, err)
	}
	// 订阅建立是异步的：等对端真正在订阅后再切换（订阅生效前的广播会丢——那正是兜底要覆盖的情形，下面单独测）。
	time.Sleep(500 * time.Millisecond)

	if r := adm.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": "commercial"}); r.Status != 200 {
		t.Fatalf("切换: %d %s", r.Status, r.Body)
	}
	took, ok := waitMode(t, peer, "commercial", 3*time.Second)
	if !ok {
		t.Fatalf("对端应在广播后几乎立即看到新模式（快照 TTL 为 1 小时，不可能靠过期）：等待 %s 仍未更新", took)
	}
	t.Logf("对端感知用时 %s（缓存 TTL 1h，仅靠广播）", took)
	// 通用 PATCH 同样广播。
	if r := patch(adm, map[string]any{"brand.name": "广播品牌"}); r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := peer.Brand(context.Background(), false); err == nil && b.Name == "广播品牌" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("PATCH 变更也应广播到对端")
}

// 广播丢失/Redis 不可用时的兜底：对端的 Redis 不可达（收不到任何广播），仍在快照 TTL 内最终感知。
func TestRunModeChangeFallsBackToSnapshotTTLWithoutBroadcast(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	adm.Reauth()
	const ttl = 4 * time.Second
	peer := newPeerInstance(t, w, "127.0.0.1:1", ttl) // 不可达的 Redis：既收不到广播也发不出
	time.Sleep(300 * time.Millisecond)                // 订阅启动时的对账（Poll）会丢弃一次快照：等它先发生，再装载快照
	if m, err := peer.RunMode(context.Background()); err != nil || m != "self_hosted" {
		t.Fatalf("%q %v", m, err)
	}
	loaded := time.Now()
	if r := adm.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": "commercial"}); r.Status != 200 {
		t.Fatalf("切换: %d %s", r.Status, r.Body)
	}
	if time.Since(loaded) < ttl-time.Second { // 仅当确定仍在 TTL 内才断言「旧值」（避免慢机器上误报）
		if m, _ := peer.RunMode(context.Background()); m != "self_hosted" {
			t.Fatalf("无广播时 TTL 内仍是旧值（证明确实没有广播）: %q", m)
		}
	}
	if took, ok := waitMode(t, peer, "commercial", ttl+4*time.Second); !ok {
		t.Fatalf("无广播时应在快照 TTL 内最终一致: 等待 %s", took)
	}
}
