-- lingedge:class=expand
-- lingedge:summary=平台核心：系统设置、安装记录、任务与事件、任务/迁移租约、幂等键、事件 outbox（7 张表）
-- 依据 database/README §8.1，platform-identity.md §1、§4；users/tenants 已在 000001 创建，故本批外键均为后向引用。
-- tasks.type 的 CHECK 取值以 docs/api/openapi.yaml 的 TaskType 枚举为唯一事实源（新增类型走 expand：放宽 CHECK 后发布代码）。
-- job_locks 的 name/holder/fence/lease_until/acquired_at/updated_at 与迁移框架的租约记录（internal/platform/migrate/lock.go）对齐。
-- +goose Up

-- ---- system_settings：平台级键值配置（C1；平台级表）----
-- value_json（非敏感）与 value_enc（5 段信封密文，ADR-011）二选一。TEXT/JSON 列无 DEFAULT 字面量，应用层必须显式赋值。
CREATE TABLE IF NOT EXISTS system_settings (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  setting_key VARCHAR(128) NOT NULL,
  value_json JSON NULL,
  value_enc TEXT NULL,
  group_name VARCHAR(48) NOT NULL,
  updated_by BIGINT UNSIGNED NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_system_settings_uid (uid),
  UNIQUE KEY uk_system_settings_key (setting_key),
  KEY idx_system_settings_group (group_name),
  CONSTRAINT ck_system_settings_one_value CHECK ((value_json IS NULL) <> (value_enc IS NULL)),
  CONSTRAINT fk_system_settings_users FOREIGN KEY (updated_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON system_settings TO lingedge_app;

-- ---- system_installations：安装状态镜像（C1；Tools 本地 state.json 为事实源）----
CREATE TABLE IF NOT EXISTS system_installations (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  installation_id CHAR(26) NOT NULL,
  component VARCHAR(32) NOT NULL,
  instance_name VARCHAR(64) NOT NULL,
  host VARCHAR(255) NOT NULL,
  version VARCHAR(64) NOT NULL,
  channel VARCHAR(16) NOT NULL DEFAULT 'stable',
  status VARCHAR(16) NOT NULL DEFAULT 'installed',
  installed_at DATETIME(3) NOT NULL,
  last_upgraded_at DATETIME(3) NULL,
  install_locked_at DATETIME(3) NULL,
  meta_json JSON NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_system_installations_uid (uid),
  UNIQUE KEY uk_system_installations_inst (component, host, instance_name),
  KEY idx_system_installations_install (installation_id),
  CONSTRAINT ck_system_installations_status CHECK (status IN ('installed', 'upgrading', 'failed', 'uninstalled'))
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON system_installations TO lingedge_app;

-- ---- tasks：异步 Task 与 Worker 队列（C1 + T?；逐节点子任务在 node_tasks，不在本表）----
CREATE TABLE IF NOT EXISTS tasks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NULL,
  type VARCHAR(64) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  priority SMALLINT NOT NULL DEFAULT 100,
  payload_json JSON NOT NULL,
  result_json JSON NULL,
  error_code VARCHAR(96) NULL,
  error_message VARCHAR(1024) NULL,
  progress TINYINT UNSIGNED NOT NULL DEFAULT 0,
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  max_attempts INT UNSIGNED NOT NULL DEFAULT 5,
  run_after DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  lease_owner VARCHAR(128) NULL,
  lease_until DATETIME(3) NULL,
  fence BIGINT UNSIGNED NOT NULL DEFAULT 0,
  idempotency_key VARCHAR(128) NULL,
  parent_task_id BIGINT UNSIGNED NULL,
  request_id CHAR(26) NOT NULL,
  created_by_user_id BIGINT UNSIGNED NULL,
  cancel_requested TINYINT(1) NOT NULL DEFAULT 0,
  started_at DATETIME(3) NULL,
  finished_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_tasks_uid (uid),
  UNIQUE KEY uk_tasks_idem (type, idempotency_key),
  KEY idx_tasks_claim (status, run_after, priority),
  KEY idx_tasks_lease (status, lease_until),
  KEY idx_tasks_tenant (tenant_id, created_at),
  KEY idx_tasks_parent (parent_task_id),
  CONSTRAINT ck_tasks_status CHECK (status IN ('pending', 'running', 'success', 'partial', 'failed', 'canceled')),
  CONSTRAINT ck_tasks_type CHECK (type IN (
    'deployment.publish', 'deployment.retry', 'publish.dispatch',
    'cache.purge_url', 'cache.purge_site', 'cache.prefetch', 'cache.purge_prefix',
    'cert.issue', 'cert.renew', 'cert.distribute',
    'node.upgrade', 'node.rollback', 'realip.debug',
    'system.backup', 'dns.sync', 'retention.cleanup', 'rollup.aggregate', 'usage.derive', 'heartbeat.sweep',
    'secrets.rewrap', 'server.cert_rotate', 'media.cleanup_orphans',
    'cms.revalidate', 'cms.publish_due', 'cms.prerender')),
  CONSTRAINT ck_tasks_progress CHECK (progress <= 100),
  CONSTRAINT fk_tasks_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_tasks_parent FOREIGN KEY (parent_task_id) REFERENCES tasks (id) ON DELETE CASCADE,
  CONSTRAINT fk_tasks_users FOREIGN KEY (created_by_user_id) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON tasks TO lingedge_app;

-- ---- task_events：任务过程事件（C2u：对外只暴露 uid）----
CREATE TABLE IF NOT EXISTS task_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  task_id BIGINT UNSIGNED NOT NULL,
  level VARCHAR(8) NOT NULL,
  message VARCHAR(1024) NOT NULL,
  data_json JSON NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_task_events_uid (uid),
  KEY idx_task_events_task (task_id, id),
  CONSTRAINT ck_task_events_level CHECK (level IN ('info', 'warn', 'error')),
  CONSTRAINT fk_task_events_tasks FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE CASCADE
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON task_events TO lingedge_app;

-- ---- job_locks：周期任务/单例任务租约（ADR-019；平台级；name 为主键）----
CREATE TABLE IF NOT EXISTS job_locks (
  name VARCHAR(128) NOT NULL,
  holder VARCHAR(128) NOT NULL,
  fence BIGINT UNSIGNED NOT NULL DEFAULT 0,
  lease_until DATETIME(3) NOT NULL,
  acquired_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  PRIMARY KEY (name)
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON job_locks TO lingedge_app;

-- ---- idempotency_keys：幂等键（C2；保留 24 小时；response_status/response_body 在请求完成后才写入）----
CREATE TABLE IF NOT EXISTS idempotency_keys (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  principal_key VARCHAR(64) NOT NULL,
  idem_key VARCHAR(128) NOT NULL,
  method VARCHAR(8) NOT NULL,
  path VARCHAR(255) NOT NULL,
  request_hash CHAR(64) NOT NULL,
  response_status SMALLINT NULL,
  response_body MEDIUMBLOB NULL,
  task_id BIGINT UNSIGNED NULL,
  expires_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_idempotency_keys (principal_key, idem_key),
  KEY idx_idempotency_keys_exp (expires_at),
  CONSTRAINT fk_idempotency_keys_tasks FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO lingedge_app;

-- ---- event_outbox：需可靠投递的进程内事件（C2；无外键，已发送保留 7 天）----
CREATE TABLE IF NOT EXISTS event_outbox (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  event_type VARCHAR(96) NOT NULL,
  aggregate_type VARCHAR(48) NOT NULL,
  aggregate_uid VARCHAR(64) NOT NULL,
  tenant_id BIGINT UNSIGNED NULL,
  payload_json JSON NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  available_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  sent_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  KEY idx_event_outbox_poll (status, available_at),
  CONSTRAINT ck_event_outbox_status CHECK (status IN ('pending', 'sent', 'failed'))
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON event_outbox TO lingedge_app;
