package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/idempotency"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
)

// APIPrefix 是 REST 前缀（openapi servers[0].url）；RouteMeta.Path 相对于它。
const APIPrefix = "/api/v1"

// Options 是 NewRouter 的参数。
type Options struct {
	Logger *zap.Logger
	Clock  clock.Clock
	// IDs 生成 Request ID（*uid.Generator 满足）；必填。
	IDs requestid.Generator
	// Hooks 为 nil 的项使用 Deny* 默认实现（安全拒绝）。
	Hooks Hooks
	// CORSOrigins 是精确的跨源白名单（如 https://console.example.com）；空表示不允许任何跨源。
	CORSOrigins []string
	// TrustedProxies 是可信反代 CIDR/IP（决定 X-Forwarded-For 是否被采信）；默认只信任本机反代 127.0.0.1、::1。
	TrustedProxies []string
	// Metrics 缺省时内部新建；通过 Router.Metrics() 取得以便挂到 /metrics 服务。
	Metrics *Metrics
	// Idempotency 是幂等键存储（MySQL 实现见 idempotency/repository）；未配置时，注册声明了 Idempotent 的路由会失败。
	Idempotency idempotency.Store
	// Health 是 /healthz/ready 的依赖。
	Health HealthOptions
	// MaxBodyBytes 是请求体上限（默认 1 MiB）。
	MaxBodyBytes int64
	// ManifestOnly 仅用于导出路由清单（lingedge-server routes）：不需要幂等存储即可注册 Idempotent 路由，
	// 但该 Router 不可对外服务（Handler() 恒返回 ErrManifestOnly）。
	ManifestOnly bool
}

// ErrManifestOnly 表示该 Router 仅用于导出清单，不能提供 HTTP 服务。
var ErrManifestOnly = errors.New("httpx: ManifestOnly 的 Router 不能对外服务")

// Router 是固定了中间件链的 Gin 引擎封装。
type Router struct {
	engine  *gin.Engine
	hooks   Hooks
	idem    idempotency.Store
	metrics *Metrics
	dryRun  bool

	mu     sync.Mutex
	routes map[string]RouteMeta // operationId → meta
	byPath map[string]string    // "METHOD gin路径" → operationId
	errs   []error
}

var ginModeOnce sync.Once

