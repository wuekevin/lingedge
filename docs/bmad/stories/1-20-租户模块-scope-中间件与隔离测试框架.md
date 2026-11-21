---
title: 'Story 1.20：租户模块、Scope 中间件与隔离测试框架'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'dc751b2'
context:
  - docs/bmad/epics.md（Story 1.20）
  - docs/architecture/adr/ADR-009-tenant-isolation.md §4–§8、§11（尤其 §5 租户不可见节点/集群、R-T1）；ADR-018 §10；ADR-017
  - docs/api/openapi.yaml 的 /tenants*（含 /tenants/{tenantId}/members、/quotas）与全部 x-tenant-scope；docs/database/（tenants、tenant_members、tenant_quotas、placement 设置）
  - docs/bmad/review-architecture*.md（BLK-02 / IMP-12 / R-T1）；D-13（默认租户种子）
  - docs/bmad/stories/1-5-*（tenantscope.Scope 设计）与 1-12…1-19-*.md（约定与遗留）；internal/platform/{tenantscope,httpx,apierr}；internal/auth
---

## Intent

**Problem:** 1.18/1.19 之后「租户上下文」只完成了一半：授权钩子已能判定 acting-as，但 Scope 钩子是过渡实现（`WithAccessScope`），acting-as 一律 403——没有任何东西验证 `X-Tenant-ID` 指向的租户真实存在且可用；没有租户模块（`/tenants*` 无处读写）；没有隔离测试框架，后续每个租户资源 Story 没有统一防线；R-T1（租户响应不得含节点/集群内部标识）只有 OpenAPI 静态扫描，没有对真实响应的断言。

**Approach:** ① 完整 Scope 钩子 `httpx.WithTenantScope`（替换过渡钩子，不放行任何未验证的情形）+ 租户目录 `httpx.TenantResolver`；② 租户模块 `internal/tenant`：`/tenants*` 共 8 个 operation（经 `httpx.Router.Register`，元数据与 OpenAPI 一致，无 501 占位），配额只存取；③ 路由元数据检查 `make tenantscope-check`（真实路由表 + 探针钩子 + 负向样本）；④ `tests/security/tenant_isolation`：隔离矩阵（骨架由 openapi 自动生成 + 覆盖登记：新增租户资源未覆盖则失败），跑在真实临时 MySQL/Redis 的真实进程内装配上；⑤ R-T1 响应字段断言器 `internal/platform/leakguard`（通用助手，带正反样本）；⑥ `scopeMintPaths` 收窄。

## 核对结论：表结构与授权（无缺口，无新迁移）

- `tenants`（`000001`）：`uid/name/slug/status(active|suspended|closed|pending)/is_default/default_flag/owner_user_id/contact_email/locale/settings_json/suspended_reason/deleted_at` 已齐备；`tenant_members`（`status active|invited|removed`、`is_owner`、`invited_by`、`joined_at`）、`tenant_quotas`（`quota_key/limit_value NULL=不限/source manual|plan|default`）、`user_roles(tenant_id)`、`roles`（模板角色 `tenant_id IS NULL`）已齐备；`lingedge_app` 对这些表在 `000001` 逐表授予 `SELECT, INSERT, UPDATE, DELETE`，对 `users`、`system_settings`（读 placement）同样已授。
- **种子（D-13）已含**：`000004_seed_builtin.sql` 写入默认租户（`uid=01M7QP5V346STC0AT56NME5NR3`、`slug=default`、`is_default=1`、`status=active`）、6 个内置角色（含 `tenant_owner/site_operator/tenant_viewer` 与 `tenant:impersonate`、`tenant:read|manage`、`tenant:member:read|manage` 权限）。**不含** `placement.default_cluster_uids`——它由后续建站 Story 读取，缺省即「没有默认集群」，本 Story 不种它（测试里种入它来证明它不进入任何租户相关响应）。
- **冻结迁移 `000001`–`000018` 一字未改，无新迁移**；`make migrations-check` 通过。
- **契约（仅加法）**：CC-8（`docs/bmad/contract-changes/CC-8-tenant-current-error-responses.md`）：`getCurrentTenant` 补声明 `400`/`404`（实现后真实会出现，原契约只有 200/401/403/500，kin-openapi 对未声明状态码会失败）。`make openapi-breaking` 相对 `contract-v1` 无破坏性变更。
- **门禁变更（须评审）**：`scopeMintPaths` 由 `internal/platform/` 整体 + `internal/tenant/` + `internal/scheduler/` + `cmd/lingedge-(server|worker|scheduler)/` 收窄为 `^internal/platform/(httpx|taskrt|server)/`（见下「门禁变更」）。`make tenantscope-check` 为新增门禁。

