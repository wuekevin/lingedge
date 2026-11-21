# 契约变更说明 CC-12：安装向导的管理员 TOTP/语言、系统时区与新增错误码（均为加法）；新设置键与配置键；无迁移变更

> 用法与格式见 `_template.md`。由 Story 1.24 实现 CC-11（AA-7，ADR-026）时发现的、tools-design §13 已要求而契约未表达的缺口。发生在 `contract-v1`（CF-1）与 `migrations-m1`（CF-2）之后。**没有任何迁移改动**（冻结迁移 000001–000018 一字未改；向导进度存于 `system_installations.meta_json`，该列已存在），**proto 一字未改**。

- **日期**：2026-11-21
- **基线标签**：contract-v1
- **影响范围**：
  1. `docs/api/openapi.yaml`（仅加法，均为可选属性）：`components.schemas.SetupAdminCreate` 新增 `locale`（zh-CN|en）、`enable_totp`（bool）、`totp_code`（6 位数字）——tools-design §13 步骤 5「可选立即绑定 TOTP」；`components.schemas.SetupSystemConfig` 新增 `timezone`（IANA 名）——步骤 6「时区」；`components.schemas.SetupStepResult.one_time_secrets` 在 CC-11 的 `migrate_password` 之外新增只读可选属性 `totp_secret`、`totp_otpauth_uri`、`recovery_codes`（仅 `setupCreateAdmin` 绑定 TOTP 时返回一次，`Cache-Control: no-store`）；`SetupStatus.installed` 的描述由「一律 404」更正为「一律 410 setup.already_installed」（与 `Gone` 响应、tools-design §13 一致，文字更正，无行为/形状变化）。所有 operation、`x-*` 扩展、`required` 与响应码集合不变。
  2. 错误码（`docs/api/README.md` §5，经既有 `BadRequest`/`ServiceUnavailable` 语义返回；OpenAPI 未枚举各业务错误码）：新增 `setup.database_not_empty`（400，目标库非空，FR-6）、`setup.precheck_failed`（400，MySQL/Redis 预检未通过，`details` 说明原因与处置）、`setup.mode_active`（503，setup mode 下除 `/setup` 与 `/public/setup/*` 外的接口）。`setup.insecure_transport` 已由 CC-11 登记。i18n 键 `errors.setup.*` 同名派生。
  3. 系统设置键（`system_settings`，非 OpenAPI 契约面，设置注册表加法）：`general.base_url`（Server 公开 URL，写入需再认证）、`general.timezone`（缺省 `UTC`）。`PATCH /admin/settings` 的键集合因此增加两项（加法）。
  4. 配置（非 OpenAPI/proto）：`server.yaml` 新增可选键 `secrets.setup_token_hash_file`（setup token 哈希文件路径，缺省 `/etc/lingedge/server/setup-token.hash`，仅 setup mode 读取）与可选键 `database.tls`（`true|false|skip-verify|preferred`，缺省空；非回环主机必须显式设置，与 `db.Config` 既有校验一致）——运行期凭据文件的 `database.tls` 缺省提供。`secrets.runtime_credentials_file` 已由 CC-11 登记。
  5. 审计动作（`audit.Catalog`，内部）：新增 `setup.checks`、`setup.database`、`setup.redis`、`setup.initialize`、`setup.admin`、`setup.system`、`setup.keys`、`setup.complete`（`/public/setup/*` 各 `x-audit: true` operation 的审计动作，Actor 为系统组件 `setup`）。
  6. 迁移、proto：无改动。
- **是否破坏**：否（新增可选请求属性、新增只读可选响应属性、新增错误码、新增设置键/配置键；无字段删除/改名/收紧）
- **关联 ADR**：N/A（非破坏；决策见 ADR-026）
- **变更动机**：tools-design §13 已要求向导「可选绑定 TOTP」「站点名称、公开 URL、运行模式、时区」，但契约里的 `SetupAdminCreate`/`SetupSystemConfig` 无法表达 TOTP 与时区，`one_time_secrets` 也只覆盖迁移口令；向导进度（`system_installations.state`）在冻结的 000002 里没有独立列，而 `meta_json` 足以承载（只存步骤名/布尔/版本号/公开配置，不含任何机密），因此无需新迁移。
- **兼容与迁移方案**：旧客户端（只发送原有字段）行为不变：管理员步骤不绑定 TOTP、系统步骤不设时区（缺省 UTC）。`/public/setup/*` 在本变更前尚无实现，无存量客户端。不升 `contract-vN`。
- **验证**：`make openapi-check`（含运行时路由清单 ⊆ openapi、元数据一致、相对 `contract-v1` 无破坏性变更，见提交时的实际输出）、`make contract-check`、`make migrations-check`（迁移集合不变）。实现与响应形状的集成测试见 `tests/integration/setup`。
- **评审**：开发代理（bmad-method:bmad-build）自检，2026-11-21；未经人工签收。
