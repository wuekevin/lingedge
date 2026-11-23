package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.40：`lingedge install admin-web …` 的命令层行为（退出码、envelope、schema）。真实命令 + 真实 installer/生产静态前端规格；只有「操作系统」是伪的。

func staticHarness(t *testing.T) (*harness, *toolsworld.World) {
	t.Helper()
	w, err := toolsworld.NewWorldWith(t.TempDir(), toolsworld.Options{Static: true})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, asRoot())
	applyWorld(t, h, w)
	wr, err := service.NewWebServerRunner([]string{w.Bin})
	if err != nil {
		t.Fatal(err)
	}
	h.env.Install.WebRunner = wr
	return h, w
}

func TestInstallStaticFrontendCommand(t *testing.T) {
	h, w := staticHarness(t)
	if code := h.run("install", "admin-web", "--offline", w.Bundle, "--set", "host=admin.example.com", "--json"); code != 0 {
		t.Fatalf("退出码 %d\nstdout=%s\nstderr=%s", code, h.stdout, h.stderr)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.install.schema.json", env["data"]); err != nil {
		t.Fatalf("data 不符合 schema: %v\n%s", err, h.stdout)
	}
	d := env["data"].(map[string]any)
	if d["outcome"] != "installed" {
		t.Fatalf("%v", d)
	}
	kinds := map[string]bool{}
	for _, a := range d["access"].([]any) {
		kinds[a.(map[string]any)["kind"].(string)] = true
	}
	if !kinds["site_url"] || !kinds["nginx_include"] {
		t.Errorf("访问信息应含 site_url 与 nginx_include: %v", d["access"])
	}
	if _, err := os.Stat(filepath.Join(w.Root, "etc/lingedge/nginx/admin-web.conf")); err != nil {
		t.Fatalf("片段未生成: %v", err)
	}
	// 人类可读输出：站点地址与手工接入指引。
	h2, w2 := staticHarness(t)
	if code := h2.run("install", "docs", "--offline", w2.Bundle, "--set", "host=docs.example.com", "--non-interactive"); code != 0 {
		t.Fatalf("退出码 %d\n%s", code, h2.stderr)
	}
	if out := h2.stdout.String(); !strings.Contains(out, "http://docs.example.com/") || !strings.Contains(out, "/etc/lingedge/nginx/docs.conf") {
		t.Errorf("人类可读输出应含站点地址与片段路径:\n%s", out)
	}
	// list 能看到（状态 installed）。
	h.run("list", "--json")
	if !strings.Contains(h.stdout.String(), `"admin-web"`) {
		t.Errorf("list 应含 admin-web: %s", h.stdout)
	}
}

