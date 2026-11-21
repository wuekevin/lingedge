package taskapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/leakguard"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/pkg/redact"
)

// 路由元数据（与 openapi.yaml 的 x-permissions/x-tenant-scope/x-audit/x-phase 一一对应）。
var (
	metaList = httpx.RouteMeta{OperationID: "listTasks", Method: "GET", Path: "/tasks", Auth: httpx.AuthRequired,
		Permissions: []string{"task:read"}, TenantScope: httpx.ScopeEither, Phase: "M1"}
	metaGet = httpx.RouteMeta{OperationID: "getTask", Method: "GET", Path: "/tasks/{taskId}", Auth: httpx.AuthRequired,
		Permissions: []string{"task:read"}, TenantScope: httpx.ScopeEither, Phase: "M1"}
	metaEvents = httpx.RouteMeta{OperationID: "listTaskEvents", Method: "GET", Path: "/tasks/{taskId}/events", Auth: httpx.AuthRequired,
		Permissions: []string{"task:read"}, TenantScope: httpx.ScopeEither, Phase: "M1"}
	metaCancel = httpx.RouteMeta{OperationID: "cancelTask", Method: "POST", Path: "/tasks/{taskId}/cancel", Auth: httpx.AuthRequired,
		Permissions: []string{"task:cancel"}, TenantScope: httpx.ScopeEither, Audit: true, Phase: "M1"}
)

// TxFunc 划定事务边界（db.Pool.WithTx）：取消任务与其审计记录同一事务（Story 1.21）。
type TxFunc func(ctx context.Context, fn func(ctx context.Context) error) error

// Register 注册四个任务中心路由。store、rec、withTx 在 ManifestOnly（路由清单导出）模式下可为 nil（处理函数不会被调用）；
// 实际服务时 rec 与 withTx 必填，否则取消任务（x-audit: true）一律 500（fail-closed：没有审计就不执行）。
func Register(r *httpx.Router, store taskrt.Store, rec audit.Recorder, withTx TxFunc) error {
	h := &handlers{store: store, audit: rec, withTx: withTx}
	return errors.Join(
		r.Register(metaList, h.list),
		r.Register(metaGet, h.get),
		r.Register(metaEvents, h.events),
		r.Register(metaCancel, h.cancel),
	)
}

type handlers struct {
	store  taskrt.Store
	audit  audit.Recorder
	withTx TxFunc
}

// TaskView 是 OpenAPI Task schema 的 JSON 形态。
type TaskView struct {
	UID             string          `json:"uid"`
	Type            string          `json:"type"`
	Status          string          `json:"status"`
	Progress        int             `json:"progress"`
	TenantUID       *string         `json:"tenant_uid"`
	Payload         json.RawMessage `json:"payload"`
	Result          json.RawMessage `json:"result"`
	Error           *ErrorView      `json:"error"`
	Attempts        uint32          `json:"attempts"`
	RequestID       string          `json:"request_id"`
	ParentTaskUID   *string         `json:"parent_task_uid"`
	CreatedBy       *string         `json:"created_by"`
	CreatedAt       string          `json:"created_at"`
	StartedAt       *string         `json:"started_at"`
	FinishedAt      *string         `json:"finished_at"`
	CancelRequested bool            `json:"cancel_requested"`
	Resource        *ResourceView   `json:"resource"`
}

// ErrorView 是 Task.error。
type ErrorView struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ResourceView 是 Task.resource。
type ResourceView struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

