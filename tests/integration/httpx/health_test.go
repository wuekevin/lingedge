package httpxintegration

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func newReadyRouter(t *testing.T, e *env, rc httpx.RedisStatus) func() (int, httpx.ReadyReport) {
	t.Helper()
	lg, err := logx.New(logx.Options{Level: "debug", Output: &discard{}})
	if err != nil {
		t.Fatal(err)
	}
	o := httpx.Options{Logger: lg, IDs: uid.NewGenerator(clock.System()),
		Health: httpx.HealthOptions{DB: e.app, Migrations: httpx.MigrationProbeFromRunner(e.runner), Timeout: 3 * time.Second}}
	if rc != nil {
		o.Health.Redis = rc
	}
	r, err := httpx.NewRouter(o)
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return func() (int, httpx.ReadyReport) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz/ready", nil))
		var rep httpx.ReadyReport
		if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
			t.Fatalf("就绪响应不是 JSON: %v\n%s", err, rec.Body)
		}
		return rec.Code, rep
	}
}

type discard struct{}

func (*discard) Write(p []byte) (int, error) { return len(p), nil }

func eventually(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("超时未达成: %s", what)
}

// AC6：就绪 = DB + 迁移版本 + Redis；Redis 降级不是失败（200 + degraded），DB/迁移异常才 503。
func TestReadyWithRealMySQLAndRedisDegradation(t *testing.T) {
	e := newEnv(t)
	in := redistest.Start(t)
	rc, err := redisx.New(context.Background(), redisx.Config{Addr: in.Addr(), Env: "test", ProbeInterval: 100 * time.Millisecond,
		DialTimeout: 300 * time.Millisecond, ReadTimeout: 300 * time.Millisecond, WriteTimeout: 300 * time.Millisecond}, redisx.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	ready := newReadyRouter(t, e, rc)

	// 全部健康。
	code, rep := ready()
	if code != 200 || rep.Status != "ok" || rep.Checks["database"].Status != "up" || rep.Checks["redis"].Status != "up" {
		t.Fatalf("全部健康: %d %+v", code, rep)
	}
	m := rep.Checks["migrations"]
	if m.Status != "ok" || m.Applied == nil || m.Latest == nil || *m.Applied != 15 || *m.Latest != 15 || *m.Pending != 0 {
		t.Fatalf("迁移应为 15/15（含 000015）: %+v", m)
	}

	// 杀掉 Redis：就绪仍 200，但整体 degraded。
	in.Kill()
	eventually(t, "Redis 被探测为降级", 10*time.Second, func() bool { return rc.State() == redisx.StateDegraded })
	code, rep = ready()
	if code != 200 || rep.Status != "degraded" || rep.Checks["redis"].Status != "degraded" || rep.Checks["database"].Status != "up" || rep.Checks["migrations"].Status != "ok" {
		t.Fatalf("Redis 降级应 200 + degraded: %d %+v", code, rep)
	}
	if rep.Checks["redis"].Since == "" {
		t.Error("应报告进入降级的时间")
	}
	// 重启：自愈。
	in.Restart(t)
	eventually(t, "Redis 恢复", 15*time.Second, func() bool { return rc.State() == redisx.StateUp })
	if code, rep = ready(); code != 200 || rep.Status != "ok" {
		t.Fatalf("Redis 恢复后应 ok: %d %+v", code, rep)
	}
}

// 迁移版本不一致 / dirty → 503；恢复后 200。数据库不可用 → 503 且迁移状态未知。
func TestReadyMigrationsAndDatabaseFailures(t *testing.T) {
	e := newEnv(t)
	ready := newReadyRouter(t, e, nil)
	code, rep := ready()
	if code != 200 || rep.Status != "degraded" || rep.Checks["redis"].Status != "unconfigured" {
		t.Fatalf("无 Redis 时应 200 + degraded（未装配视为降级，不是失败）: %d %+v", code, rep)
	}

	// 有一个迁移尚未应用（旁路删掉最新版本的记录）。
	e.root(t, "DELETE FROM §.schema_migrations WHERE version_id = 15")
	e.root(t, "DELETE FROM §.schema_migrations_runs WHERE version = 15")
	code, rep = ready()
	if code != 503 || rep.Status != "unavailable" || rep.Checks["migrations"].Status != "mismatch" || *rep.Checks["migrations"].Pending != 1 || rep.Checks["database"].Status != "up" {
		t.Fatalf("迁移未应用应 503 + mismatch: %d %+v", code, rep)
	}
	// 恢复该记录 → dirty（running 状态的运行记录且无迁移锁在持有）。
	e.root(t, "INSERT INTO §.schema_migrations (version_id, is_applied) VALUES (15, 1)")
	e.root(t, "INSERT INTO §.schema_migrations_runs (version, name, class, checksum, direction, status, started_at) VALUES (15, 'sessions_uid', 'expand', REPEAT('0', 64), 'up', 'failed', NOW(3))")
	code, rep = ready()
	if code != 503 || rep.Checks["migrations"].Status != "invalid" {
		t.Fatalf("dirty/校验和异常应 503 + invalid: %d %+v", code, rep)
	}
	e.root(t, "DELETE FROM §.schema_migrations_runs WHERE version = 15")

	// 数据库不可用（关闭 app 连接池）→ 503，迁移状态未知；不泄漏错误原文。
	_ = e.app.Close()
	code, rep = ready()
	if code != 503 || rep.Checks["database"].Status != "down" || rep.Checks["migrations"].Status != "unknown" {
		t.Fatalf("数据库不可用应 503: %d %+v", code, rep)
	}
}

// 存活探针不依赖任何依赖：数据库关闭后 /healthz 仍 200。
func TestLivenessIndependentOfDependencies(t *testing.T) {
	e := newEnv(t)
	lg, _ := logx.New(logx.Options{Output: &discard{}})
	r, err := httpx.NewRouter(httpx.Options{Logger: lg, IDs: uid.NewGenerator(clock.System()), Health: httpx.HealthOptions{DB: e.app}})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := r.Handler()
	_ = e.app.Close()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("/healthz: %d", rec.Code)
	}
}
