---
title: 'Story 1.16：认证 I：登录 / 会话 / CSRF / 登录节流 / 登录日志'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '0df058b'
context:
  - docs/bmad/epics.md（Story 1.16，1190–1215 行）
  - docs/architecture/adr/ADR-018-authn-authz.md §1–4、§6；ADR-025（Redis 边界）；ADR-009 §1/§5（Scope）
  - docs/api/openapi.yaml 的 /auth/*（x-* 元数据）、docs/bmad/review-architecture*.md（MIN-01）、docs/bmad/decisions-adopted.md（UD-27）
  - docs/database/platform-identity.md（users / sessions / login_logs / system_settings）
  - docs/bmad/stories/1-12…1-15-*.md（约定与遗留）；internal/platform/{httpx,redisx,tenantscope,apierr,logx,config,server}、cmd/lingedge-server/assembly.go
---

## Intent

**Problem:** 1.12 的认证钩子是「一律 401」的安全拒绝默认实现，没有任何方式建立会话；登录、会话、CSRF、登录节流与登录日志都不存在，后台没有可追溯、抗暴力破解的入口。

**Approach:** 新增认证模块 `internal/auth`（根包只含契约 `api.go`，实现在 `internal/auth/internal/{domain,repository,service,handler}`）：argon2id 口令哈希（PHC）、不透明会话（32B CSPRNG，库存 SHA-256）、realm 隔离、会话固定防护、CSRF（会话令牌派生）、账号+IP 双维度登录节流（Redis 主路径 / `users.failed_login_count/locked_until` 降级路径）、登录日志；把 1.12 的 `Authenticator` 钩子实现为真实的会话认证并在 `assembleHooks` 装配，经 `httpx.Router.Register` 注册 8 条 `/auth/*` 路由。授权/Scope/审计钩子**仍为安全拒绝默认**（1.18/1.20/1.21），仅对「自助」operation 授予最小授权语义（见下）。

## 核对结论：表结构与授权 + 一次追加迁移 + 一份契约变更说明

- `users`（`password_hash` PHC、`failed_login_count`/`locked_until`/`must_change_password`/`totp_enabled`）、`sessions`（`token_hash`/`csrf_hash`/`realm`/`tenant_id`/过期列/`revoked_at`/`revoke_reason`）、`login_logs`（`event`/`identifier`/`reason`/`request_id`…）、`tenant_members`、`roles`/`user_roles`/`role_permissions`/`permissions`、`system_settings`（`uid` 必填）：000001/000002 已建表，`lingedge_app` 对它们均有表级 `SELECT, INSERT, UPDATE, DELETE`——登录、会话、日志、改密、设置读取所需全部满足，**无新授权**。
- **缺口 1：`sessions` 没有 `uid`。** OpenAPI 的 `Session.session_ref` 与 `DELETE /auth/sessions/{sessionId}` 是 `Uid`（ULID，不暴露自增 id，ADR-017），而 `sessions` 只有自增 `id` 与令牌哈希。**不改冻结迁移（000001–000014 一字未改）**，追加 `migrations/000015_sessions_uid.sql`（`ADD COLUMN uid CHAR(26) NULL` + 唯一索引；`expand`；CHECKSUMS 已封存）。
- **缺口 2：`login` 没有声明 401。** 登录失败统一 `401 auth.invalid_credentials`（错误码表已有），但 `login` 的 responses 只有 200/400/429/500，契约响应校验会把 401 判为未声明。补充 `'401'`（加法）。同时把 `changePassword` 的描述由「吊销其余会话」改为「吊销全部会话」（与 AC5/ADR-018 §2 一致）。
- 以上均记录在 **`docs/bmad/contract-changes/CC-4-sessions-uid-login-401.md`**（非破坏；`make openapi-check` 相对 `contract-v1`、`make migrations-check` 相对 `migrations-m1` 均通过）。`docs/database/platform-identity.md`、`docs/database/README.md §8.1` 已同步。
- **没有改动**：错误码、其他 operation、proto、`audit_logs`（x-audit 的统一接入属 1.21，见「审计」）。 【Story 1.21 已收口：审计记录器 internal/platform/audit 在业务事务内写 audit_logs，过渡的结构化日志与 adminAuditor 已移除；见 1-21 说明】

## 架构与门禁层面的三处调整（均有自测）

1. **身份表豁免（`scripts/gates/data/identity-tables.txt`）**：repo-scope 门禁要求 Repository 导出方法首参是 `scope.Scope`。认证钩子里 Scope 尚未产生（Scope 钩子在认证之后才由主体生成，ADR-009 §5），而访问键恰好是用户自己的标识（email / token_hash / user_id）。新增受限例外 `lingedge:identity-table=<表>`：清单仅 `sessions, login_logs, tenant_members, user_roles, roles`，须在 `docs/database` 有定义、不得与平台表重复、不得是业务/审计表（`TestIdentityTablesMatchDocs`）；`AcrossTenants` 方法仍不得豁免。选择 (a) 扩展门禁而不是 (b) 把 `sessions` 等硬塞进平台表清单——后者违背该清单「文档里带 tenant_id 的表一律不得列入」的既有校验。自测：`tests/gates` 增 3 个负向用例（业务表不能用身份表豁免 / 夹带业务表 / AcrossTenants）与 1 个正向。
2. **模块根包的纯委托构造函数（`scripts/gates/archcheck.go`）**：ADR-001 §3 规定根包 `api.go` 只含接口与 DTO、不得含函数；但实现包在 `internal/<mod>/internal/…`，按 Go 的 internal 规则只有 `internal/<mod>` 子树能 import 它们，`cmd/lingedge-server` 的装配层（ADR-001 §4「手写 wire」）因而**无法拿到模块实例**——这是 ADR 设计里此前未被任何模块触发的缺口。修订：`api.go` 可导出**纯委托的 `New*` 构造函数**（无接收者、函数体恰为单条 `return <本模块 internal 包>.<函数>(…)`）；其他函数/方法/包级变量仍一律禁止，原有负向用例（`func New() int { return 1 }`）继续失败。自测：5 个负向（不委托 / 委托别的模块 / 非 New 函数 / 多语句 / 含方法）+ 1 个正向。ADR-001 已追加修订记录。
3. **`httpx.WithSelfService`（`internal/platform/httpx/selfservice.go`）**：见下节。

## 「最小授权语义」：/auth/* 自助 operation（授权/Scope/审计钩子仍为安全拒绝默认）

授权（1.18）/Scope（1.20）/审计（1.21）钩子未实现，默认拒绝。但 `/auth/me`、`/auth/csrf`、`/auth/sessions`、`/auth/login-logs`、登出、改密、吊销会话是「已认证用户操作自己」的 operation：openapi 里 `x-permissions: []`，没有可校验的权限串，handler 只用主体自身标识查询（`user_id` 来自会话，不来自请求参数）。它们在 1.18 之前就必须可用，否则登录之后什么都做不了。

做法：`auth.Module.SelfService(operationID)` 是代码可核对的闭集（本模块路由元数据中权限为空且不要求再认证的 operation，共 8 个含公开的 `login`）；`httpx.WithSelfService(hooks, isSelf)` 只对这些 operation：授权钩子放行、Scope 钩子由 `tenantscope.FromPrincipal(p, either, 无目标租户)` 从主体自身推导（**忽略 `X-Tenant-ID`，不支持代租户**，acting-as 属 1.20）、审计钩子不再因「未装配」而 500（留痕由 `login_logs` 承担）。**其他任何 operation 原样委托给默认拒绝钩子**：认证通过也不放行（真实验证：已登录的 admin 访问任务中心 `GET /tasks` 与探针路由均 `403 auth.forbidden`，handler 不执行）。1.18/1.20/1.21 交付真实钩子后，应把默认钩子换成真实实现，并评估是否保留这层包装。

**审计**：x-audit 的 `logout`/`changePassword`/`revokeSession` 当前只写 `login_logs`（`logout` 事件，`reason` 区分 `logout`/`session_revoked`/`password_changed`），**不写 `audit_logs`**——audit_logs 的同事务统一接入、字段（actor/tenant/acting_as/changes）与脱敏口径属 1.21，这里不抢跑；这是遗留项（见末节）。 【Story 1.21 已收口：审计记录器 internal/platform/audit 在业务事务内写 audit_logs，过渡的结构化日志与 adminAuditor 已移除；见 1-21 说明】

## Code Map

- `internal/auth/api.go` -- 模块契约：`Module` 接口（`Authenticator/Register/SelfService/Flush`）、`Options`（别名）、纯委托的 `New`。
- `internal/auth/internal/domain` -- `Realm`（admin↔platform / console↔tenant、Cookie 名）、`User/Session*/LoginLog`、`Principal`（实现 `tenantscope.Principal`；`HasPermission` 在 1.18 前恒 false，fail-closed）、`Settings`。
- `internal/auth/internal/repository` -- 全部 SQL 常量 + 占位符；只接受 app 池；所有查询以「某用户」为边界（无按租户/全局枚举他人会话与日志的方法）。
- `internal/auth/internal/service/{password,tokens,settings,service}.go` -- argon2id（PHC 编解码、参数上限、并发信号量）、令牌/CSRF、`system_settings` 读取（15s 缓存、越界回退默认并告警）、登录/认证/登出/会话/日志/改密。
- `internal/auth/internal/handler/{authenticator,handler,module}.go` -- 认证钩子（realm 选择、CSRF、must_change_password）、8 个路由处理函数与元数据、模块装配。
- `internal/platform/httpx/selfservice.go` -- `WithSelfService`。
- `internal/platform/redisx/limiter.go` -- 新增 `Limiter.Peek`（只读不计数）与 `Reset`：登录节流「只计失败」所需；Redis 只经该封装。
- `internal/platform/config/config.go` -- `auth.argon2.{memory_kib,iterations,parallelism}`（缺省 64MiB/4/4；下限 OWASP 最低建议，低于下限拒绝启动；可经 `LINGEDGE_AUTH__ARGON2__*` 覆盖）；`configs/server.example.yaml` 已更新。
- `internal/platform/server` -- `Deps` 增 `DB/Redis/Clock/IDs/Logger/Config`，`Options.BuildHooks(Deps)`（基础设施就绪后构造钩子）。
- `cmd/lingedge-server/{assembly,routes,serve}.go` -- `assembly` 持有模块实例（认证钩子与 `/auth/*` 路由是同一实例）；`routes` 子命令用 `ManifestOnly` 模块导出清单（不需要 DB/Redis）。
- `migrations/000015_sessions_uid.sql`、`docs/bmad/contract-changes/CC-4-*.md`。

## 关键实现点

- **口令（AC1）**：`golang.org/x/crypto/argon2` IDKey，PHC `$argon2id$v=19$m=…,t=…,p=…$盐$哈希`（16B 盐、32B 密钥、无填充 base64）。参数取自配置；校验按**哈希串自带参数**计算，改配置不使旧哈希失效，登录成功后发现参数不同则透明 rehash。库里取出的哈希串先做格式与上限校验（m≤1GiB、t≤32、p≤32…），损坏/被篡改的哈希一律按口令错误处理（401，不 panic、不放行）。并发哈希受信号量限制（`max(2, CPU)`）并受 ctx 约束。**代码、迁移、种子、示例配置无任何默认口令**（G-6 全绿）；测试内用 `crypto/rand` 生成口令并真实 argon2id 哈希。
- **会话（AC2）**：32B CSPRNG → Cookie 为 base64url（43 字符）；库存 `SHA-256(原值)` 的十六进制。Cookie `le_admin_sid` / `le_console_sid`：`HttpOnly; Secure; SameSite=Lax; Path=/`，不设 Domain（主机范围）。登录响应、`/auth/me`、`/auth/csrf` 都不含令牌明文以外的凭据；库里/日志里/Redis 里都没有令牌明文（端到端扫描）。
- **realm 隔离（AC2）**：①登录：admin 仅 `platform` 用户、console 仅 `tenant` 用户且须有有效成员关系（默认租户写入 `sessions.tenant_id`），交叉登录返回与口令错误逐字节相同的 401；②会话：按 Cookie 名确定 realm，`sessions.realm` 必须等于 Cookie 的 realm（admin 令牌放进 `le_console_sid` → 401，反之亦然）；③路由：`platform` 路由只认 admin 会话（console 会话 → 401），`tenant` 路由认 console 也认 admin（平台管理员代租户，授权/Scope 由 1.18/1.20 判定），`either` 皆可。
- **会话固定防护**：登录成功在同一事务里吊销请求携带的该 realm 旧会话（`revoke_reason=relogin`）并签发全新令牌；客户端预置的令牌从不被采纳；失败的登录不改变既有会话。
- **滑动 + 绝对过期**：`auth.session_idle_ttl_s` / `auth.session_absolute_ttl_s`（system_settings；代码默认 2h / 12h，**[ASSUMPTION]，UD-27 待基线**）。每次校验 `now < expires_at ∧ now < absolute_expires_at`；活跃会话每 ≥60s 续期（`expires_at = min(now+idle, absolute)`，避免每请求写库）。滑动期限不得长于绝对期限（配置冲突时以绝对期限为准）。
- **CSRF（AC3）**：Cookie 会话的写请求（POST/PUT/PATCH/DELETE）要求 ①`Sec-Fetch-Site` 缺省或为 `same-origin`；②`Origin` 缺省，或主机与请求 Host 相同、或在额外白名单；③`X-CSRF-Token` 与 `sessions.csrf_hash` 常量时间一致——任何一项不符 `403 auth.csrf_invalid`。CSRF 令牌由会话令牌派生（`base64url(HMAC-SHA256(key=会话令牌原值, "lingedge/csrf/v1"))`，库存其 SHA-256），因此 `/auth/me`、`/auth/csrf`、登录响应对同一会话返回同一令牌、无需另存明文。携带 `Authorization: Bearer` 的请求完全不走 Cookie 与 CSRF 路径（API Token 认证由 1.19 注入；此前一律 401，**不会回落到 Cookie**）。部署要求：反向代理必须保留原始 `Host`（否则同源请求被误判跨源，fail-closed）。
- **登录节流（AC4/AC7）**：两个维度——identifier（`SHA-256(realm,邮箱)` 前 40 位，不论账号是否存在）与来源 IP；Redis 主路径用 `redisx.Limiter`（先 `Peek` 判断是否已锁定，失败后 `Allow` 计数，成功后 `Reset` 账号计数；键 `le:<env>:auth:login_acct|login_ip:…`，全部带 TTL，不含明文邮箱/口令/令牌）。窗口内失败数达到阈值后，**下一次**尝试返回 `429 rate_limit.exceeded` + `Retry-After`（锁定期间连正确口令也 429，不再校验口令）。Redis 不可用时 `Limiter` 降级为进程内窗口；同时 `users.failed_login_count/locked_until` 作为 DB 兜底：**失败计数一律维护**，**仅在 Redis 降级时被查询**，因此进程重启后（进程内窗口清空）降级期间仍保持锁定。阈值/窗口 `login.throttle_account_max`（默认 10）/`login.throttle_ip_max`（默认 30）/`login.throttle_window_s`（默认 900），**[ASSUMPTION]，UD-27**。`login_logs`：每次失败 `login_failed`（`reason` 区分 unknown_user/bad_password/realm_mismatch/disabled/no_membership，不存在账号 `user_id` 为空）、达到阈值时 `locked`（`threshold_reached`）、锁定期间 429 的 `locked`（`throttled`，同一 identifier 每分钟至多一条，防止日志被刷爆）。
- **无账号枚举（AC4/AC7）**：「用户不存在 / 口令错误 / realm 不符 / 账号停用」返回同一 401（状态码、响应头、响应体逐项一致，测试断言）；任何情形都做一次完整的 argon2id 计算（不存在/不合格账号用启动时计算的同参数哑哈希）。**统计检查发现并修复了一处耗时预言机**：失败时同步写 `users.failed_login_count` 会让「存在的账号」比「不存在的账号」多一次事务提交。把后台写入临时改回同步、随机化采样顺序后重跑：「用户不存在」比「口令错误」快 0.6–0.9 ms，Mann-Whitney z=4.9/6.9/6.5，测试稳定失败；改为 DB 兜底计数在请求路径之外异步写入（有界并发、5s 超时、尽力而为，ADR-025 §5 本就把兜底层定义为尽力而为）后，连续 4 次运行 |z|≤1.6、中位数差 ≤0.1 ms。统计测试同时带「不做哈希」的对照组，证明该检查有区分力。
- **改密（AC5）**：当前口令（错误 → 401 并计入节流，超限 429）+ 策略（12–128 字符、不得与当前相同、不得等于邮箱 → `400 auth.password_policy`）；同一事务内更新哈希、清除 `must_change_password`、**吊销该用户全部会话（含当前，`revoke_reason=password_change`）**并清除 Cookie——必须重新登录。`must_change_password` 用户的会话只能调用 `getMe/getCsrfToken/logout/changePassword`，其余 `403 auth.forbidden`（由认证钩子对所有受保护路由统一强制）。
- **TOTP（1.17 前 fail-closed）**：启用了 TOTP 的账号口令正确时**不建立会话**，返回 `401 auth.totp_required`（`/auth/totp/verify` 与 `mfa_token` 属 1.17）。
- **`/auth/me`**：用户、realm、租户（console）、角色/权限串/菜单键（只读自 `user_roles`/`role_permissions`/`permissions`；授权判定属 1.18）、`capabilities`（`run_mode` 自 `system.run_mode`；`modules` 仅 `["auth"]`，其余模块由各自 Story 追加）、`csrf_token`、`session_expires_at`；`acting_tenant` 恒 null（1.20）。
- **日志与审计（AC6）**：访问日志只记路径（不记查询串、头、体）；服务日志不含口令/令牌/CSRF；日志与库内容经 G-5 与端到端扫描。

## 默认值、基准与口径（[ASSUMPTION]，基线未规定）

| 参数 | 默认 | 说明 |
|---|---|---|
| argon2id | m=64 MiB、t=4、p=4 | **本机基准**（Apple M2 / 8 核 / 8 GiB，Go 1.27，`BenchmarkArgon2idHash -benchtime 15x`）：m=19 MiB t=2 p=1 ≈ 21 ms；64 MiB t=2 ≈ 24 ms；64 MiB t=3 ≈ 36 ms；**64 MiB t=4 ≈ 47 ms（采用）**；128 MiB t=3 ≈ 77 ms；128 MiB t=4 ≈ 101 ms；256 MiB t=3 ≈ 158 ms。**生产机必须复测**：`go test -run '^$' -bench Argon2idHash -benchtime 20x ./internal/auth/internal/service/`（`LE_ARGON2_M/T/P` 环境变量试组合），目标单次 50–250 ms，并发峰值内存 ≈ 并发登录数 × m；结果写入 `server.yaml` 的 `auth.argon2.*`。低于 OWASP 下限（m≥19456 KiB、t≥2、p≥1）拒绝启动；改参数后用户下次登录自动 rehash。 |
| 会话滑动 / 绝对过期 | 2 h / 12 h | system_settings `auth.session_idle_ttl_s` / `auth.session_absolute_ttl_s`；UD-27 待基线，不改结构 |
| 节流窗口 / 账号阈值 / IP 阈值 | 15 min / 10 / 30 | `login.throttle_window_s` / `login.throttle_account_max` / `login.throttle_ip_max`；UD-27 待基线；设置 15 s 内生效，无需重启 |
| 滑动续期最小间隔 | 60 s | 工程缺省，避免每请求写库 |
| 并发 argon2id 计算数 | `max(2, CPU)` | 内存保护；超出者排队，受请求 ctx 约束 |

## Tasks & Acceptance

- [x] AC1 口令：argon2id PHC（`TestHasherPHC`：格式/盐随机/校验/旧参数提示 rehash；`TestVerifyRejectsMalformedHashes`：11 种损坏/极端参数哈希即时拒绝；`TestPasswordRehashOnLogin`、`TestCorruptPasswordHashFailsClosed`；配置边界 `TestAuthArgon2Config`）；参数基准与方法见上；G-6（`make security` 全绿，代码/迁移/种子/示例配置无默认口令；测试口令运行期随机生成，不加豁免）。
- [x] AC2 会话与 realm：`TestLoginSuccessSessionAndCookie`（platform/tenant：Cookie 全属性、32B 令牌、库内仅 SHA-256、明文不入库、uid、login_success 日志、`/auth/me`、kin-openapi 契约）、`TestRealmIsolation`（交叉登录 401 且响应一致、令牌交叉 401、platform 路由拒绝 console 会话）、`TestAuthenticatedButUnauthorizedRoutesStayDenied`（授权钩子未实现时已认证的非自助路由 403，handler 不执行）、`TestSessionFixation`、`TestLoginIneligibleAccounts`、`TestLoginValidation`、`TestSessionSlidingAndAbsoluteExpiry`（假时钟：活跃会话滑动续期至绝对期限 12h 后失效；空闲 >2h 失效）。
- [x] AC3 CSRF：`TestCSRF`（缺头/错头/他人令牌/Origin 不符/`Origin: null`/`Sec-Fetch-Site` cross-site 与 same-site → 403 `auth.csrf_invalid` 且无副作用；GET 免 CSRF；正确组合 204；`/auth/csrf` 令牌可用；Bearer 请求不走 CSRF 也不回落 Cookie——1.19 前 401 而不是 403）、`TestMustChangePasswordEnforced`。
- [x] AC4 节流：`TestThrottleAccountLockout`（存在/不存在账号状态码序列同为 401×3→429×2、响应体逐字节一致、锁定期间正确口令与换 IP 仍 429、无会话、日志 login_failed×3/locked、locked 限频、DB 兜底计数）、`TestThrottleWindowExpiryAndResetOnSuccess`、`TestThrottleIPDimension`、`TestRedisDownLoginAndThrottle`（**真实 redis-server**：运行中 SIGKILL → 降级后登录/会话/节流仍工作 → 「进程重启」（新模块实例）后 DB 兜底仍锁定 → Redis 重启后自愈；以及 Redis 从未可用）、`TestLimiterPeekAndReset`（真实 Redis 上 Peek 只读/Reset/降级/恢复）。
- [x] AC4/AC7 无枚举：`TestNoAccountEnumeration`（4 类失败的状态码/响应头/响应体逐项一致；各 40 次随机顺序交错采样，Mann-Whitney z 与中位数相对差检验 + 「不做哈希」对照组证明灵敏度；本机实测中位数差 <1 ms、|z|<2.5）。
- [x] AC5 `/auth/*`：`TestChangePasswordRevokesAllSessions`（策略/错误当前口令 401 且计入节流 429/成功后全部会话失效、他人不受影响、旧口令失效、PHC 存储、清 Cookie）、`TestSessionsListAndRevoke`（只见自己的、`current`、分页、他人/不存在 404、非法 400、缺 CSRF 403、吊销当前清 Cookie）、`TestMyLoginLogsOnlyOwn`（AC7：仅本人、游标分页无重复、他人/陌生标识不可见）。
- [x] AC6 日志：`TestLogsContainNoSecrets`（真实 MySQL/Redis 跑完整流程后，扫描 debug 级访问+应用日志、Redis 全部键值、`login_logs/sessions/users` 全部文本列：无口令/令牌/CSRF 明文；Redis 键全带 TTL 且不含明文邮箱）、`tests/security` 的 `TestLogScanAuthScenarios`（G-5 认证场景：Cookie/Set-Cookie 头、会话令牌、CSRF、登录体口令经消息/字段/错误链/嵌套对象）。
- [x] 契约：每个 operation 的成功与错误响应经 kin-openapi 校验（login 200/400/401/429、logout 204/403、getMe、getCsrfToken、changePassword 204/400/401/429、listSessions、revokeSession 204/404、listMyLoginLogs）；`TestRouteMetadataMatchesOpenAPI`（8 条路由元数据与 x-* 一致、无 501）；`make contract-check`：路由清单 12 条（任务中心 4 + 认证 8），「M1 未实现 operation 清单」196→188。
- [x] 真实进程：`TestServerProcessAuthLogin`（`lingedge-server` 子进程，默认 argon2id 参数、`assembleHooks` 真实接线：登录 → `/auth/me` → `GET /tasks` 403（授权钩子未实现）→ Redis 被杀后登录仍成功 → 登出 → 进程日志无秘密）。
- [x] 门禁：`make lint security gate-selftest race vuln contract-check build verify-build migrate-test` 全绿，gofmt 干净（结果见开发记录）。
- [x] sprint-status、本说明、本地提交。

## 等待环境验证（本机通过不代表这些已验证）

1. **真实 HTTPS 链路下 `Secure` Cookie 的行为**：本机测试在 `http://127.0.0.1` 上断言 `Set-Cookie` 的属性（含 `Secure`），浏览器端「仅 HTTPS 发送」、Nginx 反代终结 TLS 后的整链、`Host` 头是否被反代保留（CSRF 的 Origin 同源判定依赖它）、`X-Forwarded-For` 可信反代配置，均需在部署环境（ENV-LNX + Nginx）验证。
2. **MySQL 8.0.16（最低版本）与 Linux 验收机**重跑 `tests/integration/auth`、`tests/integration/server`；`000015`（`ADD COLUMN … + ADD UNIQUE KEY`）在 8.0.16 上的行为。
3. **argon2id 参数在生产机上的复测**与并发峰值内存评估（本机数值仅供参考）。
4. **UD-27 基线**：会话滑动/绝对过期与节流窗口/阈值的默认值（2h/12h，15min/10/30）待基线报告后确认。
5. **Redis 8.10.1 之外的版本、跨主机/TLS Redis**下降级与自愈（沿用 1.10 的验证范围）。

## 遗留与风险

- **审计**：`logout/changePassword/revokeSession` 的 `audit_logs` 同事务接入、`login` 的审计字段（actor/tenant/acting_as）属 1.21；当前只有 `login_logs`。授权/Scope 钩子（1.18/1.20）交付后，应评估是否保留 `httpx.WithSelfService` 这层包装（或把自助判定并入真实授权钩子）。 【Story 1.21 已收口：审计记录器 internal/platform/audit 在业务事务内写 audit_logs，过渡的结构化日志与 adminAuditor 已移除；见 1-21 说明】
- **Bearer / API Token**：认证钩子已预留 `Bearer` 注入点（`auth.Options.Bearer`），1.19 实现；此前携带 Bearer 一律 401。Bearer 成功路径免 CSRF 的端到端用例待 1.19。
- **TOTP**：1.17 实现 `/auth/totp/*`、`mfa_token`、`reauth`；此前启用了 TOTP 的账号无法登录（fail-closed）。`mfa_verified_at`/`reauth_at` 已有列，本 Story 不写。
- **`/auth/me` 的权限集**只读自角色表，种子里的内置角色与权限由 1.7/1.18 提供；`acting_tenant` 恒 null（1.20）。
- **identifier 维度的账号锁定可被用作 DoS**（攻击者对受害者邮箱制造失败即可锁定其登录至窗口结束）：这是 ADR-018 §6「按 identifier 计数、不区分存在性」的固有取舍；缓解来自 IP 维度单独限速与较长窗口内的 Retry-After；是否引入「已知良好 IP」豁免属后续决策。
- **DB 兜底的残余枚举面**：降级且进程重启后，存在的账号可能因 DB 锁定而比不存在的账号更早 429（不存在账号只有进程内窗口）。仅在 Redis 故障 + 重启叠加时出现，响应形态相同（都是 429），只有「何时」不同；接受并记录。
- 后台写入（DB 兜底失败计数）在进程停机时可能被截断（尽力而为）；`Module.Flush` 已提供，server 停机钩子接入可随后续装配迭代。 【Story 1.22b 已收口：1.17 已接入，现在位于共享装配根 `cmd/lingedge-server/assembly`，`TestShutdownFlushesAuthBackgroundWrites` 覆盖；见 1-22b-遗留项清理.md】
- 滑动续期粒度 60 s：会话实际过期时间可比「最后一次请求 + idle」早最多 60 s 之内的偏差（保守方向，不会延长）。

## 开发记录

### 设计取舍

- **CSRF 令牌派生而非随机存储**：随机令牌要么每次 `/auth/me` 轮换（多标签页互相踢掉），要么另存明文（违背「库内只存哈希」）。由会话令牌 HMAC 派生后无状态、稳定、可校验，库里仍存 SHA-256（满足 ADR-018 §3「比对 `sessions.csrf_hash`」）。
- **节流只计失败**：`Limiter.Allow` 每次调用都计数，若用它拦截尝试，成功登录也会累计并最终锁死正常用户。新增 `Peek`（只读）使「先判断是否已锁定、失败后再计数、成功后清零」成立；Redis 访问仍全部经 `redisx`。
- **哑哈希计算一次**：启动时用与配置相同的参数计算一次哑哈希，之后每次「不存在/不合格账号」校验同参数哈希；比每次重新哈希更便宜且耗时对称。
- **异步 DB 兜底写入**：见「无账号枚举」——这是统计检查发现的真实预言机，不是理论推演。首版统计测试用固定采样顺序，出现了与实现无关的位置效应（改成异步后 z 仍≈7），因此改为每轮随机化顺序；随机化后把写入临时改回同步，验证出了真实差异（见上），再恢复异步。
- **`WithSelfService` 放在 `internal/platform/httpx`**：Scope 构造入口受 `[tenantscope-mint]` 路径白名单限制（`internal/platform/` 在内），认证模块不能自己造 Scope；把包装放在 httpx 也让测试直接复用同一份真实装配。

### 实际验证（本机）

见下方「门禁结果」。临时 `mysqld`（8.0.46，`tests/mysqltest`，用后删除）与 `redis-server`（`tests/redistest`）均为测试自拉起的独立实例；本机常驻 `mysqld(576/744)` 与 redis 未被触碰；未使用 Docker；未 push。

### 门禁结果

全部在本机（macOS arm64，Go 1.27.4）实际执行并通过（退出码 0）：

- `gofmt -l .` 无输出；
- `make lint`（vet / 脚手架 / arch / golangci-lint 0 issues / shell / repo-scope，含新增的身份表豁免与根包委托构造函数规则）；
- `make security`（G-5/G-6/G-11/G-12/tasktype/品牌；G-6 在代码、迁移、示例配置与测试中均未触发，测试未加豁免）；
- `make gate-selftest`（`scripts/gates` + `tests/gates`，含本 Story 新增的正/负向自测）；
- `make race`（`CGO_ENABLED=1 go test -race ./...`，38 个测试包全部 ok，含 `tests/integration/auth` 在竞态检测下的 213 s 全量运行）；
- `make vuln`（No vulnerabilities found）；
- `make contract-check`（selftest / openapi-check / openapi-breaking 相对 `contract-v1` 无破坏 / proto-check / migrations-check 15 个迁移哈希一致且仅追加 / freeze-check；路由清单 12 条，「M1 未实现 operation 清单」188 个）；
- `make build`、`make verify-build`、`make migrate-test`（真实 MySQL 全量迁移含 000015）；
- `go test -count=1 ./...` 全量无失败（含 `tests/integration/{auth,server,httpx,redis,db,taskrt,secrets}`）。

本 Story 新增/调整的测试面：`tests/integration/auth`（真实 MySQL + 真实 redis-server：登录/realm/固定/CSRF/节流/Redis 故障/枚举统计/改密/会话/登录日志/脱敏扫描/契约/路由元数据/rehash/损坏哈希，共 20 个顶层用例）、`tests/integration/server` 的 `TestServerProcessAuthLogin`（真实子进程）、`tests/integration/redis` 的 `TestLimiterPeekAndReset`、`tests/security` 的 `TestLogScanAuthScenarios`、`internal/auth/internal/service` 单元测试与 argon2id 基准、`config` 的 `TestAuthArgon2Config`；因新增迁移 000015，`tests/integration/{httpx,server}`、`tests/migrations` 中「最新迁移版本」的断言由 14 更新为 15（`m1Only=13` 的 M1 冻结范围口径不变）。

环境：临时 `mysqld`（8.0.46）与 `redis-server`（8.10.1）均为测试自拉起的独立实例，用后删除；结束时无残留测试进程，本机常驻 `mysqld(576/744)` 与 redis 未被启动/改动/终止；未使用 Docker；未 push。
