package authit

import (
	"fmt"
	"strings"
	"testing"
)

func (s *stack) mustStatus(r resp, want int, what string) resp {
	s.t.Helper()
	if r.Status != want {
		s.t.Fatalf("%s: 期望 %d，得到 %d %s", what, want, r.Status, r.Body)
	}
	return r
}

func createUserBody(email string, extra map[string]any) map[string]any {
	b := map[string]any{"email": email, "display_name": "新管理员", "user_type": "platform"}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func newEmail() string { return strings.ToLower(ids.MustNew()) + "@example.test" }

// AC2：无权限角色访问受保护 API → 403 auth.forbidden；未认证 → 401；全部响应符合契约。
func TestUnprivilegedPrincipalsCannotUseAdminAPIs(t *testing.T) {
	s := newStack(t)
	type call struct {
		id, method, path string
		body             any
	}
	uid, rid := ids.MustNew(), ids.MustNew()
	calls := []call{
		{"listUsers", "GET", "/admin/users", nil},
		{"createUser", "POST", "/admin/users", createUserBody(newEmail(), nil)},
		{"getUser", "GET", "/admin/users/" + uid, nil},
		{"updateUser", "PATCH", "/admin/users/" + uid, map[string]any{"display_name": "x"}},
		{"deleteUser", "DELETE", "/admin/users/" + uid, nil},
		{"setUserRoles", "PUT", "/admin/users/" + uid + "/roles", map[string]any{"role_codes": []string{}}},
		{"unlockUser", "POST", "/admin/users/" + uid + "/unlock", nil},
		{"resetUserPassword", "POST", "/admin/users/" + uid + "/reset-password", map[string]any{}},
		{"revokeUserSessions", "DELETE", "/admin/users/" + uid + "/sessions", nil},
		{"listRoles", "GET", "/admin/roles", nil},
		{"createRole", "POST", "/admin/roles", map[string]any{"scope": "platform", "code": "x_role", "name": "x"}},
		{"getRole", "GET", "/admin/roles/" + rid, nil},
		{"updateRole", "PATCH", "/admin/roles/" + rid, map[string]any{"name": "x"}},
		{"deleteRole", "DELETE", "/admin/roles/" + rid, nil},
		{"setRolePermissions", "PUT", "/admin/roles/" + rid + "/permissions", map[string]any{"permissions": []string{}}},
		{"listPermissions", "GET", "/admin/permissions", nil},
	}
	// 1) 未认证 → 401。
	for _, c := range calls {
		r := s.do(c.method, c.path, c.body)
		if r.Status != 401 || r.errCode(t) != "auth.unauthenticated" {
			t.Errorf("%s 未认证应 401: %d %s", c.id, r.Status, r.Body)
		}
	}
	// 2) 已认证但没有任何角色 → 403 auth.forbidden（不论是否再认证：先权限后再认证，不提示再认证）。
	none := s.adminSession()
	for _, c := range calls {
		r := s.api(none, c.method, c.path, c.body)
		if r.Status != 403 || r.errCode(t) != "auth.forbidden" {
			t.Errorf("%s 无角色应 403 auth.forbidden: %d %s", c.id, r.Status, r.Body)
		}
		s.contract(c.id, c.method, c.path, r)
	}
	// 3) 只读角色（platform_auditor：全部只读权限）：读可以，所有写 403 auth.forbidden（而不是 auth.reauth_required）。
	aud := s.adminSession("platform_auditor")
	for _, c := range calls {
		r := s.api(aud, c.method, c.path, c.body)
		isRead := c.method == "GET"
		switch {
		case isRead && c.id != "getUser" && c.id != "getRole":
			if r.Status != 200 {
				t.Errorf("%s 审计员应可读: %d %s", c.id, r.Status, r.Body)
			}
		case isRead: // 不存在的目标 → 404（有读权限）
			if r.Status != 404 {
				t.Errorf("%s 审计员读不存在的目标应 404: %d %s", c.id, r.Status, r.Body)
			}
		default:
			if r.Status != 403 || r.errCode(t) != "auth.forbidden" {
				t.Errorf("%s 审计员写操作应 403 auth.forbidden: %d %s", c.id, r.Status, r.Body)
			}
		}
		s.contract(c.id, c.method, c.path, r)
	}
	// 4) 租户用户（Console 会话）访问 platform 路由：realm 隔离 → 401；租户角色即便持有「同名」权限也进不来。
	tu := s.seedUser("tenant")
	s.grantTenantRole(tu, "tenant_owner")
	_, ctok, ccsrf := s.login(tu, freshIP())
	for _, c := range calls[:3] {
		if r := s.authed("console", ctok, ccsrf, c.method, c.path, c.body); r.Status != 401 {
			t.Errorf("%s 租户会话应 401（realm 隔离）: %d %s", c.id, r.Status, r.Body)
		}
	}
	// 5) 授权顺序：有权限但未再认证 → 403 auth.reauth_required；无权限者永远是 auth.forbidden。
	op := s.operator("user:write", "user:read")
	if r := s.api(op, "POST", "/admin/users", createUserBody(newEmail(), nil)); r.Status != 403 || r.errCode(t) != "auth.reauth_required" {
		t.Errorf("有权限未再认证应 403 auth.reauth_required: %d %s", r.Status, r.Body)
	}
	if r := s.api(none, "POST", "/admin/users", createUserBody(newEmail(), nil)); r.errCode(t) != "auth.forbidden" {
		t.Errorf("无权限者即使未再认证也应 auth.forbidden: %s", r.Body)
	}
	// 多权限须全部具备：setUserRoles 需要 user:write 与 role:write，只有前者 → 403 auth.forbidden。
	s.freshReauth(op)
	if r := s.api(op, "PUT", "/admin/users/"+uid+"/roles", map[string]any{"role_codes": []string{}}); r.Status != 403 || r.errCode(t) != "auth.forbidden" {
		t.Errorf("缺 role:write 应 403 auth.forbidden: %d %s", r.Status, r.Body)
	}
	s.assertNoPlatformPermInTenantRoles()
}

// AC3：用户 CRUD、角色分配、unlock、reset-password、会话撤销；创建/重置口令强制首次登录改密；服务端生成的口令只回显一次且不入日志/库/幂等记录。
func TestAdminUsersLifecycle(t *testing.T) {
	s := newStack(t, stackOpts{settings: map[string]string{"login.throttle_account_max": "3", "login.throttle_ip_max": "1000"}})
	adm := s.adminSession("platform_admin")
	s.freshReauth(adm)

	// ---- 创建：服务端生成口令 ----
	email := newEmail()
	key := "idem-" + ids.MustNew()
	r := s.mustStatus(s.api(adm, "POST", "/admin/users", createUserBody(email, map[string]any{"role_codes": []string{"platform_auditor"}}), withHeader("Idempotency-Key", key)), 201, "createUser")
	s.contract("createUser", "POST", "/admin/users", r)
	j := r.JSON(t)
	temp, _ := j["temporary_password"].(string)
	if len(temp) < 20 || j["must_change_password"] != true || j["status"] != "active" || j["user_type"] != "platform" {
		t.Fatalf("创建响应应含一次性口令且强制改密: %s", r.Body)
	}
	if cc := r.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("含口令的响应必须 no-store: %q", cc)
	}
	if rc, _ := j["role_codes"].([]any); len(rc) != 1 || rc[0] != "platform_auditor" {
		t.Errorf("role_codes: %v", j["role_codes"])
	}
	newUID := j["uid"].(string)
	// 库里只有 argon2id 哈希，不含明文；must_change_password=1。
	if h := s.e.rootStr(t, "SELECT password_hash FROM §.users WHERE uid = ?", newUID); !strings.HasPrefix(h, "$argon2id$") || strings.Contains(h, temp) {
		t.Errorf("库里必须是 argon2id 哈希: %q", h)
	}
	if n := s.e.rootInt(t, "SELECT must_change_password FROM §.users WHERE uid = ?", newUID); n != 1 {
		t.Errorf("创建后必须强制首次登录改密")
	}
	// 幂等：同键同体重放 → 同一用户，但不再回显口令；幂等记录里没有口令明文。
	r2 := s.mustStatus(s.api(adm, "POST", "/admin/users", createUserBody(email, map[string]any{"role_codes": []string{"platform_auditor"}}), withHeader("Idempotency-Key", key)), 201, "createUser 重放")
	if r2.Header.Get("Idempotent-Replayed") != "true" || strings.Contains(string(r2.Body), temp) || r2.JSON(t)["uid"] != newUID {
		t.Errorf("重放应返回同一用户且不含口令: %s", r2.Body)
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.idempotency_keys WHERE CAST(response_body AS CHAR) LIKE ?", "%"+temp+"%"); n != 0 {
		t.Errorf("幂等记录不得保存一次性口令明文（%d 行含明文）", n)
	}
	if r3 := s.api(adm, "POST", "/admin/users", createUserBody(newEmail(), nil), withHeader("Idempotency-Key", key)); r3.Status != 422 || r3.errCode(t) != "idempotency.key_reuse" {
		t.Errorf("同键异体应 422: %d %s", r3.Status, r3.Body)
	}

	// 用临时口令登录 → must_change_password，其余 API 在改密前 403；改密后恢复。
	nu := user{ID: uint64(s.e.rootInt(t, "SELECT id FROM §.users WHERE uid = ?", newUID)), UID: newUID, Email: email, Password: temp, Type: "platform"}
	lr, ntok, ncsrf := s.login(nu, freshIP())
	if lr.JSON(t)["status"] != "must_change_password" {
		t.Errorf("首次登录应要求改密: %s", lr.Body)
	}
	if r := s.authed("admin", ntok, ncsrf, "GET", "/admin/users", nil); r.Status != 403 {
		t.Errorf("改密前其他 API 应 403: %d %s", r.Status, r.Body)
	}
	chosen := randPassword(t)
	if r := s.authed("admin", ntok, ncsrf, "POST", "/auth/password/change", map[string]string{"current_password": temp, "new_password": chosen}); r.Status != 204 {
		t.Fatalf("改密: %d %s", r.Status, r.Body)
	}
	nu.Password = chosen
	_, ntok, ncsrf = s.login(nu, freshIP())
	if r := s.authed("admin", ntok, ncsrf, "GET", "/admin/users", nil); r.Status != 200 { // platform_auditor 有 user:read
		t.Errorf("改密后审计员应可读: %d %s", r.Status, r.Body)
	}

	// ---- 创建：创建者设定口令（不回显）；校验与冲突 ----
	supplied := randPassword(t)
	email2 := newEmail()
	r = s.mustStatus(s.api(adm, "POST", "/admin/users", createUserBody(email2, map[string]any{"initial_password": supplied})), 201, "createUser 自设口令")
	s.contract("createUser", "POST", "/admin/users", r)
	if _, has := r.JSON(t)["temporary_password"]; has || strings.Contains(string(r.Body), supplied) {
		t.Errorf("创建者设定的口令不得回显: %s", r.Body)
	}
	u2 := user{UID: r.JSON(t)["uid"].(string), Email: email2, Password: supplied, Type: "platform"}
	if lr, _, _ := s.login(u2, freshIP()); lr.JSON(t)["status"] != "must_change_password" {
		t.Errorf("自设口令也必须强制首次改密")
	}
	bad := []struct {
		name string
		body map[string]any
		want int
		code string
	}{
		{"邮箱非法", createUserBody("not-an-email", nil), 400, "validation.failed"},
		{"邮箱冲突", createUserBody(email2, nil), 409, "resource.conflict"},
		{"口令过短", createUserBody(newEmail(), map[string]any{"initial_password": shortPW}), 400, "auth.password_policy"},
		{"租户类型", createUserBody(newEmail(), map[string]any{"user_type": "tenant"}), 400, "validation.failed"},
		{"未知角色", createUserBody(newEmail(), map[string]any{"role_codes": []string{"no_such_role"}}), 400, "validation.failed"},
		{"租户角色不能给平台用户", createUserBody(newEmail(), map[string]any{"role_codes": []string{"tenant_owner"}}), 400, "validation.failed"},
		{"展示名为空", map[string]any{"email": newEmail(), "display_name": "  ", "user_type": "platform"}, 400, "validation.failed"},
		{"未知字段", createUserBody(newEmail(), map[string]any{"is_admin": true}), 400, "validation.failed"},
	}
	for _, b := range bad {
		r := s.api(adm, "POST", "/admin/users", b.body)
		if r.Status != b.want || r.errCode(t) != b.code {
			t.Errorf("%s: 期望 %d %s，得到 %d %s", b.name, b.want, b.code, r.Status, r.Body)
		}
		s.contract("createUser", "POST", "/admin/users", r)
	}

	// ---- 读取/更新/列表 ----
	r = s.mustStatus(s.api(adm, "GET", "/admin/users/"+newUID, nil), 200, "getUser")
	s.contract("getUser", "GET", "/admin/users/"+newUID, r)
	r = s.mustStatus(s.api(adm, "PATCH", "/admin/users/"+newUID, map[string]any{"display_name": "改名后", "locale": "en", "timezone": "Asia/Shanghai"}), 200, "updateUser")
	s.contract("updateUser", "PATCH", "/admin/users/"+newUID, r)
	if j := r.JSON(t); j["display_name"] != "改名后" || j["locale"] != "en" || j["timezone"] != "Asia/Shanghai" {
		t.Errorf("更新未生效: %s", r.Body)
	}
	for _, b := range []map[string]any{{"status": "locked"}, {"locale": "fr"}, {"display_name": ""}, {"timezone": "bad zone!"}, {"role_codes": []string{}}} {
		if r := s.api(adm, "PATCH", "/admin/users/"+newUID, b); r.Status != 400 {
			t.Errorf("非法更新 %v 应 400: %d %s", b, r.Status, r.Body)
		}
	}
	r = s.mustStatus(s.api(adm, "GET", "/admin/users?page=1&page_size=2&sort=-email", nil), 200, "listUsers")
	s.contract("listUsers", "GET", "/admin/users?page=1&page_size=2&sort=-email", r)
	if j := r.JSON(t); len(j["items"].([]any)) != 2 || j["total"].(float64) != 3 || j["page_size"].(float64) != 2 {
		t.Errorf("分页: %s", r.Body)
	}
	if r := s.api(adm, "GET", "/admin/users?sort=password_hash", nil); r.Status != 400 {
		t.Errorf("非白名单排序应 400: %d", r.Status)
	}
	if r := s.api(adm, "GET", "/admin/users/"+ids.MustNew(), nil); r.Status != 404 || r.errCode(t) != "resource.not_found" {
		t.Errorf("不存在应 404: %d %s", r.Status, r.Body)
	}
	// 租户用户不在平台用户管理范围内：404（不泄漏存在性）。
	tu := s.seedUser("tenant")
	if r := s.api(adm, "GET", "/admin/users/"+tu.UID, nil); r.Status != 404 {
		t.Errorf("租户用户应 404: %d %s", r.Status, r.Body)
	}

	// ---- 设置角色（x-reauth）：新增角色、移除角色，permission_version 递增；响应符合契约 ----
	before := s.permVersion(nu.ID)
	r = s.mustStatus(s.api(adm, "PUT", "/admin/users/"+newUID+"/roles", map[string]any{"role_codes": []string{"platform_operator", "platform_auditor"}}), 200, "setUserRoles")
	s.contract("setUserRoles", "PUT", "/admin/users/"+newUID+"/roles", r)
	if rc, _ := r.JSON(t)["role_codes"].([]any); len(rc) != 2 {
		t.Errorf("role_codes: %s", r.Body)
	}
	if s.permVersion(nu.ID) != before+1 {
		t.Errorf("角色变更必须递增 permission_version（%d→%d）", before, s.permVersion(nu.ID))
	}
	if r := s.api(adm, "PUT", "/admin/users/"+newUID+"/roles", map[string]any{"role_codes": []string{"nope"}}); r.Status != 400 {
		t.Errorf("未知角色应 400: %d", r.Status)
	}
	if r := s.api(adm, "PUT", "/admin/users/"+newUID+"/roles", map[string]any{}); r.Status != 400 {
		t.Errorf("缺 role_codes 应 400: %d", r.Status)
	}

	// ---- reset-password：服务端生成（200 一次性口令）/ 管理员设定（204）；吊销全部会话；强制改密 ----
	r = s.mustStatus(s.api(adm, "POST", "/admin/users/"+newUID+"/reset-password", map[string]any{}), 200, "resetUserPassword 生成")
	s.contract("resetUserPassword", "POST", "/admin/users/"+newUID+"/reset-password", r)
	reset1, _ := r.JSON(t)["temporary_password"].(string)
	if len(reset1) < 20 || !strings.Contains(r.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("重置应回显一次性口令: %s", r.Body)
	}
	if r := s.authed("admin", ntok, ncsrf, "GET", "/admin/users", nil); r.Status != 401 {
		t.Errorf("重置后目标的旧会话应失效: %d", r.Status)
	}
	nu.Password = reset1
	if lr, _, _ := s.login(nu, freshIP()); lr.JSON(t)["status"] != "must_change_password" {
		t.Errorf("重置后首次登录必须改密")
	}
	setPW := randPassword(t)
	r = s.mustStatus(s.api(adm, "POST", "/admin/users/"+newUID+"/reset-password", map[string]any{"new_password": setPW}), 204, "resetUserPassword 设定")
	s.contract("resetUserPassword", "POST", "/admin/users/"+newUID+"/reset-password", r)
	if r := s.api(adm, "POST", "/admin/users/"+newUID+"/reset-password", map[string]any{"new_password": shortPW}); r.Status != 400 || r.errCode(t) != "auth.password_policy" {
		t.Errorf("过短口令应 400 auth.password_policy: %d %s", r.Status, r.Body)
	}
	nu.Password = setPW
	if lr, _, _ := s.login(nu, freshIP()); lr.JSON(t)["status"] != "must_change_password" {
		t.Errorf("管理员设定的口令同样必须改密")
	}

	// ---- unlock：真实失败登录触发锁定 → 429；unlock 后可登录 ----
	ip := freshIP()
	for i := 0; i < 3; i++ {
		s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: email, Password: randPassword(t)}, withIP(ip))
	}
	if r := s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: email, Password: nu.Password}, withIP(freshIP())); r.Status != 429 {
		t.Fatalf("应被节流锁定: %d %s", r.Status, r.Body)
	}
	s.e.root(t, "UPDATE §.users SET status = 'locked', failed_login_count = 9, locked_until = DATE_ADD(NOW(3), INTERVAL 1 HOUR) WHERE uid = ?", newUID)
	r = s.mustStatus(s.api(adm, "POST", "/admin/users/"+newUID+"/unlock", nil), 200, "unlockUser")
	s.contract("unlockUser", "POST", "/admin/users/"+newUID+"/unlock", r)
	if j := r.JSON(t); j["status"] != "active" {
		t.Errorf("unlock 后应为 active: %s", r.Body)
	}
	if n := s.e.rootInt(t, "SELECT failed_login_count FROM §.users WHERE uid = ?", newUID) + s.e.rootInt(t, "SELECT locked_until IS NOT NULL FROM §.users WHERE uid = ?", newUID); n != 0 {
		t.Errorf("unlock 应清除失败计数与锁定截止")
	}
	if lr, _, _ := s.login(nu, freshIP()); lr.Status != 200 {
		t.Errorf("unlock 后应可登录（Redis 节流计数同时清零）")
	}

	// ---- 会话撤销 ----
	_, vtok, vcsrf := s.login(nu, freshIP())
	_, vtok2, _ := s.login(nu, freshIP())
	r = s.mustStatus(s.api(adm, "DELETE", "/admin/users/"+newUID+"/sessions", nil), 204, "revokeUserSessions")
	s.contract("revokeUserSessions", "DELETE", "/admin/users/"+newUID+"/sessions", r)
	for _, tk := range []string{vtok, vtok2} {
		if r := s.authed("admin", tk, vcsrf, "GET", "/auth/me", nil); r.Status != 401 {
			t.Errorf("被撤销的会话应 401: %d", r.Status)
		}
	}

	// ---- 停用 / 删除 ----
	r = s.mustStatus(s.api(adm, "PATCH", "/admin/users/"+u2.UID, map[string]any{"status": "disabled"}), 200, "停用")
	if r.JSON(t)["status"] != "disabled" {
		t.Errorf("应已停用: %s", r.Body)
	}
	if lr := s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: u2.Email, Password: u2.Password}, withIP(freshIP())); lr.Status != 401 {
		t.Errorf("停用账号不可登录: %d", lr.Status)
	}
	r = s.mustStatus(s.api(adm, "DELETE", "/admin/users/"+u2.UID, nil), 204, "deleteUser")
	s.contract("deleteUser", "DELETE", "/admin/users/"+u2.UID, r)
	if r := s.api(adm, "GET", "/admin/users/"+u2.UID, nil); r.Status != 404 {
		t.Errorf("已删除应 404: %d", r.Status)
	}
	if r := s.api(adm, "DELETE", "/admin/users/"+u2.UID, nil); r.Status != 404 {
		t.Errorf("重复删除应 404: %d", r.Status)
	}
	// 删除后邮箱可重新使用（软删除释放唯一约束）。
	if r := s.api(adm, "POST", "/admin/users", createUserBody(email2, nil)); r.Status != 201 {
		t.Errorf("软删除后邮箱应可复用: %d %s", r.Status, r.Body)
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.user_roles ur JOIN §.users u ON u.id = ur.user_id WHERE u.uid = ?", u2.UID); n != 0 {
		t.Errorf("删除用户应清除其角色授予")
	}

	// ---- 日志脱敏（G-5）：生成/设定/当前的所有口令都不得出现在任何日志里，也不得出现在审计记录里（Story 1.21）----
	s.mod.Flush()
	logs := s.logs.String()
	for _, secret := range []string{temp, reset1, supplied, chosen, setPW} {
		if strings.Contains(logs, secret) {
			t.Errorf("日志泄露了口令 %q…", secret[:6])
		}
		if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs WHERE CONVERT(CONCAT_WS(' ', actor_label, resource_uid, CAST(changes_json AS CHAR), CAST(meta_json AS CHAR)) USING utf8mb4) LIKE ?", "%"+secret+"%"); n != 0 {
			t.Errorf("审计记录泄露了口令 %q…", secret[:6])
		}
	}
	// 审计记录（与业务同事务写入 audit_logs）：管理写操作各留一条，操作者是平台管理员，无 acting_as。
	for _, act := range []string{"user.create", "user.reset_password", "user.update", "user.delete", "user.set_roles"} {
		if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs WHERE action = ? AND actor_type = 'user' AND acting_as = 0 AND result = 'success'", act); n < 1 {
			t.Errorf("管理操作 %s 应写入 audit_logs", act)
		}
	}
	s.assertNoPlatformPermInTenantRoles()
}

