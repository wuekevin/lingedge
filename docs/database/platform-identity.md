---
title: "数据库：平台、身份与基础设施表"
status: draft
created: 2026-11-16
updated: 2026-11-16（依据架构评审修订，见 docs/bmad/architecture-revision-log.md）
---

# 平台、身份与基础设施表

> 返回 [README（约定/ERD/索引）](README.md) · [architecture.md](../architecture/architecture.md)。通用列简写 C1/C2/T/T?/SD 见 README §2.1。「默认 —」表示无默认值。

## 1. 系统

### 1.1 `system_settings`　**M1**

用途：平台级键值配置（运行模式、品牌、登录策略、保留期、邮件等）。平台级表（无 tenant_id）。
通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| setting_key | VARCHAR(128) | N | — | 点分键，如 `system.run_mode`（`self_hosted`/`commercial`）、`brand.name`、`login.totp_required_for_admin`、`retention.audit_days` |
| value_json | JSON | Y | — | 非敏感值 |
| value_enc | TEXT | Y | — | 敏感值信封密文（5 段：`v1:<kek_id>:<wrapped_dek>:<nonce>:<ct>`，README §2.2、ADR-011） |
| group_name | VARCHAR(48) | N | — | 分组：general/brand/login/mail/api/security/retention |
| updated_by | BIGINT UNSIGNED | Y | — | FK → users(id) ON DELETE SET NULL |

- 索引：`uk_system_settings_key(setting_key)`、`idx_system_settings_group(group_name)`。
- 约束：`ck_system_settings_one_value`：`(value_json IS NULL) <> (value_enc IS NULL)`。
- 保留：永久。变更写审计。
- **键注册表（Story 1.22）**：应用层的类型化键注册表是 `internal/platform/sysconfig/registry.go`（键、类型、范围/枚举/格式校验、默认值、分组、是否敏感、是否可经通用 `PATCH /admin/settings` 修改）——**该文件是认证模块读取键的唯一来源**（`TestAuthKeysMatchRegistry` 核对）；写入经 schema 校验、与审计同一事务；敏感键（如 `mail.smtp_password`）只存 `value_enc`（`value_json` 为 NULL），AAD = `system_settings|value_enc|<uid>`，读取脱敏；未设置的键按注册表默认值生效（库里没有行）；`null` = 删除行恢复默认。新增键：`general.registration_open|admin_url|console_url|docs_url|status_url|locales`、`brand.name|name_en|logo_media_uid|favicon_media_uid|primary_color|support_email`、`mail.smtp_host|smtp_port|smtp_username|smtp_password|from_address`、`retention.audit_days|login_logs_days`（仅存储，清理由 3.48 执行）。`system.run_mode` 与 `ssrf.allow_cidrs` 只能经需再认证的专用端点修改；`capability.<name>.status` 不存库（恒 `unverified`，NFR-27）。
- **已定义的键**（节选，其余随功能追加；键名为跨文档契约）：`system.run_mode`、`brand.*`、`login.*`、`retention.*`、`deploy.lineage`（BIGINT，初值 1；DB 恢复后由 `lingedge-server lineage bump` 提升，ADR-002 §12）、`ssrf.allow_cidrs`（平台 SSRF 白名单 CIDR 数组，写需 `setting:write` + 再认证，进入各集群 `global` 块的 `ssrf_allow_cidrs`，ADR-004 §6）、`auth.reauth_window_s`（L4 再认证窗口，默认 300，UD-29 采纳，ADR-018）、`auth.mfa_window_s`（近期 MFA 窗口，默认 900 `[ASSUMPTION]`：基线与 UD 均未规定，Story 1.17）、`auth.api_token_default_ttl_s` / `auth.api_token_max_ttl_s`（API Token 默认有效期 / 系统上限，秒；代码默认 7776000（90 天）/ 31536000（365 天）`[ASSUMPTION]`，UD-27 同口径，Story 1.19）、`login.totp_required_for_admin`（JSON 布尔，强制管理员启用 TOTP，缺省 false）、`auth.session_idle_ttl_s` / `auth.session_absolute_ttl_s`（会话滑动/绝对过期，秒；代码默认 7200 / 43200 `[ASSUMPTION]`，UD-27 待基线，Story 1.16）、`login.throttle_window_s` / `login.throttle_account_max` / `login.throttle_ip_max`（登录节流窗口与「账号 / 来源 IP」两个维度的失败阈值；代码默认 900 秒 / 10 / 30 `[ASSUMPTION]`，UD-27，Story 1.16）、`placement.default_cluster_uids`（租户建站时由平台默认绑定的集群，租户不可选，ADR-009 §5）、`grpc.tls.san_extra`（Server 叶证书额外 SAN）、`website.revalidate_secret`（`value_enc`，P2）。

