# ADR-018 认证与授权：会话、API Token、TOTP、RBAC、realm 隔离

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §16、§19、§24；PRD FR-17、FR-18、FR-25、FR-37、NFR-6、NFR-9、NFR-12；architecture AD-18｜阶段：M1（认证、RBAC、Token、TOTP）/ P5（租户自助注册与 Console）

## 背景

需要一套同时服务 Admin（运营者）、Console（租户）与机器调用（API Token）的认证授权方案；Admin 与 Console 是不同入口，权限必须隔离（基线 §2.2）；TOTP、登录节流、API Token 安全、CSRF 都是基线/PRD 的明确要求，且生产不得有固定默认口令（NFR-6）。表结构见 [database/platform-identity.md](../../database/platform-identity.md) §2。

## 决策

1. **身份模型**：`users` 为全局身份，`user_type ∈ platform|tenant`；租户归属经 `tenant_members`；角色 `roles.scope ∈ platform|tenant`，`user_roles` 可带 `tenant_id`。**Admin realm 仅允许 `platform` 用户建立会话，Console realm 仅允许 `tenant` 用户**。
2. **会话**：不透明令牌——32 字节 CSPRNG，Cookie 传明文，库内存 `SHA-256`（`sessions.token_hash`）。**不使用 JWT**（不需要无状态校验，且需即时吊销；golang-jwt 不采用）。Cookie：`le_admin_sid` / `le_console_sid`，`HttpOnly; Secure; SameSite=Lax; Path=/`，两个 realm **名称、有效范围、数据库 `realm` 列均不同，互不通用**；滑动过期 + 绝对过期（具体时长为系统设置，默认值待用户确认）；密码修改/管理员吊销使全部会话失效。热路径可用 Redis 缓存已验证会话（TTL≤60s，ADR-025），吊销时同时删除缓存。
3. **CSRF**：Cookie 会话的写请求（POST/PUT/PATCH/DELETE）必须带 `X-CSRF-Token`（服务端比对 `sessions.csrf_hash`，经 `GET /auth/csrf` 或登录响应获取）；同时校验 `Origin`/`Sec-Fetch-Site`；Bearer API Token 请求不使用 Cookie，不需要 CSRF。
4. **密码**：argon2id（`golang.org/x/crypto/argon2`，v0.59.0 为查询值，参数以实现时基准测定并写入配置），PHC 字符串存 `users.password_hash`；**无任何默认口令**，管理员密码由安装向导设定（FR-11、NFR-6）；`must_change_password` 用于重置后强制修改；弱口令策略可配置。
5. **TOTP**（近期 MFA 与再认证的区别见 §12）：`pquerna/otp`（v1.5.0 查询值，实现时核对）；密钥经信封加密（ADR-011）存 `totp_secret_enc`；启用时发放一次性恢复码（`user_recovery_codes`，仅存哈希）；系统设置可强制管理员启用 TOTP；TOTP 通过后会话记录 `mfa_verified_at`，敏感操作（改密码、创建 API Token、导出私钥相关）要求近期 MFA。
6. **登录节流**：按「账号 + IP」双维度计数。主路径 Redis（滑动窗口/计数，ADR-025）；降级路径 `users.failed_login_count/locked_until`（Redis 不可用时仍有效）；超限**统一返回 `429 rate_limit.exceeded`（含 `Retry-After`）**并写 `login_logs`（`login_failed`、`locked`）——不再使用 `423 Locked`/`auth.account_locked`（锁定态只会出现在存在的账号上，会形成账号存在性预言机，MIN-01）；**节流按 identifier 计数，不论账号是否存在**，故存在与不存在的账号响应一致；错误信息不区分「用户不存在」与「口令错误」；窗口与阈值为配置，默认值待基线测试后确定。
7. **API Token**：形如 `le_<8位前缀>_<随机>`；明文仅创建时展示一次；库存 `SHA-256(完整令牌)`（令牌为高熵随机值，无需慢哈希）+ 可检索 `token_prefix`；作用域 `scopes_json` 必须是创建者权限的**子集**；可选 `allowed_cidrs_json`、`expires_at`；吊销立即失效；轮换产生新令牌并记 `rotated_from_id`；日志只记前缀。租户令牌不能访问平台资源（NFR-14）。
8. **RBAC**：权限串 `resource:action`（如 `site:read`、`site:write`、`config:publish`、`node:manage`、`tenant:manage`、`cms:page:write`），清单与 `openapi.yaml` 的 `x-permissions` 一致（见 api/README 权限索引）；内置系统角色随迁移种子写入，**租户角色只能授予 `scope=tenant` 的权限**；授权判定 = 主体权限集 ∩ 路由声明；权限集缓存键含 `permission_version`，变更时递增并经 Redis pub/sub 清缓存，缓存失效兜底靠短 TTL（ADR-025）。
9. **动态菜单**：`GET /auth/me` 返回权限集、可用功能（`features`：运行模式、阶段开关）与租户上下文，前端据此裁剪菜单；**前端裁剪只是体验，服务端对每个请求独立授权**（ADR-014）。
10. **Admin 代租户**：新增权限 **`tenant:impersonate`**（`permissions.scope=platform`，api/README §4 权限索引已登记）。平台用户带 `X-Tenant-ID` 访问 `x-tenant-scope: tenant|either` 的路由须**同时**具备 `tenant:impersonate` 与该路由 `x-permissions` 的全部权限串（这些权限的 `scope ∈ {tenant, both}`；`both` = 平台角色代租户与租户角色均可持有）；内置 `platform_admin` 含全部。平台主体**不带** `X-Tenant-ID` 访问 `either` 路由 = 跨租户只读（只需路由权限）。审计同时记录 actor 与目标租户（ADR-009 §5）。
11. **运行模式**：`self_hosted` 下商业类 API（订单/充值/代理商等）返回 `403 feature.disabled`（FR-24）。
12. **再认证（L4 step-up，IMP-12）**：UX 的“L4 重新验证身份”落为 `POST /auth/reauth`：口令；已启用 TOTP 的用户另需 TOTP；通过后写 `sessions.reauth_at`。声明 `x-reauth: true` 的 operation 要求 `NOW(3) - reauth_at ≤ auth.reauth_window_s`（默认 300 秒 `[ASSUMPTION]`，`system_settings` 可配），否则 `403 auth.reauth_required`（前端据此弹出验证）。与 §5 的“近期 MFA”区别：近期 MFA 是 TOTP 通过时间（改密码、创建 API Token 等），再认证是对高危操作的**即时口令确认**，两者可同时要求。L4 清单：改运行模式、创建/提权平台管理员与角色/角色权限变更、重置管理员口令、吊销节点证书、平台 SSRF 白名单、签名密钥轮换（P2）、导出/删除密钥类操作；清单随 OpenAPI 的 `x-reauth` 标记生成并同步到 EXPERIENCE。
13. **审计与日志**：登录、登出、MFA、令牌创建/吊销、权限变更均写 `audit_logs`/`login_logs`；口令、令牌、TOTP 密钥不入任何日志（NFR-12）。

