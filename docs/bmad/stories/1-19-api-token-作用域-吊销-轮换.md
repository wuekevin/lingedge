---
title: 'Story 1.19：API Token（作用域 / 吊销 / 轮换）'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '730ede5'
context:
  - docs/bmad/epics.md（Story 1.19，1266–1285 行）
  - docs/architecture/adr/ADR-018-authn-authz.md §7、§5、§12、§13；ADR-025（Redis 边界）；ADR-009（Scope）
  - docs/api/openapi.yaml 的 /auth/api-tokens*（x-permissions / x-tenant-scope / x-audit）；docs/database/platform-identity.md（api_tokens）
  - docs/bmad/stories/1-12…1-18-*.md；internal/auth；internal/platform/{httpx,redisx,tenantscope,apierr,rbac}；pkg/redact
---

## Intent

**Problem:** 1.16/1.17 的认证钩子里 Bearer 只是占位（一律 401，测试用替身）：没有令牌的创建/吊销/轮换，机器调用无从谈起；`createApiToken` 的近期 MFA 钩子位置已留，但没有真实路由。

**Approach:** 在 `internal/auth` 内实现 API Token：`/auth/api-tokens*` 共 5 个 operation（经 `httpx.Router.Register`，元数据与 OpenAPI 一致，无 501 占位）+ 真实的 Bearer 认证（替换占位，移除 `Options.Bearer` 注入点）。主体有效权限 = 令牌 scopes ∩ 创建者**当前**权限；令牌只能调用业务路由；令牌管理只能用 Cookie 会话。没有迁移变更（`api_tokens` 与 `lingedge_app` 授权经核对已足够）；OpenAPI 仅加法（CC-7）；`httpx` 新增 `ClientIPFrom`（认证钩子取来源 IP 的唯一合法方式）。

## 核对结论：表结构与授权（无缺口，无新迁移）

- `api_tokens`（`000001`）：`user_id`（FK CASCADE）、`tenant_id`（NULL=平台令牌，FK RESTRICT）、`name`、`token_prefix`（唯一）、`token_hash`（唯一，CHAR(64)）、`scopes_json`、`allowed_cidrs_json`、`expires_at`、`last_used_at`、`last_used_ip`（VARBINARY(16)）、`revoked_at`、`rotated_from_id`（自引用 SET NULL）已齐备；`lingedge_app` 在 `000001` 有表级 `SELECT, INSERT, UPDATE, DELETE`；权限 `token:manage`（scope=both）已随 `000004` 种子写入并在 `tenant_owner`/`platform_admin` 等内置角色内。→ **冻结迁移 `000001`–`000018` 一字未改，无新迁移。**
- **契约（仅加法，CC-7）**：`ApiToken.rotated_from_uid`（可选，`Uid | null`）。`make openapi-breaking` 相对 `contract-v1` 无破坏性变更。
- **门禁变更（须评审）**：`scripts/gates/data/identity-tables.txt` 登记 `api_tokens`，并把 `scripts/gates/gates_test.go`（`TestIdentityTablesMatchDocs`）「禁入身份表」名单中的 `api_tokens` 移出。理由：Bearer 认证发生在 Scope 钩子**之前**、以 `token_hash` 为键，令牌管理以创建者自己的 `user_id`（及其租户）为边界——与已列入的 `sessions` 同类，不是「按租户列举」的业务表；`sites/tasks/certificates/origins/rules/audit_logs` 仍禁入。

## Code Map

- `internal/auth/internal/service/apitoken.go` -- 令牌生成/哈希/格式、CIDR 规范化与匹配、创建/轮换/吊销/读取/列表、Bearer 认证（`AuthenticateAPIToken`）、`last_used` 节流异步写、`token_auth_failed` 日志；`service/perms.go`（Bearer 主体：版本化权限缓存 + 与 scopes 取交集）、`service/settings.go`（默认/上限有效期）。
- `internal/auth/internal/repository/apitoken.go`、`domain/apitoken.go` -- 数据访问（编译期常量 SQL）与领域类型；`Principal` 增 `TokenID/TokenPrefix/TokenScopes`。
- `internal/auth/internal/handler/tokens.go` -- 5 个路由元数据与 handler、`bearerAuthenticator`、`bearerAllowed`；`authenticator.go`（Bearer 策略、`rotateApiToken` 入近期 MFA 清单）、`authorizer.go`（令牌管理的「自有资源」授权路径）、`audit.go`（过渡审计覆盖令牌管理）、`module.go`（移除 `Options.Bearer`，内置真实实现）。
- `internal/platform/httpx/clientip.go` + `middleware.go` -- `ClientIPFrom`（与 Gin `ClientIP`/`TrustedProxies` 同源）。
- `docs/api/openapi.yaml`（加法）、`docs/bmad/contract-changes/CC-7-*.md`、ADR-018 修订记录、`docs/database/platform-identity.md`。
- 测试：`internal/auth/internal/service/apitoken_test.go`、`handler/authenticator_test.go`、`internal/platform/httpx/clientip_test.go`、`tests/integration/auth/apitoken_*_test.go`（+ `reauth_test.go`/`csrf_test.go` 去掉 Bearer 替身）、`tests/security/logscan_test.go`（G-5 新用例）。

