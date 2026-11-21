package repository

import (
	"context"
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/db"
)

// RBAC 与平台用户管理的数据访问（Story 1.18）。所有 SQL 为编译期常量 + 占位符；写操作由 Service 用 WithTx 划定事务边界。
// 表访问豁免登记：users/permissions/role_permissions 为平台级表；roles/user_roles/sessions 在认证/授权（Scope 产生之前）被访问，以用户或角色自身标识为键。

// ErrUnknownPermission 表示权限串不在权限点字典（permissions 表）内。
var ErrUnknownPermission = errors.New("auth/repository: 权限串不在字典内")

// ---- 有效权限集（授权判定用）----

// PlatformPermissions 读取平台用户的有效权限集：平台范围（user_roles.tenant_id IS NULL）内平台角色所授予的全部权限。
// lingedge:identity-table=user_roles,roles
// lingedge:platform-table=role_permissions,permissions
func (r *Repo) PlatformPermissions(ctx context.Context, userID uint64) ([]string, error) {
	var codes []string
	err := r.ex(ctx).Select(ctx, &codes, db.Q(`SELECT DISTINCT p.code FROM user_roles ur
		JOIN roles ro ON ro.id = ur.role_id AND ro.scope = 'platform'
		JOIN role_permissions rp ON rp.role_id = ro.id JOIN permissions p ON p.id = rp.permission_id
		WHERE ur.user_id = ? AND ur.tenant_key = 0 ORDER BY p.code`), userID)
	return codes, err
}

// TenantPermissions 读取租户用户在某租户内的有效权限集：该租户内租户角色所授予的权限，且仅 scope 为 tenant/both 的权限（platform 权限永不进入租户主体）。
// lingedge:identity-table=user_roles,roles
// lingedge:platform-table=role_permissions,permissions
func (r *Repo) TenantPermissions(ctx context.Context, userID, tenantID uint64) ([]string, error) {
	var codes []string
	err := r.ex(ctx).Select(ctx, &codes, db.Q(`SELECT DISTINCT p.code FROM user_roles ur
		JOIN roles ro ON ro.id = ur.role_id AND ro.scope = 'tenant'
		JOIN role_permissions rp ON rp.role_id = ro.id JOIN permissions p ON p.id = rp.permission_id AND p.scope IN ('tenant', 'both')
		WHERE ur.user_id = ? AND ur.tenant_key = ? AND ur.tenant_key <> 0 ORDER BY p.code`), userID, tenantID)
	return codes, err
}

// UserPermissionVersion 读取用户当前的权限版本（权限缓存的对账用）。
// lingedge:platform-table=users
func (r *Repo) UserPermissionVersion(ctx context.Context, userID uint64) (uint64, error) {
	var v uint64
	err := r.ex(ctx).Get(ctx, &v, db.Q(`SELECT permission_version FROM users WHERE id = ? AND deleted_at IS NULL`), userID)
	return v, notFound(err)
}

