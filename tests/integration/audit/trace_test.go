package auditit

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/tests/apistack"
)

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// NFR-21（AC5）：一次 API 调用可经 Request ID 追到它的审计与它派生的 Task（tasks.request_id、task_events、Worker 日志）。
// 用测试专用的探针路由（经真实 Router.Register 与真实钩子链）在同一业务事务里「入队任务 + 写审计」，真实 Worker 执行任务并写日志与事件。
func TestRequestIDTracesAuditAndTask(t *testing.T) {
	var enq *taskrt.Enqueuer
	probe := func(w *apistack.World, r *httpx.Router) {
		reg := taskrt.NewRegistry()
		if err := reg.Register(taskrt.Spec{Type: taskrt.TypeCachePurgeURL, Handler: func(ctx context.Context, run *taskrt.Run) (taskrt.Result, error) {
			_ = run.Event(ctx, "info", "purge executed by worker", map[string]any{"urls": 1})
			return taskrt.Result{}, nil
		}}); err != nil {
			t.Fatal(err)
		}
		var err error
		if enq, err = taskrt.NewEnqueuer(w.Store, reg, apistack.IDs, nil); err != nil {
			t.Fatal(err)
		}
		m := httpx.RouteMeta{OperationID: "probeTrace", Method: "POST", Path: "/probe/trace", Auth: httpx.AuthRequired, Permissions: []string{"task:cancel"}, TenantScope: httpx.ScopeEither, Audit: true, Phase: "M1"}
		if err := r.Register(m, func(c *gin.Context) {
			sc, _ := tenantscope.FromContext(c.Request.Context())
			var task taskrt.Task
			err := w.App.WithTx(c.Request.Context(), func(ctx context.Context) error {
				var e error
				if task, _, e = enq.Enqueue(ctx, sc, taskrt.EnqueueRequest{Type: taskrt.TypeCachePurgeURL,
					Payload: []byte(`{"resource_type":"cache_task","resource_uid":"` + apistack.IDs.MustNew() + `","urls":1}`)}); e != nil {
					return e
				}
				return w.Audit.Record(ctx, audit.ActionTaskCancel, audit.Event{ResourceType: audit.ResourceTask, ResourceUID: task.UID, Changes: map[string]any{"enqueued": "cache.purge_url"}})
			})
			if err != nil {
				httpx.Fail(c, apierr.Internal(err))
				return
			}
			c.JSON(http.StatusAccepted, map[string]string{"task_uid": task.UID})
		}); err != nil {
			t.Fatal(err)
		}
	}
	w := apistack.New(t, apistack.Options{Routes: probe, ExtraAuditOps: []audit.Op{{OperationID: "probeTrace", Actions: []audit.Action{audit.ActionTaskCancel}}}})
	tn := w.SeedTenant("trace")
	u := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(u, "tenant_owner")
	s := w.Login(u)

	// 真实 Worker（真实 MySQL 队列、日志输出到缓冲）。
	logs := &lockedBuf{}
	lg, err := logx.New(logx.Options{Level: "info", Format: "json", Output: logs})
	if err != nil {
		t.Fatal(err)
	}
	workerReg := taskrt.NewRegistry()
	if err := workerReg.Register(taskrt.Spec{Type: taskrt.TypeCachePurgeURL, Handler: func(ctx context.Context, run *taskrt.Run) (taskrt.Result, error) {
		_ = run.Event(ctx, "info", "purge executed by worker", map[string]any{"urls": 1})
		return taskrt.Result{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	wk, err := taskrt.NewWorker(taskrt.WorkerConfig{Store: w.Store, Registry: workerReg, Logger: lg, Owner: "audit-trace-worker", Lease: time.Second, PollInterval: 100 * time.Millisecond, Concurrency: 1,
		Backoff: taskrt.BackoffConfig{Base: 100 * time.Millisecond, Max: time.Second}, Scope: tenantscope.PlatformForJob(0)}) // [tenantscope-mint] 测试：Worker 是平台进程
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = wk.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	rid := apistack.IDs.MustNew()
	r := s.Do("POST", "/probe/trace", nil, apistack.WithHeader("X-Request-ID", rid))
	if r.Status != 202 || r.Header.Get("X-Request-ID") != rid {
		t.Fatalf("探针应 202 并回显 Request ID: %d %s", r.Status, r.Body)
	}
	taskUID := r.JSON(t)["task_uid"].(string)

	// ① 审计：经 Request ID 查到这次调用的审计（审计 API 与库）。
	ar := s.Do("GET", "/audit-logs?request_id="+rid, nil)
	if ar.Status != 200 || len(uidsOf(t, ar)) != 1 || itemsOf(t, ar)[0]["request_id"] != rid || itemsOf(t, ar)[0]["resource_uid"] != taskUID {
		t.Fatalf("经 Request ID 应追到审计: %d %s", ar.Status, ar.Body)
	}
	// ② Task：任务记录带同一 Request ID（任务中心 API 可按 request_id 过滤），事件可读。
	tr := s.Do("GET", "/tasks?request_id="+rid, nil)
	if tr.Status != 200 || len(uidsOf(t, tr)) != 1 || uidsOf(t, tr)[0] != taskUID || itemsOf(t, tr)[0]["request_id"] != rid {
		t.Fatalf("经 Request ID 应追到 Task: %d %s", tr.Status, tr.Body)
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.tasks WHERE uid = ? AND request_id = ?", taskUID, rid); n != 1 {
		t.Fatalf("tasks.request_id 应等于调用的 Request ID: %d", n)
	}
	// ③ Task 日志：真实 Worker 执行任务，日志与 task_events 都可追到该 Request ID 对应的任务。
	deadline := time.Now().Add(20 * time.Second)
	for w.RootStr("SELECT status FROM §.tasks WHERE uid = ?", taskUID) != "success" {
		if time.Now().After(deadline) {
			t.Fatalf("任务未在期限内完成: %s\n%s", w.RootStr("SELECT status FROM §.tasks WHERE uid = ?", taskUID), logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	ev := s.Do("GET", "/tasks/"+taskUID+"/events", nil)
	if ev.Status != 200 || !strings.Contains(string(ev.Body), "purge executed by worker") {
		t.Fatalf("task_events 应含 Worker 写入的事件: %d %s", ev.Status, ev.Body)
	}
	if l := logs.String(); !strings.Contains(l, `"request_id":"`+rid+`"`) || !strings.Contains(l, `"task_uid":"`+taskUID+`"`) {
		t.Fatalf("Worker 日志应带 request_id 与 task_uid: %s", l)
	}
	// 反向：同一个 Request ID 在审计与 Task 上完全一致；别的 Request ID 追不到它们。
	if r2 := s.Do("GET", "/audit-logs?request_id="+apistack.IDs.MustNew(), nil); len(uidsOf(t, r2)) != 0 {
		t.Error("无关的 Request ID 不应追到审计")
	}
}
