---
title: "数据库：规则引擎、缓存与证书"
status: draft
created: 2026-11-16
updated: 2026-11-16（依据架构评审修订，见 docs/bmad/architecture-revision-log.md）
---

# 规则引擎、缓存与证书

> 返回 [README](README.md) · [architecture.md](../architecture/architecture.md)。相关 ADR：005 规则引擎、007 缓存失效、011 密钥。简写见 README §2.1。

## 1. 统一规则

统一规则（FR-63）：**所有规则类型共用 `site_rules` 一张表与同一份规则文档（JSON IR）**；新增规则类型只需放宽 `rule_type` CHECK，不新增表。规则文档结构（`doc_json`）：

```json
{ "when": {"all":[{"field":"path","op":"starts_with","value":"/old/"}]},
  "action": {"redirect":{"status":301,"strip_prefix":"/old/","location":"/new/${path_suffix}"}} }
```

> `doc_json` 只含 `{when, action}`；`rule_type / phase / name / priority / enabled / scope_json` 是**独立列**（ADR-005 §1 已回写为此形态）。变量表、插值后校验与 `strip_prefix` 语义见 ADR-005 §1；**缓存键封闭规则（KCR）**——条件化回源/改写规则读取的维度必须 ⊆ 生效缓存键维度，否则发布失败 `rule.cache_key_dimension_conflict`，或规则声明 `cache_interaction="bypass"`——见 ADR-005 §3.7。

### 1.1 `site_rules`　**M1**

用途：站点规则的**工作态**（草稿，可编辑）；已发布内容由 `published_version_id` 指向的不可变 `rule_versions` 决定。通用列：C1 + T + SD。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| site_id | BIGINT UNSIGNED | N | — | FK→sites(tenant_id,id) RESTRICT |
| rule_type | VARCHAR(32) | N | — | `redirect/url_rewrite/origin_rule/request_header/origin_request_header/response_header/cache_rule/access_control/page_rule/error_page/hotlink/rate_limit` |
| phase | VARCHAR(32) | N | — | **由 `rule_type` 派生（只读，API 不接收）**，合法映射：redirect→redirect；url_rewrite、request_header→rewrite；cache_rule→cache_rule；origin_rule→origin_rule；origin_request_header→origin_headers；error_page→upstream_response；response_header→response_headers；access_control、hotlink→access_control；rate_limit→waf_cc；page_rule→展开（P2）。列保留用于索引与 CHECK 兜底：`ck_site_rules_type_phase` 校验该映射。规则可挂载阶段全集：`access_control/waf_cc/redirect/rewrite/cache_rule/origin_rule/origin_headers/upstream_response/response_headers/respond`（引擎固定阶段 `real_ip/cache_lookup/cache_write` 不接受规则；`page_rule` 的 phase 存其主阶段，编译期展开） |
| name | VARCHAR(128) | N | — | |
| description | VARCHAR(512) | Y | — | |
| priority | INT | N | 1000 | 小者先执行；同阶段同优先级按 id |
| enabled | TINYINT(1) | N | 1 | |
| scope_json | JSON | Y | — | 作用范围：**仅 `{hosts:[…]}`**（主机名子集）；NULL=整站。`path_prefix` 已删除——重定向的前缀剥离由 `action.redirect.strip_prefix` 显式表达 |
| doc_json | JSON | N | — | 当前工作态规则文档（when/action） |
| doc_sha256 | CHAR(64) | N | — | doc_json 规范化哈希 |
| template_id | BIGINT UNSIGNED | Y | — | FK → rule_templates(id) SET NULL |
| current_version_id | BIGINT UNSIGNED | Y | — | 最近一次“保存为版本”，FK → rule_versions(id) SET NULL。**循环 FK**：`rule_versions.rule_id → site_rules`，故先建两表（本列建为无 FK 列），再 `ALTER TABLE site_rules ADD CONSTRAINT`（README §2.4、§8.1 000010） |
| published_version_id | BIGINT UNSIGNED | Y | — | 已发布版本，FK → rule_versions(id) SET NULL（同上，ALTER 追加）。仅在**部署成功后**前移（ADR-002 D-PUB） |
| active_name | VARCHAR(128) | Y | — | 生成列 `IF(deleted_at IS NULL, name, NULL)` |

