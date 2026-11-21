package authit

import (
	"testing"
)

// Story 1.22b：登录节流（IP 维度）与 API Token 的 allowed_cidrs 使用同一个来源 IP——都来自路由器按 server.trusted_proxies 解析的 c.ClientIP()
// （httpx.ClientIPFrom），不存在第二处解析 X-Forwarded-For 的代码。
func TestThrottleAndAllowedCIDRsShareTheSameSourceIP(t *testing.T) {
	settings := map[string]string{"login.throttle_account_max": "1000", "login.throttle_ip_max": "2", "login.throttle_window_s": "60"}

	t.Run("可信反代：XFF 同时决定节流维度与令牌来源", func(t *testing.T) {
		s := newTokStack(t, stackOpts{settings: settings}) // 默认信任 127.0.0.1（测试服务器的对端）
		adm := s.adminSession("platform_admin")
		tk := s.issue(adm, map[string]any{"name": "来源", "scopes": []string{"user:read"}, "allowed_cidrs": []string{"198.51.100.0/24"}})
		s.requireBearer(tk.Token, "GET", "/admin/users", 200, "XFF 在白名单内", withIP("198.51.100.5"))
		s.requireBearer(tk.Token, "GET", "/admin/users", 401, "XFF 不在白名单内", withIP("203.0.113.5"))

		// 同一个 XFF 来源 IP 的失败登录累计到 IP 维度；另一个来源不受影响。
		const bad = "198.51.100.5"
		for i := 0; i < 2; i++ {
			if r := failLogin(s, "admin", "nobody-"+string(rune('a'+i))+"@example.test", bad); r.Status != 401 {
				t.Fatalf("第 %d 次应 401: %d", i+1, r.Status)
			}
		}
		if r := failLogin(s, "admin", "nobody-z@example.test", bad); r.Status != 429 {
			t.Errorf("同一来源 IP 超限应 429: %d %s", r.Status, r.Body)
		}
		if r := failLogin(s, "admin", "nobody-y@example.test", "198.51.100.6"); r.Status != 401 {
			t.Errorf("另一个来源 IP 不受影响: %d", r.Status)
		}
	})

	t.Run("不可信：伪造 XFF 既不能绕过令牌来源限制也不能轮换 IP 躲避节流", func(t *testing.T) {
		s := newTokStack(t, stackOpts{settings: settings, trustedProxies: []string{"10.255.255.1"}}) // 对端 127.0.0.1 不在可信范围
		adm := s.adminSession("platform_admin")
		tk := s.issue(adm, map[string]any{"name": "来源", "scopes": []string{"user:read"}, "allowed_cidrs": []string{"198.51.100.0/24"}})
		s.requireBearer(tk.Token, "GET", "/admin/users", 401, "伪造 XFF 不能满足白名单", withIP("198.51.100.5"))

		// 每次换一个伪造的 XFF：全部计入对端 127.0.0.1 的同一个 IP 维度，第 3 次起 429。
		var last resp
		for i := 0; i < 3; i++ {
			last = failLogin(s, "admin", "spray-"+string(rune('a'+i))+"@example.test", freshIP())
		}
		if last.Status != 429 {
			t.Errorf("轮换伪造 XFF 不应躲过 IP 维度节流: %d %s", last.Status, last.Body)
		}
	})
}
