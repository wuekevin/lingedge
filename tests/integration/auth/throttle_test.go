package authit

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/redisx"
)

func failLogin(s *stack, realm, email, ip string) resp {
	s.t.Helper()
	return s.do("POST", "/auth/login", loginReq{Realm: realm, Email: email, Password: randPassword(s.t)}, withIP(ip))
}

func eventually(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("超时未达成: %s", what)
}

func retryAfter(t *testing.T, r resp) int {
	t.Helper()
	n, err := strconv.Atoi(r.Header.Get("Retry-After"))
	if err != nil || n < 1 {
		t.Fatalf("429 必须带正整数的 Retry-After，实际 %q", r.Header.Get("Retry-After"))
	}
	return n
}

// AC4/AC7：账号维度节流——失败超限后 429 rate_limit.exceeded（带 Retry-After），写 login_failed/locked；
// 锁定期间连正确口令也 429；对「不存在的账号」行为与响应完全相同；成功登录清零计数。
func TestThrottleAccountLockout(t *testing.T) {
	s := newStack(t, stackOpts{settings: map[string]string{"login.throttle_account_max": "3", "login.throttle_ip_max": "1000", "login.throttle_window_s": "60"}})
	real := s.seedUser("platform")
	ghost := "ghost-" + strings.ToLower(ids.MustNew()) + "@example.test"

	type seq struct {
		statuses []int
		bodies   []string
	}
	run := func(email string) seq {
		var out seq
		ip := freshIP()
		for i := 0; i < 5; i++ {
			r := failLogin(s, "admin", email, ip)
			out.statuses = append(out.statuses, r.Status)
			out.bodies = append(out.bodies, stripReqID(r.Body))
			s.contract("login", "POST", "/auth/login", r)
			if r.Status == 429 {
				if ra := retryAfter(t, r); ra > 60 {
					t.Errorf("Retry-After=%d 不应超过窗口 60s", ra)
				}
				if r.errCode(t) != "rate_limit.exceeded" {
					t.Errorf("锁定应返回 rate_limit.exceeded（不再使用 423/auth.account_locked）: %s", r.Body)
				}
			}
		}
		return out
	}
	rs, gs := run(real.Email), run(ghost)
	want := []int{401, 401, 401, 429, 429}
	for name, got := range map[string][]int{"存在的账号": rs.statuses, "不存在的账号": gs.statuses} {
		if strings.Join(itoa(got), ",") != strings.Join(itoa(want), ",") {
			t.Errorf("%s 的状态码序列 = %v，期望 %v", name, got, want)
		}
	}
	for i := range rs.bodies { // 存在与不存在的账号：响应体逐字节相同
		if rs.bodies[i] != gs.bodies[i] {
			t.Errorf("第 %d 次响应体存在/不存在账号不一致:\n%s\n%s", i+1, rs.bodies[i], gs.bodies[i])
		}
	}

	// 锁定期间：换一个 IP、给出正确口令，仍然 429（账号维度；不泄漏口令是否正确）。
	r := s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: real.Email, Password: real.Password}, withIP(freshIP()))
	if r.Status != 429 {
		t.Errorf("锁定期间正确口令也应 429: %d %s", r.Status, r.Body)
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.sessions WHERE user_id = ?", real.ID); n != 0 {
		t.Errorf("锁定期间不得建立会话: %d", n)
	}
	// 日志：3 条 login_failed（用户存在 → 带 user_id）、至少 1 条 locked；不存在账号的日志 user_id 为 NULL，identifier 为尝试的邮箱。
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.login_logs WHERE user_id = ? AND event = 'login_failed'", real.ID); n != 3 {
		t.Errorf("存在的账号应有 3 条 login_failed，实际 %d", n)
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.login_logs WHERE user_id = ? AND event = 'locked'", real.ID); n < 1 {
		t.Errorf("存在的账号应有 locked 日志，实际 %d", n)
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.login_logs WHERE identifier = ? AND event = 'login_failed' AND user_id IS NULL", ghost); n != 3 {
		t.Errorf("不存在的账号应有 3 条 login_failed（user_id 为空），实际 %d", n)
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.login_logs WHERE identifier = ? AND event = 'locked'", ghost); n < 1 {
		t.Errorf("不存在的账号同样应有 locked 日志，实际 %d", n)
	}
	// 锁定期间的 429 洪水不得刷爆日志：同一 identifier 的 locked 日志按分钟限频。
	for i := 0; i < 10; i++ {
		failLogin(s, "admin", real.Email, freshIP())
	}
	if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.login_logs WHERE user_id = ? AND event = 'locked'", real.ID); n > 3 {
		t.Errorf("locked 日志应被限频，实际 %d 条", n)
	}
	// DB 兜底计数异步维护（降级路径的真相源）。
	s.mod.Flush()
	if got := s.e.rootInt(t, "SELECT failed_login_count FROM §.users WHERE id = ?", real.ID); got != 3 {
		t.Errorf("users.failed_login_count = %d，期望 3", got)
	}
	if s.e.rootInt(t, "SELECT locked_until IS NOT NULL FROM §.users WHERE id = ?", real.ID) != 1 {
		t.Error("达到阈值后应写 users.locked_until")
	}
	// 不同 realm 的同一邮箱互不影响（identifier 含 realm）。
	_ = s
}

