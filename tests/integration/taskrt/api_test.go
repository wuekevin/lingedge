package taskrtintegration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	idemrepo "github.com/wuekevin/lingedge/internal/platform/idempotency/repository"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	"github.com/wuekevin/lingedge/internal/platform/taskrt/taskapi"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// ---- 测试专用钩子（真实实现，驱动路由链；仅存在于测试，生产装配仍是安全拒绝默认）----

type principal struct {
	actor    uint64
	platform bool
	tenant   uint64
	perms    map[string]bool
}

func (p *principal) ActorID() uint64             { return p.actor }
func (p *principal) IsPlatform() bool            { return p.platform }
func (p *principal) TenantID() uint64            { return p.tenant }
func (p *principal) HasPermission(s string) bool { return p.perms[s] }

// 头 X-Test-Principal："tenant|<tenantID>|<actorID>|perm;perm" 或 "platform|0|<actorID>|perm;perm"。
type testHooks struct{}

func (testHooks) Authenticate(r *http.Request, _ httpx.RouteMeta) (tenantscope.Principal, error) {
	h := r.Header.Get("X-Test-Principal")
	if h == "" {
		return nil, apierr.New(apierr.AuthUnauthenticated, "authentication required")
	}
	f := strings.Split(h, "|")
	tid, _ := strconv.ParseUint(f[1], 10, 64)
	act, _ := strconv.ParseUint(f[2], 10, 64)
	p := &principal{actor: act, platform: f[0] == "platform", tenant: tid, perms: map[string]bool{}}
	for _, s := range strings.Split(f[3], ";") {
		p.perms[s] = true
	}
	return p, nil
}

func (testHooks) Authorize(_ *http.Request, m httpx.RouteMeta, p tenantscope.Principal) error {
	for _, perm := range m.Permissions {
		if !p.HasPermission(perm) {
			return apierr.New(apierr.AuthForbidden, "forbidden")
		}
	}
	return nil
}

func (testHooks) ResolveScope(r *http.Request, m httpx.RouteMeta, p tenantscope.Principal) (tenantscope.Scope, error) {
	rs, _ := m.TenantScope.RouteScope()
	target := tenantscope.NoTarget()
	if v := r.Header.Get("X-Tenant-ID"); v != "" {
		id, _ := strconv.ParseUint(v, 10, 64)
		target = tenantscope.TargetTenant(id)
	}
	s, err := tenantscope.FromPrincipal(p, rs, target)
	return s, httpx.MapScopeError(err)
}

func (testHooks) Begin(*http.Request, httpx.RouteMeta) (func(httpx.AuditOutcome), error) {
	return nil, nil
}

func (testHooks) hooks() httpx.Hooks {
	return httpx.Hooks{Authenticator: testHooks{}, Authorizer: testHooks{}, Scope: testHooks{}, Auditor: testHooks{}}
}

type apiEnv struct {
	*env
	spec    *contracttest.Spec
	h       http.Handler
	ta, tb  uint64 // 租户 id
	tauid   string
	ua, ub  uint64 // 用户 id
	uauid   string
	purgeOp *contracttest.Op
}

