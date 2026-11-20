package httpxintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	idemrepo "github.com/wuekevin/lingedge/internal/platform/idempotency/repository"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// ---- 测试用钩子：按 X-Actor 头构造租户主体（tenantscope.Principal 的真实实现），审计钩子记录结果 ----

type principal struct{ actor uint64 }

func (p principal) ActorID() uint64           { return p.actor }
func (p principal) IsPlatform() bool          { return false }
func (p principal) TenantID() uint64          { return p.actor }
func (p principal) HasPermission(string) bool { return true }

type testHooks struct {
	mu       sync.Mutex
	audits   []httpx.AuditOutcome
	auditBeg atomic.Int64
}

func (h *testHooks) Authenticate(r *http.Request, _ httpx.RouteMeta) (tenantscope.Principal, error) {
	n, err := strconv.ParseUint(r.Header.Get("X-Actor"), 10, 64)
	if err != nil || n == 0 {
		return nil, apierr.New(apierr.AuthUnauthenticated, "no actor")
	}
	return principal{n}, nil
}
func (h *testHooks) Authorize(*http.Request, httpx.RouteMeta, tenantscope.Principal) error {
	return nil
}
func (h *testHooks) ResolveScope(_ *http.Request, _ httpx.RouteMeta, p tenantscope.Principal) (tenantscope.Scope, error) {
	return tenantscope.FromTask(p.TenantID(), p.ActorID())
}
func (h *testHooks) Begin(*http.Request, httpx.RouteMeta) (func(httpx.AuditOutcome), error) {
	h.auditBeg.Add(1)
	return func(o httpx.AuditOutcome) { h.mu.Lock(); h.audits = append(h.audits, o); h.mu.Unlock() }, nil
}
func (h *testHooks) auditOutcomes() []httpx.AuditOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]httpx.AuditOutcome(nil), h.audits...)
}

// ---- 夹具 ----

type body struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

type fixture struct {
	t     *testing.T
	e     *env
	clk   *fakeClock
	srv   http.Handler
	hooks *testHooks
	execs atomic.Int64
	gate  chan struct{} // mode=block 的 handler 等待它被关闭
	start chan struct{} // mode=block 的 handler 开始执行时发信号
	ids   *uid.Generator
	logs  *bytes.Buffer
	mu    sync.Mutex
}

func (f *fixture) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs.Write(p)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, e: newEnv(t), clk: newFakeClock(), hooks: &testHooks{}, gate: make(chan struct{}), start: make(chan struct{}, 8),
		ids: uid.NewGenerator(clock.System()), logs: &bytes.Buffer{}}
	store, err := idemrepo.New(f.e.app, idemrepo.Options{Clock: f.clk})
	if err != nil {
		t.Fatal(err)
	}
	lg, err := logx.New(logx.Options{Level: "debug", Output: f})
	if err != nil {
		t.Fatal(err)
	}
	r, err := httpx.NewRouter(httpx.Options{
		Logger: lg, Clock: f.clk, IDs: f.ids, Idempotency: store,
		Hooks: httpx.Hooks{Authenticator: f.hooks, Authorizer: f.hooks, Scope: f.hooks, Auditor: f.hooks},
	})
	if err != nil {
		t.Fatal(err)
	}
	post := func(id, path string) httpx.RouteMeta {
		return httpx.RouteMeta{OperationID: id, Method: "POST", Path: path, Auth: httpx.AuthRequired, Permissions: []string{"thing:write"},
			TenantScope: httpx.ScopeTenant, Audit: true, Idempotent: true, Phase: "M1"}
	}
	for _, m := range []httpx.RouteMeta{post("createThing", "/things"), post("createOther", "/others")} {
		if err := r.Register(m, f.handle); err != nil {
			t.Fatal(err)
		}
	}
	if f.srv, err = r.Handler(); err != nil {
		t.Fatal(err)
	}
	return f
}

