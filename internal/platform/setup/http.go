package setup

import (
	_ "embed"
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

//go:embed static/setup.html
var pageHTML []byte

// 路由元数据（与 openapi.yaml 的 /public/setup/* 一一对应；x-public: true、x-permissions: []、x-tenant-scope: public、x-phase: M1）。
var (
	MetaStatus     = meta("getSetupStatus", "GET", "/public/setup/status", false)
	MetaChecks     = meta("runSetupChecks", "POST", "/public/setup/checks", true)
	MetaDatabase   = meta("setupDatabase", "PUT", "/public/setup/database", true)
	MetaRedis      = meta("setupRedis", "PUT", "/public/setup/redis", true)
	MetaInitialize = meta("setupInitializeDb", "POST", "/public/setup/initialize", true)
	MetaAdmin      = meta("setupCreateAdmin", "POST", "/public/setup/admin", true)
	MetaSystem     = meta("setupSystemConfig", "PUT", "/public/setup/system", true)
	MetaKeys       = meta("setupInitKeys", "POST", "/public/setup/keys", true)
	MetaComplete   = meta("setupComplete", "POST", "/public/setup/complete", true)
)

func meta(op, method, path string, auditOn bool) httpx.RouteMeta {
	return httpx.RouteMeta{OperationID: op, Method: method, Path: path, Auth: httpx.AuthPublic, Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopePublic, Audit: auditOn, Phase: "M1"}
}

// Metas 返回全部向导路由元数据。
func Metas() []httpx.RouteMeta {
	return []httpx.RouteMeta{MetaStatus, MetaChecks, MetaDatabase, MetaRedis, MetaInitialize, MetaAdmin, MetaSystem, MetaKeys, MetaComplete}
}

// Register 注册 /public/setup/* 全部 operation（经 httpx.Register，元数据与 openapi 一致）。正常模式（已安装）与 setup mode 都注册：
// 已安装时每个 operation 都返回 410 setup.already_installed（MIN-04），而不是 404 或 501。
func (s *Service) Register(r *httpx.Router) error {
	if r == nil {
		return errors.New("setup: Router 为空")
	}
	return errors.Join(
		r.Register(MetaStatus, s.guarded(false, func(c *gin.Context) (any, error) { return s.Status(c.Request.Context()) })),
		r.Register(MetaChecks, s.guarded(false, func(c *gin.Context) (any, error) { return s.Checks(c.Request.Context()) })),
		r.Register(MetaDatabase, s.guarded(true, func(c *gin.Context) (any, error) {
			var in DatabaseInput
			if err := httpx.BindJSON(c, &in); err != nil {
				return nil, err
			}
			return s.ConfigureDatabase(c.Request.Context(), in)
		})),
		r.Register(MetaRedis, s.guarded(true, func(c *gin.Context) (any, error) {
			var in RedisInput
			if err := httpx.BindJSON(c, &in); err != nil {
				return nil, err
			}
			return s.ConfigureRedis(c.Request.Context(), in)
		})),
		r.Register(MetaInitialize, s.guarded(false, func(c *gin.Context) (any, error) { return s.InitializeDB(c.Request.Context()) })),
		r.Register(MetaAdmin, s.guarded(true, func(c *gin.Context) (any, error) {
			var in AdminInput
			if err := httpx.BindJSON(c, &in); err != nil {
				return nil, err
			}
			return s.CreateAdmin(c.Request.Context(), in)
		})),
		r.Register(MetaSystem, s.guarded(false, func(c *gin.Context) (any, error) {
			var in SystemInput
			if err := httpx.BindJSON(c, &in); err != nil {
				return nil, err
			}
			return s.ConfigureSystem(c.Request.Context(), in)
		})),
		r.Register(MetaKeys, s.guarded(false, func(c *gin.Context) (any, error) { return s.InitKeys(c.Request.Context()) })),
		r.Register(MetaComplete, s.guarded(true, func(c *gin.Context) (any, error) {
			res, err := s.Complete(c.Request.Context())
			if err != nil {
				return nil, err
			}
			// 响应写出后清零（one_time_secrets 仅此一次）：这里直接写出并 flush，再清零与通知 server。
			httpx.OK(c, res)
			c.Writer.Flush()
			if res.Secrets != nil {
				res.Secrets.MigratePassword = ""
			}
			s.AfterComplete()
			return nil, errHandled
		})),
	)
}

var errHandled = errors.New("handled")

// RegisterPage 注册内嵌的 /setup 静态页（仅 setup mode）。
func (s *Service) RegisterPage(r *httpx.Router) error {
	return r.HandlePage("/setup", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Data(http.StatusOK, "text/html; charset=utf-8", pageHTML)
	})
}

