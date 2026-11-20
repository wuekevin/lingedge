package httpx

import (
	"context"
	"net/http"

	"github.com/wuekevin/lingedge/internal/platform/rbac"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// 授权决策的传递（Story 1.18 → 1.20）：授权钩子（RBAC）在判定通过后用 RecordAccess 登记 rbac.Decision（访问模式：
// 自己的租户 / 平台范围 / 跨租户只读 / acting-as），授权中间件把它放进请求上下文，Scope 钩子经 AccessFrom 读取。
// 这样「谁可以以什么方式访问」只在授权钩子里判定一次，Scope 钩子（Story 1.20）只负责把决策落成 tenantscope.Scope
// （含解析 X-Tenant-ID、校验目标租户存在），不重复也不绕过授权。

type accessKey int

const (
	accessDecisionKey accessKey = iota + 1
	accessCarrierKey
)

type accessCarrier struct {
	set bool
	d   rbac.Decision
}

// RecordAccess 由授权钩子在放行时调用，登记授权决策；对不经授权中间件的请求是空操作。
func RecordAccess(r *http.Request, d rbac.Decision) {
	if c, ok := r.Context().Value(accessCarrierKey).(*accessCarrier); ok && c != nil && d.Err == nil {
		c.d, c.set = d, true
	}
}

// AccessFrom 取出授权钩子登记的授权决策（Scope 钩子读取）；没有登记（例如自助 operation 走最小授权）时 ok=false。
func AccessFrom(ctx context.Context) (rbac.Decision, bool) {
	d, ok := ctx.Value(accessDecisionKey).(rbac.Decision)
	return d, ok
}

func withAccessCarrier(ctx context.Context, c *accessCarrier) context.Context {
	return context.WithValue(ctx, accessCarrierKey, c)
}

func withAccess(ctx context.Context, d rbac.Decision) context.Context {
	return context.WithValue(ctx, accessDecisionKey, d)
}

// WithAccessScope 装配「按授权决策生成 Scope」的过渡 Scope 钩子（Story 1.18；完整实现归 Story 1.20）。
//
// 边界（写明，以免被当作 1.20 的实现）：
//   - 只处理授权钩子已放行且「不需要解析 X-Tenant-ID」的决策：ModeOwnTenant（租户主体、自己的租户）、ModePlatform（平台级路由）、
//     ModeCrossTenantRead（平台主体不带 X-Tenant-ID 的跨租户只读）——这三种 Scope 完全由主体与路由推导（tenantscope.FromPrincipal，目标为空）。
//   - ModeActingAs（带 X-Tenant-ID 代租户）需要把 X-Tenant-ID（租户 uid）解析并校验为存在且可用的租户——那是 Story 1.20 的职责；
//     在此一律委托给 h 原有的 Scope 钩子（缺省为 DenyScope，即 403），不放行未实现的部分。
//   - 没有授权决策时（例如公开路由、自助 operation 由 WithSelfService 先行处理）同样委托给 h 原有的 Scope 钩子。
func WithAccessScope(h Hooks) Hooks {
	h = h.withDefaults()
	h.Scope = accessScope{inner: h.Scope}
	return h
}

type accessScope struct{ inner ScopeResolver }

func (s accessScope) ResolveScope(r *http.Request, meta RouteMeta, p tenantscope.Principal) (tenantscope.Scope, error) {
	d, ok := AccessFrom(r.Context())
	if !ok || d.Err != nil {
		return s.inner.ResolveScope(r, meta, p)
	}
	switch d.Mode {
	case rbac.ModeOwnTenant, rbac.ModePlatform, rbac.ModeCrossTenantRead:
		route, ok := meta.TenantScope.RouteScope()
		if !ok {
			return tenantscope.Scope{}, tenantscope.ErrInvalidScope
		}
		return tenantscope.FromPrincipal(p, route, tenantscope.NoTarget())
	}
	return s.inner.ResolveScope(r, meta, p)
}
