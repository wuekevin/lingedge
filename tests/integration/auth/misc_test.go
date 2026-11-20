package authit

import (
	"context"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// AC6（NFR-12）：口令、会话令牌、CSRF 令牌不入任何日志、不入 Redis、不入库（库里只有哈希）。
// 在真实 MySQL/Redis 上跑完整的登录/失败/锁定/CSRF 拒绝/改密/登出流程，然后扫描：真实访问日志与应用日志（debug 级）、Redis 全部键与值、
// 数据库里认证相关表的全部文本列。
func TestLogsContainNoSecrets(t *testing.T) {
	s := newStack(t, stackOpts{settings: map[string]string{"login.throttle_account_max": "3", "login.throttle_ip_max": "1000"}})
	u, tu := s.seedUser("platform"), s.seedUser("tenant")
	var secrets []string
	secrets = append(secrets, u.Password, tu.Password)

	ip := freshIP()
	for i := 0; i < 4; i++ { // 失败 + 锁定
		pw := randPassword(t)
		secrets = append(secrets, pw)
		s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: u.Email, Password: pw}, withIP(ip))
	}
	_, tok, csrf := s.login(tu, ip)
	secrets = append(secrets, tok, csrf)
	s.authed("console", tok, "", "POST", "/auth/logout", nil)            // CSRF 缺失 → 403
	s.authed("console", tok, "wrong-"+csrf, "POST", "/auth/logout", nil) // CSRF 错误
	newPW := randPassword(t)
	badCur := randPassword(t)
	secrets = append(secrets, newPW, badCur)
	s.authed("console", tok, csrf, "POST", "/auth/password/change", map[string]string{"current_password": badCur, "new_password": newPW})
	s.authed("console", tok, csrf, "POST", "/auth/password/change", map[string]string{"current_password": tu.Password, "new_password": newPW})
	_, tok2, csrf2 := s.login(user{ID: tu.ID, Email: tu.Email, Password: newPW, Type: "tenant"}, ip)
	secrets = append(secrets, tok2, csrf2)
	s.authed("console", tok2, csrf2, "POST", "/auth/logout", nil)
	s.mod.Flush()

	logs := s.logs.String()
	if len(logs) < 500 || !strings.Contains(logs, "http_request") {
		t.Fatalf("应有真实的访问日志可供扫描（%d 字节）", len(logs))
	}
	for _, sec := range secrets {
		if strings.Contains(logs, sec) {
			t.Errorf("日志泄露了秘密值 %q…", sec[:8])
		}
	}
	// 日志里没有 Cookie/CSRF/Authorization 头与请求体的痕迹（访问日志只记路径，不记头与体）。
	for _, bad := range []string{"le_admin_sid=", "le_console_sid=", "X-CSRF-Token", "Set-Cookie", "current_password", "new_password"} {
		if strings.Contains(logs, bad) {
			t.Errorf("日志不应出现 %q", bad)
		}
	}

	// Redis：键与值。
	rdb := goredis.NewClient(&goredis.Options{Addr: s.redis.Addr()})
	defer func() { _ = rdb.Close() }()
	ctx := context.Background()
	keys, err := rdb.Keys(ctx, "*").Result()
	if err != nil || len(keys) == 0 {
		t.Fatalf("Redis 里应有节流键: %v %v", keys, err)
	}
	for _, k := range keys {
		ttl := rdb.PTTL(ctx, k).Val()
		if ttl <= 0 {
			t.Errorf("Redis 键 %s 必须带 TTL（ADR-025）: %v", k, ttl)
		}
		v, _ := rdb.Get(ctx, k).Result()
		dump := k + "=" + v
		for _, sec := range secrets {
			if strings.Contains(dump, sec) {
				t.Errorf("Redis 泄露了秘密值: %s", k)
			}
		}
		if strings.Contains(strings.ToLower(k), strings.ToLower(u.Email)) || strings.Contains(strings.ToLower(k), "@") {
			t.Errorf("Redis 键不应含明文邮箱: %s", k)
		}
	}

	// 数据库：认证相关表的全部文本列里没有任何明文秘密。
	for _, q := range []string{
		"SELECT COUNT(*) FROM §.login_logs WHERE CONCAT_WS('|', identifier, event, user_agent, request_id, reason) LIKE CONCAT('%', ?, '%')",
		"SELECT COUNT(*) FROM §.sessions WHERE CONCAT_WS('|', uid, token_hash, csrf_hash, user_agent, revoke_reason) LIKE CONCAT('%', ?, '%')",
		"SELECT COUNT(*) FROM §.users WHERE CONCAT_WS('|', email, display_name, password_hash) LIKE CONCAT('%', ?, '%')",
	} {
		for _, sec := range secrets {
			if n := s.e.rootInt(t, q, sec); n != 0 {
				t.Errorf("数据库泄露明文秘密（%d 行）: %s", n, strings.Fields(q)[3])
			}
		}
	}
}

