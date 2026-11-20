-- lingedge:class=expand
-- lingedge:summary=证书：证书（私钥仅存信封密文）、证书部署记录
-- 依据 database/README §2.2、§8.1，rules-cache-cert.md §3.1、§3.4。原属 Story 1.9 的范围，因 000009 的 site_domains.certificate_id、
-- site_settings.default_certificate_id 外键依赖而前移到 1.8（epics Story 1.8 AC1）。
-- private_key_enc 为 5 段信封密文 v1:<kek_id>:<wrapped_dek>:<nonce>:<ct>（ADR-011），API 永不回显；MEDIUMTEXT 无 DEFAULT（README §2.2）。
-- certificates.acme_account_id 先建为无 FK 普通列：acme_accounts 随 P2 创建时追加外键。
-- 均为租户范围表（T）；certificates 软删除（SD）。证书→站点用复合外键 (tenant_id, site_id) → sites(tenant_id, id)。
-- 到期扫描/按节点维度的索引属平台侧跨租户查询，按 README §2.6 显式登记豁免。
-- +goose Up

-- ---- certificates：证书（C1 + T + SD；手动上传 M1，ACME P2）----
-- lingedge:cross-tenant-index=idx_certificates_expiry 平台侧到期扫描与续期告警按 (status, not_after) 跨租户查询
CREATE TABLE IF NOT EXISTS certificates (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NULL,
  name VARCHAR(128) NOT NULL,
  source VARCHAR(16) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  primary_domain VARCHAR(253) NOT NULL,
  domains_json JSON NOT NULL,
  is_wildcard TINYINT(1) NOT NULL DEFAULT 0,
  serial VARCHAR(64) NOT NULL,
  issuer VARCHAR(255) NOT NULL,
  not_before DATETIME(3) NOT NULL,
  not_after DATETIME(3) NOT NULL,
  fingerprint_sha256 CHAR(64) NOT NULL,
  key_algorithm VARCHAR(16) NOT NULL,
  chain_pem MEDIUMTEXT NOT NULL,
  private_key_enc MEDIUMTEXT NOT NULL,
  enc_key_id VARCHAR(32) NOT NULL,
  cert_version INT UNSIGNED NOT NULL DEFAULT 1,
  acme_account_id BIGINT UNSIGNED NULL,
  auto_renew TINYINT(1) NOT NULL DEFAULT 0,
  renew_before_days INT UNSIGNED NULL,
  last_renew_at DATETIME(3) NULL,
  last_renew_error VARCHAR(512) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_certificates_uid (uid),
  KEY idx_certificates_tenant (tenant_id, site_id),
  KEY idx_certificates_expiry (status, not_after),
  KEY idx_certificates_fp (tenant_id, fingerprint_sha256),
  CONSTRAINT ck_certificates_source CHECK (source IN ('upload', 'acme')),
  CONSTRAINT ck_certificates_status CHECK (status IN ('pending', 'active', 'expiring', 'expired', 'revoked', 'error')),
  CONSTRAINT ck_certificates_key_algorithm CHECK (key_algorithm IN ('rsa2048', 'rsa3072', 'rsa4096', 'ecdsa_p256', 'ecdsa_p384')),
  CONSTRAINT ck_certificates_validity CHECK (not_before < not_after),
  CONSTRAINT ck_certificates_auto_renew CHECK (auto_renew = 0 OR source = 'acme'),
  CONSTRAINT fk_certificates_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_certificates_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON certificates TO lingedge_app;

-- ---- certificate_deployments：证书分发到每个节点的部署记录（C2 + updated_at + T；FR-86）----
-- lingedge:cross-tenant-index=uk_cert_deploy 平台侧按 (证书, 版本, 节点) 幂等 upsert 部署记录（certificate_id 已隐含租户）
-- lingedge:cross-tenant-index=idx_cert_deploy_node 节点上线/重载时按节点拉取待部署证书（平台侧跨租户查询）
CREATE TABLE IF NOT EXISTS certificate_deployments (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  certificate_id BIGINT UNSIGNED NOT NULL,
  cert_version INT UNSIGNED NOT NULL,
  node_id BIGINT UNSIGNED NOT NULL,
  cluster_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  error VARCHAR(512) NULL,
  deployed_at DATETIME(3) NULL,
  last_report_at DATETIME(3) NULL,
  revision_seq BIGINT UNSIGNED NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_cert_deploy (certificate_id, cert_version, node_id),
  KEY idx_cert_deploy_node (node_id, status),
  KEY idx_cert_deploy_tenant (tenant_id, certificate_id),
  CONSTRAINT ck_certificate_deployments_status CHECK (status IN ('pending', 'distributed', 'applied', 'failed', 'removed')),
  CONSTRAINT fk_certificate_deployments_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_certificate_deployments_certificates FOREIGN KEY (certificate_id) REFERENCES certificates (id) ON DELETE RESTRICT,
  CONSTRAINT fk_certificate_deployments_nodes FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE RESTRICT,
  CONSTRAINT fk_certificate_deployments_clusters FOREIGN KEY (cluster_id) REFERENCES clusters (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON certificate_deployments TO lingedge_app;
