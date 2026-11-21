package httpx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/rbac"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// recordAuthz 在放行时登记一个授权决策（模拟 RBAC 授权钩子）。
type recordAuthz struct {
	d   rbac.Decision
	err error
}

func (a recordAuthz) Authorize(r *http.Request, _ httpx.RouteMeta, _ tenantscope.Principal) error {
	if a.err != nil {
		return a.err
	}
	httpx.RecordAccess(r, a.d)
	return nil
}

type denyAllScope struct{ called *bool }

func (d denyAllScope) ResolveScope(*http.Request, httpx.RouteMeta, tenantscope.Principal) (tenantscope.Scope, error) {
	*d.called = true
	return tenantscope.Scope{}, apierr.New(apierr.AuthForbidden, "forbidden")
}

type kindPrincipal struct{ platform bool }

func (kindPrincipal) ActorID() uint64           { return 7 }
func (p kindPrincipal) IsPlatform() bool        { return p.platform }
func (p kindPrincipal) TenantID() uint64        { return map[bool]uint64{true: 0, false: 9}[p.platform] }
func (kindPrincipal) HasPermission(string) bool { return true }

type pAuthn struct{ p tenantscope.Principal }

func (a pAuthn) Authenticate(*http.Request, httpx.RouteMeta) (tenantscope.Principal, error) {
	return a.p, nil
}

const tenantA = "01M7QP5V346STC0AT56NME5NR3"

// fakeTenants 是租户目录替身（只用于验证 Scope 钩子对目录结果的处理；真实目录在 tests/security/tenant_isolation 里用真实 MySQL 验证）。
type fakeTenants struct {
	ref   httpx.TenantRef
	err   error
	calls int
}

func (f *fakeTenants) ResolveTenant(_ context.Context, uid string) (httpx.TenantRef, error) {
	f.calls++
	if f.err != nil {
		return httpx.TenantRef{}, f.err
	}
	r := f.ref
	if r.UID == "" {
		r.UID = uid
	}
	return r, nil
}

func scopeRouter(t *testing.T, principal tenantscope.Principal, d *rbac.Decision, ts httpx.TenantResolver, inner *bool, scope httpx.TenantScope) (http.Handler, *syncBuf) {
	t.Helper()
	az := recordAuthz{}
	if d != nil {
		az.d = *d
	}
	buf := &syncBuf{}
	lg, err := logx.New(logx.Options{Level: "debug", Output: buf})
	if err != nil {
		t.Fatal(err)
	}
	h := httpx.WithTenantScope(httpx.Hooks{Authenticator: pAuthn{principal}, Authorizer: az, Scope: denyAllScope{inner}, Auditor: noAudit{}}, ts, lg)
	r, _ := newRouter(t, httpx.Options{Hooks: h})
	m := meta("probeX", "GET", "/probe-x")
	m.TenantScope = scope
	if err := r.Register(m, func(ctx *gin.Context) {
		sc, _ := tenantscope.FromContext(ctx.Request.Context())
		a, acting := httpx.ActingAsFrom(ctx.Request.Context())
		httpx.OK(ctx, map[string]any{"tenant_id": sc.TenantID(), "platform": sc.IsPlatform(), "acting_as": sc.ActingAs(), "ctx_acting": acting, "ctx_tenant_uid": a.TenantUID, "ctx_actor": a.ActorID, "ctx_op": a.OperationID})
	}); err != nil {
		t.Fatal(err)
	}
	return handler(t, r), buf
}