## Binds / Prevents / Rule

- 约束：auth 模块、全部 handler 中间件（含再认证中间件）、前端登录流程、Tools 安装向导（创建管理员）。
- 防止：Admin 与 Console 会话互通；令牌明文落库；节流依赖单点 Redis；租户令牌越权到平台资源。
- 规则（AD-18）：**会话为不透明令牌并按 realm 隔离，机器调用用 API Token，权限为 `resource:action` 串；任何请求的授权在服务端独立完成。**

## 备选方案与权衡

- **JWT 无状态会话**：否决——吊销与权限变更不及时，需额外黑名单，复杂度与不透明令牌+DB 相当，却失去即时失效。
- **Admin/Console 共用一个 Cookie + 角色区分**：否决——一个 XSS 或会话窃取即跨越入口，违背基线 §2.2 的隔离意图。
- **OAuth2/OIDC 内建授权服务器**：否决（M1）——超出范围；外部 SSO 作为后续插件。
- **Redis 存会话为唯一来源**：否决——违反 ADR-025（Redis 不是真相源）。

## 后果

- 每次请求的会话校验需 DB/Redis 查询；以 Redis 缓存缓解（降级到 DB 仍正确）。
- RBAC 的权限串是跨文档契约（OpenAPI、种子数据、前端菜单）；新增权限需同步三处，CI 检查 `x-permissions` 均存在于种子。
- argon2id 参数过高会拉高登录延迟与内存，过低削弱安全；需基准测试。

