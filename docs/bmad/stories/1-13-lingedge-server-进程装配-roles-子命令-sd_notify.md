---
title: 'Story 1.13：lingedge-server 进程装配（roles / 子命令 / sd_notify）'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'c9f9c63'
context:
  - docs/bmad/epics.md（Story 1.13 全文，1118–1143 行）
  - docs/architecture/architecture.md §5.4、docs/architecture/tools-design.md §15.1/§17.5、docs/architecture/adr/ADR-002 §12（lineage）
  - docs/bmad/review-architecture*.md 与 architecture-revision-log.md 的 IMP-07/H6
  - docs/bmad/stories/1-10/1-11/1-12-*.md（约定与上一 Story 遗留：迁移版本探针）
  - cmd/lingedge-server/、internal/platform/{httpx,config,db,redisx,migrate,logx,secrets,idempotency}、pkg/secretbox
---

## Intent

**Problem:** 1.4–1.12 已交付配置、日志、DB/Redis 池、迁移框架、密钥库与 HTTP 骨架，但 `lingedge-server` 还不是一个进程：没有角色装配、没有 systemd 就绪通知、没有优雅停机，`migrate/healthz/lineage` 之外的运维入口缺失，且 1.12 留下「就绪检查读不到迁移版本」的遗留（`lingedge_app` 对 `schema_migrations` 无 SELECT）。

**Approach:** 新增 `internal/platform/server`（角色解析、装配、生命周期）、`internal/platform/sdnotify`（零依赖 sd_notify）、`internal/platform/lineage`（deploy.lineage 读取/提升），`cmd/lingedge-server` 变为子命令分发 + `Run`。迁移版本探针的遗留用**方案 (a)** 解决：新迁移 000014 为 app 账号增加对 `schema_migrations` 的唯一 SELECT 授权，经 CC-3 走契约变更流程。

## Boundaries & Constraints

**Always:** 启动失败（主密钥缺失/权限不符、DB 不可连、监听冲突、非法角色、路由注册错误）一律拒绝启动、非零退出、给出指引；Redis 不可达只降级；日志含 request-id；认证/授权/Scope/审计钩子在 1.16–1.21 前保持安全拒绝默认实现；测试只用自拉起的独立临时 MySQL/Redis；`000001–000013` 不改，新授权走新迁移文件与 CC-3。

**Never:** Mock 冒充生产；让 Server 进程持有 migrate 凭据；给 app 增加 `schema_migrations` 的 SELECT 以外的任何权限；伪造 grpc/scheduler/worker 功能；注册 `setup`（1.24 前）；启动/改动/终止本机常驻 mysqld(576/744) 与 redis；Docker；push；改 git config。

## 遗留项决策：迁移版本探针（方案 (a)，理由）

核对 `migrations/*.sql` 的实际 GRANT：app 对 `schema_migrations`/`schema_migrations_runs` 均无授权（版本表由迁移框架自建，迁移文件从无授权语句）。

- **采用 (a)**：`migrations/000014_app_read_schema_migrations.sql` = 一条 `GRANT SELECT ON schema_migrations TO lingedge_app`。严格最小：只读、只这一张表（不含 `schema_migrations_runs`：含错误文本与校验和，探针不需要）。迁移 lint 增加例外并锁死——框架版本表对 app 只允许恰为 SELECT（INSERT/UPDATE/DELETE/ALL、`WITH GRANT OPTION`、授 maint、授其他未创建的表均被拒，且各有负向样本）。
- **否决 (b)**：`lingedge_migrate` 是 `ALL ON db.* WITH GRANT OPTION`，「只读使用」只是代码约定而非权限边界；把它放进常驻、面向网络的进程，一旦被攻破即得整库 DDL/授权能力，直接违反三账号最小权限（IMP-19）；且其凭据落点（AA-7）未定稿。
- **流程**：`docs/bmad/contract-changes/CC-3-app-read-schema-migrations.md`；`migrations/CHECKSUMS` 经 `make migrations-seal` 只追加登记；M1 冻结口径（000001–000013、61 表、README §8.1 十三行）不变，README 在表后追加「冻结后追加迁移」说明；依赖「迁移总数 == 13」的测试改为只取 M1 冻结范围。
- **探针能力边界**：`httpx.MigrationProbeFromApp` 只读 `schema_migrations`，与内嵌迁移集合比对（最高版本一致、无待应用、无二进制不认识的版本）；**无法看到 dirty/校验和**（那在 `schema_migrations_runs`，仅 migrate 账号可读），这些由持有迁移凭据的 `lingedge-server migrate status` 检查；迁移中途失败时版本行未写入，仍表现为 `Applied < Latest` 而不就绪。库未升级到 000014 时读取失败 → `/healthz/ready` 报 `migrations: unknown` 并 503（fail-closed）。

