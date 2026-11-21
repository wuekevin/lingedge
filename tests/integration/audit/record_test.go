package auditit

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// recordOutsideTx 直接用真实记录器在没有事务的 ctx 上写一条审计。
func recordOutsideTx(w *apistack.World) error {
	return w.Audit.Record(context.Background(), audit.ActionTaskCancel, audit.Event{ResourceType: audit.ResourceTask, Actor: &audit.Actor{Type: audit.ActorSystem, Label: "x"}})
}

// AC1：Audit=true 的路由，写操作执行后记录操作者、租户、acting_as、对象类型/uid、动作、结果、脱敏的变更摘要、request_id、来源 IP。
// 本用例逐个驱动 audit.Catalog 登记的全部 31 个 operation（真实 API、真实事务），断言每次成功都恰好留下一条对应 request_id 的审计行；
// 最后断言 Catalog 里的每个动作都真的出现在 audit_logs（「声明 Audit 但未记录」的动态检查），且运行期兜底计数为 0。
// 同时做 G-5 脱敏扫描：运行期生成的口令/令牌/TOTP 种子/恢复码/MFA 令牌不得出现在任何审计行与审计 API 响应里。
func TestEveryAuditedOperationWritesExactlyOneAuditRow(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	tn, other := w.SeedTenant("rec"), w.SeedTenant("rec-other")
	owner := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(owner, "tenant_owner")

	seen := map[string]bool{}
	secrets := []string{}
	keep := func(vals ...string) {
		for _, v := range vals {
			if v != "" {
				secrets = append(secrets, v)
				w.Secret(v)
			}
		}
	}
	// expect 断言一次成功调用恰好写了一条审计，返回审计行的 uid。
	expect := func(op, action string, r apistack.Resp, wantStatus int) {
		t.Helper()
		if r.Status != wantStatus {
			t.Fatalf("%s: 期望 %d，得到 %d %s", op, wantStatus, r.Status, r.Body)
		}
		rid := r.Header.Get("X-Request-ID")
		if n := auditCount(w, "request_id = ? AND action = ? AND result = 'success'", rid, action); n != 1 {
			t.Fatalf("%s：应恰好留下 1 条 %s 审计（request_id=%s）: %d", op, action, rid, n)
		}
		if n := auditCount(w, "request_id = ?", rid); n != 1 {
			t.Fatalf("%s：一次调用应恰好一条审计: %d", op, n)
		}
		seen[action] = true
	}

	// ---- 登录 / 会话 / 再认证 ----
	ownerLogin := w.Do("POST", "/auth/login", map[string]string{"realm": "console", "email": owner.Email, pwKey: owner.Password}, apistack.WithHeader("X-Forwarded-For", "10.9.0.1"))
	expect("login", "auth.login", ownerLogin, 200)
	row := w.RootInt("SELECT id FROM §.audit_logs WHERE request_id = ?", ownerLogin.Header.Get("X-Request-ID"))
	if n := auditCount(w, "id = ? AND actor_type = 'user' AND actor_user_id = ? AND actor_label = ? AND actor_tenant_id = ? AND tenant_id = ? AND acting_as = 0 AND resource_type = 'session' AND ip = INET6_ATON('10.9.0.1')",
		row, owner.ID, owner.Email, tn.ID, tn.ID); n != 1 {
		t.Fatalf("登录审计字段（操作者/租户/对象/来源 IP）不符")
	}
	ownerSes := apistack.Session{W: w, U: owner, Token: ownerLogin.Cookie(apistack.CookieName("console")).Value, CSRF: ownerLogin.JSON(t)["csrf_token"].(string)}
	keep(ownerSes.Token, ownerSes.CSRF)
	adm.Reauth()
	expect("reauth", "auth.reauth", adm.Do("POST", "/auth/reauth", map[string]string{pwKey: adm.U.Password}), 200)

	// 第二个会话：revokeSession（吊销自己的另一个会话）。
	second := w.Login(owner)
	sl := ownerSes.Do("GET", "/auth/sessions", nil)
	var secondUID string
	for _, it := range itemsOf(t, sl) {
		if cur, _ := it["current"].(bool); !cur {
			secondUID = it["session_ref"].(string)
		}
	}
	if secondUID == "" {
		t.Fatalf("应能列出另一个会话: %s", sl.Body)
	}
	_ = second
	expect("revokeSession", "auth.session.revoke", ownerSes.Do("DELETE", "/auth/sessions/"+secondUID, nil), 204)

	// ---- TOTP：setup → enable → login(MFA 挑战) → verify → disable ----
	tu := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(tu, "tenant_owner")
	ts := w.Login(tu)
	su := ts.Do("POST", "/auth/totp/setup", nil)
	expect("setupTotp", "auth.totp.setup", su, 200)
	u, err := url.Parse(su.JSON(t)["otpauth_uri"].(string))
	if err != nil {
		t.Fatal(err)
	}
	secret := u.Query().Get("secret")
	keep(secret)
	code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	en := ts.Do("POST", "/auth/totp/enable", map[string]string{"code": code})
	expect("enableTotp", "auth.totp.enable", en, 200)
	var recovery []string
	for _, c := range en.JSON(t)["recovery_codes"].([]any) {
		recovery = append(recovery, c.(string))
	}
	keep(recovery...)
	ml := w.Do("POST", "/auth/login", map[string]string{"realm": "console", "email": tu.Email, pwKey: tu.Password}, apistack.WithHeader("X-Forwarded-For", "10.9.0.2"))
	expect("login(MFA 挑战)", "auth.mfa_challenge", ml, 200)
	mfa, _ := ml.JSON(t)["mfa_token"].(string)
	keep(mfa)
	vr := w.Do("POST", "/auth/totp/verify", map[string]string{"mfa_token": mfa, "code": recovery[0]}, apistack.WithHeader("X-Forwarded-For", "10.9.0.3"))
	expect("verifyTotp", "auth.login", vr, 200)
	expect("disableTotp", "auth.totp.disable", ts.Do("POST", "/auth/totp/disable", map[string]string{pwKey: tu.Password, "code": recovery[1]}), 204)

	// ---- 改密 / 登出 ----
	pu := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(pu, "tenant_owner")
	ps := w.Login(pu)
	np := apistack.RandPassword(t)
	keep(np, pu.Password)
	expect("changePassword", "auth.password.change", ps.Do("POST", "/auth/password/change", map[string]string{"current_password": pu.Password, "new_password": np}), 204)
	lu := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(lu, "tenant_owner")
	expect("logout", "auth.logout", w.Login(lu).Do("POST", "/auth/logout", nil), 204)

	// ---- /admin 用户与角色 ----
	email := strings.ToLower(apistack.IDs.MustNew()) + "@example.test"
	cu := adm.Do("POST", "/admin/users", map[string]any{"email": email, "display_name": "审计对象", "user_type": "platform", "role_codes": []string{}}, apistack.WithHeader("Idempotency-Key", "rec-"+apistack.IDs.MustNew()))
	expect("createUser", "user.create", cu, 201)
	keep(cu.JSON(t)["temporary_password"].(string))
	uuid := cu.JSON(t)["uid"].(string)
	expect("updateUser", "user.update", adm.Do("PATCH", "/admin/users/"+uuid, map[string]any{"display_name": "改名"}), 200)
	expect("setUserRoles", "user.set_roles", adm.Do("PUT", "/admin/users/"+uuid+"/roles", map[string]any{"role_codes": []string{"platform_auditor"}}), 200)
	expect("unlockUser", "user.unlock", adm.Do("POST", "/admin/users/"+uuid+"/unlock", nil), 200)
	rp := adm.Do("POST", "/admin/users/"+uuid+"/reset-password", map[string]any{})
	expect("resetUserPassword", "user.reset_password", rp, 200)
	if tp, _ := rp.JSON(t)["temporary_password"].(string); tp != "" {
		keep(tp)
	}
	expect("revokeUserSessions", "user.revoke_sessions", adm.Do("DELETE", "/admin/users/"+uuid+"/sessions", nil), 204)
	expect("deleteUser", "user.delete", adm.Do("DELETE", "/admin/users/"+uuid, nil), 204)

	cr := adm.Do("POST", "/admin/roles", map[string]any{"scope": "tenant", "code": "audit_probe_role", "name": "审计探针角色", "permissions": []string{"task:read"}}, apistack.WithHeader("Idempotency-Key", "rec-"+apistack.IDs.MustNew()))
	expect("createRole", "role.create", cr, 201)
	rid := cr.JSON(t)["uid"].(string)
	expect("updateRole", "role.update", adm.Do("PATCH", "/admin/roles/"+rid, map[string]any{"name": "改名后的角色"}), 200)
	expect("setRolePermissions", "role.set_permissions", adm.Do("PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{"task:read", "audit:read"}}), 200)
	expect("deleteRole", "role.delete", adm.Do("DELETE", "/admin/roles/"+rid, nil), 204)

	// ---- 租户 ----
	expect("updateTenant", "tenant.update", adm.Do("PATCH", "/tenants/"+tn.UID, map[string]any{"name": "审计租户", "settings": map[string]any{"theme": "dark"}}), 200)
	expect("putTenantQuotas", "tenant.quotas.put", adm.Do("PUT", "/tenants/"+tn.UID+"/quotas", map[string]any{"limits": map[string]any{"sites": 7, "members": nil}}), 200)
	cand := w.SeedUser("tenant", &other)
	w.Root("UPDATE §.tenant_members SET status = 'removed' WHERE user_id = ?", cand.ID)
	expect("addTenantMember", "tenant.member.add", adm.Do("POST", "/tenants/"+tn.UID+"/members", map[string]any{"email": cand.Email, "role_codes": []string{"tenant_viewer"}}), 201)

	// ---- 任务 ----
	task := w.SeedTaskStatus(&tn.ID, "pending", map[string]any{"note": "rec"})
	expect("cancelTask", "task.cancel", ownerSes.Do("POST", "/tasks/"+task+"/cancel", nil), 200)

	// ---- 系统设置（Story 1.22）：通用 PATCH、品牌、运行模式（x-reauth，上面已再认证）、SSRF 白名单 ----
	expect("patchSettings", "setting.update", adm.Do("PATCH", "/admin/settings", map[string]any{"values": map[string]any{"login.throttle_ip_max": 40, "brand.name": "审计品牌"}}), 200)
	expect("putBrandSettings", "setting.brand.update", adm.Do("PUT", "/admin/settings/brand", map[string]any{"name": "审计品牌", "primary_color": "#102030"}), 200)
	expect("putRunMode", "system.run_mode.update", adm.Do("PUT", "/admin/system/run-mode", map[string]any{"run_mode": "commercial"}), 200)
	expect("putSsrfAllowlist", "setting.ssrf_allowlist.update", adm.Do("PUT", "/admin/settings/ssrf-allowlist", map[string]any{"entries": []map[string]string{{"cidr": "10.8.0.0/16", "note": "审计"}}}), 200)

	// ---- API Token ----
	ct := ownerSes.Do("POST", "/auth/api-tokens", map[string]any{"name": "审计令牌", "scopes": []string{"task:read"}})
	expect("createApiToken", "api_token.create", ct, 201)
	keep(ct.JSON(t)["token"].(string))
	tid := ct.JSON(t)["uid"].(string)
	rt := ownerSes.Do("POST", "/auth/api-tokens/"+tid+"/rotate", nil)
	expect("rotateApiToken", "api_token.rotate", rt, 200)
	keep(rt.JSON(t)["token"].(string))
	expect("revokeApiToken", "api_token.revoke", ownerSes.Do("DELETE", "/auth/api-tokens/"+rt.JSON(t)["uid"].(string), nil), 204)

	// ---- 动态「声明了审计就必须真的记录」：Catalog 的每个动作都出现在 audit_logs；运行期兜底为 0 ----
	var catalogActions []string
	for _, op := range audit.Catalog {
		for _, a := range op.Actions {
			catalogActions = append(catalogActions, string(a))
		}
	}
	// 安装向导（Story 1.24）的 8 个 operation 只存在于 setup mode 的路由器上（正常模式下恒 410，apistack 的进程内路由里不会被驱动）：它们的动作
	// （setup.*，Actor=系统组件 setup）由 tests/integration/setup 驱动并逐个断言（TestFullWizardCreateAccountsMode：每个动作的审计行数与 Actor），
	// 这里只排除「所有动作都以 setup. 开头」的 operation，其余 31 个照旧必须被本用例驱动。
	isSetupOp := func(op audit.Op) bool {
		for _, a := range op.Actions {
			if !strings.HasPrefix(string(a), "setup.") {
				return false
			}
		}
		return len(op.Actions) > 0
	}
	driven := 0
	for _, op := range audit.Catalog {
		if !isSetupOp(op) {
			driven++
		}
	}
	catalogActions = catalogActions[:0]
	for _, op := range audit.Catalog {
		if isSetupOp(op) {
			continue
		}
		for _, a := range op.Actions {
			catalogActions = append(catalogActions, string(a))
		}
	}
	for _, a := range catalogActions {
		if !seen[a] {
			t.Errorf("Catalog 动作 %s 没有被本用例驱动（补驱动，不得放过）", a)
		}
		if n := auditCount(w, "action = ?", a); n < 1 {
			t.Errorf("Catalog 动作 %s 声明了审计却从未被记录", a)
		}
	}
	if driven != 31 {
		t.Errorf("Catalog 里由本用例驱动的 operation 应为 31 个（与 x-audit: true 的已实现路由一致，不含 setup 向导的 8 个）: %d", driven)
	}
	if n := w.Audit.Missing(); n != 0 {
		t.Errorf("运行期兜底：声明了审计却没有记录的请求数应为 0: %d", n)
	}

	// ---- G-5 脱敏扫描：运行期生成的敏感值不得出现在任何审计行或审计 API 响应里（含正向对照）----
	api := adm.Do("GET", "/audit-logs?limit=200", nil)
	if api.Status != 200 {
		t.Fatalf("%d %s", api.Status, api.Body)
	}
	for _, sct := range secrets {
		if len(sct) < 8 {
			continue
		}
		if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE CONVERT(CONCAT_WS(' ', actor_label, resource_uid, CAST(changes_json AS CHAR), CAST(meta_json AS CHAR), user_agent) USING utf8mb4) LIKE ?", "%"+sct+"%"); n != 0 {
			t.Errorf("审计行含敏感值 %s…", sct[:4])
		}
		if strings.Contains(string(api.Body), sct) {
			t.Errorf("审计 API 响应含敏感值 %s…", sct[:4])
		}
	}
	probe := "probe-" + apistack.IDs.MustNew()
	w.Root("INSERT INTO §.audit_logs (uid, actor_type, actor_label, action, resource_type, result, changes_json, request_id) VALUES (?, 'system', 's', 'test.plant', 'test', 'success', JSON_OBJECT('v', ?), ?)", apistack.IDs.MustNew(), probe, apistack.IDs.MustNew())
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE CONVERT(CAST(changes_json AS CHAR) USING utf8mb4) LIKE ?", "%"+probe+"%"); n != 1 {
		t.Fatal("扫描器应能发现种入的值（正向对照，证明扫描有区分力）")
	}
}