// RecentlyChangedAccess 列出自 since 起 users 行被更新且 permission_version>0 的用户及其角色范围（轮询兜底：清理这些用户上一版本的缓存键）。
// lingedge:identity-table=user_roles
// lingedge:platform-table=users
func (r *Repo) RecentlyChangedAccess(ctx context.Context, since time.Time, limit int) ([]domain.AffectedAccess, error) {
	var rows []domain.AffectedAccess
	err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT u.uid, 0 AS tenant_key, u.permission_version FROM users u WHERE u.updated_at >= ? AND u.permission_version > 0
		UNION ALL
		SELECT u.uid, ur.tenant_key, u.permission_version FROM users u JOIN user_roles ur ON ur.user_id = u.id
		WHERE u.updated_at >= ? AND u.permission_version > 0 AND ur.tenant_key <> 0
		LIMIT ?`), since, since, limit)
	return rows, err
}

// ---- 平台用户 ----

const adminUserCols = `id, uid, user_type, email, display_name, status, locale, timezone, totp_enabled, must_change_password, last_login_at, last_login_ip, locked_until, permission_version, created_at, updated_at`

// ListPlatformUsers 分页列出未删除的平台用户；sort 取 created_at | email（desc 为降序），其余值回退 created_at。
// lingedge:platform-table=users
func (r *Repo) ListPlatformUsers(ctx context.Context, limit, offset int, sort string, desc bool) ([]domain.AdminUser, error) {
	var rows []domain.AdminUser
	q := db.Q(`SELECT ` + adminUserCols + ` FROM users WHERE user_type = 'platform' AND deleted_at IS NULL ORDER BY created_at, id LIMIT ? OFFSET ?`)
	switch {
	case sort == "email" && !desc:
		q = db.Q(`SELECT ` + adminUserCols + ` FROM users WHERE user_type = 'platform' AND deleted_at IS NULL ORDER BY email, id LIMIT ? OFFSET ?`)
	case sort == "email" && desc:
		q = db.Q(`SELECT ` + adminUserCols + ` FROM users WHERE user_type = 'platform' AND deleted_at IS NULL ORDER BY email DESC, id DESC LIMIT ? OFFSET ?`)
	case desc:
		q = db.Q(`SELECT ` + adminUserCols + ` FROM users WHERE user_type = 'platform' AND deleted_at IS NULL ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`)
	}
	err := r.ex(ctx).Select(ctx, &rows, q, limit, offset)
	return rows, err
}

// CountPlatformUsers 统计未删除的平台用户。
// lingedge:platform-table=users
func (r *Repo) CountPlatformUsers(ctx context.Context) (int64, error) {
	var n int64
	err := r.ex(ctx).Get(ctx, &n, db.Q(`SELECT COUNT(*) FROM users WHERE user_type = 'platform' AND deleted_at IS NULL`))
	return n, err
}

// FindPlatformUserByUID 按 uid 取未删除的平台用户（租户用户、已删除用户与不存在一样返回 ErrNotFound）。
// lingedge:platform-table=users
func (r *Repo) FindPlatformUserByUID(ctx context.Context, uid string) (domain.AdminUser, error) {
	var u domain.AdminUser
	err := r.ex(ctx).Get(ctx, &u, db.Q(`SELECT `+adminUserCols+` FROM users WHERE uid = ? AND user_type = 'platform' AND deleted_at IS NULL`), uid)
	return u, notFound(err)
}

// FindPlatformUserByUIDForUpdate 同上并加行锁（须在事务内）。
// lingedge:platform-table=users
func (r *Repo) FindPlatformUserByUIDForUpdate(ctx context.Context, uid string) (domain.AdminUser, error) {
	var u domain.AdminUser
	err := r.ex(ctx).Get(ctx, &u, db.Q(`SELECT `+adminUserCols+` FROM users WHERE uid = ? AND user_type = 'platform' AND deleted_at IS NULL FOR UPDATE`), uid)
	return u, notFound(err)
}

// PlatformRoleCodesByUser 返回全部平台用户的平台角色 code（user_id → codes；平台用户数量有限，列表页一次取全）。
// lingedge:identity-table=user_roles,roles
func (r *Repo) PlatformRoleCodesByUser(ctx context.Context) (map[uint64][]string, error) {
	var rows []struct {
		UserID uint64 `db:"user_id"`
		Code   string `db:"code"`
	}
	if err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT ur.user_id, ro.code FROM user_roles ur JOIN roles ro ON ro.id = ur.role_id
		WHERE ur.tenant_key = 0 AND ro.scope = 'platform' ORDER BY ur.user_id, ro.code`)); err != nil {
		return nil, err
	}
	out := map[uint64][]string{}
	for _, x := range rows {
		out[x.UserID] = append(out[x.UserID], x.Code)
	}
	return out, nil
}

// UserPlatformRoleCodes 返回用户的平台角色 code（升序）。
// lingedge:identity-table=user_roles,roles
func (r *Repo) UserPlatformRoleCodes(ctx context.Context, userID uint64) ([]string, error) {
	var codes []string
	err := r.ex(ctx).Select(ctx, &codes, db.Q(`SELECT ro.code FROM user_roles ur JOIN roles ro ON ro.id = ur.role_id
		WHERE ur.user_id = ? AND ur.tenant_key = 0 AND ro.scope = 'platform' ORDER BY ro.code`), userID)
	return codes, err
}

// InsertPlatformUser 创建平台用户（缺省 must_change_password=1：创建后首次登录强制改密；NewUser.KeepPassword 为真时为 0，用于安装向导的本人设定口令）；邮箱冲突返回 db.IsDuplicate 为真的错误。
// lingedge:platform-table=users
func (r *Repo) InsertPlatformUser(ctx context.Context, u domain.NewUser) (uint64, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO users (uid, created_at, user_type, email, display_name, password_hash, status, locale, must_change_password)
		VALUES (?, ?, 'platform', ?, ?, ?, 'active', ?, ?)`), u.UID, u.Now, u.Email, u.DisplayName, u.PasswordHash, u.Locale, !u.KeepPassword)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return uint64(id), err
}

// UpdateUserProfile 更新展示名/状态/语言/时区（nil 表示不改）。
// lingedge:platform-table=users
func (r *Repo) UpdateUserProfile(ctx context.Context, id uint64, displayName, status, locale, timezone *string) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET display_name = COALESCE(?, display_name), status = COALESCE(?, status),
		locale = COALESCE(?, locale), timezone = COALESCE(?, timezone) WHERE id = ? AND deleted_at IS NULL`), displayName, status, locale, timezone, id)
	return err
}