// AC3：不得删除或降权最后一个超级管理员（异常路径）；内置角色不可变。
func TestLastSuperAdminProtected(t *testing.T) {
	s := newStack(t)
	sole := s.adminSession("platform_admin")
	s.freshReauth(sole)
	conflictCase := func(what string, r resp) {
		t.Helper()
		if r.Status != 409 || r.errCode(t) != "resource.conflict" {
			t.Errorf("%s: 最后一个超级管理员应 409 resource.conflict: %d %s", what, r.Status, r.Body)
		}
	}
	p := "/admin/users/" + sole.u.UID
	conflictCase("删除", s.api(sole, "DELETE", p, nil))
	conflictCase("停用", s.api(sole, "PATCH", p, map[string]any{"status": "disabled"}))
	conflictCase("移除 platform_admin", s.api(sole, "PUT", p+"/roles", map[string]any{"role_codes": []string{"platform_operator"}}))
	conflictCase("清空角色", s.api(sole, "PUT", p+"/roles", map[string]any{"role_codes": []string{}}))
	s.contract("deleteUser", "DELETE", p, s.api(sole, "DELETE", p, nil))
	if s.activeSuperAdmins() != 1 {
		t.Fatalf("超级管理员数应仍为 1")
	}
	// 保留 platform_admin 的角色设置是允许的（不降权）。
	if r := s.api(sole, "PUT", p+"/roles", map[string]any{"role_codes": []string{"platform_admin", "platform_auditor"}}); r.Status != 200 {
		t.Errorf("保留 platform_admin 应允许: %d %s", r.Status, r.Body)
	}
	// 内置角色不可改名、不可改权限、不可删除（降权的另一条路径）。
	rid := s.roleUID("platform_admin")
	conflictCase("删除内置角色", s.api(sole, "DELETE", "/admin/roles/"+rid, nil))
	conflictCase("改内置角色权限", s.api(sole, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{"user:read"}}))
	conflictCase("改内置角色名称", s.api(sole, "PATCH", "/admin/roles/"+rid, map[string]any{"name": "x"}))
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.role_permissions rp JOIN §.roles r ON r.id = rp.role_id WHERE r.code = 'platform_admin'"); n != 81 {
		t.Errorf("platform_admin 的权限不得减少: %d", n)
	}

	// 有第二个超级管理员时，可以删除/降权其中一个；之后剩下的那个又受保护。
	second := s.adminSession("platform_admin")
	if r := s.api(sole, "PUT", "/admin/users/"+second.u.UID+"/roles", map[string]any{"role_codes": []string{}}); r.Status != 200 {
		t.Fatalf("两个超管时降权其中一个应允许: %d %s", r.Status, r.Body)
	}
	conflictCase("降权后唯一剩下的超管再被降权", s.api(sole, "PUT", p+"/roles", map[string]any{"role_codes": []string{}}))
	if s.activeSuperAdmins() != 1 {
		t.Fatalf("超级管理员数应为 1")
	}
	// 停用的超管不算「有效」：把唯一的超管之外再加一个、停用后仍不能删除/降权真正唯一有效的那个。
	third := s.adminSession("platform_admin")
	s.mustStatus(s.api(sole, "PATCH", "/admin/users/"+third.u.UID, map[string]any{"status": "disabled"}), 200, "停用第二个超管")
	conflictCase("停用的超管不计入，唯一有效者仍受保护", s.api(sole, "DELETE", p, nil))
}

