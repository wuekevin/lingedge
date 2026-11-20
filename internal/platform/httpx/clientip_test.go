package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// ClientIPFrom 与 gin 的 ClientIP 同源（遵守 TrustedProxies）：来自可信反代的连接采信 X-Forwarded-For，来自不可信来源的连接则忽略该头
// （伪造 X-Forwarded-For 不能改变来源 IP——API Token 的 allowed_cidrs 依赖这一点，Story 1.19）。
func TestClientIPFollowsTrustedProxies(t *testing.T) {
	cases := []struct {
		name    string
		trusted []string
		remote  string
		xff     string
		want    string
	}{
		{"可信反代 + XFF → 采信", []string{"127.0.0.1"}, "127.0.0.1:1234", "203.0.113.9", "203.0.113.9"},
		{"可信反代 + 多级 XFF → 取最右侧的非可信地址", []string{"127.0.0.1"}, "127.0.0.1:1234", "198.51.100.1, 203.0.113.9", "203.0.113.9"},
		{"不可信来源伪造 XFF → 忽略", []string{"127.0.0.1"}, "192.0.2.7:4321", "10.0.0.1", "192.0.2.7"},
		{"无 XFF → 对端地址", []string{"127.0.0.1"}, "192.0.2.7:4321", "", "192.0.2.7"},
		{"IPv6 对端", []string{"127.0.0.1"}, "[2001:db8::7]:4321", "10.0.0.1", "2001:db8::7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newRouter(t, httpx.Options{TrustedProxies: tc.trusted, Hooks: (&orderHooks{p: &testPrincipal{actor: 1, tenant: 1}}).hooks()})
			var got string
			if err := r.Register(meta("listSites", "GET", "/sites"), func(c *gin.Context) {
				got = httpx.ClientIPFrom(c.Request.Context())
				httpx.OK(c, map[string]bool{"ok": true})
			}); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/sites", nil)
			req.RemoteAddr = tc.remote
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			rec := httptest.NewRecorder()
			handler(t, r).ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("状态 %d: %s", rec.Code, rec.Body)
			}
			if got != tc.want {
				t.Errorf("ClientIPFrom = %q，期望 %q", got, tc.want)
			}
		})
	}
	if httpx.ClientIPFrom(httptest.NewRequest("GET", "/", nil).Context()) != "" {
		t.Error("不经路由器的请求应返回空串（来源未知）")
	}
}
