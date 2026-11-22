// Package assembly 是 lingedge-server 的「手写 wire」装配根（ADR-001 §4）：把认证、租户、审计、系统设置、任务中心等模块装配成
// REST 钩子（BuildHooks）与路由（Register）。生产进程（cmd/lingedge-server）与测试被测系统（tests/apistack，隔离矩阵/租户集成测试）
// 调用同一份构造函数——不再各写一份镜像（Story 1.22b 收敛了 1.20 起的重复装配）；测试只通过 Options 调小缓存 TTL/限流、追加探针审计动作。
//
// 它位于 cmd/lingedge-server 的子目录（而非 internal/）：装配根按 ADR-001 属于二进制入口，且 internal/ 下的目录须登记为模块（arch-check）。
package assembly

import (
	"context"
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/auth"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/audit/auditapi"
	auditrepo "github.com/wuekevin/lingedge/internal/platform/audit/repository"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/server"
	"github.com/wuekevin/lingedge/internal/platform/setup"
	"github.com/wuekevin/lingedge/internal/platform/signingkeys"
	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
	"github.com/wuekevin/lingedge/internal/platform/taskrt/taskapi"
	"github.com/wuekevin/lingedge/internal/tenant"
)

// Options 是装配的可调项；生产装配传零值。
type Options struct {
	// ExtraAuditOps 追加登记到审计记录器的 operation 与动作（测试专用探针路由；生产不传）。
	ExtraAuditOps []audit.Op
	// SysCacheTTL / SysPublicRateLimit 调整系统设置快照缓存 TTL 与公开接口限流（<=0 取默认；测试调小以观察过期与 429）。
	SysCacheTTL        time.Duration
	SysPublicRateLimit int
	// DeniedWindow / DeniedActorCap 调整被拒绝类审计的去重窗口与主体级上限（<=0 取默认；测试缩短窗口以观察「窗口过后再记」）。
	DeniedWindow   time.Duration
	DeniedActorCap int
}

// Assembly 持有一次进程装配中被多处共享的模块实例（认证模块同时提供认证钩子与 /auth/* 路由，二者必须是同一个实例）。
// 每次 New 一个，不用包级变量，因此同一进程内可以重复运行（测试）。装配完成后各模块可经导出字段访问（测试据此直接造数据/读设置）。
type Assembly struct {
	opts   Options
	Auth   auth.Module
	Tenant tenant.Module
	Audit  *audit.Writer
	// Sys 是系统设置模块（Story 1.22）：/admin/settings*、/admin/system/*、/public/*、功能开关（feature.disabled）与 /auth/me 的 features。
	Sys *sysconfig.Module
	// Keys 是签名/CA 密钥服务（Story 1.23）：GET /admin/system/signing-keys；Initialize 供 Story 1.24 安装向导调用，Sign 供修订发布（3.x）调用。
	Keys *signingkeys.Service
}

// NewSetupAdmin 是安装向导（setup mode，Story 1.24）创建管理员所需的认证能力：用向导迁移完成后装配的 app 池与 Redis 构造一个认证模块，
// 把它的 Bootstrap 入口适配给 internal/platform/setup（platform 层不得 import internal/auth，经装配层注入）。
func NewSetupAdmin(d setup.PostDeps) (setup.AdminService, error) {
	m, err := auth.New(d.Ctx, auth.Options{Pool: d.Pool, Redis: d.Redis, Clock: d.Clock, IDs: d.IDs, Log: d.Log, Argon2: d.Argon2, Box: d.Box, Audit: d.Audit})
	if err != nil {
		return nil, err
	}
	return setupAdmin{b: m.Bootstrap()}, nil
}

type setupAdmin struct{ b auth.Bootstrapper }

func (a setupAdmin) CreateAdmin(ctx context.Context, in setup.AdminCreate) (setup.AdminCreated, error) {
	r, err := a.b.CreateAdmin(ctx, auth.BootstrapAdminInput{Email: in.Email, DisplayName: in.DisplayName, Password: in.Password, Locale: in.Locale})
	return setup.AdminCreated{UserUID: r.UserUID, Created: r.Created, TOTPEnabled: r.TOTPEnabled}, err
}

