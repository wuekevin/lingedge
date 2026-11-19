-- lingedge:class=expand
-- lingedge:summary=身份与租户核心：用户、租户、成员、配额、权限字典、角色、会话、API Token、登录日志（12 张表）
-- 依据 database/README §2、§8.1，platform-identity.md §2，tenant-site.md §1；外键按拓扑排序（users → tenants → …），无前向外键。
-- 约定：所有指向 tenants(id) 的外键一律 RESTRICT；T? 表用 VIRTUAL 生成列保证平台级唯一；邮箱/slug 应用层小写化 + CHECK 兜底；枚举用 VARCHAR+CHECK。
-- 每张表建成后紧跟 lingedge_app 的逐表授权（库级授权无法在表级收窄，database/README §5.1）。
-- +goose Up

-- ---- users：全局身份（平台管理员与租户用户同表；C1 + SD）----
CREATE TABLE IF NOT EXISTS users (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  user_type VARCHAR(16) NOT NULL,
  email VARCHAR(255) NOT NULL,
  active_email VARCHAR(255) AS (IF(deleted_at IS NULL, email, NULL)) STORED,
  display_name VARCHAR(128) NOT NULL,
  password_hash VARCHAR(255) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  locale VARCHAR(8) NOT NULL DEFAULT 'zh-CN',
  timezone VARCHAR(48) NOT NULL DEFAULT 'UTC',
  totp_enabled TINYINT(1) NOT NULL DEFAULT 0,
  totp_secret_enc TEXT NULL,
  must_change_password TINYINT(1) NOT NULL DEFAULT 0,
  password_changed_at DATETIME(3) NULL,
  failed_login_count INT UNSIGNED NOT NULL DEFAULT 0,
  locked_until DATETIME(3) NULL,
  last_login_at DATETIME(3) NULL,
  last_login_ip VARBINARY(16) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_users_uid (uid),
  UNIQUE KEY uk_users_active_email (active_email),
  KEY idx_users_type_status (user_type, status),
  CONSTRAINT ck_users_user_type CHECK (user_type IN ('platform', 'tenant')),
  CONSTRAINT ck_users_status CHECK (status IN ('active', 'disabled', 'locked', 'pending')),
  CONSTRAINT ck_users_totp CHECK (totp_enabled = 0 OR totp_secret_enc IS NOT NULL),
  CONSTRAINT ck_users_email_lower CHECK (BINARY email = BINARY LOWER(email))
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON users TO lingedge_app;

-- ---- tenants：资源与数据隔离的最小单位（平台级表；C1 + SD）----
-- reseller_id 先建为无 FK 的普通列：resellers 表 P5 才创建，届时 ALTER 追加外键（README §2.4 前向引用清单）。
CREATE TABLE IF NOT EXISTS tenants (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  name VARCHAR(128) NOT NULL,
  slug VARCHAR(63) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  is_default TINYINT(1) NOT NULL DEFAULT 0,
  default_flag TINYINT AS (IF(is_default = 1, 1, NULL)) STORED,
  owner_user_id BIGINT UNSIGNED NULL,
  reseller_id BIGINT UNSIGNED NULL,
  contact_email VARCHAR(255) NULL,
  locale VARCHAR(8) NOT NULL DEFAULT 'zh-CN',
  settings_json JSON NULL,
  suspended_reason VARCHAR(255) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_tenants_uid (uid),
  UNIQUE KEY uk_tenants_slug (slug),
  UNIQUE KEY uk_tenants_default (default_flag),
  KEY idx_tenants_status (status),
  KEY idx_tenants_reseller (reseller_id),
  CONSTRAINT ck_tenants_status CHECK (status IN ('active', 'suspended', 'closed', 'pending')),
  CONSTRAINT ck_tenants_slug CHECK (REGEXP_LIKE(slug, '^[a-z0-9][a-z0-9-]{0,62}$', 'c')),
  CONSTRAINT fk_tenants_users FOREIGN KEY (owner_user_id) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenants TO lingedge_app;

-- ---- user_recovery_codes：TOTP 一次性恢复码（C2）----
CREATE TABLE IF NOT EXISTS user_recovery_codes (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  user_id BIGINT UNSIGNED NOT NULL,
  code_hash CHAR(64) NOT NULL,
  used_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_user_recovery_codes (user_id, code_hash),
  CONSTRAINT fk_user_recovery_codes_users FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_recovery_codes TO lingedge_app;

-- ---- tenant_members：用户在租户中的成员关系（C2 + T）----
-- 租户表：所有索引以 tenant_id 打头，唯一例外 idx_tenant_members_user（登录后按用户反查所属租户，是平台级跨租户查询，同时支撑 fk_tenant_members_users）。
-- lingedge:cross-tenant-index=idx_tenant_members_user 登录后按用户反查所属租户（跨租户查询，同时支撑外键 fk_tenant_members_users）
CREATE TABLE IF NOT EXISTS tenant_members (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  is_owner TINYINT(1) NOT NULL DEFAULT 0,
  invited_by BIGINT UNSIGNED NULL,
  joined_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_tenant_members (tenant_id, user_id),
  KEY idx_tenant_members_user (user_id),
  CONSTRAINT ck_tenant_members_status CHECK (status IN ('active', 'invited', 'removed')),
  CONSTRAINT fk_tenant_members_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_tenant_members_users FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
  CONSTRAINT fk_tenant_members_inviter FOREIGN KEY (invited_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_members TO lingedge_app;

-- ---- tenant_quotas：租户配额（C2 + updated_at + T；M1 默认租户无限制 = limit_value NULL）----
-- subscription_id 先建为无 FK 的普通列：subscriptions 表 P5 才创建。
CREATE TABLE IF NOT EXISTS tenant_quotas (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  quota_key VARCHAR(64) NOT NULL,
  limit_value BIGINT NULL,
  source VARCHAR(16) NOT NULL DEFAULT 'manual',
  subscription_id BIGINT UNSIGNED NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_tenant_quotas (tenant_id, quota_key),
  CONSTRAINT ck_tenant_quotas_limit CHECK (limit_value IS NULL OR limit_value >= 0),
  CONSTRAINT ck_tenant_quotas_source CHECK (source IN ('manual', 'plan', 'default')),
  CONSTRAINT fk_tenant_quotas_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_quotas TO lingedge_app;

-- ---- permissions：权限点字典（平台级，无 uid；内容由种子迁移 000004 写入）----
CREATE TABLE IF NOT EXISTS permissions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  code VARCHAR(96) NOT NULL,
  resource VARCHAR(48) NOT NULL,
  action VARCHAR(48) NOT NULL,
  scope VARCHAR(16) NOT NULL,
  module VARCHAR(32) NOT NULL,
  menu_key VARCHAR(64) NULL,
  description VARCHAR(255) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_permissions_code (code),
  CONSTRAINT ck_permissions_scope CHECK (scope IN ('platform', 'tenant', 'both')),
  CONSTRAINT ck_permissions_code CHECK (code = CONCAT(resource, ':', action))
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON permissions TO lingedge_app;

-- ---- roles：角色（C1 + T?：tenant_id NULL = 平台内置/平台定义；非空 = 租户自定义，P5）----
-- uniq_key 是 VIRTUAL 生成列：STORED 在 tenant_id 外键为 CASCADE 时 ERROR 1215（实测，README §9 第 1 行）；CHECK 引用 FK 列时外键只能 RESTRICT/CASCADE。
CREATE TABLE IF NOT EXISTS roles (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NULL,
  scope VARCHAR(16) NOT NULL,
  code VARCHAR(64) NOT NULL,
  name VARCHAR(128) NOT NULL,
  description VARCHAR(512) NULL,
  is_builtin TINYINT(1) NOT NULL DEFAULT 0,
  uniq_key VARCHAR(160) AS (CONCAT(scope, ':', COALESCE(tenant_id, 0), ':', code)) VIRTUAL NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_roles_uid (uid),
  UNIQUE KEY uk_roles_uniq (uniq_key),
  CONSTRAINT ck_roles_scope CHECK (scope IN ('platform', 'tenant')),
  CONSTRAINT ck_roles_platform_scope CHECK (scope <> 'platform' OR tenant_id IS NULL),
  CONSTRAINT fk_roles_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON roles TO lingedge_app;

-- ---- role_permissions：角色-权限（C2）----
CREATE TABLE IF NOT EXISTS role_permissions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  role_id BIGINT UNSIGNED NOT NULL,
  permission_id BIGINT UNSIGNED NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_role_permissions (role_id, permission_id),
  CONSTRAINT fk_role_permissions_roles FOREIGN KEY (role_id) REFERENCES roles (id) ON DELETE CASCADE,
  CONSTRAINT fk_role_permissions_permissions FOREIGN KEY (permission_id) REFERENCES permissions (id) ON DELETE CASCADE
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON role_permissions TO lingedge_app;

-- ---- user_roles：用户-角色授予（C2 + T?；tenant_key 保证平台级授予也唯一）----
CREATE TABLE IF NOT EXISTS user_roles (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  user_id BIGINT UNSIGNED NOT NULL,
  role_id BIGINT UNSIGNED NOT NULL,
  tenant_id BIGINT UNSIGNED NULL,
  tenant_key BIGINT UNSIGNED AS (COALESCE(tenant_id, 0)) VIRTUAL NOT NULL,
  granted_by BIGINT UNSIGNED NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_user_roles (user_id, role_id, tenant_key),
  KEY idx_user_roles_tenant (tenant_id, user_id),
  CONSTRAINT fk_user_roles_users FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
  CONSTRAINT fk_user_roles_roles FOREIGN KEY (role_id) REFERENCES roles (id) ON DELETE CASCADE,
  CONSTRAINT fk_user_roles_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_user_roles_granter FOREIGN KEY (granted_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_roles TO lingedge_app;

-- ---- sessions：Cookie 会话（ADR-018；C2，会话标识为令牌哈希）----
CREATE TABLE IF NOT EXISTS sessions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  token_hash CHAR(64) NOT NULL,
  csrf_hash CHAR(64) NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  realm VARCHAR(16) NOT NULL,
  tenant_id BIGINT UNSIGNED NULL,
  ip VARBINARY(16) NULL,
  user_agent VARCHAR(512) NULL,
  mfa_verified_at DATETIME(3) NULL,
  reauth_at DATETIME(3) NULL,
  last_seen_at DATETIME(3) NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  absolute_expires_at DATETIME(3) NOT NULL,
  revoked_at DATETIME(3) NULL,
  revoke_reason VARCHAR(32) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_sessions_token (token_hash),
  KEY idx_sessions_user (user_id, revoked_at),
  KEY idx_sessions_exp (expires_at),
  CONSTRAINT ck_sessions_realm CHECK (realm IN ('admin', 'console')),
  CONSTRAINT fk_sessions_users FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
  CONSTRAINT fk_sessions_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON sessions TO lingedge_app;

-- ---- api_tokens：机器访问令牌（C1 + T?：NULL = 平台令牌）----
CREATE TABLE IF NOT EXISTS api_tokens (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(128) NOT NULL,
  token_prefix CHAR(8) NOT NULL,
  token_hash CHAR(64) NOT NULL,
  scopes_json JSON NOT NULL,
  allowed_cidrs_json JSON NULL,
  expires_at DATETIME(3) NULL,
  last_used_at DATETIME(3) NULL,
  last_used_ip VARBINARY(16) NULL,
  revoked_at DATETIME(3) NULL,
  rotated_from_id BIGINT UNSIGNED NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_api_tokens_uid (uid),
  UNIQUE KEY uk_api_tokens_prefix (token_prefix),
  UNIQUE KEY uk_api_tokens_hash (token_hash),
  KEY idx_api_tokens_tenant (tenant_id, user_id),
  CONSTRAINT fk_api_tokens_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_api_tokens_users FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
  CONSTRAINT fk_api_tokens_rotated_from FOREIGN KEY (rotated_from_id) REFERENCES api_tokens (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON api_tokens TO lingedge_app;

-- ---- login_logs：登录事件（FR-17；C2u：对外只暴露 uid；高频，无外键，引用完整性由写入路径与清理任务保证）----
CREATE TABLE IF NOT EXISTS login_logs (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  user_id BIGINT UNSIGNED NULL,
  tenant_id BIGINT UNSIGNED NULL,
  realm VARCHAR(16) NOT NULL,
  identifier VARCHAR(255) NOT NULL,
  event VARCHAR(24) NOT NULL,
  ip VARBINARY(16) NULL,
  user_agent VARCHAR(512) NULL,
  request_id CHAR(26) NULL,
  reason VARCHAR(64) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_login_logs_uid (uid),
  KEY idx_login_logs_user (user_id, created_at),
  KEY idx_login_logs_ip (ip, created_at),
  KEY idx_login_logs_time (created_at)
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON login_logs TO lingedge_app;
