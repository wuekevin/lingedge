package authit

import (
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

func (s *stack) reauth(realm, token, csrf string, body map[string]string, opts ...reqOpt) resp {
	s.t.Helper()
	r := s.authed(realm, token, csrf, "POST", "/auth/reauth", body, opts...)
	s.contract("reauth", "POST", "/auth/reauth", r)
	return r
}

// ---- AC6：POST /auth/reauth ----

func TestReauthRefreshesSession(t *testing.T) {
	s := newTOTPStack(t)
	u := s.seedUser("platform")
	_, token, csrf := s.login(u, freshIP())
	sid := s.e.rootInt(t, "SELECT id FROM §.sessions WHERE user_id = ?", u.ID)

	// 缺 CSRF → 403；口令错误 → 401 并计入节流/写日志；缺口令 → 400。
	if r := s.authed("admin", token, "", "POST", "/auth/reauth", map[string]string{"password": u.Password}); r.Status != 403 || r.errCode(t) != "auth.csrf_invalid" {
		t.Errorf("缺 CSRF 应 403: %d", r.Status)
	}
	if r := s.reauth("admin", token, csrf, map[string]string{"password": randPassword(t)}); r.Status != 401 || r.errCode(t) != "auth.invalid_credentials" {
		t.Errorf("口令错误应 401: %d %s", r.Status, r.Body)
	}
	if got := s.logReason(u.ID, "login_failed"); len(got) != 1 || got[0] != "reauth_bad_password" {
		t.Errorf("应写 reauth_bad_password: %v", got)
	}
	if r := s.reauth("admin", token, csrf, map[string]string{}); r.Status != 400 {
		t.Errorf("缺口令应 400: %d", r.Status)
	}
	if s.e.rootInt(t, "SELECT reauth_at IS NULL FROM §.sessions WHERE id = ?", sid) != 1 {
		t.Fatal("失败的再认证不得写 reauth_at")
	}

	r := s.reauth("admin", token, csrf, map[string]string{"password": u.Password})
	if r.Status != 200 {
		t.Fatalf("再认证应成功: %d %s", r.Status, r.Body)
	}
	j := r.JSON(t)
	at, _ := time.Parse(time.RFC3339Nano, j["reauth_at"].(string))
	exp, _ := time.Parse(time.RFC3339Nano, j["expires_at"].(string))
	if exp.Sub(at) != 300*time.Second {
		t.Errorf("缺省窗口应为 300 秒（UD-29）: %v", exp.Sub(at))
	}
	if s.e.rootInt(t, "SELECT reauth_at IS NOT NULL FROM §.sessions WHERE id = ?", sid) != 1 {
		t.Error("成功应写 sessions.reauth_at")
	}
	// 未启用 TOTP 的会话：再认证不写 mfa_verified_at。
	if s.e.rootInt(t, "SELECT mfa_verified_at IS NULL FROM §.sessions WHERE id = ?", sid) != 1 {
		t.Error("无 TOTP 的账号再认证不应写 mfa_verified_at")
	}
	// 未认证 → 401。
	if r := s.do("POST", "/auth/reauth", map[string]string{"password": u.Password}); r.Status != 401 {
		t.Errorf("未认证应 401: %d", r.Status)
	}
}

func TestReauthWithTOTP(t *testing.T) {
	s := newTOTPStack(t)
	a, _, _ := s.enroll(s.seedUser("platform"))
	token, csrf := s.loginTOTP(a, s.fresh(a))
	sid := s.e.rootInt(t, "SELECT id FROM §.sessions WHERE user_id = ? AND revoked_at IS NULL ORDER BY id DESC LIMIT 1", a.user.ID)

	// 已启用 TOTP：缺验证码 → 401 auth.totp_required（不计失败）。
	failedBefore := s.logRows(a.user.ID, "login_failed") + s.logRows(a.user.ID, "totp_failed")
	if r := s.reauth("admin", token, csrf, map[string]string{"password": a.user.Password}); r.Status != 401 || r.errCode(t) != "auth.totp_required" {
		t.Errorf("缺验证码应 401 totp_required: %d %s", r.Status, r.Body)
	}
	if s.logRows(a.user.ID, "login_failed")+s.logRows(a.user.ID, "totp_failed") != failedBefore {
		t.Error("缺验证码是正常的两步交互，不计失败")
	}
	// 错误验证码 / 非 6 位数字。
	if r := s.reauth("admin", token, csrf, map[string]string{"password": a.user.Password, "totp_code": s.wrongCode(a.secret)}); r.Status != 401 || r.errCode(t) != "auth.totp_invalid" {
		t.Errorf("验证码错误应 401 totp_invalid: %d %s", r.Status, r.Body)
	}
	if r := s.reauth("admin", token, csrf, map[string]string{"password": a.user.Password, "totp_code": "12ab"}); r.Status != 400 {
		t.Errorf("非法验证码应 400: %d", r.Status)
	}
	// 恢复码不能用于再认证（契约只允许 6 位 TOTP）。
	if r := s.reauth("admin", token, csrf, map[string]string{"password": a.user.Password, "totp_code": a.recovery[0]}); r.Status != 400 {
		t.Errorf("恢复码不能用于再认证: %d", r.Status)
	}
	if s.e.rootInt(t, "SELECT reauth_at IS NULL FROM §.sessions WHERE id = ?", sid) != 1 {
		t.Fatal("失败不得写 reauth_at")
	}
	// 登录时已用掉当前 time-step 的验证码不能再用于再认证（重放防护是按用户的，跨用途）。
	if r := s.reauth("admin", token, csrf, map[string]string{"password": a.user.Password, "totp_code": genCode(t, a.secret, s.clk.Now())}); r.Status != 401 {
		t.Errorf("已用 time-step 的验证码重放应被拒: %d", r.Status)
	}
	s.clk.Advance(2 * time.Minute)
	if r := s.reauth("admin", token, csrf, map[string]string{"password": a.user.Password, "totp_code": s.fresh(a)}); r.Status != 200 {
		t.Fatalf("口令 + 验证码应通过: %d %s", r.Status, r.Body)
	}
	if s.e.rootInt(t, "SELECT reauth_at IS NOT NULL AND mfa_verified_at >= reauth_at - INTERVAL 1 SECOND FROM §.sessions WHERE id = ?", sid) != 1 {
		t.Error("含第二因子的再认证应同时刷新 mfa_verified_at")
	}
}

// 失败的再认证计入登录节流（共享账号维度）。
func TestReauthFailuresThrottle(t *testing.T) {
	s := newTOTPStack(t, map[string]string{"login.throttle_account_max": "3", "login.throttle_ip_max": "1000"})
	u := s.seedUser("platform")
	_, token, csrf := s.login(u, freshIP())
	var st []int
	for i := 0; i < 4; i++ {
		st = append(st, s.reauth("admin", token, csrf, map[string]string{"password": randPassword(t)}).Status)
	}
	if st[0] != 401 || st[2] != 401 || st[3] != 429 {
		t.Errorf("应 401…然后 429: %v", st)
	}
	if r := s.reauth("admin", token, csrf, map[string]string{"password": u.Password}); r.Status != 429 {
		t.Errorf("锁定期间正确口令也应 429: %d", r.Status)
	}
}

// ---- AC6：x-reauth operation 在授权链里统一执行 ----

func TestXReauthOperationsEnforcedInChain(t *testing.T) {
	s := newStack(t, stackOpts{fakeClock: true, reauthRoutes: true})
	// 集合与 1.3 固定清单逐项相等（契约实际为 11 个；修订日志里的「10 个」已在 scripts/contract/data/x-reauth.txt 注明以契约为准）。
	pinnedRaw, err := os.ReadFile("../../../scripts/contract/data/x-reauth.txt")
	if err != nil {
		t.Fatal(err)
	}
	var pinned []string
	for _, l := range strings.Split(string(pinnedRaw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			pinned = append(pinned, l)
		}
	}
	type opx struct{ id, method, path string }
	var ops []opx
	var got []string
	for _, m := range s.router.Routes() {
		if m.Reauth {
			ops = append(ops, opx{m.OperationID, m.Method, concretePath(m.Path)})
			got = append(got, m.OperationID)
		}
	}
	sort.Strings(got)
	sort.Strings(pinned)
	if strings.Join(got, ",") != strings.Join(pinned, ",") || len(ops) != 11 {
		t.Fatalf("已注册的 x-reauth 路由应与固定清单一致:\n got %v\nwant %v", got, pinned)
	}

	u := s.seedUser("platform")
	s.grantPlatformRole(u, "platform_admin") // 先权限后再认证（Story 1.18）：x-reauth 路由的再认证检查只对已通过权限判定的主体运行
	_, token, csrf := s.login(u, freshIP())
	call := func(o opx, extra ...reqOpt) resp {
		var body any
		if o.method != "GET" && o.method != "DELETE" {
			body = map[string]string{}
		}
		return s.authed("admin", token, csrf, o.method, o.path, body, extra...)
	}
	// 窗口外（从未再认证）：全部 403 auth.reauth_required，且符合契约；handler 不执行。
	for _, o := range ops {
		r := call(o)
		if r.Status != 403 || r.errCode(t) != "auth.reauth_required" || strings.Contains(string(r.Body), "reached") {
			t.Errorf("%s 窗口外应 403 auth.reauth_required: %d %s", o.id, r.Status, r.Body)
		}
		s.contract(o.id, o.method, o.path, r)
	}
	// 再认证后：窗口内越过再认证与授权（占位路由到达 handler → 200；1.18 已实现的真实 handler 因空请求体/不存在的目标返回 400/404，但绝不是 403）。
	if r := s.reauth("admin", token, csrf, map[string]string{"password": u.Password}); r.Status != 200 {
		t.Fatalf("reauth: %d %s", r.Status, r.Body)
	}
	// Story 1.18 已有真实实现的 x-reauth operation（其余仍是按 openapi 元数据注册的占位路由）。
	realOps := map[string]bool{"createUser": true, "deleteUser": true, "resetUserPassword": true, "setUserRoles": true,
		"createRole": true, "updateRole": true, "deleteRole": true, "setRolePermissions": true}
	audited := map[string]bool{}
	for _, m := range s.router.Routes() {
		audited[m.OperationID] = m.Audit
	}
	for _, o := range ops {
		r := call(o)
		if r.Status == 403 {
			t.Errorf("%s 窗口内且有权限，不应再 403: %s", o.id, r.Body)
		}
		if realOps[o.id] {
			continue
		}
		// 占位路由：要求审计（x-audit）的在 audit 模块（1.21）装配前仍被审计钩子安全拒绝（500，不放宽）；其余应到达 handler。
		if audited[o.id] {
			if r.Status != 500 || strings.Contains(string(r.Body), "reached") {
				t.Errorf("%s 要求审计但审计钩子未实现，应被安全拒绝（500）: %d %s", o.id, r.Status, r.Body)
			}
		} else if !strings.Contains(string(r.Body), "reached") {
			t.Errorf("%s 占位路由应到达 handler: %d %s", o.id, r.Status, r.Body)
		}
	}
	// 窗口边界：恰好 300 秒仍在窗口内；多 1 毫秒即过期。
	s.clk.Advance(300 * time.Second)
	if r := call(ops[0]); r.Status == 403 {
		t.Errorf("恰在窗口末尾应仍有效: %d %s", r.Status, r.Body)
	}
	s.clk.Advance(time.Millisecond)
	for _, o := range ops {
		if r := call(o); r.Status != 403 || r.errCode(t) != "auth.reauth_required" {
			t.Errorf("%s 窗口过期后应 403 auth.reauth_required: %d %s", o.id, r.Status, r.Body)
		}
	}
	// 非 x-reauth 的路由不受影响（探针路由：认证通过后由授权钩子拒绝，而不是 reauth_required）。
	if r := s.authed("admin", token, csrf, "POST", "/probe/platform", nil); r.errCode(t) != "auth.forbidden" {
		t.Errorf("非 x-reauth 路由不应要求再认证: %s", r.Body)
	}

	// Bearer API Token 请求不能满足再认证（真实令牌，Story 1.19）：令牌带着这些路由所需的全部 scopes，授权钩子放行，
	// 但令牌没有会话，ReauthChecker 永远不满足 → 403 auth.reauth_required；handler 不执行。即使会话刚刚再认证过也一样。
	need := map[string]bool{}
	for _, m := range s.router.Routes() {
		if m.Reauth {
			for _, p := range m.Permissions {
				need[p] = true
			}
		}
	}
	var scopes []string
	for p := range need {
		scopes = append(scopes, p)
	}
	sort.Strings(scopes)
	ss := session{u: u, token: token, csrf: csrf}
	bearer := s.issue(ss, map[string]any{"name": "reauth-probe", "scopes": scopes}).Token
	for _, o := range ops {
		r := s.do(o.method, o.path, map[string]string{}, withHeader("Authorization", "Bearer "+bearer))
		if r.Status != 403 || r.errCode(t) != "auth.reauth_required" || strings.Contains(string(r.Body), "reached") {
			t.Errorf("%s: Bearer 请求不能越过 x-reauth 路由（应 403 auth.reauth_required）: %d %s", o.id, r.Status, r.Body)
		}
	}
	if r := s.do("POST", "/auth/reauth", map[string]string{"password": u.Password}, withHeader("Authorization", "Bearer "+bearer)); r.Status == 200 {
		t.Errorf("Bearer 请求不得通过 /auth/reauth: %d", r.Status)
	}
}

// 窗口来自系统设置 auth.reauth_window_s（可配置）。
func TestReauthWindowConfigurable(t *testing.T) {
	s := newStack(t, stackOpts{fakeClock: true, reauthRoutes: true, settings: map[string]string{"auth.reauth_window_s": "60"}})
	u := s.seedUser("platform")
	s.grantPlatformRole(u, "platform_admin")
	_, token, csrf := s.login(u, freshIP())
	r := s.reauth("admin", token, csrf, map[string]string{"password": u.Password})
	j := r.JSON(t)
	at, _ := time.Parse(time.RFC3339Nano, j["reauth_at"].(string))
	exp, _ := time.Parse(time.RFC3339Nano, j["expires_at"].(string))
	if exp.Sub(at) != 60*time.Second {
		t.Fatalf("窗口应取自系统设置: %v", exp.Sub(at))
	}
	s.clk.Advance(61 * time.Second)
	if r := s.authed("admin", token, csrf, "PUT", "/admin/system/run-mode", map[string]string{}); r.errCode(t) != "auth.reauth_required" {
		t.Errorf("61 秒应已过期: %d %s", r.Status, r.Body)
	}
}

func concretePath(p string) string {
	for strings.Contains(p, "{") {
		i, j := strings.Index(p, "{"), strings.Index(p, "}")
		p = p[:i] + ids.MustNew() + p[j+1:]
	}
	return p
}

// ---- AC3：近期 MFA ----

func TestRecentMFAForSensitiveOperations(t *testing.T) {
	s := newTOTPStack(t, map[string]string{"auth.mfa_window_s": "120"})
	a, _, _ := s.enroll(s.seedUser("platform"))
	token, csrf := s.loginTOTP(a, s.fresh(a))
	newPW := randPassword(t)
	change := func(cur string) resp {
		r := s.authed("admin", token, csrf, "POST", "/auth/password/change", map[string]string{"current_password": cur, "new_password": newPW})
		return r
	}
	// 窗口外：改密被拒（先于口令校验：403 auth.reauth_required，口令不变、会话不被吊销）。
	s.clk.Advance(121 * time.Second)
	r := change(a.user.Password)
	s.contract("changePassword", "POST", "/auth/password/change", r)
	if r.Status != 403 || r.errCode(t) != "auth.reauth_required" || !strings.Contains(string(r.Body), "mfa_verified_at") {
		t.Fatalf("MFA 过期后改密应 403 auth.reauth_required: %d %s", r.Status, r.Body)
	}
	if s.e.rootInt(t, "SELECT COUNT(*) FROM §.sessions WHERE user_id = ? AND revoked_at IS NULL", a.user.ID) == 0 {
		t.Error("被拒的改密不得吊销会话")
	}
	// 其他（非敏感）操作不受影响。
	if r := s.authed("admin", token, csrf, "GET", "/auth/sessions", nil); r.Status != 200 {
		t.Errorf("非敏感操作不要求近期 MFA: %d", r.Status)
	}
	// 经 /auth/reauth（口令 + 验证码）刷新 mfa_verified_at 后可改密。
	if r := s.reauth("admin", token, csrf, map[string]string{"password": a.user.Password, "totp_code": s.fresh(a)}); r.Status != 200 {
		t.Fatalf("reauth: %d %s", r.Status, r.Body)
	}
	if r := change(a.user.Password); r.Status != 204 {
		t.Fatalf("MFA 刷新后改密应成功: %d %s", r.Status, r.Body)
	}
	// 窗口边界：恰好等于窗口仍有效；多 1 毫秒过期（用另一个账号，改密失败路径不吊销会话）。
	b, _, _ := s.enroll(s.seedUser("platform"))
	tk, cs := s.loginTOTP(b, s.fresh(b))
	s.clk.Advance(120 * time.Second)
	if r := s.authed("admin", tk, cs, "POST", "/auth/password/change", map[string]string{"current_password": randPassword(t), "new_password": newPW}); r.Status != 401 {
		t.Errorf("窗口末尾应越过 MFA 检查（随后因当前口令错误 401）: %d %s", r.Status, r.Body)
	}
	s.clk.Advance(time.Millisecond)
	if r := s.authed("admin", tk, cs, "POST", "/auth/password/change", map[string]string{"current_password": b.user.Password, "new_password": newPW}); r.Status != 403 || r.errCode(t) != "auth.reauth_required" {
		t.Errorf("超出窗口 1 毫秒应被拒: %d %s", r.Status, r.Body)
	}
}

// 未启用 TOTP 的账号没有第二因子可验证，改密不要求近期 MFA；缺省窗口为 15 分钟。
func TestRecentMFADefaultsAndNoTOTP(t *testing.T) {
	s := newTOTPStack(t)
	u := s.seedUser("tenant")
	_, token, csrf := s.login(u, freshIP())
	s.clk.Advance(time.Hour)
	if r := s.authed("console", token, csrf, "POST", "/auth/password/change", map[string]string{"current_password": u.Password, "new_password": randPassword(t)}); r.Status != 204 {
		t.Errorf("未启用 TOTP 的账号改密不要求近期 MFA: %d %s", r.Status, r.Body)
	}
	a, _, _ := s.enroll(s.seedUser("tenant"))
	tk, cs := s.loginTOTP(a, s.fresh(a))
	s.clk.Advance(15 * time.Minute)
	if r := s.authed("console", tk, cs, "POST", "/auth/password/change", map[string]string{"current_password": randPassword(t), "new_password": randPassword(t)}); r.Status != 401 {
		t.Errorf("缺省窗口 15 分钟：恰好 15 分钟仍有效（随后因口令错误 401）: %d %s", r.Status, r.Body)
	}
	s.clk.Advance(time.Millisecond)
	if r := s.authed("console", tk, cs, "POST", "/auth/password/change", map[string]string{"current_password": a.user.Password, "new_password": randPassword(t)}); r.Status != 403 {
		t.Errorf("超过 15 分钟应要求再次验证: %d %s", r.Status, r.Body)
	}
}

// 登录并通过 TOTP 后，会话立即满足近期 MFA；创建 API Token（1.19）等敏感操作的钩子位置与 Bearer 行为见 handler 单测。
func TestFreshTOTPLoginSatisfiesRecentMFA(t *testing.T) {
	s := newTOTPStack(t)
	a, _, _ := s.enroll(s.seedUser("platform"))
	tk, cs := s.loginTOTP(a, s.fresh(a))
	if r := s.authed("admin", tk, cs, "POST", "/auth/password/change", map[string]string{"current_password": a.user.Password, "new_password": randPassword(t)}); r.Status != 204 {
		t.Errorf("刚通过 TOTP 登录应可改密: %d %s", r.Status, r.Body)
	}
}

// ---- Redis 关闭下 TOTP / 恢复码 / 再认证 仍工作 ----

func TestTOTPWorksWithoutRedis(t *testing.T) {
	s := newStack(t, stackOpts{fakeClock: true, noRedis: true})
	a, _, _ := s.enroll(s.seedUser("platform"))
	token, csrf := s.loginTOTP(a, s.fresh(a))
	// 重放防护不依赖 Redis（已持久化到 MySQL）。
	code := s.fresh(a)
	s.loginTOTP(a, code)
	ip := freshIP()
	if r := s.verify(s.passwordStep(a.user, ip), code, ip); r.Status != 401 {
		t.Errorf("Redis 关闭时重放仍应被拒: %d", r.Status)
	}
	if r := s.verify(s.passwordStep(a.user, ip), a.recovery[0], ip); r.Status != 200 {
		t.Errorf("Redis 关闭时恢复码仍可登录: %d %s", r.Status, r.Body)
	}
	if r := s.reauth("admin", token, csrf, map[string]string{"password": a.user.Password, "totp_code": s.fresh(a)}); r.Status != 200 {
		t.Errorf("Redis 关闭时再认证仍可用: %d %s", r.Status, r.Body)
	}
	// 失败仍计入节流（降级路径）并写日志。
	for i := 0; i < 3; i++ {
		s.verify(s.passwordStep(a.user, ip), s.wrongCode(a.secret), ip)
	}
	if s.logRows(a.user.ID, "totp_failed") < 4 {
		t.Error("Redis 关闭时失败仍应写 totp_failed")
	}
}

// ---- 密钥不明文落库扫描 + 日志脱敏（AC：NFR-12）----

func TestNoPlaintextSecretsAnywhere(t *testing.T) {
	s := newTOTPStack(t)
	admin := s.seedUser("platform")
	a, _, _ := s.enroll(admin)
	needles := map[string]string{"TOTP 种子": a.secret}
	for i, c := range a.recovery {
		needles["恢复码"+string(rune('A'+i))] = c
		needles["恢复码(无连字符)"+string(rune('A'+i))] = strings.ReplaceAll(c, "-", "")
	}

	// 走完：口令步骤 → 第二步（TOTP）→ 恢复码登录 → 再认证 → 失败路径。
	ip := freshIP()
	mfa1 := s.passwordStep(a.user, ip)
	needles["mfa_token 1"] = mfa1
	needles["口令"] = a.user.Password
	s.verify(mfa1, s.fresh(a), ip)
	mfa2 := s.passwordStep(a.user, ip)
	needles["mfa_token 2"] = mfa2
	r := s.verify(mfa2, a.recovery[0], ip)
	tok := r.cookie("le_admin_sid").Value
	csrf2, _ := r.JSON(t)["csrf_token"].(string)
	needles["会话令牌"], needles["CSRF"] = tok, csrf2
	s.reauth("admin", tok, csrf2, map[string]string{"password": a.user.Password, "totp_code": s.fresh(a)})
	bad := randPassword(t)
	needles["错误口令"] = bad
	s.reauth("admin", tok, csrf2, map[string]string{"password": bad, "totp_code": s.wrongCode(a.secret)})
	s.mod.Flush()

	// 1) 日志。
	logs := s.logs.String()
	if len(logs) < 500 {
		t.Fatalf("应有日志可扫描: %d", len(logs))
	}
	for name, n := range needles {
		if strings.Contains(logs, n) {
			t.Errorf("日志泄露了 %s", name)
		}
	}
	if !strings.Contains(logs, "TOTP 已启用") {
		t.Error("应有「TOTP 已启用」的结构化日志（审计待 1.21 补写 audit_logs）")
	}
	// 2) Redis 全部键与值。
	rdb := goredis.NewClient(&goredis.Options{Addr: s.redis.Addr()})
	defer func() { _ = rdb.Close() }()
	keys, err := rdb.Keys(ctxT(t), "*").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		v, _ := rdb.Get(ctxT(t), k).Result()
		for name, n := range needles {
			if strings.Contains(k, n) || strings.Contains(v, n) {
				t.Errorf("Redis 键 %s 泄露了 %s", k, name)
			}
		}
	}
	// 3) 库内所有表、所有文本/二进制/JSON 列。
	if f := s.scanDB(needles); len(f) > 0 {
		t.Errorf("库内出现明文秘密:\n%s", strings.Join(f, "\n"))
	}
	// totp_secret_enc 是 5 段信封密文。
	if enc := s.e.rootStr(t, "SELECT totp_secret_enc FROM §.users WHERE id = ?", a.user.ID); len(strings.Split(enc, ":")) != 5 || strings.Contains(enc, a.secret) {
		t.Errorf("totp_secret_enc 应是 5 段信封密文: %q", enc)
	}
	// mfa 挑战只存哈希。
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.login_mfa_challenges WHERE token_hash REGEXP '^[0-9a-f]{64}$'"); n < 2 {
		t.Errorf("挑战应存 64 位十六进制哈希: %d", n)
	}
}