func (a setupAdmin) BeginTOTP(ctx context.Context, userUID string) (setup.TOTPEnrollment, error) {
	e, err := a.b.BeginTOTP(ctx, userUID)
	if err != nil {
		return setup.TOTPEnrollment{}, err
	}
	return setup.TOTPEnrollment{Secret: e.Secret, OTPAuthURI: e.OTPAuthURI}, nil
}

func (a setupAdmin) ConfirmTOTP(ctx context.Context, userUID, code string) ([]string, error) {
	return a.b.ConfirmTOTP(ctx, userUID, code)
}

func (a setupAdmin) HasAdmin(ctx context.Context) (bool, error) { return a.b.HasAdmin(ctx) }

// New 创建装配根。
func New(o ...Options) *Assembly {
	a := &Assembly{}
	if len(o) > 0 {
		a.opts = o[0]
	}
	return a
}

// BuildHooks 是 REST 认证/授权/租户 Scope/审计四个钩子的唯一装配点（Story 1.12 约定，1.13 接入进程）；具体装配在 auth.Module.Hooks()
// （集成测试共用同一份，避免漂移）。
//
// 现状（诚实声明）：
//
//	Authenticator  已实现（Story 1.16/1.17）：Cookie 会话（realm 隔离/滑动+绝对过期/CSRF）；Bearer API Token（Story 1.19）：不使用 Cookie 与 CSRF，主体权限 = 令牌 scopes ∩ 创建者当前权限，只能调用业务路由。
//	Authorizer     已实现（Story 1.18）：RBAC——权限集 ∩ 路由声明（须全部具备）、平台/租户主体 × x-tenant-scope × X-Tenant-ID 授权矩阵；
//	               权限集缓存键含 permission_version（Redis TTL≤60s + Pub/Sub 失效 + 轮询兜底，Redis 不可用直读 MySQL）。
//	Reauth         已实现（Story 1.17，1.18 调整顺序）：x-reauth 路由先权限后再认证，窗口外 403 auth.reauth_required。
//	Scope          完整实现（Story 1.20）：Scope 只由已认证主体 + 路由元数据 + X-Tenant-ID（经授权钩子放行后）生成；租户模块提供租户目录，
//	               acting-as 的目标租户须真实存在且状态 active（否则 404，与他租户资源逐字节一致）；自助 operation 只从主体自身推导 Scope（忽略 X-Tenant-ID）。
//	Features       已实现（Story 1.22）：运行模式 → 功能开关；声明了 x-feature 的路由（商业类）在自用模式 403 feature.disabled，位置在认证之后、授权之前（ADR-018 §11）；
//	               未装配时默认安全拒绝（DenyFeatures）。
//	Auditor        已实现（Story 1.21）：audit.Writer。审计行由业务 Service 在**业务事务内**写 audit_logs（失败 = 业务整体回滚，fail-closed，IMP-19）；
//	               钩子兜底「声明了 x-audit 的成功请求却没有审计行」，未在 audit.Catalog 登记的 x-audit operation 一律 500。
//
// 因此：任何未被授权的路由认证通过后仍会被授权钩子拒绝（403），不会因为「已登录」而放行。
func (a *Assembly) BuildHooks(d server.Deps) (httpx.Hooks, error) {
	if d.DB == nil || d.Redis == nil || d.Config == nil || d.Secrets == nil {
		return httpx.Hooks{}, errors.New("装配认证模块需要 MySQL 池、Redis、配置与信封加密（主密钥）")
	}
	aw, err := audit.New(audit.Options{Pool: d.DB, IDs: d.IDs, Clock: d.Clock, Log: d.Logger, ExtraOps: a.opts.ExtraAuditOps, Redis: d.Redis, DeniedWindow: a.opts.DeniedWindow, DeniedActorCap: a.opts.DeniedActorCap})
	if err != nil {
		return httpx.Hooks{}, err
	}
	a.Audit = aw
	sys, err := sysconfig.New(sysconfig.Options{Pool: d.DB, Clock: d.Clock, IDs: d.IDs, Log: d.Logger, Box: d.Secrets, Audit: aw, Migrations: d.Migrations, Redis: d.Redis, CacheTTL: a.opts.SysCacheTTL, PublicRateLimit: a.opts.SysPublicRateLimit})
	if err != nil {
		return httpx.Hooks{}, err
	}
	a.Sys = sys
	keys, err := signingkeys.New(signingkeys.Options{Pool: d.DB, Box: d.Secrets, Audit: aw, IDs: d.IDs, Clock: d.Clock, Log: d.Logger})
	if err != nil {
		return httpx.Hooks{}, err
	}
	a.Keys = keys
	m, err := auth.New(context.Background(), auth.Options{
		Pool: d.DB, Redis: d.Redis, Clock: d.Clock, IDs: d.IDs, Log: d.Logger, Argon2: d.Config.Auth.Argon2, Box: d.Secrets, Audit: aw, Features: sys,
	})
	if err != nil {
		return httpx.Hooks{}, err
	}
	a.Auth = m
	// 安全敏感设置键（登录节流/强制 TOTP/会话与 MFA 窗口/令牌有效期/邮件/保留期等）的写入要求近期再认证：设置模块用认证模块的 ReauthChecker（Story 1.22b）。
	sys.SetReauthChecker(m.ReauthChecker())
	tm, err := tenant.New(context.Background(), tenant.Options{Pool: d.DB, Clock: d.Clock, Log: d.Logger, Audit: aw})
	if err != nil {
		return httpx.Hooks{}, err
	}
	a.Tenant = tm
	// 停机时等待认证模块的后台写入（DB 兜底失败计数）落库：在监听排空之后、MySQL 关闭之前执行（Story 1.16 遗留，Story 1.17 接入）。
	if d.OnShutdown != nil {
		d.OnShutdown("认证模块后台写入", func(context.Context) error { m.Flush(); return nil })
	}
	return assembleHooks(m, tm, aw, sys), nil
}