// 完整 Scope 钩子：授权决策（自己的租户/平台级/跨租户只读）落成 Scope；没有决策、决策与主体不一致一律委托内层（安全拒绝），不放行未验证的情形。
func TestTenantScopeModes(t *testing.T) {
	cases := []struct {
		name      string
		principal tenantscope.Principal
		scope     httpx.TenantScope
		decision  *rbac.Decision
		status    int
		inner     bool // 是否委托给了内层 Scope 钩子
	}{
		{"租户主体 自己的租户", kindPrincipal{false}, httpx.ScopeEither, &rbac.Decision{Mode: rbac.ModeOwnTenant}, 200, false},
		{"平台主体 平台级", kindPrincipal{true}, httpx.ScopePlatform, &rbac.Decision{Mode: rbac.ModePlatform}, 200, false},
		{"平台主体 跨租户只读", kindPrincipal{true}, httpx.ScopeEither, &rbac.Decision{Mode: rbac.ModeCrossTenantRead}, 200, false},
		{"没有授权决策 委托内层", kindPrincipal{true}, httpx.ScopeEither, nil, 403, true},
		{"决策与主体不一致：平台主体 + own_tenant", kindPrincipal{true}, httpx.ScopeEither, &rbac.Decision{Mode: rbac.ModeOwnTenant}, 403, false},
		{"决策与主体不一致：租户主体 + platform", kindPrincipal{false}, httpx.ScopePlatform, &rbac.Decision{Mode: rbac.ModePlatform}, 403, false},
		{"平台主体 ModePlatform + either 路由（自有平台令牌管理）", kindPrincipal{true}, httpx.ScopeEither, &rbac.Decision{Mode: rbac.ModePlatform}, 200, false},
		{"决策与路由不一致：cross_tenant_read + platform 路由", kindPrincipal{true}, httpx.ScopePlatform, &rbac.Decision{Mode: rbac.ModeCrossTenantRead}, 403, false},
		{"未知模式 委托内层", kindPrincipal{true}, httpx.ScopeEither, &rbac.Decision{Mode: "bogus"}, 403, true},
	}
	for _, c := range cases {
		inner := false
		h, _ := scopeRouter(t, c.principal, c.decision, &fakeTenants{}, &inner, c.scope)
		rec := do(h, "GET", "/api/v1/probe-x", "")
		if rec.Code != c.status || inner != c.inner {
			t.Errorf("%s: 得到 %d inner=%v，期望 %d inner=%v\n%s", c.name, rec.Code, inner, c.status, c.inner, rec.Body.String())
		}
	}
}

