package main

import (
	"context"
	"errors"

	"github.com/wuekevin/lingedge/internal/auth"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/server"
)

// assembly 持有一次进程装配中被多处共享的模块实例（认证模块同时提供认证钩子与 /auth/* 路由，二者必须是同一个实例）。
// 每次 serveMain 新建一个，不用包级变量，因此同一进程内可以重复运行（测试）。
type assembly struct {
	auth auth.Module
}

// buildHooks 是 REST 认证/授权/租户 Scope/审计四个钩子的唯一装配点（Story 1.12 约定，1.13 接入进程）；具体装配在 auth.Module.Hooks()
// （集成测试共用同一份，避免漂移）。
//
// 现状（诚实声明）：
//
//	Authenticator  已实现（Story 1.16/1.17）：Cookie 会话（realm 隔离/滑动+绝对过期/CSRF）；Bearer API Token 待 1.19（携带 Bearer 一律 401）。
//	Authorizer     已实现（Story 1.18）：RBAC——权限集 ∩ 路由声明（须全部具备）、平台/租户主体 × x-tenant-scope × X-Tenant-ID 授权矩阵；
//	               权限集缓存键含 permission_version（Redis TTL≤60s + Pub/Sub 失效 + 轮询兜底，Redis 不可用直读 MySQL）。
//	Reauth         已实现（Story 1.17，1.18 调整顺序）：x-reauth 路由先权限后再认证，窗口外 403 auth.reauth_required。
//	Scope          过渡（1.18）→ 完整实现 1.20：只把「不需要解析 X-Tenant-ID」的授权决策（自己的租户/平台级/跨租户只读）落成 Scope；
//	               acting-as（带 X-Tenant-ID）与其他一切仍是安全拒绝默认，不放行未实现的部分；自助 operation 只从主体自身推导 Scope（忽略 X-Tenant-ID）。
//	Auditor        未实现（1.21）：默认拒绝需要审计的路由；例外：本模块的 /admin 管理 operation 只写结构化日志（audit_logs_written=false，待 1.21 补写 audit_logs），
//	               /auth/* 自助 operation 的留痕由 login_logs 承担。
//
// 因此：任何未被授权的路由认证通过后仍会被授权钩子拒绝（403），不会因为「已登录」而放行。
func (a *assembly) buildHooks(d server.Deps) (httpx.Hooks, error) {
	if d.DB == nil || d.Redis == nil || d.Config == nil || d.Secrets == nil {
		return httpx.Hooks{}, errors.New("装配认证模块需要 MySQL 池、Redis、配置与信封加密（主密钥）")
	}
	m, err := auth.New(context.Background(), auth.Options{
		Pool: d.DB, Redis: d.Redis, Clock: d.Clock, IDs: d.IDs, Log: d.Logger, Argon2: d.Config.Auth.Argon2, Box: d.Secrets,
	})
	if err != nil {
		return httpx.Hooks{}, err
	}
	a.auth = m
	// 停机时等待认证模块的后台写入（DB 兜底失败计数）落库：在监听排空之后、MySQL 关闭之前执行（Story 1.16 遗留，Story 1.17 接入）。
	if d.OnShutdown != nil {
		d.OnShutdown("认证模块后台写入", func(context.Context) error { m.Flush(); return nil })
	}
	return assembleHooks(m), nil
}

// assembleHooks 把认证模块装配成 httpx.Hooks（见 buildHooks 说明）。
func assembleHooks(m auth.Module) httpx.Hooks { return m.Hooks() }

// 周期作业的登记点见 cron.go（Story 1.15）。
