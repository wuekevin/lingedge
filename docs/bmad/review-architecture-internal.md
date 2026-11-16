# 架构集跨文档一致性评审（内部评审员，只读）

> 来源：架构代理内部评审员（用 ruby 将 openapi.yaml 转 JSON 比对、protoc 36.1 编译 proto、通读 ADR-001..025 与 database/tools/website/threat-model）。时间：2026-11-16。状态：**未修订**，待架构修订任务处理。格式：问题 → 位置 → 建议。

## 高

**H1. DDL 在 MySQL 8.0 上无法创建**（规则 A 把握较高；规则 B 须在 8.0.x 实测）
- A：STORED/带索引的生成列，其基列上的外键使用了 CASCADE（MySQL 禁止）。位置：`database/platform-identity.md` §2.3 `roles.uniq_key`（`tenant_id` FK ON DELETE CASCADE）；§2.6 `user_roles.tenant_key`。
- B：CHECK 约束引用带 CASCADE/SET NULL 外键的列（错误 3823）。位置：`roles` 的 `scope='platform'→tenant_id IS NULL`；`node-deploy.md` §4.2 `node_certificates` 的 `subject_type='node'=(node_id IS NOT NULL)`；`rules-cache-cert.md` §2.1 `ck_cache_policies_dangerous` 引用 `risk_ack_by`（SET NULL，且该动作自身违反该 CHECK）。
- 建议：这些 FK 改 RESTRICT，由 Service/任务清理子行；或去掉生成列唯一键改应用层保证；把此类 DDL 预检加入 `make migrate-test`。

**H2. §8.1 迁移拆分存在前向外键引用**
- 000001 `system_settings.updated_by`、`tasks.created_by_user_id` 引用 users（000002）；000001 `tasks.tenant_id`、000002 `roles/sessions/api_tokens/user_roles.tenant_id` 引用 tenants（000004）；000008 `site_settings.default_certificate_id` 引用 certificates（000011）；P2 `provider_credentials.plugin_id` 引用 `plugins`（P5 才建，且 platform-identity §5.3 又写 P2 起作为种子）。位置：`database/README.md` §8.1；`platform-identity.md` §1.1/§4.1/§5.2。
- 建议：重排迁移，或统一「先建表、后 ALTER ADD CONSTRAINT」（如 `sites.last_deployment_id`），并补入 README §2.4 循环外键清单。

**H3. 租户接口越权暴露并可操控节点/集群拓扑**
- `POST /sites/{id}/deployments`（tenant scope）的 `DeploymentCreate` 含 `node_uids/strategy/batch_size/max_failures/cluster_uids`；`DeploymentTarget` 返回 `node_uid/node_name`；`debugRealIp` 接受 `node_uid/cluster_uid`；`PUT /sites/{id}/clusters` 暴露 `cluster_name`。违背 architecture §3 第 3 条「租户永不直达节点」、§9.2「平台资源不接受租户主体」、ADR-009 §5、NFR-14。位置：`openapi.yaml` 的 `DeploymentCreate/DeploymentDetail/DeploymentTarget/RealIpDebugRequest/SiteClusterBindings`。
- 建议：租户发布不带节点参数，灰度由平台策略定；租户只见脱敏摘要（健康 N/M、失败阶段）；调试节点由服务端按站点所属集群选取。

**H4. 节点本地布局三种描述相互矛盾，多集群并集无法落地**
- (a) architecture §6：nginx.conf/conf.d/data 在 `/opt/lingedge/openresty`；(b) ADR-004 §7、ADR-020、node-protocol §7：`generations/<cluster>/<seq>/` + `openresty -t -p <该代前缀>`；(c) tools §15.2 unit：`-p /var/lib/lingedge-node/runtime/ -c …/current/conf/nginx.conf`。只有一个 `current` 符号链接，但代目录按集群分、FR-59/F-3 要求多集群并集，未说明如何合成单份 nginx.conf；预检前缀与生产前缀不同，`-t` 不等价；§6 与 ReadWritePaths 缺 `cache/`、`runtime/`。
- 建议：定义节点级「复合代」（各集群 seq 集合 + 单一渲染目录），统一 `-p/-c`，补全 `/var/lib/lingedge-node` 目录表。

**H5. 集群级单序列修订 + 站点级发布 + 灰度会带出未完成/失败灰度**
- copy-forward 基线是「最新修订」：站点 A 处于 canary/paused 或已失败回退时，站点 B 发布会把 A 的新块推给全部节点。位置：ADR-002 §1/§2/§8；`database/tenant-site.md` §2.2 `published_block_sha256` 发布时即前移。
- 建议：copy-forward 以「已全量确认健康的修订」为基线，或同集群同时只允许一个未完成部署（排队）；写入 ADR-002。

