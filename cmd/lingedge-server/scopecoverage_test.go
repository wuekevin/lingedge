package main

import (
	"net/http"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	"github.com/wuekevin/lingedge/internal/platform/server"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// TestTenantScopeCoverage 是 Story 1.20 AC4 的 CI 门禁（make tenantscope-check）：真实的路由集合（registerRoutes，与进程装配、
// lingedge-server routes 同一份）里，x-tenant-scope 为 tenant|either（顺带 platform）的每条路由都必须经 tenantscope（Scope 钩子），
// 否则失败。检查逻辑与负向样本见 internal/platform/httpx/contracttest（scopecoverage_test.go）。
func TestTenantScopeCoverage(t *testing.T) {
	build := func(h httpx.Hooks) (http.Handler, []httpx.RouteMeta, error) {
		r, err := httpx.NewRouter(httpx.Options{IDs: uid.NewGenerator(clock.System()), Hooks: h, Idempotency: contracttest.NopIdempotencyStore{}})
		if err != nil {
			return nil, nil, err
		}
		if err := registerRoutes(r, server.Deps{}); err != nil {
			return nil, nil, err
		}
		hd, err := r.Handler()
		return hd, r.Routes(), err
	}
	if problems := contracttest.CheckScopeCoverage(build); len(problems) > 0 {
		for _, p := range problems {
			t.Error(p)
		}
	}
}
