package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// bearerStandIn 是 Bearer 认证器的单元测试替身：返回一个 Bearer=true 的 domain.Principal，只用来隔离验证「认证钩子对 Bearer 请求的策略」
// （近期 MFA 拒绝、可调用的 operation 范围）；真实的令牌认证由 tests/integration/auth/apitoken_*_test.go 用真实 MySQL/Redis 验证。
type bearerStandIn struct{}

func (bearerStandIn) Authenticate(*http.Request, httpx.RouteMeta) (tenantscope.Principal, error) {
	return &domain.Principal{Bearer: true, User: domain.User{ID: 1, UserType: domain.UserTypePlatform}}, nil
}

// Bearer 请求没有会话，无法满足「近期 MFA」：对 recentMFAOperations（改密码、创建 API Token）一律 403 auth.reauth_required；其他 operation 正常通过且免 CSRF。
func TestBearerCannotSatisfyRecentMFA(t *testing.T) {
	a := NewAuthenticator(nil, nil, bearerStandIn{})
	req := httptest.NewRequest("POST", "/x", nil)
	req.Header.Set("Authorization", "Bearer le_ab12cd34_xxxxxxxxxxxx")
	for op := range recentMFAOperations {
		_, err := a.Authenticate(req, httpx.RouteMeta{OperationID: op})
		if e, ok := apierr.As(err); !ok || e.Code != apierr.AuthReauthRequired {
			t.Errorf("%s: Bearer 应得到 auth.reauth_required: %v", op, err)
		}
	}
	if p, err := a.Authenticate(req, httpx.RouteMeta{OperationID: "listSites", Permissions: []string{"site:read"}}); err != nil || p == nil {
		t.Errorf("业务路由（声明了 x-permissions）的 Bearer 请求应通过（免 CSRF）: %v", err)
	}
	// Bearer 只能调用业务路由：自助 operation（无权限声明）与令牌管理要求 Cookie 会话。
	for _, op := range []string{"getMe", "logout", "listSessions", "setupTotp", "enableTotp", "disableTotp", "reauth", "listMyLoginLogs", "listApiTokens", "getApiToken", "revokeApiToken"} {
		_, err := a.Authenticate(req, httpx.RouteMeta{OperationID: op, Permissions: map[bool][]string{true: {"token:manage"}, false: nil}[IsTokenOperation(op)]})
		if e, ok := apierr.As(err); !ok || e.Code != apierr.AuthForbidden {
			t.Errorf("%s: Bearer 应得到 auth.forbidden: %v", op, err)
		}
	}
	for _, op := range []string{"changePassword", "createApiToken", "rotateApiToken"} {
		if !recentMFAOperations[op] {
			t.Errorf("%s 必须在近期 MFA 清单内", op)
		}
	}
}

// 强制管理员启用 TOTP 时仍允许的 operation 只有启用流程与自助最小集；未知 operation 一律不在清单内（默认受限）。
func TestEnrollmentAllowedSet(t *testing.T) {
	for _, op := range []string{"getMe", "getCsrfToken", "logout", "setupTotp", "enableTotp", "changePassword"} {
		if !enrollmentAllowed[op] {
			t.Errorf("%s 应在启用流程允许清单内", op)
		}
	}
	for _, op := range []string{"disableTotp", "listSessions", "reauth", "createApiToken", "listMyLoginLogs", "revokeSession", "listSites"} {
		if enrollmentAllowed[op] {
			t.Errorf("%s 不应在启用流程允许清单内", op)
		}
	}
}

// reauthChecker：非本模块产出的主体（无法核对会话）一律视为未再认证；非 x-reauth 路由不检查。
func TestReauthCheckerFailsClosedForForeignPrincipal(t *testing.T) {
	c := reauthChecker{}
	r := httptest.NewRequest("POST", "/x", nil)
	if err := c.CheckReauth(r, httpx.RouteMeta{Reauth: false}, nil); err != nil {
		t.Errorf("非 x-reauth 路由不检查: %v", err)
	}
	if err := c.CheckReauth(r, httpx.RouteMeta{Reauth: true}, foreign{}); err == nil {
		t.Error("外来主体不能满足再认证")
	} else if e, ok := apierr.As(err); !ok || e.Code != apierr.AuthReauthRequired {
		t.Errorf("应 403 auth.reauth_required: %v", err)
	}
}

type foreign struct{}

func (foreign) ActorID() uint64           { return 1 }
func (foreign) IsPlatform() bool          { return true }
func (foreign) TenantID() uint64          { return 0 }
func (foreign) HasPermission(string) bool { return true }

// Bearer 可调用范围（Story 1.19/1.20）：有 x-permissions 的业务路由；其他模块「无权限要求、只返回自身租户上下文」的路由（GET /tenants/current）；
// 本模块的自助 operation（含 getMe）与令牌管理一律要求 Cookie 会话。
func TestBearerAllowedPolicy(t *testing.T) {
	for _, c := range []struct {
		name string
		meta httpx.RouteMeta
		want bool
	}{
		{"业务路由（有权限声明）", httpx.RouteMeta{OperationID: "listTasks", Permissions: []string{"task:read"}}, true},
		{"租户上下文（其他模块、无权限要求）", httpx.RouteMeta{OperationID: "getCurrentTenant", Permissions: []string{}}, true},
		{"自助 operation：getMe", MetaMe, false},
		{"自助 operation：changePassword", MetaChangePassword, false},
		{"自助 operation：listSessions", MetaListSessions, false},
		{"自助 operation：logout", MetaLogout, false},
		{"令牌管理：createApiToken", MetaCreateAPIToken, false},
		{"令牌管理：listApiTokens", MetaListAPITokens, false},
		{"管理路由（有权限声明）", MetaListUsers, true},
	} {
		if got := bearerAllowed(c.meta); got != c.want {
			t.Errorf("%s: bearerAllowed=%v，期望 %v", c.name, got, c.want)
		}
	}
}
