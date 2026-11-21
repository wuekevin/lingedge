package tenantit

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/leakguard"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// 种子迁移写入的默认租户（自用模式的唯一租户，D-13）。
const defaultTenantUID = "01M7QP5V346STC0AT56NME5NR3"

func admin(w *apistack.World) apistack.Session {
	u := w.SeedUser("platform", nil)
	w.GrantPlatformRole(u, "platform_admin")
	return w.Login(u)
}

// AC3：种子「默认租户」是 MVP 运营入口（D-13）：平台管理员可列出/读取它，并以 X-Tenant-ID 代其操作。
func TestSeededDefaultTenantIsTheOperatingEntry(t *testing.T) {
	w := apistack.New(t)
	if n := w.RootInt("SELECT COUNT(*) FROM §.tenants WHERE is_default = 1 AND slug = 'default' AND status = 'active' AND uid = ?", defaultTenantUID); n != 1 {
		t.Fatalf("种子迁移应含恰好一个默认租户（is_default=1）: %d", n)
	}
	a := admin(w)

	r := a.Do("GET", "/tenants", nil)
	if r.Status != 200 {
		t.Fatalf("listTenants: %d %s", r.Status, r.Body)
	}
	w.Contract("listTenants", "GET", "/tenants", r)
	items := r.JSON(t)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["uid"] != defaultTenantUID || items[0].(map[string]any)["is_default"] != true {
		t.Fatalf("M1 仅有默认租户: %s", r.Body)
	}

	// 代默认租户操作（运营入口）：GET /tenants/current + X-Tenant-ID。
	c := a.Do("GET", "/tenants/current", nil, apistack.ActingAs(defaultTenantUID))
	if c.Status != 200 || c.JSON(t)["uid"] != defaultTenantUID || c.JSON(t)["name"] != "默认租户" {
		t.Fatalf("acting-as 默认租户: %d %s", c.Status, c.Body)
	}
	w.Contract("getCurrentTenant", "GET", "/tenants/current", c)
	// 默认租户的配额「无限制」（M1：limit_value 为 null）。
	q := a.Do("GET", "/tenants/"+defaultTenantUID+"/quotas", nil)
	if q.Status != 200 {
		t.Fatalf("getTenantQuotas: %d %s", q.Status, q.Body)
	}
	w.Contract("getTenantQuotas", "GET", "/tenants/"+defaultTenantUID+"/quotas", q)
	for _, it := range q.JSON(t)["items"].([]any) {
		m := it.(map[string]any)
		if m["limit_value"] != nil || m["source"] != "default" {
			t.Errorf("默认租户配额应为无限制/default: %v", m)
		}
	}
}

