# ADR-002 配置修订：不可变、签名、灰度与回滚

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §10.3、§8.3、§24；PRD FR-55、FR-56、FR-67、NFR-20、D-12、R-4、反馈 F-3；architecture AD-2、§9.3｜阶段：M1

## 背景

基线 §10.3：不可变配置版本；主控生成→校验→版本签名→节点拉取→签名验证→生成文件→`nginx -t`→灰度部署→健康检查→确认；失败恢复上一有效版本；**必须记录每个节点实际生效的配置版本**。PRD R-4：签名不得带会使离线节点停服的过期；D-12：配置签名为独立信任域。节点可加入多个集群（基线 §3.2），站点绑定集群。

## 决策

1. **修订范围 = 集群**。`config_revisions(cluster_id, lineage, seq)`：`seq` 在同一 `lineage` 内的集群内单调递增（`lineage` 见 §12）；一个修订是该集群全部站点（及集群级全局参数 `global` 块，如可信代理、缓存 zone 容量、`ssrf_allow_cidrs`）的完整快照。
2. **内容按站点分块、内容寻址**：每个站点的"已编译配置块"（站点元数据、域名、源站组、编译后的规则 IR、缓存策略、TLS 引用、真实 IP 配置）序列化为 canonical JSON → zstd → 以 `sha256` 为键存入 `config_blocks`；修订由 `config_revision_blocks(revision_id, block_key, block_id)` 清单组成（`block_key ∈ global|site:<uid>|l4`，`block_id` → `config_blocks`，块内容存放在 MySQL，无独立修订 blob 文件）。未变化站点的块被新修订复用（**copy-forward**，基线见 §13）。
3. **清单与签名**：`manifest = {schema_version, cluster_uid, lineage, seq, parent_seq, created_at, blocks[{key, sha256, size}]}`，`key ∈ global|site:<uid>|l4`（**全局块与 L4 块也是 `blocks[]` 的条目，没有并列的 `global{…}` 对象**——DB `config_revision_blocks.block_key`、proto `RevisionHeader.manifest_json`、本文三处表示一致，避免签名输入分歧）；canonical JSON：键排序、无空白、UTF-8、整数不带小数点，由 `pkg/canonicaljson` 单一实现，Server/Agent/Tools 共用 `pkg/edgeconfig/testdata/manifest-vectors.json` golden 向量测试；签名 = `Ed25519(priv, "LINGEDGE-REVISION-V1\n" + manifest_json)`，记录 `key_id`。**签名不含过期时间；节点校验不依赖本机时钟**（R-4）。
4. **不可变**：`config_revisions`、`config_blocks`、`config_revision_blocks` 在 Repository 无 UPDATE/DELETE 方法，并由 DB 触发器拒绝对关键列的 UPDATE 与任何 DELETE（database README §5）；保留期清理只能由受控 `retention.cleanup` 任务删除"未被任何 `node_cluster_state`/部署引用且超期"的修订（仍经触发器的维护开关，需审计）。
5. **证书私钥不入修订；修订只引用 `certificate_uid`**（站点/域名 ↔ 证书的绑定关系），**不含证书版本与指纹**（IMP-06 选 (a)）：证书是外部可变依赖，节点经 `FetchCertificate`（mTLS）取**当前活动版本**，Server 审计每次取用；续期/替换经 `cert_reload` 任务与 `certificate_deployments` 追踪，**不产生新修订**；**回滚不回退证书**（过期或被吊销的旧证书不应复活，旧版本私钥与链也不保留）。因此“同一修订 = 同一有效配置”的含义是“同一站点块 + 当前有效证书”；回滚前 Service 校验被引用证书未被删除（已过期给警告）。
6. **回滚 = 生成新修订**：`rollback_of` 指向被回滚到的修订，`seq` 继续递增；站点级回滚 = "上一修订 + 该站点块取自旧修订"；集群级回滚 = 复制旧修订块清单。节点据此"拒绝 `(lineage, seq)` 不大于已应用值的修订"（字典序**严格大于**才接受，§12），既防重放也不需要特例。规则级回滚属于站点块的重新编译（`rule_versions` 不可变）。
7. **站点发布流程**：编辑写入草稿（`site_*` 工作态）→ 租户/平台提交发布请求（带 `config_version`，与当前不符 → `412`）→ 进入集群发布队列（§13）→ `publish.dispatch` 领取后 Server 汇编（各 `ConfigContributor`，基线 = `clusters.confirmed_revision_id`）→ **多层校验**（IR 结构、规则（FR-68）、值白名单、SSRF、保留 Header、循环重定向、**缓存键封闭规则 KCR**（ADR-005 §3.7）；失败返回 `config.validation_failed` 且不产生修订）→ 生成修订并签名 → 创建 `deployments` 与 `deployment_targets`。
8. **灰度**：Server 仅对批次内节点暴露新 `desired`（`strategy ∈ all|canary|batch|single`）；批次内**应参与**的目标全 `HEALTHY` 且失败比例未超阈值才放行下一批（`waiting_offline`/`skipped` 目标不计入，§13）；超阈值暂停（`paused`）并告警，由**平台**继续/重试/取消/回滚。灰度参数只存在于 `clusters.settings_json.rollout`（平台设置）与平台入口 `POST /clusters/{id}/deployments` 的覆盖项——**租户发布接口不接受任何 node/cluster/strategy/批次参数**（ADR-009 §5 规则 R-T1，BLK-02），因而不存在“租户发起的灰度只有平台能恢复”的错位。阈值默认值待基线测试（FR-56）。
9. **节点侧流程与状态机**：`PENDING → DOWNLOADED → VERIFIED → TESTED → APPLIED → HEALTHY`（proto `ApplyState`），失败终态 `FAILED`（保持上一代）或 `ROLLED_BACK`（应用后健康检查失败而回退）；`NOTIFIED`、`waiting_offline`、`skipped`、`expired` 为 Server 侧目标状态（`deployment_targets.state`，DB 事实源）。逐节点逐集群的**实际生效版本**写入 `node_cluster_state`（权威记录），逐步幂等记录在 `deployment_target_steps`。详见 `docs/api/node-protocol.md` §6–7。
10. **多集群并集与节点代**：节点有效配置 = 其各集群 `desired` 修订的并集。**一个 OpenResty 进程只有一份 `nginx.conf`**，故节点以**节点代（node generation）**为渲染与回退单位：节点代 `g<N>` = `{cluster_uid → (lineage, seq)}` 集合，单一渲染目录、单一 `current`（目录总表见 ADR-020 §2）。Agent 把所有待应用的集群更新合并成一次候选代渲染并 `openresty -t`；候选失败则整体回退 `g<N-1>`，再逐集群单独重试以定位出错集群；`ReportApply` 仍按集群上报。Server 在节点入群与站点绑定时校验：同一节点上主机名不重复、各集群 `settings_json.node_global` 键（`proxy_cache_path` 集合、`lua_shared_dict` 容量、监听端口/HTTP2/TLS 基线、PROXY protocol 入站、日志格式版本）**逐键相等**、站点级可信代理不冲突（反馈 F-3 扩展，ADR-020 §4）。冲突直接拒绝入群/绑定/修改集群设置（`node.cluster_conflict`）。
11. **签名密钥轮换**：公钥集 `SigningKeyset` 链式（新 keyset 由旧有效 key 签名）、重叠期内新旧均可验证；详见 ADR-012 与 node-protocol §8。

