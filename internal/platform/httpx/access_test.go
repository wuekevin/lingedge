package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
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

// WithAccessScope：授权决策（自己的租户/平台级/跨租户只读）落成 Scope；acting-as 与「没有决策」委托给原 Scope 钩子（安全拒绝），不放行未实现的部分。
func TestWithAccessScopeBoundary(t *testing.T) {
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
		{"acting-as 委托内层（1.20 前安全拒绝）", kindPrincipal{true}, httpx.ScopeEither, &rbac.Decision{Mode: rbac.ModeActingAs}, 403, true},
		{"没有授权决策 委托内层", kindPrincipal{true}, httpx.ScopeEither, nil, 403, true},
	}
	for _, c := range cases {
		inner := false
		az := recordAuthz{}
		if c.decision != nil {
			az.d = *c.decision
		}
		h := httpx.WithAccessScope(httpx.Hooks{Authenticator: pAuthn{c.principal}, Authorizer: az, Scope: denyAllScope{&inner}, Auditor: noAudit{}})
		r, _ := newRouter(t, httpx.Options{Hooks: h})
		m := meta("probeX", "GET", "/probe-x")
		m.TenantScope = c.scope
		if err := r.Register(m, func(ctx *gin.Context) { httpx.OK(ctx, map[string]bool{"ok": true}) }); err != nil {
			t.Fatal(err)
		}
		rec := do(handler(t, r), "GET", "/api/v1/probe-x", "")
		if rec.Code != c.status || inner != c.inner {
			t.Errorf("%s: 得到 %d inner=%v，期望 %d inner=%v\n%s", c.name, rec.Code, inner, c.status, c.inner, rec.Body.String())
		}
	}
}

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
