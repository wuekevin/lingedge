# ADR-007 缓存失效方案（URL / 整站版本标记；目录与通配符 PoC）

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §11、§6.7；PRD FR-76、FR-78、FR-79、D-6、反馈 F-4；architecture AD-7、PoC-3/8｜阶段：M1（URL + 整站）、P2（预热、目录/通配符 PoC）

## 背景

基线 §11：URL 刷新、目录刷新、预热、集群级失效**必须设计可验证实现，不得仅靠不受控地扫描/删除缓存目录**。PRD D-6：MVP 提供精确 URL 刷新与整站逻辑失效；目录/通配符/集群级失效为 P2，须先 PoC（失效后抽样 100% 回源、无误删、有任务回执）。OpenResty 开源版没有内置按键清除；第三方 purge 模块存在兼容性与按 URI 枚举 Vary 变体的难题。

## 决策

### 1. 失效即"缓存键版本标记"

缓存键包含两段版本（ADR-005 §3.3）：`site_epoch`（整站）、`url_ver`（URL）。**不物理删除对象**：递增版本使旧键永不命中，旧对象由 `inactive` 与 `max_size`（LRU）淘汰。物理清理为尽力而为，不属于正确性。

**键维度由 ADR-005 §3.3/§3.7 冻结**：缓存键的维度集合（`keydims`）只由缓存策略决定；**`origin_rule`/`origin_request_header`/`url_rewrite`/`request_header`/`error_page` 不得引入键外维度**（缓存键封闭规则 KCR，违反则发布失败或该规则强制 bypass）。因此本 ADR 的失效版本标记对“同一键 ⇒ 同一源站响应”这一前提不需额外假设。

**版本比较与 lineage（IMP-07）**：`site_epoch` 与 URL `marker_version` 的比较一律为 `(lineage, value)` **字典序**取 max（`lineage` 见 ADR-002 §12；数据库恢复后 Server 提升 lineage，使低于节点已持久值的计数不会被 `max()` 吞掉）。下文“取 `max`”均指该元组比较。

### 2. 整站失效（`cache.purge_site`，M1）

- `site_epoch` 是 `cache_policies.cache_epoch`（Server 权威，**站点级单值**，与 lineage 组成 `(lineage, epoch)`）。Server 递增后：① 经 Heartbeat 响应的 `site_epochs`（增量游标）下发；② 同时创建 `cache_tasks` + 每节点 `cache_task_nodes`（业务回执）与 `node_tasks`（逐节点投递/租约/attempt，`NodeTask.task_uid = node_tasks.uid`），经 `PullTasks` 下发（便于每节点回执可视）。**扇出范围 = 站点绑定的全部集群的节点**；M1 **不提供“只失效某集群”**（epoch 是站点级单值，经 Heartbeat 全局下发，无法按集群局部失效；原 `PurgeSiteRequest.cluster_uid` 与 `cache_tasks.cluster_id` 删除，集群级失效列入 P2/PoC-8）。
- Agent 取 `max(本地, 下发)`（`(lineage, epoch)` 比较），**持久化到 `state.json`**，并经本地控制接口写入共享字典 `le_epoch`；重启后从 `state.json` 恢复；离线节点重连后经 Heartbeat 增量补齐（因此失效不因离线丢失）。

### 3. URL 精确失效（`cache.purge_url`，M1）

