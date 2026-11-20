package httpx

import (
	"context"
	"net/http"

	"github.com/wuekevin/lingedge/internal/platform/rbac"
)

// 授权决策的传递（Story 1.18 → 1.20）：授权钩子（RBAC）在判定通过后用 RecordAccess 登记 rbac.Decision（访问模式：
// 自己的租户 / 平台范围 / 跨租户只读 / acting-as），授权中间件把它放进请求上下文，Scope 钩子经 AccessFrom 读取。
// 这样「谁可以以什么方式访问」只在授权钩子里判定一次，Scope 钩子（Story 1.20，scopehook.go 的 WithTenantScope）只负责把决策落成 tenantscope.Scope
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