## Code Map

- `internal/platform/httpx/scopehook.go` -- `WithTenantScope`（完整 Scope 钩子）、`TenantResolver`/`TenantRef`/`ErrTenantNotFound`（租户目录端口）、`ActingAs`/`ActingAsFrom`/`RecordActingAs`（acting-as 审计上下文）；`middleware.go` 的 Scope 中间件携带审计上下文；`access.go` 删除过渡的 `WithAccessScope`。
- `internal/tenant/`（`api.go` 根包契约；`internal/{domain,repository,service,handler}`）-- 租户模块：8 个路由、租户目录实现、配额字典、成员与角色授予。
- `internal/auth/internal/handler/{module.go,audit.go,tokens.go,authorizer.go}`、`internal/auth/api.go` -- `Hooks(HooksOptions)`（装配租户目录与其他模块的过渡审计登记）；过渡审计钩子带 acting-as 字段；`bearerAllowed` 放行其他模块的「无权限要求、只返回自身租户上下文」路由（`GET /tenants/current`）。
- `cmd/lingedge-server/{assembly.go,routes.go}` -- 装配租户模块、`registerRoutes` 增加租户路由；`scopecoverage_test.go`（`TestTenantScopeCoverage`）。
- `internal/platform/httpx/contracttest/{scopecoverage.go,scopecoverage_test.go,contracttest.go}` -- 路由元数据检查器（`CheckScopeCoverage`）、`NopIdempotencyStore`、`LoadFromBytes`；负向样本自测。
- `internal/platform/leakguard/` -- R-T1 响应字段断言器（`Scan`/`ForbiddenKey`/`ForbiddenKeysIn`）与正反样本。
- `internal/platform/taskrt/taskapi/taskapi.go` -- 租户视图剔除键名与断言器同一规则（`egress_node*`/`targets` 等，矩阵发现的 R-T1 缺口）。
- `tests/apistack/` -- 真实进程内装配的共用被测系统；`tests/security/tenant_isolation/` -- 隔离矩阵；`tests/integration/tenant/` -- 租户模块集成测试。
- `scripts/gates/spec.go`、`.golangci.yml`、`tests/gates/gates_selftest_test.go` -- 铸造白名单收窄与自测；`Makefile`（`tenantscope-check`，并入 `contract-check`；`security` 现含 G-2）。
- `docs/testing/tenant-isolation-matrix.md`（规则）、`docs/bmad/contract-changes/CC-8-*.md`、ADR-009/ADR-018 修订记录、`docs/database/tenant-site.md`。

## 关键实现点