12. **lineage：备份恢复后 seq 仍可前进**（IMP-07/H6）。`seq` 的“节点拒绝不大于已应用值”防重放，与“不可逆迁移回滚 = 恢复备份”相冲突：恢复后 DB 的 `clusters.current_seq`、`cache_epoch`、`marker_version` 低于节点已持久化的值，新修订会被所有节点永久拒绝、失效标记被 `max()` 吞掉。解决：引入 **`lineage`**（`system_settings.deploy.lineage`，BIGINT，初值 1），写入 manifest、`config_revisions.lineage`、`AppliedState.applied_lineage`、`SiteEpoch/CachePurge*.lineage`。**接受规则**：节点接受修订当且仅当 `(lineage, seq) > (本地 lineage, 本地 applied_seq)` 的字典序；epoch/marker 同样按 `(lineage, value)` 取 max。**恢复流程**：`lingedge backup restore`（及任何 DB 恢复）完成后、Server 启动前执行 `lingedge-server lineage bump`，令 `lineage = max(恢复后 DB 值 + 1, 当前 UNIX 秒)`——取时间戳下界保证“多次恢复/恢复到更早备份”仍单调不重用（`doctor` 检查时钟回拨）；序号比较统一为**严格大于**（architecture §9.3、node-protocol §7 一致）。rollup `generation` 与日志偏移恢复后无需特殊处理（节点重发更高 generation 即 REPLACED；日志 GAP 严格模式默认关闭）。验收：AC-J-5 增“备份→推进若干修订→恢复→发布新修订，所有节点仍可接受；失效标记与 epoch 不被旧值吞掉”。
13. **发布编排：集群内串行队列、已确认基线、毒块隔离、离线追平**（BLK-05/H5）。
    1. **每集群串行**：同一集群同一时刻至多一个**活动部署**（`pending/running/paused`）。其余发布请求写入 `publish_queue`（FIFO，同站点合并，`queued_key` 唯一），Worker 任务 `publish.dispatch` 在持有 `clusters` 行 `FOR UPDATE` 锁时领取队首，**此时才构建修订**——该锁同时串行化“读基线 → 汇编 → 落修订 → 创建部署”整体（单靠 `current_seq` 的 `FOR UPDATE` 只保证 seq 单调，不防丢更新）。
    2. **copy-forward 基线 = `clusters.confirmed_revision_id`**（最近一次全量确认健康的修订），而非“最新修订”；新修订 = 基线块清单 + 本次变更站点的新块（M1 每个修订至多变更一个站点块，不跨站点合并）。因此处于 canary/paused/失败回退中的修订永远不会被别的站点“捎带”推给全部节点。
    3. **毒块隔离**：一个站点块能过 Server 校验却在节点 `openresty -t`/健康检查失败时，该修订整体失败并回退，**但失败修订被放弃**，下一次发布仍从 `confirmed_revision_id` 出发——毒块不会污染其他站点的后续修订，也不会“反复失败”；失败站点保持 `unpublished_changes` 并向租户展示脱敏失败原因码（`failure_summary`）。
    4. **提交点**：只有部署成功（所有应参与目标 `healthy`）才前移 `clusters.confirmed_revision_id`、`sites.published_*`、`site_versions.status=live`（原“发布时即前移”作废）。
    5. **离线/维护节点**：`deployment_targets.state` 增 `waiting_offline`（下发时离线）与 `skipped`（维护，不计入 M）；`deadline_at`（`rollout.target_timeout_s`）超时 → `failed(timeout)`；`waiting_offline` 不阻塞批次、不计失败比例；节点上线后其 `desired` = 集群 `confirmed_revision_id`，应用成功时**回写**该目标为 `healthy` 并刷新部署汇总；超过 `rollout.catch_up_ttl_s` → `expired`。部署终态不因离线节点悬挂（汇总分别计 `waiting_offline`）。
    6. **管理动作全部平台独占**：暂停/恢复/取消/**重试失败目标**（`POST /deployments/{id}/retry`，新增）；租户视图把 `paused` 显示为“发布中”，不提供这些动作。队列阻塞（活动部署 `paused` 过久、排队过长）触发告警，不静默堆积。
14. **节点级回滚与 held_seq**（BLK-06）。`POST /nodes/{id}/rollback`（切回本地上一**节点代**，不产生新修订）经 `node_tasks.kind='node_rollback'`（proto `NodeRollback`）下发。回退后节点对受影响集群设置 `held_seq`（= 被回退掉的 seq），`node_cluster_state.held_seq` 同步；Heartbeat 对 `desired.seq ≤ held_seq` 返回 `ACTION_HOLD`，否则节点会立刻重新应用被回退的修订；平台发布更高 seq 的修订时自动解除。该操作不违反“拒绝 `(lineage, seq)` 不大于已应用值”（它作用于本地已有代，而非新拉取的修订）。

## Binds / Prevents / Rule

- 约束：`node` 模块（修订构建器）、所有 `ConfigContributor`、Node Agent、Admin 的发布/回滚 UI。
- 防止：在线修改已发布配置；节点执行未签名/被篡改配置；回滚破坏单调性导致重放；节点版本无法对账；不同模块各自拼装下发内容。
- 规则（AD-2）：**修订不可变、内容寻址、Ed25519 签名；回滚是新修订；节点实际生效版本逐节点持久记录；集群内发布串行、基线=已确认修订；备份恢复经 lineage 提升。**

## 备选方案与权衡

- **每节点一个修订**：否决——N 个节点 × 每次发布 N 次汇编/签名/存储；与集群并集模型相比无必要。
- **全量快照单一 blob（不分块）**：否决——站点多时每次发布重复存储/传输；分块去重降低存储与传输（`have_blocks` 预留，M1 可不实现块级增量传输）。
- **Server 推送配置到节点**：否决——需要入站控制端口，扩大攻击面；拉取式使节点 NAT 友好且天然离线自治。
- **签名带 `not_after`**：否决——违反 R-4，离线节点可能自停服。
- **修订可就地回滚（改指针）**：否决——破坏单调性与审计；"回滚即新修订"更简单。

## 后果

- 每次回滚都会产生新修订（存储可忽略，因块复用）。
- 集群发布串行：同一集群的发布吞吐受“一次一个部署”约束（M1 规模可接受；P2 评估“合并窗口 + 失败二分”）；队列阻塞需要告警与平台处置。
- 集群级汇编要求所有站点编译结果确定（canonical、无时间戳）以便块复用；需要 golden 测试保证确定性。
- 一节点多集群的冲突校验增加约束（已写入 PRD 反馈 F-3）。

## 可被推翻处

若用户要求"每节点独立配置"（如节点级定制），可在修订之上加"节点覆盖层"（独立表+同样签名）；若要求签名过期，则必须同时放弃 FR-57 的离线继续服务保证。

## 验证

- 修改已发布修订的尝试被拒（DB 触发器测试 + Repository 单测）。
- 篡改 bundle/签名的修订被节点拒绝并上报（`nginx -t` 失败配置保持旧版本继续服务）。
- 灰度：未选中的节点版本不变；`node_cluster_state` 与节点本地 `state.json` 一致。
- golden 测试：相同输入多次汇编得到相同 `manifest_sha256` 与块哈希；`manifest-vectors.json` 供 Server/Agent/Tools 共用。
- 编排：并发发布同一集群 → 串行且无丢更新；站点 A 在 canary/paused/失败回退时站点 B 的发布不携带 A 的块；毒块失败后 B/C 的后续发布不受影响；离线节点上线后目标回写 `healthy`；`retry` 仅重试失败目标。
- lineage：备份恢复后推进 `lineage`，节点仍接受新修订；epoch/marker 不被旧值吞掉（AC-J-5）。
- 节点级回滚后 `ACTION_HOLD` 生效，平台发布更高 seq 后自动解除。

## 修订记录 2026-11-16

依据两份独立评审（`docs/bmad/review-architecture.md`、`review-architecture-internal.md`）原地修订，逐条处理见 `docs/bmad/architecture-revision-log.md`：

- §1/§2/§3（IMP-16）：manifest 的 `blocks[]` 统一为 `{key, sha256, size}`，删除并列 `global{…}`；块内容在 MySQL；清单增 `lineage`；`pkg/canonicaljson` + golden 向量。
- §5（IMP-06）：修订只引用 `certificate_uid`，不含版本/指纹；回滚不回退证书。
- §6/§12（IMP-07、H6、L24）：lineage 机制与严格大于比较；备份恢复后 `lineage bump`；AC-J-5 扩展。
- §7/§8/§13（BLK-05、H5、BLK-02）：集群内发布串行队列 `publish_queue`、基线 = `confirmed_revision_id`、毒块隔离、提交点后移、`waiting_offline`/`skipped`/超时、平台独占的暂停/恢复/取消/重试；租户不再接受灰度参数。
- §9/§10（BLK-04、H4）：节点代与单一渲染目录；`node_global` 键逐键相等；候选代整体回退后逐集群重试。
- §14（BLK-06）：节点级回滚载体 `node_rollback` 与 `held_seq`。
- 原因：原文 copy-forward 基线取“最新修订”会带出未确认/失败灰度；`seq` 单调与备份恢复冲突；多集群并集在“一个进程一份配置”下无落点；租户可操控灰度违反 NFR-14。
