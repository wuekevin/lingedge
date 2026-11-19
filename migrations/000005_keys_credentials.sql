-- lingedge:class=expand
-- lingedge:summary=签名与 CA 密钥：signing_keys（provider_credentials 随 P2，其 plugin_id 为无 FK 普通列，P5 建 plugins 时追加外键）
-- 依据 database/README §8.1，platform-identity.md §5.1，ADR-011/012（信任域 ②配置签名、③节点 mTLS CA；发行签名私钥离线不入库）。
-- private_key_enc 仅存 5 段信封密文（v1:<kek_id>:<wrapped_dek>:<nonce>:<ct>），enc_key_id = 当前包裹 DEK 的 kek_id（供 secrets.rewrap 扫描）。
-- 每个 purpose 同时至多一个 active：生成列 active_key = IF(status='active', purpose, NULL) 上建唯一索引（基列无外键，STORED 可用）。
-- +goose Up
CREATE TABLE IF NOT EXISTS signing_keys (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  purpose VARCHAR(24) NOT NULL,
  key_id VARCHAR(32) NOT NULL,
  algorithm VARCHAR(24) NOT NULL,
  public_key VARBINARY(128) NOT NULL,
  cert_pem MEDIUMTEXT NULL,
  private_key_enc MEDIUMTEXT NOT NULL,
  enc_key_id VARCHAR(32) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  activated_at DATETIME(3) NULL,
  retire_after DATETIME(3) NULL,
  revoked_at DATETIME(3) NULL,
  keyset_signature VARBINARY(64) NULL,
  active_key VARCHAR(24) AS (IF(status = 'active', purpose, NULL)) STORED,
  PRIMARY KEY (id),
  UNIQUE KEY uk_signing_keys_uid (uid),
  UNIQUE KEY uk_signing_keys_keyid (key_id),
  UNIQUE KEY uk_signing_keys_active (active_key),
  KEY idx_signing_keys_purpose (purpose, status),
  CONSTRAINT ck_signing_keys_purpose CHECK (purpose IN ('config_signing', 'mtls_ca', 'grpc_server')),
  CONSTRAINT ck_signing_keys_algorithm CHECK (algorithm IN ('ed25519', 'ecdsa_p256')),
  CONSTRAINT ck_signing_keys_status CHECK (status IN ('pending', 'active', 'retiring', 'revoked'))
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON signing_keys TO lingedge_app;
