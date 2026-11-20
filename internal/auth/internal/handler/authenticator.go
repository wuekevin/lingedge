// Package handler 是认证模块的 HTTP 层（Story 1.16）：把 httpx 的认证钩子实现为真实的会话认证，并提供 /auth/* 路由。
package handler

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/auth/internal/service"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// Authenticator 实现 httpx.Authenticator：Cookie 会话（realm 隔离、滑动+绝对过期、CSRF）与 Bearer API Token（Story 1.19，bearer.go 的 bearerAuthenticator）。
type Authenticator struct {
	svc *service.Service
	// origins 是除「与请求同源」之外额外允许的 Origin（精确匹配）。
	origins map[string]bool
	// bearer 是 API Token 认证（Story 1.19）；nil 时携带 Bearer 的请求一律 401（fail-closed；仅用于单元测试钩子策略）。
	bearer httpx.Authenticator
}

var _ httpx.Authenticator = (*Authenticator)(nil)

// NewAuthenticator 创建认证钩子。
func NewAuthenticator(svc *service.Service, extraOrigins []string, bearer httpx.Authenticator) *Authenticator {
	o := map[string]bool{}
	for _, v := range extraOrigins {
		o[strings.ToLower(v)] = true
	}
	return &Authenticator{svc: svc, origins: o, bearer: bearer}
}

// mustChangeAllowed 是 must_change_password 状态下仍允许调用的 operation（其余一律拒绝，直到改密）。
var mustChangeAllowed = map[string]bool{"getMe": true, "getCsrfToken": true, "logout": true, "changePassword": true}

// enrollmentAllowed 是「强制管理员启用 TOTP」生效且账号尚未启用时仍允许调用的 operation：只能走启用流程（以及读取自身、登出、改密）。
var enrollmentAllowed = map[string]bool{"getMe": true, "getCsrfToken": true, "logout": true, "setupTotp": true, "enableTotp": true, "changePassword": true}

// recentMFAOperations 是要求「近期 MFA」的敏感 operation（ADR-018 §5：改密码、创建 API Token…）。createApiToken 属 Story 1.19，
// 路由注册后即由本钩子统一生效（Bearer 请求没有会话，无法满足近期 MFA）。rotateApiToken 同样产生新的凭据，同口径要求近期 MFA。
var recentMFAOperations = map[string]bool{"changePassword": true, "createApiToken": true, "rotateApiToken": true}

// realmsFor 决定一条路由接受哪些 realm 的会话（按优先顺序）。
// platform 路由只认 admin；tenant 路由认 console，也认 admin（平台管理员经 X-Tenant-ID 代租户，授权由 1.18/1.20 判定）；either 两者皆可。
// 因此 console 会话访问 platform 路由、或把一个 realm 的令牌放进另一个 realm 的 Cookie，都得到 401（realm 隔离，ADR-018 §1）。
func realmsFor(s httpx.TenantScope) []domain.Realm {
	switch s {
	case httpx.ScopePlatform:
		return []domain.Realm{domain.RealmAdmin}
	case httpx.ScopeTenant:
		return []domain.Realm{domain.RealmConsole, domain.RealmAdmin}
	}
	return []domain.Realm{domain.RealmAdmin, domain.RealmConsole}
}

// Authenticate 实现 httpx.Authenticator。
func (a *Authenticator) Authenticate(r *http.Request, meta httpx.RouteMeta) (tenantscope.Principal, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		if scheme, _, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "Bearer") {
			if a.bearer == nil {
				return nil, apierr.New(apierr.AuthUnauthenticated, "authentication required")
			}
			p, err := a.bearer.Authenticate(r, meta) // Bearer 不使用 Cookie，不需要 CSRF（NFR-9）
			if err != nil {
				return nil, err
			}
			// Bearer 没有会话，无法满足「近期 MFA」（fail-closed）；再认证同理，由 httpx.WithReauth 的检查组件拒绝。
			if recentMFAOperations[meta.OperationID] {
				return nil, apierr.New(apierr.AuthReauthRequired, "recent MFA required", apierr.Detail{Field: "mfa_verified_at", Reason: "API tokens cannot satisfy recent MFA; use a cookie session"})
			}
			// 令牌只能调用业务路由（x-permissions 非空且不是令牌管理）：自助 operation 与令牌管理要求 Cookie 会话。
			if !bearerAllowed(meta) {
				return nil, apierr.New(apierr.AuthForbidden, "forbidden", apierr.Detail{Field: "Authorization", Reason: "API tokens cannot call this operation; use a cookie session"})
			}
			return p, nil
		}
	}
	var lastErr error
	for _, realm := range realmsFor(meta.TenantScope) {
		ck, err := r.Cookie(realm.CookieName())
		if err != nil || ck.Value == "" {
			continue
		}
		p, err := a.svc.Authenticate(r.Context(), realm, ck.Value)
		if err != nil {
			lastErr = err
			continue
		}
		if err := a.checkCSRF(r, p); err != nil {
			return nil, err
		}
		if p.User.MustChange && !mustChangeAllowed[meta.OperationID] {
			return nil, apierr.New(apierr.AuthForbidden, "password change required", apierr.Detail{Field: "must_change_password", Reason: "change the password before using other operations"})
		}
		if !enrollmentAllowed[meta.OperationID] && a.svc.EnrollmentRequired(r.Context(), p) {
			return nil, apierr.New(apierr.AuthForbidden, "totp enrollment required", apierr.Detail{Field: "totp_required_for_admin", Reason: "enable TOTP (setup, then enable) before using other operations"})
		}
		if recentMFAOperations[meta.OperationID] {
			if err := a.svc.CheckRecentMFA(r.Context(), p); err != nil {
				return nil, err
			}
		}
		return p, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, apierr.New(apierr.AuthUnauthenticated, "authentication required")
}

// checkCSRF 对 Cookie 会话的写请求要求：Sec-Fetch-Site/Origin 不得指向别的源，且 X-CSRF-Token 与会话的 csrf_hash 一致（ADR-018 §3）。
func (a *Authenticator) checkCSRF(r *http.Request, p *domain.Principal) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	bad := func(reason string) error {
		return apierr.New(apierr.AuthCsrfInvalid, "csrf check failed", apierr.Detail{Field: "X-CSRF-Token", Reason: reason})
	}
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" {
		return bad("cross-site request not allowed")
	}
	if o := r.Header.Get("Origin"); o != "" && !a.originAllowed(r, o) {
		return bad("origin not allowed")
	}
	if !service.CSRFMatches(r.Header.Get("X-CSRF-Token"), p.Session.CSRFHash) {
		return bad("missing or invalid token")
	}
	return nil
}

// originAllowed：Origin 的主机（含端口）与请求 Host 相同，或在额外白名单内。
// 部署要求：反向代理必须保留原始 Host（proxy_set_header Host $host），否则同源请求会被误判为跨源（拒绝，fail-closed）。
func (a *Authenticator) originAllowed(r *http.Request, origin string) bool {
	if a.origins[strings.ToLower(origin)] {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.User != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
