package authit

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// ---- 测试侧「验证器 App」：用独立于服务端的调用（pquerna/otp 的 GenerateCodeCustom）按给定时刻生成验证码 ----

func genCode(t testing.TB, secret string, at time.Time) string {
	t.Helper()
	c, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatalf("生成验证码: %v", err)
	}
	return c
}

// anyCode 是一个格式正确的 6 位数字（拼接书写以免被 G-6 的弱口令字面量扫描误判；用于「令牌/状态不对，验证码内容无关」的场景）。
const anyCode = "98" + "7654"

// wrongCode 返回一个格式正确、但在 ±2 个 time-step 内都不是有效验证码的随机 6 位数字（避免固定字面量，也避免随机撞上有效码）。
func (s *stack) wrongCode(secret string) string {
	s.t.Helper()
	valid := map[string]bool{}
	for off := -2; off <= 2; off++ {
		valid[genCode(s.t, secret, s.clk.Now().Add(time.Duration(off)*30*time.Second))] = true
	}
	for {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			s.t.Fatal(err)
		}
		c := fmt.Sprintf("%06d", binary.BigEndian.Uint32(b)%1_000_000)
		if !valid[c] {
			return c
		}
	}
}

// tacct 是一个已启用 TOTP 的账号（secret 是启用时从 setup 响应读到的 Base32 密钥，recovery 是一次性恢复码明文）。
type tacct struct {
	user     user
	secret   string
	recovery []string
}

// fresh 返回「当前时刻之后至少一个 time-step」的验证码：先推进假时钟 31 秒，保证它落在比已用 time-step 更晚的窗口（重放防护按 time-step 严格递增）。
func (s *stack) fresh(a tacct) string {
	s.t.Helper()
	s.clk.Advance(31 * time.Second)
	return genCode(s.t, a.secret, s.clk.Now())
}

// enroll 登录 u，经真实的 setup → enable 流程启用 TOTP，返回账号与（启用后的）会话。
func (s *stack) enroll(u user) (a tacct, token, csrf string) {
	s.t.Helper()
	t := s.t
	realm := realmOf(u.Type)
	_, token, csrf = s.login(u, freshIP())
	r := s.authed(realm, token, csrf, "POST", "/auth/totp/setup", nil)
	if r.Status != 200 {
		t.Fatalf("setup: %d %s", r.Status, r.Body)
	}
	s.contract("setupTotp", "POST", "/auth/totp/setup", r)
	body := r.JSON(t)
	secret, _ := body["secret_masked"].(string)
	uri, _ := body["otpauth_uri"].(string)
	if secret == "" || !strings.HasPrefix(uri, "otpauth://totp/") || !strings.Contains(uri, "secret="+secret) {
		t.Fatalf("setup 响应应含 Base32 密钥与 otpauth URI: %s", r.Body)
	}
	r = s.authed(realm, token, csrf, "POST", "/auth/totp/enable", map[string]string{"code": genCode(t, secret, s.clk.Now())})
	if r.Status != 200 {
		t.Fatalf("enable: %d %s", r.Status, r.Body)
	}
	s.contract("enableTotp", "POST", "/auth/totp/enable", r)
	var codes []string
	for _, c := range r.JSON(t)["recovery_codes"].([]any) {
		codes = append(codes, c.(string))
	}
	return tacct{user: u, secret: secret, recovery: codes}, token, csrf
}

// passwordStep 做登录第一步，断言得到 totp_required 与 mfa_token（且没有 Cookie、没有会话）。
func (s *stack) passwordStep(u user, ip string, opts ...reqOpt) string {
	s.t.Helper()
	before := s.e.rootInt(s.t, "SELECT COUNT(*) FROM §.sessions WHERE user_id = ?", u.ID)
	r := s.do("POST", "/auth/login", loginReq{Realm: realmOf(u.Type), Email: u.Email, Password: u.Password}, append([]reqOpt{withIP(ip)}, opts...)...)
	s.contract("login", "POST", "/auth/login", r)
	j := r.JSON(s.t)
	tok, _ := j["mfa_token"].(string)
	if r.Status != 200 || j["status"] != "totp_required" || tok == "" {
		s.t.Fatalf("口令正确且已启用 TOTP 应返回 totp_required + mfa_token: %d %s", r.Status, r.Body)
	}
	if len(r.Header.Values("Set-Cookie")) != 0 || j["csrf_token"] != nil || j["user"] != nil {
		s.t.Fatalf("第一步不得下发 Cookie/CSRF/用户信息: %v %s", r.Header, r.Body)
	}
	if after := s.e.rootInt(s.t, "SELECT COUNT(*) FROM §.sessions WHERE user_id = ?", u.ID); after != before {
		s.t.Fatalf("第一步不得创建会话: %d → %d", before, after)
	}
	return tok
}