## Code Map

- `internal/platform/server/{roles.go,server.go}` -- `ParseRoles`/`RoleSet`；`Run`：装配顺序 主密钥（`secretbox.FileKeyProvider`）→ app 池 → Redis → 迁移探针 → 幂等存储 → metrics 监听 → 按角色装配 → READY=1；优雅停机顺序 STOPPING=1 → 关闭监听并排空（`http.Server.Shutdown`，`--shutdown-timeout` 缺省 30s）→ 停角色 → 关 Redis → 关 MySQL → 清零主密钥。启动/停机用进程级 request-id，访问日志用请求 id。
- `internal/platform/sdnotify/` -- `Notify`/`NotifyTo`：只写 `$NOTIFY_SOCKET`（unixgram，`@` 前缀=抽象 socket 换 NUL，其他只接受绝对路径）；未设置=no-op；无外部依赖。
- `internal/platform/lineage/` -- `Show`/`Bump`：单事务 `SELECT … FOR UPDATE` → `max(当前值+1, 当前 UNIX 秒)` → UPDATE → 写 `audit_logs`（actor_type=system，action=`system.lineage.bump`，changes_json 含前后值，request_id）；设置项不存在 → `ErrNotInstalled`，不隐式创建；只接受 app 池。
- `internal/platform/httpx/health.go` -- 新增 `MigrationProbeFromApp`（app 池只读版本表）；`MigrationProbeFromRunner` 保留给持有迁移凭据的工具/测试。
- `internal/platform/migrate/lint.go` -- `schema_migrations` 对 app 仅 SELECT 的例外 + 负向样本（`lint_test.go`）。
- `migrations/000014_app_read_schema_migrations.sql`、`migrations/CHECKSUMS`、`docs/bmad/contract-changes/CC-3-…md`、`docs/database/README.md` §8.1 说明。
- `cmd/lingedge-server/{main.go,serve.go,healthz.go,lineagecmd.go,assembly.go,routes.go}` -- 子命令分发（`migrate`/`routes`/`healthz`/`lineage`/`version`，`--config … --roles …` 即运行）；`assembly.go` 的 `assembleHooks()` 是认证/授权/Scope/审计钩子的唯一装配点（现为零值=安全拒绝，各钩子由 1.16/1.18/1.19/1.20/1.21 填入）；`routes.go` 的 `registerRoutes` 仍是路由唯一装配点。
- 测试：`internal/platform/{sdnotify,server}`、`cmd/lingedge-server` 单测；`tests/integration/server/`（真实子进程 + 真实临时 MySQL/Redis）；`tests/migrations/cc3_test.go`。

## 角色与现状（不假装有功能）

| 角色 | 现状 | 实现 Story |
|---|---|---|
| api | **真实**：httpx 路由器（固定中间件链、/healthz、/healthz/ready、业务路由经 `registerRoutes`）与 HTTP 监听（`http.listen`） | 业务路由 1.16 起 |
| grpc | 仅生命周期骨架：**不监听任何端口**（日志标明） | 3.13（mTLS :9443 / Enroll :9444） |
| scheduler | 仅生命周期骨架：不调度任何任务 | 1.15 |
| worker | 仅生命周期骨架：不消费 `tasks` | 1.14 |

metrics（`metrics.listen`，缺省 127.0.0.1:9180，非回环地址会告警）对所有角色常开。幂等键保留期清理（`idempotency.Store.DeleteExpired`）等内部周期作业**不在本 Story 登记**，归 1.14/1.15（`assembly.go` 注释已标注）。 【Story 1.22b 已收口：`metrics.listen` 现由 config 层直接拒绝非回环（不再只告警），`server.Run` 纵深防御再拒一次；见 1-22b-遗留项清理.md】

