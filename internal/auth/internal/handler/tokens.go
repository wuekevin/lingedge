package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/auth/internal/service"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// /auth/api-tokens*（Story 1.19，FR-25、ADR-018 §7）。路由元数据与 docs/api/openapi.yaml 的 x-permissions / x-tenant-scope / x-audit / x-phase
// 及 IdempotencyKey 参数逐项一致（make contract-check 与 contracttest 核对）。
//
// 令牌管理的边界：这些 operation 只作用于**调用者自己名下**的令牌（user_id 来自会话，不来自请求参数）——平台用户管理平台令牌，租户用户管理其租户内自己创建的令牌；
// 只能用 Cookie 会话调用（Bearer 一律拒绝：API Token 不能创建、轮换、吊销或列出令牌），创建/轮换需要近期 MFA（认证钩子统一检查）。
// 授权钩子对它们走「自有资源」路径（authorizer.go：要求 token:manage，不接受 acting-as 的 X-Tenant-ID）。
var (
	tokenRoute = func(id, method, path string, audit, idem bool) httpx.RouteMeta {
		return httpx.RouteMeta{OperationID: id, Method: method, Path: path, Auth: httpx.AuthRequired, Permissions: []string{"token:manage"},
			TenantScope: httpx.ScopeEither, Audit: audit, Idempotent: idem, Phase: "M1"}
	}
	MetaListAPITokens  = tokenRoute("listApiTokens", "GET", "/auth/api-tokens", false, false)
	MetaCreateAPIToken = tokenRoute("createApiToken", "POST", "/auth/api-tokens", true, true)
	MetaGetAPIToken    = tokenRoute("getApiToken", "GET", "/auth/api-tokens/{tokenId}", false, false)
	MetaRevokeAPIToken = tokenRoute("revokeApiToken", "DELETE", "/auth/api-tokens/{tokenId}", true, false)
	MetaRotateAPIToken = tokenRoute("rotateApiToken", "POST", "/auth/api-tokens/{tokenId}/rotate", true, false)
)

// TokenMetas 是本 Story 注册的全部令牌管理路由元数据。
func TokenMetas() []httpx.RouteMeta {
	return []httpx.RouteMeta{MetaListAPITokens, MetaCreateAPIToken, MetaGetAPIToken, MetaRevokeAPIToken, MetaRotateAPIToken}
}

// IsTokenOperation 报告 operationID 是否属于令牌管理路由（授权走自有资源路径；写操作审计的结构化日志兜底只对它们与 /admin 路由生效）。
func IsTokenOperation(operationID string) bool {
	for _, m := range TokenMetas() {
		if m.OperationID == operationID {
			return true
		}
	}
	return false
}

func (h *Handlers) registerTokens(r *httpx.Router) error {
	return errors.Join(
		r.Register(MetaListAPITokens, h.listAPITokens),
		r.Register(MetaCreateAPIToken, h.createAPIToken),
		r.Register(MetaGetAPIToken, h.getAPIToken),
		r.Register(MetaRevokeAPIToken, h.revokeAPIToken),
		r.Register(MetaRotateAPIToken, h.rotateAPIToken),
	)
}

// apiTokenJSON 是 OpenAPI ApiToken。
type apiTokenJSON struct {
	UID            string   `json:"uid"`
	Name           string   `json:"name"`
	TokenPrefix    string   `json:"token_prefix"`
	Scopes         []string `json:"scopes"`
	AllowedCIDRs   []string `json:"allowed_cidrs"`
	ExpiresAt      *string  `json:"expires_at"`
	LastUsedAt     *string  `json:"last_used_at"`
	LastUsedIP     *string  `json:"last_used_ip"`
	RevokedAt      *string  `json:"revoked_at"`
	TenantUID      *string  `json:"tenant_uid"`
	RotatedFromUID *string  `json:"rotated_from_uid"`
	CreatedAt      string   `json:"created_at"`
}

// apiTokenCreatedJSON 是 OpenAPI ApiTokenCreated：元数据 + 仅此一次出现的明文令牌。
type apiTokenCreatedJSON struct {
	apiTokenJSON
	Token string `json:"token"`
}

func tsPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

func toAPITokenJSON(v service.TokenView) apiTokenJSON {
	j := apiTokenJSON{UID: v.UID, Name: v.Name, TokenPrefix: v.Prefix, Scopes: v.Scopes, AllowedCIDRs: v.CIDRs, ExpiresAt: tsPtr(v.ExpiresAt),
		LastUsedAt: tsPtr(v.LastUsedAt), RevokedAt: tsPtr(v.RevokedAt), TenantUID: v.TenantUID, RotatedFromUID: v.RotatedFromUID, CreatedAt: ts(v.CreatedAt)}
	if len(v.CIDRs) == 0 {
		j.AllowedCIDRs = nil
	}
	if len(v.LastUsedIP) > 0 {
		s := service.IPString(v.LastUsedIP)
		j.LastUsedIP = &s
	}
	return j
}

