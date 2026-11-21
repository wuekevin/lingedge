package sysconfigit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// AC1：设置读写——基本/品牌/登录等分组，生效并可读；schema 校验（非法值 400，整批原子）；变更审计。
func TestSettingsPatchAndRead(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	adm.Reauth() // 安全敏感设置键的写入需近期再认证（Story 1.22b）

	r := adm.Do("GET", "/admin/settings", nil)
	w.Contract("getSettings", "GET", "/admin/settings", r)
	m := r.JSON(t)
	if r.Status != 200 || m["login.throttle_window_s"] != float64(900) || m["auth.reauth_window_s"] != float64(300) || m["system.run_mode"] != "self_hosted" ||
		m["login.totp_required_for_admin"] != false || m["brand.name"] != "凌界云" {
		t.Fatalf("未设置时应返回默认值（UD-29 再认证窗口 300 秒）: %d %s", r.Status, r.Body)
	}
	if g := adm.Do("GET", "/admin/settings?group=login", nil); len(g.JSON(t)) != 4 {
		t.Fatalf("group=login 应恰含 4 个键: %s", g.Body)
	}
	if g := adm.Do("GET", "/admin/settings?group=nope", nil); g.Status != 400 {
		t.Fatalf("未知分组应 400: %d", g.Status)
	}

	r = patch(adm, map[string]any{"login.throttle_window_s": 600, "login.totp_required_for_admin": true, "brand.name": "新品牌", "general.registration_open": true,
		"general.console_url": "https://console.example.test", "auth.reauth_window_s": 120})
	w.Contract("patchSettings", "PATCH", "/admin/settings", r)
	if r.Status != 200 {
		t.Fatalf("合法批量更新应 200: %d %s", r.Status, r.Body)
	}
	m = r.JSON(t)
	if m["login.throttle_window_s"] != float64(600) || m["login.totp_required_for_admin"] != true || m["brand.name"] != "新品牌" || m["auth.reauth_window_s"] != float64(120) {
		t.Fatalf("响应应是更新后的完整设置: %s", r.Body)
	}
	if got := w.RootStr("SELECT CAST(value_json AS CHAR) FROM §.system_settings WHERE setting_key = 'login.throttle_window_s'"); got != "600" {
		t.Errorf("应落库为 JSON 数字: %s", got)
	}
	// 一个事务一条审计，摘要含 from/to。
	if n := auditCount(w, "action = 'setting.update' AND actor_user_id = ?", adm.U.ID); n != 1 {
		t.Fatalf("应恰有一条 setting.update 审计: %d", n)
	}
	if got := w.RootStr("SELECT CAST(JSON_EXTRACT(changes_json, '$.settings.\"login.throttle_window_s\".to') AS CHAR) FROM §.audit_logs WHERE action = 'setting.update'"); got != "600" {
		t.Errorf("审计摘要应含新值: %s", got)
	}
	// 与认证模块对齐：经 PATCH 写入的值被认证模块读取（15 秒缓存内生效）——这里用独立断言：已登记键的读取范围一致由单元测试保证。
	if g := adm.Do("GET", "/admin/settings?group=login", nil); g.JSON(t)["login.throttle_window_s"] != float64(600) {
		t.Fatalf("写入后应可读: %s", g.Body)
	}

	// 非法值：整批 400，库不变，无审计。
	before := auditCount(w, "action = 'setting.update'")
	bad := []map[string]any{
		{"login.throttle_window_s": "600"}, {"login.throttle_window_s": 0}, {"login.throttle_window_s": 1.5}, {"login.throttle_window_s": 99999999},
		{"login.totp_required_for_admin": "yes"}, {"brand.primary_color": "red"}, {"brand.name": ""}, {"brand.support_email": "bad"},
		{"general.console_url": "ftp://x.example.test"}, {"general.locales": []string{"fr"}}, {"no.such.key": 1},
		{"auth.session_idle_ttl_s": 86400, "auth.session_absolute_ttl_s": 3600},
		{"auth.api_token_default_ttl_s": 31536000 * 2, "auth.api_token_max_ttl_s": 86400 * 10},
		{"system.run_mode": "commercial"}, {"ssrf.allow_cidrs": []any{}},
	}
	for _, v := range bad {
		r := patch(adm, v)
		if r.Status != 400 || r.ErrCode(t) != "validation.failed" {
			t.Errorf("%v 应 400 validation.failed: %d %s", v, r.Status, r.Body)
		}
		w.Contract("patchSettings", "PATCH", "/admin/settings", r)
	}
	if r := adm.Do("PATCH", "/admin/settings", map[string]any{}); r.Status != 400 {
		t.Errorf("缺 values 应 400: %d", r.Status)
	}
	if r := adm.Do("PATCH", "/admin/settings", map[string]any{"values": map[string]any{}}); r.Status != 400 {
		t.Errorf("空 values 应 400: %d", r.Status)
	}
	if r := adm.Do("PATCH", "/admin/settings", map[string]any{"values": map[string]any{"brand.name": "x"}, "junk": 1}); r.Status != 400 {
		t.Errorf("未知顶层字段应 400: %d", r.Status)
	}
	// 混合批：一个非法键让整批拒绝，合法键也不写。
	if r := patch(adm, map[string]any{"brand.name_en": "Mixed", "brand.primary_color": "nope"}); r.Status != 400 || settingRow(w, "brand.name_en") != 0 {
		t.Errorf("整批应原子: %d", r.Status)
	}
	if auditCount(w, "action = 'setting.update'") != before || w.RootStr("SELECT CAST(value_json AS CHAR) FROM §.system_settings WHERE setting_key = 'login.throttle_window_s'") != "600" {
		t.Error("被拒绝的批次不得改库或写审计")
	}
	// run_mode 经通用 PATCH 被拒绝并指向专用端点（否则绕过再认证）。
	if r := patch(adm, map[string]any{"system.run_mode": "commercial"}); !strings.Contains(string(r.Body), "run-mode") || settingRow(w, "system.run_mode") != 0 {
		t.Errorf("run_mode 不得经通用 PATCH 修改: %s", r.Body)
	}

	// null = 恢复默认（删除行）。
	if r := patch(adm, map[string]any{"login.throttle_window_s": nil}); r.Status != 200 || r.JSON(t)["login.throttle_window_s"] != float64(900) || settingRow(w, "login.throttle_window_s") != 0 {
		t.Errorf("null 应恢复默认: %d %s", r.Status, r.Body)
	}

	// 权限：无 setting:write 403；只读可读不可写。
	ro := w.SeedUser("platform", nil)
	w.GrantPlatformRole(ro, w.CustomRole("platform", "setting:read"))
	rs := w.Login(ro)
	if r := rs.Do("GET", "/admin/settings", nil); r.Status != 200 {
		t.Errorf("只读可读: %d", r.Status)
	}
	if r := patch(rs, map[string]any{"brand.name": "x"}); r.Status != 403 {
		t.Errorf("只读不可写: %d %s", r.Status, r.Body)
	}
	if hits := findSecrets(w.Logs.String(), w.Secrets()); len(hits) > 0 {
		t.Errorf("日志含明文敏感值（G-5）: %v", hits)
	}
}