## Tasks & Acceptance

- [x] AC1 `--config`/`--roles`（默认全开）按角色装配；非法角色/空项/重复/大小写错 → 退出码 2 且在任何 I/O 之前（`TestParseRoles`、`TestInvalidRolesExitNonZeroBeforeAnyIO`、子进程 `TestServerRefusesToStart/非法角色|空角色项`）；roles 矩阵 8 种组合（仅 api、仅 worker、仅 scheduler、仅 grpc、api+worker、scheduler+worker、默认、全列）真实 MySQL/Redis 进程内装配（`TestRolesMatrix`）
- [x] AC2 子命令：`migrate plan|up|down|status`（沿用 1.6，子进程 `migrate status` 真实连库）、`healthz [--ready]`、`version`/`--version`、`lineage show|bump`；`setup` 未注册（与未知子命令同为退出码 2，用法文本不出现 setup：`TestSetupIsNotRegisteredBeforeStory124`、`TestSubcommandSurface`）。`lineage bump`：真实 MySQL——规则 `max(当前值+1, UNIX 秒)`（含时钟回拨/前进）、重复严格递增、并发 12 路串行为连续值、审计行内容、审计写失败整体回滚、未安装拒绝并提示且不创建设置项/不写审计、值非法拒绝；真实二进制子命令的退出码与输出（`TestLineage*`）
- [x] AC3 `sd_notify`：自建 unixgram socket 验证 `READY=1`/`STOPPING=1`；未设置/空 = no-op；非法取值报错；子进程集成测试断言通知序列恰为 `[READY=1 STOPPING=1]`（只写 `NOTIFY_SOCKET`）；抽象 socket：`@` 前缀翻译全平台验证，端到端收发仅 Linux（本机 macOS 跳过并标「等待环境验证」）
- [x] AC4 SIGTERM 优雅停机：真实子进程；`SIGSTOP` 临时 mysqld 让 `/healthz/ready` 在途，随后 SIGTERM——新连接立即被拒、在途请求在 SIGTERM 之后完成且响应完整、进程排空后退出码 0（`TestServerSIGTERMDrainsInFlightRequest`）；全生命周期测试亦断言退出码 0、监听全部关闭（`TestServerProcessLifecycle`）
- [x] AC5 缺主密钥/权限不符（0644）/格式非法/DB 口令错/端口占用/配置写口令 → 非零退出 + 指引，不发 READY；日志均为 JSON 且每行含 `request_id`；日志不含口令与主密钥；单进程默认全角色可启动（`TestServerRefusesToStart`、`TestServerProcessLifecycle`）
- [x] 遗留：迁移版本探针（方案 (a)，见上；`tests/migrations/cc3_test.go`、`TestServerProcessLifecycle` 的迁移版本 14/14、`TestReadyMigrationProbeFailClosedWhenUnmigrated`）
- [x] sprint-status、开发记录、本地提交

## Verification

`gofmt -l .` 为空；`make lint security gate-selftest race vuln contract-check build verify-build migrate-test` 全绿（结果见「开发记录」）。

## Spec Change Log

- 2026-11-21：CC-3（`docs/bmad/contract-changes/CC-3-app-read-schema-migrations.md`）：新增迁移 000014（app 对 `schema_migrations` 的 SELECT）。非破坏；OpenAPI/proto 无改动；`contract-v1`/`migrations-m1` 未触碰，000001–000013 哈希不变。

## 开发记录

**开发会话**：2026-11-21，bmad-build（非交互、完全自主）。环境：macOS（Darwin 25.6.0 arm64）、go1.27.4、MySQL 8.0.46（Homebrew mysql@8.0，仅用于启动**自拉起的独立临时实例**）、redis-server 8.10.1。本机常驻 mysqld（PID 576/744）与 redis 全程未被启动、改动或终止；SIGSTOP/SIGCONT 只作用于测试自己拉起的临时 mysqld（pid 取自其 `m.pid`）。

