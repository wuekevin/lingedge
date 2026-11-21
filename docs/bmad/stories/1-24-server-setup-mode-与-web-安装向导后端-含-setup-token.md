---
title: 'Story 1.24：Server setup mode 与 Web 安装向导后端（含 setup token）'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '5ebf8aa'
context:
  - docs/bmad/epics.md Story 1.24（AC1–AC12，含 AA-7 改写后的 AC3/AC6/AC7 与新增 AC9–AC12）
  - docs/architecture/adr/ADR-026-database-credential-handoff.md（UD-31，**严格按其执行**，含「实现指引」）、ADR-016（迁移框架）、ADR-011（secretbox）、ADR-003/012、docs/architecture/tools-design.md §13/§13.1/§17.1/§17.3
  - docs/api/openapi.yaml `/public/setup/*`；docs/bmad/contract-changes/CC-11（已落地的加法）与本 Story 新增 CC-12；docs/ux/screens.md §9
  - docs/database/README.md §5.1（三类账号）、docs/operations/{secrets.md §8, server-config.md}
  - docs/bmad/stories/1-6-*（迁移执行器与 CLI）、1-13-*（server 装配、sd_notify）、1-15-*、1-22b-*、1-23-*（signingkeys.Initialize、系统动作审计）
---

## Intent

**Problem:** 全新服务器上 Server 没有任何入口完成首次初始化：没有数据库账号与库、没有迁移、没有管理员、没有信任域密钥；`lingedge-server setup` 在 1.13 约定「1.24 才注册」；ADR-026 已定稿的凭据交接（migrate 口令只在内存/root 侧文件、app/maint 口令信封加密落 `runtime-credentials.json`）尚无实现，各子命令各自直接读 `database.*`。

**Approach:** 新增 `internal/platform/setup`（向导 Service：与传输无关，HTTP 与无头路径共用；token/限流/传输安全在 HTTP 层）、`internal/platform/dbcred`（运行期凭据文件读写/校验/合并/重包装）、`internal/platform/migrate/{window.go,credsource.go,provision/}`（migrate 窗口、root 侧凭据来源与 `credential init|reset`、建账号与预检）、`internal/platform/redisx/probe.go`（`INFO server` 版本探测）、`pkg/setuptoken`（Tools 生成/Server 校验共享原语）。无 `install.lock` 且无完成的安装记录时 Server 进入 setup mode（仅 `/setup` 占位页与 `/api/v1/public/setup/*`，其余 503，默认 `127.0.0.1:8080`），向导 `complete` 后**同一进程内**切换为正常模式。注册 `setup --from-file`、`db credential rotate`、`migrate credential init|reset`。**无新迁移**（冻结迁移 000001–000018 一字未改）；契约变更均为加法，登记 CC-12。

## 逐条验收