// assembleHooks 把认证模块、租户模块（租户目录）、审计钩子与功能开关（sysconfig，运行模式 → x-feature，认证后授权前）装配成 httpx.Hooks（见 BuildHooks 说明）。
func assembleHooks(m auth.Module, t tenant.Module, aw *audit.Writer, sys *sysconfig.Module) httpx.Hooks {
	return m.Hooks(auth.HooksOptions{Tenants: t.Resolver(), Auditor: aw.Auditor(), Features: sys.Gate()})
}

// Register 注册全部路由（REST 路由的唯一装配点，Story 1.12）：认证模块的 /auth/* 与 /admin/*、租户模块的 /tenants*、任务中心、审计读取、系统设置与签名密钥元数据（Story 1.23）。
// 进程装配（server.Run 的 Register）与 `lingedge-server routes` 共用它，因此导出的清单就是真实会被服务的路由集合；
// 模块实例为空（未经 BuildHooks）时只用于导出清单（ManifestOnly：不需要数据库与 Redis，处理函数不会被调用）。
func (a *Assembly) Register(r *httpx.Router, deps server.Deps) error {
	m, t := a.Auth, a.Tenant
	if m == nil {
		var err error
		if m, err = auth.New(context.Background(), auth.Options{ManifestOnly: true}); err != nil {
			return err
		}
	}
	if t == nil {
		var err error
		if t, err = tenant.New(context.Background(), tenant.Options{ManifestOnly: true}); err != nil {
			return err
		}
	}
	sys := a.Sys
	if sys == nil {
		var err error
		if sys, err = sysconfig.New(sysconfig.Options{ManifestOnly: true}); err != nil {
			return err
		}
	}
	keys := a.Keys
	if keys == nil {
		var err error
		if keys, err = signingkeys.New(signingkeys.Options{ManifestOnly: true}); err != nil {
			return err
		}
	}
	var (
		auditRepo *auditrepo.Repo
		rec       audit.Recorder
		withTx    taskapi.TxFunc
	)
	if a.Audit != nil { // nil = 仅导出路由清单：处理函数不会被调用
		auditRepo, rec, withTx = a.Audit.Repo(), a.Audit, deps.DB.WithTx
	}
	// 安装向导路由（Story 1.24）：正常模式（已安装）与清单导出都注册——每个 operation 返回 410 setup.already_installed（MIN-04），setup mode 由
	// server.RunSetup 另行装配（只含这些路由、/setup 页与 /healthz，其余 503）。正常模式下 /setup 与 /setup/* 同样永久 410（Story 1.37）。
	wiz, err := setup.New(setup.Options{Installed: true})
	if err != nil {
		return err
	}
	return errors.Join(m.Register(r), t.Register(r), taskapi.Register(r, deps.Tasks, rec, withTx), auditapi.Register(r, auditRepo), sys.Register(r), keys.Register(r), wiz.Register(r), wiz.RegisterGonePages(r))
}
