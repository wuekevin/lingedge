package httpx

import (
	"context"
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/rbac"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// 完整的租户 Scope 钩子（Story 1.20、ADR-009 §1/§4/§5、ADR-018 §10）。
//
// Scope 只由三样东西生成：已认证主体（认证钩子）、路由元数据（x-tenant-scope）与 X-Tenant-ID——而且 X-Tenant-ID 只在授权钩子已经
// 判定「该主体可以代该租户操作」（rbac.ModeActingAs：同时具备 tenant:impersonate 与路由权限）之后才被解析。这里不重复授权判定，
// 也不接受任何绕过授权钩子的路径：没有授权决策、决策与主体不一致、租户目录未装配，一律委托给内层钩子（缺省 DenyScope，403）。

// TenantStatusActive 是租户「可用」的状态：只有 active 的租户可被代操作（suspended/closed/pending 与不存在一样表现为 404）。
const TenantStatusActive = "active"

// ErrTenantNotFound 是 TenantResolver 对「不存在（或已软删除）的租户」返回的错误。
var ErrTenantNotFound = errors.New("httpx: 租户不存在")

// TenantRef 是租户目录对一个租户 uid 的解析结果。
type TenantRef struct {
	ID     uint64
	UID    string
	Status string
}

// TenantResolver 是租户目录：把请求里的租户 uid（X-Tenant-ID）解析为租户。实现属租户模块（internal/tenant）；
// 不存在或已删除返回 ErrTenantNotFound，其他错误表示基础设施故障（按 500 处理，fail-closed）。
type TenantResolver interface {
	ResolveTenant(ctx context.Context, tenantUID string) (TenantRef, error)
}

// ActingAs 是「平台管理员代租户操作」的审计上下文：Scope 钩子成功铸造 acting-as Scope 时登记，审计钩子（Story 1.21）经 ActingAsFrom 直接消费，
// 同时记录操作者（ActorID）与目标租户（TenantID/TenantUID），即 audit_logs 的 actor_user_id + tenant_id + acting_as（ADR-009 §5）。
type ActingAs struct {
	ActorID     uint64
	TenantID    uint64
	TenantUID   string
	OperationID string
}

type scopeCarrierKey struct{}
type actingAsKey struct{}

type scopeCarrier struct {
	set bool
	a   ActingAs
}

// RecordActingAs 由 Scope 钩子在成功铸造 acting-as Scope 后调用；对不经 Scope 中间件的请求是空操作。
func RecordActingAs(r *http.Request, a ActingAs) {
	if c, ok := r.Context().Value(scopeCarrierKey{}).(*scopeCarrier); ok && c != nil {
		c.a, c.set = a, true
	}
}

// ActingAsFrom 取出 acting-as 审计上下文；非代租户请求 ok=false。
func ActingAsFrom(ctx context.Context) (ActingAs, bool) {
	a, ok := ctx.Value(actingAsKey{}).(ActingAs)
	return a, ok
}

// WithTenantScope 用完整的租户 Scope 钩子装配 h.Scope（h 原有的 Scope 钩子作为内层，处理一切「没有被授权放行」的情形，缺省 DenyScope）。
// tenants 为 nil（未装配租户目录）时 acting-as 保持安全拒绝（403），其余三种模式照常。log 为 nil 时不记日志。
//
// 行为（授权决策来自 RecordAccess/AccessFrom）：
//   - ModeOwnTenant / ModePlatform / ModeCrossTenantRead：由主体与路由推导（tenantscope.FromPrincipal，无目标租户），并校验决策与主体种类一致；
//   - ModeActingAs：解析 X-Tenant-ID → 租户目录；不存在、已删除、状态非 active、目录返回的 uid 与请求不符，**一律 404 resource.not_found**
//     （响应逐字节与「他租户资源」一致，不泄漏存在性）；通过后 FromPrincipal(…, TargetTenant) 再次核对 tenant:impersonate，
//     登记 ActingAs 审计上下文并写结构化日志（audit_logs 待 Story 1.21：本钩子不写 audit_logs，也不假装已写）。
func WithTenantScope(h Hooks, tenants TenantResolver, log *zap.Logger) Hooks {
	h = h.withDefaults()
	if log == nil {
		log = zap.NewNop()
	}
	h.Scope = tenantScope{inner: h.Scope, tenants: tenants, log: log}
	return h
}

type tenantScope struct {
	inner   ScopeResolver
	tenants TenantResolver
	log     *zap.Logger
}

func (s tenantScope) ResolveScope(r *http.Request, meta RouteMeta, p tenantscope.Principal) (tenantscope.Scope, error) {
	d, ok := AccessFrom(r.Context())
	if !ok || d.Err != nil {
		return s.inner.ResolveScope(r, meta, p)
	}
	route, ok := meta.TenantScope.RouteScope()
	if !ok {
		return tenantscope.Scope{}, tenantscope.ErrInvalidScope
	}
	switch d.Mode {
	case rbac.ModeOwnTenant:
		if p.IsPlatform() { // 决策与主体不一致：fail-closed
			return tenantscope.Scope{}, tenantscope.ErrForbiddenRoute
		}
		return tenantscope.FromPrincipal(p, route, tenantscope.NoTarget())
	case rbac.ModePlatform, rbac.ModeCrossTenantRead:
		// ModePlatform 由 rbac.Decide 给 platform 路由，也由令牌管理的「自有资源」授权路径给 either 路由（平台用户管理自己的平台令牌）；
		// ModeCrossTenantRead 只属于 either 路由。两者都要求平台主体，且最终仍由 FromPrincipal 按（主体, 路由）推导（tenant 路由无目标 → 拒绝）。
		if !p.IsPlatform() || (d.Mode == rbac.ModeCrossTenantRead && route == tenantscope.RoutePlatform) {
			return tenantscope.Scope{}, tenantscope.ErrForbiddenRoute
		}
		return tenantscope.FromPrincipal(p, route, tenantscope.NoTarget())
	case rbac.ModeActingAs:
		if s.tenants == nil || !p.IsPlatform() || route == tenantscope.RoutePlatform {
			return s.inner.ResolveScope(r, meta, p)
		}
		return s.actingAs(r, meta, p, route)
	}
	return s.inner.ResolveScope(r, meta, p)
}

func (s tenantScope) actingAs(r *http.Request, meta RouteMeta, p tenantscope.Principal, route tenantscope.RouteScope) (tenantscope.Scope, error) {
	vals := r.Header.Values("X-Tenant-ID")
	if len(vals) != 1 || !uid.Valid(vals[0]) {
		return tenantscope.Scope{}, tenantscope.ErrInvalidScope
	}
	ref, err := s.tenants.ResolveTenant(r.Context(), vals[0])
	switch {
	case errors.Is(err, ErrTenantNotFound):
		return tenantscope.Scope{}, tenantscope.ErrTenantMismatch // → 404，与他租户/不存在同形
	case err != nil:
		return tenantscope.Scope{}, apierr.Internal(err)
	case ref.ID == 0 || ref.UID != vals[0] || ref.Status != TenantStatusActive:
		return tenantscope.Scope{}, tenantscope.ErrTenantMismatch
	}
	sc, err := tenantscope.FromPrincipal(p, route, tenantscope.TargetTenant(ref.ID))
	if err != nil {
		return tenantscope.Scope{}, err
	}
	a := ActingAs{ActorID: p.ActorID(), TenantID: ref.ID, TenantUID: ref.UID, OperationID: meta.OperationID}
	RecordActingAs(r, a)
	reqID, _ := requestid.FromContext(r.Context())
	s.log.Info("管理员代租户操作（结构化日志；audit_logs 待 Story 1.21 写入 actor+目标租户+acting_as）",
		zap.String("operation_id", meta.OperationID), zap.Uint64("actor_id", a.ActorID), zap.Uint64("acting_as_tenant_id", a.TenantID),
		zap.String("acting_as_tenant_uid", a.TenantUID), zap.String("request_id", reqID), zap.Bool("audit_logs_written", false))
	return sc, nil
}
