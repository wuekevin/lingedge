package authit

import (
	"strings"
	"testing"
	"time"
)

// AC5：修改密码使该用户的全部会话失效；旧口令失效、新口令可用；策略与当前口令校验；失败计入节流。
func TestChangePasswordRevokesAllSessions(t *testing.T) {
	s := newStack(t, stackOpts{settings: map[string]string{"login.throttle_account_max": "3", "login.throttle_ip_max": "1000"}})
	u := s.seedUser("platform")
	other := s.seedUser("platform")
	ip := freshIP()
	_, t1, c1 := s.login(u, ip)
	_, t2, _ := s.login(u, ip)
	_, t3, _ := s.login(u, ip)
	_, otherTok, _ := s.login(other, ip)
	change := func(tok, csrf string, body map[string]string) resp {
		return s.authed("admin", tok, csrf, "POST", "/auth/password/change", body)
	}

	// 策略：太短 / 与当前相同 / 等于邮箱 → 400 auth.password_policy；缺少字段 → 400 validation.failed。
	for name, body := range map[string]map[string]string{
		"太短":    {"current_password": u.Password, "new_password": shortPW},
		"与当前相同": {"current_password": u.Password, "new_password": u.Password},
		"等于邮箱":  {"current_password": u.Password, "new_password": u.Email},
	} {
		r := change(t1, c1, body)
		if r.Status != 400 || r.errCode(t) != "auth.password_policy" {
			t.Errorf("%s: 期望 400 auth.password_policy，实际 %d %s", name, r.Status, r.Body)
		}
		s.contract("changePassword", "POST", "/auth/password/change", r)
	}
	if r := change(t1, c1, map[string]string{"current_password": u.Password}); r.Status != 400 || r.errCode(t) != "validation.failed" {
		t.Errorf("缺字段应 400 validation.failed: %d %s", r.Status, r.Body)
	}
	// 当前口令错误 → 401，且不改任何东西；失败计入节流（3 次后 429）。
	newPW := randPassword(t)
	for i := 0; i < 3; i++ {
		r := change(t1, c1, map[string]string{"current_password": randPassword(t), "new_password": newPW})
		if r.Status != 401 || r.errCode(t) != "auth.invalid_credentials" {
			t.Fatalf("当前口令错误应 401: %d %s", r.Status, r.Body)
		}
		s.contract("changePassword", "POST", "/auth/password/change", r)
	}
	r := change(t1, c1, map[string]string{"current_password": u.Password, "new_password": newPW})
	if r.Status != 429 {
		t.Fatalf("改密的失败应计入节流（3 次后 429）: %d %s", r.Status, r.Body)
	}
	s.contract("changePassword", "POST", "/auth/password/change", r)
	if s.authed("admin", t1, "", "GET", "/auth/me", nil).Status != 200 {
		t.Fatal("失败的改密不得使会话失效")
	}
	// 清零节流后正确改密：用另一位用户证明路径（同一账号已被锁定，等窗口太久）。
	u2 := s.seedUser("platform")
	_, a, ac := s.login(u2, freshIP())
	_, b, _ := s.login(u2, freshIP())
	_, c, _ := s.login(u2, freshIP())
	_, oth, _ := s.login(other, freshIP())
	pw2 := randPassword(t)
	r = s.authed("admin", a, ac, "POST", "/auth/password/change", map[string]string{"current_password": u2.Password, "new_password": pw2})
	if r.Status != 204 {
		t.Fatalf("改密应 204: %d %s", r.Status, r.Body)
	}
	s.contract("changePassword", "POST", "/auth/password/change", r)
	for name, tok := range map[string]string{"当前会话": a, "会话 B": b, "会话 C": c} {
		if rr := s.authed("admin", tok, "", "GET", "/auth/me", nil); rr.Status != 401 {
			t.Errorf("改密后 %s 应失效: %d", name, rr.Status)
		}
	}
	if rr := s.authed("admin", oth, "", "GET", "/auth/me", nil); rr.Status != 200 {
		t.Errorf("他人的会话不受影响: %d", rr.Status)
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.sessions WHERE user_id = ? AND revoked_at IS NULL", u2.ID); n != 0 {
		t.Errorf("库里不应再有未吊销会话: %d", n)
	}
	if got := s.e.rootStr(t, "SELECT revoke_reason FROM §.sessions WHERE token_hash IS NOT NULL AND user_id = ? ORDER BY id LIMIT 1", u2.ID); got != "password_change" {
		t.Errorf("吊销原因 = %q", got)
	}
	if c := r.cookie("le_admin_sid"); c == nil || c.MaxAge >= 0 {
		t.Errorf("改密后应清除 Cookie: %+v", c)
	}
	// 旧口令不能登录，新口令可以；库里是 argon2id PHC，不含明文。
	if rr := s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: u2.Email, Password: u2.Password}, withIP(freshIP())); rr.Status != 401 {
		t.Errorf("旧口令不应可登录: %d", rr.Status)
	}
	u2.Password = pw2
	s.login(u2, freshIP())
	hash := s.e.rootStr(t, "SELECT password_hash FROM §.users WHERE id = ?", u2.ID)
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=") || strings.Contains(hash, pw2) {
		t.Errorf("口令应以 argon2id PHC 串存储: %q", hash)
	}
	_, _, _ = t2, t3, otherTok
}

