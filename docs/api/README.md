---
title: "LingEdge API 文档入口"
status: draft
created: 2026-11-16
updated: 2026-11-16
---

# LingEdge API 文档入口

> 权威顺序：需求基线 > PRD > 本目录文档。总入口见 [architecture.md](../architecture/architecture.md)；ADR 见 [adr/README.md](../architecture/adr/README.md)。

| 面向 | 协议 | 文档 |
|---|---|---|
| Admin / Console / 官网 / 机器调用 | REST `/api/v1`（OpenAPI 3.1） | [openapi.yaml](openapi.yaml) |
| Node / DNS / Reporter ↔ Server | gRPC + mTLS（Protobuf） | [node-protocol.md](node-protocol.md)、`proto/lingedge/node/v1/*.proto` |
| 安装向导（仅无 install.lock 时） | REST `/api/v1/public/setup/*` + `X-Setup-Token` | 见 openapi.yaml 的 `public` 标签、[tools-design.md](../architecture/tools-design.md) §13 |

**REST 与 gRPC 是两套独立的协议与权限体系**：用户 Token 无法调用节点内部接口，节点证书无法调用用户 API（FR-27、NFR-14）。

## 1. 草案规模（由 openapi.yaml 汇总）

- 路径 307 条、操作 486 个；M1 范围写详细（请求/响应/错误/分页/Task），P2–P6 为「路径 + 摘要 + `x-phase` + 主要 schema 名」，字段以 `docs/database/*.md` 为准，阶段到来前须补全并重新冻结。

| 阶段（`x-phase`） | 操作数 |
|---|---|
| M1 | 200 |
| P2 | 128 |
| P3 | 63 |
| P4 | 38 |
| P5 | 56 |
| P6 | 1 |

| 标签（tag） | 操作数 |
|---|---|
| `auth` | 19 |
| `admin` | 31 |
| `tenants` | 9 |
| `sites` | 66 |
| `nodes` | 18 |
| `clusters` | 22 |
| `certificates` | 20 |
| `cache` | 10 |
| `tasks` | 4 |
| `metrics` | 7 |
| `public` | 22 |
| `dns` | 66 |
| `waf` | 22 |
| `l4` | 16 |
| `billing` | 43 |
| `notifications` | 18 |
| `plugins` | 13 |
| `admin/cms` | 80 |

## 2. 约定速查（完整版见 openapi.yaml `info.description`，与 architecture.md §9.2 一致）

- 前缀 `/api/v1`；时间 RFC 3339 UTC；金额 `{amount_minor, currency}`；资源标识 `uid`（ULID）。
- 每个响应返回 `X-Request-ID`；错误体统一 `{"error":{"code","message","message_key","details[]","request_id"}}`。
- 分页：页码 `page/page_size`（默认 20，最大 200）→ `{items,page,page_size,total}`；游标 `cursor/limit` → `{items,next_cursor}`。
- 异步：耗时操作 `202 + Task`（`Location: /api/v1/tasks/{uid}`）；任务类型为白名单枚举。
- **TaskType 事实源与内部周期作业（AA-8，Story 1.3 AC9）**：OpenAPI `TaskType`（25 项）是 API 可见任务类型的**唯一事实源**。**内部周期作业**（`tasks.reap`、`idempotency.cleanup`、`sessions.cleanup`、`partition.maintain`）由 Scheduler 注册，不属于 `TaskType`、不对外暴露、不可经 API 入队；"注册表 ⊆ TaskType" 的一致性 CI（Story 1.14）只覆盖 `TaskType` 范围，内部周期作业以此清单为界（`scripts/contract` 的 `freeze-check` 断言二者互不相交）。
- 幂等：创建类 `POST` 支持 `Idempotency-Key`；乐观并发：`If-Match` + 资源 `version`。
- 租户：Console 主体=其租户；Admin 代租户须带 `X-Tenant-ID`（须 `tenant:impersonate`）；`x-tenant-scope` 取值 `platform|tenant|either|public`；跨租户访问一律 `404 resource.not_found`。
- **租户响应脱敏（R-T1，NFR-14）**：`tenant|either` 的 operation 请求/响应不得含 node/cluster 内部标识；灰度/批次/集群放置仅平台可见可设；租户发布请求只有 `config_version` 与 `change_note`。
- **再认证（L4）**：`x-reauth: true` 的 operation 须先 `POST /auth/reauth`（窗口 `auth.reauth_window_s`，默认 300 秒），否则 `403 auth.reauth_required`。
- 认证：`adminSession` / `consoleSession`（Cookie，互不通用，写操作需 `X-CSRF-Token`）、`apiToken`（Bearer `le_<prefix>_<secret>`）。

