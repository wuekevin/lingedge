package httpx_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// syncBuf 是并发安全的日志缓冲。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// newRouter 构造使用真实 logx（脱敏核心）与真实 ULID 生成器的 Router。
func newRouter(t testing.TB, o httpx.Options) (*httpx.Router, *syncBuf) {
	t.Helper()
	buf := &syncBuf{}
	if o.Logger == nil {
		l, err := logx.New(logx.Options{Level: "debug", Output: buf})
		if err != nil {
			t.Fatal(err)
		}
		o.Logger = l
	}
	if o.IDs == nil {
		o.IDs = uid.NewGenerator(clock.System())
	}
	r, err := httpx.NewRouter(o)
	if err != nil {
		t.Fatal(err)
	}
	return r, buf
}

func handler(t testing.TB, r *httpx.Router) http.Handler {
	t.Helper()
	h, err := r.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return h
}

func do(h http.Handler, method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// testPrincipal 是测试用的主体实现（tenantscope.Principal 的真实实现，不是 mock 框架）。
type testPrincipal struct {
	actor, tenant uint64
	platform      bool
	perms         map[string]bool
}

func (p *testPrincipal) ActorID() uint64             { return p.actor }
func (p *testPrincipal) IsPlatform() bool            { return p.platform }
func (p *testPrincipal) TenantID() uint64            { return p.tenant }
func (p *testPrincipal) HasPermission(s string) bool { return p.perms[s] }

// 允许型钩子仅用于测试链行为，生产默认是 Deny*。
type orderHooks struct {
	mu    sync.Mutex
	trace []string
	p     tenantscope.Principal
}

func (o *orderHooks) add(s string) { o.mu.Lock(); o.trace = append(o.trace, s); o.mu.Unlock() }
func (o *orderHooks) get() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.trace...)
}

func (o *orderHooks) Authenticate(r *http.Request, m httpx.RouteMeta) (tenantscope.Principal, error) {
	o.add("authn")
	return o.p, nil
}
func (o *orderHooks) Authorize(r *http.Request, m httpx.RouteMeta, p tenantscope.Principal) error {
	o.add("authz")
	return nil
}
func (o *orderHooks) ResolveScope(r *http.Request, m httpx.RouteMeta, p tenantscope.Principal) (tenantscope.Scope, error) {
	o.add("scope")
	return tenantscope.FromPrincipal(p, tenantscope.RouteTenant, tenantscope.NoTarget())
}
func (o *orderHooks) Begin(r *http.Request, m httpx.RouteMeta) (func(httpx.AuditOutcome), error) {
	o.add("audit")
	return func(httpx.AuditOutcome) { o.add("audit-end") }, nil
}

func (o *orderHooks) hooks() httpx.Hooks {
	return httpx.Hooks{Authenticator: o, Authorizer: o, Scope: o, Auditor: o}
}

func okHandler(body string) gin.HandlerFunc {
	return func(c *gin.Context) { httpx.JSON(c, http.StatusOK, map[string]string{"v": body}) }
}

func meta(id, method, path string) httpx.RouteMeta {
	return httpx.RouteMeta{
		OperationID: id, Method: method, Path: path, Auth: httpx.AuthRequired, Permissions: []string{"site:read"},
		TenantScope: httpx.ScopeTenant, Audit: true, Phase: "M1",
	}
}

func newIDs() *uid.Generator { return uid.NewGenerator(clock.System()) }