- 规范化 URL（同 ADR-005 §3.4，含 host、path、按站点 Query 策略规范化后的 query）计算 `url_key = sha256(site_id | host | normalized_uri_and_query)`；节点共享字典 `le_urlver[url_key] = ver`（`ver` 来自 Server 任务的 `marker_version`，Server 侧单调；Agent 取 `max`）。
- 缓存键含 `url_ver = le_urlver[url_key] or 0`，故该 URL **所有 Vary/协议变体**同时失效（无需枚举变体）。
- **标记寿命**：标记 TTL ≥ 站点配置的最大缓存寿命 + 余量（`inactive` 与最大 TTL 取大者，默认上限待配置；`cache_policies.max_ttl_s` 默认 30 天）。到期删除标记前该 URL 的旧对象已必然过期，因此安全。
- **共享字典写入语义（IMP-04）**：`ngx.shared.DICT:set` 在内存不足时会 LRU 强制淘汰**未过期**条目且仍返回成功，会让别的站点的失效标记被静默淘汰（已刷新的旧对象重新命中）。因此**失效标记与 epoch 一律用 `safe_set`**（内存不足时返回错误而不淘汰），并配**每站点标记数配额** `marker_quota_per_site`（配置项，默认待基线测试；`le_urlver` 另维护每站点计数）以防一个站点把他人标记挤出字典。
- **持久化与重注入**：Agent 把未过期标记写入 `state.json`（或同目录文件）。**不假设共享字典在 reload 中保留**：`lua_shared_dict` 容量属节点全局参数（`node_global`，ADR-020 §2），容量变化时 reload 会新建 zone 并丢数据（待 PoC-3 确认）；因此 **每次 reload 后 Agent 从 `state.json` 重新注入全部 epoch 与未过期标记并校验**，重启同理。
- **容量与降级（升级的主体与权威）**：站点标记数超过配额、总容量使用率超过阈值（默认 80%，待配置）或 `safe_set` 写入失败时，Agent 对**触发该写入的站点**自动升级为整站失效：本地置 `site_epoch = 已知 server_epoch + 1`（同 lineage），回执 `escalated_to_site_epoch=true` 并带上新 epoch；**Server 收到后权威上调该站点的 `cache_epoch`**（取与回执的最大值，全局单调，并经 Heartbeat 下发给其余节点），记录并告警。升级是过度失效但安全的；**绝不静默丢弃失效请求。**
- **Service 层限额**：URL 刷新单次 ≤ 1000 条（`cache.purge_limit_exceeded`）+ **每站点每日上限**（配置项，默认待基线测试；`tenant_quotas.cache_tasks_per_day` 于 P5 强制，M1 起 Service 层即按配置项限额），防止单站点大量 purge-url 逼迫他人整站失效或挤占共享字典。

### 4. 预热（`cache.prefetch`，P2）

受控任务：Agent 经**本机回环**以正常请求路径（Host=目标域名，走 OpenResty）发起 GET，使对象按规则入缓存；URL 来自 Task 载荷，经 SSRF/域名归属校验（必须属于该站点域名，否则拒绝）；速率与并发受限；逐节点回执。

### 5. 目录/通配符失效（P2 PoC，通过前入口不可用）

候选方案，PoC-8 比较后择一：
- **A. 前缀版本链**：缓存键纳入路径各级前缀（至深度 N，默认 4，可配）的版本号 `pv(prefix_i)`；目录失效 `/a/b/` = 递增该前缀版本；缺点：深度超过 N 的路径需整体降级；每请求最多 N 次共享字典读取（开销需测）。
- **B. 缓存键索引**：节点维护"键→元数据"的索引（可由 `proxy_cache` 的 `log_by_lua` 记录写入），失效时枚举匹配键后逐个置标记；缺点：内存/一致性成本，索引与磁盘缓存可能漂移。
- 通配符（`*.jpg` 类）按"扩展名维度版本" PoC；**拒绝**无约束的 `*`/正则式批量失效（成本不可控）。
- PoC 判据（FR-79）：失效后抽样请求 100% 回源；无关路径不受影响；任务有每节点回执；报告记录环境与步骤（NFR-28）。未通过：UI/API 不开放入口。

### 6. 任务扇出与回执（M1 无“集群级失效”）

任何失效类型都是 `tasks` 的扇出：每个目标节点一行 `cache_task_nodes`（业务回执）+ 一行 `node_tasks`（`kind=cache_purge_url|cache_purge_site|…`，承载租约、`attempt`、`deadline_at`，`NodeTask.task_uid = node_tasks.uid`，BLK-06）。目标节点 = 站点绑定的全部集群的节点；离线节点在线后补执行（任务 `deadline` 过后标 `expired`，但整站/URL 类的**效果**经 epoch/标记补齐，不依赖 deadline）。

**状态集（以 DB/OpenAPI 为事实源，M14）**：`cache_task_nodes.status ∈ pending | sent | success | failed | skipped_offline | expired`。与 `node_tasks.status`、proto `TaskStatus` 的映射：

| node_tasks.status（投递） | proto TaskStatus（回执） | cache_task_nodes.status（业务/OpenAPI） |
|---|---|---|
| `pending` | — | `pending` |
| `leased` / `running` | `TASK_STATUS_RUNNING` | `sent` |
| `success` | `TASK_STATUS_SUCCESS` | `success` |
| `failed` | `TASK_STATUS_FAILED` | `failed` |
| `skipped` | `TASK_STATUS_SKIPPED` | `skipped_offline`（下发时节点离线/维护且效果由 epoch 补齐）；其余原因的 SKIPPED 仍记 `skipped_offline` 并在 `result_code` 说明 |
| `expired` | `TASK_STATUS_EXPIRED` | `expired` |
| `canceled` | — | `failed`（`result_code=canceled`） |

