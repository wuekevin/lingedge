// Package handler 是租户模块的 HTTP 处理与模块装配（Story 1.20）：/tenants* 共 8 个 operation，全部经 httpx.Router.Register 注册，
// 元数据与 docs/api/openapi.yaml 的 x-permissions / x-tenant-scope / x-audit / x-phase 一一对应（make openapi-check 与 contracttest 核对），无 501 占位。
//
// 未注册：createTenant（POST /tenants，x-phase: P5——商业模式创建租户；M1 只有种子的默认租户，D-13）。它不在「M1 未实现 operation 清单」里。
package handler

import (
	"errors"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/tenant/internal/domain"
	"github.com/wuekevin/lingedge/internal/tenant/internal/service"
)

var (
	route = func(id, method, path string, perms []string, scope httpx.TenantScope, audit bool) httpx.RouteMeta {
		return httpx.RouteMeta{OperationID: id, Method: method, Path: path, Auth: httpx.AuthRequired, Permissions: perms, TenantScope: scope, Audit: audit, Phase: "M1"}
	}
	MetaListTenants      = route("listTenants", "GET", "/tenants", []string{"tenant:read"}, httpx.ScopePlatform, false)
	MetaGetCurrentTenant = route("getCurrentTenant", "GET", "/tenants/current", []string{}, httpx.ScopeTenant, false)
	MetaGetTenant        = route("getTenant", "GET", "/tenants/{tenantId}", []string{"tenant:read"}, httpx.ScopePlatform, false)
	MetaUpdateTenant     = route("updateTenant", "PATCH", "/tenants/{tenantId}", []string{"tenant:manage"}, httpx.ScopePlatform, true)
	MetaListMembers      = route("listTenantMembers", "GET", "/tenants/{tenantId}/members", []string{"tenant:member:read"}, httpx.ScopePlatform, false)
	MetaAddMember        = route("addTenantMember", "POST", "/tenants/{tenantId}/members", []string{"tenant:member:manage"}, httpx.ScopePlatform, true)
	MetaGetQuotas        = route("getTenantQuotas", "GET", "/tenants/{tenantId}/quotas", []string{"tenant:read"}, httpx.ScopePlatform, false)
	MetaPutQuotas        = route("putTenantQuotas", "PUT", "/tenants/{tenantId}/quotas", []string{"tenant:manage"}, httpx.ScopePlatform, true)
)

// Metas 是本模块注册的全部路由元数据。
func Metas() []httpx.RouteMeta {
	return []httpx.RouteMeta{MetaListTenants, MetaGetCurrentTenant, MetaGetTenant, MetaUpdateTenant, MetaListMembers, MetaAddMember, MetaGetQuotas, MetaPutQuotas}
}

// Handlers 是租户模块的 HTTP 处理集合。
type Handlers struct{ svc *service.Service }

// New 创建处理集合（svc 在 ManifestOnly 下为 nil：处理函数不会被调用）。
func New(svc *service.Service) *Handlers { return &Handlers{svc: svc} }

