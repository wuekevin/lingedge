---
title: "数据库：DNS、调度、安全、四层代理、观测与告警"
status: draft
created: 2026-11-16
updated: 2026-11-16（依据架构评审修订，见 docs/bmad/architecture-revision-log.md）
---

# DNS、调度、安全、四层代理、观测与告警

> 返回 [README](README.md) · [architecture.md](../architecture/architecture.md)。相关 ADR：008、021、022。简写见 README §2.1。除 `metric_rollups`、告警/通知外，本文件表均为 P3/P4 阶段表（M1 不创建）。

## 1. DNS（P3）

### 1.1 `dns_providers`　**P3**
用途：第三方 DNS Provider 实例（Cloudflare/阿里云/DNSPod/插件）。通用列：C1 + T?。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| provider_type | VARCHAR(48) | N | — | `cloudflare/alidns/dnspod/<plugin_key>` |
| name | VARCHAR(128) | N | — | |
| credential_id | BIGINT UNSIGNED | N | — | FK → provider_credentials(id) RESTRICT |
| status | VARCHAR(16) | N | 'active' | `active/invalid/disabled` |
| settings_json | JSON | Y | — | |
| last_sync_at | DATETIME(3) | Y | — | |

- 索引：`idx_dns_providers_tenant(tenant_id, provider_type)`。

### 1.2 `dns_zones`　**P3**
用途：DNS Zone（自建权威 or Provider 托管）。通用列：C1 + T + SD。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| name | VARCHAR(253) | N | — | 小写区名，无尾点 |
| active_name | VARCHAR(253) | Y | — | 生成列 `IF(deleted_at IS NULL, name, NULL)`，**全局唯一**（同一 Zone 不得被两个租户同时托管） |
| mode | VARCHAR(16) | N | — | `authoritative/provider` |
| provider_id | BIGINT UNSIGNED | Y | — | `mode='provider'` 必填；FK → dns_providers(id) RESTRICT |
| status | VARCHAR(16) | N | 'pending' | `pending/active/error/disabled` |
| soa_serial | BIGINT UNSIGNED | N | 1 | 记录变更递增（整数 SOA 序号） |
| soa_json | JSON | N | — | mname/rname/refresh/retry/expire/minimum（整数） |
| default_ttl | INT UNSIGNED | N | 300 | |
| dnssec_enabled | TINYINT(1) | N | 0 | P6 前恒为 0 |
| zone_version | BIGINT UNSIGNED | N | 1 | DNS 节点同步游标 |

- 唯一：`uk_dns_zones_name(active_name)`。索引：`idx_dns_zones_tenant(tenant_id, status)`。CHECK：mode/status；`mode='provider' = (provider_id IS NOT NULL)`；`dnssec_enabled=0`（P6 放宽）。

### 1.3 `dns_records`　**P3**
通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| zone_id | BIGINT UNSIGNED | N | — | FK → dns_zones(id) CASCADE |
| name | VARCHAR(253) | N | — | 相对名，`@` 表示根 |
| type | VARCHAR(8) | N | — | `A/AAAA/CNAME/TXT/MX/CAA/SRV/SOA/NS` |
| ttl | INT UNSIGNED | N | 300 | |
| rdata | TEXT | N | — | 规范化 RDATA（逐类型校验） |
| priority | SMALLINT UNSIGNED | Y | — | MX/SRV |
| weight | SMALLINT UNSIGNED | Y | — | SRV 权重；A/AAAA 的调度权重见 `dns_routes` |
| port | SMALLINT UNSIGNED | Y | — | SRV |
| route_id | BIGINT UNSIGNED | Y | — | 线路/调度：FK → dns_routes(id) SET NULL |
| managed_by | VARCHAR(16) | N | 'user' | `user/lingedge`（自动生成的 CDN CNAME 等） |
| proxied_site_id | BIGINT UNSIGNED | Y | — | FK → sites(id) SET NULL |
| record_version | BIGINT UNSIGNED | N | 1 | 同步游标 |

- 索引：`idx_dns_records_zone(tenant_id, zone_id, name, type)`、`idx_dns_records_route(route_id)`。CHECK：type/managed_by 取值；`type IN ('MX','SRV') OR priority IS NULL`。
- 业务（Service）：CNAME 与同名其它记录互斥；SOA/NS 受保护。

