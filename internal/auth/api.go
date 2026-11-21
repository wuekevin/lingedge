package auth

import (
	"context"

	"github.com/wuekevin/lingedge/internal/auth/internal/handler"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// Bootstrapper 是安装向导（Story 1.24）的管理员引导入口：创建首个超级管理员（口令由安装者设定、强度校验）、可选绑定 TOTP；
// 复用 1.16/1.17/1.18 的口令哈希、TOTP 与角色授予，审计动作 setup.admin（Actor = 系统组件 setup）。装配层把它适配给 internal/platform/setup。
type Bootstrapper = handler.Bootstrapper

// BootstrapAdminInput / BootstrapAdminResult / TOTPEnrollment 是 Bootstrapper 的输入输出类型。
type (
	BootstrapAdminInput  = handler.BootstrapAdminInput
	BootstrapAdminResult = handler.BootstrapAdminResult
	TOTPEnrollment       = handler.TOTPEnrollment
)

// HooksOptions 是 Module.Hooks 的装配参数（租户目录与审计钩子）。
type HooksOptions = handler.HooksOptions

// Options 是 New 的参数。字段含义见 handler.Options：Pool（必须是 app 池）、Redis、Clock、IDs、Log、Argon2（config.Auth.Argon2，由基准测定）、
// Box（信封加密，TOTP 密钥）、Audit（审计记录器，必填，Story 1.21）、ExtraOrigins（额外允许的 Origin，通常为空）、MaxConcurrentHashes、ManifestOnly（仅导出路由清单）。
type Options = handler.Options

// Module 是认证模块（Story 1.16）的对外契约：真实的会话认证钩子 + /auth/* 路由 + 自助 operation 判定。
// 实现在 internal/auth/internal/handler（模块根包只含契约，ADR-001 §3）。
type Module interface {
	// Authenticator 返回 httpx 认证钩子的真实实现（装配点：cmd/lingedge-server）。Cookie 会话：realm 隔离、滑动+绝对过期、CSRF；
	// Bearer API Token 由 Options.Bearer 注入（1.19），未注入时携带 Bearer 的请求一律 401。
	Authenticator() httpx.Authenticator
	// Register 经 httpx.Router.Register 注册全部 /auth/* 路由（元数据与 openapi.yaml 一致）。
	Register(r *httpx.Router) error
	// SelfService 报告 operationID 是否是「自助」operation：只作用于调用者自己的身份/会话/登录日志，x-permissions 为空且不要求再认证。
	// 装配层（httpx.WithSelfService）据此给它们最小授权语义；其他路由仍走授权/Scope/审计钩子。
	SelfService(operationID string) bool
	// ReauthChecker 返回 L4 再认证检查组件：对声明 x-reauth: true 的 operation，会话的 reauth_at 须在 auth.reauth_window_s 内，
	// 否则 403 auth.reauth_required；Bearer API Token 请求永远不能满足。装配层用 httpx.WithReauth 接入授权链，1.18 的授权钩子可复用。
	ReauthChecker() httpx.ReauthChecker
	// Hooks 返回 REST 钩子的完整装配（认证 + RBAC 授权 + 再认证 + 完整租户 Scope 钩子 + 真实审计钩子（HooksOptions.Auditor）+ 自助最小授权）：装配点与集成测试共用。
	// o.Tenants 是租户目录（租户模块）：未装配时 acting-as 保持安全拒绝。
	Hooks(o HooksOptions) httpx.Hooks
	// Bootstrap 返回安装向导的管理员引导入口（Story 1.24）。
	Bootstrap() Bootstrapper
	// Flush 等待已派发的后台写入（DB 兜底失败计数）完成；停机前与测试断言前调用。
	Flush()
}

// New 创建认证模块；纯委托给实现包（根包不含实现，见 scripts/gates 的 arch 门禁对 api.go 构造函数的规定）。
func New(ctx context.Context, o Options) (Module, error) {
	return handler.NewModule(ctx, o)
}
