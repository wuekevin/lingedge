# 多租户隔离矩阵（G-2）：规则与扩充方法

> 来源：Story 1.20、ADR-009 §11、NFR-8、BLK-02 / IMP-12 / R-T1。代码在 `tests/security/tenant_isolation/`（测试）与 `tests/apistack/`（真实进程内装配的共用被测系统）。
> `make security` 运行它（`go test ./tests/security/...`）；`make race` / `make test` 同样运行。缺 MySQL/Redis 时测试以「等待环境验证」跳过——**跳过不代表通过**（`LINGEDGE_REQUIRE_MYSQL=1` / `LINGEDGE_REQUIRE_REDIS=1` 把缺失视为失败）。

## 1. 矩阵是什么

资源 × 操作 × 身份：

| 身份 | 说明 |
|---|---|
| 租户 A / 租户 B | Console 会话（`tenant_owner`），真实 `/auth/login` 产生 |
| 租户 A / B 的 API Token | 真实 `/auth/api-tokens` 创建，经 Bearer 进入同一 Scope 生成（令牌所属租户即 Scope） |
| 平台管理员 acting-as | `platform_admin`（含 `tenant:impersonate`）会话 + `X-Tenant-ID` |
| 平台令牌 acting-as | 带 `tenant:impersonate` 的平台令牌 + `X-Tenant-ID` |
| 平台审计员 | 只读、**无** `tenant:impersonate`：不得代租户操作 |
| 未认证 | 一律 401 |

目标租户情形（`X-Tenant-ID`）：不带 / 自己的租户 / 他租户 / 不存在的 uid / 存在但状态不可用（suspended，另有 closed、pending）。

不手写 operation 清单：**骨架由 `docs/api/openapi.yaml` 的 `x-tenant-scope` 自动生成**（`skeleton_test.go` 的 `Skeleton`）。期望值只取决于「路由类型 × 身份 × 情形」的授权语义（ADR-009 §4/§5、ADR-018 §10）：

- `platform` 路由：租户 Console 会话 401（realm 隔离，ADR-018 §1），租户令牌 403（NFR-14）；平台主体带 `X-Tenant-ID` 400（平台级资源不接受目标租户）。
- `tenant | either` 路由：租户主体/令牌指定他租户、不存在、不可用的租户一律 **404 `resource.not_found`，且响应体逐字节一致**（去掉 `request_id`）；平台主体无 `tenant:impersonate` 403；acting-as 的目标租户不存在或状态不可用 → 同一个 404；`tenant` 路由平台主体缺 `X-Tenant-ID` 400。

已注册（`httpx.Register`）的 operation 才会被执行；尚未实现的 operation 只被统计，待各自 Story 注册后**自动纳入**。

## 2. 资源的功能性隔离：覆盖登记（新增租户资源必须扩充）

骨架只覆盖授权层。每个**已注册**且 `x-tenant-scope` 为 `tenant | either` 的路由，必须在 `coverage_test.go` 的 `Registry` 里登记它的覆盖方式，否则 `TestTenantIsolationMatrix/coverage_registry` 失败：

| 方式 | 含义 | 要求 |
|---|---|---|
| `Driven` | 矩阵里有该资源的驱动（`matrix_test.go` 的 `drive*`） | 驱动必须对 **{租户 A、租户 B、API Token、平台 acting-as}** 四类身份都发过请求（驱动自己登记 (operation, 身份组)，缺任何一类即失败）；断言：列表只含本租户、他租户 id → 404 且与「不存在」逐字节一致、响应字段 R-T1、kin-openapi 响应契约 |
| `FailClosed` | 依赖能力交付前，路由被钩子安全拒绝 | 断言任何身份调用都被拒绝且不改数据；依赖能力交付后必须转为 `Driven`（「被拒绝」的断言会失败，迫使转换）。**Story 1.21 已把唯一的 FailClosed 登记 `cancelTask`（依赖审计模块）转为 `Driven`**，该类型保留给之后依赖尚未交付能力的路由 |
| `Exempt` | 不是按租户列举/寻址的资源（自助 operation、令牌管理） | 写明理由；至少引用一个**已存在**的专项测试函数（`Refs`，门禁核对函数真实存在） |

反向同样检查：登记项必须对应一个已注册的 `tenant | either` 路由（陈旧/误登记失败）；`platform` 路由不需要登记（由骨架行统一覆盖）。登记机制自身的正反样本在 `selftest_test.go`（未登记 → 失败、缺理由/缺引用/引用不存在 → 失败、完整 → 通过、骨架由合成 OpenAPI 生成）。

### 每个租户资源 Story 的 DoD（本规则是 DoD 的一部分）

1. 在 `Registry` 登记新路由（通常 `Driven`）。
2. 在 `matrix_test.go` 增加 `drive<资源>`：用 `fixture` 里的租户 A/B、令牌、平台 acting-as 身份；种入两个租户（及平台）各自的数据；对 list / get / sub-resource / 写操作断言上述四点；请求经 `matrix.req`（自动登记身份组，并对租户可见的成功响应自动做 R-T1 断言）；响应契约用 `matrix.contract`。写操作要验证「他租户 id → 404 且数据不变」。
3. 租户数据里若含节点/集群内部标识，**种入真实的反例**（`apistack.World.SeedInternals`：集群/节点的 uid、名称、主机名、IP；`placement.default_cluster_uids` 平台设置），证明租户视图剔除、平台视图保留。
4. `make security` 与 `make race` 通过。

### 已登记的 Driven 资源（随各 Story 追加）