## 3. 契约冻结与破坏性变更检测（PRD D-11）

1. 冻结点 CF-1（Story 1.3）：`openapi.yaml`、`proto/` 与 ERD/迁移结构设计打 git 标签 `contract-v1`，官网与基础平台自此可并行开发；评审记录见 `docs/bmad/contract-changes/CF-1.md`。
2. CI 将 PR 与标签版本比较：**删除字段 / 收紧约束 / 改变语义 = 破坏性**，需 ADR 与版本号变更；新增字段/路径/枚举值允许（expand）。工具（Story 1.3 已落地，版本固定在 Makefile）：OpenAPI 用 **oasdiff v1.38.0**（`make openapi-check`，另加 operationId/`x-permissions`/`x-tenant-scope`/`x-reauth` 与本文 §5 错误码索引的比较），proto 用 **buf v1.75.0**（`make proto-check`），迁移用 `migrations/CHECKSUMS`（`make migrations-check`）。破坏性变更须附变更说明与 ADR：`make openapi-check CONTRACT_NOTE=docs/bmad/contract-changes/<说明>.md`（模板 `_template.md`）。oasdiff 默认把「响应枚举新增值」判为破坏，本项目按 expand 约定降级（`scripts/contract/data/oasdiff-severity-levels.txt`），客户端须容忍未知枚举值；而「删除响应可选字段」默认只算提示，本项目升级为破坏。
3. 路由一致性（NFR-4、NFR-22）：CI 校验每条已注册路由在本文件存在，且声明 `x-permissions`、`x-tenant-scope`、`x-audit`、`x-phase`；未声明者构建失败；**路径段不得含 `:`**（Gin 通配歧义，IMP-21）；`x-tenant-scope: tenant|either` 的 schema 不得含 node/cluster 标识（R-T1 静态扫描，见 §6）。以上静态规则已由 `make openapi-check` 落地，另含：`x-reauth: true` 的 operation 集合由 `scripts/contract/data/x-reauth.txt` 固定（IMP-12）；list 类 operation 必须声明分页参数；对外 schema 不得暴露内部整型 `id`（L22）；**商业类 operation（`billing` 标签、`register`、`listPublicPlans`、`createTenant`）必须声明 `x-feature: commercial`**（FR-24：自用模式 403 `feature.disabled`，Story 1.22），未声明或取值未知则失败，运行时路由清单的 feature 须与契约一致。
4. 前端 API 类型自本文件生成，禁止手写重复类型（ADR-014）。
5. 未交付阶段（`x-phase` 晚于当前阶段）的接口：服务端不注册或返回 `403 feature.disabled`；前端不渲染（NFR-1/2）。

## 4. 权限串索引（汇总自各操作的 `x-permissions`）

调用者须**同时**具备操作所列的全部权限串。权限字典落表 `permissions`（`resource:action`，种子迁移同步）。权限的 `scope ∈ platform|tenant|both`：平台角色代租户操作（`X-Tenant-ID`）时，须持有 `tenant:impersonate` 且 operation 所需权限的 scope 为 `tenant` 或 `both`；`x-tenant-scope: either` 的授权语义见 openapi.yaml `info.description`。