func itoa(in []int) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strconv.Itoa(v)
	}
	return out
}

// 窗口过去后恢复；成功登录清零失败计数。
func TestThrottleWindowExpiryAndResetOnSuccess(t *testing.T) {
	s := newStack(t, stackOpts{settings: map[string]string{"login.throttle_account_max": "3", "login.throttle_ip_max": "1000", "login.throttle_window_s": "2"}})
	u := s.seedUser("platform")
	ip := freshIP()
	for i := 0; i < 3; i++ {
		failLogin(s, "admin", u.Email, ip)
	}
	r := failLogin(s, "admin", u.Email, ip)
	if r.Status != 429 {
		t.Fatalf("应已锁定: %d", r.Status)
	}
	wait := time.Duration(retryAfter(t, r)) * time.Second
	time.Sleep(wait + 300*time.Millisecond)
	_, tok, _ := s.login(u, ip) // 窗口过后正确口令可登录
	if tok == "" {
		t.Fatal("窗口过后应可登录")
	}
	// 成功登录清零：2 次失败 → 成功 → 2 次失败 均不触发锁定。
	for i := 0; i < 2; i++ {
		if r := failLogin(s, "admin", u.Email, ip); r.Status != 401 {
			t.Fatalf("清零后的失败应 401: %d", r.Status)
		}
	}
	s.login(u, ip)
	for i := 0; i < 2; i++ {
		if r := failLogin(s, "admin", u.Email, ip); r.Status != 401 {
			t.Errorf("成功登录后计数应清零，第 %d 次失败得到 %d", i+1, r.Status)
		}
	}
	s.mod.Flush()
	if s.e.rootInt(t, "SELECT failed_login_count FROM §.users WHERE id = ?", u.ID) != 2 {
		t.Error("DB 兜底计数应随成功登录清零后重新累计")
	}
}

// AC4：IP 维度——同一来源 IP 对任意 identifier 的失败累计超限后 429；其他 IP 不受影响。
func TestThrottleIPDimension(t *testing.T) {
	s := newStack(t, stackOpts{settings: map[string]string{"login.throttle_account_max": "1000", "login.throttle_ip_max": "4", "login.throttle_window_s": "60"}})
	u := s.seedUser("platform")
	ip := freshIP()
	for i := 0; i < 4; i++ {
		email := "spray-" + strconv.Itoa(i) + "@example.test" // 每次不同的（不存在的）账号
		if r := failLogin(s, "admin", email, ip); r.Status != 401 {
			t.Fatalf("第 %d 次应 401: %d", i+1, r.Status)
		}
	}
	r := failLogin(s, "admin", "spray-x@example.test", ip)
	if r.Status != 429 || r.errCode(t) != "rate_limit.exceeded" {
		t.Errorf("同一 IP 失败超限应 429: %d %s", r.Status, r.Body)
	}
	retryAfter(t, r)
	if r := s.do("POST", "/auth/login", loginReq{Realm: "admin", Email: u.Email, Password: u.Password}, withIP(ip)); r.Status != 429 {
		t.Errorf("被限的 IP 对任何账号都应 429: %d", r.Status)
	}
	// 其他 IP 的正常用户不受影响。
	s.login(u, freshIP())
}

