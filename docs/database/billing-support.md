---
title: "数据库：计费、计量与工单"
status: draft
created: 2026-11-16
updated: 2026-11-16（依据架构评审修订，见 docs/bmad/architecture-revision-log.md）
---

# 计费、计量与工单（P5）

> 返回 [README](README.md) · [architecture.md](../architecture/architecture.md)。相关 ADR：010 金额与计量幂等。**全部金额为 `BIGINT` 最小货币单位 + `currency CHAR(3)`，库内与代码无浮点金额。** 具体价格、单价与支付渠道不在文档中虚构（D-4），由运营者配置。自用模式下本文件表存在但商业功能被开关禁用（FR-24）。简写见 README §2.1。

## 1. 产品与套餐

### 1.1 `products`　**P5**
用途：可售/可计量的产品（CDN 流量、带宽、请求数、四层、DNS、附加项）。平台级。通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| code | VARCHAR(48) | N | — | 唯一，如 `cdn_traffic` |
| name | VARCHAR(128) | N | — | |
| product_type | VARCHAR(16) | N | — | `cdn_traffic/cdn_bandwidth/requests/l4/dns/addon` |
| billing_mode | VARCHAR(20) | N | — | `prepaid_package/postpaid_usage/metered` |
| unit | VARCHAR(16) | N | — | `byte/bps_peak/bps_p95/request/connection` |
| status | VARCHAR(16) | N | 'active' | `active/retired` |
| description | VARCHAR(512) | Y | — | |
| display_json | JSON | Y | — | 官网展示用的**非价格**元数据（图标、特性要点 key） |

- 唯一：`uk_products_code(code)`。CHECK：枚举取值。

### 1.2 `plans`　**P5**
用途：套餐（正式价格的唯一来源；官网只引用，FR-45）。平台级。通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| product_id | BIGINT UNSIGNED | N | — | FK → products(id) RESTRICT |
| code | VARCHAR(64) | N | — | 唯一 |
| name | VARCHAR(128) | N | — | |
| price_minor | BIGINT | N | — | 套餐价（最小单位）；纯按量套餐为 0 |
| currency | CHAR(3) | N | — | |
| billing_period | VARCHAR(8) | N | — | `month/year/once/usage` |
| included_json | JSON | Y | — | 含额（整数，例如 `{"bytes_out": ...}`） |
| quota_json | JSON | Y | — | 订阅后写入 `tenant_quotas` 的配额 |
| visibility | VARCHAR(8) | N | 'public' | `public/private` |
| status | VARCHAR(16) | N | 'draft' | `draft/active/retired` |
| sort_order | INT | N | 0 | |

- 唯一：`uk_plans_code(code)`。索引：`idx_plans_product(product_id, status)`。CHECK：`price_minor >= 0`；枚举取值。
- 约束：**`website_*` 表不得含任何价格列**；官网仅存 `plans.uid`（cms.md）。

### 1.3 `pricing_rules`　**补充 · P5**
用途：定价规则（阶梯/单价），全整数。C1 风格 C2 + 平台级。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| plan_id | BIGINT UNSIGNED | N | — | FK → plans(id) CASCADE |
| metric | VARCHAR(32) | N | — | 对应 `usage_records.metric` |
| tier_from | BIGINT UNSIGNED | N | 0 | 阶梯起点（含），单位同计量 |
| tier_to | BIGINT UNSIGNED | Y | — | 终点（不含）；NULL=无上限 |
| unit_size | BIGINT UNSIGNED | N | — | 计价单位大小（如 1073741824 字节），>0 |
| unit_price_minor | BIGINT | N | — | 每 `unit_size` 的价格 |
| currency | CHAR(3) | N | — | |
| version | INT UNSIGNED | N | 1 | 生效版本；账单记录所用 `pricing_rule_id` 以便重算 |
| effective_from | DATETIME(3) | N | — | |

- 唯一：`uk_pricing_rules(plan_id, metric, tier_from, version)`。CHECK：`unit_size>0`；`tier_to IS NULL OR tier_to>tier_from`；`unit_price_minor>=0`。
- 计算：`amount_minor = ceil(quantity_in_tier * unit_price_minor / unit_size)`，使用 64 位整数/大整数，**禁止浮点**；溢出检查。**舍入固定为向上取整（`ceil`）且只在账单行级舍入一次**（ADR-010 §2，`[ASSUMPTION]`，舍入方向待用户确认 Q-5）；原 `rounding` 列（ceil/floor/half_up）已删除，避免“舍入规则单一且显式”与可配置舍入相矛盾。

## 2. 订阅与订单