- **AC1 setup mode**：`setup.DetectInstalled`（`install.lock` 存在 → 已安装；否则运行期凭据可解析且库里有完成记录 → 视为已安装并补写缺失的 lock；其余 → setup mode）。`server.RunSetup` 只装配 `/healthz`、`/setup`、`/api/v1/public/setup/*`，其余（含 `/healthz/ready`、认证、任务、未知路径）503 `setup.mode_active`；主密钥缺失也进入 setup mode（环境检查报告它）。真实二进制测试 `TestRealProcessSetupModeLifecycle`/`TestRealProcessSetupModeWithoutMasterKey`。
- **AC2 token 与节流**：全部 9 个 operation（含 `getSetupStatus`）校验 `X-Setup-Token`（SHA-256 与哈希文件常量时间比较，文件缺失/损坏 fail-closed）；缺少/错误 → 401 `setup.token_invalid` 并按来源节流（缺省 5 分钟 10 次 → 429，含此后携带正确 token 的请求；Redis 可用时 `redisx.Limiter`，Redis 不可用/降级时进程内计数，按来源隔离）；另有每来源每分钟 120 个请求的防滥用上限。
- **AC3 向导步骤**：环境检查（版本/时间/数据目录可写/主密钥可读不展示/迁移集合/监听传输/token）→ MySQL（建账号模式与预建账号模式；版本 ≥8.0.16 非 MariaDB、`utf8mb4_0900_ai_ci`、严格 `sql_mode`、`@@log_bin`/trust、目标库必须为空 `400 setup.database_not_empty`、GRANT OPTION 与触发器探针 ERROR 1419 阻断并给两种处置、不改 MySQL 全局变量）→ Redis（`INFO server` 主版本 ≥8，不使用 FLUSHDB）→ 数据库初始化（进程内同一 runner，逐个显示迁移分类，18 个全部应用）→ 创建管理员（口令当场设定并强度校验、argon2id、授予 `platform_admin`、可选 TOTP 两阶段绑定并一次性发放恢复码）→ 系统基础配置（站点名/公开 URL/运行模式/时区/缺省语言，走 sysconfig，整批校验）→ 内部身份与密钥（调用 1.23 `signingkeys.Initialize`，提示单独备份 master.key）→ 完成。
- **AC4 完成**：单事务（行锁 + 条件更新 `install_locked_at IS NULL` + 审计）写 `system_installations` → **提交后最后**写 `install.lock` → 删除 setup token 哈希（无权删除时清空内容使其失效）→ 关闭 migrate 窗口；此后 `/public/setup/*` 永久 `410 setup.already_installed`。
- **AC5 续做与并发**：进度存于 `system_installations.meta_json["setup"]`（无独立 `state` 列，无需迁移），database/redis 由凭据文件段推导、init_db 由 `schema_migrations` 推导；各步幂等。并发 `complete`：仓库层 8 路事务恰一个成功；3 个 Service × 9 路并发恰 1 成功、8 个 410，审计与完成记录各恰一条。
- **AC6/AC11 无头路径**：`lingedge-server setup --from-file`：恰 0600、调用者属主、非符号链接的普通文件，否则退出码 3；首次成功解析后清零并 0 覆盖后删除，失败醒目警告且结果 `from_file_residual=true`（残留文件已全零）；口令字段支持 `*_file`；自动生成的迁移口令只在结果输出一次；已安装退出码 1。
- **AC7 三类账号**：`lingedge_app`（迁移内逐表授权；`audit_logs` 仅 SELECT/INSERT，UPDATE/DELETE/DROP/ALTER 被拒 1142）、`lingedge_maint`（仅分区表 SELECT/ALTER/DROP，不能 DROP/读其它表）、`lingedge_migrate`（`ALL ON db.* WITH GRANT OPTION` + 全局 SET_USER_ID，库名下划线转义）；账号名固定（迁移 lint 只允许授权给 app/maint）。真实权限负向测试见 `TestFullWizardCreateAccountsMode`。
- **AC9 凭据交接**：migrate 池只经 `migrate.Window` 构造（门禁测试扫描全仓库调用点），`complete` 后 `ErrWindowClosed`；步骤 2 成功后立即写 `runtime-credentials.json`（`lingedge:lingedge` 0600、目录 0700、原子写、写前往返自检、AAD=`runtime_credentials|<路径>|<账号>`、转储无明文）；启动校验拒绝符号链接/过宽权限/属主不符/未知版本/未知字段/过大；Server 装配与各子命令统一经 `dbcred.Merge`（显式通道优先，只解当前角色需要的字段）；自动生成的迁移口令仅在 `complete` 响应 `one_time_secrets.migrate_password` 返回一次（`Cache-Control: no-store`），响应后清零并给出 `credential init` 提示。
- **AC10 续做重新提供**：重启后 `initialize` 返回 409 并要求重新提供迁移凭据（绝不静默取回）；重新提交 MySQL 步骤（`migrate_password`，或建账号模式 + 临时管理员 `ALTER USER` 重置，旧口令随之失效）；进行中安装的库（非空但属本向导中断的安装）允许续做。
- **AC12 传输与脱敏**：监听非回环且非 TLS（直接 TLS 或可信反代的 `X-Forwarded-Proto: https`，不可信来源的头无效）时携带口令的接口 `400 setup.insecure_transport`；`migrate credential init|reset` 与 `db credential rotate`（`RETAIN`/`DISCARD CURRENT PASSWORD` 两阶段）真实 MySQL 测试；root 侧 migrate 的连接目标不取自 `runtime-credentials.json`（攻击者监听器 0 连接、migrate 包不 import dbcred）；日志/响应/审计/state/凭据文件全量注入扫描无机密。

## 改动清单（主要文件）

