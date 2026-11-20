# 契约变更说明 CC-8：`getCurrentTenant` 补声明 400 / 404 响应（加法）；无迁移变更

> 用法与格式见 `_template.md`。对应 Story 1.20（`docs/bmad/stories/1-20-租户模块-scope-中间件与隔离测试框架.md`），发生在 `contract-v1`（CF-1）与 `migrations-m1`（CF-2）之后。**本变更没有任何迁移改动**：`tenants`、`tenant_members`、`tenant_quotas`（`000001`）与种子默认租户（`000004`）、`lingedge_app` 的表级 `SELECT, INSERT, UPDATE, DELETE` 授权经核对已满足本 Story 全部需求。

- **日期**：2026-11-21
- **基线标签**：contract-v1
- **影响范围**：
  1. `docs/api/openapi.yaml`（仅加法）：`GET /tenants/current`（`getCurrentTenant`）新增 `400`（`BadRequest`：平台主体不带 `X-Tenant-ID`，或 `X-Tenant-ID` 不是合法 uid / 重复）与 `404`（`NotFound`：`X-Tenant-ID` 指向他租户、不存在、已删除或状态不可用的租户——对租户主体与平台 acting-as 一视同仁、响应逐字节一致，ADR-009 §4）。未新增路径/operation/错误码，未改任何 `x-*` 扩展、参数与必填项。
  2. 迁移、proto：无改动。
- **是否破坏**：否（只增加已存在的统一错误码 `validation.failed` / `resource.not_found` 在该 operation 上的声明；`make openapi-breaking` 相对 `contract-v1` 无破坏性变更）
- **关联 ADR**：N/A（非破坏；依据 ADR-009 §4/§5：越权与不存在一律 404、acting-as 目标租户须真实存在且可用；ADR-018 §10）
- **变更动机**：Story 1.20 实现完整 Scope 钩子后，`/tenants/current` 的实际响应包含 400（平台主体缺 `X-Tenant-ID`）与 404（目标租户不存在/不可用）。契约原先只声明 200/401/403/500，隔离矩阵的 kin-openapi 响应校验对「未声明的状态码」会失败，客户端也无从得知这两种合法响应。
- **兼容与迁移方案**：无迁移；已有客户端不受影响（只是文档化了本来就会出现的错误响应）。
- **验证**：`make openapi-check`（含 `openapi-breaking`）、`make contract-check`、`tests/security/tenant_isolation`（`/tenants/current` 的 200/400/404 响应经 kin-openapi 校验）。
- **评审**：开发代理（bmad-method:bmad-build）依据机器验证出具，2026-11-21；未经人工签收，用户可事后推翻（推翻则以新的契约变更删除这两个响应声明；删除是破坏性变更，须走 ADR-017 §10 流程）。