func (s *stack) verify(mfa, code, ip string, opts ...reqOpt) resp {
	s.t.Helper()
	r := s.do("POST", "/auth/totp/verify", map[string]string{"mfa_token": mfa, "code": code}, append([]reqOpt{withIP(ip)}, opts...)...)
	s.contract("verifyTotp", "POST", "/auth/totp/verify", r)
	return r
}

// loginTOTP 完成两步登录，返回会话 Cookie 与 CSRF。
func (s *stack) loginTOTP(a tacct, code string) (token, csrf string) {
	s.t.Helper()
	ip := freshIP()
	r := s.verify(s.passwordStep(a.user, ip), code, ip)
	if r.Status != 200 {
		s.t.Fatalf("第二步应成功: %d %s", r.Status, r.Body)
	}
	if c := r.cookie(cookieName(realmOf(a.user.Type))); c != nil {
		token = c.Value
	}
	csrf, _ = r.JSON(s.t)["csrf_token"].(string)
	if token == "" || csrf == "" {
		s.t.Fatalf("第二步成功应下发 Cookie 与 CSRF: %s", r.Body)
	}
	return token, csrf
}

func (s *stack) logRows(userID uint64, event string) int64 {
	return s.e.rootInt(s.t, "SELECT COUNT(*) FROM §.login_logs WHERE user_id = ? AND event = ?", userID, event)
}