// AC3：租户读取 / 更新（平台）、输入校验、持久化、契约；不存在与非法 uid 同形 404。
func TestTenantReadUpdateAndValidation(t *testing.T) {
	w := apistack.New(t)
	a := admin(w)
	tn := w.SeedTenant("alpha")
	path := "/tenants/" + tn.UID

	g := a.Do("GET", path, nil)
	if g.Status != 200 || g.JSON(t)["slug"] != tn.Slug || g.JSON(t)["status"] != "active" || g.JSON(t)["is_default"] != false {
		t.Fatalf("getTenant: %d %s", g.Status, g.Body)
	}
	w.Contract("getTenant", "GET", path, g)

	// 不存在 / 非法 uid：同一个 404。
	n1, n2 := a.Do("GET", "/tenants/"+apistack.IDs.MustNew(), nil), a.Do("GET", "/tenants/not-a-uid", nil)
	if n1.Status != 404 || n2.Status != 404 || n1.ErrBodyKey(t) != n2.ErrBodyKey(t) {
		t.Fatalf("不存在与非法 uid 应同形 404: %d/%d %s | %s", n1.Status, n2.Status, n1.Body, n2.Body)
	}
	w.Contract("getTenant", "GET", path, n1)

	// 更新。
	u := a.Do("PATCH", path, map[string]any{"name": "  新名称  ", "contact_email": "Ops@Example.Test", "locale": "en", "settings": map[string]any{"theme": "dark", "n": 3}})
	if u.Status != 200 {
		t.Fatalf("updateTenant: %d %s", u.Status, u.Body)
	}
	w.Contract("updateTenant", "PATCH", path, u)
	j := u.JSON(t)
	if j["name"] != "新名称" || j["contact_email"] != "ops@example.test" || j["locale"] != "en" || j["settings"].(map[string]any)["theme"] != "dark" || j["slug"] != tn.Slug {
		t.Fatalf("更新结果: %s", u.Body)
	}
	if got := w.RootStr("SELECT name FROM §.tenants WHERE uid = ?", tn.UID); got != "新名称" {
		t.Fatalf("应持久化: %s", got)
	}
	// 只改一个字段：其余保持；清除 contact_email；settings 整体替换。
	u2 := a.Do("PATCH", path, map[string]any{"contact_email": "", "settings": map[string]any{}})
	if u2.Status != 200 || u2.JSON(t)["contact_email"] != nil || u2.JSON(t)["name"] != "新名称" || len(u2.JSON(t)["settings"].(map[string]any)) != 0 {
		t.Fatalf("部分更新: %d %s", u2.Status, u2.Body)
	}

	bad := []struct {
		name string
		body any
	}{
		{"空对象", map[string]any{}},
		{"空名称", map[string]any{"name": "   "}},
		{"名称过长", map[string]any{"name": strings.Repeat("长", 129)}},
		{"名称含控制字符", map[string]any{"name": "a\x01b"}},
		{"非法 locale", map[string]any{"locale": "fr"}},
		{"非法邮箱", map[string]any{"contact_email": "not-an-email"}},
		{"未知字段 slug", map[string]any{"slug": "x"}},
		{"未知字段 status", map[string]any{"status": "closed"}},
		{"settings 含 cluster_uid（R-T1）", map[string]any{"settings": map[string]any{"a": map[string]any{"cluster_uid": "x"}}}},
		{"settings 含 node_id（R-T1）", map[string]any{"settings": map[string]any{"node_id": 1}}},
		{"settings 过大", map[string]any{"settings": map[string]any{"k": strings.Repeat("x", 17<<10)}}},
		{"name 类型错", map[string]any{"name": 5}},
	}
	for _, c := range bad {
		r := a.Do("PATCH", path, c.body)
		if r.Status != 400 || r.ErrCode(t) != "validation.failed" {
			t.Errorf("%s: 应 400 validation.failed: %d %s", c.name, r.Status, r.Body)
		}
		w.Contract("updateTenant", "PATCH", path, r)
	}
	if got := w.RootStr("SELECT name FROM §.tenants WHERE uid = ?", tn.UID); got != "新名称" {
		t.Fatalf("校验失败不得改库: %s", got)
	}
	if r := a.Do("PATCH", "/tenants/"+apistack.IDs.MustNew(), map[string]any{"name": "x"}); r.Status != 404 {
		t.Fatalf("更新不存在的租户应 404: %d", r.Status)
	}

	// 分页与排序白名单。
	w.SeedTenant("beta")
	l := a.Do("GET", "/tenants?sort=-name&page_size=2", nil)
	if l.Status != 200 || len(l.JSON(t)["items"].([]any)) != 2 || l.JSON(t)["total"].(float64) != 3 {
		t.Fatalf("list: %d %s", l.Status, l.Body)
	}
	w.Contract("listTenants", "GET", "/tenants", l)
	for _, q := range []string{"sort=uid", "sort=name,slug", "page=0", "page_size=201"} {
		if r := a.Do("GET", "/tenants?"+q, nil); r.Status != 400 {
			t.Errorf("%s 应 400: %d", q, r.Status)
		}
	}
}

