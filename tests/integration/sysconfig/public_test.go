package sysconfigit

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/leakguard"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/pkg/version"
	"github.com/wuekevin/lingedge/tests/apistack"
)

func keysOf(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// Story 1.22 AC4：公开接口——无需认证、契约一致（kin-openapi 响应校验）、无敏感信息（字段白名单 + leakguard + 脱敏扫描）、缓存头。
func TestPublicSiteInfoAndVersion(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	adm.Reauth() // 安全敏感设置键的写入需近期再认证（Story 1.22b）
	in := w.SeedInternals()
	// 种入「不得出现在公开响应里」的真实敏感/内部值：SMTP 口令（加密落库）、内部主机/IP、管理员邮箱。
	secret := apistack.RandPassword(t)
	w.Secret(secret)
	if r := patch(adm, map[string]any{"mail.smtp_password": secret, "mail.smtp_host": "smtp.internal.example.test", "mail.smtp_username": "mailer-" + strings.ToLower(apistack.IDs.MustNew()[:10]),
		"general.docs_url": "https://docs.example.test"}); r.Status != 200 {
		t.Fatal(r.Body)
	}

	for _, c := range []struct{ op, path, keys string }{
		{"getPublicSiteInfo", "/public/site-info", "admin_url,brand,console_installed,console_url,docs_url,locales,registration_open,run_mode,status_url"},
		{"getPublicVersion", "/public/version", "api_version,run_mode,version"},
	} {
		r := w.Do("GET", c.path, nil) // 无任何凭据
		w.Contract(c.op, "GET", c.path, r)
		if r.Status != 200 {
			t.Fatalf("%s 匿名应 200: %d %s", c.path, r.Status, r.Body)
		}
		if got := keysOf(r.JSON(t)); got != c.keys {
			t.Errorf("%s 字段集合 %s 应恰为白名单 %s", c.path, got, c.keys)
		}
		if cc := r.Header.Get("Cache-Control"); cc != "public, max-age=30" {
			t.Errorf("%s 缓存头 = %q", c.path, cc)
		}
		if r.Header.Get("X-Request-ID") == "" {
			t.Errorf("%s 应回显 X-Request-ID", c.path)
		}
		// 敏感信息扫描：leakguard（内部标识键、内部取值、IP 字面量、内部错误全文）+ 运行期敏感值/主机路径/库名/邮箱/提交信息。
		body := string(r.Body)
		if v := leakguard.Scan(r.Body, leakguard.Options{Internal: append(in.Strings(), "smtp.internal", "mailer-"), ForbidIPValues: true}); len(v) > 0 {
			t.Errorf("%s 含内部信息: %v", c.path, v)
		}
		for _, bad := range append(w.Secrets(), w.DBName, w.Inst.Host(), "/Users/", "/home/", "/var/", "/etc/lingedge", "mysql", "redis", "@example.test", "x-setup-token", "master", version.Commit+"x") {
			if len(bad) >= 5 && strings.Contains(strings.ToLower(body), strings.ToLower(bad)) {
				t.Errorf("%s 响应含不应公开的内容 %q: %s", c.path, bad, body)
			}
		}
	}

	pv := w.Do("GET", "/public/version", nil).JSON(t)
	if pv["version"] != version.Version || pv["api_version"] != "v1" || pv["run_mode"] != "self_hosted" {
		t.Errorf("公开版本: %v", pv)
	}
	if _, has := pv["commit"]; has {
		t.Error("公开版本不暴露提交哈希")
	}

	// site-info 语义：自用模式注册不开放（即使设置为 true）；商业模式 + 设置开启才开放；console_installed 取自安装记录或 console_url。
	si := w.Do("GET", "/public/site-info", nil).JSON(t)
	if si["run_mode"] != "self_hosted" || si["registration_open"] != false || si["console_installed"] != false || si["docs_url"] != "https://docs.example.test" || si["admin_url"] != nil {
		t.Errorf("site-info 默认: %v", si)
	}
	if r := patch(adm, map[string]any{"general.registration_open": true}); r.Status != 200 {
		t.Fatal(r.Body)
	}
	if si := w.Do("GET", "/public/site-info", nil).JSON(t); si["registration_open"] != false {
		t.Error("自用模式下注册不开放（商业路由 403 feature.disabled），即使设置为 true")
	}
	adm.Reauth()
	if r := adm.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": "commercial"}); r.Status != 200 {
		t.Fatal(r.Body)
	}
	if si := w.Do("GET", "/public/site-info", nil).JSON(t); si["registration_open"] != true || si["run_mode"] != "commercial" {
		t.Errorf("商业模式 + 开启注册: %v", si)
	}
	w.Root("INSERT INTO §.system_installations (uid, installation_id, component, instance_name, host, version, channel, status, installed_at) VALUES (?, ?, 'console', 'default', 'host-a', '1.0.0', 'stable', 'installed', NOW(3))",
		apistack.IDs.MustNew(), apistack.IDs.MustNew())
	if si := w.Do("GET", "/public/site-info", nil).JSON(t); si["console_installed"] != true {
		t.Errorf("存在 installed 的 console 记录 → console_installed: %v", si)
	}
}

