package authit

import (
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/rbac"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// AC6：授权矩阵（HTTP 级、表驱动、真实钩子链）——主体类型（platform/tenant）× x-tenant-scope（platform/tenant/either）×
// 有无/哪种 X-Tenant-ID × 有无 tenant:impersonate × 有无路由权限。
// 决策表本身的穷举（192 种组合不变量）在 internal/platform/rbac 的单测里；这里证明它被真实装配进钩子链，并且：
//   - 授权钩子放行 acting-as 时把决策（ModeActingAs）交给 Scope 钩子；
//   - 但 acting-as 需要解析/校验 X-Tenant-ID 指向的租户（Story 1.20 的 Scope 钩子 + 租户目录）；本矩阵的装配不接租户目录（auth.HooksOptions{}），
//     因此保持安全拒绝：请求 403、handler 不执行（「未装配租户目录 → fail-closed」的不变量）。有租户目录的 acting-as 见 tests/security/tenant_isolation。
func TestAuthorizationMatrixHTTP(t *testing.T) {
	probe := func(r *httpx.Router, id, method, path string, scope httpx.TenantScope, perms []string) {
		m := httpx.RouteMeta{OperationID: id, Method: method, Path: path, Auth: httpx.AuthRequired, Permissions: perms, TenantScope: scope, Phase: "M1"}
		if err := r.Register(m, func(c *gin.Context) {
			sc, _ := tenantscope.FromContext(c.Request.Context())
			httpx.OK(c, map[string]any{"reached": true, "platform": sc.IsPlatform(), "tenant_id": sc.TenantID(), "acting_as": sc.ActingAs()})
		}); err != nil {
			t.Fatal(err)
		}
	}
	s := newStack(t, stackOpts{spyScope: true, routes: func(r *httpx.Router) {
		probe(r, "mxPlatform", "GET", "/probe/mx/platform", httpx.ScopePlatform, []string{"user:read"})
		probe(r, "mxEitherRead", "GET", "/probe/mx/either", httpx.ScopeEither, []string{"site:read"})
		probe(r, "mxEitherWrite", "POST", "/probe/mx/either", httpx.ScopeEither, []string{"site:write"})
		probe(r, "mxTenant", "POST", "/probe/mx/tenant", httpx.ScopeTenant, []string{"site:write"})
		probe(r, "mxOpen", "GET", "/probe/mx/open", httpx.ScopeEither, httpx.NoPermissions())
	}})

	// 平台主体。
	admin := s.adminSession("platform_admin")                                 // 全部权限，含 tenant:impersonate
	auditor := s.adminSession("platform_auditor")                             // 只读，无 impersonate
	operator := s.adminSession("platform_operator")                           // 含 site:write、tenant:impersonate
	siteReader := s.adminSession(s.customRole("platform", "site:read"))       // 有路由权限，无 impersonate
	impOnly := s.adminSession(s.customRole("platform", rbac.Impersonate))     // 有 impersonate，无路由权限
	siteWriterNoImp := s.adminSession(s.customRole("platform", "site:write")) // 有写权限，无 impersonate
	// 租户主体（Console 会话；各自独立的租户）。
	mkTenant := func(role string) (user, string, string) {
		u := s.seedUser("tenant")
		if role != "" {
			s.grantTenantRole(u, role)
		}
		_, tok, csrf := s.login(u, freshIP())
		return u, tok, csrf
	}
	owner, ownerTok, ownerCSRF := mkTenant("tenant_owner")
	viewer, viewerTok, viewerCSRF := mkTenant("tenant_viewer")
	none, noneTok, noneCSRF := mkTenant("")
	other, _, _ := mkTenant("tenant_owner") // 他人的租户
	tenantUID := func(u user) string { return s.e.rootStr(t, "SELECT uid FROM §.tenants WHERE id = ?", u.TenantID) }
	ownerT, otherT := tenantUID(owner), tenantUID(other)

	platformScope := wantScope{platform: true}
	tenantScopeOf := func(u user) wantScope { return wantScope{tenantID: u.TenantID} }
	type who struct {
		name string
		call func(method, path, hdr string) resp
	}
	plat := func(n string, ss session) who {
		return who{n, func(m, p, h string) resp {
			var o []reqOpt
			if h != "" {
				o = append(o, withHeader("X-Tenant-ID", h))
			}
			var body any
			if m == "POST" {
				body = map[string]any{}
			}
			return s.api(ss, m, p, body, o...)
		}}
	}
	ten := func(n, tok, csrf string) who {
		return who{n, func(m, p, h string) resp {
			var o []reqOpt
			if h != "" {
				o = append(o, withHeader("X-Tenant-ID", h))
			}
			var body any
			if m == "POST" {
				body = map[string]any{}
			}
			return s.authed("console", tok, csrf, m, p, body, o...)
		}}
	}
	pAdmin, pAud, pOp := plat("平台·超管", admin), plat("平台·审计员", auditor), plat("平台·运维", operator)
	pSite, pImp, pWriter := plat("平台·仅 site:read", siteReader), plat("平台·仅 impersonate", impOnly), plat("平台·仅 site:write", siteWriterNoImp)
	tOwner, tViewer, tNone := ten("租户·owner", ownerTok, ownerCSRF), ten("租户·viewer", viewerTok, viewerCSRF), ten("租户·无角色", noneTok, noneCSRF)
	someTenant := ids.MustNew() // 任意合法 uid（本装配不接租户目录，不校验其存在性；校验见 tests/security/tenant_isolation）

	const (
		platPath, eitherPath, tenantPath, openPath = "/probe/mx/platform", "/probe/mx/either", "/probe/mx/tenant", "/probe/mx/open"
	)
	type row struct {
		w          who
		method     string
		path       string
		hdr        string
		wantStatus int
		wantMode   rbac.Mode // 授权钩子交给 Scope 钩子的决策；空 = 授权钩子拒绝（没有决策）
		wantScope  wantScope // 200 时 handler 看到的 Scope
	}
	rows := []row{
		// ---- platform 路由 ----
		{pAdmin, "GET", platPath, "", 200, rbac.ModePlatform, platformScope},
		{pAud, "GET", platPath, "", 200, rbac.ModePlatform, platformScope},
		{pSite, "GET", platPath, "", 403, "", wantScope{}},
		{pImp, "GET", platPath, "", 403, "", wantScope{}},
		{pAdmin, "GET", platPath, someTenant, 400, "", wantScope{}}, // 平台级资源不接受目标租户
		{pSite, "GET", platPath, someTenant, 403, "", wantScope{}},  // 先权限后形态
		{tOwner, "GET", platPath, "", 401, "", wantScope{}},         // realm 隔离：Console 会话进不了 platform 路由
		// ---- either 路由（读）----
		{pAdmin, "GET", eitherPath, "", 200, rbac.ModeCrossTenantRead, platformScope}, // 跨租户只读
		{pAud, "GET", eitherPath, "", 200, rbac.ModeCrossTenantRead, platformScope},
		{pSite, "GET", eitherPath, "", 200, rbac.ModeCrossTenantRead, platformScope},
		{pImp, "GET", eitherPath, "", 403, "", wantScope{}},                          // 只有 impersonate、没有路由权限
		{pWriter, "GET", eitherPath, "", 403, "", wantScope{}},                       // 有 site:write 但路由要 site:read
		{pAdmin, "GET", eitherPath, someTenant, 403, rbac.ModeActingAs, wantScope{}}, // 授权通过 → acting-as；本装配未接租户目录 → Scope 钩子安全拒绝
		{pOp, "GET", eitherPath, someTenant, 403, rbac.ModeActingAs, wantScope{}},    // 运维：impersonate + 路由权限
		{pAud, "GET", eitherPath, someTenant, 403, "", wantScope{}},                  // 缺 impersonate
		{pSite, "GET", eitherPath, someTenant, 403, "", wantScope{}},                 // 缺 impersonate
		{pImp, "GET", eitherPath, someTenant, 403, "", wantScope{}},                  // 缺路由权限
		{tOwner, "GET", eitherPath, "", 200, rbac.ModeOwnTenant, tenantScopeOf(owner)},
		{tViewer, "GET", eitherPath, "", 200, rbac.ModeOwnTenant, tenantScopeOf(viewer)},
		{tNone, "GET", eitherPath, "", 403, "", wantScope{}},
		{tOwner, "GET", eitherPath, ownerT, 200, rbac.ModeOwnTenant, tenantScopeOf(owner)},
		{tOwner, "GET", eitherPath, otherT, 404, "", wantScope{}}, // 他人的租户：404，不泄漏存在性
		{tViewer, "GET", eitherPath, otherT, 404, "", wantScope{}},
		{tNone, "GET", eitherPath, otherT, 404, "", wantScope{}}, // 先 404 后权限：无权限者也无法探测租户存在性
		{tOwner, "GET", eitherPath, "not-a-uid", 400, "", wantScope{}},
		{pAdmin, "GET", eitherPath, "not-a-uid", 400, "", wantScope{}},
		// ---- either 路由（写）----
		{pAdmin, "POST", eitherPath, "", 400, "", wantScope{}}, // 平台写操作必须指定租户（跨租户只读）
		{pOp, "POST", eitherPath, "", 400, "", wantScope{}},
		{pAud, "POST", eitherPath, "", 403, "", wantScope{}}, // 无 site:write
		{pAdmin, "POST", eitherPath, someTenant, 403, rbac.ModeActingAs, wantScope{}},
		{pOp, "POST", eitherPath, someTenant, 403, rbac.ModeActingAs, wantScope{}},
		{pWriter, "POST", eitherPath, someTenant, 403, "", wantScope{}}, // 有 site:write，缺 impersonate
		{pAud, "POST", eitherPath, someTenant, 403, "", wantScope{}},
		{tOwner, "POST", eitherPath, "", 200, rbac.ModeOwnTenant, tenantScopeOf(owner)},
		{tViewer, "POST", eitherPath, "", 403, "", wantScope{}},
		{tOwner, "POST", eitherPath, otherT, 404, "", wantScope{}},
		// ---- tenant 路由 ----
		{pAdmin, "POST", tenantPath, "", 400, "", wantScope{}}, // 平台主体只能经 acting-as
		{pOp, "POST", tenantPath, "", 400, "", wantScope{}},
		{pAud, "POST", tenantPath, "", 403, "", wantScope{}},
		{pWriter, "POST", tenantPath, "", 400, "", wantScope{}}, // 权限够，但缺 X-Tenant-ID
		{pAdmin, "POST", tenantPath, someTenant, 403, rbac.ModeActingAs, wantScope{}},
		{pOp, "POST", tenantPath, someTenant, 403, rbac.ModeActingAs, wantScope{}},
		{pWriter, "POST", tenantPath, someTenant, 403, "", wantScope{}}, // 缺 impersonate
		{pImp, "POST", tenantPath, someTenant, 403, "", wantScope{}},    // 缺路由权限
		{tOwner, "POST", tenantPath, "", 200, rbac.ModeOwnTenant, tenantScopeOf(owner)},
		{tViewer, "POST", tenantPath, "", 403, "", wantScope{}},
		{tOwner, "POST", tenantPath, otherT, 404, "", wantScope{}},
		// ---- 无权限要求（任何已认证主体）----
		{pAdmin, "GET", openPath, "", 200, rbac.ModeCrossTenantRead, platformScope},
		{pImp, "GET", openPath, "", 200, rbac.ModeCrossTenantRead, platformScope},
		{tNone, "GET", openPath, "", 200, rbac.ModeOwnTenant, tenantScopeOf(none)},
	}

	for _, r := range rows {
		name := r.w.name + " " + r.method + " " + r.path + " hdr=" + hdrName(r.hdr, ownerT, otherT, someTenant)
		t.Run(name, func(t *testing.T) {
			s.takeAccess()
			resp := r.w.call(r.method, r.path, r.hdr)
			if resp.Status != r.wantStatus {
				t.Fatalf("期望 %d，得到 %d %s", r.wantStatus, resp.Status, resp.Body)
			}
			d := s.takeAccess()
			switch {
			case r.wantMode == "" && d != nil:
				t.Errorf("授权钩子应拒绝，却交出了决策 %+v", *d)
			case r.wantMode != "" && (d == nil || d.Mode != r.wantMode):
				t.Errorf("授权决策应为 %s，实际 %v", r.wantMode, d)
			}
			if r.wantStatus == 200 {
				j := resp.JSON(t)
				got := wantScope{platform: j["platform"] == true, tenantID: uint64(j["tenant_id"].(float64)), acting: j["acting_as"] == true}
				if j["reached"] != true || got != r.wantScope {
					t.Errorf("handler 看到的 Scope 不对: %s（期望 %+v）", resp.Body, r.wantScope)
				}
			} else if strings.Contains(string(resp.Body), "reached") {
				t.Errorf("被拒绝的请求不得到达 handler: %s", resp.Body)
			}
			if resp.Status == 403 {
				if code := resp.errCode(t); code != "auth.forbidden" {
					t.Errorf("403 应为 auth.forbidden: %s", code)
				}
			}
		})
	}
	// 自助 operation 不做代租户：带 X-Tenant-ID 也只作用于自己（忽略头）。
	r := s.api(admin, "GET", "/auth/me", nil, withHeader("X-Tenant-ID", someTenant))
	if r.Status != 200 {
		t.Errorf("/auth/me 不受 X-Tenant-ID 影响: %d %s", r.Status, r.Body)
	}
}

func hdrName(h, own, other, some string) string {
	switch h {
	case "":
		return "无"
	case own:
		return "自己的租户"
	case other:
		return "他人的租户"
	case some:
		return "任意租户"
	}
	return "非法"
}

// wantScope 是 handler 看到的 tenantscope.Scope 的可比较投影。
type wantScope struct {
	platform bool
	tenantID uint64
	acting   bool
}
