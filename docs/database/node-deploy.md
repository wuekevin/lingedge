---
title: "数据库：节点、集群、配置修订与部署"
status: draft
created: 2026-11-16
updated: 2026-11-16（依据架构评审修订，见 docs/bmad/architecture-revision-log.md）
---

# 节点、集群、配置修订与部署

> 返回 [README](README.md) · [architecture.md](../architecture/architecture.md)（§9.3 配置修订契约）。相关 ADR：002、003、008、020。本文件所有表均为**平台级**（无 `tenant_id`），租户永不直接访问；`deployments.tenant_id`、`publish_queue.tenant_id`、`site_versions.tenant_id`、`access_log_chunks.tenant_id` 仅为过滤/归属列。租户视图只经 Service 裁剪后的 `SiteDeploymentView` 等脱敏结构暴露，**不得出现 node/cluster 内部标识**（ADR-009 §5 规则 R-T1）。

## 1. 节点与集群

### 1.1 `nodes`　**M1**

用途：边缘节点（Agent + OpenResty）。通用列：C1 + SD。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| name | VARCHAR(128) | N | — | 运营者命名；活动唯一 |
| active_name | VARCHAR(128) | Y | — | 生成列 `IF(deleted_at IS NULL, name, NULL)` 唯一 |
| hostname | VARCHAR(255) | Y | — | 注册后由 Agent 上报 |
| status | VARCHAR(16) | N | 'pending_enroll' | `pending_enroll/online/offline/maintenance/disabled/decommissioned` |
| maintenance_reason | VARCHAR(255) | Y | — | `status='maintenance'` 时 |
| region_id | BIGINT UNSIGNED | Y | — | FK → regions(id) SET NULL |
| isp_line_id | BIGINT UNSIGNED | Y | — | FK → isp_lines(id) SET NULL |
| primary_ipv4 / primary_ipv6 | VARCHAR(45) | Y | — | 主服务地址 |
| egress_ips_json | JSON | Y | — | 出口 IP 清单（NFR-17，供租户源站 `set_real_ip_from`） |
| os / arch / kernel | VARCHAR(64) | Y | — | `linux`/`amd64|arm64` |
| agent_version | VARCHAR(64) | Y | — | |
| openresty_version | VARCHAR(64) | Y | — | |
| cpu_cores | SMALLINT UNSIGNED | Y | — | |
| mem_bytes / disk_bytes | BIGINT UNSIGNED | Y | — | |
| capabilities_json | JSON | Y | — | Agent 声明能力（如支持的 schema_version、l4） |
| labels_json | JSON | Y | — | |
| last_status_json | JSON | Y | — | 最新心跳摘要（避免扫描 `node_heartbeats`） |
| last_heartbeat_at | DATETIME(3) | Y | — | 判定离线用 |
| last_seen_ip | VARBINARY(16) | Y | — | |
| enrolled_at | DATETIME(3) | Y | — | |
| current_cert_serial | VARCHAR(64) | Y | — | 当前有效节点证书序列号 |
| ca_bundle_version | BIGINT UNSIGNED | N | 0 | 节点已持有的 CA bundle 版本（Heartbeat `ca_bundle_version` 回写；CA 轮换 S2 门禁依据，ADR-003 §5） |
| upgrade_channel | VARCHAR(16) | N | 'stable' | |

- 唯一：`uk_nodes_active_name(active_name)`。索引：`idx_nodes_status(status)`、`idx_nodes_region(region_id)`、`idx_nodes_heartbeat(last_heartbeat_at)`。
- CHECK：status 取值。保留：软删除，历史修订/部署引用保留。
- 离线判定：Scheduler 任务 `heartbeat.sweep` 依据 `last_heartbeat_at` 与可配置窗口（默认待基线测试，FR-53）更新 `status`。

### 1.2 `clusters`　**M1**

用途：节点集群 = 一组节点的服务单元；也是**配置修订的范围**（ADR-002）。通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| name | VARCHAR(128) | N | — | 唯一 |
| description | VARCHAR(512) | Y | — | |
| status | VARCHAR(16) | N | 'active' | `active/disabled` |
| settings_json | JSON | N | — | 集群级参数（Schema 校验）。分两类键：**`node_global`**（节点全局：`proxy_cache_path` 集合、`lua_shared_dict` 容量、监听端口/HTTP2/TLS 基线、PROXY protocol 入站、日志格式版本、worker 参数）——一个节点加入多个集群时这些键**逐键相等**，否则拒绝入群/拒绝修改（`node.cluster_conflict`，ADR-020 §4）；**`rollout`**（灰度由平台定，租户不可改）：`{strategy: all|canary|batch|single, batch_size, max_failures, failure_policy: pause|continue, target_timeout_s, catch_up_ttl_s}`（数值默认待基线测试，FR-56） |
| trusted_proxies_json | JSON | Y | — | 入站可信代理 CIDR 集群默认（ADR-006，站点可覆盖；CIDR 最小前缀与禁用范围见 ADR-006 §2） |
| current_seq | BIGINT UNSIGNED | N | 0 | 最近分配的修订序号；仅在 `publish.dispatch` 持有本行 `FOR UPDATE` 锁时 +1（该锁同时串行化“读基线 → 汇编 → 落修订 → 创建部署”，D-PUB） |
| confirmed_revision_id | BIGINT UNSIGNED | Y | — | **最近一次全量确认健康的修订**：copy-forward 基线，也是离线/新入群节点追平的 `desired`。部署成功（所有应参与目标 `healthy`）才前移。FK → config_revisions(id) RESTRICT（循环外键，000012 追加；原名 `desired_revision_id` 作废） |

