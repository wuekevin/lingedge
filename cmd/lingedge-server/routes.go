package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/wuekevin/lingedge/internal/auth"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/server"
	"github.com/wuekevin/lingedge/internal/platform/taskrt/taskapi"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/internal/tenant"
)

// registerRoutes 是 REST 路由的唯一装配点（Story 1.12）：各业务模块的路由注册函数在这里被调用（Story 1.16 起逐个追加），
// 进程装配（Story 1.13）与 `lingedge-server routes` 共用它，因此导出的清单就是真实会被服务的路由集合。
//
// 已注册：租户 /tenants*（Story 1.20，createTenant 属 P5 不在此）；任务中心 listTasks/getTask/listTaskEvents/cancelTask（Story 1.14）；认证 /auth/* 的 login/logout/getMe/getCsrfToken/changePassword/
// listSessions/revokeSession/listMyLoginLogs（Story 1.16）。其余 M1 业务路由由各 Story 追加，不用 501 占位（NFR-2）；
// `make openapi-check` 会输出「M1 未实现 operation 清单」。授权/Scope/审计钩子仍是安全拒绝默认（1.18–1.21）：任务中心路由认证通过后仍被授权钩子 403。
func registerRoutes(r *httpx.Router, deps server.Deps) error {
	return (&assembly{}).register(r, deps)
}

// register 注册全部路由：认证模块的 /auth/* 与 /admin/*、租户模块的 /tenants*（a.auth/a.tenant 为 nil 时只用于导出清单）与任务中心。
func (a *assembly) register(r *httpx.Router, deps server.Deps) error {
	m, t := a.auth, a.tenant
	if m == nil { // 仅导出路由清单（lingedge-server routes）：不需要数据库与 Redis
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
	return errors.Join(m.Register(r), t.Register(r), taskapi.Register(r, deps.Tasks))
}

// routesMain 实现 `lingedge-server routes [-o 文件]`：把路由清单（JSON）写到 stdout 或文件，供 make openapi-check 校验
// 「运行时路由集合 ⊆ openapi.yaml」与元数据一致。任何注册错误（元数据缺失、路径含 ':'、重复…）都以非零退出码失败。
func routesMain(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("routes", flag.ContinueOnError)
	fl.SetOutput(stderr)
	out := fl.String("o", "", "输出文件（缺省写 stdout）")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	r, err := httpx.NewRouter(httpx.Options{IDs: uid.NewGenerator(clock.System()), ManifestOnly: true})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "routes:", err)
		return 1
	}
	if err := registerRoutes(r, server.Deps{}); err != nil {
		_, _ = fmt.Fprintln(stderr, "routes: 注册路由失败:", err)
		return 1
	}
	if _, err := r.Handler(); err != nil && !errors.Is(err, httpx.ErrManifestOnly) {
		_, _ = fmt.Fprintln(stderr, "routes:", err)
		return 1
	}
	b, err := r.ManifestJSON()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "routes:", err)
		return 1
	}
	if *out == "" {
		_, _ = stdout.Write(b)
		return 0
	}
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		_, _ = fmt.Fprintln(stderr, "routes:", err)
		return 1
	}
	return 0
}