- 唯一：`uk_site_rules_name(site_id, active_name)`。
- 索引：`idx_site_rules_site_phase(tenant_id, site_id, phase, priority)`、`idx_site_rules_tenant(tenant_id, site_id, enabled)`。
- CHECK：`rule_type`、`phase` 取值；`ck_site_rules_type_phase`（映射见上）；`priority BETWEEN -100000 AND 100000`。（`template_id/current_version_id/published_version_id` 为 `SET NULL` FK 列，**不得被任何 CHECK 引用**，README §2.2）
- 业务：保存规则 → Service 校验（FR-68，含 regex RE2 子集、Header 名、重定向目标）→ 写 `rule_versions` 新版本（`version_no+1`）→ `sites.config_version+1`；发布时 `published_version_id` 前移；回滚 = 把旧版本内容保存为新版本（`rollback_of_version_id`）并发布。
- 命中统计：不存本表，来自 `metric_rollups`（`dim_type='rule'`，`metric='rule_hits'`）。
- 保留：软删除；规则版本随清理策略。

### 1.2 `rule_versions`　**M1**（不可变）

用途：规则的不可变版本快照（FR-67 回滚、NFR-20）。**不可 UPDATE**；DELETE 仅清理任务。通用列：C1 的 `id/uid/created_at`（无 `updated_at`）+ T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| rule_id | BIGINT UNSIGNED | N | — | FK → site_rules(id) RESTRICT |
| site_id | BIGINT UNSIGNED | N | — | 冗余；FK→sites(tenant_id,id) |
| version_no | INT UNSIGNED | N | — | 规则内单调递增 |
| rule_type | VARCHAR(32) | N | — | 快照 |
| phase | VARCHAR(32) | N | — | 快照 |
| priority | INT | N | — | 快照 |
| enabled | TINYINT(1) | N | — | 快照 |
| scope_json | JSON | Y | — | 快照 |
| doc_json | JSON | N | — | 规则文档快照 |
| doc_sha256 | CHAR(64) | N | — | |
| change_note | VARCHAR(512) | Y | — | |
| rollback_of_version_id | BIGINT UNSIGNED | Y | — | 自引用 FK SET NULL |
| created_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |

- 唯一：`uk_rule_versions(rule_id, version_no)`。索引：`idx_rule_versions_tenant(tenant_id, site_id, rule_id)`。
- 不可变触发器见 README §5（`trg_rule_versions_bu/bd`）。保留：每规则最近 100 个及已发布版本 `[ASSUMPTION]`。

### 1.3 `rule_templates`　**M1**（表）/ P2（模板功能与配置模板 FR-50）

用途：规则模板与配置模板（内置与租户自建）。通用列：C1 + T?（NULL=平台内置）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| name | VARCHAR(128) | N | — | |
| description | VARCHAR(512) | Y | — | |
| rule_type | VARCHAR(32) | N | — | 同 site_rules |
| phase | VARCHAR(32) | N | — | |
| doc_json | JSON | N | — | 含参数占位 `{{param}}`（仅允许声明过的参数，值经校验器渲染，不做文本拼接） |
| params_schema_json | JSON | Y | — | 参数声明与类型 |
| is_builtin | TINYINT(1) | N | 0 | |
| version | INT UNSIGNED | N | 1 | |
| status | VARCHAR(16) | N | 'active' | `active/deprecated` |

- 索引：`idx_rule_templates_tenant(tenant_id, rule_type)`。唯一：`uk_rule_templates(tenant_key, name)`（`tenant_key` 为 T? 统一的 VIRTUAL 生成列，README §2.1）。