// SoftDeleteUser 软删除用户（释放邮箱的唯一约束）并置为停用。
// lingedge:platform-table=users
func (r *Repo) SoftDeleteUser(ctx context.Context, id uint64, now time.Time) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET deleted_at = ?, status = 'disabled' WHERE id = ? AND deleted_at IS NULL`), now, id)
	return err
}

// ResetPassword 写入新口令哈希并强制首次登录改密：清除失败计数与锁定。
// lingedge:platform-table=users
func (r *Repo) ResetPassword(ctx context.Context, id uint64, hash string, now time.Time) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET password_hash = ?, must_change_password = 1, password_changed_at = ?, failed_login_count = 0, locked_until = NULL WHERE id = ? AND deleted_at IS NULL`),
		hash, now, id)
	return err
}

// UnlockUser 清除失败计数与锁定；状态为 locked 的恢复为 active。
// lingedge:platform-table=users
func (r *Repo) UnlockUser(ctx context.Context, id uint64) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET failed_login_count = 0, locked_until = NULL, status = IF(status = 'locked', 'active', status) WHERE id = ? AND deleted_at IS NULL`), id)
	return err
}

// ---- 角色授予（用户 ↔ 平台角色）----

// UserPlatformRoleIDs 返回用户的平台角色 id（升序）。
// lingedge:identity-table=user_roles
func (r *Repo) UserPlatformRoleIDs(ctx context.Context, userID uint64) ([]uint64, error) {
	var ids []uint64
	err := r.ex(ctx).Select(ctx, &ids, db.Q(`SELECT role_id FROM user_roles WHERE user_id = ? AND tenant_key = 0 ORDER BY role_id`), userID)
	return ids, err
}

// ReplaceUserPlatformRoles 整体替换用户的平台角色授予（先删后插；调用方在事务内并已加锁）。
// lingedge:identity-table=user_roles
func (r *Repo) ReplaceUserPlatformRoles(ctx context.Context, userID uint64, roleIDs []uint64, grantedBy uint64) error {
	if _, err := r.ex(ctx).Exec(ctx, db.Q(`DELETE FROM user_roles WHERE user_id = ? AND tenant_key = 0`), userID); err != nil {
		return err
	}
	for _, id := range roleIDs {
		var by any
		if grantedBy != 0 {
			by = grantedBy
		}
		if _, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO user_roles (user_id, role_id, tenant_id, granted_by) VALUES (?, ?, NULL, ?)`), userID, id, by); err != nil {
			return err
		}
	}
	return nil
}

// BumpUserPermissionVersion 使该用户的权限版本 +1（权限缓存键随之换代）。
// lingedge:platform-table=users
func (r *Repo) BumpUserPermissionVersion(ctx context.Context, userID uint64) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET permission_version = permission_version + 1 WHERE id = ?`), userID)
	return err
}

// BumpRolePermissionVersion 使持有该角色的全部用户的权限版本 +1（每个用户只加一次）；返回被波及的（用户, 范围）与新版本。
// lingedge:identity-table=user_roles
// lingedge:platform-table=users
func (r *Repo) BumpRolePermissionVersion(ctx context.Context, roleID uint64) ([]domain.AffectedAccess, error) {
	if _, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users u JOIN user_roles ur ON ur.user_id = u.id SET u.permission_version = u.permission_version + 1 WHERE ur.role_id = ?`), roleID); err != nil {
		return nil, err
	}
	var rows []domain.AffectedAccess
	err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT u.uid, ur.tenant_key, u.permission_version FROM user_roles ur JOIN users u ON u.id = ur.user_id WHERE ur.role_id = ?`), roleID)
	return rows, err
}

// UserAccessScopes 返回用户当前持有角色的全部范围（含平台范围 0）及其权限版本（清缓存用）。
// lingedge:identity-table=user_roles
// lingedge:platform-table=users
func (r *Repo) UserAccessScopes(ctx context.Context, userID uint64) ([]domain.AffectedAccess, error) {
	var rows []domain.AffectedAccess
	err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT u.uid, 0 AS tenant_key, u.permission_version FROM users u WHERE u.id = ?
		UNION
		SELECT u.uid, ur.tenant_key, u.permission_version FROM user_roles ur JOIN users u ON u.id = ur.user_id WHERE ur.user_id = ? AND ur.tenant_key <> 0`), userID, userID)
	return rows, err
}

