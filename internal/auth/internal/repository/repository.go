package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/db"
)

// ErrWrongRole：只接受 app 连接池。
var ErrWrongRole = errors.New("auth/repository: 只接受 app 连接池")

// Repo 是认证模块的 Repository。
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

// ---- users ----

// FindUserByEmail 按（已小写的）邮箱取未删除的用户。
// lingedge:platform-table=users
func (r *Repo) FindUserByEmail(ctx context.Context, email string) (domain.User, error) {
	var u domain.User
	err := r.ex(ctx).Get(ctx, &u, db.Q(`SELECT id, uid, user_type, email, display_name, password_hash, status, locale, totp_enabled,
		must_change_password, failed_login_count, locked_until, last_login_at, last_login_ip
		FROM users WHERE active_email = ?`), email)
	return u, notFound(err)
}

// FindUserByID 按 id 取未删除的用户。
// lingedge:platform-table=users
func (r *Repo) FindUserByID(ctx context.Context, id uint64) (domain.User, error) {
	var u domain.User
	err := r.ex(ctx).Get(ctx, &u, db.Q(`SELECT id, uid, user_type, email, display_name, password_hash, status, locale, totp_enabled,
		must_change_password, failed_login_count, locked_until, last_login_at, last_login_ip
		FROM users WHERE id = ? AND deleted_at IS NULL`), id)
	return u, notFound(err)
}

// FindUserByIDForUpdate 在事务内锁定并取用户行（登录失败计数的读改写；须在事务中调用）。
// lingedge:platform-table=users
func (r *Repo) FindUserByIDForUpdate(ctx context.Context, id uint64) (domain.User, error) {
	var u domain.User
	err := r.ex(ctx).Get(ctx, &u, db.Q(`SELECT id, uid, user_type, email, display_name, password_hash, status, locale, totp_enabled,
		must_change_password, failed_login_count, locked_until, last_login_at, last_login_ip
		FROM users WHERE id = ? AND deleted_at IS NULL FOR UPDATE`), id)
	return u, notFound(err)
}

// SetFailureState 写回失败计数与锁定截止（降级路径的真相源，ADR-018 §6）。
// lingedge:platform-table=users
func (r *Repo) SetFailureState(ctx context.Context, userID uint64, count uint32, lockedUntil *time.Time) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET failed_login_count = ?, locked_until = ? WHERE id = ?`), count, lockedUntil, userID)
	return err
}

// RecordLoginSuccess 记录登录成功：清零失败计数与锁定、写最近登录时间与 IP；rehash 非空时同时升级口令哈希。
// lingedge:platform-table=users
func (r *Repo) RecordLoginSuccess(ctx context.Context, userID uint64, ip []byte, now time.Time, rehash string) error {
	if rehash != "" {
		_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET failed_login_count = 0, locked_until = NULL, last_login_at = ?, last_login_ip = ?, password_hash = ? WHERE id = ?`),
			now, ip, rehash, userID)
		return err
	}
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET failed_login_count = 0, locked_until = NULL, last_login_at = ?, last_login_ip = ? WHERE id = ?`), now, ip, userID)
	return err
}

// UpdatePassword 写入新口令哈希：清除强制改密标记与失败计数。
// lingedge:platform-table=users
func (r *Repo) UpdatePassword(ctx context.Context, userID uint64, hash string, now time.Time) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET password_hash = ?, must_change_password = 0, password_changed_at = ?, failed_login_count = 0, locked_until = NULL WHERE id = ?`),
		hash, now, userID)
	return err
}

// ---- tenants / tenant_members ----

// DefaultMembership 取用户的默认租户（有效成员关系 + 有效租户；优先默认租户，其次最早加入）；无则 ErrNotFound。
// lingedge:identity-table=tenant_members
// lingedge:platform-table=tenants
func (r *Repo) DefaultMembership(ctx context.Context, userID uint64) (domain.Tenant, error) {
	var t domain.Tenant
	err := r.ex(ctx).Get(ctx, &t, db.Q(`SELECT t.id, t.uid, t.name, t.slug, t.is_default
		FROM tenant_members m JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = ? AND m.status = 'active' AND t.status = 'active' AND t.deleted_at IS NULL
		ORDER BY t.is_default DESC, m.id ASC LIMIT 1`), userID)
	return t, notFound(err)
}

// ---- sessions ----

