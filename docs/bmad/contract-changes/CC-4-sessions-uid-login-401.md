# 契约变更说明 CC-4：sessions 追加对外引用 `uid`；`login` 补充 401 响应；`changePassword` 描述与 AC 对齐

> 用法与格式见 `_template.md`。对应 Story 1.16（`docs/bmad/stories/1-16-认证-i-登录-会话-csrf-登录节流-登录日志.md`），发生在 `contract-v1`（CF-1）与 `migrations-m1`（CF-2）之后：M1 冻结范围（`000001`–`000013`、61 张表）不动，仅追加新迁移 `000015`；OpenAPI 只做**加法**与描述修订。

- **日期**：2026-11-21
- **基线标签**：contract-v1
- **影响范围**：
  1. 新增迁移 `migrations/000015_sessions_uid.sql`：`ALTER TABLE sessions ADD COLUMN uid CHAR(26) NULL AFTER id, ADD UNIQUE KEY uk_sessions_uid (uid)`；`migrations/CHECKSUMS` 追加登记；`docs/database/platform-identity.md` §2.7 与 `docs/database/README.md` §8.1 追加说明。`000001`–`000014` 一字未改。
  2. `docs/api/openapi.yaml`：`POST /auth/login`（`login`）补充 `'401'` 响应（引用既有 `Unauthorized`）并在 description 写明「凭据无效统一 401 auth.invalid_credentials」；`POST /auth/password/change`（`changePassword`）的 `PasswordChangeRequest` 描述由「吊销其余会话」改为「吊销全部会话（含当前）」（与 Story 1.16 AC5「修改密码使该用户全部会话失效」及 ADR-018 §2「密码修改…使全部会话失效」一致）。
  3. 错误码、其他 operation、schema、proto：无改动。
- **是否破坏**：否（新增响应码、新增可空列与索引、描述文字修订；不删除/改名/收紧任何既有项）
- **关联 ADR**：N/A（非破坏；依据 ADR-018 §2/§6、ADR-017 §6）
- **变更动机**：
  1. **sessions.uid**：openapi 的 `Session.session_ref` 与路径参数 `{sessionId}`（`DELETE /auth/sessions/{sessionId}`）的类型是 `Uid`（26 位 ULID，「不暴露内部自增 id」，ADR-017），但 `sessions` 表（database/platform-identity.md §2.7，C2）没有 `uid` 列，只有自增 `id` 与令牌哈希。直接暴露 `id` 违反 ADR-017；暴露令牌哈希等于把凭据的等价物放进列表响应。方案对比：(a) 加 `uid` 列（采用）——与全库 `uid` 约定一致、无状态、可索引；(b) 由 `id` 加服务端 MAC 推导 26 位编码——无需迁移，但引用可被解码出自增 id、需要额外的服务端密钥管理，且无法在库里按引用排查。
  2. **login 401**：Story 1.16 AC4/AC7 要求登录失败（用户不存在 / 口令错误 / realm 不符 / 账号停用）统一返回 `401 auth.invalid_credentials`（`apierr` 错误码表与 `docs/api/README.md` §5 已定义），但 `login` 的 `responses` 只声明了 200/400/429/500，契约响应校验会把 401 判为「未声明的状态码」。这是契约遗漏而非行为变更。
  3. **changePassword 描述**：原描述（其余会话）与 AC5/ADR-018 §2（全部会话）矛盾；以 AC/ADR 为准（也更安全：改密后任何持有旧会话的设备都必须重新登录）。
- **兼容与迁移方案**：
  1. 新迁移 `expand`、无 Down、`ADD COLUMN … NULL` + 唯一索引（多个 NULL 不冲突，对已存在行安全）。迁移 lint 通过（`make migrate-test`）。应用写入路径永远写 `uid`；读取时 `COALESCE(uid,'')` 容忍升级前遗留的无 uid 行（此前没有任何代码创建过会话，实际不会有）。授权无需变更：`sessions` 已在 000001 对 `lingedge_app` 表级授予 SELECT/INSERT/UPDATE/DELETE。
  2. OpenAPI 为加法：旧客户端不受影响（401 本来就会被它们收到，只是契约没有写）。
  3. **M1 冻结口径**：「M1 = 000001–000013、61 张表」不变；`freeze-check` 不变；`000014`、`000015` 为追加迁移。
  4. 并行泳道返工：无。前端按 `Session.session_ref`（ULID）使用即可。
- **验证**：`make migrate-test`（真实 MySQL 全量迁移含 000015）、`tests/integration/auth`（会话 uid 写入/按 uid 吊销/他人 404、登录 401 与 429 的 kin-openapi 响应契约）、`make migrations-check`（相对 `migrations-m1` 只新增）、`make openapi-check`/`make contract-check` 全绿（见 Story 1.16 开发记录）。
- **评审**：开发代理（bmad-build）依据机器验证出具，2026-11-21；未经人工签收，用户可事后推翻（推翻 (1) 则以新的 expand/contract 迁移删除该列并改用 MAC 方案；推翻 (3) 则恢复「其余会话」并同步 AC5）。