| 权限串 | 说明 | 出现于阶段 |
|---|---|---|
| `alert:read` | 告警规则/历史 | P2 |
| `alert:write` | 告警规则/历史 | P2 |
| `audit:read` | 审计日志 | M1 |
| `billing:manage` | 计费/订单/用量/账单 | P5 |
| `billing:read` | 计费/订单/用量/账单 | P5 |
| `billing:write` | 计费/订单/用量/账单 | P5 |
| `cache:disk:read` | 缓存策略/刷新/统计/磁盘 | M1 |
| `cache:purge` | 缓存策略/刷新/统计/磁盘 | M1, P2 |
| `cache:read` | 缓存策略/刷新/统计/磁盘 | M1 |
| `cache:write` | 缓存策略/刷新/统计/磁盘 | M1 |
| `certificate:distribute` | 证书 | M1 |
| `certificate:read` | 证书 | M1, P2 |
| `certificate:write` | 证书 | M1, P2 |
| `cluster:bind` | 集群、成员、站点绑定 | M1 |
| `cluster:manage` | 集群、成员、站点绑定 | M1 |
| `cluster:read` | 集群、成员、站点绑定 | M1 |
| `cms:content:write` | 官网运营（CMS） | P2 |
| `cms:media:write` | 官网运营（CMS） | P2 |
| `cms:nav:write` | 官网运营（CMS） | P2 |
| `cms:page:write` | 官网运营（CMS） | P2 |
| `cms:plan-display:write` | 官网运营（CMS） | P2 |
| `cms:publish` | 官网运营（CMS） | P2 |
| `cms:read` | 官网运营（CMS） | P2 |
| `cms:seo:write` | 官网运营（CMS） | P2 |
| `cms:settings:write` | 官网运营（CMS） | P2 |
| `config:publish` | 配置修订、发布、回滚 | M1 |
| `config:read` | 配置修订、发布、回滚 | M1 |
| `config:rollback` | 配置修订、发布、回滚 | M1 |
| `credential:read` | 第三方凭据 | P2 |
| `credential:write` | 第三方凭据 | P2 |
| `deployment:read` | 部署记录 | M1 |
| `dns:manage` | DNS 与 Zone/记录 | P3 |
| `dns:read` | DNS 与 Zone/记录 | P3 |
| `dns:write` | DNS 与 Zone/记录 | P3, P6 |
| `domain:write` | 站点域名 | M1, P5 |
| `l4:read` | 四层代理 | P4 |
| `l4:write` | 四层代理 | P4 |
| `log:read` | 访问日志与安全日志查询 | M1, P4 |
| `login-log:read` | 登录日志 | M1 |
| `metrics:platform:read` | 指标/rollup/工作台 | M1 |
| `metrics:read` | 指标/rollup/工作台 | M1, P3, P4 |
| `node:enroll` | 节点管理、注册令牌、升级 | M1 |
| `node:manage` | 节点管理、注册令牌、升级 | M1, P2, P3 |
| `node:read` | 节点管理、注册令牌、升级 | M1, P2, P3 |
| `node:upgrade` | 节点管理、注册令牌、升级 | M1 |
| `notification:read` | 通知渠道/模板 | P2 |
| `notification:write` | 通知渠道/模板 | P2 |
| `origin:read` | 源站组/源站 | M1 |
| `origin:write` | 源站组/源站 | M1 |
| `plugin:manage` | 插件 | P5 |
| `plugin:read` | 插件 | P5 |
| `probe:manage` | 探测/Reporter | P3 |
| `probe:read` | 探测/Reporter | P3 |
| `role:read` | 角色与权限分配 | M1 |
| `role:write` | 角色与权限分配 | M1 |
| `rule:read` | 统一规则 | M1, P2 |
| `rule:write` | 统一规则 | M1 |
| `scheduling:read` | 调度策略 | P3 |
| `scheduling:write` | 调度策略 | P3 |
| `setting:read` | 系统设置（含品牌、运行模式） | M1 |
| `setting:write` | 系统设置（含品牌、运行模式） | M1 |
| `site:delete` | 站点基础、设置、网络（真实 IP/可信代理/出口 IP）、TLS | M1 |
| `site:read` | 站点基础、设置、网络（真实 IP/可信代理/出口 IP）、TLS | M1 |
| `site:tls:write` | 站点基础、设置、网络（真实 IP/可信代理/出口 IP）、TLS | M1 |
| `site:write` | 站点基础、设置、网络（真实 IP/可信代理/出口 IP）、TLS | M1 |
| `system:read` | 版本信息、密钥元数据、已安装组件 | M1 |
| `task:cancel` | 任务中心 | M1 |
| `task:read` | 任务中心 | M1 |
| `tenant:impersonate` | 平台主体经 `X-Tenant-ID` 代租户操作（acting-as）的前置权限；scope=platform；不出现在任何 operation 的 `x-permissions`，由 `tenantscope` 中间件在带 `X-Tenant-ID` 时与 operation 权限串一并判定（ADR-018 §10） | M1 |
| `tenant:manage` | 租户与成员、配额 | M1, P5 |
| `tenant:member:manage` | 租户与成员、配额 | M1 |
| `tenant:member:read` | 租户与成员、配额 | M1 |
| `tenant:read` | 租户与成员、配额 | M1 |
| `ticket:manage` | 工单 | P5 |
| `ticket:read` | 工单 | P5 |
| `ticket:write` | 工单 | P5 |
| `token:manage` | 自己名下的 API Token | M1 |
| `user:read` | 平台用户管理 | M1 |
| `user:write` | 平台用户管理 | M1 |
| `waf:read` | WAF/CC/IP 限制 | P4 |
| `waf:write` | WAF/CC/IP 限制 | P4 |

