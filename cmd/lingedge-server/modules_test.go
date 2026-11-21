package main

import (
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	"github.com/wuekevin/lingedge/internal/platform/server"
	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// TestModulesDeliveredMatchRoutes（Story 1.22 AC3、FR-31）：/auth/me 的 modules_delivered 来自能力注册表（sysconfig.DeliveredModules），
// 它必须与**真实装配的路由集合**（registerRoutes，与进程装配、lingedge-server routes 同一份）一致：不虚报（登记了却没有路由）、不漏登
// （有路由却没登记）、不半交付（模块路径下仍有未注册的 M1 operation）。
func TestModulesDeliveredMatchRoutes(t *testing.T) {
	r, err := httpx.NewRouter(httpx.Options{IDs: uid.NewGenerator(clock.System()), Idempotency: contracttest.NopIdempotencyStore{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registerRoutes(r, server.Deps{}); err != nil {
		t.Fatal(err)
	}
	spec := contracttest.LoadRepo(t)
	var contract []sysconfig.ContractOp
	for _, id := range spec.OperationIDs() {
		op, _ := spec.Operation(id)
		contract = append(contract, sysconfig.ContractOp{OperationID: id, Path: op.Meta.Path, Phase: op.Meta.Phase})
	}
	for _, p := range sysconfig.CheckModules(sysconfig.DeliveredModules, r.Routes(), contract) {
		t.Error(p)
	}
	// 注册表里的模块名必须唯一且 modules_delivered 恰是它们。
	seen := map[string]bool{}
	for _, m := range sysconfig.DeliveredModules {
		if seen[m.Name] {
			t.Errorf("模块 %q 重复登记", m.Name)
		}
		seen[m.Name] = true
	}
	if got := sysconfig.ModulesDelivered(); len(got) != len(sysconfig.DeliveredModules) {
		t.Errorf("ModulesDelivered 与登记表不一致: %v", got)
	}
}

// 商业类路由（x-feature: commercial）在 M1 尚无真实路由；一旦出现，它们必须在契约里声明 x-feature 且被 feature 中间件保护——
// 这里核对运行时已注册的每条路由的 Feature 与契约一致（contracttest.CheckRoutes 已含，此处再显式断言 M1 没有未标注的商业路径）。
func TestNoCommercialRouteRegisteredWithoutFeature(t *testing.T) {
	r, err := httpx.NewRouter(httpx.Options{IDs: uid.NewGenerator(clock.System()), Idempotency: contracttest.NopIdempotencyStore{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := registerRoutes(r, server.Deps{}); err != nil {
		t.Fatal(err)
	}
	spec := contracttest.LoadRepo(t)
	if probs := spec.CheckRoutes(r.Routes()); len(probs) > 0 {
		for _, p := range probs {
			t.Error(p)
		}
	}
	for _, m := range r.Routes() {
		if op, _ := spec.Operation(m.OperationID); op != nil && op.Meta.Feature != m.Feature {
			t.Errorf("%s: Feature 与契约不一致（代码 %q，契约 %q）", m.OperationID, m.Feature, op.Meta.Feature)
		}
	}
}