## 可被推翻处

若引入外部 IdP/SSO：保留会话机制，仅替换「凭据校验」步骤；若放宽为单 Cookie，需同步放弃 FR-37 的 realm 验收。TOTP 是否对管理员强制为系统设置，用户可调整。

## 验证

- 单元/集成：登录失败节流、TOTP 缺失/错误无法登录、令牌吊销即时失效、Token 无法超出作用域/租户（FR-17、FR-25）。
- 安全测试：CSRF（缺头/错头被拒）、会话固定、realm 互用（Admin Cookie 访问 Console 端点被拒，反之亦然）、日志脱敏扫描（G 系列门禁，threat-model）。
- 隔离矩阵：租户 A 凭据访问租户 B 资源 → 404（ADR-009）；Redis 不可用时登录与节流仍工作（ADR-025）。

## 修订记录 2026-11-16

- §6（MIN-01）：登录节流统一 `429 rate_limit.exceeded`，移除 423/`auth.account_locked`；按 identifier 计数，避免账号枚举。原因：ADR 与 OpenAPI/README 对锁定态码不一致，且锁定态只出现在存在的账号上。
- §10（IMP-12、M18）：定义 `tenant:impersonate` 与 acting-as 授权模型、`permissions.scope=both` 语义、`either` 路由的授权矩阵。
- §12（IMP-12）：新增再认证 `POST /auth/reauth`、`sessions.reauth_at`、窗口 `auth.reauth_window_s`、错误码 `auth.reauth_required`、`x-reauth` 路由元数据与 L4 清单。原因：UX 要求 L4 重新验证身份，但契约中无 API/错误码/窗口，权限串字典也缺代租户权限。

## 修订记录 2026-11-21（Story 1.17，实现细化，非推翻）

- §5（TOTP）：落地细节——① 密钥：`pquerna/otp` v1.5.0（SHA-1/6 位/30 秒，20 字节密钥），Base32 经 `pkg/secretbox` 信封加密存 `users.totp_secret_enc`，AAD=`users|totp_secret_enc|<uid>`；② 防重放：`users.totp_last_step`（000017）原子 compare-and-set，step 严格递增，多实例一致、不依赖 Redis；允许 ±1 个 time-step 漂移；③ 两步登录：`login` 口令正确且已启用 TOTP 时返回 `200 {status: totp_required, mfa_token}`（取代 1.16 的临时 `401 auth.totp_required` fail-closed），`mfa_token` 存 `login_mfa_challenges`（000016，只存哈希，5 分钟、一次性、至多 5 次验证码尝试），失败计入登录节流并写 `login_logs.totp_failed`；④ 恢复码：10 个、各 80 位 CSPRNG，库存 SHA-256（高熵随机数无需慢哈希；确定性哈希使「用掉一个码」可原子完成），单次有效；⑤ 「近期 MFA」窗口：`auth.mfa_window_s`（默认 900 秒 `[ASSUMPTION]`，基线与 UD 均未规定），仅对已启用 TOTP 的账号生效（未启用者无第二因子可验证，由「强制管理员启用 TOTP」策略引导其启用），不满足时 `403 auth.reauth_required`（`details.field=mfa_verified_at`），经 `POST /auth/reauth`（口令 + 验证码）刷新；Bearer API Token 无会话，永远不能满足。
- §12（再认证）：落地细节——`POST /auth/reauth` 仅限 Cookie 会话；失败计入登录节流；成功写 `sessions.reauth_at`（已启用 TOTP 时同时写 `mfa_verified_at`）；`x-reauth: true` 的检查是可复用组件 `httpx.ReauthChecker`，经 `httpx.WithReauth` 在授权链中统一执行（位于授权钩子之前；1.18 交付真实授权钩子后可改为「先权限后再认证」）；窗口边界 `now - reauth_at ≤ auth.reauth_window_s`（默认 300 秒，UD-29）。契约中声明 `x-reauth` 的 operation 实为 11 个（`scripts/contract/data/x-reauth.txt` 已注明以契约为准，修订日志与 epics 中「10 个」为旧口径）。
- 审计：`audit_logs` 的统一接入属 Story 1.21；在此之前，启用/停用 TOTP 与再认证成功只留结构化日志，失败写 `login_logs`（`totp_failed`/`login_failed`/`locked`）。