// 敏感键（mail.smtp_password）：信封加密落 value_enc（AAD = system_settings|value_enc|uid），库内无明文；读取脱敏；日志/审计/响应无明文；清除。
func TestSensitiveSettingEncryptedAtRestAndRedacted(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	adm.Reauth() // 安全敏感设置键的写入需近期再认证（Story 1.22b）
	secret := apistack.RandPassword(t)
	w.Secret(secret)

	r := patch(adm, map[string]any{"mail.smtp_password": secret, "mail.smtp_host": "smtp.example.test"})
	w.Contract("patchSettings", "PATCH", "/admin/settings", r)
	if r.Status != 200 {
		t.Fatalf("设置敏感键: %d %s", r.Status, r.Body)
	}
	if got, _ := r.JSON(t)["mail.smtp_password"].(map[string]any); got["configured"] != true || len(got) != 1 {
		t.Fatalf("响应必须脱敏为 {configured:true}: %s", r.Body)
	}
	if strings.Contains(string(r.Body), secret) {
		t.Fatal("响应不得含明文")
	}
	// 落库：value_enc 为信封格式且非空、value_json 为 NULL；整张表 + 审计 + 日志都扫不到明文（库扫描）。
	enc := w.RootStr("SELECT value_enc FROM §.system_settings WHERE setting_key = 'mail.smtp_password'")
	if !strings.HasPrefix(enc, "v1:") || strings.Count(enc, ":") != 4 || strings.Contains(enc, secret) {
		t.Fatalf("应为 5 段信封密文: %q", enc)
	}
	if w.RootInt("SELECT COUNT(*) FROM §.system_settings WHERE setting_key = 'mail.smtp_password' AND value_json IS NULL AND value_enc IS NOT NULL") != 1 {
		t.Fatal("敏感键只能存 value_enc")
	}
	for _, q := range []string{
		"SELECT COALESCE(GROUP_CONCAT(CONCAT_WS('|', uid, setting_key, CAST(value_json AS CHAR), value_enc, group_name)), '') FROM §.system_settings",
		"SELECT COALESCE(GROUP_CONCAT(CONCAT_WS('|', action, resource_type, CAST(changes_json AS CHAR), CAST(meta_json AS CHAR))), '') FROM §.audit_logs",
	} {
		if dump := w.RootStr(q); strings.Contains(dump, secret) {
			t.Fatalf("库扫描发现明文敏感值: %.80s", dump)
		}
	}
	// 审计摘要只记「已变更」。
	if got := w.RootStr("SELECT CAST(changes_json AS CHAR) FROM §.audit_logs WHERE action = 'setting.update'"); !strings.Contains(got, "mail.smtp_password") || strings.Contains(got, secret) {
		t.Errorf("审计摘要应记录该键已变更但不含取值: %s", got)
	}
	// 读取脱敏（全量与分组）。
	for _, path := range []string{"/admin/settings", "/admin/settings?group=mail"} {
		g := adm.Do("GET", path, nil)
		if strings.Contains(string(g.Body), secret) || nested(t, g.JSON(t), "mail.smtp_password", "configured") != true {
			t.Fatalf("%s 读取必须脱敏: %s", path, g.Body)
		}
	}
	// 内部消费者可解密；AAD 绑定表|列|uid：把密文挪到别的行（改 uid）即解密失败。
	svc := w.Sys.Service()
	got, ok, err := svc.OpenSecret(context.Background(), sysconfig.KeyMailCredential)
	if err != nil || !ok || got != secret {
		t.Fatalf("OpenSecret: %v %v", ok, err)
	}
	if _, _, err := svc.OpenSecret(context.Background(), sysconfig.KeyBrandName); err == nil {
		t.Error("非敏感键不得经 OpenSecret 读取")
	}
	origUID := w.RootStr("SELECT uid FROM §.system_settings WHERE setting_key = 'mail.smtp_password'")
	w.Root("UPDATE §.system_settings SET uid = ? WHERE setting_key = 'mail.smtp_password'", apistack.IDs.MustNew())
	if _, _, err := svc.OpenSecret(context.Background(), sysconfig.KeyMailCredential); err == nil {
		t.Error("密文被挪到另一 uid 时解密必须失败（AAD 绑定）")
	}
	w.Root("UPDATE §.system_settings SET uid = ? WHERE setting_key = 'mail.smtp_password'", origUID)

	// 更换：新密文（随机 nonce），uid 不变；非字符串/空串 400。
	secret2 := apistack.RandPassword(t)
	w.Secret(secret2)
	if r := patch(adm, map[string]any{"mail.smtp_password": secret2}); r.Status != 200 {
		t.Fatal(r.Status)
	}
	if enc2 := w.RootStr("SELECT value_enc FROM §.system_settings WHERE setting_key = 'mail.smtp_password'"); enc2 == enc {
		t.Error("更换后密文应变化")
	}
	if w.RootStr("SELECT uid FROM §.system_settings WHERE setting_key = 'mail.smtp_password'") != origUID {
		t.Error("更换不应改变行 uid")
	}
	for _, v := range []any{"", 12345, true, []string{"a"}} {
		if r := patch(adm, map[string]any{"mail.smtp_password": v}); r.Status != 400 {
			t.Errorf("%v 应 400: %d", v, r.Status)
		}
	}
	// 清除。
	r = patch(adm, map[string]any{"mail.smtp_password": nil})
	if nested(t, r.JSON(t), "mail.smtp_password", "configured") != false || settingRow(w, "mail.smtp_password") != 0 {
		t.Fatalf("null 应清除: %s", r.Body)
	}
	if hits := findSecrets(w.Logs.String(), w.Secrets()); len(hits) > 0 {
		t.Errorf("日志含明文敏感值（G-5）: %v", hits)
	}
}