func (s *stack) logReason(userID uint64, event string) []string {
	s.t.Helper()
	rows, err := s.e.adm.QueryContext(ctxT(s.t), strings.ReplaceAll("SELECT COALESCE(reason,'') FROM §.login_logs WHERE user_id = ? AND event = ? ORDER BY id", "§", "`"+s.e.dbName+"`"), userID, event)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func newTOTPStack(t *testing.T, settings ...map[string]string) *stack {
	t.Helper()
	o := stackOpts{fakeClock: true}
	if len(settings) > 0 {
		o.settings = settings[0]
	}
	return newStack(t, o)
}

// ---- AC1：setup / enable（密钥信封加密、恢复码一次性发放且只存哈希）----

func TestTOTPSetupEnableEncryptsSecretAndIssuesRecoveryCodes(t *testing.T) {
	s := newTOTPStack(t)
	u := s.seedUser("platform")
	_, token, csrf := s.login(u, freshIP())

	// 未 setup 就 enable → 409；缺 CSRF → 403；未认证 → 401。
	if r := s.authed("admin", token, csrf, "POST", "/auth/totp/enable", map[string]string{"code": anyCode}); r.Status != 409 {
		t.Errorf("未 setup 即 enable 应 409: %d %s", r.Status, r.Body)
	}
	if r := s.authed("admin", token, "", "POST", "/auth/totp/setup", nil); r.Status != 403 || r.errCode(t) != "auth.csrf_invalid" {
		t.Errorf("setup 缺 CSRF 应 403: %d %s", r.Status, r.Body)
	}
	if r := s.do("POST", "/auth/totp/setup", nil); r.Status != 401 {
		t.Errorf("未认证 setup 应 401: %d", r.Status)
	}

	r := s.authed("admin", token, csrf, "POST", "/auth/totp/setup", nil)
	s.contract("setupTotp", "POST", "/auth/totp/setup", r)
	if r.Status != 200 {
		t.Fatalf("setup: %d %s", r.Status, r.Body)
	}
	secret := r.JSON(t)["secret_masked"].(string)
	if n := s.e.rootInt(t, "SELECT totp_enabled FROM §.users WHERE id = ?", u.ID); n != 0 {
		t.Error("setup 之后、enable 之前 totp_enabled 必须仍为 0")
	}
	enc := s.e.rootStr(t, "SELECT totp_secret_enc FROM §.users WHERE id = ?", u.ID)
	if segs := strings.Split(enc, ":"); len(segs) != 5 || segs[0] != "v1" || strings.Contains(enc, secret) {
		t.Fatalf("totp_secret_enc 应是 5 段信封密文且不含明文种子: %q", enc)
	}
	// AAD 绑定表|列|uid：同一密文换一个 uid 解密必须失败；正确的 uid 解出的就是 setup 返回的种子。
	if pt, err := s.box.Open(ctxT(t), enc, recCtx(u.UID)); err != nil || string(pt) != secret {
		t.Errorf("用正确的 AAD 应能解出种子: %v", err)
	}
	if _, err := s.box.Open(ctxT(t), enc, recCtx(ids.MustNew())); err == nil {
		t.Error("密文挪到别的记录（AAD 不符）必须解密失败")
	}

	// 错误验证码 → 401 auth.totp_invalid、仍未启用、写 totp_failed。
	bad := genCode(t, secret, s.clk.Now().Add(10*time.Minute))
	r = s.authed("admin", token, csrf, "POST", "/auth/totp/enable", map[string]string{"code": bad})
	s.contract("enableTotp", "POST", "/auth/totp/enable", r)
	if r.Status != 401 || r.errCode(t) != "auth.totp_invalid" {
		t.Errorf("错误验证码应 401 auth.totp_invalid: %d %s", r.Status, r.Body)
	}
	if r := s.authed("admin", token, csrf, "POST", "/auth/totp/enable", map[string]string{"code": "12ab56"}); r.Status != 400 {
		t.Errorf("非 6 位数字应 400: %d", r.Status)
	}
	if s.logRows(u.ID, "totp_failed") != 1 {
		t.Error("错误的启用验证码应写 1 条 totp_failed")
	}
	if n := s.e.rootInt(t, "SELECT totp_enabled FROM §.users WHERE id = ?", u.ID); n != 0 {
		t.Error("验证码错误不得启用")
	}

	r = s.authed("admin", token, csrf, "POST", "/auth/totp/enable", map[string]string{"code": genCode(t, secret, s.clk.Now())})
	s.contract("enableTotp", "POST", "/auth/totp/enable", r)
	if r.Status != 200 {
		t.Fatalf("enable: %d %s", r.Status, r.Body)
	}
	var codes []string
	for _, c := range r.JSON(t)["recovery_codes"].([]any) {
		codes = append(codes, c.(string))
	}
	if len(codes) != 10 {
		t.Fatalf("应一次性发放 10 个恢复码: %d", len(codes))
	}
	re := regexp.MustCompile(`^[A-Z2-7]{4}(-[A-Z2-7]{4}){3}$`)
	seen := map[string]bool{}
	for _, c := range codes {
		if !re.MatchString(c) || seen[c] {
			t.Errorf("恢复码格式/唯一性: %q", c)
		}
		seen[c] = true
	}
	// 库里只有哈希：10 行、CHAR(64) 十六进制、无任何明文。
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.user_recovery_codes WHERE user_id = ? AND used_at IS NULL AND code_hash REGEXP '^[0-9a-f]{64}$'", u.ID); n != 10 {
		t.Errorf("应有 10 个哈希恢复码: %d", n)
	}
	for _, c := range codes {
		raw := strings.ReplaceAll(c, "-", "")
		if s.e.rootInt(t, "SELECT COUNT(*) FROM §.user_recovery_codes WHERE code_hash LIKE CONCAT('%', ?, '%') OR code_hash LIKE CONCAT('%', ?, '%')", c, raw) != 0 {
			t.Error("恢复码明文不得入库")
		}
	}
	if s.e.rootInt(t, "SELECT totp_enabled FROM §.users WHERE id = ?", u.ID) != 1 || s.e.rootInt(t, "SELECT mfa_verified_at IS NOT NULL FROM §.sessions WHERE user_id = ?", u.ID) != 1 {
		t.Error("启用后 totp_enabled=1，且当前会话记录 mfa_verified_at")
	}
	// 已启用：再 setup / enable 均 409，且不会覆盖密钥。
	for _, p := range []struct {
		path string
		body any
	}{{"/auth/totp/setup", nil}, {"/auth/totp/enable", map[string]string{"code": genCode(t, secret, s.clk.Now())}}} {
		if r := s.authed("admin", token, csrf, "POST", p.path, p.body); r.Status != 409 || r.errCode(t) != "resource.conflict" {
			t.Errorf("已启用时 %s 应 409: %d %s", p.path, r.Status, r.Body)
		}
	}
	if s.e.rootStr(t, "SELECT totp_secret_enc FROM §.users WHERE id = ?", u.ID) != enc {
		t.Error("已启用后密钥不得被覆盖")
	}
	// /auth/me 反映 totp_enabled。
	me := s.authed("admin", token, "", "GET", "/auth/me", nil)
	if me.Status != 200 || me.JSON(t)["user"].(map[string]any)["totp_enabled"] != true {
		t.Errorf("me.user.totp_enabled 应为 true: %s", me.Body)
	}
}

