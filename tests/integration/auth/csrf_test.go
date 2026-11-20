package authit

import (
	"strings"
	"testing"
)

// AC3：Cookie 会话的写请求必须带正确的 X-CSRF-Token，且 Origin/Sec-Fetch-Site 不得指向别的源；Bearer 请求不适用 CSRF。
func TestCSRF(t *testing.T) {
	s := newStack(t)
	u := s.seedUser("platform")
	origin := s.srv.URL // http://127.0.0.1:port，与请求 Host 同源

	// 每个子用例用独立会话（成功的 logout 会吊销会话）。
	newSession := func() (string, string) { return tokenAndCSRF(t, s, u) }
	post := func(tok, csrf string, opts ...reqOpt) resp {
		return s.authed("admin", tok, csrf, "POST", "/auth/logout", nil, opts...)
	}
	expect403 := func(name string, r resp) {
		t.Helper()
		if r.Status != 403 || r.errCode(t) != "auth.csrf_invalid" {
			t.Errorf("%s: 期望 403 auth.csrf_invalid，实际 %d %s", name, r.Status, r.Body)
		}
		s.contract("logout", "POST", "/auth/logout", r)
	}

	tok, csrf := newSession()
	expect403("缺失 X-CSRF-Token", post(tok, ""))
	expect403("错误的 X-CSRF-Token", post(tok, "not-the-token"))
	expect403("他人会话的 CSRF 令牌", func() resp { _, other := newSession(); return post(tok, other) }())
	expect403("Origin 不符", post(tok, csrf, withHeader("Origin", "https://evil.example")))
	expect403("Origin: null", post(tok, csrf, withHeader("Origin", "null")))
	expect403("Sec-Fetch-Site=cross-site", post(tok, csrf, withHeader("Sec-Fetch-Site", "cross-site")))
	expect403("Sec-Fetch-Site=same-site", post(tok, csrf, withHeader("Sec-Fetch-Site", "same-site")))
	// 所有被拒的请求都没有副作用：会话仍有效。
	if r := s.authed("admin", tok, "", "GET", "/auth/me", nil); r.Status != 200 {
		t.Fatalf("CSRF 拒绝不得吊销会话: %d", r.Status)
	}
	// 读请求（GET）不要求 CSRF。
	if r := s.authed("admin", tok, "", "GET", "/auth/csrf", nil); r.Status != 200 {
		t.Errorf("GET 不需要 CSRF: %d", r.Status)
	}
	// 正确：同源 Origin + Sec-Fetch-Site=same-origin + 正确令牌。
	r := post(tok, csrf, withHeader("Origin", origin), withHeader("Sec-Fetch-Site", "same-origin"))
	if r.Status != 204 {
		t.Fatalf("正确的 CSRF 应 204: %d %s", r.Status, r.Body)
	}
	s.contract("logout", "POST", "/auth/logout", r)
	// 登出后 Cookie 被清除（Max-Age<0），令牌失效。
	if c := r.cookie("le_admin_sid"); c == nil || c.MaxAge >= 0 {
		t.Errorf("登出应清除 Cookie: %+v", c)
	}
	if r := s.authed("admin", tok, "", "GET", "/auth/me", nil); r.Status != 401 {
		t.Errorf("登出后会话应失效: %d", r.Status)
	}

	// GET /auth/csrf 返回的令牌与登录响应一致，且可用于写请求（无 Origin 头的非浏览器客户端也可）。
	tok, csrf = newSession()
	g := s.authed("admin", tok, "", "GET", "/auth/csrf", nil)
	s.contract("getCsrfToken", "GET", "/auth/csrf", g)
	if g.JSON(t)["csrf_token"] != csrf {
		t.Errorf("/auth/csrf 应返回会话的 CSRF 令牌")
	}
	if r := post(tok, g.JSON(t)["csrf_token"].(string)); r.Status != 204 {
		t.Errorf("用 /auth/csrf 的令牌应成功: %d", r.Status)
	}

	// Bearer：不使用 Cookie，不适用 CSRF——请求不会因缺少 CSRF 令牌而得到 403，而是按令牌认证的结果处理。
	// API Token 认证属 Story 1.19：在它交付前携带 Bearer 一律 401（fail-closed），所以这里断言「401 而不是 403」。
	// 即使同时带着有效的会话 Cookie，Bearer 请求也不走 Cookie 路径。
	tok, _ = newSession()
	r = s.do("POST", "/auth/logout", nil, withHeader("Authorization", "Bearer le_abcdefgh_"+strings.Repeat("x", 32)))
	if r.Status != 401 {
		t.Errorf("Bearer 请求应由令牌认证处理（1.19 前一律 401），不得是 CSRF 403: %d %s", r.Status, r.Body)
	}
	r = s.authed("admin", tok, "", "POST", "/auth/logout", nil, withHeader("Authorization", "Bearer le_abcdefgh_"+strings.Repeat("x", 32)))
	if r.Status != 401 {
		t.Errorf("带 Bearer 的请求不得回落到 Cookie 会话: %d", r.Status)
	}
}

// MustChangePassword 的会话：除 getMe/getCsrfToken/logout/changePassword 外的操作一律被拒绝。
func TestMustChangePasswordEnforced(t *testing.T) {
	s := newStack(t)
	u := s.seedUser("platform", withMustChange())
	lr, tok, csrf := s.login(u, freshIP())
	if lr.JSON(t)["status"] != "must_change_password" {
		t.Fatalf("登录响应应为 must_change_password: %s", lr.Body)
	}
	s.contract("login", "POST", "/auth/login", lr)
	for _, p := range []string{"/auth/sessions", "/auth/login-logs"} {
		r := s.authed("admin", tok, csrf, "GET", p, nil)
		if r.Status != 403 || r.errCode(t) != "auth.forbidden" {
			t.Errorf("%s 在 must_change_password 下应 403: %d %s", p, r.Status, r.Body)
		}
	}
	for _, p := range []string{"/auth/me", "/auth/csrf"} {
		if r := s.authed("admin", tok, csrf, "GET", p, nil); r.Status != 200 {
			t.Errorf("%s 应允许: %d", p, r.Status)
		}
	}
	newPW := randPassword(t)
	r := s.authed("admin", tok, csrf, "POST", "/auth/password/change", map[string]string{"current_password": u.Password, "new_password": newPW})
	if r.Status != 204 {
		t.Fatalf("改密应成功: %d %s", r.Status, r.Body)
	}
	if s.e.rootInt(t, "SELECT must_change_password FROM §.users WHERE id = ?", u.ID) != 0 {
		t.Error("改密后应清除 must_change_password")
	}
	u.Password = newPW
	_, tok2, csrf2 := s.login(u, freshIP())
	if r := s.authed("admin", tok2, csrf2, "GET", "/auth/sessions", nil); r.Status != 200 {
		t.Errorf("改密后应恢复正常: %d %s", r.Status, r.Body)
	}
}
