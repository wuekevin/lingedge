# LingEdge 架构修订日志（2026-11-16）

> 依据：`docs/bmad/review-architecture.md`（外部对抗式评审：阻断 7 / 重要 22 / 次要 14，重合项标 ◆）与 `docs/bmad/review-architecture-internal.md`（内部一致性评审：高 10 / 中 9 / 低 5）。
> 权威基线：`docs/prd/LINGEDGE_V2_REQUIREMENTS.md`；PRD：`docs/prd/prd.md`。
> 方法：先逐条**核实**评审（需实测者在本机 Homebrew `mysql@8.0`（8.0.46）临时实例与 Gin v1.12.0 最小用例上实测），再原地修订被评审文档集；ADR 不改编号，每篇被改 ADR 末尾有「修订记录 2026-11-16」。
> 命名/枚举冲突一律**以 OpenAPI 与数据库文档为事实源，ADR 回写**。

## 1. 处理统计

| 来源 | 条目数 | 已修 | 不成立 | 延后 |
|---|---|---|---|---|
| 外部评审（BLK/IMP/MIN） | 43 | 42 | 0 | 1 |
| 内部评审（H/M/L） | 24 | 24 | 0 | 0 |
| **合计（按评审 ID 计，重合项各计一次）** | **67** | **66** | **0** | **1** |

- 「已修」含：设计与文档已修改，但其中个别子项因需 Linux/OpenResty 环境而**待验证**或其中个别子论点经核实**不成立**（括注于结果列，如 BLK-01(c)、IMP-14 的 `available-clusters`），这些在 §3 单列。
- 内部评审各条与外部评审重合者，结果列写“同 X”，改动文件见对应外部条目。
- **整条评审不成立：0 条**；**子论点不成立/不采纳/降级：4 处**（§3，另 IMP-21 经实测成立）；**延后：1 条**（MIN-09，需用户决策）；**待环境验证的验证项：5 处**（§4）。

## 2. MySQL 实测结论（BLK-01 / H1 / H2 / IMP-19 / M12）

环境：本机 Homebrew `mysql@8.0`，**mysqld 8.0.46**；用**独立临时 datadir、独立端口、`--log-bin`、`log_bin_trust_function_creators=0`** 另起实例，测试后已关闭并删除（未触碰本机已在运行的 MySQL 与任何已有数据）。完整结果见 `docs/database/README.md` §9 与可复现脚本 `docs/database/verification/`：

- **通过/失败结论**：STORED 生成列 + 基列 `ON DELETE CASCADE` 失败（ERROR 1215），VIRTUAL 通过；CHECK 引用的 FK 列为 CASCADE/RESTRICT 通过、为 `SET NULL`/`ON UPDATE CASCADE` 失败（ERROR 3823）；前向 FK 失败（ERROR 1824）；分区表无 `pmax` 越界 INSERT 报 ERROR 1526；分区表不能作 FK 父/子（ERROR 1506）；binlog 开启且 trust=0 时仅 `ALL ON db.*` 账号建触发器失败（ERROR 1419），授予 `SET_USER_ID` 或 trust=1 后通过；表级 `SELECT,ALTER,DROP` 的 maint 账号可 `REORGANIZE/DROP PARTITION` 且不能 DROP 其它表；`GRANT OPTION` 迁移账号可在迁移内为 maint 授权；复合 FK 跨租户写入 ERROR 1452；`CHECK(BINARY col=BINARY LOWER(col))` 生效。
- **迁移骨架**：按新顺序（13 个迁移文件）在空库整体执行 `verification/m1-fk-skeleton.sql`，**0 错误，67 张表**（61 张 M1 + 6 张后续阶段样例）；旧顺序在 000001 即失败。
- **规则回归**：`verification/ddl-rules.sql` + `run_ddl_rules.py` 26 个用例 **26/26 通过**。
- **版本门槛**：CHECK 自 **8.0.16** 起强制执行，最低支持版本定为 ≥8.0.16；本机只有 8.0.46，8.0.16 的空库迁移标「等待环境验证」（`make migrate-test` 在 CI/验收机执行）。
- 数据库文档表数量：基线 §18 的 **80 张核心表全部保留**（脚本核对每表均有独立章节），另新增 6 张补充表（`node_tasks`、`deployment_target_steps`、`publish_queue`、`site_versions`、`rollup_windows`，P2 `website_preview_links`）。

## 3. 经核实“不成立/部分不成立”的结论

| 评审子论点 | 结论 | 理由（实测/推理） |
|---|---|---|
| BLK-01(c)：`roles` 的 `scope='platform'→tenant_id IS NULL` 与 `node_certificates` 的 `subject_type` CHECK 会报 ER 3823 | **不成立** | 实测：CHECK 引用的 FK 列在 `ON DELETE CASCADE`（及 RESTRICT/NO ACTION）下可正常建表并生效，只有 `ON DELETE SET NULL`/`ON UPDATE CASCADE` 才报 3823；故仅修 `cache_policies.risk_ack_by`（SET NULL→RESTRICT），其余 CHECK 保留 |
| BLK-01(b) 建议“去掉生成列唯一键改应用层保证” | **不采纳** | 实测 VIRTUAL 生成列 + 唯一索引在 CASCADE/SET NULL/RESTRICT 下均可建，保留库层唯一性（改 VIRTUAL，而不是退回应用层） |
| IMP-14：租户可见 `GET /available-clusters` | **不采纳** | 与 BLK-02“租户不选/不见集群”直接矛盾；放置由平台 `placement.default_cluster_uids` 决定，P5 如需另议不暴露集群身份的 `placement_profile`（architecture §13 Q-10） |
| IMP-02：ngx_lua 对含 CR/LF 的 `Location` 头值的处理 | **降级** | 插值一律按编码形式输出，CR/LF 不可能出现；保留为 PoC-2 的回归断言，不再作为设计风险 |
| IMP-21：Gin 对 `rules:validate` 的行为（评审标“待确认”） | **实测成立** | Gin v1.12.0 不 panic，但冒号后被当作通配参数，`/sites/S1/rulesXXX` 也命中该 handler |

