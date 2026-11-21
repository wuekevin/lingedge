package tenant

import (
	"context"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/tenant/internal/handler"
)

// Options 是 New 的参数：Pool（必须是 app 池）、Clock、Log、Audit（审计记录器，必填，Story 1.21）、ManifestOnly（仅导出路由清单：不需要数据库，处理函数不可被调用）。
type Options = handler.Options

// Module 是租户模块的对外契约。实现在 internal/tenant/internal/handler（模块根包只含契约，ADR-001 §3）。
type Module interface {
	// Register 经 httpx.Router.Register 注册全部 /tenants* 路由（元数据与 openapi.yaml 一致）。
	Register(r *httpx.Router) error
	// Resolver 返回租户目录：把租户 uid 解析为租户（不存在/已删除返回 httpx.ErrTenantNotFound）；ManifestOnly 时为 nil。
	// 装配点把它交给 Scope 钩子（auth.Module.Hooks 的 HooksOptions.Tenants），acting-as 才能被放行。
	Resolver() httpx.TenantResolver
}

// New 创建租户模块；纯委托给实现包（根包不含实现，见 scripts/gates 的 arch 门禁对 api.go 构造函数的规定）。
func New(ctx context.Context, o Options) (Module, error) {
	return handler.NewModule(ctx, o)
}