// handle 按 body.mode 表现：ok 201；bad 400；fail 500；panic；block 等 gate；task 202 + 真实 tasks 行。
func (f *fixture) handle(c *gin.Context) {
	var in body
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	n := f.execs.Add(1)
	switch in.Mode {
	case "bad":
		httpx.Fail(c, apierr.Validation(apierr.Detail{Field: "name", Reason: "bad"}))
	case "fail":
		httpx.Fail(c, fmt.Errorf("db exploded password=hunter2"))
	case "panic":
		panic("boom")
	case "block":
		f.start <- struct{}{}
		<-f.gate
		httpx.Created(c, "/api/v1/things/"+strconv.FormatInt(n, 10), map[string]any{"n": n, "name": in.Name})
	case "slow":
		time.Sleep(150 * time.Millisecond)
		httpx.Created(c, "/api/v1/things/"+strconv.FormatInt(n, 10), map[string]any{"n": n, "name": in.Name})
	case "task":
		rid, _ := requestid.FromContext(c.Request.Context())
		tuid := f.ids.MustNew()
		f.e.root(f.t, "INSERT INTO §.tasks (uid, type, payload_json, request_id) VALUES (?, 'cache.purge_url', '{}', ?)", tuid, rid)
		id := uint64(f.e.rootInt(f.t, "SELECT id FROM §.tasks WHERE uid = ?", tuid))
		httpx.Accepted(c, tuid, id, map[string]any{"uid": tuid, "status": "pending"})
	default:
		httpx.Created(c, "/api/v1/things/"+strconv.FormatInt(n, 10), map[string]any{"n": n, "name": in.Name})
	}
}

func (f *fixture) post(path string, b body, hdr ...string) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(b)
	return f.postRaw(path, string(raw), hdr...)
}

func (f *fixture) postRaw(path, raw string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", "1")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) rows() int64 {
	return f.e.rootInt(f.t, "SELECT COUNT(*) FROM §.idempotency_keys")
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b apierr.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("不是统一错误体: %v\n%s", err, rec.Body)
	}
	return b.Error.Code
}

const key = "Idempotency-Key"

// AC5：同键同体重放返回首次响应（含同一 Task）；同键异体 422；真实 MySQL，handler 只执行一次。
func TestReplaySameKeySameBody(t *testing.T) {
	f := newFixture(t)
	first := f.post("/api/v1/things", body{Name: "a"}, key, "k-replay-1")
	if first.Code != 201 || first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("首次请求: %d %s", first.Code, first.Body)
	}
	for i := 0; i < 3; i++ {
		again := f.post("/api/v1/things", body{Name: "a"}, key, "k-replay-1")
		if again.Code != 201 || again.Body.String() != first.Body.String() || again.Header().Get("Location") != first.Header().Get("Location") {
			t.Fatalf("重放应原样返回首次响应: %d %s (首次 %s)", again.Code, again.Body, first.Body)
		}
		if again.Header().Get("Idempotent-Replayed") != "true" {
			t.Error("重放响应应带 Idempotent-Replayed: true")
		}
		if again.Header().Get("X-Request-ID") == "" || again.Header().Get("X-Request-ID") == first.Header().Get("X-Request-ID") {
			t.Errorf("重放响应应带本次请求自己的 X-Request-ID: %q vs %q", again.Header().Get("X-Request-ID"), first.Header().Get("X-Request-ID"))
		}
		if !strings.HasPrefix(again.Header().Get("Content-Type"), "application/json") {
			t.Errorf("Content-Type 丢失: %q", again.Header().Get("Content-Type"))
		}
	}
	if f.execs.Load() != 1 {
		t.Fatalf("handler 应只执行一次，实际 %d", f.execs.Load())
	}
	if n := f.e.rootInt(t, "SELECT response_status FROM §.idempotency_keys"); n != 201 || f.rows() != 1 {
		t.Errorf("库内应有 1 行、状态 201: rows=%d status=%d", f.rows(), n)
	}
	// 审计：首次记录一次，重放被标记为 Replayed（不应重复留痕）。
	var replays, real int
	for _, o := range f.hooks.auditOutcomes() {
		if o.Replayed {
			replays++
		} else {
			real++
		}
	}
	if real != 1 || replays != 3 {
		t.Errorf("审计结果: 实际执行 %d 次、重放 %d 次，期望 1 与 3", real, replays)
	}
	// expires_at = created_at + 24h。
	if n := f.e.rootInt(t, "SELECT TIMESTAMPDIFF(HOUR, created_at, expires_at) FROM §.idempotency_keys"); n != 24 {
		t.Errorf("保留期应为 24h: %d", n)
	}
}