// ---- 超级管理员保护 ----

// LockSuperAdminRole 取得内置超级管理员角色（platform_admin）的行锁并返回其 id：所有可能让「有效超级管理员」减少的操作都先串行化于此锁
// （事务内；锁在事务结束时释放）。用单一行锁而不是锁一批用户行，是为了没有加锁顺序死锁、没有幻读缺口。
// lingedge:identity-table=roles
func (r *Repo) LockSuperAdminRole(ctx context.Context) (uint64, error) {
	var id uint64
	err := r.ex(ctx).Get(ctx, &id, db.Q(`SELECT id FROM roles WHERE scope = 'platform' AND tenant_id IS NULL AND code = 'platform_admin' FOR UPDATE`))
	return id, notFound(err)
}

// ActiveSuperAdminIDs 返回当前有效（未删除、状态 active）的超级管理员用户 id（事务内、加锁读取最新提交的数据）。
// lingedge:identity-table=user_roles
// lingedge:platform-table=users
func (r *Repo) ActiveSuperAdminIDs(ctx context.Context, roleID uint64) ([]uint64, error) {
	var ids []uint64
	err := r.ex(ctx).Select(ctx, &ids, db.Q(`SELECT u.id FROM user_roles ur JOIN users u ON u.id = ur.user_id
		WHERE ur.role_id = ? AND ur.tenant_key = 0 AND u.status = 'active' AND u.deleted_at IS NULL AND u.user_type = 'platform' ORDER BY u.id FOR UPDATE`), roleID)
	return ids, err
}

// ---- 角色 ----

const roleCols = `id, uid, scope, code, name, description, is_builtin, created_at`

// ListRoles 分页列出模板角色（tenant_id IS NULL）。
// lingedge:identity-table=roles
func (r *Repo) ListRoles(ctx context.Context, limit, offset int) ([]domain.Role, error) {
	var rows []domain.Role
	err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT `+roleCols+` FROM roles WHERE tenant_id IS NULL ORDER BY is_builtin DESC, scope, code LIMIT ? OFFSET ?`), limit, offset)
	return rows, err
}

// CountRoles 统计模板角色。
// lingedge:identity-table=roles
func (r *Repo) CountRoles(ctx context.Context) (int64, error) {
	var n int64
	err := r.ex(ctx).Get(ctx, &n, db.Q(`SELECT COUNT(*) FROM roles WHERE tenant_id IS NULL`))
	return n, err
}

// FindRoleByUID 按 uid 取模板角色。
// lingedge:identity-table=roles
func (r *Repo) FindRoleByUID(ctx context.Context, uid string) (domain.Role, error) {
	var ro domain.Role
	err := r.ex(ctx).Get(ctx, &ro, db.Q(`SELECT `+roleCols+` FROM roles WHERE uid = ? AND tenant_id IS NULL`), uid)
	return ro, notFound(err)
}

// FindRoleByUIDForUpdate 同上并加行锁（须在事务内）。
// lingedge:identity-table=roles
func (r *Repo) FindRoleByUIDForUpdate(ctx context.Context, uid string) (domain.Role, error) {
	var ro domain.Role
	err := r.ex(ctx).Get(ctx, &ro, db.Q(`SELECT `+roleCols+` FROM roles WHERE uid = ? AND tenant_id IS NULL FOR UPDATE`), uid)
	return ro, notFound(err)
}

// FindPlatformRoleByCode 按 code 取平台角色。
// lingedge:identity-table=roles
func (r *Repo) FindPlatformRoleByCode(ctx context.Context, code string) (domain.Role, error) {
	var ro domain.Role
	err := r.ex(ctx).Get(ctx, &ro, db.Q(`SELECT `+roleCols+` FROM roles WHERE scope = 'platform' AND tenant_id IS NULL AND code = ?`), code)
	return ro, notFound(err)
}

// InsertRole 创建模板角色（非内置；code 在同 scope 内唯一，冲突返回 db.IsDuplicate 为真的错误）。
// lingedge:identity-table=roles
func (r *Repo) InsertRole(ctx context.Context, uid, scope, code, name string, description *string) (uint64, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO roles (uid, tenant_id, scope, code, name, description, is_builtin) VALUES (?, NULL, ?, ?, ?, ?, 0)`), uid, scope, code, name, description)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return uint64(id), err
}

