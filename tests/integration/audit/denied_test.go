package auditit

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// Story 1.22b：被拒绝类审计（audit_logs.result=denied、action=access.denied）。
// 范围：只记「已认证主体」的授权类拒绝（403 auth.forbidden / auth.reauth_required / feature.disabled）；
// 未认证请求、校验失败、404、CSRF 失败、登录失败一律不写 audit_logs（登录失败只写 login_logs）。
// 防刷：同一 (主体, operation, 窗口) 只记一条；每个主体每窗口有上限；Redis 不可用时降级；写入失败不影响响应。

const deniedWhere = "action = 'access.denied' AND result = 'denied'"

// deniedProbes 注册 n 条代表「无人拥有其权限」的业务探针路由（经真实 Router.Register，走真实钩子链），以及一条商业类（x-feature: commercial）探针。
func deniedProbes(n int) func(w *apistack.World, r *httpx.Router) {
	return func(_ *apistack.World, r *httpx.Router) {
		ok := func(c *gin.Context) { httpx.OK(c, map[string]bool{"reached": true}) }
		for i := 0; i < n; i++ {
			m := httpx.RouteMeta{OperationID: fmt.Sprintf("probeDenied%d", i), Method: "GET", Path: fmt.Sprintf("/probe/denied%d", i), Auth: httpx.AuthRequired,
				Permissions: []string{"node:read"}, TenantScope: httpx.ScopeEither, Phase: "M1"}
			if err := r.Register(m, ok); err != nil {
				panic(err)
			}
		}
		m := httpx.RouteMeta{OperationID: "probeCommercial", Method: "GET", Path: "/probe/commercial", Auth: httpx.AuthRequired, Feature: "commercial",
			Permissions: []string{}, TenantScope: httpx.ScopeEither, Phase: "M1"}
		if err := r.Register(m, ok); err != nil {
			panic(err)
		}
	}
}

// restrictedMember 是租户内只有 task:read 的成员（调用 cancelTask 等写操作会被 RBAC 拒绝）。
func restrictedMember(w *apistack.World, tn apistack.Tenant) apistack.Session {
	u := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(u, w.CustomRole("tenant", "task:read"))
	return w.Login(u)
}