## 2. 缓存

### 2.1 `cache_policies`　**M1**

用途：站点缓存策略。每站点一条**默认策略**（`is_default=1`，同时承载站点缓存 epoch）+ 可选命名策略（被 `cache_rule` 规则引用）。通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| site_id | BIGINT UNSIGNED | N | — | FK→sites(tenant_id,id) RESTRICT |
| name | VARCHAR(128) | N | — | |
| is_default | TINYINT(1) | N | 0 | |
| default_flag | BIGINT UNSIGNED | Y | — | 生成列 `IF(is_default=1, site_id, NULL)` 唯一 |
| mode | VARCHAR(16) | N | 'respect_origin' | `respect_origin/override/bypass` |
| default_ttl_s | INT UNSIGNED | N | 0 | 秒 |
| max_ttl_s | INT UNSIGNED | N | 2592000 | 上限（需 ≥ 共享字典 URL 标记 TTL 要求，ADR-007） |
| cacheable_statuses_json | JSON | N | — | 可缓存状态码及各自 TTL，例 `{"200":3600,"301":600}` |
| negative_ttl_json | JSON | Y | — | 负缓存 |
| file_type_ttls_json | JSON | Y | — | 扩展名 → TTL |
| query_policy | VARCHAR(24) | N | 'include_all' | `ignore_all/include_all/include_whitelist/exclude_list`（OpenAPI/DB 为事实源；ADR-005 旧称 `allowlist/denylist` 已废止） |
| query_params_json | JSON | Y | — | 白名单/排除列表 |
| cookie_policy | VARCHAR(24) | N | 'ignore' | `ignore/include_named/bypass_on_named` |
| cookie_names_json | JSON | Y | — | |
| vary_headers_json | JSON | Y | — | 允许进入缓存键的请求头（超出者含 Vary 则不缓存）。**与 `query_*`、`cookie_*`、`scheme_in_key` 共同构成缓存键维度集合 `keydims(P)`**，条件化回源/改写规则的读取维度必须 ⊆ 它（KCR，ADR-005 §3.7） |
| scheme_in_key | TINYINT(1) | N | 0 | |
| ignore_origin_cache_control | TINYINT(1) | N | 0 | **仅忽略 TTL 类指令**（`max-age`/`s-maxage`/`Expires`）；`private/no-store/no-cache`、`Vary: *`、`Set-Cookie`、`Authorization` 的默认保护**始终有效**；`cache_rule` 的 `mode=override` 同样只覆盖 TTL，绕过保护只能经下面的危险开关（ADR-005 §3.2） |
| allow_cache_set_cookie | TINYINT(1) | N | 0 | **危险开关**（FR-69）：允许缓存带 Set-Cookie 响应 |
| allow_cache_authorization | TINYINT(1) | N | 0 | **危险开关**：允许缓存带 Authorization 的请求 |
| risk_ack_by | BIGINT UNSIGNED | Y | — | FK → users(id) **RESTRICT**（被 `ck_cache_policies_dangerous` 引用；`ON DELETE SET NULL` 会使该 CHECK 建表即 ERROR 3823，且用户被删后 CHECK 自身即被违反，实测，README §9）。用户软删除，物理清理前须先清理引用 |
| risk_ack_at | DATETIME(3) | Y | — | |
| cache_lock_enabled | TINYINT(1) | N | 1 | 回源合并 |
| cache_lock_timeout_ms | INT UNSIGNED | N | 5000 | |
| range_mode | VARCHAR(16) | N | 'cache_slices' | `passthrough/cache_slices` |
| slice_size_kb | INT UNSIGNED | N | 1024 | |
| large_file_threshold_mb | INT UNSIGNED | N | 64 | |
| stale_if_error_s | INT UNSIGNED | N | 0 | |
| stale_while_revalidate_s | INT UNSIGNED | N | 0 | |
| cache_epoch | BIGINT UNSIGNED | N | 0 | **站点缓存 epoch**（仅默认策略行有意义，**站点级单值，无集群粒度**）：整站逻辑失效 = `cache_epoch+1`，Server 权威，经 Heartbeat/任务下发，Agent 持久化；与 `deploy.lineage` 组成 `(lineage, epoch)` 比较键（ADR-007）。Agent 因共享字典告急本地升级时，Server 权威上调到不小于其回执值 |
| epoch_bumped_at | DATETIME(3) | Y | — | |