// AC5：会话列表与吊销——只见自己的会话；吊销他人/不存在的会话统一 404；吊销后令牌立即失效。
func TestSessionsListAndRevoke(t *testing.T) {
	s := newStack(t)
	u, other := s.seedUser("platform"), s.seedUser("platform")
	_, t1, c1 := s.login(u, freshIP(), withHeader("User-Agent", "browser-1"))
	_, t2, c2 := s.login(u, freshIP(), withHeader("User-Agent", "browser-2"))
	_, oTok, oCSRF := s.login(other, freshIP())

	r := s.authed("admin", t1, c1, "GET", "/auth/sessions?page_size=10", nil)
	if r.Status != 200 {
		t.Fatalf("listSessions: %d %s", r.Status, r.Body)
	}
	s.contract("listSessions", "GET", "/auth/sessions", r)
	body := r.JSON(t)
	items := body["items"].([]any)
	if len(items) != 2 || body["total"].(float64) != 2 {
		t.Fatalf("应只见自己的 2 个会话: %s", r.Body)
	}
	var cur, notCur map[string]any
	for _, it := range items {
		m := it.(map[string]any)
		if m["current"] == true {
			cur = m
		} else {
			notCur = m
		}
		if m["realm"] != "admin" || len(m["session_ref"].(string)) != 26 {
			t.Errorf("条目不符: %v", m)
		}
	}
	if cur == nil || notCur == nil || cur["user_agent"] != "browser-1" || notCur["user_agent"] != "browser-2" {
		t.Errorf("current 标记不符: %s", r.Body)
	}
	for _, leak := range []string{t1, t2, "token_hash", "csrf_hash"} {
		if strings.Contains(string(r.Body), leak) {
			t.Errorf("会话列表泄漏 %q", leak)
		}
	}
	// 分页。
	r1 := s.authed("admin", t1, c1, "GET", "/auth/sessions?page=2&page_size=1", nil)
	if r1.JSON(t)["page"].(float64) != 2 || len(r1.JSON(t)["items"].([]any)) != 1 {
		t.Errorf("分页不符: %s", r1.Body)
	}
	if rr := s.authed("admin", t1, c1, "GET", "/auth/sessions?page=0", nil); rr.Status != 400 {
		t.Errorf("非法 page 应 400: %d", rr.Status)
	}

	// 他人尝试吊销我的会话：我方的会话引用对他人是 404。
	ref := notCur["session_ref"].(string)
	rr := s.authed("admin", oTok, oCSRF, "DELETE", "/auth/sessions/"+ref, nil)
	if rr.Status != 404 || rr.errCode(t) != "resource.not_found" {
		t.Errorf("吊销他人会话应 404: %d %s", rr.Status, rr.Body)
	}
	s.contract("revokeSession", "DELETE", "/auth/sessions/"+ref, rr)
	if s.authed("admin", t2, "", "GET", "/auth/me", nil).Status != 200 {
		t.Error("他人的 404 请求不得影响会话")
	}
	// 非法引用 → 400；不存在的引用 → 404。
	if rr := s.authed("admin", t1, c1, "DELETE", "/auth/sessions/not-a-uid", nil); rr.Status != 400 {
		t.Errorf("非法引用应 400: %d %s", rr.Status, rr.Body)
	}
	if rr := s.authed("admin", t1, c1, "DELETE", "/auth/sessions/"+ids.MustNew(), nil); rr.Status != 404 {
		t.Errorf("不存在的引用应 404: %d", rr.Status)
	}
	// 缺 CSRF → 403。
	if rr := s.authed("admin", t1, "", "DELETE", "/auth/sessions/"+ref, nil); rr.Status != 403 {
		t.Errorf("DELETE 缺 CSRF 应 403: %d", rr.Status)
	}
	// 吊销另一个会话：立即失效；列表只剩 1 个；再吊销同一个 → 404。
	rr = s.authed("admin", t1, c1, "DELETE", "/auth/sessions/"+ref, nil)
	if rr.Status != 204 {
		t.Fatalf("吊销应 204: %d %s", rr.Status, rr.Body)
	}
	s.contract("revokeSession", "DELETE", "/auth/sessions/"+ref, rr)
	if s.authed("admin", t2, "", "GET", "/auth/me", nil).Status != 401 {
		t.Error("被吊销的会话应立即失效")
	}
	if rr := s.authed("admin", t1, c1, "DELETE", "/auth/sessions/"+ref, nil); rr.Status != 404 {
		t.Errorf("重复吊销应 404: %d", rr.Status)
	}
	_ = c2
	// 吊销当前会话：204 并清除 Cookie。
	r = s.authed("admin", t1, c1, "GET", "/auth/sessions", nil)
	curRef := r.JSON(t)["items"].([]any)[0].(map[string]any)["session_ref"].(string)
	rr = s.authed("admin", t1, c1, "DELETE", "/auth/sessions/"+curRef, nil)
	if rr.Status != 204 || rr.cookie("le_admin_sid") == nil || rr.cookie("le_admin_sid").MaxAge >= 0 {
		t.Errorf("吊销当前会话应 204 并清 Cookie: %d %+v", rr.Status, rr.cookie("le_admin_sid"))
	}
	if s.authed("admin", t1, "", "GET", "/auth/me", nil).Status != 401 {
		t.Error("当前会话吊销后应失效")
	}
}