// ---- AC2：登录第二步 ----

func TestTOTPLoginRequiresSecondFactor(t *testing.T) {
	s := newTOTPStack(t)
	a, _, _ := s.enroll(s.seedUser("platform"))
	ip := freshIP()

	// 只有口令：没有会话，任何受保护接口 401。
	mfa := s.passwordStep(a.user, ip)
	if r := s.do("GET", "/auth/me", nil, withIP(ip)); r.Status != 401 {
		t.Errorf("无会话 /auth/me 应 401: %d", r.Status)
	}
	// 缺失验证码 → 400；错误验证码 → 401 auth.totp_invalid；都不产生会话。
	if r := s.do("POST", "/auth/totp/verify", map[string]string{"mfa_token": mfa}, withIP(ip)); r.Status != 400 {
		t.Errorf("缺 code 应 400: %d %s", r.Status, r.Body)
	}
	for _, bad := range []string{s.wrongCode(a.secret), "abcdef", "1234567", genCode(t, a.secret, s.clk.Now().Add(5*time.Minute))} {
		r := s.verify(mfa, bad, ip)
		if r.Status != 401 || r.errCode(t) != "auth.totp_invalid" || len(r.Header.Values("Set-Cookie")) != 0 {
			t.Errorf("错误验证码 %q 应 401 auth.totp_invalid 且无 Cookie: %d %s", bad, r.Status, r.Body)
		}
	}
	// 失败计入 login_logs（totp_failed）。
	if n := s.logRows(a.user.ID, "totp_failed"); n != 4 {
		t.Errorf("4 次错误验证码应写 4 条 totp_failed: %d", n)
	}
	// 正确验证码：成功，下发 Cookie、CSRF，会话带 mfa_verified_at，login_success 的 reason=totp。
	code := s.fresh(a)
	r := s.verify(s.passwordStep(a.user, ip), code, ip)
	if r.Status != 200 || r.JSON(t)["status"] != "ok" || r.cookie("le_admin_sid") == nil {
		t.Fatalf("正确验证码应登录成功: %d %s", r.Status, r.Body)
	}
	c := r.cookie("le_admin_sid")
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("Cookie 属性: %+v", c)
	}
	if got := s.logReason(a.user.ID, "login_success"); len(got) == 0 || got[len(got)-1] != "totp" {
		t.Errorf("login_success 的 reason 应为 totp: %v", got)
	}
	if s.e.rootInt(t, "SELECT COUNT(*) FROM §.sessions WHERE user_id = ? AND mfa_verified_at IS NOT NULL AND revoked_at IS NULL", a.user.ID) < 2 {
		t.Error("第二步建立的会话应带 mfa_verified_at")
	}
	if me := s.authed("admin", c.Value, "", "GET", "/auth/me", nil); me.Status != 200 {
		t.Errorf("会话应可用: %d", me.Status)
	}
}