- **AC1 Scope 的唯一来源与受控铸造**：`tenantscope.Scope` 字段全部未导出（Story 1.5），包外无法造非零 Scope；铸造入口 `FromPrincipal/FromTask/PlatformForJob/NewContext` 的调用白名单收窄到 `internal/platform/{httpx,taskrt,server}/`（租户模块、调度器模块、cmd 都不能铸造；自测见 `tests/gates`：新增 4 个新违规样本 + 合法样本按新白名单重写）。Scope 钩子的输入只有：已认证主体、路由元数据、`X-Tenant-ID`——后者只在授权钩子已判定 `ModeActingAs`（同时具备 `tenant:impersonate` 与路由权限，且权限 scope ∈ {tenant, both}）之后才被解析。没有授权决策、决策与主体/路由不一致（如平台主体 + `own_tenant`）、租户目录未装配，一律委托内层安全拒绝（403，handler 不执行）。
- **acting-as**：目标租户经租户目录解析（每次现读、不缓存，停用/删除下一个请求即生效）；**不存在、已软删除、状态非 `active`（suspended/closed/pending）、目录返回的 uid 与请求不符，一律 `404 resource.not_found`，响应与「他租户资源」逐字节一致**（矩阵 113 个 404 样本逐字节比较）；目录故障 500（fail-closed）；之后 `FromPrincipal(…, TargetTenant)` 再次核对 `tenant:impersonate`。**API Token**：同一条链——租户令牌的 Scope 是令牌所属租户（他租户 `X-Tenant-ID` 404）；平台令牌 acting-as 须 scopes 含 `tenant:impersonate` 与路由权限（否则 403），目标租户存在性校验与会话一致（1.19 遗留已补）。
- **审计（待 1.21，不假装已写）**：acting-as 成功时 Scope 钩子把 `httpx.ActingAs{ActorID, TenantID, TenantUID, OperationID}` 放进请求上下文（`httpx.ActingAsFrom`），1.21 直接消费即可写 `audit_logs.actor_user_id + tenant_id + acting_as`；本 Story 写结构化日志（Scope 钩子一条、过渡审计钩子对写操作一条，均带操作者与目标租户，`audit_logs_written=false`）；`audit_logs` 表保持为空（测试断言）。租户模块的 3 个写 operation（`updateTenant`/`addTenantMember`/`putTenantQuotas`，`x-audit: true`）经 `HooksOptions.AuditedOperation` 登记到过渡审计钩子（其余要求审计的 operation 仍被 `DenyAuditor` 拒绝）。 【Story 1.21 已收口：审计记录器 internal/platform/audit 在业务事务内写 audit_logs，过渡的结构化日志与 adminAuditor 已移除；见 1-21 说明】
- **AC2 平台路由与 404 语义**：`platform` 路由：租户 Console 会话 401（realm 隔离，ADR-018 §1，认证层）、租户令牌 403（`rbac.Decide`，NFR-14）、平台主体带 `X-Tenant-ID` 400；租户资源越权/不存在一律 404 且逐字节一致。
- **AC3 租户模块**：`GET /tenants`（分页、排序白名单 `created_at|name|slug`）、`GET /tenants/current`（tenant 路由：租户主体 = 自己的租户；平台经 `X-Tenant-ID` = 目标租户）、`GET /tenants/{id}`、`PATCH /tenants/{id}`（`name/contact_email/locale/settings`，至少一个字段；`settings` 整体替换、≤16 KiB、**拒绝含节点/集群内部标识的键**；`contact_email` 空串清除）、`GET|POST /tenants/{id}/members`（加入**已存在**的租户用户并授予租户内模板角色；已是有效成员 409；removed/invited 复用行置回 active；同事务递增 `users.permission_version`，授予立即生效；成员邮箱不存在与平台用户同形 400）、`GET|PUT /tenants/{id}/quotas`（字典 8 个键；未设置读作 `null/default`；`PUT` 全成功或全不写；**只存取，不强制**，响应无 `used`）。除 `/tenants/current` 外全是 platform 路由（OpenAPI 如此声明）。`createTenant`（P5）不实现、不在 M1 未实现清单。
- **AC4 路由元数据检查**：`make tenantscope-check`：真实路由集合（`registerRoutes`）装配探针钩子（Scope 钩子一律拒绝并计数），每条需认证的 tenant|either（顺带 platform）路由必须恰好经过一次 Scope 钩子且 handler 未执行；变异验证（临时删除路由链里的 `scopeMiddleware`）后该检查失败并逐条点名。检查器负向样本：旁路 mux 的路由、装配方自带宽松钩子、空路由集不得空转通过、装配失败。
- **AC5 隔离矩阵**：见 `docs/testing/tenant-isolation-matrix.md`。骨架由 openapi 的 `x-tenant-scope` 自动生成（9731 行：需认证 operation × 身份 × 目标租户情形；已注册的 46 条路由里 633 行被执行，288 行由 Exempt 引用的专项测试覆盖，416 个尚未实现的 operation 随各 Story 注册自动纳入）；身份：租户 A/B 会话、A/B 令牌、平台 acting-as（管理员/令牌）、无 impersonate 的平台会话与平台令牌、未认证。覆盖登记 `Registry`：新增 tenant|either 路由未登记 → 失败（正反样本自测）。Driven：`getCurrentTenant` 与任务中心 `listTasks/getTask/listTaskEvents`（真实 `tasks` 数据，A/B/平台各一批）；FailClosed：`cancelTask`（审计模块前 500，断言任务状态不变，1.21 交付后必须转 Driven）；Exempt：自助 operation 与令牌管理（各引用已有专项测试，门禁核对函数存在）。
- **AC6 R-T1 响应字段断言**：`internal/platform/leakguard`：键名（`node_*`、`cluster_*`、`egress_node*`、`targets`、`nodes/clusters`、驼峰）、已种入的内部取值（节点/集群 uid、名称、主机名、IP，子串大小写不敏感）、内部错误全文特征、IP 字面量（可选）；聚合计数 `summary.healthy/total/offline` 允许。矩阵种入真实的集群/节点行与 `placement.default_cluster_uids`，租户可见的成功响应（租户主体、令牌、平台 acting-as）一律断言；矩阵还证明断言器对真实数据有区分力（同一批任务：租户视角零违规、平台视角有违规）。**矩阵发现并修复一处缺口**：任务中心租户视图只剔除 `cluster*`/`node*` 前缀键，`egress_node*`、`targets` 未剔除——现同一规则（变异验证：撤销修复后矩阵立即失败）。集群放置由平台 `placement.default_cluster_uids` 决定，租户不选、不可见。

