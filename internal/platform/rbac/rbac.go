// Package rbac 是 RBAC 的平台级基础：权限注册表（代码常量）与授权决策函数（Story 1.18、ADR-018 §8–§10、IMP-12）。
//
// 权限注册表与种子迁移（migrations/000004）的 permissions 行、docs/api/openapi.yaml 的 x-permissions 三方一致，
// 由 `go run ./scripts/gates perms`（make perms-check，已接入 make contract-check）与 tests/integration 核对：
// OpenAPI 引用的权限串必须存在于注册表且已种子；路由的权限 scope 必须与路由的 x-tenant-scope 相容。
//
// Decide 是纯函数：不读库、不读时钟，只据（路由元数据、主体类型、主体权限集、是否带 X-Tenant-ID）产出授权决策，
// 因此授权矩阵可以表驱动穷举测试。决策结果（Decision.Mode）由 httpx 的授权中间件交给后续的 Scope 钩子（Story 1.20）。
package rbac

import (
	"errors"
	"sort"

	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// Scope 是权限点的适用范围（permissions.scope）。
type Scope string

// 权限 scope 取值。
const (
	ScopePlatform Scope = "platform" // 仅平台角色可持有
	ScopeTenant   Scope = "tenant"   // 仅租户角色可持有（平台主体经 acting-as 使用）
	ScopeBoth     Scope = "both"     // 平台与租户角色均可持有
)

// Permission 是权限点字典中的一项。
type Permission struct {
	Code        string
	Resource    string
	Action      string
	Scope       Scope
	Module      string
	Description string
}

// Impersonate 是代租户（acting-as）的前置权限（ADR-018 §10；scope=platform）。
const Impersonate = tenantscope.PermImpersonate

var byCode = func() map[string]Permission {
	m := make(map[string]Permission, len(registry))
	for _, p := range registry {
		m[p.Code] = p
	}
	return m
}()

// All 返回注册表副本（按权限串升序）。
func All() []Permission {
	out := make([]Permission, len(registry))
	copy(out, registry)
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// Lookup 按权限串查注册表。
func Lookup(code string) (Permission, bool) {
	p, ok := byCode[code]
	return p, ok
}

// Known 报告权限串是否在注册表内。
func Known(code string) bool { _, ok := byCode[code]; return ok }

// GrantableToTenantRole 报告权限能否授予租户角色（scope 为 tenant 或 both；platform 权限永不进入租户角色）。
func GrantableToTenantRole(code string) bool {
	p, ok := byCode[code]
	return ok && (p.Scope == ScopeTenant || p.Scope == ScopeBoth)
}

// GrantableToPlatformRole 报告权限能否授予平台角色（注册表内的任意权限；平台角色可经 acting-as 使用 tenant/both 权限）。
func GrantableToPlatformRole(code string) bool { return Known(code) }

// Mode 是授权决策的访问模式，交给后续钩子（Scope 钩子，Story 1.20）据此生成 tenantscope.Scope。
type Mode string

// 访问模式。
const (
	// ModeOwnTenant：租户主体访问自己的租户（路由 tenant/either）。
	ModeOwnTenant Mode = "own_tenant"
	// ModePlatform：平台主体访问平台级路由。
	ModePlatform Mode = "platform"
	// ModeCrossTenantRead：平台主体不带 X-Tenant-ID 访问 either 路由——跨租户只读。
	ModeCrossTenantRead Mode = "cross_tenant_read"
	// ModeActingAs：平台主体带 X-Tenant-ID 代租户操作（须同时具备 tenant:impersonate 与路由权限）。
	// 目标租户是否存在/可用由 Scope 钩子（Story 1.20）验证，本决策只表示「授权通过」。
	ModeActingAs Mode = "acting_as"
)

// ErrMissingPermission 表示主体缺少路由声明的权限（映射为 403 auth.forbidden）。
var ErrMissingPermission = errors.New("rbac: 主体缺少路由声明的权限")

// Input 是授权决策的输入。
type Input struct {
	// Route 是路由的 x-tenant-scope（platform | tenant | either；公开路由不经授权）。
	Route tenantscope.RouteScope
	// Required 是路由的 x-permissions：须全部具备（空表示无权限要求）。
	Required []string
	// Platform 报告主体是否为平台主体（否则为租户主体）。
	Platform bool
	// Has 报告主体是否持有某权限串（平台主体：平台角色授予的权限；租户主体：其租户内角色授予的权限）。
	Has func(code string) bool
	// ReadOnly 报告请求方法是否只读（GET/HEAD/OPTIONS）。
	ReadOnly bool
	// HasTarget 报告请求是否带 X-Tenant-ID。
	HasTarget bool
	// TargetIsOwn 仅对租户主体有意义：X-Tenant-ID 是否就是主体自己的租户。
	TargetIsOwn bool
}

// Decision 是授权决策。Err 非 nil 表示拒绝，且是 tenantscope 的哨兵错误或 ErrMissingPermission
// （httpx.MapScopeError 把前者映射为 403/404/400，ErrMissingPermission 为 403）。
type Decision struct {
	Mode Mode
	Err  error
}

// Decide 实现 ADR-018 §10 的授权矩阵。顺序固定：先判定路由类型与目标租户对该主体是否合法，再判定权限，最后判定形态：
//
//	租户主体：platform 路由 → 403；X-Tenant-ID 指向他人租户 → 404（不泄漏存在性）；缺权限 → 403；否则 ModeOwnTenant。
//	平台主体 + platform 路由：缺权限 → 403；带 X-Tenant-ID → 400（平台级资源不接受目标租户）；否则 ModePlatform。
//	平台主体 + either/tenant 路由 + 不带 X-Tenant-ID：缺权限 → 403；tenant 路由、或 either 路由的写操作 → 400（须经 acting-as 指定租户）；
//	  否则 ModeCrossTenantRead（只读）。
//	平台主体 + either/tenant 路由 + 带 X-Tenant-ID：缺 tenant:impersonate → 403；路由权限须全部具备且 scope 为 tenant 或 both → 否则 403；
//	  通过则 ModeActingAs。
func Decide(in Input) Decision {
	deny := func(err error) Decision { return Decision{Err: err} }
	if in.Has == nil {
		return deny(ErrMissingPermission) // fail-closed
	}
	switch in.Route {
	case tenantscope.RoutePlatform, tenantscope.RouteTenant, tenantscope.RouteEither:
	default:
		return deny(tenantscope.ErrInvalidScope)
	}
	hasAll := func() bool {
		for _, p := range in.Required {
			if !in.Has(p) {
				return false
			}
		}
		return true
	}

	if !in.Platform {
		if in.Route == tenantscope.RoutePlatform {
			return deny(tenantscope.ErrForbiddenRoute)
		}
		if in.HasTarget && !in.TargetIsOwn {
			return deny(tenantscope.ErrTenantMismatch)
		}
		if !hasAll() {
			return deny(ErrMissingPermission)
		}
		return Decision{Mode: ModeOwnTenant}
	}

	if in.Route == tenantscope.RoutePlatform {
		if !hasAll() {
			return deny(ErrMissingPermission)
		}
		if in.HasTarget {
			return deny(tenantscope.ErrInvalidScope)
		}
		return Decision{Mode: ModePlatform}
	}

	if !in.HasTarget {
		if !hasAll() {
			return deny(ErrMissingPermission)
		}
		if in.Route == tenantscope.RouteTenant || !in.ReadOnly {
			return deny(tenantscope.ErrTenantRequired)
		}
		return Decision{Mode: ModeCrossTenantRead}
	}

	if !in.Has(Impersonate) {
		return deny(tenantscope.ErrImpersonationDenied)
	}
	for _, p := range in.Required {
		if !in.Has(p) || !GrantableToTenantRole(p) {
			return deny(ErrMissingPermission)
		}
	}
	return Decision{Mode: ModeActingAs}
}
