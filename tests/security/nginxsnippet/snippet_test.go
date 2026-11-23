// Package nginxsnippet 是 Story 1.40 AC5 的 CI 静态检查（tools-design §11.3「安全头继承规则」、IMP-17）：
// 对安装器**生成的**全部 Nginx 片段（5 个静态前端 × 设置组合）与仓库里的 golden 做静态检查——
// 含 add_header 的 location 必须显式 include 该组件的安全头文件、server 级不得 add_header、安全头文件每条带 always、不得改写 Cookie 的 Domain。
// 随 make security 运行（go test ./tests/security/...）。
package nginxsnippet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
)

func combos() []map[string]string {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"host": "x.example.com"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	return []map[string]string{
		base(),
		base("ipv6", "no"),
		base("tls-cert", "/etc/ssl/c.pem", "tls-key", "/etc/ssl/k.pem"),
		base("tls-cert", "/etc/ssl/c.pem", "tls-key", "/etc/ssl/k.pem", "http-port", "8080", "https-port", "8443"),
		base("api-upstream", "https://api.example.com", "api-ca", "/etc/ssl/ca.pem"),
		base("api-upstream", "http://[2001:db8::1]:9000"),
	}
}

func TestGeneratedSnippetsPassStaticChecks(t *testing.T) {
	n := 0
	for _, comp := range installer.StaticComponents() {
		for _, set := range combos() {
			st, err := installer.ParseStaticSettings(comp, set)
			if err != nil {
				continue // 该组件不认识这些键（docs/status 没有 API 反代）：组合不适用
			}
			conf, hdr, err := installer.RenderStaticSite(installer.SiteParams{Component: comp, Host: st.Host, APIUpstream: st.APIUpstream, APICA: st.APICA,
				TLSCert: st.TLSCert, TLSKey: st.TLSKey, HTTPPort: st.HTTPPort, HTTPSPort: st.HTTPSPort, IPv6: st.IPv6})
			if err != nil {
				t.Fatalf("%s %v: %v", comp, set, err)
			}
			n++
			if bad := installer.LintNginxSnippet(string(conf), installer.StaticHeadersPath(comp)); len(bad) > 0 {
				t.Errorf("%s %v：%v", comp, set, bad)
			}
			if bad := installer.LintSecurityHeaders(string(hdr)); len(bad) > 0 {
				t.Errorf("%s %v 的安全头文件：%v", comp, set, bad)
			}
			// 片段只能 include 自己组件的头文件（不得串到别的组件的文件）。
			for _, c := range installer.StaticComponents() {
				if c != comp && strings.Contains(string(conf), installer.StaticHeadersPath(c)) {
					t.Errorf("%s 的片段 include 了 %s 的头文件", comp, c)
				}
			}
		}
	}
	if n < 20 {
		t.Fatalf("只检查了 %d 个组合，检查没有覆盖到足够的生成结果", n)
	}
}

// golden（评审时人眼看到的样例）同样必须通过检查：golden 被误改成违规内容时，这里先于渲染测试之外再拦一道。
func TestGoldenSnippetsPassStaticChecks(t *testing.T) {
	root := filepath.Join("..", "..", "..", "internal", "tools", "installer", "testdata")
	files, err := filepath.Glob(filepath.Join(root, "static.*.conf"))
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到 golden: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Base(f)
		if strings.HasSuffix(base, ".headers.conf") {
			if bad := installer.LintSecurityHeaders(string(b)); len(bad) > 0 {
				t.Errorf("%s：%v", base, bad)
			}
			continue
		}
		comp := strings.Split(strings.TrimPrefix(base, "static."), ".")[0]
		if bad := installer.LintNginxSnippet(string(b), installer.StaticHeadersPath(comp)); len(bad) > 0 {
			t.Errorf("%s：%v", base, bad)
		}
	}
}

// 负向：把生成的合法片段改坏（删掉某个 location 的 include / 在 server 级加 add_header / 去掉 always），检查必须报出——证明检查本身有效，不是空转。
func TestStaticCheckCatchesMutations(t *testing.T) {
	st, _ := installer.ParseStaticSettings("admin-web", map[string]string{"host": "x.example.com"})
	conf, hdr, err := installer.RenderStaticSite(installer.SiteParams{Component: "admin-web", Host: st.Host, APIUpstream: st.APIUpstream, HTTPPort: 80, HTTPSPort: 443, IPv6: true})
	if err != nil {
		t.Fatal(err)
	}
	hf := installer.StaticHeadersPath("admin-web")
	c := string(conf)
	inc := "        include " + hf + ";\n"
	if strings.Count(c, inc) < 3 {
		t.Fatalf("样本里应有至少 3 处 include: %d", strings.Count(c, inc))
	}
	for i := 0; i < strings.Count(c, inc); i++ {
		mut := replaceNth(c, inc, "", i)
		if bad := installer.LintNginxSnippet(mut, hf); len(bad) == 0 {
			t.Errorf("删掉第 %d 处 include 应被检出", i)
		}
	}
	if bad := installer.LintNginxSnippet(strings.Replace(c, "    server_tokens off;\n", "    server_tokens off;\n    add_header X-Frame-Options DENY always;\n", 1), hf); len(bad) == 0 {
		t.Error("server 级 add_header 应被检出")
	}
	if bad := installer.LintSecurityHeaders(strings.Replace(string(hdr), `"nosniff" always;`, `"nosniff";`, 1)); len(bad) == 0 {
		t.Error("去掉 always 应被检出")
	}
}

func replaceNth(s, old, repl string, n int) string {
	idx := 0
	for i := 0; i <= n; i++ {
		j := strings.Index(s[idx:], old)
		if j < 0 {
			return s
		}
		if i == n {
			return s[:idx+j] + repl + s[idx+j+len(old):]
		}
		idx += j + len(old)
	}
	return s
}
