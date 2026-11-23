package installer_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.40：静态前端（admin-web / console / docs / status / website static）的生产 ComponentSpec 经 toolsworld 的「伪操作系统」跑完整安装流程。
// 真实：状态机、journal、撤销、冲突判定、registry、downloader（真实签名的 bundle，平台 any）、resolver、生产的静态前端规格与 Nginx 片段渲染器。
// 伪的只有「操作系统」：伪 useradd/systemctl 与伪 nginx（只记录 -t / -s reload 的调用，可注入失败）。真实 nginx 加载生成片段的断言在 tests/release/frontend_test.go。

func staticWorld(t testing.TB) *toolsworld.World {
	t.Helper()
	w, err := toolsworld.NewWorldWith(t.TempDir(), toolsworld.Options{Static: true})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func setReq(set map[string]string, mut ...func(*installer.Request)) []func(*installer.Request) {
	return append([]func(*installer.Request){func(r *installer.Request) {
		r.Settings = set
		if r.Component == "website" {
			r.Variant = "static"
		}
	}}, mut...)
}

func hostSet(h string, kv ...string) map[string]string {
	m := map[string]string{"host": h}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func readW(t testing.TB, w *toolsworld.World, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- 渲染 ----

func TestStaticRenderGolden(t *testing.T) {
	type tc struct {
		name string
		comp string
		set  map[string]string
	}
	cases := []tc{
		{"admin-web.default", "admin-web", hostSet("admin.example.com")},
		{"admin-web.tls", "admin-web", hostSet("admin.example.com", "tls-cert", "/etc/ssl/lingedge/admin.pem", "tls-key", "/etc/ssl/lingedge/admin.key")},
		{"admin-web.remote-https-upstream", "admin-web", hostSet("admin.example.com", "api-upstream", "https://server.example.com:8443", "api-ca", "/etc/ssl/lingedge/ca.pem", "ipv6", "no")},
		{"console.default", "console", hostSet("console.example.com")},
		{"docs.default", "docs", hostSet("docs.example.com")},
		{"status.default", "status", hostSet("status.example.com", "http-port", "8081")},
		{"website.default", "website", hostSet("www.example.com")},
		{"website.tls-ports", "website", hostSet("www.example.com", "tls-cert", "/etc/ssl/lingedge/www.pem", "tls-key", "/etc/ssl/lingedge/www.key", "http-port", "8080", "https-port", "8443")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, err := installer.ParseStaticSettings(c.comp, c.set)
			if err != nil {
				t.Fatal(err)
			}
			p := installer.SiteParams{Component: c.comp, Host: st.Host, APIUpstream: st.APIUpstream, APICA: st.APICA, TLSCert: st.TLSCert, TLSKey: st.TLSKey,
				HTTPPort: st.HTTPPort, HTTPSPort: st.HTTPSPort, IPv6: st.IPv6}
			conf, hdr, err := installer.RenderStaticSite(p)
			if err != nil {
				t.Fatal(err)
			}
			conf2, hdr2, _ := installer.RenderStaticSite(p)
			if !bytes.Equal(conf, conf2) || !bytes.Equal(hdr, hdr2) {
				t.Fatal("渲染必须确定性")
			}
			if bad := installer.LintNginxSnippet(string(conf), installer.StaticHeadersPath(c.comp)); len(bad) > 0 {
				t.Errorf("片段未通过静态检查: %v", bad)
			}
			if bad := installer.LintSecurityHeaders(string(hdr)); len(bad) > 0 {
				t.Errorf("安全头文件未通过静态检查: %v", bad)
			}
			golden(t, "static."+c.name+".conf", conf)
			golden(t, "static."+c.name+".headers.conf", hdr)
		})
	}
}

// 官网/Admin/docs 的 API 反代范围与 IMP-17：靠结构断言而不是只靠 golden（golden 被误更新也不会放过）。
func TestStaticRenderAPIScopes(t *testing.T) {
	render := func(comp string) string {
		st, err := installer.ParseStaticSettings(comp, hostSet("x.example.com"))
		if err != nil {
			t.Fatal(err)
		}
		conf, _, err := installer.RenderStaticSite(installer.SiteParams{Component: comp, Host: st.Host, APIUpstream: st.APIUpstream, HTTPPort: 80, HTTPSPort: 443, IPv6: true})
		if err != nil {
			t.Fatal(err)
		}
		return string(conf)
	}
	web := render("website")
	for _, want := range []string{"location ^~ /api/v1/public/ {", "location ^~ /api/ {\n        return 404;", "location = /api/v1/public/setup {\n        return 404;",
		"location ^~ /api/v1/public/setup/ {\n        return 404;", "proxy_set_header X-Forwarded-For $remote_addr;"} {
		if !strings.Contains(web, want) {
			t.Errorf("官网片段缺少 %q", want)
		}
	}
	if strings.Contains(web, "location ^~ /api/ {\n        proxy_pass") {
		t.Error("官网不得整体反代 /api/")
	}
	if strings.Contains(web, "$http_x_forwarded_for") || strings.Contains(web, "proxy_add_x_forwarded_for") {
		t.Error("不得信任/追加访客的 X-Forwarded-For")
	}
	if !strings.Contains(render("admin-web"), "location ^~ /api/ {\n        proxy_pass http://127.0.0.1:8080;") {
		t.Error("Admin 应把 /api/ 反代到 127.0.0.1:8080")
	}
	for _, c := range []string{"docs", "status"} {
		if s := render(c); strings.Contains(s, "proxy_pass") || !strings.Contains(s, "location ^~ /api/ {\n        return 404;") {
			t.Errorf("%s 是纯静态站点：不得反代，/api/ 一律 404", c)
		}
	}
}

func TestLintNginxSnippet(t *testing.T) {
	const hdr = "/etc/lingedge/nginx/x-security-headers.conf"
	good := "server {\n  location / {\n    include " + hdr + ";\n    add_header Cache-Control no-cache always;\n  }\n  location /api/ { proxy_pass http://127.0.0.1:8080; }\n}\n"
	if bad := installer.LintNginxSnippet(good, hdr); len(bad) != 0 {
		t.Fatalf("合法片段被误伤: %v", bad)
	}
	neg := []struct{ name, conf, want string }{
		{"location 有 add_header 没有 include", "server { location / { add_header X-A 1 always; } }", "没有显式 include"},
		{"include 了别的文件", "server { location / { include /etc/other.conf; add_header X-A 1 always; } }", "没有显式 include"},
		{"嵌套 location 缺 include", "server { location / { include " + hdr + "; add_header X 1 always; location /a/ { add_header Y 1 always; } } }", "location /a/"},
		{"server 级 add_header", "server { add_header X-A 1 always; location / { return 404; } }", "server 级出现 add_header"},
		{"add_trailer 同样算", "server { location / { add_trailer X-A 1 always; } }", "没有显式 include"},
		{"改写 Cookie 域", "server { location / { proxy_cookie_domain a.example.com example.com; } }", "host-only"},
		{"自行下发 Set-Cookie", "server { location / { include " + hdr + "; add_header Set-Cookie a=b always; } }", "Set-Cookie"},
		{"无法解析", "server { location / { ", "无法解析"},
	}
	for _, c := range neg {
		bad := installer.LintNginxSnippet(c.conf, hdr)
		if len(bad) == 0 || !strings.Contains(strings.Join(bad, ";"), c.want) {
			t.Errorf("%s：应检出 %q，得 %v", c.name, c.want, bad)
		}
	}
	if bad := installer.LintNginxSnippet("server { location / { proxy_cookie_domain off; } }", hdr); len(bad) != 0 {
		t.Errorf("proxy_cookie_domain off 是允许的: %v", bad)
	}
	if bad := installer.LintSecurityHeaders("add_header X-A 1 always;\nadd_header X-B 2;\n"); len(bad) != 1 || !strings.Contains(bad[0], "X-B") {
		t.Errorf("应只检出缺 always 的 X-B: %v", bad)
	}
	if bad := installer.LintSecurityHeaders("server {}"); len(bad) == 0 {
		t.Error("头文件里出现 server 块应被检出")
	}
	if bad := installer.LintSecurityHeaders("# nothing\n"); len(bad) == 0 {
		t.Error("空的头文件应被检出")
	}
}

func TestParseStaticSettings(t *testing.T) {
	bad := []struct {
		name string
		comp string
		set  map[string]string
		want string
	}{
		{"缺 host", "admin-web", nil, "缺少必填项 host"},
		{"host 通配符", "admin-web", hostSet("*.example.com"), "host"},
		{"host 是 IP", "admin-web", hostSet("10.0.0.1"), "IP"},
		{"host 是 IPv6", "admin-web", hostSet("::1"), "host"},
		{"host 带端口", "admin-web", hostSet("a.example.com:8080"), "host"},
		{"host 含空白/分号（配置注入）", "admin-web", hostSet("a.example.com; return 200"), "host"},
		{"host 含换行", "admin-web", hostSet("a.example.com\nlisten 1"), "host"},
		{"host 下划线", "admin-web", hostSet("a_b.example.com"), "host"},
		{"host 非 ASCII", "admin-web", hostSet("管理.example.com"), "标签"},
		{"host 纯数字", "admin-web", hostSet("123"), "纯数字"},
		{"host 超长标签", "admin-web", hostSet(strings.Repeat("a", 64) + ".example.com"), "标签"},
		{"未知键（不存在绕过开关）", "admin-web", hostSet("a.example.com", "allow-shared-host", "yes"), "不认识"},
		{"docs 不认识 api-upstream", "docs", hostSet("d.example.com", "api-upstream", "http://127.0.0.1:8080"), "不认识"},
		{"上游带路径", "admin-web", hostSet("a.example.com", "api-upstream", "http://127.0.0.1:8080/x"), "api-upstream"},
		{"上游带用户信息", "admin-web", hostSet("a.example.com", "api-upstream", "http://u"+":"+"x@127.0.0.1:8080"), "api-upstream"},
		{"上游非 http(s)", "admin-web", hostSet("a.example.com", "api-upstream", "unix:/run/x.sock"), "api-upstream"},
		{"上游端口越界", "admin-web", hostSet("a.example.com", "api-upstream", "http://127.0.0.1:70000"), "端口"},
		{"上游含注入", "admin-web", hostSet("a.example.com", "api-upstream", "http://a.example.com;return"), "api-upstream"},
		{"https 上游缺 api-ca", "admin-web", hostSet("a.example.com", "api-upstream", "https://s.example.com"), "api-ca"},
		{"http 上游带 api-ca", "admin-web", hostSet("a.example.com", "api-ca", "/etc/ca.pem"), "https"},
		{"只给 tls-cert", "admin-web", hostSet("a.example.com", "tls-cert", "/etc/c.pem"), "同时"},
		{"tls 路径相对", "admin-web", hostSet("a.example.com", "tls-cert", "c.pem", "tls-key", "k.pem"), "绝对路径"},
		{"tls 路径含 ..", "admin-web", hostSet("a.example.com", "tls-cert", "/etc/../c.pem", "tls-key", "/etc/k.pem"), ".."},
		{"tls 路径含空格", "admin-web", hostSet("a.example.com", "tls-cert", "/etc/c d.pem", "tls-key", "/etc/k.pem"), "绝对路径"},
		{"端口越界", "admin-web", hostSet("a.example.com", "http-port", "0"), "http-port"},
		{"http/https 端口相同", "admin-web", hostSet("a.example.com", "tls-cert", "/etc/c.pem", "tls-key", "/etc/k.pem", "http-port", "8443", "https-port", "8443"), "不能相同"},
		{"ipv6 取值", "admin-web", hostSet("a.example.com", "ipv6", "maybe"), "ipv6"},
		{"nginx-link 取值", "admin-web", hostSet("a.example.com", "nginx-link", "auto"), "nginx-link"},
	}
	for _, c := range bad {
		_, err := installer.ParseStaticSettings(c.comp, c.set)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：应拒绝并提到 %q，得 %v", c.name, c.want, err)
		}
	}
	// 错误信息只回显键与规则，不回显取值（值可能是路径/口令形态的东西）。
	_, err := installer.ParseStaticSettings("admin-web", hostSet("a.example.com", "tls-cert", "/etc/secret-looking-path/x y.pem", "tls-key", "/etc/k.pem"))
	if err == nil || strings.Contains(err.Error(), "secret-looking-path") {
		t.Errorf("错误信息不应回显取值: %v", err)
	}
	st, err := installer.ParseStaticSettings("admin-web", hostSet("Admin.Example.COM"))
	if err != nil || st.Host != "admin.example.com" || st.APIUpstream != "http://127.0.0.1:8080" || st.HTTPPort != 80 || st.HTTPSPort != 443 || !st.IPv6 {
		t.Errorf("默认值/规范化不符: %+v %v", st, err)
	}
	if _, err := installer.ParseStaticSettings("admin-web", hostSet("localhost")); err != nil {
		t.Errorf("localhost 是合法主机名: %v", err)
	}
	if u := st.SiteURL(); u != "http://admin.example.com/" {
		t.Errorf("SiteURL=%q", u)
	}
	if _, err := installer.ParseStaticSettings("server", hostSet("a.example.com")); err == nil {
		t.Error("server 不是静态前端")
	}
}

func TestStaticSpecBasics(t *testing.T) {
	specs := installer.ProductionSpecs()
	for _, n := range installer.StaticComponents() {
		s, ok := specs[n]
		if !ok {
			t.Fatalf("生产规格缺少静态前端 %s", n)
		}
		if s.Name() != n || len(s.Executables("0.1.0", "")) != 0 {
			t.Errorf("%s 规格基本契约不符", n)
		}
		if err := s.ValidateSettings(nil); err == nil || !strings.Contains(err.Error(), "host") {
			t.Errorf("%s：空设置应因缺 host 被拒绝: %v", n, err)
		}
		if err := s.Start(context.Background(), &installer.Runtime{}); err != nil {
			t.Errorf("%s 的 Start 应为空操作: %v", n, err)
		}
	}
	if _, err := specs["website"].InitConfig(context.Background(), installer.ConfigInput{Component: "website", Variant: "ssr", Settings: hostSet("www.example.com")}); err == nil || !strings.Contains(err.Error(), "2.14") {
		t.Errorf("ssr 变体应明确拒绝并指向 Story 2.14: %v", err)
	}
	pv, ok := specs["admin-web"].(interface{ ValidatePayload(string) error })
	if !ok {
		t.Fatal("静态前端规格应校验包布局")
	}
	mk := func(files map[string]string) string {
		d := t.TempDir()
		for rel, body := range files {
			p := filepath.Join(d, filepath.FromSlash(rel))
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return d
	}
	if err := pv.ValidatePayload(mk(map[string]string{"public/index.html": "<html>", "public/a.js": "x", "share/lingedge-sbom.cdx.json": "{}"})); err != nil {
		t.Errorf("合法布局被拒绝: %v", err)
	}
	for name, files := range map[string]map[string]string{
		"缺 index.html":  {"public/a.js": "x"},
		"index.html 为空": {"public/index.html": ""},
		"顶层多出文件":        {"public/index.html": "x", "evil.sh": "x"},
		"顶层多出目录":        {"public/index.html": "x", "bin/x": "x"},
		"index 在错误的位置":  {"index.html": "x", "public/a.js": "x"},
	} {
		if err := pv.ValidatePayload(mk(files)); err == nil {
			t.Errorf("%s：应被拒绝", name)
		}
	}
	d := mk(map[string]string{"public/real.html": "x"})
	if err := os.Symlink("real.html", filepath.Join(d, "public", "index.html")); err != nil {
		t.Fatal(err)
	}
	if err := pv.ValidatePayload(d); err == nil {
		t.Error("index.html 是符号链接应被拒绝")
	}
}

// ---- 安装 ----

func TestStaticInstallLayoutAndNeverTouchesExistingNginx(t *testing.T) {
	w := staticWorld(t)
	if err := w.SetNativeNginx(); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(w.Root, "etc/nginx/conf.d/other.conf")
	if err := os.WriteFile(other, []byte("server { listen 80; server_name other.example.com; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nginxBefore := snapshot(t, filepath.Join(w.Root, "etc/nginx"), nil)

	res := must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com"))...))
	if res.Outcome != installer.OutcomeInstalled || len(res.Components) != 1 || res.Components[0].State != "installed" {
		t.Fatalf("结果异常: %+v", res)
	}
	// 目录与原子切换。
	if tg, err := os.Readlink(filepath.Join(w.Root, "opt/lingedge/admin-web/current")); err != nil || tg != "releases/0.1.0" {
		t.Fatalf("current 应指向 releases/0.1.0: %q %v", tg, err)
	}
	if !strings.Contains(readW(t, w, "opt/lingedge/admin-web/current/public/index.html"), "admin-web") {
		t.Error("public/index.html 应来自包")
	}
	for _, rel := range []string{"opt/lingedge/admin-web/releases/0.1.0/public/index.html.gz", "opt/lingedge/admin-web/releases/0.1.0/public/assets/app-1a2b3c.js.gz"} {
		if !w.Has(rel) {
			t.Errorf("预压缩资源 %s 应随包解出（gzip_static）", rel)
		}
	}
	// 片段与安全头：内容、权限、属主。
	conf := readW(t, w, "etc/lingedge/nginx/admin-web.conf")
	if comp, host, ok := installer.ParseSiteMarker([]byte(conf)); !ok || comp != "admin-web" || host != "admin.example.com" {
		t.Errorf("标记不符: %q", conf[:80])
	}
	for _, want := range []string{"server_name admin.example.com;", "root /opt/lingedge/admin-web/current/public;", "gzip_static on;", "proxy_pass http://127.0.0.1:8080;", "try_files $uri /index.html;"} {
		if !strings.Contains(conf, want) {
			t.Errorf("片段缺少 %q", want)
		}
	}
	if bad := installer.LintNginxSnippet(conf, installer.StaticHeadersPath("admin-web")); len(bad) > 0 {
		t.Errorf("已安装片段未通过静态检查: %v", bad)
	}
	if bad := installer.LintSecurityHeaders(readW(t, w, "etc/lingedge/nginx/admin-web-security-headers.conf")); len(bad) > 0 {
		t.Errorf("已安装安全头文件未通过静态检查: %v", bad)
	}
	for _, rel := range []string{"etc/lingedge/nginx/admin-web.conf", "etc/lingedge/nginx/admin-web-security-headers.conf"} {
		if fi, err := os.Stat(filepath.Join(w.Root, rel)); err != nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("%s 应为 0644: %v %v", rel, fi, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(w.Root, "etc/lingedge/nginx")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("/etc/lingedge/nginx 应为 0755: %v %v", fi, err)
	}
	// 绝不触碰既有 Nginx 配置（AC1）：整棵 /etc/nginx 逐字节不变；没有调用过 nginx；没有创建软链。
	if d := diffSnap(nginxBefore, snapshot(t, filepath.Join(w.Root, "etc/nginx"), nil)); d != "" {
		t.Fatalf("安装改动了既有 Nginx 配置:\n%s", d)
	}
	if calls := w.NginxCalls(); len(calls) != 0 {
		t.Errorf("默认（非交互、未指定 nginx-link）不得调用 nginx: %v", calls)
	}
	// 状态记录与访问信息。
	st, _, err := w.Registry("run-state").LoadState()
	if err != nil {
		t.Fatal(err)
	}
	rec := st.Components["admin-web"]
	if rec.State != "installed" || rec.ConfigDir != "/etc/lingedge/nginx" || rec.Unit != "" || rec.RunUser != "" || rec.ReleaseDir != "/opt/lingedge/admin-web/releases/0.1.0" {
		t.Errorf("state 记录不符: %+v", rec)
	}
	kinds := map[string]string{}
	for _, a := range res.Access {
		kinds[a.Kind] += a.Value + "\n"
	}
	if !strings.Contains(kinds["site_url"], "http://admin.example.com/") || !strings.Contains(kinds["nginx_include"], "/etc/lingedge/nginx/admin-web.conf") ||
		!strings.Contains(kinds["nginx_include"], "ln -s") || !strings.Contains(kinds["note"], "HTTP 站点") {
		t.Errorf("访问信息不完整: %+v", res.Access)
	}
	// 没有运行用户、没有数据目录、没有 systemd 单元被创建。
	for _, rel := range []string{"etc/systemd/system/lingedge-admin-web.service", "var/lib/lingedge/admin-web"} {
		if w.Has(rel) {
			t.Errorf("静态前端不应创建 %s", rel)
		}
	}
	if w.FakeHas("users/lingedge") {
		t.Error("静态前端不应创建运行用户")
	}
	// 重装同一版本、同一设置：已安装 → 无事可做（不重复写、不冲突）。
	if r2 := must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com"))...)); r2.Outcome != installer.OutcomeNoop {
		t.Errorf("已是目标版本应 Noop: %s", r2.Outcome)
	}
}

func TestStaticInstallAllComponents(t *testing.T) {
	w := staticWorld(t)
	hosts := map[string]string{"admin-web": "admin.example.com", "console": "console.example.com", "docs": "docs.example.com", "status": "status.example.com", "website": "www.example.com"}
	for _, c := range installer.StaticComponents() {
		res := must(t, doInstall(t, w, c, nil, setReq(hostSet(hosts[c]))...))
		if res.Outcome != installer.OutcomeInstalled {
			t.Fatalf("%s: %+v", c, res)
		}
		conf := readW(t, w, "etc/lingedge/nginx/"+c+".conf")
		if !strings.Contains(conf, "server_name "+hosts[c]+";") || !strings.Contains(conf, "root /opt/lingedge/"+c+"/current/public;") {
			t.Errorf("%s 片段不符", c)
		}
	}
}

// ---- 独立主机名（AC5，IMP-17）----

func TestStaticDistinctHostsEnforced(t *testing.T) {
	w := staticWorld(t)
	must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com"))...))
	before := sysSnap(t, w)

	// 同主机名（含大小写差异）装其他前端：退出码 2 的类别（KindSettings），明确点名占用者，且没有任何副作用。
	for _, tc := range []struct{ comp, host string }{
		{"website", "admin.example.com"}, {"website", "ADMIN.example.com"}, {"console", "Admin.Example.Com"}, {"docs", "admin.example.com"}, {"status", "admin.example.com"},
	} {
		o := doInstall(t, w, tc.comp, nil, setReq(hostSet(tc.host))...)
		if o.err == nil || kindOf(t, o.err) != installer.KindSettings {
			t.Fatalf("%s host=%s 应以 KindSettings（退出码 2）拒绝: %v", tc.comp, tc.host, o.err)
		}
		var ie *installer.Error
		if _ = errors.As(o.err, &ie); ie == nil || !strings.Contains(ie.Msg, "admin-web") || !strings.Contains(ie.Hint, "没有绕过开关") || ie.SystemState != "unchanged" {
			t.Errorf("错误应点名占用者、说明无绕过开关: %+v", ie)
		}
		if d := diffSnap(before, sysSnap(t, w)); d != "" {
			t.Fatalf("被拒绝的安装产生了副作用:\n%s", d)
		}
	}
	// dry-run 同样强制（计划阶段就拒绝）。
	in := w.NewInstaller("run-dry")
	req := baseReq(w, "console", func(r *installer.Request) { r.Settings = hostSet("admin.example.com"); r.DryRun = true })
	if _, err := in.Plan(t.Context(), req); err == nil || kindOf(t, err) != installer.KindSettings {
		t.Errorf("--dry-run 也应拒绝: %v", err)
	}
	// 缺 host 也是 2；不存在任何放宽的键。
	if o := doInstall(t, w, "console", nil, setReq(nil)...); o.err == nil || kindOf(t, o.err) != installer.KindSettings {
		t.Errorf("缺 host 应 KindSettings: %v", o.err)
	}
	if o := doInstall(t, w, "console", nil, setReq(hostSet("admin.example.com", "allow-shared-host", "yes"))...); o.err == nil || kindOf(t, o.err) != installer.KindSettings {
		t.Errorf("不得有绕过开关: %v", o.err)
	}
	// 不同主机名 → 都能装；三者（Admin/Console/官网）互不相同。
	must(t, doInstall(t, w, "console", nil, setReq(hostSet("console.example.com"))...))
	must(t, doInstall(t, w, "website", nil, setReq(hostSet("www.example.com"))...))
	// 已装前端的片段读不了 = 无法确认 → 失败关闭，而不是放过。
	if err := os.Chmod(filepath.Join(w.Root, "etc/lingedge/nginx/console.conf"), 0); err == nil {
		defer func() { _ = os.Chmod(filepath.Join(w.Root, "etc/lingedge/nginx/console.conf"), 0o644) }()
		if os.Getuid() != 0 {
			o := doInstall(t, w, "docs", nil, setReq(hostSet("docs.example.com"))...)
			if o.err == nil || kindOf(t, o.err) != installer.KindPrecondition {
				t.Errorf("无法读取已装前端的片段应失败关闭（KindPrecondition）: %v", o.err)
			}
		}
	}
}

// ---- 冲突不覆盖（AC3，FR-6）----

func TestStaticExistingConfIsNeverOverwritten(t *testing.T) {
	w := staticWorld(t)
	confDir := filepath.Join(w.Root, "etc/lingedge/nginx")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "# 运维手写的站点，不能被覆盖\nserver { listen 80; server_name admin.example.com; }\n"
	p := filepath.Join(confDir, "admin-web.conf")
	if err := os.WriteFile(p, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	before := sysSnap(t, w)
	o := doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com"))...)
	if o.err == nil || kindOf(t, o.err) != installer.KindConfirmRequired {
		t.Fatalf("同名 conf 已存在、非交互且无 --yes 应停下确认（退出码 9）: %v", o.err)
	}
	var ie *installer.Error
	_ = errors.As(o.err, &ie)
	found := false
	for _, c := range ie.Conflicts {
		found = found || (c.Kind == "file" && strings.HasSuffix(c.Path, "/admin-web.conf"))
	}
	if !found {
		t.Errorf("冲突清单应列出 admin-web.conf: %+v", ie.Conflicts)
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" {
		t.Fatalf("停止确认时不应有任何改动（连 .new 也不写）:\n%s", d)
	}
	// 经 --yes 确认：既有文件逐字节原样，新内容写入 .new。
	res := must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com"), func(r *installer.Request) { r.Yes = true })...))
	if readW(t, w, "etc/lingedge/nginx/admin-web.conf") != mine {
		t.Fatal("既有 conf 被覆盖了")
	}
	if !strings.Contains(readW(t, w, "etc/lingedge/nginx/admin-web.conf.new"), "server_name admin.example.com;") {
		t.Error("新内容应写入 .new")
	}
	warned := false
	for _, wn := range res.Warnings {
		warned = warned || wn.Code == "config.new_written"
	}
	if !warned {
		t.Errorf("应有 config.new_written 警告: %+v", res.Warnings)
	}
}

func TestStaticSameDomainSiteStopsForConfirmation(t *testing.T) {
	w := staticWorld(t)
	if err := w.SetNativeNginx(); err != nil {
		t.Fatal(err)
	}
	// 探测到宿主机上已有站点声明了同一域名（宝塔/系统均可）。
	w.Probe.Baota.Sites = []baota.Site{{File: "/etc/nginx/conf.d/blog.conf", Source: "system", ServerNames: []string{"Admin.Example.com"}, Listens: []baota.Listen{{Port: 80}}}}
	before := sysSnap(t, w)
	o := doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com", "nginx-link", "yes"))...)
	if o.err == nil || kindOf(t, o.err) != installer.KindConfirmRequired {
		t.Fatalf("同域名站点应停下确认（9）: %v", o.err)
	}
	var ie *installer.Error
	_ = errors.As(o.err, &ie)
	ok := false
	for _, c := range ie.Conflicts {
		ok = ok || (c.Kind == "site" && c.Path == "/etc/nginx/conf.d/blog.conf")
	}
	if !ok {
		t.Errorf("冲突清单应含 site 冲突: %+v", ie.Conflicts)
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" {
		t.Fatalf("停止确认时不应有任何改动:\n%s", d)
	}
	// 确认后：只生成片段，**不**创建 conf.d 软链、不调用 nginx（即使 nginx-link=yes），也不动对方站点。
	res := must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com", "nginx-link", "yes"), func(r *installer.Request) { r.Yes = true })...))
	if w.Has("etc/nginx/conf.d/lingedge-admin-web.conf") || len(w.NginxCalls()) != 0 {
		t.Error("同域名站点存在时不得创建软链/调用 nginx")
	}
	skipped := false
	for _, wn := range res.Warnings {
		skipped = skipped || wn.Code == "nginx.link_skipped_site_conflict"
	}
	if !skipped {
		t.Errorf("应有 nginx.link_skipped_site_conflict 警告: %+v", res.Warnings)
	}
	// 我们自己的文件不算冲突：把站点记录改成 Tools 片段的位置，重装不应报同域名。
	w2 := staticWorld(t)
	w2.Probe.Baota.Sites = []baota.Site{{File: "/etc/lingedge/nginx/admin-web.conf", Source: "system", ServerNames: []string{"admin.example.com"}}}
	must(t, doInstall(t, w2, "admin-web", nil, setReq(hostSet("admin.example.com"))...))
}

// ---- Nginx conf.d 软链（AC2）----

func linkPath(w *toolsworld.World) string {
	return filepath.Join(w.Root, "etc/nginx/conf.d/lingedge-admin-web.conf")
}

func TestStaticNginxLink(t *testing.T) {
	native := func(t *testing.T) *toolsworld.World {
		w := staticWorld(t)
		if err := w.SetNativeNginx(); err != nil {
			t.Fatal(err)
		}
		return w
	}
	accessKinds := func(res *installer.Result) string {
		var b strings.Builder
		for _, a := range res.Access {
			b.WriteString(a.Kind + ":" + a.Value + "\n")
		}
		return b.String()
	}
	hasWarn := func(res *installer.Result, code string) bool {
		for _, wn := range res.Warnings {
			if wn.Code == code {
				return true
			}
		}
		return false
	}

	t.Run("显式 nginx-link=yes：创建软链，nginx -t 之后 reload", func(t *testing.T) {
		w := native(t)
		res := must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com", "nginx-link", "yes"))...))
		if tg, err := os.Readlink(linkPath(w)); err != nil || tg != "/etc/lingedge/nginx/admin-web.conf" {
			t.Fatalf("软链应指向 Tools 片段: %q %v", tg, err)
		}
		calls := w.NginxCalls()
		if len(calls) != 2 || calls[0] != "-t" || calls[1] != "-s reload" {
			t.Errorf("应依次 nginx -t、nginx -s reload: %v", calls)
		}
		if !strings.Contains(accessKinds(res), "已创建") {
			t.Errorf("访问信息应说明已创建软链: %s", accessKinds(res))
		}
		// 计划里有这一步（dry-run 同样展示）。
		found := false
		for _, s := range res.Pre.Plan.Steps {
			found = found || (s.Kind == "start" && strings.Contains(s.Summary, "软链"))
		}
		if !found {
			t.Errorf("计划应含软链步骤: %+v", res.Pre.Plan.Steps)
		}
	})

	t.Run("交互：回答是 → 创建；回答否 → 只生成片段并给手工指引", func(t *testing.T) {
		for _, answer := range []bool{true, false} {
			w := native(t)
			asked := 0
			confirm := func(d *installer.Deps) {
				d.Confirm = func(p installer.Prompt) (bool, error) {
					if p.Kind != "nginx_link" {
						t.Errorf("意外的提问 %q", p.Kind)
					}
					if !strings.Contains(p.Message, "lingedge-admin-web.conf") {
						t.Errorf("提问应点明软链路径: %q", p.Message)
					}
					asked++
					return answer, nil
				}
			}
			res := must(t, doInstall(t, w, "admin-web", []func(*installer.Deps){confirm}, setReq(hostSet("admin.example.com"), func(r *installer.Request) { r.NonInteractive = false })...))
			if asked != 1 {
				t.Fatalf("应询问恰好一次，实际 %d", asked)
			}
			_, lerr := os.Lstat(linkPath(w))
			if answer != (lerr == nil) {
				t.Errorf("回答 %v 但软链存在=%v", answer, lerr == nil)
			}
			if !answer && (len(w.NginxCalls()) != 0 || !strings.Contains(accessKinds(res), "ln -s")) {
				t.Errorf("回答否：不得调用 nginx，且应给手工指引: %v %s", w.NginxCalls(), accessKinds(res))
			}
		}
	})

	t.Run("非交互且未指定：默认不创建（--yes 也不隐式启用 reload）", func(t *testing.T) {
		w := native(t)
		res := must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com"), func(r *installer.Request) { r.Yes = true })...))
		if w.Has("etc/nginx/conf.d/lingedge-admin-web.conf") || len(w.NginxCalls()) != 0 {
			t.Error("--yes 只用于确认冲突，不得隐式创建软链/reload")
		}
		if !hasWarn(res, "nginx.link_not_asked") {
			t.Errorf("应提示可用 --set nginx-link=yes: %+v", res.Warnings)
		}
	})

	t.Run("nginx -t 失败：撤销软链，Nginx 配置不变，安装仍完成并给出原因", func(t *testing.T) {
		w := native(t)
		w.SetCtl("fail-nginx-t", true)
		nginxBefore := snapshot(t, filepath.Join(w.Root, "etc/nginx"), nil)
		res := must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com", "nginx-link", "yes"))...))
		if d := diffSnap(nginxBefore, snapshot(t, filepath.Join(w.Root, "etc/nginx"), nil)); d != "" {
			t.Fatalf("nginx -t 失败后 /etc/nginx 必须与安装前一致（软链已撤销）:\n%s", d)
		}
		calls := w.NginxCalls()
		if len(calls) != 1 || calls[0] != "-t" {
			t.Errorf("-t 失败后不得 reload: %v", calls)
		}
		var msg string
		for _, wn := range res.Warnings {
			if wn.Code == "nginx.test_failed" {
				msg = wn.Message
			}
		}
		if !strings.Contains(msg, "unknown directive") || !strings.Contains(msg, "已撤销软链") {
			t.Errorf("警告应带 nginx 自己的报错并说明已撤销: %q", msg)
		}
		if !w.Has("etc/lingedge/nginx/admin-web.conf") || !w.Has("opt/lingedge/admin-web/current") {
			t.Error("静态文件与片段应已就位（只是未接入 Nginx）")
		}
	})

	t.Run("reload 失败：同样撤销软链", func(t *testing.T) {
		w := native(t)
		w.SetCtl("fail-nginx-reload", true)
		res := must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com", "nginx-link", "yes"))...))
		if w.Has("etc/nginx/conf.d/lingedge-admin-web.conf") {
			t.Error("reload 失败后软链应已撤销")
		}
		if !hasWarn(res, "nginx.reload_failed") {
			t.Errorf("应有 nginx.reload_failed: %+v", res.Warnings)
		}
	})

	t.Run("conf.d 里同名的既有文件：不覆盖，列为冲突，确认后也不创建软链", func(t *testing.T) {
		w := native(t)
		theirs := "# 别人的\n"
		if err := os.WriteFile(linkPath(w), []byte(theirs), 0o644); err != nil {
			t.Fatal(err)
		}
		o := doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com", "nginx-link", "yes"))...)
		if o.err == nil || kindOf(t, o.err) != installer.KindConfirmRequired {
			t.Fatalf("应停下确认（9）: %v", o.err)
		}
		must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com", "nginx-link", "yes"), func(r *installer.Request) { r.Yes = true })...))
		if b, _ := os.ReadFile(linkPath(w)); string(b) != theirs {
			t.Fatal("既有的 conf.d 同名文件被覆盖了")
		}
		if len(w.NginxCalls()) != 0 {
			t.Errorf("不得调用 nginx: %v", w.NginxCalls())
		}
	})

	t.Run("宝塔环境一律只打印指引（FR-7）；nginx-link=yes 明确失败", func(t *testing.T) {
		w := native(t)
		w.Probe.Baota.Detected, w.Probe.Baota.Confidence = true, "high"
		w.Probe.Baota.Evidence = []baota.Evidence{{Kind: "panel_dir", Path: "/www/server/panel"}, {Kind: "bt_command", Path: "/usr/bin/bt"}, {Kind: "port_file", Path: "/www/server/panel/data/port.pl"}}
		w.Probe.Baota.WebServers = []baota.WebServer{{Variant: "baota", Kind: "nginx", Conf: "/www/server/nginx/conf/nginx.conf"}}
		before := sysSnap(t, w)
		o := doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com", "nginx-link", "yes"))...)
		if o.err == nil || kindOf(t, o.err) != installer.KindSettings || !strings.Contains(o.err.Error(), "宝塔") {
			t.Fatalf("宝塔下 nginx-link=yes 应以 KindSettings 失败并说明原因: %v", o.err)
		}
		if d := diffSnap(before, sysSnap(t, w)); d != "" {
			t.Fatalf("失败不得有副作用:\n%s", d)
		}
		res := must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com"))...))
		if w.Has("etc/nginx/conf.d/lingedge-admin-web.conf") || len(w.NginxCalls()) != 0 {
			t.Error("宝塔环境不得创建软链/调用 nginx")
		}
		ak := accessKinds(res)
		if !strings.Contains(ak, "baota_guide:") || !strings.Contains(ak, "/opt/lingedge/admin-web/current/public") || !strings.Contains(ak, "/etc/lingedge/nginx/admin-web.conf") {
			t.Errorf("应打印「新建站点并 include 该文件 / root 指向 current/public」指引: %s", ak)
		}
		if !hasWarn(res, "nginx.baota_guidance_only") {
			t.Errorf("应有 nginx.baota_guidance_only: %+v", res.Warnings)
		}
	})

	t.Run("原生 Nginx 但主配置没有 include conf.d：不假定约定，给手工指引；nginx-link=yes 失败", func(t *testing.T) {
		w := native(t)
		if err := os.WriteFile(filepath.Join(w.Root, "etc/nginx/nginx.conf"), []byte("events {}\nhttp {\n  server { listen 80; }\n}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if o := doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com", "nginx-link", "yes"))...); o.err == nil || kindOf(t, o.err) != installer.KindSettings {
			t.Fatalf("没有 conf.d 约定时 nginx-link=yes 应失败: %v", o.err)
		}
		res := must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com"))...))
		if len(w.NginxCalls()) != 0 || !strings.Contains(accessKinds(res), "nginx_include:") {
			t.Error("应只给手工指引")
		}
	})

	t.Run("--skip-start 不创建软链", func(t *testing.T) {
		w := native(t)
		must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com", "nginx-link", "yes"), func(r *installer.Request) { r.SkipStart = true })...))
		if w.Has("etc/nginx/conf.d/lingedge-admin-web.conf") || len(w.NginxCalls()) != 0 {
			t.Error("--skip-start：只物化，不接入 Nginx")
		}
	})
}

// 激活之后失败（此处：健康检查）→ 整体回滚：软链被摘除并再 reload 一次，整棵系统树与安装前相同。
func TestStaticRollbackRemovesLinkAndReloads(t *testing.T) {
	w := staticWorld(t)
	if err := w.SetNativeNginx(); err != nil {
		t.Fatal(err)
	}
	before := sysSnap(t, w)
	breakCurrent := func(d *installer.Deps) {
		fastHealth(d)
		d.Hook = func(point string) {
			if point == "effect:19" { // 启动步骤（含软链与 reload）完成之后、健康检查之前
				_ = os.Remove(filepath.Join(w.Root, "opt/lingedge/admin-web/current"))
			}
		}
	}
	o := doInstall(t, w, "admin-web", []func(*installer.Deps){breakCurrent}, setReq(hostSet("admin.example.com", "nginx-link", "yes"))...)
	if o.err == nil || kindOf(t, o.err) != installer.KindRolledBack {
		t.Fatalf("健康检查失败应回滚（KindRolledBack→退出码 10）: %v", o.err)
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" {
		t.Fatalf("回滚后系统状态与安装前不同（残留）:\n%s", d)
	}
	calls := w.NginxCalls()
	// 安装期 -t、reload；回滚时摘除软链后再 reload 一次。
	if len(calls) != 3 || calls[0] != "-t" || calls[1] != "-s reload" || calls[2] != "-s reload" {
		t.Errorf("nginx 调用序列应为 -t、reload（安装）、reload（回滚）: %v", calls)
	}
	for _, rel := range []string{"etc/nginx/conf.d/lingedge-admin-web.conf", "etc/lingedge/nginx/admin-web.conf", "opt/lingedge/admin-web"} {
		if w.Has(rel) {
			t.Errorf("回滚后不应残留 %s", rel)
		}
	}
}

// dry-run：只读、零变化；计划里有文件、片段与（被要求时的）软链步骤。
func TestStaticDryRunChangesNothing(t *testing.T) {
	w := staticWorld(t)
	if err := w.SetNativeNginx(); err != nil {
		t.Fatal(err)
	}
	before := fullSnap(t, w)
	in := w.NewInstaller("run-dry")
	pre, err := in.Plan(t.Context(), baseReq(w, "admin-web", func(r *installer.Request) {
		r.Settings = hostSet("admin.example.com", "nginx-link", "yes")
		r.DryRun = true
	}))
	if err != nil {
		t.Fatal(err)
	}
	if d := diffSnap(before, fullSnap(t, w)); d != "" {
		t.Fatalf("dry-run 不得改动任何东西:\n%s", d)
	}
	files := map[string]bool{}
	for _, f := range pre.Plan.Files {
		files[f.Path] = true
	}
	for _, p := range []string{"/etc/lingedge/nginx/admin-web.conf", "/etc/lingedge/nginx/admin-web-security-headers.conf", "/opt/lingedge/admin-web/current"} {
		if !files[p] {
			t.Errorf("计划应列出 %s", p)
		}
	}
	linkStep := false
	for _, s := range pre.Plan.Steps {
		linkStep = linkStep || (s.Kind == "start" && strings.Contains(s.Summary, "lingedge-admin-web.conf"))
	}
	if !linkStep || len(w.NginxCalls()) != 0 {
		t.Errorf("计划应含软链步骤且 dry-run 不得调用 nginx: %v %v", linkStep, w.NginxCalls())
	}
}

// 包布局不符（没有 public/index.html、顶层多出文件）：在落地之前被拒绝——第 14 步失败并回滚，没有任何 release 内容或配置残留。
func TestStaticBrokenPackageLayoutIsRejectedBeforeLanding(t *testing.T) {
	w, err := toolsworld.NewWorldWith(t.TempDir(), toolsworld.Options{Static: true, StaticBrokenLayout: true})
	if err != nil {
		t.Fatal(err)
	}
	before := sysSnap(t, w)
	o := doInstall(t, w, "docs", nil, setReq(hostSet("docs.example.com"))...)
	if o.err == nil || !strings.Contains(o.err.Error(), "包布局") {
		t.Fatalf("应因包布局被拒绝: %v", o.err)
	}
	var ie *installer.Error
	if !errors.As(o.err, &ie) || ie.Step != installer.StepExtract {
		t.Errorf("应在第 14 步失败: %+v", ie)
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" {
		t.Fatalf("被拒绝后系统树应与安装前相同:\n%s", d)
	}
	if w.Has("opt/lingedge/docs/releases/0.1.0") {
		t.Error("不合格的包不得落地")
	}
}

// 计划与执行之间，conf.d 里出现了别人的同名文件：绝不覆盖、不 reload，回滚也不会删它。
func TestStaticNginxLinkNeverOverwritesLateAppearingFile(t *testing.T) {
	w := staticWorld(t)
	if err := w.SetNativeNginx(); err != nil {
		t.Fatal(err)
	}
	theirs := "# 计划之后才出现的别人的文件\n"
	appear := func(d *installer.Deps) {
		d.Hook = func(point string) {
			if point == "before:19" {
				_ = os.WriteFile(linkPath(w), []byte(theirs), 0o644)
			}
		}
	}
	res := must(t, doInstall(t, w, "admin-web", []func(*installer.Deps){appear}, setReq(hostSet("admin.example.com", "nginx-link", "yes"))...))
	if b, _ := os.ReadFile(linkPath(w)); string(b) != theirs {
		t.Fatal("后出现的同名文件被覆盖了")
	}
	if len(w.NginxCalls()) != 0 {
		t.Errorf("不得 reload: %v", w.NginxCalls())
	}
	ok := false
	for _, wn := range res.Warnings {
		ok = ok || wn.Code == "nginx.link_conflict"
	}
	if !ok {
		t.Errorf("应有 nginx.link_conflict 警告: %+v", res.Warnings)
	}
}
