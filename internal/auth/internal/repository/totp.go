package repository

import (
	"context"
	"time"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/db"
)

// ---- users 的 TOTP 状态 ----

// GetTOTPState 读取用户的 TOTP 状态（含密钥密文）；forUpdate 为真时锁行（须在事务内）。
// lingedge:platform-table=users
func (r *Repo) GetTOTPState(ctx context.Context, userID uint64, forUpdate bool) (domain.TOTPState, error) {
	var st domain.TOTPState
	var err error
	if forUpdate {
		err = r.ex(ctx).Get(ctx, &st, db.Q(`SELECT uid, totp_enabled AS enabled, totp_secret_enc AS secret_enc, totp_last_step AS last_step
			FROM users WHERE id = ? AND deleted_at IS NULL FOR UPDATE`), userID)
	} else {
		err = r.ex(ctx).Get(ctx, &st, db.Q(`SELECT uid, totp_enabled AS enabled, totp_secret_enc AS secret_enc, totp_last_step AS last_step
			FROM users WHERE id = ? AND deleted_at IS NULL`), userID)
	}
	return st, notFound(err)
}

// SetPendingTOTP 保存「待确认」的 TOTP 密钥密文（totp_enabled 仍为 0）；已启用时不改动并返回 false。
// lingedge:platform-table=users
func (r *Repo) SetPendingTOTP(ctx context.Context, userID uint64, enc string) (bool, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET totp_secret_enc = ?, totp_last_step = NULL WHERE id = ? AND totp_enabled = 0`), enc, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// EnableTOTP 把待确认的密钥转为已启用，并记下确认时用掉的 time-step；非「待确认」状态返回 false。
// lingedge:platform-table=users
func (r *Repo) EnableTOTP(ctx context.Context, userID uint64, step uint64) (bool, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET totp_enabled = 1, totp_last_step = ? WHERE id = ? AND totp_enabled = 0 AND totp_secret_enc IS NOT NULL`), step, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DisableTOTP 停用并清除密钥与已用 time-step。
// lingedge:platform-table=users
func (r *Repo) DisableTOTP(ctx context.Context, userID uint64) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET totp_enabled = 0, totp_secret_enc = NULL, totp_last_step = NULL WHERE id = ?`), userID)
	return err
}

// AdvanceTOTPStep 原子地记录已用 time-step：仅当它严格大于已记录值时成功；返回 false 即重放（同一或更早的时间窗内的验证码已用过）。
// lingedge:platform-table=users
func (r *Repo) AdvanceTOTPStep(ctx context.Context, userID uint64, step uint64) (bool, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_enabled = 1 AND (totp_last_step IS NULL OR totp_last_step < ?)`), step, userID, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---- 恢复码 ----

