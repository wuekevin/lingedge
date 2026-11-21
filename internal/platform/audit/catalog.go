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

	// 系统设置（Story 1.22）。
	ActionSettingUpdate       Action = "setting.update"
	ActionSettingBrandUpdate  Action = "setting.brand.update"
	ActionSSRFAllowlistUpdate Action = "setting.ssrf_allowlist.update"
	ActionRunModeUpdate       Action = "system.run_mode.update"
)

// 系统发起的动作（Story 1.23）：不是任何 REST operation 触发的、由安装向导/CLI/后台流程在业务事务内写入的审计动作（Actor 为系统组件，经 Event.Actor 显式给出）。
// 它们**不属于任何 operation 的 Catalog 登记**（没有对应的 x-audit operation）；门禁 `gates audit` 对它们有独立规则：
// 必须登记在 SystemActions、不得出现在 Catalog、必须有 `.Record(ctx, audit.ActionX, …)` 调用者。
const (
	// ActionSigningKeysInit：信任域②③密钥初始化（配置签名密钥、私有 CA、Server gRPC 叶证书与 deploy.lineage，internal/platform/signingkeys）。
	ActionSigningKeysInit Action = "system.signing_keys.init"
)

// SystemActions 是系统发起的动作清单（唯一来源，见上）。
var SystemActions = []Action{ActionSigningKeysInit}

// 被拒绝类动作（Story 1.22b）：已认证主体被授权类拒绝（403 auth.forbidden / auth.reauth_required / feature.disabled）。它们**不属于任何 operation 的
// Catalog 登记**（不是业务变更、不在业务事务内写入），只由审计钩子的 DeniedRecorder（denied.go）写入，result=denied；
// 门禁 `gates audit` 对它们有独立规则（audit-denied-*）：必须登记在 DeniedActions、不得出现在 Catalog、不得被业务代码经 Record 写入、denied.go 必须真有写入实现。
const (
	ActionAccessDenied Action = "access.denied"
)

// DeniedActions 是被拒绝类动作清单（唯一来源，见上）。
var DeniedActions = []Action{ActionAccessDenied}

// 资源类型（audit_logs.resource_type）。
const (
	// ResourceOperation 是被拒绝类审计的对象：被拒绝访问的 operation（resource_uid = operationId）。
	ResourceOperation = "operation"

	ResourceSession  = "session"
	ResourceUser     = "user"
	ResourceRole     = "role"
	ResourceAPIToken = "api_token"
	ResourceTenant   = "tenant"
	ResourceTask     = "task"
	ResourceSetting  = "setting"
	ResourceSystem   = "system"
	// ResourceSigningKey 是签名/CA 密钥初始化与（后续）轮换的审计对象（Story 1.23）。
	ResourceSigningKey = "signing_key"
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

	{"patchSettings", []Action{ActionSettingUpdate}},
	{"putBrandSettings", []Action{ActionSettingBrandUpdate}},
	{"putSsrfAllowlist", []Action{ActionSSRFAllowlistUpdate}},
	{"putRunMode", []Action{ActionRunModeUpdate}},
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

// knownAction 报告动作是否出现在 Catalog 的任一 operation 下，或属于系统发起的动作（SystemActions）。
func knownAction(a Action) bool {
	if isSystemAction(a) {
		return true
	}
	for _, o := range Catalog {
		for _, x := range o.Actions {
			if x == a {
				return true
			}
		}
	}
	return false
}

func isSystemAction(a Action) bool {
	for _, x := range SystemActions {
		if x == a {
			return true
		}
	}
	return false
}