- 新增：`internal/platform/setup/{service,types,steps_db,steps_post,http,guard,mode,fromfile(+_unix/_windows)}.go`、`setup/repository/repository.go`、`setup/static/setup.html`；`internal/platform/dbcred/*`；`internal/platform/migrate/{window,credsource(+_unix/_windows)}.go`、`migrate/provision/provision.go`；`internal/platform/redisx/probe.go`；`internal/platform/httpx/pages.go`；`internal/platform/server/setupmode.go`；`internal/auth/internal/service/bootstrap.go`；`internal/platform/sysconfig/setup.go`；`pkg/setuptoken`；`cmd/lingedge-server/{setupcmd,dbcmd,runtimecfg}.go`；`docs/bmad/contract-changes/CC-12-*.md`；`tests/integration/setup/*`、`tests/migrations/credential_test.go`、`tests/security/logscan_setup_test.go` 与各包单测。
- 修改：`internal/platform/{config,db/config,migrate/cli,server/server,audit/catalog,apierr/codes,sysconfig/{registry,capability},signingkeys/repository}`、`internal/auth/{api.go,internal/{domain,handler/module,repository/rbac}}`、`pkg/redact/redact.go`（SQL 口令字面量兜底）、`cmd/lingedge-server/{main,serve,lineagecmd,assembly/assembly}.go`、`docs/api/{openapi.yaml,README.md}`、`docs/operations/{secrets,server-config}.md`、`docs/database/README.md`、`docs/architecture/{tools-design,dev-environment,adr/ADR-026}.md`、`configs/server.example.yaml`、`Makefile`（注释）、`go.mod`（`golang.org/x/term`，用于无回显口令输入）。
- 既有测试的必要调整（逐条）：`cmd/lingedge-server/main_test.go`（原「setup 未注册」断言按 1.24 改为「已注册、缺参数为用法错误」）、`routes_test.go`（62 → 71 条路由）、`internal/platform/audit/audit_test.go`（31 → 39 个 operation）、`tests/integration/server/{env_test,process_test}.go`（这些用例测「已安装」的正常模式：env 写入 install.lock；`setup` 子命令断言同上）、`internal/platform/migrate/cli_test.go`（非交互缺口令退出码 3 而非 1；`dbConfig` 签名）。**未修改任何门禁脚本与门禁自测，未新增任何豁免**。

## 设计决定与说明

- **审计与 SystemActions**：8 个向导 operation 在冻结的 openapi 里是 `x-audit: true`，覆盖门禁要求它们登记在 `audit.Catalog`，而门禁又禁止把系统动作放进 Catalog；因此新动作 `setup.*` 登记在 Catalog（对应各 operation），记录时统一以 Actor=系统组件 `setup`、在不携带路由元数据的上下文里写入（向导请求发生在已认证体系建立之前）。`audit_logs` 在迁移完成前不存在：`checks/database/redis` 三步的事件在内存缓冲，于 `initialize` 完成后与进度行同一事务补写（进程在补写前退出则丢失，已记运行日志）；其后各步审计与业务写入同一事务，失败即整体回滚（fail-closed）。门禁 `gates audit` 仍通过（每个动作都有字面 `.Record(ctx, audit.ActionX, …)` 调用）。
- **只创建专用库与账号**：建账号模式（`create_if_missing=true`）要求库名为 `lingedge` 或 `lingedge_*`（系统库名等一律 400），账号名固定为 `lingedge_app/lingedge_maint/lingedge_migrate`（后者可自定义但不得与前两者同名）；账号已存在时仅当其全部授权都只作用于目标库（或仅 USAGE/SET_USER_ID）才允许重置口令（续做），否则 `ErrAccountInUse` 拒绝（账号是全局对象，避免改写别的系统的账号）。
- **migrate 窗口**：`migrate.Window` 是 setup 期瞬时持有 migrate 池的唯一授权句柄；`OpenRunner` 调用点由 `internal/platform/setup/gate_test.go` 静态扫描限定（比 forbidigo 的目录白名单更窄），正常模式的 Service 没有窗口。
- **setup token 删除的权限现实**：向导以 `lingedge` 运行而 `/etc/lingedge/server` 属主是 root，无权 `unlink`；退而清空文件（空文件对校验非法，fail-closed）并提示手工删除。
- **TOTP 绑定**为两阶段（首次提交返回密钥与 otpauth URI，带 `totp_code` 再提交确认并发放 10 个恢复码），复用 1.17 的密钥加密/恢复码哈希/重放保护逻辑；错误验证码 401。管理员口令强度校验在 1.18 策略之上加强（三类字符或长口令短语、不含邮箱本地部分/弱片段）。
- **新增设置键** `general.base_url`（写入需再认证）与 `general.timezone`；`brand.name/name_en`、`general.locales`（缺省语言置首）、`system.run_mode` 经同一设置服务与审计事务写入。
- **`pkg/redact` 加固**：新增 SQL 口令字面量（`IDENTIFIED` 子句 `BY` 之后的字符串、`SET PASSWORD` 之后的字符串）兜底（G-5 补用例暴露的缺口：此前 SQL 语句文本不被识别）。向导按设计从不把语句文本写进日志/错误（`provision` 的错误只含错误号与已抹掉口令的消息）。
- **`cron.go`**：`openMaint` 本身无需改动——scheduler 把合并后的 `database.*` 传入；统一合并入口是 `server.Run`（含 maint 仅 scheduler 角色解密）与 `cmd/lingedge-server/runtimecfg.go`（`lineage` 等子命令）。
- **用到的外部库**：`golang.org/x/term`（模块缓存内 v0.48.0，仅用于 TTY 无回显读口令，已进 go.mod 直接依赖）。