func TestDeniedAuditRecordedForAuthenticatedPrincipal(t *testing.T) {
	w := apistack.New(t, apistack.Options{Routes: deniedProbes(2)})
	tn := w.SeedTenant("den")
	m := restrictedMember(w, tn)
	pending := w.SeedTaskStatus(&tn.ID, "pending", map[string]any{"note": "denied"})

	r := m.Do("POST", "/tasks/"+pending+"/cancel", nil)
	if r.Status != 403 || r.ErrCode(t) != "auth.forbidden" {
		t.Fatalf("无 task:cancel 应 403 auth.forbidden: %d %s", r.Status, r.Body)
	}
	rid := r.Header.Get("X-Request-ID")
	// 一条 denied 行：主体归因、对象是被拒绝的 operation、租户=主体所属租户、来源 IP/UA/请求 ID 齐全；变更摘要只含拒绝环节与路由元数据。
	if n := auditCount(w, deniedWhere+" AND resource_type = 'operation' AND resource_uid = 'cancelTask' AND actor_type = 'user' AND actor_user_id = ? AND actor_label = ? AND actor_tenant_id = ? AND tenant_id = ? AND acting_as = 0 AND request_id = ? AND ip IS NOT NULL",
		m.U.ID, m.U.Email, tn.ID, tn.ID, rid); n != 1 {
		t.Fatalf("应恰有 1 条被拒绝审计，实际 %d", n)
	}
	ch := w.RootStr("SELECT CAST(changes_json AS CHAR) FROM §.audit_logs WHERE "+deniedWhere+" AND request_id = ?", rid)
	for _, want := range []string{`"stage": "authorize"`, `"code": "auth.forbidden"`, `"method": "POST"`, `"permissions": ["task:cancel"]`} {
		if !strings.Contains(ch, want) {
			t.Errorf("changes 应含 %s: %s", want, ch)
		}
	}
	if strings.Contains(ch, pending) || strings.Contains(ch, m.Token) {
		t.Errorf("changes 不得含请求参数/令牌: %s", ch)
	}

	// 去重：同一 (主体, operation) 在窗口内重复拒绝仍只有一条。
	for i := 0; i < 5; i++ {
		if r := m.Do("POST", "/tasks/"+pending+"/cancel", nil); r.Status != 403 {
			t.Fatalf("重复拒绝: %d", r.Status)
		}
	}
	if n := auditCount(w, deniedWhere+" AND resource_uid = 'cancelTask' AND actor_user_id = ?", m.U.ID); n != 1 {
		t.Errorf("同窗口重复拒绝应只记一条，实际 %d", n)
	}
	if w.Audit.DeniedSuppressed() < 5 {
		t.Errorf("被抑制计数应 >= 5: %d", w.Audit.DeniedSuppressed())
	}
	// 另一个 operation 是另一条；另一个主体也各记各的。
	if r := m.Do("GET", "/probe/denied0", nil); r.Status != 403 {
		t.Fatalf("租户成员访问无权限的路由应 403: %d %s", r.Status, r.Body)
	}
	m2 := restrictedMember(w, tn)
	if r := m2.Do("POST", "/tasks/"+pending+"/cancel", nil); r.Status != 403 {
		t.Fatalf("%d", r.Status)
	}
	if n := auditCount(w, deniedWhere+" AND tenant_id = ?", tn.ID); n != 3 {
		t.Errorf("(m,cancelTask) (m,probeDenied0) (m2,cancelTask) 共 3 条，实际 %d", n)
	}

	// 平台管理员有权限但未再认证：auth.reauth_required 同样被记录（actor_tenant_id/tenant_id 为空）。
	adm := admin(w)
	rr := adm.Do("POST", "/admin/users", map[string]any{"email": "x-" + apistack.IDs.MustNew() + "@example.test", "display_name": "x", "user_type": "platform", "role_codes": []string{}},
		apistack.WithHeader("Idempotency-Key", "den-"+apistack.IDs.MustNew()))
	if rr.Status != 403 || rr.ErrCode(t) != "auth.reauth_required" {
		t.Fatalf("未再认证应 403 auth.reauth_required: %d %s", rr.Status, rr.Body)
	}
	if n := auditCount(w, deniedWhere+" AND resource_uid = 'createUser' AND actor_user_id = ? AND tenant_id IS NULL AND actor_tenant_id IS NULL AND JSON_UNQUOTE(JSON_EXTRACT(changes_json, '$.code')) = 'auth.reauth_required'", adm.U.ID); n != 1 {
		t.Errorf("reauth_required 应被记录 1 条: %d", n)
	}

	// 可经审计 API 读取：租户视角看到本租户成员的越权尝试（操作者显示展示名，不是邮箱）；契约相符。
	owner := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(owner, "tenant_owner")
	os := w.Login(owner)
	lr := os.Do("GET", "/audit-logs?result=denied&limit=50", nil)
	w.Contract("listAuditLogs", "GET", "/audit-logs?result=denied&limit=50", lr)
	items := itemsOf(t, lr)
	if lr.Status != 200 || len(items) != 3 {
		t.Fatalf("租户应看到本租户 3 条被拒绝记录: %d %s", lr.Status, lr.Body)
	}
	for _, it := range items {
		if it["result"] != "denied" || it["action"] != "access.denied" || it["resource_type"] != "operation" || strings.Contains(fmt.Sprint(it["actor_label"]), "@") {
			t.Errorf("被拒绝记录的对外形态不符: %v", it)
		}
	}
	if pv := adm.Do("GET", "/audit-logs?result=denied&limit=100", nil); pv.Status != 200 || len(itemsOf(t, pv)) != 4 {
		t.Errorf("平台视角应看到全部 4 条: %d %s", pv.Status, pv.Body)
	}
}

