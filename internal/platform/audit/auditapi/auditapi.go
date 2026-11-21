// Package auditapi 实现审计日志读取 API（Story 1.21）：GET /audit-logs（游标分页）与 GET /audit-logs/{auditId}。
// 路径不在 /admin/ 之下，Console 可合法使用（ADR-014 §5、IMP-14）；经 httpx.Router.Register 注册，元数据与 openapi.yaml 一致。
//
// 可见范围（ADR-009 §5、platform-identity §3.1）由请求 Scope 决定，调用方无法放宽：
//   - 租户主体（含平台主体带 X-Tenant-ID 的 acting-as）：只见 tenant_id = Scope 租户的记录；
//   - 平台主体不带 X-Tenant-ID：跨租户可读（可再用 tenant_uid 过滤）。
//
// 字段裁剪：面向租户的响应（租户范围 Scope）不含平台内部信息——代租户（acting_as）记录里的平台操作者身份（actor_uid/actor_label/ip/user_agent）被抹去，
// 变更摘要里 node_*/cluster_* 等内部标识键被剔除（R-T1，复用 leakguard.ForbiddenKey）。
package auditapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/audit/repository"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/leakguard"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// 路由元数据（与 openapi.yaml 的 x-permissions/x-tenant-scope/x-audit/x-phase 一一对应）。
var (
	MetaList = httpx.RouteMeta{OperationID: "listAuditLogs", Method: "GET", Path: "/audit-logs", Auth: httpx.AuthRequired,
		Permissions: []string{"audit:read"}, TenantScope: httpx.ScopeEither, Phase: "M1"}
	MetaGet = httpx.RouteMeta{OperationID: "getAuditLog", Method: "GET", Path: "/audit-logs/{auditId}", Auth: httpx.AuthRequired,
		Permissions: []string{"audit:read"}, TenantScope: httpx.ScopeEither, Phase: "M1"}
)

// Register 注册审计读取路由。repo 在 ManifestOnly（路由清单导出）模式下可为 nil（处理函数不会被调用）。
func Register(r *httpx.Router, repo *repository.Repo) error {
	h := &handlers{repo: repo}
	return errors.Join(r.Register(MetaList, h.list), r.Register(MetaGet, h.get))
}

type handlers struct{ repo *repository.Repo }

