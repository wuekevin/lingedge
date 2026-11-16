# ADR-010 金额与计量幂等（整数最小货币单位）

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §16、§17、§18；PRD FR-107、FR-108、FR-109、FR-112、D-3、D-4、NFR-8；architecture AD-10、§9.1｜阶段：数据模型 M1 预留；业务 P5

## 背景

基线 §16：**计费原始记录必须幂等、可追溯、可对账；金额不得用浮点数。** PRD：重复上报同一窗口不重复计费；账单金额可由 `usage_records` 重算一致；支付回调幂等入账；价格/单价不虚构，由用户配置（D-4）。

## 决策

1. **金额**：全系统（DB/Go/API/前端）以**整数最小货币单位**（如分）表示：DB `BIGINT` + `currency CHAR(3)`；Go `type Money struct{ AmountMinor int64; Currency string }`；API `{ "amount_minor": 12345, "currency": "CNY" }`。**禁止 `float32/64`、`DECIMAL` 运算替代**（DB `DECIMAL` 仅用于不参与金额运算的展示字段；默认不用）。前端展示格式化由 `Intl.NumberFormat` 基于整数 + 币种精度表，不做浮点累加。
2. **单价**：以"每 N 单位的最小货币单位整数"表示，避免小数：`unit_price_minor` + `unit_size`（如每 `1_073_741_824` 字节 = 1 GiB 的价格；与 DB `pricing_rules` 列名一致，旧称 `per_unit` 作废），费率阶梯表 `pricing_rules` 存整数阈值与整数价格。**舍入规则单一且显式**：先按整数乘法累加（以最小计量单位，如字节），再在**账单行级**一次性以"向上取整到最小货币单位"（`ceil_div`）[ASSUMPTION：舍入方向待用户确认，Q-5]；不得中途多次舍入；**不提供可配置舍入**（DB 已删除 `pricing_rules.rounding` 列，避免与“单一显式”矛盾）。
3. **计量来源**：节点 rollup（ADR-008）→ `usage_records`（P5）。**只取一种计量粒度**：`usage_records` 由已闭合的 1m `site_node` 窗口聚合为 **300 秒**窗口（5 个对齐窗口均 finalized，取各自最高 `generation`）幂等派生——避免 1m 与 5m 同时派生在唯一键下冲突/重复计量。唯一键 **`(node_id, site_id, window_start, window_seconds, metric)`**（与 DB 一致）；upsert 规则统一为 `generation` 语义（不再使用 `report_seq/revision` 的并行说法）：同键同值=忽略；更高 `generation`（且 `billed_invoice_id IS NULL`）=替换并 `revision+1`、写审计；**同代不同值=冲突事件，不覆盖**（DB 经 `rollup_windows.content_sha256` 判定，与 proto `CONFLICT` 一致）；已出账窗口冻结。度量值全整数：`bytes_out`、`bytes_in`、`requests`、`bandwidth_peak_bps`（DB/proto 同名；旧称 `peak_bandwidth_bps` 作废）。`bandwidth_peak_bps` 非加总类：Agent 以日志秒级 `ts` 与 `bytes_sent` 按 1 秒分桶取窗口内峰值（聚合 `max`），属于 proto `RollupMetrics` 的新增字段。**计量口径固定**：`bytes_out = $bytes_sent`（Agent rollup 定义，`pricing_rules` 不再有 `metric_def` 列；若产品改用 `body_bytes_sent`，须经 ADR 变更 rollup 定义与 proto 注释，[ASSUMPTION]）。
4. **账单生成**：`invoices` + `invoice_items`；`invoice_items` 引用"计量窗口范围 + 规则版本 + 数量 + 单价 + 小计"，**可由 `usage_records` 按 `pricing_rules` 版本重算**，重算函数是纯函数（可单测，计费对账测试 §23）。账单关闭（`closed`）后不可修改，更正通过冲销行（红字）。
5. **钱包**：`wallets.balance_minor` 为缓存列；真相是 `wallet_entries`（追加只写，不可 UPDATE/DELETE，触发器保障）；每条含 `entry_type`、`amount_minor`（正=入，负=出）、`balance_after_minor`、`idempotency_key`（UNIQUE）、`ref_type/ref_id`（订单/账单/支付）。并发：`wallets.version` 乐观锁或 `SELECT … FOR UPDATE` 行锁；扣费在事务内"校验余额 → 写 entry → 更新 balance → version+1"；不允许产生负余额除非钱包策略允许透支（默认不允许）。
6. **支付与回调**：`payments`/`payment_events`：回调事件以 `(provider, event_id)` 唯一键去重；验签失败拒绝；入账与 `wallet_entries` 用同一 `idempotency_key`（由 `provider:event_id` 派生）；重复回调只入账一次；对账任务比对 `payments` 与 `wallet_entries`/订单，未匹配项生成对账差异记录（FR-112）。手工入账走同一条路径，`provider='manual'`，必须带操作员与凭证说明并审计。
7. **自用模式**：表与代码同一套，业务开关 `system_settings.run_mode`；`self_hosted` 下计费 API 返回 `403 feature.disabled`，但 rollup→`usage_records` 的派生**可选**启用（仅用于统计，不产生账单）。
8. **溯源**：`usage_records` 以 `(node_id, site_id, window_start)` 与 `rollup_windows`/`access_log_chunks`（`line_count`）关联，使"账单行 → 计量窗口 → rollup → 日志 chunk"链路可追溯（NFR-18/FR-108）；DB 无 `source_rollup_ids`/`usage_revisions` 列或表，窗口被覆盖的次数记在 `usage_records.revision` 并写审计。
9. **代码规范**：Lint 规则禁止在 `internal/billing` 与 `pkg/money` 之外对金额字段做算术；`pkg/money` 提供 `Add/Sub/MulInt/CeilDiv` 带溢出检查（`math/bits`）。

