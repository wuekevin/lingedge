package authit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Story 1.19：API Token（作用域 / 吊销 / 轮换）。真实认证模块 + 真实 httpx 钩子链 + 自拉起的临时 MySQL/Redis，无任何 Mock。
// 覆盖：AC1 令牌形态与明文仅一次、AC2 scopes ⊆ 创建者权限 / CIDR / 过期 / 近期 MFA、AC3 吊销即时性与轮换链、AC4 Bearer 认证（租户令牌不能访问平台资源、作用域越界）。

// ---- AC1：令牌形态、SHA-256 + 前缀入库、明文仅创建响应返回一次 ----

func TestAPITokenCreatePlaintextOnce(t *testing.T) {
	s := newTokStack(t)
	adm := s.adminSession("platform_admin")
	key := ids.MustNew()
	body := map[string]any{"name": "ci 流水线", "scopes": []string{"user:read", "role:read"}, "allowed_cidrs": []string{"198.51.100.0/24", "203.0.113.7", "2001:db8::/32"}}
	r := s.as(adm, "POST", "/auth/api-tokens", body, withHeader("Idempotency-Key", key))
	if r.Status != 201 {
		t.Fatalf("创建: %d %s", r.Status, r.Body)
	}
	s.contract("createApiToken", "POST", "/auth/api-tokens", r)
	tk := s.parseIssued(r)
	if r.Header.Get("Cache-Control") != "no-store" || !strings.HasSuffix(r.Header.Get("Location"), "/auth/api-tokens/"+tk.UID) {
		t.Errorf("响应头: Cache-Control=%q Location=%q", r.Header.Get("Cache-Control"), r.Header.Get("Location"))
	}
	if tk.JSON["tenant_uid"] != nil || tk.JSON["revoked_at"] != nil || tk.JSON["last_used_at"] != nil || tk.JSON["rotated_from_uid"] != nil {
		t.Errorf("新平台令牌的元数据: %v", tk.JSON)
	}
	if got := fmt.Sprint(tk.JSON["allowed_cidrs"]); got != "[198.51.100.0/24 2001:db8::/32 203.0.113.7/32]" {
		t.Errorf("allowed_cidrs 应规范化（单个 IP → /32）并排序: %s", got)
	}

	// 库内：SHA-256(完整令牌) 与可检索前缀；平台令牌 tenant_id 为 NULL；默认有效期 90 天（UD-27 同口径的工程假设）。
	if h := s.tokenRow(tk.UID, "token_hash"); h != sha256hex(tk.Token) {
		t.Errorf("库内应存 SHA-256(完整令牌): %s", h)
	}
	if p := s.tokenRow(tk.UID, "token_prefix"); p != tk.Prefix {
		t.Errorf("库内前缀: %s", p)
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.api_tokens WHERE uid = ? AND tenant_id IS NULL AND user_id = ?", tk.UID, adm.u.ID); n != 1 {
		t.Errorf("平台令牌应 tenant_id=NULL 且属于创建者")
	}
	if d := s.e.rootInt(t, "SELECT TIMESTAMPDIFF(SECOND, created_at, expires_at) FROM §.api_tokens WHERE uid = ?", tk.UID); d != 90*24*3600 {
		t.Errorf("默认有效期应为 90 天: %d 秒", d)
	}

	// 明文仅创建响应返回一次：get/list 不含 token 字段。
	g := s.as(adm, "GET", "/auth/api-tokens/"+tk.UID, nil)
	s.contract("getApiToken", "GET", "/auth/api-tokens/"+tk.UID, g)
	if _, has := g.JSON(t)["token"]; has || g.Status != 200 {
		t.Errorf("GET 不得再返回明文: %d %s", g.Status, g.Body)
	}
	l := s.as(adm, "GET", "/auth/api-tokens", nil)
	s.contract("listApiTokens", "GET", "/auth/api-tokens", l)
	if strings.Contains(string(l.Body), tk.Secret) || strings.Contains(string(l.Body), `"token":`) {
		t.Errorf("列表不得含明文: %s", l.Body)
	}

	// 幂等：同键同体重放 → 201 + Idempotent-Replayed，不再产生第二个令牌，也不再回显明文（幂等记录里存的是去掉明文的响应体）。
	r2 := s.as(adm, "POST", "/auth/api-tokens", body, withHeader("Idempotency-Key", key))
	if r2.Status != 201 || r2.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("重放: %d %v %s", r2.Status, r2.Header, r2.Body)
	}
	if strings.Contains(string(r2.Body), tk.Secret) || r2.JSON(t)["uid"] != tk.UID {
		t.Errorf("重放不得回显明文，且应是同一个令牌: %s", r2.Body)
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.api_tokens WHERE user_id = ?", adm.u.ID); n != 1 {
		t.Errorf("重放不得创建第二个令牌: %d", n)
	}
	if r3 := s.as(adm, "POST", "/auth/api-tokens", tokBody("别的", "user:read"), withHeader("Idempotency-Key", key)); r3.Status != 422 || r3.errCode(t) != "idempotency.key_reuse" {
		t.Errorf("同键不同体应 422: %d %s", r3.Status, r3.Body)
	}

	// 令牌真的可用，用过之后 Redis 里有「哈希 → id」缓存；全库（含 idempotency_keys、login_logs 等全部文本/JSON 列）、Redis 键值、日志里都没有明文。
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "新令牌可用", withIP("198.51.100.20"))
	if !s.cached(s.tokenKey(tk.Token)) {
		t.Errorf("使用后应缓存「哈希 → 令牌 id」")
	}
	needles := map[string]string{"令牌明文": tk.Token, "令牌随机部分": tk.Secret}
	if f := s.scanDB(needles); len(f) > 0 {
		t.Errorf("库内出现令牌明文:\n%s", strings.Join(f, "\n"))
	}
	for k, v := range s.redisDump() {
		if n := containsAny(k+"="+v, tk.Token, tk.Secret); n != "" {
			t.Errorf("Redis 键 %s 泄露了令牌", k)
		}
		if strings.Contains(k, ":apitoken:") && v != fmt.Sprint(s.e.rootInt(t, "SELECT id FROM §.api_tokens WHERE uid = ?", tk.UID)) {
			t.Errorf("令牌缓存值应只是令牌 id: %s=%s", k, v)
		}
	}
	logs := s.logs.String()
	if n := containsAny(logs, tk.Token, tk.Secret); n != "" {
		t.Errorf("日志泄露了令牌明文")
	}
	if !strings.Contains(logs, "API Token 已创建") || !strings.Contains(logs, `"prefix":"`+tk.Prefix+`"`) {
		t.Errorf("应有只含前缀的结构化日志")
	}
	// 审计（Story 1.21）：创建令牌与令牌写入同一事务，变更摘要只含前缀，绝不含令牌明文/密钥。
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs WHERE action = 'api_token.create' AND resource_uid = ? AND JSON_UNQUOTE(JSON_EXTRACT(changes_json, '$.prefix')) = ?", tk.UID, tk.Prefix); n != 1 {
		t.Errorf("创建令牌应恰好写一条含前缀的审计记录: %d", n)
	}
	for _, secret := range []string{tk.Token, tk.Secret} {
		if secret == "" {
			continue
		}
		if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs WHERE CONVERT(CONCAT_WS(' ', actor_label, resource_uid, CAST(changes_json AS CHAR), CAST(meta_json AS CHAR)) USING utf8mb4) LIKE ?", "%"+secret+"%"); n != 0 {
			t.Errorf("审计记录泄露了令牌明文")
		}
	}
}

// ---- AC2：scopes 必须是创建者当前权限的子集 + 输入校验 ----