// NewRouter 创建 Router 并装配全局中间件链：RequestID → recover → 访问日志 → 安全头/CORS，并挂载 /healthz 与 /healthz/ready。
func NewRouter(o Options) (*Router, error) {
	if o.IDs == nil {
		return nil, errors.New("httpx: 缺少 Request ID 生成器（Options.IDs）")
	}
	if o.Logger == nil {
		o.Logger = zap.NewNop()
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Metrics == nil {
		o.Metrics = NewMetrics()
	}
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
	origins, err := normalizeOrigins(o.CORSOrigins)
	if err != nil {
		return nil, err
	}
	ginModeOnce.Do(func() { gin.SetMode(gin.ReleaseMode) })

	e := gin.New()
	// 路由行为收紧：不做尾斜杠/大小写重定向，不对 405 泄漏路由存在性。
	e.RedirectTrailingSlash, e.RedirectFixedPath = false, false
	e.HandleMethodNotAllowed, e.UseRawPath, e.RemoveExtraSlash = false, false, false
	e.ForwardedByClientIP = true
	trusted := o.TrustedProxies
	if trusted == nil {
		trusted = []string{"127.0.0.1", "::1"}
	}
	if err := e.SetTrustedProxies(trusted); err != nil {
		return nil, fmt.Errorf("httpx: 可信代理配置非法: %w", err)
	}

	// 全局链：顺序即 AC1 的前四项，不可重排。
	e.Use(
		requestIDMiddleware(o.IDs, o.Logger, o.MaxBodyBytes),
		recoverMiddleware(o.Metrics),
		accessLogMiddleware(o.Logger, o.Clock, o.Metrics),
		securityMiddleware(origins),
	)
	notFound := func(c *gin.Context) { Fail(c, apierr.NotFound()) }
	e.NoRoute(notFound)
	e.NoMethod(notFound)

	hh := newHealthHandler(o.Health, o.Logger, o.Clock)
	e.GET("/healthz", hh.live)
	e.GET("/healthz/ready", hh.ready)

	return &Router{
		engine: e, hooks: o.Hooks.withDefaults(), idem: o.Idempotency, metrics: o.Metrics, dryRun: o.ManifestOnly,
		routes: map[string]RouteMeta{}, byPath: map[string]string{},
	}, nil
}

// Metrics 返回指标集合（挂到 NewMetricsServer）。
func (r *Router) Metrics() *Metrics { return r.metrics }

// Register 注册一条路由。元数据缺失/非法、路径含 ':'、重复 operationId 或方法+路径、handler 为空、声明 Idempotent 却无存储，
// 都返回错误；错误同时被记录，Handler() 会因此失败——忽略返回值也无法绕过启动失败（NFR-4）。
func (r *Router) Register(meta RouteMeta, h gin.HandlerFunc) (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() {
		if err != nil {
			r.errs = append(r.errs, err)
		}
	}()
	if h == nil {
		return fmt.Errorf("httpx: 路由 %q 的 handler 为空", meta.OperationID)
	}
	if probs := meta.Validate(); len(probs) > 0 {
		return fmt.Errorf("httpx: 路由元数据不合法: %s", strings.Join(probs, "; "))
	}
	if meta.Idempotent && meta.Auth == AuthPublic {
		return fmt.Errorf("httpx: %s: 公开路由不支持 Idempotency-Key（无主体可隔离幂等键，会泄漏他人的响应）", meta.OperationID)
	}
	if meta.Idempotent && r.idem == nil && !r.dryRun {
		return fmt.Errorf("httpx: %s 声明了 Idempotent，但 Options.Idempotency 未配置", meta.OperationID)
	}
	if prev, dup := r.routes[meta.OperationID]; dup {
		return fmt.Errorf("httpx: operationId %q 重复注册（已有 %s %s）", meta.OperationID, prev.Method, prev.Path)
	}
	gp := APIPrefix + ginPath(meta.Path)
	pk := meta.Method + " " + gp
	if prev, dup := r.byPath[pk]; dup {
		return fmt.Errorf("httpx: %s %s 已被 %s 注册", meta.Method, meta.Path, prev)
	}

	denied, _ := r.hooks.Auditor.(DeniedRecorder) // 可选：已认证主体的授权类拒绝留痕（Story 1.22b）
	chain := []gin.HandlerFunc{
		setMetaMiddleware(meta),
		authnMiddleware(r.hooks.Authenticator, meta),
		featureMiddleware(r.hooks.Features, meta, denied), // 认证后、授权前（ADR-018 §11）：未启用的功能不泄漏授权细节
		authzMiddleware(r.hooks.Authorizer, meta, denied),
		scopeMiddleware(r.hooks.Scope, meta, denied),
		auditMiddleware(r.hooks.Auditor, meta),
	}
	if meta.Idempotent {
		chain = append(chain, idempotencyMiddleware(r.idem, r.metrics))
	}
	chain = append(chain, h)

	func() {
		defer func() {
			if rec := recover(); rec != nil { // gin 在通配符冲突等情况下 panic
				err = fmt.Errorf("httpx: 注册 %s %s 失败: %v", meta.Method, meta.Path, rec)
			}
		}()
		r.engine.Handle(meta.Method, gp, chain...)
	}()
	if err != nil {
		return err
	}
	r.routes[meta.OperationID] = meta
	r.byPath[pk] = meta.OperationID
	return nil
}

// Routes 返回已注册路由的元数据（按 operationId 升序）。
func (r *Router) Routes() []RouteMeta {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RouteMeta, 0, len(r.routes))
	for _, m := range r.routes {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OperationID < out[j].OperationID })
	return out
}

// Handler 返回 http.Handler；存在任何注册错误时返回错误（启动失败）。
func (r *Router) Handler() (http.Handler, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.errs) > 0 {
		return nil, fmt.Errorf("httpx: %d 个路由注册错误: %w", len(r.errs), errors.Join(r.errs...))
	}
	if r.dryRun {
		return nil, ErrManifestOnly
	}
	return r.engine, nil
}