func TestSameKeyDifferentRequestIs422(t *testing.T) {
	f := newFixture(t)
	if rec := f.post("/api/v1/things", body{Name: "a"}, key, "k-diff"); rec.Code != 201 {
		t.Fatal(rec.Code)
	}
	cases := map[string]*httptest.ResponseRecorder{
		"异体":       f.post("/api/v1/things", body{Name: "b"}, key, "k-diff"),
		"异路径":      f.post("/api/v1/others", body{Name: "a"}, key, "k-diff"),
		"异查询":      f.post("/api/v1/things?dry_run=1", body{Name: "a"}, key, "k-diff"),
		"空白差异也算异体": f.postRaw("/api/v1/things", ` {"name":"a","mode":""}`, key, "k-diff"),
	}
	for name, rec := range cases {
		if rec.Code != 422 || errCode(t, rec) != "idempotency.key_reuse" {
			t.Errorf("%s: 应 422 idempotency.key_reuse: %d %s", name, rec.Code, rec.Body)
		}
	}
	if f.execs.Load() != 1 {
		t.Errorf("422 的请求不得执行 handler: %d", f.execs.Load())
	}
	// 同键异体不影响原记录：原请求仍可重放。
	if rec := f.post("/api/v1/things", body{Name: "a"}, key, "k-diff"); rec.Code != 201 || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("原请求应仍可重放: %d", rec.Code)
	}
}

func TestKeyScopedPerPrincipalAndOptional(t *testing.T) {
	f := newFixture(t)
	a := f.post("/api/v1/things", body{Name: "a"}, key, "same-key", "X-Actor", "1")
	b := f.post("/api/v1/things", body{Name: "a"}, key, "same-key", "X-Actor", "2")
	if a.Code != 201 || b.Code != 201 || b.Header().Get("Idempotent-Replayed") != "" || f.execs.Load() != 2 {
		t.Fatalf("不同主体的同名键互不影响: %d %d execs=%d", a.Code, b.Code, f.execs.Load())
	}
	if n := f.e.rootInt(t, "SELECT COUNT(DISTINCT principal_key) FROM §.idempotency_keys"); n != 2 {
		t.Errorf("应有两个 principal_key: %d", n)
	}
	if pk := f.e.rootInt(t, "SELECT COUNT(*) FROM §.idempotency_keys WHERE principal_key IN ('tenant:1:1','tenant:2:2')"); pk != 2 {
		t.Errorf("principal_key 应为 <kind>:<actor>:<tenant>: %d", pk)
	}
	// 无键：每次都执行，且不写表。
	before := f.rows()
	f.post("/api/v1/things", body{Name: "x"})
	f.post("/api/v1/things", body{Name: "x"})
	if f.execs.Load() != 4 || f.rows() != before {
		t.Errorf("无 Idempotency-Key 时每次都执行且不入库: execs=%d rows=%d", f.execs.Load(), f.rows())
	}
}

