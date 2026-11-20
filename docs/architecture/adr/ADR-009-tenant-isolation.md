# ADR-009 多租户隔离强制点（Repository 层 tenant scope）

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §1.3、§3.2、§18、§24；PRD FR-19、NFR-4、NFR-8、D-13；architecture AD-9｜阶段：M1（数据模型与强制从 MVP 起）

## 背景

基线 §18/§24：**租户隔离必须由服务端强制执行**，普通租户只能操作已授权资源；§1.3 禁止绕过租户隔离。PRD D-13：MVP 虽只有 Admin 入口，数据模型从第一天起多租户，Admin 以管理员身份"代租户操作"。隔离若依赖每个 Handler 记得加 `WHERE tenant_id=?`，必然在某处遗漏。

## 决策

1. **强制点放在 Repository 层**，Handler/Service 无法绕过：所有租户范围表的 Repository 方法**第一个业务参数是 `scope.Scope`**。
   ```go
   type Scope struct {
       TenantID   uint64 // 非零：限定到该租户
       Platform   bool   // true：平台范围（跨租户读/写），仅平台主体且带 platform 权限才可构造
       ActorID    uint64
       ActingAs   bool   // Admin 代租户操作
   }
   ```
   `Scope` 只能由 `platform/tenantscope` 的构造函数从**已认证主体 + 路由元数据**生成（`FromPrincipal(p, meta, tenantHeader)`），不能由 Handler 直接填充 TenantID。
2. **查询构建器**：Repository 使用 `platform/db/tenantdb` 提供的构建器，统一追加 `tenant_id = ?`（`Scope.TenantID`）；`Platform=true` 的方法必须使用名字带 `AcrossTenants` 的专用方法（便于审计与 grep），且 Service 层需校验权限串。**所有 INSERT 由构建器强制写入 `scope.TenantID`**，忽略请求体里的 `tenant_id`。
3. **跨租户引用的校验**：写入涉及外键引用（如把 `origin_group_id` 绑到站点）时，Repository 以 `(id, tenant_id)` 复合条件校验被引用对象属于同一租户（数据库层复合外键 `(tenant_id, site_id)` 与 `origins(site_id, origin_group_id)`，database README §2.4/§6，已在 MySQL 8.0.46 实测跨租户写入被 ERROR 1452 拒绝）。
4. **越权表现为 `404 resource.not_found`**，不区分"不存在"与"无权"（防枚举，ADR-017）；对租户主体不返回 `403` 暴露存在性。
5. **主体与上下文**：Console 主体取其租户（会话绑定）；Admin 主体访问租户资源必须带 `X-Tenant-ID`（租户 uid），中间件校验管理员**同时具备 `tenant:impersonate`（scope=platform）与该路由 `x-permissions` 全部权限**（权限 scope ∈ {tenant, both}），设置 `ActingAs=true`；**审计同时记录操作者与目标租户**（`audit_logs.actor_user_id` + `audit_logs.tenant_id` + `acting_as`）。`x-tenant-scope: either` 的授权语义：租户主体 → 限定自己租户；平台主体不带 `X-Tenant-ID` → 跨租户只读（须具备路由权限串）；平台主体带 `X-Tenant-ID` → 代租户（需 `tenant:impersonate`）。平台级资源（nodes、clusters、system、全局用户视图、站点↔集群绑定、部署详情）声明 `x-tenant-scope: platform`，租户主体直接 404/403（路由层拒绝）。

   **规则 R-T1（租户响应脱敏，BLK-02/H3）**：**`x-tenant-scope ∈ {tenant, either}` 的请求/响应 schema 与实际响应不得含 node/cluster 的内部标识**——属性名匹配 `^(node|cluster)_(uid|name|id|uids)$`、`node_*`、`egress_node*`、`targets`，以及任何节点主机名/IP/内部错误全文；**聚合计数**（如部署 `summary.healthy/total/offline`）允许。租户永不能：枚举节点、选择/排除节点或集群、指定灰度策略/批次、把任务投递到指定节点（NFR-14、architecture §3 边界规则 3）。落实：
   - 租户发布 `DeploymentCreate = {config_version, change_note}`；灰度由平台策略（`clusters.settings_json.rollout`）决定；租户视图为脱敏的 `SiteDeploymentView`（健康 N/M + 失败阶段/原因码摘要，paused 显示为 running）；
   - 源站健康为每源站聚合 `OriginHealthView`；出口 IP 清单为 `EgressIpList{address,family}`；真实 IP 调试不接受 `node_uid`（服务端按站点所属集群选一个在线节点）；
   - 站点↔集群绑定（`/sites/{siteId}/cluster-bindings`）为**平台独占**；租户建站不选集群（`placement.default_cluster_uids` 默认绑定；未绑定时发布 `409 site.no_cluster_bound`）；**不提供租户可见的 `GET /available-clusters`**（与“租户不选集群”矛盾）；
   - CI：对 `openapi.yaml` 静态递归检查 R-T1（跟随 `$ref`）；隔离矩阵测试再对**真实响应字段**断言（NFR-8）。