### 设计要点与取舍

- **装配顺序与失败语义**：先主密钥再连库（fail-closed 优先暴露配置问题）；监听全部先绑定再开始服务（地址冲突立即失败，不会出现一半起来）；`router.Handler()` 的任何注册错误（元数据缺失等）变成启动失败（NFR-4）；失败路径统一回收已打开的资源。就绪通知放在全部监听就绪之后；通知失败只告警不终止（`Type=notify` 由 systemd 超时判定）。
- **停机**：SIGTERM/SIGINT 触发一次；之后恢复默认信号行为，再来一个信号即强制终止。`http.Server.Shutdown` 先关监听再等在途请求，超时则强制关闭并以非零码退出；任何关闭错误汇总为非零退出。
- **request-id**：启动/停机阶段无入站请求，每次进程启动生成一个进程级 ULID 写入所有启动/停机日志；访问日志沿用 1.12 的逐请求 id。
- **配置加载器的一个事实**：任何带 `LINGEDGE_` 前缀但不对应配置键的环境变量会被拒绝（见 1.4）；因此运维脚本/测试环境里的 `LINGEDGE_REQUIRE_MYSQL` 之类变量不得透传给 Server 进程（集成测试已过滤）。`LINGEDGE_CONFIG` 被 EnvIgnore 登记，可作为 `--config` 的环境变量形态。
- **lineage 用 app 池**：app 对 `system_settings` 有 SELECT/UPDATE、对 `audit_logs` 有 INSERT，无需更高权限账号；与 Tools 的调用时机一致（恢复数据库后、启动 Server 前）。审计与更新同事务。`system_settings.value_json` 以 JSON 数值存取（`CAST(? AS JSON)`），读取时拒绝非正整数。
- **healthz 子命令**：目标取 `--addr`，否则配置的 `http.listen`（通配地址换 127.0.0.1）；200 即成功（就绪 `degraded` 也是 200）。

### 实际验证（本机）

- 单测：`sdnotify`（路径 socket READY/STOPPING、no-op、非法输入、`@` 前缀翻译）、`server`（`ParseRoles` 正负样本、骨架角色均登记实现 Story）、`cmd/lingedge-server`（setup 未注册、版本、非法角色先于任何 I/O、lineage 用法错误）、`migrate` lint（版本表授权 6 个正/负样本）。
- 集成（`tests/integration/server`，`-race` 连跑两次均 `ok`，约 15s）：见上 AC 对照；`tests/migrations`：`TestCC3AppReadsSchemaMigrationsOnly`（真实 MySQL：app 能读版本、对版本表的 INSERT/UPDATE/DELETE/DROP 与读 `schema_migrations_runs` 均被拒绝、授权恰为 SELECT、重放幂等）。
提交前最后一次完整运行 `make lint security gate-selftest race vuln contract-check build verify-build migrate-test`，退出码 0：

- `gofmt -l .` 为空；`make lint`：vet（含 `lingedge_testdouble` 标签）、scaffold、arch-check、golangci-lint（0 issues）、shell-check、repo-check 全部通过。
- `make security`：G-5/G-6/G-11/G-12/品牌通过（其余 G-x 仍未接入，同 1.11/1.12 的声明）；G-6 固定口令门禁曾拦下本 Story 测试里的字面量口令写法，已改为运行期随机值。
- `make gate-selftest`：`scripts/gates` 与 `tests/gates` 通过；本 Story 未新增静态门禁规则（版本表授权例外属迁移 lint，其负向样本在 `internal/platform/migrate/lint_test.go` 与 `tests/migrations/cc3_test.go`）。
- `make race`（`-race`）：全部包 ok，含 `tests/integration/server`（真实子进程 + 真实临时 MySQL/Redis）、`tests/migrations`、`tests/integration/httpx`（就绪迁移版本改为 14/14，旁路篡改改作用于 000014）、`tests/scaffold`（lingedge-server 的 main 依赖与无参行为随 1.13 更新）。
- `make vuln`：No vulnerabilities found。
- `make contract-check`：`openapi-check OK`（路由清单 OK）、`openapi-breaking OK`（相对 contract-v1 无破坏性变更）、`proto-check`、`migrations-check OK`（已发布迁移 14 个哈希一致，清单相对 contract-v1 仅追加）、`freeze-check OK`（M1 61 表、迁移顺序与骨架一致）。
- `make build`、`make verify-build`、`make migrate-test`（迁移 lint 通过 14 个迁移；DDL 规则 26 例失败 0；`tests/migrations` 含 CC-3）：通过。
- 收尾核对：本机常驻 mysqld（PID 744，及 576 的 mysqld_safe）与 redis 未被触碰；SIGSTOP/SIGCONT 仅作用于测试自拉起的临时 mysqld。调试中被强制中止的测试进程遗留的两个临时 redis-server 已手工清理（均为测试自拉起的随机端口实例）。

