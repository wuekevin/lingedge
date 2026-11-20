package auth

import (
	"context"

	"github.com/wuekevin/lingedge/internal/auth/internal/handler"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// Options 是 New 的参数。字段含义见 handler.Options：Pool（必须是 app 池）、Redis、Clock、IDs、Log、Argon2（config.Auth.Argon2，由基准测定）、
// ExtraOrigins（额外允许的 Origin，通常为空）、Bearer（API Token 认证，Story 1.19 注入）、MaxConcurrentHashes、ManifestOnly（仅导出路由清单）。
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
	// Flush 等待已派发的后台写入（DB 兜底失败计数）完成；停机前与测试断言前调用。
	Flush()
}

// New 创建认证模块；纯委托给实现包（根包不含实现，见 scripts/gates 的 arch 门禁对 api.go 构造函数的规定）。
func New(ctx context.Context, o Options) (Module, error) {
	return handler.NewModule(ctx, o)
}
