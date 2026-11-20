---
title: "数据库：租户、站点、域名与源站"
status: draft
created: 2026-11-16
updated: 2026-11-16（依据架构评审修订，见 docs/bmad/architecture-revision-log.md）
---

# 租户、站点、域名与源站

> 返回 [README](README.md) · [architecture.md](../architecture/architecture.md)。简写 C1/T/SD 见 README §2.1。站点子表一律声明复合外键 `FK→sites(tenant_id,id)`（README §2.4）。

## 1. 租户

### 1.1 `tenants`　**M1**

用途：资源与数据隔离的最小单位。M1 即多租户模型：安装时创建一个默认租户（自用模式，`is_default=1`），运营者在 Admin 以 `X-Tenant-ID` 代其操作（D-13）。平台级表（无 tenant_id）。通用列：C1 + SD。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| name | VARCHAR(128) | N | — | |
| slug | VARCHAR(63) | N | — | 小写字母数字连字符，唯一 |
| status | VARCHAR(16) | N | 'active' | `active/suspended/closed/pending` |
| is_default | TINYINT(1) | N | 0 | 默认租户标记 |
| default_flag | TINYINT | Y | — | 生成列 `IF(is_default=1,1,NULL)`，唯一 → 至多一个默认租户 |
| owner_user_id | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| reseller_id | BIGINT UNSIGNED | Y | — | FK → resellers(id) SET NULL（P5 追加，expand） |
| contact_email | VARCHAR(255) | Y | — | |
| locale | VARCHAR(8) | N | 'zh-CN' | |
| settings_json | JSON | Y | — | 租户级偏好 |
| suspended_reason | VARCHAR(255) | Y | — | |

- 唯一：`uk_tenants_slug(slug)`、`uk_tenants_default(default_flag)`。索引：`idx_tenants_status(status)`、`idx_tenants_reseller(reseller_id)`。
- CHECK：status 取值；slug 正则 `REGEXP_LIKE(slug,'^[a-z0-9][a-z0-9-]{0,62}$')`。
- 保留：软删除后保留；物理清理需其所有子数据先清理，待用户确认（OQ-6）。

### 1.2 `tenant_members`　**M1**

用途：用户在租户中的成员关系（角色授予在 `user_roles`）。C2 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| user_id | BIGINT UNSIGNED | N | — | FK → users(id) CASCADE |
| status | VARCHAR(16) | N | 'active' | `active/invited/removed` |
| is_owner | TINYINT(1) | N | 0 | |
| invited_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| joined_at | DATETIME(3) | Y | — | |

- 唯一：`uk_tenant_members(tenant_id, user_id)`；索引 `idx_tenant_members_user(user_id)`。CHECK：status 取值。

### 1.3 `tenant_quotas`　**M1**（模型）/ 强制 P5

用途：租户配额（FR-105）。M1 默认租户无限制（`limit_value NULL`）。C2（含 `updated_at`）+ T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| quota_key | VARCHAR(64) | N | — | `sites`、`domains_per_site`、`rules_per_site`、`api_tokens`、`members`、`cache_tasks_per_day`、`certificates`、`l4_applications`… |
| limit_value | BIGINT | Y | — | NULL=不限；≥0 |
| source | VARCHAR(16) | N | 'manual' | `manual/plan/default` |
| subscription_id | BIGINT UNSIGNED | Y | — | FK → subscriptions(id) SET NULL（P5 追加） |
| updated_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) ON UPDATE | |

- 唯一：`uk_tenant_quotas(tenant_id, quota_key)`。CHECK：`limit_value IS NULL OR limit_value >= 0`。

## 2. 站点

### 2.1 `site_groups`　**M1**

用途：站点分组（FR-33）。通用列：C1 + T。列：`name VARCHAR(128) N`、`description VARCHAR(512) Y`、`sort_order INT N 0`。唯一 `uk_site_groups(tenant_id, name)`。

### 2.2 `sites`　**M1**

