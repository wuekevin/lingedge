package httpx_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

var ulidRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func decodeErr(t *testing.T, body []byte) apierr.Payload {
	t.Helper()
	var b apierr.Body
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("错误体不是统一格式: %v\n%s", err, body)
	}
	return b.Error
}

// AC1：钩子顺序固定为 认证 → 授权 → Scope → 审计 → handler，审计结束回调在 handler 之后。
func TestHookChainOrder(t *testing.T) {
	oh := &orderHooks{p: &testPrincipal{actor: 7, tenant: 3}}
	r, _ := newRouter(t, httpx.Options{Hooks: oh.hooks()})
	var sawScope tenantscope.Scope
	var sawPrincipal bool
	if err := r.Register(meta("listSites", "GET", "/sites"), func(c *gin.Context) {
		oh.add("handler")
		sawScope, _ = tenantscope.FromContext(c.Request.Context())
		_, sawPrincipal = httpx.PrincipalFrom(c.Request.Context())
		httpx.OK(c, map[string]any{"ok": true})
	}); err != nil {
		t.Fatal(err)
	}
	rec := do(handler(t, r), "GET", "/api/v1/sites", "")
	if rec.Code != 200 {
		t.Fatalf("状态 %d: %s", rec.Code, rec.Body)
	}
	want := []string{"authn", "authz", "scope", "audit", "handler", "audit-end"}
	if got := oh.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("钩子顺序 = %v，期望 %v", got, want)
	}
	if !sawPrincipal || sawScope.TenantID() != 3 {
		t.Errorf("handler 应能取得主体与 Scope: principal=%v scope=%v", sawPrincipal, sawScope)
	}
}

// 认证失败时后续钩子与 handler 都不执行。
func TestHookChainShortCircuits(t *testing.T) {
	oh := &orderHooks{p: nil} // 认证钩子返回 (nil,nil) → 必须按未认证处理
	r, _ := newRouter(t, httpx.Options{Hooks: oh.hooks()})
	called := false
	if err := r.Register(meta("listSites", "GET", "/sites"), func(c *gin.Context) { called = true; httpx.OK(c, 1) }); err != nil {
		t.Fatal(err)
	}
	rec := do(handler(t, r), "GET", "/api/v1/sites", "")
	if rec.Code != 401 || decodeErr(t, rec.Body.Bytes()).Code != "auth.unauthenticated" {
		t.Fatalf("nil 主体必须 401: %d %s", rec.Code, rec.Body)
	}
	if called || !reflect.DeepEqual(oh.get(), []string{"authn"}) {
		t.Fatalf("短路失败: called=%v trace=%v", called, oh.get())
	}
}

// 公开路由跳过认证/授权/Scope，但仍经审计钩子。
func TestPublicRouteSkipsAuthHooks(t *testing.T) {
	oh := &orderHooks{}
	r, _ := newRouter(t, httpx.Options{Hooks: oh.hooks()})
	m := httpx.RouteMeta{OperationID: "login", Method: "POST", Path: "/auth/login", Auth: httpx.AuthPublic, Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopePublic, Audit: true, Phase: "M1"}
	if err := r.Register(m, func(c *gin.Context) { oh.add("handler"); httpx.OK(c, 1) }); err != nil {
		t.Fatal(err)
	}
	if rec := do(handler(t, r), "POST", "/api/v1/auth/login", ""); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if got, want := oh.get(), []string{"audit", "handler", "audit-end"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("公开路由链 = %v，期望 %v", got, want)
	}
}

// 默认钩子是安全拒绝而不是放行：未装配任何钩子时，非公开路由 401，handler 绝不执行。
func TestDefaultHooksDenyNotAllow(t *testing.T) {
	r, _ := newRouter(t, httpx.Options{})
	called := false
	h := func(c *gin.Context) { called = true; httpx.OK(c, 1) }
	if err := r.Register(meta("listSites", "GET", "/sites"), h); err != nil {
		t.Fatal(err)
	}
	pub := httpx.RouteMeta{OperationID: "login", Method: "POST", Path: "/auth/login", Auth: httpx.AuthPublic, Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopePublic, Audit: true, Phase: "M1"}
	if err := r.Register(pub, h); err != nil {
		t.Fatal(err)
	}
	noaudit := pub
	noaudit.OperationID, noaudit.Path, noaudit.Audit = "ping", "/ping", false
	if err := r.Register(noaudit, h); err != nil {
		t.Fatal(err)
	}
	srv := handler(t, r)
	if rec := do(srv, "GET", "/api/v1/sites", ""); rec.Code != 401 {
		t.Errorf("默认认证钩子应 401: %d", rec.Code)
	}
	if rec := do(srv, "POST", "/api/v1/auth/login", ""); rec.Code != 500 { // 要求审计但审计未装配 → 拒绝
		t.Errorf("Audit=true 且审计未装配应拒绝: %d", rec.Code)
	}
	if called {
		t.Fatal("默认钩子放行了请求：handler 被执行")
	}
	if rec := do(srv, "POST", "/api/v1/ping", ""); rec.Code != 200 { // 无需审计的公开路由不受影响
		t.Errorf("Audit=false 的公开路由应可达: %d", rec.Code)
	}

	// 逐个钩子的默认实现单测：Deny* 各自独立拒绝。
	if _, err := (httpx.DenyAuthenticator{}).Authenticate(nil, httpx.RouteMeta{}); err == nil {
		t.Error("DenyAuthenticator 必须返回错误")
	}
	if err := (httpx.DenyAuthorizer{}).Authorize(nil, httpx.RouteMeta{}, &testPrincipal{actor: 1}); err == nil {
		t.Error("DenyAuthorizer 必须返回错误")
	}
	if s, err := (httpx.DenyScope{}).ResolveScope(nil, httpx.RouteMeta{}, &testPrincipal{actor: 1}); err == nil || !s.IsZero() {
		t.Error("DenyScope 必须返回错误与零值 Scope")
	}
}