| 资源 | operation | Story | 驱动要点 |
|---|---|---|---|
| tenants/current | `getCurrentTenant` | 1.20 | 自己的租户 / acting-as 目标租户；不可用租户 404 |
| tasks | `listTasks`、`getTask`、`listTaskEvents`、`cancelTask` | 1.14 / 1.20 / **1.21** | 列表只含本租户；他租户/平台/不存在 404 逐字节一致；`cancelTask`（取消 + 审计同事务）：自己的 pending 任务 200 并恰好写一条审计（操作者、所属租户、`acting_as`、令牌）、越权 404 不写审计、终态 409 不写审计 |
| audit-logs | `listAuditLogs`、`getAuditLog` | **1.21** | A/B/平台级各一批审计行：列表只含本租户；平台 acting-as 与租户视角一致；`acting_as` 记录抹去平台操作者身份；`changes` 里内部标识键被剔除、聚合计数保留；他租户/平台级/不存在 404 逐字节一致；租户用 `tenant_uid` 过滤他租户得空集；平台不带 `X-Tenant-ID` 跨租户、保留管理员标签（断言器有区分力） |

**Story 1.22 说明（系统设置、运行模式、版本、安装记录、公开信息）**：新增的 12 个 operation 都是 `platform`（`/admin/settings*`、`/admin/system/run-mode|version`、`/admin/installations`）或 `public`（`/public/site-info|version`）路由——按本节规则 `platform` 路由不需要登记（由骨架行统一覆盖：租户 Console 会话 401、租户令牌 403、平台主体带 `X-Tenant-ID` 400，骨架对所有已注册 operation 自动执行），`public` 路由不属于租户隔离范围；它们没有租户可寻址的资源。`GET /auth/me`（Exempt）新增的 `features` 对租户可见，其 R-T1 扫描（模块键/能力键为通用词）与「`modules_delivered` 与真实路由一致」由 `tests/integration/sysconfig` 的 `TestMeFeaturesAndCapabilityRegistry` 断言，已加入 `getMe` 的 `Refs`。

**Story 1.23 说明（签名/CA 密钥元数据）**：新增的 `listSigningKeys`（`GET /admin/system/signing-keys`）是 `platform` 路由，按本节规则无需登记 Registry，由骨架行统一覆盖（租户 Console 会话 401、租户令牌 403、平台主体带 `X-Tenant-ID` 400）；`tests/integration/signingkeys` 的 `TestListSigningKeysAPI` 另外显式断言了租户会话 401、租户令牌 403（含带 `X-Tenant-ID`）、无 `system:read` 的平台用户 403、未登录 401 与「只读（无写方法）」，并断言响应只含公钥/指纹白名单字段、不含任何私钥材料。

`ipAllowedOps`（`matrix_test.go`）：审计记录的 `ip` 是租户自己用户的来源地址（AuditLog schema 字段），因此这两个 operation 的响应不做「任何 IP 字面量」检查；节点/出口 IP 仍由内部取值表逐一检查。

## 3. 响应字段断言（R-T1 / BLK-02）

`internal/platform/leakguard`（通用助手，业务 Story 可直接复用，带正反样本自测 `leakguard_test.go`）：

- 键名：`node_*`、`cluster_*`、`egress_node*`、`targets`、`nodes/clusters` 列表、驼峰形态（与 `scripts/contract` 的 OpenAPI 静态扫描同源并更严）；
- 已登记的内部取值（子串、大小写不敏感）：节点主机名/IP/uid、集群名称等；
- 内部错误全文特征（SQL/驱动错误、goroutine 栈、源码路径、网络层错误）；
- `ForbidIPValues`：任何 IP 字面量（租户/成员/配额/任务等不应出现地址的资源）。

聚合计数（`summary.healthy/total/offline`）允许。**适用范围**：租户可见响应——租户主体、API Token、平台 acting-as（Scope 是目标租户）。平台主体不带 `X-Tenant-ID` 的跨租户只读是平台视角（`either` 路由，如任务中心），不属租户可见响应；矩阵用它证明断言器对真实数据有区分力（同一批任务：租户视角零违规、平台视角有违规）。`tenants.settings` 等租户可见的自由 JSON 在写入时用 `leakguard.ForbiddenKey` 拒绝内部标识键。

集群放置由平台 `system_settings.placement.default_cluster_uids` 决定，租户不选、不可见集群（矩阵种入该设置并断言它不进入任何租户相关响应）。

## 4. 路由元数据检查（AC4）

`make tenantscope-check`（并入 `make contract-check`）：对**真实路由集合**（`cmd/lingedge-server` 的 `registerRoutes`）装配探针钩子（Scope 钩子是一律拒绝的计数探针），对每条需认证的 `tenant | either`（及 `platform`）路由发请求，断言它恰好经过一次 Scope 钩子且 handler 未执行；绕过 Scope 钩子的路由（链被改坏、旁路 mux、自带宽松钩子）、空路由集都会失败。检查器与负向样本：`internal/platform/httpx/contracttest/scopecoverage*.go`。

## 5. 不 Mock

被测系统是 `tests/apistack`：测试自拉起的独立临时 mysqld（应用全部内嵌迁移、真实 `lingedge_app` 账号）与 redis-server、真实认证/租户模块、真实任务中心、httpx 固定中间件链与真实钩子装配。口令/令牌/会话 Cookie/CSRF 令牌全部在运行期生成（G-6：无固定口令、无豁免）并登记，用例结束前扫描日志无明文（G-5）。矩阵还核对进程内装配与真实 `lingedge-server routes` 的路由集合一致（`TestRegisteredRoutesMatchServerBinary`），防止装配漂移让登记机制漏掉真实路由。