### 1.4 `dns_routes`　**P3**
用途：智能线路（地域/运营商/权重/默认）。通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| zone_id | BIGINT UNSIGNED | N | — | FK → dns_zones(id) CASCADE |
| name | VARCHAR(128) | N | — | |
| match_type | VARCHAR(16) | N | — | `default/geo/isp/weighted` |
| match_json | JSON | Y | — | 国家/区域/运营商编码列表（依赖 GeoIP，OQ-5） |
| node_pool_id | BIGINT UNSIGNED | Y | — | 应答来源池；FK → node_pools(id) SET NULL |
| answers_json | JSON | Y | — | 静态应答（非池） |
| weight | SMALLINT UNSIGNED | N | 100 | |
| priority | INT | N | 1000 | |
| scheduling_policy_id | BIGINT UNSIGNED | Y | — | FK → scheduling_policies(id) SET NULL |
| enabled | TINYINT(1) | N | 1 | |

- 唯一：`uk_dns_routes(zone_id, name)`。索引：`idx_dns_routes_tenant(tenant_id, zone_id, priority)`。

### 1.5 `dns_nodes`　**补充 · P3**
用途：自建权威 DNS 服务实例（`lingedge-dns`）。平台级。通用列：C1。列：`name VARCHAR(128) N`（唯一）、`status VARCHAR(16) N 'pending_enroll'`（`pending_enroll/online/offline/disabled`）、`ns_hostname VARCHAR(253) Y`、`ipv4/ipv6 VARCHAR(45) Y`、`agent_version VARCHAR(64) Y`、`zone_sync_version BIGINT UNSIGNED N 0`、`last_sync_at/last_heartbeat_at DATETIME(3) Y`。证书台账复用 `node_certificates(subject_type='dns')`。

### 1.6 `dns_sync_tasks`　**补充 · P3**
用途：DNS 同步（Provider API 或 DNS 节点）任务，可重试。通用列：C1 + T。列：`zone_id`（FK CASCADE）、`provider_id`（FK→dns_providers SET NULL）、`dns_node_id`（FK→dns_nodes SET NULL）、`record_id`（FK→dns_records SET NULL）、`op VARCHAR(16)`（`create/update/delete/full_sync`）、`status VARCHAR(16)`（`pending/running/success/failed`）、`attempts INT UNSIGNED`、`next_retry_at DATETIME(3) Y`、`last_error VARCHAR(1024) Y`、`task_id`（FK→tasks SET NULL）。索引 `idx_dns_sync_pending(status, next_retry_at)`。

## 2. 调度（P3）

### 2.1 `scheduling_policies`　**P3**
通用列：C1 + T?（NULL=平台策略）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| name | VARCHAR(128) | N | — | |
| algorithm | VARCHAR(16) | N | — | `weighted/geo/latency/composite` |
| config_json | JSON | N | — | 权重、容量、带宽余量阈值；**防抖参数** `min_probes/fail_threshold/recover_threshold/min_ejection_s`（默认值待基线测试，ADR-021） |
| failover_pool_id | BIGINT UNSIGNED | Y | — | FK → node_pools(id) SET NULL |
| status | VARCHAR(16) | N | 'active' | `active/disabled` |

### 2.2 `node_pools`　**P3**
用途：节点池（主用/备用池）。通用列：C1 + T?。列：`name VARCHAR(128) N`、`kind VARCHAR(8) N`（`primary/backup`）、`cluster_id`（FK→clusters SET NULL）、`region_id`（FK→regions SET NULL）、`isp_line_id`（FK→isp_lines SET NULL）、`description VARCHAR(255) Y`。唯一 `uk_node_pools(tenant_key, name)`。

### 2.3 `node_pool_members`　**补充 · P3**
用途：池成员与调度状态（摘除/恢复，含防抖状态）。C2（含 `updated_at`）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| node_pool_id | BIGINT UNSIGNED | N | — | FK → node_pools(id) CASCADE |
| node_id | BIGINT UNSIGNED | N | — | FK → nodes(id) CASCADE |
| weight | SMALLINT UNSIGNED | N | 100 | |
| status | VARCHAR(16) | N | 'active' | `active/ejected/maintenance` |
| ejected_at | DATETIME(3) | Y | — | |
| eject_reason | VARCHAR(255) | Y | — | |
| consecutive_fail_windows | INT UNSIGNED | N | 0 | 防抖状态 |
| consecutive_ok_windows | INT UNSIGNED | N | 0 | |
| last_change_at | DATETIME(3) | Y | — | |

