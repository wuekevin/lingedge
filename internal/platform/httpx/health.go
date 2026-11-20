package httpx

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
)

// 健康检查（Story 1.12 AC6）：
//
//	GET /healthz        存活：进程能响应即 200，不探测任何依赖（liveness）。
//	GET /healthz/ready  就绪：DB 连通、迁移版本一致、Redis 状态。DB 或迁移不满足 → 503 "unavailable"；
//	                    Redis 不可用 → 仍 200，status="degraded"（ADR-025 §8：Redis 降级不是失败）。
//
// 响应只含固定的状态词与迁移版本号，不含任何错误原文（依赖的真实错误只写日志，且已脱敏）。

// 状态词。
const (
	StatusOK          = "ok"
	StatusDegraded    = "degraded"
	StatusUnavailable = "unavailable"
)

// DBPinger 是数据库连通性探测（db.Pool 满足）。
type DBPinger interface {
	Ping(ctx context.Context) error
}

// MigrationState 是迁移版本探测结果。
type MigrationState struct {
	Applied int64  // 库内已应用的最高版本
	Latest  int64  // 二进制内嵌的最高版本
	Pending int    // 待应用数
	Problem string // 完整性问题（dirty/校验和/未知版本…）；空表示健康
}

// MigrationProbe 探测库内迁移版本与二进制内嵌迁移是否一致。
//
// 真实实现有二：MigrationProbeFromApp（Story 1.13、CC-3，进程装配使用）经 app 连接池只读 schema_migrations（迁移 000014 为 lingedge_app
// 授予了该表唯一的 SELECT，不持有 migrate 凭据，符合三账号最小权限）；MigrationProbeFromRunner 基于 migrate.Runner（migrate 连接池，
// 含 dirty/校验和完整性检查），仅供持有迁移凭据的工具/测试使用，Server 进程不使用它。
type MigrationProbe interface {
	MigrationState(ctx context.Context) (MigrationState, error)
}

// MigrationProbeFromRunner 用迁移框架的 Status 实现 MigrationProbe。
func MigrationProbeFromRunner(r *migrate.Runner) MigrationProbe { return runnerProbe{r} }

type runnerProbe struct{ r *migrate.Runner }

func (p runnerProbe) MigrationState(ctx context.Context) (MigrationState, error) {
	st, err := p.r.Status(ctx)
	if err != nil {
		return MigrationState{}, err
	}
	s := MigrationState{Applied: st.DBVersion, Latest: st.LatestKnown, Pending: st.Pending, Problem: st.Problem}
	if st.Dirty != nil && s.Problem == "" {
		s.Problem = "dirty migration"
	}
	if st.InProgress && s.Problem == "" {
		s.Problem = "migration in progress"
	}
	return s, nil
}

// MigrationProbeFromApp 用 app 连接池读取 schema_migrations（只读），与内嵌迁移集合比较：
// 库内最高版本 == 内嵌最高版本、无待应用迁移、库内没有二进制不认识的版本，才算一致。
//
// 能力边界（诚实声明）：app 账号看不到 schema_migrations_runs，因此探针无法发现 dirty/校验和不一致；这些由 `lingedge-server migrate status`
// 持有迁移凭据时检查。迁移执行中途失败时版本表里没有该版本（goose 在同一事务内写入版本行），表现为 Applied < Latest，仍会报告不就绪。
// 库尚未升级到 000014（app 无授权）时读取失败 → 调用方按失败处理（fail-closed，/healthz/ready 报 unknown 并 503）。
func MigrationProbeFromApp(pool db.Pool, set *migrate.Set) MigrationProbe {
	known := make(map[int64]bool, len(set.Migrations))
	var latest int64
	for _, m := range set.Migrations {
		known[m.Version] = true
		if m.Version > latest {
			latest = m.Version
		}
	}
	return appProbe{pool: pool, known: known, latest: latest}
}

type appProbe struct {
	pool   db.Pool
	known  map[int64]bool
	latest int64
}

func (p appProbe) MigrationState(ctx context.Context) (MigrationState, error) {
	var applied []int64
	if err := p.pool.Executor(ctx).Select(ctx, &applied, db.Q("SELECT version_id FROM schema_migrations WHERE is_applied = 1 AND version_id > 0")); err != nil {
		return MigrationState{}, err
	}
	st := MigrationState{Latest: p.latest}
	have := make(map[int64]bool, len(applied))
	for _, v := range applied {
		have[v] = true
		if v > st.Applied {
			st.Applied = v
		}
		if !p.known[v] && st.Problem == "" {
			st.Problem = "database has a migration unknown to this binary"
		}
	}
	for v := range p.known {
		if !have[v] {
			st.Pending++
		}
	}
	return st, nil
}

// RedisStatus 是 Redis 状态来源（*redisx.Client 满足）。
type RedisStatus interface {
	Status() redisx.Status
}