## 关键实现点

- **AC1 令牌形态与明文仅一次**：`le_<8 位前缀>_<43 位随机>`，前缀 5 字节 CSPRNG 的小写 base32（`[a-z2-7]{8}`，唯一索引冲突重试），随机部分 32 字节 CSPRNG 的 base64url；库存 `SHA-256(完整令牌)` 十六进制 + `token_prefix`；明文只在创建/轮换响应出现一次（`Cache-Control: no-store`）；不入库（`createApiToken` 经 `httpx.SetReplayBody` 登记去掉明文的响应体，重放返回 201 + 元数据且不再回显明文，复用 1.18 的做法）、不入 Redis（Redis 里只有「哈希 → 令牌 id」）、不入日志（日志只记前缀，字段名用 `prefix`/`uid`，避开 logx 对 `token` 键名的遮蔽，令牌形态本身也被 `pkg/redact` 遮蔽）。
- **AC2 scopes ⊆ 创建者当前权限**：字典外权限串 400、超出创建者当前权限 403（先报 400 再报 403）、`token:manage` 不可授予令牌；租户创建者只能授予其租户内权限（平台权限永远越界）；`allowed_cidrs`：IPv4/IPv6 CIDR 与单个 IP（折叠 IPv4 映射地址，拒绝 `/0`、zone、非法值，≤32 项，规范化排序去重）；`expires_at`：RFC 3339、须在未来、不得超过系统上限，省略取默认（见「默认值与口径」）；创建/轮换需近期 MFA（`recentMFAOperations` 统一钩子）。
- **令牌管理的边界**：只作用于调用者自己名下的令牌（平台用户的平台令牌、租户用户在其租户内创建的令牌；他人的与不存在的一样 404）；**只能用 Cookie 会话**（Bearer 一律拒绝）；授权走「自有资源」路径（`rbac.Decide` 对 `either` 路由写操作要求 acting-as，而平台用户创建自己的平台令牌不是代租户，所以单独处理）：要求 `token:manage`，不接受代租户（平台主体带 `X-Tenant-ID` → 400，租户主体带他人租户 → 404）。
- **AC3 吊销立即失效**：授权相关状态（吊销/过期/创建者状态/成员关系/权限版本）**每个请求从 MySQL 现读**；Redis 只缓存不可变的「哈希 → 令牌 id」（`le:<env>:auth:apitoken:<sha256>`，TTL ≤ 60 秒）。吊销：先删键、提交、再删键（堵并发回填窗口）；Redis 不可用/删除失败/残留旧值都不会让已吊销令牌复活（测试：缓存命中状态下吊销、人为回填残留键、运行中 SIGKILL、从未可用）。轮换：新令牌继承名称/来源限制/有效期时长，scopes 与创建者当前权限取交集（全失去 → 403），记 `rotated_from_id`，**旧令牌立即吊销**（ADR-018 §7 与 OpenAPI 均写「新令牌 + 吊销旧令牌」，无宽限期）；已吊销的令牌不能轮换（404）。
- **AC4 Bearer 认证**：替换占位；不使用 Cookie 与 CSRF；Bearer 与 Cookie 同时出现时只按 Bearer 处理（无效 Bearer 不回落 Cookie）；有效权限 = 令牌 scopes ∩ 创建者**当前**权限（创建者降权 → 令牌随之收缩，立即生效；被禁用/删除/移出租户/租户停用 → 401；强制管理员 TOTP 与须先改密同样约束令牌；创建者新增权限不扩大令牌）；**令牌过期、吊销、来源 IP 不在 `allowed_cidrs`、哈希不符、格式错误、不存在一律同一个 401**（逐字节相同，除 `request_id`；原因与前缀只写服务端日志，另写 `login_logs.token_auth_failed`，同一令牌至多每分钟一条）；租户令牌访问平台资源（`platform` 路由、`/admin/*`）403（NFR-14，`rbac.Decide`），他租户 `X-Tenant-ID` 404，超出 scopes 403（FR-25）；Bearer 只能调用声明了 `x-permissions` 的业务路由——自助 operation（登出/改密/会话/TOTP/再认证/登录日志…）与令牌管理要求 Cookie 会话；Bearer 不能满足近期 MFA 与再认证（1.17 行为保持，改用真实令牌验证）。
- **来源 IP**：`httpx.ClientIPFrom`（路由器在链首登记 Gin `ClientIP()`，遵守 `Options.TrustedProxies`）——伪造 `X-Forwarded-For` 不能绕过 `allowed_cidrs`（测试：可信代理下左侧伪造的白名单地址无效、多个 XFF 头、`X-Real-IP`/`Forwarded` 不被采信；对端不可信时 XFF 完全被忽略；IPv6、IPv4 映射 IPv6）；来源未知 fail-closed。
- **`last_used_at`**：每令牌每进程至多每分钟异步写一次（后台写入、并发有上限，不阻塞请求）。
- **Scope（1.20 边界，保持 fail-closed）**：本 Story 不产出完整 Scope 钩子。令牌主体的授权决策经 `rbac.Decide` 产出并交给过渡 Scope 钩子：自己的租户/平台级/跨租户只读 → 落成 Scope；平台令牌 + `X-Tenant-ID`（acting-as，需 `tenant:impersonate`）授权通过但 Scope 仍**安全拒绝 403**（测试逐项断言 handler 不执行），不放行未实现的部分。
- **审计**：audit 模块（1.21）未实现，**未写 audit_logs**，登记「待 1.21 补写」；令牌创建/轮换/吊销只写不含敏感信息的结构化日志（前缀、令牌 uid、操作者 uid、`request_id`，`audit_logs_written=false`），`adminAuditor` 仅对本模块 `/admin/*` 与 `/auth/api-tokens*` 放宽，其余要求审计的 operation 仍被 DenyAuditor 拒绝。