// 公开接口按 IP 限流：超限 429 rate_limit.exceeded + Retry-After（契约声明了 429）。
func TestPublicEndpointsAreRateLimitedPerIP(t *testing.T) {
	w := apistack.New(t, apistack.Options{SysPublicRateLimit: 5})
	ip := func(s string) apistack.ReqOpt { return apistack.WithHeader("X-Forwarded-For", s) }
	// 测试服务端只信任回环代理，httptest 的对端是 127.0.0.1，XFF 被采信。
	for i := 0; i < 5; i++ {
		if r := w.Do("GET", "/public/version", nil, ip("198.51.100.1")); r.Status != 200 {
			t.Fatalf("前 5 次应放行: %d", r.Status)
		}
	}
	r := w.Do("GET", "/public/version", nil, ip("198.51.100.1"))
	w.Contract("getPublicVersion", "GET", "/public/version", r)
	if r.Status != 429 || r.ErrCode(t) != "rate_limit.exceeded" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("超限应 429 rate_limit.exceeded + Retry-After: %d %s", r.Status, r.Body)
	}
	if r := w.Do("GET", "/public/site-info", nil, ip("198.51.100.1")); r.Status != 429 {
		t.Errorf("同一 IP 的两个公开接口共享配额: %d", r.Status)
	}
	if r := w.Do("GET", "/public/version", nil, ip("198.51.100.2")); r.Status != 200 {
		t.Errorf("其他 IP 不受影响: %d", r.Status)
	}
	if r := w.Do("GET", "/public/version", nil, ip("2001:db8::1")); r.Status != 200 {
		t.Errorf("IPv6 来源应可限流且不出错: %d", r.Status)
	}
}

// AC5：/admin/system/version 返回真实 Server 版本与迁移版本；/admin/installations 返回安装记录（分页）。
func TestVersionAndInstallations(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	iid := apistack.IDs.MustNew()
	for i, comp := range []string{"server", "console", "node"} {
		w.Root("INSERT INTO §.system_installations (uid, installation_id, component, instance_name, host, version, channel, status, installed_at, last_upgraded_at) VALUES (?, ?, ?, 'default', ?, ?, 'stable', ?, NOW(3), ?)",
			apistack.IDs.MustNew(), iid, comp, "host-"+comp, "1.0."+string(rune('0'+i)), []string{"installed", "installed", "upgrading"}[i], []any{nil, nil, "2026-11-20 10:00:00.000"}[i])
	}
	set, _, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	var latest int64
	for _, m := range set.Migrations {
		latest = max(latest, m.Version)
	}

	r := adm.Do("GET", "/admin/system/version", nil)
	w.Contract("getVersionInfo", "GET", "/admin/system/version", r)
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	m := r.JSON(t)
	if m["server_version"] != version.Version || m["api_version"] != sysconfig.APIVersion || m["installation_id"] != iid {
		t.Errorf("版本信息: %v", m)
	}
	if got := nested(t, m, "schema_migration", "current"); got != padVersion(latest) || nested(t, m, "schema_migration", "pending") != float64(0) {
		t.Errorf("迁移版本应为真实的已应用最高版本 %s、待应用 0: %v", padVersion(latest), m["schema_migration"])
	}
	if cs := m["components"].([]any); len(cs) != 3 || cs[0].(map[string]any)["component"] != "console" {
		t.Errorf("components 应为 3 条安装记录（按组件排序）: %v", m["components"])
	}
	// 真实性：新增一个迁移版本以外的「未知版本」不会被虚报；这里验证 current 来自库而非常量——把库内最高版本标记为未应用后 pending 变 1。
	w.Root("UPDATE §.schema_migrations SET is_applied = 0 WHERE version_id = ?", latest)
	if r := adm.Do("GET", "/admin/system/version", nil); nested(t, r.JSON(t), "schema_migration", "pending") != float64(1) || nested(t, r.JSON(t), "schema_migration", "current") == padVersion(latest) {
		t.Errorf("迁移版本应来自库（探针）而非常量: %s", r.Body)
	}
	w.Root("UPDATE §.schema_migrations SET is_applied = 1 WHERE version_id = ?", latest)

	l := adm.Do("GET", "/admin/installations?page_size=2", nil)
	w.Contract("listInstallations", "GET", "/admin/installations?page_size=2", l)
	lm := l.JSON(t)
	if l.Status != 200 || len(lm["items"].([]any)) != 2 || nested(t, lm, "total") != float64(3) {
		t.Fatalf("安装记录分页: %d %s", l.Status, l.Body)
	}
	if l2 := adm.Do("GET", "/admin/installations?page=2&page_size=2", nil); len(l2.JSON(t)["items"].([]any)) != 1 {
		t.Errorf("第二页应 1 条: %s", l2.Body)
	}
	if b, _ := json.Marshal(lm["items"]); strings.Contains(string(b), "meta_json") || strings.Contains(string(b), "install_locked") {
		t.Errorf("安装记录不得暴露内部列: %s", b)
	}

	// 授权：system:read；租户 Console 401、租户令牌 403、无权限平台用户 403。
	nobody := w.Login(w.SeedUser("platform", nil))
	for _, p := range []string{"/admin/system/version", "/admin/installations"} {
		if r := nobody.Do("GET", p, nil); r.Status != 403 {
			t.Errorf("%s 无 system:read 应 403: %d", p, r.Status)
		}
	}
	tn := w.SeedTenant("ver")
	tu := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(tu, "tenant_owner")
	ts := w.Login(tu)
	if r := ts.Do("GET", "/admin/system/version", nil); r.Status != 401 {
		t.Errorf("租户 Console 会话应 401: %d", r.Status)
	}
	tok := ts.IssueToken("ver-probe", "task:read")
	if r := w.Bearer(tok, "GET", "/admin/installations", nil); r.Status != 403 {
		t.Errorf("租户令牌应 403: %d", r.Status)
	}
}

func padVersion(v int64) string {
	s := "000000" + itoa(v)
	return s[len(s)-6:]
}