// scanDB 在库内全部表的全部文本/二进制/JSON 列里搜索 needles（名称 → 明文），返回命中描述。
func (s *stack) scanDB(needles map[string]string) []string {
	s.t.Helper()
	cols := s.e.columns(s.t)
	if len(cols) < 100 {
		s.t.Fatalf("应扫描全部表的文本列（实际仅 %d 列）", len(cols))
	}
	var out []string
	for _, c := range cols {
		for name, n := range needles {
			q := "SELECT COUNT(*) FROM §.`" + c.table + "` WHERE CAST(`" + c.col + "` AS CHAR) LIKE CONCAT('%', ?, '%')"
			if cnt := s.e.rootInt(s.t, q, n); cnt != 0 {
				out = append(out, name+" 出现在 "+c.table+"."+c.col)
			}
		}
	}
	return out
}

// 扫描本身必须有区分力：故意把种子明文写进一列，扫描必须能发现（负向自测，防止扫描逻辑失效而「永远通过」）。
func TestPlaintextScanDetectsLeak(t *testing.T) {
	s := newTOTPStack(t)
	u := s.seedUser("platform")
	seed := "JBSWY3DPEHPK3PXP" + strings.ToUpper(mustRand(t))
	if f := s.scanDB(map[string]string{"seed": seed}); len(f) != 0 {
		t.Fatalf("干净的库不应有命中: %v", f)
	}
	s.e.root(t, "UPDATE §.users SET display_name = ? WHERE id = ?", "x"+seed+"y", u.ID)
	f := s.scanDB(map[string]string{"seed": seed})
	if len(f) != 1 || !strings.Contains(f[0], "users.display_name") {
		t.Errorf("应发现 users.display_name 中的明文: %v", f)
	}
}

func mustRand(t testing.TB) string { return strings.ReplaceAll(randPassword(t), "-", "")[:12] }