- 唯一：`uk_node_pool_members(node_pool_id, node_id)`。每次翻转写 `node_events` 与调度日志（`event_type='sched.eject'/'sched.restore'`）。

### 2.4 `health_checks`　**P3**
用途：统一健康检查定义（节点/源站/DNS 应答/四层后端）；M1 的源站检查参数在 `origin_groups.health_check_json`，P3 起可迁移到此表。通用列：C1 + T?。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| target_type | VARCHAR(16) | N | — | `node/origin/dns_answer/l4_backend` |
| target_id | BIGINT UNSIGNED | N | — | 多态，无 FK |
| protocol | VARCHAR(8) | N | — | `http/https/tcp/tls/dns/udp` |
| config_json | JSON | N | — | 协议专属：HTTP(path/期望状态/Host)、TCP(连接)、UDP(探测载荷/期望响应，**与 TCP 字段不同**，FR-103) |
| interval_s | INT UNSIGNED | N | 10 | 数值默认待基线测试 |
| timeout_ms | INT UNSIGNED | N | 3000 | |
| fail_threshold | SMALLINT UNSIGNED | N | 3 | |
| recover_threshold | SMALLINT UNSIGNED | N | 3 | |
| enabled | TINYINT(1) | N | 1 | |

- 索引：`idx_health_checks_target(target_type, target_id)`。

### 2.5 `reporters`　**补充 · P3**
Reporter 探针实例。平台级。通用列：C1。列：`name VARCHAR(128) N`（唯一）、`region_id`（FK→regions SET NULL）、`platform VARCHAR(16) N`（`linux-amd64/linux-arm64/windows-amd64`）、`status VARCHAR(16) N 'pending_enroll'`、`version VARCHAR(64) Y`、`last_seen_at DATETIME(3) Y`。证书复用 `node_certificates(subject_type='reporter')`。

## 3. WAF 与安全（P4）

### 3.1 `waf_rulesets`　**P4**
通用列：C1 + T?（NULL=平台托管规则集）。列：`name VARCHAR(128) N`、`kind VARCHAR(8) N`（`managed/custom`）、`version VARCHAR(32) N`、`mode VARCHAR(8) N 'block'`（`observe/block`，默认模式；站点可覆盖）、`status VARCHAR(16) N 'active'`、`description VARCHAR(512) Y`。唯一 `uk_waf_rulesets(tenant_key, name, version)`。

### 3.2 `waf_rules`　**P4**
通用列：C1 + T?。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| ruleset_id | BIGINT UNSIGNED | N | — | FK → waf_rulesets(id) CASCADE |
| rule_ref | VARCHAR(64) | N | — | 规则编号，如 `LE-SQLI-0001`（集合内唯一） |
| category | VARCHAR(16) | N | — | `sqli/xss/traversal/custom/bot/other` |
| severity | VARCHAR(8) | N | 'medium' | `low/medium/high/critical` |
| action | VARCHAR(16) | N | — | `block/allow/log/challenge` |
| doc_json | JSON | N | — | 与 `site_rules.doc_json` **同一条件 IR**（统一规则引擎，FR-63） |
| priority | INT | N | 1000 | |
| enabled | TINYINT(1) | N | 1 | |
| version | INT UNSIGNED | N | 1 | |

- 唯一：`uk_waf_rules(ruleset_id, rule_ref)`。索引：`idx_waf_rules_set(ruleset_id, priority)`。

### 3.3 `site_waf_bindings`　**补充 · P4**
C2 + T。`site_id`（FK→sites(tenant_id,id) CASCADE）、`ruleset_id`（FK→waf_rulesets RESTRICT）、`mode_override VARCHAR(8) Y`（`observe/block`）、`enabled TINYINT(1)`。唯一 `uk_swb(site_id, ruleset_id)`。