// 路由清单：认证模块经 httpx.Router.Register 注册的 13 条路由（1.16 的 8 条 + 1.17 的 TOTP/再认证 5 条），元数据与 openapi.yaml 的 x-* 完全一致；全部是真实 handler（不是 501 占位）。
func TestRouteMetadataMatchesOpenAPI(t *testing.T) {
	s := newStack(t)
	var routes []httpx.RouteMeta
	for _, m := range s.router.Routes() {
		if strings.HasPrefix(m.Path, "/auth/") {
			routes = append(routes, m)
		}
	}
	if len(routes) != 18 { // 13 条登录/会话/TOTP 等 + 5 条 API Token（Story 1.19）
		t.Fatalf("应注册 18 条 /auth 路由，实际 %d", len(routes))
	}
	if probs := s.spec.CheckRoutes(routes); len(probs) > 0 {
		t.Errorf("路由元数据与 openapi.yaml 不一致:\n%s", strings.Join(probs, "\n"))
	}
	// 没有任何一条返回 501。
	for _, p := range []struct{ m, path string }{{"POST", "/auth/login"}, {"GET", "/auth/me"}, {"GET", "/auth/csrf"}, {"GET", "/auth/sessions"}, {"GET", "/auth/login-logs"}} {
		if r := s.do(p.m, p.path, nil); r.Status == 501 {
			t.Errorf("%s %s 不得返回 501", p.m, p.path)
		}
	}
	// 未认证访问受保护的自助路由 → 401 并符合契约。
	for _, c := range []struct{ id, m, path string }{{"getMe", "GET", "/auth/me"}, {"getCsrfToken", "GET", "/auth/csrf"}, {"listSessions", "GET", "/auth/sessions"}, {"listMyLoginLogs", "GET", "/auth/login-logs"}, {"logout", "POST", "/auth/logout"}, {"changePassword", "POST", "/auth/password/change"}, {"revokeSession", "DELETE", "/auth/sessions/" + ids.MustNew()}} {
		r := s.do(c.m, c.path, nil)
		if r.Status != 401 {
			t.Errorf("%s 未认证应 401: %d", c.id, r.Status)
		}
		s.contract(c.id, c.m, c.path, r)
	}
}

// 哈希升级：库里的口令哈希参数与当前配置不同时，登录成功后透明地重新哈希（口令不变、不影响登录结果）。
func TestPasswordRehashOnLogin(t *testing.T) {
	s := newStack(t)
	pw := randPassword(t)
	weak := weakHash(t, pw) // 低于当前参数的哈希
	u := s.seedUser("platform", withHash(weak))
	u.Password = pw
	before := s.e.rootStr(t, "SELECT password_hash FROM §.users WHERE id = ?", u.ID)
	s.login(u, freshIP())
	after := s.e.rootStr(t, "SELECT password_hash FROM §.users WHERE id = ?", u.ID)
	if before == after || !strings.Contains(after, "$m=19456,t=2,p=1$") {
		t.Errorf("登录成功后应升级到当前参数: %q → %q", before, after)
	}
	s.login(u, freshIP()) // 升级后的哈希仍可登录
}

// 损坏/被篡改的口令哈希：按口令错误处理（401），绝不放行，也不 panic；极端参数被拒绝而不是被执行。
func TestCorruptPasswordHashFailsClosed(t *testing.T) {
	s := newStack(t)
	for name, h := range map[string]string{
		"空":      "x",
		"明文":     "plaintext",
		"超大内存参数": "$argon2id$v=19$m=4294967295,t=3,p=4$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"错误算法":   "$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaGhhc2g",
	} {
		u := s.seedUser("platform", withHash(h))
		r := s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: u.Email, Password: u.Password}, withIP(freshIP()))
		if r.Status != 401 || r.errCode(t) != "auth.invalid_credentials" {
			t.Errorf("%s: 损坏的哈希应 401: %d %s", name, r.Status, r.Body)
		}
	}
}