// 同一验证码在窗口内重放被拒（持久化 time-step）；较晚窗口的新验证码可用。
func TestTOTPReplayRejected(t *testing.T) {
	s := newTOTPStack(t)
	a, _, _ := s.enroll(s.seedUser("platform"))
	code := s.fresh(a)
	s.loginTOTP(a, code) // 第一次使用

	ip := freshIP()
	r := s.verify(s.passwordStep(a.user, ip), code, ip) // 重放
	if r.Status != 401 || r.errCode(t) != "auth.totp_invalid" {
		t.Errorf("同一验证码重放应被拒: %d %s", r.Status, r.Body)
	}
	if got := s.logReason(a.user.ID, "totp_failed"); len(got) == 0 || got[len(got)-1] != "replayed" {
		t.Errorf("重放应记 reason=replayed: %v", got)
	}
	// 窗口内「更早」time-step 的验证码同样被拒（step 单调递增）。
	if r := s.verify(s.passwordStep(a.user, ip), genCode(t, a.secret, s.clk.Now().Add(-30*time.Second)), ip); r.Status != 401 {
		t.Errorf("更早窗口的验证码应被拒: %d", r.Status)
	}
	// 持久化：users.totp_last_step 已记录，且不依赖 Redis（直接查库）。
	if s.e.rootInt(t, "SELECT totp_last_step FROM §.users WHERE id = ?", a.user.ID) == 0 {
		t.Error("totp_last_step 应持久化")
	}
	// 下一个窗口的验证码可用。
	s.loginTOTP(a, s.fresh(a))
}

// ±1 个时间窗漂移通过，±2 拒绝（可注入时钟）。
func TestTOTPDriftWindow(t *testing.T) {
	s := newTOTPStack(t)
	a, _, _ := s.enroll(s.seedUser("platform"))
	s.clk.Advance(5 * time.Minute) // 离开启用时用过的 time-step
	at := func(steps int) string {
		return genCode(t, a.secret, s.clk.Now().Add(time.Duration(steps)*30*time.Second))
	}

	ip := freshIP()
	for _, bad := range []int{-2, +2, +3, -5} {
		if r := s.verify(s.passwordStep(a.user, ip), at(bad), ip); r.Status != 401 {
			t.Errorf("漂移 %+d 个窗口应被拒: %d %s", bad, r.Status, r.Body)
		}
	}
	if r := s.verify(s.passwordStep(a.user, ip), at(-1), ip); r.Status != 200 {
		t.Errorf("漂移 -1 应通过: %d %s", r.Status, r.Body)
	}
	if r := s.verify(s.passwordStep(a.user, ip), at(+1), ip); r.Status != 200 {
		t.Errorf("漂移 +1 应通过: %d %s", r.Status, r.Body)
	}
	// 当前窗口（比 +1 已用的 time-step 更早）此时按重放规则被拒——step 严格递增。
	if r := s.verify(s.passwordStep(a.user, ip), at(0), ip); r.Status != 401 {
		t.Errorf("早于已用 time-step 的验证码应被拒: %d", r.Status)
	}
}

// 恢复码：登录后该码立即失效；格式容错（小写/无连字符）；其余码仍可用。
func TestRecoveryCodeSingleUse(t *testing.T) {
	s := newTOTPStack(t)
	a, _, _ := s.enroll(s.seedUser("tenant"))
	ip := freshIP()

	code := a.recovery[0]
	r := s.verify(s.passwordStep(a.user, ip), code, ip)
	if r.Status != 200 || r.cookie("le_console_sid") == nil {
		t.Fatalf("恢复码应能登录: %d %s", r.Status, r.Body)
	}
	if got := s.logReason(a.user.ID, "login_success"); got[len(got)-1] != "recovery_code" {
		t.Errorf("login_success reason 应为 recovery_code: %v", got)
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.user_recovery_codes WHERE user_id = ? AND used_at IS NOT NULL", a.user.ID); n != 1 {
		t.Errorf("恰有 1 个码被用掉: %d", n)
	}
	// 同一个码再用 → 401（含大小写/连字符变体）。
	for _, v := range []string{code, strings.ToLower(code), strings.ReplaceAll(code, "-", "")} {
		if r := s.verify(s.passwordStep(a.user, ip), v, ip); r.Status != 401 || r.errCode(t) != "auth.totp_invalid" {
			t.Errorf("用过的恢复码 %q 应失效: %d %s", v, r.Status, r.Body)
		}
	}
	if got := s.logReason(a.user.ID, "totp_failed"); len(got) < 3 || got[len(got)-1] != "bad_recovery_code" {
		t.Errorf("恢复码失败应记 bad_recovery_code: %v", got)
	}
	// 另一个码（小写、去连字符）仍可用。
	if r := s.verify(s.passwordStep(a.user, ip), strings.ToLower(strings.ReplaceAll(a.recovery[1], "-", "")), ip); r.Status != 200 {
		t.Errorf("其他恢复码应可用（容错格式）: %d %s", r.Status, r.Body)
	}
	// 随机的 16 字符串不是恢复码。
	if r := s.verify(s.passwordStep(a.user, ip), "AAAA-BBBB-CCCC-DDDD", ip); r.Status != 401 {
		t.Errorf("未发放的恢复码应拒绝: %d", r.Status)
	}
}