用途：租户下的加速对象，站点聚合根；含“工作态 vs 已发布态”指针（ADR-002）。通用列：C1 + T + SD。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| name | VARCHAR(128) | N | — | |
| group_id | BIGINT UNSIGNED | Y | — | FK → site_groups(id) SET NULL |
| status | VARCHAR(16) | N | 'active' | `active/disabled/deleting` |
| description | VARCHAR(512) | Y | — | |
| config_version | BIGINT UNSIGNED | N | 1 | 工作态变更计数（域名/源站/设置/规则/缓存任一变更 +1）。租户发布请求带 `config_version`（所见即所发），与当前值不符 → `412 resource.precondition_failed`（MIN-02） |
| published_config_version | BIGINT UNSIGNED | N | 0 | 最近一次**部署成功**的 `config_version`（**仅在部署成功后前移**，不在发布请求/构建时前移，D-PUB；失败/回退/暂停中的修订不使站点显示为已发布） |
| published_block_sha256 | CHAR(64) | Y | — | 该站点**已成功发布**块的哈希（→ `config_blocks.sha256`；同样仅部署成功后前移；用于判断是否有未发布变更与回滚） |
| published_version_no | BIGINT UNSIGNED | N | 0 | 当前线上 `site_versions.version_no`（站点线性版本，MIN-14；部署成功后前移） |
| published_at | DATETIME(3) | Y | — | |
| published_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| last_deployment_id | BIGINT UNSIGNED | Y | — | FK → deployments(id) SET NULL（循环外键，迁移 000012 末尾 `ALTER` 追加；本列在 000007 先建为无 FK 普通列） |
| purge_requested_at | DATETIME(3) | Y | — | 删除/停用后的节点配置清除确认时间（FR-47） |

- 唯一：`uk_sites_tenant_name(tenant_id, name)`（软删除后名称复用见下）、`uk_sites_tenant_id(tenant_id, id)`（复合外键目标）。索引：`idx_sites_tenant_status(tenant_id, status)`、`idx_sites_group(tenant_id, group_id)`。
- 名称复用：`active_name` 生成列 `IF(deleted_at IS NULL, name, NULL)` 取代 `uk_sites_tenant_name` 的唯一语义（实现二选一，推荐生成列）。
- 派生：`config_dirty = config_version > published_config_version`（Service 计算，不存列）。
- CHECK：status 取值；`published_config_version <= config_version`。（`group_id/published_by/last_deployment_id` 为 SET NULL FK 列，不被 CHECK 引用）
- 派生：`publish_state`（只读，OpenAPI `PublishState`）= `never_published`（`published_version_no=0`）/ `published`（`config_version = published_config_version`）/ `unpublished_changes`；部署进度（排队/发布中/失败/回滚）来自最近的 `publish_queue`/`deployments`，租户只见脱敏视图（ADR-009 §5）。
- 保留：软删除，待所有节点确认移除配置后物理清理（保留期待用户确认）。

### 2.3 `site_domains`　**M1**

用途：站点的访问主机名（FR-48）。通用列：C1 + T + SD。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| site_id | BIGINT UNSIGNED | N | — | FK→sites(tenant_id,id) RESTRICT |
| hostname | VARCHAR(253) | N | — | 小写，IDN 已转 punycode；可为 `*.example.com` 通配 |
| active_hostname | VARCHAR(253) | Y | — | 生成列 `IF(deleted_at IS NULL, hostname, NULL)` STORED |
| is_wildcard | TINYINT(1) | N | 0 | 生成/应用赋值：`hostname LIKE '*.%'` |
| is_primary | TINYINT(1) | N | 0 | |
| certificate_id | BIGINT UNSIGNED | Y | — | 显式绑定证书；复合 FK `(tenant_id, certificate_id)` → certificates(tenant_id, id) **RESTRICT**（CC-1：库层强制同租户；复合 FK 不能 SET NULL，证书本身软删除，物理清理前须先解绑） |
| verification_status | VARCHAR(16) | N | 'pending' | `exempt/pending/verified/failed`（F-1）。**默认 `pending`**（原默认 `exempt` 在漏写校验分支时会把未验证域名当作豁免，MIN-11）；自用模式由 Service 在创建时**显式**置 `exempt`（`verification_method='admin_exempt'`），商业模式（P5）必须走 `verified` |
| verification_method | VARCHAR(16) | Y | — | `txt/cname/file/admin_exempt` |
| verification_token | VARCHAR(64) | Y | — | |
| verified_at | DATETIME(3) | Y | — | |
| last_check_at | DATETIME(3) | Y | — | |
| cname_status | VARCHAR(16) | N | 'unknown' | `unknown/ok/mismatch`（解析是否指向 LingEdge，仅展示） |

