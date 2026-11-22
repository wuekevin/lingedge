package setup

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

func pageRouter(t *testing.T, installed bool) http.Handler {
	t.Helper()
	opts := Options{Installed: installed}
	if !installed {
		opts = Options{Installed: false}
	}
	svc := &Service{o: opts}
	r, err := httpx.NewRouter(httpx.Options{IDs: uid.NewGenerator(clock.System()), Hooks: httpx.Hooks{Auditor: allowAuditor{}}})
	if err != nil {
		t.Fatal(err)
	}
	if installed {
		err = svc.RegisterGonePages(r)
	} else {
		err = svc.RegisterPage(r)
	}
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

// 内嵌的向导前端是真实的 SPA 生产构建（不是占位页）：入口存在、引用的资源都在、无内联脚本、无外链、无 sourcemap。
func TestEmbeddedWizardIsARealBuild(t *testing.T) {
	root, err := webFiles()
	if err != nil {
		t.Fatalf("内嵌产物校验失败: %v", err)
	}
	index, _ := fs.ReadFile(root, "index.html")
	s := string(index)
	if !strings.Contains(s, `<div id="app">`) {
		t.Error("入口缺少挂载点 #app")
	}
	for _, bad := range []string{"<script>", "http://", "https://", "onclick", "javascript:"} {
		if strings.Contains(s, bad) {
			t.Errorf("入口不应含 %q", bad)
		}
	}
	var js, css int
	_ = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := fs.ReadFile(root, p)
		switch {
		case strings.HasSuffix(p, ".js"):
			js++
			if len(b) < 50_000 {
				t.Errorf("%s 只有 %d 字节：不像真实的 Vue 构建（占位页？）", p, len(b))
			}
			if regexp.MustCompile(`sourceMappingURL`).Match(b) {
				t.Errorf("%s 含 sourcemap 引用", p)
			}
			for _, m := range []string{"LE_MOCK_ENTRY", "mockServiceWorker", "__LE_MOCK__"} {
				if strings.Contains(string(b), m) {
					t.Errorf("%s 含 mock 标记 %s", p, m)
				}
			}
			if !strings.Contains(string(b), "/api/v1/public/setup") {
				t.Errorf("%s 没有向导 API 前缀", p)
			}
		case strings.HasSuffix(p, ".css"):
			css++
		}
		return nil
	})
	if js == 0 || css == 0 {
		t.Errorf("应至少有 1 个 js 与 1 个 css，实际 js=%d css=%d", js, css)
	}
}

// verifyWeb 的缺失检查：入口缺失、引用的资源缺失、内联脚本、无脚本都必须失败。
func TestVerifyWebRejectsBrokenBuilds(t *testing.T) {
	good := `<!doctype html><script type="module" src="/setup/assets/a.js"></script><link rel="stylesheet" href="/setup/assets/a.css"><div id="app"></div>`
	ok := fstest.MapFS{"index.html": {Data: []byte(good)}, "assets/a.js": {Data: []byte("x")}, "assets/a.css": {Data: []byte("x")}}
	if err := verifyWeb(ok); err != nil {
		t.Fatalf("完整产物应通过: %v", err)
	}
	cases := map[string]fstest.MapFS{
		"无入口":         {"assets/a.js": {Data: []byte("x")}},
		"空入口":         {"index.html": {Data: []byte("  ")}},
		"资源缺失":        {"index.html": {Data: []byte(good)}, "assets/a.js": {Data: []byte("x")}},
		"资源为空":        {"index.html": {Data: []byte(good)}, "assets/a.js": {Data: []byte("x")}, "assets/a.css": {Data: []byte{}}},
		"无任何引用":       {"index.html": {Data: []byte(`<div id="app"></div>`)}},
		"只有样式":        {"index.html": {Data: []byte(`<link rel="stylesheet" href="/setup/assets/a.css">`)}, "assets/a.css": {Data: []byte("x")}},
		"内联脚本":        {"index.html": {Data: []byte(good + `<script>alert(1)</script>`)}, "assets/a.js": {Data: []byte("x")}, "assets/a.css": {Data: []byte("x")}},
		"javascript:": {"index.html": {Data: []byte(good + `<a href="javascript:void(0)">`)}, "assets/a.js": {Data: []byte("x")}, "assets/a.css": {Data: []byte("x")}},
	}
	for name, fsys := range cases {
		if err := verifyWeb(fsys); err == nil {
			t.Errorf("%s：应被拒绝", name)
		}
	}
}

// setup mode：/setup 返回入口（严格 CSP、no-store），资源带正确类型与长缓存；路径穿越/未知资源 404；只接受 GET。
func TestSetupModeServesWizard(t *testing.T) {
	h := pageRouter(t, false)
	for _, p := range []string{"/setup", "/setup/"} {
		w := get(h, "GET", p)
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d %s", p, w.Code, w.Header().Get("Content-Type"))
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s 入口必须 no-store: %q", p, w.Header().Get("Cache-Control"))
		}
		csp := w.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("CSP 缺 %q: %s", want, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("CSP 不得含 unsafe-*: %s", csp)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("安全响应头不全: %v", w.Header())
		}
	}
	index := get(h, "GET", "/setup").Body.String()
	for _, m := range reAssetRef.FindAllStringSubmatch(index, -1) {
		w := get(h, "GET", m[1])
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("%s: %d", m[1], w.Code)
		}
		want := map[string]string{".js": "text/javascript", ".css": "text/css"}[m[1][strings.LastIndex(m[1], "."):]]
		if !strings.HasPrefix(w.Header().Get("Content-Type"), want) {
			t.Errorf("%s 的 Content-Type=%q", m[1], w.Header().Get("Content-Type"))
		}
		if !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("%s 应可长缓存: %q", m[1], w.Header().Get("Cache-Control"))
		}
	}
	for _, p := range []string{"/setup/assets/nope.js", "/setup/assets/", "/setup/assets/../index.html", "/setup/assets/%2e%2e/index.html", "/setup/assets/..%2findex.html"} {
		if w := get(h, "GET", p); w.Code == 200 && strings.Contains(w.Body.String(), `<div id="app">`) {
			t.Errorf("%s 不应越出 assets 目录取到入口", p)
		} else if w.Code == 200 {
			t.Errorf("%s 应 404，得到 %d", p, w.Code)
		}
	}
	if w := get(h, "POST", "/setup"); w.Code == 200 {
		t.Error("/setup 只接受 GET")
	}
}

// 正常模式（install.lock 之后）：/setup 与 /setup/* 一律 410 setup.already_installed（AC3/AC5，MIN-04），不泄漏页面内容。
func TestInstalledModeGonePages(t *testing.T) {
	h := pageRouter(t, true)
	for _, p := range []string{"/setup", "/setup/", "/setup/assets/index-x.js", "/setup/assets/"} {
		w := get(h, "GET", p)
		if w.Code != http.StatusGone {
			t.Fatalf("%s 应 410，实际 %d", p, w.Code)
		}
		var body struct {
			Error struct{ Code string } `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != "setup.already_installed" {
			t.Errorf("%s: 错误体 %q（%v）", p, w.Body.String(), err)
		}
		if strings.Contains(w.Body.String(), "app") && strings.Contains(w.Body.String(), "<div") {
			t.Errorf("%s 不应返回页面内容", p)
		}
	}
}
