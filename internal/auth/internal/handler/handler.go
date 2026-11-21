package handler

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/auth/internal/service"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
)

// 路由元数据：与 docs/api/openapi.yaml 的 x-permissions / x-tenant-scope / x-audit / x-phase 及 security 一一对应
// （make contract-check 与 contracttest 核对）。
var (
	MetaLogin = httpx.RouteMeta{OperationID: "login", Method: "POST", Path: "/auth/login", Auth: httpx.AuthPublic,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopePublic, Audit: true, Phase: "M1"}
	MetaLogout = httpx.RouteMeta{OperationID: "logout", Method: "POST", Path: "/auth/logout", Auth: httpx.AuthRequired,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopeEither, Audit: true, Phase: "M1"}
	MetaMe = httpx.RouteMeta{OperationID: "getMe", Method: "GET", Path: "/auth/me", Auth: httpx.AuthRequired,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopeEither, Phase: "M1"}
	MetaCSRF = httpx.RouteMeta{OperationID: "getCsrfToken", Method: "GET", Path: "/auth/csrf", Auth: httpx.AuthRequired,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopeEither, Phase: "M1"}
	MetaChangePassword = httpx.RouteMeta{OperationID: "changePassword", Method: "POST", Path: "/auth/password/change", Auth: httpx.AuthRequired,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopeEither, Audit: true, Phase: "M1"}
	MetaListSessions = httpx.RouteMeta{OperationID: "listSessions", Method: "GET", Path: "/auth/sessions", Auth: httpx.AuthRequired,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopeEither, Phase: "M1"}
	MetaRevokeSession = httpx.RouteMeta{OperationID: "revokeSession", Method: "DELETE", Path: "/auth/sessions/{sessionId}", Auth: httpx.AuthRequired,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopeEither, Audit: true, Phase: "M1"}
	MetaVerifyTotp = httpx.RouteMeta{OperationID: "verifyTotp", Method: "POST", Path: "/auth/totp/verify", Auth: httpx.AuthPublic,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopePublic, Audit: true, Phase: "M1"}
	MetaSetupTotp = httpx.RouteMeta{OperationID: "setupTotp", Method: "POST", Path: "/auth/totp/setup", Auth: httpx.AuthRequired,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopeEither, Audit: true, Phase: "M1"}
	MetaEnableTotp = httpx.RouteMeta{OperationID: "enableTotp", Method: "POST", Path: "/auth/totp/enable", Auth: httpx.AuthRequired,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopeEither, Audit: true, Phase: "M1"}
	MetaDisableTotp = httpx.RouteMeta{OperationID: "disableTotp", Method: "POST", Path: "/auth/totp/disable", Auth: httpx.AuthRequired,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopeEither, Audit: true, Phase: "M1"}
	MetaReauth = httpx.RouteMeta{OperationID: "reauth", Method: "POST", Path: "/auth/reauth", Auth: httpx.AuthRequired,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopeEither, Audit: true, Phase: "M1"}
	MetaLoginLogs = httpx.RouteMeta{OperationID: "listMyLoginLogs", Method: "GET", Path: "/auth/login-logs", Auth: httpx.AuthRequired,
		Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopeEither, Phase: "M1"}
)

// Metas 是本 Story 注册的全部路由元数据。
func Metas() []httpx.RouteMeta {
	return []httpx.RouteMeta{MetaLogin, MetaLogout, MetaMe, MetaCSRF, MetaChangePassword, MetaListSessions, MetaRevokeSession, MetaLoginLogs,
		MetaVerifyTotp, MetaSetupTotp, MetaEnableTotp, MetaDisableTotp, MetaReauth}
}

// Handlers 是 /auth/* 的处理函数集合。svc 在 ManifestOnly（路由清单导出）模式下可为 nil（处理函数不会被调用）。
type Handlers struct {
	svc *service.Service
	// features 提供 /auth/me 的 features（运行模式 + 已交付模块 + 能力状态，sysconfig 模块，Story 1.22）；nil 时用本模块读到的运行模式与注册表常量。
	features FeaturesProvider
}

// FeaturesProvider 提供 /auth/me 的 features（sysconfig.Module 满足）。
type FeaturesProvider interface {
	Features(ctx context.Context) (sysconfig.FeaturesView, error)
}

// New 创建 Handlers。
func New(svc *service.Service) *Handlers { return &Handlers{svc: svc} }

