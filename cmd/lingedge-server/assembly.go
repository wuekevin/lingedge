package main

import (
	"context"
	"errors"

	"github.com/wuekevin/lingedge/internal/auth"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/server"
	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
	"github.com/wuekevin/lingedge/internal/tenant"
)

// assembly 持有一次进程装配中被多处共享的模块实例（认证模块同时提供认证钩子与 /auth/* 路由，二者必须是同一个实例）。
// 每次 serveMain 新建一个，不用包级变量，因此同一进程内可以重复运行（测试）。
type assembly struct {
	auth   auth.Module
	tenant tenant.Module
	audit  *audit.Writer
	// sys 是系统设置模块（Story 1.22）：/admin/settings*、/admin/system/*、/public/*、功能开关（feature.disabled）与 /auth/me 的 features。
	sys *sysconfig.Module
}

// buildHooks 是 REST 认证/授权/租户 Scope/审计四个钩子的唯一装配点（Story 1.12 约定，1.13 接入进程）；具体装配在 auth.Module.Hooks()
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
func (a *assembly) buildHooks(d server.Deps) (httpx.Hooks, error) {
	if d.DB == nil || d.Redis == nil || d.Config == nil || d.Secrets == nil {
		return httpx.Hooks{}, errors.New("装配认证模块需要 MySQL 池、Redis、配置与信封加密（主密钥）")
	}
	aw, err := audit.New(audit.Options{Pool: d.DB, IDs: d.IDs, Clock: d.Clock, Log: d.Logger})
	if err != nil {
		return httpx.Hooks{}, err
	}
	a.audit = aw
	sys, err := sysconfig.New(sysconfig.Options{Pool: d.DB, Clock: d.Clock, IDs: d.IDs, Log: d.Logger, Box: d.Secrets, Audit: aw, Migrations: d.Migrations, Redis: d.Redis})
	if err != nil {
		return httpx.Hooks{}, err
	}
	a.sys = sys
	m, err := auth.New(context.Background(), auth.Options{
		Pool: d.DB, Redis: d.Redis, Clock: d.Clock, IDs: d.IDs, Log: d.Logger, Argon2: d.Config.Auth.Argon2, Box: d.Secrets, Audit: aw, Features: sys,
	})
	if err != nil {
		return httpx.Hooks{}, err
	}
	a.auth = m
	tm, err := tenant.New(context.Background(), tenant.Options{Pool: d.DB, Clock: d.Clock, Log: d.Logger, Audit: aw})
	if err != nil {
		return httpx.Hooks{}, err
	}
	a.tenant = tm
	// 停机时等待认证模块的后台写入（DB 兜底失败计数）落库：在监听排空之后、MySQL 关闭之前执行（Story 1.16 遗留，Story 1.17 接入）。
	if d.OnShutdown != nil {
		d.OnShutdown("认证模块后台写入", func(context.Context) error { m.Flush(); return nil })
	}
	return assembleHooks(m, tm, aw, sys), nil
}

// assembleHooks 把认证模块、租户模块（租户目录）、审计钩子与功能开关（sysconfig，运行模式 → x-feature，认证后授权前）装配成 httpx.Hooks（见 buildHooks 说明）。
func assembleHooks(m auth.Module, t tenant.Module, aw *audit.Writer, sys *sysconfig.Module) httpx.Hooks {
	return m.Hooks(auth.HooksOptions{Tenants: t.Resolver(), Auditor: aw.Auditor(), Features: sys.Gate()})
}

// 周期作业的登记点见 cron.go（Story 1.15）。