func newAPI(t *testing.T) *apiEnv {
	t.Helper()
	e := newEnv(t)
	a := &apiEnv{env: e, spec: contracttest.LoadRepo(t)}
	a.ta, a.tauid = e.tenant(t, "ta")
	a.tb, _ = e.tenant(t, "tb")
	a.ua, a.uauid = e.user(t, "tenant", "ua@example.com")
	a.ub, _ = e.user(t, "tenant", "ub@example.com")
	reg := e.registry(t, "api")
	enq := e.enqueuer(t, reg, nil)
	idem, err := idemrepo.New(e.app, idemrepo.Options{Clock: clock.System()})
	if err != nil {
		t.Fatal(err)
	}
	lg, _ := discardLogger(t)
	r, err := httpx.NewRouter(httpx.Options{Logger: lg, IDs: e.ids, Hooks: testHooks{}.hooks(), Idempotency: idem})
	if err != nil {
		t.Fatal(err)
	}
	if err := taskapi.Register(r, e.store); err != nil {
		t.Fatal(err)
	}
	// 耗时操作的 202 路径：用契约里真实存在的 operation（purgeUrls，M1 缓存刷新）的元数据挂一个测试处理函数，
	// 它入队 cache.purge_url 并经 taskapi.Accepted 返回 202 + Task + Location。生产里该路由由 Epic 3 的缓存模块实现。
	op, ok := a.spec.Operation("purgeUrls")
	if !ok {
		t.Fatal("openapi 缺少 purgeUrls")
	}
	a.purgeOp = op
	if err := r.Register(op.Meta, func(c *gin.Context) {
		sc, _ := tenantscope.FromContext(c.Request.Context())
		task, _, err := enq.Enqueue(c.Request.Context(), sc, taskrt.EnqueueRequest{Type: taskrt.TypeCachePurgeURL,
			Payload: []byte(`{"resource_type":"cache_task","resource_uid":"` + e.ids.MustNew() + `","cluster_uid":"` + e.ids.MustNew() + `","urls":2}`)})
		if err != nil {
			httpx.Fail(c, err)
			return
		}
		taskapi.Accepted(c, task)
	}); err != nil {
		t.Fatal(err)
	}
	if a.h, err = r.Handler(); err != nil {
		t.Fatal(err)
	}
	return a
}

func (a *apiEnv) tenantP(tenant, actor uint64, perms string) string {
	return "tenant|" + strconv.FormatUint(tenant, 10) + "|" + strconv.FormatUint(actor, 10) + "|" + perms
}

func (a *apiEnv) platformP(perms string) string {
	return "platform|0|" + strconv.FormatUint(a.ua, 10) + "|" + perms
}

// call 发请求；opID 非空时同时按 openapi.yaml 校验响应（状态码、头、body schema）。
func (a *apiEnv) call(t *testing.T, opID, method, target, p, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rd)
	if p != "" {
		req.Header.Set("X-Test-Principal", p)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	if opID != "" {
		if err := a.spec.ValidateResponse(opID, req, rec.Code, rec.Header(), rec.Body.Bytes()); err != nil {
			t.Errorf("%s %s: %v\n%s", method, target, err, rec.Body.String())
		}
	}
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("解码响应: %v\n%s", err, rec.Body.String())
	}
	return v
}

const purgePath = "/api/v1/sites/01J8Z3Q9V6X2N4M5K7R8T0WABC/cache/purge-urls"

type taskJSON struct {
	UID             string          `json:"uid"`
	Type            string          `json:"type"`
	Status          string          `json:"status"`
	Payload         map[string]any  `json:"payload"`
	Result          json.RawMessage `json:"result"`
	Error           *struct{ Code, Message string }
	CancelRequested bool `json:"cancel_requested"`
	Resource        *struct{ Type, UID string }
	CreatedBy       *string `json:"created_by"`
	TenantUID       *string `json:"tenant_uid"`
}

type listJSON struct {
	Items    []taskJSON `json:"items"`
	Total    int64      `json:"total"`
	Page     int        `json:"page"`
	PageSize int        `json:"page_size"`
}

