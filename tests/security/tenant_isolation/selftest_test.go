package tenantisolation

import (
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
)

// 以下是登记机制与骨架生成器的正反样本自测（不需要数据库）。

func meta(id, method, path string, scope httpx.TenantScope) httpx.RouteMeta {
	return httpx.RouteMeta{OperationID: id, Method: method, Path: path, Auth: httpx.AuthRequired, Permissions: []string{"x:read"}, TenantScope: scope, Phase: "M1"}
}

// 负向：新增租户资源（已注册）未登记 → 失败，并点名。
func TestCoverageFailsForUnregisteredNewTenantResource(t *testing.T) {
	registered := []httpx.RouteMeta{meta("listWidgets", "GET", "/widgets", httpx.ScopeEither), meta("getWidget", "GET", "/widgets/{widgetId}", httpx.ScopeTenant), meta("listNodes", "GET", "/nodes", httpx.ScopePlatform)}
	reg := map[string]Coverage{"getWidget": {Kind: Driven, Resource: "widgets"}}
	p := AuditCoverage(registered, reg, func(TestRef) bool { return true })
	if len(p) != 1 || !strings.Contains(p[0], "listWidgets") || !strings.Contains(p[0], "未在隔离矩阵登记") {
		t.Fatalf("应只点名未登记的 listWidgets（platform 路由不需要登记）: %v", p)
	}
}

// 负向：陈旧登记、误登记 platform 路由、缺理由/缺引用/引用不存在 → 失败。
func TestCoverageRejectsBadRegistrations(t *testing.T) {
	registered := []httpx.RouteMeta{meta("getWidget", "GET", "/widgets/{widgetId}", httpx.ScopeTenant), meta("listNodes", "GET", "/nodes", httpx.ScopePlatform), meta("getMine", "GET", "/mine", httpx.ScopeEither)}
	reg := map[string]Coverage{
		"getWidget": {Kind: Driven},                    // 缺资源名
		"listNodes": {Kind: Driven, Resource: "nodes"}, // platform 路由误登记
		"getMine":   {Kind: Exempt},                    // 缺理由与引用
		"gone":      {Kind: Driven, Resource: "x"},     // 陈旧
	}
	p := strings.Join(AuditCoverage(registered, reg, func(TestRef) bool { return true }), "\n")
	for _, want := range []string{"getWidget: Driven 登记必须给出资源名", "listNodes: 登记项没有对应的已注册 tenant|either 路由", "getMine: Exempt 登记必须给出理由", "getMine: Exempt 登记必须引用至少一个", "gone: 登记项没有对应"} {
		if !strings.Contains(p, want) {
			t.Errorf("应报告 %q:\n%s", want, p)
		}
	}
	// 引用的测试不存在。
	reg2 := map[string]Coverage{"getMine": {Kind: Exempt, Reason: "r", Refs: []TestRef{{"tests/integration/auth", "TestNoSuchThing"}}}}
	p2 := AuditCoverage([]httpx.RouteMeta{meta("getMine", "GET", "/mine", httpx.ScopeEither)}, reg2, testFuncExists("../../.."))
	if len(p2) != 1 || !strings.Contains(p2[0], "TestNoSuchThing 不存在") {
		t.Fatalf("不存在的引用应失败: %v", p2)
	}
	// 正向：真实存在的引用通过。
	reg3 := map[string]Coverage{"getMine": {Kind: Exempt, Reason: "r", Refs: []TestRef{{authIT, "TestTenantTokenIsolation"}}}}
	if p3 := AuditCoverage([]httpx.RouteMeta{meta("getMine", "GET", "/mine", httpx.ScopeEither)}, reg3, testFuncExists("../../..")); len(p3) != 0 {
		t.Fatalf("存在的引用应通过: %v", p3)
	}
}

// 正向：登记完整时通过；公开路由与 platform 路由不要求登记。
func TestCoveragePassesWhenComplete(t *testing.T) {
	pub := meta("login", "POST", "/auth/login", httpx.ScopePublic)
	pub.Auth = httpx.AuthPublic
	registered := []httpx.RouteMeta{meta("getWidget", "GET", "/widgets/{widgetId}", httpx.ScopeTenant), meta("listNodes", "GET", "/nodes", httpx.ScopePlatform), pub}
	if p := AuditCoverage(registered, map[string]Coverage{"getWidget": {Kind: Driven, Resource: "widgets"}}, func(TestRef) bool { return true }); len(p) != 0 {
		t.Fatalf("不应有问题: %v", p)
	}
}

