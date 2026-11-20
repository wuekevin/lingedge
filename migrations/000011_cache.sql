-- lingedge:class=expand
-- lingedge:summary=缓存：缓存策略（含站点缓存 epoch）、缓存刷新任务头、缓存任务逐节点业务回执
-- 依据 database/README §2.2、§2.4、§8.1，rules-cache-cert.md §2（FR-69、FR-78/79）；ADR-007。
-- cache_tasks 无 cluster_id：扇出范围恒为站点绑定的全部集群，epoch 是站点级单值（IMP-04）。
-- cache_policies.risk_ack_by 的外键为 RESTRICT：它被 ck_cache_policies_dangerous 引用，ON DELETE SET NULL 会使该 CHECK 建表即 ERROR 3823（README §2.2、§9）。
-- cache_policies.default_flag 为 STORED 生成列，基列 site_id 的外键为 RESTRICT（README §2.2 生成列规则）。
-- cache_task_nodes.node_task_id → node_tasks（000009）：业务回执与逐节点投递队列的关联；proto NodeTask.task_uid = node_tasks.uid。
-- +goose Up

-- ---- cache_policies：站点缓存策略（C1 + T；每站点一条默认策略并承载站点缓存 epoch，另有可选命名策略）----
-- 数值默认值是建站时 Service 写入的建议默认，不是业务承诺（TTL 默认待基线测试，NFR-30）。
-- lingedge:cross-tenant-index=uk_cache_policies_name 站点内缓存策略名唯一（site_id 已隐含租户，复合外键保证 tenant 一致）
-- lingedge:cross-tenant-index=uk_cache_policies_default 每站点至多一条默认策略（default_flag 取值为 site_id，已隐含租户）
CREATE TABLE IF NOT EXISTS cache_policies (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(128) NOT NULL,
  is_default TINYINT(1) NOT NULL DEFAULT 0,
  default_flag BIGINT UNSIGNED AS (IF(is_default = 1, site_id, NULL)) STORED,
  mode VARCHAR(16) NOT NULL DEFAULT 'respect_origin',
  default_ttl_s INT UNSIGNED NOT NULL DEFAULT 0,
  max_ttl_s INT UNSIGNED NOT NULL DEFAULT 2592000,
  cacheable_statuses_json JSON NOT NULL,
  negative_ttl_json JSON NULL,
  file_type_ttls_json JSON NULL,
  query_policy VARCHAR(24) NOT NULL DEFAULT 'include_all',
  query_params_json JSON NULL,
  cookie_policy VARCHAR(24) NOT NULL DEFAULT 'ignore',
  cookie_names_json JSON NULL,
  vary_headers_json JSON NULL,
  scheme_in_key TINYINT(1) NOT NULL DEFAULT 0,
  ignore_origin_cache_control TINYINT(1) NOT NULL DEFAULT 0,
  allow_cache_set_cookie TINYINT(1) NOT NULL DEFAULT 0,
  allow_cache_authorization TINYINT(1) NOT NULL DEFAULT 0,
  risk_ack_by BIGINT UNSIGNED NULL,
  risk_ack_at DATETIME(3) NULL,
  cache_lock_enabled TINYINT(1) NOT NULL DEFAULT 1,
  cache_lock_timeout_ms INT UNSIGNED NOT NULL DEFAULT 5000,
  range_mode VARCHAR(16) NOT NULL DEFAULT 'cache_slices',
  slice_size_kb INT UNSIGNED NOT NULL DEFAULT 1024,
  large_file_threshold_mb INT UNSIGNED NOT NULL DEFAULT 64,
  stale_if_error_s INT UNSIGNED NOT NULL DEFAULT 0,
  stale_while_revalidate_s INT UNSIGNED NOT NULL DEFAULT 0,
  cache_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0,
  epoch_bumped_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_cache_policies_uid (uid),
  UNIQUE KEY uk_cache_policies_name (site_id, name),
  UNIQUE KEY uk_cache_policies_default (default_flag),
  KEY idx_cache_policies_tenant (tenant_id, site_id),
  CONSTRAINT ck_cache_policies_mode CHECK (mode IN ('respect_origin', 'override', 'bypass')),
  CONSTRAINT ck_cache_policies_query CHECK (query_policy IN ('ignore_all', 'include_all', 'include_whitelist', 'exclude_list')),
  CONSTRAINT ck_cache_policies_cookie CHECK (cookie_policy IN ('ignore', 'include_named', 'bypass_on_named')),
  CONSTRAINT ck_cache_policies_range CHECK (range_mode IN ('passthrough', 'cache_slices')),
  CONSTRAINT ck_cache_policies_dangerous CHECK ((allow_cache_set_cookie = 0 AND allow_cache_authorization = 0) OR (risk_ack_by IS NOT NULL AND risk_ack_at IS NOT NULL)),
  CONSTRAINT ck_cache_policies_epoch CHECK (is_default = 1 OR cache_epoch = 0),
  CONSTRAINT ck_cache_policies_ttl CHECK (default_ttl_s <= max_ttl_s),
  CONSTRAINT fk_cache_policies_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_cache_policies_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_cache_policies_users FOREIGN KEY (risk_ack_by) REFERENCES users (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON cache_policies TO lingedge_app;

-- ---- cache_tasks：缓存刷新/预热任务头（C1 + T；M1：URL 刷新、整站失效，其余 kind 在 P2 由 Service 放行）----
-- lingedge:cross-tenant-index=idx_cache_tasks_status 调度/汇总按状态扫描未完成任务（平台侧跨租户查询）
CREATE TABLE IF NOT EXISTS cache_tasks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL,
  task_id BIGINT UNSIGNED NULL,
  kind VARCHAR(24) NOT NULL,
  targets_json JSON NULL,
  target_count INT UNSIGNED NOT NULL DEFAULT 0,
  epoch_after BIGINT UNSIGNED NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  total_nodes INT UNSIGNED NOT NULL DEFAULT 0,
  ok_nodes INT UNSIGNED NOT NULL DEFAULT 0,
  failed_nodes INT UNSIGNED NOT NULL DEFAULT 0,
  requested_by BIGINT UNSIGNED NULL,
  request_id CHAR(26) NOT NULL,
  finished_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_cache_tasks_uid (uid),
  KEY idx_cache_tasks_site (tenant_id, site_id, created_at),
  KEY idx_cache_tasks_status (status, created_at),
  CONSTRAINT ck_cache_tasks_kind CHECK (kind IN ('purge_url', 'purge_site', 'prefetch', 'purge_prefix', 'purge_wildcard')),
  CONSTRAINT ck_cache_tasks_status CHECK (status IN ('pending', 'dispatching', 'partial', 'success', 'failed', 'canceled')),
  CONSTRAINT fk_cache_tasks_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_cache_tasks_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_cache_tasks_tasks FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE SET NULL,
  CONSTRAINT fk_cache_tasks_users FOREIGN KEY (requested_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON cache_tasks TO lingedge_app;

-- ---- cache_task_nodes：缓存任务在每个节点的业务回执（C2 + updated_at + T；离线节点上线补执行）----
-- status 是事实源，与 node_tasks.status / proto TaskStatus 的映射见 ADR-007 §6（RUNNING→sent，SKIPPED→skipped_offline，EXPIRED→expired）。
-- lingedge:cross-tenant-index=uk_cache_task_nodes 每个缓存任务在每个节点一条回执（cache_task_id 已隐含租户）
-- lingedge:cross-tenant-index=idx_ctn_node_status 节点上线时按节点拉取待办回执（平台侧跨租户查询，同时支撑外键 fk_cache_task_nodes_nodes）
CREATE TABLE IF NOT EXISTS cache_task_nodes (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  cache_task_id BIGINT UNSIGNED NOT NULL,
  node_id BIGINT UNSIGNED NOT NULL,
  node_task_id BIGINT UNSIGNED NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  result_code VARCHAR(32) NULL,
  error VARCHAR(512) NULL,
  started_at DATETIME(3) NULL,
  finished_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_cache_task_nodes (cache_task_id, node_id),
  KEY idx_ctn_node_status (node_id, status),
  KEY idx_ctn_tenant (tenant_id, cache_task_id),
  CONSTRAINT ck_cache_task_nodes_status CHECK (status IN ('pending', 'sent', 'success', 'failed', 'skipped_offline', 'expired')),
  CONSTRAINT fk_cache_task_nodes_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_cache_task_nodes_cache_tasks FOREIGN KEY (cache_task_id) REFERENCES cache_tasks (id) ON DELETE CASCADE,
  CONSTRAINT fk_cache_task_nodes_nodes FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE RESTRICT,
  CONSTRAINT fk_cache_task_nodes_node_tasks FOREIGN KEY (node_task_id) REFERENCES node_tasks (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON cache_task_nodes TO lingedge_app;