## Binds / Prevents / Rule

- 约束：billing 模块、monitoring→usage 派生、支付插件、API 与前端的金额表示。
- 防止：浮点累计误差；重复上报/重复回调重复计费或重复入账；账单无法重算；并发扣费丢失更新。
- 规则（AD-10）：**金额与计量一律整数最小单位；原始计量以幂等键唯一；账单可由原始记录重算；钱包流水追加只写。**

## 备选方案与权衡

- **DECIMAL(18,4) 存金额**：否决——避免定点小数四舍五入分歧；整数最小单位更简单、跨语言一致。
- **在账单生成时再聚合原始日志**：否决——成本高；以 rollup→usage 的确定性窗口为计量基本单位。
- **用 Redis 计数做实时扣费**：否决——Redis 非真相源（ADR-025）；扣费必须在 MySQL 事务内。

## 后果

- 费率表达需要"每 N 单位"的整数表示，产品配置界面要解释；舍入方向待用户确认（[ASSUMPTION]）。
- `generation` 更正机制增加 `usage_records` 的修订管理。

## 可被推翻处

舍入方向与口径（`bytes_sent` vs `body_bytes_sent`）、币种精度表、是否允许透支；定价模型（阶梯/带宽峰值/95 计费）待 P5 产品定义，不影响整数原则。

## 验证

- 计费对账测试：重放同一批 rollup/usage，用量与账单金额不变；从 `usage_records` 重算与账单逐项一致；代码检索无金额浮点（静态规则）。
- 并发扣费压测无负余额/丢失更新；支付回调重复到达只入账一次；`wallet_entries` 的 UPDATE/DELETE 被触发器拒绝。

## 修订记录 2026-11-16

依据评审 IMP-09、M15（计量部分）将 ADR-010 回写为 `docs/database/billing-support.md` 与 `dns-scheduling-security.md` 的事实：
- §2：`per_unit`→`unit_size`；删除“可配置舍入”（DB `pricing_rules.rounding` 已删）。
- §3：唯一键含 `window_seconds`（DB 同步）；`generation` 取代 `report_seq/revision` 的并行说法；`peak_bandwidth_bps`→`bandwidth_peak_bps` 并补生产者（proto `bandwidth_peak_bps`）；**单一 300 秒计量粒度**；口径固定 `$bytes_sent`；去掉 DB 不存在的 `metric_def`/`usage_revisions`。
- §8：溯源方式回写。
- 原因：ADR 与 DB 在唯一键、更正计数、指标名上各说各话；同代异值在 proto 是 CONFLICT 而 DB 静默覆盖；`peak_bandwidth_bps` 无生产者；1m/5m 粒度未定会重复计量。
