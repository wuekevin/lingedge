-- lingedge:class=expand
-- lingedge:summary=站点配置：域名、站点设置、源站组、源站、源站健康、站点-集群绑定、逐节点任务队列（7 张表）
-- 依据 database/README §2.2、§2.4、§8.1，tenant-site.md §2.3–§2.5、§3，node-deploy.md §4.4。
-- 站点子表一律声明复合外键 (tenant_id, site_id) → sites(tenant_id, id)（子行租户与父行不一致在库层不可能）；origins 另有
-- (site_id, origin_group_id) → origin_groups(site_id, id)，跨站点/跨租户绑定源站组在库层不可能（MIN-13）。
-- site_domains.active_hostname 为 STORED 生成列：基列 hostname/deleted_at 无外键；origin_groups.default_flag 的基列 site_id 的外键为 RESTRICT，
-- 满足 README §2.2 生成列规则（STORED 的基列外键不得 CASCADE/SET NULL）。
-- node_tasks 为逐节点任务队列（C2u，租约/attempt/回执，BLK-06）：kind 与 proto NodeTask.payload 的 oneof 一一对应（封闭枚举，无 exec 类）。
-- 租户表索引以 tenant_id 打头；确属跨租户/外键支撑的索引按 README §2.6 显式登记豁免（原因必填）。
-- +goose Up

