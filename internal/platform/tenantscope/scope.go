// Package tenantscope 定义租户 Scope 及其唯一的构造入口（Story 1.5 AC2、ADR-009 §1/§5/§8）。
//
// Scope 的四个字段均为未导出：包外代码无法用复合字面量、赋值或反射之外的任何方式写出非零 Scope
// （编译期失败，见 TestForgingScopeOutsidePackageFailsToCompile）。唯一的构造函数是本包的
// FromPrincipal（请求路径：已认证主体 + 路由元数据 + X-Tenant-ID）、FromTask（后台任务：以任务记录的 tenant_id）
// 与 PlatformForJob（登记在册的跨租户批处理）。这些函数只允许被少数白名单包调用（forbidigo 规则 [tenantscope-mint]，
// 见 scripts/gates/genlint.go），Handler 与 Service 只能经 FromContext 取得中间件放入 context 的 Scope。
//
// 零值 Scope{} 表示「没有 Scope」：任何查询构建器遇到它一律失败（ErrNoScope），不会退化为无租户条件的查询。
package tenantscope

import (
	"errors"
	"fmt"
	"reflect"
)

// isNilPrincipal 识别 nil 接口与「持有 nil 指针的接口」（后者调用方法会 panic）。
func isNilPrincipal(p Principal) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// Scope 是一次数据访问的租户范围。
//
// 不变量（由构造函数保证）：
//   - 租户范围：TenantID > 0，Platform=false；ActingAs=true 表示平台管理员代该租户操作（ADR-009 §5，审计须同时记录操作者与目标租户）；
//   - 平台范围：Platform=true，TenantID=0，ActingAs=false；只能经名称含 AcrossTenants 的方法使用（ADR-009 §2）。
type Scope struct {
	tenantID uint64
	platform bool
	actorID  uint64
	actingAs bool
}

// TenantID 返回被限定的租户（平台范围返回 0）。
func (s Scope) TenantID() uint64 { return s.tenantID }

// IsPlatform 报告是否为平台范围（跨租户）。
func (s Scope) IsPlatform() bool { return s.platform }

// ActorID 返回操作者（用户 id；系统任务为 0）。
func (s Scope) ActorID() uint64 { return s.actorID }

// ActingAs 报告是否为管理员代租户操作。
func (s Scope) ActingAs() bool { return s.actingAs }

// IsZero 报告是否为零值（没有 Scope）。
func (s Scope) IsZero() bool { return s == Scope{} }

// String 供日志使用：只含数字与布尔，不含任何敏感信息。
func (s Scope) String() string {
	switch {
	case s.IsZero():
		return "scope{none}"
	case s.platform:
		return fmt.Sprintf("scope{platform actor=%d}", s.actorID)
	default:
		return fmt.Sprintf("scope{tenant=%d actor=%d acting_as=%t}", s.tenantID, s.actorID, s.actingAs)
	}
}

// 构造失败的原因。上层应把 ErrTenantMismatch/ErrForbiddenRoute 表现为 404（不区分「不存在」与「无权」，ADR-009 §4），
// ErrImpersonationDenied 表现为 403。
var (
	ErrNoPrincipal         = errors.New("tenantscope: 缺少已认证主体")
	ErrForbiddenRoute      = errors.New("tenantscope: 该主体不得访问此路由的租户范围")
	ErrTenantRequired      = errors.New("tenantscope: 平台主体访问租户资源必须指定目标租户（X-Tenant-ID）")
	ErrTenantMismatch      = errors.New("tenantscope: 指定的租户与主体所属租户不一致")
	ErrImpersonationDenied = errors.New("tenantscope: 缺少 tenant:impersonate 权限，不得代租户操作")
	ErrInvalidScope        = errors.New("tenantscope: 无效的 Scope 参数")
)

// RouteScope 是路由元数据 x-tenant-scope（ADR-009 §5、ADR-017）。
type RouteScope string

const (
	// RouteTenant：租户资源。租户主体限定自己的租户；平台主体必须指定目标租户（代租户）。
	RouteTenant RouteScope = "tenant"
	// RouteEither：租户主体限定自己的租户；平台主体不带目标租户 → 跨租户只读，带目标租户 → 代租户。
	RouteEither RouteScope = "either"
	// RoutePlatform：平台级资源，租户主体不得访问。
	RoutePlatform RouteScope = "platform"
)