## 5. 错误码索引（草案；稳定后随代码生成）

`code` 为稳定的小写点分字符串，前端以 `message_key = errors.<code>` 映射 i18n；未知码回退「发生未知错误」+ Request ID。

| code | HTTP | 含义 |
|---|---|---|
| `auth.unauthenticated` | 401 | 未登录或会话/令牌失效 |
| `auth.forbidden` | 403 | 已登录但缺少权限 |
| `auth.invalid_credentials` | 401 | 邮箱或口令错误（不区分原因） |
| `auth.reauth_required` | 403 | L4 敏感操作要求近期再认证（先 `POST /auth/reauth`，窗口 `auth.reauth_window_s`） |
| `auth.totp_required` / `auth.totp_invalid` | 401 | 需要二次验证 / 验证码错误 |
| `auth.csrf_invalid` | 403 | 缺少或错误的 X-CSRF-Token |
| `auth.password_policy` | 400 | 口令不满足策略 |
| `auth.token_revoked` | 401 | API Token 已吊销 |
| `validation.failed` | 400 | 参数/请求体校验失败（`details[]` 逐字段） |
| `config.validation_failed` | 400 | 发布前配置/规则校验失败（含注入向量被拒、regex 受限、Header 名非法等） |
| `resource.not_found` | 404 | 不存在或不属于当前租户 |
| `resource.conflict` | 409 | 唯一性、状态不允许、被引用 |
| `resource.precondition_failed` | 412 | `If-Match` 版本不匹配 |
| `idempotency.key_reuse` | 422 | 同一幂等键用于不同请求体 |
| `rate_limit.exceeded` | 429 | 触发限流（含登录节流/临时锁定：统一 429 + `Retry-After`，按 identifier 计数不论账号是否存在，无 423） |
| `feature.disabled` | 403 | 运行模式或阶段未启用该功能 |
| `task.failed` | — | Task 终态错误（出现在 `Task.error.code`） |
| `task.not_cancelable` | 409 | 任务已终态 |
| `site.domain_conflict` | 409 | 域名已被其他活动站点占用 |
| `site.domain_invalid` | 400 | 域名格式非法 |
| `site.domain_unverified` | 409 | 商业模式下域名未通过所有权校验（F-1，P5） |
| `origin.address_blocked` | 400 | 回源地址未通过 SSRF 校验 |
| `rule.invalid` | 400 | 规则结构/字段/操作符不匹配 |
| `rule.regex_rejected` | 400 | 正则不在 RE2 子集或超出长度/复杂度限制 |
| `rule.type_unavailable` | 400 | 规则类型在当前阶段未交付 |
| `realip.header_invalid` | 400 | 自定义 Header 名语法非法 |
| `realip.header_reserved` | 400 | 命中保留名/保留前缀/与内置 Header 重复 |
| `realip.trusted_proxy_forbidden` | 400 | 可信代理范围被禁止；`details[].reason ∈ prefix_too_short\|loopback\|multicast\|reserved\|self`（0.0.0.0/0、::/0、短于 IPv4 /8 或 IPv6 /32、回环/多播/保留段、节点自身地址） |
| `rule.cache_key_dimension_conflict` | 400 | 缓存键封闭规则被违反：规则读取了未入缓存键的维度却参与回源/改写/回源头（跨用户缓存泄漏防线，ADR-005 §3.7）；作为 `config.validation_failed` 的 `details[].reason` 与 `RuleValidationIssue.code` 出现；处置：该维度入键，或动作声明 `cache_interaction=bypass` |
| `rule.regex_ambiguous` | 400 | 正则存在指数歧义（EDA），拒绝（ADR-005 §4） |
| `rule.interpolation_invalid` / `rule.redirect_target_invalid` / `rule.loop_detected` | 400 | 变量插值/重定向目标模板非法（须以 `/` 或字面量 `http(s)://主机/` 开头）/ 循环重定向 |
| `cache.dangerous_not_acknowledged` | 400 | 启用危险开关未提供 risk_ack |
| `cache.purge_limit_exceeded` | 400 | 刷新 URL 数量超限 |
| `cache.feature_unavailable` | 403 | 目录/通配符失效在 PoC 通过前不可用 |
| `cert.key_mismatch` | 400 | 私钥与证书不匹配 |
| `cert.chain_invalid` | 400 | 证书链无效或已过期 |
| `cert.in_use` | 409 | 证书仍被站点引用 |
| `enroll.token_invalid` | — | 注册令牌无效/已用/过期（统一，不区分原因；仅 gRPC `:9444` 与 Tools，gRPC `NOT_FOUND`/`FAILED_PRECONDITION`，Tools 退出码 5；REST 创建令牌时无此错误） |
| `node.cluster_conflict` | 409 | 入群/绑定导致同节点主机名或集群级参数冲突（F-3） |
| `node.not_online` | 409 | 目标节点不在线，无法执行调试/升级 |
| `deployment.not_pausable` / `deployment.not_resumable` / `deployment.not_retryable` | 409 | 部署当前状态不允许该操作（retry 仅限 paused/failed/partial_failed 且修订仍为集群最新） |
| `site.no_cluster_bound` | 409 | 站点尚未被平台绑定到任何集群，无法发布（租户无法自行处理，联系平台；`placement.default_cluster_uids`） |
| `origin.group_inconsistent` | 400 | 同一源站组内 `protocol/tls_verify/sni` 必须一致（ADR-004 §6） |
| `setup.token_invalid` | 401 | 安装向导令牌错误 |
| `setup.already_installed` | 410 | 已存在 install.lock，向导接口永久不可用（与 tools-design §13 一致） |
| `setup.insecure_transport` | 400 | 携带口令的向导接口在监听非回环地址且请求非 TLS 时拒绝（CC-11、ADR-026 §10） |
| `setup.database_not_empty` | 400 | 目标库不为空，向导不静默覆盖（FR-6） |
| `setup.precheck_failed` | 400 | MySQL/Redis 预检未通过（版本 ≥8.0.16/Redis ≥8、utf8mb4、sql_mode、binlog 触发器权限 ERROR 1419、账号权限），`details` 说明原因与处置（CC-12） |
| `setup.mode_active` | 503 | Server 处于 setup mode（未完成安装）：除 `/setup` 与 `/public/setup/*` 外所有接口 503（Story 1.24） |
| `tenant.quota_exceeded` | 403 | 租户配额超限 |
| `internal.error` | 500 | 内部错误（凭 request_id 排查） |