## 修订记录 2026-11-21（Story 1.18，实现细化，非推翻）

- §8（RBAC）：落地细节——① **权限注册表**是代码常量（`internal/platform/rbac`），与种子迁移 `000004` 的 `permissions` 行、`openapi.yaml` 的 `x-permissions` 三方一致，由 `make perms-check`（`scripts/gates perms`，已接入 `make contract-check`；负向样本在 `scripts/gates/perms_test.go`）机器核对，并核对权限 scope 与路由 `x-tenant-scope` 相容（platform 路由不得引用 tenant 权限；tenant/either 路由不得引用 platform 权限，否则 acting-as 永远无法授权）；② 授权判定 = 路由 `x-permissions` **须全部具备**（空表示任何已认证主体）；租户角色（含内置模板）只能授予 `scope ∈ {tenant, both}` 的权限（`both` 与 §10 一致：平台角色与租户角色均可持有），API 与种子均强制，库级不变量由测试扫描；③ **权限缓存**键 `le:<env>:auth:perms:<用户 uid>:s<范围>:v<permission_version>`，TTL ≤ 60 秒；`users.permission_version`（`000018`，CC-6）随会话同行读出，权限/角色变更时同事务 +1，旧版本缓存项即刻不可达（变更立即生效，不依赖广播）；变更提交后删除旧键并经 `redisx.PurposePermVersion` Pub/Sub 广播（有损），订阅端删除旧键，**轮询兜底**从 MySQL 对账最近变更的用户，TTL 是库被带外修改时的陈旧上限；Redis 不可用时直读 MySQL；④ **防提权**：授予角色（含创建用户时指定角色）、向角色加权限，要求操作者自己持有这些权限；创建用户时指定角色另需 `role:write`；⑤ **最后一个超级管理员**（内置 `platform_admin` 的有效持有者）不得被删除、停用或移除该角色：所有可能减少它的操作先取得 `platform_admin` 角色行的事务锁再计数（并发竞态被串行化），内置角色本身不可改/删/改权限。
- §10（代租户）：授权矩阵由纯函数 `rbac.Decide` 实现并表驱动穷举测试：平台主体不带 `X-Tenant-ID` 访问 `either` 路由 = 跨租户**只读**（`either` 路由的写操作与 `tenant` 路由一样必须经 acting-as，缺 `X-Tenant-ID` → 400）；带 `X-Tenant-ID` 须同时具备 `tenant:impersonate` 与路由权限（且这些权限 scope 为 tenant/both）；租户主体带他人租户的 `X-Tenant-ID` → 404（先于权限判定，不泄漏存在性）。授权钩子放行时把决策（`own_tenant | platform | cross_tenant_read | acting_as`）交给 Scope 钩子（`httpx.RecordAccess/AccessFrom`）。**边界**：解析 `X-Tenant-ID`（租户 uid → 租户）并校验目标租户存在/可用属 Story 1.20 的 Scope 钩子；在此之前 `httpx.WithAccessScope` 只把不需要解析目标租户的决策落成 Scope，acting-as 一律安全拒绝（403），不放行未实现的部分。
- §12（再认证）：**顺序调整为先权限后再认证**（Story 1.17 遗留）：`httpx.WithReauth` 先运行授权钩子，通过后才对 `x-reauth` 路由运行 `ReauthChecker`——无权限者得到 `403 auth.forbidden`，不会被提示去再认证。
- §13（审计）：`audit_logs` 的统一接入属 Story 1.21；在此之前 `/admin/users|roles|permissions` 的写操作只写不含敏感信息的结构化日志（operation、操作者、状态、`request_id`，明确标注 `audit_logs_written=false`），并仅对这些 operation 放宽审计钩子的安全拒绝默认（其余要求审计的 operation 仍被拒绝）。**待 Story 1.21 补写 audit_logs。**