// EventView 是 OpenAPI TaskEvent。
type EventView struct {
	UID       string          `json:"uid"`
	Level     string          `json:"level"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	CreatedAt string          `json:"created_at"`
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func tsp(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

// View 把任务转为对外形态：payload/result/事件 data 再经脱敏；面向租户主体（租户 Scope，含代租户操作）时剔除 cluster*/node* 标识（R-T1）。
func View(t taskrt.Task, sc tenantscope.Scope) TaskView {
	tenantFacing := !sc.IsPlatform()
	v := TaskView{
		UID: t.UID, Type: string(t.Type), Status: string(t.Status), Progress: t.Progress, TenantUID: t.TenantUID,
		Payload: sanitize(t.Payload, tenantFacing, json.RawMessage(`{}`)), Result: sanitize(t.Result, tenantFacing, nil),
		Attempts: t.Attempts, RequestID: t.RequestID, ParentTaskUID: t.ParentTaskUID, CreatedBy: t.CreatedByUID,
		CreatedAt: ts(t.CreatedAt), StartedAt: tsp(t.StartedAt), FinishedAt: tsp(t.FinishedAt), CancelRequested: t.CancelRequested,
	}
	if v.Result == nil {
		v.Result = json.RawMessage("null")
	}
	if t.ErrorCode != nil {
		msg := ""
		if t.ErrorMessage != nil {
			msg = *t.ErrorMessage
		}
		v.Error = &ErrorView{Code: *t.ErrorCode, Message: taskrtRedact(msg)}
	}
	v.Resource = resourceOf(t.Payload)
	return v
}

// resourceOf 约定：payload 里的 resource_type + resource_uid 即任务的主资源（如 deployment、cache_task）。
func resourceOf(payload json.RawMessage) *ResourceView {
	var p struct {
		Type string `json:"resource_type"`
		UID  string `json:"resource_uid"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Type == "" || !uid.Valid(p.UID) {
		return nil
	}
	return &ResourceView{Type: p.Type, UID: p.UID}
}

func sanitize(raw json.RawMessage, tenantFacing bool, def json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return def
	}
	out := taskrt.RedactJSON(raw)
	if out == nil {
		return def
	}
	if tenantFacing {
		var v any
		if json.Unmarshal(out, &v) == nil {
			if b, err := json.Marshal(stripPlatformKeys(v)); err == nil {
				return b
			}
		}
		return def
	}
	return out
}

func stripPlatformKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			// R-T1：cluster*/node* 前缀（大小写不敏感，1.14 既有规则）与 leakguard 的内部标识键名（egress_node*、targets、nodes/clusters、驼峰形态；Story 1.20 补）都剔除。
			if l := strings.ToLower(k); strings.HasPrefix(l, "cluster") || strings.HasPrefix(l, "node") || leakguard.ForbiddenKey(k) {
				continue
			}
			out[k] = stripPlatformKeys(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = stripPlatformKeys(val)
		}
		return out
	}
	return v
}

// Accepted 返回 202 + Task + Location: /api/v1/tasks/{uid}，并把 tasks.id 登记给幂等中间件（业务模块的耗时操作入队后调用）。
func Accepted(c *gin.Context, t taskrt.Task) {
	sc, _ := tenantscope.FromContext(c.Request.Context())
	httpx.Accepted(c, t.UID, t.ID, View(t, sc))
}

func scopeOf(c *gin.Context) (tenantscope.Scope, bool) {
	sc, ok := tenantscope.FromContext(c.Request.Context())
	if !ok {
		httpx.Fail(c, apierr.New(apierr.AuthForbidden, "forbidden"))
	}
	return sc, ok
}

func taskIDParam(c *gin.Context) (string, bool) {
	id := c.Param("taskId")
	if !uid.Valid(id) {
		httpx.Fail(c, apierr.NotFound()) // 非法 uid 与「不存在」同形，不泄漏格式校验细节
		return "", false
	}
	return id, true
}

