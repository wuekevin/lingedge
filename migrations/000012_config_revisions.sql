-- lingedge:class=expand
-- lingedge:summary=配置修订与发布：配置块、修订（不可变）、修订-块清单、站点版本、发布队列、部署、部署目标及其步骤、节点集群状态（9 张）；末尾追加循环外键
-- 依据 database/README §2.2、§2.4、§5、§8.1，node-deploy.md §2–§3；ADR-002（修订/lineage/held_seq）、ADR-009、ADR-020。
-- 块内容存 MySQL（config_blocks.content，随数据库逻辑备份，无独立修订 blob 文件；IMP-16/L20）；manifest 的 blocks[]={key,sha256,size}，config_revision_blocks.block_key ∈ global|site:<uid>|l4 与之一一对应。
-- 不可变（README §5）：config_blocks/config_revisions 任何 UPDATE 一律拒绝，DELETE 仅清理任务（@lingedge_purge=1）可执行；config_revision_blocks 拒绝 UPDATE（随修订级联清理）。
-- 触发器创建需迁移账号具 SET_USER_ID 或 log_bin_trust_function_creators=1（同 1.7/1.8）；级联/SET NULL 的外键动作不触发触发器，故不影响清理与审计型 SET NULL 列。
-- 不可变表对 lingedge_app 仍按框架 lint 授 SELECT/INSERT/UPDATE/DELETE（清理任务经 app 连接 DELETE 需要 DELETE；与 rule_versions 同）：不可变性由 BEFORE UPDATE/DELETE 触发器保证，
-- 对 root 与 app 均生效（单测覆盖）；Repository 不提供 UPDATE/DELETE 方法是第一道防线（README §5）。
-- 外键纪律：指向 tenants(id) 一律 RESTRICT；被 CHECK 引用的外键列不得 SET NULL（ERROR 3823）；STORED 生成列 publish_queue.queued_key 的基列 site_id 外键为 RESTRICT。
-- 站点子表一律复合外键 (tenant_id, site_id) → sites(tenant_id, id)。publish_queue 的 tenant_id/site_id 可为 NULL（cluster_rollback），MATCH SIMPLE 下 NULL 绕过校验，
-- 故 ck_publish_queue_site_tenant 要求有站点必有租户。
-- 修订与集群一致性（评审加固）：config_revisions 增 UNIQUE(cluster_id, id)，rollback_of、deployments.revision_id、node_cluster_state.desired/applied、clusters.confirmed_revision_id
-- 均用复合外键 (cluster_id, <rev>) → config_revisions(cluster_id, id)，使跨集群引用修订在库层不可能。
-- 循环外键在本迁移末尾以带 information_schema 守卫的 ALTER TABLE ADD CONSTRAINT 追加（可重入）：
--   publish_queue.deployment_id → deployments、clusters.confirmed_revision_id → config_revisions、sites.last_deployment_id → deployments。
-- 租户表索引以 tenant_id 打头；跨租户/外键支撑的索引按 README §2.6 显式登记豁免（原因必填）。
-- +goose Up

-- ---- config_blocks：内容寻址的配置块（C2；不可变；site 块带 site_uid）----
-- lingedge:cross-tenant-index=idx_config_blocks_site 站点块按 site_uid 查找（tenant_id 可空，平台级块无租户）
CREATE TABLE IF NOT EXISTS config_blocks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  sha256 CHAR(64) NOT NULL,
  kind VARCHAR(16) NOT NULL,
  site_uid CHAR(26) NULL,
  tenant_id BIGINT UNSIGNED NULL,
  schema_version INT UNSIGNED NOT NULL,
  encoding VARCHAR(8) NOT NULL DEFAULT 'zstd',
  size_raw BIGINT UNSIGNED NOT NULL,
  size_stored BIGINT UNSIGNED NOT NULL,
  content MEDIUMBLOB NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_config_blocks_sha (sha256),
  KEY idx_config_blocks_site (site_uid),
  KEY idx_config_blocks_tenant (tenant_id),
  CONSTRAINT ck_config_blocks_kind CHECK (kind IN ('site', 'global', 'l4')),
  CONSTRAINT ck_config_blocks_encoding CHECK (encoding IN ('zstd', 'identity')),
  CONSTRAINT ck_config_blocks_site_uid CHECK ((kind = 'site') = (site_uid IS NOT NULL) AND (site_uid IS NULL OR CHAR_LENGTH(site_uid) = 26)),
  CONSTRAINT fk_config_blocks_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON config_blocks TO lingedge_app;