- 唯一：`uk_cache_policies_name(site_id, name)`、`uk_cache_policies_default(default_flag)`。索引：`idx_cache_policies_tenant(tenant_id, site_id)`。
- CHECK：枚举列取值；`ck_cache_policies_dangerous`：`(allow_cache_set_cookie=0 AND allow_cache_authorization=0) OR (risk_ack_by IS NOT NULL AND risk_ack_at IS NOT NULL)`（危险开关必须有显式风险确认，且变更写审计）；`ck_cache_policies_epoch`：`is_default=1 OR cache_epoch=0`；`default_ttl_s <= max_ttl_s`。
- 默认值说明：上表“默认”是**迁移/建站时 Service 写入的建议默认**，不是业务数值承诺；TTL 数值默认待基线测试后确定（NFR-30），此处 0 表示“遵循源站”。
- 缓存磁盘限额等节点级参数不在此，见 `clusters.settings_json`。

### 2.2 `cache_tasks`　**M1**（URL 刷新、整站失效）/ P2（预热、目录、通配符）

用途：缓存刷新/预热任务头（FR-78/79）。每个任务同时对应一条 `tasks` 行。通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| site_id | BIGINT UNSIGNED | N | — | FK→sites(tenant_id,id) RESTRICT |
| task_id | BIGINT UNSIGNED | Y | — | FK → tasks(id) SET NULL |
| kind | VARCHAR(24) | N | — | `purge_url/purge_site/prefetch/purge_prefix/purge_wildcard`（后两者 P2 PoC 通过前 CHECK 允许、Service 拒绝） |
| targets_json | JSON | Y | — | URL 列表（数量上限由 Service/配额限制）；`purge_site` 为 NULL |
| target_count | INT UNSIGNED | N | 0 | |
| epoch_after | BIGINT UNSIGNED | Y | — | `purge_site` 生成的新 epoch |
| status | VARCHAR(16) | N | 'pending' | `pending/dispatching/partial/success/failed/canceled` |
| total_nodes | INT UNSIGNED | N | 0 | |
| ok_nodes | INT UNSIGNED | N | 0 | |
| failed_nodes | INT UNSIGNED | N | 0 | |
| requested_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| request_id | CHAR(26) | N | — | |
| finished_at | DATETIME(3) | Y | — | |

- 扇出范围恒为**站点绑定的全部集群**：`cluster_id` 列已删除——epoch 是站点级单值，M1 不提供“按集群整站失效”（集群级失效列入 P2/PoC-8，ADR-007 §6）。
- 索引：`idx_cache_tasks_site(tenant_id, site_id, created_at)`、`idx_cache_tasks_status(status, created_at)`。CHECK：kind/status 取值。保留：90 天 `[ASSUMPTION]`。

### 2.3 `cache_task_nodes`　**M1**