## 默认值与口径（[ASSUMPTION] 标注）

| 参数/口径 | 取值 | 说明 |
|---|---|---|
| 默认有效期（省略 `expires_at`） | 90 天 | 基线与 UD 均未规定 `[ASSUMPTION]`；令牌不应默认永不过期；可配置 `auth.api_token_default_ttl_s`（UD-27 同口径，改默认值只是配置改动） |
| 有效期系统上限 | 365 天 | 同上；`auth.api_token_max_ttl_s`；默认有效期不得超过上限 |
| 缓存 TTL（哈希 → id） | 60 秒（ADR-025 §1 上限） | 仅映射缓存，状态每次现读 |
| `last_used_*` 写入间隔 | 每令牌每进程 ≤ 1 次/分钟 | 工程取值 |
| `token_auth_failed` 日志 | 同一令牌 ≤ 1 条/分钟 | 防止被盗令牌持有者刷爆 `login_logs` |
| scopes ≤ 64 项、`allowed_cidrs` ≤ 32 项、名称 ≤ 128 字符 | — | 工程取值 |
| 令牌列表范围 | 调用者自己名下 | OpenAPI 描述「tenant 主体只能看到自己租户下的令牌」；权限描述为「自己名下的 API Token」，取更严的后者；租户管理员代管成员令牌不在本 Story |
| 轮换无宽限期 | 旧令牌立即吊销 | 见上；推翻需新列与契约 |
| 创建/轮换要求近期 MFA | 是 | 创建按 AC2；轮换同样产生新凭据，同口径（`rotateApiToken` 入近期 MFA 清单，契约已声明 403） |
| Bearer 可调用范围 | 声明了 `x-permissions` 的业务路由 | 自助 operation 与令牌管理要求 Cookie 会话（防令牌接管账号/繁殖令牌）；属对 AC 的安全加固 |

## Tasks & Acceptance

