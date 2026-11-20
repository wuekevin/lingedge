-- lingedge:class=expand
-- lingedge:summary=TOTP 二步登录：login_mfa_challenges（一次性、限次的 mfa_token 挑战）（Story 1.17、CC-5）
-- 依据 docs/bmad/contract-changes/CC-5-totp-mfa-schema.md：000001 的 users/user_recovery_codes/sessions 已有 totp_enabled/totp_secret_enc/恢复码/mfa_verified_at/reauth_at，
-- 但缺两处：①OpenAPI LoginResponse.mfa_token 是「5 分钟、一次性」的挑战令牌，需要可跨实例、Redis 不可用时仍有效的存放处；
-- （②已用 time-step 列见 000017。）M1 冻结范围（000001–000013、61 张表）不变，本文件是其后的追加迁移。
-- 挑战令牌：32 字节 CSPRNG，库内只存 SHA-256；attempts 限制同一挑战的验证码尝试次数；consumed_at 保证一次性。无外键以外的外部引用；用户删除时级联清理。
-- 授权：新表在此给 lingedge_app 授予表级 SELECT/INSERT/UPDATE/DELETE；users 已有表级授权，新增列自动适用。
-- +goose Up
CREATE TABLE IF NOT EXISTS login_mfa_challenges (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  token_hash CHAR(64) NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  realm VARCHAR(16) NOT NULL,
  ip VARBINARY(16) NULL,
  expires_at DATETIME(3) NOT NULL,
  attempts SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  consumed_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_login_mfa_token (token_hash),
  KEY idx_login_mfa_user (user_id),
  KEY idx_login_mfa_exp (expires_at),
  CONSTRAINT ck_login_mfa_realm CHECK (realm IN ('admin', 'console')),
  CONSTRAINT fk_login_mfa_users FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON login_mfa_challenges TO lingedge_app;