- 唯一：`uk_clusters_name(name)`。CHECK：status 取值。保留：永久。

### 1.3 `cluster_nodes`　**M1**

用途：节点 ↔ 集群成员关系（一节点可入多集群，F-3 约束）。C2（含 `updated_at`）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| cluster_id | BIGINT UNSIGNED | N | — | FK → clusters(id) RESTRICT |
| node_id | BIGINT UNSIGNED | N | — | FK → nodes(id) RESTRICT |
| serving_state | VARCHAR(16) | N | 'active' | `active/draining/standby` |
| weight | SMALLINT UNSIGNED | N | 100 | 预留（P3 调度） |
| joined_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) | |

- 唯一：`uk_cluster_nodes(cluster_id, node_id)`；索引：`idx_cluster_nodes_node(node_id)`。
- 业务：入群校验该节点所属各集群的站点主机名无冲突，且各集群 `settings_json.node_global` 逐键相等（F-3 扩展，ADR-020 §4）；冲突返回 `node.cluster_conflict`。

### 1.4 `node_heartbeats`　**M1**（高频，无外键）

用途：心跳时序（趋势与排障）。**建议按日 RANGE 分区**；当前状态以 `nodes.last_status_json` 为准。C2。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| node_id | BIGINT UNSIGNED | N | — | 无 FK（高频写） |
| received_at | DATETIME(3) | N | — | 服务端接收时间（分区键）；主键 `(id, received_at)` |
| agent_version | VARCHAR(64) | N | — | |
| openresty_state | VARCHAR(16) | N | — | `running/stopped/failed/unknown`（与 proto `OpenRestyState` 映射：RUNNING/RELOADING→`running`，STOPPED→`stopped`，CONFIG_ERROR→`failed`，UNSPECIFIED→`unknown`，node-protocol §5） |
| cpu_permille | SMALLINT UNSIGNED | Y | — | 0–1000（整数，避免浮点） |
| load1_milli | INT UNSIGNED | Y | — | |
| mem_used_bytes / mem_total_bytes | BIGINT UNSIGNED | Y | — | |
| disk_used_bytes / disk_total_bytes | BIGINT UNSIGNED | Y | — | |
| cache_disk_used_bytes | BIGINT UNSIGNED | Y | — | |
| conn_active | INT UNSIGNED | Y | — | |
| clock_skew_ms | INT | Y | — | Agent 与 Server 时间差 |
| applied_json | JSON | Y | — | `{cluster_uid: applied_seq}` |

- 索引：`idx_node_heartbeats_node(node_id, received_at)`。保留：7 天 `[ASSUMPTION]`，DROP PARTITION。

## 2. 配置修订（ADR-002，不可变）

> 修订范围 = 集群；内容按站点分块（内容寻址）；签名为 Ed25519（`LINGEDGE-REVISION-V1\n` + canonical manifest）；**签名无过期**；回滚 = 以旧块清单生成新修订；证书私钥**不入修订**。

### 2.1 `config_blocks`　**补充 · M1**（不可变，内容寻址）

用途：修订的内容块（站点块 `site:<uid>`、全局块 `global`、P4 的 L4 块）。同内容只存一份，使“站点级发布 copy-forward”与回滚无需复制。**块内容存放在 MySQL 本表 `content` 列**（随 mysqldump 与修订元数据同库备份；不存在“修订 blob 文件”，architecture §3 / tools §17.1 已回写）。C2（无 updated_at）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| sha256 | CHAR(64) | N | — | 压缩前规范化 JSON 的 SHA-256；唯一 |
| kind | VARCHAR(16) | N | — | `site/global/l4` |
| site_uid | CHAR(26) | Y | — | `kind='site'` 时站点 uid。**入库强校验**（IMP-08 同类：伪造防线）：Server 汇编时 `site_uid` 取自 `sites.uid`，且须匹配 `^[0-9A-HJKMNP-TV-Z]{26}$`；CHECK `ck_config_blocks_site_uid`：`(kind='site') = (site_uid IS NOT NULL)` 且 `site_uid IS NULL OR CHAR_LENGTH(site_uid)=26`（不引用任何 FK 列） |
| tenant_id | BIGINT UNSIGNED | Y | — | 站点块所属租户（追溯/清理用；FK → tenants(id) RESTRICT） |
| schema_version | INT UNSIGNED | N | — | IR Schema 版本 |
| encoding | VARCHAR(8) | N | 'zstd' | `zstd/identity` |
| size_raw | BIGINT UNSIGNED | N | — | |
| size_stored | BIGINT UNSIGNED | N | — | |
| content | MEDIUMBLOB | N | — | 压缩内容；应用层上限 8 MB/块（超限拒绝发布，需拆分） |

- 唯一：`uk_config_blocks_sha(sha256)`。索引：`idx_config_blocks_site(site_uid)`、`idx_config_blocks_tenant(tenant_id)`。
- 不可变：BEFORE UPDATE 触发器；DELETE 仅清理（无 `config_revision_blocks` 引用且不被 `sites.published_block_sha256` 引用）。

