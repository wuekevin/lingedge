package authit

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// AC2：登录成功建立不透明会话（32B CSPRNG、库存 SHA-256），Cookie 属性、响应体、数据库内容与日志。
func TestLoginSuccessSessionAndCookie(t *testing.T) {
	s := newStack(t)
	for _, typ := range []string{"platform", "tenant"} {
		t.Run(typ, func(t *testing.T) {
			u := s.seedUser(typ)
			ip := freshIP()
			r, token, csrf := s.login(u, ip, withHeader("User-Agent", "authit/1.0"))
			realm := realmOf(typ)
			s.contract("login", "POST", "/auth/login", r)

			// Cookie：名称按 realm，HttpOnly + Secure + SameSite=Lax + Path=/，不设 Domain（主机范围）。
			c := r.cookie(cookieName(realm))
			if c == nil {
				t.Fatalf("缺少 Set-Cookie %s: %v", cookieName(realm), r.Header["Set-Cookie"])
			}
			if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || c.MaxAge <= 0 {
				t.Errorf("Cookie 属性不符: %+v", c)
			}
			raw := r.Header.Get("Set-Cookie")
			for _, want := range []string{"HttpOnly", "Secure", "SameSite=Lax"} {
				if !strings.Contains(raw, want) {
					t.Errorf("Set-Cookie 缺少 %s: %s", want, raw)
				}
			}
			// 令牌：32 字节 CSPRNG 的 base64url（43 字符），两次登录不同。
			b, err := base64.RawURLEncoding.DecodeString(token)
			if err != nil || len(b) != 32 {
				t.Fatalf("令牌应是 32 字节的 base64url: len=%d err=%v", len(b), err)
			}
			_, token2, _ := s.login(u, ip)
			if token2 == token {
				t.Error("两次登录得到相同的令牌")
			}
			// 库内只有 SHA-256；明文令牌与 CSRF 令牌不在任何表里。
			sum := sha256.Sum256(b)
			if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.sessions WHERE token_hash = ?", hex.EncodeToString(sum[:])); n != 1 {
				t.Errorf("库内应恰有 1 行 token_hash=SHA-256(令牌)，实际 %d", n)
			}
			if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.sessions WHERE token_hash = ? OR csrf_hash = ?", token, csrf); n != 0 {
				t.Errorf("明文令牌/CSRF 不得入库，实际命中 %d 行", n)
			}
			if got := s.e.rootStr(t, "SELECT realm FROM §.sessions WHERE token_hash = ?", hex.EncodeToString(sum[:])); got != realm {
				t.Errorf("sessions.realm = %q，期望 %q", got, realm)
			}
			if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.sessions WHERE uid IS NULL AND user_id = ?", u.ID); n != 0 {
				t.Errorf("会话必须带 uid（session_ref），实际 %d 行缺失", n)
			}
			// 登录成功更新最近登录，写 login_success 日志（带 request_id、UA、IP）。
			if s.e.rootInt(t, "SELECT COUNT(*) FROM §.users WHERE id = ? AND last_login_at IS NOT NULL AND last_login_ip IS NOT NULL", u.ID) != 1 {
				t.Error("登录成功应更新 last_login_at/last_login_ip")
			}
			if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.login_logs WHERE user_id = ? AND event = 'login_success' AND realm = ? AND request_id IS NOT NULL AND user_agent = 'authit/1.0' AND ip IS NOT NULL", u.ID, realm); n < 1 {
				t.Errorf("缺少 login_success 日志: %d", n)
			}
			// 响应体：CSRF 令牌、用户摘要；不含口令/哈希/令牌。
			body := r.JSON(t)
			if body["status"] != "ok" || csrf == "" {
				t.Errorf("响应体不符: %s", r.Body)
			}
			for _, bad := range []string{u.Password, token, "password_hash", "argon2id"} {
				if strings.Contains(string(r.Body), bad) {
					t.Errorf("登录响应体泄漏 %q", bad)
				}
			}
			// 会话可用于 /auth/me。
			me := s.authed(realm, token, "", "GET", "/auth/me", nil)
			if me.Status != 200 {
				t.Fatalf("/auth/me: %d %s", me.Status, me.Body)
			}
			s.contract("getMe", "GET", "/auth/me", me)
			mb := me.JSON(t)
			if mb["realm"] != realm || mb["csrf_token"] != csrf {
				t.Errorf("/auth/me 的 realm/csrf_token 与登录不一致: %s", me.Body)
			}
			if (typ == "tenant") != (mb["tenant"] != nil) {
				t.Errorf("tenant 字段与 realm 不符: %s", me.Body)
			}
		})
	}
}