// 设置变更与审计同一事务：审计写入失败 = 变更整体回滚（500，不泄漏内部原因）；清除故障后重试成功且恰好一条审计。
func TestSettingChangeAndAuditShareOneTransaction(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	adm.Reauth()
	type tc struct {
		name, action string
		do           func() apistack.Resp
		unchanged    func() bool
		applied      func() bool
	}
	cases := []tc{
		{"patchSettings", "setting.update", func() apistack.Resp {
			return patch(adm, map[string]any{"brand.name": "原子A", "login.throttle_ip_max": 77})
		},
			func() bool { return settingRow(w, "brand.name") == 0 && settingRow(w, "login.throttle_ip_max") == 0 },
			func() bool { return settingRow(w, "brand.name") == 1 && settingRow(w, "login.throttle_ip_max") == 1 }},
		{"putBrandSettings", "setting.brand.update", func() apistack.Resp {
			return adm.Do("PUT", "/admin/settings/brand", map[string]any{"name": "原子B", "name_en": "AtomicB", "primary_color": "#123456"})
		}, func() bool { return settingRow(w, "brand.primary_color") == 0 }, func() bool { return settingRow(w, "brand.primary_color") == 1 }},
		{"putRunMode", "system.run_mode.update", func() apistack.Resp {
			return adm.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": "commercial"})
		},
			func() bool { return settingRow(w, "system.run_mode") == 0 }, func() bool { return settingRow(w, "system.run_mode") == 1 }},
		{"putSsrfAllowlist", "setting.ssrf_allowlist.update", func() apistack.Resp {
			return adm.Do("PUT", "/admin/settings/ssrf-allowlist", map[string]any{"entries": []map[string]string{{"cidr": "10.9.0.0/16"}}})
		}, func() bool { return settingRow(w, "ssrf.allow_cidrs") == 0 }, func() bool { return settingRow(w, "ssrf.allow_cidrs") == 1 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			injectAuditFailure(w, c.action)
			defer clearAuditFailure(w)
			before := auditCount(w, "action = ?", c.action)
			r := c.do()
			if r.Status != 500 || r.ErrCode(t) != "internal.error" || contains(string(r.Body), "injected", "45000") {
				t.Fatalf("审计失败应使变更整体失败（500 internal.error，不泄漏原因）: %d %s", r.Status, r.Body)
			}
			if !c.unchanged() {
				t.Fatal("审计失败后设置不得残留（整体回滚）")
			}
			if auditCount(w, "action = ?", c.action) != before {
				t.Fatal("失败的事务不得留下审计行")
			}
			clearAuditFailure(w)
			if r := c.do(); r.Status != 200 {
				t.Fatalf("清除故障后重试应成功: %d %s", r.Status, r.Body)
			}
			if !c.applied() || auditCount(w, "action = ?", c.action) != before+1 {
				t.Fatal("重试成功应生效并恰好一条审计")
			}
		})
	}
	// 回滚后缓存不得残留脏值：运行模式此时已是 commercial（最后一个成功用例），而失败期间读到的一直是旧值。
	if g := adm.Do("GET", "/admin/system/run-mode", nil); g.JSON(t)["run_mode"] != "commercial" {
		t.Errorf("run_mode = %s", g.Body)
	}
}

