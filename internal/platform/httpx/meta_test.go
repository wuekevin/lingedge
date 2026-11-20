package httpx_test

import (
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

func TestRouteMetaValidate(t *testing.T) {
	good := meta("listSites", "GET", "/sites/{siteId}/rules")
	if p := good.Validate(); len(p) != 0 {
		t.Fatalf("合法元数据被拒绝: %v", p)
	}
	pub := httpx.RouteMeta{OperationID: "login", Method: "POST", Path: "/auth/login", Auth: httpx.AuthPublic, Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopePublic, Audit: true, Phase: "M1"}
	if p := pub.Validate(); len(p) != 0 {
		t.Fatalf("合法公开路由被拒绝: %v", p)
	}

	cases := []struct {
		name string
		mut  func(m *httpx.RouteMeta)
		want string
	}{
		{"缺 operationId", func(m *httpx.RouteMeta) { m.OperationID = "" }, "缺少 OperationID"},
		{"operationId 非法", func(m *httpx.RouteMeta) { m.OperationID = "List_Sites" }, "OperationID 非法"},
		{"缺方法", func(m *httpx.RouteMeta) { m.Method = "" }, "Method"},
		{"非法方法", func(m *httpx.RouteMeta) { m.Method = "TRACE" }, "Method"},
		{"缺 Auth", func(m *httpx.RouteMeta) { m.Auth = "" }, "缺少 Auth"},
		{"非法 Auth", func(m *httpx.RouteMeta) { m.Auth = "maybe" }, "Auth"},
		{"缺 TenantScope", func(m *httpx.RouteMeta) { m.TenantScope = "" }, "缺少 TenantScope"},
		{"非法 TenantScope", func(m *httpx.RouteMeta) { m.TenantScope = "all" }, "TenantScope"},
		{"public 与 scope 不一致", func(m *httpx.RouteMeta) { m.TenantScope = httpx.ScopePublic }, "当且仅当"},
		{"缺 Permissions(nil)", func(m *httpx.RouteMeta) { m.Permissions = nil }, "缺少 Permissions"},
		{"权限串非法", func(m *httpx.RouteMeta) { m.Permissions = []string{"SiteRead"} }, "权限串"},
		{"权限串重复", func(m *httpx.RouteMeta) { m.Permissions = []string{"site:read", "site:read"} }, "重复"},
		{"缺 Phase", func(m *httpx.RouteMeta) { m.Phase = "" }, "Phase"},
		{"非法 Phase", func(m *httpx.RouteMeta) { m.Phase = "later" }, "Phase"},
		{"Idempotent 非 POST", func(m *httpx.RouteMeta) { m.Idempotent = true }, "仅适用于 POST"},
		{"空路径", func(m *httpx.RouteMeta) { m.Path = "" }, "Path"},
		{"路径无前导斜杠", func(m *httpx.RouteMeta) { m.Path = "sites" }, "Path"},
		{"路径含冒号(IMP-21)", func(m *httpx.RouteMeta) { m.Path = "/sites/{siteId}/rules:validate" }, "IMP-21"},
		{"路径含冒号参数写法", func(m *httpx.RouteMeta) { m.Path = "/sites/:siteId" }, "IMP-21"},
		{"路径含通配", func(m *httpx.RouteMeta) { m.Path = "/sites/*all" }, "非法字符"},
		{"路径尾斜杠", func(m *httpx.RouteMeta) { m.Path = "/sites/" }, "不得"},
		{"路径空段", func(m *httpx.RouteMeta) { m.Path = "/sites//rules" }, "不得"},
		{"参数未占满整段", func(m *httpx.RouteMeta) { m.Path = "/sites/x{id}" }, "路径段"},
		{"参数名重复", func(m *httpx.RouteMeta) { m.Path = "/a/{id}/b/{id}" }, "重复"},
	}
	for _, c := range cases {
		m := meta("listSites", "GET", "/sites")
		c.mut(&m)
		probs := m.Validate()
		if len(probs) == 0 || !strings.Contains(strings.Join(probs, "|"), c.want) {
			t.Errorf("%s: 期望含 %q，实际 %v", c.name, c.want, probs)
		}
	}
	// 公开路由不得声明权限/Reauth。
	p := pub
	p.Permissions = []string{"site:read"}
	if len(p.Validate()) == 0 {
		t.Error("公开路由声明权限应被拒绝")
	}
	p = pub
	p.Reauth = true
	if len(p.Validate()) == 0 {
		t.Error("公开路由声明 Reauth 应被拒绝")
	}
}

func TestRegisterFailuresMakeHandlerFail(t *testing.T) {
	noop := gin.HandlerFunc(func(c *gin.Context) {})
	bad := []struct {
		name string
		m    httpx.RouteMeta
		h    gin.HandlerFunc
	}{
		{"元数据缺失", httpx.RouteMeta{OperationID: "x"}, noop},
		{"handler 为空", meta("a", "GET", "/a"), nil},
		{"路径含冒号", meta("a", "POST", "/sites/{siteId}/rules:validate"), noop},
		{"Idempotent 无存储", func() httpx.RouteMeta { m := meta("a", "POST", "/a"); m.Idempotent = true; return m }(), noop},
		{"公开 + Idempotent", httpx.RouteMeta{OperationID: "p", Method: "POST", Path: "/p", Auth: httpx.AuthPublic, Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopePublic, Phase: "M1", Idempotent: true}, noop},
	}
	for _, c := range bad {
		r, _ := newRouter(t, httpx.Options{})
		err := r.Register(c.m, c.h)
		if err == nil {
			t.Errorf("%s: Register 应返回错误", c.name)
		}
		// 即使调用方忽略了错误，Handler() 仍然失败（启动失败，NFR-4）。
		if _, herr := r.Handler(); herr == nil {
			t.Errorf("%s: Handler() 应失败", c.name)
		}
	}

	// 重复 operationId / 重复方法+路径 / gin 通配符冲突。
	r, _ := newRouter(t, httpx.Options{})
	if err := r.Register(meta("a", "GET", "/a"), noop); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(meta("a", "GET", "/b"), noop); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Errorf("重复 operationId 应失败: %v", err)
	}
	if err := r.Register(meta("b", "GET", "/a"), noop); err == nil {
		t.Error("重复方法+路径应失败")
	}
	if err := r.Register(meta("c", "GET", "/s/{siteId}"), noop); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(meta("d", "GET", "/s/{id}/x"), noop); err == nil {
		t.Error("同位置不同参数名（gin 通配符冲突）应以错误而非 panic 返回")
	}
	if _, err := r.Handler(); err == nil {
		t.Error("存在注册错误时 Handler() 必须失败")
	}
}