- [x] AC1 `TestAPITokenCreatePlaintextOnce`（形态、SHA-256+前缀入库、`tenant_id` NULL、默认 90 天、GET/列表不含明文、幂等重放不回显且不重复创建、全库全部文本/JSON 列 + Redis 键值 + 日志无明文、日志只记前缀且 `audit_logs_written=false`、kin-openapi 契约）；`service/apitoken_test.go`（形态/熵/格式校验、redact 识别令牌形态）。
- [x] AC2 `TestAPITokenScopesMustBeSubsetOfCreator`（子集通过；越权 403；字典外/`token:manage`/空/过多/名称/未知字段/非法 CIDR 400；被拒绝不写库；无 `token:manage` 403；租户创建者平台权限越界 403）、`TestAPITokenCreateRequiresRecentMFAAndCookieSession`（TOTP 账号：窗口外创建与轮换 403 `auth.reauth_required`，列表/吊销不受影响，再认证后可创建；Bearer 不能创建/轮换/吊销/列出令牌且不改令牌表）、`TestAPITokenAllowedCIDRs`、`TestAPITokenExpiry`、`TestAPITokenExpirySettings`。
- [x] AC3 `TestAPITokenRevocationIsImmediate`（缓存命中状态下吊销下一个请求即 401 且缓存键被清除；残留/回填的旧缓存键不能复活；Redis 运行中 SIGKILL 后认证/吊销/轮换仍正确、Restart 后自动回填；Redis 从未可用）、`TestAPITokenRotationChain`（3 级轮换链、`rotated_from_id`/`rotated_from_uid`、旧令牌立即 401、继承名称/CIDR/scopes/有效期、已吊销不能再轮换、他人 404、scopes 与创建者当前权限取交集、全失去 403 且不吊销）。
- [x] AC4 `TestBearerScopeEnforcementAndPolicy`（作用域越界 403、平台令牌访问平台资源、不回落 Cookie、Bearer 优先于 Cookie、自助 operation 一律 403 且不影响创建者会话、8 种失败原因 401 逐字节相同且不泄漏原因、服务端日志记原因与前缀、`token_auth_failed` 恰一条）、`TestTenantTokenIsolation`（租户令牌读写自己租户免 CSRF；他租户/不存在的 `X-Tenant-ID` 404；平台资源 403（NFR-14）；他租户与平台管理员对令牌的 GET/DELETE/rotate 404；令牌管理不接受代租户；acting-as 安全拒绝；成员被移除/租户停用 401）、`TestAPITokenShrinksWithCreator`（降权收缩、新增权限不扩大、禁用/管理员停用/删除 401 与恢复、真实管理 API 路径）、`TestAPITokenFollowsCreatorLoginPolicy`（强制 TOTP、须先改密）。
- [x] `TestXReauthOperationsEnforcedInChain`/`csrf` 去掉 Bearer 替身，改用真实令牌（带齐 11 个 x-reauth 路由所需 scopes 的令牌：全部 403 `auth.reauth_required`，handler 不执行）。
- [x] `TestAPITokenLastUsedThrottled`（首次写、窗口内 50 次不写、窗口后再写含新 IP）；`TestAPITokenManagementOwnershipAndList`（分页/排序白名单/他人 404/已吊销仍可读）；`TestAPITokenRoutesRegisteredAndMatchOpenAPI`（5 个路由与 OpenAPI 元数据逐项一致）。
- [x] 日志脱敏（G-5，补用例）：`TestAPITokenPlaintextNeverLeaks`（平台/租户/轮换令牌全流程使用后：日志、Redis、全库无明文）、`tests/security` 的 `TestLogScanAPITokenRuntimeShape`（运行期生成、含 `-`/`_` 的生产同形令牌经消息/敏感与不敏感字段/错误链/嵌套对象/Authorization/panic，全部遮蔽；只记前缀的合规写法不被遮蔽）；令牌与口令均运行期生成，G-6 不触发、无豁免。
- [x] `httpx.ClientIPFrom` 单测（可信代理采信 XFF、不可信来源忽略、多级、IPv6、不经路由器为空）。
- [x] 契约：5 个令牌管理 operation 的响应（201/200/204/400/403/404/422 等）经 kin-openapi 校验；`make openapi-check`/`openapi-breaking` 无破坏性变更。
- [x] 其他：`TestRouteMetadataMatchesOpenAPI`（/auth 路由 13 → 18）、`TestRoutesSubcommandExportsManifest`（路由清单 33 → 38）；`make openapi-check`：路由清单 38 条，「M1 未实现 operation 清单」167 → 162；`make race` 增加 `-timeout 30m`（竞态检测下 `tests/integration/auth` 单包约 10 分钟，已逼近 go test 默认的 10 分钟包超时——本 Story 新增 17 个集成测试后一次完整 race 运行曾因默认超时而失败，已放宽并重跑）。
- [x] 门禁：`make lint security gate-selftest race vuln contract-check build verify-build migrate-test` 全绿（见「开发记录」）。
- [x] sprint-status、本说明、本地提交（不 push）。

## 等待环境验证（本机通过不代表这些已验证）