// 权限：读与管理分离（tenant:read / tenant:manage / tenant:member:*），无权限 403；全部只对平台主体开放。
func TestTenantPermissionsAreEnforced(t *testing.T) {
	w := apistack.New(t)
	tn := w.SeedTenant("perm")
	reader := w.Login(func() apistack.User {
		u := w.SeedUser("platform", nil)
		w.GrantPlatformRole(u, "platform_auditor") // 全部只读，无 tenant:manage / tenant:impersonate
		return u
	}())
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"GET", "/tenants", nil, 200},
		{"GET", "/tenants/" + tn.UID, nil, 200},
		{"GET", "/tenants/" + tn.UID + "/members", nil, 200},
		{"GET", "/tenants/" + tn.UID + "/quotas", nil, 200},
		{"PATCH", "/tenants/" + tn.UID, map[string]any{"name": "x"}, 403},
		{"POST", "/tenants/" + tn.UID + "/members", map[string]any{"email": "a@b.test", "role_codes": []string{}}, 403},
		{"PUT", "/tenants/" + tn.UID + "/quotas", map[string]any{"limits": map[string]any{"sites": 1}}, 403},
	} {
		if r := reader.Do(c.method, c.path, c.body); r.Status != c.want {
			t.Errorf("只读管理员 %s %s: 期望 %d 得到 %d %s", c.method, c.path, c.want, r.Status, r.Body)
		}
	}
	if w.RootStr("SELECT name FROM §.tenants WHERE uid = ?", tn.UID) != tn.Name {
		t.Fatal("被拒绝的写不得改库")
	}
	// 无任何角色的平台用户：全部 403；未认证 401。
	none := w.Login(w.SeedUser("platform", nil))
	if r := none.Do("GET", "/tenants", nil); r.Status != 403 {
		t.Fatalf("无角色: %d", r.Status)
	}
	if r := w.Do("GET", "/tenants", nil); r.Status != 401 {
		t.Fatalf("未认证: %d", r.Status)
	}
}