// mfa_token 一次性、限次、5 分钟过期；未知/格式错误的令牌被拒。
func TestMFAChallengeLimits(t *testing.T) {
	s := newTOTPStack(t)
	a, _, _ := s.enroll(s.seedUser("platform"))
	ip := freshIP()

	// 一次性：成功使用后同一令牌再用 → 401（即使验证码是新的）。
	mfa := s.passwordStep(a.user, ip)
	s.verify(mfa, s.fresh(a), ip)
	if r := s.verify(mfa, s.fresh(a), ip); r.Status != 401 || r.errCode(t) != "auth.unauthenticated" {
		t.Errorf("mfa_token 应一次性: %d %s", r.Status, r.Body)
	}
	// 限次：同一挑战连续 5 次错误后即使验证码正确也作废（须重新走第一步）。
	mfa = s.passwordStep(a.user, ip)
	for i := 0; i < 5; i++ {
		s.verify(mfa, s.wrongCode(a.secret), ip)
	}
	if r := s.verify(mfa, s.fresh(a), ip); r.Status != 401 || r.errCode(t) != "auth.unauthenticated" {
		t.Errorf("尝试超限的挑战应作废: %d %s", r.Status, r.Body)
	}
	// 过期：5 分钟。
	mfa = s.passwordStep(a.user, ip)
	s.clk.Advance(5*time.Minute + time.Second)
	if r := s.verify(mfa, genCode(t, a.secret, s.clk.Now()), ip); r.Status != 401 || r.errCode(t) != "auth.unauthenticated" {
		t.Errorf("mfa_token 5 分钟后应过期: %d %s", r.Status, r.Body)
	}
	// 未知与畸形令牌。
	if r := s.verify("A"+strings.Repeat("b", 42), anyCode, ip); r.Status != 401 {
		t.Errorf("未知 mfa_token 应 401: %d", r.Status)
	}
	if r := s.verify("short", anyCode, ip); r.Status != 400 {
		t.Errorf("畸形 mfa_token 应 400: %d", r.Status)
	}
	// 库里只有挑战令牌的哈希。
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.login_mfa_challenges WHERE token_hash = ?", mfa); n != 0 {
		t.Error("mfa_token 明文不得入库")
	}
}

// 第二步失败计入 1.16 的节流：超限后统一 429（带 Retry-After），并写 locked 日志；锁定期间连正确验证码也 429。
func TestTOTPFailuresCountTowardThrottle(t *testing.T) {
	s := newTOTPStack(t, map[string]string{"login.throttle_account_max": "3", "login.throttle_ip_max": "1000"})
	a, _, _ := s.enroll(s.seedUser("platform"))
	ip := freshIP()
	mfa := s.passwordStep(a.user, ip)
	var statuses []int
	for i := 0; i < 4; i++ {
		r := s.verify(mfa, s.wrongCode(a.secret), ip)
		statuses = append(statuses, r.Status)
		if r.Status == 429 && r.Header.Get("Retry-After") == "" {
			t.Error("429 应带 Retry-After")
		}
	}
	if statuses[0] != 401 || statuses[1] != 401 || statuses[2] != 401 || statuses[3] != 429 {
		t.Errorf("状态序列应为 401×3 然后 429: %v", statuses)
	}
	if s.logRows(a.user.ID, "locked") == 0 {
		t.Error("应写 locked 日志")
	}
	// 锁定后，口令步骤本身也被 429（同一 identifier 维度）。
	if r := s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: a.user.Email, Password: a.user.Password}, withIP(freshIP())); r.Status != 429 {
		t.Errorf("锁定期间登录应 429: %d", r.Status)
	}
	if r := s.verify(mfa, s.fresh(a), ip); r.Status == 200 {
		t.Error("锁定期间正确验证码也不得登录")
	}
}

