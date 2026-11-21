package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// ---- 1. RequestID ----

func requestIDMiddleware(gen requestid.Generator, log *zap.Logger, maxBody int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, id, err := requestid.Ensure(c.Request.Context(), c.GetHeader(requestid.Header), gen)
		if err != nil {
			// 无可用 ID（crypto/rand 失败）：统一错误体，request_id 为空。
			r := apierr.Render(apierr.Internal(err), "")
			c.Header("Content-Type", contentTypeJSON)
			c.Writer.WriteHeader(r.Status)
			_, _ = c.Writer.Write([]byte(`{"error":{"code":"internal.error","message":"internal error","message_key":"errors.internal.error","details":[],"request_id":""}}` + "\n"))
			c.Abort()
			return
		}
		requestid.SetResponseHeader(c.Writer.Header(), id)
		c.Request = c.Request.WithContext(ctx)
		c.Set(loggerKey, logx.WithContext(ctx, log))
		c.Set(maxBodyKey, maxBody)
		c.Next()
	}
}

// ---- 2. recover ----

func recoverMiddleware(m *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler { //nolint:errorlint // 标准库约定的哨兵值，须按值比较并继续向上抛
				panic(rec)
			}
			m.panics.Add(1)
			fields := append([]zap.Field{zap.String("route", c.FullPath())}, logx.PanicFields(rec)...)
			Logger(c).Error("handler panic（已恢复，响应 500）", fields...)
			if !c.Writer.Written() {
				id, _ := requestid.FromContext(c.Request.Context())
				r := apierr.Render(apierr.Internal(errPanic), id)
				c.Header("Content-Type", contentTypeJSON)
				c.Writer.WriteHeader(r.Status)
				b, _ := marshalBody(r.Body)
				_, _ = c.Writer.Write(b)
			}
			c.Abort()
		}()
		c.Next()
	}
}

var errPanic = errors.New("httpx: handler panic")

// ---- 3. 访问日志（脱敏）+ 指标 ----

func accessLogMiddleware(log *zap.Logger, clk clock.Clock, m *Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := clk.Now()
		m.inflight.Add(1)
		panicked := false
		defer func() {
			m.inflight.Add(-1)
			if r := recover(); r != nil {
				panicked = true
				emitAccess(c, log, clk, m, start, panicked)
				panic(r) // 交还给外层 recover 渲染 500
			}
			emitAccess(c, log, clk, m, start, panicked)
		}()
		c.Next()
	}
}

