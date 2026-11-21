package contracttest_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	"github.com/wuekevin/lingedge/internal/platform/idempotency"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// 契约里的全部 operation 都能被生成出合法的 RouteMeta 并注册进 Gin 路由树（无通配符冲突、无冒号、无重复）。
// 这是对「openapi.yaml 路径」与「路由注册层」两端的一致性回归：以后契约里加了会让 Gin 冲突的路径，这里立刻失败。
func TestEveryOperationRegistersCleanly(t *testing.T) {
	spec := contracttest.LoadRepo(t)
	ids := spec.OperationIDs()
	if len(ids) < 400 {
		t.Fatalf("operation 数量异常: %d", len(ids))
	}
	// idempotency.Store 仅为让 Idempotent 路由通过 Register 的前置检查；本测试从不发请求，因此不会被调用。
	r, err := httpx.NewRouter(httpx.Options{IDs: uid.NewGenerator(clock.System()), Idempotency: struct{ idempotency.Store }{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range spec.Expected() {
		if strings.Contains(m.Path, ":") {
			t.Errorf("%s: 契约路径含 ':'（IMP-21）: %s", m.OperationID, m.Path)
		}
		if err := r.Register(m, func(c *gin.Context) {}); err != nil {
			t.Errorf("%s %s 注册失败: %v", m.Method, m.Path, err)
		}
	}
	if _, err := r.Handler(); err != nil {
		t.Fatalf("Handler: %v", err)
	}
	// 注册进去的就是契约本身：运行时集合 ⊆ openapi 且元数据一致；未实现清单 = 空（因为我们全注册了）。
	if probs := spec.CheckRoutes(r.Routes()); len(probs) != 0 {
		t.Errorf("元数据与契约不一致: %v", probs)
	}
	if left := spec.Unimplemented(r.Routes(), "M1"); len(left) != 0 {
		t.Errorf("全量注册后未实现清单应为空: %v", left)
	}
}

func TestCheckRoutesNegativeSamples(t *testing.T) {
	spec := contracttest.LoadRepo(t)
	op, _ := spec.Operation("login")
	base := op.Meta

	if p := spec.CheckRoutes([]httpx.RouteMeta{base}); len(p) != 0 {
		t.Fatalf("契约原样不应有问题: %v", p)
	}
	mut := func(f func(m *httpx.RouteMeta)) []string {
		m := base
		f(&m)
		return spec.CheckRoutes([]httpx.RouteMeta{m})
	}
	for name, probs := range map[string][]string{
		"未知 operationId": spec.CheckRoutes([]httpx.RouteMeta{{OperationID: "notInSpec"}}),
		"路径不一致":          mut(func(m *httpx.RouteMeta) { m.Path = "/auth/signin" }),
		"方法不一致":          mut(func(m *httpx.RouteMeta) { m.Method = "PUT" }),
		"权限不一致":          mut(func(m *httpx.RouteMeta) { m.Permissions = []string{"site:read"} }),
		"租户范围不一致":        mut(func(m *httpx.RouteMeta) { m.TenantScope = httpx.ScopeEither }),
		"审计不一致":          mut(func(m *httpx.RouteMeta) { m.Audit = !m.Audit }),
		"再认证不一致":         mut(func(m *httpx.RouteMeta) { m.Reauth = true }),
		"阶段不一致":          mut(func(m *httpx.RouteMeta) { m.Phase = "P2" }),
		"功能开关不一致":        mut(func(m *httpx.RouteMeta) { m.Feature = "commercial" }),
		"认证不一致":          mut(func(m *httpx.RouteMeta) { m.Auth = httpx.AuthRequired }),
	} {
		if len(probs) == 0 {
			t.Errorf("%s 应被检出", name)
		}
	}
	// 未实现清单：没有任何路由时恰为全部 M1 operation。
	all := spec.Unimplemented(nil, "M1")
	if len(all) != 200 {
		t.Errorf("M1 operation 应为 200 个（CF-1/CF-2 冻结口径）: %d", len(all))
	}
	if left := spec.Unimplemented([]httpx.RouteMeta{base}, "M1"); len(left) != len(all)-1 {
		t.Errorf("注册 login 后应少一个: %d", len(left))
	}
}

// 响应契约助手：真实路由产生的响应通过契约；偏离契约（未声明状态码、body 结构错误、缺 X-Request-ID）失败。
func TestValidateResponseHelper(t *testing.T) {
	spec := contracttest.LoadRepo(t)
	op, _ := spec.Operation("login")
	// 测试用的允许型审计钩子；login 是公开路由 + x-audit=true。
	r, err := httpx.NewRouter(httpx.Options{IDs: uid.NewGenerator(clock.System()), Hooks: httpx.Hooks{Auditor: allowAudit{}}})
	if err != nil {
		t.Fatal(err)
	}
	var respond func(c *gin.Context)
	if err := r.Register(op.Meta, func(c *gin.Context) { respond(c) }); err != nil {
		t.Fatal(err)
	}
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}
	call := func() (*http.Request, *httptest.ResponseRecorder) {
		req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return req, rec
	}

	// 统一错误体符合 openapi 的 BadRequest / TooManyRequests / InternalError。
	for name, c := range map[string]struct {
		err    error
		status int
	}{
		"400": {apierr.Validation(apierr.Detail{Field: "email", Reason: "required"}), 400},
		"429": {apierr.New(apierr.RateLimitExceeded, "slow down").WithRetryAfter(30e9), 429},
		"500": {apierr.Internal(http.ErrAbortHandler), 500},
	} {
		respond = func(ctx *gin.Context) { httpx.Fail(ctx, c.err) }
		req, rec := call()
		if rec.Code != c.status {
			t.Fatalf("%s: 状态 %d", name, rec.Code)
		}
		spec.AssertResponse(t, "login", req, rec.Result())
	}

	// 负向：以下偏离必须被助手检出（用 ValidateResponse 直接断言返回 error，而不是让测试失败）。
	respond = func(ctx *gin.Context) { httpx.Fail(ctx, apierr.NotFound()) }
	req, rec := call() // 404 不在 login 声明的状态码里
	if err := spec.ValidateResponse("login", req, rec.Code, rec.Header(), rec.Body.Bytes()); err == nil {
		t.Error("未声明的状态码 404 应被检出")
	}
	respond = func(ctx *gin.Context) {
		httpx.JSON(ctx, 400, map[string]string{"error": "plain string, not the unified body"})
	}
	req, rec = call()
	if err := spec.ValidateResponse("login", req, rec.Code, rec.Header(), rec.Body.Bytes()); err == nil {
		t.Error("不符合 ErrorResponse schema 的 body 应被检出")
	}
	respond = func(ctx *gin.Context) { httpx.Fail(ctx, apierr.Validation()) }
	req, rec = call()
	rec.Header().Del("X-Request-ID")
	if err := spec.ValidateResponse("login", req, rec.Code, rec.Header(), rec.Body.Bytes()); err == nil || !strings.Contains(err.Error(), "X-Request-ID") {
		t.Errorf("缺少 X-Request-ID 应被检出: %v", err)
	}
	if err := spec.ValidateResponse("noSuchOperation", req, 200, nil, nil); err == nil {
		t.Error("未知 operationId 应报错")
	}
}

type allowAudit struct{}

func (allowAudit) Begin(*http.Request, httpx.RouteMeta) (func(httpx.AuditOutcome), error) {
	return nil, nil
}