// Register 注册全部 /tenants* 路由。
func (h *Handlers) Register(r *httpx.Router) error {
	return errors.Join(
		r.Register(MetaListTenants, h.list),
		r.Register(MetaGetCurrentTenant, h.current),
		r.Register(MetaGetTenant, h.get),
		r.Register(MetaUpdateTenant, h.update),
		r.Register(MetaListMembers, h.members),
		r.Register(MetaAddMember, h.addMember),
		r.Register(MetaGetQuotas, h.quotas),
		r.Register(MetaPutQuotas, h.putQuotas),
	)
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// scopeOf 取中间件放入的 Scope；没有则 403（链被错误组装时 fail-closed）。
func scopeOf(c *gin.Context) (tenantscope.Scope, bool) {
	sc, ok := tenantscope.FromContext(c.Request.Context())
	if !ok {
		httpx.Fail(c, apierr.New(apierr.AuthForbidden, "forbidden"))
	}
	return sc, ok
}

// ---- 视图（OpenAPI Tenant / TenantMember / TenantQuota）----
// 租户可见字段只含上述 schema 的属性，不含节点/集群内部标识（R-T1）。

type tenantJSON struct {
	UID          string         `json:"uid"`
	Name         string         `json:"name"`
	Slug         string         `json:"slug"`
	Status       string         `json:"status"`
	IsDefault    bool           `json:"is_default"`
	ContactEmail *string        `json:"contact_email"`
	Locale       string         `json:"locale"`
	OwnerUID     *string        `json:"owner_uid"`
	Settings     map[string]any `json:"settings"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
}

func toTenantJSON(t domain.Tenant) tenantJSON {
	loc := t.Locale
	if loc != "zh-CN" && loc != "en" {
		loc = "zh-CN"
	}
	return tenantJSON{UID: t.UID, Name: t.Name, Slug: t.Slug, Status: t.Status, IsDefault: t.IsDefault, ContactEmail: t.ContactEmail, Locale: loc,
		OwnerUID: t.OwnerUID, Settings: service.ParseSettings(t.SettingsJSON), CreatedAt: ts(t.CreatedAt), UpdatedAt: ts(t.UpdatedAt)}
}

type userSummaryJSON struct {
	UID                string `json:"uid"`
	Email              string `json:"email"`
	DisplayName        string `json:"display_name"`
	UserType           string `json:"user_type"`
	Locale             string `json:"locale"`
	TOTPEnabled        bool   `json:"totp_enabled"`
	MustChangePassword bool   `json:"must_change_password"`
}

type memberJSON struct {
	User     userSummaryJSON `json:"user"`
	Status   string          `json:"status"`
	IsOwner  bool            `json:"is_owner"`
	Roles    []string        `json:"roles"`
	JoinedAt *string         `json:"joined_at"`
}

func toMemberJSON(m domain.Member) memberJSON {
	loc := m.Locale
	if loc != "zh-CN" && loc != "en" {
		loc = "zh-CN"
	}
	j := memberJSON{User: userSummaryJSON{UID: m.UserUID, Email: m.Email, DisplayName: m.DisplayName, UserType: m.UserType, Locale: loc, TOTPEnabled: m.TOTPEnabled, MustChangePassword: m.MustChange},
		Status: m.Status, IsOwner: m.IsOwner, Roles: service.ParseMemberRoles(m.RolesJSON)}
	if m.JoinedAt != nil {
		s := ts(*m.JoinedAt)
		j.JoinedAt = &s
	}
	return j
}

type quotaJSON struct {
	QuotaKey   string `json:"quota_key"`
	LimitValue *int64 `json:"limit_value"`
	Source     string `json:"source"`
}

// ---- 处理函数 ----

func (h *Handlers) list(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	pg, err := httpx.ParsePage(c.Request.URL.Query())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	srt, err := httpx.ParseSort(c.Request.URL.Query(), []string{"created_at", "name", "slug"}, httpx.Sort{Field: "created_at"})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	items, total, err := h.svc.List(c.Request.Context(), sc, srt.Field, srt.Desc, pg.Limit(), pg.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := make([]tenantJSON, 0, len(items))
	for _, t := range items {
		out = append(out, toTenantJSON(t))
	}
	httpx.OK(c, httpx.NewPageResult(out, pg, total))
}

func (h *Handlers) current(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	t, err := h.svc.Current(c.Request.Context(), sc)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, toTenantJSON(t))
}

func (h *Handlers) get(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	t, err := h.svc.Get(c.Request.Context(), sc, c.Param("tenantId"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, toTenantJSON(t))
}

type updateRequest struct {
	Name         *string        `json:"name"`
	ContactEmail *string        `json:"contact_email"`
	Locale       *string        `json:"locale"`
	Settings     map[string]any `json:"settings"`
}

func (h *Handlers) update(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	var req updateRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	t, err := h.svc.Update(c.Request.Context(), sc, c.Param("tenantId"), service.UpdateInput{Name: req.Name, ContactEmail: req.ContactEmail, Locale: req.Locale, Settings: req.Settings})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, toTenantJSON(t))
}

func (h *Handlers) members(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	pg, err := httpx.ParsePage(c.Request.URL.Query())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	srt, err := httpx.ParseSort(c.Request.URL.Query(), []string{"joined_at", "email"}, httpx.Sort{Field: "joined_at"})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	items, total, err := h.svc.Members(c.Request.Context(), sc, c.Param("tenantId"), srt.Field, srt.Desc, pg.Limit(), pg.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := make([]memberJSON, 0, len(items))
	for _, m := range items {
		out = append(out, toMemberJSON(m))
	}
	httpx.OK(c, httpx.NewPageResult(out, pg, total))
}

func (h *Handlers) addMember(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	p, ok := httpx.PrincipalFrom(c.Request.Context())
	if !ok {
		httpx.Fail(c, apierr.New(apierr.AuthUnauthenticated, "authentication required"))
		return
	}
	var raw struct {
		Email     string    `json:"email"`
		RoleCodes *[]string `json:"role_codes"`
	}
	if err := httpx.BindJSON(c, &raw); err != nil {
		httpx.Fail(c, err)
		return
	}
	if raw.RoleCodes == nil {
		httpx.Fail(c, apierr.Validation(apierr.Detail{Field: "role_codes", Reason: "is required"}))
		return
	}
	m, err := h.svc.AddMember(c.Request.Context(), sc, p.ActorID(), c.Param("tenantId"), service.AddMemberInput{Email: raw.Email, RoleCodes: *raw.RoleCodes})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.Created(c, httpx.APIPrefix+"/tenants/"+c.Param("tenantId")+"/members", toMemberJSON(m))
}

func quotaPage(c *gin.Context, items []service.QuotaView) (httpx.PageResult[quotaJSON], error) {
	pg, err := httpx.ParsePage(c.Request.URL.Query())
	if err != nil {
		return httpx.PageResult[quotaJSON]{}, err
	}
	total := int64(len(items))
	lo := min(pg.Offset(), len(items))
	hi := min(lo+pg.Limit(), len(items))
	out := make([]quotaJSON, 0, hi-lo)
	for _, q := range items[lo:hi] {
		out = append(out, quotaJSON{QuotaKey: q.Key, LimitValue: q.Limit, Source: q.Source})
	}
	return httpx.NewPageResult(out, pg, total), nil
}

func (h *Handlers) quotas(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	if _, err := httpx.ParsePage(c.Request.URL.Query()); err != nil { // 先于数据访问校验分页参数
		httpx.Fail(c, err)
		return
	}
	items, err := h.svc.Quotas(c.Request.Context(), sc, c.Param("tenantId"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	res, err := quotaPage(c, items)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

type putQuotasRequest struct {
	Limits map[string]*int64 `json:"limits"`
}

func (h *Handlers) putQuotas(c *gin.Context) {
	sc, ok := scopeOf(c)
	if !ok {
		return
	}
	var req putQuotasRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	items, err := h.svc.SetQuotas(c.Request.Context(), sc, c.Param("tenantId"), req.Limits)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	// 返回全部配额（一页：page=1，page_size=字典大小上限内）。
	out := make([]quotaJSON, 0, len(items))
	for _, q := range items {
		out = append(out, quotaJSON{QuotaKey: q.Key, LimitValue: q.Limit, Source: q.Source})
	}
	httpx.OK(c, httpx.NewPageResult(out, httpx.PageParams{Page: 1, PageSize: httpx.MaxPageSize}, int64(len(out))))
}