## 修订记录 2026-11-21（Story 1.19，实现细化，非推翻）

- §7（API Token）：落地细节——① **形态**：`le_<8 位前缀>_<43 位随机>`；前缀是 5 字节 CSPRNG 的小写 base32（`[a-z2-7]{8}`，可检索，唯一索引冲突时重试），随机部分是 32 字节 CSPRNG 的 base64url；库存 `SHA-256(完整令牌)` 十六进制与前缀，明文只在创建/轮换响应里出现一次（`Cache-Control: no-store`），不入库（含 `idempotency_keys`：`createApiToken` 经 `httpx.SetReplayBody` 登记去掉明文的响应体，同键重放返回令牌元数据而不再回显明文）、不入 Redis、不入日志（日志只记前缀）；② **所有权与范围**：令牌只属于创建者本人：平台用户创建平台令牌（`tenant_id` NULL），租户用户创建其当前租户的令牌；令牌管理 operation（`/auth/api-tokens*`）只作用于调用者自己名下的令牌（他人的与不存在的一样 404），**只能用 Cookie 会话调用**——Bearer 一律拒绝（令牌不能创建/轮换/吊销/列出令牌，也不能调用任何自助 operation）；创建与轮换要求近期 MFA（§5）；不接受代租户：平台主体带 `X-Tenant-ID` → 400，租户主体带他人租户 → 404；③ **scopes**：必须是权限字典内的权限串、是创建者**当前**权限的子集（越权 403、字典外 400），`token:manage` 不可授予令牌；租户创建者天然只能授予 `scope ∈ {tenant, both}` 的权限；④ **有效权限 = 令牌 scopes ∩ 创建者当前权限**：每个请求按创建者当前的 `permission_version` 装载权限集（权限缓存键含版本，立即生效）再与 scopes 取交集——创建者被降权后令牌随之收缩，被禁用/删除/移出租户/租户停用则令牌 401，创建者新增权限不会扩大令牌；强制管理员 TOTP 与「须先改密」同样约束令牌（401）；⑤ **认证**：不使用 Cookie 与 CSRF（NFR-9）；令牌过期、吊销、来源 IP 不在 `allowed_cidrs`、哈希不符、格式错误、令牌不存在一律同一个 `401 auth.unauthenticated`（响应不泄漏原因；原因与前缀只写服务端日志，并以 `login_logs.token_auth_failed` 记录——同一令牌至多每分钟一条）；租户令牌访问平台资源（platform 路由）由 `rbac.Decide` 拒绝 403（NFR-14），他租户 `X-Tenant-ID` 404，超出 scopes 403（FR-25）；Bearer 请求不能满足近期 MFA 与再认证（§5、§12），且只能调用声明了 `x-permissions` 的业务路由；⑥ **来源 IP**：取自 `httpx.ClientIPFrom`——与访问日志/节流同一取值（Gin `ClientIP`，遵守 `TrustedProxies`：只有来自可信反代的连接才采信 `X-Forwarded-For`，取最右侧的非可信地址），伪造 `X-Forwarded-For` 不能绕过 `allowed_cidrs`；`allowed_cidrs` 接受 IPv4/IPv6 CIDR 与单个 IP（折叠 IPv4 映射地址，拒绝 `/0` 与带 zone 的地址）；来源未知（不经路由器）则 fail-closed；⑦ **Redis（ADR-025）**：只缓存不可变的「哈希 → 令牌 id」映射（`le:<env>:auth:apitoken:<sha256>`，TTL ≤ 60 秒）；吊销/过期/创建者状态/成员关系/权限版本**每个请求都从 MySQL 现读**，所以吊销立即生效、删除缓存键只是卫生措施（吊销时先删、提交、再删；Redis 不可用/删除失败/残留旧值都不能让已吊销的令牌复活；Redis 缺失/故障直读 MySQL）；⑧ **轮换**：新令牌继承名称/`allowed_cidrs`/有效期时长，scopes 与创建者当前权限取交集（全部失去则 403），记 `rotated_from_id`，**旧令牌立即吊销、无宽限期**（ADR 与 OpenAPI 均写「新令牌 + 吊销旧令牌」，宽限期需要额外的列与状态机；推翻则以新迁移加 `grace_until` 并改契约）；已吊销的令牌不能轮换（404）；⑨ **有效期**：`expires_at` 须在未来且不超过系统上限；省略时取默认有效期——基线与 UD 均未规定，工程假设 **默认 90 天、上限 365 天** `[ASSUMPTION]`，均为 `system_settings` 可配置项（`auth.api_token_default_ttl_s` / `auth.api_token_max_ttl_s`，UD-27 同口径，改默认值只是配置改动）；⑩ `last_used_at`/`last_used_ip` 每令牌每进程至多每分钟异步写一次（不阻塞请求）。
- §13（审计）：`audit_logs` 的统一接入属 Story 1.21；在此之前令牌创建/轮换/吊销只写不含敏感信息的结构化日志（前缀、令牌 uid、操作者 uid、`request_id`，明确标注 `audit_logs_written=false`），并仅对 `/auth/api-tokens*` 的 operation 放宽审计钩子的安全拒绝默认（与 `/admin/*` 同口径）。**待 Story 1.21 补写 audit_logs。**