6. **静态防线**：自定义 `go vet` 风格分析器（或 `depguard` + `forbidigo`，实现时核对）：禁止在 `internal/*/repository` 之外引用 `*sqlx.DB`/`*sqlx.Tx`；禁止 Repository 方法出现缺少 `Scope` 首参的租户表访问（表清单来自 `docs/database` 元数据生成）。
7. **路由元数据检查**（ADR-017）：每个路由声明 `TenantScope`；`tenant|either` 的路由必须经 `tenantscope` 中间件；缺失则 CI 失败（NFR-4）。
8. **后台作业与任务**：Worker/Scheduler 任务运行时以任务记录的 `tenant_id` 构造 `Scope`；确需跨租户批处理（保留清理、rollup 聚合）使用平台范围专用 Repository 方法，且代码路径在清单中登记。
9. **gRPC（节点）侧**：拦截器以证书主体授权，资源必须属于节点所在集群；节点不属于租户，Server 在内部以平台 `Scope` 访问，但**对外返回的内容按集群/站点范围裁剪**（ADR-003）。
10. **缓存/日志/对象存储**：访问日志文件路径含 `site_uid`（入库强校验，ADR-008 §4），`LogQuery` 强制校验站点属于 `Scope`；媒体/备份等文件接口同理；Redis 租户相关键遵循 ADR-025 §4 的统一命名 `le:<env>:<模块>:<用途>:t<tenant_uid>:<标识>`（MIN-06，不再使用独立的 `t:<tenant_uid>:` 前缀）。
11. **隔离测试矩阵**（NFR-8）：`tests/security/tenant_isolation`：资源（站点、域名、源站、规则、证书/私钥、缓存任务、日志、Token、账单、工单、插件配置…）× 操作（list/get/create/update/delete/sub-resource）× 身份（租户 A、租户 B、平台管理员 acting-as、API Token）。矩阵由 `openapi.yaml` 的 `x-tenant-scope` 自动生成骨架，新增资源未覆盖则失败。

## Binds / Prevents / Rule

- 约束：所有 Repository、`tenantscope` 中间件、Admin 代租户流程、日志/文件/Redis 访问、CI。
- 防止：个别接口遗漏租户条件；Handler 伪造 `tenant_id`；管理员"代租户"操作无审计；通过关联对象（如把他租户源站绑到自己站点）横向越权。
- 规则（AD-9）：**租户隔离在 Repository 层强制：每个租户表查询经 `Scope` 注入 `tenant_id`；越权对外表现为 404；Repository 外禁止直接使用 `*sqlx.DB`。**

## 备选方案与权衡

- **Handler/Service 手写 `tenant_id` 条件**：否决——人为遗漏不可避免。
- **仅依赖 MySQL Row-Level 机制**：MySQL 无原生 RLS；用视图/存储过程加租户上下文会引入连接级状态与复杂度，否决（保留复合外键作纵深防御）。
- **每租户一个数据库/Schema**：否决——运维与迁移成本高，与"单库模块化单体"不符；自用模式只有一个租户。
- **ORM 全局作用域（GORM Scope）**：栈固定为 sqlx，不引入 ORM。

## 后果

- Repository 方法签名更冗长；换来可被机器检查的纪律。
- 平台级跨租户查询需显式 `AcrossTenants` 方法，管理员全局视图开发稍多样板。
- 复合外键增加 DDL 复杂度与迁移成本（可按表选择启用）。

## 可被推翻处

若采用 ORM 或不同 DB 访问层，强制点须迁移但原则不变；复合外键是否启用按表评估。P5 商业模式如需租户可选“线路/区域套餐”，应以**不暴露集群身份**的 `placement_profile` 抽象另议（不得退回直接暴露集群/节点，R-T1）。

## 验证

- 隔离矩阵全绿（任一越权用例失败即阻断发布，SM-3）。
- 静态规则：Repository 外使用 `*sqlx.DB` 失败；租户表方法缺 `Scope` 失败。
- 审计测试：Admin 代租户写操作的审计含操作者与目标租户；无 `tenant:impersonate` 的平台角色带 `X-Tenant-ID` 访问被拒。
- R-T1：OpenAPI 静态检查（tenant|either 响应无 node/cluster 标识，27 处历史泄漏点已清零）与真实响应断言。

## 修订记录 2026-11-16

- §5（BLK-02、H3、IMP-12、M18）：新增**规则 R-T1**（租户响应不得含 node/cluster 内部标识，聚合计数除外）及落实清单（脱敏的 `SiteDeploymentView`/`OriginHealthView`/`EgressIpList`、去掉灰度与节点参数、集群绑定平台独占、不提供 `available-clusters`）；明确 `tenant:impersonate` 与 `either` 的授权语义。原因：评审发现租户可枚举节点、指定灰度节点与策略、读取节点内部错误，违反 NFR-14。
- §3（MIN-13）：复合外键扩展到 `origins`（已实测）。
- §10（MIN-06）：Redis 键命名统一到 ADR-025 §4。

