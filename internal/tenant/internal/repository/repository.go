// Package repository 是租户模块的数据访问（Story 1.20）。所有 SQL 为编译期常量 + 占位符；写操作由 Service 用 WithTx 划定事务边界。
//
// Scope 规则（ADR-009 §1、make repo-check）：
//   - 解析租户目录（FindTenantByUID）发生在 Scope 钩子内部、Scope 产生之前，登记为平台级表访问；
//   - 其余方法首参均为 scope.Scope：租户路由（/tenants/current）只取 Scope 自身的租户；平台路由（/tenants*，x-tenant-scope: platform）
//     以平台 Scope + 路径里的目标租户操作，并在内部校验 Scope 确为平台范围；只涉及 tenants/tenant_quotas 的跨租户方法名带 AcrossTenants
//     （repo 门禁：AcrossTenants 方法不得使用平台表/身份表豁免，故联结 users/tenant_members/user_roles/roles 的方法不用该后缀，但同样以 Scope 为首参）。
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/tenant/internal/domain"
)

// ErrWrongRole：只接受 app 连接池。
var ErrWrongRole = errors.New("tenant/repository: 只接受 app 连接池")

// ErrNotPlatformScope：AcrossTenants 方法要求平台范围 Scope。
var ErrNotPlatformScope = errors.New("tenant/repository: 该方法要求平台范围 Scope")

// Repo 是租户模块的 Repository。
type Repo struct{ pool db.Pool }

// New 创建 Repository。
func New(pool db.Pool) (*Repo, error) {
	if pool == nil || pool.Role() != db.RoleApp {
		return nil, ErrWrongRole
	}
	return &Repo{pool: pool}, nil
}

func (r *Repo) ex(ctx context.Context) db.Executor { return r.pool.Executor(ctx) }

func notFound(err error) error {
	if errors.Is(err, db.ErrNotFound) {
		return domain.ErrNotFound
	}
	return err
}

func platformOnly(sc scope.Scope) error {
	if !sc.IsPlatform() {
		return ErrNotPlatformScope
	}
	return nil
}

const tenantCols = `t.id, t.uid, t.name, t.slug, t.status, t.is_default, t.contact_email, t.locale, o.uid AS owner_uid, t.settings_json, t.created_at, t.updated_at`
const tenantFrom = ` FROM tenants t LEFT JOIN users o ON o.id = t.owner_user_id`

// FindTenantByUID 按 uid 取未删除的租户（租户目录与平台路由共用）。不存在返回 domain.ErrNotFound。
// lingedge:platform-table=tenants,users
func (r *Repo) FindTenantByUID(ctx context.Context, uid string) (domain.Tenant, error) {
	var t domain.Tenant
	err := r.ex(ctx).Get(ctx, &t, db.Q(`SELECT `+tenantCols+tenantFrom+` WHERE t.uid = ? AND t.deleted_at IS NULL`), uid)
	return t, notFound(err)
}

// GetScopedTenant 取 Scope 所限定的租户（租户范围 Scope：自己的租户或 acting-as 的目标租户）。平台范围 Scope（无租户）返回 ErrNotFound。
func (r *Repo) GetScopedTenant(ctx context.Context, sc scope.Scope) (domain.Tenant, error) {
	var t domain.Tenant
	if sc.IsPlatform() || sc.TenantID() == 0 {
		return t, domain.ErrNotFound
	}
	err := r.ex(ctx).Get(ctx, &t, db.Q(`SELECT `+tenantCols+tenantFrom+` WHERE t.id = ? AND t.deleted_at IS NULL`), sc.TenantID())
	return t, notFound(err)
}

// ListTenantsAcrossTenants 分页列出未删除的租户。orderBy 只取白名单（name | slug | created_at）。
func (r *Repo) ListTenantsAcrossTenants(ctx context.Context, sc scope.Scope, orderBy string, desc bool, limit, offset int) ([]domain.Tenant, int64, error) {
	if err := platformOnly(sc); err != nil {
		return nil, 0, err
	}
	var total int64
	if err := r.ex(ctx).Get(ctx, &total, db.Q(`SELECT COUNT(*) FROM tenants WHERE deleted_at IS NULL`)); err != nil {
		return nil, 0, err
	}
	var q db.Query
	switch {
	case orderBy == "name" && desc:
		q = db.Q(listTenantsSQL + ` ORDER BY t.name DESC, t.id DESC LIMIT ? OFFSET ?`)
	case orderBy == "name":
		q = db.Q(listTenantsSQL + ` ORDER BY t.name ASC, t.id ASC LIMIT ? OFFSET ?`)
	case orderBy == "slug" && desc:
		q = db.Q(listTenantsSQL + ` ORDER BY t.slug DESC, t.id DESC LIMIT ? OFFSET ?`)
	case orderBy == "slug":
		q = db.Q(listTenantsSQL + ` ORDER BY t.slug ASC, t.id ASC LIMIT ? OFFSET ?`)
	case desc:
		q = db.Q(listTenantsSQL + ` ORDER BY t.created_at DESC, t.id DESC LIMIT ? OFFSET ?`)
	default:
		q = db.Q(listTenantsSQL + ` ORDER BY t.created_at ASC, t.id ASC LIMIT ? OFFSET ?`)
	}
	var out []domain.Tenant
	err := r.ex(ctx).Select(ctx, &out, q, limit, offset)
	return out, total, err
}