- 唯一：`uk_site_domains_active_host(active_hostname)` —— **全局**活动主机名唯一，跨租户（域名被他租户占用即冲突，FR-48）。
- 索引：`idx_site_domains_site(tenant_id, site_id)`、`idx_site_domains_cert(tenant_id, certificate_id)`（CC-1：以 tenant_id 打头，同时支撑复合外键）。
- CHECK：`REGEXP_LIKE(hostname, '^(\\*\\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z0-9-]{2,63}$')`；`BINARY hostname = BINARY LOWER(hostname)`（应用层小写化的库层兜底，README §2.2，已实测）；verification_status/cname_status 取值。
- 业务（Service）：通配符与具体主机名的重叠判定规则待 PRD 明确（见 architecture 反馈 F-1）；非 `exempt` 才允许在商业模式下发布。
- 保留：软删除；活动唯一释放。

### 2.4 `site_settings`　**M1**

用途：站点 1:1 设置：**真实 IP 传递（FR-70）**、TLS、代理/监听参数。通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| site_id | BIGINT UNSIGNED | N | — | 唯一；FK→sites(tenant_id,id) RESTRICT |
| real_ip_xff_enabled | TINYINT(1) | N | 1 | 向源站传 X-Forwarded-For（OpenAPI `xff_enabled`；列名与 API 字段映射：去 `real_ip_` 前缀并 `*_xff/xri/custom_*` 对应 `xff_enabled/xri_enabled/custom_enabled/custom_header`，ADR-006 §5） |
| real_ip_xri_enabled | TINYINT(1) | N | 1 | 传 X-Real-IP |
| real_ip_custom_enabled | TINYINT(1) | N | 0 | 传自定义 Header |
| real_ip_custom_header | VARCHAR(64) | Y | — | 如 `Ling-Client-IP`（ADR-006 规则校验） |
| xff_policy | VARCHAR(24) | N | 'overwrite' | `overwrite` / `append_trusted` |
| trusted_proxies_json | JSON | Y | — | 站点级入站可信代理 CIDR 覆盖（NULL=继承集群）。**站点级只支持 `X-Forwarded-For` 链**；`proxy_protocol` 入站是集群级 `node_global` 参数（平台可写），不在此处。CIDR 须满足最小前缀（IPv4 ≥/8、IPv6 ≥/32）且不含回环/多播/保留段/节点自身地址（`realip.trusted_proxy_forbidden` + `details.reason`，ADR-006 §2） |
| force_https | TINYINT(1) | N | 0 | |
| http_to_https_redirect | TINYINT(1) | N | 0 | |
| tls_min_version | VARCHAR(8) | N | 'TLSv1.2' | `TLSv1.2/TLSv1.3` |
| http2_enabled | TINYINT(1) | N | 1 | |
| hsts_json | JSON | Y | — | `{enabled,max_age,include_subdomains}`（数值整数） |
| default_certificate_id | BIGINT UNSIGNED | Y | — | 复合 FK `(tenant_id, default_certificate_id)` → certificates(tenant_id, id) **RESTRICT**（CC-1，同上） |
| origin_tls_verify | TINYINT(1) | N | 1 | 回源默认校验源站 TLS（NFR-17）；关闭告警 |
| listen_ports_json | JSON | Y | — | 自定义端口（FR-51） |
| proxy_json | JSON | N | — | 超时、缓冲、WebSocket/SSE 等受 Schema 约束的代理参数 |
| waf_json | JSON | Y | — | P4：模式与绑定摘要（细节在 `site_waf_bindings`） |
| extra_json | JSON | Y | — | 预留 |