// guarded 是所有向导接口的前置链（顺序即规格）：已安装 → 410；来源限流 → 429；X-Setup-Token（缺少/错误 → 401 并节流；已被节流 → 429）；
// 携带口令的接口在「非回环监听且非 TLS」时 → 400 setup.insecure_transport；最后才解析请求体。
func (s *Service) guarded(sensitive bool, fn func(c *gin.Context) (any, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		if s.Installed() {
			httpx.Fail(c, apierr.New(apierr.SetupAlreadyInstalled, "setup already completed"))
			return
		}
		ctx := c.Request.Context()
		ip := httpx.ClientIPFrom(ctx)
		if ok, retry := s.guard.allowRequest(ctx, ip); !ok {
			httpx.Fail(c, apierr.New(apierr.RateLimitExceeded, "too many requests").WithRetryAfter(retry))
			return
		}
		if blocked, retry := s.guard.blocked(ctx, ip); blocked {
			httpx.Fail(c, apierr.New(apierr.RateLimitExceeded, "too many failed attempts").WithRetryAfter(retry))
			return
		}
		tok := c.GetHeader("X-Setup-Token")
		if tok == "" || !s.guard.verify(tok) {
			s.guard.noteFailure(ctx, ip)
			httpx.Fail(c, apierr.New(apierr.SetupTokenInvalid, "setup token missing or invalid"))
			return
		}
		if sensitive && !s.transportOK(c) {
			httpx.Fail(c, apierr.New(apierr.SetupInsecureTransport, "this endpoint carries credentials and requires TLS when the setup listener is not a loopback address",
				apierr.Detail{Reason: "use an SSH tunnel to the loopback listener, or terminate TLS (directly or at a trusted reverse proxy that sets X-Forwarded-Proto: https)"}))
			return
		}
		v, err := fn(c)
		switch {
		case errors.Is(err, errHandled):
			return
		case err != nil:
			httpx.Fail(c, err)
		default:
			httpx.OK(c, v)
		}
	}
}

// transportOK 实现 ADR-026 §10：监听回环地址不受限（沿用 SSH 隧道默认）；否则要求请求是 TLS（直接 TLS，或来自可信代理且 X-Forwarded-Proto: https）。
func (s *Service) transportOK(c *gin.Context) bool {
	if config.IsLoopbackListen(s.o.ListenAddr) {
		return true
	}
	if c.Request.TLS != nil {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")), "https") {
		if peer, err := netip.ParseAddr(c.RemoteIP()); err == nil {
			for _, p := range s.trus {
				if p.Contains(peer) {
					return true
				}
			}
		}
	}
	return false
}

// allowAuditor 是 setup mode 的审计钩子：向导各步骤的审计由 Service 自己在对应事务里写（迁移完成前 audit_logs 尚不存在，见 Service.audit），
// 钩子本身不拒绝、不兜底（没有「已认证主体」可言）。
type allowAuditor struct{}

func (allowAuditor) Begin(*http.Request, httpx.RouteMeta) (func(httpx.AuditOutcome), error) {
	return nil, nil
}

// BuildHandler 构造 setup mode 的 HTTP 处理器：只有 /healthz（存活）、/setup 静态页与 /api/v1/public/setup/*；其余一律 503（setup.mode_active）。
func (s *Service) BuildHandler(log *zap.Logger, clk clock.Clock, ids *uid.Generator) (http.Handler, error) {
	r, err := httpx.NewRouter(httpx.Options{Logger: log, Clock: clk, IDs: ids, Hooks: httpx.Hooks{Auditor: allowAuditor{}}, TrustedProxies: s.cfg.Server.TrustedProxies})
	if err != nil {
		return nil, err
	}
	if err := s.Register(r); err != nil {
		return nil, err
	}
	if err := s.RegisterPage(r); err != nil {
		return nil, err
	}
	r.SetFallback(func(c *gin.Context) {
		c.Header("Retry-After", "30")
		httpx.Fail(c, apierr.New(apierr.SetupModeActive, "the server is in setup mode: complete the installation wizard first"))
	})
	return r.Handler()
}
