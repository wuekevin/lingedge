-- lingedge:class=expand
-- lingedge:summary=观测：指标聚合（metric_rollups）、聚合窗口幂等/同代冲突判定（rollup_windows）、访问日志分片元数据（access_log_chunks）；高频，无外键（3 张）
-- 依据 database/README §2.4（高频例外无外键）、§8.1，dns-scheduling-security.md §5.1/§5.1.1，node-deploy.md §6.1；ADR-008/010。
-- 高频/大体量表仅保留索引列，引用完整性由写入路径与清理任务保证（README §2.4 例外清单）；access_log_chunks 的日志正文在文件系统，不入 MySQL。
-- metric_rollups.window_start 与 rollup_windows.window_start 统一 DATETIME(3)（契约 §9.1）。
-- rollup_windows 的同代异值 CONFLICT 判定（IMP-09）：同事务内与 metric_rollups 写入一起；同键同代同哈希 = DUPLICATE，同代不同哈希 = CONFLICT（保留首次，不覆盖）。
-- access_log_chunks 唯一键含 log_date（IMP-08：批次跨 UTC 零点拆成两行时同键不冲突）。
-- metric_rollups/access_log_chunks 的 tenant_id 为 NOT NULL（0 = 平台级/未归属）：租户表索引以 tenant_id 打头，跨租户索引按 README §2.6 显式登记豁免。
-- 删除（保留期清理）按 window_start / log_date 小批量执行，故 app 账号保留 DELETE；授权按框架 lint 为 SELECT/INSERT/UPDATE/DELETE（仅追加只写表 audit_logs/wallet_entries 例外）。
-- +goose Up

-- ---- metric_rollups：聚合指标长表（C2；1m/5m/1h/1d；dim_type 维度、generation 修订代数）----
-- lingedge:cross-tenant-index=uk_metric_rollups 聚合窗口唯一键与幂等 upsert（含 dim_type/dim_id/node_id，跨租户去重）
-- lingedge:cross-tenant-index=idx_metric_rollups_time 保留期按 (granularity, window_start) 小批量清理（平台任务，跨租户）
CREATE TABLE IF NOT EXISTS metric_rollups (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  granularity VARCHAR(4) NOT NULL,
  window_start DATETIME(3) NOT NULL,
  dim_type VARCHAR(12) NOT NULL,
  dim_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  node_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  tenant_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  metric VARCHAR(48) NOT NULL,
  dim_sub VARCHAR(64) NOT NULL DEFAULT '',
  agg VARCHAR(4) NOT NULL DEFAULT 'sum',
  value BIGINT NOT NULL,
  generation INT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (id),
  UNIQUE KEY uk_metric_rollups (granularity, window_start, dim_type, dim_id, node_id, metric, dim_sub),
  KEY idx_metric_rollups_tenant (tenant_id, dim_type, dim_id, granularity, window_start),
  KEY idx_metric_rollups_time (granularity, window_start),
  CONSTRAINT ck_metric_rollups_granularity CHECK (granularity IN ('1m', '5m', '1h', '1d')),
  CONSTRAINT ck_metric_rollups_dim_type CHECK (dim_type IN ('node', 'site', 'site_node', 'rule', 'cluster', 'dns_zone', 'l4_app', 'platform')),
  CONSTRAINT ck_metric_rollups_agg CHECK (agg IN ('sum', 'max', 'last'))
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON metric_rollups TO lingedge_app;

-- ---- rollup_windows：rollup 窗口的幂等与同代冲突判定（C2；幂等键 (node, site, window_start, window_seconds)）----
CREATE TABLE IF NOT EXISTS rollup_windows (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  node_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  window_start DATETIME(3) NOT NULL,
  window_seconds INT UNSIGNED NOT NULL,
  generation INT UNSIGNED NOT NULL,
  content_sha256 CHAR(64) NOT NULL,
  received_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_rollup_windows (node_id, site_id, window_start, window_seconds),
  KEY idx_rollup_windows_time (window_start)
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON rollup_windows TO lingedge_app;

-- ---- access_log_chunks：访问日志文件分片的索引元数据（C2；site_id=0 为未归属）----
-- lingedge:cross-tenant-index=uk_alc_dedupe 至少一次投递 + 服务端去重键（节点分段区间 + site + log_date，跨租户写入路径）
-- lingedge:cross-tenant-index=idx_alc_node 按节点/分段号对账与重放（平台侧）
-- lingedge:cross-tenant-index=idx_alc_date 按 log_date 小批量清理日志索引（平台任务）
CREATE TABLE IF NOT EXISTS access_log_chunks (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL,
  node_id BIGINT UNSIGNED NOT NULL,
  log_date DATE NOT NULL,
  segment_seq BIGINT UNSIGNED NOT NULL,
  start_offset BIGINT UNSIGNED NOT NULL,
  end_offset BIGINT UNSIGNED NOT NULL,
  storage_path VARCHAR(512) NOT NULL,
  file_offset BIGINT UNSIGNED NOT NULL,
  stored_length BIGINT UNSIGNED NOT NULL,
  line_count INT UNSIGNED NOT NULL,
  raw_bytes BIGINT UNSIGNED NOT NULL,
  stored_bytes BIGINT UNSIGNED NOT NULL,
  min_ts DATETIME(3) NOT NULL,
  max_ts DATETIME(3) NOT NULL,
  status_counts_json JSON NULL,
  sha256 CHAR(64) NOT NULL,
  received_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_alc_dedupe (node_id, segment_seq, start_offset, end_offset, site_id, log_date),
  KEY idx_alc_query (tenant_id, site_id, log_date, min_ts),
  KEY idx_alc_node (node_id, segment_seq),
  KEY idx_alc_date (log_date),
  CONSTRAINT ck_access_log_chunks_range CHECK (start_offset <= end_offset),
  CONSTRAINT ck_access_log_chunks_ts CHECK (min_ts <= max_ts)
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON access_log_chunks TO lingedge_app;
