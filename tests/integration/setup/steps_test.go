package setupit

import (
	"net/http"
	"strings"
	"testing"
)

// AC1：setup mode 的路由面——仅 /setup 静态页、/healthz 与 /public/setup/*；其余 API（认证、任务、设置、未知路径）一律 503 setup.mode_active；就绪探针 503。
func TestSetupModeRouteSurface(t *testing.T) {
	w := newWiz(t)
	get := func(method, path string) (*http.Response, string) {
		req, _ := http.NewRequest(method, w.srv.URL+path, strings.NewReader("{}"))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return res, sb.String()
	}
	res, body := get("GET", "/setup")
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "text/html") || !strings.Contains(body, `<div id="app">`) ||
		!strings.Contains(res.Header.Get("Cache-Control"), "no-store") || !strings.Contains(res.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("/setup 应返回内嵌的向导 SPA 入口（no-store、严格 CSP；Story 1.37）: %d %v", res.StatusCode, res.Header)
	}
	if res, _ = get("GET", "/healthz"); res.StatusCode != 200 {
		t.Errorf("/healthz 存活探针应 200: %d", res.StatusCode)
	}
	if res, _ = get("GET", "/healthz/ready"); res.StatusCode != 503 {
		t.Errorf("/healthz/ready 在 setup mode 应 503: %d", res.StatusCode)
	}
	for _, c := range [][2]string{{"POST", "/api/v1/auth/login"}, {"GET", "/api/v1/tasks"}, {"GET", "/api/v1/admin/settings"}, {"GET", "/api/v1/public/version"}, {"GET", "/api/v1/sites"},
		{"GET", "/nope"}, {"DELETE", "/api/v1/auth/logout"}, {"GET", "/api/v1/public/setup/unknown"}, {"GET", "/setup/other"}} {
		res, body := get(c[0], c[1])
		if res.StatusCode != 503 || !strings.Contains(body, "setup.mode_active") || res.Header.Get("Retry-After") == "" {
			t.Errorf("%s %s 在 setup mode 应 503 setup.mode_active: %d %s", c[0], c[1], res.StatusCode, body)
		}
	}
}

// 步骤顺序与请求校验（每步都需要前置步骤完成；错误请求不产生副作用）。
func TestStepOrderingAndValidation(t *testing.T) {
	w := newWiz(t)
	// 没有数据库之前：initialize/admin/system/keys/complete 全部 409。
	for _, c := range []struct {
		m, p string
		body any
	}{{"POST", "initialize", nil}, {"POST", "admin", w.adminBody()}, {"PUT", "system", w.systemBody()}, {"POST", "keys", nil}, {"POST", "complete", nil}} {
		r := w.do(c.m, api+c.p, w.token, c.body)
		if r.Status != 409 || r.errCode() != "resource.conflict" {
			t.Errorf("%s 在前置步骤之前应 409: %d %s", c.p, r.Status, r.Raw)
		}
	}
	w.runThrough("init")
	// 管理员：弱口令被拒（auth.password_policy）；缺字段 400。
	weak := w.adminBody()
	weak["password"] = "Pass" + "word123!" // 运行期拼接：源码里不出现弱口令字面量（G-6）
	if r := w.post("admin", weak); r.Status != 400 || r.errCode() != "auth.password_policy" {
		t.Errorf("弱口令应 400 auth.password_policy: %d %s", r.Status, r.Raw)
	}
	if r := w.post("admin", map[string]any{"email": "a@example.test"}); r.Status != 400 || r.errCode() != "validation.failed" {
		t.Errorf("缺字段应 400: %d %s", r.Status, r.Raw)
	}
	if r := w.post("admin", map[string]any{"email": "not-an-email", "display_name": "x", "password": w.admPW}); r.Status != 400 {
		t.Errorf("邮箱非法应 400: %d", r.Status)
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.users"); n != 0 {
		t.Errorf("被拒绝的请求不得创建用户: %d", n)
	}
	// 系统配置要求管理员已创建；校验：run_mode、url、时区。
	if r := w.put("system", w.systemBody()); r.Status != 409 {
		t.Errorf("管理员之前配置系统应 409: %d", r.Status)
	}
	w.mustOK(w.post("admin", w.adminBody()), "admin")
	for name, mut := range map[string]func(m map[string]any){
		"run_mode":       func(m map[string]any) { m["run_mode"] = "bogus" },
		"base_url":       func(m map[string]any) { m["base_url"] = "ftp://x" },
		"timezone":       func(m map[string]any) { m["timezone"] = "Mars/Base" },
		"default_locale": func(m map[string]any) { m["default_locale"] = "fr" },
	} {
		b := w.systemBody()
		mut(b)
		if r := w.put("system", b); r.Status != 400 || r.errCode() != "validation.failed" {
			t.Errorf("%s 非法应 400: %d %s", name, r.Status, r.Raw)
		}
	}
	if r := w.put("system", map[string]any{}); r.Status != 400 {
		t.Errorf("缺 run_mode 应 400: %d", r.Status)
	}
	// 没有 base_url 时密钥步骤拒绝（叶证书 SAN 依赖它）。
	noURL := w.systemBody()
	delete(noURL, "base_url")
	w.mustOK(w.put("system", noURL), "system(无 base_url)")
	if r := w.post("keys", nil); r.Status != 409 || !strings.Contains(r.details(), "base_url") {
		t.Errorf("无 base_url 时 keys 应 409: %d %s", r.Status, r.Raw)
	}
	w.mustOK(w.put("system", w.systemBody()), "system")
	w.mustOK(w.post("keys", nil), "keys")
	// 全部校验失败的系统请求没有写入任何设置（整批 400）。
	if v := w.dbStr("SELECT CAST(value_json AS CHAR) FROM §.system_settings WHERE setting_key = 'general.timezone'"); v != `"Asia/Shanghai"` {
		t.Errorf("时区应为最后一次成功写入的值: %s", v)
	}
	// TOTP：错误验证码 401 且不启用；未开始绑定就确认 409；非法格式 400。
	ab := w.adminBody()
	ab["enable_totp"] = true
	ab["totp_code"] = "123456"
	if r := w.post("admin", ab); r.Status != 409 {
		t.Errorf("未开始绑定就确认应 409: %d %s", r.Status, r.Raw)
	}
	ab["totp_code"] = ""
	r := w.mustOK(w.post("admin", ab), "admin(开始绑定)")
	if sec, _ := r.secrets()["totp_secret"].(string); sec == "" {
		t.Fatalf("应返回 TOTP 密钥: %s", r.Raw)
	} else {
		w.bodies[len(w.bodies)-1] = strings.ReplaceAll(w.bodies[len(w.bodies)-1], sec, "<ONE_TIME>")
		w.secrets = append(w.secrets, sec)
	}
	ab["totp_code"] = "000000"
	if r := w.post("admin", ab); r.Status != 401 || r.errCode() != "auth.totp_invalid" {
		t.Errorf("错误验证码应 401 auth.totp_invalid: %d %s", r.Status, r.Raw)
	}
	ab["totp_code"] = "12ab"
	if r := w.post("admin", ab); r.Status != 400 {
		t.Errorf("验证码格式非法应 400: %d", r.Status)
	}
	if n := w.dbInt("SELECT totp_enabled FROM §.users WHERE email = ?", w.admEmail); n != 0 {
		t.Error("错误验证码不得启用 TOTP")
	}
	w.assertNoSecrets()
}