// 授权/Scope 钩子各自拒绝时的状态；Scope 钩子返回零值也视为拒绝。
type partialHooks struct {
	orderHooks
	authzErr  error
	scopeErr  error
	scopeZero bool
}

func (p *partialHooks) Authorize(r *http.Request, m httpx.RouteMeta, pr tenantscope.Principal) error {
	p.add("authz")
	return p.authzErr
}
func (p *partialHooks) ResolveScope(r *http.Request, m httpx.RouteMeta, pr tenantscope.Principal) (tenantscope.Scope, error) {
	p.add("scope")
	if p.scopeZero {
		return tenantscope.Scope{}, nil
	}
	return tenantscope.Scope{}, p.scopeErr
}

func TestAuthzAndScopeDenials(t *testing.T) {
	cases := []struct {
		name   string
		ph     *partialHooks
		status int
		code   string
		trace  []string
	}{
		{"授权拒绝", &partialHooks{authzErr: apierr.New(apierr.AuthForbidden, "no")}, 403, "auth.forbidden", []string{"authn", "authz"}},
		{"再认证要求", &partialHooks{authzErr: apierr.New(apierr.AuthReauthRequired, "reauth")}, 403, "auth.reauth_required", []string{"authn", "authz"}},
		{"Scope 零值", &partialHooks{scopeZero: true}, 403, "auth.forbidden", []string{"authn", "authz", "scope"}},
		{"Scope 跨租户", &partialHooks{scopeErr: tenantscope.ErrTenantMismatch}, 404, "resource.not_found", []string{"authn", "authz", "scope"}},
		{"Scope 缺目标租户", &partialHooks{scopeErr: tenantscope.ErrTenantRequired}, 400, "validation.failed", []string{"authn", "authz", "scope"}},
		{"Scope 越权平台路由", &partialHooks{scopeErr: tenantscope.ErrForbiddenRoute}, 403, "auth.forbidden", []string{"authn", "authz", "scope"}},
		{"钩子未知错误", &partialHooks{authzErr: http.ErrHandlerTimeout}, 500, "internal.error", []string{"authn", "authz"}},
	}
	for _, c := range cases {
		c.ph.p = &testPrincipal{actor: 1, tenant: 1}
		hk := httpx.Hooks{Authenticator: &c.ph.orderHooks, Authorizer: c.ph, Scope: c.ph, Auditor: &c.ph.orderHooks}
		r, _ := newRouter(t, httpx.Options{Hooks: hk})
		called := false
		if err := r.Register(meta("listSites", "GET", "/sites"), func(ctx *gin.Context) { called = true }); err != nil {
			t.Fatal(err)
		}
		rec := do(handler(t, r), "GET", "/api/v1/sites", "")
		e := decodeErr(t, rec.Body.Bytes())
		if rec.Code != c.status || e.Code != c.code || called {
			t.Errorf("%s: 得到 %d %s called=%v，期望 %d %s", c.name, rec.Code, e.Code, called, c.status, c.code)
		}
		if got := c.ph.get(); !reflect.DeepEqual(got, c.trace) {
			t.Errorf("%s: 钩子轨迹 %v，期望 %v", c.name, got, c.trace)
		}
		if c.status == 500 && strings.Contains(rec.Body.String(), "timeout") {
			t.Errorf("%s: 500 不得回显原因", c.name)
		}
	}
}

