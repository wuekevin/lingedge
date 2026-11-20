package handler

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/auth/internal/repository"
	"github.com/wuekevin/lingedge/internal/auth/internal/service"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// Options 是 NewModule 的参数（根包 auth.Options 是它的别名）。
type Options struct {
	// Pool 必须是 app 连接池。
	Pool  db.Pool
	Redis *redisx.Client
	Clock clock.Clock
	IDs   *uid.Generator
	Log   *zap.Logger
	// Argon2 是口令哈希参数（config.Auth.Argon2，由基准测定）。
	Argon2 config.Argon2
	// ExtraOrigins 是除「与请求同源」外额外允许发起 Cookie 会话写请求的 Origin（精确匹配）；通常为空。
	ExtraOrigins []string
	// Box 是信封加密（TOTP 密钥经它加密存储；Story 1.17）；非 ManifestOnly 时必填。
	Box *secretbox.Box
	// MaxConcurrentHashes 限制并发的 argon2id 计算数（每次占用 m KiB 内存）；<=0 取 max(2, CPU 数)。
	MaxConcurrentHashes int
	// PermCacheTTL 是权限集缓存的 TTL（<=0 或 >60 秒时取 60 秒，ADR-025 §1）；测试可调小以验证 TTL 兜底。
	PermCacheTTL time.Duration
	// PermPollEvery 是权限失效广播的轮询兜底周期（<=0 取 30 秒）；测试可调小。
	PermPollEvery time.Duration
	// ManifestOnly 仅用于导出路由清单（lingedge-server routes）：不需要数据库与 Redis，处理函数不可被调用。
	ManifestOnly bool
}

// Module 是认证模块（Story 1.16）：真实的会话认证钩子 + /auth/* 路由。
type Module struct {
	svc      *service.Service
	handlers *Handlers
	authn    *Authenticator
	authz    *Authorizer
	log      *zap.Logger
}

// NewModule 创建认证模块。
func NewModule(ctx context.Context, o Options) (*Module, error) {
	if o.ManifestOnly {
		return &Module{handlers: New(nil), log: zap.NewNop()}, nil
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	repo, err := repository.New(o.Pool)
	if err != nil {
		return nil, err
	}
	h, err := service.NewHasher(service.Params{MemoryKiB: uint32(o.Argon2.MemoryKiB), Iterations: uint32(o.Argon2.Iterations), Parallelism: uint8(o.Argon2.Parallelism)}, o.MaxConcurrentHashes)
	if err != nil {
		return nil, err
	}
	svc, err := service.New(ctx, service.Options{Repo: repo, WithTx: o.Pool.WithTx, Redis: o.Redis, Hasher: h, Box: o.Box, Clock: o.Clock, IDs: o.IDs, Log: o.Log, PermCacheTTL: o.PermCacheTTL, PermPollEvery: o.PermPollEvery})
	if err != nil {
		return nil, err
	}
	return &Module{svc: svc, handlers: New(svc), authn: NewAuthenticator(svc, o.ExtraOrigins, bearerAuthenticator{svc: svc}), authz: NewAuthorizer(svc), log: o.Log.Named("auth")}, nil
}

// Authenticator 返回 httpx 认证钩子的真实实现（装配点：cmd/lingedge-server 的 assembleHooks）；ManifestOnly 时为 nil。
func (m *Module) Authenticator() httpx.Authenticator {
	if m.authn == nil {
		return nil
	}
	return m.authn
}

// Register 注册全部 /auth/* 路由（经 httpx.Router.Register，元数据与 openapi 一致）。
func (m *Module) Register(r *httpx.Router) error {
	if r == nil {
		return errors.New("auth: Router 为空")
	}
	return m.handlers.Register(r)
}

// SelfService 报告 operationID 是否是「自助」operation：只作用于调用者自己的身份/会话/登录日志，不声明任何权限（x-permissions: []），
// 也不要求再认证。装配层据此给这些路由授予**最小授权语义**（已认证主体即可；见 httpx.WithSelfService）；
// 其他任何路由仍走授权/Scope/审计钩子（1.17–1.21 之前为安全拒绝默认）。
func (m *Module) SelfService(operationID string) bool {
	for _, meta := range Metas() {
		if meta.OperationID == operationID {
			return len(meta.Permissions) == 0 && !meta.Reauth
		}
	}
	return false
}

// ReauthChecker 返回 L4 再认证检查组件（httpx.ReauthChecker）：声明 x-reauth: true 的 operation 在窗口外 403 auth.reauth_required。
// 装配层经 httpx.WithReauth 把它接入授权链；Story 1.18 的真实授权钩子可直接复用同一个组件。ManifestOnly 时为 nil。
func (m *Module) ReauthChecker() httpx.ReauthChecker {
	if m.authn == nil {
		return nil
	}
	return reauthChecker{svc: m.svc}
}

// Flush 等待认证模块已派发的后台写入（DB 兜底失败计数）完成；停机前与测试断言前调用。
func (m *Module) Flush() {
	if m.svc != nil {
		m.svc.Flush()
	}
}

// AdminRoute 报告 operationID 是否是本模块的 /admin 管理路由。
func (m *Module) AdminRoute(operationID string) bool { return IsAdminOperation(operationID) }

// Hooks 是 REST 四个钩子的唯一装配（cmd/lingedge-server 与集成测试共用，避免装配漂移）：
//
//	Authenticator  Cookie 会话 + Bearer API Token（Story 1.19：主体权限 = 令牌 scopes ∩ 创建者当前权限）。
//	Authorizer     RBAC（Story 1.18）：权限集 ∩ 路由声明；平台/租户主体 × x-tenant-scope × X-Tenant-ID 的授权矩阵（rbac.Decide），
//	               放行时登记授权决策；x-reauth 在权限通过之后由 httpx.WithReauth 执行（先权限后再认证）。
//	Scope          过渡实现 httpx.WithAccessScope：只把「不需要解析 X-Tenant-ID」的授权决策落成 Scope；acting-as 与其他一切委托给安全拒绝默认（完整实现 Story 1.20）。
//	Auditor        安全拒绝默认；仅本模块的 /admin 管理 operation 走结构化日志过渡（不是 audit_logs，待 Story 1.21）。
//	WithSelfService  /auth/* 自助 operation 的最小授权语义（1.16；1.18–1.21 交付后评估是否保留）。
func (m *Module) Hooks() httpx.Hooks {
	base := httpx.Hooks{Authenticator: m.Authenticator(), Authorizer: m.authzHook(), Auditor: adminAuditor{inner: httpx.DenyAuditor{}, log: m.log}}
	base = httpx.WithAccessScope(base)
	return httpx.WithReauth(httpx.WithSelfService(base, m.SelfService), m.ReauthChecker())
}

func (m *Module) authzHook() httpx.Authorizer {
	if m.authz == nil {
		return nil // ManifestOnly：默认安全拒绝
	}
	return m.authz
}
