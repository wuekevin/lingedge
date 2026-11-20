-- LingEdge M1 迁移骨架（仅保留主键、外键、生成列、CHECK、分区与触发器，用于验证迁移顺序与 DDL 规则）
-- 依据：docs/database/README.md §8.1（迁移顺序）、§2.2/§2.4（生成列 / CHECK / 外键规则）、node-deploy.md 等各表文档。
-- 用法：在**空库**上整体执行，期望 0 错误（MySQL >= 8.0.16；2026-11-16 在 8.0.46 实测通过）：
--   mysql -h127.0.0.1 -P<port> -uroot -e 'DROP DATABASE IF EXISTS le_m1; CREATE DATABASE le_m1 CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci' && \
--   mysql -h127.0.0.1 -P<port> -uroot le_m1 < m1-fk-skeleton.sql
-- 说明：列只保留与约束相关者；真实迁移以各表文档的完整字段为准。触发器创建需要 SUPER/SET_USER_ID 或 log_bin_trust_function_creators=1（README §5.1）。
-- 每个“-- ==== 000nnn”分段对应 §8.1 的一个迁移文件；其中 ALTER ... ADD CONSTRAINT 即循环/前向外键的追加。

SET NAMES utf8mb4;

-- ==== 000001 identity_core
CREATE TABLE users (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  uid CHAR(26) NOT NULL, email VARCHAR(255) NOT NULL, deleted_at DATETIME(3) NULL,
  user_type VARCHAR(16) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'active',
  totp_enabled TINYINT(1) NOT NULL DEFAULT 0, totp_secret_enc TEXT NULL,
  active_email VARCHAR(255) AS (IF(deleted_at IS NULL, email, NULL)) STORED,
  UNIQUE KEY uk_users_uid (uid), UNIQUE KEY uk_users_active_email (active_email),
  CONSTRAINT ck_users_type CHECK (user_type IN ('platform','tenant')),
  CONSTRAINT ck_users_totp CHECK (totp_enabled=0 OR totp_secret_enc IS NOT NULL),
  CONSTRAINT ck_users_email_lower CHECK (BINARY email = BINARY LOWER(email))
) ENGINE=InnoDB;