// AC3（并发竞态）：并发地降权/停用/删除「两个超级管理员」的请求，事务内加锁保证只有一个成功，永远至少剩一个有效超管。
func TestLastSuperAdminConcurrentRace(t *testing.T) {
	s := newStack(t)
	// 操作者 C 不是超管：持有 user:read/user:write/role:read/role:write 的自定义角色（直接写库），用它并发操作两个超管。
	c := s.operator("user:read", "user:write", "role:read", "role:write")
	s.freshReauth(c)
	ops := []struct {
		name string
		do   func(uid string) resp
	}{
		{"demote", func(uid string) resp {
			return s.api(c, "PUT", "/admin/users/"+uid+"/roles", map[string]any{"role_codes": []string{}})
		}},
		{"disable", func(uid string) resp {
			return s.api(c, "PATCH", "/admin/users/"+uid, map[string]any{"status": "disabled"})
		}},
		{"delete", func(uid string) resp { return s.api(c, "DELETE", "/admin/users/"+uid, nil) }},
	}
	const rounds = 18
	for i := 0; i < rounds; i++ {
		a, b := s.seedUser("platform"), s.seedUser("platform")
		s.grantPlatformRole(a, "platform_admin")
		s.grantPlatformRole(b, "platform_admin")
		if n := s.activeSuperAdmins(); n != 2 {
			t.Fatalf("第 %d 轮前超管数应为 2: %d", i, n)
		}
		opA, opB := ops[i%3], ops[(i/3+i+1)%3]
		start := make(chan struct{})
		res := make([]resp, 2)
		done := make(chan int, 2)
		go func() { <-start; res[0] = opA.do(a.UID); done <- 0 }()
		go func() { <-start; res[1] = opB.do(b.UID); done <- 1 }()
		close(start)
		<-done
		<-done
		ok, conf := 0, 0
		for _, r := range res {
			switch r.Status {
			case 200, 204:
				ok++
			case 409:
				conf++
			default:
				t.Fatalf("第 %d 轮（%s a / %s b）出现意外状态 %d %s", i, opA.name, opB.name, r.Status, r.Body)
			}
		}
		if ok != 1 || conf != 1 {
			t.Fatalf("第 %d 轮（%s a / %s b）应恰好一个成功一个 409，实际 成功=%d 冲突=%d: %s | %s", i, opA.name, opB.name, ok, conf, res[0].Body, res[1].Body)
		}
		if n := s.activeSuperAdmins(); n != 1 {
			t.Fatalf("第 %d 轮后必须恰剩 1 个有效超管，实际 %d", i, n)
		}
		// 清场：剩下的那个超管降为无角色（直接写库），进入下一轮。
		s.e.root(t, "DELETE FROM §.user_roles WHERE role_id = (SELECT id FROM §.roles WHERE code = 'platform_admin' AND tenant_id IS NULL)")
	}
}