func emitAccess(c *gin.Context, log *zap.Logger, clk clock.Clock, m *Metrics, start time.Time, panicked bool) {
	status := c.Writer.Status()
	if panicked {
		status = http.StatusInternalServerError
	}
	elapsed := clk.Now().Sub(start)
	route := c.FullPath()
	if route == "" {
		route = "unmatched"
	}
	m.observe(c.Request.Method, route, status, elapsed)

	// 只记录路径、不记录查询串与任何请求头/体（令牌、口令可能出现其中）；其余值再经 logx 的脱敏核心过滤。
	fields := []zap.Field{
		zap.String("method", c.Request.Method),
		zap.String("route", route),
		zap.String("path", truncate(c.Request.URL.Path, 512)),
		zap.Int("status", status),
		zap.Int("bytes", max(c.Writer.Size(), 0)),
		zap.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
		zap.String("client_ip", c.ClientIP()),
		zap.String("user_agent", truncate(c.Request.UserAgent(), 200)),
	}
	if meta, ok := MetaFrom(c.Request.Context()); ok {
		fields = append(fields, zap.String("operation_id", meta.OperationID))
	}
	l := log
	if v, ok := c.Get(loggerKey); ok {
		l = v.(*zap.Logger)
	}
	msg := "http_request"
	switch {
	case strings.HasPrefix(c.Request.URL.Path, "/healthz"):
		l.Debug(msg, fields...)
	case status >= 500:
		l.Error(msg, fields...)
	case status >= 400:
		l.Warn(msg, fields...)
	default:
		l.Info(msg, fields...)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- 4. 安全头 / CORS 白名单 ----

// allowedMethods/Headers 与 ADR-018 §3（CSRF）、ADR-017（租户/幂等/并发头）一致。
const (
	corsMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsHeaders = "Content-Type, Authorization, X-CSRF-Token, X-Request-ID, X-Tenant-ID, Idempotency-Key, If-Match"
	corsExpose  = "X-Request-ID, Location, Retry-After, ETag, Idempotent-Replayed"
)

// ErrBadCORSOrigin 表示 CORS 白名单项非法。
var ErrBadCORSOrigin = errors.New("httpx: CORS 白名单项必须是精确的 http(s)://host[:port] 源（不允许 *、null、路径或通配）")

func normalizeOrigins(in []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, o := range in {
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
			u.User != nil || strings.ContainsAny(o, "*") || o != u.Scheme+"://"+u.Host {
			return nil, ErrBadCORSOrigin
		}
		out[o] = true
	}
	return out, nil
}

func securityMiddleware(origins map[string]bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-store")

		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
		}
		h.Add("Vary", "Origin")
		preflight := c.Request.Method == http.MethodOptions && c.GetHeader("Access-Control-Request-Method") != ""
		if !origins[origin] {
			if preflight {
				Fail(c, apierr.New(apierr.AuthForbidden, "origin not allowed"))
				return
			}
			c.Next() // 非白名单源：不加任何 CORS 头，浏览器会拦截跨源读取
			return
		}
		h.Set("Access-Control-Allow-Origin", origin) // 精确回显白名单源，绝不使用 *
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Expose-Headers", corsExpose)
		if preflight {
			h.Set("Access-Control-Allow-Methods", corsMethods)
			h.Set("Access-Control-Allow-Headers", corsHeaders)
			h.Set("Access-Control-Max-Age", "600")
			c.Status(http.StatusNoContent)
			c.Writer.WriteHeaderNow()
			c.Abort()
			return
		}
		c.Next()
	}
}

// ---- 5–8. 认证 / 授权 / Scope / 审计钩子（每条路由）----

func setMetaMiddleware(meta RouteMeta) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(withUserAgent(withClientIP(withMeta(c.Request.Context(), meta), c.ClientIP()), c.Request.UserAgent()))
		c.Next()
	}
}

func authnMiddleware(h Authenticator, meta RouteMeta) gin.HandlerFunc {
	return func(c *gin.Context) {
		if meta.Auth == AuthPublic {
			c.Next()
			return
		}
		p, err := h.Authenticate(c.Request, meta)
		if err == nil && (p == nil || isNilPrincipal(p)) {
			err = apierr.New(apierr.AuthUnauthenticated, "authentication required")
		}
		if err != nil {
			Fail(c, err)
			return
		}
		c.Request = c.Request.WithContext(withPrincipal(c.Request.Context(), p))
		c.Next()
	}
}

// featureMiddleware 执行功能开关（Story 1.22）：声明了 Feature 的路由，功能未启用 → 403 feature.disabled；开关读取失败 → 500（fail-closed）。
// 它在认证之后：未认证的请求仍得到 401（不向匿名者暴露哪些功能被关闭），已认证主体（含 API Token）在授权之前就被拒绝。
func featureMiddleware(g FeatureGate, meta RouteMeta, dr DeniedRecorder) gin.HandlerFunc {
	return func(c *gin.Context) {
		if meta.Feature == "" {
			c.Next()
			return
		}
		ok, err := g.Enabled(c.Request.Context(), meta.Feature)
		if err != nil {
			Fail(c, apierr.Internal(err))
			return
		}
		if !ok {
			err := apierr.New(apierr.FeatureDisabled, "feature is not enabled")
			Fail(c, err)
			noteDenied(c, dr, meta, DenialStageFeature, err)
			return
		}
		c.Next()
	}
}