// 变更摘要写入前一律经 pkg/redact：敏感键整体替换为 ***，值里嵌入的令牌形态也被清洗；摘要超限不会让审计写入失败。
func TestChangesAreRedactedBeforeStorage(t *testing.T) {
	w := apistack.New(t)
	planted := "Sup3rSecretValue-" + apistack.IDs.MustNew()
	big := strings.Repeat("x", 20<<10)
	err := w.App.WithTx(apistack.Ctx(t), func(ctx context.Context) error {
		if err := w.Audit.Record(ctx, audit.ActionTaskCancel, audit.Event{ResourceType: audit.ResourceTask, ResourceUID: "redact-1",
			Actor:   &audit.Actor{Type: audit.ActorSystem, Label: "redact-test"},
			Changes: map[string]any{"password": planted, "api_token": planted, "totp_secret": planted, "recovery_codes": []string{planted}, "nested": map[string]any{"client_secret": planted, "ok": "visible"}, "name": "kept"}}); err != nil {
			return err
		}
		return w.Audit.Record(ctx, audit.ActionTaskCancel, audit.Event{ResourceType: audit.ResourceTask, ResourceUID: "redact-big",
			Actor: &audit.Actor{Type: audit.ActorSystem, Label: "redact-test"}, Changes: map[string]any{"big": big}})
	})
	if err != nil {
		t.Fatal(err)
	}
	stored := w.RootStr("SELECT CAST(changes_json AS CHAR) FROM §.audit_logs WHERE resource_uid = 'redact-1'")
	if strings.Contains(stored, planted) {
		t.Fatalf("变更摘要含明文敏感值: %s", stored)
	}
	for _, want := range []string{`"password": "***"`, `"api_token": "***"`, `"totp_secret": "***"`, `"client_secret": "***"`, `"ok": "visible"`, `"name": "kept"`} {
		if !strings.Contains(stored, want) {
			t.Errorf("摘要应含 %s: %s", want, stored)
		}
	}
	if got := w.RootStr("SELECT CAST(changes_json AS CHAR) FROM §.audit_logs WHERE resource_uid = 'redact-big'"); !strings.Contains(got, "truncated") {
		t.Errorf("超限摘要应被整体替换为 truncated 标记: %.80s", got)
	}
}