// acting-as：目标租户经租户目录解析；成功时 Scope 是该租户 + ActingAs，并登记审计上下文（actor + 目标租户）与结构化日志。
func TestTenantScopeActingAsSuccess(t *testing.T) {
	inner := false
	ts := &fakeTenants{ref: httpx.TenantRef{ID: 42, Status: "active"}}
	h, buf := scopeRouter(t, kindPrincipal{true}, &rbac.Decision{Mode: rbac.ModeActingAs}, ts, &inner, httpx.ScopeTenant)
	rec := do(h, "GET", "/api/v1/probe-x", "", "X-Tenant-ID", tenantA)
	if rec.Code != 200 || inner {
		t.Fatalf("%d inner=%v %s", rec.Code, inner, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"tenant_id":42`, `"acting_as":true`, `"ctx_acting":true`, `"ctx_tenant_uid":"` + tenantA + `"`, `"ctx_actor":7`, `"ctx_op":"probeX"`} {
		if !strings.Contains(body, want) {
			t.Errorf("响应应含 %s: %s", want, body)
		}
	}
	logs := buf.String()
	for _, want := range []string{"管理员代租户操作", `"acting_as_tenant_uid":"` + tenantA + `"`, `"actor_id":7`} {
		if !strings.Contains(logs, want) {
			t.Errorf("日志应含 %s", want)
		}
	}
	// 非代租户请求没有 acting-as 上下文。
	h2, _ := scopeRouter(t, kindPrincipal{true}, &rbac.Decision{Mode: rbac.ModeCrossTenantRead}, ts, &inner, httpx.ScopeEither)
	if rec := do(h2, "GET", "/api/v1/probe-x", ""); !strings.Contains(rec.Body.String(), `"ctx_acting":false`) {
		t.Errorf("跨租户只读不应有 acting-as 上下文: %s", rec.Body.String())
	}
}

// acting-as 失败：不存在/已删除/状态不可用/目录 uid 不符一律 404 且响应体逐字节相同（不泄漏存在性）；目录故障 500；目录未装配 403。
func TestTenantScopeActingAsFailures(t *testing.T) {
	cases := []struct {
		name   string
		ts     httpx.TenantResolver
		header []string
		status int
		inner  bool
	}{
		{"不存在", &fakeTenants{err: httpx.ErrTenantNotFound}, []string{"X-Tenant-ID", tenantA}, 404, false},
		{"suspended", &fakeTenants{ref: httpx.TenantRef{ID: 1, Status: "suspended"}}, []string{"X-Tenant-ID", tenantA}, 404, false},
		{"closed", &fakeTenants{ref: httpx.TenantRef{ID: 1, Status: "closed"}}, []string{"X-Tenant-ID", tenantA}, 404, false},
		{"pending", &fakeTenants{ref: httpx.TenantRef{ID: 1, Status: "pending"}}, []string{"X-Tenant-ID", tenantA}, 404, false},
		{"目录 uid 与请求不符", &fakeTenants{ref: httpx.TenantRef{ID: 1, UID: "01M7QP5V346STC0AT56NME5NR4", Status: "active"}}, []string{"X-Tenant-ID", tenantA}, 404, false},
		{"目录返回 id=0", &fakeTenants{ref: httpx.TenantRef{ID: 0, Status: "active"}}, []string{"X-Tenant-ID", tenantA}, 404, false},
		{"目录故障 fail-closed", &fakeTenants{err: errors.New("db down")}, []string{"X-Tenant-ID", tenantA}, 500, false},
		{"缺少 X-Tenant-ID", &fakeTenants{ref: httpx.TenantRef{ID: 1, Status: "active"}}, nil, 400, false},
		{"X-Tenant-ID 非法", &fakeTenants{ref: httpx.TenantRef{ID: 1, Status: "active"}}, []string{"X-Tenant-ID", "not-a-uid"}, 400, false},
		{"租户目录未装配：安全拒绝", nil, []string{"X-Tenant-ID", tenantA}, 403, true},
	}
	var notFound []string
	for _, c := range cases {
		inner := false
		h, _ := scopeRouter(t, kindPrincipal{true}, &rbac.Decision{Mode: rbac.ModeActingAs}, c.ts, &inner, httpx.ScopeTenant)
		rec := do(h, "GET", "/api/v1/probe-x", "", c.header...)
		if rec.Code != c.status || inner != c.inner {
			t.Errorf("%s: 得到 %d inner=%v，期望 %d inner=%v\n%s", c.name, rec.Code, inner, c.status, c.inner, rec.Body.String())
		}
		if c.status == 404 {
			notFound = append(notFound, withoutRequestID(rec.Body.String()))
		}
	}
	for _, b := range notFound[1:] {
		if b != notFound[0] {
			t.Errorf("404 响应体应逐字节一致:\n%s\n%s", notFound[0], b)
		}
	}
	// 租户目录未装配时不会被调用；平台路由、非平台主体的 acting-as 决策同样委托内层。
	for _, c := range []struct {
		p     tenantscope.Principal
		scope httpx.TenantScope
	}{{kindPrincipal{false}, httpx.ScopeTenant}, {kindPrincipal{true}, httpx.ScopePlatform}} {
		inner := false
		ts := &fakeTenants{ref: httpx.TenantRef{ID: 1, Status: "active"}}
		h, _ := scopeRouter(t, c.p, &rbac.Decision{Mode: rbac.ModeActingAs}, ts, &inner, c.scope)
		if rec := do(h, "GET", "/api/v1/probe-x", "", "X-Tenant-ID", tenantA); rec.Code != 403 || !inner || ts.calls != 0 {
			t.Errorf("非法 acting-as 决策应委托内层且不查目录: %d inner=%v calls=%d", rec.Code, inner, ts.calls)
		}
	}
}

var reqIDRe = regexp.MustCompile(`"request_id":"[^"]*"`)

func withoutRequestID(s string) string { return reqIDRe.ReplaceAllString(s, `"request_id":""`) }

// 授权钩子拒绝时不登记决策，也不会进入 Scope 钩子。
func TestRecordAccessIgnoredOnDenyAndOutsideChain(t *testing.T) {
	httpx.RecordAccess(httptest.NewRequest("GET", "/x", nil), rbac.Decision{Mode: rbac.ModePlatform}) // 链外调用：空操作，不 panic
	called := false
	h := httpx.Hooks{Authenticator: pAuthn{kindPrincipal{true}}, Authorizer: recordAuthz{err: apierr.New(apierr.AuthForbidden, "no")}, Scope: denyAllScope{&called}, Auditor: noAudit{}}
	r, _ := newRouter(t, httpx.Options{Hooks: h})
	if err := r.Register(meta("probeY", "GET", "/probe-y"), func(ctx *gin.Context) { httpx.OK(ctx, map[string]bool{"ok": true}) }); err != nil {
		t.Fatal(err)
	}
	if rec := do(handler(t, r), "GET", "/api/v1/probe-y", ""); rec.Code != 403 || called {
		t.Errorf("授权拒绝应止步于授权钩子: %d called=%v", rec.Code, called)
	}
}
