// Package domain 是认证模块的领域类型（Story 1.16、ADR-018）：realm、用户/会话/登录日志记录、已认证主体、系统设置。
// 不含 I/O；口令明文、会话令牌明文不会出现在这里的任何持久化类型里（只有哈希）。
package domain

import (
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// Realm 是登录入口：admin 仅 platform 用户，console 仅 tenant 用户；会话与 Cookie 互不通用（ADR-018 §1/§2）。
type Realm string

const (
	RealmAdmin   Realm = "admin"
	RealmConsole Realm = "console"
)

// Valid 报告 realm 取值是否合法。
func (r Realm) Valid() bool { return r == RealmAdmin || r == RealmConsole }

// CookieName 是该 realm 的会话 Cookie 名。
func (r Realm) CookieName() string {
	if r == RealmAdmin {
		return "le_admin_sid"
	}
	return "le_console_sid"
}

// UserType 是该 realm 允许登录的 users.user_type。
func (r Realm) UserType() string {
	if r == RealmAdmin {
		return UserTypePlatform
	}
	return UserTypeTenant
}

// users.user_type / status 取值。
const (
	UserTypePlatform = "platform"
	UserTypeTenant   = "tenant"
	StatusActive     = "active"
)

// 登录事件（login_logs.event）。
const (
	EventLoginSuccess = "login_success"
	EventLoginFailed  = "login_failed"
	EventLogout       = "logout"
	EventLocked       = "locked"
	EventTOTPFailed   = "totp_failed"
	// EventTokenAuthFailed：API Token 认证失败（令牌本身有效匹配但已过期/吊销/来源不符等；Story 1.19）。
	EventTokenAuthFailed = "token_auth_failed"
)

// 会话吊销原因（sessions.revoke_reason）。
const (
	RevokeLogout           = "logout"
	RevokeCredentialChange = "password_change"
	RevokeRelogin          = "relogin" // 登录时旧会话被换新（会话固定防护）
	RevokeAdmin            = "admin"   // 管理员吊销/重置口令/停用/删除账号（Story 1.18）
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("auth: 记录不存在")

// User 是 users 行的认证相关投影。
type User struct {
	ID               uint64     `db:"id"`
	UID              string     `db:"uid"`
	UserType         string     `db:"user_type"`
	Email            string     `db:"email"`
	DisplayName      string     `db:"display_name"`
	PasswordHash     string     `db:"password_hash"`
	Status           string     `db:"status"`
	Locale           string     `db:"locale"`
	TOTPEnabled      bool       `db:"totp_enabled"`
	MustChange       bool       `db:"must_change_password"`
	FailedLoginCount uint32     `db:"failed_login_count"`
	LockedUntil      *time.Time `db:"locked_until"`
	LastLoginAt      *time.Time `db:"last_login_at"`
	LastLoginIP      []byte     `db:"last_login_ip"`
}

// Tenant 是租户摘要（Console 会话的当前租户）。
type Tenant struct {
	ID        uint64 `db:"id"`
	UID       string `db:"uid"`
	Name      string `db:"name"`
	Slug      string `db:"slug"`
	IsDefault bool   `db:"is_default"`
}

// NewSession 是待写入的会话（只含哈希）。
type NewSession struct {
	UID               string
	TokenHash         string
	CSRFHash          string
	UserID            uint64
	Realm             Realm
	TenantID          *uint64
	IP                []byte
	UserAgent         string
	Now               time.Time
	ExpiresAt         time.Time
	AbsoluteExpiresAt time.Time
	// MFAVerifiedAt 非空表示建立会话时已通过第二因子（TOTP/恢复码）。
	MFAVerifiedAt *time.Time
}

// SessionView 是会话校验时一次查出的会话 + 用户 + 租户视图。
type SessionView struct {
	ID                uint64     `db:"id"`
	UID               string     `db:"uid"`
	CSRFHash          string     `db:"csrf_hash"`
	UserID            uint64     `db:"user_id"`
	Realm             string     `db:"realm"`
	TenantID          *uint64    `db:"tenant_id"`
	CreatedAt         time.Time  `db:"created_at"`
	LastSeenAt        time.Time  `db:"last_seen_at"`
	ExpiresAt         time.Time  `db:"expires_at"`
	AbsoluteExpiresAt time.Time  `db:"absolute_expires_at"`
	RevokedAt         *time.Time `db:"revoked_at"`
	MFAVerifiedAt     *time.Time `db:"mfa_verified_at"`
	ReauthAt          *time.Time `db:"reauth_at"`

	UserUID       string     `db:"user_uid"`
	UserType      string     `db:"user_type"`
	Email         string     `db:"email"`
	DisplayName   string     `db:"display_name"`
	UserStatus    string     `db:"user_status"`
	Locale        string     `db:"locale"`
	TOTPEnabled   bool       `db:"totp_enabled"`
	MustChange    bool       `db:"must_change_password"`
	UserDeletedAt *time.Time `db:"user_deleted_at"`
	TenantUID     *string    `db:"tenant_uid"`
	TenantName    *string    `db:"tenant_name"`
	TenantSlug    *string    `db:"tenant_slug"`
	TenantDefault *bool      `db:"tenant_is_default"`
	TenantStatus  *string    `db:"tenant_status"`
	MembershipOK  bool       `db:"member_ok"`
	// PermVersion 是 users.permission_version（权限集版本号，权限缓存键的一部分；Story 1.18）。
	PermVersion   uint64     `db:"permission_version"`
	TenantDeleted *time.Time `db:"tenant_deleted_at"`
}

// SessionRow 是会话列表行。
type SessionRow struct {
	ID         uint64    `db:"id"`
	UID        *string   `db:"uid"`
	Realm      string    `db:"realm"`
	IP         []byte    `db:"ip"`
	UserAgent  *string   `db:"user_agent"`
	CreatedAt  time.Time `db:"created_at"`
	LastSeenAt time.Time `db:"last_seen_at"`
	ExpiresAt  time.Time `db:"expires_at"`
}

// LoginLog 是 login_logs 行。
type LoginLog struct {
	ID         uint64    `db:"id"`
	UID        string    `db:"uid"`
	CreatedAt  time.Time `db:"created_at"`
	UserID     *uint64   `db:"user_id"`
	TenantID   *uint64   `db:"tenant_id"`
	Realm      string    `db:"realm"`
	Identifier string    `db:"identifier"`
	Event      string    `db:"event"`
	IP         []byte    `db:"ip"`
	UserAgent  *string   `db:"user_agent"`
	RequestID  *string   `db:"request_id"`
	Reason     *string   `db:"reason"`
}

// Access 是用户在某一范围（平台或某租户）内的角色与权限集（读取用，授权判定属 Story 1.18）。
type Access struct {
	Roles       []string
	Permissions []string
	MenuKeys    []string
}

// Principal 是认证钩子产出的已认证主体：实现 tenantscope.Principal，并携带会话信息供 /auth/* handler 使用。
// 权限集由授权钩子（Story 1.18）在判定前装载（SetPermissions）；装载之前 HasPermission 恒为 false（fail-closed：不会因为缺少权限数据而放行任何东西）。
type Principal struct {
	User      User
	Session   SessionView
	Realm     Realm
	Tenant    *Tenant
	CSRFToken string // 由会话令牌派生（见 service.DeriveCSRF）；仅内存中使用
	// Bearer 为 true 表示由 API Token 认证（无会话；免 CSRF）。此时 Session 只有创建者的 UserID/UserUID/PermVersion（ID 恒为 0）。
	Bearer bool
	// TokenID/TokenPrefix 仅 Bearer 主体有值（日志只记前缀）；TokenScopes 是令牌的作用域：Bearer 主体的有效权限 = TokenScopes ∩ 创建者**当前**权限。
	TokenID     uint64
	TokenPrefix string
	TokenScopes map[string]struct{}

	perms map[string]struct{}
}

var _ tenantscope.Principal = (*Principal)(nil)

// ActorID 实现 tenantscope.Principal。
func (p *Principal) ActorID() uint64 { return p.User.ID }

// IsPlatform 实现 tenantscope.Principal。
func (p *Principal) IsPlatform() bool { return p.User.UserType == UserTypePlatform }

// TenantID 实现 tenantscope.Principal。
func (p *Principal) TenantID() uint64 {
	if p.IsPlatform() || p.Tenant == nil {
		return 0
	}
	return p.Tenant.ID
}

// AuditActor 实现 audit.ActorProvider（返回基本类型，使 domain 不依赖审计包）：审计记录器据此取得操作者快照——
// 类型（user / api_token）、操作者用户 id（API Token 取其拥有者）、令牌 id、标签（邮箱 / 令牌前缀，绝不含令牌明文）、所属租户（平台用户为 0）。
func (p *Principal) AuditActor() (typ string, userID, tokenID uint64, label string, tenantID uint64) {
	if p.Bearer {
		return "api_token", p.User.ID, p.TokenID, p.TokenPrefix, p.TenantID()
	}
	return "user", p.User.ID, 0, p.User.Email, p.TenantID()
}

// SetPermissions 装载主体的权限集（平台主体：平台角色授予的权限；租户主体：其当前租户内角色授予的权限）。
// 只由授权钩子在请求开始时调用一次；主体是请求级对象，不跨请求共享。
func (p *Principal) SetPermissions(set map[string]struct{}) { p.perms = set }

// Permissions 返回已装载权限集的副本（升序不保证）。
func (p *Principal) Permissions() []string {
	out := make([]string, 0, len(p.perms))
	for k := range p.perms {
		out = append(out, k)
	}
	return out
}

// HasPermission 实现 tenantscope.Principal：权限集未装载（或不含该权限）一律为 false。
func (p *Principal) HasPermission(code string) bool {
	_, ok := p.perms[code]
	return ok
}

// Settings 是认证相关的系统设置（system_settings；UD-27：默认值是待基线的工程假设，可配置）。
type Settings struct {
	SessionIdleTTL     time.Duration // 滑动过期
	SessionAbsoluteTTL time.Duration // 绝对过期
	ThrottleWindow     time.Duration
	ThrottleAccountMax int // 窗口内同一 identifier 的最大失败次数（超过即锁定）
	ThrottleIPMax      int // 窗口内同一来源 IP 的最大失败次数
	RunMode            string
	// ReauthWindow 是 L4 再认证窗口（auth.reauth_window_s，默认 300 秒，UD-29）。
	ReauthWindow time.Duration
	// MFAWindow 是「近期 MFA」窗口（auth.mfa_window_s；基线与 UD 未规定默认值，工程假设 15 分钟）。
	MFAWindow time.Duration
	// TOTPRequiredForAdmin 是「强制管理员启用 TOTP」（login.totp_required_for_admin）。
	TOTPRequiredForAdmin bool
	// APITokenDefaultTTL 是创建 API Token 未给 expires_at 时的默认有效期；APITokenMaxTTL 是系统上限（Story 1.19，均为可配置的工程假设，UD-27）。
	APITokenDefaultTTL time.Duration
	APITokenMaxTTL     time.Duration
}

// MFAChallenge 是 login_mfa_challenges 行（登录第一步通过后发放的一次性 mfa_token 挑战）。
type MFAChallenge struct {
	ID         uint64     `db:"id"`
	UserID     uint64     `db:"user_id"`
	Realm      string     `db:"realm"`
	ExpiresAt  time.Time  `db:"expires_at"`
	Attempts   uint32     `db:"attempts"`
	ConsumedAt *time.Time `db:"consumed_at"`
}

// TOTPState 是用户的 TOTP 状态（含密钥密文；仅在 TOTP 流程内读取）。
type TOTPState struct {
	UID       string  `db:"uid"`
	Enabled   bool    `db:"enabled"`
	SecretEnc *string `db:"secret_enc"`
	LastStep  *uint64 `db:"last_step"`
}

// AdminUser 是 /admin/users 视图所需的 users 行（平台用户管理，Story 1.18）。
type AdminUser struct {
	ID          uint64     `db:"id"`
	UID         string     `db:"uid"`
	UserType    string     `db:"user_type"`
	Email       string     `db:"email"`
	DisplayName string     `db:"display_name"`
	Status      string     `db:"status"`
	Locale      string     `db:"locale"`
	Timezone    string     `db:"timezone"`
	TOTPEnabled bool       `db:"totp_enabled"`
	MustChange  bool       `db:"must_change_password"`
	LastLoginAt *time.Time `db:"last_login_at"`
	LastLoginIP []byte     `db:"last_login_ip"`
	LockedUntil *time.Time `db:"locked_until"`
	PermVersion uint64     `db:"permission_version"`
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

// NewUser 是待创建的平台用户（只含口令哈希）。
type NewUser struct {
	UID          string
	Email        string
	DisplayName  string
	PasswordHash string
	Locale       string
	Now          time.Time
	// KeepPassword 为真表示口令由本人当场设定（安装向导创建首个超级管理员，Story 1.24）：不强制首次登录改密（must_change_password=0）。
	KeepPassword bool
}

// Role 是 roles 行（模板角色：tenant_id 为 NULL）。
type Role struct {
	ID          uint64    `db:"id"`
	UID         string    `db:"uid"`
	Scope       string    `db:"scope"`
	Code        string    `db:"code"`
	Name        string    `db:"name"`
	Description *string   `db:"description"`
	IsBuiltin   bool      `db:"is_builtin"`
	CreatedAt   time.Time `db:"created_at"`
}

// Permission 是权限点字典行。
type Permission struct {
	Code        string  `db:"code"`
	Resource    string  `db:"resource"`
	Action      string  `db:"action"`
	Scope       string  `db:"scope"`
	Module      string  `db:"module"`
	MenuKey     *string `db:"menu_key"`
	Description *string `db:"description"`
}

// 内置角色与权限常量。
const (
	RoleSuperAdmin = "platform_admin" // 平台超级管理员：最后一个不得被删除/停用/降权
	ScopePlatform  = "platform"
	ScopeTenantStr = "tenant"
)

// AffectedAccess 是一次权限变更波及的（用户, 范围）与其新的权限版本：用于精确清除旧版本的缓存键并广播。
type AffectedAccess struct {
	UserUID string `db:"uid"`
	// ScopeID 是 user_roles.tenant_key：0 = 平台范围，否则为租户 id。
	ScopeID uint64 `db:"tenant_key"`
	Version uint64 `db:"permission_version"`
}