// 品牌：PUT 整体替换并立即可读（Admin 与官网读同一份）；校验；省略字段恢复默认。
func TestBrandReadWrite(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	g := adm.Do("GET", "/admin/settings/brand", nil)
	w.Contract("getBrandSettings", "GET", "/admin/settings/brand", g)
	if g.Status != 200 || g.JSON(t)["name"] != "凌界云" || g.JSON(t)["name_en"] != "LingEdge" || g.JSON(t)["logo_media_uid"] != nil {
		t.Fatalf("默认品牌: %d %s", g.Status, g.Body)
	}
	logo := apistack.IDs.MustNew()
	in := map[string]any{"name": "示例云", "name_en": "Example Cloud", "logo_media_uid": logo, "primary_color": "#0A1B2C", "support_email": "help@example.test"}
	r := adm.Do("PUT", "/admin/settings/brand", in)
	w.Contract("putBrandSettings", "PUT", "/admin/settings/brand", r)
	if r.Status != 200 || r.JSON(t)["name"] != "示例云" || r.JSON(t)["logo_media_uid"] != logo || r.JSON(t)["favicon_media_uid"] != nil {
		t.Fatalf("PUT: %d %s", r.Status, r.Body)
	}
	// 官网读到同一份（立即生效，无需等缓存）。
	si := w.Do("GET", "/public/site-info", nil)
	if nested(t, si.JSON(t), "brand", "name") != "示例云" || nested(t, si.JSON(t), "brand", "primary_color") != "#0A1B2C" {
		t.Fatalf("官网应读到同一份品牌: %s", si.Body)
	}
	// 经 /admin/settings 也可读同一份。
	if gs := adm.Do("GET", "/admin/settings?group=brand", nil); gs.JSON(t)["brand.name"] != "示例云" {
		t.Fatalf("设置读取: %s", gs.Body)
	}
	// 校验。
	for _, bad := range []map[string]any{{"name": ""}, {"name": "x", "primary_color": "blue"}, {"name": "x", "logo_media_uid": "nope"}, {"name": "x", "support_email": "bad"},
		{"name": "x", "unknown": 1}, {"name": 5}} {
		if r := adm.Do("PUT", "/admin/settings/brand", bad); r.Status != 400 {
			t.Errorf("%v 应 400: %d %s", bad, r.Status, r.Body)
		}
	}
	// 省略字段 = 恢复默认（PUT 是整体替换）。
	r = adm.Do("PUT", "/admin/settings/brand", map[string]any{"name": "仅名称"})
	if r.Status != 200 || r.JSON(t)["name_en"] != "LingEdge" || r.JSON(t)["logo_media_uid"] != nil || r.JSON(t)["primary_color"] != "#2563EB" {
		t.Fatalf("省略字段应恢复默认: %s", r.Body)
	}
	if got := auditCount(w, "action = 'setting.brand.update'"); got != 2 {
		t.Errorf("每次 PUT 一条审计: %d", got)
	}
	// 平台主体之外不可写。
	tn := w.SeedTenant("brand")
	tu := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(tu, "tenant_owner")
	if r := w.Login(tu).Do("PUT", "/admin/settings/brand", in); r.Status != 401 {
		t.Errorf("租户不得写品牌: %d", r.Status)
	}
}

