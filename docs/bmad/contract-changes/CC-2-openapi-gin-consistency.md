# 契约变更说明 CC-2：OpenAPI 与 Gin 路由层的一致性修正（同位置路径参数名统一；retryDeployment 补 422 响应）

> 用法与格式见 `_template.md`。本说明对应 Story 1.12（HTTP 服务骨架），发生在 `contract-v1`（CF-1）之后、`migrations-m1`（CF-2）封存之后；只改 `docs/api/openapi.yaml`（路径参数名统一 + 1 个 operation 补声明 422 响应），迁移与 proto 无任何改动。

- **日期**：2026-11-21
- **基线标签**：contract-v1
- **影响范围**：`docs/api/openapi.yaml` 中 3 组路径模板的参数名与其对应的 `components/parameters` 引用，共 17 个 operation（均为 P3/P4 阶段，**不含任何 M1 operation**）：
  - `/dns/zones/{zoneId}/{records,records/{dnsRecordId},routes,routes/{dnsRouteId},sync,dnssec}` → `/dns/zones/{dnsZoneId}/…`（P3；对齐同资源的 `/dns/zones/{dnsZoneId}`）
  - `/waf/rulesets/{rulesetId}/{rules,rules/{wafRuleId}}` → `/waf/rulesets/{wafRulesetId}/…`（P4）
  - `/l4/applications/{appId}/{listeners,listeners/{l4ListenerId},backends,backends/{l4BackendId},stats}` → `/l4/applications/{l4ApplicationId}/…`（P4）
  - `components/parameters`：删除未再被引用的 `p_zoneId`、`p_rulesetId`、`p_appId`，保留的 `p_dnsZoneId`、`p_wafRulesetId`、`p_l4ApplicationId` 的描述补全为原先的人话描述。
  - **补 422 响应（唯一的内容性改动）**：`retryDeployment`（M1，`POST /deployments/{deploymentId}/retry`）声明了 `Idempotency-Key` 参数却没有 `422 idempotency.key_reuse` 响应，与 ADR-017 §3/§4 及其余 56 个声明该参数的 operation 不一致；补上 `'422': $ref IdempotencyKeyReuse`（仅新增响应码，请求/成功响应不变）。
- **是否破坏**：否（新增一个已有于其他 operation 的响应码属向后兼容的增量；路径参数名只是模板里的占位符名；线上请求的 URL、方法、参数类型/取值、请求/响应 schema、权限与 `x-*` 扩展、operationId 均不变；`make openapi-check` 的 oasdiff 对 `contract-v1` 报告无破坏性变更）
- **关联 ADR**：N/A（非破坏；约束依据 ADR-017 §2「路由元数据与 OpenAPI 一一对应」与 Gin 路由树的通配符规则）
- **变更动机**：Story 1.12 的路由注册层把 OpenAPI 路径模板转为 Gin 路径（`{name}` → `:name`）。Gin 的基数树（每个 HTTP 方法一棵）要求同一位置只能有一个通配符名，否则注册时 panic：`GET /dns/zones/{dnsZoneId}` 与 `GET /dns/zones/{zoneId}/records`、`GET /l4/applications/{l4ApplicationId}` 与 `GET /l4/applications/{appId}/listeners`、`GET /waf/rulesets/{wafRulesetId}` 与 `GET /waf/rulesets/{rulesetId}/rules` 在同一位置用了不同的参数名。契约测试（`httpx/contracttest.TestEveryOperationRegistersCleanly`）把全部 486 个 operation 注册进真实 Gin 路由树时暴露了 17 处冲突。这些路径属于 P3/P4，实现前改名没有任何返工成本；晚到阶段到来时再发现则会在装配期崩溃。`retryDeployment` 缺 422 则由 Story 1.12 的幂等集成测试（`TestKeyReuseResponseMatchesOpenAPIContract`：对全部 57 个声明 Idempotency-Key 的 operation 校验 422 响应符合契约）发现。
- **兼容与迁移方案**：线上协议零变化，旧客户端无感。前端 API 类型生成（ADR-014）以 OpenAPI 为源，路径参数名只影响生成代码里的形参名，P3/P4 前端尚未开始。不升 `contract-vN`。同时为防回归，`make openapi-check` 新增规则 `path-param-name` 与 `idempotency-422`（声明 Idempotency-Key ⇔ 声明 422，引用 IdempotencyKeyReuse）（同一方法、同一路径位置的参数名必须一致；`scripts/contract/routes_test.go` 含对原样本的负向用例，对改名前的 openapi 副本报 8 处路径参数违规，改名后 0 处；`TestIdempotencyResponsesRule` 含 4 个正/负向样本）。
- **验证**：`make openapi-check`（OK：路径 307、operation 486；相对 `contract-v1` 无破坏性变更）；`make contract-check` 全绿；`go test ./internal/platform/httpx/contracttest/`（486 个 operation 全量注册进 Gin 无冲突）；`scripts/contract` 单测（含 `TestPathParamNamesConsistent` 的正/负向样本，及对真实 openapi 的 `TestRealOpenAPIPassesAllChecks`）。
- **评审**：开发代理（bmad-build）依据机器验证出具，2026-11-21；未经人工签收，用户可事后推翻（推翻则恢复 3 组路径名，并改用「每个 operation 一个独立路径参数别名」的路由层方案，成本较高，不建议）。