-- ---- site_domains：站点访问主机名（C1 + T + SD；FR-48）----
-- verification_status 默认 pending（MIN-11）：自用模式由 Service 创建时显式置 exempt（verification_method='admin_exempt'）。
-- lingedge:cross-tenant-index=uk_site_domains_active_host 活动主机名全局唯一、跨租户冲突检测（他租户占用即冲突，FR-48）；软删除后释放
-- lingedge:cross-tenant-index=idx_site_domains_cert 证书删除/替换时按证书反查绑定域名（同时支撑外键 fk_site_domains_certificates）
CREATE TABLE IF NOT EXISTS site_domains (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL,
  hostname VARCHAR(253) NOT NULL,
  active_hostname VARCHAR(253) AS (IF(deleted_at IS NULL, hostname, NULL)) STORED,
  is_wildcard TINYINT(1) NOT NULL DEFAULT 0,
  is_primary TINYINT(1) NOT NULL DEFAULT 0,
  certificate_id BIGINT UNSIGNED NULL,
  verification_status VARCHAR(16) NOT NULL DEFAULT 'pending',
  verification_method VARCHAR(16) NULL,
  verification_token VARCHAR(64) NULL,
  verified_at DATETIME(3) NULL,
  last_check_at DATETIME(3) NULL,
  cname_status VARCHAR(16) NOT NULL DEFAULT 'unknown',
  PRIMARY KEY (id),
  UNIQUE KEY uk_site_domains_uid (uid),
  UNIQUE KEY uk_site_domains_active_host (active_hostname),
  KEY idx_site_domains_site (tenant_id, site_id),
  KEY idx_site_domains_cert (certificate_id),
  CONSTRAINT ck_site_domains_hostname CHECK (REGEXP_LIKE(hostname, '^(\\*\\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z0-9-]{2,63}$', 'c')),
  CONSTRAINT ck_site_domains_lower CHECK (BINARY hostname = BINARY LOWER(hostname)),
  CONSTRAINT ck_site_domains_verification CHECK (verification_status IN ('exempt', 'pending', 'verified', 'failed')),
  CONSTRAINT ck_site_domains_cname CHECK (cname_status IN ('unknown', 'ok', 'mismatch')),
  CONSTRAINT fk_site_domains_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_site_domains_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_site_domains_certificates FOREIGN KEY (certificate_id) REFERENCES certificates (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON site_domains TO lingedge_app;

-- ---- site_settings：站点 1:1 设置（C1 + T；真实 IP 传递 FR-70、TLS、代理参数）----
-- lingedge:cross-tenant-index=uk_site_settings_site 站点 1:1 设置的唯一性（site_id 已隐含租户，复合外键保证 tenant 一致）
CREATE TABLE IF NOT EXISTS site_settings (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL,
  real_ip_xff_enabled TINYINT(1) NOT NULL DEFAULT 1,
  real_ip_xri_enabled TINYINT(1) NOT NULL DEFAULT 1,
  real_ip_custom_enabled TINYINT(1) NOT NULL DEFAULT 0,
  real_ip_custom_header VARCHAR(64) NULL,
  xff_policy VARCHAR(24) NOT NULL DEFAULT 'overwrite',
  trusted_proxies_json JSON NULL,
  force_https TINYINT(1) NOT NULL DEFAULT 0,
  http_to_https_redirect TINYINT(1) NOT NULL DEFAULT 0,
  tls_min_version VARCHAR(8) NOT NULL DEFAULT 'TLSv1.2',
  http2_enabled TINYINT(1) NOT NULL DEFAULT 1,
  hsts_json JSON NULL,
  default_certificate_id BIGINT UNSIGNED NULL,
  origin_tls_verify TINYINT(1) NOT NULL DEFAULT 1,
  listen_ports_json JSON NULL,
  proxy_json JSON NOT NULL,
  waf_json JSON NULL,
  extra_json JSON NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_site_settings_uid (uid),
  UNIQUE KEY uk_site_settings_site (site_id),
  KEY idx_site_settings_tenant (tenant_id, site_id),
  CONSTRAINT ck_site_settings_realip_header CHECK (real_ip_custom_enabled = 0 OR (real_ip_custom_header IS NOT NULL AND REGEXP_LIKE(real_ip_custom_header, '^[A-Za-z][A-Za-z0-9-]{0,63}$', 'c'))),
  CONSTRAINT ck_site_settings_xff_policy CHECK (xff_policy IN ('overwrite', 'append_trusted')),
  CONSTRAINT ck_site_settings_tls_min CHECK (tls_min_version IN ('TLSv1.2', 'TLSv1.3')),
  CONSTRAINT fk_site_settings_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_site_settings_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_site_settings_certificates FOREIGN KEY (default_certificate_id) REFERENCES certificates (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON site_settings TO lingedge_app;

-- ---- origin_groups：源站组（C1 + T）----
-- default_flag = IF(is_default=1, site_id, NULL) STORED + 唯一 → 每站点至多一个默认组；uk_origin_groups_site_id 供 origins 的复合外键引用（MIN-13）。
-- lingedge:cross-tenant-index=uk_origin_groups_name 站点内源站组名唯一（site_id 已隐含租户，复合外键保证 tenant 一致）
-- lingedge:cross-tenant-index=uk_origin_groups_default 每站点至多一个默认源站组（default_flag 取值为 site_id，已隐含租户）
-- lingedge:cross-tenant-index=uk_origin_groups_site_id 供 origins 复合外键 (site_id, origin_group_id) 引用的目标键（MIN-13）
CREATE TABLE IF NOT EXISTS origin_groups (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(128) NOT NULL,
  is_default TINYINT(1) NOT NULL DEFAULT 0,
  default_flag BIGINT UNSIGNED AS (IF(is_default = 1, site_id, NULL)) STORED,
  lb_policy VARCHAR(24) NOT NULL DEFAULT 'weighted',
  retry_json JSON NULL,
  health_check_enabled TINYINT(1) NOT NULL DEFAULT 1,
  health_check_json JSON NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_origin_groups_uid (uid),
  UNIQUE KEY uk_origin_groups_name (site_id, name),
  UNIQUE KEY uk_origin_groups_default (default_flag),
  UNIQUE KEY uk_origin_groups_site_id (site_id, id),
  KEY idx_origin_groups_tenant (tenant_id, site_id),
  CONSTRAINT ck_origin_groups_lb CHECK (lb_policy IN ('weighted', 'primary_backup', 'ip_hash')),
  CONSTRAINT fk_origin_groups_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_origin_groups_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON origin_groups TO lingedge_app;

-- ---- origins：源站（回源目标，C1 + T）----
-- 复合外键 (site_id, origin_group_id) → origin_groups(site_id, id) ON DELETE CASCADE：源站随源站组删除；跨站点/跨租户绑定在库层不可能。
-- lingedge:cross-tenant-index=idx_origins_group 支撑复合外键 fk_origins_origin_groups (site_id, origin_group_id)（site_id 已隐含租户）
CREATE TABLE IF NOT EXISTS origins (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL,
  origin_group_id BIGINT UNSIGNED NOT NULL,
  address VARCHAR(253) NOT NULL,
  port SMALLINT UNSIGNED NOT NULL,
  protocol VARCHAR(8) NOT NULL DEFAULT 'http',
  weight SMALLINT UNSIGNED NOT NULL DEFAULT 100,
  role VARCHAR(8) NOT NULL DEFAULT 'primary',
  enabled TINYINT(1) NOT NULL DEFAULT 1,
  host_header VARCHAR(253) NULL,
  sni VARCHAR(253) NULL,
  tls_verify TINYINT(1) NULL,
  timeouts_json JSON NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_origins_uid (uid),
  KEY idx_origins_group (site_id, origin_group_id),
  KEY idx_origins_tenant (tenant_id, site_id),
  CONSTRAINT ck_origins_port CHECK (port BETWEEN 1 AND 65535),
  CONSTRAINT ck_origins_weight CHECK (weight <= 1000),
  CONSTRAINT ck_origins_protocol CHECK (protocol IN ('http', 'https')),
  CONSTRAINT ck_origins_role CHECK (role IN ('primary', 'backup')),
  CONSTRAINT fk_origins_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_origins_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_origins_origin_groups FOREIGN KEY (site_id, origin_group_id) REFERENCES origin_groups (site_id, id) ON DELETE CASCADE
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON origins TO lingedge_app;

-- ---- origin_health：源站在各节点视角下的当前健康状态（C2 + T；平台专用，租户只见聚合视图 OriginHealthView）----
-- lingedge:cross-tenant-index=uk_origin_health 按 (源站, 节点) upsert 当前状态（origin_id 已隐含租户）
CREATE TABLE IF NOT EXISTS origin_health (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  origin_id BIGINT UNSIGNED NOT NULL,
  node_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(8) NOT NULL DEFAULT 'unknown',
  consecutive_failures INT UNSIGNED NOT NULL DEFAULT 0,
  consecutive_successes INT UNSIGNED NOT NULL DEFAULT 0,
  last_check_at DATETIME(3) NULL,
  last_status_code SMALLINT UNSIGNED NULL,
  last_latency_ms INT UNSIGNED NULL,
  last_error VARCHAR(255) NULL,
  changed_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_origin_health (origin_id, node_id),
  KEY idx_origin_health_tenant (tenant_id, origin_id),
  CONSTRAINT ck_origin_health_status CHECK (status IN ('up', 'down', 'unknown')),
  CONSTRAINT fk_origin_health_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_origin_health_origins FOREIGN KEY (origin_id) REFERENCES origins (id) ON DELETE CASCADE,
  CONSTRAINT fk_origin_health_nodes FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE CASCADE
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON origin_health TO lingedge_app;

-- ---- site_cluster_bindings：站点 ↔ 集群绑定（C2 + T；平台管理，租户不可见，ADR-009 §5 R-T1）----
-- lingedge:cross-tenant-index=uk_site_cluster 站点-集群绑定唯一（site_id 已隐含租户）
-- lingedge:cross-tenant-index=idx_scb_cluster 集群侧按集群列出绑定站点（平台侧跨租户查询，同时支撑外键 fk_site_cluster_bindings_clusters）
CREATE TABLE IF NOT EXISTS site_cluster_bindings (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL,
  cluster_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  PRIMARY KEY (id),
  UNIQUE KEY uk_site_cluster (site_id, cluster_id),
  KEY idx_scb_cluster (cluster_id),
  KEY idx_scb_tenant (tenant_id, site_id),
  CONSTRAINT ck_site_cluster_bindings_status CHECK (status IN ('active', 'draining')),
  CONSTRAINT fk_site_cluster_bindings_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_site_cluster_bindings_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE CASCADE,
  CONSTRAINT fk_site_cluster_bindings_clusters FOREIGN KEY (cluster_id) REFERENCES clusters (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON site_cluster_bindings TO lingedge_app;

-- ---- node_tasks：逐节点任务投递队列（C2u；租约/attempt/回执，BLK-06；平台级，tenant_id/site_id 仅为发起上下文）----
-- uid = proto NodeTask.task_uid（对外只暴露 uid）；kind 与 proto NodeTask.payload 的 oneof 一一对应（封闭枚举，无 exec 类）。
-- 复合外键 (tenant_id, site_id) → sites(tenant_id, id)：MATCH SIMPLE 下 tenant_id 为 NULL 会绕过校验，故 ck_node_tasks_site_tenant 要求有站点必有租户。
-- ck_node_tasks_site_tenant 引用 tenant_id/site_id，其外键均为 RESTRICT（被 CHECK 引用的外键列不得 SET NULL，README §2.2）。
CREATE TABLE IF NOT EXISTS node_tasks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  node_id BIGINT UNSIGNED NOT NULL,
  task_id BIGINT UNSIGNED NULL,
  tenant_id BIGINT UNSIGNED NULL,
  site_id BIGINT UNSIGNED NULL,
  kind VARCHAR(24) NOT NULL,
  payload_json JSON NOT NULL,
  status VARCHAR(12) NOT NULL DEFAULT 'pending',
  attempt INT UNSIGNED NOT NULL DEFAULT 0,
  max_attempts INT UNSIGNED NOT NULL DEFAULT 5,
  lease_until DATETIME(3) NULL,
  deadline_at DATETIME(3) NULL,
  result_json JSON NULL,
  error_code VARCHAR(96) NULL,
  error_message VARCHAR(512) NULL,
  request_id CHAR(26) NOT NULL,
  started_at DATETIME(3) NULL,
  finished_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_node_tasks_uid (uid),
  KEY idx_node_tasks_pull (node_id, status, deadline_at),
  KEY idx_node_tasks_task (task_id),
  KEY idx_node_tasks_tenant (tenant_id, site_id, created_at),
  CONSTRAINT ck_node_tasks_kind CHECK (kind IN ('cache_purge_url', 'cache_purge_site', 'cache_purge_prefix', 'cache_prefetch', 'node_upgrade', 'node_rollback', 'cert_reload', 'debug_request')),
  CONSTRAINT ck_node_tasks_status CHECK (status IN ('pending', 'leased', 'running', 'success', 'failed', 'skipped', 'expired', 'canceled')),
  CONSTRAINT ck_node_tasks_attempt CHECK (attempt <= max_attempts),
  CONSTRAINT ck_node_tasks_site_tenant CHECK (site_id IS NULL OR tenant_id IS NOT NULL),
  CONSTRAINT fk_node_tasks_nodes FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE RESTRICT,
  CONSTRAINT fk_node_tasks_tasks FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE SET NULL,
  CONSTRAINT fk_node_tasks_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_node_tasks_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON node_tasks TO lingedge_app;