// HealthOptions 是健康检查的依赖。DB 与 Migrations 缺失（nil）时对应检查视为失败（就绪必须有依据，不能默认通过）；
// Redis 缺失时报告 "unconfigured" 并按 degraded 处理。
type HealthOptions struct {
	DB         DBPinger
	Migrations MigrationProbe
	Redis      RedisStatus
	// Timeout 是单个依赖的探测超时（默认 2s）。
	Timeout time.Duration
	// CacheTTL > 0 时缓存最近一次就绪结果，避免探针风暴放大到数据库（默认 0=不缓存）。
	CacheTTL time.Duration
}

// CheckResult 是单项检查的结果。
type CheckResult struct {
	Status  string `json:"status"`
	Applied *int64 `json:"applied,omitempty"`
	Latest  *int64 `json:"latest,omitempty"`
	Pending *int   `json:"pending,omitempty"`
	Since   string `json:"since,omitempty"` // Redis 进入当前状态的时间（RFC 3339）
}

// ReadyReport 是 /healthz/ready 的响应体。
type ReadyReport struct {
	Status    string                 `json:"status"`
	Checks    map[string]CheckResult `json:"checks"`
	RequestID string                 `json:"request_id"`
}

type healthHandler struct {
	o   HealthOptions
	log *zap.Logger
	clk clock.Clock

	mu      sync.Mutex
	cached  ReadyReport
	cachedA time.Time
}

func newHealthHandler(o HealthOptions, log *zap.Logger, clk clock.Clock) *healthHandler {
	if o.Timeout <= 0 {
		o.Timeout = 2 * time.Second
	}
	return &healthHandler{o: o, log: log.Named("health"), clk: clk}
}

func (h *healthHandler) live(c *gin.Context) {
	id, _ := requestid.FromContext(c.Request.Context())
	JSON(c, http.StatusOK, map[string]string{"status": StatusOK, "request_id": id})
}

func (h *healthHandler) ready(c *gin.Context) {
	rep := h.report(c.Request.Context())
	id, _ := requestid.FromContext(c.Request.Context())
	rep.RequestID = id
	status := http.StatusOK
	if rep.Status == StatusUnavailable {
		status = http.StatusServiceUnavailable
	}
	JSON(c, status, rep)
}

func (h *healthHandler) report(ctx context.Context) ReadyReport {
	if ttl := h.o.CacheTTL; ttl > 0 {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.cachedA.IsZero() && h.clk.Now().Sub(h.cachedA) < ttl {
			return h.cached
		}
		r := h.compute(ctx)
		h.cached, h.cachedA = r, h.clk.Now()
		return r
	}
	return h.compute(ctx)
}

func (h *healthHandler) compute(ctx context.Context) ReadyReport {
	rep := ReadyReport{Status: StatusOK, Checks: map[string]CheckResult{}}
	fail := func() { rep.Status = StatusUnavailable }

	// 数据库。
	dbOK := false
	if h.o.DB == nil {
		rep.Checks["database"] = CheckResult{Status: "unconfigured"}
		fail()
	} else {
		cctx, cancel := context.WithTimeout(ctx, h.o.Timeout)
		err := h.o.DB.Ping(cctx)
		cancel()
		if err != nil {
			h.log.Warn("就绪检查：数据库不可用", zap.Error(err))
			rep.Checks["database"] = CheckResult{Status: "down"}
			fail()
		} else {
			rep.Checks["database"] = CheckResult{Status: "up"}
			dbOK = true
		}
	}

	// 迁移版本（数据库不可用时无从判断）。
	switch {
	case !dbOK:
		rep.Checks["migrations"] = CheckResult{Status: "unknown"}
	case h.o.Migrations == nil:
		rep.Checks["migrations"] = CheckResult{Status: "unconfigured"}
		fail()
	default:
		cctx, cancel := context.WithTimeout(ctx, h.o.Timeout)
		st, err := h.o.Migrations.MigrationState(cctx)
		cancel()
		switch {
		case err != nil:
			h.log.Warn("就绪检查：读取迁移状态失败", zap.Error(err))
			rep.Checks["migrations"] = CheckResult{Status: "unknown"}
			fail()
		default:
			res := CheckResult{Applied: &st.Applied, Latest: &st.Latest, Pending: &st.Pending, Status: "ok"}
			switch {
			case st.Problem != "":
				h.log.Warn("就绪检查：迁移状态异常", zap.String("problem", st.Problem))
				res.Status = "invalid"
				fail()
			case st.Pending > 0 || st.Applied != st.Latest:
				res.Status = "mismatch"
				fail()
			}
			rep.Checks["migrations"] = res
		}
	}

	// Redis：不可用只降级。
	if h.o.Redis == nil {
		rep.Checks["redis"] = CheckResult{Status: "unconfigured"}
		if rep.Status == StatusOK {
			rep.Status = StatusDegraded
		}
	} else {
		rs := h.o.Redis.Status()
		res := CheckResult{Status: rs.State.String()}
		if !rs.Since.IsZero() {
			res.Since = rs.Since.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		rep.Checks["redis"] = res
		if rs.State != redisx.StateUp && rep.Status == StatusOK {
			rep.Status = StatusDegraded
		}
	}
	return rep
}