const listTenantsSQL = `SELECT ` + tenantCols + tenantFrom + ` WHERE t.deleted_at IS NULL`

// UpdateTenantAcrossTenants 更新目标租户的可改字段（nil 不改）；返回是否有行被匹配。
func (r *Repo) UpdateTenantAcrossTenants(ctx context.Context, sc scope.Scope, tenantID uint64, u domain.TenantUpdate) (bool, error) {
	if err := platformOnly(sc); err != nil {
		return false, err
	}
	var settings any
	if u.SettingsJSON != nil {
		settings = string(u.SettingsJSON)
	}
	var contact any
	if u.ContactEmail != nil && *u.ContactEmail != "" {
		contact = *u.ContactEmail
	}
	res, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE tenants SET
		name = COALESCE(?, name),
		contact_email = IF(?, ?, contact_email),
		locale = COALESCE(?, locale),
		settings_json = IF(?, CAST(? AS JSON), settings_json)
		WHERE id = ? AND deleted_at IS NULL`),
		u.Name, u.ContactEmail != nil, contact, u.Locale, u.SettingsJSON != nil, settings, tenantID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---- 成员 ----

// ListMembers 分页列出目标租户的成员（含租户内角色 code 的 JSON 数组）。orderBy 只取白名单（email | joined_at）。
func (r *Repo) ListMembers(ctx context.Context, sc scope.Scope, tenantID uint64, orderBy string, desc bool, limit, offset int) ([]domain.Member, int64, error) {
	if err := platformOnly(sc); err != nil {
		return nil, 0, err
	}
	var total int64
	if err := r.ex(ctx).Get(ctx, &total, db.Q(`SELECT COUNT(*) FROM tenant_members m JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL
		WHERE m.tenant_id = ? AND m.status <> 'removed'`), tenantID); err != nil {
		return nil, 0, err
	}
	var q db.Query
	switch {
	case orderBy == "email" && desc:
		q = db.Q(listMembersSQL + ` ORDER BY u.email DESC, m.id DESC LIMIT ? OFFSET ?`)
	case orderBy == "email":
		q = db.Q(listMembersSQL + ` ORDER BY u.email ASC, m.id ASC LIMIT ? OFFSET ?`)
	case desc:
		q = db.Q(listMembersSQL + ` ORDER BY m.joined_at DESC, m.id DESC LIMIT ? OFFSET ?`)
	default:
		q = db.Q(listMembersSQL + ` ORDER BY m.joined_at ASC, m.id ASC LIMIT ? OFFSET ?`)
	}
	var out []domain.Member
	err := r.ex(ctx).Select(ctx, &out, q, tenantID, limit, offset)
	return out, total, err
}

const listMembersSQL = memberSelect + ` WHERE m.tenant_id = ? AND m.status <> 'removed'`

const memberSelect = `SELECT u.id AS user_id, u.uid AS user_uid, u.email, u.display_name, u.user_type, u.locale, u.totp_enabled, u.must_change_password,
	m.status, m.is_owner, m.joined_at,
	(SELECT JSON_ARRAYAGG(r.code) FROM user_roles ur JOIN roles r ON r.id = ur.role_id WHERE ur.user_id = m.user_id AND ur.tenant_id = m.tenant_id) AS roles_json
	FROM tenant_members m JOIN users u ON u.id = m.user_id AND u.deleted_at IS NULL`

// GetMember 取目标租户内某用户的成员视图（含角色）。不存在返回 ErrNotFound。
func (r *Repo) GetMember(ctx context.Context, sc scope.Scope, tenantID, userID uint64) (domain.Member, error) {
	var m domain.Member
	if err := platformOnly(sc); err != nil {
		return m, err
	}
	err := r.ex(ctx).Get(ctx, &m, db.Q(memberSelect+` WHERE m.tenant_id = ? AND m.user_id = ?`), tenantID, userID)
	return m, notFound(err)
}

// FindUserForMembership 按（已小写的）邮箱取未删除的用户。
func (r *Repo) FindUserForMembership(ctx context.Context, sc scope.Scope, email string) (domain.UserRef, error) {
	var u domain.UserRef
	if err := platformOnly(sc); err != nil {
		return u, err
	}
	err := r.ex(ctx).Get(ctx, &u, db.Q(`SELECT id, uid, user_type FROM users WHERE active_email = ? AND status <> 'disabled'`), email)
	return u, notFound(err)
}

// LockMember 在事务内锁定并取成员关系行；不存在返回 ErrNotFound。
func (r *Repo) LockMember(ctx context.Context, sc scope.Scope, tenantID, userID uint64) (domain.MemberRow, error) {
	var m domain.MemberRow
	if err := platformOnly(sc); err != nil {
		return m, err
	}
	err := r.ex(ctx).Get(ctx, &m, db.Q(`SELECT id, status FROM tenant_members WHERE tenant_id = ? AND user_id = ? FOR UPDATE`), tenantID, userID)
	return m, notFound(err)
}

// InsertMember 新增成员关系。唯一冲突由调用方经 db.IsDuplicate 判断。
func (r *Repo) InsertMember(ctx context.Context, sc scope.Scope, tenantID, userID, invitedBy uint64, now time.Time) error {
	if err := platformOnly(sc); err != nil {
		return err
	}
	_, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO tenant_members (tenant_id, user_id, status, is_owner, invited_by, joined_at) VALUES (?, ?, 'active', 0, ?, ?)`),
		tenantID, userID, invitedBy, now)
	return err
}