## 修订记录 2026-11-21（Story 1.20，实现细化，非推翻）

- §10（Admin 代租户）：acting-as 的完整链路落地——授权钩子（`rbac.Decide`：`tenant:impersonate` + 路由权限，权限 scope ∈ {tenant, both}）→ Scope 钩子解析 `X-Tenant-ID` 并经租户目录校验目标租户**存在且状态 `active`**（否则 404，与他租户资源逐字节一致）→ 铸造 `ActingAs` Scope 并登记审计上下文。**API Token 经 Bearer 进入同一 Scope 生成**：租户令牌的 Scope 是令牌所属租户（他租户 `X-Tenant-ID` → 404）；平台令牌 acting-as 同样须令牌 scopes 含 `tenant:impersonate` 与路由权限（令牌权限 = scopes ∩ 创建者当前权限），目标租户存在性校验与会话一致（Story 1.19 遗留已补）。
- §7（Bearer 可调用范围）：除「声明了 `x-permissions` 的业务路由」外，其他模块「无权限要求、只返回调用者自身租户上下文」的路由（`GET /tenants/current`）也允许 Bearer；本模块的自助 operation（`/auth/*`）与令牌管理仍要求 Cookie 会话。
- §13（审计）：audit 模块（Story 1.21）仍未实现——**待 1.21 写 audit_logs**；租户模块的写 operation（`updateTenant`/`addTenantMember`/`putTenantQuotas`）与 `/admin/*`、`/auth/api-tokens*` 同口径走过渡的结构化日志（`audit_logs_written=false`），acting-as 请求的日志同时带操作者与目标租户。
