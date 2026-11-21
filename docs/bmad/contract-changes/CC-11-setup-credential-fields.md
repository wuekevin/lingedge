# 契约变更说明 CC-11：安装向导数据库凭据字段（`SetupDbConfig`/`SetupStepResult` 加法）、`setup.insecure_transport`、`secrets.runtime_credentials_file`；无迁移变更

> 用法与格式见 `_template.md`。对应 AA-7 的架构定稿（ADR-026，UD-31），将由 Story 1.24 实现；发生在 `contract-v1`（CF-1）与 `migrations-m1`（CF-2）之后。**本变更没有任何迁移改动**（冻结迁移 000001–000018 一字未改），**proto 一字未改**。

- **日期**：2026-11-21
- **基线标签**：contract-v1
- **影响范围**：
  1. `docs/api/openapi.yaml`（仅加法，均为可选属性）：`components.schemas.SetupDbConfig` 新增 `admin_username`、`admin_password`（临时管理员，建账号模式）、`migrate_username`、`migrate_password`、`maint_username`、`maint_password`（均可选；`required` 列表不变）；`username`/`password` 的 description 澄清为运行期 `lingedge_app` 账号。`components.schemas.SetupStepResult` 新增只读可选属性 `one_time_secrets`（`{migrate_password}`），仅 `setupComplete` 在迁移口令由服务端自动生成时返回一次。所有 operation、`x-*` 扩展、`required` 与响应码集合不变。
  2. 错误码：新增 `setup.insecure_transport`（400，经既有 `BadRequest` 响应；携带管理员/迁移/DB/Redis 口令的向导接口，在监听非回环地址且请求非 TLS 时拒绝）。OpenAPI 未枚举各业务错误码（沿用 `BadRequest`），i18n 键 `errors.setup.insecure_transport` 随 1.24 登记。
  3. 配置（非 OpenAPI/proto，因 Story 1.15 已约定定稿变更走本流程而登记）：`server.yaml` 新增可选键 `secrets.runtime_credentials_file`（缺省 `<server.data_dir>/secrets/runtime-credentials.json`）；`database.maint.*`、`database.password_file`、`redis.password_file` **保留不变**（作为显式覆盖通道）。
  4. 新文件格式（内部，非契约面）：`runtime-credentials.json`（v1）与 `/etc/lingedge/server/migrate.yaml`（见 ADR-026 §3/§4）；`--from-file` YAML 的 `database.admin/migrate/app/maint`、`redis` 字段（tools-design §13）。
  5. 迁移、proto：无改动。
- **是否破坏**：否（新增可选请求属性、新增只读可选响应属性、新增错误码；无字段删除/改名/收紧）
- **关联 ADR**：N/A（非破坏；决策见 ADR-026，推翻影响见其「可被推翻处」）
- **变更动机**：原契约的 `SetupDbConfig` 只有单一账号的 username/password，无法表达「临时管理员建三类账号」「迁移账号口令（仅内存）」「维护账号口令」，也无法在完成时一次性返回自动生成的迁移口令——而 tools-design §13 与 AC 早已要求这些能力（AA-7）。
- **兼容与迁移方案**：旧客户端（仅发送原有字段）行为不变：视为「预建账号模式」且要求提供迁移凭据的校验放在服务端（缺失返回 400）；该接口在本变更前尚无实现（1.24 未开工），无存量客户端。不升 `contract-vN`。
- **验证**：`make openapi-check`、`make contract-check`（含 `openapi-breaking` 相对 `contract-v1` 无破坏，见提交时的实际输出）；实现与响应的 kin-openapi 校验随 Story 1.24。
- **评审**：架构代理（bmad-method:bmad-architecture）依据机器验证出具，2026-11-21；未经人工签收，用户可事后推翻（见 ADR-026「可被推翻处」；推翻后缩减属性属收紧，须走 ADR-017 §10 流程，故在 1.24 实现前推翻代价最低）。