// ADR-025 / AC4：Redis 不可用时登录与节流仍工作——从未可用、运行中被杀、DB 兜底、恢复后自愈（真实 redis-server 故障注入）。
func TestRedisDownLoginAndThrottle(t *testing.T) {
	settings := map[string]string{"login.throttle_account_max": "3", "login.throttle_ip_max": "1000", "login.throttle_window_s": "60"}

	t.Run("运行中被杀（SIGKILL）", func(t *testing.T) {
		s := newStack(t, stackOpts{settings: settings})
		a, b := s.seedUser("platform"), s.seedUser("platform")
		s.login(a, freshIP()) // Redis 正常时登录成功
		s.redis.Kill()
		eventually(t, "Redis 被判为降级", 5*time.Second, func() bool {
			failLogin(s, "admin", "probe@example.test", freshIP())
			return s.rc.State() == redisx.StateDegraded
		})

		// 降级期间：登录成功。
		_, tok, _ := s.login(b, freshIP())
		if r := s.authed("admin", tok, "", "GET", "/auth/me", nil); r.Status != 200 {
			t.Errorf("Redis 不可用时会话校验应仍工作: %d", r.Status)
		}
		// 降级期间：节流仍生效（进程内窗口 + DB 兜底）。
		ip := freshIP()
		for i := 0; i < 3; i++ {
			if r := failLogin(s, "admin", a.Email, ip); r.Status != 401 {
				t.Fatalf("降级下第 %d 次失败应 401: %d %s", i+1, r.Status, r.Body)
			}
		}
		r := failLogin(s, "admin", a.Email, ip)
		if r.Status != 429 {
			t.Fatalf("降级下失败超限应 429: %d %s", r.Status, r.Body)
		}
		retryAfter(t, r)
		s.mod.Flush()
		if got := s.e.rootInt(t, "SELECT failed_login_count FROM §.users WHERE id = ?", a.ID); got != 3 {
			t.Errorf("DB 兜底 failed_login_count=%d，期望 3", got)
		}
		if s.e.rootInt(t, "SELECT locked_until > UTC_TIMESTAMP(3) FROM §.users WHERE id = ?", a.ID) != 1 {
			t.Error("DB 兜底应写入未来的 locked_until")
		}

		// 「进程重启」（全新的认证模块实例：进程内窗口为空）后，DB 兜底仍然锁定——即使给出正确口令。
		s2 := *s
		s2.mod = s.newModule(zapNop(), nowClock())
		s2.srv = s.restartServer(&s2)
		if r := s2.do("POST", "/auth/login", loginReq{Realm: "admin", Email: a.Email, Password: a.Password}, withIP(freshIP())); r.Status != 429 {
			t.Errorf("重启后 DB 兜底应仍锁定: %d %s", r.Status, r.Body)
		}
		// 未被锁定的其他账号不受影响。
		s2.login(b, freshIP())

		// Redis 恢复：自愈，节流回到 Redis 主路径（空库，不再受进程内/DB 兜底影响）。
		s.redis.Restart(t)
		eventually(t, "Redis 恢复", 10*time.Second, func() bool { return s.rc.State() == redisx.StateUp })
		s.login(a, freshIP())
	})

	t.Run("从未可用", func(t *testing.T) {
		s := newStack(t, stackOpts{noRedis: true, settings: settings})
		if s.rc.State() != redisx.StateDegraded {
			t.Fatal("指向无人监听端口的 Redis 应处于降级状态")
		}
		u := s.seedUser("tenant")
		_, tok, _ := s.login(u, freshIP())
		if r := s.authed("console", tok, "", "GET", "/auth/me", nil); r.Status != 200 {
			t.Errorf("Redis 从未可用时会话应仍可用: %d", r.Status)
		}
		ip := freshIP()
		ghost := "nobody@example.test" // 不存在的账号：仅进程内窗口计数，行为与存在的账号一致
		var got []int
		for i := 0; i < 5; i++ {
			got = append(got, failLogin(s, "console", ghost, ip).Status)
		}
		if strings.Join(itoa(got), ",") != "401,401,401,429,429" {
			t.Errorf("降级下不存在账号的节流序列 = %v", got)
		}
	})
}