### 2.2 `config_revisions`　**M1**（不可变）

用途：集群内某序号的完整配置快照元数据与签名。通用列：C1 的 `id/uid/created_at`（无 `updated_at`）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| cluster_id | BIGINT UNSIGNED | N | — | FK → clusters(id) RESTRICT |
| lineage | BIGINT UNSIGNED | N | — | 修订血统：来自 `system_settings.deploy.lineage`（初值 1；DB 恢复后由 `lingedge-server lineage bump` 提升为 `max(恢复后值+1, 当前 UNIX 秒)`）。节点按 `(lineage, seq)` **字典序严格大于**接受修订，使“备份恢复后 seq 低于节点已应用值”不再导致新修订被永久拒绝（ADR-002 §12） |
| seq | BIGINT UNSIGNED | N | — | 同一 lineage 内集群内单调递增；节点拒绝 `(lineage, seq)` 不大于已应用值的修订 |
| parent_seq | BIGINT UNSIGNED | Y | — | 生成时的上一修订（copy-forward 基线） |
| kind | VARCHAR(16) | N | — | `publish/rollback/system` |
| schema_version | INT UNSIGNED | N | — | manifest/IR 版本 |
| manifest_canonical | MEDIUMBLOB | N | — | **被签名的原始规范化字节**（manifest：`schema_version, cluster_uid, lineage, seq, parent_seq, created_at, blocks[{key,sha256,size}]`；`key ∈ global|site:<uid>|l4`，无并列 `global{…}`，ADR-002 §3） |
| manifest_sha256 | CHAR(64) | N | — | |
| signature | VARBINARY(64) | N | — | Ed25519 签名 |
| key_id | VARCHAR(32) | N | — | 对应 `signing_keys.key_id`（无 FK：密钥可归档） |
| block_count | INT UNSIGNED | N | — | |
| total_size | BIGINT UNSIGNED | N | — | 块存储大小合计 |
| rollback_of_revision_id | BIGINT UNSIGNED | Y | — | 自引用 FK → config_revisions(id) RESTRICT |
| trigger_site_id | BIGINT UNSIGNED | Y | — | 触发本次发布的站点；FK → sites(id) SET NULL（站点软删除不影响） |
| change_note | VARCHAR(512) | Y | — | |
| created_by_user_id | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| created_by_task_id | BIGINT UNSIGNED | Y | — | FK → tasks(id) SET NULL |
| request_id | CHAR(26) | N | — | |

- 唯一：`uk_config_revisions_seq(cluster_id, lineage, seq)`、`uk_config_revisions_manifest(cluster_id, manifest_sha256)`（同内容避免重复发布；回滚时 `kind='rollback'` 因 `parent_seq`/`seq` 进入 manifest，哈希不同）。
- 索引：`idx_config_revisions_cluster_time(cluster_id, created_at)`、`idx_config_revisions_site(trigger_site_id, created_at)`。
- CHECK：kind 取值；`seq >= 1`；`kind='rollback' = (rollback_of_revision_id IS NOT NULL)`（以 `(kind='rollback') = (rollback_of_revision_id IS NOT NULL)` 表达）；`parent_seq IS NULL OR parent_seq < seq`。（`rollback_of_revision_id` 为 RESTRICT FK，被 CHECK 引用安全，README §2.2/§9）
- 不可变：UPDATE 全部拒绝；DELETE 仅清理（README §5、§7）。保留：每集群最近 50 个及一切被引用者 `[ASSUMPTION]`。

### 2.3 `config_revision_blocks`　**补充 · M1**

用途：修订 ↔ 块的组成清单（copy-forward：新修订只为变更站点新增块，其余复用）。C2（无 updated_at）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| revision_id | BIGINT UNSIGNED | N | — | FK → config_revisions(id) CASCADE（清理时） |
| block_id | BIGINT UNSIGNED | N | — | FK → config_blocks(id) RESTRICT |
| block_key | VARCHAR(40) | N | — | `global` / `site:<uid>` / `l4`；与 manifest 一致 |

- 唯一：`uk_crb(revision_id, block_key)`；索引：`idx_crb_block(block_id)`（块引用计数）。不可变：UPDATE 拒绝。

### 2.4 `node_cluster_state`　**补充 · M1**