// AC3：成员 —— 加入已有租户用户并授予租户内角色；列表含角色；冲突/校验；权限版本递增使授权立即生效；新成员端到端可用。
func TestTenantMembers(t *testing.T) {
	w := apistack.New(t)
	a := admin(w)
	tn, other := w.SeedTenant("m1"), w.SeedTenant("m2")
	owner := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(owner, "tenant_owner")
	// 一个尚未属于任何租户成员的租户用户（经 root 写：租户用户的创建属自助注册/邀请，P5）。
	cand := w.SeedUser("tenant", &other)
	w.Root("UPDATE §.tenant_members SET status = 'removed' WHERE user_id = ?", cand.ID) // 脱离原租户
	platformUser := w.SeedUser("platform", nil)

	path := "/tenants/" + tn.UID + "/members"
	l := a.Do("GET", path, nil)
	if l.Status != 200 || l.JSON(t)["total"].(float64) != 1 {
		t.Fatalf("初始成员: %d %s", l.Status, l.Body)
	}
	w.Contract("listTenantMembers", "GET", path, l)
	first := l.JSON(t)["items"].([]any)[0].(map[string]any)
	if first["user"].(map[string]any)["email"] != owner.Email || first["roles"].([]any)[0] != "tenant_owner" || first["status"] != "active" {
		t.Fatalf("成员视图: %s", l.Body)
	}

	pv := w.RootInt("SELECT permission_version FROM §.users WHERE id = ?", cand.ID)
	add := a.Do("POST", path, map[string]any{"email": strings.ToUpper(cand.Email), "role_codes": []string{"tenant_viewer", "tenant_viewer"}})
	if add.Status != 201 {
		t.Fatalf("addTenantMember: %d %s", add.Status, add.Body)
	}
	w.Contract("addTenantMember", "POST", path, add)
	m := add.JSON(t)
	if m["status"] != "active" || m["is_owner"] != false || fmt.Sprint(m["roles"]) != "[tenant_viewer]" || m["user"].(map[string]any)["uid"] != cand.UID {
		t.Fatalf("新成员: %s", add.Body)
	}
	if w.RootInt("SELECT permission_version FROM §.users WHERE id = ?", cand.ID) != pv+1 {
		t.Fatal("授予角色应递增 permission_version（权限缓存键含版本，立即生效）")
	}
	if w.RootInt("SELECT COUNT(*) FROM §.user_roles WHERE user_id = ? AND tenant_id = ?", cand.ID, tn.ID) != 1 {
		t.Fatal("同一角色重复传入只授予一次")
	}

	// 端到端：新成员登录 Console，当前租户是该租户，权限即授予的角色。
	s := w.Login(cand)
	cur := s.Do("GET", "/tenants/current", nil)
	if cur.Status != 200 || cur.JSON(t)["uid"] != tn.UID {
		t.Fatalf("新成员的当前租户: %d %s", cur.Status, cur.Body)
	}
	if r := s.Do("GET", "/tasks", nil); r.Status != 200 { // tenant_viewer 含 task:read
		t.Fatalf("tenant_viewer 应可读任务: %d %s", r.Status, r.Body)
	}
	if r := s.Do("POST", "/tasks/"+apistack.IDs.MustNew()+"/cancel", nil); r.Status != 403 { // 无 task:cancel
		t.Fatalf("tenant_viewer 不应可取消任务: %d", r.Status)
	}

	// 冲突与校验。
	for _, c := range []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"已是有效成员", map[string]any{"email": cand.Email, "role_codes": []string{}}, 409, "resource.conflict"},
		{"用户不存在", map[string]any{"email": "nobody@example.test", "role_codes": []string{}}, 400, "validation.failed"},
		{"平台用户不可加入租户", map[string]any{"email": platformUser.Email, "role_codes": []string{}}, 400, "validation.failed"},
		{"未知角色", map[string]any{"email": owner.Email, "role_codes": []string{"no_such_role"}}, 400, "validation.failed"},
		{"平台角色不可授予租户成员", map[string]any{"email": owner.Email, "role_codes": []string{"platform_admin"}}, 400, "validation.failed"},
		{"缺 role_codes", map[string]any{"email": cand.Email}, 400, "validation.failed"},
		{"非法邮箱", map[string]any{"email": "x", "role_codes": []string{}}, 400, "validation.failed"},
		{"角色过多", map[string]any{"email": cand.Email, "role_codes": manyCodes(21)}, 400, "validation.failed"},
	} {
		r := a.Do("POST", path, c.body)
		if r.Status != c.status || r.ErrCode(t) != c.code {
			t.Errorf("%s: 期望 %d %s，得到 %d %s", c.name, c.status, c.code, r.Status, r.Body)
		}
	}
	// 用户不存在与平台用户：同一个错误体（不区分）。
	e1 := a.Do("POST", path, map[string]any{"email": "nobody@example.test", "role_codes": []string{}})
	e2 := a.Do("POST", path, map[string]any{"email": platformUser.Email, "role_codes": []string{}})
	if e1.ErrBodyKey(t) != e2.ErrBodyKey(t) {
		t.Errorf("不存在与平台用户应同形:\n%s\n%s", e1.ErrBodyKey(t), e2.ErrBodyKey(t))
	}
	if r := a.Do("POST", "/tenants/"+apistack.IDs.MustNew()+"/members", map[string]any{"email": cand.Email, "role_codes": []string{}}); r.Status != 404 {
		t.Errorf("租户不存在应 404: %d", r.Status)
	}

	// 已移除的成员可重新加入（置回 active，不新增行）。
	w.Root("UPDATE §.tenant_members SET status = 'removed' WHERE user_id = ? AND tenant_id = ?", cand.ID, tn.ID)
	if l := a.Do("GET", path, nil); l.JSON(t)["total"].(float64) != 1 {
		t.Fatalf("已移除成员不应出现在列表: %s", l.Body)
	}
	if r := a.Do("POST", path, map[string]any{"email": cand.Email, "role_codes": []string{"site_operator"}}); r.Status != 201 {
		t.Fatalf("重新加入: %d %s", r.Status, r.Body)
	}
	if w.RootInt("SELECT COUNT(*) FROM §.tenant_members WHERE user_id = ? AND tenant_id = ?", cand.ID, tn.ID) != 1 {
		t.Fatal("重新加入应复用成员行")
	}
	// 排序白名单与分页。
	if r := a.Do("GET", path+"?sort=-email&page_size=1&page=2", nil); r.Status != 200 || len(r.JSON(t)["items"].([]any)) != 1 {
		t.Fatalf("分页排序: %d %s", r.Status, r.Body)
	}
	if r := a.Do("GET", path+"?sort=password", nil); r.Status != 400 {
		t.Fatalf("排序白名单: %d", r.Status)
	}
}

func manyCodes(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("role_%d", i)
	}
	return out
}