DROP TRIGGER IF EXISTS trg_config_blocks_bu;
CREATE TRIGGER trg_config_blocks_bu BEFORE UPDATE ON config_blocks FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'config_blocks is immutable';

DROP TRIGGER IF EXISTS trg_config_blocks_bd;
-- +goose StatementBegin
CREATE TRIGGER trg_config_blocks_bd BEFORE DELETE ON config_blocks
FOR EACH ROW
BEGIN
  IF COALESCE(@lingedge_purge, 0) <> 1 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'config_blocks delete forbidden outside purge';
  END IF;
END;
-- +goose StatementEnd

-- ---- config_revisions：集群内某序号的完整配置快照元数据与签名（C1 的 id/uid/created_at，无 updated_at；不可变；ADR-002）----
CREATE TABLE IF NOT EXISTS config_revisions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  cluster_id BIGINT UNSIGNED NOT NULL,
  lineage BIGINT UNSIGNED NOT NULL,
  seq BIGINT UNSIGNED NOT NULL,
  parent_seq BIGINT UNSIGNED NULL,
  kind VARCHAR(16) NOT NULL,
  schema_version INT UNSIGNED NOT NULL,
  manifest_canonical MEDIUMBLOB NOT NULL,
  manifest_sha256 CHAR(64) NOT NULL,
  signature VARBINARY(64) NOT NULL,
  key_id VARCHAR(32) NOT NULL,
  block_count INT UNSIGNED NOT NULL,
  total_size BIGINT UNSIGNED NOT NULL,
  rollback_of_revision_id BIGINT UNSIGNED NULL,
  trigger_site_id BIGINT UNSIGNED NULL,
  change_note VARCHAR(512) NULL,
  created_by_user_id BIGINT UNSIGNED NULL,
  created_by_task_id BIGINT UNSIGNED NULL,
  request_id CHAR(26) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_config_revisions_uid (uid),
  UNIQUE KEY uk_config_revisions_cluster_id (cluster_id, id),
  UNIQUE KEY uk_config_revisions_seq (cluster_id, lineage, seq),
  UNIQUE KEY uk_config_revisions_manifest (cluster_id, manifest_sha256),
  KEY idx_config_revisions_cluster_time (cluster_id, created_at),
  KEY idx_config_revisions_site (trigger_site_id, created_at),
  KEY idx_config_revisions_rollback (cluster_id, rollback_of_revision_id),
  KEY idx_config_revisions_user (created_by_user_id),
  KEY idx_config_revisions_task (created_by_task_id),
  CONSTRAINT ck_config_revisions_kind CHECK (kind IN ('publish', 'rollback', 'system')),
  CONSTRAINT ck_config_revisions_seq CHECK (seq >= 1),
  CONSTRAINT ck_config_revisions_rollback CHECK ((kind = 'rollback') = (rollback_of_revision_id IS NOT NULL)),
  CONSTRAINT ck_config_revisions_parent CHECK (parent_seq IS NULL OR parent_seq < seq),
  CONSTRAINT fk_config_revisions_clusters FOREIGN KEY (cluster_id) REFERENCES clusters (id) ON DELETE RESTRICT,
  CONSTRAINT fk_config_revisions_rollback FOREIGN KEY (cluster_id, rollback_of_revision_id) REFERENCES config_revisions (cluster_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_config_revisions_sites FOREIGN KEY (trigger_site_id) REFERENCES sites (id) ON DELETE SET NULL,
  CONSTRAINT fk_config_revisions_users FOREIGN KEY (created_by_user_id) REFERENCES users (id) ON DELETE SET NULL,
  CONSTRAINT fk_config_revisions_tasks FOREIGN KEY (created_by_task_id) REFERENCES tasks (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON config_revisions TO lingedge_app;

DROP TRIGGER IF EXISTS trg_config_revisions_bu;
CREATE TRIGGER trg_config_revisions_bu BEFORE UPDATE ON config_revisions FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'config_revisions is immutable';

DROP TRIGGER IF EXISTS trg_config_revisions_bd;
-- +goose StatementBegin
CREATE TRIGGER trg_config_revisions_bd BEFORE DELETE ON config_revisions
FOR EACH ROW
BEGIN
  IF COALESCE(@lingedge_purge, 0) <> 1 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'config_revisions delete forbidden outside purge';
  END IF;
END;
-- +goose StatementEnd

-- ---- config_revision_blocks：修订 ↔ 块的组成清单（C2；copy-forward 复用块；不可 UPDATE）----
CREATE TABLE IF NOT EXISTS config_revision_blocks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  revision_id BIGINT UNSIGNED NOT NULL,
  block_id BIGINT UNSIGNED NOT NULL,
  block_key VARCHAR(40) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_config_revision_blocks (revision_id, block_key),
  KEY idx_config_revision_blocks_block (block_id),
  CONSTRAINT ck_config_revision_blocks_key CHECK (block_key IN ('global', 'l4') OR (block_key LIKE 'site:%' AND CHAR_LENGTH(block_key) = 31)),
  CONSTRAINT fk_config_revision_blocks_revisions FOREIGN KEY (revision_id) REFERENCES config_revisions (id) ON DELETE CASCADE,
  CONSTRAINT fk_config_revision_blocks_blocks FOREIGN KEY (block_id) REFERENCES config_blocks (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON config_revision_blocks TO lingedge_app;

DROP TRIGGER IF EXISTS trg_config_revision_blocks_bu;
CREATE TRIGGER trg_config_revision_blocks_bu BEFORE UPDATE ON config_revision_blocks FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'config_revision_blocks is immutable';

-- ---- site_versions：站点线性版本（C1 + T；站点内单调 version_no，MIN-14）----
-- lingedge:cross-tenant-index=uk_site_versions 站点内版本号单调唯一（site_id 已隐含租户，复合外键保证 tenant 一致）
-- lingedge:cross-tenant-index=idx_site_versions_block 按块反查引用（块引用计数/清理，同时支撑外键 fk_site_versions_blocks）
-- lingedge:cross-tenant-index=idx_site_versions_user 支撑外键 fk_site_versions_users（created_by SET NULL 的父行删除检查）
CREATE TABLE IF NOT EXISTS site_versions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL,
  version_no BIGINT UNSIGNED NOT NULL,
  kind VARCHAR(16) NOT NULL DEFAULT 'publish',
  rollback_of_version_no BIGINT UNSIGNED NULL,
  block_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  change_note VARCHAR(512) NULL,
  created_by BIGINT UNSIGNED NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_site_versions_uid (uid),
  UNIQUE KEY uk_site_versions (site_id, version_no),
  KEY idx_site_versions_tenant (tenant_id, site_id, created_at),
  KEY idx_site_versions_block (block_id),
  KEY idx_site_versions_user (created_by),
  CONSTRAINT ck_site_versions_version CHECK (version_no >= 1),
  CONSTRAINT ck_site_versions_kind CHECK (kind IN ('publish', 'rollback')),
  CONSTRAINT ck_site_versions_status CHECK (status IN ('pending', 'live', 'failed', 'superseded')),
  CONSTRAINT ck_site_versions_rollback CHECK ((kind = 'rollback') = (rollback_of_version_no IS NOT NULL)),
  CONSTRAINT fk_site_versions_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_site_versions_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_site_versions_blocks FOREIGN KEY (block_id) REFERENCES config_blocks (id) ON DELETE RESTRICT,
  CONSTRAINT fk_site_versions_users FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON site_versions TO lingedge_app;

-- ---- publish_queue：每集群串行发布队列（C1 + T?；ADR-002 §13，D-PUB）----
-- deployment_id 先建为无 FK 普通列（publish_queue ↔ deployments 循环），本迁移末尾追加外键。
-- queued_key = IF(status='queued', COALESCE(site_id, 0), NULL) STORED + 唯一 → 同站点同集群至多一个排队项；cluster_rollback（site_id NULL）记为 0，每集群至多一个排队中的集群回滚。
CREATE TABLE IF NOT EXISTS publish_queue (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NULL,
  cluster_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NULL,
  kind VARCHAR(20) NOT NULL,
  expected_config_version BIGINT UNSIGNED NULL,
  target_version_no BIGINT UNSIGNED NULL,
  change_note VARCHAR(512) NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'queued',
  queued_key BIGINT UNSIGNED AS (IF(status = 'queued', COALESCE(site_id, 0), NULL)) STORED,
  deployment_id BIGINT UNSIGNED NULL,
  site_version_id BIGINT UNSIGNED NULL,
  task_id BIGINT UNSIGNED NULL,
  requested_by BIGINT UNSIGNED NULL,
  error_code VARCHAR(96) NULL,
  started_at DATETIME(3) NULL,
  finished_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_publish_queue_uid (uid),
  UNIQUE KEY uk_publish_queue_queued (cluster_id, queued_key),
  KEY idx_pq_cluster_status (cluster_id, status, id),
  KEY idx_pq_site (tenant_id, site_id, created_at),
  KEY idx_pq_deployment (deployment_id),
  KEY idx_pq_site_version (site_version_id),
  KEY idx_pq_task (task_id),
  KEY idx_pq_user (requested_by),
  CONSTRAINT ck_publish_queue_kind CHECK (kind IN ('site_publish', 'site_rollback', 'cluster_rollback')),
  CONSTRAINT ck_publish_queue_status CHECK (status IN ('queued', 'building', 'dispatched', 'superseded', 'canceled', 'failed')),
  CONSTRAINT ck_publish_queue_site CHECK ((kind = 'cluster_rollback') = (site_id IS NULL)),
  CONSTRAINT ck_publish_queue_site_tenant CHECK (site_id IS NULL OR tenant_id IS NOT NULL),
  CONSTRAINT fk_publish_queue_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_publish_queue_clusters FOREIGN KEY (cluster_id) REFERENCES clusters (id) ON DELETE RESTRICT,
  CONSTRAINT fk_publish_queue_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_publish_queue_site_versions FOREIGN KEY (site_version_id) REFERENCES site_versions (id) ON DELETE SET NULL,
  CONSTRAINT fk_publish_queue_tasks FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE SET NULL,
  CONSTRAINT fk_publish_queue_users FOREIGN KEY (requested_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON publish_queue TO lingedge_app;

-- ---- deployments：把某集群的某修订推送到一批节点的发布动作（C1 + T?；FR-55/56）----
-- queue_id → publish_queue 直接带 FK（publish_queue 已建）；deployments.trigger_site_id 为 SET NULL，不能与 tenant_id 组成复合外键（见开发记录）。
CREATE TABLE IF NOT EXISTS deployments (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  cluster_id BIGINT UNSIGNED NOT NULL,
  revision_id BIGINT UNSIGNED NOT NULL,
  kind VARCHAR(16) NOT NULL DEFAULT 'publish',
  queue_id BIGINT UNSIGNED NULL,
  tenant_id BIGINT UNSIGNED NULL,
  trigger_site_id BIGINT UNSIGNED NULL,
  strategy VARCHAR(16) NOT NULL DEFAULT 'all',
  batch_plan_json JSON NULL,
  current_batch INT UNSIGNED NOT NULL DEFAULT 0,
  max_failures INT UNSIGNED NOT NULL DEFAULT 0,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  task_id BIGINT UNSIGNED NULL,
  started_at DATETIME(3) NULL,
  finished_at DATETIME(3) NULL,
  error_summary VARCHAR(1024) NULL,
  created_by_user_id BIGINT UNSIGNED NULL,
  request_id CHAR(26) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_deployments_uid (uid),
  KEY idx_deployments_cluster (cluster_id, created_at),
  KEY idx_deployments_site (tenant_id, trigger_site_id, created_at),
  KEY idx_deployments_status (status),
  KEY idx_deployments_revision (cluster_id, revision_id),
  KEY idx_deployments_queue (queue_id),
  KEY idx_deployments_trigger_site (trigger_site_id),
  KEY idx_deployments_task (task_id),
  KEY idx_deployments_user (created_by_user_id),
  CONSTRAINT ck_deployments_kind CHECK (kind IN ('publish', 'rollback')),
  CONSTRAINT ck_deployments_strategy CHECK (strategy IN ('all', 'canary', 'batch', 'single')),
  CONSTRAINT ck_deployments_status CHECK (status IN ('pending', 'running', 'paused', 'success', 'partial_failed', 'failed', 'canceled', 'rolled_back')),
  CONSTRAINT fk_deployments_clusters FOREIGN KEY (cluster_id) REFERENCES clusters (id) ON DELETE RESTRICT,
  CONSTRAINT fk_deployments_revisions FOREIGN KEY (cluster_id, revision_id) REFERENCES config_revisions (cluster_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_deployments_queue FOREIGN KEY (queue_id) REFERENCES publish_queue (id) ON DELETE SET NULL,
  CONSTRAINT fk_deployments_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_deployments_sites FOREIGN KEY (trigger_site_id) REFERENCES sites (id) ON DELETE SET NULL,
  CONSTRAINT fk_deployments_tasks FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE SET NULL,
  CONSTRAINT fk_deployments_users FOREIGN KEY (created_by_user_id) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON deployments TO lingedge_app;

-- ---- deployment_targets：部署在每个节点上的逐步状态机（C2u；uid = proto DesiredState.deployment_target_uid）----
CREATE TABLE IF NOT EXISTS deployment_targets (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deployment_id BIGINT UNSIGNED NOT NULL,
  node_id BIGINT UNSIGNED NOT NULL,
  batch_no INT UNSIGNED NOT NULL DEFAULT 0,
  attempt INT UNSIGNED NOT NULL DEFAULT 1,
  deadline_at DATETIME(3) NULL,
  state VARCHAR(16) NOT NULL DEFAULT 'pending',
  failure_stage VARCHAR(16) NULL,
  applied_seq BIGINT UNSIGNED NULL,
  rolled_back TINYINT(1) NOT NULL DEFAULT 0,
  health_json JSON NULL,
  error VARCHAR(1024) NULL,
  state_updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  started_at DATETIME(3) NULL,
  finished_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_deployment_targets (deployment_id, node_id),
  UNIQUE KEY uk_deployment_targets_uid (uid),
  KEY idx_dt_node_state (node_id, state),
  KEY idx_dt_batch (deployment_id, batch_no, state),
  KEY idx_dt_deadline (state, deadline_at),
  CONSTRAINT ck_deployment_targets_state CHECK (state IN ('pending', 'notified', 'downloaded', 'verified', 'tested', 'applied', 'healthy', 'failed', 'rolled_back', 'skipped', 'waiting_offline', 'expired')),
  CONSTRAINT ck_deployment_targets_failure_stage CHECK (failure_stage IS NULL OR failure_stage IN ('download', 'verify', 'render', 'test', 'apply', 'health')),
  CONSTRAINT ck_deployment_targets_attempt CHECK (attempt >= 1),
  CONSTRAINT fk_deployment_targets_deployments FOREIGN KEY (deployment_id) REFERENCES deployments (id) ON DELETE CASCADE,
  CONSTRAINT fk_deployment_targets_nodes FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON deployment_targets TO lingedge_app;

-- ---- deployment_target_steps：ReportApply 的幂等键承载与逐步历史（C2；幂等键 (deployment_target, attempt, state)）----
CREATE TABLE IF NOT EXISTS deployment_target_steps (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  deployment_target_id BIGINT UNSIGNED NOT NULL,
  attempt INT UNSIGNED NOT NULL,
  state VARCHAR(16) NOT NULL,
  started_at DATETIME(3) NULL,
  finished_at DATETIME(3) NULL,
  message VARCHAR(512) NULL,
  reported_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_dts (deployment_target_id, attempt, state),
  CONSTRAINT ck_deployment_target_steps_state CHECK (state IN ('downloaded', 'verified', 'tested', 'applied', 'healthy', 'failed', 'rolled_back')),
  CONSTRAINT fk_deployment_target_steps_targets FOREIGN KEY (deployment_target_id) REFERENCES deployment_targets (id) ON DELETE CASCADE
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON deployment_target_steps TO lingedge_app;

-- ---- node_cluster_state：每个节点在每个集群上实际生效/期望的修订（复合主键；ADR-020；含 held_seq）----
CREATE TABLE IF NOT EXISTS node_cluster_state (
  node_id BIGINT UNSIGNED NOT NULL,
  cluster_id BIGINT UNSIGNED NOT NULL,
  desired_revision_id BIGINT UNSIGNED NULL,
  desired_seq BIGINT UNSIGNED NULL,
  applied_revision_id BIGINT UNSIGNED NULL,
  applied_lineage BIGINT UNSIGNED NULL,
  applied_seq BIGINT UNSIGNED NULL,
  node_generation BIGINT UNSIGNED NULL,
  held_seq BIGINT UNSIGNED NULL,
  previous_applied_seq BIGINT UNSIGNED NULL,
  state VARCHAR(16) NOT NULL DEFAULT 'idle',
  applied_at DATETIME(3) NULL,
  last_error VARCHAR(1024) NULL,
  last_report_at DATETIME(3) NULL,
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (node_id, cluster_id),
  KEY idx_ncs_cluster (cluster_id, applied_seq),
  KEY idx_ncs_desired (cluster_id, desired_seq),
  KEY idx_ncs_desired_revision (cluster_id, desired_revision_id),
  KEY idx_ncs_applied_revision (cluster_id, applied_revision_id),
  CONSTRAINT ck_node_cluster_state_state CHECK (state IN ('idle', 'applying', 'failed', 'rolled_back')),
  CONSTRAINT fk_node_cluster_state_nodes FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE CASCADE,
  CONSTRAINT fk_node_cluster_state_clusters FOREIGN KEY (cluster_id) REFERENCES clusters (id) ON DELETE CASCADE,
  CONSTRAINT fk_node_cluster_state_desired FOREIGN KEY (cluster_id, desired_revision_id) REFERENCES config_revisions (cluster_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_node_cluster_state_applied FOREIGN KEY (cluster_id, applied_revision_id) REFERENCES config_revisions (cluster_id, id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON node_cluster_state TO lingedge_app;

-- ---- 循环/前向外键（README §2.4）：先建表后 ALTER；information_schema 守卫保证重跑安全 ----
-- lingedge:guard=经 information_schema.TABLE_CONSTRAINTS 判断外键是否已存在，已存在则 DO 0，重跑安全
SET @le_ddl = IF((SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'publish_queue' AND CONSTRAINT_NAME = 'fk_publish_queue_deployments' AND CONSTRAINT_TYPE = 'FOREIGN KEY') = 0, 'ALTER TABLE publish_queue ADD CONSTRAINT fk_publish_queue_deployments FOREIGN KEY (deployment_id) REFERENCES deployments (id) ON DELETE SET NULL', 'DO 0');
PREPARE le_stmt FROM @le_ddl;
EXECUTE le_stmt;
DEALLOCATE PREPARE le_stmt;

-- lingedge:guard=经 information_schema.TABLE_CONSTRAINTS 判断外键是否已存在，已存在则 DO 0，重跑安全
SET @le_ddl = IF((SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'clusters' AND CONSTRAINT_NAME = 'fk_clusters_confirmed_revision' AND CONSTRAINT_TYPE = 'FOREIGN KEY') = 0, 'ALTER TABLE clusters ADD CONSTRAINT fk_clusters_confirmed_revision FOREIGN KEY (id, confirmed_revision_id) REFERENCES config_revisions (cluster_id, id) ON DELETE RESTRICT', 'DO 0');
PREPARE le_stmt FROM @le_ddl;
EXECUTE le_stmt;
DEALLOCATE PREPARE le_stmt;

-- lingedge:guard=经 information_schema.TABLE_CONSTRAINTS 判断外键是否已存在，已存在则 DO 0，重跑安全
SET @le_ddl = IF((SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'sites' AND CONSTRAINT_NAME = 'fk_sites_last_deployment' AND CONSTRAINT_TYPE = 'FOREIGN KEY') = 0, 'ALTER TABLE sites ADD CONSTRAINT fk_sites_last_deployment FOREIGN KEY (last_deployment_id) REFERENCES deployments (id) ON DELETE SET NULL', 'DO 0');
PREPARE le_stmt FROM @le_ddl;
EXECUTE le_stmt;
DEALLOCATE PREPARE le_stmt;
