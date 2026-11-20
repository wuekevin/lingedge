package httpx_test

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// recordingChecker 记录 WithReauth 的调用顺序，并按 err 返回。
type recordingChecker struct {
	trace *[]string
	err   error
}

func (c recordingChecker) CheckReauth(*http.Request, httpx.RouteMeta, tenantscope.Principal) error {
	*c.trace = append(*c.trace, "reauth")
	return c.err
}

type recordingAuthz struct {
	trace *[]string
	err   error
}

func (a recordingAuthz) Authorize(*http.Request, httpx.RouteMeta, tenantscope.Principal) error {
	*a.trace = append(*a.trace, "authz")
	return a.err
}

type okAuthn struct{}

func (okAuthn) Authenticate(*http.Request, httpx.RouteMeta) (tenantscope.Principal, error) {
	return &testPrincipal{actor: 1, tenant: 1}, nil
}

// WithReauth：先权限后再认证（Story 1.18 修订）——授权钩子失败则不运行再认证检查（无权限者不会被提示再认证）；
// 授权通过后 x-reauth 路由再过再认证检查（失败则 handler 不执行）；非 x-reauth 路由不检查；未装配检查组件时 fail-closed。
func TestWithReauthOrderingAndFailClosed(t *testing.T) {
	reauthMeta := meta("putRunMode", "PUT", "/admin/system/run-mode")
	reauthMeta.Reauth, reauthMeta.TenantScope = true, httpx.ScopePlatform
	plain := meta("listSites", "GET", "/sites")

	cases := []struct {
		name    string
		meta    httpx.RouteMeta
		checker httpx.ReauthChecker
		authz   error
		status  int
		code    string
		trace   []string
		called  bool
	}{
		{"授权通过后 x-reauth 检查失败", reauthMeta, recordingChecker{err: apierr.New(apierr.AuthReauthRequired, "reauth")}, nil, 403, "auth.reauth_required", []string{"authz", "reauth"}, false},
		{"无权限者先得到 403 auth.forbidden，再认证检查不运行", reauthMeta, recordingChecker{err: apierr.New(apierr.AuthReauthRequired, "reauth")}, apierr.New(apierr.AuthForbidden, "no"), 403, "auth.forbidden", []string{"authz"}, false},
		{"授权与 x-reauth 检查都通过", reauthMeta, recordingChecker{}, nil, 200, "", []string{"authz", "reauth"}, true},
		{"非 x-reauth 路由不检查", plain, recordingChecker{err: apierr.New(apierr.AuthReauthRequired, "reauth")}, nil, 200, "", []string{"authz"}, true},
		{"未装配检查组件：fail-closed", reauthMeta, nil, nil, 403, "auth.forbidden", []string{"authz"}, false},
	}
	for _, c := range cases {
		var trace []string
		switch ch := c.checker.(type) {
		case recordingChecker:
			ch.trace = &trace
			c.checker = ch
		}
		h := httpx.WithReauth(httpx.Hooks{Authenticator: okAuthn{}, Authorizer: recordingAuthz{trace: &trace, err: c.authz}, Scope: allowScope{}, Auditor: noAudit{}}, c.checker)
		r, _ := newRouter(t, httpx.Options{Hooks: h})
		called := false
		if err := r.Register(c.meta, func(ctx *gin.Context) { called = true; httpx.OK(ctx, map[string]bool{"ok": true}) }); err != nil {
			t.Fatal(err)
		}
		rec := do(handler(t, r), c.meta.Method, "/api/v1"+c.meta.Path, "")
		if rec.Code != c.status || called != c.called {
			t.Errorf("%s: 得到 %d called=%v，期望 %d called=%v\n%s", c.name, rec.Code, called, c.status, c.called, rec.Body.String())
			continue
		}
		if c.code != "" {
			if e := decodeErr(t, rec.Body.Bytes()); e.Code != c.code {
				t.Errorf("%s: 错误码 %s，期望 %s", c.name, e.Code, c.code)
			}
		}
		if len(trace) != len(c.trace) || (len(trace) > 0 && trace[0] != c.trace[0]) || (len(trace) > 1 && trace[1] != c.trace[1]) {
			t.Errorf("%s: 轨迹 %v，期望 %v", c.name, trace, c.trace)
		}
	}
}

type allowScope struct{}

func (allowScope) ResolveScope(_ *http.Request, _ httpx.RouteMeta, p tenantscope.Principal) (tenantscope.Scope, error) {
	return tenantscope.FromPrincipal(p, tenantscope.RouteEither, tenantscope.NoTarget())
}

type noAudit struct{}

func (noAudit) Begin(*http.Request, httpx.RouteMeta) (func(httpx.AuditOutcome), error) {
	return nil, nil
}