### 1.2 `system_installations`　**M1**

用途：安装状态镜像（Tools 本地 `state.json` 为事实源，Server 记录自身与上报的组件实例，供 Admin「版本信息/节点升级」展示）。
通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| installation_id | CHAR(26) | N | — | 平台安装 ID（同一部署内相同），安装向导生成 |
| component | VARCHAR(32) | N | — | server/admin-web/console/website/node/dns/scheduler/worker/reporter/docs/status |
| instance_name | VARCHAR(64) | N | — | 实例名（单机多实例时区分） |
| host | VARCHAR(255) | N | — | 主机名或节点 uid |
| version | VARCHAR(64) | N | — | 语义化版本 |
| channel | VARCHAR(16) | N | 'stable' | 发行通道 |
| status | VARCHAR(16) | N | 'installed' | `installed/upgrading/failed/uninstalled` |
| installed_at | DATETIME(3) | N | — | |
| last_upgraded_at | DATETIME(3) | Y | — | |
| install_locked_at | DATETIME(3) | Y | — | 仅 `server` 行：安装向导完成并写 `install.lock` 的时间 |
| meta_json | JSON | Y | — | 路径/端口/进程归属摘要（不含凭据） |

- 唯一：`uk_system_installations_inst(component, host, instance_name)`；索引 `idx_system_installations_install(installation_id)`。
- CHECK：status 取值。保留：永久。

## 2. 身份与权限

### 2.1 `users`　**M1**

用途：全局身份（平台管理员与租户用户同表）。租户归属经 `tenant_members`。平台级表（无 tenant_id）。
通用列：C1 + SD。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| user_type | VARCHAR(16) | N | — | `platform`（运营者）/`tenant`；决定可登录的 realm（admin 仅 platform） |
| email | VARCHAR(255) | N | — | 小写化后存储 |
| active_email | VARCHAR(255) | Y | — | 生成列 `IF(deleted_at IS NULL, email, NULL)` STORED |
| display_name | VARCHAR(128) | N | — | |
| password_hash | VARCHAR(255) | N | — | argon2id PHC 字符串；无明文、无默认口令 |
| status | VARCHAR(16) | N | 'active' | `active/disabled/locked/pending` |
| locale | VARCHAR(8) | N | 'zh-CN' | `zh-CN`/`en` |
| timezone | VARCHAR(48) | N | 'UTC' | |
| totp_enabled | TINYINT(1) | N | 0 | |
| totp_secret_enc | TEXT | Y | — | TOTP 密钥信封密文（5 段，README §2.2）；AAD=`users|totp_secret_enc|<uid>`（Story 1.17）。setup 后、enable 前为「待确认」密文（`totp_enabled=0`） |
| totp_last_step | BIGINT UNSIGNED | Y | — | 已用的 TOTP time-step（`unix/30`）；原子 compare-and-set 实现「同一验证码在窗口内重放被拒」。**000017 追加**（Story 1.17、CC-5） |
| permission_version | BIGINT UNSIGNED | N | 0 | 权限集版本号：用户的角色授予变化、或其任一角色的权限变化时，同一事务内 +1；认证钩子随会话读取，权限缓存键含它（ADR-018 §8）。**000018 追加**（Story 1.18、CC-6） |
| must_change_password | TINYINT(1) | N | 0 | 初始/重置后强制修改 |
| password_changed_at | DATETIME(3) | Y | — | |
| failed_login_count | INT UNSIGNED | N | 0 | 连续失败计数（节流以 Redis 为主，此处为兜底） |
| locked_until | DATETIME(3) | Y | — | |
| last_login_at | DATETIME(3) | Y | — | |
| last_login_ip | VARBINARY(16) | Y | — | |

- 唯一：`uk_users_active_email(active_email)`。索引：`idx_users_type_status(user_type, status)`。
- CHECK：`user_type IN ('platform','tenant')`；`status IN (…)`；`totp_enabled=0 OR totp_secret_enc IS NOT NULL`；`ck_users_email_lower`：`BINARY email = BINARY LOWER(email)`（应用层小写化的库层兜底，README §2.2）。
- 保留：软删除后 PII 脱敏任务（待用户确认 OQ-6）；审计引用保留 `actor_label` 快照。

### 2.2 `user_recovery_codes`　**补充 · M1**

用途：TOTP 恢复码（一次性）。C2。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| user_id | BIGINT UNSIGNED | N | — | FK → users(id) ON DELETE CASCADE |
| code_hash | CHAR(64) | N | — | SHA-256（高熵随机码） |
| used_at | DATETIME(3) | Y | — | |