用途：**每个节点在每个集群上实际生效的修订**（基线 §10.3 必须记录）及期望修订（用于灰度与心跳应答）。可变状态表。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| node_id | BIGINT UNSIGNED | N | — | 复合主键之一；FK → nodes(id) CASCADE |
| cluster_id | BIGINT UNSIGNED | N | — | 复合主键之一；FK → clusters(id) CASCADE |
| desired_revision_id | BIGINT UNSIGNED | Y | — | 期望修订（灰度：仅批次内节点被设置；未参与任何活动部署的节点其有效 `desired` = `clusters.confirmed_revision_id`，由 Heartbeat 计算，不落此列）；FK → config_revisions(id) RESTRICT |
| desired_seq | BIGINT UNSIGNED | Y | — | 冗余便于比较 |
| applied_revision_id | BIGINT UNSIGNED | Y | — | **实际生效**修订；FK → config_revisions(id) RESTRICT |
| applied_lineage | BIGINT UNSIGNED | Y | — | 实际生效修订的 lineage（`ReportApply.now_serving` 回写） |
| applied_seq | BIGINT UNSIGNED | Y | — | |
| node_generation | BIGINT UNSIGNED | Y | — | 节点本地“节点代”编号 `g<N>`（仅展示/排障；节点代 = 各集群 seq 的复合，ADR-020 §2） |
| held_seq | BIGINT UNSIGNED | Y | — | **节点级回滚保持**：`NodeRollback` 成功后记录被回退的 seq；Heartbeat 对 `desired.seq ≤ held_seq` 返回 `ACTION_HOLD`，平台发布更高 seq 的修订时自动清空（D-PUB 第 6 条） |
| previous_applied_seq | BIGINT UNSIGNED | Y | — | 回退提示（节点本地保留代） |
| state | VARCHAR(16) | N | 'idle' | `idle/applying/failed/rolled_back` |
| applied_at | DATETIME(3) | Y | — | |
| last_error | VARCHAR(1024) | Y | — | |
| last_report_at | DATETIME(3) | Y | — | |
| updated_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) ON UPDATE | |

- 主键：`(node_id, cluster_id)`；索引：`idx_ncs_cluster(cluster_id, applied_seq)`、`idx_ncs_desired(cluster_id, desired_seq)`。CHECK：state 取值（`idle/applying/failed/rolled_back`）。
- 写入：仅来自 `ReportApply`（`applied_*`）与部署编排（`desired_*`）。

## 3. 部署

### 3.1 `deployments`　**M1**

用途：把某集群的某修订推送到一批节点的发布动作（FR-55/56）。通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| cluster_id | BIGINT UNSIGNED | N | — | FK → clusters(id) RESTRICT。**同一集群同一时刻至多一个活动部署**（`pending/running/paused`；其余发布请求在 `publish_queue` 排队，§3.4） |
| revision_id | BIGINT UNSIGNED | N | — | FK → config_revisions(id) RESTRICT |
| kind | VARCHAR(16) | N | 'publish' | `publish/rollback` |
| queue_id | BIGINT UNSIGNED | Y | — | 来源队列项；FK → publish_queue(id) SET NULL（循环外键，000012 内先建 publish_queue 再建本表，故直接带 FK） |
| tenant_id | BIGINT UNSIGNED | Y | — | 发起方租户上下文（Admin 代租户为目标租户）；FK → tenants(id) **RESTRICT**（所有 tenant 外键统一 RESTRICT，README §2.4） |
| trigger_site_id | BIGINT UNSIGNED | Y | — | 租户“部署记录”按此过滤；FK → sites(id) SET NULL |
| strategy | VARCHAR(16) | N | 'all' | `all/canary/batch/single`。**取自 `clusters.settings_json.rollout`（平台定）**；仅平台入口 `POST /clusters/{id}/deployments` 可覆盖，租户发布接口不接受 strategy/node/batch 参数（ADR-009 §5 R-T1） |
| batch_plan_json | JSON | Y | — | 批次与节点列表（canary/batch；平台可见，租户视图不含） |
| current_batch | INT UNSIGNED | N | 0 | |
| max_failures | INT UNSIGNED | N | 0 | 批内失败数达到则暂停 |
| status | VARCHAR(16) | N | 'pending' | `pending/running/paused/success/partial_failed/failed/canceled/rolled_back`。`success` = 所有应参与目标 `healthy`（`waiting_offline` 目标不阻塞，汇总单独计数，§3.2） |
| task_id | BIGINT UNSIGNED | Y | — | FK → tasks(id) SET NULL |
| started_at / finished_at | DATETIME(3) | Y | — | |
| error_summary | VARCHAR(1024) | Y | — | |
| created_by_user_id | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| request_id | CHAR(26) | N | — | |

- 索引：`idx_deployments_cluster(cluster_id, created_at)`、`idx_deployments_site(tenant_id, trigger_site_id, created_at)`、`idx_deployments_status(status)`。CHECK：kind/strategy/status 取值。
- 循环外键：`sites.last_deployment_id → deployments(id)`、`clusters.confirmed_revision_id → config_revisions(id)` 于 000012 末尾追加。**部署成功才**前移 `clusters.confirmed_revision_id`、`sites.published_*` 并把 `site_versions.status` 置 `live`。保留：终态 365 天 `[ASSUMPTION]`，不早于引用修订。

### 3.2 `deployment_targets`　**M1**

