package httpx

import (
	"net/http"

	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// ReauthChecker 是 L4 再认证检查组件（ADR-018 §12、Story 1.17）：对声明 x-reauth: true 的 operation，核对主体的会话是否在
// auth.reauth_window_s 内通过了 POST /auth/reauth；不满足返回 403 auth.reauth_required。Bearer API Token 请求没有会话，永远不能满足。
// 这是一个可复用的组件：Story 1.18 的真实授权钩子可以直接持有它（见 WithReauth 的顺序说明）。
type ReauthChecker interface {
	CheckReauth(r *http.Request, meta RouteMeta, p tenantscope.Principal) error
}

// WithReauth 在授权链里统一执行再认证检查：对 meta.Reauth 为真的路由，先运行 checker，通过后才委托给 h 的授权钩子；
// 其他路由原样委托。检查失败即中止（不会调用内层授权钩子，handler 更不会执行）。
//
// 顺序说明：此处「再认证先于授权」。在 1.18 之前授权钩子是安全拒绝默认，这个顺序让再认证窗口在现有链中真实生效且可被验证
// （窗口外 403 auth.reauth_required；窗口内继续走授权，当前仍为 403 auth.forbidden）。1.18 交付真实授权钩子后，可把
// 「权限判定 → 再认证」的顺序改为先权限后再认证（避免向无权限者提示再认证），只需把 checker 移入授权钩子内部。
func WithReauth(h Hooks, c ReauthChecker) Hooks {
	h = h.withDefaults()
	h.Authorizer = reauthAuthorizer{inner: h.Authorizer, check: c}
	return h
}

type reauthAuthorizer struct {
	inner Authorizer
	check ReauthChecker
}

func (a reauthAuthorizer) Authorize(r *http.Request, meta RouteMeta, p tenantscope.Principal) error {
	if meta.Reauth {
		if a.check == nil { // 未装配检查组件：fail-closed
			return DenyAuthorizer{}.Authorize(r, meta, p)
		}
		if err := a.check.CheckReauth(r, meta, p); err != nil {
			return err
		}
	}
	return a.inner.Authorize(r, meta, p)
}