## 默认值与口径（[ASSUMPTION] 标注）

| 参数/口径 | 取值 | 说明 |
|---|---|---|
| acting-as 目标租户「可用」 | 仅 `status='active'` 且未软删除 | 任务要求「真实存在且状态可用」；suspended/closed/pending 与不存在同形 404。平台运维对 suspended 租户的管理走 platform 路由（`PATCH /tenants/{id}` 等，不经 acting-as）`[ASSUMPTION]` |
| 租户 Console 会话访问 platform 路由 | 401（realm 隔离），租户令牌 403 | 已有 1.16/1.18/1.19 行为，矩阵固化；对 AC2「路由层拒绝」的具体状态码 |
| `PATCH /tenants/{id}` 的 `settings` | 整体替换；≤16 KiB；拒绝内部标识键 | 工程取值；R-T1 写入侧防线 |
| `contact_email` 空串 | 清除（NULL） | 契约类型是 string，没有 null；工程取值 |
| 配额键字典 | `api_tokens, cache_tasks_per_day, certificates, domains_per_site, l4_applications, members, rules_per_site, sites` | 取自 database 文档列出的键；未写入读作 null/default；值 ∈ [0, 2^53] |
| 成员加入 | 仅已存在的租户用户；角色仅租户范围模板角色（≤20 个） | 租户用户账号创建（注册/邀请）属 P5 |
| 令牌调用 `GET /tenants/current` | 允许 | 无权限要求、只返回令牌所属租户（或 acting-as 目标）；本模块自助 operation 仍要求 Cookie 会话 |
| 任务中心的 R-T1 适用范围 | 租户可见响应 | 平台跨租户只读（不带 `X-Tenant-ID`）是平台视角，不属租户可见响应（1.14 既有设计）；写入文档 |

## Tasks & Acceptance