// CreateSession 写入新会话（只含令牌与 CSRF 的 SHA-256）。
// lingedge:identity-table=sessions
func (r *Repo) CreateSession(ctx context.Context, s domain.NewSession) error {
	var ua *string
	if s.UserAgent != "" {
		v := s.UserAgent
		ua = &v
	}
	_, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO sessions (uid, created_at, token_hash, csrf_hash, user_id, realm, tenant_id, ip, user_agent,
		mfa_verified_at, last_seen_at, expires_at, absolute_expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		s.UID, s.Now, s.TokenHash, s.CSRFHash, s.UserID, string(s.Realm), s.TenantID, nilIfEmpty(s.IP), ua, s.MFAVerifiedAt, s.Now, s.ExpiresAt, s.AbsoluteExpiresAt)
	return err
}

// FindSessionByTokenHash 取会话及其用户/租户（不判断有效性，由 Service 判定）；不存在返回 ErrNotFound。
// lingedge:identity-table=sessions,tenant_members
// lingedge:platform-table=users,tenants
func (r *Repo) FindSessionByTokenHash(ctx context.Context, tokenHash string) (domain.SessionView, error) {
	var v domain.SessionView
	err := r.ex(ctx).Get(ctx, &v, db.Q(`SELECT s.id, COALESCE(s.uid, '') AS uid, s.csrf_hash, s.user_id, s.realm, s.tenant_id, s.created_at, s.last_seen_at, s.expires_at,
		s.absolute_expires_at, s.revoked_at, s.mfa_verified_at, s.reauth_at,
		u.uid AS user_uid, u.user_type, u.email, u.display_name, u.status AS user_status, u.locale, u.totp_enabled, u.must_change_password,
		u.deleted_at AS user_deleted_at,
		t.uid AS tenant_uid, t.name AS tenant_name, t.slug AS tenant_slug, t.is_default AS tenant_is_default, t.status AS tenant_status, t.deleted_at AS tenant_deleted_at,
		(m.id IS NOT NULL) AS member_ok
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		LEFT JOIN tenants t ON t.id = s.tenant_id
		LEFT JOIN tenant_members m ON m.tenant_id = s.tenant_id AND m.user_id = s.user_id AND m.status = 'active'
		WHERE s.token_hash = ?`), tokenHash)
	return v, notFound(err)
}

// TouchSession 滑动续期：更新最近活动与滑动过期时间（只对未吊销会话）。
// lingedge:identity-table=sessions
func (r *Repo) TouchSession(ctx context.Context, id uint64, lastSeen, expiresAt time.Time) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ? AND revoked_at IS NULL`), lastSeen, expiresAt, id)
	return err
}

// RevokeSession 吊销一个会话（已吊销则不变）；返回是否有行被吊销。
// lingedge:identity-table=sessions
func (r *Repo) RevokeSession(ctx context.Context, id uint64, reason string, now time.Time) (bool, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE sessions SET revoked_at = ?, revoke_reason = ? WHERE id = ? AND revoked_at IS NULL`), now, reason, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RevokeUserSessionByUID 吊销「属于该用户」的指定 uid 会话（他人的会话与不存在的会话一样返回 ErrNotFound，不泄漏存在性）。
// 返回被吊销会话的 id；已吊销/已过期的会话同样视为不存在。
// lingedge:identity-table=sessions
func (r *Repo) RevokeUserSessionByUID(ctx context.Context, userID uint64, sessionUID, reason string, now time.Time) (uint64, error) {
	var id uint64
	if err := r.ex(ctx).Get(ctx, &id, db.Q(`SELECT id FROM sessions WHERE uid = ? AND user_id = ? AND revoked_at IS NULL AND expires_at > ? AND absolute_expires_at > ?`),
		sessionUID, userID, now, now); err != nil {
		return 0, notFound(err)
	}
	ok, err := r.RevokeSession(ctx, id, reason, now)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, domain.ErrNotFound
	}
	return id, nil
}

// RevokeUserSessions 吊销用户的全部未吊销会话；返回数量。
// lingedge:identity-table=sessions
func (r *Repo) RevokeUserSessions(ctx context.Context, userID uint64, reason string, now time.Time) (int64, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE sessions SET revoked_at = ?, revoke_reason = ? WHERE user_id = ? AND revoked_at IS NULL`), now, reason, userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountActiveSessions 统计用户当前有效（未吊销、未过期）的会话数。
// lingedge:identity-table=sessions
func (r *Repo) CountActiveSessions(ctx context.Context, userID uint64, now time.Time) (int64, error) {
	var n int64
	err := r.ex(ctx).Get(ctx, &n, db.Q(`SELECT COUNT(*) FROM sessions WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ? AND absolute_expires_at > ?`), userID, now, now)
	return n, err
}

// ListActiveSessions 按创建时间倒序列出用户当前有效的会话。
// lingedge:identity-table=sessions
func (r *Repo) ListActiveSessions(ctx context.Context, userID uint64, now time.Time, limit, offset int) ([]domain.SessionRow, error) {
	var rows []domain.SessionRow
	err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT id, uid, realm, ip, user_agent, created_at, last_seen_at, expires_at FROM sessions
		WHERE user_id = ? AND revoked_at IS NULL AND expires_at > ? AND absolute_expires_at > ? ORDER BY id DESC LIMIT ? OFFSET ?`), userID, now, now, limit, offset)
	return rows, err
}