// AC1 的前四项：RequestID → recover → 访问日志 → 安全头；panic 仍带 Request ID、被访问日志记为 500、不泄漏原因。
func TestPanicRecoveredWithRequestIDAndAccessLog(t *testing.T) {
	oh := &orderHooks{p: &testPrincipal{actor: 1, tenant: 1}}
	r, logs := newRouter(t, httpx.Options{Hooks: oh.hooks()})
	if err := r.Register(meta("listSites", "GET", "/sites"), func(c *gin.Context) {
		panic("boom password=hunter2-SECRET")
	}); err != nil {
		t.Fatal(err)
	}
	rec := do(handler(t, r), "GET", "/api/v1/sites", "")
	if rec.Code != 500 {
		t.Fatalf("panic 应 500: %d", rec.Code)
	}
	e := decodeErr(t, rec.Body.Bytes())
	if e.Code != "internal.error" || !ulidRe.MatchString(e.RequestID) || rec.Header().Get("X-Request-ID") != e.RequestID {
		t.Errorf("500 应带统一错误体与 Request ID: %+v hdr=%q", e, rec.Header().Get("X-Request-ID"))
	}
	if strings.Contains(rec.Body.String(), "boom") || strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("响应泄漏了 panic 内容: %s", rec.Body)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("panic 响应也应带安全头")
	}
	out := logs.String()
	if strings.Contains(out, "hunter2-SECRET") {
		t.Errorf("日志泄漏了 panic 中的口令: %s", out)
	}
	if !strings.Contains(out, `"msg":"http_request"`) || !strings.Contains(out, `"status":500`) || !strings.Contains(out, `"request_id":"`+e.RequestID+`"`) {
		t.Errorf("访问日志应记录 500 与 request_id: %s", out)
	}
	if !strings.Contains(out, "handler panic") {
		t.Errorf("应有 panic 日志: %s", out)
	}
	// panic 时审计结束回调仍被调用，并标记 Panicked（见下方）。
	if got := oh.get(); got[len(got)-1] != "audit-end" {
		t.Errorf("panic 后审计结束回调应仍被调用: %v", got)
	}
}

// 访问日志脱敏：不记录查询串、Authorization、Cookie 与请求体；路径里的敏感片段经脱敏核心处理。
func TestAccessLogRedaction(t *testing.T) {
	oh := &orderHooks{p: &testPrincipal{actor: 1, tenant: 1}}
	r, logs := newRouter(t, httpx.Options{Hooks: oh.hooks()})
	if err := r.Register(meta("listSites", "GET", "/sites"), okHandler("x")); err != nil {
		t.Fatal(err)
	}
	rec := do(handler(t, r), "GET", "/api/v1/sites?token=le_abcd1234_SUPERSECRETTOKENVALUE&password=hunter2", "",
		"Authorization", "Bearer le_abcd1234_SUPERSECRETTOKENVALUE", "Cookie", "le_admin_sid=SESSIONSECRETVALUE123456", "User-Agent", "ua/1.0")
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	out := logs.String()
	for _, leak := range []string{"SUPERSECRETTOKENVALUE", "hunter2", "SESSIONSECRETVALUE", "token=", "password="} {
		if strings.Contains(out, leak) {
			t.Errorf("访问日志泄漏 %q: %s", leak, out)
		}
	}
	for _, want := range []string{`"route":"/api/v1/sites"`, `"path":"/api/v1/sites"`, `"status":200`, `"operation_id":"listSites"`, `"method":"GET"`} {
		if !strings.Contains(out, want) {
			t.Errorf("访问日志缺少 %s: %s", want, out)
		}
	}
}

// Request ID：合法 ULID 沿用（规整为大写），非法（含日志注入）丢弃重新生成；404 也带头与统一错误体；405 不泄漏路由存在性。
func TestRequestIDAndNotFound(t *testing.T) {
	r, logs := newRouter(t, httpx.Options{})
	if err := r.Register(meta("listSites", "GET", "/sites"), okHandler("x")); err != nil {
		t.Fatal(err)
	}
	h := handler(t, r)
	rec := do(h, "GET", "/nope", "", "X-Request-ID", "01arz3ndektsv4rrffq69g5fav")
	if rec.Code != 404 {
		t.Fatalf("未知路径应 404: %d", rec.Code)
	}
	e := decodeErr(t, rec.Body.Bytes())
	if e.Code != "resource.not_found" || e.RequestID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" || rec.Header().Get("X-Request-ID") != e.RequestID {
		t.Errorf("沿用合法 ULID: %+v hdr=%q", e, rec.Header().Get("X-Request-ID"))
	}
	rec = do(h, "GET", "/nope", "", "X-Request-ID", "evil\\ninjected {\"level\":\"error\"}")
	if id := rec.Header().Get("X-Request-ID"); !ulidRe.MatchString(id) {
		t.Errorf("非法 Request ID 应被重新生成: %q", id)
	}
	if strings.Contains(logs.String(), "injected") {
		t.Error("非法 Request ID 不得进入日志")
	}
	// 方法不符：与不存在的路径同样返回 404（不泄漏存在性）。
	if rec := do(h, "DELETE", "/api/v1/sites", ""); rec.Code != 404 || decodeErr(t, rec.Body.Bytes()).Code != "resource.not_found" {
		t.Errorf("405 应折叠为 404: %d %s", rec.Code, rec.Body)
	}
	// 尾斜杠不重定向。
	if rec := do(h, "GET", "/api/v1/sites/", ""); rec.Code != 404 {
		t.Errorf("尾斜杠不应命中/重定向: %d", rec.Code)
	}
}