CREATE TABLE tenants (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  uid CHAR(26) NOT NULL, slug VARCHAR(63) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'active',
  is_default TINYINT(1) NOT NULL DEFAULT 0, default_flag TINYINT AS (IF(is_default=1,1,NULL)) STORED,
  owner_user_id BIGINT UNSIGNED NULL, reseller_id BIGINT UNSIGNED NULL, deleted_at DATETIME(3) NULL,
  UNIQUE KEY uk_tenants_uid (uid), UNIQUE KEY uk_tenants_slug (slug), UNIQUE KEY uk_tenants_default (default_flag),
  CONSTRAINT ck_tenants_slug CHECK (REGEXP_LIKE(slug,'^[a-z0-9][a-z0-9-]{0,62}$')),
  CONSTRAINT fk_tenants_owner FOREIGN KEY (owner_user_id) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE user_recovery_codes (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, user_id BIGINT UNSIGNED NOT NULL, code_hash CHAR(64) NOT NULL,
  UNIQUE KEY uk_user_recovery_codes (user_id, code_hash),
  CONSTRAINT fk_urc_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE tenant_members (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  tenant_id BIGINT UNSIGNED NOT NULL, user_id BIGINT UNSIGNED NOT NULL, invited_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_tenant_members (tenant_id, user_id),
  CONSTRAINT fk_tm_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_tm_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
  CONSTRAINT fk_tm_inviter FOREIGN KEY (invited_by) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE tenant_quotas (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  tenant_id BIGINT UNSIGNED NOT NULL, quota_key VARCHAR(64) NOT NULL, limit_value BIGINT NULL, subscription_id BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_tenant_quotas (tenant_id, quota_key),
  CONSTRAINT ck_tq_limit CHECK (limit_value IS NULL OR limit_value >= 0),
  CONSTRAINT fk_tq_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE permissions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  code VARCHAR(96) NOT NULL, resource VARCHAR(48) NOT NULL, action VARCHAR(48) NOT NULL, scope VARCHAR(16) NOT NULL,
  UNIQUE KEY uk_permissions_code (code),
  CONSTRAINT ck_permissions_scope CHECK (scope IN ('platform','tenant','both')),
  CONSTRAINT ck_permissions_code CHECK (code = CONCAT(resource, ':', action))
) ENGINE=InnoDB;

CREATE TABLE roles (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NULL, scope VARCHAR(16) NOT NULL, code VARCHAR(64) NOT NULL,
  uniq_key VARCHAR(160) AS (CONCAT(scope,':',COALESCE(tenant_id,0),':',code)) VIRTUAL NOT NULL,
  UNIQUE KEY uk_roles_uid (uid), UNIQUE KEY uk_roles_uniq (uniq_key),
  CONSTRAINT ck_roles_scope CHECK (scope IN ('platform','tenant')),
  CONSTRAINT ck_roles_platform CHECK (scope <> 'platform' OR tenant_id IS NULL),
  CONSTRAINT fk_roles_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE role_permissions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, role_id BIGINT UNSIGNED NOT NULL, permission_id BIGINT UNSIGNED NOT NULL,
  UNIQUE KEY uk_role_permissions (role_id, permission_id),
  CONSTRAINT fk_rp_role FOREIGN KEY (role_id) REFERENCES roles(id) ON DELETE CASCADE,
  CONSTRAINT fk_rp_perm FOREIGN KEY (permission_id) REFERENCES permissions(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE user_roles (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  user_id BIGINT UNSIGNED NOT NULL, role_id BIGINT UNSIGNED NOT NULL, tenant_id BIGINT UNSIGNED NULL,
  tenant_key BIGINT UNSIGNED AS (COALESCE(tenant_id,0)) VIRTUAL NOT NULL, granted_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_user_roles (user_id, role_id, tenant_key), KEY idx_user_roles_tenant (tenant_id, user_id),
  CONSTRAINT fk_ur_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
  CONSTRAINT fk_ur_role FOREIGN KEY (role_id) REFERENCES roles(id) ON DELETE CASCADE,
  CONSTRAINT fk_ur_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_ur_granter FOREIGN KEY (granted_by) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE sessions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  token_hash CHAR(64) NOT NULL, user_id BIGINT UNSIGNED NOT NULL, realm VARCHAR(16) NOT NULL, tenant_id BIGINT UNSIGNED NULL,
  mfa_verified_at DATETIME(3) NULL, reauth_at DATETIME(3) NULL,
  UNIQUE KEY uk_sessions_token (token_hash),
  CONSTRAINT ck_sessions_realm CHECK (realm IN ('admin','console')),
  CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
  CONSTRAINT fk_sessions_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE api_tokens (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NULL, user_id BIGINT UNSIGNED NOT NULL,
  token_prefix CHAR(8) NOT NULL, token_hash CHAR(64) NOT NULL, rotated_from_id BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_api_tokens_uid (uid), UNIQUE KEY uk_api_tokens_prefix (token_prefix), UNIQUE KEY uk_api_tokens_hash (token_hash),
  CONSTRAINT fk_at_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_at_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
  CONSTRAINT fk_at_rotated FOREIGN KEY (rotated_from_id) REFERENCES api_tokens(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE login_logs (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL,
  user_id BIGINT UNSIGNED NULL, tenant_id BIGINT UNSIGNED NULL, event VARCHAR(24) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_login_logs_uid (uid), KEY idx_login_logs_user (user_id, created_at)
) ENGINE=InnoDB;

-- ==== 000002 platform_core
CREATE TABLE system_settings (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL,
  setting_key VARCHAR(128) NOT NULL, value_json JSON NULL, value_enc TEXT NULL, updated_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_system_settings_uid (uid), UNIQUE KEY uk_system_settings_key (setting_key),
  CONSTRAINT ck_system_settings_one_value CHECK ((value_json IS NULL) <> (value_enc IS NULL)),
  CONSTRAINT fk_ss_user FOREIGN KEY (updated_by) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE system_installations (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL,
  component VARCHAR(32) NOT NULL, host VARCHAR(255) NOT NULL, instance_name VARCHAR(64) NOT NULL,
  UNIQUE KEY uk_si_uid (uid), UNIQUE KEY uk_system_installations_inst (component, host, instance_name)
) ENGINE=InnoDB;

CREATE TABLE tasks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL,
  tenant_id BIGINT UNSIGNED NULL, type VARCHAR(64) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'pending',
  progress TINYINT UNSIGNED NOT NULL DEFAULT 0, run_after DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  idempotency_key VARCHAR(128) NULL, parent_task_id BIGINT UNSIGNED NULL, created_by_user_id BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_tasks_uid (uid), UNIQUE KEY uk_tasks_idem (type, idempotency_key), KEY idx_tasks_claim (status, run_after),
  CONSTRAINT ck_tasks_progress CHECK (progress <= 100),
  CONSTRAINT fk_tasks_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_tasks_parent FOREIGN KEY (parent_task_id) REFERENCES tasks(id) ON DELETE CASCADE,
  CONSTRAINT fk_tasks_user FOREIGN KEY (created_by_user_id) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE task_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, task_id BIGINT UNSIGNED NOT NULL,
  UNIQUE KEY uk_task_events_uid (uid), KEY idx_task_events_task (task_id, id),
  CONSTRAINT fk_te_task FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE job_locks (name VARCHAR(128) NOT NULL PRIMARY KEY, holder VARCHAR(128) NOT NULL, fence BIGINT UNSIGNED NOT NULL DEFAULT 0, lease_until DATETIME(3) NOT NULL) ENGINE=InnoDB;

CREATE TABLE idempotency_keys (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, principal_key VARCHAR(64) NOT NULL, idem_key VARCHAR(128) NOT NULL,
  task_id BIGINT UNSIGNED NULL, expires_at DATETIME(3) NOT NULL,
  UNIQUE KEY uk_idem (principal_key, idem_key), KEY idx_idem_exp (expires_at),
  CONSTRAINT fk_idem_task FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE event_outbox (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, event_type VARCHAR(96) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'pending',
  available_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3), KEY idx_outbox_poll (status, available_at)
) ENGINE=InnoDB;

-- ==== 000003 audit_logs（分区表：无外键；含 pmax；触发器）
CREATE TABLE audit_logs (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NULL, actor_user_id BIGINT UNSIGNED NULL, action VARCHAR(96) NOT NULL,
  PRIMARY KEY (id, created_at), UNIQUE KEY uk_audit_logs_uid (uid, created_at),
  KEY idx_audit_tenant (tenant_id, created_at), KEY idx_audit_actor (actor_user_id, created_at)
) ENGINE=InnoDB
PARTITION BY RANGE COLUMNS(created_at) (
  PARTITION p202611 VALUES LESS THAN ('2026-12-01 00:00:00'),
  PARTITION p202612 VALUES LESS THAN ('2027-01-01 00:00:00'),
  PARTITION pmax VALUES LESS THAN (MAXVALUE)
);
CREATE TRIGGER trg_audit_logs_bu BEFORE UPDATE ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_logs is immutable';
CREATE TRIGGER trg_audit_logs_bd BEFORE DELETE ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_logs delete forbidden';
-- 真实迁移末尾：GRANT SELECT, ALTER, DROP ON <db>.audit_logs TO lingedge_maint;（迁移账号需 GRANT OPTION，README §5.1）

-- ==== 000005 keys_credentials
CREATE TABLE signing_keys (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL,
  purpose VARCHAR(24) NOT NULL, key_id VARCHAR(32) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'pending',
  active_key VARCHAR(24) AS (IF(status='active', purpose, NULL)) STORED,
  UNIQUE KEY uk_signing_keys_uid (uid), UNIQUE KEY uk_signing_keys_keyid (key_id), UNIQUE KEY uk_signing_keys_active (active_key),
  CONSTRAINT ck_signing_keys_purpose CHECK (purpose IN ('config_signing','mtls_ca','grpc_server'))
) ENGINE=InnoDB;

-- ==== 000006 nodes_clusters
CREATE TABLE regions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, code VARCHAR(32) NOT NULL, parent_id BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_regions_uid (uid), UNIQUE KEY uk_regions_code (code),
  CONSTRAINT fk_regions_parent FOREIGN KEY (parent_id) REFERENCES regions(id) ON DELETE SET NULL
) ENGINE=InnoDB;
CREATE TABLE isp_lines (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, code VARCHAR(32) NOT NULL, UNIQUE KEY uk_isp_lines_uid (uid), UNIQUE KEY uk_isp_lines_code (code)) ENGINE=InnoDB;
CREATE TABLE ip_pools (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, name VARCHAR(128) NOT NULL, UNIQUE KEY uk_ip_pools_uid (uid), UNIQUE KEY uk_ip_pools_name (name)) ENGINE=InnoDB;

CREATE TABLE nodes (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL,
  name VARCHAR(128) NOT NULL, deleted_at DATETIME(3) NULL, status VARCHAR(16) NOT NULL DEFAULT 'pending_enroll',
  region_id BIGINT UNSIGNED NULL, isp_line_id BIGINT UNSIGNED NULL, ca_bundle_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
  active_name VARCHAR(128) AS (IF(deleted_at IS NULL, name, NULL)) STORED,
  UNIQUE KEY uk_nodes_uid (uid), UNIQUE KEY uk_nodes_active_name (active_name),
  CONSTRAINT fk_nodes_region FOREIGN KEY (region_id) REFERENCES regions(id) ON DELETE SET NULL,
  CONSTRAINT fk_nodes_isp FOREIGN KEY (isp_line_id) REFERENCES isp_lines(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE ip_pool_members (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, ip_pool_id BIGINT UNSIGNED NOT NULL, node_id BIGINT UNSIGNED NULL, address VARCHAR(45) NOT NULL,
  UNIQUE KEY uk_ip_pool_members (address),
  CONSTRAINT fk_ipm_pool FOREIGN KEY (ip_pool_id) REFERENCES ip_pools(id) ON DELETE CASCADE,
  CONSTRAINT fk_ipm_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE clusters (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, name VARCHAR(128) NOT NULL,
  current_seq BIGINT UNSIGNED NOT NULL DEFAULT 0,
  confirmed_revision_id BIGINT UNSIGNED NULL,   -- FK 在 000012 末尾追加（循环：config_revisions.cluster_id → clusters）
  UNIQUE KEY uk_clusters_uid (uid), UNIQUE KEY uk_clusters_name (name)
) ENGINE=InnoDB;

CREATE TABLE cluster_nodes (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, cluster_id BIGINT UNSIGNED NOT NULL, node_id BIGINT UNSIGNED NOT NULL,
  UNIQUE KEY uk_cluster_nodes (cluster_id, node_id), KEY idx_cluster_nodes_node (node_id),
  CONSTRAINT fk_cn_cluster FOREIGN KEY (cluster_id) REFERENCES clusters(id) ON DELETE RESTRICT,
  CONSTRAINT fk_cn_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE enroll_tokens (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, node_id BIGINT UNSIGNED NOT NULL,
  role VARCHAR(16) NOT NULL DEFAULT 'node', token_hash CHAR(64) NOT NULL, created_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_enroll_tokens_uid (uid), UNIQUE KEY uk_enroll_tokens_hash (token_hash),
  CONSTRAINT ck_et_role CHECK (role IN ('node','dns','reporter','scheduler','worker')),
  CONSTRAINT fk_et_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE CASCADE,
  CONSTRAINT fk_et_user FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE node_certificates (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, subject_type VARCHAR(16) NOT NULL DEFAULT 'node',
  subject_id BIGINT UNSIGNED NOT NULL, node_id BIGINT UNSIGNED NULL, serial VARCHAR(64) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active', superseded_at DATETIME(3) NULL, grace_until DATETIME(3) NULL,
  UNIQUE KEY uk_node_certs_serial (serial), KEY idx_node_certs_subject (subject_type, subject_id, status),
  CONSTRAINT ck_nc_subject CHECK ((subject_type='node') = (node_id IS NOT NULL)),
  CONSTRAINT ck_nc_status CHECK (status IN ('pending','active','superseded','revoked','expired')),
  CONSTRAINT fk_nc_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE node_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, node_id BIGINT UNSIGNED NOT NULL, event_type VARCHAR(48) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_node_events_uid (uid), KEY idx_node_events_node (node_id, id),
  CONSTRAINT fk_ne_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE node_heartbeats (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, node_id BIGINT UNSIGNED NOT NULL, received_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id, received_at), KEY idx_node_heartbeats_node (node_id, received_at)
) ENGINE=InnoDB
PARTITION BY RANGE COLUMNS(received_at) (PARTITION p20261116 VALUES LESS THAN ('2026-11-17'), PARTITION pmax VALUES LESS THAN (MAXVALUE));

-- ==== 000007 sites_core
CREATE TABLE site_groups (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL, name VARCHAR(128) NOT NULL,
  UNIQUE KEY uk_site_groups_uid (uid), UNIQUE KEY uk_site_groups (tenant_id, name),
  CONSTRAINT fk_sg_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE sites (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(128) NOT NULL, deleted_at DATETIME(3) NULL, group_id BIGINT UNSIGNED NULL,
  config_version BIGINT UNSIGNED NOT NULL DEFAULT 1, published_config_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
  published_version_no BIGINT UNSIGNED NOT NULL DEFAULT 0, published_by BIGINT UNSIGNED NULL,
  last_deployment_id BIGINT UNSIGNED NULL,       -- FK 在 000012 末尾追加（循环）
  active_name VARCHAR(128) AS (IF(deleted_at IS NULL, name, NULL)) STORED,
  UNIQUE KEY uk_sites_uid (uid), UNIQUE KEY uk_sites_tenant_id (tenant_id, id), UNIQUE KEY uk_sites_active_name (tenant_id, active_name),
  CONSTRAINT ck_sites_pub CHECK (published_config_version <= config_version),
  CONSTRAINT fk_sites_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_sites_group FOREIGN KEY (group_id) REFERENCES site_groups(id) ON DELETE SET NULL,
  CONSTRAINT fk_sites_pub_by FOREIGN KEY (published_by) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;

-- ==== 000008 certificates
CREATE TABLE certificates (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NULL,
  deleted_at DATETIME(3) NULL, source VARCHAR(16) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'active', auto_renew TINYINT(1) NOT NULL DEFAULT 0,
  not_before DATETIME(3) NOT NULL, not_after DATETIME(3) NOT NULL, cert_version INT UNSIGNED NOT NULL DEFAULT 1, acme_account_id BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_certificates_uid (uid), UNIQUE KEY uk_certificates_tenant_id (tenant_id, id), KEY idx_certificates_tenant (tenant_id, site_id),
  CONSTRAINT ck_cert_dates CHECK (not_before < not_after), CONSTRAINT ck_cert_renew CHECK (auto_renew=0 OR source='acme'),
  CONSTRAINT fk_cert_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_cert_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE certificate_deployments (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL,
  certificate_id BIGINT UNSIGNED NOT NULL, cert_version INT UNSIGNED NOT NULL, node_id BIGINT UNSIGNED NOT NULL, cluster_id BIGINT UNSIGNED NOT NULL,
  UNIQUE KEY uk_cert_deploy (certificate_id, cert_version, node_id),
  CONSTRAINT fk_cd_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_cd_cert FOREIGN KEY (tenant_id, certificate_id) REFERENCES certificates(tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_cd_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE RESTRICT,
  CONSTRAINT fk_cd_cluster FOREIGN KEY (cluster_id) REFERENCES clusters(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

-- ==== 000009 site_config
CREATE TABLE site_domains (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL,
  hostname VARCHAR(253) NOT NULL, deleted_at DATETIME(3) NULL, certificate_id BIGINT UNSIGNED NULL,
  verification_status VARCHAR(16) NOT NULL DEFAULT 'pending',
  active_hostname VARCHAR(253) AS (IF(deleted_at IS NULL, hostname, NULL)) STORED,
  UNIQUE KEY uk_site_domains_uid (uid), UNIQUE KEY uk_site_domains_active_host (active_hostname), KEY idx_site_domains_site (tenant_id, site_id),
  CONSTRAINT ck_sd_host CHECK (REGEXP_LIKE(hostname, '^(\\*\\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z0-9-]{2,63}$')),
  CONSTRAINT ck_sd_lower CHECK (BINARY hostname = BINARY LOWER(hostname)),
  CONSTRAINT ck_sd_verif CHECK (verification_status IN ('exempt','pending','verified','failed')),
  CONSTRAINT fk_sd_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_sd_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_sd_cert FOREIGN KEY (tenant_id, certificate_id) REFERENCES certificates(tenant_id, id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE site_settings (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL,
  real_ip_custom_enabled TINYINT(1) NOT NULL DEFAULT 0, real_ip_custom_header VARCHAR(64) NULL, default_certificate_id BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_site_settings_uid (uid), UNIQUE KEY uk_site_settings_site (site_id),
  CONSTRAINT ck_ss_realip CHECK (real_ip_custom_enabled=0 OR (real_ip_custom_header IS NOT NULL AND REGEXP_LIKE(real_ip_custom_header,'^[A-Za-z][A-Za-z0-9-]{0,63}$'))),
  CONSTRAINT fk_sset_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_sset_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_sset_cert FOREIGN KEY (tenant_id, default_certificate_id) REFERENCES certificates(tenant_id, id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE origin_groups (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(128) NOT NULL, is_default TINYINT(1) NOT NULL DEFAULT 0,
  default_flag BIGINT UNSIGNED AS (IF(is_default=1, site_id, NULL)) STORED,
  UNIQUE KEY uk_origin_groups_uid (uid), UNIQUE KEY uk_origin_groups_name (site_id, name), UNIQUE KEY uk_origin_groups_default (default_flag),
  UNIQUE KEY uk_origin_groups_site_id (site_id, id),
  CONSTRAINT fk_og_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_og_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE origins (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL,
  origin_group_id BIGINT UNSIGNED NOT NULL, port SMALLINT UNSIGNED NOT NULL, weight SMALLINT UNSIGNED NOT NULL DEFAULT 100,
  UNIQUE KEY uk_origins_uid (uid), UNIQUE KEY uk_origins_tenant_id (tenant_id, id),
  CONSTRAINT ck_origins_port CHECK (port BETWEEN 1 AND 65535), CONSTRAINT ck_origins_weight CHECK (weight <= 1000),
  CONSTRAINT fk_o_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_o_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_o_group FOREIGN KEY (site_id, origin_group_id) REFERENCES origin_groups(site_id, id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE origin_health (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, origin_id BIGINT UNSIGNED NOT NULL, node_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(8) NOT NULL DEFAULT 'unknown', UNIQUE KEY uk_origin_health (origin_id, node_id),
  CONSTRAINT fk_oh_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_oh_origin FOREIGN KEY (tenant_id, origin_id) REFERENCES origins(tenant_id, id) ON DELETE CASCADE,
  CONSTRAINT fk_oh_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE site_cluster_bindings (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL, cluster_id BIGINT UNSIGNED NOT NULL,
  UNIQUE KEY uk_site_cluster (site_id, cluster_id), KEY idx_scb_cluster (cluster_id),
  CONSTRAINT fk_scb_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_scb_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE CASCADE,
  CONSTRAINT fk_scb_cluster FOREIGN KEY (cluster_id) REFERENCES clusters(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE node_tasks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, node_id BIGINT UNSIGNED NOT NULL, task_id BIGINT UNSIGNED NULL,
  tenant_id BIGINT UNSIGNED NULL, site_id BIGINT UNSIGNED NULL, kind VARCHAR(24) NOT NULL, status VARCHAR(12) NOT NULL DEFAULT 'pending',
  attempt INT UNSIGNED NOT NULL DEFAULT 0, max_attempts INT UNSIGNED NOT NULL DEFAULT 5, lease_until DATETIME(3) NULL, deadline_at DATETIME(3) NULL,
  UNIQUE KEY uk_node_tasks_uid (uid), KEY idx_node_tasks_pull (node_id, status, deadline_at),
  CONSTRAINT ck_nt_kind CHECK (kind IN ('cache_purge_url','cache_purge_site','cache_purge_prefix','cache_prefetch','node_upgrade','node_rollback','cert_reload','debug_request')),
  CONSTRAINT ck_nt_attempt CHECK (attempt <= max_attempts),
  CONSTRAINT fk_nt_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE RESTRICT,
  CONSTRAINT fk_nt_task FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE SET NULL,
  CONSTRAINT fk_nt_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_nt_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT
) ENGINE=InnoDB;

-- ==== 000010 rules（site_rules ↔ rule_versions 循环：先建两表，再 ALTER）
CREATE TABLE rule_templates (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NULL, name VARCHAR(128) NOT NULL,
  tenant_key BIGINT UNSIGNED AS (COALESCE(tenant_id,0)) VIRTUAL NOT NULL,
  UNIQUE KEY uk_rule_templates_uid (uid), UNIQUE KEY uk_rule_templates (tenant_key, name),
  CONSTRAINT fk_rt_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE site_rules (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL,
  rule_type VARCHAR(32) NOT NULL, phase VARCHAR(32) NOT NULL, name VARCHAR(128) NOT NULL, deleted_at DATETIME(3) NULL, priority INT NOT NULL DEFAULT 1000,
  template_id BIGINT UNSIGNED NULL, current_version_id BIGINT UNSIGNED NULL, published_version_id BIGINT UNSIGNED NULL,
  active_name VARCHAR(128) AS (IF(deleted_at IS NULL, name, NULL)) STORED,
  UNIQUE KEY uk_site_rules_uid (uid), UNIQUE KEY uk_site_rules_tenant_site_id (tenant_id, site_id, id), UNIQUE KEY uk_site_rules_name (site_id, active_name), KEY idx_site_rules_site_phase (tenant_id, site_id, phase, priority),
  CONSTRAINT ck_site_rules_prio CHECK (priority BETWEEN -100000 AND 100000),
  CONSTRAINT ck_site_rules_type_phase CHECK (
    (rule_type='redirect' AND phase='redirect') OR (rule_type IN ('url_rewrite','request_header') AND phase='rewrite') OR
    (rule_type='cache_rule' AND phase='cache_rule') OR (rule_type='origin_rule' AND phase='origin_rule') OR
    (rule_type='origin_request_header' AND phase='origin_headers') OR (rule_type='error_page' AND phase='upstream_response') OR
    (rule_type='response_header' AND phase='response_headers') OR (rule_type IN ('access_control','hotlink') AND phase='access_control') OR
    (rule_type='rate_limit' AND phase='waf_cc') OR (rule_type='page_rule')),
  CONSTRAINT fk_sr_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_sr_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_sr_template FOREIGN KEY (template_id) REFERENCES rule_templates(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE rule_versions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL,
  rule_id BIGINT UNSIGNED NOT NULL, version_no INT UNSIGNED NOT NULL, rollback_of_version_id BIGINT UNSIGNED NULL, created_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_rule_versions_uid (uid), UNIQUE KEY uk_rule_versions (rule_id, version_no),
  CONSTRAINT fk_rv_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_rv_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_rv_rule FOREIGN KEY (tenant_id, site_id, rule_id) REFERENCES site_rules(tenant_id, site_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_rv_rollback FOREIGN KEY (rollback_of_version_id) REFERENCES rule_versions(id) ON DELETE SET NULL,
  CONSTRAINT fk_rv_user FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;
ALTER TABLE site_rules
  ADD CONSTRAINT fk_sr_current_version FOREIGN KEY (current_version_id) REFERENCES rule_versions(id) ON DELETE SET NULL,
  ADD CONSTRAINT fk_sr_published_version FOREIGN KEY (published_version_id) REFERENCES rule_versions(id) ON DELETE SET NULL;
CREATE TRIGGER trg_rule_versions_bu BEFORE UPDATE ON rule_versions FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'rule_versions is immutable';

-- ==== 000011 cache
CREATE TABLE cache_policies (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(128) NOT NULL, is_default TINYINT(1) NOT NULL DEFAULT 0,
  default_flag BIGINT UNSIGNED AS (IF(is_default=1, site_id, NULL)) STORED,
  default_ttl_s INT UNSIGNED NOT NULL DEFAULT 0, max_ttl_s INT UNSIGNED NOT NULL DEFAULT 2592000,
  allow_cache_set_cookie TINYINT(1) NOT NULL DEFAULT 0, allow_cache_authorization TINYINT(1) NOT NULL DEFAULT 0,
  risk_ack_by BIGINT UNSIGNED NULL, risk_ack_at DATETIME(3) NULL, cache_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0,
  UNIQUE KEY uk_cache_policies_uid (uid), UNIQUE KEY uk_cache_policies_name (site_id, name), UNIQUE KEY uk_cache_policies_default (default_flag),
  CONSTRAINT ck_cache_policies_dangerous CHECK ((allow_cache_set_cookie=0 AND allow_cache_authorization=0) OR (risk_ack_by IS NOT NULL AND risk_ack_at IS NOT NULL)),
  CONSTRAINT ck_cache_policies_epoch CHECK (is_default=1 OR cache_epoch=0),
  CONSTRAINT ck_cache_policies_ttl CHECK (default_ttl_s <= max_ttl_s),
  CONSTRAINT fk_cp_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_cp_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_cp_ack FOREIGN KEY (risk_ack_by) REFERENCES users(id) ON DELETE RESTRICT   -- 被 CHECK 引用：必须 RESTRICT（SET NULL → ERROR 3823）
) ENGINE=InnoDB;

CREATE TABLE cache_tasks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL,
  task_id BIGINT UNSIGNED NULL, kind VARCHAR(24) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'pending', requested_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_cache_tasks_uid (uid), UNIQUE KEY uk_cache_tasks_tenant_id (tenant_id, id), KEY idx_cache_tasks_site (tenant_id, site_id),
  CONSTRAINT fk_ct_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_ct_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_ct_task FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE SET NULL,
  CONSTRAINT fk_ct_user FOREIGN KEY (requested_by) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE cache_task_nodes (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, cache_task_id BIGINT UNSIGNED NOT NULL,
  node_id BIGINT UNSIGNED NOT NULL, node_task_id BIGINT UNSIGNED NULL, status VARCHAR(16) NOT NULL DEFAULT 'pending',
  UNIQUE KEY uk_cache_task_nodes (cache_task_id, node_id), KEY idx_ctn_node_status (node_id, status),
  CONSTRAINT fk_ctn_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_ctn_task FOREIGN KEY (tenant_id, cache_task_id) REFERENCES cache_tasks(tenant_id, id) ON DELETE CASCADE,
  CONSTRAINT fk_ctn_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE RESTRICT,
  CONSTRAINT fk_ctn_nodetask FOREIGN KEY (node_task_id) REFERENCES node_tasks(id) ON DELETE SET NULL
) ENGINE=InnoDB;

-- ==== 000012 config_revisions
CREATE TABLE config_blocks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, sha256 CHAR(64) NOT NULL, kind VARCHAR(16) NOT NULL,
  site_uid CHAR(26) NULL, tenant_id BIGINT UNSIGNED NULL, content MEDIUMBLOB NOT NULL,
  UNIQUE KEY uk_config_blocks_sha (sha256), KEY idx_config_blocks_site (site_uid),
  CONSTRAINT ck_cb_kind CHECK (kind IN ('site','global','l4')),
  CONSTRAINT ck_config_blocks_site_uid CHECK ((kind='site') = (site_uid IS NOT NULL) AND (site_uid IS NULL OR CHAR_LENGTH(site_uid)=26)),
  CONSTRAINT fk_cb_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE config_revisions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, cluster_id BIGINT UNSIGNED NOT NULL,
  lineage BIGINT UNSIGNED NOT NULL, seq BIGINT UNSIGNED NOT NULL, parent_seq BIGINT UNSIGNED NULL, kind VARCHAR(16) NOT NULL,
  manifest_sha256 CHAR(64) NOT NULL, rollback_of_revision_id BIGINT UNSIGNED NULL, trigger_site_id BIGINT UNSIGNED NULL,
  created_by_user_id BIGINT UNSIGNED NULL, created_by_task_id BIGINT UNSIGNED NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_config_revisions_uid (uid), UNIQUE KEY uk_config_revisions_cluster_id (cluster_id, id), UNIQUE KEY uk_config_revisions_seq (cluster_id, lineage, seq),
  UNIQUE KEY uk_config_revisions_manifest (cluster_id, manifest_sha256),
  CONSTRAINT ck_cr_kind CHECK (kind IN ('publish','rollback','system')), CONSTRAINT ck_cr_seq CHECK (seq >= 1),
  CONSTRAINT ck_cr_rollback CHECK ((kind='rollback') = (rollback_of_revision_id IS NOT NULL)),
  CONSTRAINT ck_cr_parent CHECK (parent_seq IS NULL OR parent_seq < seq),
  CONSTRAINT fk_cr_cluster FOREIGN KEY (cluster_id) REFERENCES clusters(id) ON DELETE RESTRICT,
  CONSTRAINT fk_cr_rollback FOREIGN KEY (cluster_id, rollback_of_revision_id) REFERENCES config_revisions(cluster_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_cr_site FOREIGN KEY (trigger_site_id) REFERENCES sites(id) ON DELETE SET NULL,
  CONSTRAINT fk_cr_user FOREIGN KEY (created_by_user_id) REFERENCES users(id) ON DELETE SET NULL,
  CONSTRAINT fk_cr_task FOREIGN KEY (created_by_task_id) REFERENCES tasks(id) ON DELETE SET NULL
) ENGINE=InnoDB;
CREATE TRIGGER trg_config_revisions_bu BEFORE UPDATE ON config_revisions FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'config_revisions is immutable';

CREATE TABLE config_revision_blocks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, revision_id BIGINT UNSIGNED NOT NULL, block_id BIGINT UNSIGNED NOT NULL, block_key VARCHAR(40) NOT NULL,
  UNIQUE KEY uk_crb (revision_id, block_key), KEY idx_crb_block (block_id),
  CONSTRAINT fk_crb_rev FOREIGN KEY (revision_id) REFERENCES config_revisions(id) ON DELETE CASCADE,
  CONSTRAINT fk_crb_block FOREIGN KEY (block_id) REFERENCES config_blocks(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE site_versions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL,
  version_no BIGINT UNSIGNED NOT NULL, kind VARCHAR(16) NOT NULL DEFAULT 'publish', rollback_of_version_no BIGINT UNSIGNED NULL,
  block_id BIGINT UNSIGNED NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'pending', created_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_site_versions_uid (uid), UNIQUE KEY uk_site_versions (site_id, version_no), KEY idx_site_versions_block (block_id),
  CONSTRAINT ck_sv_rollback CHECK ((kind='rollback') = (rollback_of_version_no IS NOT NULL)),
  CONSTRAINT fk_sv_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_sv_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_sv_block FOREIGN KEY (block_id) REFERENCES config_blocks(id) ON DELETE RESTRICT,
  CONSTRAINT fk_sv_user FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE publish_queue (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NULL, cluster_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NULL, kind VARCHAR(20) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'queued',
  queued_key BIGINT UNSIGNED AS (IF(status='queued', COALESCE(site_id,0), NULL)) STORED,
  deployment_id BIGINT UNSIGNED NULL,            -- FK 在本迁移末尾追加（publish_queue ↔ deployments 循环）
  site_version_id BIGINT UNSIGNED NULL, task_id BIGINT UNSIGNED NULL, requested_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_publish_queue_uid (uid), UNIQUE KEY uk_pq_queued (cluster_id, queued_key), KEY idx_pq_cluster_status (cluster_id, status, id),
  CONSTRAINT ck_pq_kind CHECK (kind IN ('site_publish','site_rollback','cluster_rollback')),
  CONSTRAINT ck_pq_site CHECK ((kind='cluster_rollback') = (site_id IS NULL)),
  CONSTRAINT fk_pq_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_pq_cluster FOREIGN KEY (cluster_id) REFERENCES clusters(id) ON DELETE RESTRICT,
  CONSTRAINT fk_pq_site FOREIGN KEY (tenant_id, site_id) REFERENCES sites(tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_pq_sv FOREIGN KEY (site_version_id) REFERENCES site_versions(id) ON DELETE SET NULL,
  CONSTRAINT fk_pq_task FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE SET NULL,
  CONSTRAINT fk_pq_user FOREIGN KEY (requested_by) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE deployments (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, cluster_id BIGINT UNSIGNED NOT NULL, revision_id BIGINT UNSIGNED NOT NULL,
  queue_id BIGINT UNSIGNED NULL, tenant_id BIGINT UNSIGNED NULL, trigger_site_id BIGINT UNSIGNED NULL, task_id BIGINT UNSIGNED NULL,
  created_by_user_id BIGINT UNSIGNED NULL, kind VARCHAR(16) NOT NULL DEFAULT 'publish', strategy VARCHAR(16) NOT NULL DEFAULT 'all', status VARCHAR(16) NOT NULL DEFAULT 'pending',
  UNIQUE KEY uk_deployments_uid (uid), KEY idx_deployments_cluster (cluster_id), KEY idx_deployments_status (status),
  CONSTRAINT fk_dep_cluster FOREIGN KEY (cluster_id) REFERENCES clusters(id) ON DELETE RESTRICT,
  CONSTRAINT fk_dep_rev FOREIGN KEY (cluster_id, revision_id) REFERENCES config_revisions(cluster_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_dep_queue FOREIGN KEY (queue_id) REFERENCES publish_queue(id) ON DELETE SET NULL,
  CONSTRAINT fk_dep_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT,
  CONSTRAINT fk_dep_site FOREIGN KEY (trigger_site_id) REFERENCES sites(id) ON DELETE SET NULL,
  CONSTRAINT fk_dep_task FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE SET NULL,
  CONSTRAINT fk_dep_user FOREIGN KEY (created_by_user_id) REFERENCES users(id) ON DELETE SET NULL
) ENGINE=InnoDB;

CREATE TABLE deployment_targets (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, deployment_id BIGINT UNSIGNED NOT NULL, node_id BIGINT UNSIGNED NOT NULL,
  attempt INT UNSIGNED NOT NULL DEFAULT 1, deadline_at DATETIME(3) NULL, state VARCHAR(16) NOT NULL DEFAULT 'pending',
  UNIQUE KEY uk_deployment_targets (deployment_id, node_id), UNIQUE KEY uk_deployment_targets_uid (uid), KEY idx_dt_deadline (state, deadline_at),
  CONSTRAINT ck_dt_state CHECK (state IN ('pending','notified','downloaded','verified','tested','applied','healthy','failed','rolled_back','skipped','waiting_offline','expired')),
  CONSTRAINT fk_dt_dep FOREIGN KEY (deployment_id) REFERENCES deployments(id) ON DELETE CASCADE,
  CONSTRAINT fk_dt_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE deployment_target_steps (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, deployment_target_id BIGINT UNSIGNED NOT NULL, attempt INT UNSIGNED NOT NULL, state VARCHAR(16) NOT NULL,
  UNIQUE KEY uk_dts (deployment_target_id, attempt, state),
  CONSTRAINT fk_dts_target FOREIGN KEY (deployment_target_id) REFERENCES deployment_targets(id) ON DELETE CASCADE
) ENGINE=InnoDB;

CREATE TABLE node_cluster_state (
  node_id BIGINT UNSIGNED NOT NULL, cluster_id BIGINT UNSIGNED NOT NULL, desired_revision_id BIGINT UNSIGNED NULL, applied_revision_id BIGINT UNSIGNED NULL,
  applied_lineage BIGINT UNSIGNED NULL, applied_seq BIGINT UNSIGNED NULL, held_seq BIGINT UNSIGNED NULL, state VARCHAR(16) NOT NULL DEFAULT 'idle',
  PRIMARY KEY (node_id, cluster_id), KEY idx_ncs_cluster (cluster_id, applied_seq),
  CONSTRAINT ck_ncs_state CHECK (state IN ('idle','applying','failed','rolled_back')),
  CONSTRAINT fk_ncs_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE CASCADE,
  CONSTRAINT fk_ncs_cluster FOREIGN KEY (cluster_id) REFERENCES clusters(id) ON DELETE CASCADE,
  CONSTRAINT fk_ncs_desired FOREIGN KEY (cluster_id, desired_revision_id) REFERENCES config_revisions(cluster_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_ncs_applied FOREIGN KEY (cluster_id, applied_revision_id) REFERENCES config_revisions(cluster_id, id) ON DELETE RESTRICT
) ENGINE=InnoDB;

-- 循环/前向外键在本迁移末尾追加（README §2.4）
ALTER TABLE publish_queue ADD CONSTRAINT fk_pq_deployment FOREIGN KEY (deployment_id) REFERENCES deployments(id) ON DELETE SET NULL;
ALTER TABLE clusters ADD CONSTRAINT fk_clusters_confirmed FOREIGN KEY (id, confirmed_revision_id) REFERENCES config_revisions(cluster_id, id) ON DELETE RESTRICT;
ALTER TABLE sites ADD CONSTRAINT fk_sites_last_deployment FOREIGN KEY (last_deployment_id) REFERENCES deployments(id) ON DELETE SET NULL;

-- ==== 000013 observability（高频，无外键）
CREATE TABLE metric_rollups (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, granularity VARCHAR(4) NOT NULL, window_start DATETIME(3) NOT NULL,
  dim_type VARCHAR(12) NOT NULL, dim_id BIGINT UNSIGNED NOT NULL DEFAULT 0, node_id BIGINT UNSIGNED NOT NULL DEFAULT 0, tenant_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  metric VARCHAR(48) NOT NULL, dim_sub VARCHAR(64) NOT NULL DEFAULT '', agg VARCHAR(4) NOT NULL DEFAULT 'sum', value BIGINT NOT NULL, generation INT UNSIGNED NOT NULL DEFAULT 0,
  UNIQUE KEY uk_metric_rollups (granularity, window_start, dim_type, dim_id, node_id, metric, dim_sub),
  KEY idx_metric_rollups_tenant (tenant_id, dim_type, dim_id, granularity, window_start)
) ENGINE=InnoDB;
CREATE TABLE rollup_windows (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, node_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  window_start DATETIME(3) NOT NULL, window_seconds INT UNSIGNED NOT NULL, generation INT UNSIGNED NOT NULL, content_sha256 CHAR(64) NOT NULL,
  UNIQUE KEY uk_rollup_windows (node_id, site_id, window_start, window_seconds)
) ENGINE=InnoDB;
CREATE TABLE access_log_chunks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL, node_id BIGINT UNSIGNED NOT NULL,
  log_date DATE NOT NULL, segment_seq BIGINT UNSIGNED NOT NULL, start_offset BIGINT UNSIGNED NOT NULL, end_offset BIGINT UNSIGNED NOT NULL,
  file_offset BIGINT UNSIGNED NOT NULL, stored_length BIGINT UNSIGNED NOT NULL, storage_path VARCHAR(512) NOT NULL, sha256 CHAR(64) NOT NULL,
  UNIQUE KEY uk_alc_dedupe (node_id, segment_seq, start_offset, end_offset, site_id, log_date),
  KEY idx_alc_query (tenant_id, site_id, log_date)
) ENGINE=InnoDB;

-- ==== 后续阶段 DDL 规则样例（P2/P5；不属于 M1 迁移，仅验证规则）
-- P2 notification/alert：open_key VARCHAR(255) STORED，基列 tenant_id FK RESTRICT
CREATE TABLE alert_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NULL, dedup_key VARCHAR(190) NOT NULL, status VARCHAR(12) NOT NULL DEFAULT 'firing',
  open_key VARCHAR(255) AS (IF(status='firing', CONCAT(COALESCE(tenant_id,0),':',dedup_key), NULL)) STORED,
  UNIQUE KEY uk_alert_events_open (open_key),
  CONSTRAINT fk_ae_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT
) ENGINE=InnoDB;
-- P2 provider_credentials 先建无 FK 的 plugin_id，P5 建 plugins 后 ALTER 追加
CREATE TABLE provider_credentials (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NULL, plugin_id BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_pc_uid (uid), CONSTRAINT fk_pc_tenant FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE RESTRICT) ENGINE=InnoDB;
CREATE TABLE plugins (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, plugin_key VARCHAR(96) NOT NULL, UNIQUE KEY uk_plugins_uid (uid), UNIQUE KEY uk_plugins_key (plugin_key)) ENGINE=InnoDB;
ALTER TABLE provider_credentials ADD CONSTRAINT fk_pc_plugin FOREIGN KEY (plugin_id) REFERENCES plugins(id) ON DELETE SET NULL;
-- P2 CMS 循环：website_pages ↔ website_page_versions
CREATE TABLE website_pages (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, page_key VARCHAR(96) NOT NULL, deleted_at DATETIME(3) NULL,
  status VARCHAR(12) NOT NULL DEFAULT 'draft', scheduled_publish_at DATETIME(3) NULL, scheduled_version_id BIGINT UNSIGNED NULL,
  current_version_id BIGINT UNSIGNED NULL, published_version_id BIGINT UNSIGNED NULL,
  active_key VARCHAR(96) AS (IF(deleted_at IS NULL, page_key, NULL)) VIRTUAL,
  UNIQUE KEY uk_website_pages_uid (uid), UNIQUE KEY uk_website_pages_key (active_key),
  CONSTRAINT ck_wp_sched CHECK (status<>'scheduled' OR scheduled_publish_at IS NOT NULL)) ENGINE=InnoDB;
CREATE TABLE website_page_versions (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, page_id BIGINT UNSIGNED NOT NULL, version_no INT UNSIGNED NOT NULL,
  UNIQUE KEY uk_wpv_uid (uid), UNIQUE KEY uk_website_page_versions (page_id, version_no),
  CONSTRAINT fk_wpv_page FOREIGN KEY (page_id) REFERENCES website_pages(id) ON DELETE RESTRICT) ENGINE=InnoDB;
ALTER TABLE website_pages
  ADD CONSTRAINT fk_wp_current FOREIGN KEY (current_version_id) REFERENCES website_page_versions(id) ON DELETE SET NULL,
  ADD CONSTRAINT fk_wp_published FOREIGN KEY (published_version_id) REFERENCES website_page_versions(id) ON DELETE SET NULL,
  ADD CONSTRAINT fk_wp_scheduled FOREIGN KEY (scheduled_version_id) REFERENCES website_page_versions(id) ON DELETE SET NULL;
-- P5 usage_records：无外键、唯一键含 window_seconds、出账后冻结触发器
CREATE TABLE usage_records (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL, node_id BIGINT UNSIGNED NOT NULL,
  metric VARCHAR(32) NOT NULL, window_start DATETIME(3) NOT NULL, window_seconds INT UNSIGNED NOT NULL, quantity BIGINT UNSIGNED NOT NULL,
  generation INT UNSIGNED NOT NULL DEFAULT 0, revision INT UNSIGNED NOT NULL DEFAULT 1, billed_invoice_id BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_usage_records (node_id, site_id, window_start, window_seconds, metric)
) ENGINE=InnoDB;
DELIMITER $$
CREATE TRIGGER trg_usage_records_bu BEFORE UPDATE ON usage_records FOR EACH ROW
BEGIN
  IF OLD.billed_invoice_id IS NOT NULL THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'usage_records frozen after billing'; END IF;
  IF NEW.node_id <> OLD.node_id OR NEW.site_id <> OLD.site_id OR NEW.window_start <> OLD.window_start OR NEW.window_seconds <> OLD.window_seconds
     OR NEW.metric <> OLD.metric OR NEW.tenant_id <> OLD.tenant_id THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'usage_records identity immutable'; END IF;
END$$
DELIMITER ;

SELECT 'M1 skeleton OK' AS result, COUNT(*) AS tables FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE();