// AC2：realm 隔离——Admin realm 仅 platform 用户，Console realm 仅 tenant 用户；令牌不能跨 realm 使用。
func TestRealmIsolation(t *testing.T) {
	s := newStack(t)
	plat, ten := s.seedUser("platform"), s.seedUser("tenant")

	// 登录入口交叉：口令正确也返回 401（与口令错误完全相同的响应）。
	ip := freshIP()
	cross := []struct {
		u     user
		realm string
	}{{plat, "console"}, {ten, "admin"}}
	var bodies []string
	for _, c := range cross {
		r := s.do("POST", "/auth/login", loginReq{Realm: c.realm, Email: c.u.Email, Password: c.u.Password}, withIP(ip))
		if r.Status != 401 || r.errCode(t) != "auth.invalid_credentials" {
			t.Errorf("%s 用户登录 %s realm 应 401 auth.invalid_credentials: %d %s", c.u.Type, c.realm, r.Status, r.Body)
		}
		if len(r.Header.Values("Set-Cookie")) != 0 {
			t.Errorf("失败的登录不得下发 Cookie: %v", r.Header["Set-Cookie"])
		}
		bodies = append(bodies, stripReqID(r.Body))
	}
	if bodies[0] != bodies[1] {
		t.Errorf("两种交叉登录的响应体应一致: %q vs %q", bodies[0], bodies[1])
	}

	// 令牌交叉：admin 会话令牌放进 le_console_sid（反之亦然）→ 401。
	_, adminTok, _ := s.login(plat, ip)
	_, consoleTok, _ := s.login(ten, ip)
	if r := s.authed("console", adminTok, "", "GET", "/auth/me", nil); r.Status != 401 {
		t.Errorf("admin 令牌用于 console Cookie 应 401: %d %s", r.Status, r.Body)
	}
	if r := s.authed("admin", consoleTok, "", "GET", "/auth/me", nil); r.Status != 401 {
		t.Errorf("console 令牌用于 admin Cookie 应 401: %d %s", r.Status, r.Body)
	}
	// 各自的 realm 仍可用。
	if r := s.authed("admin", adminTok, "", "GET", "/auth/me", nil); r.Status != 200 {
		t.Errorf("admin 令牌用于 admin Cookie 应 200: %d", r.Status)
	}
	if r := s.authed("console", consoleTok, "", "GET", "/auth/me", nil); r.Status != 200 {
		t.Errorf("console 令牌用于 console Cookie 应 200: %d", r.Status)
	}

	// 路由 realm 隔离：platform 路由只认 admin 会话（console 会话 → 401，而不是 403）。
	_, ccsrf := tokenAndCSRF(t, s, ten)
	if r := s.authed("console", consoleTok, ccsrf, "POST", "/probe/platform", map[string]string{}); r.Status != 401 {
		t.Errorf("console 会话访问 platform 路由应 401: %d %s", r.Status, r.Body)
	}
}

func tokenAndCSRF(t *testing.T, s *stack, u user) (string, string) {
	t.Helper()
	_, tok, csrf := s.login(u, freshIP())
	return tok, csrf
}

func stripReqID(b []byte) string {
	s := string(b)
	i := strings.Index(s, `"request_id":"`)
	if i < 0 {
		return s
	}
	j := strings.Index(s[i+14:], `"`)
	return s[:i+14] + s[i+14+j:]
}

// 授权钩子未实现（1.18）：认证通过的非自助路由仍被拒绝（403），不会因为「已登录」而放行；自助路由放行。
func TestAuthenticatedButUnauthorizedRoutesStayDenied(t *testing.T) {
	s := newStack(t)
	plat := s.seedUser("platform")
	tok, csrf := tokenAndCSRF(t, s, plat)
	r := s.authed("admin", tok, csrf, "POST", "/probe/platform", map[string]string{})
	if r.Status != 403 || r.errCode(t) != "auth.forbidden" {
		t.Errorf("授权钩子未实现时，已认证的非自助路由应 403 auth.forbidden: %d %s", r.Status, r.Body)
	}
	if strings.Contains(string(r.Body), "reached") {
		t.Error("handler 不应被执行")
	}
	ten := s.seedUser("tenant")
	ttok, tcsrf := tokenAndCSRF(t, s, ten)
	if r := s.authed("console", ttok, tcsrf, "POST", "/probe/tenant", map[string]string{}); r.Status != 403 {
		t.Errorf("租户路由同样应 403: %d %s", r.Status, r.Body)
	}
	// 未认证 → 401。
	if r := s.do("POST", "/probe/platform", map[string]string{}); r.Status != 401 {
		t.Errorf("未认证应 401: %d", r.Status)
	}
}