### 3.4 `ip_lists`　**P4**
用途：IP/ASN/国家地区的黑白名单集合。通用列：C1 + T?。列：`name VARCHAR(128) N`、`list_type VARCHAR(8) N`（`ip/asn/country`）、`action VARCHAR(8) N`（`allow/deny`）、`site_id`（可空，FK→sites(id) CASCADE；NULL=租户/平台级）、`description VARCHAR(255) Y`、`entry_count INT UNSIGNED N 0`。唯一 `uk_ip_lists(tenant_key, name)`。
**`ip_list_entries`（补充·P4）**：C2；`ip_list_id` FK CASCADE、`value VARCHAR(64) N`（CIDR/ASN 数字/ISO 国家码，按 `list_type` 校验）、`note VARCHAR(255) Y`、`expires_at DATETIME(3) Y`；唯一 `uk_ip_list_entries(ip_list_id, value)`。M1 的 `access_control` 规则以内联 CIDR 写入规则文档，不依赖本表。

### 3.5 `security_events`　**P4**（高频，无外键）
用途：安全事件的**采样/聚合**记录（逐请求攻击日志在文件分片，ADR-008；速率受 Agent 限制）。C2。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| tenant_id / site_id / node_id | BIGINT UNSIGNED | N | — | 无 FK |
| event_time | DATETIME(3) | N | — | 分区键；主键 `(id, event_time)` |
| event_type | VARCHAR(16) | N | — | `waf_block/waf_log/cc_limit/bot/acl` |
| rule_ref | VARCHAR(64) | Y | — | |
| action | VARCHAR(16) | N | — | `block/allow/log/challenge` |
| client_ip | VARBINARY(16) | N | — | |
| country | CHAR(2) | Y | — | |
| host | VARCHAR(253) | Y | — | |
| method | VARCHAR(8) | Y | — | |
| uri_path | VARCHAR(512) | Y | — | 已脱敏、已截断 |
| summary_json | JSON | Y | — | 请求摘要（敏感值脱敏） |
| request_id | CHAR(26) | Y | — | |
| hit_count | INT UNSIGNED | N | 1 | 聚合计数（同窗口同规则同 IP 合并） |

- 索引：`idx_sec_events_site(tenant_id, site_id, event_time)`、`idx_sec_events_rule(tenant_id, rule_ref, event_time)`、`idx_sec_events_ip(tenant_id, client_ip, event_time)`。按日/月 `RANGE COLUMNS(event_time)` 分区，**含 `pmax` 兜底分区**，滚动由 `lingedge_maint` 表级 ALTER/DROP 执行（README §5.1）；分区表无外键。保留 30 天 `[ASSUMPTION]`。

## 4. 四层代理（P4）

> TCP 与 UDP 分别定义配置与健康检查语义；**不含任何 HTTP 缓存/规则字段**（FR-104）。

### 4.1 `l4_applications`　**P4**
通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| name | VARCHAR(128) | N | — | |
| protocol | VARCHAR(4) | N | — | `tcp/udp` |
| cluster_id | BIGINT UNSIGNED | N | — | FK → clusters(id) RESTRICT |
| status | VARCHAR(16) | N | 'active' | `active/disabled` |
| lb_policy | VARCHAR(16) | N | 'weighted' | `weighted/ip_hash/least_conn`（`least_conn` 仅 tcp） |
| proxy_protocol | VARCHAR(4) | N | 'off' | 到后端的 PROXY Protocol：`off/v1/v2`（UDP 仅 `off`） |
| conn_limit_json | JSON | Y | — | tcp：`max_conn_per_ip/max_conn_total`；udp：`max_sessions_per_ip/session_timeout_s` |
| acl_json | JSON | Y | — | 源 IP 访问控制（allow/deny CIDR） |
| health_check_json | JSON | N | — | **tcp**：`{type:"connect",interval,timeout,thresholds}`；**udp**：`{type:"probe",send_hex,expect_hex_or_regex,...}` |
| timeouts_json | JSON | Y | — | |
| config_version | BIGINT UNSIGNED | N | 1 | 同站点 `config_version` 语义，编入 `l4` 配置块 |

- 唯一：`uk_l4_apps(tenant_id, name)`。CHECK：协议约束 `protocol='tcp' OR proxy_protocol='off'`。