func TestSecurityHeadersAndCORS(t *testing.T) {
	oh := &orderHooks{p: &testPrincipal{actor: 1, tenant: 1}}
	r, _ := newRouter(t, httpx.Options{Hooks: oh.hooks(), CORSOrigins: []string{"https://console.example.com"}})
	if err := r.Register(meta("listSites", "GET", "/sites"), okHandler("x")); err != nil {
		t.Fatal(err)
	}
	h := handler(t, r)

	rec := do(h, "GET", "/api/v1/sites", "")
	for k, v := range map[string]string{
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "Cache-Control": "no-store",
	} {
		if rec.Header().Get(k) != v {
			t.Errorf("安全头 %s = %q，期望 %q", k, rec.Header().Get(k), v)
		}
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("缺少 CSP")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("无 Origin 的请求不应有 CORS 头")
	}

	// 白名单源：精确回显，带 Vary；绝不是 *。
	rec = do(h, "GET", "/api/v1/sites", "", "Origin", "https://console.example.com")
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://console.example.com" || rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("白名单源应获得 CORS 头: %v", rec.Header())
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Error("应带 Vary: Origin")
	}
	// 非白名单源（含子域/端口/协议差异）：无 CORS 头。
	for _, o := range []string{"https://evil.example.com", "https://console.example.com:8443", "http://console.example.com", "https://console.example.com.evil.com", "null"} {
		rec = do(h, "GET", "/api/v1/sites", "", "Origin", o)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("源 %q 不在白名单却得到了 CORS 头", o)
		}
	}
	// 预检：白名单 → 204 + 允许的方法/头；非白名单 → 403。
	rec = do(h, "OPTIONS", "/api/v1/sites", "", "Origin", "https://console.example.com", "Access-Control-Request-Method", "POST")
	if rec.Code != 204 || !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key") || !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "X-CSRF-Token") {
		t.Errorf("白名单预检: %d %v", rec.Code, rec.Header())
	}
	rec = do(h, "OPTIONS", "/api/v1/sites", "", "Origin", "https://evil.example.com", "Access-Control-Request-Method", "POST")
	if rec.Code != 403 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("非白名单预检应 403 且无 CORS 头: %d", rec.Code)
	}
}

func TestBadCORSWhitelistRejected(t *testing.T) {
	for _, o := range []string{"*", "null", "https://*.example.com", "https://a.example.com/path", "ftp://a.example.com", "a.example.com", "https://a.example.com/", "https://someone@a.example.com"} {
		if _, err := httpx.NewRouter(httpx.Options{IDs: newIDs(), CORSOrigins: []string{o}}); err == nil {
			t.Errorf("CORS 白名单项 %q 应被拒绝", o)
		}
	}
}

// ManifestOnly：不需要幂等存储即可注册（用于导出清单），但不能对外服务；清单里的元数据与注册的一致（权限排序）。
func TestManifestOnlyRouter(t *testing.T) {
	r, _ := newRouter(t, httpx.Options{ManifestOnly: true})
	m := meta("createThing", "POST", "/things")
	m.Idempotent = true
	m.Permissions = []string{"thing:write", "thing:read"}
	if err := r.Register(m, func(c *gin.Context) {}); err != nil {
		t.Fatalf("ManifestOnly 应允许注册 Idempotent 路由: %v", err)
	}
	if _, err := r.Handler(); err != httpx.ErrManifestOnly {
		t.Fatalf("ManifestOnly 的 Router 不得对外服务: %v", err)
	}
	b, err := r.ManifestJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got []httpx.ManifestEntry
	if err := json.Unmarshal(b, &got); err != nil || len(got) != 1 {
		t.Fatalf("%v %s", err, b)
	}
	e := got[0]
	if e.OperationID != "createThing" || e.Path != "/things" || !e.Idempotent || strings.Join(e.Permissions, ",") != "thing:read,thing:write" || e.TenantScope != "tenant" || e.Phase != "M1" {
		t.Errorf("清单内容: %+v", e)
	}
	// 非 ManifestOnly 的 Router 对 Idempotent 路由仍要求存储（已在 TestRegisterFailuresMakeHandlerFail 覆盖）。
}
