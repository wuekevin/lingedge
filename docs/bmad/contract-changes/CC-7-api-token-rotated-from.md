# 契约变更说明 CC-7：ApiToken 增加 `rotated_from_uid`（加法）；无迁移变更

> 用法与格式见 `_template.md`。对应 Story 1.19（`docs/bmad/stories/1-19-api-token-作用域-吊销-轮换.md`），发生在 `contract-v1`（CF-1）与 `migrations-m1`（CF-2）之后。**本变更没有任何迁移改动**：`api_tokens` 表（`000001`，含 `rotated_from_id`、`token_prefix`、`token_hash`、`allowed_cidrs_json`、`last_used_*`、`revoked_at`）与 `lingedge_app` 的表级 `SELECT, INSERT, UPDATE, DELETE` 授权经核对已满足本 Story 全部需求。

- **日期**：2026-11-21
- **基线标签**：contract-v1
- **影响范围**：
  1. `docs/api/openapi.yaml`（仅加法）：`ApiToken` 新增可选只读属性 `rotated_from_uid`（`Uid | null`，轮换产生的新令牌指向被轮换的旧令牌；旧令牌随即被吊销）。未新增路径/operation/状态码/错误码，未改任何 `x-*` 扩展与必填项。
  2. 迁移、proto、`scripts/gates/data/*.txt`：迁移与 proto 无改动；`identity-tables.txt` 登记 `api_tokens`（认证钩子在 Scope 之前以 `token_hash` 为键读取；管理 API 以创建者自己的 `user_id` 为边界，与 `sessions` 同类），并把 `scripts/gates/gates_test.go` 的「禁入身份表」名单中的 `api_tokens` 移出（其余业务/审计表仍禁入）——见 Story 1.19「门禁变更」。
- **是否破坏**：否（响应侧只新增可选字段；旧客户端忽略即可。`make openapi-breaking` 相对 `contract-v1` 无破坏性变更）
- **关联 ADR**：N/A（非破坏；依据 ADR-018 §7 的「轮换产生新令牌并记 `rotated_from_id`」）
- **变更动机**：OpenAPI 的 `rotateApiToken` 说明写「生成新令牌并吊销旧令牌（rotated_from）」，但 `ApiToken` 没有任何字段把轮换链暴露给调用方；管理界面与排障需要「这个令牌是从哪个令牌轮换来的」。暴露的是旧令牌的 **uid**（不是库内 bigint id），不含任何前缀以外的秘密。
- **兼容与迁移方案**：无迁移。`rotated_from_uid` 在非轮换产生的令牌上恒为 `null`；响应体里原有字段与语义不变。
- **验证**：`make openapi-check`（含 `openapi-breaking`）、`make contract-check`、`tests/integration/auth` 的 `TestAPIToken*`（令牌管理 5 个 operation 的响应经 kin-openapi 逐一校验，含轮换链的 `rotated_from_uid`）。
- **评审**：开发代理（bmad-build）依据机器验证出具，2026-11-21；未经人工签收，用户可事后推翻（推翻则以新的契约变更删除该字段；删除是破坏性变更，须走 ADR-017 §10 流程）。