func TestKeyValidation(t *testing.T) {
	f := newFixture(t)
	ok128 := strings.Repeat("k", 128)
	if rec := f.post("/api/v1/things", body{Name: "a"}, key, ok128); rec.Code != 201 {
		t.Errorf("128 字符应被接受: %d %s", rec.Code, rec.Body)
	}
	for name, k := range map[string]string{"129 字符": strings.Repeat("k", 129), "含空格": "a b", "非 ASCII": "键-1", "含控制字符": "a\tb"} {
		req := httptest.NewRequest("POST", "/api/v1/things", strings.NewReader(`{"name":"a"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Actor", "1")
		req.Header.Set(key, k)
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != 400 || errCode(t, rec) != "validation.failed" {
			t.Errorf("%s: 应 400 validation.failed: %d %s", name, rec.Code, rec.Body)
		}
	}
	// 空值与重复头。
	req := httptest.NewRequest("POST", "/api/v1/things", strings.NewReader(`{"name":"a"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", "1")
	req.Header[key] = []string{""}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("空键应 400: %d", rec.Code)
	}
	req = httptest.NewRequest("POST", "/api/v1/things", strings.NewReader(`{"name":"a"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", "1")
	req.Header[key] = []string{"a", "b"}
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("重复的键头应 400: %d", rec.Code)
	}
	if f.execs.Load() != 1 {
		t.Errorf("非法键不得执行 handler: %d", f.execs.Load())
	}
}

// 失败（4xx/5xx/panic）不缓存：占位被释放，同一个键可在纠正后重试，且不会因「体不同」被 422。
func TestFailuresAreNotCached(t *testing.T) {
	f := newFixture(t)
	for _, mode := range []string{"bad", "fail", "panic"} {
		k := "k-" + mode
		first := f.post("/api/v1/things", body{Name: "a", Mode: mode}, key, k)
		if first.Code < 400 {
			t.Fatalf("%s: 应失败: %d", mode, first.Code)
		}
		if strings.Contains(first.Body.String(), "hunter2") || strings.Contains(first.Body.String(), "boom") {
			t.Errorf("%s: 响应泄漏内部原因: %s", mode, first.Body)
		}
		if n := f.e.rootInt(t, "SELECT COUNT(*) FROM §.idempotency_keys WHERE idem_key = ?", k); n != 0 {
			t.Errorf("%s: 失败后占位应被释放，库内仍有 %d 行", mode, n)
		}
		before := f.execs.Load()
		again := f.post("/api/v1/things", body{Name: "a", Mode: mode}, key, k)
		if again.Code != first.Code || f.execs.Load() != before+1 {
			t.Errorf("%s: 同键重试应重新执行: %d→%d execs %d→%d", mode, first.Code, again.Code, before, f.execs.Load())
		}
		fixed := f.post("/api/v1/things", body{Name: "a-fixed"}, key, k) // 纠正后的请求（体不同）
		if fixed.Code != 201 {
			t.Errorf("%s: 纠正后同键重试应成功（而非 422）: %d %s", mode, fixed.Code, fixed.Body)
		}
	}
}

// 并发：同键同体的并发请求，handler 只执行一次，其余是 409（仍在执行）或重放。
func TestConcurrentSameKeyExecutesOnce(t *testing.T) {
	f := newFixture(t)
	const n = 20
	var wg sync.WaitGroup
	codes := make([]int, n)
	bodies := make([]string, n)
	wg.Add(n)
	startGun := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-startGun
			rec := f.post("/api/v1/things", body{Name: "c", Mode: "slow"}, key, "k-concurrent")
			codes[i], bodies[i] = rec.Code, rec.Body.String()
		}(i)
	}
	close(startGun)
	wg.Wait()
	var created int
	var firstBody string
	for i, c := range codes {
		switch c {
		case 201:
			created++
			if firstBody == "" {
				firstBody = bodies[i]
			}
			if bodies[i] != firstBody {
				t.Errorf("所有 201 应是同一份响应: %s vs %s", bodies[i], firstBody)
			}
		case 409:
		default:
			t.Errorf("并发请求只应得到 201 或 409: %d %s", c, bodies[i])
		}
	}
	if f.execs.Load() != 1 || created < 1 || f.rows() != 1 {
		t.Fatalf("应只执行一次: execs=%d 201 数=%d 行数=%d", f.execs.Load(), created, f.rows())
	}
	if rec := f.post("/api/v1/things", body{Name: "c", Mode: "slow"}, key, "k-concurrent"); rec.Code != 201 || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("完成后应可重放: %d", rec.Code)
	}
}

// 执行中的同键请求得到 409（带 Retry-After）；执行完成后重放；崩溃留下的陈旧占位超过租约后可被接管。
func TestInFlightConflictAndStaleTakeover(t *testing.T) {
	f := newFixture(t)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.post("/api/v1/things", body{Name: "b", Mode: "block"}, key, "k-inflight") }()
	<-f.start
	in := f.post("/api/v1/things", body{Name: "b", Mode: "block"}, key, "k-inflight")
	if in.Code != 409 || errCode(t, in) != "resource.conflict" || in.Header().Get("Retry-After") == "" {
		t.Fatalf("执行中的同键请求应 409 + Retry-After: %d %s", in.Code, in.Body)
	}
	if f.execs.Load() != 1 {
		t.Fatalf("409 的请求不得执行 handler: %d", f.execs.Load())
	}

	// 租约（5 分钟）未到：仍是 409；到期后（执行方被视为已崩溃）同键请求接管并执行。
	f.clk.Advance(4 * time.Minute)
	if rec := f.post("/api/v1/things", body{Name: "b", Mode: "block"}, key, "k-inflight"); rec.Code != 409 {
		t.Fatalf("租约内应 409: %d", rec.Code)
	}
	f.clk.Advance(2 * time.Minute)
	takeover := make(chan *httptest.ResponseRecorder, 1)
	go func() { takeover <- f.post("/api/v1/things", body{Name: "b", Mode: "block"}, key, "k-inflight") }()
	<-f.start // 接管方开始执行
	if f.execs.Load() != 2 {
		t.Fatalf("陈旧占位应被接管并重新执行: %d", f.execs.Load())
	}
	close(f.gate) // 放行两个 handler
	a, b := <-done, <-takeover
	if a.Code != 201 || b.Code != 201 {
		t.Fatalf("两者最终都应 201: %d %d", a.Code, b.Code)
	}
	if f.rows() != 1 || f.e.rootInt(t, "SELECT response_status FROM §.idempotency_keys") != 201 {
		t.Errorf("库内应只有一行已完成记录: rows=%d", f.rows())
	}
	if rec := f.post("/api/v1/things", body{Name: "b", Mode: "block"}, key, "k-inflight"); rec.Code != 201 || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Errorf("完成后应可重放: %d", rec.Code)
	}
}

// 24h 过期：过期后同键视为新请求；DeleteExpired 清理。
func TestExpiryAndCleanup(t *testing.T) {
	f := newFixture(t)
	if rec := f.post("/api/v1/things", body{Name: "e"}, key, "k-exp"); rec.Code != 201 {
		t.Fatal(rec.Code)
	}
	f.clk.Advance(23*time.Hour + 59*time.Minute)
	if rec := f.post("/api/v1/things", body{Name: "e"}, key, "k-exp"); rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("24h 内仍应重放: %d", rec.Code)
	}
	f.clk.Advance(2 * time.Minute) // 已满 24h
	rec := f.post("/api/v1/things", body{Name: "e2"}, key, "k-exp")
	if rec.Code != 201 || rec.Header().Get("Idempotent-Replayed") != "" || f.execs.Load() != 2 {
		t.Fatalf("过期后同键应视为新请求（体不同也不 422）: %d execs=%d", rec.Code, f.execs.Load())
	}
	if f.rows() != 1 {
		t.Errorf("过期记录应被替换: %d", f.rows())
	}

	store, err := idemrepo.New(f.e.app, idemrepo.Options{Clock: f.clk})
	if err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(25 * time.Hour)
	n, err := store.DeleteExpired(context.Background(), 100)
	if err != nil || n != 1 || f.rows() != 0 {
		t.Errorf("DeleteExpired: n=%d err=%v rows=%d", n, err, f.rows())
	}
}

// 重放返回「同一 Task」：202 + Location + tasks.id 关联；真实 tasks 行只创建一次。
func TestReplayReturnsSameTask(t *testing.T) {
	f := newFixture(t)
	first := f.post("/api/v1/things", body{Name: "t", Mode: "task"}, key, "k-task")
	if first.Code != 202 || !regexp.MustCompile(`^/api/v1/tasks/[0-9A-Z]{26}$`).MatchString(first.Header().Get("Location")) {
		t.Fatalf("应 202 + Location: %d %v", first.Code, first.Header())
	}
	again := f.post("/api/v1/things", body{Name: "t", Mode: "task"}, key, "k-task")
	if again.Code != 202 || again.Header().Get("Location") != first.Header().Get("Location") || again.Body.String() != first.Body.String() {
		t.Fatalf("重放应返回同一 Task: %v %s", again.Header(), again.Body)
	}
	if n := f.e.rootInt(t, "SELECT COUNT(*) FROM §.tasks"); n != 1 {
		t.Errorf("tasks 只应创建一次: %d", n)
	}
	if n := f.e.rootInt(t, "SELECT COUNT(*) FROM §.idempotency_keys i JOIN §.tasks t ON t.id = i.task_id WHERE i.idem_key = 'k-task'"); n != 1 {
		t.Errorf("idempotency_keys.task_id 应关联到该 Task: %d", n)
	}
}

// 构造约束：只接受 app 池；存储不可用时创建类请求被拒绝（500），而不是绕过幂等保护执行。
func TestStoreBoundaryAndFailClosed(t *testing.T) {
	f := newFixture(t)
	if _, err := idemrepo.New(nil, idemrepo.Options{}); err == nil {
		t.Error("nil 池应被拒绝")
	}
	// 关闭 app 池：模拟数据库不可用。
	_ = f.e.app.Close()
	rec := f.post("/api/v1/things", body{Name: "x"}, key, "k-down")
	if rec.Code != 500 || f.execs.Load() != 0 {
		t.Errorf("存储不可用时不得执行创建类 handler: %d execs=%d", rec.Code, f.execs.Load())
	}
	// 无键的请求不依赖幂等存储（仍会执行；是否可用取决于 handler 自身的依赖）。
	if rec := f.post("/api/v1/things", body{Name: "x"}); rec.Code != 201 {
		t.Errorf("无键请求不经幂等存储: %d", rec.Code)
	}
}

// 契约：真实契约里声明了 Idempotency-Key 的 operation，其 422 响应符合 openapi（含 ErrorResponse schema 与 X-Request-ID）。
func TestKeyReuseResponseMatchesOpenAPIContract(t *testing.T) {
	spec := contracttest.LoadRepo(t)
	f := newFixtureForSpec(t, spec)
	checked := 0
	for _, m := range spec.Expected() {
		if !m.Idempotent {
			continue
		}
		path := "/api/v1" + regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(m.Path, "X1")
		k := "k-contract-" + m.OperationID
		if rec := f.post(path, body{Name: "one"}, key, k); rec.Code != 201 {
			t.Fatalf("%s: 首次请求 %d %s", m.OperationID, rec.Code, rec.Body)
		}
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"name":"two"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Actor", "1")
		req.Header.Set(key, k)
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, req)
		if rec.Code != 422 {
			t.Fatalf("%s: 异体应 422: %d", m.OperationID, rec.Code)
		}
		spec.AssertResponse(t, m.OperationID, req, rec.Result())
		checked++
	}
	if checked < 50 {
		t.Errorf("契约中应有 50 个以上声明 Idempotency-Key 的 operation，检查了 %d", checked)
	}
}

// newFixtureForSpec 把契约里全部 Idempotent operation 以真实元数据注册到路由上（handler 同 fixture.handle）。
func newFixtureForSpec(t *testing.T, spec *contracttest.Spec) *fixture {
	t.Helper()
	f := &fixture{t: t, e: newEnv(t), clk: newFakeClock(), hooks: &testHooks{}, gate: make(chan struct{}), start: make(chan struct{}, 8),
		ids: uid.NewGenerator(clock.System()), logs: &bytes.Buffer{}}
	store, err := idemrepo.New(f.e.app, idemrepo.Options{Clock: f.clk})
	if err != nil {
		t.Fatal(err)
	}
	r, err := httpx.NewRouter(httpx.Options{
		Clock: f.clk, IDs: f.ids, Idempotency: store,
		Hooks: httpx.Hooks{Authenticator: f.hooks, Authorizer: f.hooks, Scope: f.hooks, Auditor: f.hooks},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range spec.Expected() {
		if m.Idempotent {
			if err := r.Register(m, f.handle); err != nil {
				t.Fatal(err)
			}
		}
	}
	if f.srv, err = r.Handler(); err != nil {
		t.Fatal(err)
	}
	return f
}
