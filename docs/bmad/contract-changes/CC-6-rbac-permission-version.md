# 契约变更说明 CC-6：权限版本列（`users.permission_version`）与 /admin/users 口令生成的加法补充

> 用法与格式见 `_template.md`。对应 Story 1.18（`docs/bmad/stories/1-18-rbac-权限串-角色-用户管理-api.md`），发生在 `contract-v1`（CF-1）与 `migrations-m1`（CF-2）之后：M1 冻结范围（`000001`–`000013`、61 张表）不动，仅追加新迁移 `000018`；OpenAPI 只做**加法**（可选字段、新增 200 响应、补充 409）。

- **日期**：2026-11-21
- **基线标签**：contract-v1
- **影响范围**：
  1. 新增迁移 `migrations/000018_users_permission_version.sql`：`ALTER TABLE users ADD COLUMN permission_version BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER totp_last_step`；`migrations/CHECKSUMS` 追加登记；`docs/database/platform-identity.md`（users 列表）、`docs/database/README.md §8.1` 追加说明。`000001`–`000017` 一字未改；无新表，`scripts/gates/data/*.txt` 无需登记。
  2. `docs/api/openapi.yaml`（仅加法）：
     - `UserCreate.initial_password`：由必填改为可选（省略时服务端生成一次性口令），更新 description；
     - `User` 新增可选只读属性 `temporary_password`（仅 `createUser` 的 201 响应且口令由服务端生成时出现一次，响应 `Cache-Control: no-store`）；
     - `resetUserPassword`：请求体 `new_password` 由必填改为可选；新增 `200` 响应（新 schema `TemporaryPassword{temporary_password}`，服务端生成口令时），原 `204`（管理员设定口令）保留；
     - `setUserRoles`、`setRolePermissions` 补充 `409`（引用既有 `Conflict`：最后一个超级管理员不可降权 / 内置角色不可变）。
     错误码、`x-*` 扩展、proto：无改动。
- **是否破坏**：否（新增带默认值的列；请求侧只是放宽必填；响应侧只新增可选字段与新增状态码；旧客户端继续传 `initial_password` / `new_password` 的行为不变。`make openapi-breaking` 相对 `contract-v1` 无破坏性变更）
- **关联 ADR**：N/A（非破坏；依据 ADR-018 §8、§10、§12，ADR-025）
- **变更动机**：
  1. **`permission_version`**：ADR-018 §8 规定「权限集缓存键含 `permission_version`，变更时递增并经 Redis pub/sub 清缓存，缓存失效兜底靠短 TTL」，AC4 要求「权限变更在 TTL 内最终生效」。M1 的 `users` 没有该列（`docs/database` 未定义），所以版本号无处可存。方案对比：(a) `users.permission_version`（采用）——认证钩子本来每个请求都读 `sessions ⋈ users`，版本号随同一行免费取得；缓存键含它使旧版本缓存项即刻不可达，变更立即生效而不依赖广播；角色权限变更时一条 `UPDATE users JOIN user_roles … SET permission_version = permission_version + 1` 波及全部持有者；(b) 全局单一版本号（`system_settings`）——任何权限变更都使全体用户缓存失效，且每个请求多一次读取；(c) 只靠 TTL，不要版本号——变更最长陈旧 60 秒，违背「撤销权限应尽快生效」的安全期望。
  2. **口令由服务端生成**：契约原设计是创建者/管理员手工设定口令并在带外转交。Story 1.18 要求「创建/重置口令强制首次登录修改、一次性明文只回显一次、不入日志」：由服务端 CSPRNG 生成并一次性回显，避免人为选择弱口令、避免口令经聊天/邮件转交；原有「由创建者设定」路径保留（兼容），两种路径都强制首次登录改密（`must_change_password=1`）。
  3. **补充 409**：最后一个超级管理员保护与内置角色不可变都以 `409 resource.conflict` 返回，原 operation 未声明该状态码（与 CC-4/CC-5 同类的契约遗漏）。
- **兼容与迁移方案**：
  1. `000018` 为 `expand`、无 Down；新列 `NOT NULL DEFAULT 0`，对已存在行与旧二进制的 INSERT 安全（迁移 lint 的「expand 不得新增无默认值的 NOT NULL 列」规则满足）；`users` 已在 `000001` 对 `lingedge_app` 授予表级授权，新列自动适用。
  2. 旧客户端：仍可在 `createUser` 传 `initial_password`、在 `resetUserPassword` 传 `new_password`（行为同前：创建后/重置后强制改密；管理员设定口令的重置仍返回 `204`）；不识别 `temporary_password` 的客户端忽略该字段即可。并行泳道（前端）需据此实现「一次性口令展示」界面，属新增能力而非返工。
  3. **幂等记录脱敏**：`createUser` 声明了 `Idempotency-Key`；幂等记录会把响应体存进 MySQL，因此服务端生成口令时，handler 经 `httpx.SetReplayBody` 登记**不含 `temporary_password` 的响应体**——同键重放返回同一用户但不再回显口令，口令明文不入库（`tests/integration/auth` 的 `TestAdminUsersLifecycle` 扫描 `idempotency_keys`）。
  4. M1 冻结口径：「M1 = 000001–000013、61 张表」不变；`freeze-check` 不变；`000014`–`000018` 为追加迁移。
- **验证**：`make migrate-test`（真实 MySQL 全量迁移含 000018）、`make migrations-check`（相对 `migrations-m1` 只新增）、`make openapi-check`（含 `openapi-breaking` 相对 `contract-v1` 无破坏性变更）、`make contract-check` 全绿；`tests/integration/auth`（16 个管理 operation 的响应经 kin-openapi 校验：createUser 201/400/409、resetUserPassword 200/204/400、setUserRoles/setRolePermissions 的 409 等）。
- **评审**：开发代理（bmad-build）依据机器验证出具，2026-11-21；未经人工签收，用户可事后推翻（推翻 (1) 则以新迁移删除该列并改用「仅 TTL」；推翻 (2) 则以新的契约变更把 `initial_password` 恢复为必填）。