- 唯一：`uk_site_settings_site(site_id)`。索引：`idx_site_settings_tenant(tenant_id)`、`idx_site_settings_cert(tenant_id, default_certificate_id)`（CC-1）。
- CHECK：
  - `ck_site_settings_realip_header`：`real_ip_custom_enabled=0 OR (real_ip_custom_header IS NOT NULL AND REGEXP_LIKE(real_ip_custom_header,'^[A-Za-z][A-Za-z0-9-]{0,63}$'))`（保留名清单在 Service/ADR-006 校验，数据库只做语法兜底）。
  - `xff_policy IN ('overwrite','append_trusted')`；`tls_min_version IN ('TLSv1.2','TLSv1.3')`。
- 变更：每次修改 `sites.config_version` +1（同事务）。

### 2.5 `site_cluster_bindings`　**补充 · M1**

用途：站点 ↔ 集群绑定（一集群服务多站点；站点可绑定多集群）。**由平台管理，租户不可见、不可改**（租户 API 不含集群标识，ADR-009 §5 R-T1）：建站时按 `system_settings.placement.default_cluster_uids` 默认绑定，平台经 `PUT /sites/{siteId}/cluster-bindings` 调整；未绑定时发布返回 `409 site.no_cluster_bound`。C2 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| site_id | BIGINT UNSIGNED | N | — | FK→sites(tenant_id,id) CASCADE |
| cluster_id | BIGINT UNSIGNED | N | — | FK → clusters(id) RESTRICT |
| status | VARCHAR(16) | N | 'active' | `active/draining` |

- 唯一：`uk_site_cluster(site_id, cluster_id)`；索引 `idx_scb_cluster(cluster_id)`。
- 业务：绑定时校验“同一节点上主机名不跨集群冲突”（F-3）。

## 3. 源站

### 3.1 `origin_groups`　**M1**

用途：源站组（负载策略、重试、健康检查配置）。通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| site_id | BIGINT UNSIGNED | N | — | FK→sites(tenant_id,id) RESTRICT |
| name | VARCHAR(128) | N | — | |
| is_default | TINYINT(1) | N | 0 | 站点默认回源组 |
| lb_policy | VARCHAR(24) | N | 'weighted' | `weighted/primary_backup/ip_hash` |
| retry_json | JSON | Y | — | 重试次数、`proxy_next_upstream` 白名单条件 |
| health_check_enabled | TINYINT(1) | N | 1 | |
| health_check_json | JSON | Y | — | M1：HTTP/TCP 检查参数（间隔、超时、路径、期望状态、阈值）；P3 起可引用 `health_checks` |
| default_flag | BIGINT UNSIGNED | Y | — | 生成列 `IF(is_default=1, site_id, NULL)` STORED，唯一 → 每站点至多一个默认组（基列 `site_id` 的 FK 为 RESTRICT，实测 STORED 可用，README §2.2） |

- 唯一：`uk_origin_groups_name(site_id, name)`、`uk_origin_groups_default(default_flag)`、**`uk_origin_groups_site_id(site_id, id)`**（供 `origins` 的复合外键引用，MIN-13）。索引：`idx_origin_groups_tenant(tenant_id, site_id)`。CHECK：lb_policy 取值。

### 3.2 `origins`　**M1**