- 唯一：`uk_user_recovery_codes(user_id, code_hash)`。保留：随用户。
- 实现（Story 1.17）：启用 TOTP 时一次性发放 10 个码（各 80 位 CSPRNG，Base32 16 字符），`code_hash = SHA-256("lingedge/recovery/v1\0" + 规整后的码)`；码本身是高熵随机数，故不用慢哈希（离线穷举 2^80 不可行；确定性哈希才能按 `(user_id, code_hash)` 原子地「用掉」一个码）。单次有效：`UPDATE … SET used_at = ? WHERE … AND used_at IS NULL`。停用 TOTP 或重新启用时整批删除/替换。

### 2.2.1 `login_mfa_challenges`　**追加 · 000016**

用途：登录第一步（口令正确、账号已启用 TOTP）发放的一次性 `mfa_token` 挑战（OpenAPI `LoginResponse.mfa_token`：5 分钟、一次性）。存 MySQL 而非 Redis，因此多实例一致、Redis 不可用时仍有效（ADR-025）。C2，无 `uid`（不对外引用）。**不属 M1 冻结的 61 表**（Story 1.17、CC-5）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| id | BIGINT UNSIGNED | N | AUTO_INCREMENT | |
| created_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) | |
| token_hash | CHAR(64) | N | — | SHA-256(32 字节 CSPRNG 令牌)；明文只在登录响应里出现一次 |
| user_id | BIGINT UNSIGNED | N | — | FK → users(id) ON DELETE CASCADE |
| realm | VARCHAR(16) | N | — | `admin`/`console`（CHECK） |
| ip | VARBINARY(16) | Y | — | |
| expires_at | DATETIME(3) | N | — | 发放后 5 分钟 |
| attempts | SMALLINT UNSIGNED | N | 0 | 验证码失败次数；达到 5 次即作废 |
| consumed_at | DATETIME(3) | Y | — | 成功使用时间（原子 UPDATE，保证一次性） |

- 唯一：`uk_login_mfa_token(token_hash)`。索引：`idx_login_mfa_user(user_id)`、`idx_login_mfa_exp(expires_at)`。
- 保留：创建新挑战时顺带清理过期一小时以上的行（`DELETE … LIMIT 100`）；其余随用户级联删除。

### 2.3 `roles`　**M1**

用途：角色（内置与租户自定义）。通用列：C1 + `tenant_id`（T?：NULL=平台内置/平台定义角色；非空=租户自定义，P5）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| scope | VARCHAR(16) | N | — | `platform`/`tenant` |
| code | VARCHAR(64) | N | — | 如 `platform_admin`、`site_operator`、`tenant_owner` |
| name | VARCHAR(128) | N | — | 展示名（i18n key 或文本） |
| description | VARCHAR(512) | Y | — | |
| is_builtin | TINYINT(1) | N | 0 | 内置不可删 |
| uniq_key | VARCHAR(160) | N | — | 生成列 `CONCAT(scope,':',COALESCE(tenant_id,0),':',code)` **VIRTUAL**（STORED 在 tenant_id FK 为 CASCADE 时 ERROR 1215，实测） |

- 唯一：`uk_roles_uniq(uniq_key)`。FK：tenant_id → tenants(id) **ON DELETE RESTRICT**（2026-11-16 由 CASCADE 改 RESTRICT，README §2.4）。CHECK：scope 取值；`scope='platform' → tenant_id IS NULL`（FK 为 RESTRICT/CASCADE 时可建，实测）。

### 2.4 `permissions`　**M1**

用途：权限点字典（启动时/种子迁移同步）。平台级；无 uid。C2（无 created_at 之外字段）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| code | VARCHAR(96) | N | — | `resource:action`，如 `site:write`、`config:publish`、`node:manage` |
| resource | VARCHAR(48) | N | — | |
| action | VARCHAR(48) | N | — | |
| scope | VARCHAR(16) | N | — | `platform`/`tenant`/`both`。`both` = 平台角色（代租户 acting-as）与租户角色均可持有；平台主体带 `X-Tenant-ID` 访问 `tenant|either` 路由须同时具备 `tenant:impersonate`（scope=platform）与该路由 `x-permissions` 全部权限（scope ∈ {tenant, both}），见 ADR-018 §10 |
| module | VARCHAR(32) | N | — | 所属模块 |
| menu_key | VARCHAR(64) | Y | — | 控制 Admin/Console 菜单显隐的键 |
| description | VARCHAR(255) | Y | — | |

- 唯一：`uk_permissions_code(code)`。CHECK：scope 取值；`code = CONCAT(resource, ':', action)`。