- [x] AC1 `internal/platform/httpx` 单测：`TestTenantScopeModes`（授权决策落成 Scope；无决策/决策与主体或路由不一致 → 委托内层安全拒绝）、`TestTenantScopeActingAsSuccess`（Scope + 审计上下文 + 日志）、`TestTenantScopeActingAsFailures`（不存在/suspended/closed/pending/uid 不符/id=0 → 404 逐字节一致；目录故障 500；缺/非法 `X-Tenant-ID` 400；目录未装配 403；非法 acting-as 决策不查目录）。矩阵 `TestActingAsCarriesAuditContext`（管理员/令牌 acting-as 的上下文含操作者与目标租户；非代租户无上下文；无 impersonate 403 且无「代租户」日志；写操作过渡审计日志同时带操作者与目标租户；`audit_logs` 为空）。
- [x] AC2 矩阵骨架行：platform 路由租户会话 401/令牌 403/平台带 `X-Tenant-ID` 400；tenant|either 路由租户主体/令牌他租户·不存在·suspended → 404；全部 404 响应体逐字节一致（113 个样本）。
- [x] AC3 `tests/integration/tenant`：`TestSeededDefaultTenantIsTheOperatingEntry`（种子默认租户、列表、acting-as 默认租户、配额无限制）、`TestTenantReadUpdateAndValidation`、`TestTenantPermissionsAreEnforced`、`TestTenantMembers`（加入/角色/冲突/校验/重新加入/权限版本递增/新成员端到端）、`TestTenantQuotasStoredNotEnforced`、`TestTenantWritesAuditIsPendingStory121`、`TestTenantResponsesCarryNoClusterOrNodeIdentity`；每个 operation 的响应经 kin-openapi 校验。`handler_test.go`：8 个路由与 OpenAPI 元数据逐项一致。
- [x] AC4 `make tenantscope-check`（`TestTenantScopeCoverage` + 检查器 4 个负向/正向样本）。
- [x] AC5 `tests/security/tenant_isolation`：`TestTenantIsolationMatrix`（coverage registry / skeleton rows / driven tenants/current / driven tasks / fail-closed cancelTask / driven coverage is complete / 404 逐字节一致 / G-5）、`TestRegisteredRoutesMatchServerBinary`、`selftest_test.go`（登记机制正反样本：未登记新资源失败、陈旧/缺理由/缺引用/引用不存在失败、完整通过；骨架由合成 OpenAPI 自动生成；骨架非空）。
- [x] AC6 `internal/platform/leakguard` 正反样本（18 种泄漏形态必须发现、7 种合法响应不得误报、键名表）；矩阵对租户可见响应自动断言；`taskapi` 单测补 `egress_node*`/`targets`。
- [x] 响应契约：8 个租户 operation + `getCurrentTenant` 的 200/400/404 + 任务中心 200/404 经 kin-openapi 校验。
- [x] Bearer 令牌租户边界（含 1.19 遗留的目标租户存在性校验）：矩阵 tokenA/tokenB/platformToken/platformTokenNoImp 全部情形；`TestBearerAllowedPolicy`。
- [x] G-5 日志脱敏：矩阵与 acting-as 测试结束前扫描日志，运行期生成的口令/令牌/会话 Cookie/CSRF 无一出现（扫描器自测：泄漏必须发现、干净日志不误报）；测试口令/令牌全部运行期生成，G-6 不触发、未加任何豁免。
- [x] 门禁：`make lint security gate-selftest race vuln contract-check build verify-build migrate-test` 全绿（见「开发记录」）。
- [x] `make openapi-check`：路由清单 38 → 46 条，「M1 未实现 operation 清单」162 → 154；`TestRoutesSubcommandExportsManifest` 同步（38 → 46）。
- [x] sprint-status、本说明、本地提交（不 push）。

## 门禁变更（须评审，逐条说明理由）

1. `scripts/gates/spec.go` `scopeMintPaths` 收窄（`make gen-lint` 重新生成 `.golangci.yml`）。**这是收紧而不是放宽**：此前白名单是过渡性的宽路径（1.5 注释已登记「应在 1.12/1.14/1.20 落地后收窄」）。
2. `tests/gates/gates_selftest_test.go`：因白名单收窄，3 个原「合法」样本随之调整——`internal/tenant/internal/handler/ok_mint.go`、`cmd/lingedge-server/ok_scope.go`、`internal/platform/task/ok_scope.go` 不再合法：前两者**移入违规样本**（租户模块、cmd 铸造 Scope 现在必须被拦截），第三个路径改为真实的 `internal/platform/taskrt/ok_scope.go`，并新增合法样本 `internal/platform/httpx/`、`internal/platform/server/`；另新增违规样本 `internal/platform/task/`、`internal/scheduler/`。**未删除任何原有违规样本，未放宽任何原有断言。**
3. 新增 `make tenantscope-check`（并入 `contract-check`）；`make security` 现含 `tests/security/...`（G-2 隔离矩阵），回显文案同步。
4. 没有放宽任何既有门禁自测。