1. **MySQL 8.0.16（最低版本）与 Linux 验收机**重跑 `tests/integration/auth`、`tests/integration/server`、`make migrate-test`：`api_tokens` 的 `(tenant_id <=> ?)`、`CAST(? AS JSON)`、`FOR UPDATE` 行为在 8.0.16 上的表现。
2. **真实反向代理下的来源 IP**：本机用 `httptest` 监听 127.0.0.1 + 配置 `TrustedProxies` 验证 `X-Forwarded-For` 取值规则；生产装配（`internal/platform/server`）沿用 `httpx.Options.TrustedProxies` 的默认值（仅信任本机反代 127.0.0.1/::1；配置文件尚未暴露该项，属 1.12/1.13 既有现状）。Nginx 与 Server 同机部署时成立；反代不在本机时 `allowed_cidrs` 会取到反代地址——须在真实部署形态上验证，必要时另开 Story 暴露该配置项。
3. **多 Server 实例**：`last_used_at` 节流是进程内的（每实例每令牌每分钟一次）；多实例并发吊销/使用的整链、Redis 缓存键清除在多实例间的行为。
4. **真实 Redis 部署形态**（Upstash/Redis 版本差异）下令牌映射缓存的 TTL 与键清除。
5. 产品/安全确认三项口径：默认有效期 90 天/上限 365 天 `[ASSUMPTION]`；轮换无宽限期；令牌管理仅限本人名下令牌且只能用 Cookie 会话。

## 遗留与风险

- **待 1.21 补写 audit_logs**：令牌创建/轮换/吊销当前只有结构化日志（`audit_logs_written=false`）；1.21 交付后以真实 Auditor 替换 `adminAuditor`，并记录 actor / 资源 / 变更摘要（禁含明文、哈希）。
- **完整 Scope 钩子属 1.20**：平台令牌的 acting-as（`X-Tenant-ID`）仍被过渡 Scope 钩子安全拒绝；租户令牌的 Scope 仅按令牌所属租户设定（授权决策 `own_tenant`）。1.20 交付后应补：acting-as 下的令牌（`tenant:impersonate` 令牌）整链、目标租户存在/可用性校验、令牌写操作审计里 actor 与目标租户。
- **租户配额 `api_tokens`**：`tenant_quotas` 已有 `api_tokens` 配额键，本 Story 未接线（租户/配额模块属后续 Story）；当前没有令牌数量上限（仅 scopes/CIDR 数量上限）。
- **门禁变更**：`api_tokens` 移出「禁入身份表」名单（见上）——请评审确认「Scope 之前以 token_hash 为键」的豁免理由。
- **缓存映射的残余作用**：Redis 只缓存「哈希 → id」，对 MySQL 的读取减少有限（仍有一次按主键的联表读取）；这是用「吊销不依赖缓存失效」换来的确定性。若后续压测显示热路径瓶颈，可加极短 TTL（≤5 秒）的状态缓存，但须接受该窗口内的陈旧。
- Bearer 在 `must_change_password`、强制 TOTP 下一律 401（与会话的「受限放行」不同：令牌没有「只能调用启用流程」的概念）。
- 日志里令牌前缀可见（约定的合规写法）；前缀是 40 位随机，不构成可用凭据。

## 开发记录

### 设计取舍

- **缓存什么**：权限/状态若缓存都会带来「降权/禁用/吊销延迟」，与「立即失效、创建者降权即收缩」冲突；因此只缓存不可变映射，其余现读。比「缓存整个认证结果 + 广播失效」简单且无失效竞态。
- **令牌管理的授权路径**：`rbac.Decide` 把平台主体对 `either` 路由的写操作判为「须 acting-as」，会让平台用户无法创建自己的平台令牌；令牌管理是「自有资源」，改走专门路径（权限 + 拒绝代租户 + 自身 Scope），不扩大 `Decide` 的语义。
- **来源 IP**：认证钩子只拿到 `*http.Request`；让路由器在链首把 Gin 的 `ClientIP()` 登记进请求上下文，比在钩子里重复解析 XFF 安全（同一信任模型，且不会漂移）。
- **不同 401 原因的耗时**：未做耗时均衡（令牌是 256 位随机值，不存在口令式枚举/猜测价值）；响应体逐字节一致。

### 实际验证（本机）

- 真实 MySQL（自拉起临时 mysqld，应用全部内嵌迁移 000001–000018、真实 `lingedge_app` 账号）+ 真实 redis-server 临时实例 + 真实 httpx 钩子链；Redis 故障注入为真实 SIGKILL/Restart；无 Mock。

### 门禁结果

（见提交前的最终运行记录：`gofmt` 无输出；`make lint`、`security`、`gate-selftest`、`race`、`vuln`、`contract-check`、`build`、`verify-build`、`migrate-test` 均通过——在最后一次改动之后完整重跑。）