### 2.5 `role_permissions`　**补充 · M1**

C2。`role_id` FK→roles CASCADE；`permission_id` FK→permissions CASCADE。唯一 `uk_role_permissions(role_id, permission_id)`。

### 2.6 `user_roles`　**M1**

用途：用户-角色授予。C2。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| user_id | BIGINT UNSIGNED | N | — | FK → users(id) ON DELETE CASCADE |
| role_id | BIGINT UNSIGNED | N | — | FK → roles(id) ON DELETE CASCADE |
| tenant_id | BIGINT UNSIGNED | Y | — | NULL=平台角色；非空=该租户内角色；FK → tenants(id) **RESTRICT** |
| tenant_key | BIGINT UNSIGNED | N | — | 生成列 `COALESCE(tenant_id,0)` **VIRTUAL**（T? 统一定义，README §2.1） |
| granted_by | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |

- 唯一：`uk_user_roles(user_id, role_id, tenant_key)`；索引 `idx_user_roles_tenant(tenant_id, user_id)`。
- 业务约束（Service）：平台角色只授予 `user_type='platform'`；租户角色要求 `tenant_members` 存在。

### 2.7 `sessions`　**M1**

用途：Cookie 会话（ADR-018）。C2；会话标识为令牌哈希；**对外引用 `uid`**（迁移 `000015`、CC-4）：OpenAPI 的 `session_ref`/`{sessionId}` 是 ULID，不暴露自增 id 与令牌哈希。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| uid | CHAR(26) | Y | — | 对外引用（ULID，`session_ref`）；000015 追加，应用写入路径永远带值；唯一 `uk_sessions_uid` |
| token_hash | CHAR(64) | N | — | SHA-256(32B 随机令牌)；明文只在 Cookie |
| csrf_hash | CHAR(64) | N | — | CSRF 令牌哈希：`SHA-256(csrf)`，其中 `csrf = base64url(HMAC-SHA256(key=会话令牌原值, "lingedge/csrf/v1"))`——由会话令牌派生，故 `/auth/me`、`/auth/csrf` 对同一会话返回同一令牌（Story 1.16） |
| user_id | BIGINT UNSIGNED | N | — | FK → users(id) ON DELETE CASCADE |
| realm | VARCHAR(16) | N | — | `admin`/`console`（互不通用） |
| tenant_id | BIGINT UNSIGNED | Y | — | Console 当前租户；FK → tenants(id) **RESTRICT** |
| ip | VARBINARY(16) | Y | — | |
| user_agent | VARCHAR(512) | Y | — | |
| mfa_verified_at | DATETIME(3) | Y | — | 第二因子（TOTP/恢复码）通过时间：两步登录建立会话时写入；`enable` 与含验证码的 `POST /auth/reauth` 刷新。改密码、创建 API Token 等敏感操作要求 `NOW - mfa_verified_at ≤ auth.mfa_window_s`（已启用 TOTP 的账号；Story 1.17） |
| reauth_at | DATETIME(3) | Y | — | 最近一次 `POST /auth/reauth`（L4 再认证）通过时间；`x-reauth` 路由要求 `NOW(3) - reauth_at ≤ auth.reauth_window_s`，否则 `403 auth.reauth_required`（ADR-018） |
| last_seen_at | DATETIME(3) | N | — | |
| expires_at | DATETIME(3) | N | — | 滑动过期 |
| absolute_expires_at | DATETIME(3) | N | — | 绝对过期 |
| revoked_at | DATETIME(3) | Y | — | |
| revoke_reason | VARCHAR(32) | Y | — | `logout/admin/password_change/expired` |

- 唯一：`uk_sessions_token(token_hash)`。索引：`idx_sessions_user(user_id, revoked_at)`、`idx_sessions_exp(expires_at)`。CHECK：realm 取值。
- 保留：过期/吊销后 30 天 `[ASSUMPTION]`。

### 2.8 `api_tokens`　**M1**

用途：机器访问令牌。通用列：C1 + T?（NULL=平台令牌，仅平台用户可创建）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| user_id | BIGINT UNSIGNED | N | — | 创建者/所有者，FK → users(id) CASCADE |
| name | VARCHAR(128) | N | — | |
| token_prefix | CHAR(8) | N | — | 令牌 `le_<prefix>_<secret>` 的前缀，可检索 |
| token_hash | CHAR(64) | N | — | SHA-256(完整令牌)；明文仅创建时展示一次 |
| scopes_json | JSON | N | — | 权限串数组，必须是创建者权限子集 |
| allowed_cidrs_json | JSON | Y | — | 可选来源 IP 白名单 |
| expires_at | DATETIME(3) | Y | — | |
| last_used_at | DATETIME(3) | Y | — | |
| last_used_ip | VARBINARY(16) | Y | — | |
| revoked_at | DATETIME(3) | Y | — | 吊销立即失效 |
| rotated_from_id | BIGINT UNSIGNED | Y | — | 轮换来源，FK → api_tokens(id) SET NULL |

