# 契约变更说明 CC-10：`SigningKey` 响应补全公钥/指纹/到期与 `grpc_server`；keyset canonical JSON 字段编码；新增设置键 `grpc.tls.san_extra`（加法）；无迁移变更

> 用法与格式见 `_template.md`。对应 Story 1.23（`docs/bmad/stories/1-23-签名与-ca-密钥初始化-信任域②③.md`），发生在 `contract-v1`（CF-1）与 `migrations-m1`（CF-2）之后。**本变更没有任何迁移改动**（冻结迁移 000001–000018 一字未改）：`signing_keys`（`000005`）已有 `purpose` CHECK（含 `grpc_server`）、`status` CHECK、生成列 `active_key` 上的唯一索引 `uk_signing_keys_active`（「每个 purpose 至多一个 active」由数据库保证）、`cert_pem` 与 `lingedge_app` 的表级 `SELECT, INSERT, UPDATE, DELETE`；`system_settings`（`000002`）app 账号 CRUD、`audit_logs`（`000003`）app 账号 INSERT；`system:read` 权限串（`000004` 种子）——初始化只需 `lingedge_app`，**不需要也不接受 migrate/maint 账号**，因此对 AA-7（安装向导的迁移凭据交接，未决）没有任何依赖，也没有私自定稿。

- **日期**：2026-11-21
- **基线标签**：contract-v1
- **影响范围**：
  1. `docs/api/openapi.yaml`（仅加法）：`components.schemas.SigningKey`——① `purpose` 枚举补 `grpc_server`（迁移 CHECK 与 AC1 早已包含，契约漏列；`GET /admin/system/signing-keys` 在初始化后必然返回该用途的行）；② 新增属性 `public_key`（标准 base64：ed25519 为 32 字节原始公钥，ecdsa_p256 为 PKIX DER）、`fingerprint`（小写十六进制 SHA-256：config_signing 为原始公钥摘要，mtls_ca/grpc_server 为证书 DER 摘要，CA 的即 enroll Token 的 `ca_fp`）、`not_after`（证书到期，可空）；③ `required` 增加 `algorithm`、`public_key`、`fingerprint`（**响应**必填项增加，对客户端是加法）。`x-permissions/x-tenant-scope/x-audit/x-phase` 与 operation 本身不变。契约原有文字「仅元数据；私钥永不经 API 返回」不变且由类型保证（响应结构体没有任何私钥/密文字段）。
  2. `docs/api/node-protocol.md` §8（澄清，非变更）：`canonical_json(version,keys,issued_at,previous_version)` 在原文未给出字段编码——补充固定形态（键按字典序、无空白、UTF-8、整数无小数点；`issued_at`/`not_before` 为 UTC RFC 3339 纳秒去尾零；`public_key` 标准 base64；`status` 用 proto 枚举名 `ACTIVE|RETIRING|REVOKED`；`keys[]` 按 `key_id` 升序），以及 keyset 链的验证规则（版本严格连续、签署者须为上一版 ACTIVE/RETIRING、保留 key 的公钥不得改变、REVOKED 不得复活）。**proto 文件一字未改**（`proto-check` 不受影响）；该澄清使 Server（`pkg/signing`）与 Agent 的实现不会因各自理解字段编码而互不兼容。
  3. 新增系统设置键 `grpc.tls.san_extra`（`docs/database/platform-identity.md` §1 早已把它列为跨文档契约键名，类型/校验/再认证此前未定义）：主机名/IP 字面量列表，可为空，最多 32 项，规范化后存 `system_settings.value_json`；经既有 `PATCH /admin/settings` 修改（该 operation 的 `x-permissions`/`x-reauth` 不变，写入此键由设置服务按键判定再认证，与 1.22b 的敏感键分级同口径）。OpenAPI 的 `getSettings`/`patchSettings` 本就是开放键值（按注册表校验），无 schema 变更。
  4. 审计：新增系统发起的审计动作 `system.signing_keys.init`（`audit.SystemActions`，不属于任何 operation 的 Catalog；门禁 `gates audit` 新增 `audit-system-catalog`/`audit-system-unrecorded` 规则与负向样本）。`audit_logs.action` 是自由字符串列，无 schema 变更。
  5. 迁移、proto：无改动。
- **是否破坏**：否（枚举值补全、可选/新增响应属性、响应必填项增加；`make openapi-breaking` 相对 `contract-v1` 无破坏性变更）
- **关联 ADR**：N/A（非破坏；依据 ADR-002 §3/§11/§12、ADR-003 §3、ADR-012 §2）
- **变更动机**：AC4 要求 `GET /admin/system/signing-keys`「仅返回公钥与指纹」，而契约的 `SigningKey` 既没有公钥/指纹字段也漏了 `grpc_server`；节点与 Server 需要对 keyset 签名输入的字节形态达成无歧义的一致（协议只写了函数名没写字段编码）；Server 叶证书 SAN 依赖的 `grpc.tls.san_extra` 没有类型定义。
- **兼容与迁移方案**：无迁移；已有客户端不受影响（只新增字段与枚举值；该接口在本 Story 之前无实现）。
- **验证**：`make openapi-check`（含 `openapi-breaking` 无破坏、M1 未实现清单不含 `listSigningKeys`）、`make contract-check`、`make audit-check`、`make perms-check`、`tests/integration/signingkeys`（响应经 kin-openapi 校验）。
- **评审**：开发代理（bmad-method:bmad-build）依据机器验证出具，2026-11-21；未经人工签收，用户可事后推翻（推翻 `public_key/fingerprint` 以新的契约变更删除属性，属收紧，须走 ADR-017 §10 流程）。