// PermImpersonate 是代租户操作所需的权限串（scope=platform）。
const PermImpersonate = "tenant:impersonate"

// Principal 是已认证主体的最小投影；由认证中间件（Story 1.16/1.20）的主体类型实现。
// 本包只读取这四项，不依赖 auth 模块（platform 不得依赖业务模块）。
type Principal interface {
	// ActorID 是操作者用户 id（API Token 主体取其拥有者）。
	ActorID() uint64
	// IsPlatform 报告是否为平台主体（Admin/平台角色）；否则为租户主体。
	IsPlatform() bool
	// TenantID 是租户主体所属租户（>0）；平台主体返回 0。
	TenantID() uint64
	// HasPermission 报告主体是否具备权限串（路由 x-permissions 的校验由中间件完成，这里只用于 tenant:impersonate）。
	HasPermission(perm string) bool
}

// Target 是请求指定的目标租户（X-Tenant-ID 已由中间件解析为租户 id）。
type Target struct {
	present  bool
	tenantID uint64
}

// NoTarget 表示请求未指定目标租户（函数而非变量：包级变量可被任意包重新赋值而改变所有调用方的语义）。
func NoTarget() Target { return Target{} }

// TargetTenant 表示请求指定了目标租户 id（须 >0，否则按「未指定」之外的无效参数处理，FromPrincipal 返回 ErrInvalidScope）。
func TargetTenant(id uint64) Target { return Target{present: true, tenantID: id} }

// FromPrincipal 从已认证主体、路由元数据与目标租户生成请求路径上的 Scope（ADR-009 §1、§5）。
func FromPrincipal(p Principal, route RouteScope, target Target) (Scope, error) {
	if isNilPrincipal(p) {
		return Scope{}, ErrNoPrincipal
	}
	actor := p.ActorID()
	if actor == 0 {
		return Scope{}, ErrInvalidScope // 审计必须能归因到操作者（ADR-009 §5）；系统任务用 FromTask/PlatformForJob
	}
	if target.present && target.tenantID == 0 {
		return Scope{}, ErrInvalidScope
	}
	switch route {
	case RouteTenant, RouteEither, RoutePlatform:
	default:
		return Scope{}, ErrInvalidScope
	}

	if !p.IsPlatform() {
		tid := p.TenantID()
		if tid == 0 {
			return Scope{}, ErrInvalidScope
		}
		if route == RoutePlatform {
			return Scope{}, ErrForbiddenRoute
		}
		if target.present && target.tenantID != tid {
			return Scope{}, ErrTenantMismatch
		}
		return Scope{tenantID: tid, actorID: actor}, nil
	}

	// 平台主体。
	switch {
	case route == RoutePlatform:
		if target.present {
			return Scope{}, ErrInvalidScope // 平台级资源不接受目标租户
		}
		return Scope{platform: true, actorID: actor}, nil
	case !target.present && route == RouteEither:
		return Scope{platform: true, actorID: actor}, nil // 跨租户只读（读取限制由查询构建器的 AcrossTenants 方法族保证）
	case !target.present:
		return Scope{}, ErrTenantRequired
	}
	if !p.HasPermission(PermImpersonate) {
		return Scope{}, ErrImpersonationDenied
	}
	return Scope{tenantID: target.tenantID, actorID: actor, actingAs: true}, nil
}

// FromTask 以任务记录的 tenant_id 构造租户范围 Scope（ADR-009 §8）；tenantID 必须 >0。
func FromTask(tenantID, actorID uint64) (Scope, error) {
	if tenantID == 0 {
		return Scope{}, ErrInvalidScope
	}
	return Scope{tenantID: tenantID, actorID: actorID}, nil
}

// PlatformForJob 构造平台范围 Scope，仅用于跨租户批处理（保留清理、rollup 聚合等，ADR-009 §8）。
// 调用点由 [tenantscope-mint] 规则限定在白名单路径内（路径级，非逐调用点登记）；新增调用点须评审，并在 Story 1.14/1.15 的作业清单中登记。
func PlatformForJob(actorID uint64) Scope {
	return Scope{platform: true, actorID: actorID}
}