// View 是 OpenAPI AuditLog 的 JSON 形态。
type View struct {
	UID          string          `json:"uid"`
	CreatedAt    string          `json:"created_at"`
	ActorType    string          `json:"actor_type"`
	ActorLabel   string          `json:"actor_label"`
	ActorUID     *string         `json:"actor_uid"`
	TenantUID    *string         `json:"tenant_uid"`
	ActingAs     bool            `json:"acting_as"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceUID  *string         `json:"resource_uid"`
	Result       string          `json:"result"`
	Changes      json.RawMessage `json:"changes"`
	RequestID    string          `json:"request_id"`
	IP           *string         `json:"ip"`
	UserAgent    *string         `json:"user_agent"`
}

// PlatformActorLabel 是面向租户时代租户操作的平台操作者的统一标签（不暴露平台管理员的身份）。
const PlatformActorLabel = "platform"

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// ToView 把一行审计记录转为对外形态；tenantFacing 为 true（租户范围 Scope，含 acting-as）时做字段裁剪。
func ToView(r repository.LogRow, tenantFacing bool) View {
	v := View{UID: r.UID, CreatedAt: ts(r.CreatedAt), ActorType: r.ActorType, ActorLabel: r.ActorLabel, ActorUID: r.ActorUID, TenantUID: r.TenantUID,
		ActingAs: r.ActingAs, Action: r.Action, ResourceType: r.ResourceType, ResourceUID: r.ResourceUID, Result: r.Result, RequestID: r.RequestID,
		UserAgent: r.UserAgent, Changes: json.RawMessage("null")}
	if len(r.IP) > 0 {
		s := net.IP(r.IP).String()
		v.IP = &s
	}
	if len(r.ChangesJSON) > 0 {
		var x any
		if json.Unmarshal(r.ChangesJSON, &x) == nil {
			if tenantFacing {
				x = StripInternal(x)
			}
			if b, err := json.Marshal(x); err == nil {
				v.Changes = b
			}
		}
	}
	if tenantFacing && r.ActingAs {
		// 平台管理员代租户操作：租户可见「平台运营做了什么」，但不得得知平台管理员是谁、从哪里来。
		v.ActorType, v.ActorLabel, v.ActorUID, v.IP, v.UserAgent = "user", PlatformActorLabel, nil, nil, nil
	}
	return v
}

// StripInternal 递归剔除 R-T1 禁止的内部标识键（node_*、cluster_*、egress_node*、nodes/clusters…，leakguard.ForbiddenKey）。
func StripInternal(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if leakguard.ForbiddenKey(k) || strings.HasPrefix(strings.ToLower(k), "cluster") || strings.HasPrefix(strings.ToLower(k), "node") {
				continue
			}
			out[k] = StripInternal(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = StripInternal(val)
		}
		return out
	}
	return v
}

func scopeOf(c *gin.Context) (tenantscope.Scope, bool) {
	sc, ok := tenantscope.FromContext(c.Request.Context())
	if !ok {
		httpx.Fail(c, apierr.New(apierr.AuthForbidden, "forbidden"))
	}
	return sc, ok
}

const cursorPrefix = "id:"

func encodeCursor(id uint64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.FormatUint(id, 10)))
}

func decodeCursor(s string) (uint64, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(b), cursorPrefix) {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(string(b), cursorPrefix), 10, 64)
	return n, err == nil && n > 0
}

func parseTime(q, field string, ds *[]apierr.Detail) *time.Time {
	if q == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, q)
	if err != nil {
		*ds = append(*ds, apierr.Detail{Field: field, Reason: "must be an RFC 3339 timestamp"})
		return nil
	}
	t = t.UTC()
	return &t
}

func (h *handlers) list(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	q := c.Request.URL.Query()
	cp, err := httpx.ParseCursor(q)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var ds []apierr.Detail
	f := repository.Filter{Limit: cp.Limit + 1}
	if cp.Cursor != "" {
		if f.BeforeID, ok = decodeCursor(cp.Cursor); !ok {
			ds = append(ds, apierr.Detail{Field: "cursor", Reason: "invalid cursor"})
		}
	}
	short := func(field string, max int, dst *string) {
		if v := q.Get(field); v != "" {
			if len(v) > max {
				ds = append(ds, apierr.Detail{Field: field, Reason: "too long"})
				return
			}
			*dst = v
		}
	}
	short("action", 96, &f.Action)
	short("resource_type", 48, &f.ResourceType)
	short("resource_uid", 64, &f.ResourceUID)
	short("request_id", 64, &f.RequestID)
	for _, u := range []struct {
		field string
		dst   *string
	}{{"actor_uid", &f.ActorUID}, {"tenant_uid", &f.TenantUID}} {
		if v := q.Get(u.field); v != "" {
			if uid.Valid(v) {
				*u.dst = v
			} else {
				ds = append(ds, apierr.Detail{Field: u.field, Reason: "must be a valid uid"})
			}
		}
	}
	if v := q.Get("result"); v != "" {
		switch v {
		case "success", "failure", "denied":
			f.Result = v
		default:
			ds = append(ds, apierr.Detail{Field: "result", Reason: "must be one of success, failure, denied"})
		}
	}
	f.From = parseTime(q.Get("from"), "from", &ds)
	f.To = parseTime(q.Get("to"), "to", &ds)
	if len(ds) > 0 {
		httpx.Fail(c, apierr.Validation(ds...))
		return
	}
	rows, err := h.repo.List(c.Request.Context(), sc, f)
	if err != nil {
		httpx.Fail(c, apierr.Internal(err))
		return
	}
	next := ""
	if len(rows) > cp.Limit {
		rows = rows[:cp.Limit]
		next = encodeCursor(rows[len(rows)-1].ID)
	}
	tenantFacing := !sc.IsPlatform()
	items := make([]View, len(rows))
	for i, r := range rows {
		items[i] = ToView(r, tenantFacing)
	}
	httpx.OK(c, httpx.NewCursorResult(items, next))
}

func (h *handlers) get(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	id := c.Param("auditId")
	if !uid.Valid(id) {
		httpx.Fail(c, apierr.NotFound())
		return
	}
	row, err := h.repo.Get(c.Request.Context(), sc, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		httpx.Fail(c, apierr.NotFound())
		return
	case err != nil:
		httpx.Fail(c, apierr.Internal(err))
		return
	}
	httpx.OK(c, ToView(row, !sc.IsPlatform()))
}