// 真实的登记表自身必须自洽（Exempt 的引用都存在）。
func TestRealRegistryRefsExist(t *testing.T) {
	exists := testFuncExists("../../..")
	for id, c := range Registry {
		for _, r := range c.Refs {
			if !exists(r) {
				t.Errorf("%s 引用的测试 %s.%s 不存在", id, r.Dir, r.Func)
			}
		}
	}
}

// 骨架由 openapi.yaml 自动生成：行只取决于 x-tenant-scope；加一个 operation 就多出对应的行（用合成的 OpenAPI 验证，不依赖手写清单）。
func TestSkeletonIsGeneratedFromOpenAPI(t *testing.T) {
	const doc = `openapi: 3.1.0
info: {title: t, version: "1"}
servers: [{url: /api/v1}]
paths:
  /widgets:
    get:
      operationId: listWidgets
      responses: {"200": {description: ok}}
      x-permissions: [widget:read]
      x-tenant-scope: either
      x-audit: false
      x-phase: M1
  /nodes:
    get:
      operationId: listNodes
      responses: {"200": {description: ok}}
      x-permissions: [node:read]
      x-tenant-scope: platform
      x-audit: false
      x-phase: M1
  /login:
    post:
      operationId: login
      security: []
      responses: {"200": {description: ok}}
      x-permissions: []
      x-tenant-scope: public
      x-public: true
      x-audit: true
      x-phase: M1
`
	spec, err := contracttest.LoadFromBytes([]byte(doc))
	if err != nil {
		t.Fatalf("合成 OpenAPI 应可加载: %v", err)
	}
	rows := Skeleton(spec)
	byOp := map[string][]Row{}
	for _, r := range rows {
		byOp[r.OperationID] = append(byOp[r.OperationID], r)
	}
	if len(byOp["login"]) != 0 {
		t.Error("公开路由不进矩阵")
	}
	has := func(op, ident, sit string, want int) bool {
		for _, r := range byOp[op] {
			if r.Identity == ident && r.Situation == sit && r.Want == want {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		op, ident, sit string
		want           int
	}{
		{"listWidgets", IdTenantA, SitOther, 404}, {"listWidgets", IdTokenA, SitMissing, 404}, {"listWidgets", IdPlatformNoImp, SitOwn, 403},
		{"listWidgets", IdPlatformAdmin, SitSuspended, 404}, {"listWidgets", IdAnonymous, SitNone, 401},
		{"listNodes", IdTenantA, SitNone, 401}, {"listNodes", IdTokenA, SitOwn, 403}, {"listNodes", IdPlatformAdmin, SitOwn, 400},
	} {
		if !has(c.op, c.ident, c.sit, c.want) {
			t.Errorf("骨架应含 %s × %s × %s → %d", c.op, c.ident, c.sit, c.want)
		}
	}
	if has("listNodes", IdTenantA, SitOther, 404) || has("listWidgets", IdTenantA, SitNone, 403) {
		t.Error("骨架不应含与路由类型不符的期望")
	}
}

// 登记机制不能被空表绕过：真实 openapi 的骨架必须非空，且覆盖三类路由。
func TestSkeletonFromRealOpenAPIIsNonTrivial(t *testing.T) {
	rows := Skeleton(contracttest.LoadRepo(t))
	scopes := map[httpx.TenantScope]int{}
	for _, r := range rows {
		scopes[r.Scope]++
	}
	if scopes[httpx.ScopePlatform] == 0 || scopes[httpx.ScopeTenant] == 0 || scopes[httpx.ScopeEither] == 0 {
		t.Fatalf("骨架应覆盖 platform/tenant/either 三类路由: %v", scopes)
	}
	t.Logf("骨架共 %d 行（由 openapi.yaml 的 x-tenant-scope 自动生成：需认证 operation × 身份 × 目标租户情形）", len(rows))
}