// 防提权：权限不足者不能把自己没有的权限授予他人/角色。
func TestPrivilegeEscalationBlocked(t *testing.T) {
	s := newStack(t)
	// O 持有用户与角色管理权限，但没有 setting:write、没有 platform_admin 的全部权限。
	o := s.operator("user:read", "user:write", "role:read", "role:write")
	s.freshReauth(o)
	target := s.seedUser("platform")
	tp := "/admin/users/" + target.UID
	deny := func(what string, r resp) {
		t.Helper()
		if r.Status != 403 || r.errCode(t) != "auth.forbidden" {
			t.Errorf("%s 应 403 auth.forbidden: %d %s", what, r.Status, r.Body)
		}
	}
	deny("授予 platform_admin", s.api(o, "PUT", tp+"/roles", map[string]any{"role_codes": []string{"platform_admin"}}))
	deny("创建用户时指定 platform_admin", s.api(o, "POST", "/admin/users", createUserBody(newEmail(), map[string]any{"role_codes": []string{"platform_admin"}})))
	deny("授予含其未持有权限的 platform_operator", s.api(o, "PUT", tp+"/roles", map[string]any{"role_codes": []string{"platform_operator"}}))
	deny("创建含未持有权限的角色", s.api(o, "POST", "/admin/roles", map[string]any{"scope": "platform", "code": "esc_role", "name": "x", "permissions": []string{"setting:write"}}))
	// 自己的权限范围内：可以。
	good := s.mustStatus(s.api(o, "POST", "/admin/roles", map[string]any{"scope": "platform", "code": "mine_ok", "name": "ok", "permissions": []string{"user:read"}}), 201, "创建范围内的角色")
	rid := good.JSON(t)["uid"].(string)
	deny("给角色加未持有的权限", s.api(o, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{"user:read", "setting:write"}}))
	s.mustStatus(s.api(o, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{"user:read", "role:read"}}), 200, "范围内改权限")
	s.mustStatus(s.api(o, "PUT", tp+"/roles", map[string]any{"role_codes": []string{"mine_ok"}}), 200, "授予范围内的角色")
	// 创建用户时指定角色还要求 role:write：只有 user:write 的操作者不行。
	w := s.operator("user:read", "user:write")
	s.freshReauth(w)
	deny("缺 role:write 却指定角色", s.api(w, "POST", "/admin/users", createUserBody(newEmail(), map[string]any{"role_codes": []string{"mine_ok"}})))
	s.mustStatus(s.api(w, "POST", "/admin/users", createUserBody(newEmail(), nil)), 201, "不指定角色的创建")
}

