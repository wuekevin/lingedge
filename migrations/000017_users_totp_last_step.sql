-- lingedge:class=expand
-- lingedge:summary=users 追加 totp_last_step：已用的 TOTP time-step，用于「同一验证码在窗口内重放被拒」（Story 1.17、CC-5）
-- 依据 docs/bmad/contract-changes/CC-5-totp-mfa-schema.md：重放防护须持久化（多实例一致、Redis 不可用时仍有效），用原子 compare-and-set：
-- UPDATE users SET totp_last_step = ? WHERE id = ? AND (totp_last_step IS NULL OR totp_last_step < ?)；受影响行数为 0 即重放。可空列，对已存在行安全。
-- 授权：users 已在 000001 对 lingedge_app 授予表级 SELECT/INSERT/UPDATE/DELETE，新增列自动适用。
-- +goose Up
ALTER TABLE users ADD COLUMN totp_last_step BIGINT UNSIGNED NULL AFTER totp_secret_enc;