// AC2：会话固定防护——登录后换新会话；请求携带的旧/攻击者预置的 Cookie 不会被沿用，并在登录成功后作废。
func TestSessionFixation(t *testing.T) {
	s := newStack(t)
	u := s.seedUser("platform")
	ip := freshIP()
	_, old, _ := s.login(u, ip)
	if r := s.authed("admin", old, "", "GET", "/auth/me", nil); r.Status != 200 {
		t.Fatalf("旧会话应可用: %d", r.Status)
	}
	// 携带旧会话 Cookie 再次登录：签发新令牌，旧令牌作废。
	r, fresh, _ := s.login(u, ip, withCookie("le_admin_sid", old))
	if fresh == "" || fresh == old {
		t.Fatalf("登录后必须换新会话（新=%q 旧=%q）", fresh, old)
	}
	if r := s.authed("admin", old, "", "GET", "/auth/me", nil); r.Status != 401 {
		t.Errorf("登录后旧会话应失效: %d", r.Status)
	}
	if r := s.authed("admin", fresh, "", "GET", "/auth/me", nil); r.Status != 200 {
		t.Errorf("新会话应可用: %d", r.Status)
	}
	if got := s.e.rootStr(t, "SELECT revoke_reason FROM §.sessions WHERE user_id = ? AND revoked_at IS NOT NULL ORDER BY id DESC LIMIT 1", u.ID); got != "relogin" {
		t.Errorf("旧会话吊销原因 = %q，期望 relogin", got)
	}
	_ = r

	// 攻击者预置的 Cookie（服务端从未签发过的值）不会被「采纳」：登录响应给出的是服务端新生成的令牌，且预置值不可用。
	planted := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("A", 32)))
	r2, tok2, _ := s.login(u, ip, withCookie("le_admin_sid", planted))
	if tok2 == planted {
		t.Error("服务端不得采纳客户端预置的会话标识")
	}
	if rr := s.authed("admin", planted, "", "GET", "/auth/me", nil); rr.Status != 401 {
		t.Errorf("预置的令牌不得可用: %d", rr.Status)
	}
	_ = r2

	// 失败的登录不改变既有会话。
	bad := s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: u.Email, Password: randPassword(t)}, withIP(ip), withCookie("le_admin_sid", tok2))
	if bad.Status != 401 {
		t.Fatalf("错误口令应 401: %d", bad.Status)
	}
	if r := s.authed("admin", tok2, "", "GET", "/auth/me", nil); r.Status != 200 {
		t.Errorf("失败的登录不应使既有会话失效: %d", r.Status)
	}
}

// 账号状态与成员关系：停用账号、无租户成员关系的租户用户、启用了 TOTP（第二步属 1.17）的账号，都不能仅凭口令建立会话。
func TestLoginIneligibleAccounts(t *testing.T) {
	s := newStack(t)
	cases := []struct {
		name string
		u    user
		code string
	}{
		{"停用账号", s.seedUser("platform", withStatus("disabled")), "auth.invalid_credentials"},
		{"待激活账号", s.seedUser("tenant", withStatus("pending")), "auth.invalid_credentials"},
		{"租户用户无成员关系", s.seedUser("tenant", withNoMembership()), "auth.invalid_credentials"},
		{"启用 TOTP（1.17 前 fail-closed）", s.seedUser("platform", withTOTP()), "auth.totp_required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := s.do("POST", "/auth/login", loginReq{Realm: realmOf(c.u.Type), Email: c.u.Email, Password: c.u.Password}, withIP(freshIP()))
			if r.Status != 401 || r.errCode(t) != c.code {
				t.Errorf("期望 401 %s: %d %s", c.code, r.Status, r.Body)
			}
			if len(r.Header.Values("Set-Cookie")) != 0 {
				t.Error("不得下发 Cookie")
			}
			s.contract("login", "POST", "/auth/login", r)
			if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.sessions WHERE user_id = ?", c.u.ID); n != 0 {
				t.Errorf("不得创建会话，实际 %d", n)
			}
		})
	}
}

// 入参校验：未知字段、缺字段、非法 realm、超长口令 → 400；不泄漏口令。
func TestLoginValidation(t *testing.T) {
	s := newStack(t)
	ip := freshIP()
	for name, body := range map[string]any{
		"非法 realm": loginReq{Realm: "root", Email: "a@b.c", Password: shortPW},
		"缺邮箱":      loginReq{Realm: "admin", Password: shortPW},
		"非邮箱":      loginReq{Realm: "admin", Email: "not-an-email", Password: shortPW},
		"空口令":      loginReq{Realm: "admin", Email: "a@b.c"},
		"未知字段":     map[string]string{"realm": "admin", "email": "a@b.c", fieldPW: shortPW, "extra": "1"},
		"超长口令":     loginReq{Realm: "admin", Email: "a@b.c", Password: strings.Repeat("x", 2000)},
	} {
		r := s.do("POST", "/auth/login", body, withIP(ip))
		if r.Status != 400 || r.errCode(t) != "validation.failed" {
			t.Errorf("%s: 期望 400 validation.failed，实际 %d %s", name, r.Status, r.Body)
		}
		s.contract("login", "POST", "/auth/login", r)
	}
}