- 唯一：`uk_api_tokens_prefix(token_prefix)`、`uk_api_tokens_hash(token_hash)`。索引：`idx_api_tokens_tenant(tenant_id, user_id)`。
- 保留：吊销后 90 天物理删除 `[ASSUMPTION]`。
- **Story 1.19 用法**（表结构未变）：`token_hash` = SHA-256(完整令牌) 十六进制，`token_prefix` = 前缀 8 位（`[a-z2-7]`）；`tenant_id` NULL = 平台令牌（创建者须是平台用户），非 NULL = 该租户内的令牌（创建者须是该租户的有效成员）；`scopes_json` 为排序去重的权限串数组；`allowed_cidrs_json` 为规范化 CIDR 数组（NULL = 不限来源）；`expires_at` 恒非空（默认有效期见 ADR-018 修订记录 2026-11-21）；`last_used_*` 节流异步写；吊销写 `revoked_at`（行保留至保留期满）；轮换新行 `rotated_from_id` 指向旧行（旧行同事务吊销）。

### 2.9 `login_logs`　**补充 · M1**

用途：登录事件（FR-17）。高频，无外键。**C2u**（对外暴露 `uid`，不暴露 bigint id）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| user_id | BIGINT UNSIGNED | Y | — | 未知用户为 NULL |
| tenant_id | BIGINT UNSIGNED | Y | — | |
| realm | VARCHAR(16) | N | — | |
| identifier | VARCHAR(255) | N | — | 尝试的邮箱（已小写，不含口令） |
| event | VARCHAR(24) | N | — | `login_success/login_failed/logout/totp_failed/locked/token_auth_failed` |
| ip | VARBINARY(16) | Y | — | |
| user_agent | VARCHAR(512) | Y | — | |
| request_id | CHAR(26) | Y | — | |
| reason | VARCHAR(64) | Y | — | |

- 索引：`idx_login_logs_user(user_id, created_at)`、`idx_login_logs_ip(ip, created_at)`、`idx_login_logs_time(created_at)`。保留：180 天 `[ASSUMPTION]`。

### 2.10 `user_verification_tokens`　**补充 · P2**

用途：邮箱验证/密码重置/成员邀请令牌（需 SMTP，P2）。C1 + T?。列：`user_id`（FK CASCADE，可空于邀请）、`email`、`purpose`（`verify_email/reset_password/invite`）、`token_hash CHAR(64)` 唯一、`expires_at`、`used_at`。保留：过期后 7 天。

## 3. 审计

### 3.1 `audit_logs`　**M1**

用途：全部管理操作、登录、配置发布、升级、计费变更的审计（FR-23）。**不可变、按月 RANGE 分区；无外键**。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| id | BIGINT UNSIGNED | N | AUTO_INCREMENT | 与 created_at 组成主键 |
| uid | CHAR(26) | N | — | ULID，API 游标/引用 |
| created_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) | 分区键 |
| actor_type | VARCHAR(16) | N | — | `user/api_token/system/node` |
| actor_user_id | BIGINT UNSIGNED | Y | — | |
| actor_token_id | BIGINT UNSIGNED | Y | — | |
| actor_label | VARCHAR(255) | N | — | 操作者快照（邮箱/令牌前缀/节点名/系统组件名）。**面向租户的读取接口不直接返回邮箱**：租户用户显示其展示名（缺失时脱敏邮箱 `a***@example.com`），平台操作者统一 `platform`，节点 `system`；平台视角原样返回快照（Story 1.22b，读取时转换，库里不改） |
| actor_tenant_id | BIGINT UNSIGNED | Y | — | 操作者所属租户 |
| tenant_id | BIGINT UNSIGNED | Y | — | **被操作对象所属租户**（代租户操作时与 actor 不同） |
| acting_as | TINYINT(1) | N | 0 | 平台管理员经 `X-Tenant-ID` 代租户操作 |
| action | VARCHAR(96) | N | — | 如 `site.create`、`deployment.publish`、`auth.login` |
| resource_type | VARCHAR(48) | N | — | |
| resource_uid | VARCHAR(64) | Y | — | |
| result | VARCHAR(16) | N | — | `success/failure/denied`。`success` 由业务 Service 在业务事务内写入；`denied`（Story 1.22b）只记**已认证主体**的授权类拒绝（403 auth.forbidden/auth.reauth_required/feature.disabled，action=`access.denied`，resource_type=`operation`，resource_uid=operationId），带防刷（同一 (主体, operation, 60 秒窗口) 一条、每主体每窗口至多 20 条），不与业务事务同提交、写入失败不影响响应；未认证请求/校验失败/登录失败不写 audit_logs（登录失败只写 login_logs）。`failure` 暂未写 |
| changes_json | JSON | Y | — | 脱敏后的变更摘要（禁止含口令/令牌/私钥） |
| meta_json | JSON | Y | — | |
| request_id | CHAR(26) | N | — | NFR-21 |
| ip | VARBINARY(16) | Y | — | |
| user_agent | VARCHAR(512) | Y | — | |