## 6. 生成与维护

`openapi.yaml` 是**手工维护的事实源**（改动用定点文本编辑，不要整文件重新序列化）。2026-11-16 修订后已验证：`openapi-spec-validator` 0.7.2 报 `OK`；Ruby `YAML` 可解析；所有 `$ref` 可解析；operationId 唯一；每个操作带 `x-permissions/x-tenant-scope/x-audit/x-phase`；路径段不含 `:`；**租户脱敏静态扫描通过**（规则：对 `x-tenant-scope ∈ {tenant, either}` 的 operation，递归展开请求参数/请求体/响应 schema（跟随 `$ref`），断言不出现属性名匹配 `^(node|cluster)_(uid|name|id|uids)$`、`node_*`、`cluster_*`、`egress_node*`、`targets` 的字段；该规则已由 `make openapi-check`（`scripts/contract`）落地，附负向自测）。实现阶段建议：`oapi-codegen` 生成服务端类型（实现时核对）、`openapi-typescript` 生成前端类型（实现时核对）。

## 7. API 枚举 → UX 固定词映射（唯一映射表）

> UX 固定词以 `docs/ux/EXPERIENCE.md`「配置状态表达」为准；本表是 **API 枚举到词的唯一映射**，前端不得自造。租户视图与平台视图分开；离线/维护单独计数（UX 铁律 3，如「1 失败 · 1 离线」）。