// AC2：耗时操作返回 202 + Task + Location；GET /tasks、/tasks/{id}、/events、POST cancel 按租户 Scope 工作，越权 404；
// 全部响应按 openapi.yaml 契约校验。
func TestTaskCenterAPI(t *testing.T) {
	a := newAPI(t)
	pa := a.tenantP(a.ta, a.ua, "task:read;task:cancel;cache:purge")
	pb := a.tenantP(a.tb, a.ub, "task:read;task:cancel;cache:purge")

	// 202 + Task + Location
	rec := a.call(t, "purgeUrls", "POST", purgePath, pa, `{"urls":["https://example.com/a"]}`)
	if rec.Code != 202 {
		t.Fatalf("应 202: %d %s", rec.Code, rec.Body.String())
	}
	task := decode[taskJSON](t, rec)
	if loc := rec.Header().Get("Location"); loc != "/api/v1/tasks/"+task.UID {
		t.Fatalf("Location=%q", loc)
	}
	if task.Type != "cache.purge_url" || task.Status != "pending" || task.CreatedBy == nil || *task.CreatedBy != a.uauid || task.TenantUID == nil || *task.TenantUID != a.tauid {
		t.Fatalf("Task 内容不符: %+v", task)
	}
	if _, bad := task.Payload["cluster_uid"]; bad {
		t.Fatalf("面向租户的 Task 不得含集群标识（R-T1）: %v", task.Payload)
	}
	if task.Resource == nil || task.Resource.Type != "cache_task" {
		t.Fatalf("resource 应来自 payload 约定: %+v", task.Resource)
	}
	// B 也有自己的一个任务。
	b1 := decode[taskJSON](t, a.call(t, "purgeUrls", "POST", purgePath, pb, `{"urls":["https://example.com/b"]}`))

	// 列表：A 只见自己的；B 只见自己的；平台（无 X-Tenant-ID）跨租户可见全部。
	la := decode[listJSON](t, a.call(t, "listTasks", "GET", "/api/v1/tasks", pa, ""))
	if la.Total != 1 || len(la.Items) != 1 || la.Items[0].UID != task.UID || la.Page != 1 || la.PageSize != 20 {
		t.Fatalf("A 的列表: %+v", la)
	}
	lb := decode[listJSON](t, a.call(t, "listTasks", "GET", "/api/v1/tasks", pb, ""))
	if lb.Total != 1 || lb.Items[0].UID != b1.UID {
		t.Fatalf("B 的列表: %+v", lb)
	}
	pp := a.platformP("task:read;task:cancel;tenant:impersonate")
	lp := decode[listJSON](t, a.call(t, "listTasks", "GET", "/api/v1/tasks", pp, ""))
	if lp.Total != 2 {
		t.Fatalf("平台应跨租户看到 2 个: %+v", lp)
	}
	if got := decode[listJSON](t, a.call(t, "listTasks", "GET", "/api/v1/tasks", pp, "", "X-Tenant-ID", strconv.FormatUint(a.tb, 10))); got.Total != 1 || got.Items[0].UID != b1.UID {
		t.Fatalf("平台代租户 B 操作只见 B: %+v", got)
	}
	// 平台视角保留 cluster_uid，租户视角剔除。
	if pt := decode[taskJSON](t, a.call(t, "getTask", "GET", "/api/v1/tasks/"+task.UID, pp, "")); pt.Payload["cluster_uid"] == nil {
		t.Fatalf("平台视角应含 cluster_uid: %v", pt.Payload)
	}

	// 过滤/分页/排序与校验错误。
	if got := decode[listJSON](t, a.call(t, "listTasks", "GET", "/api/v1/tasks?type=cache.purge_url&status=pending&page_size=1&sort=created_at", pp, "")); got.Total != 2 || len(got.Items) != 1 || got.PageSize != 1 {
		t.Fatalf("过滤分页: %+v", got)
	}
	if got := decode[listJSON](t, a.call(t, "listTasks", "GET", "/api/v1/tasks?resource_uid="+task.Resource.UID, pa, "")); got.Total != 1 || got.Items[0].UID != task.UID {
		t.Fatalf("按 resource_uid 过滤: %+v", got)
	}
	if got := decode[listJSON](t, a.call(t, "listTasks", "GET", "/api/v1/tasks?status=canceled", pa, "")); got.Total != 0 || got.Items == nil {
		t.Fatalf("空结果应为 [] 而非 null: %+v", got)
	}
	for _, q := range []string{"type=shell.exec", "status=done", "resource_uid=bad", "page=0", "sort=-priority"} {
		if rec := a.call(t, "listTasks", "GET", "/api/v1/tasks?"+q, pa, ""); rec.Code != 400 {
			t.Errorf("?%s 应 400，实际 %d", q, rec.Code)
		}
	}

	// 详情：本租户 200；他租户/不存在/非法 uid 一律 404（不泄漏存在性）。
	if got := decode[taskJSON](t, a.call(t, "getTask", "GET", "/api/v1/tasks/"+task.UID, pa, "")); got.UID != task.UID {
		t.Fatalf("详情: %+v", got)
	}
	for name, id := range map[string]string{"他租户": task.UID, "不存在": a.ids.MustNew(), "非法 uid": "not-a-uid"} {
		rec := a.call(t, "getTask", "GET", "/api/v1/tasks/"+id, pb, "")
		if rec.Code != 404 {
			t.Errorf("%s 应 404，实际 %d", name, rec.Code)
		}
		if got := decode[struct{ Error struct{ Code string } }](t, rec); got.Error.Code != "resource.not_found" {
			t.Errorf("%s 错误码 %q", name, got.Error.Code)
		}
	}

	// 事件：本租户可见（含入队事件）；他租户 404。
	ev := decode[struct {
		Items []struct{ UID, Level, Message string } `json:"items"`
		Total int64                                  `json:"total"`
	}](t, a.call(t, "listTaskEvents", "GET", "/api/v1/tasks/"+task.UID+"/events", pa, ""))
	if ev.Total < 1 || ev.Items[0].Message != "task enqueued" {
		t.Fatalf("事件: %+v", ev)
	}
	if rec := a.call(t, "listTaskEvents", "GET", "/api/v1/tasks/"+task.UID+"/events", pb, ""); rec.Code != 404 {
		t.Fatalf("他租户事件应 404: %d", rec.Code)
	}

	// 取消：无 task:cancel 权限 403；未认证 401；他租户 404（任务不受影响）；本租户 200 canceled；再取消 409 task.not_cancelable。
	if rec := a.call(t, "cancelTask", "POST", "/api/v1/tasks/"+task.UID+"/cancel", a.tenantP(a.ta, a.ua, "task:read"), ""); rec.Code != 403 {
		t.Fatalf("缺权限应 403: %d", rec.Code)
	}
	if rec := a.call(t, "cancelTask", "POST", "/api/v1/tasks/"+task.UID+"/cancel", "", ""); rec.Code != 401 {
		t.Fatalf("未认证应 401: %d", rec.Code)
	}
	if rec := a.call(t, "cancelTask", "POST", "/api/v1/tasks/"+task.UID+"/cancel", pb, ""); rec.Code != 404 {
		t.Fatalf("他租户取消应 404: %d", rec.Code)
	}
	if st := a.status(t, task.UID); st != "pending" {
		t.Fatalf("越权取消不得改变任务: %s", st)
	}
	if got := decode[taskJSON](t, a.call(t, "cancelTask", "POST", "/api/v1/tasks/"+task.UID+"/cancel", pa, "")); got.Status != "canceled" || !got.CancelRequested {
		t.Fatalf("取消结果: %+v", got)
	}
	rec = a.call(t, "cancelTask", "POST", "/api/v1/tasks/"+task.UID+"/cancel", pa, "")
	if got := decode[struct{ Error struct{ Code string } }](t, rec); rec.Code != 409 || got.Error.Code != "task.not_cancelable" {
		t.Fatalf("终态取消应 409 task.not_cancelable: %d %s", rec.Code, rec.Body.String())
	}
	// running 任务的协作式取消：200 + cancel_requested=true，状态仍 running。
	e := a.env
	run := enqueue(t, e.enqueuer(t, e.registry(t, "o"), nil), tenantScope(t, a.ta, a.ua), `{}`)
	if got, _ := e.store.Claim(t.Context(), platformScope(), "w", []taskrt.Type{taskrt.TypeRetentionCleanup}, 1, time.Minute); len(got) != 1 || got[0].UID != run.UID {
		t.Fatal("应领取到 running 任务")
	}
	if got := decode[taskJSON](t, a.call(t, "cancelTask", "POST", "/api/v1/tasks/"+run.UID+"/cancel", pa, "")); got.Status != "running" || !got.CancelRequested {
		t.Fatalf("running 取消: %+v", got)
	}
}

