# ADR-017 API 约定：OpenAPI-first、统一错误/分页/Task、契约冻结

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §19、§26；PRD FR-20、FR-21、NFR-4、NFR-21、NFR-22、D-11；architecture AD-17、§9.2｜阶段：M1

## 背景

基线 §19 要求每个接口具备：认证、鉴权、租户隔离、参数验证、错误码规范、Request ID、审计、分页、API 文档、测试，耗时任务用异步 Task ID。基线 §26 与 PRD D-11 要求官网/基础平台并行开发时**不得产生不一致的 API、库结构或重复实现**，因此契约必须先冻结、变更可检测。OpenAPI 草案见 [docs/api/openapi.yaml](../../api/openapi.yaml)，入口见 [docs/api/README.md](../../api/README.md)。

## 决策

1. **OpenAPI 3.1 为 REST 的唯一事实源（OpenAPI-first）**：先改 `openapi.yaml`，再改实现。类型/服务端骨架生成工具（oapi-codegen v2.8.2 为查询值、kin-openapi v0.153.0 用于运行期契约校验）**实现时核对**后择一；若不生成服务端骨架，则必须有契约测试（以 kin-openapi 校验真实响应符合 schema）。
2. **路由元数据**：每条路由在注册时声明 `RouteMeta{OperationID, Auth, Permissions[], TenantScope(platform|tenant|public|either), Audit, Reauth, Phase}`，与 `openapi.yaml` 中的 `x-permissions`、`x-tenant-scope`、`x-audit`、`x-reauth`、`x-phase` 一一对应。**路径模板禁止含 `:`**（IMP-21：Gin v1.12.0 把 `…/rules:validate` 解析为静态段 `rules` + 通配参数，实测 `/sites/S1/rulesXXX` 也会命中该 handler；自定义动作一律用名词子资源，如 `POST /sites/{siteId}/rule-validations`、`POST /certificate-parses`）；CI 检查 OpenAPI 路径不含 `:`。**CI 检查**：(a) 路由表与 OpenAPI 的 operationId 集合双向一致；(b) 任何缺少元数据的路由使构建失败（NFR-4、FR-20）；(c) `x-tenant-scope: tenant` 的 operation 必须经租户 `Scope` 中间件（ADR-009）；(d) **R-T1**（ADR-009 §5）：`tenant|either` 的 operation 请求/响应 schema 递归展开（跟随 `$ref`）后不得含 node/cluster 内部标识属性（`^(node|cluster)_(uid|name|id|uids)$`、`node_*`、`egress_node*`、`targets`）；(e) 路径模板不含 `:`；(f) 声明 `x-reauth: true` 的路由必须经再认证中间件。
3. **统一错误体**（新增 `403 auth.reauth_required`：`x-reauth` 操作在再认证窗口外，ADR-018 §12）：`{"error":{"code","message","message_key","details":[{field,reason}],"request_id"}}`；`code` 为稳定小写点分字符串。状态码映射：校验失败**统一 `400 validation.failed`**（不用 422）；配置发布前校验失败 `400 config.validation_failed`；`401 auth.unauthenticated`；`403 auth.forbidden` / `403 feature.disabled`（运行模式或阶段未启用）；跨租户或不存在一律 `404 resource.not_found`（不泄漏存在性，ADR-009）；`409 resource.conflict` 或模块码（如 `site.domain_conflict`）；`412 resource.precondition_failed`（乐观并发 `If-Match`）；`429 rate_limit.exceeded`；`500 internal.error`。
4. **Idempotency-Key**：创建类 POST 支持该头（≤128 字符，服务端保存 24h，表 `idempotency_keys`，database/platform-identity §4.4）。同键同请求体返回首次响应（含同一 Task）；**同键不同请求体返回 `422 idempotency.key_reuse`**。这是「校验统一 400」的**唯一例外**：它不是字段校验失败，而是客户端协议误用，需与可修复的 `validation.failed` 区分。与 `openapi.yaml`、`api/README.md` 错误码索引、architecture §9.2 通用码清单一致（`idempotency.key_reuse`、`resource.precondition_failed` 均已列入）。
5. **分页**：普通列表 `page/page_size`（默认 20，最大 200）返回 `{items,page,page_size,total}`；大体量集合（审计、访问日志、事件）用游标 `cursor/limit` 返回 `{items,next_cursor}`；排序 `sort=-created_at`，排序字段为每个资源声明的白名单（防任意列排序泄漏/慢查询）。
6. **Request ID**：入口中间件读取或生成 `X-Request-ID`（ULID），写入 `context`、Zap 字段、审计、Task（`tasks.request_id`）、出站 gRPC 元数据 `x-request-id`，响应必返回。
7. **异步 Task**：耗时操作 `202` + `Task` + `Location: /api/v1/tasks/{uid}`；`status ∈ pending|running|success|partial|failed|canceled`；Task `type` 为白名单枚举，**事实源是 `openapi.yaml` 的 `TaskType`**（architecture §9.2、ADR-019、`tasks` 表文档均只引用、不另列清单，避免漂移，M14），**无任意命令类**（FR-35）。回滚沿用 `deployment.publish`（回滚即新修订，ADR-002）。
8. **租户上下文**：Console 主体取其租户；Admin 代租户须 `X-Tenant-ID`（租户 uid），审计双记（ADR-009）；平台资源（nodes、clusters、系统）拒绝租户主体。
9. **资源标识与格式**：对外只用 `uid`（ULID）；时间 RFC 3339 UTC；金额 `{amount_minor,currency}`（ADR-010）；JSON 字段 `snake_case`。
10. **契约冻结与破坏检测（D-11）**：
    - 冻结点：架构评审通过后给 `openapi.yaml`、`proto/`、`migrations/` 打 git 标签 `contract-vN`；此后官网/Admin/Server 才并行。
    - 变更流程：契约改动 PR 必须附 ADR 或「变更说明」；**允许**向后兼容改动（新增可选字段、新增 operation、新增枚举值且客户端约定忽略未知值）；**破坏性改动**（删除/改名字段、收紧校验、改类型、删 operation、改错误码）须新版本前缀或 ADR 批准。
    - CI：`openapi.yaml` 与上一标签做差异检测（工具实现时核对，如 oasdiff 类）；`proto/` 用 buf breaking 或等效（dev-environment §1.4，实现时核对）；`migrations/` 检查已发布迁移文件不得被修改（哈希清单）。