### 4.2 `l4_listeners`　**P4**
C1 + T。`app_id`（FK→l4_applications CASCADE）、`protocol VARCHAR(4) N`（冗余，用于端口冲突唯一性）、`cluster_id`（冗余，FK→clusters RESTRICT）、`listen_address VARCHAR(45) Y`、`listen_port SMALLINT UNSIGNED N`、`addr_key VARCHAR(45) N`（生成列 `COALESCE(listen_address,'*')`）。唯一 **`uk_l4_listeners_bind(cluster_id, protocol, addr_key, listen_port)`**（同集群同协议端口不冲突）；CHECK 端口 1–65535，且不得与保留端口（80/443 等，由 Service 校验）冲突。

### 4.3 `l4_backends`　**P4**
C1 + T。`app_id`（FK CASCADE）、`address VARCHAR(253) N`（SSRF 校验）、`port SMALLINT UNSIGNED N`、`weight SMALLINT UNSIGNED N 100`、`backup TINYINT(1) N 0`、`enabled TINYINT(1) N 1`、`max_conns INT UNSIGNED Y`。索引 `idx_l4_backends_app(tenant_id, app_id)`。

## 5. 指标与探测

### 5.1 `metric_rollups`　**M1**（高频，无外键）

用途：聚合指标（图表、告警、计量输入），**长表**结构，同一份 rollup 复用于图表/告警/计费（addendum A）。Agent 产出 60 秒窗口（窗口 ID 确定性、带 `generation`），Server 下采样 5m/1h/1d。C2（含 `updated_at`）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| granularity | VARCHAR(4) | N | — | `1m/5m/1h/1d` |
| window_start | DATETIME(3) | N | — | UTC，对齐粒度（毫秒部分恒为 0；与契约 §9.1 `DATETIME(3)` 一致） |
| dim_type | VARCHAR(12) | N | — | `node/site/site_node/rule/cluster/dns_zone/l4_app/platform` |
| dim_id | BIGINT UNSIGNED | N | 0 | 维度对象 id（`platform` 为 0） |
| node_id | BIGINT UNSIGNED | N | 0 | 0=跨节点汇总；`site_node` 时为具体节点 |
| tenant_id | BIGINT UNSIGNED | N | 0 | 0=平台级；站点类维度必填（租户视图隔离） |
| metric | VARCHAR(48) | N | — | 见下“metric 词表” |
| dim_sub | VARCHAR(64) | N | '' | 子维度（规则 ID、状态码、国家；直方图为 `<bounds_version>:<桶序号>`） |
| agg | VARCHAR(4) | N | 'sum' | `sum/max/last`（`bandwidth_peak_bps` 为 `max`） |
| value | BIGINT | N | — | 整数值（平均值用 `*_sum` + `requests` 在查询时计算） |
| generation | INT UNSIGNED | N | 0 | 窗口修订代数（proto `Rollup.generation`）：**更大者覆盖，同代同值 DUPLICATE，同代异值 CONFLICT（保留首次并记事件，不静默覆盖）**。由 `rollup_windows` 判定 |
| updated_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) ON UPDATE | |

- 唯一：`uk_metric_rollups(granularity, window_start, dim_type, dim_id, node_id, metric, dim_sub)`。
- 索引：`idx_metric_rollups_tenant(tenant_id, dim_type, dim_id, granularity, window_start)`、`idx_metric_rollups_time(granularity, window_start)`（清理）。
- 写入（1m，来自 `ReportRollups`）：先在 `rollup_windows` 判定（见下），仅当“新窗口或更高 generation”才 `INSERT … ON DUPLICATE KEY UPDATE value=IF(VALUES(generation)>generation, VALUES(value), value), generation=GREATEST(generation, VALUES(generation))`（已实测）。5m/1h/1d 为 Scheduler 任务 `rollup.aggregate` 由 1m 幂等重算，不经 Agent。
- **metric 词表（以本表为准，与 proto `RollupMetrics` 一一对应，2026-11-16 对齐）**：`requests`、`bytes_out`、`bytes_in`、`status_1xx/2xx/3xx/4xx/5xx`、`cache_hit/miss/expired/stale/bypass/revalidated/updating/none`、`origin_requests`、`origin_bytes`（= proto `origin_bytes_in`）、`origin_latency_ms_sum`、`origin_latency_count`、`ttfb_ms_sum`（proto 新增 `ttfb_ms_sum=25`）、`rule_hits`（`dim_type='site_node'`，`dim_sub=<rule_uid>`，基数受限）、`websocket_upgrades`、`bandwidth_peak_bps`（`agg='max'`；proto 新增 `bandwidth_peak_bps=26`，Agent 由日志秒级 `ts` 与 `bytes_sent` 按 1 秒分桶计算窗口内峰值）、`req_time_bucket`（`dim_sub='<latency_bounds_version>:<桶序号>'`，承载 proto `request_time_buckets` 直方图）、`dns_queries`、`waf_events`（P3/P4）。**已删除**无生产者的 `conn_active_max`（连接数走 Heartbeat 的 `NodeStatus.active_connections`）。ADR-010 的 `peak_bandwidth_bps` 作废，统一 `bandwidth_peak_bps`。
- 保留：1m 7 天 / 5m 30 天 / 1h 180 天 / 1d 730 天 `[ASSUMPTION]`；按 `window_start` 小批量删除。
- 计费：`usage_records` 只取**一种粒度**（300 秒），由 `dim_type='site_node'` 的已闭合 1m 窗口（5 个对齐窗口均已 finalized 且取各自最高 generation）幂等重算 upsert（billing-support §4.1）。