// AC7：GET /auth/login-logs 只返回本人的登录事件（游标分页）；他人的事件、未关联用户的失败尝试不可见。
func TestMyLoginLogsOnlyOwn(t *testing.T) {
	s := newStack(t, stackOpts{settings: map[string]string{"login.throttle_account_max": "1000", "login.throttle_ip_max": "1000"}})
	u, other := s.seedUser("platform"), s.seedUser("platform")
	for i := 0; i < 3; i++ {
		failLogin(s, "admin", u.Email, freshIP())
	}
	failLogin(s, "admin", other.Email, freshIP())
	failLogin(s, "admin", "stranger@example.test", freshIP())
	_, tok, csrf := s.login(u, freshIP())
	s.login(other, freshIP())

	r := s.authed("admin", tok, csrf, "GET", "/auth/login-logs?limit=3", nil)
	if r.Status != 200 {
		t.Fatalf("listMyLoginLogs: %d %s", r.Status, r.Body)
	}
	s.contract("listMyLoginLogs", "GET", "/auth/login-logs", r)
	b := r.JSON(t)
	items := b["items"].([]any)
	if len(items) != 3 || b["next_cursor"] == nil {
		t.Fatalf("limit=3 应返回 3 条并带 next_cursor: %s", r.Body)
	}
	all := append([]any(nil), items...)
	cursor := b["next_cursor"].(string)
	for cursor != "" {
		rr := s.authed("admin", tok, csrf, "GET", "/auth/login-logs?limit=3&cursor="+cursor, nil)
		s.contract("listMyLoginLogs", "GET", "/auth/login-logs", rr)
		bb := rr.JSON(t)
		all = append(all, bb["items"].([]any)...)
		cursor, _ = bb["next_cursor"].(string)
	}
	// 4 条：3 次 login_failed + 1 次 login_success，全部是本人的；无重复；时间倒序。
	if len(all) != 4 {
		t.Fatalf("本人应有 4 条事件，实际 %d: %v", len(all), all)
	}
	seen := map[string]bool{}
	var failed, okc int
	for _, it := range all {
		m := it.(map[string]any)
		if m["identifier"] != u.Email {
			t.Errorf("出现了他人的事件: %v", m)
		}
		if seen[m["uid"].(string)] {
			t.Errorf("重复条目 %v", m["uid"])
		}
		seen[m["uid"].(string)] = true
		switch m["event"] {
		case "login_failed":
			failed++
		case "login_success":
			okc++
		}
	}
	if failed != 3 || okc != 1 {
		t.Errorf("事件构成不符: failed=%d success=%d", failed, okc)
	}
	for _, bad := range []string{"stranger@example.test", other.Email, u.Password} {
		for _, it := range all {
			if strings.Contains(strings.ToLower(toJSON(t, it)), strings.ToLower(bad)) {
				t.Errorf("登录日志泄漏 %q", bad)
			}
		}
	}
	// 非法游标/limit → 400。
	if rr := s.authed("admin", tok, csrf, "GET", "/auth/login-logs?cursor=@@@", nil); rr.Status != 400 {
		t.Errorf("非法游标应 400: %d", rr.Status)
	}
	if rr := s.authed("admin", tok, csrf, "GET", "/auth/login-logs?limit=0", nil); rr.Status != 400 {
		t.Errorf("非法 limit 应 400: %d", rr.Status)
	}
}