### 2.1 `subscriptions`　**P5**
通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| plan_id | BIGINT UNSIGNED | N | — | FK → plans(id) RESTRICT |
| status | VARCHAR(12) | N | 'pending' | `pending/active/past_due/canceled/expired` |
| started_at | DATETIME(3) | Y | — | |
| current_period_start / current_period_end | DATETIME(3) | Y | — | |
| auto_renew | TINYINT(1) | N | 0 | |
| canceled_at | DATETIME(3) | Y | — | |
| order_id | BIGINT UNSIGNED | Y | — | FK → orders(id) SET NULL |
| price_minor | BIGINT | N | — | 订阅时价格快照 |
| currency | CHAR(3) | N | — | |
| plan_snapshot_json | JSON | N | — | 套餐条款快照（改价不影响存量订阅） |

- 索引：`idx_subscriptions_tenant(tenant_id, status)`、`idx_subscriptions_period(status, current_period_end)`（到期处理）。

### 2.2 `orders`　**P5**
通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| order_no | VARCHAR(32) | N | — | 唯一业务单号 |
| order_type | VARCHAR(16) | N | — | `subscription/topup/manual` |
| plan_id | BIGINT UNSIGNED | Y | — | FK → plans(id) SET NULL |
| amount_minor | BIGINT | N | — | |
| currency | CHAR(3) | N | — | |
| status | VARCHAR(12) | N | 'pending' | `pending/paid/canceled/refunded/failed` |
| payment_method | VARCHAR(48) | Y | — | `<plugin_key>/manual` |
| paid_at | DATETIME(3) | Y | — | |
| idempotency_key | VARCHAR(128) | Y | — | |
| created_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |

- 唯一：`uk_orders_no(order_no)`、`uk_orders_idem(tenant_id, idempotency_key)`。索引：`idx_orders_tenant(tenant_id, status, created_at)`。CHECK：`amount_minor>=0`；状态机由 Service 校验。

## 3. 钱包与流水

### 3.1 `wallets`　**P5**
用途：租户余额账户（每币种一个）。并发控制：`version` 乐观锁 + 行锁；**余额只由 `wallet_entries` 流水推进**。通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| currency | CHAR(3) | N | — | |
| balance_minor | BIGINT | N | 0 | 当前余额（冗余，随流水事务更新） |
| credit_limit_minor | BIGINT | N | 0 | 允许透支额度（≥0） |
| low_balance_threshold_minor | BIGINT | Y | — | 余额不足告警阈值（运营者配置） |
| version | BIGINT UNSIGNED | N | 0 | 每次更新 +1 |

- 唯一：`uk_wallets(tenant_id, currency)`。CHECK：`balance_minor >= -credit_limit_minor`（并发扣费不得产生非法负余额，FR-107）；`credit_limit_minor>=0`。
- 更新：`UPDATE wallets SET balance_minor=balance_minor+?, version=version+1 WHERE id=? AND balance_minor+?>=-credit_limit_minor`，受影响 0 行即余额不足；与追加流水同一事务。

### 3.2 `wallet_entries`　**P5**（追加只写）
用途：余额流水（充值、扣费、退款、调整）。**不可 UPDATE/DELETE**（触发器，README §5）；更正用反向分录。**C2u** + T（对外暴露 `uid`，不暴露 bigint id）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| wallet_id | BIGINT UNSIGNED | N | — | FK → wallets(id) RESTRICT |
| entry_type | VARCHAR(16) | N | — | `topup/charge/refund/adjust/package_grant` |
| amount_minor | BIGINT | N | — | 带符号，非 0（充值 +，扣费 −） |
| balance_after_minor | BIGINT | N | — | 本分录后余额（对账校验） |
| currency | CHAR(3) | N | — | |
| idem_key | VARCHAR(128) | N | — | **幂等键**（如 `invoice:<uid>`、`payment:<provider>:<txn>`） |
| ref_type | VARCHAR(16) | Y | — | `order/invoice/payment/manual` |
| ref_id | BIGINT UNSIGNED | Y | — | 多态引用 |
| description | VARCHAR(255) | Y | — | |
| created_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |

- 唯一：`uk_wallet_entries_idem(wallet_id, idem_key)`。索引：`idx_wallet_entries_wallet(wallet_id, id)`、`idx_wallet_entries_tenant(tenant_id, created_at)`。CHECK：`amount_minor <> 0`。
- 对账不变量：`wallets.balance_minor = Σ wallet_entries.amount_minor`（定期校验任务，差异告警）。保留：永久。

## 4. 计量

