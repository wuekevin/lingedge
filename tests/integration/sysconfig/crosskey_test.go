package sysconfigit

import (
	"sync"
	"testing"

	"github.com/wuekevin/lingedge/tests/apistack"
)

// Story 1.22b（1.22 遗留）：跨键约束（滑动过期 ≤ 绝对过期；Token 默认有效期 ≤ 上限）在**并发 PATCH** 下也不会被「各自合法、合起来违反」突破——
// 校验在写事务内、相关键行加锁（升序）之后以锁内现值进行，后到者看到先到者已提交的值。
func TestCrossKeyConstraintsHoldUnderConcurrentPatches(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	adm.Reauth() // 安全敏感键需要近期再认证（1.22b）

	type pair struct {
		name, lo, hi   string
		loInit, hiInit int
		loNew, hiNew   int // 单独看各自合法（loNew <= hiInit，hiNew >= loInit），合起来 loNew > hiNew
	}
	pairs := []pair{
		{"session", "auth.session_idle_ttl_s", "auth.session_absolute_ttl_s", 3600, 7200, 5400, 4000},
		{"token", "auth.api_token_default_ttl_s", "auth.api_token_max_ttl_s", 86400 * 30, 86400 * 60, 86400 * 50, 86400 * 40},
	}
	set := func(key string, v int) {
		w.Root("INSERT INTO §.system_settings (uid, setting_key, value_json, group_name) VALUES (?, ?, CAST(? AS JSON), 'security') ON DUPLICATE KEY UPDATE value_json = VALUES(value_json)", apistack.IDs.MustNew(), key, v)
	}
	val := func(key string) int64 {
		return w.RootInt("SELECT CAST(value_json AS SIGNED) FROM §.system_settings WHERE setting_key = ?", key)
	}

	const rounds = 12
	for round := 0; round < rounds; round++ {
		for _, p := range pairs {
			set(p.lo, p.loInit)
			set(p.hi, p.hiInit)
		}
		type result struct {
			pair   string
			status int
		}
		var (
			wg  sync.WaitGroup
			mu  sync.Mutex
			out []result
		)
		start := make(chan struct{})
		for _, p := range pairs {
			for _, ch := range []map[string]any{{p.lo: p.loNew}, {p.hi: p.hiNew}} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					r := patch(adm, ch)
					mu.Lock()
					out = append(out, result{p.name, r.Status})
					mu.Unlock()
				}()
			}
		}
		close(start)
		wg.Wait()
		for _, p := range pairs {
			ok, bad := 0, 0
			for _, r := range out {
				if r.pair != p.name {
					continue
				}
				switch r.status {
				case 200:
					ok++
				case 400:
					bad++
				default:
					t.Errorf("轮 %d %s：意外状态 %d", round, p.name, r.status)
				}
			}
			if ok != 1 || bad != 1 {
				t.Errorf("轮 %d %s：两个互相冲突的并发 PATCH 应恰一个成功一个 400，实际成功 %d 拒绝 %d", round, p.name, ok, bad)
			}
			if lo, hi := val(p.lo), val(p.hi); lo > hi {
				t.Fatalf("轮 %d %s：跨键约束被突破：%s=%d > %s=%d", round, p.name, p.lo, lo, p.hi, hi)
			}
		}
	}
}

