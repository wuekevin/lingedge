package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// 四个钩子的具体实现分别属于 Story 1.16（认证）、1.17（CSRF/会话，经认证钩子）、1.18（授权/再认证）、1.20（租户 Scope）、1.21（审计）。
// 本包只固定它们在链中的位置与「默认安全拒绝」的契约：钩子返回 error 即中止请求；返回 *apierr.Error 则按其码渲染，
// 其他 error 一律按 500 internal.error（原因只进日志）。钩子拿到的是 *http.Request：主体与 Scope 经 PrincipalFrom/tenantscope.FromContext 读取。

// Authenticator 是认证钩子：从请求（Cookie 会话 / Bearer API Token）解析主体。仅对 Auth=required 的路由调用。
// 返回 (nil, nil) 视为未认证（401）——不存在「没有主体也放行」的路径。
type Authenticator interface {
	Authenticate(r *http.Request, meta RouteMeta) (tenantscope.Principal, error)
}

// Authorizer 是授权钩子：校验 meta.Permissions（须全部具备）、meta.Reauth（再认证窗口）等。仅对 Auth=required 的路由调用。
type Authorizer interface {
	Authorize(r *http.Request, meta RouteMeta, p tenantscope.Principal) error
}

// ScopeResolver 是租户 Scope 钩子：据主体、meta.TenantScope 与 X-Tenant-ID 生成请求路径上的 Scope（通常封装 tenantscope.FromPrincipal）。
// 仅对 Auth=required 的路由调用；返回零值 Scope 视为拒绝。
type ScopeResolver interface {
	ResolveScope(r *http.Request, meta RouteMeta, p tenantscope.Principal) (tenantscope.Scope, error)
}

// AuditOutcome 是请求结束时交给审计钩子的结果。
type AuditOutcome struct {
	Status int
	// Replayed 为 true 表示响应来自幂等重放：操作并未再次执行，审计不应重复记录。
	Replayed bool
	// Panicked 为 true 表示 handler panic（对外是 500）。
	Panicked bool
}

// Auditor 是审计钩子：Begin 在 handler 之前调用（可据 meta.Audit 决定是否记录；失败则请求被拒绝，因为无法保证留痕），
// 返回的 end 在请求结束后调用（可为 nil）。对所有路由调用，包括公开路由（如登录，x-audit: true）。
type Auditor interface {
	Begin(r *http.Request, meta RouteMeta) (end func(AuditOutcome), err error)
}

// Hooks 是钩子集合；任何为 nil 的项在 NewRouter 时被替换为 Deny* 默认实现。
type Hooks struct {
	Authenticator Authenticator
	Authorizer    Authorizer
	Scope         ScopeResolver
	Auditor       Auditor
}

// ErrHookNotConfigured 是默认（拒绝）钩子返回的错误：对应能力尚未装配，请求被安全拒绝。
var ErrHookNotConfigured = errors.New("httpx: 钩子未装配（默认安全拒绝）")

// DenyAuthenticator：一律 401 auth.unauthenticated。
type DenyAuthenticator struct{}

// Authenticate 实现 Authenticator。
func (DenyAuthenticator) Authenticate(*http.Request, RouteMeta) (tenantscope.Principal, error) {
	return nil, apierr.Wrap(ErrHookNotConfigured, apierr.AuthUnauthenticated, "authentication required")
}

// DenyAuthorizer：一律 403 auth.forbidden。
type DenyAuthorizer struct{}

// Authorize 实现 Authorizer。
func (DenyAuthorizer) Authorize(*http.Request, RouteMeta, tenantscope.Principal) error {
	return apierr.Wrap(ErrHookNotConfigured, apierr.AuthForbidden, "forbidden")
}

// DenyScope：一律 403 auth.forbidden。
type DenyScope struct{}

// ResolveScope 实现 ScopeResolver。
func (DenyScope) ResolveScope(*http.Request, RouteMeta, tenantscope.Principal) (tenantscope.Scope, error) {
	return tenantscope.Scope{}, apierr.Wrap(ErrHookNotConfigured, apierr.AuthForbidden, "forbidden")
}

// DenyAuditor：一律 500 internal.error——要求审计（Audit=true）的操作在审计能力装配前不得执行；
// 不要求审计的路由（Audit=false）不受影响（无需留痕）。
type DenyAuditor struct{}

// Begin 实现 Auditor。
func (DenyAuditor) Begin(_ *http.Request, meta RouteMeta) (func(AuditOutcome), error) {
	if !meta.Audit {
		return nil, nil
	}
	return nil, apierr.Internal(ErrHookNotConfigured)
}

func (h Hooks) withDefaults() Hooks {
	if h.Authenticator == nil {
		h.Authenticator = DenyAuthenticator{}
	}
	if h.Authorizer == nil {
		h.Authorizer = DenyAuthorizer{}
	}
	if h.Scope == nil {
		h.Scope = DenyScope{}
	}
	if h.Auditor == nil {
		h.Auditor = DenyAuditor{}
	}
	return h
}

// MapScopeError 把 tenantscope 的哨兵错误映射为统一错误（供 ScopeResolver 实现复用；其他 error 原样返回）：
// 越权访问平台路由/缺少代租户权限 → 403 auth.forbidden；指定了不属于主体的租户 → 404 resource.not_found（不泄漏存在性，ADR-009）；
// 缺少/非法的目标租户 → 400 validation.failed（field=X-Tenant-ID）。
func MapScopeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, tenantscope.ErrNoPrincipal):
		return apierr.Wrap(err, apierr.AuthUnauthenticated, "authentication required")
	case errors.Is(err, tenantscope.ErrForbiddenRoute), errors.Is(err, tenantscope.ErrImpersonationDenied):
		return apierr.Wrap(err, apierr.AuthForbidden, "forbidden")
	case errors.Is(err, tenantscope.ErrTenantMismatch):
		return apierr.Wrap(err, apierr.ResourceNotFound, "resource not found")
	case errors.Is(err, tenantscope.ErrTenantRequired), errors.Is(err, tenantscope.ErrInvalidScope):
		return apierr.Wrap(err, apierr.ValidationFailed, "request validation failed", apierr.Detail{Field: "X-Tenant-ID", Reason: "invalid or missing target tenant"})
	}
	return err
}

type ctxKey int

const (
	principalKey ctxKey = iota + 1
	metaKey
)

// PrincipalFrom 取出认证钩子放入请求上下文的主体。
func PrincipalFrom(ctx context.Context) (tenantscope.Principal, bool) {
	p, ok := ctx.Value(principalKey).(tenantscope.Principal)
	return p, ok && p != nil
}

// MetaFrom 取出当前路由的元数据。
func MetaFrom(ctx context.Context) (RouteMeta, bool) {
	m, ok := ctx.Value(metaKey).(RouteMeta)
	return m, ok
}

func withMeta(ctx context.Context, m RouteMeta) context.Context {
	return context.WithValue(ctx, metaKey, m)
}

func withPrincipal(ctx context.Context, p tenantscope.Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// isNilPrincipal 识别「装着 nil 指针的非 nil 接口」（避免之后调用方法时 panic，并按未认证处理）。
func isNilPrincipal(p tenantscope.Principal) bool {
	if p == nil {
		return true
	}
	v := reflect.ValueOf(p)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.Chan:
		return v.IsNil()
	}
	return false
}

func marshalBody(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	return append(b, '\n'), err
}
