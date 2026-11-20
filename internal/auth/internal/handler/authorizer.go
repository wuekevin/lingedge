package handler

import (
	"errors"
	"net/http"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/auth/internal/service"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/rbac"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// Authorizer 是 RBAC 授权钩子（Story 1.18、ADR-018 §8–§10）：装载主体的有效权限集（缓存见 service/perms.go），
// 按 rbac.Decide 的授权矩阵判定，放行时用 httpx.RecordAccess 把授权决策交给后续的 Scope 钩子。
//
// 边界：本钩子只回答「这个主体能不能以这种方式访问这条路由」，不解析 X-Tenant-ID 指向的租户是否存在/可用——那是 Scope 钩子
// （Story 1.20）的职责；因此 acting-as 即使在这里被授权，Scope 钩子在 1.20 之前仍是安全拒绝（见 httpx.WithAccessScope）。
// x-reauth 不在这里判定：它由 httpx.WithReauth 在本钩子通过之后执行（先权限后再认证）。
type Authorizer struct{ svc *service.Service }

var _ httpx.Authorizer = (*Authorizer)(nil)

// NewAuthorizer 创建授权钩子。
func NewAuthorizer(svc *service.Service) *Authorizer { return &Authorizer{svc: svc} }

// Authorize 实现 httpx.Authorizer。不是本模块产出的主体（无法装载权限集）一律拒绝（fail-closed）。
func (a *Authorizer) Authorize(r *http.Request, meta httpx.RouteMeta, p tenantscope.Principal) error {
	dp, ok := p.(*domain.Principal)
	if !ok || dp == nil {
		return apierr.New(apierr.AuthForbidden, "forbidden")
	}
	if IsTokenOperation(meta.OperationID) {
		return a.authorizeOwnToken(r, meta, dp)
	}
	route, ok := meta.TenantScope.RouteScope()
	if !ok {
		return apierr.New(apierr.AuthForbidden, "forbidden") // 公开路由不经授权；其他取值是装配错误
	}
	target, present, err := targetTenant(r)
	if err != nil {
		return err
	}
	perms, err := a.svc.LoadPermissions(r.Context(), dp)
	if err != nil {
		return err
	}
	dp.SetPermissions(perms)
	d := rbac.Decide(rbac.Input{
		Route: route, Required: meta.Permissions, Platform: dp.IsPlatform(), Has: dp.HasPermission,
		ReadOnly:  r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions,
		HasTarget: present, TargetIsOwn: present && dp.Tenant != nil && target == dp.Tenant.UID,
	})
	if d.Err != nil {
		if errors.Is(d.Err, rbac.ErrMissingPermission) {
			return apierr.Wrap(d.Err, apierr.AuthForbidden, "forbidden")
		}
		return httpx.MapScopeError(d.Err)
	}
	httpx.RecordAccess(r, d)
	return nil
}

// targetTenant 读取 X-Tenant-ID：缺省 → (空, false)；重复头或不是合法 uid → 400。
func targetTenant(r *http.Request) (string, bool, error) {
	vals := r.Header.Values("X-Tenant-ID")
	switch {
	case len(vals) == 0:
		return "", false, nil
	case len(vals) > 1 || !uid.Valid(vals[0]):
		return "", false, apierr.Validation(apierr.Detail{Field: "X-Tenant-ID", Reason: "must be a single tenant uid"})
	}
	return vals[0], true, nil
}

// authorizeOwnToken 授权令牌管理 operation（/auth/api-tokens*）：它们只作用于调用者自己名下的令牌（user_id 来自会话），不是「跨租户资源」，
// 所以不走 rbac.Decide 的 acting-as 矩阵（否则平台用户创建平台令牌会被判为「写操作须指定租户」）。规则：
//   - 只有 Cookie 会话的主体（Bearer 不能管理令牌；认证钩子已拒绝，这里再 fail-closed 一次）；
//   - 须具备路由声明的权限（token:manage，scope=both：平台角色与租户角色均可持有）；
//   - 不接受代租户：平台主体带 X-Tenant-ID → 400；租户主体带他人租户 → 404（不泄漏存在性），带自己的租户 → 允许；
//   - 放行时登记授权决策（平台主体 = platform，租户主体 = own_tenant），Scope 钩子据此从主体自身推导 Scope。
func (a *Authorizer) authorizeOwnToken(r *http.Request, meta httpx.RouteMeta, dp *domain.Principal) error {
	if dp.Bearer || dp.Session.ID == 0 {
		return apierr.New(apierr.AuthForbidden, "forbidden")
	}
	target, present, err := targetTenant(r)
	if err != nil {
		return err
	}
	mode := rbac.ModePlatform
	if dp.IsPlatform() {
		if present {
			return apierr.Validation(apierr.Detail{Field: "X-Tenant-ID", Reason: "API tokens are managed by their owner; acting-as is not supported"})
		}
	} else {
		if dp.Tenant == nil {
			return apierr.New(apierr.AuthForbidden, "forbidden")
		}
		if present && target != dp.Tenant.UID {
			return httpx.MapScopeError(tenantscope.ErrTenantMismatch)
		}
		mode = rbac.ModeOwnTenant
	}
	perms, err := a.svc.LoadPermissions(r.Context(), dp)
	if err != nil {
		return err
	}
	dp.SetPermissions(perms)
	for _, need := range meta.Permissions {
		if !dp.HasPermission(need) {
			return apierr.Wrap(rbac.ErrMissingPermission, apierr.AuthForbidden, "forbidden")
		}
	}
	httpx.RecordAccess(r, rbac.Decision{Mode: mode})
	return nil
}