## 等待环境验证（本机通过不代表这些已验证）

1. **MySQL 8.0.16（最低版本）与 Linux 验收机**重跑 `tests/security/tenant_isolation`、`tests/integration/tenant`、`tests/integration/auth`、`tests/integration/server`、`make migrate-test`：成员列表的 `JSON_ARRAYAGG` 相关子查询、`INSERT … ON DUPLICATE KEY UPDATE user_roles`（含虚拟生成列 `tenant_key` 的唯一键）、`IF(?, CAST(? AS JSON), settings_json)`、`SELECT … FOR UPDATE` 在 8.0.16 上的表现。
2. **多 Server 实例**：租户目录每次现读 MySQL（无缓存），acting-as 的「停用/删除下一个请求即生效」在多实例下天然成立，但本机只有单进程；加成员后权限缓存失效依赖 `permission_version`（键含版本），多实例下的 Pub/Sub 回收行为同 1.18 的待验证项。
3. **真实 Redis 部署形态**（Upstash/云 Redis）下令牌与权限缓存相关链路（矩阵里令牌认证与权限装载依赖 Redis 缓存/直读降级）。
4. 产品/安全确认的口径：acting-as 仅允许 `active` 租户；租户 Console 会话访问 platform 路由为 401（realm 隔离）而非 403；令牌可调用 `GET /tenants/current`；`settings` 拒绝内部标识键。

## 遗留与风险

- **待 1.21 写 audit_logs**：`updateTenant`/`addTenantMember`/`putTenantQuotas` 与 acting-as 请求目前只有结构化日志（`audit_logs_written=false`）；1.21 须消费 `httpx.ActingAsFrom` 同事务写 `actor_user_id + tenant_id + acting_as`，并替换过渡审计钩子；`cancelTask` 须转为矩阵的 `Driven`（当前断言它被审计钩子拒绝，1.21 交付后该断言会失败，迫使转换）。 【Story 1.21 已收口：审计记录器 internal/platform/audit 在业务事务内写 audit_logs，过渡的结构化日志与 adminAuditor 已移除；见 1-21 说明】
- **createTenant（P5）与租户用户账号创建未实现**：`addTenantMember` 只能加入已存在的租户用户；M1 的租户用户由安装向导/运维经库写入。
- **配额不强制、无 `used`**：强制与用量统计在 Story 6.9；`members`/`api_tokens` 等配额目前对 `addTenantMember`/令牌创建无任何约束（有测试断言「不强制」，避免假装强制）。
- **`placement.default_cluster_uids` 未种、未被任何代码读取**：由后续建站 Story 使用；本 Story 只保证租户可见响应不含集群标识。
- **任务中心的租户视图只做键名剔除**：`error.message` 等自由文本里若出现节点主机名/IP（由各任务处理函数写入）不会被剔除——属于各任务类型 Story 的写入纪律；矩阵的断言器对已种入的内部取值做子串扫描，新增任务类型的 Story 应在其驱动里种入对应反例。
- **每个租户资源 Story 必须扩充矩阵**（`docs/testing/tenant-isolation-matrix.md` §2）：新增已注册的 tenant|either 路由未登记，矩阵即失败——这是有意的，也是 DoD 的一部分。
- **Registry 的 Exempt 是人工登记**：理由与引用的专项测试需评审把关（门禁只核对引用函数存在）。
- `tests/apistack` 的装配是 `cmd/lingedge-server` 装配的镜像：漂移由 `TestRegisteredRoutesMatchServerBinary`（路由集合）与 `TestTenantScopeCoverage`（真实路由表的 Scope 覆盖）兜底，但钩子装配本身（`Hooks` 选项）仍是两处各写一次。

## 开发记录

### 设计取舍