### 5.1.1 `rollup_windows`　**补充 · M1**（高频，无外键）

用途：rollup 窗口的**幂等与同代冲突判定**（proto `Rollup` 幂等键 `(node_uid, site_uid, window_start, window_seconds)`；`ReportRollups` 逐窗口返回 ACCEPTED/DUPLICATE/REPLACED/CONFLICT）。C2。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| node_id | BIGINT UNSIGNED | N | — | 无 FK（高频） |
| site_id | BIGINT UNSIGNED | N | 0 | 0=节点级汇总 |
| window_start | DATETIME(3) | N | — | |
| window_seconds | INT UNSIGNED | N | — | M1 固定 60 |
| generation | INT UNSIGNED | N | — | 当前保留的最高 generation |
| content_sha256 | CHAR(64) | N | — | 该 generation 的 `RollupMetrics` 规范化序列化哈希：同代比对用 |
| received_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) | |

- 唯一：`uk_rollup_windows(node_id, site_id, window_start, window_seconds)`。判定（同一事务内与 `metric_rollups` 写入一起）：无行 → ACCEPTED；`generation` 更大 → REPLACED（更新本行并重写该窗口的 metric 行）；同代且 `content_sha256` 相同 → DUPLICATE；同代不同 → **CONFLICT**（保留首次，写 `node_events`/监控事件，不覆盖）。保留：与 1m `metric_rollups` 同期。

### 5.2 `probe_results`　**P3**（高频，无外键）
用途：Reporter 探测结果（调度防抖输入，FR-95）。C2。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| probed_at | DATETIME(3) | N | — | 分区键；主键 `(id, probed_at)` |
| reporter_id | BIGINT UNSIGNED | N | — | 无 FK |
| region_id | BIGINT UNSIGNED | Y | — | |
| target_type | VARCHAR(12) | N | — | `node/origin/dns/site` |
| target_id | BIGINT UNSIGNED | N | — | |
| probe_type | VARCHAR(8) | N | — | `http/https/tcp/tls/dns` |
| ok | TINYINT(1) | N | — | |
| latency_ms | INT UNSIGNED | Y | — | |
| ttfb_ms | INT UNSIGNED | Y | — | |
| status_code | SMALLINT UNSIGNED | Y | — | |
| failure_reason | VARCHAR(16) | Y | — | `timeout/refused/tls_error/dns_error/http_status/other` |
| detail_json | JSON | Y | — | |

- 索引：`idx_probe_target(target_type, target_id, probed_at)`、`idx_probe_region(region_id, probed_at)`。分区 `RANGE COLUMNS(probed_at)` + `pmax`，保留：14 天 `[ASSUMPTION]`，`DROP PARTITION`（README §5.1）。

## 6. 告警与通知（P2）