- 主键：`(id, created_at)`；唯一 `uk_audit_logs_uid(uid, created_at)`（分区表唯一键必含分区键）。
- 索引：`idx_audit_tenant(tenant_id, created_at)`、`idx_audit_actor(actor_user_id, created_at)`、`idx_audit_resource(resource_type, resource_uid, created_at)`、`idx_audit_request(request_id)`、`idx_audit_action(action, created_at)`。
- 分区：`PARTITION BY RANGE COLUMNS(created_at)` 按月，**含 `pmax VALUES LESS THAN (MAXVALUE)` 兜底分区**（无 `pmax` 时越界 INSERT 报 ERROR 1526，实测）；Worker 的内部周期作业 `partition.maintain`（非 OpenAPI TaskType，AA-8）用 `lingedge_maint`（表级 ALTER/DROP）每日把 `pmax` `REORGANIZE` 出未来 N 个月分区，保留期到期 `DROP PARTITION`（README §5.1）。
- 与业务变更**同事务**写入；分区表无外键（实测 ERROR 1506）。
- 不可变：BEFORE UPDATE/DELETE 触发器（README §5；建触发器需迁移账号具 `SET_USER_ID` 或 `log_bin_trust_function_creators=1`，README §5.1）；应用账号仅 `INSERT, SELECT`。
- 保留：365 天 `[ASSUMPTION]`（OQ-6），DROP PARTITION。**保留期的 `DROP PARTITION` 清理属 Story 3.48（`retention.cleanup`），Story 1.21 未实现**：1.21 只落地「预建分区 + 保留 `pmax`」（内部周期作业 `partition.maintain`，Story 1.15 内置，非 TaskType）与真实审计写入下的联调（`tests/integration/audit` 的 `TestPartitionMaintainRealAuditTable`：lingedge_maint 把 `pmax` 里的行搬入新分区、行数不变、`pmax` 保留、维护后应用账号写入落入新分区、maint 账号没有 INSERT/UPDATE/DELETE）。
- 写入（Story 1.21）：由业务 Service 在业务事务内经 `audit.Recorder.Record` 写入（失败 = 业务整体回滚，fail-closed）；`changes_json` 写入前经 `pkg/redact` 脱敏，序列化超 16 KiB 整体替换为 `{"truncated":true}`；`ip` 为 4/16 字节二进制；`request_id` 取请求的 `X-Request-ID`（后台来源新生成）。应用账号对该表只有 `SELECT, INSERT`（真实账号的 UPDATE/DELETE/TRUNCATE/DROP/ALTER 被拒，1142）；API 没有任何修改/删除路径。
- 租户可见范围：租户仅可读 `tenant_id = 自己` 且 `acting_as` 不泄漏平台内部字段（Service 做字段裁剪）。

## 4. 任务、租约、幂等、事件

### 4.1 `tasks`　**补充 · M1**

用途：异步 Task（API 契约 §9.2/§9.4）与 Worker 队列。通用列：C1 + T?。**逐节点投递的子任务不在本表**，见 `node-deploy.md` §4.4 `node_tasks`（`tasks` 无 node 维度）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| type | VARCHAR(64) | N | — | 白名单：**以 OpenAPI `TaskType` 枚举为唯一事实源**（本文不另列清单；新增类型走 expand：放宽 CHECK 后发布代码） |
| status | VARCHAR(16) | N | 'pending' | `pending/running/success/partial/failed/canceled` |
| priority | SMALLINT | N | 100 | 小者优先 |
| payload_json | JSON | N | — | 入参（不含敏感明文） |
| result_json | JSON | Y | — | |
| error_code | VARCHAR(96) | Y | — | |
| error_message | VARCHAR(1024) | Y | — | |
| progress | TINYINT UNSIGNED | N | 0 | 0–100 |
| attempts | INT UNSIGNED | N | 0 | |
| max_attempts | INT UNSIGNED | N | 5 | |
| run_after | DATETIME(3) | N | CURRENT_TIMESTAMP(3) | 延迟/退避 |
| lease_owner | VARCHAR(128) | Y | — | `<host>:<pid>:<role>` |
| lease_until | DATETIME(3) | Y | — | |
| fence | BIGINT UNSIGNED | N | 0 | 每次领取 +1；写回须校验 |
| idempotency_key | VARCHAR(128) | Y | — | |
| parent_task_id | BIGINT UNSIGNED | Y | — | 扇出子任务，FK → tasks(id) CASCADE |
| request_id | CHAR(26) | N | — | |
| created_by_user_id | BIGINT UNSIGNED | Y | — | FK → users(id) SET NULL |
| cancel_requested | TINYINT(1) | N | 0 | |
| started_at / finished_at | DATETIME(3) | Y | — | |