## 4. 延后 / 待环境验证 / 仍需用户决策

- **延后**：MIN-09 WebP/AVIF 编码（是否接受 cgo）——文档已收敛为 P2 只做 PNG/JPEG/GIF。
- **待环境验证（设计已修，验证需 Linux/OpenResty/旧版 MySQL）**：① MySQL 8.0.16 最低版本空库迁移；② IMP-01(4) PCRE JIT 与解释执行下 `lua_regex_match_limit` 的生效差异（PoC-5）；③ IMP-04(3) `lua_shared_dict` 容量变化时 reload 是否新建 zone（PoC-3）；④ IMP-20 Agent 单元内 `openresty -t` 与生产启动一致（Linux 验收机）；⑤ IMP-03 `ngx.var.le_*` 跨 `ngx.exec` 保留与 access 不重入（PoC-1，失败回退双跳）。
- **仍需用户决策**（汇总）：Q-8 是否接受 cgo（WebP/AVIF）；Q-9 再认证窗口默认 300 秒与 rollout/超时默认值（待基线测试）；Q-10 P5 商业模式是否提供“线路/区域套餐”；Q-5 计量舍入方向（ceil）与口径（`$bytes_sent`）；其余沿用 architecture §13 既有开放项。

## 5. 逐条处理结果

> 结果：已修 / 不成立 / 延后；括注为子项说明。「改动文件」为相对 `docs/` 或项目根的路径（目录以 `/` 结尾表示该目录下相关文件，`ADR-xxx` 位于 `docs/architecture/adr/`）。

### 5.1 外部评审 `review-architecture.md`