## 修订记录 2026-11-17（Story 1.5 实现备注）

- §1/§2/§6 的落地形态：`Scope` 与构造入口在 `internal/platform/tenantscope`（字段未导出，包外无法构造非零 Scope；`internal/platform/scope` 仅为类型别名），查询构建器在 `internal/platform/db/tenantdb`，SQL 文本只接受编译期常量（`db.Q`）或构建器产物；静态防线为 depguard（sqlx/database/sql/驱动仅限 `internal/platform/db` 与 repository 包）、forbidigo（Repository 内禁 SQL 拼接；受限池与 Scope 构造入口的调用白名单，来源 `scripts/gates/spec.go`）与 `make repo-check`（Repository 导出方法/接口方法的 Scope 首参；平台级表须登记在 `scripts/gates/data/platform-tables.txt`）。
- 「租户表清单来自 docs/database 元数据生成」未采用：文档对 T / T? / 平台级的标注是叙述性的，无法可靠解析；改为「默认全部要求 Scope、平台级表显式登记」的反向清单（漏登记只会多报）。
- 白名单变更须先改本 ADR 与 ADR-016，再改 `spec.go` 并 `make gen-lint`。

## 修订记录 2026-11-21（Story 1.20，实现细化，非推翻）

- §1/§5（Scope 钩子）：完整的租户 Scope 钩子 `httpx.WithTenantScope`（`internal/platform/httpx/scopehook.go`）替换 Story 1.18 的过渡钩子。Scope 只由三样东西生成：已认证主体、路由元数据 `x-tenant-scope`、`X-Tenant-ID`——且 `X-Tenant-ID` 只在授权钩子（RBAC，`rbac.Decide`）已判定「可代该租户操作」（同时具备 `tenant:impersonate` 与路由权限）之后才被解析；没有授权决策、决策与主体/路由不一致、租户目录未装配，一律安全拒绝。acting-as 的目标租户经**租户目录**（`httpx.TenantResolver`，租户模块 `internal/tenant` 实现，每次现读不缓存）解析，**不存在、已软删除、状态非 `active`（suspended/closed/pending）一律 `404 resource.not_found`**，响应与「他租户资源」逐字节一致；目录故障按 500 处理（fail-closed）。
- §5（审计）：acting-as 成功时 Scope 钩子把 `httpx.ActingAs{ActorID, TenantID, TenantUID, OperationID}` 放进请求上下文（`httpx.ActingAsFrom`），审计模块（Story 1.21）直接消费即可写 `audit_logs.actor_user_id + tenant_id + acting_as`。**audit_logs 仍未实现（待 Story 1.21）**：Story 1.20 只写结构化日志（Scope 钩子与过渡审计钩子都带操作者与目标租户，`audit_logs_written=false`），不假装已写。
- §4（越权语义）：租户 Console 会话访问 `platform` 路由 → 401（realm 隔离，ADR-018 §1）；租户 API Token 访问 `platform` 路由 → 403（`rbac.Decide`，NFR-14）；二者都在路由层（认证/授权钩子）拒绝，不进入 handler。租户主体指定他租户（含不存在、不可用）的 `X-Tenant-ID` → 404（先于权限判定，不泄漏存在性）。
- §7（路由元数据检查）：`make tenantscope-check`——真实路由集合里每条 `tenant | either` 路由必须经 Scope 钩子（探针钩子 + 真实路由表，负向样本自测），见 `docs/testing/tenant-isolation-matrix.md` §4。
- §8（铸造白名单收窄）：`tenantscope.FromPrincipal/FromTask/PlatformForJob/NewContext` 的调用白名单由 `internal/platform/` 整体 + `internal/tenant/` + `internal/scheduler/` + `cmd/*` 收窄为 `internal/platform/{httpx,taskrt,server}/`（`scripts/gates/spec.go`，`make gen-lint`）；租户模块、调度器模块、cmd 都不构造 Scope。
- §11（隔离矩阵）：`tests/security/tenant_isolation` 已落地（骨架由 openapi 自动生成；覆盖登记；R-T1 响应字段断言器 `internal/platform/leakguard`），规则与扩充方法见 `docs/testing/tenant-isolation-matrix.md`。矩阵暴露并修复一处 R-T1 缺口：任务中心的租户视图原先只剔除 `cluster*`/`node*` 前缀的键，`egress_node*`、`targets` 等未剔除，现与断言器同一规则。
- 租户模块：`/tenants`、`/tenants/current`、`/tenants/{id}`、`/tenants/{id}/members`、`/tenants/{id}/quotas`（配额只存取，强制执行属 Story 6.9）；种子「默认租户」（`000004`）是 MVP 运营入口（D-13）。`createTenant`（POST /tenants）是 P5，不在 M1 实现。