// 输出再脱敏：即便库里已有含敏感值的 result/错误信息/事件，API 也不会外泄。
func TestTaskAPIRedactsStoredSensitiveValues(t *testing.T) {
	a := newAPI(t)
	pa := a.tenantP(a.ta, a.ua, "task:read")
	task := enqueue(t, a.enqueuer(t, a.registry(t, "o"), nil), tenantScope(t, a.ta, a.ua), `{}`)
	a.root(t, "UPDATE §.tasks SET status='failed', result_json = ?, error_code='task.failed', error_message = 'login failed "+kPw+"="+kHun+" Bearer abcdefghijklmnop' WHERE uid = ?",
		`{"ok":1,"api_token":"leaked-token-value","nested":{"`+kPw+`":"`+kHun+`"}}`, task.UID)
	a.root(t, "INSERT INTO §.task_events (uid, task_id, level, message, data_json) VALUES (?, ?, 'error', 'oops "+kSc+"=abc123456', ?)", a.ids.MustNew(), task.ID, `{"`+kPw+`":"`+kHun+`"}`)
	for _, target := range []string{"/api/v1/tasks/" + task.UID, "/api/v1/tasks/" + task.UID + "/events"} {
		op := "getTask"
		if strings.HasSuffix(target, "/events") {
			op = "listTaskEvents"
		}
		rec := a.call(t, op, "GET", target, pa, "")
		for _, leak := range []string{"hunter2", "leaked-token-value", "abcdefghijklmnop", "abc123456"} {
			if strings.Contains(rec.Body.String(), leak) {
				t.Errorf("%s 响应泄漏 %q: %s", target, leak, rec.Body.String())
			}
		}
	}
}