// 滑动 + 绝对过期（system_settings；默认值是 UD-27 的工程假设）：活跃会话被续期，但不超过绝对期限；空闲超过滑动期限失效。
func TestSessionSlidingAndAbsoluteExpiry(t *testing.T) {
	s := newStack(t, stackOpts{fakeClock: true, settings: map[string]string{"auth.session_idle_ttl_s": "7200", "auth.session_absolute_ttl_s": "43200"}})
	u := s.seedUser("platform")
	_, tok, _ := s.login(u, freshIP())
	me := func() int { return s.authed("admin", tok, "", "GET", "/auth/me", nil).Status }

	// 每 100 分钟访问一次（< 空闲期限 2h）：持续有效（滑动），直到绝对期限 12h。
	for i := 1; i <= 7; i++ {
		s.clk.Advance(100 * time.Minute)
		if got := me(); got != 200 {
			t.Fatalf("活跃会话在 %d×100min 处应仍有效（滑动续期）: %d", i, got)
		}
	}
	// 700 分钟 < 720 分钟；再推进到 > 12h：即使刚刚活跃，绝对期限到了也失效。
	s.clk.Advance(30 * time.Minute) // 730 min
	if got := me(); got != 401 {
		t.Errorf("超过绝对期限（12h）应 401: %d", got)
	}

	// 空闲超过滑动期限（2h）即失效。
	_, tok2, _ := s.login(u, freshIP())
	s.clk.Advance(2*time.Hour + time.Minute)
	if got := s.authed("admin", tok2, "", "GET", "/auth/me", nil).Status; got != 401 {
		t.Errorf("空闲超过滑动期限应 401: %d", got)
	}
	// 滑动期限不得长于绝对期限：配置里 idle>absolute 时以 absolute 为准（见 settings.get），登录响应中的会话到期时间受此约束。
	_, tok3, _ := s.login(u, freshIP())
	r := s.authed("admin", tok3, "", "GET", "/auth/me", nil)
	exp, _ := time.Parse(time.RFC3339, r.JSON(t)["session_expires_at"].(string))
	if d := exp.Sub(s.clk.Now()); d > 2*time.Hour+time.Second || d < time.Hour {
		t.Errorf("session_expires_at 应约为 now+2h，实际 +%s", d)
	}
}
