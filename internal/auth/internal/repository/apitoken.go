package repository

import (
	"context"
	"time"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/db"
)

// API Token 的数据访问（Story 1.19）。api_tokens 在认证钩子（Scope 产生之前）按 token_hash/id 访问，在管理 API 里以创建者自己的 user_id 为边界，
// 故登记为认证/身份表（scripts/gates/data/identity-tables.txt）；所有 SQL 为编译期常量 + 占位符；写操作由 Service 用 WithTx 划定事务边界。

const tokenCols = `t.id, t.uid, t.created_at, t.tenant_id, tn.uid AS tenant_uid, t.user_id, t.name, t.token_prefix, t.token_hash, t.scopes_json,
	t.allowed_cidrs_json, t.expires_at, t.last_used_at, t.last_used_ip, t.revoked_at, t.rotated_from_id, rf.uid AS rotated_from_uid`

// InsertAPIToken 写入令牌；token_prefix / token_hash 冲突返回 db.IsDuplicate 为真的错误（调用方重试换前缀）。返回新行 id。
// lingedge:identity-table=api_tokens
func (r *Repo) InsertAPIToken(ctx context.Context, t domain.NewAPIToken) (uint64, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO api_tokens (uid, created_at, tenant_id, user_id, name, token_prefix, token_hash, scopes_json,
		allowed_cidrs_json, expires_at, rotated_from_id) VALUES (?, ?, ?, ?, ?, ?, ?, CAST(? AS JSON), CAST(? AS JSON), ?, ?)`),
		t.UID, t.Now, t.TenantID, t.UserID, t.Name, t.Prefix, t.Hash, string(t.ScopesJSON), nilJSON(t.CIDRsJSON), t.ExpiresAt, t.RotatedFromID)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return uint64(id), err
}

func nilJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// FindAPITokenIDByHash 按 SHA-256 取令牌 id（Redis 缓存的真相源）；不存在返回 ErrNotFound。
// lingedge:identity-table=api_tokens
func (r *Repo) FindAPITokenIDByHash(ctx context.Context, hash string) (uint64, error) {
	var id uint64
	err := r.ex(ctx).Get(ctx, &id, db.Q(`SELECT id FROM api_tokens WHERE token_hash = ?`), hash)
	return id, notFound(err)
}

// FindTokenAuthView 按令牌 id 现读令牌 + 创建者 + 租户（含成员关系）；不判断有效性，由 Service 判定。不存在返回 ErrNotFound。
// lingedge:identity-table=api_tokens,tenant_members
// lingedge:platform-table=users,tenants
func (r *Repo) FindTokenAuthView(ctx context.Context, id uint64) (domain.TokenAuthView, error) {
	var v domain.TokenAuthView
	err := r.ex(ctx).Get(ctx, &v, db.Q(`SELECT t.id AS token_id, t.uid AS token_uid, t.token_hash, t.token_prefix, t.scopes_json, t.allowed_cidrs_json,
		t.expires_at, t.revoked_at, t.tenant_id,
		u.id AS user_id, u.uid AS user_uid, u.user_type, u.email, u.display_name, u.status AS user_status, u.locale, u.totp_enabled, u.must_change_password,
		u.permission_version, u.deleted_at AS user_deleted_at,
		tn.uid AS tenant_uid, tn.name AS tenant_name, tn.slug AS tenant_slug, tn.is_default AS tenant_is_default, tn.status AS tenant_status,
		tn.deleted_at AS tenant_deleted_at, (m.id IS NOT NULL) AS member_ok
		FROM api_tokens t
		JOIN users u ON u.id = t.user_id
		LEFT JOIN tenants tn ON tn.id = t.tenant_id
		LEFT JOIN tenant_members m ON m.tenant_id = t.tenant_id AND m.user_id = t.user_id AND m.status = 'active'
		WHERE t.id = ?`), id)
	return v, notFound(err)
}

// TouchAPITokenUsed 更新最近使用时间与来源 IP（调用方已节流并异步执行）。
// lingedge:identity-table=api_tokens
func (r *Repo) TouchAPITokenUsed(ctx context.Context, id uint64, at time.Time, ip []byte) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE api_tokens SET last_used_at = ?, last_used_ip = ? WHERE id = ?`), at, nilIfEmpty(ip), id)
	return err
}