用途：缓存任务在每个节点的**业务回执**（离线节点上线补执行）；逐节点的投递/租约/attempt 在 `node_tasks`（node-deploy §4.4），本表经 `node_task_id` 关联。C2（含 `updated_at`）+ T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| cache_task_id | BIGINT UNSIGNED | N | — | FK → cache_tasks(id) CASCADE |
| node_id | BIGINT UNSIGNED | N | — | FK → nodes(id) RESTRICT |
| node_task_id | BIGINT UNSIGNED | Y | — | FK → node_tasks(id) SET NULL（下发后回填；proto `NodeTask.task_uid` = `node_tasks.uid`） |
| status | VARCHAR(16) | N | 'pending' | `pending/sent/success/failed/skipped_offline/expired`（**事实源**；与 `node_tasks.status`、proto `TaskStatus` 的映射见 ADR-007 §6：RUNNING→`sent`，SKIPPED→`skipped_offline`，EXPIRED→`expired`） |
| attempts | INT UNSIGNED | N | 0 | |
| result_code | VARCHAR(32) | Y | — | |
| error | VARCHAR(512) | Y | — | |
| started_at / finished_at | DATETIME(3) | Y | — | |
| updated_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) ON UPDATE | |

- 唯一：`uk_cache_task_nodes(cache_task_id, node_id)`；索引：`idx_ctn_node_status(node_id, status)`（节点上线时拉取待办）。

## 3. 证书

### 3.1 `certificates`　**M1**

用途：证书（手动上传 M1；ACME 签发 P2）。证书按租户/站点授权，私钥信封加密存储，**API 永不回显私钥**（FR-85、NFR-13）。通用列：C1 + T + SD。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| site_id | BIGINT UNSIGNED | Y | — | 可选绑定站点；FK→sites(tenant_id,id) RESTRICT |
| name | VARCHAR(128) | N | — | |
| source | VARCHAR(16) | N | — | `upload/acme` |
| status | VARCHAR(16) | N | 'active' | `pending/active/expiring/expired/revoked/error` |
| primary_domain | VARCHAR(253) | N | — | |
| domains_json | JSON | N | — | SAN 列表（含通配符） |
| is_wildcard | TINYINT(1) | N | 0 | |
| serial | VARCHAR(64) | N | — | |
| issuer | VARCHAR(255) | N | — | |
| not_before / not_after | DATETIME(3) | N | — | |
| fingerprint_sha256 | CHAR(64) | N | — | 叶证书指纹 |
| key_algorithm | VARCHAR(16) | N | — | `rsa2048/rsa3072/rsa4096/ecdsa_p256/ecdsa_p384` |
| chain_pem | MEDIUMTEXT | N | — | 叶证书+中间链（公开材料） |
| private_key_enc | MEDIUMTEXT | N | — | **信封密文** 5 段 `v1:<kek_id>:<wrapped_dek>:<nonce>:<ct>`（AES-256-GCM，DEK 由 KEK 包裹，AAD=`certificates|private_key_enc|<uid>`，README §2.2、ADR-011）；节点分发时经 mTLS 通道由 Server 解密后传输，节点以 0600 保存 |
| enc_key_id | VARCHAR(32) | N | — | 主密钥版本，用于重加密任务 |
| cert_version | INT UNSIGNED | N | 1 | 续期/替换递增；`certificate_deployments` 以此标识。**本行就地更新，旧版本的证书链与私钥不保留**（ADR-002 §5 选项 (a)：配置修订只引用 `certificate_uid`，不含版本与指纹；节点取当前活动版本，回滚不回退证书，过期/被吊销的旧证书不复活；版本变更经 `cert_reload` 任务与 `certificate_deployments` 追踪） |
| acme_account_id | BIGINT UNSIGNED | Y | — | FK → acme_accounts(id) SET NULL（P2 追加） |
| auto_renew | TINYINT(1) | N | 0 | 仅 `source='acme'` |
| renew_before_days | INT UNSIGNED | Y | — | 续期/告警提前天数，**默认值待用户确认（OQ-7）** |
| last_renew_at | DATETIME(3) | Y | — | |
| last_renew_error | VARCHAR(512) | Y | — | |