### 6.1 `notification_channels`　**补充 · P2**
通用列：C1 + T?。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| type | VARCHAR(12) | N | — | `email/telegram/webhook` |
| name | VARCHAR(128) | N | — | |
| config_json | JSON | N | — | 非敏感：SMTP host/port/from、Telegram chat_id、Webhook 方法/Header 名（值在凭据） |
| credential_id | BIGINT UNSIGNED | Y | — | FK → provider_credentials(id) SET NULL（SMTP 口令、Bot Token、Webhook 签名密钥） |
| allow_private_target | TINYINT(1) | N | 0 | Webhook SSRF 例外白名单开关，仅平台管理员可置 1（NFR-10） |
| enabled | TINYINT(1) | N | 1 | |
| last_test_at | DATETIME(3) | Y | — | |

- 唯一：`uk_notif_channels(tenant_key, name)`。

### 6.2 `notification_templates`　**补充 · P2**
C1 + T?。列：`event_type VARCHAR(64) N`、`channel_type VARCHAR(12) N`、`locale VARCHAR(8) N`、`subject VARCHAR(255) Y`、`body TEXT N`、`is_builtin TINYINT(1) N 0`。唯一 `uk_notif_tpl(tenant_key, event_type, channel_type, locale)`。模板渲染使用受限占位符，不执行任意表达式。

### 6.3 `alert_rules`　**P2**
通用列：C1 + T?。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| name | VARCHAR(128) | N | — | |
| event_type | VARCHAR(48) | N | — | `node.offline/node.recovered/origin.down/dns.failover/bandwidth.anomaly/cert.expiring/waf.anomaly/wallet.low/deploy.failed/upgrade.failed` |
| condition_json | JSON | Y | — | 阈值型规则的阈值；**无内置虚构默认**，由运营者配置（FR-117） |
| severity | VARCHAR(8) | N | 'warning' | `info/warning/critical` |
| dedup_window_s | INT UNSIGNED | N | 3600 | 去重窗口（可配；默认待确认 OQ-7） |
| send_recovery | TINYINT(1) | N | 1 | 恢复通知 |
| enabled | TINYINT(1) | N | 1 | |

- 索引：`idx_alert_rules_event(tenant_id, event_type, enabled)`。

### 6.4 `alert_rule_channels`　**补充 · P2**
C2。`alert_rule_id`（FK→alert_rules CASCADE）、`channel_id`（FK→notification_channels CASCADE）；唯一 `uk_arc(alert_rule_id, channel_id)`。

### 6.5 `alert_events`　**P2**
用途：告警事件（含去重与恢复状态）。通用列：C1 + T?。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| alert_rule_id | BIGINT UNSIGNED | Y | — | FK → alert_rules(id) SET NULL |
| dedup_key | VARCHAR(190) | N | — | 如 `node.offline:<node_uid>` |
| status | VARCHAR(12) | N | 'firing' | `firing/resolved/suppressed` |
| open_key | VARCHAR(255) | Y | — | 生成列 `IF(status='firing', CONCAT(COALESCE(tenant_id,0),':',dedup_key), NULL)` STORED，**唯一** → 同一故障同一时刻仅一个 firing 事件（长度由 190 提到 255：租户前缀最长 20 位 + `:` + 190 位 `dedup_key`；基列 `tenant_id` 的 FK 为 RESTRICT，实测可用） |
| severity | VARCHAR(8) | N | — | |
| title | VARCHAR(255) | N | — | |
| subject_type / subject_uid | VARCHAR(32) / VARCHAR(64) | Y | — | |
| payload_json | JSON | Y | — | |
| fire_count | INT UNSIGNED | N | 1 | |
| first_fired_at / last_fired_at | DATETIME(3) | N | — | |
| resolved_at | DATETIME(3) | Y | — | |
| notified_at / recovery_notified_at | DATETIME(3) | Y | — | |

- 唯一：`uk_alert_events_open(open_key)`。索引：`idx_alert_events_tenant(tenant_id, status, last_fired_at)`、`idx_alert_events_rule(alert_rule_id)`。保留：1 年 `[ASSUMPTION]`。

### 6.6 `alert_notifications`　**补充 · P2**
C2。`alert_event_id`（FK→alert_events CASCADE）、`channel_id`（FK→notification_channels RESTRICT）、`kind VARCHAR(8)`（`fire/recover`）、`status VARCHAR(8)`（`pending/sent/failed`）、`attempts INT UNSIGNED`、`error VARCHAR(512) Y`、`sent_at DATETIME(3) Y`。唯一 `uk_alert_notif(alert_event_id, channel_id, kind)`（发送幂等）。