### 4.1 `usage_records`　**P5**（高频，无外键；M1 的 rollup 管线已按此键设计）
用途：**原始计量记录**，幂等、可追溯、可对账（FR-108/109）。由 `metric_rollups(dim_type='site_node')` 的已闭合 **1m 窗口聚合为 300 秒窗口**后 upsert 生成（**只有一种计量粒度**，避免 1m 与 5m 同时派生在唯一键下冲突或重复计量）。C2（含 `updated_at`）；**无 `uid`**（OpenAPI 以 `(node, site, window_start, metric)` 复合键字段表示，不暴露 bigint id）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| tenant_id | BIGINT UNSIGNED | N | — | |
| site_id | BIGINT UNSIGNED | N | — | |
| node_id | BIGINT UNSIGNED | N | — | |
| metric | VARCHAR(32) | N | — | `bytes_out/bytes_in/requests/bandwidth_peak_bps/l4_bytes…`（词表见 dns-scheduling-security §5.1；口径固定 `bytes_out=$bytes_sent`，`bandwidth_peak_bps` 取 `max`） |
| window_start | DATETIME(3) | N | — | UTC；窗口起点（与契约 `DATETIME(3)` 一致） |
| window_seconds | INT UNSIGNED | N | — | 窗口长度；**M1/P5 固定 300**，参与唯一键以便将来演进 |
| quantity | BIGINT UNSIGNED | N | — | 数量（字节/次数/bps） |
| generation | INT UNSIGNED | N | 0 | 派生所依据的 rollup 最高 generation（聚合 5 个 1m 窗口时取其中最大 generation 的哈希/最大值，使任一 1m 窗口被更正都能触发重算） |
| revision | INT UNSIGNED | N | 1 | 本窗口被覆盖次数（审计用，替代 ADR-010 旧称 `usage_revisions`） |
| source | VARCHAR(8) | N | 'agent' | `agent/backfill` |
| billed_invoice_id | BIGINT UNSIGNED | Y | — | 已计入账单后非空；此后**冻结**（触发器） |
| updated_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) ON UPDATE | |

- **幂等键**：唯一 `uk_usage_records(node_id, site_id, window_start, window_seconds, metric)`（ADR-010 §3 一致）。重复派生同窗口 → `INSERT … ON DUPLICATE KEY UPDATE`，仅当 `billed_invoice_id IS NULL AND VALUES(generation) > generation` 时覆盖 `quantity/generation/revision+1`，否则忽略；**同代异值 = 冲突事件，不覆盖**（与 proto `CONFLICT` 语义一致）；**从不累加**，故重放不重复计费（AC-H-3）。
- 索引：`idx_usage_tenant_time(tenant_id, window_start, metric)`、`idx_usage_site(tenant_id, site_id, window_start)`、`idx_usage_unbilled(billed_invoice_id, window_start)`。
- 追溯：`(node_id, site_id, window_start)` 可回溯到来源节点窗口及 `access_log_chunks.line_count` 对账。
- 迟到数据：账单周期关闭后到达的窗口不得改写已冻结记录；以账单“调整”项（`invoice_items.item_type='adjustment'`）补差，保留审计。

## 5. 账单与支付

### 5.1 `invoices`　**补充 · P5**
通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| invoice_no | VARCHAR(32) | N | — | 唯一 |
| period_start / period_end | DATETIME(3) | N | — | 账期 [start, end)（与契约 `DATETIME(3)` 一致） |
| status | VARCHAR(8) | N | 'draft' | `draft/issued/paid/void/overdue` |
| currency | CHAR(3) | N | — | |
| subtotal_minor | BIGINT | N | — | |
| tax_minor | BIGINT | N | 0 | |
| total_minor | BIGINT | N | — | |
| issued_at / due_at / paid_at | DATETIME(3) | Y | — | |
| generated_by_task_id | BIGINT UNSIGNED | Y | — | FK → tasks(id) SET NULL |
| recompute_hash | CHAR(64) | Y | — | 由用量集合与定价规则版本计算的校验哈希，供对账重算比对 |

- 唯一：`uk_invoices_no(invoice_no)`、`uk_invoices_period(tenant_id, currency, period_start, period_end)`（账期内至多一张）。CHECK：`total_minor = subtotal_minor + tax_minor`。
- `usage_records.billed_invoice_id` 外键语义指向本表（高频表不声明 FK，由出账事务保证）。