用途：源站（回源目标）。通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| origin_group_id | BIGINT UNSIGNED | N | — | 复合 FK `(site_id, origin_group_id)` → origin_groups(site_id, id) ON DELETE CASCADE：跨站点/跨租户绑定源站组在库层不可能（MIN-13，实测可用） |
| site_id | BIGINT UNSIGNED | N | — | 冗余；FK→sites(tenant_id,id) RESTRICT |
| address | VARCHAR(253) | N | — | 主机名或 IP；Agent 解析并经 SSRF 守卫（NFR-10） |
| port | SMALLINT UNSIGNED | N | — | 1–65535 |
| protocol | VARCHAR(8) | N | 'http' | `http/https` |
| weight | SMALLINT UNSIGNED | N | 100 | 0–1000 |
| role | VARCHAR(8) | N | 'primary' | `primary/backup` |
| enabled | TINYINT(1) | N | 1 | |
| host_header | VARCHAR(253) | Y | — | 回源 Host 覆盖 |
| sni | VARCHAR(253) | Y | — | 回源 SNI |
| tls_verify | TINYINT(1) | Y | — | NULL=继承站点 `origin_tls_verify` |
| timeouts_json | JSON | Y | — | connect/read/send（毫秒整数） |

- 索引：`idx_origins_group(origin_group_id)`、`idx_origins_tenant(tenant_id, site_id)`。
- CHECK：`port BETWEEN 1 AND 65535`；`weight <= 1000`；protocol/role 取值。
- 业务（Service，非 DB）：地址 SSRF 校验（拒回环/链路本地/内网/元数据，**除非命中平台白名单 `system_settings.ssrf.allow_cidrs`**；白名单由平台写、审计，随各集群 `global` 块的 `ssrf_allow_cidrs` 进入签名修订，Agent 连接时用同一份，IMP-10、ADR-004 §6）；`host_header` 经 Hostname 校验器。
- **同一源站组内一致性**（IMP-10，ADR-004 §6）：同组全部启用源站的 `protocol`、有效 `tls_verify`（`tls_verify` 为 NULL 时取 `site_settings.origin_tls_verify`）、`sni` 必须相同，否则 `400 origin.group_inconsistent`——因 `proxy_ssl_verify` 是 location 级静态指令，`balancer_by_lua` 只能换 peer。`host_header` 允许逐源站不同（经变量 `$le_upstream_host` 设置）。

### 3.3 `origin_health`　**M1**

用途：源站在各节点视角下的当前健康状态（Agent 主动检查上报；非历史）。C2（含 `updated_at` 语义的 `changed_at`）+ T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| origin_id | BIGINT UNSIGNED | N | — | 复合 FK `(tenant_id, origin_id)` → origins(tenant_id, id) CASCADE（CC-1；`origins` 含 `UNIQUE(tenant_id, id)`） |
| node_id | BIGINT UNSIGNED | N | — | FK → nodes(id) CASCADE |
| status | VARCHAR(8) | N | 'unknown' | `up/down/unknown` |
| consecutive_failures | INT UNSIGNED | N | 0 | |
| consecutive_successes | INT UNSIGNED | N | 0 | |
| last_check_at | DATETIME(3) | Y | — | |
| last_status_code | SMALLINT UNSIGNED | Y | — | |
| last_latency_ms | INT UNSIGNED | Y | — | |
| last_error | VARCHAR(255) | Y | — | `timeout/refused/tls_error/dns_error/ssrf_blocked/http_status` 等 |
| changed_at | DATETIME(3) | Y | — | 状态翻转时间 |

- 唯一：`uk_origin_health(origin_id, node_id)`（upsert）；索引：`idx_origin_health_tenant(tenant_id, origin_id)`。
- **本表逐节点存放，平台专用**；租户接口只返回聚合视图 `OriginHealthView`（每源站一条：`down` 仅当所有上报节点均 down，混合为 `degraded`，无 `node_*` 字段，ADR-009 §5 R-T1）。
- 保留：随源站/节点；历史变化事件写 `node_events`/告警，不保留逐次检查。
