package sysconfig

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// 路由元数据（与 openapi.yaml 的 x-permissions/x-tenant-scope/x-audit/x-reauth/x-phase 一一对应；权限串均已由迁移 000004 种子）。
var (
	MetaGetSettings = httpx.RouteMeta{OperationID: "getSettings", Method: "GET", Path: "/admin/settings", Auth: httpx.AuthRequired,
		Permissions: []string{"setting:read"}, TenantScope: httpx.ScopePlatform, Phase: "M1"}
	MetaPatchSettings = httpx.RouteMeta{OperationID: "patchSettings", Method: "PATCH", Path: "/admin/settings", Auth: httpx.AuthRequired,
		Permissions: []string{"setting:write"}, TenantScope: httpx.ScopePlatform, Audit: true, Phase: "M1"}
	MetaGetBrand = httpx.RouteMeta{OperationID: "getBrandSettings", Method: "GET", Path: "/admin/settings/brand", Auth: httpx.AuthRequired,
		Permissions: []string{"setting:read"}, TenantScope: httpx.ScopePlatform, Phase: "M1"}
	MetaPutBrand = httpx.RouteMeta{OperationID: "putBrandSettings", Method: "PUT", Path: "/admin/settings/brand", Auth: httpx.AuthRequired,
		Permissions: []string{"setting:write"}, TenantScope: httpx.ScopePlatform, Audit: true, Phase: "M1"}
	MetaGetSSRF = httpx.RouteMeta{OperationID: "getSsrfAllowlist", Method: "GET", Path: "/admin/settings/ssrf-allowlist", Auth: httpx.AuthRequired,
		Permissions: []string{"setting:read"}, TenantScope: httpx.ScopePlatform, Phase: "M1"}
	MetaPutSSRF = httpx.RouteMeta{OperationID: "putSsrfAllowlist", Method: "PUT", Path: "/admin/settings/ssrf-allowlist", Auth: httpx.AuthRequired,
		Permissions: []string{"setting:write"}, TenantScope: httpx.ScopePlatform, Audit: true, Reauth: true, Phase: "M1"}
	MetaGetRunMode = httpx.RouteMeta{OperationID: "getRunMode", Method: "GET", Path: "/admin/system/run-mode", Auth: httpx.AuthRequired,
		Permissions: []string{"setting:read"}, TenantScope: httpx.ScopePlatform, Phase: "M1"}
	MetaPutRunMode = httpx.RouteMeta{OperationID: "putRunMode", Method: "PUT", Path: "/admin/system/run-mode", Auth: httpx.AuthRequired,
		Permissions: []string{"setting:write"}, TenantScope: httpx.ScopePlatform, Audit: true, Reauth: true, Phase: "M1"}
	MetaGetVersion = httpx.RouteMeta{OperationID: "getVersionInfo", Method: "GET", Path: "/admin/system/version", Auth: httpx.AuthRequired,
		Permissions: []string{"system:read"}, TenantScope: httpx.ScopePlatform, Phase: "M1"}
	MetaListInstallations = httpx.RouteMeta{OperationID: "listInstallations", Method: "GET", Path: "/admin/installations", Auth: httpx.AuthRequired,
		Permissions: []string{"system:read"}, TenantScope: httpx.ScopePlatform, Phase: "M1"}
	MetaPublicVersion = httpx.RouteMeta{OperationID: "getPublicVersion", Method: "GET", Path: "/public/version", Auth: httpx.AuthPublic,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopePublic, Phase: "M1"}
	MetaPublicSiteInfo = httpx.RouteMeta{OperationID: "getPublicSiteInfo", Method: "GET", Path: "/public/site-info", Auth: httpx.AuthPublic,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopePublic, Phase: "M1"}
)

// publicCacheControl：公开信息（品牌/运行模式/版本）允许浏览器与 CDN 短时缓存；全局安全头默认 no-store，这里对公开只读接口显式放宽。
// 30 秒是运行模式切换对官网可见的延迟上限之一 [ASSUMPTION]。
const publicCacheControl = "public, max-age=30"