| ID | 评审标题 | 结果 | 改动文件 | 说明 |
|---|---|---|---|---|
| BLK-01 | M1 迁移 DDL 在 MySQL 8.0 无法执行 | 已修（(c) 部分不成立） | docs/database/README.md,docs/database/platform-identity.md,docs/database/node-deploy.md,docs/database/rules-cache-cert.md,docs/database/tenant-site.md,docs/database/dns-scheduling-security.md,docs/database/verification/*,docs/architecture/adr/ADR-016,docs/architecture/dev-environment.md | **实测**（MySQL 8.0.46，临时实例，结果写入 database/README.md §9）：(a) 前向 FK 报 ERROR 1824，成立→迁移按依赖拓扑重排（§8.1）、循环 FK 用 `ALTER ADD CONSTRAINT` 并补全清单（§2.4）；(b) STORED 生成列 + 基列 `ON DELETE CASCADE` 报 ERROR 1215，成立，**VIRTUAL 可用**→唯一性生成列改 VIRTUAL，tenant FK 统一 RESTRICT；(c) **部分不成立**：CHECK 引用的 FK 列在 `ON DELETE CASCADE`/RESTRICT 下实测可建（`roles`、`node_certificates` 的 CHECK 保留），仅 `ON DELETE SET NULL`/`ON UPDATE CASCADE` 报 ERROR 3823，故只改 `cache_policies.risk_ack_by` 为 RESTRICT。新增 `verification/m1-fk-skeleton.sql`（空库全量 0 错误，67 表）与 26 个断言用例（26/26 通过）；`make migrate-test` 增 DDL 预检门禁 |
| BLK-02 | 租户 API 暴露并可操控节点/集群拓扑 | 已修 | docs/api/openapi.yaml,docs/api/README.md,docs/architecture/adr/ADR-009,docs/database/tenant-site.md,docs/database/node-deploy.md,docs/ux/screens.md,docs/ux/ia.md | 新增规则 R-T1（ADR-009 §5）：tenant/either 请求/响应不得含 node/cluster 内部标识；租户 `DeploymentCreate` 仅 `{config_version, change_note}`，灰度由 `clusters.settings.rollout`；`SiteDeploymentView`/`OriginHealthView`/`EgressIpList`；调试去 node_uid；集群绑定改平台独占 `/sites/{id}/cluster-bindings`；扫描脚本 27 处泄漏→0 |
| BLK-03 | 条件化回源/改写规则不进缓存键（跨用户泄漏） | 已修 | docs/architecture/adr/ADR-005,docs/architecture/adr/ADR-007,docs/api/openapi.yaml,docs/api/README.md,docs/security/threat-model.md,docs/database/rules-cache-cert.md | 新增**缓存键封闭规则 KCR**（ADR-005 §3.7）：`reads(r) ⊆ keydims(P)`，否则发布失败 `rule.cache_key_dimension_conflict`，或规则声明 `cache_interaction=bypass`（命中请求既不读也不写缓存）；澄清逻辑阶段 7 与 nginx content 阶段的物理顺序；验收项 V-1 单元矩阵 / V-2 OpenResty 集成 / V-3 泄漏测试（AC-C-7a/b/c，G-10 扩展）；§3.6 warning 升级为 error；ADR-007 §1 引用并冻结键维度 |
| BLK-04 | 节点本地布局三处矛盾，多集群并集无落点 | 已修 | docs/architecture/adr/ADR-020,docs/architecture/adr/ADR-004,docs/architecture/adr/ADR-024,docs/architecture/architecture.md,docs/architecture/tools-design.md,docs/api/node-protocol.md,proto/lingedge/node/v1/*.proto,docs/database/node-deploy.md | ADR-020 §2 定义**节点代 g<N>**（{cluster→(lineage,seq)} 复合、单一渲染目录与 `current`）及权威目录总表；统一预检与生产 `-p/-c`（可写路径绝对、只读相对）；`node_global` 键逐键相等的入群校验；多集群候选代失败整体回退再逐集群重试；Agent `ReadWritePaths` 覆盖 cache/runtime |
| BLK-05 | 集群级修订发布编排缺口 | 已修 | docs/architecture/adr/ADR-002,docs/architecture/adr/ADR-019,docs/database/node-deploy.md,docs/api/openapi.yaml,docs/api/node-protocol.md,proto/lingedge/node/v1/*.proto | ADR-002 §13：每集群串行 `publish_queue`、copy-forward 基线=`confirmed_revision_id`、毒块隔离（失败修订被放弃）、提交点后移到部署成功、`waiting_offline`/`skipped`/超时与追平回写、暂停/恢复/取消/重试平台独占；新增 `POST /deployments/{id}/retry`；租户不再接受 strategy/failure_policy |
| BLK-06 | proto uid/幂等键与逐节点任务队列无 DB 落点；节点级回滚无载体 | 已修 | docs/database/node-deploy.md,docs/database/rules-cache-cert.md,docs/database/README.md,proto/lingedge/node/v1/*.proto,docs/api/node-protocol.md,docs/api/openapi.yaml | 新增 `node_tasks`（C2u，租约/attempt/回执）、`deployment_target_steps`（ReportApply 幂等）；`deployment_targets`/`node_events`/`login_logs`/`task_events` 增 uid/event_uid（新简写 C2u）；`cache_task_nodes.node_task_id`；proto 新增 `NodeRollback` 与 `held_seq`（ADR-002 §14）；`TaskType` 同步 |
| BLK-07 | 节点升级特权路径：root 向 Agent 属主目录写文件 | 已修 | docs/architecture/adr/ADR-024,docs/architecture/tools-design.md,docs/security/threat-model.md,proto/lingedge/node/v1/*.proto,docs/api/openapi.yaml,docs/api/node-protocol.md | 请求仅 `{component, version, task_uid}`（无路径/staged_files）；root 侧 dirfd+`O_NOFOLLOW`+`st_nlink==1`；**结果文件写 root 属主目录 `/var/lib/lingedge-node-upgrade/`**（0644，`O_EXCL`）；`component ∈ {node, openresty}` 在 proto/OpenAPI/ADR/threat-model 统一 |
| IMP-01 | 正则：超限 fail-open、RE2 子集≠无灾难回溯、匹配上限共享 | 已修（(4) 延后到 PoC-5） | docs/architecture/adr/ADR-005,docs/api/openapi.yaml,docs/architecture/architecture.md | EDA（指数歧义）检测取代“嵌套量词启发式”；输入长度上限；`regex` 带 `on_limit`，access_control 默认 `fail_closed`；PoC-5 增“恶意输入下 p99 与跨租户影响”。(4) PCRE JIT/解释执行下 `lua_regex_match_limit` 的差异需 OpenResty 实测，保留为 PoC-5 待验证项 |
| IMP-02 | 重定向/变量插值语义 | 已修（CRLF 项降为回归断言） | docs/architecture/adr/ADR-005,docs/api/openapi.yaml,docs/ux/screens.md | 变量表（来源/解码/输出编码）；所有插值按编码形式输出（值内不会出现 CR/LF），`location` 须以单个 `/` 或字面量 `http(s)://主机/` 开头，结果不得以 `//`、`/\` 开头；`strip_prefix` 取代 `scope.path_prefix` 与 `${path_suffix}` 歧义；UX `{path_tail}` 统一 |
| IMP-03 | `ngx.exec` 销毁 `ngx.ctx` 并重入阶段 | 已修 | docs/architecture/adr/ADR-004,docs/architecture/adr/ADR-006,docs/architecture/adr/ADR-005,docs/architecture/architecture.md | 跨 exec 状态只经 `map` 预声明的 `ngx.var.le_*`；`le_engine_done` 防重入；`$le_client_ip` 改 `ngx.var`；PoC-1 增显式通过条件，失败回退双跳并评估日志去重 |
| IMP-04 | 共享字典：LRU 静默淘汰、跨租户挤占、reload 清空 | 已修（(3) reload 行为待 PoC-3） | docs/architecture/adr/ADR-007,proto/lingedge/node/v1/*.proto,docs/database/rules-cache-cert.md,docs/api/openapi.yaml | `safe_set`；每站点标记配额；超配额/失败时触发站点本地升级并由 Server 权威上调 `cache_epoch`；reload 后从 state.json 重注入；删除 `PurgeSiteRequest.cluster_uid` 与 `cache_tasks.cluster_id`（epoch 为站点级）；Service 层 purge 限额。shm 容量变化时 reload 是否新建 zone 需 PoC-3 实测 |
| IMP-05 | mTLS：续期非原子、CA 轮换与 Server 叶证书未定义 | 已修 | docs/architecture/adr/ADR-003,docs/api/node-protocol.md,proto/lingedge/node/v1/*.proto,docs/database/node-deploy.md,docs/database/platform-identity.md,docs/architecture/tools-design.md,docs/security/threat-model.md | 续期两阶段（pending→首次握手后 active，旧证书 superseded+宽限）；Server 叶证书（同一 CA 签发、serverAuth、SAN、`signing_keys.purpose='grpc_server'`、热重载、自动轮换）；CA 有效期 10 年；CA 轮换 S1–S5 分阶段门禁，新增 `GetCaBundle`/`ca_bundle_version`/`DIRECTIVE_REFETCH_CA_BUNDLE` |
| IMP-06 | 证书版本与不可变修订/回滚冲突 | 已修 | docs/architecture/adr/ADR-002,docs/database/rules-cache-cert.md,proto/lingedge/node/v1/*.proto,docs/api/node-protocol.md | 选方案 (a)：修订只引用 `certificate_uid`，不含版本/指纹；节点取当前活动版本；回滚不回退证书；回滚前校验证书未删除 |
| IMP-07 | `seq` 单调防重放与数据库恢复冲突 | 已修 | docs/architecture/adr/ADR-002,docs/architecture/adr/ADR-013,docs/architecture/tools-design.md,docs/database/node-deploy.md,proto/lingedge/node/v1/*.proto,docs/api/node-protocol.md,docs/security/threat-model.md | 引入 `lineage`（`deploy.lineage`，恢复后 `lingedge-server lineage bump` = max(恢复值+1, UNIX 秒)）；节点按 (lineage, seq) 字典序严格大于接受，epoch/marker 同；AC-J-5 增“恢复后节点仍能接受新修订” |
| IMP-08 | 访问日志入库信任节点自报 site_uid；dedupe 缺 log_date | 已修 | docs/architecture/adr/ADR-008,docs/database/node-deploy.md,docs/api/node-protocol.md,docs/security/threat-model.md | `site_uid` ULID 正则 + 属于节点所在集群 + realpath 断言；非法整批 `REJECTED_INVALID`、合法未绑定归 `_unattributed`；chunk 按（批次,站点,日期）拆分，`log_date` 并入唯一键；`sha256` 明确为 chunk 未压缩字节哈希 |
| IMP-09 | rollup/usage 契约漂移，缺计量字段 | 已修 | docs/architecture/adr/ADR-010,docs/architecture/adr/ADR-008,docs/database/dns-scheduling-security.md,docs/database/billing-support.md,proto/lingedge/node/v1/*.proto,docs/api/node-protocol.md | 统一 `generation`；同代异值 CONFLICT（新增 `rollup_windows` 判定）；metric 词表对齐 proto（补 `ttfb_ms_sum`、`bandwidth_peak_bps`，删无生产者的 `conn_active_max`）；usage 只取 300 秒粒度、唯一键含 `window_seconds`；删 `pricing_rules.rounding`；直方图落 `req_time_bucket` |
| IMP-10 | SSRF 白名单无承载；逐源站 TLS 与静态指令冲突 | 已修 | docs/architecture/adr/ADR-004,docs/database/tenant-site.md,docs/database/platform-identity.md,docs/api/openapi.yaml | `system_settings.ssrf.allow_cidrs`（平台写+再认证+审计）随各集群 `global` 块 `ssrf_allow_cidrs` 进入签名修订；同源站组内 protocol/tls_verify/sni 必须一致（`origin.group_inconsistent`）；按 (TTL 档位×上游变体) 渲染 `@le_cp_<n>`（≤64/节点） |
| IMP-11 | 真实 IP 入站可信代理契约与调试后门 | 已修 | docs/architecture/adr/ADR-006,docs/architecture/adr/ADR-004,docs/architecture/adr/ADR-023,docs/api/openapi.yaml,proto/lingedge/node/v1/*.proto,docs/api/node-protocol.md,docs/ux/screens.md | 入站可信代理判定移入 Lua（纯函数 `le.realip.resolve`，不用 nginx realip）；站点级仅 XFF，proxy_protocol 仅集群级；最小前缀 IPv4 /8、IPv6 /32 与禁用范围（`details.reason`）；调试改 ctl 封闭操作 `debug_simulate`（纯函数，对端地址为函数入参，无回环调试源站/模拟请求分支） |
| IMP-12 | Admin 代租户权限串缺失；L4 重新验证无 API | 已修 | docs/architecture/adr/ADR-018,docs/architecture/adr/ADR-009,docs/api/openapi.yaml,docs/api/README.md,docs/database/platform-identity.md,docs/ux/EXPERIENCE.md | 新增 `tenant:impersonate` 与 acting-as/either 授权语义；`POST /auth/reauth`、`sessions.reauth_at`、窗口 `auth.reauth_window_s`、`403 auth.reauth_required`、`x-reauth` 标注 10 个高危操作 |
| IMP-13 | UX 状态词表与 API 状态模型不对应 | 已修 | docs/api/openapi.yaml,docs/api/README.md,docs/ux/EXPERIENCE.md,docs/ux/ux.md,docs/ux/screens.md | 统一只读 `PublishState`；租户部署状态枚举脱敏；api/README §7「API 枚举→UX 固定词」映射表；校验 422→400、并发 409→412；CMS `scheduled` 改“已排期” |
| IMP-14 | Console 隔离检查与 OpenAPI 路径冲突；租户无可绑定集群列表 | 已修（`available-clusters` 子项不采纳） | docs/architecture/adr/ADR-014,docs/api/openapi.yaml,docs/api/README.md,docs/ux/screens.md | 审计读取迁出 `/admin/*`（`/audit-logs`），新增 `/auth/login-logs`；Console 隔离检查改按 `x-tenant-scope: platform` 的路径模板精确匹配；站点集群绑定改 `/cluster-bindings` 平台独占。**不采纳**租户可见 `GET /available-clusters`（与 BLK-02“租户不选集群/不见集群”矛盾；放置由 `placement.default_cluster_uids` 决定，P5 如需另议 `placement_profile`） |
| IMP-15 | 模块依赖图与必需跨模块调用不一致 | 已修 | docs/architecture/adr/ADR-001,docs/architecture/architecture.md | ADR-001 §5 新增 `platform/ports` 清单（CertificateProvider、SiteDirectory、NodeDirectory、NodeTaskDispatcher、OriginHealthSink、RollupSink、LogSink/LogQuery、OriginGroupLookup、CachePolicyLookup、PlanCatalog），调用方只依赖 ports、实现在装配层注入；architecture §5.3 同步 |
| IMP-16 | 修订内容存放位置自相矛盾；manifest global/l4 表示不一致 | 已修 | docs/architecture/adr/ADR-002,docs/architecture/architecture.md,docs/architecture/tools-design.md,proto/lingedge/node/v1/*.proto,docs/database/node-deploy.md | 块内容存 MySQL（删除“修订 blob 文件”：architecture §3 图、tools §17.1）；manifest `blocks[]` 统一 `{key,sha256,size}`，删除并列 `global{…}`；增 `lineage`；`pkg/canonicaljson` + 共享 golden 向量 |
| IMP-17 | 官网构建时快照与同一签名产物冲突；Nginx 片段不成立 | 已修 | docs/architecture/website-cms-design.md,docs/architecture/adr/ADR-014,docs/architecture/adr/ADR-015,docs/architecture/tools-design.md,docs/ux/website.md | 发行产物禁用构建时快照（运营者自建可显式开启）；安全头 include 进每个含 `add_header` 的 location 并 `always`；官网只反代 `/api/v1/public/`；Admin/Console/官网强制不同主机名（host-only Cookie，Tools 校验） |
| IMP-18 | 发行清单与包签名输入定义不一致 | 已修 | docs/architecture/adr/ADR-012,docs/architecture/tools-design.md | 以 tools §9 为准回写 ADR-012：清单对文件原始字节签名、吊销拆分为 `keys.json.revoked_key_ids`+清单 `revoked_versions`、包 `.tar.gz`、包签名输入；tools §9.6 新增**真实计算**的示例向量（Ruby OpenSSL 与 Go ed25519 双实现验签通过） |
| IMP-19 | 分区表运维与 DB 账号模型不自洽 | 已修 | docs/database/README.md,docs/database/platform-identity.md,docs/architecture/tools-design.md,docs/architecture/adr/ADR-016,docs/architecture/dev-environment.md | 分区表一律加 `pmax` + `REORGANIZE PARTITION`（实测无 pmax 报 ERROR 1526）；账号三分 `app/maint/migrate`（maint 表级 `SELECT,ALTER,DROP`，migrate 带 GRANT OPTION+SET_USER_ID，实测）；审计与业务同事务；向导预检 binlog/触发器权限（实测需 SUPER/SET_USER_ID 或 trust=1）；最低 MySQL ≥8.0.16 |
| IMP-20 | Agent 的 MDWE 会被 `openresty -t` 继承 | 已修（Linux 验证待环境） | docs/architecture/adr/ADR-024,docs/architecture/tools-design.md,docs/api/node-protocol.md | Agent 单元去掉 `MemoryDenyWriteExecute`，L3 验收增“Agent 单元内 `openresty -t` 与生产启动结果一致”；该验证需 Linux+OpenResty，标「等待环境验证」 |
| IMP-21 | `rules:validate`/`certificates:parse` 在 Gin 下不可按字面实现 | 已修（实测成立） | docs/api/openapi.yaml,docs/api/README.md,docs/architecture/adr/ADR-017,docs/architecture/dev-environment.md | **Gin v1.12.0 实测**：不 panic，但冒号后被解析为通配参数，`/sites/S1/rulesXXX` 也命中该 handler，且参数值含冒号→改名 `/sites/{siteId}/rule-validations`、`/certificate-parses`；CI 增“路径不含 `:`”门禁 |
| IMP-22 | ADR-005/006 与 DB、OpenAPI 字段语义漂移 | 已修 | docs/architecture/adr/ADR-005,docs/architecture/adr/ADR-006,docs/api/openapi.yaml,docs/database/rules-cache-cert.md | 以 OpenAPI/DB 为事实源回写：`query_policy` 取值、危险开关 `allow_cache_*`（M1 无 `ignore_vary`）、规则形态=列+`doc_json{when,action}`、(rule_type→phase) 合法表（`phase` 只读派生，`error_page` 单阶段）、`mode=override` 只覆盖 TTL 不绕过保护、真实 IP 字段映射表 |
| MIN-01 | 登录锁定状态码与账号枚举 | 已修 | docs/architecture/adr/ADR-018,docs/api/openapi.yaml,docs/api/README.md | 统一 429 `rate_limit.exceeded`（Retry-After），删 423/`auth.account_locked`；节流按 identifier 计数而不论账号是否存在 |
| MIN-02 | 发布缺前置条件，所见即所发不成立 | 已修 | docs/api/openapi.yaml,docs/database/tenant-site.md | `DeploymentCreate.config_version` 必填，不符 412 |
| MIN-03 | 保留 Header 名清单有缺口 | 已修 | docs/architecture/adr/ADR-006 | 清单扩充（Accept/Accept-Language/Cache-Control/Pragma/Date/ETag/Last-Modified/Age/Vary/Location/…/True-Client-IP/CF-Connecting-IP 等） |
| MIN-04 | CLI 标志、退出码与状态码多套说法 | 已修 | docs/architecture/tools-design.md,docs/architecture/adr/ADR-012,docs/architecture/adr/ADR-013,docs/architecture/adr/ADR-003,docs/ux/cli-tui.md,docs/api/node-protocol.md,docs/api/openapi.yaml | 以 tools §4 为准回写：`--accept-irreversible`、`--force-reenroll`、`--allow-expired-manifest`、删 `--allow-rollback`、cli-tui 退出码逐项对齐；setup 完成后统一 410 `setup.already_installed` |
| MIN-05 | “expand 回退二进制即可”承诺偏强；KEK 移除与恢复冲突 | 已修 | docs/architecture/adr/ADR-013,docs/architecture/adr/ADR-011,docs/architecture/tools-design.md | 措辞限定为升级失败的即时回滚；备份清单记 `kek_ids_required`，移除被保留备份需要的 KEK 须显式 `--drop-backup-compat` |
| MIN-06 | Redis 键命名两套；广播用途清单不完整 | 已修 | docs/architecture/adr/ADR-025,docs/architecture/adr/ADR-009,docs/architecture/adr/ADR-019 | 统一 `le:<env>:<模块>:<用途>:<标识>`（`<env>` 为占位符，去掉花括号），租户段 `t<tenant_uid>`；补记证书吊销广播用途 |
| MIN-07 | threat-model 与 ADR/协议不一致 | 已修 | docs/security/threat-model.md,docs/architecture/adr/ADR-003 | TLS 1.3；§5.7 落为 Tools 强制的独立主机名；§6.8 与 ADR-024 一致 |
| MIN-08 | 官网/CMS 小问题（revalidate 回环检查、预览令牌入 URL、/media 权限） | 已修 | docs/architecture/website-cms-design.md,docs/architecture/adr/ADR-015,docs/architecture/tools-design.md | revalidate 仅 HMAC（Nginx 对公网 404、Server 直连 127.0.0.1:3000）；预览令牌置 URL fragment、页面脚本交换为 HttpOnly cookie；公开媒体目录 `/var/lib/lingedge/media/`（0755） |
| MIN-09 | WebP 重编码需要 cgo | 延后（需用户决策） | docs/architecture/website-cms-design.md | 已按评审建议收敛：P2 只做 PNG/JPEG/GIF 重编码（Go 标准库）；WebP/AVIF 待用户对 cgo 的决策（architecture §13 Q-8） |
| MIN-10 | enroll 令牌展示与命令行用法不一致 | 已修 | docs/api/openapi.yaml,docs/ux/screens.md,docs/ux/cli-tui.md,docs/architecture/architecture.md,docs/database/node-deploy.md | 展示一律 `--token-file`/stdin、前缀 `lenr1.`、有效期最短 1 小时、令牌绑定节点而非集群；`enroll_tokens` 增 `role` 列 |
| MIN-11 | 域名所有权字段默认值偏危险 | 已修 | docs/database/tenant-site.md,docs/api/openapi.yaml | `verification_status` 默认 `pending`，自用模式由 Service 显式置 `exempt` |
| MIN-12 | 未匹配 Host/SNI 行为未定义 | 已修 | docs/architecture/adr/ADR-004,docs/security/threat-model.md | 固定模板增 default_server：HTTP 444、HTTPS `ssl_reject_handshake on`、HTTP/2 连接复用不匹配 421 |
| MIN-13 | `origins`→`origin_groups` 缺租户/站点复合约束 | 已修 | docs/database/tenant-site.md,docs/database/README.md,docs/database/verification/ddl-rules.sql | `origins` 复合 FK `(site_id, origin_group_id)`→`origin_groups(site_id,id)`（实测通过） |
| MIN-14 | 站点“版本号”口径：站点线性 vs 集群 seq | 已修 | docs/database/node-deploy.md,docs/api/openapi.yaml,docs/ux/EXPERIENCE.md,docs/ux/screens.md | 新增 `site_versions`（站点内单调 `version_no`），租户只见 `version_no`，`seq` 仅平台可见；回滚路径 `/config-versions/{versionNo}/rollback` |

### 5.2 内部评审 `review-architecture-internal.md`

| ID | 评审标题 | 结果 | 改动文件 | 说明 |
|---|---|---|---|---|
| H1 | DDL 无法创建（生成列×CASCADE；CHECK×FK） | 已修（B 部分不成立） | 同 BLK-01 | 规则 A 实测成立；规则 B 仅 SET NULL/ON UPDATE CASCADE 失败，CASCADE/RESTRICT 通过（见 BLK-01） |
| H2 | §8.1 迁移拆分存在前向外键 | 已修 | docs/database/README.md,docs/database/verification/m1-fk-skeleton.sql | 迁移重排；P2 `provider_credentials.plugin_id` 先建无 FK 列，P5 建 `plugins` 后 ALTER；循环 FK 清单补全 |
| H3 | 租户接口越权暴露节点/集群拓扑 | 已修 | 同 BLK-02 | — |
| H4 | 节点本地布局三种描述矛盾 | 已修 | 同 BLK-04 | — |
| H5 | 集群级单序列修订 + 灰度会带出未完成/失败灰度 | 已修 | 同 BLK-05 | — |
| H6 | seq 单调与 DB 恢复冲突 | 已修 | 同 IMP-07 | — |
| H7 | 访问日志入库信任节点自报 site_uid | 已修 | 同 IMP-08 | — |
| H8 | 节点升级特权路径 | 已修 | 同 BLK-07 | A/B/C 三项均已统一 |
| H9 | 官网 Nginx 片段不成立且暴露面过大 | 已修 | 同 IMP-17 | — |
| H10 | proto uid/幂等键在 DB 无落点，且无节点任务队列 | 已修 | 同 BLK-06 | — |
| M11 | OpenResty 机制缺口（exec/ctx、逐源站 TLS、调试入口） | 已修 | 同 IMP-03、IMP-10、IMP-11 | — |
| M12 | MySQL 版本下限与触发器权限 | 已修 | 同 IMP-19 | 最低 ≥8.0.16；迁移账号权限与向导预检写明 |
| M13 | Server 自身 TLS 证书未定义 | 已修 | 同 IMP-05 | — |
| M14 | 状态/枚举与 TaskType 漂移 | 已修 | docs/api/openapi.yaml,docs/architecture/adr/ADR-007,docs/architecture/adr/ADR-017,docs/architecture/adr/ADR-019,docs/api/node-protocol.md,docs/database/platform-identity.md,docs/architecture/architecture.md | OpenAPI `TaskType`（25 项）为唯一事实源，其余文档改引用；`cache_task_nodes.status` 增 `expired` 并给出与 proto 的映射表；`openresty_state` 映射写入 node-protocol §5 |
| M15 | 字段/命名漂移（真实 IP、缓存策略、计量、密文格式） | 已修 | docs/architecture/adr/ADR-005,docs/architecture/adr/ADR-006,docs/architecture/adr/ADR-010,docs/architecture/adr/ADR-011,docs/database/ | 全部以 OpenAPI/DB 为准回写；密文统一 5 段格式、`secret_enc`→`credential_enc`、无 `notification_channels.config_enc` |
| M16 | Tools 与 ADR 的 CLI/格式/数值漂移 | 已修 | 同 MIN-04 | 另：release 保留默认 2；master.key 由 Tools 步骤 18 生成 root:root 0400（`LoadCredential`）；Server 单元 `LimitCORE=0`；enroll 错误码统一 `enroll.token_invalid` |
| M17 | CMS 设计与 DB/OpenAPI 冲突 | 已修 | docs/architecture/website-cms-design.md,docs/database/cms.md,docs/architecture/adr/ADR-015,docs/api/openapi.yaml | 预览令牌改不透明随机令牌+`website_preview_links`；`scheduled_version_id/schedule_error`；区块类型以 `pkg/cmsblocks/schemas` 为准；媒体内容寻址；`website.revalidate_secret` 分发；`/media` 目录权限；OpenAPI 增泛化 `POST /admin/cms/preview-links`，令牌交换在 Website 自己的 SSR 路由内完成（Server 无需新端点） |
| M18 | Admin 代租户权限串缺失 | 已修 | 同 IMP-12 | — |
| M19 | 基线 §26 产物清单未覆盖 | 已修 | docs/architecture/architecture.md | §0 文档地图登记第 11 项 Epic/Story、第 12 项开发依赖关系、第 14 项测试与验收标准由 `docs/bmad/` 下游产出，`docs/operations` 标待产出 |
| L20 | DDL 细节（tenant_key 未定义、open_key 溢出、DATETIME 精度、小写口径、循环 FK 清单） | 已修 | docs/database/README.md,docs/database/dns-scheduling-security.md,docs/database/billing-support.md,docs/architecture/architecture.md | T? 简写统一定义 VIRTUAL `tenant_key`；`open_key`→VARCHAR(255)；`window_start`/账期统一 DATETIME(3)；小写化=应用层+CHECK（architecture §9.1 回写）；循环 FK 清单补全 |
| L21 | 模块布局规则不可执行 | 已修 | docs/architecture/adr/ADR-001,docs/architecture/architecture.md | 统一为根包 `internal/<mod>`（仅 api.go）+ `internal/<mod>/internal/…`，由 Go `internal` 规则强制 |
| L22 | 列表分页、int64 id 暴露、占位 schema uid | 已修 | docs/api/openapi.yaml,docs/database/ | 16 个无分页 `{items}` 列表补分页；`LoginLog/TaskEvent/NodeEvent` 改 uid（DB 增 uid）；`UsageRecord` 去 uid；`WalletEntry/TicketMessage` DB 增 uid |
| L23 | 悬空/过时引用 | 已修 | docs/architecture/dev-environment.md,docs/api/node-protocol.md,docs/database/node-deploy.md,docs/architecture/architecture.md,docs/architecture/adr/ADR-017,docs/security/threat-model.md | dev-environment §4.3→§1.4；AC-FR-52→FR-52 验收；`ApplyStage`→`ApplyState`；architecture §5.3 的 P1→M1；ADR-017 §4 过期句删除；`revoked_serials`→`node_certificates.status='revoked'` |
| L24 | 小技术细节 | 已修 | docs/architecture/adr/ADR-024,docs/architecture/adr/ADR-020,docs/architecture/adr/ADR-002,docs/architecture/tools-design.md,docs/architecture/dev-environment.md,docs/architecture/architecture.md | Agent `RestrictAddressFamilies` 增 `AF_NETLINK`；发行包构建期消除符号链接；Server metrics 9100→9180；节点证书 doctor 阈值 <1/6；seq 比较统一严格大于；dev-environment 删除外部项目名 |

## 6. 关键设计决策索引（均已原地写入对应 ADR / 数据库文档）

| 决策 | 位置 |
|---|---|
| 缓存键封闭规则 KCR（条件化回源/改写不得读键外维度，否则发布失败或强制 bypass）；可测验收 V-1..V-3 | ADR-005 §3.7、ADR-007 §1、threat-model G-10 |
| 租户 API 脱敏规则 R-T1；集群绑定平台独占；`tenant:impersonate`；再认证 | ADR-009 §5、ADR-018 §10/§12、openapi.yaml |
| 节点代 g<N> + 单一 `-p/-c` + `node_global` 逐键相等 | ADR-020 §2/§4a/§4b、architecture §6 |
| 发布编排队列 `publish_queue`、confirmed 基线、毒块隔离、`waiting_offline`、平台独占管理动作 | ADR-002 §13、node-deploy §3 |
| 节点级回滚 `node_rollback` + `held_seq` | ADR-002 §14、task.proto |
| `lineage`：备份恢复后 seq/epoch 提升 | ADR-002 §12、ADR-013、tools §17.5 |
| 逐节点任务队列 `node_tasks`；`deployment_target_steps`；C2u 简写 | node-deploy §4.4/§3.3、README §2.1 |
| `site_uid` 入库强校验与 `_unattributed` | ADR-008 §4、node-protocol §10 |
| root 升级结果文件属主与路径；请求仅 component+version+task_uid | ADR-024 §7、tools §15.3 |
| 官网 Nginx 片段与 Admin/Console/官网独立主机名；发行产物禁用快照 | website-cms-design §3.4/§3.6、ADR-014 §5、ADR-015 |
| Server 叶证书、续期两阶段、CA 轮换 S1–S5、`GetCaBundle` | ADR-003 §3/§4/§12、node-protocol §4 |
| MySQL ≥8.0.16、三类 DB 账号、分区 `pmax`、迁移重排与 DDL 规则 | database/README §2/§5.1/§8/§9、ADR-016 |
| 枚举/命名以 OpenAPI/DB 为事实源（TaskType、缓存策略、真实 IP 字段、计量、密文格式） | ADR-005/006/007/010/011/017/019 的修订记录 |
| 入站可信代理判定移入 Lua；调试改 ctl 纯函数模拟 | ADR-006 §10、ADR-004 §8 |
| `platform/ports` 清单与模块根包规则 | ADR-001 §3/§5 |

## 7. 新增/变更的产物清单

- 新增：`docs/bmad/architecture-revision-log.md`（本文件）、`docs/database/verification/{m1-fk-skeleton.sql, ddl-rules.sql, run_ddl_rules.py}`。
- 修订：`docs/architecture/{architecture.md, tools-design.md, website-cms-design.md, dev-environment.md}`、`docs/architecture/adr/ADR-001…025`（除 021 外均有修订记录；ADR-022 仅 §6 一处同步）、`docs/database/*.md`（7 个）、`docs/api/{openapi.yaml, README.md, node-protocol.md}`、`proto/lingedge/node/v1/*.proto`（6 个，仅向后兼容新增，`protoc` 编译通过）、`docs/security/threat-model.md`、`docs/ux/*`（EXPERIENCE、screens、ia、ux、cli-tui、website 的相关段）。
- 核对无需改动：**ADR-021**（P3 调度预案）——评审未涉及其内容，已核对其与新契约（Heartbeat、Redis、节点代、lineage）无冲突；**ADR-022**（P4 边缘限流预案）仅 §6 同步可信 IP 由 Lua 计算，其余无需改动。
- 校验：`openapi.yaml` 经 `openapi-spec-validator` 校验为合法 OpenAPI 3.1（307 路径 / 486 操作，6076 个 `$ref` 全部可解析，operationId 唯一，每个 operation 带 `x-permissions/x-tenant-scope/x-audit/x-phase`，路径不含 `:`，tenant/either 响应 node/cluster 标识扫描 0 处）；6 个 `.proto` 经 `protoc`（libprotoc 36.1）编译通过；数据库文档仍覆盖基线 §18 全部 80 张表。
- 下游：基线 §26 第 11 项 Epic/Story、第 12 项开发依赖关系、第 14 项测试与验收标准由 `docs/bmad/` 下游产出（architecture.md §0 已登记）。