// ReplaceRecoveryCodes 删除用户的全部旧恢复码并写入新的一批（只含哈希；须在事务内）。
// lingedge:platform-table=user_recovery_codes
func (r *Repo) ReplaceRecoveryCodes(ctx context.Context, userID uint64, hashes []string) error {
	if _, err := r.ex(ctx).Exec(ctx, db.Q(`DELETE FROM user_recovery_codes WHERE user_id = ?`), userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO user_recovery_codes (user_id, code_hash) VALUES (?, ?)`), userID, h); err != nil {
			return err
		}
	}
	return nil
}

// DeleteRecoveryCodes 删除用户的全部恢复码。
// lingedge:platform-table=user_recovery_codes
func (r *Repo) DeleteRecoveryCodes(ctx context.Context, userID uint64) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`DELETE FROM user_recovery_codes WHERE user_id = ?`), userID)
	return err
}

// UseRecoveryCode 原子地把一个未使用的恢复码置为已用；返回是否成功（不存在/已用 = false，单次有效）。
// lingedge:platform-table=user_recovery_codes
func (r *Repo) UseRecoveryCode(ctx context.Context, userID uint64, hash string, now time.Time) (bool, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE user_recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`), now, userID, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountUnusedRecoveryCodes 统计未使用的恢复码数量。
// lingedge:platform-table=user_recovery_codes
func (r *Repo) CountUnusedRecoveryCodes(ctx context.Context, userID uint64) (int64, error) {
	var n int64
	err := r.ex(ctx).Get(ctx, &n, db.Q(`SELECT COUNT(*) FROM user_recovery_codes WHERE user_id = ? AND used_at IS NULL`), userID)
	return n, err
}

// ---- mfa 挑战 ----

// CreateMFAChallenge 写入新的挑战（只含令牌哈希），并顺带清理已过期一小时以上的旧挑战。
// lingedge:platform-table=login_mfa_challenges
func (r *Repo) CreateMFAChallenge(ctx context.Context, tokenHash string, userID uint64, realm string, ip []byte, now, expiresAt time.Time) error {
	if _, err := r.ex(ctx).Exec(ctx, db.Q(`DELETE FROM login_mfa_challenges WHERE expires_at < ? LIMIT 100`), now.Add(-time.Hour)); err != nil {
		return err
	}
	_, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO login_mfa_challenges (created_at, token_hash, user_id, realm, ip, expires_at) VALUES (?, ?, ?, ?, ?, ?)`),
		now, tokenHash, userID, realm, nilIfEmpty(ip), expiresAt)
	return err
}

// FindMFAChallenge 按令牌哈希取挑战（不判断有效性）；须在事务内用 forUpdate 锁行以串行化同一挑战的并发验证。
// lingedge:platform-table=login_mfa_challenges
func (r *Repo) FindMFAChallenge(ctx context.Context, tokenHash string, forUpdate bool) (domain.MFAChallenge, error) {
	var c domain.MFAChallenge
	var err error
	if forUpdate {
		err = r.ex(ctx).Get(ctx, &c, db.Q(`SELECT id, user_id, realm, expires_at, attempts, consumed_at FROM login_mfa_challenges WHERE token_hash = ? FOR UPDATE`), tokenHash)
	} else {
		err = r.ex(ctx).Get(ctx, &c, db.Q(`SELECT id, user_id, realm, expires_at, attempts, consumed_at FROM login_mfa_challenges WHERE token_hash = ?`), tokenHash)
	}
	return c, notFound(err)
}

// BumpMFAAttempts 记一次验证码尝试失败。
// lingedge:platform-table=login_mfa_challenges
func (r *Repo) BumpMFAAttempts(ctx context.Context, id uint64) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE login_mfa_challenges SET attempts = attempts + 1 WHERE id = ?`), id)
	return err
}

// ConsumeMFAChallenge 原子地消耗挑战（一次性）；返回是否由本次调用消耗。
// lingedge:platform-table=login_mfa_challenges
func (r *Repo) ConsumeMFAChallenge(ctx context.Context, id uint64, now time.Time) (bool, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE login_mfa_challenges SET consumed_at = ? WHERE id = ? AND consumed_at IS NULL AND expires_at > ?`), now, id, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---- 会话的 MFA / 再认证时间 ----

// SetSessionReauth 刷新会话的再认证时间；mfa 非 nil 时同时刷新 mfa_verified_at（再认证包含第二因子）。
// lingedge:identity-table=sessions
func (r *Repo) SetSessionReauth(ctx context.Context, sessionID uint64, reauthAt time.Time, mfa *time.Time) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE sessions SET reauth_at = ?, mfa_verified_at = COALESCE(?, mfa_verified_at) WHERE id = ? AND revoked_at IS NULL`), reauthAt, mfa, sessionID)
	return err
}

// SetSessionMFA 记录会话的第二因子通过时间。
// lingedge:identity-table=sessions
func (r *Repo) SetSessionMFA(ctx context.Context, sessionID uint64, at time.Time) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE sessions SET mfa_verified_at = ? WHERE id = ? AND revoked_at IS NULL`), at, sessionID)
	return err
}