// AC2/AC3：角色 CRUD；租户角色只能授予 scope=tenant/both 的权限；权限点字典；内置角色不可变；有持有者的角色不可删。
func TestRolesCRUDAndTenantRoleScopeRule(t *testing.T) {
	s := newStack(t)
	adm := s.adminSession("platform_admin")
	s.freshReauth(adm)

	// 字典。
	r := s.mustStatus(s.api(adm, "GET", "/admin/permissions?page=1&page_size=200", nil), 200, "listPermissions")
	s.contract("listPermissions", "GET", "/admin/permissions?page=1&page_size=200", r)
	if j := r.JSON(t); j["total"].(float64) != 81 || len(j["items"].([]any)) != 81 {
		t.Errorf("权限点字典应有 81 项: total=%v", j["total"])
	}
	r = s.mustStatus(s.api(adm, "GET", "/admin/permissions?page=2&page_size=50", nil), 200, "listPermissions 分页")
	if len(r.JSON(t)["items"].([]any)) != 31 {
		t.Errorf("第二页应有 31 项: %s", r.Body)
	}

	// 列表含 6 个内置角色及其权限。
	r = s.mustStatus(s.api(adm, "GET", "/admin/roles", nil), 200, "listRoles")
	s.contract("listRoles", "GET", "/admin/roles", r)
	if j := r.JSON(t); j["total"].(float64) != 6 {
		t.Errorf("应有 6 个内置角色: %s", r.Body)
	}
	r = s.mustStatus(s.api(adm, "GET", "/admin/roles/"+s.roleUID("tenant_viewer"), nil), 200, "getRole")
	s.contract("getRole", "GET", "/admin/roles/"+s.roleUID("tenant_viewer"), r)
	if j := r.JSON(t); j["is_builtin"] != true || j["scope"] != "tenant" || len(j["permissions"].([]any)) == 0 {
		t.Errorf("内置租户角色: %s", r.Body)
	}

	// 创建平台角色 / 租户角色。
	r = s.mustStatus(s.api(adm, "POST", "/admin/roles", map[string]any{"scope": "platform", "code": "support_lead", "name": "支持负责人", "description": "d", "permissions": []string{"user:read", "node:read", "user:read"}}), 201, "createRole 平台")
	s.contract("createRole", "POST", "/admin/roles", r)
	if j := r.JSON(t); j["is_builtin"] != false || len(j["permissions"].([]any)) != 2 || j["tenant_uid"] != nil {
		t.Errorf("平台角色: %s", r.Body)
	}
	plat := r.JSON(t)["uid"].(string)
	r = s.mustStatus(s.api(adm, "POST", "/admin/roles", map[string]any{"scope": "tenant", "code": "site_reader", "name": "站点读者", "permissions": []string{"site:read", "site:write", "origin:read"}}), 201, "createRole 租户")
	tenantRole := r.JSON(t)["uid"].(string)
	// 租户角色不能含 platform 权限（API 层强制），也不能含未知权限。
	for _, perm := range []string{"user:read", "tenant:impersonate", "node:manage", "setting:write", "no:such_perm"} {
		r := s.api(adm, "POST", "/admin/roles", map[string]any{"scope": "tenant", "code": "bad_role", "name": "x", "permissions": []string{perm}})
		if r.Status != 400 || r.errCode(t) != "validation.failed" {
			t.Errorf("租户角色含 %s 应 400: %d %s", perm, r.Status, r.Body)
		}
		s.contract("createRole", "POST", "/admin/roles", r)
		r = s.api(adm, "PUT", "/admin/roles/"+tenantRole+"/permissions", map[string]any{"permissions": []string{"site:read", perm}})
		if r.Status != 400 {
			t.Errorf("给租户角色设置 %s 应 400: %d %s", perm, r.Status, r.Body)
		}
	}
	if n := s.e.rootInt(t, `SELECT COUNT(*) FROM §.role_permissions rp JOIN §.roles r ON r.id = rp.role_id WHERE r.code = 'bad_role'`); n != 0 {
		t.Errorf("被拒绝的创建不得留下半成品")
	}
	// 校验失败与冲突。
	for _, c := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"code 非法", map[string]any{"scope": "platform", "code": "Bad-Code", "name": "x"}, 400},
		{"scope 非法", map[string]any{"scope": "global", "code": "okcode", "name": "x"}, 400},
		{"name 为空", map[string]any{"scope": "platform", "code": "okcode", "name": " "}, 400},
		{"code 冲突", map[string]any{"scope": "platform", "code": "support_lead", "name": "x"}, 409},
		{"与内置 code 冲突", map[string]any{"scope": "platform", "code": "platform_admin", "name": "x"}, 409},
	} {
		r := s.api(adm, "POST", "/admin/roles", c.body)
		if r.Status != c.want {
			t.Errorf("%s: 期望 %d，得到 %d %s", c.name, c.want, r.Status, r.Body)
		}
		s.contract("createRole", "POST", "/admin/roles", r)
	}
	// 更新、整体设置权限。
	r = s.mustStatus(s.api(adm, "PATCH", "/admin/roles/"+plat, map[string]any{"name": "新名称"}), 200, "updateRole")
	s.contract("updateRole", "PATCH", "/admin/roles/"+plat, r)
	if r.JSON(t)["name"] != "新名称" {
		t.Errorf("更新未生效")
	}
	r = s.mustStatus(s.api(adm, "PUT", "/admin/roles/"+plat+"/permissions", map[string]any{"permissions": []string{"node:read"}}), 200, "setRolePermissions")
	s.contract("setRolePermissions", "PUT", "/admin/roles/"+plat+"/permissions", r)
	if p := r.JSON(t)["permissions"].([]any); len(p) != 1 || p[0] != "node:read" {
		t.Errorf("整体替换: %s", r.Body)
	}
	if r := s.api(adm, "PUT", "/admin/roles/"+plat+"/permissions", map[string]any{}); r.Status != 400 {
		t.Errorf("缺 permissions 应 400: %d", r.Status)
	}
	if r := s.api(adm, "GET", "/admin/roles/"+ids.MustNew(), nil); r.Status != 404 {
		t.Errorf("不存在应 404: %d", r.Status)
	}
	// 有持有者的角色不可删；解除后可删。
	holder := s.seedUser("platform")
	s.mustStatus(s.api(adm, "PUT", "/admin/users/"+holder.UID+"/roles", map[string]any{"role_codes": []string{"support_lead"}}), 200, "授予自定义角色")
	r = s.api(adm, "DELETE", "/admin/roles/"+plat, nil)
	if r.Status != 409 || r.errCode(t) != "resource.conflict" {
		t.Errorf("仍有持有者的角色不可删: %d %s", r.Status, r.Body)
	}
	s.contract("deleteRole", "DELETE", "/admin/roles/"+plat, r)
	s.mustStatus(s.api(adm, "PUT", "/admin/users/"+holder.UID+"/roles", map[string]any{"role_codes": []string{}}), 200, "解除")
	r = s.mustStatus(s.api(adm, "DELETE", "/admin/roles/"+plat, nil), 204, "deleteRole")
	s.contract("deleteRole", "DELETE", "/admin/roles/"+plat, r)
	s.mustStatus(s.api(adm, "DELETE", "/admin/roles/"+tenantRole, nil), 204, "删除租户角色")
	// 内置租户角色同样不可变。
	for _, code := range []string{"tenant_owner", "site_operator", "tenant_viewer", "platform_operator", "platform_auditor"} {
		id := s.roleUID(code)
		for what, r := range map[string]resp{
			"改名":  s.api(adm, "PATCH", "/admin/roles/"+id, map[string]any{"name": "x"}),
			"删除":  s.api(adm, "DELETE", "/admin/roles/"+id, nil),
			"改权限": s.api(adm, "PUT", "/admin/roles/"+id+"/permissions", map[string]any{"permissions": []string{}}),
		} {
			if r.Status != 409 {
				t.Errorf("内置角色 %s %s 应 409: %d %s", code, what, r.Status, r.Body)
			}
		}
	}
	s.assertNoPlatformPermInTenantRoles()
	// 幂等：createRole 同键同体重放。
	key := "role-" + ids.MustNew()
	body := map[string]any{"scope": "platform", "code": "idem_role", "name": "幂等"}
	a := s.mustStatus(s.api(adm, "POST", "/admin/roles", body, withHeader("Idempotency-Key", key)), 201, "createRole 幂等")
	b := s.mustStatus(s.api(adm, "POST", "/admin/roles", body, withHeader("Idempotency-Key", key)), 201, "createRole 重放")
	if b.Header.Get("Idempotent-Replayed") != "true" || a.JSON(t)["uid"] != b.JSON(t)["uid"] {
		t.Errorf("同键同体应重放首次结果")
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.roles WHERE code = 'idem_role'"); n != 1 {
		t.Errorf("幂等重放不得重复创建: %d", n)
	}
	// 审计（Story 1.21）：重放不重复记录——两次请求只有一次真正执行，恰好一条 role.create 审计。
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs WHERE action = 'role.create' AND resource_uid = ?", a.JSON(t)["uid"]); n != 1 {
		t.Errorf("幂等重放不得重复写审计: %d", n)
	}
	_ = fmt.Sprintf
}
