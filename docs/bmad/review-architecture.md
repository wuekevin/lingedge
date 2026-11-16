# LingEdge 架构集独立评审（对抗式 / 边界 / 验证缺口）

> 评审日期：2026-11-16　评审方式：只读，人工逐项对抗式评审（未改动任何既有文档，未执行 git commit）。
> 评审对象：基线 `LINGEDGE_V2_REQUIREMENTS.md`、PRD、architecture.md、ADR-001..025、tools-design、website-cms-design、dev-environment、`docs/database/*`（7 个文件）、`docs/api/README.md` + `node-protocol.md` + `openapi.yaml`（用 ruby 解析 YAML，逐个核对 M1 operation 及相关 schema）、`proto/lingedge/node/v1/*.proto`（6 个）、threat-model、`docs/ux/*`（ia / EXPERIENCE / screens / ux / cli-tui 的相关章节）。
> 与内部评审的关系：`docs/bmad/review-architecture-internal.md`（架构代理内部评审）已存在。本报告独立得出结论；**与其重合且本报告独立复核成立的条目标 ◆**，其余为本报告新增。未标 ◆ 的条目不代表内部评审没提到相近现象，只表示本报告的证据与结论是独立的。
> 标注「待确认」= 依赖 MySQL 8.0.x / OpenResty / Gin 实测或用户决策，评审时无法在本机证实。
> 证据位置写法：文件 + 章节（§）或行号；`openapi.yaml` 以 operationId / schema 名定位。

## 汇总

| 严重度 | 条数 | 含义 |
|---|---|---|
| 阻断 | 7 | 不修改则无法按文档实现、或直接违反基线硬性条款；应在契约冻结（ADR-017 §10 `contract-vN`）之前修复 |
| 重要 | 22 | 会导致并行开发出现不一致实现、安全缺口或验收无法成立；冻结前应修，最迟 M1 对应 Story 开工前 |
| 次要 | 14 | 契约/命名/可用性层面的漂移或小缺口，可在修订任务中顺带处理 |

最严重的三条：BLK-03（条件化回源/改写规则绕过缓存键，造成跨用户缓存泄漏，违反基线 §8.3/FR-69）、BLK-02（租户 API 暴露并可操控节点与集群拓扑，违反 NFR-14）、BLK-01（M1 迁移 DDL 在 MySQL 8.0 上无法执行）。

---

## 一、阻断

### BLK-01 ◆ M1 迁移 DDL 在 MySQL 8.0 上无法按文档执行
- **证据**
  - (a) 前向外键（确定）：`database/README.md` §8.1 迁移拆分——000001 的 `system_settings.updated_by`（platform-identity §1.1）、`tasks.created_by_user_id`（§4.1）引用 000002 才创建的 `users`；000001 `tasks.tenant_id`（T?）与 000002 的 `roles/sessions/api_tokens/user_roles.tenant_id` 引用 000004 才创建的 `tenants`；000008 `site_settings.default_certificate_id` 引用 000011 的 `certificates`。
  - (b) STORED 生成列的基列上使用 CASCADE（MySQL 禁止）：`platform-identity.md` §2.3 `roles.uniq_key`（STORED，基列 `tenant_id`，FK `ON DELETE CASCADE`，第 106–108 行）；§2.6 `user_roles.tenant_key`（基列 `tenant_id` FK CASCADE）。
  - (c) CHECK 引用带引用动作的列（ER 3823）：roles 的 `scope='platform' → tenant_id IS NULL`；`node-deploy.md` §4.2 `node_certificates` 的 `subject_type='node'=(node_id IS NOT NULL)`（`node_id` FK CASCADE）；`rules-cache-cert.md` §2.1 `ck_cache_policies_dangerous` 引用 `risk_ack_by`（FK `ON DELETE SET NULL`，且用户被删后该 CHECK 自身即被违反）。
- **问题**：首次 `lingedge-server migrate up`（空库）即会在 000001/000002/000007/000010 失败；而 ADR-016 §5/ADR-013 假设迁移可重入且失败可回滚。
- **建议**
  - `database/README.md` §8.1：重排迁移，或统一「先建表、后 `ALTER TABLE ADD CONSTRAINT`」并把新增的循环/前向外键补进 §2.4 清单。
  - `platform-identity.md`、`node-deploy.md`、`rules-cache-cert.md`：上述 FK 改 `RESTRICT`（子行由 Service/清理任务处理），或去掉 CHECK/生成列改为应用层保证；需要唯一性的生成列用 VIRTUAL。
  - `dev-environment.md` §3 / `make migrate-test`：把「空库在 MySQL 8.0 最低支持小版本全量 up」设为门禁，并尽早在本机 8.0.46 上实测 (b)(c)（待确认，(a) 无需实测）。

### BLK-02 ◆ 租户 API 暴露并可操控节点/集群拓扑
- **证据**（`openapi.yaml`）：`publishSite`（`POST /sites/{siteId}/deployments`，`x-tenant-scope: tenant`）的 `DeploymentCreate` 含 `cluster_uids / strategy / node_uids / batch_size / max_failures / failure_policy`；`getSiteDeployment`（tenant）返回 `DeploymentDetail.targets[].node_uid/node_name/health/error`；`listOriginHealth`（tenant）的 `OriginHealth.node_uid`；`listEgressIps`（tenant）的 `node_uid/node_name`；`debugRealIp`（tenant）接受并返回 `node_uid`；`getSiteClusterBindings` 返回 `cluster_name`。对照：architecture §3 边界规则 3「租户永不直达节点」、ADR-009 §5、PRD NFR-14、基线 §24、`ux/ia.md` §2（租户看不到节点名/IP，仅「边缘出口 IP」）、EXPERIENCE「铁律 6」。
- **问题**：租户可枚举节点、指定灰度节点与策略、把任务投递到指定节点，并读到节点内部错误与健康细节。Console（P5）依赖同一契约，冻结后很难收回。
- **建议**
  - `openapi.yaml`：tenant 端点改用脱敏 schema——`DeploymentSummary`（N/M、失败阶段摘要，无 node 字段）、`OriginHealth` 聚合为「每源站一个状态」、`EgressIpList` 仅 `address/family`；`DeploymentCreate` 去掉 `node_uids/strategy/batch_size/failure_policy`（灰度策略由平台在 `clusters.settings` 决定）；`RealIpDebugRequest` 去掉 `node_uid`（服务端按站点所属集群选择）。
  - `architecture.md` §9.2 / `ADR-009` §5：增加规则「tenant-scope 响应不得含 node/cluster 内部标识」，并把它加入隔离矩阵（NFR-8）的响应字段断言。

