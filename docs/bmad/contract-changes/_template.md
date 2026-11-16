# 契约变更说明 CC-<编号>：<标题>

> 用法：契约（`docs/api/openapi.yaml`、`proto/lingedge/node/v1/*.proto`、`docs/database/` 的迁移结构、`migrations/`）在冻结标签（`contract-vN`）之后的任何变更都须有本说明；
> **破坏性变更**还必须关联 ADR（ADR-017 §10）。复制本文件为 `CC-<编号>-<slug>.md` 并逐项填写，所有 `<待填>` 须替换，否则 `make openapi-check CONTRACT_NOTE=…` 不会放行。
> 字段格式固定（`- **字段**：值`），由 `scripts/contract` 机器校验：是否破坏 ∈ {是, 否}；破坏性变更须有「关联 ADR：ADR-NNN」。

- **日期**：<待填>
- **基线标签**：contract-v1
- **影响范围**：<待填>（OpenAPI operation / schema / 错误码；proto 消息 / 字段 / RPC；迁移文件 / 表；列出具体名称）
- **是否破坏**：<待填>（是 / 否。破坏 = 删除或改名字段/operation/错误码、收紧校验、改类型、放宽响应必填、新增必需权限/再认证）
- **关联 ADR**：<待填>（破坏性变更必须填 ADR-NNN；非破坏性可写 N/A）
- **变更动机**：<待填>
- **兼容与迁移方案**：<待填>（旧客户端如何继续工作；并行泳道需返工的部分；是否升 `contract-vN`）
- **验证**：<待填>（`make openapi-check` / `make proto-check` / `make migrations-check` 的结果）
- **评审**：<待填>（评审人、日期、结论）
