package httpx_test

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// HandlePage 只允许 /setup 及其子路径：不能借它绕开 Register 的元数据校验注册任意路径；SetFallback 替换未匹配路由的响应。
func TestHandlePageOnlySetupPrefix(t *testing.T) {
	ok := func(c *gin.Context) { c.String(200, "ok") }
	for _, p := range []string{"/setup", "/setup/assets/app.js"} {
		r, _ := newRouter(t, httpx.Options{})
		if err := r.HandlePage(p, ok); err != nil {
			t.Errorf("%s 应允许: %v", p, err)
		}
	}
	for _, p := range []string{"/", "/admin", "/api/v1/x", "/setupx", "/healthz/x", ""} {
		r, _ := newRouter(t, httpx.Options{})
		err := r.HandlePage(p, ok)
		if err == nil {
			t.Errorf("%q 应被拒绝", p)
			continue
		}
		if _, herr := r.Handler(); herr == nil {
			t.Errorf("%q: 注册错误必须使 Handler() 失败（启动失败，NFR-4）", p)
		}
	}
	r, _ := newRouter(t, httpx.Options{})
	if err := r.HandlePage("/setup", nil); err == nil {
		t.Error("handler 为空应拒绝")
	}
	// 页面不进入路由清单（不属于 openapi）。
	r, _ = newRouter(t, httpx.Options{})
	if err := r.HandlePage("/setup", ok); err != nil {
		t.Fatal(err)
	}
	if len(r.Routes()) != 0 {
		t.Errorf("页面不应出现在路由清单中: %v", r.Routes())
	}
	h := handler(t, r)
	if w := do(h, "GET", "/setup", ""); w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Errorf("页面应可访问: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/setup", ""); w.Code != http.StatusNotFound {
		t.Errorf("页面只接受 GET: %d", w.Code)
	}
	// SetFallback：未匹配路由与不支持的方法统一走自定义响应。
	r, _ = newRouter(t, httpx.Options{})
	r.SetFallback(func(c *gin.Context) { c.String(http.StatusServiceUnavailable, "fb") })
	h = handler(t, r)
	for _, tc := range [][2]string{{"GET", "/nope"}, {"POST", "/api/v1/whatever"}} {
		if w := do(h, tc[0], tc[1], ""); w.Code != 503 || w.Body.String() != "fb" {
			t.Errorf("%v 应走自定义 fallback: %d %s", tc, w.Code, w.Body.String())
		}
	}
}