### BLK-03 缓存隔离缺口：条件化的回源/改写/回源头规则不进入缓存键
- **证据**：`ADR-005` §2 把 `origin_rule`(8)/`origin_headers`(9) 与 `cache_rule`(6) 同放在 `access_by_lua`，先于缓存查询；§1 变量白名单允许 `${client_ip} ${query} ${uri}` 进入回源头值；§3.6 只对 `request_header` 触及缓存键维度给出编译期 warning；缓存键只含 §3.3 的受控维度。`openapi.yaml` `RuleAction`（`origin` / `origin_request_headers` / `rewrite`）与 `ConditionLeaf` 允许以 Cookie、UA、Header、Query、client_ip 等任意字段决定源站组/Host/路径/回源头。
- **问题**：只要 `when` 或动作值依赖了缓存键之外的维度（例：`Cookie beta=1` → 回源到 beta 源站；`UA contains Mobile` → 改写路径；回源头 `X-Client: ${client_ip}`），不同访客得到不同的源站响应，却共享同一缓存对象。这正是基线 §8.3 / PRD FR-69 要求杜绝的「跨用户缓存泄漏」，且不经过任何「危险开关」，默认保守语义（§3.1）也拦不住。
- **建议**
  - `ADR-005`：新增编译期硬规则——对可缓存路径，`origin_rule / origin_request_header / url_rewrite / request_header` 的条件字段与动作插值变量必须 ⊆ 缓存键维度，否则发布失败（`config.validation_failed`）或强制该规则命中的请求 `bypass` 缓存；把「条件化 origin_rule + 缓存规则」加入泄漏测试 AC-C-7 / G-10。
  - `openapi.yaml` `RuleValidateResult`：增加对应 `code`；`ADR-005` §3.6 的「warning」升级为 error（或要求显式危险开关）。

### BLK-04 ◆ 节点本地布局三处互相矛盾，多集群「并集」无落点
- **证据**：`architecture.md` §6（nginx.conf/conf.d/data 在 `/opt/lingedge/openresty`）与目录表（`generations/<cluster>/<seq>/`、单个 `current`）；`ADR-004` §7、`node-protocol.md` §7 步骤 6/9/10（`openresty -t -p <该代前缀>`，切换 `current`）；`tools-design.md` §15.2 单元（`-t -p /var/lib/lingedge-node/runtime/ -c /var/lib/lingedge-node/current/conf/nginx.conf`）；`ADR-002` §10 / FR-59（有效配置 = 各集群 desired 的并集）；`control.proto` `AppliedState.config_hash` 按集群上报。
- **问题**：(1) 只有一个 OpenResty 进程、一份 nginx.conf，但代目录、seq、状态机都按集群独立——N 个集群如何合成一份渲染产物、「节点代」如何标识/回退/reload 未定义。(2) 预检用 `-p <代前缀>`，生产用 `-p runtime/ -c current/conf/nginx.conf`，相对路径（`lua_package_path`、`data/`、temp 路径）解析不同，`nginx -t` 通过 ≠ 生产可启动。(3) Agent 的 `ReadWritePaths` 不含 cache/runtime 目录（architecture §6 与 tools §15.2 不一致）。(4) F-3 的冲突校验只覆盖主机名/端口/可信代理，未覆盖 `proxy_cache_path`、`lua_shared_dict` 容量、日志格式等全局指令。
- **建议**：`ADR-020`（或新 ADR）定义「节点代 = {cluster→seq} 集合」、单一渲染目录与单一 `current`，ReportApply 仍按集群上报、回退以节点代为单位；统一 `-p/-c` 并在 `architecture.md` §6、`tools-design.md` §15.2、`node-protocol.md` §7 给出同一份目录总表；全局指令改为节点级、取自所属集群 settings，冲突即拒绝入群。

### BLK-05 集群级修订的发布编排存在多处缺口（◆ 含内部评审 H5）
- **证据**：`ADR-002` §1/§2/§6/§8；`node-deploy.md` §1.2 `clusters.current_seq`、§3.1/§3.2；`node-protocol.md` §6；`openapi.yaml` `DeploymentCreate`、`pauseDeployment/resumeDeployment`（platform scope）；`ux/screens.md` §6.1–§6.2。
- **问题**
  1. copy-forward 的基线是集群「最新修订」：站点 A 的修订处于 canary/paused 或失败回退中时，站点 B 的发布会把 A 的未确认块一起推给全部节点。
  2. `current_seq` 的 `FOR UPDATE` 只保证 seq 单调，没规定「读 parent → 汇编 → 落修订」整体串行；并发发布会丢更新。
  3. 毒块：一个站点块能过 Server 校验但在节点 `openresty -t`/健康检查失败，会让整集群修订失败并回退（包括其他租户的变更），此后任何站点发布都携带该毒块反复失败，无隔离机制。
  4. 批次放行条件「批内全部 HEALTHY」对离线/维护节点未定义；UX 要求「维护节点不计入 M、离线节点恢复后自动补发」，但 `deployment_targets` 无超时/skipped 语义，部署可能永远 `running`，补发发生在部署已结束之后时由谁回写 N/M 未定义。
  5. 租户可调用 `publishSite` 并设定 `strategy/failure_policy`，而 pause/resume/cancel 仅平台可用——租户发起的灰度暂停后只能由平台恢复。
  6. UX「重试失败节点」无对应 API。
- **建议**：`ADR-002` 增补：集群内发布串行化（或同集群仅允许一个未完成部署，其余排队）；copy-forward 基线 = 最近一次「全量确认健康」的修订；毒块隔离（节点上报失败的 site_uid → Server 自动生成只回退该块的新修订）；目标级 deadline 与 `waiting_offline` 状态，补发后回写 deployment_targets。`openapi.yaml`：租户发布去掉 strategy（与 BLK-02 合并），新增 `POST /deployments/{id}/retry`（platform）。`node-deploy.md` §3.2：补状态与超时字段。

### BLK-06 ◆ proto 的 uid/幂等键与逐节点任务队列在数据库无落点；节点级回滚无协议载体
- **证据**：`control.proto` `DesiredState.deployment_target_uid`（第 96 行）、`revision.proto` ReportApply 幂等键 `(deployment_target_uid,state,attempt)`、`task.proto` `NodeTask.task_uid`（注释「tasks.uid 或 cache_task_nodes 的对应 uid」第 41 行）、`telemetry.proto` `NodeEvent.event_uid`（第 224 行）；而 `deployment_targets`（node-deploy §3.2）、`cache_task_nodes`（rules-cache-cert §2.3）、`node_events`（node-deploy §4.3）都是 C2（README §2.1：无 uid），`tasks`（platform-identity §4.1）没有 node 维度。`openapi.yaml` `POST /nodes/{nodeId}/rollback`（「切回本地上一代，不产生新修订」）在 `task.proto` 的 oneof 与 `control.proto` 的 `Directive` 中都没有载体。architecture §9.1 规定 gRPC 一律用 `uid`。
- **问题**：`node_upgrade / cert_reload / debug_request` 没有逐节点租约/attempt/回执的存储；`PullTasks`「只能拉取分配给自己的任务」无查询依据；ReportApply/ReportEvents 的幂等无唯一列可依；节点级回滚无法下发。
- **建议**：`node-deploy.md` 新增 `node_tasks(uid, node_id, task_id, kind, payload, attempt, lease_until, deadline, status…)`（或给 `tasks` 加 node_id 并建索引）；给 `deployment_targets`、`cache_task_nodes` 增 `uid`，`node_events` 增 `event_uid UNIQUE`；`task.proto` 增 `NodeRollback`（或 `Directive`），并同步 `openapi.yaml` `TaskType`；`README.md` §2.1 标明哪些 C2 表对 gRPC 暴露 uid。