（早先 ADR 文本中的 `running`/`skipped` 不是独立业务状态。）

### 7. 禁止项

任何代码路径不得 `find/rm`、`os.RemoveAll` 或枚举遍历缓存目录来实现失效（静态检查 + 代码评审）。磁盘清理仅限 Agent 的"过期对象回收任务"（可选 P2，且只删除 `proxy_cache_path` 规则之外的孤儿临时文件）。

## Binds / Prevents / Rule

- 约束：缓存键定义（Lua/渲染）、Agent 运行期数据通道、`cache_tasks`/`cache_task_nodes`、Admin 缓存管理。
- 防止：不同模块对"失效"含义不一致；以扫描删除目录实现失效；离线节点漏失效；共享字典满载后静默失效失败。
- 规则（AD-7）：**缓存失效 = 缓存键中的版本标记（站点 epoch / URL 版本；目录/通配符 PoC 后才开放），禁止扫描删除缓存目录；标记满载时升级为整站失效。**

## 备选方案与权衡

- **第三方 `ngx_cache_purge` 模块**：未选——依赖第三方模块与 OpenResty 构建兼容性；对 Vary 变体与 `levels` 路径推导不总是准确。可作为 P2 PoC 对照。
- **物理删除缓存文件（按 md5 路径）**：只对已知 key 有效，Vary 变体无法枚举，且并发下与 nginx cache manager 竞态；否决。
- **TTL 极短或 `proxy_cache_bypass` 参数**：不满足"失效"语义。
- **所有失效都用 `site_epoch`**：简单但对"刷新一个 URL"过度（整站命中率归零）；故 URL 级独立标记。

## 后果

- 刷新后磁盘不立即释放（反馈 F-4），依赖 LRU/`inactive`。
- 每请求多一次共享字典读取（URL 版本），开销由 PoC-3/NFR-30 基线确认；`url_key` 计算 sha256 可用 `ngx.crc32_long` 近似风险不可接受，保持 sha256（或 128 位截断，实现时评估）。
- 标记表容量与 TTL 配置成为运维参数。

## 可被推翻处

目录/通配符方案（A/B）与深度 N 完全待 PoC；若用户要求 MVP 即含目录刷新（D-6 推翻影响）须先交付 PoC。

## 验证

- AC：刷新后下一次请求回源且返回新内容；任务回执逐节点；离线节点上线后失效补齐（节点故障测试）；共享字典满载降级为整站失效的故障注入（含：`safe_set` 失败不淘汰他人标记、单站点超配额只升级该站点、Server 权威上调 epoch）；reload（含 shm 容量变化）与重启后标记恢复；数据库恢复后 lineage 提升使旧计数不吞新失效；整站 epoch 递增后旧对象不再命中（FR-76 AC）。
- 静态检查：缓存目录遍历/删除调用禁止。

## 修订记录 2026-11-16

评审驱动的原地修订（未改编号）：

- **IMP-04**：§3 失效标记/epoch 一律 `safe_set`（原 `set` 会 LRU 静默淘汰未过期标记，违反“绝不静默丢弃失效请求”），增每站点标记配额；升级为整站失效的主体明确为触发站点，且 Server 权威上调 `cache_epoch`；每次 reload 后从 `state.json` 重注入（`lua_shared_dict` 容量属 node_global，变化时 reload 会新建 zone）；增 Service 层每站点每日 purge 限额。
- **IMP-04 (4)**：删除“集群级整站失效”（`PurgeSiteRequest.cluster_uid`、`cache_tasks.cluster_id`）——epoch 是站点级单值经 Heartbeat 全局下发，无法按集群局部失效；扇出范围 = 站点绑定的全部集群；集群级失效列入 P2/PoC-8。
- **BLK-03（关联）**：§1 引用 ADR-005 §3.7 KCR，缓存键维度冻结，origin 类规则不得引入键外维度。
- **BLK-06 / H10 / M14**：§2/§6 任务经 `node_tasks` 投递（`NodeTask.task_uid = node_tasks.uid`）；`cache_task_nodes.status` 回写 DB/OpenAPI 集 `pending|sent|success|failed|skipped_offline|expired`，并给出与 `node_tasks.status`/proto `TaskStatus` 的映射表。
- **IMP-07 / H6**：epoch 与 URL `marker_version` 按 `(lineage, value)` 字典序取 max，数据库恢复后提升 lineage 以免计数被 `max()` 吞掉。