// ---- login_logs ----

// InsertLoginLog 写一条登录事件。
// lingedge:identity-table=login_logs
func (r *Repo) InsertLoginLog(ctx context.Context, l domain.LoginLog) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO login_logs (uid, created_at, user_id, tenant_id, realm, identifier, event, ip, user_agent, request_id, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		l.UID, l.CreatedAt, l.UserID, l.TenantID, l.Realm, l.Identifier, l.Event, nilIfEmpty(l.IP), l.UserAgent, l.RequestID, l.Reason)
	return err
}

// ListUserLoginLogs 列出「该用户本人」的登录事件（id 倒序；beforeID>0 时只取更早的）。
// lingedge:identity-table=login_logs
func (r *Repo) ListUserLoginLogs(ctx context.Context, userID, beforeID uint64, limit int) ([]domain.LoginLog, error) {
	var rows []domain.LoginLog
	err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT id, uid, created_at, user_id, tenant_id, realm, identifier, event, ip, user_agent, request_id, reason
		FROM login_logs WHERE user_id = ? AND (? = 0 OR id < ?) ORDER BY id DESC LIMIT ?`), userID, beforeID, beforeID, limit)
	return rows, err
}

// ---- 角色与权限（只读；授权判定属 Story 1.18）----

// UserAccess 读取用户在某一范围内的角色与权限集：tenantID 为 0 表示平台角色（user_roles.tenant_id IS NULL），否则为该租户内的角色。
// lingedge:identity-table=user_roles,roles
// lingedge:platform-table=role_permissions,permissions
func (r *Repo) UserAccess(ctx context.Context, userID, tenantID uint64) (domain.Access, error) {
	acc := domain.Access{Roles: []string{}, Permissions: []string{}, MenuKeys: []string{}}
	var roles []string
	if err := r.ex(ctx).Select(ctx, &roles, db.Q(`SELECT DISTINCT ro.code FROM user_roles ur JOIN roles ro ON ro.id = ur.role_id
		WHERE ur.user_id = ? AND ur.tenant_key = ? ORDER BY ro.code`), userID, tenantID); err != nil {
		return acc, err
	}
	var perms []struct {
		Code    string  `db:"code"`
		MenuKey *string `db:"menu_key"`
	}
	if err := r.ex(ctx).Select(ctx, &perms, db.Q(`SELECT DISTINCT p.code, p.menu_key FROM user_roles ur
		JOIN role_permissions rp ON rp.role_id = ur.role_id JOIN permissions p ON p.id = rp.permission_id
		WHERE ur.user_id = ? AND ur.tenant_key = ? ORDER BY p.code`), userID, tenantID); err != nil {
		return acc, err
	}
	acc.Roles = append(acc.Roles, roles...)
	seen := map[string]bool{}
	for _, p := range perms {
		acc.Permissions = append(acc.Permissions, p.Code)
		if p.MenuKey != nil && !seen[*p.MenuKey] {
			seen[*p.MenuKey] = true
			acc.MenuKeys = append(acc.MenuKeys, *p.MenuKey)
		}
	}
	return acc, nil
}

// ---- system_settings ----

// ReadSettings 读取认证相关的系统设置（键 → JSON 值；不存在的键不出现）。
// lingedge:platform-table=system_settings
func (r *Repo) ReadSettings(ctx context.Context) (map[string]json.RawMessage, error) {
	var rows []struct {
		Key   string          `db:"setting_key"`
		Value json.RawMessage `db:"value_json"`
	}
	if err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT setting_key, value_json FROM system_settings
		WHERE setting_key IN ('auth.session_idle_ttl_s', 'auth.session_absolute_ttl_s', 'login.throttle_window_s', 'login.throttle_account_max', 'login.throttle_ip_max', 'system.run_mode',
		'auth.reauth_window_s', 'auth.mfa_window_s', 'login.totp_required_for_admin')`)); err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(rows))
	for _, row := range rows {
		if row.Value != nil {
			out[row.Key] = row.Value
		}
	}
	return out, nil
}

func nilIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// UpdatePasswordHash 只升级口令哈希（参数提升时的透明 rehash；不改任何其他列）。
// lingedge:platform-table=users
func (r *Repo) UpdatePasswordHash(ctx context.Context, userID uint64, hash string) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET password_hash = ? WHERE id = ?`), hash, userID)
	return err
}
