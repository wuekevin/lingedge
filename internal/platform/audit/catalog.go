package audit

// Action 是审计动作（audit_logs.action，如 user.create、auth.login）。只有本文件登记的动作可被记录。
type Action string

// 动作清单。命名：<资源>.<动作>；与 OpenAPI x-audit: true 的 operation 经 Catalog 一一对应。
const (
	ActionAuthLogin          Action = "auth.login"
	ActionAuthMFAChallenge   Action = "auth.mfa_challenge"
	ActionAuthLogout         Action = "auth.logout"
	ActionAuthSessionRevoke  Action = "auth.session.revoke"
	ActionAuthPasswordChange Action = "auth.password.change"
	ActionAuthReauth         Action = "auth.reauth"
	ActionTOTPSetup          Action = "auth.totp.setup"
	ActionTOTPEnable         Action = "auth.totp.enable"
	ActionTOTPDisable        Action = "auth.totp.disable"

	ActionUserCreate         Action = "user.create"
	ActionUserUpdate         Action = "user.update"
	ActionUserDelete         Action = "user.delete"
	ActionUserSetRoles       Action = "user.set_roles"
	ActionUserUnlock         Action = "user.unlock"
	ActionUserResetPassword  Action = "user.reset_password"
	ActionUserRevokeSessions Action = "user.revoke_sessions"

	ActionRoleCreate         Action = "role.create"
	ActionRoleUpdate         Action = "role.update"
	ActionRoleDelete         Action = "role.delete"
	ActionRoleSetPermissions Action = "role.set_permissions"

	ActionAPITokenCreate Action = "api_token.create"
	ActionAPITokenRotate Action = "api_token.rotate"
	ActionAPITokenRevoke Action = "api_token.revoke"

	ActionTenantUpdate    Action = "tenant.update"
	ActionTenantMemberAdd Action = "tenant.member.add"
	ActionTenantQuotasPut Action = "tenant.quotas.put"

	ActionTaskCancel Action = "task.cancel"
)

// 资源类型（audit_logs.resource_type）。
const (
	ResourceSession  = "session"
	ResourceUser     = "user"
	ResourceRole     = "role"
	ResourceAPIToken = "api_token"
	ResourceTenant   = "tenant"
	ResourceTask     = "task"
)

// Op 登记一个要求审计（x-audit: true）的 operation 可以记录的动作：请求上下文里的 operationId 与被记录动作不匹配即拒绝（编程错误，fail-closed）。
type Op struct {
	OperationID string
	Actions     []Action
}

// Catalog 是「要求审计的 operation → 动作」的登记表（唯一来源）。门禁 `gates audit` 要求：
//   - 每个动作都至少有一处 `.Record(ctx, audit.ActionX, …)` 调用（声明了却没有记录者即失败）；
//   - 运行时路由集合里每个 Audit=true 的 operation 都在此登记（见 contracttest.CheckAuditCoverage），且 operationId 存在于 openapi 并声明 x-audit: true。
var Catalog = []Op{
	{"login", []Action{ActionAuthLogin, ActionAuthMFAChallenge}},
	{"verifyTotp", []Action{ActionAuthLogin}},
	{"logout", []Action{ActionAuthLogout}},
	{"revokeSession", []Action{ActionAuthSessionRevoke}},
	{"changePassword", []Action{ActionAuthPasswordChange}},
	{"reauth", []Action{ActionAuthReauth}},
	{"setupTotp", []Action{ActionTOTPSetup}},
	{"enableTotp", []Action{ActionTOTPEnable}},
	{"disableTotp", []Action{ActionTOTPDisable}},

	{"createUser", []Action{ActionUserCreate}},
	{"updateUser", []Action{ActionUserUpdate}},
	{"deleteUser", []Action{ActionUserDelete}},
	{"setUserRoles", []Action{ActionUserSetRoles}},
	{"unlockUser", []Action{ActionUserUnlock}},
	{"resetUserPassword", []Action{ActionUserResetPassword}},
	{"revokeUserSessions", []Action{ActionUserRevokeSessions}},

	{"createRole", []Action{ActionRoleCreate}},
	{"updateRole", []Action{ActionRoleUpdate}},
	{"deleteRole", []Action{ActionRoleDelete}},
	{"setRolePermissions", []Action{ActionRoleSetPermissions}},

	{"createApiToken", []Action{ActionAPITokenCreate}},
	{"rotateApiToken", []Action{ActionAPITokenRotate}},
	{"revokeApiToken", []Action{ActionAPITokenRevoke}},

	{"updateTenant", []Action{ActionTenantUpdate}},
	{"addTenantMember", []Action{ActionTenantMemberAdd}},
	{"putTenantQuotas", []Action{ActionTenantQuotasPut}},

	{"cancelTask", []Action{ActionTaskCancel}},
}

// ActionsFor 返回 operationId 登记的动作；未登记返回 nil。
func ActionsFor(operationID string) []Action {
	for _, o := range Catalog {
		if o.OperationID == operationID {
			return o.Actions
		}
	}
	return nil
}

// Registered 报告 operationId 是否已在 Catalog 登记。
func Registered(operationID string) bool { return ActionsFor(operationID) != nil }

// knownAction 报告动作是否出现在 Catalog 的任一 operation 下。
func knownAction(a Action) bool {
	for _, o := range Catalog {
		for _, x := range o.Actions {
			if x == a {
				return true
			}
		}
	}
	return false
}