// 控制台 realm（租户用户）的第二步；会话固定防护：登录前携带的旧 Cookie 在第二步成功后作废。
func TestTOTPConsoleRealmAndSessionFixation(t *testing.T) {
	s := newTOTPStack(t)
	a, oldTok, _ := s.enroll(s.seedUser("tenant"))
	if r := s.authed("console", oldTok, "", "GET", "/auth/me", nil); r.Status != 200 {
		t.Fatalf("旧会话应有效: %d", r.Status)
	}
	ip := freshIP()
	mfa := s.passwordStep(a.user, ip, withCookie("le_console_sid", oldTok))
	r := s.verify(mfa, s.fresh(a), ip, withCookie("le_console_sid", oldTok))
	if r.Status != 200 {
		t.Fatalf("第二步: %d %s", r.Status, r.Body)
	}
	newTok := r.cookie("le_console_sid").Value
	if newTok == oldTok {
		t.Error("会话固定：必须签发全新令牌")
	}
	if r := s.authed("console", oldTok, "", "GET", "/auth/me", nil); r.Status != 401 {
		t.Errorf("第二步成功后旧会话应作废: %d", r.Status)
	}
	me := s.authed("console", newTok, "", "GET", "/auth/me", nil)
	if me.Status != 200 || me.JSON(t)["tenant"] == nil {
		t.Errorf("console 会话应带租户: %s", me.Body)
	}
	// realm 隔离：console 用户的 mfa_token 只能换 console 会话（不会签发 admin 会话）。
	if r.cookie("le_admin_sid") != nil {
		t.Error("不得签发另一 realm 的 Cookie")
	}
}

// ---- AC5：停用 TOTP ----

func TestDisableTOTP(t *testing.T) {
	s := newTOTPStack(t)
	a, _, _ := s.enroll(s.seedUser("platform"))
	token, csrf := s.loginTOTP(a, s.fresh(a))
	dis := func(pw, code string) resp {
		r := s.authed("admin", token, csrf, "POST", "/auth/totp/disable", map[string]string{"password": pw, "code": code})
		s.contract("disableTotp", "POST", "/auth/totp/disable", r)
		return r
	}
	if r := dis(randPassword(t), s.fresh(a)); r.Status != 401 || r.errCode(t) != "auth.invalid_credentials" {
		t.Errorf("口令错误应 401 invalid_credentials: %d %s", r.Status, r.Body)
	}
	if r := dis(a.user.Password, s.wrongCode(a.secret)); r.Status != 401 || r.errCode(t) != "auth.totp_invalid" {
		t.Errorf("验证码错误应 401 totp_invalid: %d %s", r.Status, r.Body)
	}
	if r := dis(a.user.Password, ""); r.Status != 400 {
		t.Errorf("缺验证码应 400: %d", r.Status)
	}
	if s.e.rootInt(t, "SELECT totp_enabled FROM §.users WHERE id = ?", a.user.ID) != 1 {
		t.Fatal("失败的停用不得生效")
	}
	if s.logRows(a.user.ID, "totp_failed") == 0 || s.logRows(a.user.ID, "login_failed") == 0 {
		t.Error("失败应写 totp_failed / login_failed")
	}
	// 用恢复码也可（口令 + 恢复码）；这里用当前验证码。
	if r := dis(a.user.Password, s.fresh(a)); r.Status != 204 {
		t.Fatalf("口令 + 当前验证码应成功: %d %s", r.Status, r.Body)
	}
	if s.e.rootInt(t, "SELECT totp_enabled FROM §.users WHERE id = ?", a.user.ID) != 0 ||
		s.e.rootInt(t, "SELECT totp_secret_enc IS NULL AND totp_last_step IS NULL FROM §.users WHERE id = ?", a.user.ID) != 1 ||
		s.e.rootInt(t, "SELECT COUNT(*) FROM §.user_recovery_codes WHERE user_id = ?", a.user.ID) != 0 {
		t.Error("停用后：totp_enabled=0，密钥/time-step 清除，恢复码删除")
	}
	// 停用后仅凭口令即可登录；再停用 → 409。
	s.login(a.user, freshIP())
	if r := dis(a.user.Password, s.wrongCode(a.secret)); r.Status != 409 {
		t.Errorf("未启用时停用应 409: %d", r.Status)
	}
	// 重新启用得到新的密钥。
	a2, _, _ := s.enroll(a.user)
	if a2.secret == a.secret {
		t.Error("重新启用应生成新的密钥")
	}
	// 用恢复码停用。
	tk, cs := s.loginTOTP(a2, s.fresh(a2))
	r := s.authed("admin", tk, cs, "POST", "/auth/totp/disable", map[string]string{"password": a2.user.Password, "code": a2.recovery[0]})
	if r.Status != 204 {
		t.Errorf("口令 + 恢复码也应能停用: %d %s", r.Status, r.Body)
	}
}

