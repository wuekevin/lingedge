---
title: 'Story 1.17：认证 II：TOTP / 恢复码 / 近期 MFA（含 L4 再认证）'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'ef3182d'
context:
  - docs/bmad/epics.md（Story 1.17，1216–1238 行）
  - docs/architecture/adr/ADR-018-authn-authz.md §5、§12；ADR-011（信封加密）；ADR-025（Redis 边界）
  - docs/api/openapi.yaml 的 /auth/login（status=totp_required）、/auth/totp/*、/auth/reauth 及 x-reauth operation；docs/bmad/decisions-adopted.md（UD-29）
  - docs/database/platform-identity.md（users.totp_*、user_recovery_codes、sessions.mfa_verified_at/reauth_at、system_settings）
  - docs/bmad/stories/1-11…1-16-*.md；pkg/secretbox；internal/auth；internal/platform/{httpx,server}
---

## Intent

**Problem:** 1.16 对启用了 TOTP 的账号只做了临时 fail-closed（`401 auth.totp_required`，无法登录）；TOTP 启停、恢复码、第二步登录、近期 MFA、强制管理员 TOTP 与 L4 再认证（`x-reauth`）都不存在，口令泄露时账号没有第二层保护，高危操作也没有 step-up。

**Approach:** 在 `internal/auth` 内实现完整的二步认证与再认证：`POST /auth/totp/{setup,enable,disable,verify}`、`POST /auth/reauth`；登录第一步按契约返回 `200 {status: totp_required, mfa_token}`（替换临时 fail-closed）；TOTP 密钥经 `pkg/secretbox` 信封加密；已用 time-step 与一次性 `mfa_token` 持久化在 MySQL（多实例一致、Redis 关闭下仍工作）；近期 MFA / 强制管理员 TOTP 在认证钩子统一执行；`x-reauth` 检查做成可被 1.18 复用的组件 `httpx.ReauthChecker`，经 `httpx.WithReauth` 在授权链中统一生效。顺手接入 1.16 遗留：server 优雅停机调用 auth `Module.Flush`。

## 核对结论：表结构与授权 + 两个追加迁移 + 一份契约变更说明（CC-5）

- 已有且足够：`users.totp_enabled/totp_secret_enc`（含 CHECK）、`user_recovery_codes(user_id, code_hash UNIQUE, used_at)`、`sessions.mfa_verified_at/reauth_at`、`login_logs.event` 含 `totp_failed`；`lingedge_app` 对它们均有表级 `SELECT, INSERT, UPDATE, DELETE`（`users.totp_secret_enc` 也已在 `secrets.rewrap` 白名单）。
- **缺口 1（防重放）**：AC2「同一验证码在窗口内重放被拒」需要持久化的已用 time-step，`users` 没有该列 → `migrations/000017_users_totp_last_step.sql`（`ALTER TABLE users ADD COLUMN totp_last_step BIGINT UNSIGNED NULL`；原子 `UPDATE … WHERE totp_last_step IS NULL OR totp_last_step < ?`，受影响行数为 0 即重放）。
- **缺口 2（mfa_token）**：契约 `LoginResponse.mfa_token` 是「5 分钟、一次性」令牌，且「Redis 关闭下仍工作」是硬性验收 → `migrations/000016_totp_mfa.sql` 新增 `login_mfa_challenges`（只存令牌 SHA-256；`attempts` 限次；`consumed_at` 一次性；逐表授权给 `lingedge_app`）。方案对比见 CC-5（无状态签名令牌无法保证一次性/限次且要新增 MAC 密钥管理；塞进 `sessions` 会让半登录行出现在会话表里，否决）。
- **缺口 3（契约响应）**：`verifyTotp` 的 `401 auth.totp_invalid`、`setup/enable/disable` 的 `409 resource.conflict` 在 OpenAPI 中未声明（与 CC-4 对 `login` 同类的遗漏）→ 仅加法补充响应码与描述。
- 冻结迁移 `000001`–`000015` 一字未改；`make migrations-check`（相对 `migrations-m1`）与 `make openapi-check`（相对 `contract-v1`）均通过；记录在 **`docs/bmad/contract-changes/CC-5-totp-mfa-schema.md`**；`docs/database/*`、ADR-018 已同步（`login_mfa_challenges` 登记为平台表 `platform-tables.txt`，不属 M1 的 61 表）。

## Code Map

- `internal/auth/internal/service/totp.go` -- TOTP（pquerna/otp v1.5.0）、恢复码、`checkSecondFactor`、两步登录（`issueMFAChallenge`/`VerifyLogin`）、setup/enable/disable、`Reauth`、策略检查（`EnrollmentRequired`/`CheckRecentMFA`/`CheckReauth`）、统一的失败处理 `failAttempt`（计入节流 + 写 `login_logs`）。
- `internal/auth/internal/service/service.go` -- `finishLogin` 拆为「第一步（发挑战）」与 `issueSession`（两步登录与一步登录共用，含会话固定防护与 `mfa_verified_at`）；`Options.Box`。
- `internal/auth/internal/service/settings.go` -- 新增设置：`auth.reauth_window_s`（默认 300，UD-29）、`auth.mfa_window_s`（默认 900，[ASSUMPTION]）、`login.totp_required_for_admin`（默认 false）。
- `internal/auth/internal/repository/totp.go` -- users TOTP 状态/防重放 CAS、恢复码（替换/单次使用）、mfa 挑战、会话 MFA/再认证时间；全部 SQL 为常量 + 占位符。
- `internal/auth/internal/handler/{handler,authenticator,reauth,module}.go` -- 5 条新路由（元数据与 openapi 一致）；认证钩子统一执行「强制管理员 TOTP」与「近期 MFA」；`reauthChecker`（实现 `httpx.ReauthChecker`）。
- `internal/platform/httpx/reauth.go` -- `ReauthChecker` + `WithReauth`：授权链里统一执行 x-reauth。
- `internal/platform/server/server.go` -- `Deps.Secrets`（信封加密）与 `Deps.OnShutdown`（停机钩子：监听排空之后、MySQL/Redis 关闭之前）。
- `cmd/lingedge-server/assembly.go` -- 装配：`Box`、`OnShutdown` 登记 `Module.Flush`、`WithReauth`。
- `migrations/000016_totp_mfa.sql`、`000017_users_totp_last_step.sql`、`docs/bmad/contract-changes/CC-5-*.md`。
- `pkg/redact/redact.go` -- 脱敏键扩展 `recovery_code(s)`（G-5 的 TOTP 场景测试发现自由文本里 `recovery_code=…` 与 JSON 数组形态的恢复码此前不被遮蔽）。

## 关键实现点

- **TOTP（AC1）**：pquerna/otp v1.5.0（SHA-1/6 位/30 秒，20 字节随机密钥，所有主流验证器通用）。`setup` 生成密钥并以「待确认」状态加密存库（`totp_enabled` 仍为 0），响应里的 `otpauth_uri`/`secret_masked` 是密钥唯一一次出现；`enable` 用首次验证码确认后启用并一次性返回 10 个恢复码。**信封加密**：Base32 种子经 `pkg/secretbox` 存 `users.totp_secret_enc`，AAD=`users|totp_secret_enc|<用户 uid>`（密文挪到别的行解密即失败，测试断言）。已启用再 `setup`/`enable` → `409`，不会覆盖密钥。仅 Cookie 会话可用（Bearer → 403）。
- **恢复码（AC1）**：10 个、各 80 位 CSPRNG（Base32，16 字符分 4 组展示）。库内 `SHA-256("lingedge/recovery/v1\0"+规整码)`。**哈希方式的理由**：恢复码本身是高熵随机数（2^80），离线穷举不可行，慢哈希（argon2）只会给登录路径增加延迟而不增加安全性；确定性哈希才能按 `(user_id, code_hash)` 唯一索引原子地「用掉」一个码（加盐的慢哈希会迫使逐行比对）。单次有效：`UPDATE … SET used_at WHERE … AND used_at IS NULL`；大小写/连字符/空白容错。停用或重新启用时整批删除/替换。
- **两步登录（AC2）**：`login` 口令正确且已启用 TOTP → `200 {status: totp_required, mfa_token}`，**不建立会话、不下发 Cookie、不清零失败计数、不写 login_success**；`POST /auth/totp/verify`（公开路由）凭 `mfa_token` + 6 位码/恢复码建立会话（`mfa_verified_at=now`，`login_success.reason=totp|recovery_code`，沿用会话固定防护与 realm/租户判定）。`mfa_token`：32 字节 CSPRNG、库存哈希、5 分钟、一次性、同一挑战至多 5 次验证码尝试，过期/已用/超限一律 `401 auth.unauthenticated`（须重走第一步）。缺失验证码 → 400；错误/重放/已用恢复码 → `401 auth.totp_invalid`，**计入 1.16 的账号+IP 节流**（超限 `429` 含 `Retry-After`，锁定期间连正确验证码也 429）并写 `login_logs.totp_failed`（reason：`bad_code`/`replayed`/`bad_recovery_code`）。
- **防重放与漂移（AC2）**：验证码匹配 ±1 个 time-step（`hotp.ValidateCustom` 逐步比对）；匹配的 step 经 `UPDATE users SET totp_last_step=? … WHERE totp_last_step IS NULL OR totp_last_step < ?` 原子记录——step 必须严格递增，因此同一验证码重放、窗口内更早的验证码都被拒；多实例一致，不依赖 Redis。时间一律经注入的 `clock.Clock`（测试用可推进时钟验证 ±1 通过、±2 拒绝）。
- **近期 MFA（AC3）**：会话的 `mfa_verified_at` 在两步登录、`enable`、含验证码的 `reauth` 时写入。改密码、创建 API Token（`createApiToken`，路由属 1.19，注册后由同一钩子生效）要求「已启用 TOTP 的账号 `now - mfa_verified_at ≤ auth.mfa_window_s`」，否则 `403 auth.reauth_required`（`details.field=mfa_verified_at`，经 `POST /auth/reauth` 刷新；**不新增错误码**）。未启用 TOTP 的账号无第二因子可验证，不要求（由 AC4 的强制策略引导其启用）；Bearer 请求没有会话，永远无法满足。窗口默认 900 秒——**基线与 UD 均未规定该值，属 [ASSUMPTION]**，可配置。
- **强制管理员 TOTP（AC4）**：系统设置 `login.totp_required_for_admin=true` 时，未启用 TOTP 的平台用户登录成功，但认证钩子（与 `must_change_password` 同构，统一执行）只放行 `getMe/getCsrfToken/logout/setupTotp/enableTotp/changePassword`，其余 `403 auth.forbidden`（`details.field=totp_required_for_admin`）；启用后恢复访问；策略开启期间平台用户不可停用（`403`）。租户用户不受影响。
- **停用（AC5）**：需口令 + 当前验证码（TOTP 或恢复码）；错误计入节流并写日志；成功清除密钥/time-step/恢复码；未启用 → `409`。**审计**：`audit_logs` 的统一接入属 Story 1.21——此处**没有**写 `audit_logs`，只写结构化日志（`TOTP 已启用/已停用/会话已再认证`，含用户 uid 与 request_id，不含任何秘密）与失败的 `login_logs`；登记为「待 1.21 补写 audit_logs」（见遗留）。 【Story 1.21 已收口：审计记录器 internal/platform/audit 在业务事务内写 audit_logs，过渡的结构化日志与 adminAuditor 已移除；见 1-21 说明】
- **L4 再认证（AC6）**：`POST /auth/reauth` 以口令（已启用 TOTP 时另需 6 位 `totp_code`，恢复码不可用于再认证；缺失验证码 → `401 auth.totp_required` 且不计失败）刷新 `sessions.reauth_at`（含验证码时同时刷新 `mfa_verified_at`）；失败计入节流并写日志；响应 `{reauth_at, expires_at}`。窗口 `auth.reauth_window_s`（默认 300，UD-29，可配置），边界 `now - reauth_at ≤ 窗口`。**只有 Cookie 会话能再认证**：Bearer 请求（`Bearer=true` 的主体、或非本模块产出的任何主体）一律不能满足（单测 + 集成测试）。
- **x-reauth 在授权链中的位置**：`httpx.WithReauth(hooks, checker)` 在授权钩子之前对 `meta.Reauth` 路由运行 `ReauthChecker`，失败即 `403 auth.reauth_required` 并中止（不进内层授权钩子、handler 不执行）；通过后继续走内层授权钩子（1.18 之前仍拒绝 → `auth.forbidden`，所以再认证不会放行未授权操作）。把「再认证先于授权」写明在代码注释里：1.18 交付真实授权钩子后可改为「先权限后再认证」（只需把 checker 移入授权钩子）。契约中声明 `x-reauth` 的 operation **实为 11 个**（`scripts/contract/data/x-reauth.txt` 已注明以契约为准；epics/修订日志的「10 个」是旧口径）；这些路由目前都未注册（属 1.18+），故集成测试按 openapi 的 x-reauth 元数据注册占位路由，对全部 11 个驱动验证（窗口外 403、窗口内越过、边界、Bearer）。
- **1.16 遗留：停机 Flush**：`server.Deps.OnShutdown` 登记停机钩子，在「监听排空、角色停止」之后、「MySQL/Redis/主密钥关闭」之前按相反顺序执行；`assembly.buildHooks` 登记 `Module.Flush`，异步的 DB 兜底失败计数在 MySQL 关闭前落库。`Deps.Secrets` 同时把已加载的主密钥封装为 `secretbox.Box` 交给模块。

## 默认值与口径（[ASSUMPTION] 标注）

| 参数 | 默认 | 说明 |
|---|---|---|
| `auth.reauth_window_s` | 300 秒 | **UD-29 采纳值**（可配置；改默认值为配置改动） |
| `auth.mfa_window_s` | 900 秒（15 分钟） | **[ASSUMPTION]**：基线与 UD 均未规定「近期 MFA」窗口；可配置（范围 30 秒–24 小时，越界回退默认并告警） |
| `login.totp_required_for_admin` | false | 契约已定义的设置键；JSON 布尔 |
| `mfa_token` 有效期 / 尝试上限 | 5 分钟 / 5 次 | 5 分钟来自契约；5 次为工程取值（另有账号+IP 节流兜底） |
| TOTP 参数 | SHA-1、6 位、30 秒、±1 窗 | 验证器通用参数；±1 来自验收标准 |
| 恢复码 | 10 个 × 80 位 | 工程取值 |

## Tasks & Acceptance

- [x] AC1 `TestTOTPSetupEnableEncryptsSecretAndIssuesRecoveryCodes`（setup/enable 契约校验；`totp_secret_enc` 为 5 段信封密文且不含种子；AAD 绑定：错误 uid 解密失败；错误验证码 401 + `totp_failed`；10 个恢复码格式/唯一；库内仅 64 位十六进制哈希、无明文；已启用 409 且不覆盖密钥；缺 CSRF/未认证）。
- [x] AC2 `TestTOTPLoginRequiresSecondFactor`（仅口令无会话、缺码 400、错误码 401、`totp_failed` 日志、正确码成功且 Cookie 属性/`mfa_verified_at`）、`TestTOTPReplayRejected`（同码重放/更早窗口被拒、`totp_last_step` 持久化、下一窗口可用）、`TestTOTPDriftWindow`（±1 通过、±2/±3/−5 拒绝，可注入时钟）、`TestRecoveryCodeSingleUse`（登录后立即失效，含大小写/连字符变体；其余码可用；未发放的码拒绝）、`TestMFAChallengeLimits`（一次性/5 次上限/5 分钟过期/未知与畸形令牌/库内仅哈希）、`TestTOTPFailuresCountTowardThrottle`（401×3 → 429 + `Retry-After` + `locked` 日志、锁定期间正确码也拒）、`TestTOTPConsoleRealmAndSessionFixation`；单测 `TestMatchStepSkew`、`TestSixDigitsAndRecoveryFormat`。
- [x] AC3 `TestRecentMFAForSensitiveOperations`（窗口外改密 403 且不吊销会话 → `reauth` 后可改；窗口 120 秒恰好/多 1 ms 边界）、`TestRecentMFADefaultsAndNoTOTP`（无 TOTP 不要求；缺省 15 分钟边界）、`TestFreshTOTPLoginSatisfiesRecentMFA`；handler 单测 `TestBearerCannotSatisfyRecentMFA`（`createApiToken` 与 `changePassword` 在近期 MFA 清单内，Bearer 一律拒绝）。
- [x] AC4 `TestEnforcedAdminTOTP`（登录成功但仅启用流程可用、其余 403 并指引、探针路由被拒、启用后恢复、强制期间不可停用、租户用户不受影响）、`TestAdminTOTPNotEnforcedByDefault`、`TestEnrollmentRequiredPolicy`、`TestEnrollmentAllowedSet`。
- [x] AC5 `TestDisableTOTP`（缺口令/错口令/错码/缺码；成功后密钥、time-step、恢复码清除；停用后可仅凭口令登录；未启用 409；重新启用得到新密钥；口令 + 恢复码亦可停用）。
- [x] AC6 `TestReauthRefreshesSession`/`TestReauthWithTOTP`（缺 CSRF/错口令/缺码/错码/恢复码不可用/跨用途重放；`reauth_at`、`mfa_verified_at`）、`TestReauthFailuresThrottle`、`TestXReauthOperationsEnforcedInChain`（按 openapi 的 11 个 x-reauth operation 逐个：窗口外 403、再认证后越过、恰好 300 秒仍有效/多 1 ms 过期、非 x-reauth 路由不受影响、Bearer 不能满足；集合与 1.3 固定清单逐项相等）、`TestReauthWindowConfigurable`；单测 `TestWindowBoundaries`、`TestBearerCannotSatisfyReauthOrRecentMFA`、`TestWithReauthOrderingAndFailClosed`（httpx：先检查后授权、未装配 fail-closed）、`TestReauthCheckerFailsClosedForForeignPrincipal`。
- [x] Redis 关闭：`TestTOTPWorksWithoutRedis`（setup/enable/两步登录/重放防护/恢复码/再认证/失败日志在 Redis 从未可用下仍工作）。
- [x] 密钥不明文落库扫描：`TestNoPlaintextSecretsAnywhere`（真实库全部 ~100+ 个文本/二进制/JSON 列 × TOTP 种子、10 个恢复码（含无连字符形态）、两个 mfa_token、口令、会话令牌与 CSRF；另扫全部日志与 Redis 键值）、`TestPlaintextScanDetectsLeak`（负向自测：故意写入明文，扫描必须发现）。
- [x] G-5 日志脱敏：`tests/security` 的 `TestLogScanTOTPScenarios`（种子、`otpauth://…secret=`、恢复码、mfa_token、口令经消息/字段/请求体 JSON/错误链/嵌套对象）——**发现并修复一处真实缺口**：自由文本里的 `recovery_code=…` 与 JSON 数组形态的 `recovery_codes` 此前不被遮蔽（`pkg/redact` 键扩展 + 回归用例）。
- [x] 停机 Flush：`TestShutdownFlushesAuthBackgroundWrites`（真实 `server.Run` + 真实认证模块：钩子被调用、调用时监听已关闭而 MySQL 仍可用、5 次失败登录的异步计数在 `Run` 返回后全部落库）；真实进程 `TestServerProcessAuthLogin` 增 setup（真实主密钥信封加密落库、进程日志无种子）。
- [x] 契约：每个 operation 的响应经 kin-openapi 校验（login 200 totp_required、verifyTotp 200/400/401/429、setup 200、enable 200/401、disable 204/401、reauth 200/401/403、11 个 x-reauth operation 的 403）；`TestRouteMetadataMatchesOpenAPI`（13 条 /auth 路由元数据与 x-* 一致）；`make contract-check`：路由清单 17 条，「M1 未实现 operation 清单」188 → 183。
- [x] 测试口令与种子运行期随机生成；错误验证码由 `wrongCode` 在运行期随机挑选（避开 ±2 窗内的有效码）；`make security` 的 G-6 不触发、未加任何豁免（曾因测试里的固定「000000」字面量触发，已改为随机/拼接而非豁免）。
- [x] 依赖：`github.com/pquerna/otp v1.5.0`（当时 proxy 上的最新版；间接依赖 `boombuler/barcode`）；`make vuln` 通过（见门禁结果）。
- [x] sprint-status、本说明、本地提交（不 push）。

## 等待环境验证（本机通过不代表这些已验证）

1. **MySQL 8.0.16（最低版本）与 Linux 验收机**重跑 `tests/integration/auth`、`tests/integration/server`、`make migrate-test`：`000016`（`CREATE TABLE` + `CHECK` + FK）与 `000017`（`ADD COLUMN`）在 8.0.16 上的行为。
2. **真实验证器 App 的互通**：本机测试用 pquerna/otp 在测试侧「扮演验证器」生成验证码（与服务端同一实现库，只能证明与 RFC 6238/4226 实现自洽）；与 Google Authenticator / Microsoft Authenticator / Authy / 1Password 等真实 App 扫描 `otpauth://` 的互通与时钟偏差行为需在验收环境用真机验证。
3. **多实例一致性**：防重放与 `mfa_token` 都落在 MySQL（单库原子 UPDATE），本机仅单进程/单库验证；多 Server 实例并发提交同一验证码的竞争行为需在 ENV-LNX 多实例部署下复核。
4. **UD 基线**：`auth.mfa_window_s` 默认 900 秒（[ASSUMPTION]）、`mfa_token` 5 次尝试上限待基线/用户确认；`auth.reauth_window_s=300` 为 UD-29 采纳值。
5. **1.18–1.21 交付后**：真实授权钩子装配后 `x-reauth` 路由（改运行模式、改角色/权限、创建/提权管理员、重置口令、吊销节点证书、改 SSRF 白名单）的整链端到端（目前用按 openapi 元数据注册的占位路由驱动）；`createApiToken`（1.19）的近期 MFA 与 Bearer 行为的整链端到端；`audit_logs` 补写（见遗留）。

## 遗留与风险

- **审计（待 1.21 补写 audit_logs）**：`setupTotp/enableTotp/disableTotp/reauth/verifyTotp` 声明 `x-audit: true`。audit 模块（1.21）尚未实现，本 Story **没有**写 `audit_logs`（不假装已写）：启用/停用 TOTP 与再认证成功只有结构化日志（用户 uid、request_id，无秘密）；失败写 `login_logs`（`totp_failed`/`login_failed`/`locked`）。`login_logs.event` 枚举（契约）没有 `totp_enabled/totp_disabled/reauth` 事件，为不扩展契约枚举而未写入 `login_logs`；1.21 须在同事务内补 `audit_logs`（actor/tenant/resource/changes，禁含口令/密钥/恢复码）。 【Story 1.21 已收口：审计记录器 internal/platform/audit 在业务事务内写 audit_logs，过渡的结构化日志与 adminAuditor 已移除；见 1-21 说明】
- **同一 time-step 内「登录后立即再认证」会被重放规则拒绝**：登录与再认证共用 `totp_last_step`，用户刚用某个验证码登录后，须等待下一个验证码（≤30 秒）才能再认证。这是「同一验证码窗口内不得重放」的直接后果，选择安全优先；如产品希望放宽（例如「新登录的会话视为已再认证」）需单独决策。
- **近期 MFA 对未启用 TOTP 的账号不生效**（无第二因子可验证）。租户用户（Console）在未强制的情况下可长期不启用 TOTP，此时改密/创建 Token 只靠口令与会话；是否对租户也强制属后续产品决策。
- **Bearer 与近期 MFA**：对 `recentMFAOperations`（改密码、创建 API Token）一律拒绝 Bearer（包括未启用 TOTP 的账号——钩子不知道 Bearer 主体的 TOTP 状态，保守处理）。1.19 实现 Bearer 认证时需据此评估「用 Token 创建 Token」的产品语义。
- **账号锁定 DoS 面**（1.16 已记录）因两步登录略有扩大：攻击者持有口令时，每个 `mfa_token` 至多 5 次尝试，且失败计入账号维度节流——攻击者持有口令即可借错误验证码把受害者锁到窗口结束；缓解同 1.16（IP 维度单独限速、`Retry-After`）。
- `login_mfa_challenges` 的过期行只在创建新挑战时顺带清理（每次至多 100 行）；高频登录场景下足够，若出现堆积可在 1.15 的内部作业里追加清理（未做，避免抢跑）。
- 授权钩子位置：当前「再认证先于授权」，对无权限者会先提示再认证再被拒绝；1.18 应改为先权限后再认证（见 `httpx.WithReauth` 注释）。1.18/1.20/1.21 交付后应评估是否保留 `httpx.WithSelfService`（沿用 1.16 的评估项）。

## 开发记录

### 设计取舍

- **挑战存 MySQL 而非 Redis 或无状态令牌**：见 CC-5。代价是登录第一步多一次 MySQL 写；收益是 Redis 关闭、多实例下语义不变，且令牌一次性/限次有原子保证。
- **`totp_last_step` 放在 users 而非独立表**：每用户只需一个单调递增的整数，CAS 一条 UPDATE 即可；独立表只会多一次联表。
- **再认证检查做成 `httpx` 层的可复用组件**而不是 auth 私有：1.18 的授权钩子要对全部 `x-reauth` 路由执行同一检查；接口 `ReauthChecker` 把「检查什么」留在 auth（它拥有会话语义），把「在链里的位置」留给 httpx。
- **近期 MFA / 强制 TOTP 放在认证钩子**（与 1.16 的 `must_change_password` 同构）：这两项判定只依赖主体与会话，不依赖权限串，放在认证后立即统一执行，所有受保护路由（含尚未实现的）自动受约束，handler 不需要各自记得调用。
- **失败统一走 `failAttempt`**：口令/验证码/恢复码/再认证的失败都计入同一套账号+IP 节流并写日志，避免出现「绕开节流的验证码猜测通道」。
- **不改契约枚举**：为不扩展 `LoginLog.event`（oasdiff 把响应枚举新增视为需评审的变更），成功类事件只留结构化日志，留给 1.21 的 audit_logs。

### 实际验证（本机）

见「门禁结果」。临时 `mysqld`（`tests/mysqltest`，用后删除）与 `redis-server`（`tests/redistest`）均为测试自拉起的独立实例；本机常驻 `mysqld(576/744)` 与 redis 未被触碰；未使用 Docker；未 push；未改 git config。测试内没有 Mock 冒充生产：唯一的替身是 `stubBearer`/`bearerStandIn`（Story 1.19 之前不存在真实 API Token 认证，仅用于验证「Bearer 请求不能满足再认证」，已在代码注释中标明，不进任何二进制）与 `fakeClock`（clock.Clock 的可推进实现，用于确定性地验证窗口边界）。

### 门禁结果

全部在本机（macOS arm64，Go 1.27.4）实际执行并通过（退出码 0）：`gofmt -l .` 无输出；`make lint`（golangci-lint 0 issues，含 arch/shell/repo-scope）；`make security`（G-5/G-6/G-11/G-12/tasktype/品牌）；`make gate-selftest`；`make race`（`CGO_ENABLED=1 go test -race ./...`，全部包 ok，含 `tests/integration/auth` 的竞态运行）；`make vuln`（No vulnerabilities found，含新依赖 pquerna/otp v1.5.0）；`make contract-check`（openapi-check/breaking 相对 contract-v1 无破坏、proto-check、migrations-check 17 个迁移哈希一致、freeze-check；M1 未实现清单 188→183）；`make build`、`make verify-build`；`make migrate-test`（真实 MySQL 全量迁移含 000016/000017）。首次 race 运行曾因三处遗留断言失败（路由清单数 12→17、就绪检查迁移版本 15→17），已更新测试后全量重跑通过。