- 索引：`idx_certificates_tenant(tenant_id, site_id)`、`idx_certificates_expiry(status, not_after)`（到期扫描）、`idx_certificates_fp(tenant_id, fingerprint_sha256)`。
- CHECK：source/status/key_algorithm 取值；`not_before < not_after`；`auto_renew=0 OR source='acme'`。
- 业务：上传时校验私钥与证书匹配、链有效；证书 API 响应不含 `private_key_enc`；日志脱敏。回滚（站点/集群）前 Service 校验修订引用的证书**未被删除**（已过期给警告，已删除则阻止，UX 回滚前校验）。
- 保留：软删除后物理清理时同时删除密文；节点侧撤除任务保证私钥文件删除。

### 3.2 `acme_accounts`　**P2**

用途：ACME 账户。通用列：C1 + T?（NULL=平台共享账户）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| provider | VARCHAR(24) | N | — | `letsencrypt/letsencrypt_staging/custom` |
| directory_url | VARCHAR(512) | N | — | 须经 SSRF 校验与白名单 |
| email | VARCHAR(255) | N | — | |
| account_url | VARCHAR(512) | Y | — | |
| private_key_enc | MEDIUMTEXT | N | — | 信封密文 |
| enc_key_id | VARCHAR(32) | N | — | |
| status | VARCHAR(16) | N | 'active' | `active/deactivated/error` |
| tos_agreed_at | DATETIME(3) | Y | — | |

- 唯一：`uk_acme_accounts(tenant_key, directory_url, email)`。

### 3.3 `certificate_orders`　**P2**

用途：ACME 订单与验证状态机。通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| certificate_id | BIGINT UNSIGNED | Y | — | FK → certificates(id) SET NULL |
| acme_account_id | BIGINT UNSIGNED | N | — | FK → acme_accounts(id) RESTRICT |
| domains_json | JSON | N | — | |
| challenge_type | VARCHAR(16) | N | — | `http-01/dns-01` |
| status | VARCHAR(16) | N | 'pending' | `pending/ready/valid/invalid/failed/expired/canceled` |
| order_url / finalize_url | VARCHAR(512) | Y | — | |
| authorizations_json | JSON | Y | — | 不含密钥材料 |
| dns_credential_id | BIGINT UNSIGNED | Y | — | DNS-01 用，FK → provider_credentials(id) SET NULL（P3 启用） |
| attempts | INT UNSIGNED | N | 0 | |
| last_error | VARCHAR(1024) | Y | — | |
| expires_at | DATETIME(3) | Y | — | |
| task_id | BIGINT UNSIGNED | Y | — | FK → tasks(id) SET NULL |

- 索引：`idx_cert_orders_tenant(tenant_id, status)`、`idx_cert_orders_cert(certificate_id)`。保留：终态 180 天。

### 3.4 `certificate_deployments`　**M1**

用途：证书分发到每个节点的部署记录（FR-86）。C2（含 `updated_at`）+ T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| certificate_id | BIGINT UNSIGNED | N | — | FK → certificates(id) RESTRICT |
| cert_version | INT UNSIGNED | N | — | |
| node_id | BIGINT UNSIGNED | N | — | FK → nodes(id) RESTRICT |
| cluster_id | BIGINT UNSIGNED | N | — | FK → clusters(id) RESTRICT |
| status | VARCHAR(16) | N | 'pending' | `pending/distributed/applied/failed/removed` |
| error | VARCHAR(512) | Y | — | |
| deployed_at | DATETIME(3) | Y | — | |
| last_report_at | DATETIME(3) | Y | — | |
| revision_seq | BIGINT UNSIGNED | Y | — | 参考：该证书版本首次随之生效的修订 seq（仅展示；修订本身不绑定证书版本） |
| updated_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) ON UPDATE | |

- 唯一：`uk_cert_deploy(certificate_id, cert_version, node_id)`；索引：`idx_cert_deploy_node(node_id, status)`、`idx_cert_deploy_tenant(tenant_id, certificate_id)`。保留：随证书；`removed` 后 90 天。
