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
)

// 会话吊销原因（sessions.revoke_reason）。
const (
	RevokeLogout           = "logout"
	RevokeCredentialChange = "password_change"
	RevokeRelogin          = "relogin" // 登录时旧会话被换新（会话固定防护）
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
// 权限集由 Story 1.18 装载；此前 HasPermission 恒为 false（fail-closed：不会因为缺少权限数据而放行任何东西）。
type Principal struct {
	User      User
	Session   SessionView
	Realm     Realm
	Tenant    *Tenant
	CSRFToken string // 由会话令牌派生（见 service.DeriveCSRF）；仅内存中使用
	// Bearer 为 true 表示由 API Token 认证（无会话；免 CSRF）。
	Bearer bool
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

// HasPermission 实现 tenantscope.Principal：1.18 之前恒为 false。
func (p *Principal) HasPermission(string) bool { return false }

// Settings 是认证相关的系统设置（system_settings；UD-27：默认值是待基线的工程假设，可配置）。
type Settings struct {
	SessionIdleTTL     time.Duration // 滑动过期
	SessionAbsoluteTTL time.Duration // 绝对过期
	ThrottleWindow     time.Duration
	ThrottleAccountMax int // 窗口内同一 identifier 的最大失败次数（超过即锁定）
	ThrottleIPMax      int // 窗口内同一来源 IP 的最大失败次数
	RunMode            string
}