用途：某次部署在每个节点上的逐步状态机。**C2u**（含 `updated_at`；`uid` = proto `DesiredState.deployment_target_uid`，ReportApply 经它定位）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| uid | CHAR(26) | N | — | ULID，gRPC 一律用它（architecture §9.1） |
| deployment_id | BIGINT UNSIGNED | N | — | FK → deployments(id) CASCADE |
| node_id | BIGINT UNSIGNED | N | — | FK → nodes(id) RESTRICT |
| batch_no | INT UNSIGNED | N | 0 | |
| attempt | INT UNSIGNED | N | 1 | 当前尝试序号（`retry` 递增；`deployment_target_steps` 以 `(target, attempt, state)` 去重） |
| deadline_at | DATETIME(3) | Y | — | 超时线（`clusters.settings.rollout.target_timeout_s`）；到期仍未达终态 → `failed`（`failure_stage` 视当前步骤，错误 `timeout`） |
| state | VARCHAR(16) | N | 'pending' | `pending/notified/downloaded/verified/tested/applied/healthy/failed/rolled_back/skipped/waiting_offline/expired`（小写；与 proto `ApplyState` 对应，NOTIFIED/waiting_offline/skipped/expired 为 Server 侧状态）。`waiting_offline`：下发时节点离线，不阻塞批次、不计失败比例，节点上线后其 `desired`=`clusters.confirmed_revision_id`，应用成功即回写 `healthy` 并更新部署汇总；超过 `catch_up_ttl_s` → `expired`。`skipped`：维护节点，不计入 M |
| failure_stage | VARCHAR(16) | Y | — | 失败发生的步骤：`download/verify/render/test/apply/health` |
| applied_seq | BIGINT UNSIGNED | Y | — | |
| rolled_back | TINYINT(1) | N | 0 | 节点是否已回到上一有效代 |
| health_json | JSON | Y | — | 健康检查结果（配置哈希回显、探测摘要） |
| error | VARCHAR(1024) | Y | — | |
| state_updated_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) | |
| started_at / finished_at | DATETIME(3) | Y | — | |
| updated_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) ON UPDATE | |

- 唯一：`uk_deployment_targets(deployment_id, node_id)`、`uk_deployment_targets_uid(uid)`；索引：`idx_dt_node_state(node_id, state)`、`idx_dt_batch(deployment_id, batch_no, state)`、`idx_dt_deadline(state, deadline_at)`（超时扫描）。
- CHECK：state/failure_stage 取值。合法状态迁移由 Service 强制（`pending→notified→downloaded→verified→tested→applied→healthy`，任一步可 → `failed`，失败且节点回退 → `rolled_back`；`pending/notified→waiting_offline→…`、`→skipped/expired`）。

### 3.3 `deployment_target_steps`　**补充 · M1**

用途：ReportApply 的**幂等键承载**与逐步历史（proto `ReportApplyRequest` 幂等键 = `(deployment_target_uid, state, attempt)`）。C2。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| deployment_target_id | BIGINT UNSIGNED | N | — | FK → deployment_targets(id) CASCADE |
| attempt | INT UNSIGNED | N | — | |
| state | VARCHAR(16) | N | — | 同 `deployment_targets.state` 的节点侧子集（`downloaded/verified/tested/applied/healthy/failed/rolled_back`） |
| started_at / finished_at | DATETIME(3) | Y | — | `ApplyStepResult` |
| message | VARCHAR(512) | Y | — | 不含密钥/私钥 |
| reported_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) | |

- 唯一：`uk_dts(deployment_target_id, attempt, state)`（重复上报 `INSERT IGNORE`/`ON DUPLICATE KEY` 视为同一事件）。保留：随 `deployment_targets`。

### 3.4 `publish_queue`　**补充 · M1**

用途：**发布编排队列**（D-PUB）。每集群串行：同时至多一个活动部署，其余请求在此排队；`publish.dispatch` 任务在持有 `clusters` 行锁时领取队首，**此时才构建修订**（基线 = `clusters.confirmed_revision_id`），从而失败/暂停中的修订不会被其他站点“捎带”，毒块天然隔离。通用列：C1 + T?。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| cluster_id | BIGINT UNSIGNED | N | — | FK → clusters(id) RESTRICT |
| site_id | BIGINT UNSIGNED | Y | — | 复合 FK→sites(tenant_id,id) RESTRICT；`cluster_rollback` 为 NULL（此时 `tenant_id` 亦可为 NULL，复合 FK 对 NULL 成员不检查，MATCH SIMPLE） |
| kind | VARCHAR(20) | N | — | `site_publish/site_rollback/cluster_rollback` |
| expected_config_version | BIGINT UNSIGNED | Y | — | 请求时 `Site.config_version`（MIN-02：不符 → 412）；排队期间被覆盖合并时更新 |
| target_version_no | BIGINT UNSIGNED | Y | — | `site_rollback` 的目标 `site_versions.version_no` |
| change_note | VARCHAR(512) | Y | — | |
| status | VARCHAR(16) | N | 'queued' | `queued/building/dispatched/superseded/canceled/failed` |
| queued_key | BIGINT UNSIGNED | Y | — | 生成列 `IF(status='queued', site_id, NULL)` STORED；基列 `site_id` 的 FK 为 RESTRICT，实测可用 |
| deployment_id | BIGINT UNSIGNED | Y | — | FK → deployments(id) SET NULL（`dispatched` 后回填；本表先于 deployments 创建，该列为无 FK 普通列，由 000012 末尾 `ALTER` 追加 FK） |
| site_version_id | BIGINT UNSIGNED | Y | — | FK → site_versions(id) SET NULL（本表后于 site_versions 创建时直接带 FK） |
| task_id | BIGINT UNSIGNED | Y | — | FK → tasks(id) SET NULL（租户看到的 Task） |
| requested_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| error_code | VARCHAR(96) | Y | — | 构建/校验失败原因码（如 `config.validation_failed`） |
| started_at / finished_at | DATETIME(3) | Y | — | |