// FindOwnedAPIToken 取「属于该创建者且范围一致」的令牌（平台令牌 tenant_id IS NULL；租户令牌 tenant_id = 主体租户）。
// 他人的令牌与不存在的令牌一样返回 ErrNotFound（不泄漏存在性）。
// lingedge:identity-table=api_tokens
// lingedge:platform-table=tenants
func (r *Repo) FindOwnedAPIToken(ctx context.Context, userID uint64, tenantID *uint64, tokenUID string) (domain.APIToken, error) {
	var t domain.APIToken
	err := r.ex(ctx).Get(ctx, &t, db.Q(`SELECT `+tokenCols+` FROM api_tokens t LEFT JOIN tenants tn ON tn.id = t.tenant_id
		LEFT JOIN api_tokens rf ON rf.id = t.rotated_from_id
		WHERE t.uid = ? AND t.user_id = ? AND (t.tenant_id <=> ?)`), tokenUID, userID, tenantID)
	return t, notFound(err)
}

// LockOwnedAPIToken 同 FindOwnedAPIToken，但在事务内加行锁（轮换/吊销的读改写；须在事务中调用）。
// lingedge:identity-table=api_tokens
func (r *Repo) LockOwnedAPIToken(ctx context.Context, userID uint64, tenantID *uint64, tokenUID string) (domain.APIToken, error) {
	var t domain.APIToken
	err := r.ex(ctx).Get(ctx, &t, db.Q(`SELECT id, uid, created_at, tenant_id, NULL AS tenant_uid, user_id, name, token_prefix, token_hash, scopes_json,
		allowed_cidrs_json, expires_at, last_used_at, last_used_ip, revoked_at, rotated_from_id, NULL AS rotated_from_uid
		FROM api_tokens WHERE uid = ? AND user_id = ? AND (tenant_id <=> ?) FOR UPDATE`), tokenUID, userID, tenantID)
	return t, notFound(err)
}

// RevokeAPIToken 吊销令牌（已吊销则不变）；返回是否有行被吊销。
// lingedge:identity-table=api_tokens
func (r *Repo) RevokeAPIToken(ctx context.Context, id uint64, now time.Time) (bool, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`), now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountOwnedAPITokens 统计创建者自己名下（同范围）的令牌数。
// lingedge:identity-table=api_tokens
func (r *Repo) CountOwnedAPITokens(ctx context.Context, userID uint64, tenantID *uint64) (int64, error) {
	var n int64
	err := r.ex(ctx).Get(ctx, &n, db.Q(`SELECT COUNT(*) FROM api_tokens WHERE user_id = ? AND (tenant_id <=> ?)`), userID, tenantID)
	return n, err
}

// ListOwnedAPITokens 分页列出创建者自己名下（同范围）的令牌（含已吊销/已过期的）。orderBy 只取白名单（name | created_at）。
// lingedge:identity-table=api_tokens
// lingedge:platform-table=tenants
func (r *Repo) ListOwnedAPITokens(ctx context.Context, userID uint64, tenantID *uint64, orderBy string, desc bool, limit, offset int) ([]domain.APIToken, error) {
	var out []domain.APIToken
	var q db.Query
	switch {
	case orderBy == "name" && desc:
		q = db.Q(listTokensSQL + ` ORDER BY t.name DESC, t.id DESC LIMIT ? OFFSET ?`)
	case orderBy == "name":
		q = db.Q(listTokensSQL + ` ORDER BY t.name ASC, t.id ASC LIMIT ? OFFSET ?`)
	case desc:
		q = db.Q(listTokensSQL + ` ORDER BY t.created_at DESC, t.id DESC LIMIT ? OFFSET ?`)
	default:
		q = db.Q(listTokensSQL + ` ORDER BY t.created_at ASC, t.id ASC LIMIT ? OFFSET ?`)
	}
	err := r.ex(ctx).Select(ctx, &out, q, userID, tenantID, limit, offset)
	return out, err
}

const listTokensSQL = `SELECT ` + tokenCols + ` FROM api_tokens t LEFT JOIN tenants tn ON tn.id = t.tenant_id
	LEFT JOIN api_tokens rf ON rf.id = t.rotated_from_id WHERE t.user_id = ? AND (t.tenant_id <=> ?)`