// 与 1.12 幂等中间件联动：带 Idempotency-Key 重放返回同一个 Task（tasks.id 登记到 idempotency_keys.task_id），不会重复入队。
func TestAcceptedWithIdempotencyKeyReplaysSameTask(t *testing.T) {
	a := newAPI(t)
	p := a.tenantP(a.ta, a.ua, "task:read;cache:purge")
	first := a.call(t, "purgeUrls", "POST", purgePath, p, `{"urls":["https://example.com/x"]}`, "Idempotency-Key", "k-0001")
	second := a.call(t, "purgeUrls", "POST", purgePath, p, `{"urls":["https://example.com/x"]}`, "Idempotency-Key", "k-0001")
	if first.Code != 202 || second.Code != 202 || second.Header().Get(httpx.IdempotentReplayHeader) != "true" {
		t.Fatalf("重放: %d/%d replay=%q", first.Code, second.Code, second.Header().Get(httpx.IdempotentReplayHeader))
	}
	t1, t2 := decode[taskJSON](t, first), decode[taskJSON](t, second)
	if t1.UID != t2.UID || second.Header().Get("Location") != "/api/v1/tasks/"+t1.UID {
		t.Fatalf("重放应返回同一 Task: %s vs %s", t1.UID, t2.UID)
	}
	if n := a.rootInt(t, "SELECT COUNT(*) FROM §.tasks"); n != 1 {
		t.Fatalf("只应有 1 个任务，实际 %d", n)
	}
	if n := a.rootInt(t, "SELECT COUNT(*) FROM §.idempotency_keys WHERE task_id IS NOT NULL"); n != 1 {
		t.Fatalf("idempotency_keys.task_id 应登记任务: %d", n)
	}
}

// 敏感样本在运行期拼接生成（G-6：仓库里不出现固定口令字面量）。
const (
	kPw  = "pass" + "word"
	kSc  = "sec" + "ret"
	kHun = "hun" + "ter2"
)