func TestAPITokenScopesMustBeSubsetOfCreator(t *testing.T) {
	s := newTokStack(t)
	op, _ := s.opWithTokenManage("user:read", "site:read")
	count := func() int64 { return s.e.rootInt(t, "SELECT COUNT(*) FROM §.api_tokens WHERE user_id = ?", op.u.ID) }

	// 子集 OK（含「双范围」权限 site:read）。
	s.issue(op, tokBody("读用户", "user:read"))
	s.issue(op, tokBody("读站点", "user:read", "site:read", "user:read")) // 重复项被去重
	before := count()

	deny := func(what string, body map[string]any, status int, code string) {
		t.Helper()
		r := s.as(op, "POST", "/auth/api-tokens", body)
		if r.Status != status || r.errCode(t) != code {
			t.Errorf("%s: 期望 %d %s，得到 %d %s", what, status, code, r.Status, r.Body)
		}
		s.contract("createApiToken", "POST", "/auth/api-tokens", r)
	}
	deny("超出创建者权限（user:write）", tokBody("x", "user:read", "user:write"), 403, "auth.forbidden")
	deny("超出创建者权限（平台权限 node:read）", tokBody("x", "node:read"), 403, "auth.forbidden")
	deny("超出创建者权限（tenant:impersonate）", tokBody("x", "tenant:impersonate"), 403, "auth.forbidden")
	deny("字典外的权限串", tokBody("x", "nope:read"), 400, "validation.failed")
	deny("token:manage 不能授予令牌", tokBody("x", "token:manage"), 400, "validation.failed")
	deny("空 scopes", map[string]any{"name": "x", "scopes": []string{}}, 400, "validation.failed")
	deny("缺 scopes", map[string]any{"name": "x"}, 400, "validation.failed")
	deny("空名称", tokBody("  ", "user:read"), 400, "validation.failed")
	deny("名称过长", tokBody(strings.Repeat("长", 129), "user:read"), 400, "validation.failed")
	deny("控制字符", tokBody("a\nb", "user:read"), 400, "validation.failed")
	deny("未知字段", map[string]any{"name": "x", "scopes": []string{"user:read"}, "token": "y"}, 400, "validation.failed")
	many := make([]string, 65)
	for i := range many {
		many[i] = "user:read"
	}
	deny("scopes 过多", map[string]any{"name": "x", "scopes": many}, 400, "validation.failed")
	for _, c := range []string{"nope", "10.0.0.0/33", "0.0.0.0/0", "::/0", "fe80::1%eth0", "300.1.1.1"} {
		deny("非法来源 "+c, map[string]any{"name": "x", "scopes": []string{"user:read"}, "allowed_cidrs": []string{c}}, 400, "validation.failed")
	}
	if got := count(); got != before {
		t.Errorf("被拒绝的创建不得写库: %d → %d", before, got)
	}

	// 没有 token:manage 的主体不能创建（先权限）；未认证 401。
	noTM := s.operator("user:read")
	if r := s.api(noTM, "POST", "/auth/api-tokens", tokBody("x", "user:read")); r.Status != 403 || r.errCode(t) != "auth.forbidden" {
		t.Errorf("无 token:manage 应 403: %d %s", r.Status, r.Body)
	}
	if r := s.do("POST", "/auth/api-tokens", tokBody("x", "user:read")); r.Status != 401 {
		t.Errorf("未认证应 401: %d", r.Status)
	}
	// CSRF 仍适用于 Cookie 会话的令牌管理写操作。
	if r := s.authed("admin", op.token, "", "POST", "/auth/api-tokens", tokBody("x", "user:read")); r.Status != 403 || r.errCode(t) != "auth.csrf_invalid" {
		t.Errorf("缺 CSRF 应 403: %d %s", r.Status, r.Body)
	}

	// 租户主体：scopes 只能是其租户内权限的子集；平台权限永远越界。
	owner := s.tenantSession("tenant_owner")
	s.issue(owner, tokBody("站点只读", "site:read"))
	for _, sc := range []string{"node:read", "user:read", "tenant:impersonate"} {
		if r := s.as(owner, "POST", "/auth/api-tokens", tokBody("x", sc)); r.Status != 403 {
			t.Errorf("租户创建者不得授予平台权限 %s: %d %s", sc, r.Status, r.Body)
		}
	}
	viewer := s.tenantSession("tenant_viewer")
	if r := s.as(viewer, "POST", "/auth/api-tokens", tokBody("x", "site:read")); r.Status != 403 || r.errCode(t) != "auth.forbidden" {
		t.Errorf("没有 token:manage 的租户成员不能创建: %d %s", r.Status, r.Body)
	}
}

// ---- AC2：创建需近期 MFA；Bearer 自身不能创建/轮换/管理令牌 ----

func TestAPITokenCreateRequiresRecentMFAAndCookieSession(t *testing.T) {
	s := newTokStack(t)
	a, _, _ := s.enroll(s.seedUser("platform"))
	s.grantPlatformRole(a.user, "platform_admin")
	tk, cs := s.loginTOTP(a, s.fresh(a))
	ss := session{u: a.user, token: tk, csrf: cs}
	first := s.issue(ss, tokBody("刚验证过", "user:read")) // 刚通过 TOTP 登录：近期 MFA 满足

	s.clk.Advance(16 * time.Minute) // 超过默认 15 分钟窗口
	for _, c := range []struct{ id, method, path string }{
		{"createApiToken", "POST", "/auth/api-tokens"},
		{"rotateApiToken", "POST", "/auth/api-tokens/" + first.UID + "/rotate"},
	} {
		var body any
		if c.id == "createApiToken" {
			body = tokBody("过期", "user:read")
		}
		r := s.as(ss, c.method, c.path, body)
		if r.Status != 403 || r.errCode(t) != "auth.reauth_required" || !strings.Contains(string(r.Body), "mfa_verified_at") {
			t.Errorf("%s: MFA 过期应 403 auth.reauth_required: %d %s", c.id, r.Status, r.Body)
		}
		s.contract(c.id, c.method, c.path, r)
	}
	// 只读与吊销不要求近期 MFA（吊销是收紧权限的操作）。
	if r := s.as(ss, "GET", "/auth/api-tokens", nil); r.Status != 200 {
		t.Errorf("列表不要求近期 MFA: %d", r.Status)
	}
	if r := s.as(ss, "DELETE", "/auth/api-tokens/"+first.UID, nil); r.Status != 204 {
		t.Errorf("吊销不要求近期 MFA: %d %s", r.Status, r.Body)
	}
	if r := s.reauth("admin", tk, cs, map[string]string{"password": a.user.Password, "totp_code": s.fresh(a)}); r.Status != 200 {
		t.Fatalf("reauth: %d %s", r.Status, r.Body)
	}
	s.issue(ss, tokBody("再次验证后", "user:read"))

	// Bearer 自身不能创建/轮换/吊销/列出令牌（令牌不能繁殖令牌）：即使令牌的创建者有 token:manage、且请求体合法。
	live := s.issue(ss, tokBody("机器", "user:read", "role:read"))
	before := s.e.rootInt(t, "SELECT COUNT(*) FROM §.api_tokens")
	r := s.bearer(live.Token, "POST", "/auth/api-tokens", tokBody("繁殖", "user:read"))
	if r.Status != 403 || r.errCode(t) != "auth.reauth_required" {
		t.Errorf("Bearer 创建令牌应 403（近期 MFA 无法由令牌满足）: %d %s", r.Status, r.Body)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/auth/api-tokens/" + live.UID + "/rotate"}, {"DELETE", "/auth/api-tokens/" + live.UID},
		{"GET", "/auth/api-tokens"}, {"GET", "/auth/api-tokens/" + live.UID},
	} {
		if r := s.bearer(live.Token, c.method, c.path, nil); r.Status != 403 {
			t.Errorf("Bearer 不得 %s %s: %d %s", c.method, c.path, r.Status, r.Body)
		}
	}
	if after := s.e.rootInt(t, "SELECT COUNT(*) FROM §.api_tokens"); after != before {
		t.Errorf("Bearer 请求不得改变令牌表: %d → %d", before, after)
	}
	s.requireBearer(live.Token, "GET", "/admin/users", 200, "令牌本身仍可用（上面的拒绝没有吊销它）")
}

// ---- AC4：Bearer 认证：不使用 Cookie 与 CSRF、作用域越界、自助 operation 拒绝、401 不泄漏原因 ----

