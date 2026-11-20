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

// WithReauth 在授权链里统一执行再认证检查：先运行 h 的授权钩子（权限判定），**通过之后**才对 meta.Reauth 为真的路由运行 checker；
// 其他路由原样委托。任一环节失败即中止（handler 不会执行）。
//
// 顺序说明（Story 1.18 修订 1.17 的遗留）：先权限后再认证——无权限者得到 403 auth.forbidden，不会被提示「去再认证」
// 却在再认证之后仍然被拒（泄漏「该操作存在且需要再认证」、浪费一次口令/TOTP 校验与节流计数）。
// 授权钩子为安全拒绝默认（未装配）时，再认证检查同样不会被放行：权限判定先失败。
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
	if err := a.inner.Authorize(r, meta, p); err != nil {
		return err
	}
	if meta.Reauth {
		if a.check == nil { // 未装配检查组件：fail-closed
			return DenyAuthorizer{}.Authorize(r, meta, p)
		}
		return a.check.CheckReauth(r, meta, p)
	}
	return nil
}