// SSRF 白名单：需再认证、校验 CIDR、写审计。
func TestSSRFAllowlist(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	g := adm.Do("GET", "/admin/settings/ssrf-allowlist", nil)
	w.Contract("getSsrfAllowlist", "GET", "/admin/settings/ssrf-allowlist", g)
	if g.Status != 200 || len(g.JSON(t)["entries"].([]any)) != 0 {
		t.Fatalf("默认应为空白名单: %s", g.Body)
	}
	body := map[string]any{"entries": []map[string]string{{"cidr": "10.1.2.3/16", "note": "内网"}}}
	if r := adm.Do("PUT", "/admin/settings/ssrf-allowlist", body); r.Status != 403 || r.ErrCode(t) != "auth.reauth_required" {
		t.Fatalf("改 SSRF 白名单须再认证: %d %s", r.Status, r.Body)
	}
	adm.Reauth()
	for _, bad := range []any{map[string]any{"entries": []map[string]string{{"cidr": "0.0.0.0/0"}}}, map[string]any{"entries": []map[string]string{{"cidr": "bad"}}}, map[string]any{}} {
		if r := adm.Do("PUT", "/admin/settings/ssrf-allowlist", bad); r.Status != 400 {
			t.Errorf("%v 应 400: %d", bad, r.Status)
		}
	}
	r := adm.Do("PUT", "/admin/settings/ssrf-allowlist", body)
	w.Contract("putSsrfAllowlist", "PUT", "/admin/settings/ssrf-allowlist", r)
	if r.Status != 200 || nested(t, map[string]any{"e": r.JSON(t)["entries"].([]any)[0]}, "e", "cidr") != "10.1.0.0/16" || r.JSON(t)["updated_by"] != adm.U.UID {
		t.Fatalf("PUT: %d %s", r.Status, r.Body)
	}
	if auditCount(w, "action = 'setting.ssrf_allowlist.update'") != 1 {
		t.Error("应写一条审计")
	}
	if p := patch(adm, map[string]any{"ssrf.allow_cidrs": []any{}}); p.Status != 400 {
		t.Errorf("通用 PATCH 不得改 SSRF 白名单（绕过再认证）: %d", p.Status)
	}
}

// 多实例语义（诚实声明）：本实例写入立即失效缓存；其他实例（这里用带外写库模拟）最迟 CacheTTL 内读到。
func TestSnapshotCacheTTLForOutOfBandChange(t *testing.T) {
	w := apistack.New(t, apistack.Options{SysCacheTTL: 400 * time.Millisecond})
	if pv := w.Do("GET", "/public/version", nil); pv.JSON(t)["run_mode"] != "self_hosted" {
		t.Fatal(pv.Body)
	}
	w.Root("INSERT INTO §.system_settings (uid, setting_key, value_json, group_name) VALUES (?, 'system.run_mode', CAST('\"commercial\"' AS JSON), 'general')", apistack.IDs.MustNew())
	deadline := time.Now().Add(5 * time.Second)
	for {
		if pv := w.Do("GET", "/public/version", nil); pv.JSON(t)["run_mode"] == "commercial" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("带外变更应在缓存 TTL 内被读到")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