func TestBearerScopeEnforcementAndPolicy(t *testing.T) {
	s := newTokStack(t)
	adm := s.adminSession("platform_admin")
	read := s.issue(adm, tokBody("只读用户", "user:read"))
	nodes := s.issue(adm, tokBody("平台节点", "node:read"))

	s.requireBearer(read.Token, "GET", "/admin/users", 200, "scopes 内")
	// 作用域越界：同一创建者（platform_admin 拥有全部权限）但令牌 scopes 之外的 operation 一律 403。
	for _, c := range []struct{ method, path string }{
		{"GET", "/admin/roles"}, {"GET", "/admin/permissions"}, {"GET", "/probe/nodes"}, {"POST", "/admin/users"}, {"POST", "/admin/roles"},
	} {
		r := s.bearer(read.Token, c.method, c.path, map[string]string{})
		if r.Status != 403 || r.errCode(t) != "auth.forbidden" {
			t.Errorf("超出令牌作用域 %s %s 应 403: %d %s", c.method, c.path, r.Status, r.Body)
		}
	}
	s.requireBearer(nodes.Token, "GET", "/probe/nodes", 200, "平台令牌访问平台资源（作用域内）")
	s.requireBearer(nodes.Token, "GET", "/admin/users", 403, "node:read 令牌不能读用户")

	// 不使用 Cookie：带着有效会话 Cookie 但无 Bearer 的请求走会话；Bearer 无效时不会回落到 Cookie（401）。
	if r := s.do("GET", "/admin/users", nil, withCookie("le_admin_sid", adm.token), withHeader("Authorization", "Bearer le_aaaaaaaa_"+strings.Repeat("A", 43))); r.Status != 401 {
		t.Errorf("无效 Bearer 不得回落到 Cookie 会话: %d", r.Status)
	}
	// 有效 Bearer + 他人的 Cookie：按令牌主体处理（令牌 scopes），Cookie 被忽略。
	other := s.operator("role:read")
	if r := s.do("GET", "/admin/roles", nil, withCookie("le_admin_sid", other.token), withHeader("Authorization", "Bearer "+read.Token)); r.Status != 403 {
		t.Errorf("Bearer 优先于 Cookie（令牌无 role:read）: %d %s", r.Status, r.Body)
	}

	// 自助 operation 与令牌管理一律要求 Cookie 会话：Bearer 403，且不影响创建者的会话。
	for _, c := range []struct{ method, path string }{
		{"GET", "/auth/me"}, {"POST", "/auth/logout"}, {"GET", "/auth/sessions"}, {"GET", "/auth/csrf"}, {"GET", "/auth/login-logs"},
		{"POST", "/auth/totp/setup"}, {"POST", "/auth/totp/enable"}, {"POST", "/auth/totp/disable"}, {"POST", "/auth/reauth"},
		{"POST", "/auth/password/change"}, {"DELETE", "/auth/sessions/" + ids.MustNew()},
	} {
		r := s.bearer(read.Token, c.method, c.path, map[string]string{})
		if r.Status != 403 {
			t.Errorf("Bearer 不得调用自助 operation %s %s: %d %s", c.method, c.path, r.Status, r.Body)
		}
	}
	if r := s.as(adm, "GET", "/auth/me", nil); r.Status != 200 {
		t.Errorf("Bearer 的拒绝不得影响创建者会话: %d", r.Status)
	}
	if s.e.rootInt(t, "SELECT COUNT(*) FROM §.users WHERE id = ? AND totp_enabled = 1", adm.u.ID) != 0 {
		t.Errorf("令牌不得启用 TOTP")
	}

	// 所有 401 的响应逐字节相同（除 request_id）：不泄漏「不存在 / 已吊销 / 已过期 / 来源不符 / 格式错误」。
	revoked := s.issue(adm, tokBody("将吊销", "user:read"))
	cidr := s.issue(adm, map[string]any{"name": "限来源", "scopes": []string{"user:read"}, "allowed_cidrs": []string{"198.51.100.0/24"}})
	expiring := s.issue(adm, map[string]any{"name": "将过期", "scopes": []string{"user:read"}, "expires_at": s.clk.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if r := s.as(adm, "DELETE", "/auth/api-tokens/"+revoked.UID, nil); r.Status != 204 {
		t.Fatalf("吊销: %d", r.Status)
	}
	s.clk.Advance(61 * time.Minute) // 令牌到期（1 小时）；仍在会话滑动有效期（2 小时）内
	// 令牌里 base64url 的最后一位被改动（仍是合法形态但哈希不符）。
	tamper := read.Token[:len(read.Token)-1] + map[bool]string{true: "B", false: "A"}[!strings.HasSuffix(read.Token, "B")]
	bad := map[string]resp{
		"不存在":   s.bearer("le_aaaaaaaa_"+strings.Repeat("Z", 43), "GET", "/admin/users", nil),
		"篡改":    s.bearer(tamper, "GET", "/admin/users", nil),
		"已吊销":   s.bearer(revoked.Token, "GET", "/admin/users", nil, withIP("198.51.100.9")),
		"已过期":   s.bearer(expiring.Token, "GET", "/admin/users", nil),
		"来源不符":  s.bearer(cidr.Token, "GET", "/admin/users", nil, withIP("203.0.113.1")),
		"格式错误":  s.bearer("le_short", "GET", "/admin/users", nil),
		"空凭据":   s.bearer("", "GET", "/admin/users", nil),
		"非令牌文本": s.bearer("hunter2 hunter2", "GET", "/admin/users", nil),
	}
	var ref string
	for name, r := range bad {
		if r.Status != 401 || r.errCode(t) != "auth.unauthenticated" {
			t.Errorf("%s 应 401 auth.unauthenticated: %d %s", name, r.Status, r.Body)
			continue
		}
		s.contract("listUsers", "GET", "/admin/users", r)
		if lower := strings.ToLower(string(r.Body)); strings.Contains(lower, "revok") || strings.Contains(lower, "expire") || strings.Contains(lower, "cidr") || strings.Contains(lower, "origin") {
			t.Errorf("%s 的 401 不得泄漏原因: %s", name, r.Body)
		}
		if k := errorBodyKey(t, r); ref == "" {
			ref = k
		} else if k != ref {
			t.Errorf("%s 的 401 与其他原因不同:\n%s\nvs\n%s", name, k, ref)
		}
	}
	// 原因只在服务端日志里（只含前缀），且 login_logs 记 token_auth_failed（同一令牌至多每分钟一条）。
	logs := s.logs.String()
	for _, want := range []string{"API Token 认证被拒绝", `"reason":"revoked"`, `"reason":"expired"`, `"reason":"ip_not_allowed"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("服务端日志应记录拒绝原因 %s", want)
		}
	}
	if n := containsAny(logs, revoked.Token, revoked.Secret, cidr.Token, expiring.Token, read.Token, tamper); n != "" {
		t.Errorf("日志泄露了令牌明文")
	}
	s.mod.Flush()
	for _, tk := range []issued{revoked, cidr, expiring} {
		if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.login_logs WHERE event = 'token_auth_failed' AND identifier = ?", "le_"+tk.Prefix); n != 1 {
			t.Errorf("令牌 %s 应恰有 1 条 token_auth_failed: %d", tk.Prefix, n)
		}
	}
	// 失败的令牌认证也不触发幂等/会话副作用；Cookie 路径不受影响。
	if r := s.as(adm, "GET", "/admin/users", nil); r.Status != 200 {
		t.Errorf("创建者会话不受影响: %d", r.Status)
	}
}

// ---- AC4：租户令牌：不能访问平台资源（NFR-14）；超出作用域 403、他租户资源 404（FR-25）；本装配未接租户目录时 acting-as 保持 fail-closed（完整 acting-as 整链见 tests/security/tenant_isolation） ----

func TestTenantTokenIsolation(t *testing.T) {
	s := newTokStack(t)
	ownerA := s.tenantSession("tenant_owner")
	ownerB := s.tenantSession("tenant_owner")
	tenantUID := func(o session) string { return s.e.rootStr(t, "SELECT uid FROM §.tenants WHERE id = ?", o.u.TenantID) }
	tokA := s.issue(ownerA, tokBody("A 站点读写", "site:read", "site:write"))
	if tokA.JSON["tenant_uid"] != tenantUID(ownerA) {
		t.Errorf("租户令牌应记录所属租户: %v", tokA.JSON["tenant_uid"])
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.api_tokens WHERE uid = ? AND tenant_id = ?", tokA.UID, ownerA.u.TenantID); n != 1 {
		t.Errorf("tenant_id 应为创建者的租户")
	}

	// 作用域内：GET/POST 租户资源；Bearer 不需要 CSRF 令牌（POST 没有 X-CSRF-Token 与 Cookie）。
	s.requireBearer(tokA.Token, "GET", "/probe/sites", 200, "租户令牌读自己租户")
	if r := s.bearer(tokA.Token, "POST", "/probe/sites", map[string]string{}); r.Status != 200 {
		t.Errorf("Bearer 写请求不需要 CSRF: %d %s", r.Status, r.Body)
	}
	// 显式声明自己的租户 OK；他人的租户 → 404（不泄漏存在性，FR-25）。
	s.requireBearer(tokA.Token, "GET", "/probe/sites", 200, "自己的 X-Tenant-ID", withHeader("X-Tenant-ID", tenantUID(ownerA)))
	if r := s.bearer(tokA.Token, "GET", "/probe/sites", nil, withHeader("X-Tenant-ID", tenantUID(ownerB))); r.Status != 404 || r.errCode(t) != "resource.not_found" {
		t.Errorf("他租户资源应 404: %d %s", r.Status, r.Body)
	}
	if r := s.bearer(tokA.Token, "GET", "/probe/sites", nil, withHeader("X-Tenant-ID", ids.MustNew())); r.Status != 404 {
		t.Errorf("不存在的租户与他租户一样 404: %d", r.Status)
	}
	// NFR-14：租户令牌不能访问平台资源（/nodes 类 platform 路由、/admin/*），即使令牌 scopes 里「看起来」有同名权限也不行——scopes 根本不能含平台权限。
	for _, p := range []string{"/probe/nodes", "/admin/users", "/admin/roles", "/admin/permissions"} {
		if r := s.bearer(tokA.Token, "GET", p, nil); r.Status != 403 || r.errCode(t) != "auth.forbidden" {
			t.Errorf("租户令牌访问平台资源 %s 应 403: %d %s", p, r.Status, r.Body)
		}
	}
	// 超出作用域：令牌只有 site:read 时写请求 403。
	readOnly := s.issue(ownerA, tokBody("A 站点只读", "site:read"))
	s.requireBearer(readOnly.Token, "GET", "/probe/sites", 200, "只读令牌读")
	if r := s.bearer(readOnly.Token, "POST", "/probe/sites", map[string]string{}); r.Status != 403 {
		t.Errorf("超出作用域应 403: %d %s", r.Status, r.Body)
	}

	// 令牌管理的租户隔离：B 看不到/吊销不了/轮换不了 A 的令牌（404）；平台管理员同样看不到；各自列表只含自己的。
	adm := s.adminSession("platform_admin")
	s.freshReauth(adm)
	for _, who := range []struct {
		name string
		ss   session
	}{{"他租户", ownerB}, {"平台管理员", adm}} {
		for _, c := range []struct{ method, path string }{
			{"GET", "/auth/api-tokens/" + tokA.UID}, {"DELETE", "/auth/api-tokens/" + tokA.UID}, {"POST", "/auth/api-tokens/" + tokA.UID + "/rotate"},
		} {
			if r := s.as(who.ss, c.method, c.path, nil); r.Status != 404 {
				t.Errorf("%s %s %s 应 404: %d %s", who.name, c.method, c.path, r.Status, r.Body)
			}
		}
		if r := s.as(who.ss, "GET", "/auth/api-tokens", nil); r.Status != 200 || strings.Contains(string(r.Body), tokA.UID) {
			t.Errorf("%s 的列表不得含 A 的令牌: %d %s", who.name, r.Status, r.Body)
		}
	}
	s.requireBearer(tokA.Token, "GET", "/probe/sites", 200, "他人的吊销/轮换尝试没有影响 A 的令牌")
	// 令牌管理不接受代租户：平台主体带 X-Tenant-ID → 400；租户主体带他人租户 → 404。
	if r := s.as(adm, "GET", "/auth/api-tokens", nil, withHeader("X-Tenant-ID", tenantUID(ownerA))); r.Status != 400 {
		t.Errorf("平台主体管理令牌不接受 X-Tenant-ID: %d %s", r.Status, r.Body)
	}
	if r := s.as(ownerA, "GET", "/auth/api-tokens", nil, withHeader("X-Tenant-ID", tenantUID(ownerB))); r.Status != 404 {
		t.Errorf("租户主体带他人租户应 404: %d %s", r.Status, r.Body)
	}
	if r := s.as(ownerA, "GET", "/auth/api-tokens", nil, withHeader("X-Tenant-ID", tenantUID(ownerA))); r.Status != 200 {
		t.Errorf("租户主体带自己的租户 OK: %d %s", r.Status, r.Body)
	}

	// 平台令牌 + acting-as：授权矩阵可通过（持有 tenant:impersonate 与路由权限），但本装配（auth.HooksOptions{}）没有租户目录：Scope 钩子无法验证目标租户，保持 fail-closed（403，handler 不执行）。有租户目录的完整整链（目标租户存在/可用才放行，Story 1.19 遗留）见 tests/security/tenant_isolation。
	imp := s.issue(adm, tokBody("代租户", "site:read", "tenant:impersonate"))
	r := s.bearer(imp.Token, "GET", "/probe/sites", nil, withHeader("X-Tenant-ID", tenantUID(ownerA)))
	if r.Status != 403 || strings.Contains(string(r.Body), "reached") {
		t.Errorf("未装配租户目录时 acting-as 必须安全拒绝: %d %s", r.Status, r.Body)
	}
	if r := s.bearer(imp.Token, "GET", "/probe/sites", nil); r.Status != 400 {
		t.Errorf("平台令牌访问 tenant 路由须指定租户（400）: %d %s", r.Status, r.Body)
	}

	// 创建者不再是租户成员 / 租户被停用 → 令牌 401；恢复后可用。
	s.e.root(t, "UPDATE §.tenant_members SET status = 'removed' WHERE user_id = ?", ownerA.u.ID)
	s.requireBearer(tokA.Token, "GET", "/probe/sites", 401, "成员被移除")
	s.e.root(t, "UPDATE §.tenant_members SET status = 'active' WHERE user_id = ?", ownerA.u.ID)
	s.requireBearer(tokA.Token, "GET", "/probe/sites", 200, "成员恢复")
	s.e.root(t, "UPDATE §.tenants SET status = 'suspended' WHERE id = ?", ownerA.u.TenantID)
	s.requireBearer(tokA.Token, "GET", "/probe/sites", 401, "租户被停用")
	s.e.root(t, "UPDATE §.tenants SET status = 'active' WHERE id = ?", ownerA.u.TenantID)
	s.requireBearer(tokA.Token, "GET", "/probe/sites", 200, "租户恢复")
}

// ---- AC3：吊销立即失效（含 Redis 缓存清除）；Redis 缓存命中/残留/被杀/从未可用下都正确 ----

func TestAPITokenRevocationIsImmediate(t *testing.T) {
	t.Run("缓存命中状态下吊销", func(t *testing.T) {
		s := newTokStack(t)
		adm := s.adminSession("platform_admin")
		tk := s.issue(adm, tokBody("t", "user:read"))
		for range 3 {
			s.requireBearer(tk.Token, "GET", "/admin/users", 200, "吊销前")
		}
		key := s.tokenKey(tk.Token)
		if !s.cached(key) {
			t.Fatalf("吊销前应已缓存（命中状态）")
		}
		r := s.as(adm, "DELETE", "/auth/api-tokens/"+tk.UID, nil)
		if r.Status != 204 {
			t.Fatalf("吊销: %d %s", r.Status, r.Body)
		}
		s.contract("revokeApiToken", "DELETE", "/auth/api-tokens/"+tk.UID, r)
		s.requireBearer(tk.Token, "GET", "/admin/users", 401, "吊销后下一个请求立即 401")
		if s.cached(key) {
			t.Errorf("吊销应清除 Redis 缓存键")
		}
		if s.tokenRow(tk.UID, "revoked_at") == "" {
			t.Errorf("吊销应写 revoked_at（行保留，90 天后物理删除）")
		}
		// 幂等：再次吊销 204；不存在的 404。
		if r := s.as(adm, "DELETE", "/auth/api-tokens/"+tk.UID, nil); r.Status != 204 {
			t.Errorf("重复吊销应幂等 204: %d", r.Status)
		}
		if r := s.as(adm, "DELETE", "/auth/api-tokens/"+ids.MustNew(), nil); r.Status != 404 {
			t.Errorf("不存在的令牌应 404: %d", r.Status)
		}
		g := s.as(adm, "GET", "/auth/api-tokens/"+tk.UID, nil)
		s.contract("getApiToken", "GET", "/auth/api-tokens/"+tk.UID, g)
		if g.JSON(t)["revoked_at"] == nil {
			t.Errorf("元数据应显示 revoked_at: %s", g.Body)
		}
	})

	t.Run("残留或被回填的旧缓存键不能让已吊销令牌复活", func(t *testing.T) {
		// 模拟「吊销时 Redis 删除失败而随后恢复」或并发回填：授权相关状态每个请求都从 MySQL 现读，缓存只是不可变的「哈希 → id」映射。
		s := newTokStack(t)
		adm := s.adminSession("platform_admin")
		tk := s.issue(adm, tokBody("t", "user:read"))
		s.requireBearer(tk.Token, "GET", "/admin/users", 200, "吊销前")
		s.as(adm, "DELETE", "/auth/api-tokens/"+tk.UID, nil)
		id := fmt.Sprint(s.e.rootInt(t, "SELECT id FROM §.api_tokens WHERE uid = ?", tk.UID))
		if err := s.rc.Set(context.Background(), s.tokenKey(tk.Token), id, time.Minute); err != nil {
			t.Fatal(err)
		}
		if !s.cached(s.tokenKey(tk.Token)) {
			t.Fatal("测试前提：残留缓存键存在")
		}
		for range 3 {
			s.requireBearer(tk.Token, "GET", "/admin/users", 401, "残留缓存键下仍 401")
		}
	})

	t.Run("Redis 运行中被杀", func(t *testing.T) {
		s := newTokStack(t)
		adm := s.adminSession("platform_admin")
		keep := s.issue(adm, tokBody("keep", "user:read"))
		gone := s.issue(adm, tokBody("gone", "user:read"))
		s.requireBearer(keep.Token, "GET", "/admin/users", 200, "预热")
		s.requireBearer(gone.Token, "GET", "/admin/users", 200, "预热")
		s.redis.Kill() // 真实故障注入：SIGKILL，缓存一并丢失
		s.requireBearer(keep.Token, "GET", "/admin/users", 200, "Redis 被杀后直读 MySQL 仍可认证")
		if r := s.as(adm, "DELETE", "/auth/api-tokens/"+gone.UID, nil); r.Status != 204 {
			t.Fatalf("Redis 被杀后吊销仍可用: %d %s", r.Status, r.Body)
		}
		s.requireBearer(gone.Token, "GET", "/admin/users", 401, "Redis 被杀下吊销立即生效")
		s.requireBearer(keep.Token, "GET", "/admin/users", 200, "其他令牌不受影响")
		if r := s.as(adm, "POST", "/auth/api-tokens/"+keep.UID+"/rotate", nil); r.Status != 200 {
			t.Fatalf("Redis 被杀后轮换仍可用: %d %s", r.Status, r.Body)
		}
		s.requireBearer(keep.Token, "GET", "/admin/users", 401, "轮换后旧令牌立即失效")
		s.redis.Restart(t)
		s.requireBearer(gone.Token, "GET", "/admin/users", 401, "Redis 恢复（空）后仍 401")
		fresh := s.issue(adm, tokBody("fresh", "user:read"))
		eventually(t, "Redis 恢复后缓存自动回填", 10*time.Second, func() bool {
			s.requireBearer(fresh.Token, "GET", "/admin/users", 200, "恢复后可用")
			return s.cached(s.tokenKey(fresh.Token))
		})
	})

	t.Run("Redis 从未可用", func(t *testing.T) {
		s := newTokStack(t, stackOpts{noRedis: true})
		adm := s.adminSession("platform_admin")
		tk := s.issue(adm, tokBody("t", "user:read"))
		s.requireBearer(tk.Token, "GET", "/admin/users", 200, "无 Redis 可认证")
		if r := s.as(adm, "DELETE", "/auth/api-tokens/"+tk.UID, nil); r.Status != 204 {
			t.Fatalf("无 Redis 吊销: %d %s", r.Status, r.Body)
		}
		s.requireBearer(tk.Token, "GET", "/admin/users", 401, "无 Redis 吊销立即生效")
	})
}

// ---- AC3：轮换：新令牌 + rotated_from_id，旧令牌立即吊销 ----

func TestAPITokenRotationChain(t *testing.T) {
	s := newTokStack(t)
	adm := s.adminSession("platform_admin")
	exp := s.clk.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	v1 := s.issue(adm, map[string]any{"name": "轮换链", "scopes": []string{"user:read", "role:read"}, "allowed_cidrs": []string{"198.51.100.0/24"}, "expires_at": exp.Format(time.RFC3339)})
	ip := withIP("198.51.100.4")
	s.requireBearer(v1.Token, "GET", "/admin/users", 200, "v1 可用", ip)
	s.clk.Advance(time.Hour) // 小于会话滑动有效期（2 小时）

	chain := []issued{v1}
	for i := 2; i <= 3; i++ {
		prev := chain[len(chain)-1]
		r := s.as(adm, "POST", "/auth/api-tokens/"+prev.UID+"/rotate", nil)
		if r.Status != 200 {
			t.Fatalf("轮换 v%d: %d %s", i, r.Status, r.Body)
		}
		s.contract("rotateApiToken", "POST", "/auth/api-tokens/"+prev.UID+"/rotate", r)
		if r.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("轮换响应应 no-store")
		}
		next := s.parseIssued(r)
		if next.Token == prev.Token || next.Prefix == prev.Prefix || next.UID == prev.UID {
			t.Fatalf("轮换应产生全新的令牌")
		}
		if next.JSON["rotated_from_uid"] != prev.UID || next.JSON["name"] != "轮换链" || fmt.Sprint(next.JSON["allowed_cidrs"]) != "[198.51.100.0/24]" || fmt.Sprint(next.JSON["scopes"]) != "[role:read user:read]" {
			t.Errorf("新令牌应继承名称/来源限制/scopes 并记来源: %v", next.JSON)
		}
		// 库内链：rotated_from_id → 旧行 id；旧行 revoked_at 已写。
		if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.api_tokens n JOIN §.api_tokens o ON o.id = n.rotated_from_id WHERE n.uid = ? AND o.uid = ? AND o.revoked_at IS NOT NULL AND n.revoked_at IS NULL", next.UID, prev.UID); n != 1 {
			t.Errorf("rotated_from_id 链与旧令牌吊销不符")
		}
		// 有效期长度被继承（原 10 天）：新令牌过期 = 轮换时刻 + 10 天。
		if d := s.e.rootInt(t, "SELECT TIMESTAMPDIFF(SECOND, created_at, expires_at) FROM §.api_tokens WHERE uid = ?", next.UID); d < 10*24*3600-1 || d > 10*24*3600 { // 原 expires_at 取整到秒，允许 1 秒误差
			t.Errorf("应继承有效期时长 10 天: %d 秒", d)
		}
		// 旧令牌立即失效（无宽限期），新令牌立即可用。
		s.requireBearer(prev.Token, "GET", "/admin/users", 401, "旧令牌轮换后立即失效", ip)
		s.requireBearer(next.Token, "GET", "/admin/users", 200, "新令牌可用", ip)
		chain = append(chain, next)
		// 已吊销的令牌不能再轮换/吊销以外的变更：轮换 → 404。
		if r := s.as(adm, "POST", "/auth/api-tokens/"+prev.UID+"/rotate", nil); r.Status != 404 {
			t.Errorf("已吊销的令牌不能再轮换: %d %s", r.Status, r.Body)
		}
	}
	// 链：v3.rotated_from = v2，v2.rotated_from = v1，v1 无来源。
	if chain[0].JSON["rotated_from_uid"] != nil {
		t.Errorf("v1 不应有来源")
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.api_tokens WHERE user_id = ? AND revoked_at IS NULL", adm.u.ID); n != 1 {
		t.Errorf("链上应只剩 v3 有效: %d", n)
	}
	// 他人不能轮换我的令牌（404），轮换需要 token:manage 与 Cookie 会话。
	other := s.adminSession("platform_admin")
	if r := s.as(other, "POST", "/auth/api-tokens/"+chain[2].UID+"/rotate", nil); r.Status != 404 {
		t.Errorf("他人轮换应 404: %d", r.Status)
	}
	if r := s.as(s.operator("user:read"), "POST", "/auth/api-tokens/"+chain[2].UID+"/rotate", nil); r.Status != 403 {
		t.Errorf("无 token:manage 不能轮换: %d", r.Status)
	}
	// 轮换时 scopes 与创建者当前权限取交集：创建者已不再持有的 scope 不会被「续命」；全部失去则拒绝。
	op, code := s.opWithTokenManage("user:read", "role:read")
	o1 := s.issue(op, tokBody("收缩", "user:read", "role:read"))
	root := s.adminSession("platform_admin")
	s.freshReauth(root)
	rid := s.roleUID(code)
	if r := s.api(root, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{"token:manage", "user:read"}}); r.Status != 200 {
		t.Fatalf("降权: %d %s", r.Status, r.Body)
	}
	r := s.as(op, "POST", "/auth/api-tokens/"+o1.UID+"/rotate", nil)
	if r.Status != 200 || fmt.Sprint(s.parseIssued(r).JSON["scopes"]) != "[user:read]" {
		t.Errorf("轮换应收缩为创建者当前权限的交集: %d %s", r.Status, r.Body)
	}
	o2 := s.parseIssued(r)
	if r := s.api(root, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{"token:manage"}}); r.Status != 200 {
		t.Fatalf("再降权: %d %s", r.Status, r.Body)
	}
	if r := s.as(op, "POST", "/auth/api-tokens/"+o2.UID+"/rotate", nil); r.Status != 403 {
		t.Errorf("创建者已不持有令牌的任何 scope，轮换应 403: %d %s", r.Status, r.Body)
	}
	if s.tokenRow(o2.UID, "revoked_at") != "" {
		t.Errorf("被拒绝的轮换不得吊销旧令牌")
	}
}

// ---- AC4：有效权限 = scopes ∩ 创建者当前权限：创建者被降权/禁用/删除后令牌随之收缩 ----

func TestAPITokenShrinksWithCreator(t *testing.T) {
	s := newTokStack(t)
	op, code := s.opWithTokenManage("user:read", "role:read")
	tk := s.issue(op, tokBody("双权限", "user:read", "role:read"))
	root := s.adminSession("platform_admin")
	s.freshReauth(root)
	rid := s.roleUID(code)
	setPerms := func(perms ...string) {
		t.Helper()
		if r := s.api(root, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": perms}); r.Status != 200 {
			t.Fatalf("设置角色权限: %d %s", r.Status, r.Body)
		}
	}
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "初始 user:read")
	s.requireBearer(tk.Token, "GET", "/admin/roles", 200, "初始 role:read")

	setPerms("token:manage", "user:read") // 降权：失去 role:read —— 下一个请求立即收缩（权限缓存键含 permission_version）
	s.requireBearer(tk.Token, "GET", "/admin/roles", 403, "创建者被降权后令牌随之收缩")
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "仍持有的权限不受影响")
	setPerms("token:manage", "user:read", "role:read", "node:read") // 创建者新增权限：令牌 scopes 从不扩大
	s.requireBearer(tk.Token, "GET", "/probe/nodes", 403, "创建者新增权限不会扩大令牌 scopes")
	s.requireBearer(tk.Token, "GET", "/admin/roles", 200, "创建者重新持有后，令牌的 scopes 仍然有效")
	setPerms("token:manage")
	s.requireBearer(tk.Token, "GET", "/admin/users", 403, "失去全部权限")
	setPerms("token:manage", "user:read", "role:read")

	// 创建者被禁用 → 401；恢复 → 可用；软删除 → 401。
	s.e.root(t, "UPDATE §.users SET status = 'disabled' WHERE id = ?", op.u.ID)
	s.requireBearer(tk.Token, "GET", "/admin/users", 401, "创建者被禁用")
	s.e.root(t, "UPDATE §.users SET status = 'active' WHERE id = ?", op.u.ID)
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "创建者恢复")
	// 经真实管理 API 停用（会话被吊销，令牌同样失效）。
	if r := s.api(root, "PATCH", "/admin/users/"+op.u.UID, map[string]any{"status": "disabled"}); r.Status != 200 {
		t.Fatalf("停用创建者: %d %s", r.Status, r.Body)
	}
	s.requireBearer(tk.Token, "GET", "/admin/users", 401, "创建者被管理员停用")
	if r := s.api(root, "PATCH", "/admin/users/"+op.u.UID, map[string]any{"status": "active"}); r.Status != 200 {
		t.Fatalf("恢复创建者: %d %s", r.Status, r.Body)
	}
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "创建者恢复后令牌恢复")
	s.freshReauth(root)
	if r := s.api(root, "DELETE", "/admin/users/"+op.u.UID, nil); r.Status != 204 {
		t.Fatalf("删除创建者: %d %s", r.Status, r.Body)
	}
	s.requireBearer(tk.Token, "GET", "/admin/users", 401, "创建者被删除")
}

// 强制管理员启用 TOTP 的策略、须先改密的状态同样约束令牌（与会话一致）。
func TestAPITokenFollowsCreatorLoginPolicy(t *testing.T) {
	s := newTokStack(t, stackOpts{settings: map[string]string{"login.totp_required_for_admin": "false"}})
	a := s.adminSession("platform_admin")
	tk := s.issue(a, tokBody("t", "user:read"))
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "策略未开启")
	s.e.root(t, "UPDATE §.system_settings SET value_json = CAST('true' AS JSON) WHERE setting_key = 'login.totp_required_for_admin'")
	s.clk.Advance(time.Minute) // 设置缓存 15 秒
	s.requireBearer(tk.Token, "GET", "/admin/users", 401, "强制 TOTP 且创建者未启用")
	s.e.root(t, "UPDATE §.system_settings SET value_json = CAST('false' AS JSON) WHERE setting_key = 'login.totp_required_for_admin'")
	s.clk.Advance(time.Minute)
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "策略关闭后恢复")
	// 须先改密：must_change_password 的创建者令牌 401。
	s.e.root(t, "UPDATE §.users SET must_change_password = 1 WHERE id = ?", a.u.ID)
	s.requireBearer(tk.Token, "GET", "/admin/users", 401, "创建者须先改密")
}

// Story 1.22b（设计决定，ADR-018 §7④ 的落地语义）：创建者处于「须先改密」或「强制 TOTP 但未启用」时，其令牌一律 401，
// **没有**像会话那样的「只能走启用流程」的受限放行——令牌本来就不能调用任何自助 operation（含改密、TOTP 启用，只能 Cookie 会话），
// 受限放行对令牌没有可放行的内容；且 401 的响应体与「令牌不存在」逐字节一致（去掉 request_id），调用方无法借此枚举账号状态；原因只写服务端日志。
func TestAPITokenRestrictedCreatorIsIndistinguishable401(t *testing.T) {
	s := newTokStack(t, stackOpts{settings: map[string]string{"login.totp_required_for_admin": "false"}})
	a := s.adminSession("platform_admin")
	tk := s.issue(a, tokBody("t", "user:read"))
	ghost := "le_" + strings.Repeat("a", 8) + "_" + strings.Repeat("A", 43) // 形态合法但不存在
	key := func(r resp) string {
		j := r.JSON(t)
		e, _ := j["error"].(map[string]any)
		delete(e, "request_id")
		b, _ := json.Marshal(j)
		return r.Header.Get("Content-Type") + string(b)
	}
	want := s.bearer(ghost, "GET", "/admin/users", nil)
	if want.Status != 401 {
		t.Fatalf("不存在的令牌应 401: %d", want.Status)
	}
	check := func(state string) {
		t.Helper()
		for _, path := range []string{"/admin/users", "/auth/totp/setup", "/auth/password/change"} {
			m := "GET"
			if path != "/admin/users" {
				m = "POST"
			}
			r := s.bearer(tk.Token, m, path, nil)
			if r.Status != 401 || key(r) != key(want) {
				t.Errorf("%s %s %s：应与「令牌不存在」逐字节一致的 401，得到 %d %s", state, m, path, r.Status, r.Body)
			}
		}
	}
	s.e.root(t, "UPDATE §.users SET must_change_password = 1 WHERE id = ?", a.u.ID)
	check("须先改密")
	s.e.root(t, "UPDATE §.users SET must_change_password = 0 WHERE id = ?", a.u.ID)
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "恢复后可用")
	s.e.root(t, "UPDATE §.system_settings SET value_json = CAST('true' AS JSON) WHERE setting_key = 'login.totp_required_for_admin'")
	s.clk.Advance(time.Minute)
	check("强制 TOTP 未启用")
	// 同一账号的 Cookie 会话在该状态下仍可走启用流程（受限放行只属于会话）。
	if r := s.as(a, "POST", "/auth/totp/setup", nil); r.Status == 401 {
		t.Errorf("会话在强制 TOTP 期间应仍可调用 totp/setup: %d %s", r.Status, r.Body)
	}
}

// ---- AC2/AC4：来源 IP 白名单（IPv4/IPv6/IPv4 映射；伪造 X-Forwarded-For 不能绕过） ----

func TestAPITokenAllowedCIDRs(t *testing.T) {
	t.Run("遵守可信代理的 X-Forwarded-For", func(t *testing.T) {
		s := newTokStack(t)
		adm := s.adminSession("platform_admin")
		tk := s.issue(adm, map[string]any{"name": "限来源", "scopes": []string{"user:read"}, "allowed_cidrs": []string{"198.51.100.0/24", "2001:db8::/32", "192.0.2.77"}})
		cases := []struct {
			xff  string
			want int
			why  string
		}{
			{"198.51.100.5", 200, "IPv4 在白名单内"},
			{"198.51.100.255", 200, "CIDR 末尾地址"},
			{"198.51.101.0", 401, "CIDR 之外一个地址"},
			{"203.0.113.9", 401, "不在白名单"},
			{"2001:db8::77", 200, "IPv6 在白名单内"},
			{"2001:db8:ffff::1", 200, "IPv6 /32 范围内"},
			{"2001:db9::1", 401, "IPv6 不在白名单"},
			{"::ffff:198.51.100.5", 200, "IPv4 映射的 IPv6 按 IPv4 比较"},
			{"::ffff:203.0.113.9", 401, "IPv4 映射的 IPv6（不在白名单）"},
			{"192.0.2.77", 200, "单个 IP（/32）"},
			{"192.0.2.78", 401, "单个 IP 的邻居"},
			// 伪造：客户端在 XFF 左侧塞一个白名单内的地址，可信代理在右侧追加真实来源 → 取最右侧的非可信地址 = 真实来源。
			{"198.51.100.5, 203.0.113.9", 401, "XFF 左侧伪造白名单地址不能绕过"},
			{"2001:db8::1, 203.0.113.9", 401, "XFF 左侧伪造 IPv6 白名单地址不能绕过"},
			{"203.0.113.9, 198.51.100.5", 200, "真实来源（最右）在白名单内时放行"},
		}
		for _, c := range cases {
			if r := s.bearer(tk.Token, "GET", "/admin/users", nil, withHeader("X-Forwarded-For", c.xff)); r.Status != c.want {
				t.Errorf("%s（XFF=%q）: 期望 %d，得到 %d %s", c.why, c.xff, c.want, r.Status, r.Body)
			}
		}
		// 多个 X-Forwarded-For 头：gin 按最右一个头的最右项取值，伪造头不能提前放行。
		req := s.bearer(tk.Token, "GET", "/admin/users", nil, withHeader("X-Forwarded-For", "198.51.100.5"), func(r *http.Request) { r.Header.Add("X-Forwarded-For", "203.0.113.9") })
		if req.Status != 401 {
			t.Errorf("多个 XFF 头：真实来源在最后，应 401: %d", req.Status)
		}
		// X-Real-IP 等其他头不被采信。
		if r := s.bearer(tk.Token, "GET", "/admin/users", nil, withHeader("X-Forwarded-For", "203.0.113.9"), withHeader("X-Real-IP", "198.51.100.5"), withHeader("Forwarded", "for=198.51.100.5")); r.Status != 401 {
			t.Errorf("X-Real-IP/Forwarded 不得被采信: %d", r.Status)
		}
		// 无限制的令牌不受来源影响；来源限制只约束持有该限制的令牌。
		free := s.issue(adm, tokBody("不限来源", "user:read"))
		s.requireBearer(free.Token, "GET", "/admin/users", 200, "未设 allowed_cidrs 的令牌不限来源", withIP("203.0.113.9"))
	})

	t.Run("来源不来自可信代理时 X-Forwarded-For 被忽略", func(t *testing.T) {
		// 测试服务器的对端是 127.0.0.1；把可信代理设成别的地址 → 对端不可信 → XFF 完全被忽略，来源 IP = 127.0.0.1。
		s := newTokStack(t, stackOpts{trustedProxies: []string{"10.255.255.1"}})
		adm := s.adminSession("platform_admin")
		external := s.issue(adm, map[string]any{"name": "外网", "scopes": []string{"user:read"}, "allowed_cidrs": []string{"198.51.100.0/24", "2001:db8::/32"}})
		for _, xff := range []string{"198.51.100.5", "2001:db8::1", "198.51.100.5, 198.51.100.6", "::ffff:198.51.100.5"} {
			if r := s.bearer(external.Token, "GET", "/admin/users", nil, withHeader("X-Forwarded-For", xff)); r.Status != 401 {
				t.Errorf("伪造 XFF=%q 不能绕过来源限制: %d %s", xff, r.Status, r.Body)
			}
		}
		if r := s.bearer(external.Token, "GET", "/admin/users", nil); r.Status != 401 {
			t.Errorf("对端 127.0.0.1 不在白名单: %d", r.Status)
		}
		loop := s.issue(adm, map[string]any{"name": "本机", "scopes": []string{"user:read"}, "allowed_cidrs": []string{"127.0.0.0/8"}})
		for _, xff := range []string{"", "203.0.113.9", "2001:db8::1"} {
			var o []reqOpt
			if xff != "" {
				o = append(o, withHeader("X-Forwarded-For", xff))
			}
			if r := s.bearer(loop.Token, "GET", "/admin/users", nil, o...); r.Status != 200 {
				t.Errorf("对端 127.0.0.1 在白名单内，XFF=%q 不应改变结果: %d %s", xff, r.Status, r.Body)
			}
		}
	})
}

// ---- 过期与有效期上限（系统设置） ----

func TestAPITokenExpiry(t *testing.T) {
	s := newTokStack(t)
	adm := s.adminSession("platform_admin")
	now := s.clk.Now()
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }

	// 校验：必须在未来；不得超过系统上限（默认 365 天）；格式必须是 RFC 3339。
	for name, v := range map[string]string{"过去": at(-time.Hour), "现在": at(0), "超过上限": at(366 * 24 * time.Hour), "格式": "明天"} {
		r := s.as(adm, "POST", "/auth/api-tokens", map[string]any{"name": "x", "scopes": []string{"user:read"}, "expires_at": v})
		if r.Status != 400 || r.errCode(t) != "validation.failed" || !strings.Contains(string(r.Body), "expires_at") {
			t.Errorf("expires_at=%s 应 400: %d %s", name, r.Status, r.Body)
		}
	}
	s.issue(adm, map[string]any{"name": "上限内", "scopes": []string{"user:read"}, "expires_at": at(364 * 24 * time.Hour)})

	// 过期：恰好到期时刻即失效（不在到期前）。
	tk := s.issue(adm, map[string]any{"name": "一小时", "scopes": []string{"user:read"}, "expires_at": at(time.Hour)})
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "到期前")
	s.clk.Advance(time.Hour - 2*time.Millisecond)
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "到期前 2 毫秒")
	s.clk.Advance(2 * time.Millisecond)
	s.requireBearer(tk.Token, "GET", "/admin/users", 401, "到期时刻即失效")
	g := s.as(adm, "GET", "/auth/api-tokens/"+tk.UID, nil)
	s.contract("getApiToken", "GET", "/auth/api-tokens/"+tk.UID, g)
	if g.JSON(t)["expires_at"] == nil {
		t.Errorf("元数据应含 expires_at")
	}
	// 过期令牌可被轮换（Cookie 会话 + 所有权），新令牌从现在起重新计时。
	if r := s.as(adm, "POST", "/auth/api-tokens/"+tk.UID+"/rotate", nil); r.Status != 200 {
		t.Errorf("过期令牌应可轮换: %d %s", r.Status, r.Body)
	}
}

// 系统设置：上限与默认有效期可配置（工程假设 UD-27 同口径）。
func TestAPITokenExpirySettings(t *testing.T) {
	s2 := newTokStack(t, stackOpts{settings: map[string]string{"auth.api_token_max_ttl_s": "7200", "auth.api_token_default_ttl_s": "3600"}})
	a2 := s2.adminSession("platform_admin")
	now2 := s2.clk.Now()
	if r := s2.as(a2, "POST", "/auth/api-tokens", map[string]any{"name": "x", "scopes": []string{"user:read"}, "expires_at": now2.Add(3 * time.Hour).UTC().Format(time.RFC3339)}); r.Status != 400 {
		t.Errorf("超过配置的上限（2 小时）应 400: %d %s", r.Status, r.Body)
	}
	d := s2.issue(a2, tokBody("默认", "user:read"))
	if sec := s2.e.rootInt(t, "SELECT TIMESTAMPDIFF(SECOND, created_at, expires_at) FROM §.api_tokens WHERE uid = ?", d.UID); sec != 3600 {
		t.Errorf("默认有效期应取配置值 3600 秒: %d", sec)
	}
}

// ---- last_used_at：节流异步写，避免每请求写库 ----

func TestAPITokenLastUsedThrottled(t *testing.T) {
	s := newTokStack(t)
	adm := s.adminSession("platform_admin")
	tk := s.issue(adm, tokBody("t", "user:read"))
	if s.tokenRow(tk.UID, "last_used_at") != "" {
		t.Fatal("新令牌 last_used_at 应为空")
	}
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "首次使用", withIP("198.51.100.20"))
	s.mod.Flush()
	first := s.tokenRow(tk.UID, "last_used_at")
	if first == "" || s.e.rootStr(t, "SELECT INET6_NTOA(last_used_ip) FROM §.api_tokens WHERE uid = ?", tk.UID) != "198.51.100.20" {
		t.Fatalf("首次使用应写 last_used_at 与来源 IP: %q", first)
	}
	// 一分钟内的 50 次请求不再写库（把列清空来探测：被清空后若仍为空，说明没有再次写入）。
	s.e.root(t, "UPDATE §.api_tokens SET last_used_at = NULL WHERE uid = ?", tk.UID)
	for range 50 {
		s.requireBearer(tk.Token, "GET", "/admin/users", 200, "节流窗口内")
	}
	s.mod.Flush()
	if s.tokenRow(tk.UID, "last_used_at") != "" {
		t.Errorf("节流窗口内不应再写 last_used_at")
	}
	s.clk.Advance(61 * time.Second)
	s.requireBearer(tk.Token, "GET", "/admin/users", 200, "窗口之后", withIP("198.51.100.21"))
	s.mod.Flush()
	if s.tokenRow(tk.UID, "last_used_at") == "" || s.e.rootStr(t, "SELECT INET6_NTOA(last_used_ip) FROM §.api_tokens WHERE uid = ?", tk.UID) != "198.51.100.21" {
		t.Errorf("窗口之后应再次写入（含新的来源 IP）")
	}
	g := s.as(adm, "GET", "/auth/api-tokens/"+tk.UID, nil)
	s.contract("getApiToken", "GET", "/auth/api-tokens/"+tk.UID, g)
	if g.JSON(t)["last_used_at"] == nil || g.JSON(t)["last_used_ip"] != "198.51.100.21" {
		t.Errorf("元数据应显示 last_used_at/last_used_ip: %s", g.Body)
	}
}

// ---- 令牌管理：所有权、列表分页排序、契约 ----

func TestAPITokenManagementOwnershipAndList(t *testing.T) {
	s := newTokStack(t)
	a := s.adminSession("platform_admin")
	b := s.adminSession("platform_admin")
	var mine []issued
	for _, n := range []string{"charlie", "alpha", "bravo"} {
		mine = append(mine, s.issue(a, tokBody(n, "user:read")))
	}
	theirs := s.issue(b, tokBody("bravo-b", "user:read"))

	r := s.as(a, "GET", "/auth/api-tokens?sort=name&page_size=2", nil)
	s.contract("listApiTokens", "GET", "/auth/api-tokens?sort=name&page_size=2", r)
	j := r.JSON(t)
	items, _ := j["items"].([]any)
	if r.Status != 200 || len(items) != 2 || items[0].(map[string]any)["name"] != "alpha" || items[1].(map[string]any)["name"] != "bravo" {
		t.Errorf("按名称分页排序: %d %s", r.Status, r.Body)
	}
	r = s.as(a, "GET", "/auth/api-tokens?sort=-name&page=2&page_size=2", nil)
	items, _ = r.JSON(t)["items"].([]any)
	if r.Status != 200 || len(items) != 1 || items[0].(map[string]any)["name"] != "alpha" {
		t.Errorf("降序第 2 页: %d %s", r.Status, r.Body)
	}
	if r := s.as(a, "GET", "/auth/api-tokens?sort=token_hash", nil); r.Status != 400 {
		t.Errorf("非白名单排序字段应 400: %d", r.Status)
	}
	all := s.as(a, "GET", "/auth/api-tokens", nil)
	if strings.Contains(string(all.Body), theirs.UID) || !strings.Contains(string(all.Body), mine[0].UID) {
		t.Errorf("列表只含自己的令牌: %s", all.Body)
	}
	if total := all.JSON(t)["total"]; total != float64(3) {
		t.Errorf("total 应为 3: %v", total)
	}
	// 他人的令牌：GET/DELETE/rotate 一律 404；对方的令牌不受影响。
	for _, c := range []struct{ method, path string }{
		{"GET", "/auth/api-tokens/" + theirs.UID}, {"DELETE", "/auth/api-tokens/" + theirs.UID}, {"POST", "/auth/api-tokens/" + theirs.UID + "/rotate"},
	} {
		if r := s.as(a, c.method, c.path, nil); r.Status != 404 || r.errCode(t) != "resource.not_found" {
			t.Errorf("%s %s 应 404: %d %s", c.method, c.path, r.Status, r.Body)
		}
	}
	s.requireBearer(theirs.Token, "GET", "/admin/users", 200, "他人的吊销尝试没有生效")
	// 已吊销的令牌仍出现在列表里（带 revoked_at），直到保留期满后物理删除。
	s.as(a, "DELETE", "/auth/api-tokens/"+mine[0].UID, nil)
	g := s.as(a, "GET", "/auth/api-tokens/"+mine[0].UID, nil)
	if g.Status != 200 || g.JSON(t)["revoked_at"] == nil {
		t.Errorf("已吊销的令牌元数据仍可读: %d %s", g.Status, g.Body)
	}
}

// ---- G-5：令牌明文不入日志 / 库 / Redis（全流程之后的综合扫描） ----

func TestAPITokenPlaintextNeverLeaks(t *testing.T) {
	s := newTokStack(t)
	adm := s.adminSession("platform_admin")
	owner := s.tenantSession("tenant_owner")
	var all []issued
	all = append(all, s.issue(adm, map[string]any{"name": "平台", "scopes": []string{"user:read"}, "allowed_cidrs": []string{"198.51.100.0/24"}}, withHeader("Idempotency-Key", ids.MustNew())))
	all = append(all, s.issue(owner, tokBody("租户", "site:read")))
	rot := s.as(adm, "POST", "/auth/api-tokens/"+all[0].UID+"/rotate", nil)
	all = append(all, s.parseIssued(rot))
	// 各种使用：成功、越权、来源不符、吊销后。
	for _, tk := range all {
		s.bearer(tk.Token, "GET", "/admin/users", nil, withIP("198.51.100.9"))
		s.bearer(tk.Token, "GET", "/probe/sites", nil, withIP("203.0.113.9"))
		s.bearer(tk.Token, "POST", "/auth/api-tokens", tokBody("x", "user:read"))
	}
	s.as(adm, "DELETE", "/auth/api-tokens/"+all[2].UID, nil)
	s.bearer(all[2].Token, "GET", "/admin/users", nil)
	s.mod.Flush()

	needles := map[string]string{}
	for i, tk := range all {
		needles[fmt.Sprintf("令牌%d明文", i)] = tk.Token
		needles[fmt.Sprintf("令牌%d随机部分", i)] = tk.Secret
	}
	logs := s.logs.String()
	if len(logs) < 1000 {
		t.Fatalf("日志应非空（否则扫描没有意义）: %d", len(logs))
	}
	for name, n := range needles {
		if strings.Contains(logs, n) {
			t.Errorf("日志泄露了 %s", name)
		}
	}
	// Authorization 头不得出现在日志里（访问日志/错误日志）。
	if strings.Contains(strings.ToLower(logs), "authorization") && strings.Contains(logs, "Bearer le_") {
		t.Errorf("日志不得记录 Bearer 凭据")
	}
	for k, v := range s.redisDump() {
		for name, n := range needles {
			if strings.Contains(k, n) || strings.Contains(v, n) {
				t.Errorf("Redis 键 %s 泄露了 %s", k, name)
			}
		}
	}
	if f := s.scanDB(needles); len(f) > 0 {
		t.Errorf("库内出现令牌明文:\n%s", strings.Join(f, "\n"))
	}
	// 库里只有 SHA-256 与前缀；日志只记前缀（有前缀出现、无明文）。
	for _, tk := range all {
		if s.tokenRow(tk.UID, "token_hash") != sha256hex(tk.Token) {
			t.Errorf("库内应是 SHA-256(完整令牌)")
		}
		if !strings.Contains(logs, tk.Prefix) {
			t.Errorf("日志应记录令牌前缀 %s", tk.Prefix)
		}
	}
}

// 令牌管理路由（5 个 operation）都经 httpx.Router.Register 注册，元数据与 OpenAPI 逐项一致（无 501 占位）。
func TestAPITokenRoutesRegisteredAndMatchOpenAPI(t *testing.T) {
	s := newTokStack(t)
	reg := map[string]bool{}
	for _, m := range s.router.Routes() {
		reg[m.OperationID] = true
	}
	var want []string
	for _, m := range s.spec.Expected() {
		if strings.HasPrefix(m.Path, "/auth/api-tokens") {
			want = append(want, m.OperationID)
		}
	}
	if len(want) != 5 {
		t.Fatalf("OpenAPI 中 /auth/api-tokens* 的 operation 应为 5 个: %v", want)
	}
	for _, id := range want {
		if !reg[id] {
			t.Errorf("%s 未注册", id)
		}
	}
	for _, p := range s.spec.CheckRoutes(s.router.Routes()) {
		if !strings.Contains(p, "probe") {
			t.Errorf("路由元数据与 OpenAPI 不一致: %s", p)
		}
	}
}