func registerRoutes(r *httpx.Router, svc *Service) error {
	h := &handlers{svc: svc}
	return errors.Join(
		r.Register(MetaGetSettings, h.getSettings), r.Register(MetaPatchSettings, h.patchSettings),
		r.Register(MetaGetBrand, h.getBrand), r.Register(MetaPutBrand, h.putBrand),
		r.Register(MetaGetSSRF, h.getSSRF), r.Register(MetaPutSSRF, h.putSSRF),
		r.Register(MetaGetRunMode, h.getRunMode), r.Register(MetaPutRunMode, h.putRunMode),
		r.Register(MetaGetVersion, h.getVersion), r.Register(MetaListInstallations, h.listInstallations),
		r.Register(MetaPublicVersion, h.publicVersion), r.Register(MetaPublicSiteInfo, h.publicSiteInfo),
	)
}

type handlers struct{ svc *Service }

func (h *handlers) getSettings(c *gin.Context) {
	out, err := h.svc.SettingsView(c.Request.Context(), c.Query("group"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, out)
}

func (h *handlers) patchSettings(c *gin.Context) {
	var in struct {
		Values map[string]json.RawMessage `json:"values"`
	}
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	if in.Values == nil {
		httpx.Fail(c, apierr.Validation(apierr.Detail{Field: "values", Reason: "is required"}))
		return
	}
	out, err := h.svc.Patch(c.Request.Context(), in.Values)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, out)
}

func (h *handlers) getBrand(c *gin.Context) {
	b, err := h.svc.Brand(c.Request.Context(), true)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, b)
}

func (h *handlers) putBrand(c *gin.Context) {
	var in map[string]json.RawMessage
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	if in == nil {
		httpx.Fail(c, apierr.Validation(apierr.Detail{Field: "body", Reason: "must be an object"}))
		return
	}
	b, err := h.svc.PutBrand(c.Request.Context(), in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, b)
}

func (h *handlers) getSSRF(c *gin.Context) {
	v, err := h.svc.SSRF(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, v)
}

func (h *handlers) putSSRF(c *gin.Context) {
	var in struct {
		Entries   json.RawMessage `json:"entries"`
		UpdatedAt json.RawMessage `json:"updated_at"` // 只读字段：接受并忽略
		UpdatedBy json.RawMessage `json:"updated_by"`
	}
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	if len(in.Entries) == 0 || string(in.Entries) == "null" {
		httpx.Fail(c, apierr.Validation(apierr.Detail{Field: "entries", Reason: "is required"}))
		return
	}
	v, err := h.svc.PutSSRF(c.Request.Context(), in.Entries)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, v)
}

func (h *handlers) getRunMode(c *gin.Context) {
	m, err := h.svc.RunModeFresh(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, map[string]string{"run_mode": m})
}

func (h *handlers) putRunMode(c *gin.Context) {
	var in struct {
		RunMode string `json:"run_mode"`
	}
	if err := httpx.BindJSON(c, &in); err != nil {
		httpx.Fail(c, err)
		return
	}
	m, err := h.svc.SetRunMode(c.Request.Context(), in.RunMode)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, map[string]string{"run_mode": m})
}

func (h *handlers) getVersion(c *gin.Context) {
	v, err := h.svc.Version(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, v)
}

func (h *handlers) listInstallations(c *gin.Context) {
	pg, err := httpx.ParsePage(c.Request.URL.Query())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	items, total, err := h.svc.Installations(c.Request.Context(), pg.Limit(), pg.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.NewPageResult(items, pg, total))
}

// publicGuard 对公开接口做按 IP 限流；被限流返回 429 rate_limit.exceeded + Retry-After。
func (h *handlers) publicGuard(c *gin.Context) bool {
	ok, wait := h.svc.allowPublic(c.Request.Context(), httpx.ClientIPFrom(c.Request.Context()))
	if ok {
		return true
	}
	if wait <= 0 {
		wait = time.Minute
	}
	httpx.Fail(c, apierr.New(apierr.RateLimitExceeded, "too many requests").WithRetryAfter(wait))
	return false
}

func (h *handlers) publicVersion(c *gin.Context) {
	if !h.publicGuard(c) {
		return
	}
	v, err := h.svc.PublicVersion(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	c.Header("Cache-Control", publicCacheControl)
	httpx.OK(c, v)
}

func (h *handlers) publicSiteInfo(c *gin.Context) {
	if !h.publicGuard(c) {
		return
	}
	v, err := h.svc.SiteInfo(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	c.Header("Cache-Control", publicCacheControl)
	httpx.OK(c, v)
}