- 唯一：`uk_pq_queued(cluster_id, queued_key)`（同站点同集群至多一个排队项，新发布**合并覆盖**旧排队项并标旧项 `superseded`）。索引：`idx_pq_cluster_status(cluster_id, status, id)`、`idx_pq_site(tenant_id, site_id, created_at)`。CHECK：status/kind 取值；`(kind='cluster_rollback') = (site_id IS NULL)`（`site_id` 为 RESTRICT 复合 FK 成员，可被 CHECK 引用）。
- 阻塞告警：活动部署 `paused` 过久或队列等待超阈值（运营配置项，默认值待基线测试）→ 告警事件，不静默堆积。M1 不跨站点合并（隔离优先；P2 评估“合并窗口 + 失败二分”）。

### 3.5 `site_versions`　**补充 · M1**

用途：**站点线性版本**（MIN-14）。租户与 UX 使用的 v126/v127 是站点内单调的 `version_no`，而不是集群共享的 `seq`（后者会跳号并泄漏他人发布频率）。通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| site_id | BIGINT UNSIGNED | N | — | FK→sites(tenant_id,id) RESTRICT |
| version_no | BIGINT UNSIGNED | N | — | 站点内单调递增 |
| kind | VARCHAR(16) | N | 'publish' | `publish/rollback` |
| rollback_of_version_no | BIGINT UNSIGNED | Y | — | `kind='rollback'` 时必填 |
| block_id | BIGINT UNSIGNED | N | — | FK → config_blocks(id) RESTRICT（该版本的站点块，内容寻址） |
| status | VARCHAR(16) | N | 'pending' | `pending`（已构建、部署中）→ `live`（部署成功）/ `failed`；被更新版本取代 → `superseded` |
| change_note | VARCHAR(512) | Y | — | |
| created_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |

- 唯一：`uk_site_versions(site_id, version_no)`。索引：`idx_site_versions_block(block_id)`。CHECK：`(kind='rollback') = (rollback_of_version_no IS NOT NULL)`。
- 回滚：以目标版本的 `block_id` 生成新 `version_no`（`kind='rollback'`），再入 `publish_queue`；回滚前校验被引用证书未删除（IMP-06 选项 (a)）。保留：每站点最近 200 个及 `live`。`sites.published_version_no` 在部署成功时前移。

## 4. 注册、身份与事件

### 4.1 `enroll_tokens`　**补充 · M1**

用途：一次性节点注册令牌（ADR-003）。通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| node_id | BIGINT UNSIGNED | N | — | FK → nodes(id) CASCADE（令牌绑定**节点**而非集群，UX-14 同步） |
| role | VARCHAR(16) | N | 'node' | 令牌绑定的证书角色（ADR-003 §7；M1 仅 `node`），Enroll 请求的 `role` 必须一致；CHECK 取值 `node/dns/reporter/scheduler/worker` |
| token_hash | CHAR(64) | N | — | SHA-256(令牌 secret)；明文仅创建时展示一次（命令行展示一律 `--token-file`/stdin 形式，MIN-10） |
| expires_at | DATETIME(3) | N | — | 默认创建后 24 小时（最小 1 小时） |
| used_at | DATETIME(3) | Y | — | **原子消费**：`UPDATE … SET used_at=NOW(3) WHERE id=? AND used_at IS NULL AND revoked_at IS NULL AND expires_at>NOW(3)`，影响 1 行才算成功 |
| used_ip | VARBINARY(16) | Y | — | |
| bound_cidr | VARCHAR(64) | Y | — | 可选：限定来源网段 |
| revoked_at | DATETIME(3) | Y | — | |
| created_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |

- 唯一：`uk_enroll_tokens_hash(token_hash)`；索引：`idx_enroll_tokens_node(node_id)`、`idx_enroll_tokens_exp(expires_at)`。保留：过期后 30 天。
- Server 仅在存在未用、未过期令牌时开启 :9444 监听。

### 4.2 `node_certificates`　**补充 · M1**

用途：已签发的 mTLS 证书台账与吊销（FR-26/52）。主体可为节点或后续的 DNS/Reporter。C2。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| subject_type | VARCHAR(16) | N | 'node' | `node/dns/reporter/scheduler/worker` |
| subject_id | BIGINT UNSIGNED | N | — | 对应表主键（`nodes.id`/`dns_nodes.id`/`reporters.id`）；多态，无 FK |
| node_id | BIGINT UNSIGNED | Y | — | 当 `subject_type='node'` 时冗余，FK → nodes(id) CASCADE |
| serial | VARCHAR(64) | N | — | 十六进制序列号 |
| fingerprint_sha256 | CHAR(64) | N | — | |
| issuer_key_id | VARCHAR(32) | N | — | 签发 CA 的 `signing_keys.key_id` |
| not_before / not_after | DATETIME(3) | N | — | 有效期默认 90 天（ADR-003） |
| status | VARCHAR(16) | N | 'active' | `pending/active/superseded/revoked/expired`。**续期两阶段**（ADR-003 §4）：`RenewCertificate` 签发的新证书为 `pending`（旧证书仍 `active`）；拦截器在**新证书首次成功 mTLS 握手**时把它置 `active`、旧证书置 `superseded`（记 `superseded_at`，`grace_until=min(旧 not_after, superseded_at+7d)`，宽限内仍可握手；吊销可立即切断）。响应丢失或 Agent 落盘前崩溃时旧证书继续可用 |
| superseded_at | DATETIME(3) | Y | — | |
| grace_until | DATETIME(3) | Y | — | `superseded` 证书仍被接受的截止时间 |
| revoked_at | DATETIME(3) | Y | — | |
| revoke_reason | VARCHAR(64) | Y | — | |