**H6. seq 单调防重放与 DB 恢复冲突**
- `irreversible` 回滚=恢复备份（ADR-013、tools §17.5）；恢复后 `clusters.current_seq` 低于节点已应用 seq，节点按 ADR-002 §6 永久拒绝新修订；`cache_epoch`、marker_version 同理。
- 建议：恢复流程须把 `current_seq/epoch` 提升到所有节点上报最大值之上，或给修订加 lineage/epoch；写入 `lingedge backup restore` 验收项。

**H7. 访问日志入库信任节点自报 `site_uid`**
- ADR-008 §4、node-protocol §10 按 JSONL 行内 `site_uid` 拼路径、写 chunk，未校验 ULID 格式及「站点属于该节点所在集群」；被攻破节点（威胁 A4）可路径穿越或污染其他租户日志。threat-model §5.12 假设该字段是「受控字符集」与此矛盾；DB `storage_path` 仅提「写前防穿越」。
- 建议：写前强校验（ULID 正则 + 集群归属，失败返回 `REJECTED_INVALID`），并断言最终路径在日志根内。

**H8. 节点升级特权路径**
- A：root 的 `lingedge-node-upgrade.service` 把 `upgrade.result` 写进 Agent 属主目录 `/var/lib/lingedge-node/staging/`（tools §15.3 第 4 点、ADR-024 §7.4），符号链接/TOCTOU 提权面（仅对读取请求文件做了 `O_NOFOLLOW`）。
- B：threat-model §6.8 称「请求文件仅含版本号，不含路径」，而 ADR-024/tools 请求含 `component`、`staged_files[]`。
- C：proto `NodeUpgrade.component` 注释「固定 node」，ADR-024 允许 `node|openresty`。
- 建议：结果文件写 root 属主目录，`O_EXCL|O_NOFOLLOW` 创建；Agent 通过只读副本或 `lingedge status --json` 取结果；统一三处描述。

**H9. 官网 Nginx 片段技术不成立且暴露面过大**
- A：website-cms-design §3.6 在 server 级 include 安全头（CSP/nosniff/HSTS），而 `location = /runtime-config.js`、`/_nuxt/`、`/` 内又写 `add_header`；nginx 的 location 内出现任何 add_header 就不再继承上级，CSP 实际不生效（§11.4 纵深防御落空）。
- B：`location /api/` 把整个 `/api/` 反代到官网公网源（含 `/api/v1/admin/*`、`/auth/*`、`public/setup/*`）；ADR-014/018 仅靠 Cookie 名隔离 realm，未强制 Admin/Console/官网使用不同源。
- 建议：安全头 include 到每个 location 或改统一机制；反代仅放行 `/api/v1/public/`；在 ADR-014/018 与 Tools 的 Nginx 片段中强制 Admin 独立域名。

**H10. proto 的 uid/幂等键在 DB 无落点，且无节点任务队列**
- uid 缺列：`deployment_target_uid`（`deployment_targets` 为 C2 无 uid）；`NodeTask.task_uid`（`cache_task_nodes` 无 uid）。幂等键缺列：`NodeEvent.event_uid` 去重需唯一列而 `node_events` 无；ReportApply 幂等键 `(deployment_target_uid,state,attempt)` 无承载。队列缺失：`node_upgrade/cert_reload/debug_request` 的逐节点租约与 attempt 无表（`tasks` 无 node 维度）。违反 §9.1「gRPC 一律用 uid」。位置：proto `control/revision/task/telemetry.proto`；`node-deploy.md` §3.2、§4.3；`rules-cache-cert.md` §2.3。
- 建议：给这些表加 `uid/event_uid` 列，新增 `node_tasks`（或 tasks 子行 + node_id）。

## 中

**M11. OpenResty 机制缺口**：`ngx.exec("@le_cp_<n>")`（ADR-004 §5）内部重定向会重建 `ngx.ctx`（PoC 核对），而 ADR-006 §1 把 `$le_client_ip` 存于 `ngx.ctx`、ADR-005 阶段 8–9 同在 access 阶段，exec 后状态会丢→改用 `ngx.var` 载体或重算；同源站组内逐源站 `protocol/tls_verify/host_header/sni`（DB origins、openapi）与 nginx 静态 `proxy_pass` scheme、`proxy_ssl_verify`（不支持变量）冲突，需明确实现；ADR-006 §10/proto `DebugRequest` 的「回环调试源站 + `simulated_peer_ip`」与 `pkg/ssrf` 拒绝回环相抵触，且未定义如何防止调试入口被公网请求触发。

**M12. MySQL 版本下限与触发器权限**：设计依赖 CHECK 真正生效（≥8.0.16），向导（tools §13 步骤 2）、dev-environment §1.1、ADR-016 只校验 ≥8.0；宝塔 MySQL 常开 binlog，建触发器需 SUPER/SET_USER_ID 或 `log_bin_trust_function_creators`，README §5 只写了 `lingedge_migrate` 账号。→ 最低版本提到 ≥8.0.16，写明迁移账号权限并加入向导预检。

