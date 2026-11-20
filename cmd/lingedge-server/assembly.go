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

// buildHooks 是 REST 认证/授权/租户 Scope/审计四个钩子的唯一装配点（Story 1.12 约定，1.13 接入进程）。
//
// 现状（诚实声明）：
//
//	Reauth         已实现（Story 1.17）：httpx.WithReauth + auth.ReauthChecker，x-reauth 路由在窗口外 403 auth.reauth_required（先于授权钩子）。
//	Authenticator  已实现（Story 1.16/1.17）：Cookie 会话（realm 隔离/滑动+绝对过期/CSRF）；Bearer API Token 待 1.19（携带 Bearer 一律 401）。
//	Authorizer     未实现（1.18）：默认安全拒绝；仅对「自助」operation（auth.SelfService：/auth/* 中 x-permissions 为空且不要求再认证的路由）
//	               授予最小授权语义——已认证主体即可，因为它们只作用于调用者自己的身份/会话/登录日志。见 httpx.WithSelfService。
//	Scope          未实现（1.20）：默认安全拒绝；自助 operation 同上，只从主体自身推导 Scope（忽略 X-Tenant-ID，不做代租户）。
//	Auditor        未实现（1.21）：默认拒绝需要审计的路由；自助 operation 的留痕由 login_logs 承担（登录/登出/失败/锁定/改密/会话吊销），
//	               audit_logs 的统一接入属 1.21。
//
// 因此：其余任何路由（如任务中心）认证通过后仍会被授权钩子拒绝（403），不会因为「已登录」而放行。
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
func assembleHooks(m auth.Module) httpx.Hooks {
	// 再认证（L4）在授权链里统一执行：声明 x-reauth: true 的路由在窗口外 403 auth.reauth_required；
	// WithSelfService 排除了 Reauth 路由，因此这些路由仍走（目前拒绝的）授权钩子。
	return httpx.WithReauth(httpx.WithSelfService(httpx.Hooks{Authenticator: m.Authenticator()}, m.SelfService), m.ReauthChecker())
}

// 周期作业的登记点见 cron.go（Story 1.15）。
