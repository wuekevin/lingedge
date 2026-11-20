package httpx

import (
	"net/http"

	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// WithSelfService 给「自助」operation 授予最小授权语义（Story 1.16）：对 isSelf(operationID) 为真且 Auth=required 的路由，
// 授权钩子放行、Scope 钩子只从主体自身推导（忽略 X-Tenant-ID，不做代租户）、审计钩子不再拒绝；其他一切路由原样委托给 h 的各钩子
// （h 中为 nil 的项即 Deny* 安全拒绝默认）。
//
// 为什么需要：授权（1.18）/Scope（1.20）/审计（1.21）钩子尚未实现而默认拒绝，但 /auth/me、/auth/sessions、登出、改密等
// 是「已认证用户操作自己」的 operation——openapi 里 x-permissions 为空、没有可校验的权限串，handler 只用主体自身的标识查询
// （user_id 来自会话，不来自请求参数）。它们必须在 1.18 之前就能用，否则登录之后什么都做不了。
//
// 约束：isSelf 必须是代码可核对的闭集（认证模块的路由元数据：权限为空且不要求再认证），不依赖请求里的任何输入；
// 公开路由（login，Audit=true）只放行审计钩子（其留痕由认证模块写入 login_logs）。1.18/1.20/1.21 交付真实钩子后应评估是否保留这层包装。
func WithSelfService(h Hooks, isSelf func(operationID string) bool) Hooks {
	h = h.withDefaults()
	return Hooks{
		Authenticator: h.Authenticator,
		Authorizer:    selfAuthorizer{inner: h.Authorizer, isSelf: isSelf},
		Scope:         selfScope{inner: h.Scope, isSelf: isSelf},
		Auditor:       selfAuditor{inner: h.Auditor, isSelf: isSelf},
	}
}

type selfAuthorizer struct {
	inner  Authorizer
	isSelf func(string) bool
}

func (a selfAuthorizer) Authorize(r *http.Request, meta RouteMeta, p tenantscope.Principal) error {
	if meta.Auth == AuthRequired && len(meta.Permissions) == 0 && !meta.Reauth && a.isSelf(meta.OperationID) {
		return nil
	}
	return a.inner.Authorize(r, meta, p)
}

type selfScope struct {
	inner  ScopeResolver
	isSelf func(string) bool
}

func (s selfScope) ResolveScope(r *http.Request, meta RouteMeta, p tenantscope.Principal) (tenantscope.Scope, error) {
	if meta.Auth == AuthRequired && len(meta.Permissions) == 0 && !meta.Reauth && s.isSelf(meta.OperationID) {
		return tenantscope.FromPrincipal(p, tenantscope.RouteEither, tenantscope.NoTarget())
	}
	return s.inner.ResolveScope(r, meta, p)
}

type selfAuditor struct {
	inner  Auditor
	isSelf func(string) bool
}

func (a selfAuditor) Begin(r *http.Request, meta RouteMeta) (func(AuditOutcome), error) {
	if len(meta.Permissions) == 0 && !meta.Reauth && a.isSelf(meta.OperationID) {
		return nil, nil
	}
	return a.inner.Begin(r, meta)
}
