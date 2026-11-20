package domain

import "time"

// API Token（Story 1.19、ADR-018 §7）的领域类型。令牌明文不出现在任何持久化类型里：库里只有 SHA-256 与可检索前缀。

// APIToken 是 api_tokens 行（元数据视图）；Hash 只用于服务内部比对，不对外序列化。
type APIToken struct {
	ID            uint64     `db:"id"`
	UID           string     `db:"uid"`
	CreatedAt     time.Time  `db:"created_at"`
	TenantID      *uint64    `db:"tenant_id"`
	TenantUID     *string    `db:"tenant_uid"`
	UserID        uint64     `db:"user_id"`
	Name          string     `db:"name"`
	Prefix        string     `db:"token_prefix"`
	Hash          string     `db:"token_hash"`
	ScopesJSON    []byte     `db:"scopes_json"`
	CIDRsJSON     []byte     `db:"allowed_cidrs_json"`
	ExpiresAt     *time.Time `db:"expires_at"`
	LastUsedAt    *time.Time `db:"last_used_at"`
	LastUsedIP    []byte     `db:"last_used_ip"`
	RevokedAt     *time.Time `db:"revoked_at"`
	RotatedFromID *uint64    `db:"rotated_from_id"`
	// RotatedFromUID 是轮换来源令牌的 uid（只读视图）。
	RotatedFromUID *string `db:"rotated_from_uid"`
}

// NewAPIToken 是待写入的令牌（只含哈希）。
type NewAPIToken struct {
	UID           string
	Hash          string
	Prefix        string
	Name          string
	UserID        uint64
	TenantID      *uint64 // nil = 平台令牌
	ScopesJSON    []byte
	CIDRsJSON     []byte // nil = 不限制来源
	ExpiresAt     *time.Time
	RotatedFromID *uint64
	Now           time.Time
}

// TokenAuthView 是 Bearer 认证时「按令牌 id 现读」的令牌 + 创建者 + 租户视图：授权相关状态（吊销/过期/创建者状态/成员关系/权限版本）
// 一律取自 MySQL 当前值，不经任何缓存。
type TokenAuthView struct {
	TokenID       uint64     `db:"token_id"`
	TokenUID      string     `db:"token_uid"`
	TokenHash     string     `db:"token_hash"`
	Prefix        string     `db:"token_prefix"`
	ScopesJSON    []byte     `db:"scopes_json"`
	CIDRsJSON     []byte     `db:"allowed_cidrs_json"`
	ExpiresAt     *time.Time `db:"expires_at"`
	RevokedAt     *time.Time `db:"revoked_at"`
	TokenTenantID *uint64    `db:"tenant_id"`

	UserID        uint64     `db:"user_id"`
	UserUID       string     `db:"user_uid"`
	UserType      string     `db:"user_type"`
	Email         string     `db:"email"`
	DisplayName   string     `db:"display_name"`
	UserStatus    string     `db:"user_status"`
	Locale        string     `db:"locale"`
	TOTPEnabled   bool       `db:"totp_enabled"`
	MustChange    bool       `db:"must_change_password"`
	UserDeletedAt *time.Time `db:"user_deleted_at"`
	PermVersion   uint64     `db:"permission_version"`

	TenantUID     *string    `db:"tenant_uid"`
	TenantName    *string    `db:"tenant_name"`
	TenantSlug    *string    `db:"tenant_slug"`
	TenantDefault *bool      `db:"tenant_is_default"`
	TenantStatus  *string    `db:"tenant_status"`
	TenantDeleted *time.Time `db:"tenant_deleted_at"`
	MembershipOK  bool       `db:"member_ok"`
}
