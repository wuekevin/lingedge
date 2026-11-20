package authit

import (
	"sort"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/rbac"
)

// AC1/AC2：代码注册表 ⇄ 种子（真实 MySQL 上应用全部迁移后的 permissions 表）⇄ OpenAPI x-permissions；内置系统角色随种子迁移写入；
// 租户角色只含 scope=tenant/both 的权限；没有任何默认账号/口令（G-6、NFR-6）。
func TestSeedMatchesRegistryAndOpenAPI(t *testing.T) {
	s := newStack(t)
	// 注册表 == permissions 表（逐项：权限串、scope、module、resource、action）。
	rows, err := s.e.adm.QueryContext(ctxT(t), "SELECT code, resource, action, scope, module FROM `"+s.e.dbName+"`.permissions ORDER BY code")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]rbac.Permission{}
	for rows.Next() {
		var p rbac.Permission
		var sc string
		if err := rows.Scan(&p.Code, &p.Resource, &p.Action, &sc, &p.Module); err != nil {
			t.Fatal(err)
		}
		p.Scope = rbac.Scope(sc)
		got[p.Code] = p
	}
	reg := rbac.All()
	if len(got) != len(reg) {
		t.Fatalf("permissions 表 %d 项，注册表 %d 项", len(got), len(reg))
	}
	for _, p := range reg {
		g, ok := got[p.Code]
		if !ok || g.Scope != p.Scope || g.Module != p.Module || g.Resource != p.Resource || g.Action != p.Action {
			t.Errorf("注册表 %+v 与已种子的 %+v 不一致", p, g)
		}
	}
	// OpenAPI 的每个 x-permissions 都存在于已种子的 permissions 表，且 scope 与路由的 x-tenant-scope 相容。
	seen := 0
	for _, m := range s.spec.Expected() {
		for _, perm := range m.Permissions {
			seen++
			g, ok := got[perm]
			if !ok {
				t.Errorf("%s 引用的权限串 %q 未种子", m.OperationID, perm)
				continue
			}
			switch m.TenantScope {
			case "platform":
				if g.Scope == rbac.ScopeTenant {
					t.Errorf("%s（platform 路由）引用了 tenant 权限 %s", m.OperationID, perm)
				}
			case "tenant", "either":
				if g.Scope == rbac.ScopePlatform {
					t.Errorf("%s（%s 路由）引用了 platform 权限 %s", m.OperationID, m.TenantScope, perm)
				}
			}
		}
	}
	if seen < 300 {
		t.Fatalf("OpenAPI 权限引用数异常: %d", seen)
	}
	if g := got["tenant:impersonate"]; g.Scope != rbac.ScopePlatform {
		t.Errorf("权限字典必须含 scope=platform 的 tenant:impersonate: %+v", g)
	}
}

func TestBuiltinRolesSeeded(t *testing.T) {
	s := newStack(t)
	// 内置系统角色随种子迁移写入（无需任何 API 调用）。
	want := map[string]string{"platform_admin": "platform", "platform_operator": "platform", "platform_auditor": "platform",
		"tenant_owner": "tenant", "site_operator": "tenant", "tenant_viewer": "tenant"}
	for code, scope := range want {
		if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.roles WHERE code = ? AND scope = ? AND is_builtin = 1 AND tenant_id IS NULL", code, scope); n != 1 {
			t.Errorf("内置角色 %s(%s) 应恰有一个，实际 %d", code, scope, n)
		}
	}
	// 租户角色（含内置模板）永远不含 platform 范围的权限。
	if n := s.e.rootInt(t, `SELECT COUNT(*) FROM §.role_permissions rp JOIN §.roles r ON r.id = rp.role_id JOIN §.permissions p ON p.id = rp.permission_id
		WHERE r.scope = 'tenant' AND p.scope = 'platform'`); n != 0 {
		t.Errorf("租户角色不得含 platform 权限，发现 %d 条", n)
	}
	// platform_admin 持有全部权限（含 tenant:impersonate）；platform_auditor 不含任何写类权限与 impersonate。
	if n, total := s.e.rootInt(t, `SELECT COUNT(*) FROM §.role_permissions rp JOIN §.roles r ON r.id = rp.role_id WHERE r.code = 'platform_admin'`), int64(len(rbac.All())); n != total {
		t.Errorf("platform_admin 应持有全部 %d 项权限，实际 %d", total, n)
	}
	if n := s.e.rootInt(t, `SELECT COUNT(*) FROM §.role_permissions rp JOIN §.roles r ON r.id = rp.role_id JOIN §.permissions p ON p.id = rp.permission_id
		WHERE r.code = 'platform_auditor' AND p.action <> 'read'`); n != 0 {
		t.Errorf("platform_auditor 只能有只读权限，发现 %d 条非 read", n)
	}
	// G-6/NFR-6：迁移不创建任何用户，更没有默认口令。
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.users"); n != 0 {
		t.Errorf("全新库不得有任何用户（无默认账号/口令），实际 %d", n)
	}
}

// 租户角色授权由 API 强制（见 TestRolesCRUD…）；此处补一个库级全局不变量扫描函数，供各用例收尾时复用。
func (s *stack) assertNoPlatformPermInTenantRoles() {
	s.t.Helper()
	if n := s.e.rootInt(s.t, `SELECT COUNT(*) FROM §.role_permissions rp JOIN §.roles r ON r.id = rp.role_id JOIN §.permissions p ON p.id = rp.permission_id
		WHERE r.scope = 'tenant' AND p.scope = 'platform'`); n != 0 {
		s.t.Errorf("租户角色含 platform 权限: %d 条", n)
	}
}

// 所有 /admin 管理路由的元数据都经 httpx.Router.Register 注册并与 OpenAPI 一致（无 501 占位）。
func TestAdminRoutesRegisteredAndMatchOpenAPI(t *testing.T) {
	s := newStack(t)
	reg := map[string]bool{}
	for _, m := range s.router.Routes() {
		reg[m.OperationID] = true
	}
	var admin []string
	for _, m := range s.spec.Expected() {
		if strings.HasPrefix(m.Path, "/admin/users") || strings.HasPrefix(m.Path, "/admin/roles") || strings.HasPrefix(m.Path, "/admin/permissions") {
			admin = append(admin, m.OperationID)
		}
	}
	sort.Strings(admin)
	if len(admin) != 16 {
		t.Fatalf("OpenAPI 中 /admin/users|roles|permissions 的 operation 应为 16 个: %v", admin)
	}
	for _, id := range admin {
		if !reg[id] {
			t.Errorf("%s 未注册", id)
		}
	}
	// 元数据（x-permissions/x-tenant-scope/x-audit/x-reauth/x-phase/幂等头）与 OpenAPI 逐项一致。
	if problems := s.spec.CheckRoutes(s.router.Routes()); len(problems) > 0 {
		for _, p := range problems {
			if strings.Contains(p, "probe") {
				continue
			}
			t.Errorf("路由元数据与 OpenAPI 不一致: %s", p)
		}
	}
}