- 唯一：`uk_tasks_idem(type, idempotency_key)`（key 为 NULL 不受限）。
- 索引：`idx_tasks_claim(status, run_after, priority)`（领取：`WHERE status='pending' AND run_after<=NOW(3) … FOR UPDATE SKIP LOCKED`）、`idx_tasks_lease(status, lease_until)`、`idx_tasks_tenant(tenant_id, created_at)`、`idx_tasks_parent(parent_task_id)`。
- CHECK：status 取值、`progress<=100`。保留：终态 90 天 `[ASSUMPTION]`。

### 4.2 `task_events`　**补充 · M1（可选）**

**C2u**。`task_id`（FK CASCADE）、`level`（`info/warn/error`）、`message VARCHAR(1024)`、`data_json JSON NULL`。索引 `idx_task_events_task(task_id, id)`。保留随任务。

### 4.3 `job_locks`　**补充 · M1**

用途：周期任务/单例任务租约（ADR-019）。平台级。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| name | VARCHAR(128) | N | — | 主键（如 `scheduler.leader`、`job.rollup.aggregate`） |
| holder | VARCHAR(128) | N | — | |
| fence | BIGINT UNSIGNED | N | 0 | 每次获取/续约成功 +1 |
| lease_until | DATETIME(3) | N | — | |
| acquired_at / updated_at | DATETIME(3) | N | — | |

- 获取：`INSERT … ON DUPLICATE KEY UPDATE holder=IF(lease_until<NOW(3) OR holder=VALUES(holder), VALUES(holder), holder) …` 并以影响行数判断。

### 4.4 `idempotency_keys`　**补充 · M1**

C2。`principal_key VARCHAR(64)`（`<realm>:<user_or_token_id>:<tenant_id>`）、`idem_key VARCHAR(128)`、`method VARCHAR(8)`、`path VARCHAR(255)`、`request_hash CHAR(64)`、`response_status SMALLINT`、`response_body MEDIUMBLOB NULL`、`task_id BIGINT UNSIGNED NULL`（FK→tasks SET NULL）、`expires_at DATETIME(3)`。唯一 `uk_idem(principal_key, idem_key)`；索引 `idx_idem_exp(expires_at)`。同键不同 `request_hash` → 422 `idempotency.key_reuse`。保留：24 小时。

### 4.5 `event_outbox`　**补充 · M1**

用途：需可靠投递的进程内事件（M1 仅 notification/billing 消费方，P2 起使用）。无外键。C2。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| event_type | VARCHAR(96) | N | — | `site.published`、`node.offline`… |
| aggregate_type | VARCHAR(48) | N | — | |
| aggregate_uid | VARCHAR(64) | N | — | |
| tenant_id | BIGINT UNSIGNED | Y | — | |
| payload_json | JSON | N | — | |
| status | VARCHAR(16) | N | 'pending' | `pending/sent/failed` |
| attempts | INT UNSIGNED | N | 0 | |
| available_at | DATETIME(3) | N | CURRENT_TIMESTAMP(3) | |
| sent_at | DATETIME(3) | Y | — | |

- 索引：`idx_outbox_poll(status, available_at)`。保留：已发送 7 天。

## 5. 密钥、凭据、插件

### 5.1 `signing_keys`　**补充 · M1**