func (h *Handlers) listAPITokens(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	pg, err := httpx.ParsePage(c.Request.URL.Query())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	srt, err := httpx.ParseSort(c.Request.URL.Query(), []string{"created_at", "name"}, httpx.Sort{Field: "created_at", Desc: true})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	items, total, err := h.svc.ListAPITokens(c.Request.Context(), p, pg.Limit(), pg.Offset(), srt.Field, srt.Desc)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := make([]apiTokenJSON, 0, len(items))
	for _, it := range items {
		out = append(out, toAPITokenJSON(it))
	}
	httpx.OK(c, httpx.NewPageResult(out, pg, total))
}

func (h *Handlers) getAPIToken(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	v, err := h.svc.GetAPIToken(c.Request.Context(), p, c.Param("tokenId"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, toAPITokenJSON(v))
}

type createAPITokenRequest struct {
	Name         string   `json:"name"`
	Scopes       []string `json:"scopes"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
	ExpiresAt    *string  `json:"expires_at"`
}

// created 写令牌的创建/轮换响应：明文只在这一个响应里出现（Cache-Control: no-store）；幂等记录里登记去掉明文的响应体——
// 同键重放返回 201 与令牌元数据（不含 token），明文不入 MySQL 的 idempotency_keys，也不会被再次回显。
func created(c *gin.Context, status int, v service.TokenView, token string) {
	meta := toAPITokenJSON(v)
	if status == http.StatusCreated {
		if redacted, err := json.Marshal(meta); err == nil {
			httpx.SetReplayBody(c, append(redacted, '\n'))
		} else {
			httpx.Fail(c, apierr.Internal(err))
			return
		}
	}
	c.Header("Cache-Control", "no-store")
	body := apiTokenCreatedJSON{apiTokenJSON: meta, Token: token}
	if status == http.StatusCreated {
		httpx.Created(c, httpx.APIPrefix+"/auth/api-tokens/"+v.UID, body)
		return
	}
	httpx.OK(c, body)
}

func (h *Handlers) createAPIToken(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	var req createAPITokenRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	in := service.CreateTokenInput{Name: req.Name, Scopes: req.Scopes, CIDRs: req.AllowedCIDRs}
	if req.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*req.ExpiresAt))
		if err != nil {
			httpx.Fail(c, apierr.Validation(apierr.Detail{Field: "expires_at", Reason: "must be an RFC 3339 timestamp"}))
			return
		}
		in.ExpiresAt = &t
	}
	v, token, err := h.svc.CreateAPIToken(c.Request.Context(), p, in, reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	created(c, http.StatusCreated, v, token)
}

func (h *Handlers) revokeAPIToken(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	if err := h.svc.RevokeAPIToken(c.Request.Context(), p, c.Param("tokenId"), reqInfo(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.NoContent(c)
}

func (h *Handlers) rotateAPIToken(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	v, token, err := h.svc.RotateAPIToken(c.Request.Context(), p, c.Param("tokenId"), reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	created(c, http.StatusOK, v, token)
}

// ---- Bearer 认证 ----

// bearerAuthenticator 是 API Token 的认证实现（替换 1.16/1.17 的 Bearer 占位；Options.Bearer 注入点已移除）：不使用 Cookie 与 CSRF（NFR-9）；来源 IP 取自 httpx.ClientIPFrom。
type bearerAuthenticator struct{ svc *service.Service }

var _ httpx.Authenticator = bearerAuthenticator{}

func (b bearerAuthenticator) Authenticate(r *http.Request, _ httpx.RouteMeta) (tenantscope.Principal, error) {
	_, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	p, err := b.svc.AuthenticateAPIToken(r.Context(), strings.TrimSpace(tok), httpx.ClientIPFrom(r.Context()))
	if err != nil {
		return nil, err
	}
	return p, nil
}

// bearerAllowed 决定 API Token 能调用哪些 operation：只有声明了 x-permissions 的业务路由（由「令牌 scopes ∩ 创建者当前权限」授权）。
// 自助 operation（登出、改密、会话、TOTP、再认证、登录日志…）与令牌管理本身都要求 Cookie 会话——令牌不能用来接管账号或繁殖令牌。
func bearerAllowed(meta httpx.RouteMeta) bool {
	return len(meta.Permissions) > 0 && !IsTokenOperation(meta.OperationID)
}
