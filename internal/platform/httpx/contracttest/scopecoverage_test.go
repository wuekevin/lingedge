package contracttest_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

func reg(t *testing.T, r *httpx.Router, id, method, path string, scope httpx.TenantScope, auth httpx.Auth, idem bool) httpx.RouteMeta {
	t.Helper()
	m := httpx.RouteMeta{OperationID: id, Method: method, Path: path, Auth: auth, TenantScope: scope, Idempotent: idem, Phase: "M1"}
	m.Permissions = []string{"x:read"}
	if auth == httpx.AuthPublic {
		m.Permissions = httpx.NoPermissions()
	}
	if err := r.Register(m, func(c *gin.Context) { httpx.OK(c, map[string]bool{"reached": true}) }); err != nil {
		t.Fatal(err)
	}
	return m
}

func newRouter(t *testing.T, h httpx.Hooks) *httpx.Router {
	t.Helper()
	r, err := httpx.NewRouter(httpx.Options{IDs: uid.NewGenerator(clock.System()), Hooks: h, Idempotency: contracttest.NopIdempotencyStore{}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 正样本：真实的 Router 把每条需认证的路由都接在 Scope 钩子之后——覆盖检查通过。
func TestCheckScopeCoveragePassesForRealRouter(t *testing.T) {
	build := func(h httpx.Hooks) (http.Handler, []httpx.RouteMeta, error) {
		r := newRouter(t, h)
		reg(t, r, "listWidgets", "GET", "/widgets", httpx.ScopeEither, httpx.AuthRequired, false)
		reg(t, r, "getWidget", "GET", "/widgets/{widgetId}", httpx.ScopeTenant, httpx.AuthRequired, false)
		reg(t, r, "createWidget", "POST", "/widgets", httpx.ScopeTenant, httpx.AuthRequired, true)
		reg(t, r, "listNodes", "GET", "/nodes", httpx.ScopePlatform, httpx.AuthRequired, false)
		reg(t, r, "login", "POST", "/auth/login", httpx.ScopePublic, httpx.AuthPublic, false)
		hd, err := r.Handler()
		return hd, r.Routes(), err
	}
	if p := contracttest.CheckScopeCoverage(build); len(p) > 0 {
		t.Fatalf("真实路由不应有覆盖问题: %v", p)
	}
}

// 负向样本 1：某条 tenant 路由被旁路 mux 直接挂到 handler（不经任何钩子）——必须被发现并点名。
func TestCheckScopeCoverageDetectsBypassedRoute(t *testing.T) {
	build := func(h httpx.Hooks) (http.Handler, []httpx.RouteMeta, error) {
		r := newRouter(t, h)
		good := reg(t, r, "listWidgets", "GET", "/widgets", httpx.ScopeEither, httpx.AuthRequired, false)
		real, err := r.Handler()
		if err != nil {
			return nil, nil, err
		}
		mux := http.NewServeMux()
		mux.Handle("/api/v1/leaky/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"leak":true}`)) }))
		mux.Handle("/", real)
		leaky := httpx.RouteMeta{OperationID: "getLeaky", Method: "GET", Path: "/leaky/{leakyId}", Auth: httpx.AuthRequired, TenantScope: httpx.ScopeTenant, Phase: "M1"}
		return mux, []httpx.RouteMeta{good, leaky}, nil
	}
	p := contracttest.CheckScopeCoverage(build)
	if len(p) != 1 || !strings.Contains(p[0], "getLeaky") || !strings.Contains(p[0], "未经 tenantscope") {
		t.Fatalf("应只点名被旁路的 getLeaky: %v", p)
	}
}

// 负向样本 2：装配方忽略探针钩子、自带一套宽松的 Scope 钩子（放行一切）——所有 tenant|either 路由都应被判定为未经（探针的）Scope 钩子。
func TestCheckScopeCoverageDetectsPrivateScopeHook(t *testing.T) {
	build := func(_ httpx.Hooks) (http.Handler, []httpx.RouteMeta, error) {
		r := newRouter(t, httpx.Hooks{}) // 默认钩子：认证 401，请求到不了 Scope 钩子
		m := reg(t, r, "getWidget", "GET", "/widgets/{widgetId}", httpx.ScopeTenant, httpx.AuthRequired, false)
		hd, err := r.Handler()
		return hd, []httpx.RouteMeta{m}, err
	}
	if p := contracttest.CheckScopeCoverage(build); len(p) != 1 || !strings.Contains(p[0], "getWidget") {
		t.Fatalf("不经探针 Scope 钩子的路由应被发现: %v", p)
	}
}

// 负向样本 3：路由集合为空不能空转通过；装配失败要报告。
func TestCheckScopeCoverageDoesNotPassVacuously(t *testing.T) {
	empty := func(h httpx.Hooks) (http.Handler, []httpx.RouteMeta, error) {
		r := newRouter(t, h)
		hd, err := r.Handler()
		return hd, r.Routes(), err
	}
	if p := contracttest.CheckScopeCoverage(empty); len(p) != 1 || !strings.Contains(p[0], "不能空转") {
		t.Fatalf("空路由集合应失败: %v", p)
	}
	failing := func(httpx.Hooks) (http.Handler, []httpx.RouteMeta, error) { return nil, nil, http.ErrAbortHandler }
	if p := contracttest.CheckScopeCoverage(failing); len(p) != 1 || !strings.Contains(p[0], "装配路由失败") {
		t.Fatalf("装配失败应报告: %v", p)
	}
}