// IMP-21 回归：冒号路径无法注册；改名后的名词子资源只命中精确路径，/rulesXXX 之类不会误命中。
func TestIMP21ColonRegression(t *testing.T) {
	r, _ := newRouter(t, httpx.Options{})
	if err := r.Register(meta("validateRules", "POST", "/sites/{siteId}/rules:validate"), okHandler("x")); err == nil {
		t.Fatal("含 ':' 的路径必须被注册层拒绝")
	}
	oh := &orderHooks{p: &testPrincipal{actor: 1, tenant: 1}}
	r2, _ := newRouter(t, httpx.Options{Hooks: oh.hooks()})
	if err := r2.Register(meta("createRuleValidation", "POST", "/sites/{siteId}/rule-validations"), okHandler("ok")); err != nil {
		t.Fatal(err)
	}
	h := handler(t, r2)
	if rec := do(h, "POST", "/api/v1/sites/S1/rule-validations", ""); rec.Code != 200 {
		t.Errorf("改名后的路径应命中: %d", rec.Code)
	}
	for _, p := range []string{"/api/v1/sites/S1/rule-validationsXXX", "/api/v1/sites/S1/rules", "/api/v1/sites/S1/rule-validations/x"} {
		if rec := do(h, "POST", p, ""); rec.Code != 404 {
			t.Errorf("%s 不应命中任何路由: %d", p, rec.Code)
		}
	}
}