### 5.2 `invoice_items`　**补充 · P5**
C2 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| invoice_id | BIGINT UNSIGNED | N | — | FK → invoices(id) CASCADE |
| item_type | VARCHAR(16) | N | — | `usage/subscription/adjustment/discount` |
| description | VARCHAR(255) | N | — | |
| site_id | BIGINT UNSIGNED | Y | — | |
| metric | VARCHAR(32) | Y | — | |
| quantity | BIGINT UNSIGNED | Y | — | |
| unit_size | BIGINT UNSIGNED | Y | — | |
| unit_price_minor | BIGINT | Y | — | |
| amount_minor | BIGINT | N | — | 逐项金额（可为负：折扣/调整） |
| pricing_rule_id | BIGINT UNSIGNED | Y | — | FK → pricing_rules(id) SET NULL |
| usage_from / usage_to | DATETIME(3) | Y | — | 覆盖的用量窗口范围（重算依据） |

- 索引：`idx_invoice_items_invoice(invoice_id)`、`idx_invoice_items_tenant(tenant_id, site_id)`。

### 5.3 `payments`　**补充 · P5**
通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| order_id | BIGINT UNSIGNED | Y | — | FK → orders(id) SET NULL |
| invoice_id | BIGINT UNSIGNED | Y | — | FK → invoices(id) SET NULL |
| provider | VARCHAR(48) | N | — | 插件 key 或 `manual` |
| provider_txn_id | VARCHAR(128) | N | — | 渠道交易号 |
| amount_minor | BIGINT | N | — | |
| currency | CHAR(3) | N | — | |
| status | VARCHAR(12) | N | 'pending' | `pending/succeeded/failed/refunded/unmatched` |
| reconciliation_status | VARCHAR(12) | N | 'unmatched' | `unmatched/matched/mismatch` |
| paid_at / reconciled_at | DATETIME(3) | Y | — | |

- 唯一：`uk_payments_txn(provider, provider_txn_id)`。索引：`idx_payments_tenant(tenant_id, status)`、`idx_payments_recon(reconciliation_status, paid_at)`。`tenant_id` 对“无法匹配的付款”可由平台指定默认租户或后补（`unmatched` 状态在平台对账页处理）。

### 5.4 `payment_events`　**补充 · P5**（回调幂等）
C2。`payment_id`（FK→payments SET NULL）、`provider VARCHAR(48)`、`event_id VARCHAR(128)`、`event_type VARCHAR(48)`、`payload_json JSON N`（敏感字段脱敏）、`signature_valid TINYINT(1) N`、`processed_at DATETIME(3) Y`。唯一 `uk_payment_events(provider, event_id)`：同一回调重复到达只入账一次；验签失败行仍记录但不处理。

## 6. 代理商

### 6.1 `resellers`　**补充 · P5**
用途：代理商与代理商—租户归属（D-4：分成规则待用户确认 OQ-3，**未确认前不产生分成金额**）。通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| name | VARCHAR(128) | N | — | |
| user_id | BIGINT UNSIGNED | N | — | 代理商后台账号，FK → users(id) RESTRICT |
| status | VARCHAR(16) | N | 'active' | `active/suspended` |
| commission_rule_json | JSON | Y | — | 占位，NULL=未配置 |

- 唯一：`uk_resellers_user(user_id)`。`tenants.reseller_id` 指向本表；代理商仅能看 `tenants.reseller_id = 自己` 的租户（Repository 增加 reseller scope）。

## 7. 工单

### 7.1 `tickets`　**P5**
通用列：C1 + T。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| ticket_no | VARCHAR(32) | N | — | 唯一 |
| subject | VARCHAR(255) | N | — | |
| category | VARCHAR(32) | Y | — | |
| priority | VARCHAR(8) | N | 'normal' | `low/normal/high/urgent` |
| status | VARCHAR(16) | N | 'open' | `open/pending_admin/pending_user/resolved/closed` |
| created_by_user_id | BIGINT UNSIGNED | N | — | FK → users(id) RESTRICT |
| assignee_user_id | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| last_message_at | DATETIME(3) | Y | — | |
| closed_at | DATETIME(3) | Y | — | |

- 唯一：`uk_tickets_no(ticket_no)`。索引：`idx_tickets_tenant(tenant_id, status, last_message_at)`、`idx_tickets_assignee(assignee_user_id, status)`。

### 7.2 `ticket_messages`　**P5**
**C2u** + T（对外暴露 `uid`）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| ticket_id | BIGINT UNSIGNED | N | — | FK → tickets(id) CASCADE |
| author_user_id | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| author_type | VARCHAR(8) | N | — | `tenant/admin/system` |
| body | TEXT | N | — | 渲染时转义（XSS，NFR-9） |
| attachments_json | JSON | Y | — | 引用受控存储对象，不含路径 |
| internal_note | TINYINT(1) | N | 0 | **管理员内部备注，租户接口必须过滤** |

- 索引：`idx_ticket_messages_ticket(ticket_id, id)`、`idx_ticket_messages_tenant(tenant_id, ticket_id)`。