### 7.1 资源级发布状态 `PublishState`（`Site.publish_state`、`Rule.publish_state`、`RealIpSettings.publish_state` 等全 API 统一）

| API 值 | 含义 | UX 词 |
|---|---|---|
| `never_published` | 从未发布 | 已保存，未发布 |
| `unpublished_changes` | 工作态与已发布不同（`Site.config_dirty=true`） | 已保存，未发布 |
| `published` | 工作态与已发布一致 | 已发布（再叠加 7.2 的部署状态） |

保存期校验失败不是状态，而是请求结果：`400 config.validation_failed` → **校验失败**（字段错误映射回字段；校验统一 400，不用 422；乐观并发冲突为 412，不用 409）。

### 7.2 租户视图：`SiteDeploymentView.status`（`Site.deployment_status` 同取值）

| API 值 | UX 词 |
|---|---|
| `queued` | 排队中（同集群串行发布，ADR-002） |
| `running` | 发布中 · 已生效 `summary.healthy`/`summary.total`（离线/维护单独计数） |
| `success` | `summary.offline=0 且 maintenance=0` → 已全部生效；否则 已发布 · N/M（N=healthy，M=total，离线/维护单独列出） |
| `failed` | 发布失败（附 `failure_summary` 的阶段与错误码，不含节点信息） |
| `canceled` | 已取消 |
| `rolled_back` | 已回滚至 v`site_version_no` |

> `排队中`、`已取消` 为新增词，需 UX 在「配置状态表达」补录；平台侧的 `paused` 对租户折叠为 `running`。

### 7.3 平台视图：`Deployment.status` 与 `DeploymentTarget.state`（Admin）

