package httpx_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// AC3：错误 → HTTP 状态映射（经真实的 Gin 路由与统一错误体）。
func TestErrorMappingThroughRouter(t *testing.T) {
	oh := &orderHooks{p: &testPrincipal{actor: 1, tenant: 1}}
	r, logs := newRouter(t, httpx.Options{Hooks: oh.hooks()})
	cases := []struct {
		path   string
		err    error
		status int
		code   string
	}{
		{"validation", apierr.Validation(apierr.Detail{Field: "name", Reason: "required"}), 400, "validation.failed"},
		{"notfound", apierr.NotFound(), 404, "resource.not_found"},
		{"dbnotfound", fmt.Errorf("repo: %w", db.ErrNotFound), 404, "resource.not_found"}, // 越权/不存在
		{"precondition", apierr.PreconditionFailed(), 412, "resource.precondition_failed"},
		{"conflict", apierr.Conflict("state does not allow"), 409, "resource.conflict"},
		{"keyreuse", apierr.KeyReuse(), 422, "idempotency.key_reuse"},
		{"ratelimit", apierr.New(apierr.RateLimitExceeded, "slow down").WithRetryAfter(1500 * 1e6), 429, "rate_limit.exceeded"},
		{"unknown", errors.New("pq: connection refused to 10.0.0.5 password=hunter2"), 500, "internal.error"},
	}
	for i, c := range cases {
		c := c
		if err := r.Register(meta(fmt.Sprintf("op%d", i), "GET", "/e/"+c.path), func(ctx *gin.Context) { httpx.Fail(ctx, c.err) }); err != nil {
			t.Fatal(err)
		}
	}
	h := handler(t, r)
	for _, c := range cases {
		rec := do(h, "GET", "/api/v1/e/"+c.path, "")
		e := decodeErr(t, rec.Body.Bytes())
		if rec.Code != c.status || e.Code != c.code || !ulidRe.MatchString(e.RequestID) {
			t.Errorf("%s: 得到 %d %s，期望 %d %s", c.path, rec.Code, e.Code, c.status, c.code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: Content-Type=%q", c.path, ct)
		}
		if c.status == 500 && (strings.Contains(rec.Body.String(), "10.0.0.5") || strings.Contains(rec.Body.String(), "hunter2")) {
			t.Errorf("500 响应泄漏原因: %s", rec.Body)
		}
	}
	if rec := do(h, "GET", "/api/v1/e/ratelimit", ""); rec.Header().Get("Retry-After") != "2" {
		t.Errorf("429 应带 Retry-After: %q", rec.Header().Get("Retry-After"))
	}
	if strings.Contains(logs.String(), "hunter2") {
		t.Errorf("日志泄漏口令: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "请求处理失败") {
		t.Error("5xx 原因应进日志")
	}
}

func TestBindJSONStrict(t *testing.T) {
	type req struct {
		Name string `json:"name"`
		Size int    `json:"size"`
	}
	oh := &orderHooks{p: &testPrincipal{actor: 1, tenant: 1}}
	r, _ := newRouter(t, httpx.Options{Hooks: oh.hooks(), MaxBodyBytes: 64})
	if err := r.Register(meta("createThing", "POST", "/things"), func(c *gin.Context) {
		var in req
		if err := httpx.BindJSON(c, &in); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.Created(c, "", in)
	}); err != nil {
		t.Fatal(err)
	}
	h := handler(t, r)
	jh := []string{"Content-Type", "application/json"}
	if rec := do(h, "POST", "/api/v1/things", `{"name":"a","size":2}`, jh...); rec.Code != 201 || !strings.Contains(rec.Body.String(), `"name":"a"`) {
		t.Fatalf("合法请求: %d %s", rec.Code, rec.Body)
	}
	bad := []struct {
		name, body, field string
		hdr               []string
	}{
		{"未知字段", `{"name":"a","bogus":1}`, "bogus", jh},
		{"类型错误", `{"name":"a","size":"x"}`, "size", jh},
		{"语法错误", `{"name":`, "body", jh},
		{"空体", ``, "body", jh},
		{"多余内容", `{"name":"a"} {"name":"b"}`, "body", jh},
		{"错误 Content-Type", `{"name":"a"}`, "Content-Type", []string{"Content-Type", "text/plain"}},
		{"缺 Content-Type", `{"name":"a"}`, "Content-Type", nil},
		{"超限", `{"name":"` + strings.Repeat("a", 100) + `"}`, "body", jh},
	}
	for _, b := range bad {
		rec := do(h, "POST", "/api/v1/things", b.body, b.hdr...)
		e := decodeErr(t, rec.Body.Bytes())
		ok := rec.Code == 400 && e.Code == "validation.failed"
		found := false
		for _, d := range e.Details {
			found = found || d.Field == b.field
		}
		if !ok || !found {
			t.Errorf("%s: %d %+v", b.name, rec.Code, e)
		}
		if strings.Contains(rec.Body.String(), "aaaa") {
			t.Errorf("%s: 错误体回显了请求内容", b.name)
		}
	}
}

func TestCheckIfMatch(t *testing.T) {
	oh := &orderHooks{p: &testPrincipal{actor: 1, tenant: 1}}
	r, _ := newRouter(t, httpx.Options{Hooks: oh.hooks()})
	if err := r.Register(meta("updateThing", "PUT", "/things/{id}"), func(c *gin.Context) {
		if err := httpx.CheckIfMatch(c, "7"); err != nil {
			httpx.Fail(c, err)
			return
		}
		httpx.NoContent(c)
	}); err != nil {
		t.Fatal(err)
	}
	h := handler(t, r)
	for _, c := range []struct {
		hdr    string
		status int
	}{{"", 204}, {`"7"`, 204}, {`W/"7"`, 204}, {"7", 204}, {"*", 204}, {`"6", "7"`, 204}, {`"6"`, 412}, {`"70"`, 412}, {`""`, 412}} {
		var hdr []string
		if c.hdr != "" {
			hdr = []string{"If-Match", c.hdr}
		}
		rec := do(h, "PUT", "/api/v1/things/1", "", hdr...)
		if rec.Code != c.status {
			t.Errorf("If-Match %q → %d，期望 %d", c.hdr, rec.Code, c.status)
		}
		if c.status == 412 && decodeErr(t, rec.Body.Bytes()).Code != "resource.precondition_failed" {
			t.Errorf("412 应为 resource.precondition_failed: %s", rec.Body)
		}
	}
	_ = http.StatusOK
}
