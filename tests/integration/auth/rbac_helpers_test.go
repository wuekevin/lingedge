package authit

import (
	"strings"
)

// Story 1.18（RBAC）集成测试的共用助手。

// grantPlatformRole 经 root 连接给平台用户授予内置平台角色（直接写库，模拟安装向导/已有管理员的授予；不经被测的 API）。
func (s *stack) grantPlatformRole(u user, roleCode string) {
	s.t.Helper()
	s.e.root(s.t, "INSERT INTO §.user_roles (user_id, role_id, tenant_id) SELECT ?, id, NULL FROM §.roles WHERE scope = 'platform' AND tenant_id IS NULL AND code = ?", u.ID, roleCode)
	s.e.root(s.t, "UPDATE §.users SET permission_version = permission_version + 1 WHERE id = ?", u.ID)
}

// session 是一个已登录的 Admin 会话。
type session struct {
	u     user
	token string
	csrf  string
}

// adminSession 创建平台用户、经 root 授予角色（内置角色 code）、登录（真实 /auth/login）。
func (s *stack) adminSession(roleCodes ...string) session {
	s.t.Helper()
	u := s.seedUser("platform")
	for _, c := range roleCodes {
		s.grantPlatformRole(u, c)
	}
	_, token, csrf := s.login(u, freshIP())
	return session{u: u, token: token, csrf: csrf}
}

// customRole 经 root 创建一个自定义模板角色并授予权限（不经被测 API）；返回角色 code。
func (s *stack) customRole(scope string, perms ...string) string {
	s.t.Helper()
	code := "t_" + strings.ToLower(ids.MustNew()[10:22])
	s.e.root(s.t, "INSERT INTO §.roles (uid, tenant_id, scope, code, name, is_builtin) VALUES (?, NULL, ?, ?, ?, 0)", ids.MustNew(), scope, code, "测试角色 "+code)
	for _, p := range perms {
		s.e.root(s.t, "INSERT INTO §.role_permissions (role_id, permission_id) SELECT r.id, p.id FROM §.roles r JOIN §.permissions p ON p.code = ? WHERE r.code = ? AND r.tenant_id IS NULL", p, code)
	}
	return code
}

// operator 是持有指定权限（自定义平台角色）的已登录 Admin 会话。
func (s *stack) operator(perms ...string) session {
	s.t.Helper()
	return s.adminSession(s.customRole("platform", perms...))
}

// grantTenantRole 给租户用户在其租户内授予内置租户角色。
func (s *stack) grantTenantRole(u user, roleCode string) {
	s.t.Helper()
	s.e.root(s.t, "INSERT INTO §.user_roles (user_id, role_id, tenant_id) SELECT ?, id, ? FROM §.roles WHERE scope = 'tenant' AND tenant_id IS NULL AND code = ?", u.ID, u.TenantID, roleCode)
	s.e.root(s.t, "UPDATE §.users SET permission_version = permission_version + 1 WHERE id = ?", u.ID)
}

func (s *stack) api(ss session, method, path string, body any, opts ...reqOpt) resp {
	s.t.Helper()
	return s.authed("admin", ss.token, ss.csrf, method, path, body, opts...)
}

// freshReauth 对会话做再认证（口令；这些用户都没有启用 TOTP）。
func (s *stack) freshReauth(ss session) {
	s.t.Helper()
	if r := s.reauth("admin", ss.token, ss.csrf, map[string]string{"password": ss.u.Password}); r.Status != 200 {
		s.t.Fatalf("再认证失败: %d %s", r.Status, r.Body)
	}
}

func (s *stack) roleUID(code string) string {
	return s.e.rootStr(s.t, "SELECT uid FROM §.roles WHERE code = ? AND tenant_id IS NULL", code)
}

func (s *stack) permVersion(id uint64) int64 {
	return s.e.rootInt(s.t, "SELECT permission_version FROM §.users WHERE id = ?", id)
}

// activeSuperAdmins 返回有效超级管理员（platform_admin 的未删除、active 持有者）数。
func (s *stack) activeSuperAdmins() int64 {
	return s.e.rootInt(s.t, `SELECT COUNT(*) FROM §.user_roles ur JOIN §.roles r ON r.id = ur.role_id JOIN §.users u ON u.id = ur.user_id
		WHERE r.code = 'platform_admin' AND r.tenant_id IS NULL AND ur.tenant_key = 0 AND u.status = 'active' AND u.deleted_at IS NULL`)
}