- **租户目录是 httpx 里的端口，不是 `platform/ports` 的新接口**：Scope 钩子必须在 `internal/platform/httpx`（铸造白名单所在），租户模块在 L1 业务层，platform 不得依赖业务模块；`ports` 清单变更要先修订 ADR-001。所以接口定义在 httpx，租户模块实现，`cmd` 装配时经 `auth.HooksOptions.Tenants` 注入。未装配时 acting-as 保持安全拒绝（fail-closed），不是放行。
- **目录每次现读、不缓存**：租户被停用/删除后下一个请求即生效；缓存会引入「停用后仍可代操作」的窗口，且 acting-as 是低频运维入口，一次主键/唯一键查询的代价可忽略。
- **404 的同形**：不存在/已删除/状态不可用/目录 uid 不符，全部走 `tenantscope.ErrTenantMismatch → 404`；矩阵把所有「不泄漏存在性」的 404 响应体收集起来逐字节比较，而不是只断言状态码。
- **覆盖登记 + 驱动登记身份组**：只在登记表里声明 `Driven` 不够——驱动必须真的用四类身份组调用过该 operation（`matrix.req` 自动记录），否则测试失败；`Exempt` 必须引用真实存在的专项测试函数。这样「声明覆盖」与「实际覆盖」不会分叉。
- **探针钩子 + 真实路由表做 Scope 覆盖检查**：路由链由 `Router.Register` 固定，一般不会漏掉 Scope 中间件；但「链被改坏」「旁路 mux」「自带宽松钩子」都是真实风险。对真实路由表发请求并断言探针被调用，比静态 AST 检查更直接，且变异验证有效。
- **平台跨租户只读不适用 R-T1**：任务中心对平台主体有意展示集群/节点标识（运维需要）；R-T1 的约束对象是租户可见响应。规则写进 `docs/testing/tenant-isolation-matrix.md`，矩阵用它证明断言器有区分力。

### 实际验证（本机）

- 真实 MySQL（自拉起临时 mysqld，应用全部内嵌迁移 000001–000018、真实 `lingedge_app` 账号）+ 真实 redis-server 临时实例 + 真实 httpx 钩子链；无 Mock。变异验证（均已还原）：删除路由链的 `scopeMiddleware` → `tenantscope-check` 逐条点名失败；去掉 acting-as 的状态检查 → 矩阵 suspended 相关行失败；撤销任务视图的 R-T1 修复 → 矩阵在 `listTasks/getTask/listTaskEvents` 报告 `egress_node_ip`/`targets` 泄漏。
- 本机常驻 `mysqld(576/744)` 与 redis 未被触碰；未使用 Docker；未 push；未改 git config。

### 门禁结果

全部在本机（macOS arm64，Go 1.27.4）实际执行并通过（退出码 0）：`gofmt -l .` 无输出；`make lint`（golangci-lint 0 issues，含 arch/shell/repo-scope）；`make security`（G-2 隔离矩阵、G-5/G-6/G-11/G-12/tasktype/品牌）；`make gate-selftest`（含铸造白名单收窄后的正反自测）；`make race`（`CGO_ENABLED=1 go test -race -timeout 30m ./...`，全部包 ok，无 DATA RACE；`tests/integration/auth` 约 650 秒、`tests/security/tenant_isolation` 约 48 秒、`tests/integration/tenant` 约 73 秒）；`make vuln`（No vulnerabilities found）；`make contract-check`（openapi-check：路由清单 46 条、M1 未实现清单 154；openapi-breaking 无破坏；migrations-check 18 个哈希一致；freeze-check；perms-check；tenantscope-check）；`make build`、`make verify-build`；`make migrate-test`。`make race` 在最后一次 Go 源码改动之后完整运行（其后只改了本说明与 sprint-status 等文档），文档定稿后其余门禁（lint security gate-selftest vuln contract-check build verify-build migrate-test）再完整重跑一遍，全部通过。

开发中暴露并已修复的问题（均由新增测试发现）：过渡 Scope 钩子替换时一度过严地拒绝了令牌管理（平台用户管理自己的平台令牌，授权决策 `ModePlatform` 落在 either 路由）；成员视图的角色列表对 JSON `null` 输出 `null` 而非 `[]`；任务中心租户视图对 `egress_node*`/`targets` 的剔除缺口（见 AC6）；令牌调用 `/tenants/current` 被 `bearerAllowed` 拒绝（过严，见上）；`contracttest` 非测试代码误引 `net/http/httptest`（被 depguard 拦截，改为最小 recorder）。
