package sysconfigit

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// Story 1.22 AC2：feature.disabled 中间件。M1 尚无真实商业路由，用**测试专用合成路由**（仅在本测试文件里经真实 Router.Register 注册，
// 走真实钩子链与真实运行模式；不进生产构建，满足 testdouble 门禁）验证：自用模式 403 feature.disabled、商业模式放行、切换立即生效。
func probeRoutes(w *apistack.World, r *httpx.Router) {
	ok := func(c *gin.Context) { httpx.OK(c, map[string]bool{"ok": true}) }
	must := func(err error) {
		if err != nil {
			w.T.Fatal(err)
		}
	}
	must(r.Register(httpx.RouteMeta{OperationID: "probeCommercialAdmin", Method: "GET", Path: "/probe/commercial-admin", Auth: httpx.AuthRequired,
		Permissions: []string{"setting:read"}, TenantScope: httpx.ScopePlatform, Phase: "M1", Feature: "commercial"}, ok))
	must(r.Register(httpx.RouteMeta{OperationID: "probeCommercialPublic", Method: "GET", Path: "/probe/commercial-public", Auth: httpx.AuthPublic,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopePublic, Phase: "M1", Feature: "commercial"}, ok))
	must(r.Register(httpx.RouteMeta{OperationID: "probePlainAdmin", Method: "GET", Path: "/probe/plain-admin", Auth: httpx.AuthRequired,
		Permissions: []string{"setting:read"}, TenantScope: httpx.ScopePlatform, Phase: "M1"}, ok))
	must(r.Register(httpx.RouteMeta{OperationID: "probeCommercialTenant", Method: "GET", Path: "/probe/commercial-tenant", Auth: httpx.AuthRequired,
		Permissions: []string{"task:read"}, TenantScope: httpx.ScopeTenant, Phase: "M1", Feature: "commercial"}, ok))
}

func TestFeatureDisabledInSelfHostedMode(t *testing.T) {
	w := apistack.New(t, apistack.Options{Routes: probeRoutes})
	adm := admin(w)
	tn := w.SeedTenant("feat")
	tu := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(tu, "tenant_owner")
	ts := w.Login(tu)

	assertDisabled := func(what string, r apistack.Resp) {
		t.Helper()
		if r.Status != http.StatusForbidden || r.ErrCode(t) != "feature.disabled" {
			t.Fatalf("%s：自用模式应 403 feature.disabled: %d %s", what, r.Status, r.Body)
		}
	}
	// 自用模式（默认）：商业路由 403 feature.disabled（管理员、租户、令牌、公开路由一视同仁）；非商业路由不受影响。
	tok := adm.IssueToken("feature-probe", "setting:read")
	assertDisabled("管理员", adm.Do("GET", "/probe/commercial-admin", nil))
	assertDisabled("平台令牌", w.Bearer(tok, "GET", "/probe/commercial-admin", nil))
	assertDisabled("租户会话", ts.Do("GET", "/probe/commercial-tenant", nil))
	assertDisabled("公开路由", w.Do("GET", "/probe/commercial-public", nil))
	if r := adm.Do("GET", "/probe/plain-admin", nil); r.Status != 200 {
		t.Fatalf("未声明 x-feature 的路由不受功能开关影响: %d %s", r.Status, r.Body)
	}
	// 认证先于功能开关：匿名访问私有商业路由得到 401（不向匿名者暴露哪些功能被关闭）。
	if r := w.Do("GET", "/probe/commercial-admin", nil); r.Status != 401 {
		t.Fatalf("匿名访问私有商业路由应 401: %d %s", r.Status, r.Body)
	}
	// 功能开关先于授权：无任何权限的平台用户在自用模式得到 feature.disabled，而不是 auth.forbidden；商业模式下才暴露为 403 forbidden。
	nobody := w.SeedUser("platform", nil)
	nb := w.Login(nobody)
	assertDisabled("无权限用户（认证后、授权前拒绝）", nb.Do("GET", "/probe/commercial-admin", nil))

	// 切换到商业模式：立即放行。
	adm.Reauth()
	if r := adm.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": "commercial"}); r.Status != 200 {
		t.Fatalf("切换: %d %s", r.Status, r.Body)
	}
	if r := adm.Do("GET", "/probe/commercial-admin", nil); r.Status != 200 {
		t.Fatalf("商业模式应放行: %d %s", r.Status, r.Body)
	}
	if r := w.Bearer(tok, "GET", "/probe/commercial-admin", nil); r.Status != 200 {
		t.Fatalf("商业模式令牌应放行: %d %s", r.Status, r.Body)
	}
	if r := ts.Do("GET", "/probe/commercial-tenant", nil); r.Status != 200 {
		t.Fatalf("商业模式租户应放行: %d %s", r.Status, r.Body)
	}
	if r := w.Do("GET", "/probe/commercial-public", nil); r.Status != 200 {
		t.Fatalf("商业模式公开路由应放行: %d %s", r.Status, r.Body)
	}
	if r := nb.Do("GET", "/probe/commercial-admin", nil); r.Status != 403 || r.ErrCode(t) != "auth.forbidden" {
		t.Fatalf("商业模式下无权限用户由授权钩子拒绝: %d %s", r.Status, r.Body)
	}

	// 回切：立即再次关闭，无需重启。
	if r := adm.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": "self_hosted"}); r.Status != 200 {
		t.Fatalf("回切: %d %s", r.Status, r.Body)
	}
	assertDisabled("回切后管理员", adm.Do("GET", "/probe/commercial-admin", nil))
	assertDisabled("回切后公开路由", w.Do("GET", "/probe/commercial-public", nil))
}

// 运行模式设置值被带外写成非法值时：读取回退默认（self_hosted），商业路由保持关闭（fail-closed，不会因脏数据放行）。
func TestFeatureGateIgnoresCorruptRunMode(t *testing.T) {
	w := apistack.New(t, apistack.Options{Routes: probeRoutes})
	w.Root("INSERT INTO §.system_settings (uid, setting_key, value_json, group_name) VALUES (?, 'system.run_mode', CAST('\"enterprise\"' AS JSON), 'general')", apistack.IDs.MustNew())
	if r := w.Do("GET", "/probe/commercial-public", nil); r.Status != 403 || r.ErrCode(t) != "feature.disabled" {
		t.Fatalf("脏运行模式值不得放行商业路由: %d %s", r.Status, r.Body)
	}
	adm := admin(w)
	if g := adm.Do("GET", "/admin/system/run-mode", nil); g.JSON(t)["run_mode"] != "self_hosted" {
		t.Fatalf("脏值应回退默认: %s", g.Body)
	}
}