// Story 1.22b：通用 PATCH 只需 setting:write，但写「安全敏感键」（登录节流/强制 TOTP/会话与 MFA 与再认证窗口/令牌有效期/邮件/保留期/开放注册）
// 须近期再认证（设置服务内强制，复用 1.17 的 ReauthChecker；OpenAPI 的 x-reauth 清单不变）。品牌等非敏感键不需要；API Token 永远不能写敏感键。
func TestSecuritySensitiveSettingKeysRequireRecentReauth(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w) // 刚登录：没有再认证

	sensitive := []map[string]any{
		{"login.totp_required_for_admin": false}, {"login.throttle_window_s": 3600}, {"login.throttle_account_max": 1000}, {"login.throttle_ip_max": 1000},
		{"auth.session_idle_ttl_s": 86400}, {"auth.session_absolute_ttl_s": 86400 * 30}, {"auth.reauth_window_s": 86400}, {"auth.mfa_window_s": 86400},
		{"auth.api_token_default_ttl_s": 86400 * 100}, {"auth.api_token_max_ttl_s": 86400 * 400},
		{"mail.smtp_host": "smtp.attacker.example.test"}, {"mail.smtp_port": 25}, {"mail.smtp_username": "u"}, {"mail.from_address": "x@example.test"},
		{"retention.audit_days": 30}, {"retention.login_logs_days": 30}, {"general.registration_open": true},
		{"login.throttle_window_s": nil},                              // 恢复默认同样是写入
		{"brand.name": "混合批", "login.totp_required_for_admin": false}, // 混合批：含任一敏感键即需要
	}
	before := auditCount(w, "action = 'setting.update'")
	for _, v := range sensitive {
		r := patch(adm, v)
		if r.Status != 403 || r.ErrCode(t) != "auth.reauth_required" {
			t.Errorf("%v：未再认证应 403 auth.reauth_required: %d %s", v, r.Status, r.Body)
		}
		w.Contract("patchSettings", "PATCH", "/admin/settings", r)
	}
	if w.RootInt("SELECT COUNT(*) FROM §.system_settings WHERE group_name <> 'brand'") != 0 || settingRow(w, "brand.name") != 0 || auditCount(w, "action = 'setting.update'") != before {
		t.Fatal("被拒绝的请求不得写入任何设置（含混合批里的非敏感键）也不得写审计")
	}
	// 敏感键的取值校验优先于再认证检查？—— 非法取值仍是 400（校验在先，不依赖再认证状态）。
	if r := patch(adm, map[string]any{"login.throttle_window_s": "x"}); r.Status != 400 {
		t.Errorf("非法取值应 400: %d %s", r.Status, r.Body)
	}
	// 非敏感键不需要再认证。
	if r := patch(adm, map[string]any{"brand.name": "无需再认证", "general.console_url": "https://console.example.test", "general.locales": []string{"en"}}); r.Status != 200 {
		t.Errorf("非敏感键只需 setting:write: %d %s", r.Status, r.Body)
	}

	// 再认证后可写。
	adm.Reauth()
	if r := patch(adm, map[string]any{"login.throttle_window_s": 600, "login.totp_required_for_admin": true}); r.Status != 200 {
		t.Fatalf("再认证后应可写敏感键: %d %s", r.Status, r.Body)
	}
	// 窗口过期（默认 auth.reauth_window_s = 300 秒）：把会话的 reauth_at 倒推 10 分钟，再次被要求再认证。
	w.Root("UPDATE §.sessions SET reauth_at = DATE_SUB(reauth_at, INTERVAL 10 MINUTE) WHERE user_id = ?", adm.U.ID)
	if r := patch(adm, map[string]any{"login.throttle_window_s": 900}); r.Status != 403 || r.ErrCode(t) != "auth.reauth_required" {
		t.Errorf("再认证窗口外应再次要求: %d %s", r.Status, r.Body)
	}
	if got := w.RootStr("SELECT CAST(value_json AS CHAR) FROM §.system_settings WHERE setting_key = 'login.throttle_window_s'"); got != "600" {
		t.Errorf("被拒绝的写入不得生效: %s", got)
	}
	adm.Reauth()
	if r := patch(adm, map[string]any{"login.throttle_window_s": 900}); r.Status != 200 {
		t.Errorf("再次再认证后可写: %d %s", r.Status, r.Body)
	}

	// API Token 没有会话，永远不能满足再认证：令牌（即使有 setting:write）不能写敏感键，但可以写品牌等非敏感键。
	tok := adm.IssueToken("setting-writer", "setting:write", "setting:read")
	pb := func(v map[string]any) apistack.Resp {
		return w.Bearer(tok, "PATCH", "/admin/settings", map[string]any{"values": v})
	}
	if r := pb(map[string]any{"login.totp_required_for_admin": false}); r.Status != 403 || r.ErrCode(t) != "auth.reauth_required" {
		t.Errorf("令牌不能写敏感键: %d %s", r.Status, r.Body)
	}
	if got := w.RootStr("SELECT CAST(value_json AS CHAR) FROM §.system_settings WHERE setting_key = 'login.totp_required_for_admin'"); got != "true" {
		t.Errorf("令牌的被拒绝写入不得生效: %s", got)
	}
	if r := pb(map[string]any{"brand.name": "令牌改品牌"}); r.Status != 200 {
		t.Errorf("令牌可写非敏感键: %d %s", r.Status, r.Body)
	}
}