- 唯一：`uk_node_certs_serial(serial)`。索引：`idx_node_certs_subject(subject_type, subject_id, status)`。CHECK：`(subject_type='node') = (node_id IS NOT NULL)`（`node_id` 为 `ON DELETE CASCADE` FK，实测 CHECK 可引用；若改 `SET NULL` 将 ERROR 3823）；status 取值。
- 吊销检查：gRPC 拦截器按 `serial` 查询（内存缓存，吊销时失效）；接受集合 = `active` ∪ `pending`（仅限其首次握手）∪ 宽限内 `superseded`。威胁模型/事件响应中的“`revoked_serials`”即 `status='revoked'` 的行，无同名表。保留：到期后 365 天。

### 4.3 `node_events`　**补充 · M1**

用途：节点级事件流（上下线、注册、升级、OpenResty 重启、应用失败、证书续期等）；供告警与排障。**C2u**；`event_uid` 是 Agent 生成的 ULID，**proto `NodeEvent.event_uid` 去重依据**（`INSERT IGNORE`）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| uid | CHAR(26) | N | — | 对外标识（OpenAPI `NodeEvent.uid`）；Agent 上报事件 = `event_uid`，Server 自生事件由 Server 生成 ULID |
| node_id | BIGINT UNSIGNED | N | — | FK → nodes(id) CASCADE |
| event_type | VARCHAR(48) | N | — | `online/offline/enrolled/cert_renewed/upgrade_started/upgrade_succeeded/upgrade_failed/openresty_restarted/apply_failed/maintenance_on/maintenance_off…` |
| severity | VARCHAR(8) | N | 'info' | `info/warn/error` |
| message | VARCHAR(512) | N | — | |
| data_json | JSON | Y | — | |
| request_id | CHAR(26) | Y | — | |

- 唯一：`uk_node_events_uid(uid)`（= `event_uid` 幂等）。索引：`idx_node_events_node(node_id, id)`、`idx_node_events_type(event_type, created_at)`。保留：90 天 `[ASSUMPTION]`。

### 4.4 `node_tasks`　**补充 · M1**

用途：**逐节点任务投递队列**（租约/attempt/回执）。`tasks`（ADR-019）是 Server 侧作业，无 node 维度；节点侧动作（缓存失效、升级、节点级回滚、证书重载、调试）经本表由 `TaskService.PullTasks` 下发，`ReportTask` 回执。**C2u**（`uid` = proto `NodeTask.task_uid`）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| uid | CHAR(26) | N | — | 幂等键 `task_uid` |
| node_id | BIGINT UNSIGNED | N | — | FK → nodes(id) RESTRICT；`PullTasks` 只返回 `node_id = 证书主体` 的行（归属校验依据） |
| task_id | BIGINT UNSIGNED | Y | — | 父 `tasks` 行（扇出）；FK → tasks(id) SET NULL |
| tenant_id | BIGINT UNSIGNED | Y | — | 发起租户上下文（缓存/调试任务）；FK → tenants(id) RESTRICT |
| site_id | BIGINT UNSIGNED | Y | — | 复合 FK→sites(tenant_id,id) RESTRICT（缓存/调试任务）；本表在迁移 000009（`sites` 之后）创建，直接带 FK |
| kind | VARCHAR(24) | N | — | `cache_purge_url/cache_purge_site/cache_purge_prefix/cache_prefetch/node_upgrade/node_rollback/cert_reload/debug_request`（与 proto `NodeTask.payload` oneof 一一对应；封闭枚举，无 exec 类） |
| payload_json | JSON | N | — | 结构化参数（Server 序列化 proto payload 的 JSON 形式；不含密钥） |
| status | VARCHAR(12) | N | 'pending' | `pending/leased/running/success/failed/skipped/expired/canceled` |
| attempt | INT UNSIGNED | N | 0 | 投递次数（每次租约 +1） |
| max_attempts | INT UNSIGNED | N | 5 | |
| lease_until | DATETIME(3) | Y | — | 租约；到期未回执 → 视为失败并按 attempt 重投 |
| deadline_at | DATETIME(3) | Y | — | 超过则标 `expired`（整站/URL 失效的**效果**经 epoch/marker 在 Heartbeat 补齐，不依赖 deadline，ADR-007） |
| result_json | JSON | Y | — | 回执结果（CacheTaskResult/UpgradeResult/DebugResult 的 JSON） |
| error_code / error_message | VARCHAR(96) / VARCHAR(512) | Y | — | |
| request_id | CHAR(26) | N | — | 贯穿审计 |
| started_at / finished_at | DATETIME(3) | Y | — | |
| updated_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) ON UPDATE | |