// UpdateRole 更新名称/描述（nil 表示不改）。
// lingedge:identity-table=roles
func (r *Repo) UpdateRole(ctx context.Context, id uint64, name, description *string) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE roles SET name = COALESCE(?, name), description = COALESCE(?, description) WHERE id = ?`), name, description, id)
	return err
}

// DeleteRole 删除角色（role_permissions/user_roles 经外键级联删除；调用方须先确认无人持有）。
// lingedge:identity-table=roles
func (r *Repo) DeleteRole(ctx context.Context, id uint64) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`DELETE FROM roles WHERE id = ?`), id)
	return err
}

// CountRoleHolders 统计持有该角色的授予条数。
// lingedge:identity-table=user_roles
func (r *Repo) CountRoleHolders(ctx context.Context, roleID uint64) (int64, error) {
	var n int64
	err := r.ex(ctx).Get(ctx, &n, db.Q(`SELECT COUNT(*) FROM user_roles WHERE role_id = ?`), roleID)
	return n, err
}

// RolePermissionCodes 返回角色的权限串（升序）。
// lingedge:platform-table=role_permissions,permissions
func (r *Repo) RolePermissionCodes(ctx context.Context, roleID uint64) ([]string, error) {
	var codes []string
	err := r.ex(ctx).Select(ctx, &codes, db.Q(`SELECT p.code FROM role_permissions rp JOIN permissions p ON p.id = rp.permission_id WHERE rp.role_id = ? ORDER BY p.code`), roleID)
	return codes, err
}

// AllRolePermissionCodes 返回全部模板角色的权限串（role_id → codes；列表页一次取全）。
// lingedge:identity-table=roles
// lingedge:platform-table=role_permissions,permissions
func (r *Repo) AllRolePermissionCodes(ctx context.Context) (map[uint64][]string, error) {
	var rows []struct {
		RoleID uint64 `db:"role_id"`
		Code   string `db:"code"`
	}
	if err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT rp.role_id, p.code FROM role_permissions rp JOIN roles ro ON ro.id = rp.role_id AND ro.tenant_id IS NULL
		JOIN permissions p ON p.id = rp.permission_id ORDER BY rp.role_id, p.code`)); err != nil {
		return nil, err
	}
	out := map[uint64][]string{}
	for _, x := range rows {
		out[x.RoleID] = append(out[x.RoleID], x.Code)
	}
	return out, nil
}

// ReplaceRolePermissions 整体替换角色的权限（先删后插；权限串不在字典内返回 ErrUnknownPermission）。调用方在事务内。
// lingedge:platform-table=role_permissions
func (r *Repo) ReplaceRolePermissions(ctx context.Context, roleID uint64, codes []string) error {
	if _, err := r.ex(ctx).Exec(ctx, db.Q(`DELETE FROM role_permissions WHERE role_id = ?`), roleID); err != nil {
		return err
	}
	for _, c := range codes {
		res, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO role_permissions (role_id, permission_id) SELECT ?, id FROM permissions WHERE code = ?`), roleID, c)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrUnknownPermission
		}
	}
	return nil
}

// ---- 权限点字典 ----

// ListPermissions 分页列出权限点字典。
// lingedge:platform-table=permissions
func (r *Repo) ListPermissions(ctx context.Context, limit, offset int) ([]domain.Permission, error) {
	var rows []domain.Permission
	err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT code, resource, action, scope, module, menu_key, description FROM permissions ORDER BY code LIMIT ? OFFSET ?`), limit, offset)
	return rows, err
}

// CountPermissions 统计权限点。
// lingedge:platform-table=permissions
func (r *Repo) CountPermissions(ctx context.Context) (int64, error) {
	var n int64
	err := r.ex(ctx).Get(ctx, &n, db.Q(`SELECT COUNT(*) FROM permissions`))
	return n, err
}

// AllPermissions 返回全部权限点（对账用）。
// lingedge:platform-table=permissions
func (r *Repo) AllPermissions(ctx context.Context) ([]domain.Permission, error) {
	var rows []domain.Permission
	err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT code, resource, action, scope, module, menu_key, description FROM permissions ORDER BY code`))
	return rows, err
}
