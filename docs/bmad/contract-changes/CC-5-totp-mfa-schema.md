# 契约变更说明 CC-5：TOTP 二步登录的存储缺口（`login_mfa_challenges`、`users.totp_last_step`）与 `/auth/totp/*` 响应补全

> 用法与格式见 `_template.md`。对应 Story 1.17（`docs/bmad/stories/1-17-认证-ii-totp-恢复码-近期-mfa.md`），发生在 `contract-v1`（CF-1）与 `migrations-m1`（CF-2）之后：M1 冻结范围（`000001`–`000013`、61 张表）不动，仅追加新迁移 `000016`、`000017`；OpenAPI 只做**加法**（补充响应码与描述）。

- **日期**：2026-11-21
- **基线标签**：contract-v1
- **影响范围**：
  1. 新增迁移 `migrations/000016_totp_mfa.sql`：`CREATE TABLE IF NOT EXISTS login_mfa_challenges`（一次性、限次的 `mfa_token` 挑战；`GRANT … TO lingedge_app`）。
  2. 新增迁移 `migrations/000017_users_totp_last_step.sql`：`ALTER TABLE users ADD COLUMN totp_last_step BIGINT UNSIGNED NULL`。
  3. `migrations/CHECKSUMS` 追加登记；`docs/database/platform-identity.md`（§2.1 users 列、§2.2.1 新表、sessions 列说明、系统设置键）、`docs/database/README.md §8.1` 追加说明；`scripts/gates/data/platform-tables.txt` 登记 `login_mfa_challenges`。`000001`–`000015` 一字未改。
  4. `docs/api/openapi.yaml`（仅加法）：`POST /auth/totp/verify`（`verifyTotp`）补充 `'401'`；`setupTotp`/`enableTotp`/`disableTotp` 补充 `'409'`（引用既有 `Conflict`）；四个 operation 与 `verifyTotp` 补充 description。错误码、schema、`x-*`、proto：无改动。
  5. ADR-018 追加修订记录（实现细化）。
- **是否破坏**：否（新增可空列、新表、新增响应码与描述文字；不删除/改名/收紧任何既有项）
- **关联 ADR**：N/A（非破坏；依据 ADR-018 §5/§12、ADR-025）
- **变更动机**：
  1. **`mfa_token` 的存放处**：OpenAPI `LoginResponse.mfa_token` 定义为「仅 totp_required 时返回的短期一次性令牌（5 分钟）」。「一次性」与「限制验证码尝试次数」都需要服务端状态；Redis 不是真相源且可能不可用（ADR-025），而「Redis 关闭下登录仍工作」是硬性验收——所以放 MySQL。方案对比：(a) 新表 `login_mfa_challenges`（采用）——跨实例一致、可原子消耗、可限次、令牌只存哈希；(b) 无状态签名令牌——需要新增服务端 MAC 密钥管理，且无法保证一次性与限次；(c) 把挑战塞进 `sessions`——会让「半登录」行出现在会话表里，存在被当作真实会话的风险，否决。
  2. **`totp_last_step`**：AC2「同一验证码在窗口内重放被拒」要求持久化已用 time-step 且多实例一致。`users` 没有对应列；Redis 不可作为唯一防线。原子 `UPDATE … WHERE totp_last_step IS NULL OR totp_last_step < ?` 以受影响行数判定重放。
  3. **响应码补全**：`verifyTotp` 的凭据错误是 `401 auth.totp_invalid`（错误码表已有，`apierr` 状态 401），但 operation 只声明了 200/400/429/500，契约响应校验会把它判为「未声明的状态码」——与 CC-4 对 `login` 的处理同类，是契约遗漏而非行为变更。`setup/enable/disable` 的状态冲突（已启用/未 setup/未启用）用 `409 resource.conflict`（既有码），同样补声明。
- **兼容与迁移方案**：
  1. 两个迁移均 `expand`、无 Down；`000016` 为新表，`000017` 为可空列追加（对已存在行安全）。迁移 lint 通过（`make migrate-test`）。`000017` 拆成独立单语句文件是迁移 lint 的幂等规则要求（`ALTER … ADD COLUMN` 无 `IF NOT EXISTS`）。
  2. 授权：`users` 已有表级授权，新列自动适用；`login_mfa_challenges` 在 `000016` 内逐表授予 `lingedge_app` 的 `SELECT, INSERT, UPDATE, DELETE`（最小：不授予 DDL/其他账号）。
  3. **M1 冻结口径**：「M1 = 000001–000013、61 张表」不变；`freeze-check` 不变；`000014`–`000017` 为追加迁移。
  4. OpenAPI 为加法：旧客户端不受影响。并行泳道返工：无（前端按 `status=totp_required` + `mfa_token` → `/auth/totp/verify` 实现即可，契约早已如此定义）。
- **验证**：`make migrate-test`（真实 MySQL 全量迁移含 000016/000017）、`tests/integration/auth`（两步登录、重放、漂移 ±1/±2、恢复码、挑战一次性/限次/过期、节流、强制策略、近期 MFA、再认证窗口边界、Bearer、Redis 关闭、全库明文扫描；每个响应经 kin-openapi 校验）、`make migrations-check`（相对 `migrations-m1` 只新增）、`make openapi-check`/`make contract-check` 全绿（见 Story 1.17 开发记录）。
- **评审**：开发代理（bmad-build）依据机器验证出具，2026-11-21；未经人工签收，用户可事后推翻（推翻 (1) 则以新的迁移把挑战改存 Redis 或签名令牌并放弃「Redis 关闭下仍可两步登录」；推翻 (2) 则以新迁移删除该列并改用其他重放防护）。