用途：Server 持有的签名/CA 密钥元数据与加密私钥（信任域 ②配置签名、③节点 mTLS CA；发行签名私钥离线不入库，ADR-012）。平台级。通用列：C1。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| purpose | VARCHAR(24) | N | — | `config_signing` / `mtls_ca` / `grpc_server`（Server :9443/:9444 的叶证书与私钥，由 `mtls_ca` 签发，`cert_pem` 存叶证书链，进程热重载，90 天、2/3 周期由 `server.cert_rotate` 任务轮换，ADR-003 §4） |
| key_id | VARCHAR(32) | N | — | 稳定标识（写入签名与证书） |
| algorithm | VARCHAR(24) | N | — | `ed25519` / `ecdsa_p256` |
| public_key | VARBINARY(128) | N | — | |
| cert_pem | MEDIUMTEXT | Y | — | `mtls_ca` 的 CA 证书（可含新旧并存）/ `grpc_server` 的叶证书链 |
| private_key_enc | MEDIUMTEXT | N | — | 信封密文 |
| enc_key_id | VARCHAR(32) | N | — | 加密所用主密钥版本 |
| status | VARCHAR(16) | N | 'pending' | `pending/active/retiring/revoked`（`mtls_ca` 轮换时新 CA 先为 `pending`，经 `GetCaBundle` 下发至全部节点后才进入签发，ADR-003 §5） |
| activated_at / retire_after / revoked_at | DATETIME(3) | Y | — | 轮换重叠期 |
| keyset_signature | VARBINARY(64) | Y | — | 链式 keyset 中由前一有效 key 对本 key 的签名 |

- 唯一：`uk_signing_keys_keyid(key_id)`。索引 `idx_signing_keys_purpose(purpose, status)`。
- 约束：每个 purpose 同时至多一个 `active`（生成列 `active_key = IF(status='active', purpose, NULL)` 唯一；基列无 FK，STORED 可用）。保留：永久（吊销的保留公钥以验证历史）。

### 5.2 `provider_credentials`　**P2**（DNS Provider 复用于 P3）

用途：第三方服务凭据（ACME 的 DNS-01 Provider、SMTP、Telegram、Webhook 密钥、Cloudflare/阿里云/DNSPod）。通用列：C1 + T?（NULL=平台共用）。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| provider_type | VARCHAR(48) | N | — | `cloudflare/alidns/dnspod/smtp/telegram/webhook/…` |
| category | VARCHAR(24) | N | — | `dns/cert/notification/storage/payment/cloud` |
| name | VARCHAR(128) | N | — | |
| credential_enc | MEDIUMTEXT | N | — | 5 段信封密文（JSON 明文，README §2.2）；API 永不回显 |
| enc_key_id | VARCHAR(32) | N | — | |
| status | VARCHAR(16) | N | 'active' | `active/invalid/disabled` |
| last_verified_at | DATETIME(3) | Y | — | |
| plugin_id | BIGINT UNSIGNED | Y | — | **P2 起为无 FK 的普通列**（`plugins` 表 P5 才创建）；P5 的 `plugins` 建表迁移末尾 `ALTER TABLE … ADD CONSTRAINT fk_provider_credentials_plugins … ON DELETE SET NULL`（README §2.4 循环/前向清单） |

- 索引：`idx_provider_credentials_tenant(tenant_id, category, provider_type)`。CHECK：category/status 取值。保留：永久，删除即物理删除密文。

### 5.3 `plugins`　**P5**（内置 Provider 行自 P2 起作为数据种子）

用途：插件注册表（DNS/云服务商/证书/存储/支付/通知）。平台级。通用列：C1。内置 Provider 的行自 P2 起作为数据种子需要 `plugins` 表，故 P2 若需种子则把 `plugins` 建表提前到 P2 的 `provider_credentials` 迁移之前（此时直接带 FK，无需 ALTER）；未提前则 P2 不写种子行。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| plugin_key | VARCHAR(96) | N | — | 唯一，如 `dns.cloudflare` |
| category | VARCHAR(24) | N | — | 同上 |
| name | VARCHAR(128) | N | — | |
| version | VARCHAR(32) | N | — | |
| builtin | TINYINT(1) | N | 1 | |
| enabled | TINYINT(1) | N | 1 | |
| manifest_json | JSON | N | — | 能力声明 |
| config_schema_json | JSON | Y | — | 配置 JSON Schema |

- 唯一：`uk_plugins_key(plugin_key)`。

### 5.4 `plugin_configs`　**P5**

用途：插件实例配置（非敏感）+ 凭据引用。通用列：C1 + T?。

| 列 | 类型 | 空 | 默认 | 说明 |
|---|---|---|---|---|
| plugin_id | BIGINT UNSIGNED | N | — | FK → plugins(id) RESTRICT |
| name | VARCHAR(128) | N | — | |
| config_json | JSON | N | — | 非敏感配置 |
| credential_id | BIGINT UNSIGNED | Y | — | FK → provider_credentials(id) SET NULL |
| enabled | TINYINT(1) | N | 1 | |
| tenant_key | BIGINT UNSIGNED | N | — | 生成列 `COALESCE(tenant_id,0)` VIRTUAL（T? 统一定义） |

- 唯一：`uk_plugin_configs(plugin_id, tenant_key, name)`。