## 测试（真实、无 Mock）

- `tests/integration/setup`（独立临时 mysqld〔log_bin=ON、trust=0 的生产形态；另起 NoBinlog/Trust 实例各一〕与独立临时 redis-server，运行期随机口令）：完整向导端到端（含 TOTP、18 个迁移、三类账号负向权限、凭据文件转储/权限、一次性机密、完成后 410 与 token 删除、migrate 窗口关闭、正常模式登录含 TOTP 第二步、全量泄漏扫描）；抢占（缺/错 token、节流三种后端、每个 operation、请求限流、哈希文件缺失）；传输安全；非空库（两种模式）；MySQL 预检各分支（字符集/sql_mode/ERROR 1419 阻断与处置/log_bin 关闭/trust 开启/GRANT OPTION/口令错误不泄漏/请求校验/无主密钥）；Redis 步骤（不可达、口令错误不泄漏、带口令实例、预置键不被清空）；每一步后重启续做、迁移口令重新提供与 ALTER USER 重置、预建账号模式全流程；并发 complete；`setup --from-file`（权限/符号链接/目录、成功清理、`*_file`、残留全零、严格解析）；`db credential rotate` 双口令语义；真实进程 setup mode 生命周期（503 路由面、SIGKILL 续做、同进程切换正常模式、重启、删除 lock 自愈、优雅停机码 0）。
- `tests/migrations/credential_test.go`：来源优先级、migrate.yaml 校验、`credential init|reset`（预检失败不写文件）、目标不取自 `runtime-credentials.json`。`tests/security/logscan_setup_test.go`：G-5 向导机密注入。单测：`dbcred`、`setuptoken`、`provision`（版本/事实/字面量转义/错误抹口令）、`redisx` INFO 解析、auth 口令强度、guard、setup gate/http、`httpx` 页面。
- 测试口令/令牌/主密钥均运行期随机生成，G-6 `make security` 通过，**未加任何豁免**。

## 遗留与风险（只列确属后续 Story 或需特定环境的）

- **等待环境验证**：① ENV-LNX：真实 systemd 下 `lingedge` 用户写 `/var/lib/lingedge/server/secrets`、root 写 `/etc/lingedge/server/migrate.yaml`、root 运行时对 `--config`/`migrate.yaml` 属主与权限的判定（本机仅验证权限位与当前用户属主判定）；② ENV-BT：宝塔 MySQL 专用库与账号场景（面板预建账号默认权限、socket 可访问性）；③ MySQL 8.0.16 最低版本的真实拒绝（本机 8.0.46，版本判定与事实检查有构造输入单测）；④ Redis <8 的真实拒绝（本机只有 Redis 8.x，`INFO server` 解析与判定有构造响应单测）；⑤ root 身份下「目录权限使 unlink 失败」的两个用例在 root 下跳过。
- **Deferred（确属后续）**：向导前端 SPA 在 1.37（本 Story 只有内嵌占位页）；`secrets.rewrap` 任务接线（本 Story 提供 `dbcred.RewrapFile`/`KEKIDs`，worker 任务在 Tools 密钥轮换/备份 Story）；Tools 侧生成 token 与写哈希文件的调用（1.35，已提供 `pkg/setuptoken`）；运行期凭据文件不支持 Unix socket（`config.Database` 无 socket 键）；热重载运行期口令、`lingedge_migrate` 的 `ACCOUNT LOCK`（ADR-026 Deferred）。
- **已知限制（如实）**：请求体解码出的 `string` 口令副本无法就地清零（Go 字符串不可变），实现置空请求结构体并只持有 `[]byte` 版本的 migrate 口令；迁移之前的审计事件缓冲在内存，进程在补写前退出则这些事件只剩运行日志。