func (h *handlers) list(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	q := c.Request.URL.Query()
	page, err := httpx.ParsePage(q)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	sort, err := httpx.ParseSort(q, []string{"created_at"}, httpx.Sort{Field: "created_at", Desc: true})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	f := taskrt.Filter{Desc: sort.Desc, Offset: page.Offset(), Limit: page.Limit()}
	var ds []apierr.Detail
	if v := q.Get("type"); v != "" {
		if taskrt.IsKnown(taskrt.Type(v)) {
			f.Type = taskrt.Type(v)
		} else {
			ds = append(ds, apierr.Detail{Field: "type", Reason: "unknown task type"})
		}
	}
	if v := q.Get("status"); v != "" {
		if taskrt.Status(v).Valid() {
			f.Status = taskrt.Status(v)
		} else {
			ds = append(ds, apierr.Detail{Field: "status", Reason: "unknown task status"})
		}
	}
	if v := q.Get("resource_uid"); v != "" {
		if uid.Valid(v) {
			f.ResourceUID = v
		} else {
			ds = append(ds, apierr.Detail{Field: "resource_uid", Reason: "must be a valid uid"})
		}
	}
	if v := q.Get("request_id"); v != "" {
		if len(v) <= 64 {
			f.RequestID = v
		} else {
			ds = append(ds, apierr.Detail{Field: "request_id", Reason: "too long"})
		}
	}
	if len(ds) > 0 {
		httpx.Fail(c, apierr.Validation(ds...))
		return
	}
	tasks, total, err := h.store.List(c.Request.Context(), sc, f)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	items := make([]TaskView, len(tasks))
	for i, t := range tasks {
		items[i] = View(t, sc)
	}
	httpx.OK(c, httpx.NewPageResult(items, page, total))
}

func (h *handlers) get(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	id, ok := taskIDParam(c)
	if !ok {
		return
	}
	t, err := h.store.Get(c.Request.Context(), sc, id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, View(t, sc))
}

func (h *handlers) events(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	id, ok := taskIDParam(c)
	if !ok {
		return
	}
	q := c.Request.URL.Query()
	page, err := httpx.ParsePage(q)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if _, err := httpx.ParseSort(q, []string{"created_at"}, httpx.Sort{Field: "created_at"}); err != nil {
		httpx.Fail(c, err)
		return
	}
	evs, total, err := h.store.ListEvents(c.Request.Context(), sc, id, page.Offset(), page.Limit())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	items := make([]EventView, len(evs))
	for i, e := range evs {
		d := sanitize(e.Data, !sc.IsPlatform(), json.RawMessage("null"))
		items[i] = EventView{UID: e.UID, Level: e.Level, Message: taskrtRedact(e.Message), Data: d, CreatedAt: ts(e.CreatedAt)}
	}
	httpx.OK(c, httpx.NewPageResult(items, page, total))
}

func (h *handlers) cancel(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	id, ok := taskIDParam(c)
	if !ok {
		return
	}
	if h.audit == nil || h.withTx == nil {
		httpx.Fail(c, apierr.Internal(errNoAudit))
		return
	}
	// 取消与审计同一事务：审计写入失败则取消不生效（fail-closed，500 可重试）。已结束的任务（409）不写审计（没有发生变更）。
	var t taskrt.Task
	err := h.withTx(c.Request.Context(), func(ctx context.Context) error {
		var cerr error
		if t, cerr = h.store.RequestCancel(ctx, sc, id); cerr != nil {
			return cerr
		}
		var tid uint64 // 被操作对象（任务）所属租户：平台主体跨租户取消时也归到该租户名下，租户自己的审计可见
		if t.TenantID != nil {
			tid = *t.TenantID
		}
		return h.audit.Record(ctx, audit.ActionTaskCancel, audit.Event{ResourceType: audit.ResourceTask, ResourceUID: t.UID, TenantID: tid,
			Changes: map[string]any{"type": string(t.Type), "status": string(t.Status)}})
	})
	switch {
	case errors.Is(err, taskrt.ErrNotCancelable):
		httpx.Fail(c, apierr.Wrap(err, apierr.TaskNotCancelable, "task is already finished and cannot be canceled"))
		return
	case err != nil:
		httpx.Fail(c, err)
		return
	}
	httpx.JSON(c, http.StatusOK, View(t, sc))
}

var errNoAudit = errors.New("taskapi: 未装配审计记录器或事务边界，拒绝执行需要审计的操作")

func taskrtRedact(s string) string { return redact.String(s) }