func authzMiddleware(h Authorizer, meta RouteMeta, dr DeniedRecorder) gin.HandlerFunc {
	return func(c *gin.Context) {
		if meta.Auth == AuthPublic {
			c.Next()
			return
		}
		p, _ := PrincipalFrom(c.Request.Context())
		if p == nil { // 链被错误组装时 fail-closed
			Fail(c, apierr.New(apierr.AuthUnauthenticated, "authentication required"))
			return
		}
		carrier := &accessCarrier{}
		c.Request = c.Request.WithContext(withAccessCarrier(c.Request.Context(), carrier))
		if err := h.Authorize(c.Request, meta, p); err != nil {
			Fail(c, err)
			noteDenied(c, dr, meta, DenialStageAuthorize, err)
			return
		}
		if carrier.set { // 授权决策交给后续钩子（Scope 钩子，见 access.go）
			c.Request = c.Request.WithContext(withAccess(c.Request.Context(), carrier.d))
		}
		c.Next()
	}
}

func scopeMiddleware(h ScopeResolver, meta RouteMeta, dr DeniedRecorder) gin.HandlerFunc {
	return func(c *gin.Context) {
		if meta.Auth == AuthPublic {
			c.Next()
			return
		}
		p, _ := PrincipalFrom(c.Request.Context())
		if p == nil {
			Fail(c, apierr.New(apierr.AuthUnauthenticated, "authentication required"))
			return
		}
		carrier := &scopeCarrier{}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), scopeCarrierKey{}, carrier))
		s, err := h.ResolveScope(c.Request, meta, p)
		if err == nil && s.IsZero() {
			err = apierr.New(apierr.AuthForbidden, "forbidden")
		}
		if err != nil {
			err = MapScopeError(err)
			Fail(c, err)
			noteDenied(c, dr, meta, DenialStageScope, err)
			return
		}
		ctx := tenantscope.NewContext(c.Request.Context(), s)
		if carrier.set { // acting-as 审计上下文（供审计钩子，Story 1.21）
			ctx = context.WithValue(ctx, actingAsKey{}, carrier.a)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// noteDenied 在已认证主体被授权类拒绝（403 auth.forbidden / auth.reauth_required / feature.disabled）后通知可选的 DeniedRecorder（Story 1.22b）。
// 404（不泄漏存在性）、400、401、429、5xx 一律不登记；没有主体（链被错误组装）不登记；实现的 panic 被兜住，绝不影响已确定的响应。
func noteDenied(c *gin.Context, dr DeniedRecorder, meta RouteMeta, stage string, err error) {
	if dr == nil || err == nil {
		return
	}
	ae, ok := apierr.As(err)
	if !ok || (ae.Code != apierr.AuthForbidden && ae.Code != apierr.AuthReauthRequired && ae.Code != apierr.FeatureDisabled) {
		return
	}
	if p, ok := PrincipalFrom(c.Request.Context()); !ok || p == nil {
		return
	}
	defer func() { _ = recover() }() // 防刷审计是尽力而为：自身故障不得影响响应
	dr.RecordDenied(c.Request, meta, Denial{Stage: stage, Code: ae.Code})
}

const replayedKey = "lingedge.idempotent_replay"

func auditMiddleware(h Auditor, meta RouteMeta) gin.HandlerFunc {
	return func(c *gin.Context) {
		trail := &auditTrail{}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), auditTrailKey{}, trail))
		end, err := h.Begin(c.Request, meta)
		if err != nil {
			Fail(c, err)
			return
		}
		if end == nil {
			c.Next()
			return
		}
		panicked := true
		defer func() {
			replayed, _ := c.Get(replayedKey)
			out := AuditOutcome{Status: c.Writer.Status(), Replayed: replayed == true, Panicked: panicked, Recorded: int(trail.n.Load())} //nolint:errorlint // bool 比较
			if panicked {
				out.Status = http.StatusInternalServerError
			}
			end(out)
		}()
		c.Next()
		panicked = false
	}
}