- 唯一：`uk_node_tasks_uid(uid)`。索引：`idx_node_tasks_pull(node_id, status, deadline_at)`（PullTasks：`WHERE node_id=? AND status IN ('pending','leased') AND (lease_until IS NULL OR lease_until<NOW(3)) ORDER BY id`）、`idx_node_tasks_task(task_id)`、`idx_node_tasks_tenant(tenant_id, site_id, created_at)`。CHECK：kind/status 取值；`attempt <= max_attempts`。
- 与业务回执表的关系：`cache_task_nodes`（rules-cache-cert §2.3）保留面向租户/平台的业务状态并含 `node_task_id` FK；状态映射（`node_tasks.status` → `cache_task_nodes.status`）：`pending→pending`、`leased/running→sent`、`success→success`、`failed→failed`、`skipped→skipped_offline`、`expired→expired`。
- 保留：终态 90 天 `[ASSUMPTION]`。

## 5. 区域、线路、IP 池

> M1 仅作为节点元数据（FR-59）；管理 UI 随 P3 调度启用。

### 5.1 `regions`　**补充 · M1**
通用列：C1。列：`code VARCHAR(32) N`（唯一，如 `cn-east`）、`name VARCHAR(128) N`、`country_code CHAR(2) Y`、`continent VARCHAR(16) Y`、`parent_id BIGINT UNSIGNED Y`（自引用 FK SET NULL）、`sort_order INT N 0`。唯一 `uk_regions_code(code)`。

### 5.2 `isp_lines`　**补充 · M1**
通用列：C1。列：`code VARCHAR(32) N`（唯一，如 `telecom`）、`name VARCHAR(128) N`、`asn_list_json JSON Y`（ASN 列表，P3 GeoIP 数据源确定后使用，OQ-5）、`description VARCHAR(255) Y`。唯一 `uk_isp_lines_code(code)`。

### 5.3 `ip_pools`　**补充 · M1**
通用列：C1。列：`name VARCHAR(128) N`（唯一）、`ip_family VARCHAR(4) N`（`v4/v6`）、`description VARCHAR(255) Y`。

### 5.4 `ip_pool_members`　**补充 · M1**
C2。列：`ip_pool_id`（FK → ip_pools CASCADE）、`node_id`（可空，FK → nodes SET NULL）、`address VARCHAR(45) N`、`role VARCHAR(8) N`（`serving/egress`）、`status VARCHAR(16) N 'active'`（`active/reserved/disabled`）。唯一 `uk_ip_pool_members(address)`；索引 `idx_ipm_pool(ip_pool_id)`、`idx_ipm_node(node_id)`。`role='egress'` 的地址汇总进 `nodes.egress_ips_json` 与出口 IP 清单 API（NFR-17）。

## 6. 访问日志分片元数据

### 6.1 `access_log_chunks`　**补充 · M1**（无外键）

用途：访问日志**文件分片**的索引元数据；日志正文在文件系统（ADR-008），**不入 MySQL**。每行 = **一个站点在一个上传批次内、一个 UTC 日期内**的 chunk（跨 UTC 零点的批次拆成两行）。C2。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| tenant_id | BIGINT UNSIGNED | N | — | 无 FK（体量大，由写入路径校验） |
| site_id | BIGINT UNSIGNED | N | — | `0` = unattributed（`site_uid` 格式合法但不属于该节点所在集群当前绑定的站点，文件在 `_unattributed/<node_uid>/`，仅平台可见，计 `LOG_SITE_MISMATCH` 事件） |
| node_id | BIGINT UNSIGNED | N | — | |
| log_date | DATE | N | — | UTC 日期 |
| segment_seq | BIGINT UNSIGNED | N | — | 节点侧分段号 |
| start_offset / end_offset | BIGINT UNSIGNED | N | — | 节点分段文件字节区间；去重键 |
| storage_path | VARCHAR(512) | N | — | 相对日志根：`access/<site_uid>/<YYYYMMDD>/<node_uid>-<seq>.jsonl.zst`。**写入前强校验**（IMP-08）：`site_uid` 匹配 `^[0-9A-HJKMNP-TV-Z]{26}$`（非法 → 整批 `REJECTED_INVALID`）、站点属于节点所在集群（否则归 unattributed），且最终 `realpath` 经 `filepath.Rel` 断言在日志根内 |
| file_offset | BIGINT UNSIGNED | N | — | 本 chunk 的 zstd 帧在分片文件中的起始字节（同一文件被多个批次追加） |
| stored_length | BIGINT UNSIGNED | N | — | 本 chunk 的 zstd 帧字节数 |
| line_count | INT UNSIGNED | N | — | 用于与 rollup 对账 |
| raw_bytes / stored_bytes | BIGINT UNSIGNED | N | — | |
| min_ts / max_ts | DATETIME(3) | N | — | 分片内请求时间范围 |
| status_counts_json | JSON | Y | — | `{"2xx":..,"3xx":..,"4xx":..,"5xx":..}` 供查询粗筛 |
| sha256 | CHAR(64) | N | — | **本 chunk 未压缩 JSONL 字节**的 SHA-256（不是整个分片文件的哈希；用于重放核对与 `REJECTED_INVALID` 判定） |
| received_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) | |

- 唯一：`uk_alc_dedupe(node_id, segment_seq, start_offset, end_offset, site_id, log_date)`（至少一次投递 + 服务端去重；`log_date` 并入键，避免批次跨 UTC 零点时同键出现两行冲突）。
- 索引：`idx_alc_query(tenant_id, site_id, log_date, min_ts)`、`idx_alc_node(node_id, segment_seq)`、`idx_alc_date(log_date)`。
- 保留：待用户确认（OQ-10）；按 `log_date` 删除文件后删行。