// 未认证请求、校验失败、404、CSRF 失败、登录失败：一行 audit_logs 都不产生（登录失败只写 login_logs）。
func TestDeniedAuditNotWrittenForUnauthenticatedOrNonAuthorizationFailures(t *testing.T) {
	w := apistack.New(t)
	tn := w.SeedTenant("unauth")
	m := restrictedMember(w, tn)
	adm := admin(w) // 登录本身会写 auth.login 审计：先于基线创建
	before := auditCount(w, "1 = 1")
	loginsBefore := w.RootInt("SELECT COUNT(*) FROM §.login_logs WHERE event LIKE '%fail%'")

	expect := func(what string, r apistack.Resp, status int) {
		t.Helper()
		if r.Status != status {
			t.Fatalf("%s：期望 %d，得到 %d %s", what, status, r.Status, r.Body)
		}
	}
	expect("无凭据访问受保护路由", w.Do("GET", "/admin/users", nil), 401)
	expect("无凭据访问租户路由", w.Do("GET", "/tasks", nil), 401)
	expect("伪造 Bearer", w.Bearer("le_abcdefgh_"+strings.Repeat("A", 43), "GET", "/admin/users", nil), 401)
	expect("伪造会话 Cookie", w.Do("GET", "/admin/users", nil, apistack.WithCookie(apistack.CookieName("admin"), strings.Repeat("a", 43))), 401)
	for i := 0; i < 5; i++ { // 登录失败：错误口令
		expect("错误口令", w.Do("POST", "/auth/login", map[string]string{"realm": "admin", "email": "nobody-" + apistack.IDs.MustNew() + "@example.test", pwKey: apistack.RandPassword(t)}), 401)
	}
	if r := w.Do("POST", "/auth/login", map[string]any{"realm": "admin"}); r.Status != 400 && r.Status != 422 {
		t.Errorf("缺字段的登录应是校验失败: %d", r.Status)
	}
	// 已认证主体的非授权类失败：CSRF 缺失（会话主体但 CSRF 校验在认证钩子内失败）、404（acting-as 不存在的租户）、校验失败。
	if r := w.Do("POST", "/tasks/"+apistack.IDs.MustNew()+"/cancel", nil, apistack.WithCookie(apistack.CookieName("console"), m.Token)); r.Status != 403 || r.ErrCode(t) != "auth.csrf_invalid" {
		t.Errorf("缺 CSRF 应 403 auth.csrf_invalid: %d %s", r.Status, r.Body)
	}
	expect("acting-as 不存在的租户 → 404", adm.Do("GET", "/tasks", nil, apistack.ActingAs(apistack.IDs.MustNew())), 404)
	expect("他租户资源 → 404", m.Do("GET", "/tasks/"+apistack.IDs.MustNew(), nil), 404)
	expect("已认证主体的校验失败", m.Do("GET", "/tasks?page_size=-5", nil), 400)

	if got := auditCount(w, "1 = 1"); got != before {
		t.Fatalf("上述请求不应产生任何 audit_logs 行：之前 %d 之后 %d", before, got)
	}
	if n := auditCount(w, deniedWhere); n != 0 {
		t.Fatalf("不应有被拒绝审计: %d", n)
	}
	if got := w.RootInt("SELECT COUNT(*) FROM §.login_logs WHERE event LIKE '%fail%'"); got < loginsBefore+5 {
		t.Errorf("登录失败仍应写 login_logs: %d -> %d", loginsBefore, got)
	}
}

// 窗口过后可再记一条（去重窗口是固定窗口，不是永久抑制）。
func TestDeniedAuditWindowExpires(t *testing.T) {
	w := apistack.New(t, apistack.Options{Routes: deniedProbes(1), DeniedWindow: 1500 * time.Millisecond})
	tn := w.SeedTenant("win")
	m := restrictedMember(w, tn)
	deny := func() {
		t.Helper()
		if r := m.Do("GET", "/probe/denied0", nil); r.Status != 403 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	}
	deny()
	deny()
	if n := auditCount(w, deniedWhere); n != 1 {
		t.Fatalf("窗口内应 1 条: %d", n)
	}
	time.Sleep(1700 * time.Millisecond)
	deny()
	if n := auditCount(w, deniedWhere); n != 2 {
		t.Errorf("窗口过后应再记一条（共 2）: %d", n)
	}
}