| API 值 | 词 |
|---|---|
| Deployment `pending` / `running` / `paused` / `success` / `partial_failed` / `failed` / `canceled` / `rolled_back` | 待开始 / 进行中 / 已暂停（可恢复） / 全部生效 / 部分失败 / 失败 / 已取消 / 已回滚 |
| Target `pending`…`healthy`（`notified/downloaded/verified/tested/applied`） | 待通知 / 已通知 / 已下载 / 已验证 / 已测试 / 已应用 / 健康 |
| Target `failed` / `rolled_back` / `skipped` / `waiting_offline` / `expired` | 失败（`failure_stage`） / 已回退 / 已跳过（维护） / 等待离线节点 / 已过期（未在追平窗口内生效） |

### 7.4 其它

| API | UX 词 |
|---|---|
| `SiteConfigVersion.status`：`pending` / `live` / `superseded` / `failed` | 发布中 / 当前版本 / 历史版本 / 发布失败 |
| CMS 内容 `scheduled`（P2） | 已排期（与配置「已保存，未发布」区分，不再用「待发布」） |

## 8. 修订记录 2026-11-16

依据 `docs/bmad/review-architecture.md`（BLK/IMP/MIN）与 `review-architecture-internal.md`（H/M/L）修订本契约，详表见 `docs/bmad/architecture-revision-log.md`：

- **租户 API 脱敏（BLK-02/H3，ADR-009 §5 R-T1）**：`DeploymentCreate` 只剩 `config_version`/`change_note`；新增 `SiteDeploymentView`、聚合 `OriginHealth`、`EgressIpList`（仅 address/family）、`CertificateDeploymentSummary`、`CacheTaskDetail.summary`；`RealIpDebug*` 去 node/cluster；站点集群绑定改为平台独占 `/sites/{siteId}/cluster-bindings`；`SiteCreate`/`Site` 去 `cluster_uids`；`/deployments*` 改 platform；站点×节点 rollup、缓存任务逐节点回执、证书逐节点部署、节点视角源站健康为新增平台端点。原因：租户不得枚举/操控节点与集群拓扑（NFR-14）。
- **发布编排（BLK-05）**：新增 `retryDeployment`、`listClusterPublishQueue`；`Cluster.settings.rollout`（灰度策略平台独占）；`DeploymentTarget` 增 `uid/waiting_offline/expired`；`Deployment.summary` 增 `waiting_offline/skipped/expired`。
- **缓存键封闭规则（BLK-03）**：`RuleValidationIssue.code=rule.cache_key_dimension_conflict`、`RuleAction.cache_interaction`；`redirect.strip_prefix`、删除 `scope.path_prefix`；`RuleCreate` 不再接收 `phase`（RuleType 的 `x-rule-type-phases` 给出合法映射）；regex `on_limit`；变量/重定向目标约束写入描述。
- **其它**：`TaskType` 最终枚举（+`deployment.retry/publish.dispatch/node.rollback/realip.debug/secrets.rewrap/server.cert_rotate/media.cleanup_orphans/usage.derive`）；`NodeUpgradeRequest.component`；`NodeRollbackRequest` 改节点代语义；`PurgeSiteRequest`/`PurgeUrlsRequest`/`PrefetchRequest` 去 `cluster_uid`；`TrustedProxies` 站点级仅 XFF；`/audit-logs`（迁出 `/admin`）、`/auth/login-logs`、`/auth/reauth`、`x-reauth`；`/admin/settings/ssrf-allowlist`；`/rule-validations`、`/certificate-parses`（Gin 实测冒号路径语义歧义）；登录统一 429；setup 完成后 410；`SiteConfigVersion` 改站点线性版本（回滚路径 `/config-versions/{versionNo}/rollback`）；`PublishState` 统一词表；16 个无分页列表补分页；`LoginLog/TaskEvent/NodeEvent` 暴露 `uid`；`UsageRecord` 去 `uid`；CMS 预览链接改不透明令牌并补泛化 `POST /admin/cms/preview-links` 与撤销；新增 §7 枚举→UX 词映射。
