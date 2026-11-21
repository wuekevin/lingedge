package httpx_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// gateFake 是功能开关钩子的测试实现（记录调用顺序，按名称返回是否启用）。
type gateFake struct {
	oh      *orderHooks
	enabled map[string]bool
	err     error
	mu      sync.Mutex
	asked   []string
}

func (g *gateFake) Enabled(_ context.Context, f string) (bool, error) {
	g.mu.Lock()
	g.asked = append(g.asked, f)
	g.mu.Unlock()
	if g.oh != nil {
		g.oh.add("feature")
	}
	return g.enabled[f], g.err
}

func commercial(m httpx.RouteMeta) httpx.RouteMeta { m.Feature = "commercial"; return m }

// Story 1.22 AC2：声明了 Feature 的路由——功能未启用 → 403 feature.disabled，且 handler/授权/Scope/审计都不执行；位置在认证之后、授权之前。
func TestFeatureGateOrderAndDisabled(t *testing.T) {
	oh := &orderHooks{p: &testPrincipal{actor: 7, tenant: 3}}
	g := &gateFake{oh: oh, enabled: map[string]bool{"commercial": false}}
	h := oh.hooks()
	h.Features = g
	r, _ := newRouter(t, httpx.Options{Hooks: h})
	handled := false
	if err := r.Register(commercial(meta("listOrders", "GET", "/billing/orders")), func(c *gin.Context) { handled = true; httpx.OK(c, map[string]bool{"ok": true}) }); err != nil {
		t.Fatal(err)
	}
	rec := do(handler(t, r), "GET", "/api/v1/billing/orders", "")
	if rec.Code != 403 {
		t.Fatalf("自用模式访问商业路由应 403: %d %s", rec.Code, rec.Body)
	}
	if e := decodeErr(t, rec.Body.Bytes()); e.Code != "feature.disabled" {
		t.Fatalf("错误码应为 feature.disabled: %+v", e)
	}
	if got, want := oh.get(), []string{"authn", "feature"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("调用顺序 = %v，期望 %v（认证之后、授权之前拒绝，授权/Scope/审计/handler 均不执行）", got, want)
	}
	if handled {
		t.Fatal("handler 不应执行")
	}
	// 启用后放行，顺序为 authn → feature → authz → scope → audit → handler。
	g.enabled["commercial"] = true
	oh.trace = nil
	rec = do(handler(t, r), "GET", "/api/v1/billing/orders", "")
	if rec.Code != 200 {
		t.Fatalf("商业模式应放行: %d %s", rec.Code, rec.Body)
	}
	if got, want := oh.get(), []string{"authn", "feature", "authz", "scope", "audit", "audit-end"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("放行顺序 = %v，期望 %v", got, want)
	}
}

// 未声明 Feature 的路由不调用功能开关；开关读取失败 → 500（fail-closed）；未装配开关 → 默认安全拒绝。
func TestFeatureGateFailClosedAndDefaults(t *testing.T) {
	oh := &orderHooks{p: &testPrincipal{actor: 7, tenant: 3}}
	g := &gateFake{enabled: map[string]bool{"commercial": true}}
	h := oh.hooks()
	h.Features = g
	r, _ := newRouter(t, httpx.Options{Hooks: h})
	if err := r.Register(meta("listSites", "GET", "/sites"), okHandler("s")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(commercial(meta("listOrders", "GET", "/billing/orders")), okHandler("o")); err != nil {
		t.Fatal(err)
	}
	hd := handler(t, r)
	if rec := do(hd, "GET", "/api/v1/sites", ""); rec.Code != 200 || len(g.asked) != 0 {
		t.Fatalf("未声明 Feature 的路由不应受功能开关影响: %d asked=%v", rec.Code, g.asked)
	}
	g.err = errors.New("settings unavailable")
	rec := do(hd, "GET", "/api/v1/billing/orders", "")
	if rec.Code != 500 || decodeErr(t, rec.Body.Bytes()).Code != "internal.error" {
		t.Fatalf("开关读取失败应 500 internal.error（不得放行）: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "settings unavailable") {
		t.Fatalf("内部原因不得出现在响应里: %s", rec.Body)
	}

	// 未装配（Features=nil）→ DenyFeatures。
	r2, _ := newRouter(t, httpx.Options{Hooks: oh.hooks()})
	if err := r2.Register(commercial(meta("listOrders", "GET", "/billing/orders")), okHandler("o")); err != nil {
		t.Fatal(err)
	}
	if rec := do(handler(t, r2), "GET", "/api/v1/billing/orders", ""); rec.Code != 403 || decodeErr(t, rec.Body.Bytes()).Code != "feature.disabled" {
		t.Fatalf("未装配功能开关应默认 403 feature.disabled: %d %s", rec.Code, rec.Body)
	}
}

// 公开路由（如 /public/plans）同样受功能开关保护；匿名访问私有商业路由先得到 401（认证先于功能开关）。
func TestFeatureGatePublicAndAnonymous(t *testing.T) {
	g := &gateFake{enabled: map[string]bool{}}
	h := httpx.Hooks{Features: g} // 认证钩子为默认拒绝：私有路由匿名 → 401
	r, _ := newRouter(t, httpx.Options{Hooks: h})
	pub := httpx.RouteMeta{OperationID: "listPublicPlans", Method: "GET", Path: "/public/plans", Auth: httpx.AuthPublic, Permissions: httpx.NoPermissions(),
		TenantScope: httpx.ScopePublic, Phase: "M1", Feature: "commercial"}
	if err := r.Register(pub, okHandler("p")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(commercial(meta("listOrders", "GET", "/billing/orders")), okHandler("o")); err != nil {
		t.Fatal(err)
	}
	hd := handler(t, r)
	if rec := do(hd, "GET", "/api/v1/public/plans", ""); rec.Code != 403 || decodeErr(t, rec.Body.Bytes()).Code != "feature.disabled" {
		t.Fatalf("公开商业路由在自用模式应 403 feature.disabled: %d %s", rec.Code, rec.Body)
	}
	if rec := do(hd, "GET", "/api/v1/billing/orders", ""); rec.Code != 401 {
		t.Fatalf("匿名访问私有商业路由应先 401（不向匿名者暴露功能开关）: %d %s", rec.Code, rec.Body)
	}
	g.enabled["commercial"] = true
	if rec := do(hd, "GET", "/api/v1/public/plans", ""); rec.Code != 200 {
		t.Fatalf("商业模式应放行: %d %s", rec.Code, rec.Body)
	}
}

func TestFeatureMetaValidationAndManifest(t *testing.T) {
	m := meta("listOrders", "GET", "/billing/orders")
	m.Feature = "Commercial!"
	if probs := m.Validate(); len(probs) == 0 {
		t.Error("非法 Feature 取值应被拒绝")
	}
	m.Feature = "commercial"
	if probs := m.Validate(); len(probs) != 0 {
		t.Errorf("合法 Feature 不应有问题: %v", probs)
	}
	r, _ := newRouter(t, httpx.Options{})
	if err := r.Register(m, okHandler("o")); err != nil {
		t.Fatal(err)
	}
	if got := r.Manifest(); len(got) != 1 || got[0].Feature != "commercial" {
		t.Fatalf("清单应带 feature: %+v", got)
	}
}