// AC3：配额只存取，不强制（强制在 Story 6.9）。
func TestTenantQuotasStoredNotEnforced(t *testing.T) {
	w := apistack.New(t)
	a := admin(w)
	tn := w.SeedTenant("q")
	path := "/tenants/" + tn.UID + "/quotas"

	g := a.Do("GET", path, nil)
	if g.Status != 200 {
		t.Fatalf("getTenantQuotas: %d %s", g.Status, g.Body)
	}
	w.Contract("getTenantQuotas", "GET", path, g)
	items := g.JSON(t)["items"].([]any)
	if len(items) != 8 {
		t.Fatalf("应返回配额字典内的全部键（8 项）: %s", g.Body)
	}
	for _, it := range items {
		m := it.(map[string]any)
		if m["limit_value"] != nil || m["source"] != "default" {
			t.Errorf("未设置的配额应为 null/default: %v", m)
		}
		if _, has := m["used"]; has {
			t.Errorf("用量不在本 Story 提供（强制/用量属 6.9），不得伪造 used: %v", m)
		}
	}

	p := a.Do("PUT", path, map[string]any{"limits": map[string]any{"sites": 10, "members": 0, "api_tokens": nil}})
	if p.Status != 200 {
		t.Fatalf("putTenantQuotas: %d %s", p.Status, p.Body)
	}
	w.Contract("putTenantQuotas", "PUT", path, p)
	got := map[string]map[string]any{}
	for _, it := range p.JSON(t)["items"].([]any) {
		m := it.(map[string]any)
		got[m["quota_key"].(string)] = m
	}
	if got["sites"]["limit_value"].(float64) != 10 || got["sites"]["source"] != "manual" || got["members"]["limit_value"].(float64) != 0 || got["api_tokens"]["limit_value"] != nil || got["api_tokens"]["source"] != "manual" || got["certificates"]["source"] != "default" {
		t.Fatalf("PUT 结果: %s", p.Body)
	}
	if w.RootInt("SELECT COUNT(*) FROM §.tenant_quotas WHERE tenant_id = ? AND source = 'manual'", tn.ID) != 3 {
		t.Fatal("应存 3 行 manual 配额")
	}
	// 再次 PUT 同一键：更新而非新增。
	if r := a.Do("PUT", path, map[string]any{"limits": map[string]any{"sites": 20}}); r.Status != 200 || w.RootInt("SELECT limit_value FROM §.tenant_quotas WHERE tenant_id = ? AND quota_key = 'sites'", tn.ID) != 20 || w.RootInt("SELECT COUNT(*) FROM §.tenant_quotas WHERE tenant_id = ?", tn.ID) != 3 {
		t.Fatalf("更新: %d %s", r.Status, r.Body)
	}
	// 不强制：members 配额为 0，加成员仍然成功（强制属 Story 6.9，这里不假装强制）。
	u := w.SeedUser("tenant", &tn)
	w.Root("UPDATE §.tenant_members SET status = 'removed' WHERE user_id = ?", u.ID)
	if r := a.Do("POST", "/tenants/"+tn.UID+"/members", map[string]any{"email": u.Email, "role_codes": []string{}}); r.Status != 201 {
		t.Fatalf("配额只存取不强制，加成员应成功: %d %s", r.Status, r.Body)
	}

	bad := []map[string]any{
		{}, {"limits": map[string]any{}}, {"limits": map[string]any{"unknown_key": 1}}, {"limits": map[string]any{"sites": -1}},
		{"limits": map[string]any{"sites": 1.5}}, {"limits": map[string]any{"sites": "10"}}, {"limits": map[string]any{"sites": 9007199254740993}},
		{"limits": map[string]any{"sites": 1, "bogus": 2}}, {"limits": nil},
	}
	for i, b := range bad {
		r := a.Do("PUT", path, b)
		if r.Status != 400 || r.ErrCode(t) != "validation.failed" {
			t.Errorf("#%d %v 应 400: %d %s", i, b, r.Status, r.Body)
		}
	}
	if w.RootInt("SELECT limit_value FROM §.tenant_quotas WHERE tenant_id = ? AND quota_key = 'sites'", tn.ID) != 20 {
		t.Fatal("部分非法的 PUT 不得写入任何键（全部成功或全部不写）")
	}
	if r := a.Do("PUT", "/tenants/"+apistack.IDs.MustNew()+"/quotas", map[string]any{"limits": map[string]any{"sites": 1}}); r.Status != 404 {
		t.Fatalf("租户不存在应 404: %d", r.Status)
	}
	// 分页。
	if r := a.Do("GET", path+"?page_size=3&page=3", nil); r.Status != 200 || len(r.JSON(t)["items"].([]any)) != 2 || r.JSON(t)["total"].(float64) != 8 {
		t.Fatalf("配额分页: %d %s", r.Status, r.Body)
	}
}