11. **文档与测试同步（NFR-22）**：每个 operation 有至少一个契约测试与权限/越权测试；`make openapi-check` 为合并门禁。

## Binds / Prevents / Rule

- 约束：所有 REST handler、前端 API 类型生成、测试、文档站（FR-120 同源生成）。
- 防止：多个团队各自发明错误体/分页/权限元数据；并行开发时契约悄悄漂移；缺少鉴权/租户/审计元数据的路由上线。
- 规则（AD-17）：**OpenAPI-first；统一错误体、分页、Request ID、异步 Task；每条路由携带元数据并由 CI 与契约双向校验；契约破坏变更须 ADR。**

## 备选方案与权衡

- **代码优先（从 Gin 注解反生成 OpenAPI）**：否决——契约由实现决定，难以在实现前冻结，违背 D-11 并行前提。
- **校验错误用 422**：否决——两种状态码会让前端表单处理分叉；统一 400 + `details[]` 更简单，仅幂等键误用例外。
- **GraphQL / 纯 gRPC-Web 给前端**：否决——基线固定 OpenAPI 3.1 与 `/api/v1`。
- **全部分页都用游标**：否决——Admin 表格需要总数与跳页；仅对大体量集合用游标。

## 后果

- 契约先行需要前期投入；OpenAPI 文件体积大（草案约 2.7 万行），建议后续拆分为多文件并用 bundler 合并（实现时评估）。
- 422 例外增加一个错误码，须在文档与前端错误处理中明确。
- CI 工具链（契约差异、buf）为实现时选型，选型未定前以人工评审兜底。

## 可被推翻处

若用户希望宽松的契约流程（早期迭代频繁）：可把冻结点推迟到 M1 中期，但 D-11 的「并行前提」随之失效，官网与 Server 须串行或接受返工；若采用 422 表示校验错误，需全局修改 openapi 与前端约定。

## 验证

- `make openapi-check`：路由表↔OpenAPI 双向一致、元数据完整、契约差异检测。
- 随机抽样 operation 逐项核对 FR-20 清单；越权/鉴权矩阵测试。
- 幂等键测试：同键同体重放返回同一 Task；同键异体返回 422 `idempotency.key_reuse`。
- Request ID 贯穿测试：由一次 API 调用经 Request ID 追到审计与 Task 日志（NFR-21）。

## 修订记录 2026-11-16

- §2（IMP-21、BLK-02、IMP-12）：路由元数据增 `Reauth`；CI 增 R-T1 静态检查、路径不含 `:`、`x-reauth` 检查；`rules:validate`/`certificates:parse` 改名 `rule-validations`/`certificate-parses`（Gin 实测）。
- §4（L23）：删除“architecture §9.2 通用码清单未列该两码”的过期说法。
- §7（M14）：Task 类型清单以 OpenAPI `TaskType` 为唯一事实源。
- §3：新增通用错误码 `auth.reauth_required`。

## 修订记录 2026-11-21（Story 1.21，实现细化，非推翻）

- §2（路由元数据 CI 检查）新增 (g)：**写 operation（POST/PUT/PATCH/DELETE）必须声明 `x-audit: true`**，确实不产生持久变更的「只读式 POST」须在 `scripts/gates/data/audit-exempt.txt` 登记并写明理由（当前 3 项：`checkDomainAccess`、`validateRule`、`parseCertificate`）；(h) **声明了 `x-audit` 的 operation 必须在 `audit.Catalog` 登记审计动作，且每个动作都有记录调用**（「声明 Audit 但未记录」）。`make audit-check` 执行 (g)(h)（静态门禁 + 真实路由集合检查，均带负向样本自测）。
- §6（Request ID）：NFR-21 的追踪链落地——一次 API 调用的 `X-Request-ID` 写入 `audit_logs.request_id`、`tasks.request_id`（`task_events` 经任务关联）与 Worker 日志；`GET /audit-logs?request_id=`、`GET /tasks?request_id=` 可检索（`tests/integration/audit` 的 `TestRequestIDTracesAuditAndTask`）。`task_events` 本身不含 `request_id` 列（经 `task_id` 关联，无缺口，未追加迁移）。