### 自审

| 发现 | 严重度 | 处理 |
|---|---|---|
| 探针无法发现 dirty/校验和（app 看不到 `schema_migrations_runs`） | low | accept：已在探针注释与本文登记；这类问题由 `migrate status`（迁移凭据）检查，迁移中途失败仍表现为 Applied<Latest 而不就绪 |
| 未升级到 000014 的库，就绪检查 503/unknown，而非更友好的提示 | low | accept（fail-closed 是期望）；`docs/bmad/contract-changes/CC-3` 已写明升级路径是普通 `migrate up` |
| `metrics.listen` 允许配置为非回环地址 | low | 只告警不拒绝（config 校验属 1.4 范围）；若要强制回环应由 config 层统一收紧，留待安全加固 | 【Story 1.22b 已收口：config 层已拒绝非回环（architecture §7/tools-design §10.4 没有受控的非回环场景，故无放开开关）；见 1-22b-遗留项清理.md】
| 优雅停机排空依赖 `http.Server.Shutdown`：Hijack/长连接不在其管理内 | low | 现无此类路由；1.14 之后若出现 SSE/WebSocket 需 `RegisterOnShutdown` 逐个处理，已作为遗留 |
| 同时对 grpc 角色的骨架不监听 9443/9444，Tools 安装后的 `health.server` 检查若期望这些端口会不通过 | low | 设计如此（3.13 之前没有 gRPC）；文档与日志均标明，未伪造监听 |
| `LINGEDGE_*` 未知变量导致启动失败 | info | 已有行为（1.4）；本 Story 在文档登记，不放宽 |

### DoD 与遗留

DoD（基线 §25）：除下述「等待环境验证」外满足；前端功能可用、数据库迁移完成不适用（迁移 000014 为遗留项所需，已含测试与 CC-3）。

**等待环境验证**：① 真实 systemd `Type=notify` 全流程（unit 的 `Type=notify`/`NotifyAccess`、`TimeoutStartSec` 内收到 READY=1、`systemctl stop` 触发 SIGTERM 的优雅停机与 `TimeoutStopSec`）——本机 macOS 无 systemd，仅以自建 unixgram socket 验证协议；② Linux 抽象 socket（`@` 前缀）的端到端收发（本机跳过，仅验证前缀翻译）；③ MySQL 8.0.16（最低版本）与 Linux 验收机上重跑 `tests/integration/server` 与 `tests/migrations`（本机仅 8.0.46）；④ 经 systemd `LoadCredential` 的 `$CREDENTIALS_DIRECTORY/master.key` 启动路径（沿用 1.11 的等待项）；⑤ 备份恢复整流程中 Tools 调用 `lineage bump`（1.41/1.43）的联调。

**遗留/后续**：1.14/1.15 登记幂等键保留期等内部周期作业并接入 worker/scheduler 骨架；3.13 实现 grpc 角色的真实监听；1.16–1.21 逐个替换 `assembleHooks()` 的默认拒绝钩子；1.24 注册 `setup` 子命令；1.23 写入 `deploy.lineage=1`（本 Story 的 bump 依赖其存在；**Story 1.23 已交付**，联调测试 `tests/integration/signingkeys` 的 `TestLineageBumpAfterInit`：初始化前 show/bump 拒绝，初始化后可读到 1、可 bump 且单调）；如出现长连接路由需补 `RegisterOnShutdown`。
