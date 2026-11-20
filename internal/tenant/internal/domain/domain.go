// Package domain 是租户模块的领域类型（Story 1.20）。不含 I/O。
package domain

import (
	"errors"
	"time"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("tenant: 记录不存在")

// ErrConflict 表示唯一性或状态冲突。
var ErrConflict = errors.New("tenant: 冲突")

// tenants.status 取值。
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusClosed    = "closed"
	StatusPending   = "pending"
)

// Tenant 是 tenants 行（含所有者 uid；已排除软删除）。
type Tenant struct {
	ID           uint64    `db:"id"`
	UID          string    `db:"uid"`
	Name         string    `db:"name"`
	Slug         string    `db:"slug"`
	Status       string    `db:"status"`
	IsDefault    bool      `db:"is_default"`
	ContactEmail *string   `db:"contact_email"`
	Locale       string    `db:"locale"`
	OwnerUID     *string   `db:"owner_uid"`
	SettingsJSON []byte    `db:"settings_json"`
	CreatedAt    time.Time `db:"created_at"`
	UpdatedAt    time.Time `db:"updated_at"`
}

// TenantUpdate 是 PATCH 的字段集合（nil = 不改）；ContactEmail 指向空串表示清除。
type TenantUpdate struct {
	Name         *string
	ContactEmail *string
	Locale       *string
	SettingsJSON []byte // 非 nil 表示整体替换
}

// Member 是租户成员视图（users + tenant_members）。
type Member struct {
	UserID      uint64     `db:"user_id"`
	UserUID     string     `db:"user_uid"`
	Email       string     `db:"email"`
	DisplayName string     `db:"display_name"`
	UserType    string     `db:"user_type"`
	Locale      string     `db:"locale"`
	TOTPEnabled bool       `db:"totp_enabled"`
	MustChange  bool       `db:"must_change_password"`
	Status      string     `db:"status"`
	IsOwner     bool       `db:"is_owner"`
	JoinedAt    *time.Time `db:"joined_at"`
	RolesJSON   []byte     `db:"roles_json"`
}

// MemberRow 是 tenant_members 行（加成员时判断既有关系）。
type MemberRow struct {
	ID     uint64 `db:"id"`
	Status string `db:"status"`
}

// UserRef 是按邮箱找到的用户。
type UserRef struct {
	ID       uint64 `db:"id"`
	UID      string `db:"uid"`
	UserType string `db:"user_type"`
}

// Quota 是 tenant_quotas 行。
type Quota struct {
	Key    string `db:"quota_key"`
	Limit  *int64 `db:"limit_value"`
	Source string `db:"source"`
}

// QuotaKeys 是配额键字典（docs/database/tenant-site.md §tenant_quotas）：只存取，强制执行属 Story 6.9。
var QuotaKeys = []string{"api_tokens", "cache_tasks_per_day", "certificates", "domains_per_site", "l4_applications", "members", "rules_per_site", "sites"}

// KnownQuotaKey 报告配额键是否在字典内。
func KnownQuotaKey(k string) bool {
	for _, q := range QuotaKeys {
		if q == k {
			return true
		}
	}
	return false
}