func TestInstallStaticFrontendExitCodes(t *testing.T) {
	// 缺 host、非法 host、未知键 → 2，且 envelope 给出稳定错误码。
	for _, set := range []string{"", "host=10.0.0.1", "host=a.example.com;return", "host=a.example.com,x=y"} {
		h, w := staticHarness(t)
		args := []string{"install", "admin-web", "--offline", w.Bundle, "--json", "--non-interactive"}
		if set != "" {
			args = append(args, "--set", set)
		}
		if code := h.run(args...); code != 2 {
			t.Errorf("--set %q 应退出 2，得 %d\n%s", set, code, h.stdout)
			continue
		}
		if c := errCode(h.envelope()); c != "usage.invalid_set" {
			t.Errorf("错误码 %q", c)
		}
	}

	// AC5：Admin 与官网同主机名 → 2，无绕过开关；换主机名即可。
	h, w := staticHarness(t)
	if code := h.run("install", "admin-web", "--offline", w.Bundle, "--set", "host=admin.example.com", "--non-interactive"); code != 0 {
		t.Fatalf("admin-web: %d\n%s", code, h.stderr)
	}
	if code := h.run("install", "website", "--mode", "static", "--offline", w.Bundle, "--set", "host=Admin.example.com", "--non-interactive", "--yes", "--json"); code != 2 {
		t.Fatalf("同主机名应退出 2，得 %d\n%s", code, h.stdout)
	}
	if msg := h.stdout.String(); !strings.Contains(msg, "admin-web") || !strings.Contains(msg, "互不相同") {
		t.Errorf("错误应点名占用者并说明规则: %q", msg)
	}
	if code := h.run("install", "website", "--mode", "static", "--offline", w.Bundle, "--set", "host=www.example.com", "--non-interactive"); code != 0 {
		t.Fatalf("不同主机名应成功: %d\n%s", code, h.stderr)
	}

	// AC3：同名 conf 已存在 → 9（不覆盖，列出冲突）。
	h3, w3 := staticHarness(t)
	p := filepath.Join(w3.Root, "etc/lingedge/nginx/console.conf")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := h3.run("install", "console", "--offline", w3.Bundle, "--set", "host=console.example.com", "--non-interactive", "--json"); code != 9 {
		t.Fatalf("同名 conf 应退出 9，得 %d\n%s", code, h3.stdout)
	}
	if b, _ := os.ReadFile(p); string(b) != "# mine\n" {
		t.Fatal("既有 conf 被覆盖")
	}
	// 同域名站点 → 9，冲突类型 site（schema 内）。
	h4, w4 := staticHarness(t)
	w4.Probe.Baota.Sites = []baota.Site{{File: "/www/server/panel/vhost/nginx/x.conf", Source: "baota", ServerNames: []string{"status.example.com"}}}
	if code := h4.run("install", "status", "--offline", w4.Bundle, "--set", "host=status.example.com", "--non-interactive", "--json"); code != 9 {
		t.Fatalf("同域名站点应退出 9，得 %d\n%s", code, h4.stdout)
	}
	if !strings.Contains(h4.stdout.String(), `"site"`) {
		t.Errorf("冲突应含 site 类型: %s", h4.stdout)
	}
}

func TestInstallStaticFrontendDryRunAndNginxLink(t *testing.T) {
	h, w := staticHarness(t)
	if err := w.SetNativeNginx(); err != nil {
		t.Fatal(err)
	}
	// dry-run：列出计划（含片段文件），没有任何副作用。
	if code := h.run("install", "admin-web", "--offline", w.Bundle, "--set", "host=admin.example.com", "--set", "nginx-link=yes", "--dry-run", "--json"); code != 0 {
		t.Fatalf("dry-run: %d\n%s", code, h.stdout)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.install.schema.json", env["data"]); err != nil {
		t.Fatalf("schema: %v\n%s", err, h.stdout)
	}
	if !strings.Contains(h.stdout.String(), "/etc/lingedge/nginx/admin-web.conf") || !strings.Contains(h.stdout.String(), "软链") {
		t.Errorf("计划应含片段文件与软链步骤:\n%s", h.stdout)
	}
	if _, err := os.Stat(filepath.Join(w.Root, "etc/lingedge")); err == nil {
		t.Error("dry-run 不得创建 /etc/lingedge")
	}
	// 真实执行：软链 + nginx -t + reload。
	if code := h.run("install", "admin-web", "--offline", w.Bundle, "--set", "host=admin.example.com", "--set", "nginx-link=yes", "--non-interactive", "--json"); code != 0 {
		t.Fatalf("install: %d\n%s", code, h.stdout)
	}
	if tg, err := os.Readlink(filepath.Join(w.Root, "etc/nginx/conf.d/lingedge-admin-web.conf")); err != nil || tg != "/etc/lingedge/nginx/admin-web.conf" {
		t.Errorf("软链: %q %v", tg, err)
	}
	if c := w.NginxCalls(); len(c) != 2 || c[0] != "-t" || c[1] != "-s reload" {
		t.Errorf("nginx 调用: %v", c)
	}
}