**M13. Server 自身 TLS 证书（:9443/:9444）未定义**：签发者、SAN、续期、轮换均未说；enroll Token 的 `ca_fp` 在 node-protocol §3 含糊。→ 明确服务端叶证书由同一私有 CA 签发，补 SAN 与轮换规则（ADR-003、node-protocol §3、tools §13 步骤 7）。

**M14. 状态/枚举与 TaskType 漂移**：cache_task_nodes 状态三套（ADR-007 §6 `pending|running|success|failed|skipped|expired`；DB/OpenAPI `pending/sent/success/failed/skipped_offline`；proto RUNNING/SUCCESS/FAILED/SKIPPED/EXPIRED），映射未定义；`openresty_state` DB/OpenAPI `running/stopped/failed/unknown` vs proto RUNNING/STOPPED/RELOADING/CONFIG_ERROR；OpenAPI `TaskType` 17 个，缺 `debugRealIp` 的 202 Task 类型、ADR-011 `secrets.rewrap`、website §14 `media.cleanup_orphans`；platform-identity §4.1 白名单缺 `cache.purge_prefix/cms.*`，architecture §9.2 以「…」收尾与「白名单枚举」冲突。→ 以 OpenAPI 为唯一事实源，其余引用不另列。

**M15. 字段/命名漂移**：真实 IP（ADR-006 §5 `pass_x_forwarded_for/pass_x_real_ip/pass_custom_header/custom_header_name` vs DB `real_ip_xff_enabled/…` vs OpenAPI `xff_enabled/xri_enabled/custom_enabled/custom_header`）；缓存策略（ADR-005 §3.3 `allowlist|denylist` vs DB/OpenAPI `include_whitelist/exclude_list`；危险开关 ADR-005 §3.2 `ignore_set_cookie/ignore_vary/cache_authorized` vs DB/OpenAPI `allow_cache_set_cookie/allow_cache_authorization`，无 `ignore_vary` 列）；计量（`usage_records` 唯一键 ADR-010 §3 含 `window_seconds` 而 DB 不含；更正计数 `generation` vs `report_seq/revision`；舍入应「单一且显式」而 `pricing_rules.rounding` 有 ceil/floor/half_up；ADR-010 的 `metric_def/per_unit/usage_revisions` 在 DB 无对应；`peak_bandwidth_bps` vs `bandwidth_peak_bps`）；密文格式（ADR-011 §1 5 段 `v1:<kek_id>:<wrapped_dek>:<nonce>:<ct>` vs DB `certificates.private_key_enc/system_settings.value_enc` 4 段 + `enc_key_id` 列；`secret_enc` 实为 `credential_enc`，`notification_channels.config_enc` 不存在）。→ 以 DB/OpenAPI 为准回写 ADR，统一密文格式。

**M16. Tools 与 ADR 的 CLI/格式/数值漂移**：不可逆确认 `--yes-irreversible`(ADR-013) vs `--accept-irreversible`(tools)；重注册 `--reenroll`(ADR-003/node-protocol) vs `--force-reenroll`(tools)；清单过期放行 `--allow-expired`(ADR-012) vs `--allow-expired-manifest`(tools)；ADR-012 `--allow-rollback` 在 tools 不存在（tools 视 sequence 回退为硬失败，退出码 5）；清单签名输入 ADR-012 §1.2 `canonical_json` vs tools §9.2 文件原始字节；吊销字段 ADR-012 manifest `revoked_key_ids` vs tools manifest `revoked_versions`（`revoked_key_ids` 在 keys.json）；包格式 ADR-012 §1.3 `.tar.zst`/`.tar.gz` 二选一 vs tools `.tar.gz`；release 保留份数 ADR-013 §2.7 默认 3 vs tools §8 默认 2；master.key ADR-011 §2「安装向导生成、`root:lingedge 0440`」vs tools §13「Tools 步骤 18 生成、`root:root 0400`」，ADR-011 §9 要求 `LimitCORE=0` 而 tools §15.1 unit 未写；enroll 错误码三套 `node.enroll_token_invalid`/`enroll.token_invalid`/`enroll.token_used`（后者违背「不区分已用/不存在/过期」）。→ 统一并回写。