// 写操作审计（Story 1.21 AC1）：updateTenant / putTenantQuotas / addTenantMember 在同一业务事务内写 audit_logs——操作者（平台管理员）、
// 目标租户（tenant_id = 被操作租户）、对象类型/uid、动作、结果、脱敏变更摘要、request_id；平台路由不是 acting-as（acting_as=0）。
// 全部 31 个审计 operation 的逐个驱动见 tests/integration/audit。
func TestTenantWritesAreAudited(t *testing.T) {
	w := apistack.New(t)
	a := admin(w)
	tn := w.SeedTenant("aud")
	rid := apistack.IDs.MustNew()
	r := a.Do("PATCH", "/tenants/"+tn.UID, map[string]any{"name": "审计观察", "settings": map[string]any{"theme": "dark"}}, apistack.WithHeader("X-Request-ID", rid))
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	q := "SELECT COUNT(*) FROM §.audit_logs WHERE request_id = ? AND action = ? AND actor_type = 'user' AND actor_user_id = ? AND tenant_id = ? AND acting_as = 0 AND resource_type = 'tenant' AND resource_uid = ? AND result = 'success'"
	if n := w.RootInt(q, rid, "tenant.update", a.U.ID, tn.ID, tn.UID); n != 1 {
		t.Fatalf("updateTenant 应恰好写一条审计（操作者 + 目标租户 + 对象）: %d", n)
	}
	if got := w.RootStr("SELECT CAST(changes_json AS CHAR) FROM §.audit_logs WHERE request_id = ?", rid); !strings.Contains(got, `"name": "审计观察"`) || !strings.Contains(got, `"settings_keys": ["theme"]`) || strings.Contains(got, "dark") {
		t.Errorf("变更摘要应含改动字段，settings 只记键名不记取值: %s", got)
	}
	rid2 := apistack.IDs.MustNew()
	if r := a.Do("PUT", "/tenants/"+tn.UID+"/quotas", map[string]any{"limits": map[string]any{"sites": 1}}, apistack.WithHeader("X-Request-ID", rid2)); r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if n := w.RootInt(q, rid2, "tenant.quotas.put", a.U.ID, tn.ID, tn.UID); n != 1 {
		t.Fatalf("putTenantQuotas 应恰好写一条审计: %d", n)
	}
	// 校验失败（400）的写请求没有发生变更，不写审计。
	before := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action LIKE 'tenant.%'")
	if r := a.Do("PATCH", "/tenants/"+tn.UID, map[string]any{"name": ""}); r.Status != 400 {
		t.Fatalf("%d", r.Status)
	}
	if after := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action LIKE 'tenant.%'"); after != before {
		t.Errorf("400 不得写审计: %d → %d", before, after)
	}
	if strings.Contains(w.Logs.String(), `"audit_logs_written"`) {
		t.Error("过渡的 audit_logs_written 日志标注应已移除")
	}
}

// R-T1 / 契约：租户资源的响应不含节点/集群内部标识；平台设置 placement.default_cluster_uids 不进入任何租户相关响应。
func TestTenantResponsesCarryNoClusterOrNodeIdentity(t *testing.T) {
	w := apistack.New(t)
	in := w.SeedInternals()
	a := admin(w)
	tn := w.SeedTenant("rt1")
	u := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(u, "tenant_owner")
	s := w.Login(u)
	opts := leakguard.Options{Internal: in.Strings(), ForbidIPValues: true}

	for name, r := range map[string]apistack.Resp{
		"platform list":    a.Do("GET", "/tenants", nil),
		"platform get":     a.Do("GET", "/tenants/"+tn.UID, nil),
		"platform members": a.Do("GET", "/tenants/"+tn.UID+"/members", nil),
		"platform quotas":  a.Do("GET", "/tenants/"+tn.UID+"/quotas", nil),
		"tenant current":   s.Do("GET", "/tenants/current", nil),
		"acting-as":        a.Do("GET", "/tenants/current", nil, apistack.ActingAs(tn.UID)),
	} {
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", name, r.Status, r.Body)
		}
		if vs := leakguard.Scan(r.Body, opts); len(vs) > 0 {
			t.Errorf("%s 含内部标识: %v", name, vs)
		}
	}
}