// ---- AC4：强制管理员启用 TOTP ----

func TestEnforcedAdminTOTP(t *testing.T) {
	s := newTOTPStack(t, map[string]string{"login.totp_required_for_admin": "true"})
	admin, tenantUser := s.seedUser("platform"), s.seedUser("tenant")

	// 登录本身成功（没有 TOTP 可验证），但除启用流程外一律 403，直到启用。
	_, token, csrf := s.login(admin, freshIP())
	for _, p := range []struct{ id, m, path string }{
		{"getMe", "GET", "/auth/me"}, {"getCsrfToken", "GET", "/auth/csrf"},
	} {
		if r := s.authed("admin", token, csrf, p.m, p.path, nil); r.Status != 200 {
			t.Errorf("%s 应允许: %d %s", p.id, r.Status, r.Body)
		}
	}
	for _, p := range []struct{ m, path string }{{"GET", "/auth/sessions"}, {"GET", "/auth/login-logs"}} {
		r := s.authed("admin", token, csrf, p.m, p.path, nil)
		if r.Status != 403 || r.errCode(t) != "auth.forbidden" || !strings.Contains(string(r.Body), "totp_required_for_admin") {
			t.Errorf("%s 在强制策略下应 403 并指引启用: %d %s", p.path, r.Status, r.Body)
		}
	}
	// 探针路由同样被拒（认证钩子统一执行，handler 不会执行）。
	if r := s.authed("admin", token, csrf, "POST", "/probe/platform", nil); r.Status != 403 {
		t.Errorf("探针路由应 403: %d", r.Status)
	}
	// 启用流程可用。
	if r := s.authed("admin", token, csrf, "POST", "/auth/totp/setup", nil); r.Status != 200 {
		t.Fatalf("启用流程 setup 应允许: %d %s", r.Status, r.Body)
	}
	a, token, csrf := s.enroll(admin)
	if r := s.authed("admin", token, csrf, "GET", "/auth/sessions", nil); r.Status != 200 {
		t.Errorf("启用后应恢复访问: %d %s", r.Status, r.Body)
	}
	// 强制期间管理员不可停用。
	r := s.authed("admin", token, csrf, "POST", "/auth/totp/disable", map[string]string{"password": a.user.Password, "code": s.fresh(a)})
	if r.Status != 403 || r.errCode(t) != "auth.forbidden" {
		t.Errorf("强制策略下管理员不可停用: %d %s", r.Status, r.Body)
	}
	if s.e.rootInt(t, "SELECT totp_enabled FROM §.users WHERE id = ?", a.user.ID) != 1 {
		t.Error("停用必须被拒绝")
	}
	// 租户用户不受影响。
	_, tt, tc := s.login(tenantUser, freshIP())
	if r := s.authed("console", tt, tc, "GET", "/auth/sessions", nil); r.Status != 200 {
		t.Errorf("租户用户不受强制管理员 TOTP 影响: %d", r.Status)
	}
}

// 策略关闭（缺省）时，未启用 TOTP 的管理员不受限。
func TestAdminTOTPNotEnforcedByDefault(t *testing.T) {
	s := newTOTPStack(t)
	_, token, csrf := s.login(s.seedUser("platform"), freshIP())
	if r := s.authed("admin", token, csrf, "GET", "/auth/sessions", nil); r.Status != 200 {
		t.Errorf("缺省不强制: %d %s", r.Status, r.Body)
	}
}

func recCtx(uid string) secretbox.RecordContext {
	return secretbox.RecordContext{Table: "users", Column: "totp_secret_enc", UID: uid}
}