// 主体级上限：同一主体一个窗口内最多 N 条（即使对很多不同的 operation）；其他主体不受影响；功能开关拒绝（feature.disabled）同样记录。
func TestDeniedAuditPerActorCapAndFeatureDisabled(t *testing.T) {
	w := apistack.New(t, apistack.Options{Routes: deniedProbes(6), DeniedActorCap: 3})
	tn := w.SeedTenant("cap")
	a, b := restrictedMember(w, tn), restrictedMember(w, tn)
	for i := 0; i < 6; i++ {
		if r := a.Do("GET", fmt.Sprintf("/probe/denied%d", i), nil); r.Status != 403 {
			t.Fatalf("探针 %d: %d %s", i, r.Status, r.Body)
		}
	}
	if n := auditCount(w, deniedWhere+" AND actor_user_id = ?", a.U.ID); n != 3 {
		t.Errorf("主体级上限 3：实际 %d", n)
	}
	if r := b.Do("GET", "/probe/denied5", nil); r.Status != 403 {
		t.Fatal(r.Status)
	}
	if n := auditCount(w, deniedWhere+" AND actor_user_id = ?", b.U.ID); n != 1 {
		t.Errorf("另一个主体不受影响: %d", n)
	}
	// feature.disabled：自用模式下商业类路由对已认证主体 403 feature.disabled 并留痕；未认证仍是 401 且无记录。
	if r := w.Do("GET", "/probe/commercial", nil); r.Status != 401 {
		t.Fatalf("未认证应 401: %d", r.Status)
	}
	if n := auditCount(w, deniedWhere+" AND resource_uid = 'probeCommercial'"); n != 0 {
		t.Fatalf("未认证不得留痕: %d", n)
	}
	adm := admin(w)
	if r := adm.Do("GET", "/probe/commercial", nil); r.Status != 403 || r.ErrCode(t) != "feature.disabled" {
		t.Fatalf("自用模式商业路由应 403 feature.disabled: %d %s", r.Status, r.Body)
	}
	ch := w.RootStr("SELECT CAST(changes_json AS CHAR) FROM §.audit_logs WHERE " + deniedWhere + " AND resource_uid = 'probeCommercial'")
	if !strings.Contains(ch, `"stage": "feature"`) || !strings.Contains(ch, `"feature": "commercial"`) {
		t.Errorf("feature.disabled 的摘要: %s", ch)
	}
}

// 写入失败不影响响应（与写操作审计的 fail-closed 相反）；Redis 不可用时降级照常记录且响应不变。
func TestDeniedAuditWriteFailureAndRedisDownDoNotAffectResponses(t *testing.T) {
	w := apistack.New(t, apistack.Options{Routes: deniedProbes(2)})
	tn := w.SeedTenant("fail")
	m := restrictedMember(w, tn)
	base := m.Do("GET", "/probe/denied0", nil)
	if base.Status != 403 {
		t.Fatalf("%d", base.Status)
	}
	if n := auditCount(w, deniedWhere); n != 1 {
		t.Fatalf("%d", n)
	}

	// 让被拒绝类审计的 INSERT 失败（临时库里加 BEFORE INSERT 触发器，只拦 access.denied；写操作审计不受影响）。
	w.Root("CREATE TRIGGER §.trg_deny_denied_audit BEFORE INSERT ON §.audit_logs FOR EACH ROW BEGIN IF NEW.action = 'access.denied' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected'; END IF; END")
	failed := w.Audit.DeniedWriteFailures()
	r := m.Do("GET", "/probe/denied1", nil)
	if r.Status != 403 || r.ErrCode(t) != "auth.forbidden" || string(r.Body) == "" {
		t.Fatalf("审计写入失败不得改变响应: %d %s", r.Status, r.Body)
	}
	// 响应体与成功记录时同形（仅 request_id 不同）。
	if base.ErrBodyKey(t) != r.ErrBodyKey(t) {
		t.Errorf("响应体应一致:\n%s\n%s", base.Body, r.Body)
	}
	if w.Audit.DeniedWriteFailures() != failed+1 {
		t.Errorf("写入失败应计数: %d -> %d", failed, w.Audit.DeniedWriteFailures())
	}
	if n := auditCount(w, deniedWhere+" AND resource_uid = 'probeDenied1'"); n != 0 {
		t.Errorf("注入失败时不应有记录: %d", n)
	}
	w.Root("DROP TRIGGER §.trg_deny_denied_audit")

	// Redis 被杀：去重降级为进程内计数，仍记录；响应不变。
	w.Redis.Kill()
	r2 := restrictedMember2(t, w, tn)
	if x := r2.Do("GET", "/probe/denied0", nil); x.Status != 403 || x.ErrCode(t) != "auth.forbidden" {
		t.Fatalf("Redis 不可用时响应不变: %d %s", x.Status, x.Body)
	}
	if n := auditCount(w, deniedWhere+" AND actor_user_id = ?", r2.U.ID); n != 1 {
		t.Errorf("Redis 不可用时降级后仍应记录 1 条: %d", n)
	}
	r2.Do("GET", "/probe/denied0", nil)
	if n := auditCount(w, deniedWhere+" AND actor_user_id = ?", r2.U.ID); n != 1 {
		t.Errorf("降级时进程内去重仍生效: %d", n)
	}
}

// restrictedMember2 在 Redis 已被杀之后创建主体：登录不依赖 Redis（降级运行）。
func restrictedMember2(t *testing.T, w *apistack.World, tn apistack.Tenant) apistack.Session {
	t.Helper()
	return restrictedMember(w, tn)
}
