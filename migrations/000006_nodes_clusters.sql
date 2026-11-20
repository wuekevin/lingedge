-- lingedge:class=expand
-- lingedge:summary=节点与集群：区域、线路、IP 池、节点、IP 池成员、集群、集群成员、注册令牌、节点证书台账、节点事件、心跳（分区，11 张表）
-- 依据 database/README §8.1，node-deploy.md §1、§4、§5；全部为平台级表（无 tenant_id），租户不可直接访问（ADR-009 §5 R-T1）。
-- clusters.confirmed_revision_id 先建为无 FK 的普通列：config_revisions 在 000012 才创建，届时 ALTER 追加外键（循环外键，README §2.4）。
-- node_heartbeats 为分区表：无外键（实测 ERROR 1506）、主键含分区键、含 pmax 兜底；lingedge_maint 表级 SELECT/ALTER/DROP 用于滚动预建与保留期 DROP PARTITION。
-- 每张表建成后紧跟 lingedge_app 的逐表授权（库级授权无法在表级收窄，README §5.1）。
-- +goose Up

-- ---- regions：区域（C1；M1 仅作节点元数据，FR-59）----
CREATE TABLE IF NOT EXISTS regions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  code VARCHAR(32) NOT NULL,
  name VARCHAR(128) NOT NULL,
  country_code CHAR(2) NULL,
  continent VARCHAR(16) NULL,
  parent_id BIGINT UNSIGNED NULL,
  sort_order INT NOT NULL DEFAULT 0,
  PRIMARY KEY (id),
  UNIQUE KEY uk_regions_uid (uid),
  UNIQUE KEY uk_regions_code (code),
  KEY idx_regions_parent (parent_id),
  CONSTRAINT fk_regions_regions FOREIGN KEY (parent_id) REFERENCES regions (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON regions TO lingedge_app;

-- ---- isp_lines：运营商线路（C1）----
CREATE TABLE IF NOT EXISTS isp_lines (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  code VARCHAR(32) NOT NULL,
  name VARCHAR(128) NOT NULL,
  asn_list_json JSON NULL,
  description VARCHAR(255) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_isp_lines_uid (uid),
  UNIQUE KEY uk_isp_lines_code (code)
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON isp_lines TO lingedge_app;

-- ---- ip_pools：IP 池（C1）----
CREATE TABLE IF NOT EXISTS ip_pools (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  name VARCHAR(128) NOT NULL,
  ip_family VARCHAR(4) NOT NULL,
  description VARCHAR(255) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_ip_pools_uid (uid),
  UNIQUE KEY uk_ip_pools_name (name),
  CONSTRAINT ck_ip_pools_family CHECK (ip_family IN ('v4', 'v6'))
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON ip_pools TO lingedge_app;

-- ---- nodes：边缘节点（C1 + SD；活动名称唯一用生成列）----
-- active_name 为 STORED：基列 name/deleted_at 无外键，满足 README §2.2 的 STORED 前提；region/isp 外键 SET NULL 且不被任何 CHECK 引用。
CREATE TABLE IF NOT EXISTS nodes (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  name VARCHAR(128) NOT NULL,
  active_name VARCHAR(128) AS (IF(deleted_at IS NULL, name, NULL)) STORED,
  hostname VARCHAR(255) NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending_enroll',
  maintenance_reason VARCHAR(255) NULL,
  region_id BIGINT UNSIGNED NULL,
  isp_line_id BIGINT UNSIGNED NULL,
  primary_ipv4 VARCHAR(45) NULL,
  primary_ipv6 VARCHAR(45) NULL,
  egress_ips_json JSON NULL,
  os VARCHAR(64) NULL,
  arch VARCHAR(64) NULL,
  kernel VARCHAR(64) NULL,
  agent_version VARCHAR(64) NULL,
  openresty_version VARCHAR(64) NULL,
  cpu_cores SMALLINT UNSIGNED NULL,
  mem_bytes BIGINT UNSIGNED NULL,
  disk_bytes BIGINT UNSIGNED NULL,
  capabilities_json JSON NULL,
  labels_json JSON NULL,
  last_status_json JSON NULL,
  last_heartbeat_at DATETIME(3) NULL,
  last_seen_ip VARBINARY(16) NULL,
  enrolled_at DATETIME(3) NULL,
  current_cert_serial VARCHAR(64) NULL,
  ca_bundle_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
  upgrade_channel VARCHAR(16) NOT NULL DEFAULT 'stable',
  PRIMARY KEY (id),
  UNIQUE KEY uk_nodes_uid (uid),
  UNIQUE KEY uk_nodes_active_name (active_name),
  KEY idx_nodes_status (status),
  KEY idx_nodes_region (region_id),
  KEY idx_nodes_isp_line (isp_line_id),
  KEY idx_nodes_heartbeat (last_heartbeat_at),
  CONSTRAINT ck_nodes_status CHECK (status IN ('pending_enroll', 'online', 'offline', 'maintenance', 'disabled', 'decommissioned')),
  CONSTRAINT fk_nodes_regions FOREIGN KEY (region_id) REFERENCES regions (id) ON DELETE SET NULL,
  CONSTRAINT fk_nodes_isp_lines FOREIGN KEY (isp_line_id) REFERENCES isp_lines (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON nodes TO lingedge_app;

-- ---- ip_pool_members：IP 池成员（C2；address 全局唯一）----
CREATE TABLE IF NOT EXISTS ip_pool_members (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  ip_pool_id BIGINT UNSIGNED NOT NULL,
  node_id BIGINT UNSIGNED NULL,
  address VARCHAR(45) NOT NULL,
  role VARCHAR(8) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  PRIMARY KEY (id),
  UNIQUE KEY uk_ip_pool_members (address),
  KEY idx_ipm_pool (ip_pool_id),
  KEY idx_ipm_node (node_id),
  CONSTRAINT ck_ip_pool_members_role CHECK (role IN ('serving', 'egress')),
  CONSTRAINT ck_ip_pool_members_status CHECK (status IN ('active', 'reserved', 'disabled')),
  CONSTRAINT fk_ip_pool_members_ip_pools FOREIGN KEY (ip_pool_id) REFERENCES ip_pools (id) ON DELETE CASCADE,
  CONSTRAINT fk_ip_pool_members_nodes FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON ip_pool_members TO lingedge_app;

-- ---- clusters：节点集群 = 配置修订的范围（C1）----
-- confirmed_revision_id：最近一次全量确认健康的修订；先建为无 FK 普通列，000012 追加 FK → config_revisions(id) RESTRICT（循环外键）。
CREATE TABLE IF NOT EXISTS clusters (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  name VARCHAR(128) NOT NULL,
  description VARCHAR(512) NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  settings_json JSON NOT NULL,
  trusted_proxies_json JSON NULL,
  current_seq BIGINT UNSIGNED NOT NULL DEFAULT 0,
  confirmed_revision_id BIGINT UNSIGNED NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_clusters_uid (uid),
  UNIQUE KEY uk_clusters_name (name),
  CONSTRAINT ck_clusters_status CHECK (status IN ('active', 'disabled'))
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON clusters TO lingedge_app;

-- ---- cluster_nodes：节点 ↔ 集群成员关系（C2 含 updated_at；一节点可入多集群，F-3）----
CREATE TABLE IF NOT EXISTS cluster_nodes (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  cluster_id BIGINT UNSIGNED NOT NULL,
  node_id BIGINT UNSIGNED NOT NULL,
  serving_state VARCHAR(16) NOT NULL DEFAULT 'active',
  weight SMALLINT UNSIGNED NOT NULL DEFAULT 100,
  joined_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_cluster_nodes (cluster_id, node_id),
  KEY idx_cluster_nodes_node (node_id),
  CONSTRAINT ck_cluster_nodes_state CHECK (serving_state IN ('active', 'draining', 'standby')),
  CONSTRAINT fk_cluster_nodes_clusters FOREIGN KEY (cluster_id) REFERENCES clusters (id) ON DELETE RESTRICT,
  CONSTRAINT fk_cluster_nodes_nodes FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON cluster_nodes TO lingedge_app;

-- ---- enroll_tokens：一次性节点注册令牌（C1；令牌绑定节点与证书角色，ADR-003 §7）----
-- 仅存 token_hash（SHA-256 的 64 位十六进制）；明文只在创建时展示一次。原子消费：UPDATE … WHERE used_at IS NULL AND revoked_at IS NULL AND expires_at > NOW(3)。
CREATE TABLE IF NOT EXISTS enroll_tokens (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  node_id BIGINT UNSIGNED NOT NULL,
  role VARCHAR(16) NOT NULL DEFAULT 'node',
  token_hash CHAR(64) NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  used_at DATETIME(3) NULL,
  used_ip VARBINARY(16) NULL,
  bound_cidr VARCHAR(64) NULL,
  revoked_at DATETIME(3) NULL,
  created_by BIGINT UNSIGNED NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_enroll_tokens_uid (uid),
  UNIQUE KEY uk_enroll_tokens_hash (token_hash),
  KEY idx_enroll_tokens_node (node_id),
  KEY idx_enroll_tokens_exp (expires_at),
  CONSTRAINT ck_enroll_tokens_role CHECK (role IN ('node', 'dns', 'reporter', 'scheduler', 'worker')),
  CONSTRAINT ck_enroll_tokens_hash CHECK (REGEXP_LIKE(token_hash, '^[0-9a-f]{64}$', 'c')),
  CONSTRAINT fk_enroll_tokens_nodes FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE CASCADE,
  CONSTRAINT fk_enroll_tokens_users FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON enroll_tokens TO lingedge_app;

-- ---- node_certificates：mTLS 证书台账与吊销（C2；主体可为节点或后续的 DNS/Reporter，多态无 FK）----
-- ck_node_certificates_subject 引用 node_id，其外键为 ON DELETE CASCADE（CHECK 可引用）；若改 SET NULL 将 ERROR 3823。
CREATE TABLE IF NOT EXISTS node_certificates (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  subject_type VARCHAR(16) NOT NULL DEFAULT 'node',
  subject_id BIGINT UNSIGNED NOT NULL,
  node_id BIGINT UNSIGNED NULL,
  serial VARCHAR(64) NOT NULL,
  fingerprint_sha256 CHAR(64) NOT NULL,
  issuer_key_id VARCHAR(32) NOT NULL,
  not_before DATETIME(3) NOT NULL,
  not_after DATETIME(3) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  superseded_at DATETIME(3) NULL,
  grace_until DATETIME(3) NULL,
  revoked_at DATETIME(3) NULL,
  revoke_reason VARCHAR(64) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_node_certs_serial (serial),
  KEY idx_node_certs_subject (subject_type, subject_id, status),
  KEY idx_node_certs_node (node_id),
  CONSTRAINT ck_node_certificates_subject CHECK ((subject_type = 'node') = (node_id IS NOT NULL)),
  CONSTRAINT ck_node_certificates_type CHECK (subject_type IN ('node', 'dns', 'reporter', 'scheduler', 'worker')),
  CONSTRAINT ck_node_certificates_status CHECK (status IN ('pending', 'active', 'superseded', 'revoked', 'expired')),
  CONSTRAINT ck_node_certificates_validity CHECK (not_before < not_after),
  CONSTRAINT fk_node_certificates_nodes FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE CASCADE
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON node_certificates TO lingedge_app;

-- ---- node_events：节点级事件流（C2u；gRPC 暴露，对外只用 uid，uid = Agent 的 event_uid，INSERT IGNORE 去重）----
CREATE TABLE IF NOT EXISTS node_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  node_id BIGINT UNSIGNED NOT NULL,
  event_type VARCHAR(48) NOT NULL,
  severity VARCHAR(8) NOT NULL DEFAULT 'info',
  message VARCHAR(512) NOT NULL,
  data_json JSON NULL,
  request_id CHAR(26) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_node_events_uid (uid),
  KEY idx_node_events_node (node_id, id),
  KEY idx_node_events_type (event_type, created_at),
  CONSTRAINT ck_node_events_severity CHECK (severity IN ('info', 'warn', 'error')),
  CONSTRAINT fk_node_events_nodes FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE CASCADE
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON node_events TO lingedge_app;

-- ---- node_heartbeats：心跳时序（C2；高频，按日 RANGE 分区，无外键，含 pmax 兜底）----
-- 当前状态以 nodes.last_status_json 为准；保留 7 天（[ASSUMPTION]），由 partition.maintain（内部周期作业）REORGANIZE/DROP PARTITION。
-- 初始分区覆盖 2026-11-20 至 2026-11-27（首个分区同时容纳更早的时间戳）；其后写入落入 pmax，滚动预建由 1.15 的作业负责。
CREATE TABLE IF NOT EXISTS node_heartbeats (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  node_id BIGINT UNSIGNED NOT NULL,
  received_at DATETIME(3) NOT NULL,
  agent_version VARCHAR(64) NOT NULL,
  openresty_state VARCHAR(16) NOT NULL,
  cpu_permille SMALLINT UNSIGNED NULL,
  load1_milli INT UNSIGNED NULL,
  mem_used_bytes BIGINT UNSIGNED NULL,
  mem_total_bytes BIGINT UNSIGNED NULL,
  disk_used_bytes BIGINT UNSIGNED NULL,
  disk_total_bytes BIGINT UNSIGNED NULL,
  cache_disk_used_bytes BIGINT UNSIGNED NULL,
  conn_active INT UNSIGNED NULL,
  clock_skew_ms INT NULL,
  applied_json JSON NULL,
  PRIMARY KEY (id, received_at),
  KEY idx_node_heartbeats_node (node_id, received_at),
  CONSTRAINT ck_node_heartbeats_state CHECK (openresty_state IN ('running', 'stopped', 'failed', 'unknown')),
  CONSTRAINT ck_node_heartbeats_cpu CHECK (cpu_permille IS NULL OR cpu_permille <= 1000)
) ENGINE=InnoDB
PARTITION BY RANGE COLUMNS(received_at) (
  PARTITION p20261120 VALUES LESS THAN ('2026-11-21 00:00:00'),
  PARTITION p20261121 VALUES LESS THAN ('2026-11-22 00:00:00'),
  PARTITION p20261122 VALUES LESS THAN ('2026-11-23 00:00:00'),
  PARTITION p20261123 VALUES LESS THAN ('2026-11-24 00:00:00'),
  PARTITION p20261124 VALUES LESS THAN ('2026-11-25 00:00:00'),
  PARTITION p20261125 VALUES LESS THAN ('2026-11-26 00:00:00'),
  PARTITION p20261126 VALUES LESS THAN ('2026-11-27 00:00:00'),
  PARTITION p20261127 VALUES LESS THAN ('2026-11-28 00:00:00'),
  PARTITION pmax VALUES LESS THAN (MAXVALUE)
);
-- lingedge_app 逐表授权（README §5.1：仅 audit_logs/wallet_entries 为追加只写）；保留期清理走 lingedge_maint 的 DROP PARTITION，不走行级 DELETE。
GRANT SELECT, INSERT, UPDATE, DELETE ON node_heartbeats TO lingedge_app;
-- 维护账号表级 SELECT, ALTER, DROP（REORGANIZE/DROP PARTITION，README §5.1）。
GRANT SELECT, ALTER, DROP ON node_heartbeats TO lingedge_maint;