// ReactivateMember 把 invited/removed 的成员关系置为 active。
func (r *Repo) ReactivateMember(ctx context.Context, sc scope.Scope, memberID uint64, invitedBy uint64, now time.Time) error {
	if err := platformOnly(sc); err != nil {
		return err
	}
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE tenant_members SET status = 'active', invited_by = ?, joined_at = ? WHERE id = ?`), invitedBy, now, memberID)
	return err
}

// TenantRoleID 按 code 取租户范围的模板角色 id（tenant_id 为 NULL 且 scope=tenant）；不存在返回 ErrNotFound。
func (r *Repo) TenantRoleID(ctx context.Context, sc scope.Scope, code string) (uint64, error) {
	if err := platformOnly(sc); err != nil {
		return 0, err
	}
	var id uint64
	err := r.ex(ctx).Get(ctx, &id, db.Q(`SELECT id FROM roles WHERE tenant_id IS NULL AND scope = 'tenant' AND code = ?`), code)
	return id, notFound(err)
}

// GrantTenantRole 在目标租户内给用户授予角色（已授予则不变）。
func (r *Repo) GrantTenantRole(ctx context.Context, sc scope.Scope, tenantID, userID, roleID, grantedBy uint64) error {
	if err := platformOnly(sc); err != nil {
		return err
	}
	_, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO user_roles (user_id, role_id, tenant_id, granted_by) VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE user_roles.id = user_roles.id`), userID, roleID, tenantID, grantedBy)
	return err
}

// BumpPermissionVersion 递增用户的权限版本（权限缓存键含版本号：授予/撤销立即生效，ADR-018 §8）。
func (r *Repo) BumpPermissionVersion(ctx context.Context, sc scope.Scope, userID uint64) error {
	if err := platformOnly(sc); err != nil {
		return err
	}
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET permission_version = permission_version + 1 WHERE id = ?`), userID)
	return err
}

// ---- 配额 ----

// ListQuotasAcrossTenants 列出目标租户已存储的配额行（按 quota_key 升序）。
func (r *Repo) ListQuotasAcrossTenants(ctx context.Context, sc scope.Scope, tenantID uint64) ([]domain.Quota, error) {
	if err := platformOnly(sc); err != nil {
		return nil, err
	}
	var out []domain.Quota
	err := r.ex(ctx).Select(ctx, &out, db.Q(`SELECT quota_key, limit_value, source FROM tenant_quotas WHERE tenant_id = ? ORDER BY quota_key ASC`), tenantID)
	return out, err
}

// UpsertQuotaAcrossTenants 写入目标租户的一项配额（limit 为 nil = 不限）；source 记为 manual。只存取，强制执行属 Story 6.9。
func (r *Repo) UpsertQuotaAcrossTenants(ctx context.Context, sc scope.Scope, tenantID uint64, key string, limit *int64) error {
	if err := platformOnly(sc); err != nil {
		return err
	}
	_, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO tenant_quotas (tenant_id, quota_key, limit_value, source) VALUES (?, ?, ?, 'manual')
		ON DUPLICATE KEY UPDATE limit_value = VALUES(limit_value), source = 'manual'`), tenantID, key, limit)
	return err
}