### BLK-07 ◆ 节点升级特权路径：root 进程向 Agent 属主目录写文件
- **证据**：`tools-design.md` §15.3 第 4 点与 `ADR-024` §决策 7.4：root oneshot 把 `upgrade.result` 写到 `/var/lib/lingedge-node/staging/`（属 `lingedge-node`，文件 0644「root 写」），而只对**读取**请求文件做了 `O_NOFOLLOW`/复制。`threat-model.md` §6.8 称「请求文件仅含版本号，不含路径或命令」，与 ADR-024/tools §15.3 的 `{component, version, staged_files[]}` 矛盾。`task.proto` `NodeUpgrade.component` 注释「固定 node」，ADR-024 允许 `node|openresty`，`openapi.yaml` `NodeUpgradeRequest` 无 component。
- **问题**：被攻破的 Agent（威胁 A4）可在其属主目录预置符号链接/硬链接，诱使 root 进程在写结果文件时覆盖任意文件（提权）；三处文档对请求内容与 component 的描述互相矛盾。
- **建议**：结果文件写入 root 属主目录（如 `/var/lib/lingedge/tools/node-upgrade/`，`O_EXCL|O_NOFOLLOW`），Agent 经只读副本或 `lingedge status --json` 读取；请求只含 `component+version`，staged 路径由 root 侧固定推导；统一 threat-model §6.8、ADR-024、`task.proto`、`openapi.yaml` 的描述。

---

## 二、重要

### IMP-01 正则：超限 fail-open、RE2 子集 ≠ 无灾难回溯、匹配上限全租户共享
- **证据**：`ADR-005` §4（节点用 `ngx.re`/PCRE JIT 执行，设 `lua_regex_match_limit`，「超限视为不匹配」）；threat-model TR-14；`openapi.yaml` `ConditionLeaf.op=regex`。
- **问题**：(1) `(a+)+$` 是合法的 RE2 子集，在 PCRE 回溯引擎上指数爆炸；「嵌套量词启发式拒绝」不完备。(2) 超限=不匹配：对 `access_control` 的 deny 规则，攻击者构造超限输入即可绕过拒绝（fail-open）；对 allow 规则则相反，语义不一致。(3) `lua_regex_match_limit` 是 http/server/location 级指令，所有租户共享，单个租户的病态正则会拖慢同节点其他租户。(4) PCRE JIT 与解释执行下 match limit 的生效方式不同（待确认）。
- **建议**：`ADR-005` §4：每条 regex 规则带 `on_limit` 策略（access_control 默认 fail-closed）；节点侧改用线性时间匹配（Lua 实现的 RE2 子集自动机，或限制 regex 只能是非回溯形态）；Server 侧改用 NFA 歧义检测而非启发式；PoC-5 增加「恶意输入下的 p99 与跨租户影响」验收。