**M17. CMS 设计与 DB/OpenAPI 冲突**：预览令牌 website §4.4 无状态签名令牌 + `jti` 黑名单 vs DB cms.md §0 版本行 `preview_token_hash`（`uk_wpv_preview`，一个版本只能一个链接）且无 jti 表；预览入口 OpenAPI 仅 pages 的 `preview-links`；定时发布 website §4.5 `scheduled_version_id/schedule_failed` DB 不存在；区块类型 website §7（`feature_grid/plan_table/faq_list/…`）vs DB `block_type`（`features/plans/faq/…`）；媒体存储键 website §12.1 sha256 内容寻址 vs DB `storage_key` 注释 `<ulid>.<ext>`；revalidate 密钥 Server 用 KeyProvider 派生，SSR 进程（用户 `lingedge-web`）如何获得未定义；`/media/` alias 到 `lingedge` 0750 目录，Nginx 用户读不到。

**M18. Admin 代租户权限串缺失**：ADR-018 §10 要求 `tenant:impersonate` 类权限，api/README §4 权限索引没有；`x-tenant-scope: either` 路由授权语义（租户主体 vs 平台主体）未在契约写清。→ 在权限字典与 OpenAPI 补齐，写进 §9.2。

**M19. 基线 §26 产物清单未覆盖**：第 11 项 Epic/Story、第 12 项开发依赖关系、第 14 项测试与验收标准未出现在 architecture §0 文档地图，也无对应文件；基线 §20 的 `docs/operations`、`docs/bmad` 为空（threat-model §11 引用未来 Runbook）。→ §0 登记为「待产出/由下游 BMAD 产出」。

## 低

- **L20 DDL 细节**：`tenant_key` 在 notification_channels/templates、waf_rulesets、ip_lists、acme_accounts、rule_templates、node_pools 唯一键中使用，但 README §2.1 的 T? 简写未定义（仅 user_roles/plugin_configs 显式定义）；`alert_events.open_key` VARCHAR(190) 表达式拼租户前缀 + 最长 190 的 `dedup_key` 会溢出；`metric_rollups/usage_records` 的 `window_start`、invoices 周期用无精度 DATETIME，与契约 §9.1 DATETIME(3) 不符；architecture §9.1「主机名小写生成列 + 唯一索引」vs README §2.2「应用层小写化」；website_pages↔website_page_versions 等循环外键未列入 README §2.4。
- **L21 模块布局规则不可执行**：architecture §5.2「api.go 是唯一允许被其他模块 import 的文件」Go 以包为粒度无法限制；ADR-001 §3 实现放 `internal/<mod>/internal/...` 与 §5.2 子目录写法不一致 → 统一为 ADR-001。
- **L22 列表与标识**：16 个 `{items}` 列表接口无分页（如 `/sites/{id}/rules/{ruleId}/versions`、`/clusters/{id}/sites`、`/admin/cms/faqs|banners|categories`），违反 FR-20/基线 §19；`LoginLog/NodeEvent/TaskEvent` 对外暴露 int64 `id`；P5 占位 schema `WalletEntry/UsageRecord/TicketMessage` 带 `uid` 但对应 DB 表（C2）无 uid。
- **L23 悬空/过时引用**：dev-environment L28「见 §4.3」不存在（应指 §1.4）；node-protocol L96 `AC-FR-52` 非 PRD AC 编号；DB `node-deploy.md` §3.2 与 openapi 的 `ApplyStage` 实为 proto `ApplyState`；architecture §5.3 规则 3 的「P1」应为 M1；ADR-017 §4 称 architecture §9.2 通用码清单未列 `idempotency.key_reuse/resource.precondition_failed` 而 §9.2 已列；threat-model §10/§11 `revoked_serials` 在 DB 对应 `node_certificates.status='revoked'`，无同名表。
- **L24 小技术细节**：Agent unit `RestrictAddressFamilies` 不含 `AF_NETLINK`，Go `net.Interfaces()`（`reported_ips`）会失败；tools §18.1 一律拒绝符号链接/硬链接而 OpenResty/LuaJIT 发行目录常含符号链接，需在构建期消除；Server metrics 默认 9100 与 node_exporter 撞车；节点证书续期阈值（剩余 1/3，ADR-003/node-protocol）与 ADR-020 §9 `doctor` 告警阈值同为「不足 1/3」，正常续期窗口即告警；ADR-002 §6「拒绝 seq ≤ 已应用」vs architecture §9.3「小于」vs node-protocol §7「必须 >」→ 统一为严格大于；dev-environment §8 出现外部项目名 SmartCDN，与 NFR-29「无其它产品名残留」略冲突。

## 已核对、未发现问题
基线第 18 章 80 张核心表全部有章节且阶段标注一致；基线 §24 的 20 项在 threat-model §5.1–§5.20 逐项出现；FR-52..62/70..74/114 均有落点；全文无 Docker 依赖、无固定默认口令、金额无浮点；文档中版本号均能在 dev-environment 找到；OpenAPI 296 路径/474 操作与 README 汇总一致且 operationId 唯一；错误体/分页/Task 状态/节点部署域名证书状态枚举/端口/用户名/证书 90 天与 2/3 续期跨文档一致；文档间「§N」引用除上列例外无悬空。
