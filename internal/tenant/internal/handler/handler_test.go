package handler

import (
	"context"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// 8 个路由经 Router.Register 注册，元数据与 openapi.yaml 的 x-permissions / x-tenant-scope / x-audit / x-phase 逐项一致；
// 只有 /tenants/current 是 tenant 路由，其余都是 platform 路由；createTenant（P5）不在其中。
func TestRoutesRegisteredAndMatchOpenAPI(t *testing.T) {
	m, err := NewModule(context.Background(), Options{ManifestOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := httpx.NewRouter(httpx.Options{IDs: uid.NewGenerator(clock.System()), ManifestOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register(r); err != nil {
		t.Fatal(err)
	}
	routes := r.Routes()
	if len(routes) != 8 {
		t.Fatalf("应注册 8 个路由: %d", len(routes))
	}
	spec := contracttest.LoadRepo(t)
	if problems := spec.CheckRoutes(routes); len(problems) > 0 {
		t.Fatalf("元数据与 openapi.yaml 不一致: %v", problems)
	}
	for _, rt := range routes {
		if rt.OperationID == "getCurrentTenant" != (rt.TenantScope == httpx.ScopeTenant) || (rt.TenantScope != httpx.ScopeTenant && rt.TenantScope != httpx.ScopePlatform) {
			t.Errorf("%s: 只有 getCurrentTenant 是 tenant 路由，其余为 platform: %s", rt.OperationID, rt.TenantScope)
		}
		if rt.Audit != m.AuditedOperation(rt.OperationID) {
			t.Errorf("%s: AuditedOperation 应与 x-audit 一致", rt.OperationID)
		}
	}
	if m.AuditedOperation("createTenant") || m.AuditedOperation("listTenants") {
		t.Error("读操作与未注册的 operation 不应是已审计写操作")
	}
	if m.Resolver() != nil {
		t.Error("ManifestOnly 不应提供租户目录（acting-as 因此保持安全拒绝）")
	}
	// createTenant 属 P5：不在已注册清单里，也不在「M1 未实现」清单里。
	for _, id := range spec.Unimplemented(routes, "M1") {
		if id == "createTenant" {
			t.Error("createTenant 是 P5，不应出现在 M1 未实现清单")
		}
	}
}