### IMP-02 重定向/变量插值语义：`${path_suffix}` 无法求值，存在开放重定向与 CRLF 风险
- **证据**：`ADR-005` §1 示例 `location:"/new${path_suffix}"` 配 `scope.path_prefix`；而 `openapi.yaml` `RuleScope` 与 DB `scope_json` 只有 `hosts`；`HeaderOp` 变量清单不含 `path_suffix/scheme`；`ux/screens.md` §5.2 用 `{path_tail}`；`ADR-005` §5 对重定向目标只校验「站内相对路径或 http(s)」；`threat-model.md` §6.7 要求禁 CRLF 但 ADR-005/openapi 无落实。
- **问题**：(1) `${path_suffix}` 依赖「被剥除的前缀」，API/DB 没有该概念。(2) `${path}/${uri}` 为解码后的值：请求 `//evil.com` 或 `/%2f%2fevil.com` 会使 `location:"${path}"` 产生 scheme-relative 开放重定向；解码后的 CR/LF 进入 `Location` 头（ngx_lua 的转义行为待确认）。(3) 变量值是否重新编码未定义。(4) 三套占位语法不一致。
- **建议**：`ADR-005` §1/§5 与 `openapi.yaml` `RuleAction`：给出变量表（来源、是否已解码、输出编码）；插值结果统一 percent-encode，并拒绝以 `//`、`\` 开头或含控制字符的结果；redirect 的前缀剥离显式化（`strip_prefix` 或绑定条件中的 `starts_with`）；`ux/screens.md` §5.2 改用同一语法。

### IMP-03 ◆ `ngx.exec` 会销毁 `ngx.ctx` 并重入阶段
- **证据**：`ADR-004` §5（`ngx.exec("@le_cp_<n>")` 选 TTL 档位）；`ADR-006` §1（`$le_client_ip` 存于 `ngx.ctx`）；`ADR-005` §2（阶段 8–9 同在 access）；`ADR-008` §1 日志字段 `rule_hits/cache_policy_hit`；PoC-1 只写「阶段重入」。
- **问题**：OpenResty 的内部重定向会销毁 `ngx.ctx`（新请求拥有空 ctx），命名 location 还会重新进入 rewrite/access 阶段，`access_by_lua` 若配在 server 级会被重复执行；真实 IP、规则命中、规则统计会丢失或重复。
- **建议**：`ADR-004/006`：跨 exec 的状态只经预声明的 `ngx.var.le_*` 传递；`access_by_lua` 带「已执行」标志；把「ctx 丢失 / 阶段重入」列为 PoC-1 的显式通过条件，并评估回退方案（双跳）的延迟与日志重复。

### IMP-04 共享字典：LRU 静默淘汰失效标记、跨租户挤占、reload 清空
- **证据**：`ADR-007` §3（容量水位 80% 或「写入失败」才升级 epoch；「共享字典在 HUP reload 中保留」）；`ADR-004` §8；`rules-cache-cert.md` §2.1 `max_ttl_s` 默认 30 天（marker TTL）；`openapi.yaml` `PurgeUrlsRequest.urls` 最多 1000 条；`tenant_quotas` 强制要到 P5。
- **问题**：(1) `ngx.shared.DICT:set` 在内存不足时会 LRU 强制淘汰**未过期**条目并仍返回成功，不会「写入失败」——别的租户的失效标记可被静默淘汰，已刷新的旧对象重新命中，违反「绝不静默丢弃失效请求」；必须用 `safe_set`。(2) 字典对全节点所有租户共享且 M1 无配额：一个站点大量 purge-url 可把他人的刷新逼成整站失效，或挤出他人标记。(3) reload 时 `lua_shared_dict` 容量若变化（集群 `global` 的 zone 容量参数）会新建 zone、标记全丢，「reload 保留」仅在大小不变时成立（待确认）。(4) `PurgeSiteRequest.cluster_uid` / `cache_tasks.cluster_id` 允许按集群整站失效，但 epoch 是站点级单值（`cache_policies.cache_epoch`，经 Heartbeat 全局下发），无法只失效某集群。
- **建议**：`ADR-007` §3：用 `safe_set` + 按 site 配额 + 总量水位；写失败/水位时升级的主体明确为触发站点；每次 reload 后 Agent 从 `state.json` 重注入并校验；删去 `cluster_uid` 或把 epoch 改为 (site,cluster) 粒度；M1 起在 Service 层给 purge 数量加 per-site 限额。同步 `rules-cache-cert.md`、`openapi.yaml`。

### IMP-05 ◆ mTLS：续期非原子、CA 轮换与 Server 叶证书生命周期未定义
- **证据**：`node-protocol.md` §4、`control.proto` `RenewCertificate`、`ADR-003` §4–§5；`node-deploy.md` §4.2 `node_certificates.status='superseded'`；`node-protocol.md` §4 CA 轮换（并存双 CA → 节点续期 → 撤旧）；`database/platform-identity.md` §5.1 `signing_keys.purpose` 仅 `config_signing/mtls_ca`；`tools-design.md` §13 步骤 7 只生成 CA 与签名密钥。
- **问题**：(1) 续期成功即把旧证书置 `superseded`？若响应丢失/Agent 在落盘前崩溃，节点持旧证书被拒、无法再连主控，只能人工 re-enroll；文档未规定「新证书首次成功握手后才使旧证书失效」。(2) Server 自身 `:9443/:9444` 的 TLS 叶证书：签发者、SAN、私钥存放（无对应 purpose）、有效期、轮换均未定义。(3) CA 轮换期间，仅持旧 CA bundle 的节点无法验证新 CA 签发的 Server 证书，而新 bundle 只能经已认证通道获取——滞后/离线节点会被锁死；CA 自身有效期未定义。
- **建议**：`ADR-003` / `node-protocol.md` §4：两阶段续期（新证书 pending，首次成功 mTLS 后才 supersede；旧证书保留到 `not_after` 或 N 天）；明确 Server 叶证书由私有 CA 签发、SAN 含 `server_url` 主机、`signing_keys.purpose='grpc_server'`、可热重载、轮换早于 CA 轮换；CA 轮换采用交叉签名或 Server 同时出示双链；规定 CA 有效期并加 `doctor` 告警。

### IMP-06 证书版本与「不可变修订 / 回滚」语义冲突
- **证据**：`ADR-002` §5（修订含证书引用「`certificate_uid`、版本、指纹」）；`rules-cache-cert.md` §3.1 `certificates` 单行就地更新 `cert_version/chain_pem/private_key_enc`；`revision.proto` `FetchCertificate(known_version)`；`ux/screens.md` §6.3（回滚前校验「v126 引用的证书已删除/到期则阻止」）；`openapi.yaml` `SiteRollbackRequest`。
- **问题**：续期/替换后旧版本私钥与证书链被覆盖；回滚到引用旧 `cert_version` 的修订时，节点要么指纹校验失败，要么静默使用新版本（破坏「同一修订 = 同一有效配置」）；证书到期后的回滚语义也未定义。
- **建议**：在 `ADR-002`/`ADR-011` 二选一并写明：(a) 修订仅引用 `certificate_uid`、取当前版本，回滚不回退证书；(b) 新增不可变 `certificate_versions`，保留到不再被任何保留修订引用。同步 `openapi.yaml` 回滚校验说明。

### IMP-07 ◆ `seq` 单调防重放与数据库恢复相冲突
- **证据**：`ADR-013` §3 / `tools-design.md` §17.5（含不可逆迁移的回滚 = 恢复升级前备份）；`ADR-002` §6（节点拒绝 `seq ≤ 已应用`）；`node-deploy.md` §1.2 `clusters.current_seq`（在 DB）；`ADR-007`（`cache_epoch`、`marker_version`）。
- **问题**：恢复备份后 DB 中的 `current_seq/cache_epoch` 低于节点已应用/已持久化的值，此后新修订被所有节点永久拒绝，失效标记也被 `max()` 吞掉；rollup 的 `generation`、日志偏移同理。
- **建议**：`tools-design.md` §17.5 / `ADR-002`：恢复流程必须把各单调计数提升到「各节点最近上报值 + 间隔」之上，或引入 `lineage_id`（恢复时 +1，节点接受更高 lineage 的 seq 重置）；把「备份恢复后节点仍能接收新修订」写进 AC-J-5。

### IMP-08 ◆ 访问日志入库信任节点自报 `site_uid`；`uk_alc_dedupe` 缺 `log_date`
- **证据**：`ADR-008` §4 / `node-protocol.md` §10（Server 逐行解析 `site_uid`，拼 `${data}/logs/access/<site_uid>/…`）；`node-deploy.md` §6.1（`storage_path` 仅「写入前防路径穿越校验」）；`threat-model.md` §5.12 假设该字段是受控字符集；`uk_alc_dedupe(node_id, segment_seq, start_offset, end_offset, site_id)` 不含 `log_date`，而 chunk=「批次×站点」，`log_date` 是列、`storage_path` 含日期。
- **问题**：被攻破节点可伪造 `site_uid`（`../..` 或他租户站点），写任意路径/污染他人日志与 rollup（影响计费）；批次跨 UTC 零点时同一去重键下出现两行（不同 `log_date`），触发唯一键冲突；同一 `<node_uid>-<seq>.jsonl.zst` 被多批次追加时，`sha256` 是 chunk 还是文件的哈希未定义。
- **建议**：`ADR-008` / `node-protocol.md` §10：写前强校验（ULID 正则 + 站点属于该节点所在集群，否则 `REJECTED_INVALID`），并断言最终 realpath 在日志根内；chunk 拆分改为（批次, 站点, 日期），`log_date` 并入唯一键；明确 `sha256` 的对象。

### IMP-09 rollup / usage 的契约漂移，且缺少计量所需字段
- **证据**：`ADR-010` §3 唯一键含 `window_seconds`、用 `generation`；`billing-support.md` §4.1 唯一键 `(node_id, site_id, window_start, metric)`、用 `report_seq/revision`、「`>` 才覆盖」；`dns-scheduling-security.md` §5.1 `metric_rollups` 的 upsert 用 `report_seq >=`（同序号即覆盖）；`telemetry.proto` `Rollup.generation` 规则：同代内容不同 → `CONFLICT` 保留首次；`RollupMetrics` 只有 requests/bytes/status/cache/origin/latency 桶/rule_hits/websocket，而 DB 的 `metric` 列表含 `ttfb_ms_sum`、`conn_active_max`、`bandwidth_peak_bps`（ADR-010 的 `peak_bandwidth_bps`）；proto 的 `request_time_buckets` 在 DB 无存放位置；`usage_records` 由「`site_node` 已关闭窗口」派生，粒度（1m/5m）未定。
- **问题**：同代异值在 proto 是 CONFLICT，在 DB 是静默覆盖；`peak_bandwidth_bps` 无法由 60 秒求和类字段计算（无生产者）；若 1m 与 5m 窗口同时派生 usage，在不含 `window_seconds` 的唯一键下会冲突或重复计量。
- **建议**：`ADR-010` / `billing-support.md` / `dns-scheduling-security.md` / `telemetry.proto`：统一为 `generation` + 明确的同代冲突语义；`RollupMetrics` 补充峰值带宽（需 Agent 侧更细粒度采样）等字段，或删除 DB/ADR 中无生产者的指标；规定计量只取一种粒度；给出直方图在 DB 的落点（`dim_sub`/独立表）。

### IMP-10 SSRF 白名单无承载；逐源站 TLS/协议参数与静态 nginx 指令冲突（◆ 含内部评审 M11 的一部分）
- **证据**：`openapi.yaml` `Origin.address`、`tenant-site.md` §3.2、`threat-model.md` §5.5 都写「除非平台白名单」；全文无该白名单的表/设置/`global` 块定义；`ADR-004` §6 由 **Agent** 经 `pkg/ssrf` 守卫校验；`ADR-004` §6 `upstream le_pool_<id> { server 0.0.0.1; balancer_by_lua }`；`tenant-site.md` §3.2 `origins.protocol/tls_verify/sni/host_header` 为逐源站参数。
- **问题**：(1) 自用模式的典型场景是源站在内网，白名单是刚需，但其配置位置、审计、是否随签名修订下发给 Agent（Agent 侧守卫要用）均未定义。(2) 同一源站组内混合 `http/https`、`tls_verify` 开/关、不同 `sni` 时，`proxy_pass` 的 scheme、`proxy_ssl_verify`（不支持变量）是 location 级静态指令，`balancer_by_lua` 只能换 peer，不能换这些参数。
- **建议**：`ADR-004` §6 / `database/README.md`：增 `system_settings.ssrf.allow_cidrs`，并作为 `global` 块字段进入签名修订，变更需审计；Server 保存时与 Agent 连接时使用同一份；`ADR-004`：限定「同一源站组内 protocol/tls_verify/sni 必须一致」，或渲染（类型化、数量有上限的）多 upstream/location 变体。

### IMP-11 真实 IP「入站可信代理」契约与架构冲突；调试请求含可被滥用的模拟入口
- **证据**：`openapi.yaml` `TrustedProxies.header ∈ {x_forwarded_for, proxy_protocol}` 且按站点/继承设置；`ADR-006` §2–§3（PROXY protocol 是集群级、listen 级设置，所有站点共用）；`ux/screens.md` §4.6（每行一个「读取的上游 Header ▾」+ 范围「本站点/所属集群」单选）；`ADR-006` §4（M1 仅 XFF/PROXY）；cluster 级 `trusted_proxies` 仅平台可写（`PUT /clusters`）；`README.md` 错误码 `realip.trusted_proxy_forbidden` 只拒 `0.0.0.0/0`、`::/0`；`ADR-006` §10 / `task.proto` `DebugRequest.simulated_peer_ip`「仅调试模式生效」、`openapi.yaml` `RealIpDebugRequest.simulate_peer_ip`。
- **问题**：(1) 站点级选择 `proxy_protocol` 在共享 listen 上不可实现。(2) UX 的逐行 Header 选择与「范围=所属集群」与 API/架构不一致（租户无权写集群级）。(3) `/1`、`/8` 或公网大段 CIDR 同样使该范围内任何人可伪造客户端 IP，仅禁止 `/0` 不够。(4) 「模拟对端地址」要求生产引擎存在一个可被请求影响的调试分支（等同后门），且与 ADR-023 §A.6「生产代码不得有 testing 分支」冲突；「回环调试源站」又与 `pkg/ssrf` 拒绝回环相抵触（待确认）。
- **建议**：`openapi.yaml` `TrustedProxies`：站点级仅允许 `x_forwarded_for`，`proxy_protocol` 移到 `Cluster.settings`；定义最小前缀长度与禁用范围（回环、节点自身、保留段）；`ux/screens.md` §4.6 去掉逐行 Header 与集群范围选项（M1）；`ADR-006` §10：调试走独立的仅本机 listener（带 Agent 生成的一次性 nonce），peer 模拟在函数入参层完成而非请求层，不进入线上 server 块；明确 ssrf 对该调试源站的豁免范围与生命周期。

### IMP-12 ◆ Admin「代租户」权限串缺失；UX 的 L4 重新验证无 API/ADR 对应
- **证据**：`ADR-009` §5、`ADR-018` §10（「`tenant:impersonate` 类细粒度权限，名称实现时与 OpenAPI 对齐」）；`api/README.md` §4 权限索引无此串；M1 中所有 `x-tenant-scope: tenant` 的 operation 只能由 Admin（platform realm）+`X-Tenant-ID` 调用，但「platform 角色是否持有 `site:write` 等 tenant 范围权限、`permissions.scope=both` 语义」未写；`ux/EXPERIENCE.md`「L4 重新验证身份」（改运行模式、导出密钥、创建/提权管理员…）；`ADR-018` §5 仅有 `mfa_verified_at` 与「近期 MFA」清单（改密、创建 Token、私钥相关）；无 reauth 窗口、无 `/auth/reauth`、无 `auth.reauth_required` 错误码，`PUT /admin/system/run-mode`、`PUT /admin/users/{id}/roles` 无 step-up 声明。
- **建议**：`api/README.md` §4 与权限种子新增 `tenant:impersonate`（或给出 acting-as 的完整授权模型）；`openapi.yaml`：对 L4 操作加 `x-reauth: true`、新增 `POST /auth/reauth`、错误码 `auth.reauth_required`；`ADR-018` 补再认证窗口；`ux/EXPERIENCE.md` 同步。

### IMP-13 UX 状态词表与 API 状态模型不对应
- **证据**：`ux/EXPERIENCE.md`「配置状态表达」固定词表（已保存未发布 / 校验失败 / 发布中·已生效 N/M / 已发布·N/M / 已全部生效 / 发布失败 / 已回滚至 vX）与铁律 3（离线/维护单独计数，如「1 失败 · 1 离线」）；`openapi.yaml` `Deployment.status ∈ pending/running/paused/success/partial_failed/failed/canceled/rolled_back`，`Deployment.summary` 仅 `total/healthy/failed/rolled_back/pending`；同一概念的三套枚举：`Site.config_dirty`(bool)、`RealIpSettings.publish_state ∈ published/unpublished_changes/never_published`、`Rule.publish_state ∈ never_published/published/modified`；`ux/EXPERIENCE.md`「表单校验」写「服务端 422 的字段错误映射回字段」、`ux/ux.md` §6 要「409 冲突语义」，而 `ADR-017`/`openapi.yaml` 校验统一 400、乐观并发为 412；`ux/EXPERIENCE.md` 旅程 6 用「待发布」表示 CMS `scheduled`，与配置「已保存，未发布」撞词；`paused/canceled` 在 UX 无词。
- **建议**：`openapi.yaml` 定义统一的只读 `publish_state`（并为 `Deployment.summary` 增 `offline/maintenance/skipped`），在 `api/README.md` 给出「API 枚举 → UX 固定词」的唯一映射表；`ux/EXPERIENCE.md` 改 422→400、409→412，并区分 CMS 的 `scheduled` 用词。

### IMP-14 Console 隔离检查与 OpenAPI 路径冲突；租户无法获得「可绑定集群」列表
- **证据**：`ADR-014` §5：console 产物 grep 禁止出现 `/api/v1/admin/`、`/nodes`、`/clusters` 子串；`openapi.yaml`：`GET /admin/audit-logs`、`/admin/audit-logs/{id}` 的 `x-tenant-scope: either`（`ux/ia.md` Console「账户设置」也要查审计/登录日志）；`GET/PUT /sites/{siteId}/clusters` 路径本身含 `/clusters`；`SiteCreate.cluster_uids` 要求租户提供集群 uid，但集群列表 `GET /clusters` 是 platform；`ux/screens.md` §2 步骤④在站点创建向导里展示「cluster-a（3 个节点，3 在线）」。
- **问题**：按 ADR-014 的字符串规则，Console 构建要么误报要么被迫放弃检查；租户创建站点所需的集群选择没有租户可见的接口，UX 展示的节点数/在线数又违反租户不见节点的原则（见 BLK-02）。
- **建议**：审计读取移出 `/admin/*`（`/audit-logs`，tenant 视图）；console grep 清单改为按 operation 的路径模板精确匹配（由 `x-tenant-scope: platform` 生成，不用子串）；新增租户可见的 `GET /available-clusters`（仅 uid/展示名/区域，无节点信息），自用模式可由平台固定默认集群；同步 `ADR-014` §5、`ux/screens.md` §2。

### IMP-15 模块依赖图与实际必需的跨模块调用不一致
- **证据**：`architecture.md` §5.3 规则 1–2 与依赖图、`ADR-001` §4：L2 之间互不依赖，`node` 不得 import 任何 L2。而文档实际需要：`node` 的 gRPC `FetchCertificate` 要调 `certificate`（解密私钥）并判定「证书被该节点所属集群的某站点引用」（需要 website）；`ReportOriginHealth` 写 `origin_health`（origin）；`ReportRollups` 写 `metric_rollups`（monitoring）；`cache_task_nodes` 外键 `nodes`、缓存任务向节点派发（cache↔node）；`LogQuery` 校验站点属于 Scope（monitoring→website，ADR-008 §5）；规则动作引用 `origin_group_uid/policy_uid`（ruleengine→origin/cache，openapi `RuleAction`）；`website-cms-design.md` §14 的 cms→billing（`PlanCatalog`）。
- **问题**：图中没有这些边，depguard 规则按图生成会让这些必需路径无法编译，开发时只能临时豁免，边界规则形同虚设。
- **建议**：`architecture.md` §5.3 / `ADR-001`：增加「端口(ports)清单」——在 `platform/ports` 逐一定义接口（`CertificateProvider`、`OriginHealthSink`、`RollupSink`、`SiteDirectory`、`NodeDirectory`…）与装配方向，并据此修订依赖图。

### IMP-16 修订内容的存放位置自相矛盾；manifest 中 `global`/`l4` 块的表示不一致
- **证据**：`architecture.md` §3 图 `FS[(日志分片/修订 blob 本地文件)]`、`tools-design.md` §17.1 备份含「修订 blob」文件；`node-deploy.md` §2.1 `config_blocks.content MEDIUMBLOB`（在 MySQL 内，单块 ≤8MB）。`ADR-002` §3 / architecture §9.3 / `revision.proto` 注释：manifest 为 `blocks[{site_uid,sha256,size}]` 并列 `global{…}`；DB `config_revision_blocks.block_key ∈ global|site:<uid>|l4`、`config_blocks.kind` 含 `global/l4`（把 global 当块）。
- **问题**：存放位置不一致会让备份/恢复出现「DB 与文件错位」、容量估算与清理策略也无从定；manifest 的 canonical 字节若在 Server 与 Agent 对 `global/l4` 的表示不同，签名校验会失败。
- **建议**：统一存放（建议 MySQL 只存元数据 + 内容放文件，或全 DB，并同步 architecture §3、tools §17.1、node-deploy §2.1）；`ADR-002` §3 把 manifest 的 `blocks[]` 统一为 `{key, sha256, size}`（`key ∈ global|site:<uid>|l4`），删除并列的 `global{…}`；增加跨语言 golden 向量测试。

### IMP-17 官网：构建时快照与「同一签名发行产物」冲突；Nginx 片段技术上不成立（◆ 含内部评审 H9）
- **证据**：`website-cms-design.md` §3.4(2)（`nuxt generate` 时调用 `GET /api/v1/public/stats` 生成快照）、§3.5（同一份静态产物适配所有运行模式）、§3.6（构建在 CI 完成并进入签名清单 `variants.static`）；§3.6 Nginx 片段：server 级 `include website-security-headers.conf`，而 `location = /runtime-config.js`、`/_nuxt/`、`/` 内各自 `add_header`；`location /api/` 整体反代到 Server。
- **问题**：(1) 快照来自 CI 所连的 Server，被分发到所有部署后，在浏览器端覆盖之前显示的是别人（或过期）的「节点数」，违反基线 §5.3 / FR-41（生产展示必须真实或标「示意」）。(2) nginx 的 `add_header` 在 location 内出现任何一条就不再继承上级，CSP/nosniff/HSTS 在这些 location 实际缺失；`/api/` 全量反代把 `/api/v1/admin/*`、`/auth/*`、`/public/setup/*` 暴露在官网公网域，仅靠 Cookie 名隔离 realm。
- **建议**：发行产物构建一律禁用快照（`live` 模块渲染「暂无数据」，快照仅用于运营者自建）；片段中把安全头放进每个 location（或统一用 `always` + include）；反代只放行 `/api/v1/public/`；`ADR-014`/`ADR-018`/Tools 片段强制 Admin/Console 使用独立域名。

### IMP-18 发行清单与包的签名输入定义不一致
- **证据**：`ADR-012` §1.2：签名对象为 `"LINGEDGE-MANIFEST-V1\n"+canonical_json(manifest)`，§1.4 `revoked_key_ids` 随清单；`tools-design.md` §9.2：对 `manifest.json` **文件原始字节**签名（「免去规范化歧义」），吊销在 `keys.json` 与 `revoked_versions`；包签名：ADR-012 §1.3 描述为 `SHA256SUMS.sig`，tools §9.3 为 `"LINGEDGE-PKG-V1\n<component>\n<version>\n<platform>\n"||SHA256SUMS`；包格式 ADR-012 `.tar.zst/.tar.gz` vs tools `.tar.gz`。
- **问题**：发布流水线（离线签名）与 Tools 验签是两个独立单元，按各自文档实现必然互不兼容；AC-J-2 无法交叉验证。
- **建议**：以 `tools-design.md` §9 为准回写 `ADR-012`（去掉 canonical_json 与 `revoked_key_ids` 描述）；并在 §9 增加示例向量（输入/签名/公钥）。

### IMP-19 审计等分区表的运行期维护与 DB 账号模型不自洽（◆ 含内部评审 M12）
- **证据**：`platform-identity.md` §3.1 `audit_logs` 按月 RANGE 分区、「新分区由 retention.cleanup/迁移维护任务预建」；`database/README.md` §5 应用账号对 `audit_logs/wallet_entries` 仅 `INSERT,SELECT`，DDL 账号 `lingedge_migrate`「不在常规请求路径持有」；`tools-design.md` §13 步骤 2 向导只创建一个「专用低权限用户」；`threat-model.md` §5.6 称账号分离「由 Tools 创建」。同类分区表：`node_heartbeats`、`probe_results`、`security_events`。
- **问题**：(1) 分区预建/DROP 需要 ALTER 权限，运行期 Worker 并不持有该账号。(2) 若下月分区未预建，`INSERT` 报 1526「no partition for value」，审计/登录日志写入失败，进而可能使事务性写操作整体失败。(3) 宝塔 MySQL 常开 binlog，建触发器需 SUPER/SET_USER_ID 或 `log_bin_trust_function_creators`，向导无预检；设计依赖 CHECK 真正生效（≥8.0.16），而最低版本只写 ≥8.0。
- **建议**：分区加 `pmax VALUES LESS THAN (MAXVALUE)` 兜底，维护任务用 `REORGANIZE PARTITION`；明确 app/maintenance/migrate 三类账号及向导创建步骤；决定「审计写入失败」是否阻断业务（建议不阻断并告警）；向导预检 binlog/触发器权限；`ADR-016`/`dev-environment.md` 把最低 MySQL 版本提到 ≥8.0.16。

### IMP-20 Agent 的 `MemoryDenyWriteExecute=yes` 会被其子进程 `openresty -t` 继承（待确认）
- **证据**：`ADR-024` §3、`tools-design.md` §15.2 `lingedge-node.service` 设 `MemoryDenyWriteExecute=yes`；`ADR-004` §7 / `node-protocol.md` §7 步骤 9：由 Agent 执行 `openresty -t`；ADR-024 自己承认 OpenResty 因 LuaJIT 需要 W+X 不能设 MDWE。
- **问题**：systemd 的 MDWE 是 seccomp 过滤，会被子进程继承；`openresty -t` 将在 LuaJIT/PCRE-JIT 无法分配可执行内存的条件下运行，可能误报失败，或「测试通过」所验证的运行模式（无 JIT）与生产（有 JIT）不同，削弱 FR-55 的「nginx -t 失败即保持旧版本」保证。
- **建议**：Agent 单元去掉 MDWE，或把 `-t` 放到固定的、无 MDWE 的受限 oneshot 单元里执行；L3 验收（Linux 真机）加入该场景。

### IMP-21 OpenAPI 的 `rules:validate` / `certificates:parse` 路径在 Gin 下不可按字面实现（待确认）
- **证据**：`openapi.yaml` 第 4214 行 `/sites/{siteId}/rules:validate`、第 6513 行 `/certificates:parse`；`dev-environment.md` §1.2 Gin v1.12.0；`ADR-017` §2 要求路由表与 OpenAPI 双向一致。
- **问题**：Gin（httprouter 系）把 `:` 视作参数起点，`rules:validate` 会被解析为静态 `rules` + 参数 `:validate`，与 `/rules`、`/rules/{ruleId}` 的兄弟路由冲突或产生歧义（例如匹配 `/rulesXXX`）；路由一致性 CI 会因此失败。冻结后改名属于破坏性变更。
- **建议**：冻结前改为 `POST /sites/{siteId}/rule-validations`、`POST /certificate-parses`（或 `/rules/validate`），并在 Gin 1.12 上用最小用例实测。

### IMP-22 ADR-005 / ADR-006 与 DB、OpenAPI 之间的字段与语义漂移（◆ 含内部评审 M15）
- **证据与问题**
  - 缓存策略枚举：`ADR-005` §3.3 `allowlist|denylist` vs DB/OpenAPI `include_whitelist|exclude_list`。
  - 危险开关：`ADR-005` §3.2 `ignore_set_cookie / ignore_vary / cache_authorized` vs DB/OpenAPI `allow_cache_set_cookie / allow_cache_authorization`（无 `ignore_vary`，另有 `ignore_origin_cache_control`）；且 `RuleAction.cache` 只有 `mode/ttl_s/status_ttls/cache_key/policy_uid`，`mode=override` 是否绕过 `private/no-store/Set-Cookie` 未定义，危险开关只挂在 `cache_policies` 上。
  - 规则文档形态：ADR-005 §1 `{type,priority,enabled,scope,when,action}` vs DB/OpenAPI `RuleDoc={when,action}` + 独立列；`RuleScope` 仅 `hosts`（ADR 示例含 `path_prefix`，见 IMP-02）。
  - 阶段：`error_page` 在 ADR-005 §2 表里映射到「upstream_response / respond」两个阶段，而 `site_rules.phase` 与 `RuleCreate.phase` 是单值；API 允许任意 (rule_type, phase) 组合且无合法矩阵，UX 却称阶段只读由类型决定。
  - 真实 IP 字段名四套：ADR-006 §5 `pass_x_forwarded_for/pass_x_real_ip/pass_custom_header/custom_header_name` vs DB `real_ip_xff_enabled/…` vs OpenAPI `xff_enabled/xri_enabled/custom_enabled/custom_header`。
- **建议**：以 `openapi.yaml` 为唯一事实源回写 ADR-005/006；在 OpenAPI 增加 (rule_type→phase) 合法表并拒绝非法组合；定义 `cache.mode=override` 与危险开关的关系；`error_page` 选定单一阶段或拆为两种 rule_type。

---

## 三、次要

### MIN-01 登录锁定状态码与账号枚举
`ADR-018` §6 写超限返回 `429 rate_limit.exceeded`，`openapi.yaml` login 声明 `423 Locked`，`api/README.md` §5 有 `auth.account_locked`(423)；同时 ADR-018 要求「不区分用户不存在与口令错误」，但锁定态只会出现在存在的账号上，形成存在性预言机。建议统一为 429（或对不存在的账号按 identifier 同样计数并返回相同响应），回写 README/openapi。

### MIN-02 发布操作缺少前置条件，「所见即所发」不成立
`openapi.yaml` `publishSite` 无 `If-Match`/`expected_config_version`；`ux/screens.md` §6.1 先展示变更摘要再点「发布」，期间他人修改则发布了未审阅内容。建议 `DeploymentCreate` 增加必填的 `config_version`（`Site.config_version` 已设计用于 If-Match）。

### MIN-03 保留 Header 名清单有缺口
`ADR-006` §7 的清单不含 `Accept`、`Accept-Language`（ADR-005 §3.3 的缓存键可选维度）、`Cache-Control`、`Pragma`、`Date`、`ETag`、`Last-Modified`、`X-HTTP-Method-Override`、`X-Original-URL`、`X-Rewrite-URL`、`True-Client-IP` 等；自定义头覆盖 `Accept-Language` 会污染缓存键维度与源站协商。建议扩充清单，或改为「允许的前缀白名单 + 保留清单」。

### MIN-04 CLI 标志、退出码与状态码的多套说法（◆ 含内部评审 M16）
不可逆确认：`ADR-013` §2.5 `--yes-irreversible` / `tools-design.md` `--accept-irreversible` / `ux/cli-tui.md` `--accept-irreversible-migration`；退出码：cli-tui §4.2（4=清单签名无效、5=任务锁、6=需确认、9=已回滚）vs tools §4（5=VERIFY_FAILED、6=LOCKED、9=CONFIRM_REQUIRED、10=ROLLED_BACK）；安装完成后向导路径：`ux/ux.md` UX-13 的 404、tools §13 的 410、`api/README.md` `setup.already_installed` 的 404。建议以 `tools-design.md` §4 为准回写 ADR-012/013、`ux/cli-tui.md`，并统一向导状态码。

### MIN-05 「expand 迁移回退二进制即可」的承诺偏强；KEK 移除与备份恢复冲突
`ADR-013` §1/§3、`tools-design.md` §17.2 称含 expand 迁移「回退二进制即可」；但新二进制运行期间可能已写入新枚举值/新列数据（README §2.2：新增枚举走 expand），旧二进制读取可能异常。建议限定为「升级失败的即时回滚」，并要求 N-1 兼容性测试（ADR-016 §9 已有）。另 `ADR-011` §3「旧 KEK 全部重包装后才可移除」与 `tools-design.md` §17.5 恢复旧备份冲突：恢复到 KEK 移除之前的备份会无法解密。建议备份清单记录所需 `kek_id`，移除 KEK 前核对保留备份。

### MIN-06 Redis 键命名两套；广播用途清单不完整
`ADR-009` §10 `t:<tenant_uid>:` vs `ADR-025` §4 `le:{env}:<模块>:<用途>:<标识>`；`ADR-019` §8 的 `le:{env}:task:wake` 里的花括号在 Redis Cluster 下是 hash tag。`ADR-003` §6 用 Redis pub/sub 广播证书吊销，不在 `ADR-025` §1「穷尽」用途清单中。建议统一命名并补记用途。

### MIN-07 threat-model 与 ADR/协议文档不一致
§5.2 写 TLS ≥ 1.2（实现倾向 1.3），ADR-003/node-protocol 为 TLS 1.3；§5.7 称「数据面与控制面不同注册域，部署规范要求」，但 Tools/ADR 没有任何落点（自用单域部署下，租户自定义错误页可与 Admin 处于同一站点范围）；§6.8 与 ADR-024 的矛盾见 BLK-07。

### MIN-08 官网/CMS 的小问题
`website-cms-design.md` §4.3 的 revalidate 路由「仅接受内网/回环来源」——经 Nginx 反代后来源恒为回环，该检查无意义，真正有效的只有 HMAC；§4.4 预览令牌放在 URL query 中会进入 access log/Referer，建议改为 POST 交换短期 cookie；§12.3 的 `/media/` `alias` 指向 `lingedge` 属主的 0750 目录，Nginx 运行用户读不到。

### MIN-09 WebP 重编码需要 cgo（待确认）
`website-cms-design.md` §11.2/§12.1 要求媒体管线「解码后重新编码」并产出 WebP；Go 标准库与 `x/image` 无 WebP 编码器，需 cgo/libwebp，与「单一静态 Go 二进制、避免 CGO」的倾向（ADR-020 备选方案）冲突。建议 P2 先只做 PNG/JPEG 重编码，WebP 视 cgo 决策。

### MIN-10 enroll 令牌的展示与命令行用法不一致
`architecture.md` §8.2、`openapi.yaml` `enroll_command`、`ux/screens.md` §7 的示例把令牌放在 `--token` 命令行（进 `ps`/shell 历史），而 `tools-design.md` §14、`node-protocol.md` §3 要求 stdin/文件；UX 令牌前缀 `le-enroll-…` vs `lenr1.`；UX 的有效期选项「30 分钟」vs `EnrollTokenCreate.ttl_hours` 最小 1；UX/ux.md UX-14「令牌绑定集群」vs ADR-003/DB 绑定 `node_uid`，且 `enroll_tokens` 无 `role` 列（ADR-003 §7 称绑定 role）。

### MIN-11 域名所有权字段的默认值偏危险
`tenant-site.md` §2.3 `site_domains.verification_status` `DEFAULT 'exempt'`（openapi `Domain` 同）：漏写校验分支时，未验证域名默认被视为豁免。建议默认 `pending`，由自用模式/管理员显式置 `exempt`；通配符与具体主机名跨租户重叠语义（F-1 已列开放）也需在 P5 前定。

### MIN-12 未匹配 Host/SNI 的行为未定义
`ADR-004` 没有 `default_server`/默认证书/`444`/`421` 的固定模板定义；HTTP/2 连接合并下的 `421` 处理也未提。建议在 `ADR-004` §2 的固定模板中补齐。

### MIN-13 `origins` → `origin_groups` 缺少租户/站点复合约束
`tenant-site.md` §3.2 `origins.origin_group_id` 仅按 id 外键，而 README §2.4 称高风险表启用复合 FK；建议 `FOREIGN KEY (site_id, origin_group_id) REFERENCES origin_groups(site_id, id)`，使跨站点/跨租户绑定在库层不可能。

### MIN-14 站点「版本号」的口径：UX 要的是站点线性版本，API 给的是集群 seq
`ux/screens.md` §6.3、`ux/EXPERIENCE.md` 的 v126/v127/v128 是站点线性版本；`openapi.yaml` `SiteConfigVersion.seq` 是集群内所有站点共享的修订序号（多集群站点每个集群各有 seq，相邻版本可能只是他人的发布，API 以 `block_sha256` 相同来表示）。租户会看到跳号并侧漏他人发布频率。建议 API 增加每站点单调的 `site_version`（= 站点块变更序号）供 UI 使用，`seq` 仅对平台可见。

---

## 四、待确认汇总（需实测或用户决策）

| 编号 | 需确认内容 | 如何确认 |
|---|---|---|
| BLK-01 (b)(c) | MySQL 8.0.46 上「生成列基列 × FK 引用动作」「CHECK × FK 引用动作」是否报错（其余 (a) 确定） | 在本机 `mysql@8.0` 空库实测 DDL |
| IMP-01 (4) | PCRE JIT / 解释执行下 `lua_regex_match_limit` 的实际生效方式 | Linux + OpenResty PoC-5 |
| IMP-02 | ngx_lua 对含 CR/LF 的 `Location` 头值的处理 | Linux + OpenResty |
| IMP-04 (3) | `lua_shared_dict` 容量变化时 reload 是否新建 zone 并丢数据 | PoC-3 |
| IMP-11 (4) | 调试源站（回环）与 `pkg/ssrf` 的相互作用 | PoC 阶段设计评审 |
| IMP-20 | MDWE 是否被 `openresty -t` 子进程继承并影响 LuaJIT/PCRE-JIT | Linux 验收机 |
| IMP-21 | Gin v1.12 对 `rules:validate` 的路由行为 | 最小用例 |
| MIN-09 | WebP 编码的实现路径（是否接受 cgo） | 用户决策 |

## 五、已核对、未发现问题

- 基线第 18 章核心表均有章节落点；全文未发现 Docker/Compose/K8s 依赖、固定默认口令、金额浮点（`NodeStatus.cpu_percent double` 为展示用，非金额）。
- HTTP/3：架构与 ADR 仅作 P6 预案，M1 不监听 UDP 443，与基线 §2.4 / D-7 一致。
- 缓存失效：未发现以目录扫描/删除实现失效的设计（ADR-007 §7 明确禁止）。
- 数据面不依赖 Redis/Server；配置签名无过期；回滚=新修订且 `seq` 单调（与 IMP-07 的备份恢复边界问题分开看，设计本身与基线 §10.3 一致）。
- 不可逆 Migration 的回滚承诺（恢复备份 + 旧二进制）与基线 §4.7 一致（MIN-05 只涉及 expand 类承诺的措辞）。
