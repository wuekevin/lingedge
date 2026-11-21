package handler

import (
	"encoding/json"
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/auth/internal/service"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// /admin/users|roles|permissions（Story 1.18）。路由元数据与 docs/api/openapi.yaml 的 x-permissions / x-tenant-scope / x-audit / x-reauth / x-phase
// 及 IdempotencyKey 参数逐项一致（make contract-check 与 contracttest 核对）；全部经 httpx.Router.Register 注册，没有 501 占位。
// 授权由 RBAC 授权钩子（authorizer.go）判定；x-reauth 由 httpx.WithReauth（先权限后再认证）；写操作的审计由 Service 在业务事务内写 audit_logs（Story 1.21）。
var (
	adminRoute = func(id, method, path string, perms []string, audit, reauth, idem bool) httpx.RouteMeta {
		return httpx.RouteMeta{OperationID: id, Method: method, Path: path, Auth: httpx.AuthRequired, Permissions: perms,
			TenantScope: httpx.ScopePlatform, Audit: audit, Reauth: reauth, Idempotent: idem, Phase: "M1"}
	}
	MetaListUsers          = adminRoute("listUsers", "GET", "/admin/users", []string{"user:read"}, false, false, false)
	MetaCreateUser         = adminRoute("createUser", "POST", "/admin/users", []string{"user:write"}, true, true, true)
	MetaGetUser            = adminRoute("getUser", "GET", "/admin/users/{userId}", []string{"user:read"}, false, false, false)
	MetaUpdateUser         = adminRoute("updateUser", "PATCH", "/admin/users/{userId}", []string{"user:write"}, true, false, false)
	MetaDeleteUser         = adminRoute("deleteUser", "DELETE", "/admin/users/{userId}", []string{"user:write"}, true, true, false)
	MetaSetUserRoles       = adminRoute("setUserRoles", "PUT", "/admin/users/{userId}/roles", []string{"user:write", "role:write"}, true, true, false)
	MetaUnlockUser         = adminRoute("unlockUser", "POST", "/admin/users/{userId}/unlock", []string{"user:write"}, true, false, false)
	MetaResetUserPassword  = adminRoute("resetUserPassword", "POST", "/admin/users/{userId}/reset-password", []string{"user:write"}, true, true, false)
	MetaRevokeUserSessions = adminRoute("revokeUserSessions", "DELETE", "/admin/users/{userId}/sessions", []string{"user:write"}, true, false, false)
	MetaListRoles          = adminRoute("listRoles", "GET", "/admin/roles", []string{"role:read"}, false, false, false)
	MetaCreateRole         = adminRoute("createRole", "POST", "/admin/roles", []string{"role:write"}, true, true, true)
	MetaGetRole            = adminRoute("getRole", "GET", "/admin/roles/{roleId}", []string{"role:read"}, false, false, false)
	MetaUpdateRole         = adminRoute("updateRole", "PATCH", "/admin/roles/{roleId}", []string{"role:write"}, true, true, false)
	MetaDeleteRole         = adminRoute("deleteRole", "DELETE", "/admin/roles/{roleId}", []string{"role:write"}, true, true, false)
	MetaSetRolePermissions = adminRoute("setRolePermissions", "PUT", "/admin/roles/{roleId}/permissions", []string{"role:write"}, true, true, false)
	MetaListPermissions    = adminRoute("listPermissions", "GET", "/admin/permissions", []string{"role:read"}, false, false, false)
	MetaListLoginLogs      = adminRoute("listLoginLogs", "GET", "/admin/login-logs", []string{"login-log:read"}, false, false, false)
)

// AdminMetas 是本 Story 注册的全部 /admin 路由元数据。
func AdminMetas() []httpx.RouteMeta {
	return []httpx.RouteMeta{MetaListUsers, MetaCreateUser, MetaGetUser, MetaUpdateUser, MetaDeleteUser, MetaSetUserRoles, MetaUnlockUser,
		MetaResetUserPassword, MetaRevokeUserSessions, MetaListRoles, MetaCreateRole, MetaGetRole, MetaUpdateRole, MetaDeleteRole,
		MetaSetRolePermissions, MetaListPermissions, MetaListLoginLogs}
}

// IsAdminOperation 报告 operationID 是否属于本模块的 /admin 管理路由。
func IsAdminOperation(operationID string) bool {
	for _, m := range AdminMetas() {
		if m.OperationID == operationID {
			return true
		}
	}
	return false
}

func (h *Handlers) registerAdmin(r *httpx.Router) error {
	return errors.Join(
		r.Register(MetaListUsers, h.listUsers),
		r.Register(MetaCreateUser, h.createUser),
		r.Register(MetaGetUser, h.getUser),
		r.Register(MetaUpdateUser, h.updateUser),
		r.Register(MetaDeleteUser, h.deleteUser),
		r.Register(MetaSetUserRoles, h.setUserRoles),
		r.Register(MetaUnlockUser, h.unlockUser),
		r.Register(MetaResetUserPassword, h.resetUserPassword),
		r.Register(MetaRevokeUserSessions, h.revokeUserSessions),
		r.Register(MetaListRoles, h.listRoles),
		r.Register(MetaCreateRole, h.createRole),
		r.Register(MetaGetRole, h.getRole),
		r.Register(MetaUpdateRole, h.updateRole),
		r.Register(MetaDeleteRole, h.deleteRole),
		r.Register(MetaSetRolePermissions, h.setRolePermissions),
		r.Register(MetaListPermissions, h.listPermissions),
		r.Register(MetaListLoginLogs, h.listAllLoginLogs),
	)
}

// ---- 视图（OpenAPI User / Role / Permission）----

type userJSON struct {
	UID                string   `json:"uid"`
	Email              string   `json:"email"`
	DisplayName        string   `json:"display_name"`
	UserType           string   `json:"user_type"`
	Status             string   `json:"status"`
	Locale             string   `json:"locale"`
	Timezone           string   `json:"timezone"`
	TOTPEnabled        bool     `json:"totp_enabled"`
	MustChangePassword bool     `json:"must_change_password"`
	LastLoginAt        *string  `json:"last_login_at"`
	LastLoginIP        *string  `json:"last_login_ip"`
	RoleCodes          []string `json:"role_codes"`
	CreatedAt          string   `json:"created_at"`
	UpdatedAt          string   `json:"updated_at"`
	// TemporaryPassword 仅在服务端生成口令的创建响应里出现一次（不入日志、不入幂等记录）。
	TemporaryPassword string `json:"temporary_password,omitempty"`
}

func toUserJSON(v service.UserView) userJSON {
	loc := v.Locale
	if loc != "zh-CN" && loc != "en" {
		loc = "zh-CN"
	}
	u := userJSON{UID: v.UID, Email: v.Email, DisplayName: v.DisplayName, UserType: v.UserType, Status: v.Status, Locale: loc, Timezone: v.Timezone,
		TOTPEnabled: v.TOTPEnabled, MustChangePassword: v.MustChange, RoleCodes: v.RoleCodes, CreatedAt: ts(v.CreatedAt), UpdatedAt: ts(v.UpdatedAt)}
	if u.RoleCodes == nil {
		u.RoleCodes = []string{}
	}
	if v.LastLoginAt != nil {
		s := ts(*v.LastLoginAt)
		u.LastLoginAt = &s
	}
	if len(v.LastLoginIP) > 0 {
		s := service.IPString(v.LastLoginIP)
		u.LastLoginIP = &s
	}
	return u
}

type roleJSON struct {
	UID         string   `json:"uid"`
	Scope       string   `json:"scope"`
	Code        string   `json:"code"`
	Name        string   `json:"name"`
	Description *string  `json:"description"`
	IsBuiltin   bool     `json:"is_builtin"`
	Permissions []string `json:"permissions"`
	TenantUID   *string  `json:"tenant_uid"`
	CreatedAt   string   `json:"created_at"`
}

func toRoleJSON(v service.RoleView) roleJSON {
	p := v.Permissions
	if p == nil {
		p = []string{}
	}
	return roleJSON{UID: v.UID, Scope: v.Scope, Code: v.Code, Name: v.Name, Description: v.Description, IsBuiltin: v.IsBuiltin, Permissions: p, CreatedAt: ts(v.CreatedAt)}
}

type permissionJSON struct {
	Code        string  `json:"code"`
	Resource    string  `json:"resource"`
	Action      string  `json:"action"`
	Scope       string  `json:"scope"`
	Module      string  `json:"module"`
	MenuKey     *string `json:"menu_key"`
	Description *string `json:"description"`
}

// ---- 用户 ----

func (h *Handlers) listUsers(c *gin.Context) {
	pg, err := httpx.ParsePage(c.Request.URL.Query())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	srt, err := httpx.ParseSort(c.Request.URL.Query(), []string{"created_at", "email"}, httpx.Sort{Field: "created_at"})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	items, total, err := h.svc.ListUsers(c.Request.Context(), pg.Limit(), pg.Offset(), srt.Field, srt.Desc)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := make([]userJSON, 0, len(items))
	for _, it := range items {
		out = append(out, toUserJSON(it))
	}
	httpx.OK(c, httpx.NewPageResult(out, pg, total))
}

func (h *Handlers) getUser(c *gin.Context) {
	v, err := h.svc.GetUser(c.Request.Context(), c.Param("userId"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, toUserJSON(v))
}

type createUserRequest struct {
	Email           string   `json:"email"`
	DisplayName     string   `json:"display_name"`
	UserType        string   `json:"user_type"`
	InitialPassword string   `json:"initial_password"`
	RoleCodes       []string `json:"role_codes"`
	Locale          string   `json:"locale"`
}

func (h *Handlers) createUser(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	var req createUserRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	v, temp, err := h.svc.CreateUser(c.Request.Context(), p, service.CreateUserInput{Email: req.Email, DisplayName: req.DisplayName, UserType: req.UserType,
		InitialPassword: req.InitialPassword, RoleCodes: req.RoleCodes, Locale: req.Locale}, reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	u := toUserJSON(v)
	if temp != "" {
		// 服务端生成的一次性口令：只回显这一次；幂等记录里保存的是不含口令的响应体（重放不会再次回显，也不把明文写进 MySQL）。
		redacted, merr := json.Marshal(u)
		if merr != nil {
			httpx.Fail(c, apierr.Internal(merr))
			return
		}
		httpx.SetReplayBody(c, append(redacted, '\n'))
		u.TemporaryPassword = temp
		c.Header("Cache-Control", "no-store")
	}
	httpx.Created(c, httpx.APIPrefix+"/admin/users/"+v.UID, u)
}

type updateUserRequest struct {
	DisplayName *string `json:"display_name"`
	Status      *string `json:"status"`
	Locale      *string `json:"locale"`
	Timezone    *string `json:"timezone"`
}

func (h *Handlers) updateUser(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	var req updateUserRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	v, err := h.svc.UpdateUser(c.Request.Context(), p, c.Param("userId"), service.UpdateUserInput{DisplayName: req.DisplayName, Status: req.Status, Locale: req.Locale, Timezone: req.Timezone}, reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, toUserJSON(v))
}

func (h *Handlers) deleteUser(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteUser(c.Request.Context(), p, c.Param("userId"), reqInfo(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.NoContent(c)
}

type setUserRolesRequest struct {
	RoleCodes *[]string `json:"role_codes"`
}

func (h *Handlers) setUserRoles(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	var req setUserRolesRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	if req.RoleCodes == nil {
		httpx.Fail(c, apierr.Validation(apierr.Detail{Field: "role_codes", Reason: "is required"}))
		return
	}
	v, err := h.svc.SetUserRoles(c.Request.Context(), p, c.Param("userId"), *req.RoleCodes, reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, toUserJSON(v))
}

func (h *Handlers) unlockUser(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	v, err := h.svc.UnlockUser(c.Request.Context(), p, c.Param("userId"), reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, toUserJSON(v))
}

type resetPasswordRequest struct {
	NewPassword string `json:"new_password"`
}

// temporaryPasswordJSON 是 resetUserPassword 在服务端生成口令时的 200 响应（OpenAPI TemporaryPassword）。
type temporaryPasswordJSON struct {
	TemporaryPassword string `json:"temporary_password"`
}

func (h *Handlers) resetUserPassword(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	var req resetPasswordRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	temp, err := h.svc.ResetPassword(c.Request.Context(), p, c.Param("userId"), req.NewPassword, reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if temp == "" { // 管理员自己设定了临时口令：204，不回显
		httpx.NoContent(c)
		return
	}
	c.Header("Cache-Control", "no-store")
	httpx.OK(c, temporaryPasswordJSON{TemporaryPassword: temp})
}

func (h *Handlers) revokeUserSessions(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	if err := h.svc.RevokeUserSessions(c.Request.Context(), p, c.Param("userId"), reqInfo(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.NoContent(c)
}

// ---- 角色 ----

func (h *Handlers) listRoles(c *gin.Context) {
	pg, err := httpx.ParsePage(c.Request.URL.Query())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if _, err := httpx.ParseSort(c.Request.URL.Query(), []string{"code"}, httpx.Sort{Field: "code"}); err != nil {
		httpx.Fail(c, err)
		return
	}
	items, total, err := h.svc.ListRoles(c.Request.Context(), pg.Limit(), pg.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := make([]roleJSON, 0, len(items))
	for _, it := range items {
		out = append(out, toRoleJSON(it))
	}
	httpx.OK(c, httpx.NewPageResult(out, pg, total))
}

func (h *Handlers) getRole(c *gin.Context) {
	v, err := h.svc.GetRole(c.Request.Context(), c.Param("roleId"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, toRoleJSON(v))
}

type createRoleRequest struct {
	Scope       string   `json:"scope"`
	Code        string   `json:"code"`
	Name        string   `json:"name"`
	Description *string  `json:"description"`
	Permissions []string `json:"permissions"`
}

func (h *Handlers) createRole(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	var req createRoleRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	v, err := h.svc.CreateRole(c.Request.Context(), p, service.CreateRoleInput{Scope: req.Scope, Code: req.Code, Name: req.Name, Description: req.Description, Permissions: req.Permissions}, reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.Created(c, httpx.APIPrefix+"/admin/roles/"+v.UID, toRoleJSON(v))
}

type updateRoleRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

func (h *Handlers) updateRole(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	var req updateRoleRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	v, err := h.svc.UpdateRole(c.Request.Context(), p, c.Param("roleId"), req.Name, req.Description, reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, toRoleJSON(v))
}

func (h *Handlers) deleteRole(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteRole(c.Request.Context(), p, c.Param("roleId"), reqInfo(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.NoContent(c)
}

type setRolePermissionsRequest struct {
	Permissions *[]string `json:"permissions"`
}

func (h *Handlers) setRolePermissions(c *gin.Context) {
	p, ok := principal(c)
	if !ok {
		return
	}
	var req setRolePermissionsRequest
	if err := httpx.BindJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	if req.Permissions == nil {
		httpx.Fail(c, apierr.Validation(apierr.Detail{Field: "permissions", Reason: "is required"}))
		return
	}
	v, err := h.svc.SetRolePermissions(c.Request.Context(), p, c.Param("roleId"), *req.Permissions, reqInfo(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, toRoleJSON(v))
}

// ---- 权限点字典 ----

func (h *Handlers) listPermissions(c *gin.Context) {
	pg, err := httpx.ParsePage(c.Request.URL.Query())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	items, total, err := h.svc.ListPermissions(c.Request.Context(), pg.Limit(), pg.Offset())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	out := make([]permissionJSON, 0, len(items))
	for _, it := range items {
		out = append(out, permissionJSON{Code: it.Code, Resource: it.Resource, Action: it.Action, Scope: it.Scope, Module: it.Module, MenuKey: it.MenuKey, Description: it.Description})
	}
	httpx.OK(c, httpx.NewPageResult(out, pg, total))
}