// WithFeatures 返回带 features 提供者的副本。
func (h *Handlers) WithFeatures(f FeaturesProvider) *Handlers { c := *h; c.features = f; return &c }

// Register 注册全部 /auth/* 路由。
func (h *Handlers) Register(r *httpx.Router) error {
	return errors.Join(h.registerAdmin(r), h.registerAuth(r), h.registerTokens(r))
}

func (h *Handlers) registerAuth(r *httpx.Router) error {
	return errors.Join(
		r.Register(MetaLogin, h.login),
		r.Register(MetaLogout, h.logout),
		r.Register(MetaMe, h.me),
		r.Register(MetaCSRF, h.csrf),
		r.Register(MetaChangePassword, h.changePassword),
		r.Register(MetaListSessions, h.listSessions),
		r.Register(MetaRevokeSession, h.revokeSession),
		r.Register(MetaLoginLogs, h.loginLogs),
		r.Register(MetaVerifyTotp, h.verifyTotp),
		r.Register(MetaSetupTotp, h.setupTotp),
		r.Register(MetaEnableTotp, h.enableTotp),
		r.Register(MetaDisableTotp, h.disableTotp),
		r.Register(MetaReauth, h.reauth),
	)
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func reqInfo(c *gin.Context) service.RequestInfo {
	id, _ := requestid.FromContext(c.Request.Context())
	return service.RequestInfo{IP: c.ClientIP(), UserAgent: c.Request.UserAgent(), RequestID: id}
}

// principal 取出认证钩子放入上下文的主体（链被错误组装时 fail-closed 为 401）。
func principal(c *gin.Context) (*domain.Principal, bool) {
	p, ok := httpx.PrincipalFrom(c.Request.Context())
	if !ok {
		httpx.Fail(c, apierr.New(apierr.AuthUnauthenticated, "authentication required"))
		return nil, false
	}
	dp, ok := p.(*domain.Principal)
	if !ok {
		httpx.Fail(c, apierr.New(apierr.AuthUnauthenticated, "authentication required"))
		return nil, false
	}
	return dp, true
}

// ---- Cookie ----

// 会话 Cookie：HttpOnly（脚本读不到）、Secure（仅 HTTPS；TLS 在反代终结，真实 HTTPS 链路下的行为须在部署环境验证）、SameSite=Lax、Path=/、
// 主机范围 Cookie（不设 Domain，Admin/Console 各自的主机互不可见）。
func setSessionCookie(c *gin.Context, realm domain.Realm, token string, ttl time.Duration) {
	http.SetCookie(c.Writer, &http.Cookie{Name: realm.CookieName(), Value: token, Path: "/", MaxAge: int(ttl / time.Second),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func clearSessionCookie(c *gin.Context, realm domain.Realm) {
	http.SetCookie(c.Writer, &http.Cookie{Name: realm.CookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

// ---- 视图 ----

// UserSummary 是 OpenAPI UserSummary。
type UserSummary struct {
	UID                string `json:"uid"`
	Email              string `json:"email"`
	DisplayName        string `json:"display_name"`
	UserType           string `json:"user_type"`
	Locale             string `json:"locale"`
	TOTPEnabled        bool   `json:"totp_enabled"`
	MustChangePassword bool   `json:"must_change_password"`
}

func userSummary(u domain.User) UserSummary {
	loc := u.Locale
	if loc != "zh-CN" && loc != "en" {
		loc = "zh-CN"
	}
	return UserSummary{UID: u.UID, Email: u.Email, DisplayName: u.DisplayName, UserType: u.UserType, Locale: loc, TOTPEnabled: u.TOTPEnabled, MustChangePassword: u.MustChange}
}

// TenantSummary 是 OpenAPI TenantSummary。
type TenantSummary struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	IsDefault bool   `json:"is_default"`
}

// ---- login ----

type loginRequest struct {
	Realm    string `json:"realm"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginResponse 是 OpenAPI LoginResponse。
type LoginResponse struct {
	Status    string       `json:"status"`
	MFAToken  string       `json:"mfa_token,omitempty"`
	CSRFToken string       `json:"csrf_token,omitempty"`
	User      *UserSummary `json:"user,omitempty"`
}

func (h *Handlers) login(c *gin.Context) {
	var req loginRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	realm := domain.Realm(req.Realm)
	in := service.LoginInput{Realm: realm, Email: req.Email, Password: req.Password, Req: reqInfo(c)}
	if realm.Valid() {
		if ck, err := c.Request.Cookie(realm.CookieName()); err == nil {
			in.ExistingToken = ck.Value
		}
	}
	res, err := h.svc.Login(c.Request.Context(), in)
	if err != nil {
		httpx.Fail(c, err) // 失败的登录不改变既有会话与 Cookie
		return
	}
	respondLogin(c, realm, res)
}

// respondLogin 渲染登录成功/第二步要求：status=totp_required 时不建立会话、不下发 Cookie，只返回一次性 mfa_token。
func respondLogin(c *gin.Context, realm domain.Realm, res *service.LoginResult) {
	if res.Status == service.StatusTOTPRequired {
		httpx.OK(c, LoginResponse{Status: res.Status, MFAToken: res.MFAToken})
		return
	}
	setSessionCookie(c, realm, res.Token, res.CookieTTL)
	u := userSummary(res.User)
	httpx.OK(c, LoginResponse{Status: res.Status, CSRFToken: res.CSRF, User: &u})
}

// ---- logout ----

func (h *Handlers) logout(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	if err := h.svc.Logout(c.Request.Context(), p, reqInfo(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	clearSessionCookie(c, p.Realm)
	httpx.NoContent(c)
}

// ---- me / csrf ----

type capabilities struct {
	RunMode  string         `json:"run_mode"`
	Modules  []string       `json:"modules"`
	Features map[string]any `json:"features"`
}

// MeResponse 是 OpenAPI Me。
type MeResponse struct {
	User         UserSummary    `json:"user"`
	Realm        string         `json:"realm"`
	Tenant       *TenantSummary `json:"tenant"`
	ActingTenant *TenantSummary `json:"acting_tenant"`
	Roles        []string       `json:"roles"`
	Permissions  []string       `json:"permissions"`
	MenuKeys     []string       `json:"menu_keys"`
	Capabilities capabilities   `json:"capabilities"`
	// Features 是能力清单（FR-31）：前端据此裁剪菜单，不自行猜测——运行模式、已交付模块、高级能力状态（恒 unverified，NFR-27）。
	Features         sysconfig.FeaturesView `json:"features"`
	CSRFToken        string                 `json:"csrf_token"`
	SessionExpiresAt string                 `json:"session_expires_at"`
}

func (h *Handlers) me(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	res, err := h.svc.Me(c.Request.Context(), p)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	fv := sysconfig.FeaturesView{RunMode: res.RunMode, ModulesDelivered: sysconfig.ModulesDelivered(), CapabilityStatus: sysconfig.CapabilityStatus()}
	if h.features != nil {
		v, ferr := h.features.Features(c.Request.Context())
		if ferr != nil {
			httpx.Fail(c, apierr.Internal(ferr))
			return
		}
		fv = v
	}
	out := MeResponse{
		User: userSummary(p.User), Realm: string(p.Realm), Roles: res.Access.Roles, Permissions: res.Access.Permissions, MenuKeys: res.Access.MenuKeys,
		// 已交付且可用的后端模块来自能力注册表（sysconfig.DeliveredModules，模块交付时登记，一致性测试核对）；features 是同一份数据的细化形态。
		Capabilities:     capabilities{RunMode: fv.RunMode, Modules: fv.ModulesDelivered, Features: map[string]any{"commercial": fv.RunMode == sysconfig.RunModeCommercial}},
		Features:         fv,
		CSRFToken:        p.CSRFToken,
		SessionExpiresAt: ts(p.Session.ExpiresAt),
	}
	if p.Tenant != nil {
		out.Tenant = &TenantSummary{UID: p.Tenant.UID, Name: p.Tenant.Name, Slug: p.Tenant.Slug, IsDefault: p.Tenant.IsDefault}
	}
	httpx.OK(c, out)
}

func (h *Handlers) csrf(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	httpx.OK(c, map[string]string{"csrf_token": p.CSRFToken})
}

// ---- changePassword ----

type passwordChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (h *Handlers) changePassword(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	var req passwordChangeRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := h.svc.ChangePassword(c.Request.Context(), p, req.CurrentPassword, req.NewPassword, reqInfo(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	clearSessionCookie(c, p.Realm) // 全部会话（含当前）已失效，须重新登录
	httpx.NoContent(c)
}

// ---- sessions ----

// SessionView 是 OpenAPI Session。
type SessionView struct {
	SessionRef string  `json:"session_ref"`
	Realm      string  `json:"realm"`
	IP         *string `json:"ip"`
	UserAgent  *string `json:"user_agent"`
	CreatedAt  string  `json:"created_at"`
	LastSeenAt string  `json:"last_seen_at"`
	ExpiresAt  string  `json:"expires_at"`
	Current    bool    `json:"current"`
}

func (h *Handlers) listSessions(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	pg, err := httpx.ParsePage(c.Request.URL.Query())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	items, total, err := h.svc.ListSessions(c.Request.Context(), p, pg.Page, pg.PageSize)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	views := make([]SessionView, 0, len(items))
	for _, it := range items {
		v := SessionView{SessionRef: it.Ref, Realm: it.Realm, UserAgent: it.UserAgent, CreatedAt: ts(it.CreatedAt), LastSeenAt: ts(it.LastSeenAt), ExpiresAt: ts(it.ExpiresAt), Current: it.Current}
		if it.IP != "" {
			ip := it.IP
			v.IP = &ip
		}
		views = append(views, v)
	}
	httpx.OK(c, httpx.PageResult[SessionView]{Items: views, Page: pg.Page, PageSize: pg.PageSize, Total: total})
}

func (h *Handlers) revokeSession(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	current, err := h.svc.RevokeSession(c.Request.Context(), p, c.Param("sessionId"), reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if current {
		clearSessionCookie(c, p.Realm)
	}
	httpx.NoContent(c)
}

// ---- login-logs ----

// LoginLogView 是 OpenAPI LoginLog。
type LoginLogView struct {
	UID        string  `json:"uid"`
	CreatedAt  string  `json:"created_at"`
	Realm      string  `json:"realm"`
	Identifier string  `json:"identifier"`
	Event      string  `json:"event"`
	IP         *string `json:"ip"`
	UserAgent  *string `json:"user_agent"`
	Reason     *string `json:"reason"`
	RequestID  *string `json:"request_id"`
}

type loginLogList struct {
	Items      []LoginLogView `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

func (h *Handlers) loginLogs(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	cp, err := httpx.ParseCursor(c.Request.URL.Query())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	items, next, err := h.svc.ListLoginLogs(c.Request.Context(), p, cp.Cursor, cp.Limit)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := loginLogList{Items: make([]LoginLogView, 0, len(items))}
	for _, it := range items {
		v := LoginLogView{UID: it.UID, CreatedAt: ts(it.CreatedAt), Realm: it.Realm, Identifier: it.Identifier, Event: it.Event, UserAgent: it.UserAgent, Reason: it.Reason, RequestID: it.RequestID}
		if it.IPText != "" {
			ip := it.IPText
			v.IP = &ip
		}
		out.Items = append(out.Items, v)
	}
	if next != "" {
		out.NextCursor = &next
	}
	httpx.OK(c, out)
}

// listAllLoginLogs 是 GET /admin/login-logs（平台全局登录日志，Story 1.21）：游标分页，按 event/identifier/ip/from/to 过滤。
func (h *Handlers) listAllLoginLogs(c *gin.Context) {
	q := c.Request.URL.Query()
	cp, err := httpx.ParseCursor(q)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	in := service.LoginLogQuery{Cursor: cp.Cursor, Limit: cp.Limit, Event: q.Get("event"), Identifier: q.Get("identifier")}
	var ds []apierr.Detail
	if len(in.Event) > 32 {
		ds = append(ds, apierr.Detail{Field: "event", Reason: "too long"})
	}
	if len(in.Identifier) > 255 {
		ds = append(ds, apierr.Detail{Field: "identifier", Reason: "too long"})
	}
	if v := q.Get("ip"); v != "" {
		if in.IP = net.ParseIP(v); in.IP == nil {
			ds = append(ds, apierr.Detail{Field: "ip", Reason: "must be an IPv4 or IPv6 address"})
		}
	}
	for _, t := range []struct {
		field string
		dst   **time.Time
	}{{"from", &in.From}, {"to", &in.To}} {
		if v := q.Get(t.field); v != "" {
			tm, perr := time.Parse(time.RFC3339Nano, v)
			if perr != nil {
				ds = append(ds, apierr.Detail{Field: t.field, Reason: "must be an RFC 3339 timestamp"})
				continue
			}
			tm = tm.UTC()
			*t.dst = &tm
		}
	}
	if len(ds) > 0 {
		httpx.Fail(c, apierr.Validation(ds...))
		return
	}
	items, next, err := h.svc.ListAllLoginLogs(c.Request.Context(), in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := loginLogList{Items: make([]LoginLogView, 0, len(items))}
	for _, it := range items {
		v := LoginLogView{UID: it.UID, CreatedAt: ts(it.CreatedAt), Realm: it.Realm, Identifier: it.Identifier, Event: it.Event, UserAgent: it.UserAgent, Reason: it.Reason, RequestID: it.RequestID}
		if it.IPText != "" {
			ip := it.IPText
			v.IP = &ip
		}
		out.Items = append(out.Items, v)
	}
	if next != "" {
		out.NextCursor = &next
	}
	httpx.OK(c, out)
}

// ---- TOTP / 再认证（Story 1.17）----

// sessionPrincipal 取出必须是 Cookie 会话的主体：Bearer API Token 不能做 TOTP 管理与再认证（403）。
func sessionPrincipal(c *gin.Context) (*domain.Principal, bool) {
	p, ok := principal(c)
	if !ok {
		return nil, false
	}
	if p.Bearer || p.Session.ID == 0 {
		httpx.Fail(c, apierr.New(apierr.AuthForbidden, "a cookie session is required", apierr.Detail{Field: "session", Reason: "API tokens cannot perform this operation"}))
		return nil, false
	}
	return p, true
}

type totpVerifyRequest struct {
	MFAToken string `json:"mfa_token"`
	Code     string `json:"code"`
}

func (h *Handlers) verifyTotp(c *gin.Context) {
	var req totpVerifyRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	in := service.VerifyInput{MFAToken: req.MFAToken, Code: req.Code, Req: reqInfo(c)}
	// 会话固定防护：携带的旧会话 Cookie（任一 realm）在登录成功后作废；realm 由挑战决定，这里两个 Cookie 都带上由服务层按挑战 realm 选用。
	for _, realm := range []domain.Realm{domain.RealmAdmin, domain.RealmConsole} {
		if ck, err := c.Request.Cookie(realm.CookieName()); err == nil && ck.Value != "" {
			in.ExistingTokens = append(in.ExistingTokens, service.ExistingToken{Realm: realm, Token: ck.Value})
		}
	}
	res, err := h.svc.VerifyLogin(c.Request.Context(), in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	respondLogin(c, res.Realm, res)
}

// TotpSetupResponse 是 OpenAPI TotpSetupResponse。
type TotpSetupResponse struct {
	OTPAuthURI   string `json:"otpauth_uri"`
	SecretMasked string `json:"secret_masked"`
}

func (h *Handlers) setupTotp(c *gin.Context) {
	p, ok := sessionPrincipal(c)
	if !ok {
		return
	}
	res, err := h.svc.SetupTOTP(c.Request.Context(), p)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, TotpSetupResponse{OTPAuthURI: res.OTPAuthURI, SecretMasked: res.Secret})
}

type totpEnableRequest struct {
	Code string `json:"code"`
}

// TotpEnableResponse 是 OpenAPI TotpEnableResponse。
type TotpEnableResponse struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

func (h *Handlers) enableTotp(c *gin.Context) {
	p, ok := sessionPrincipal(c)
	if !ok {
		return
	}
	var req totpEnableRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	codes, err := h.svc.EnableTOTP(c.Request.Context(), p, req.Code, reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, TotpEnableResponse{RecoveryCodes: codes})
}

type totpDisableRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (h *Handlers) disableTotp(c *gin.Context) {
	p, ok := sessionPrincipal(c)
	if !ok {
		return
	}
	var req totpDisableRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	if err := h.svc.DisableTOTP(c.Request.Context(), p, req.Password, req.Code, reqInfo(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.NoContent(c)
}

type reauthRequest struct {
	Password string `json:"password"`
	TOTPCode string `json:"totp_code"`
}

// ReauthResponse 是 OpenAPI ReauthResponse。
type ReauthResponse struct {
	ReauthAt  string `json:"reauth_at"`
	ExpiresAt string `json:"expires_at"`
}

func (h *Handlers) reauth(c *gin.Context) {
	p, ok := sessionPrincipal(c)
	if !ok {
		return
	}
	var req reauthRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	res, err := h.svc.Reauth(c.Request.Context(), p, req.Password, req.TOTPCode, reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, ReauthResponse{ReauthAt: ts(res.ReauthAt), ExpiresAt: ts(res.ExpiresAt)})
}
