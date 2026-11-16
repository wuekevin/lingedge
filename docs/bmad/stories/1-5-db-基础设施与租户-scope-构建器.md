---
title: 'Story 1.5：DB 基础设施与租户 Scope 构建器'
type: 'feature'
created: '2026-11-17'
status: 'in-review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '46e6d00fbb291c89d39796d6d2c147e66d28eeeb'
context:
  - docs/bmad/epics.md（Story 1.5 全文，约 916–939 行）
  - docs/bmad/decisions-adopted.md（UD-27、AA-7）
  - docs/architecture/adr/ADR-009-tenant-isolation.md（§1–§6、§8）
  - docs/architecture/adr/ADR-001-modular-monolith.md（§3、§6、§9）
  - docs/architecture/adr/ADR-016-migration-tool.md
  - docs/database/README.md（§2.2、§2.4、§5、§5.1、§6、§9）、docs/database/verification/
  - docs/architecture/dev-environment.md（§1.2、§2）
  - docs/bmad/stories/1-1-…、1-2-…、1-3-…、1-4-…
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** 各模块即将开始写第一条 SQL，但仓库里既没有统一的 MySQL 访问层（连接、事务、三类账号），也没有把租户隔离「从结构上」强制的机制：`WHERE tenant_id = ?` 靠每个 Repository 作者自觉，必然在某处遗漏（ADR-009 动机）；三类数据库账号（app / maint / migrate）的权限边界只存在于文档与手工实测。

**Approach:** 在 `internal/platform` 落地 `db`（连接池 / 事务 / 常量 SQL）、`db/tenantdb`（租户查询构建器）、`tenantscope`（Scope 唯一构造入口）三个包；用「未导出字段 + 编译器」「仅接受常量的 SQL 类型」「三个不可互换的池类型」把约束放到编译期，再用 depguard / forbidigo / 新增的 `repo` 门禁把边界放到 `make lint`；用独立临时 mysqld 上的真实集成测试验证越权被拒与三类账号权限，而不是 Mock。

## Boundaries & Constraints

**Always:** 查询参数化（服务端预处理，不插值）；连接会话为 UTC + utf8mb4 + parseTime，且在建池时校验；越权访问表现为 0 行 / `db.ErrNotFound`；Scope 无效（零值 / 平台范围用于普通方法）在发出任何 SQL 之前失败；集成测试用真实 MySQL 8.0，环境缺失时跳过并标「等待环境验证」；新增依赖版本固定且 `make vuln` 无漏洞；测试用临时实例必须独立 datadir/端口/socket 且用后关闭并删除。

**Never:** 触碰本机常驻 MySQL 实例（数据 / 账号 / 端口）；Docker；Mock 冒充生产；越界实现迁移框架（goose、lint、`migrate` 子命令属 Story 1.6）；改动已冻结契约（OpenAPI / proto / migrations）；push；改 `git config`；改动 `SmartCDN`；在生产代码里加「检测到测试环境」的分支。

</frozen-after-approval>

## Code Map

- `internal/platform/tenantscope/` -- `Scope`（字段未导出，包外无法构造非零值）、`FromPrincipal`（路由元数据 × 主体 × 目标租户，ADR-009 §5 全部分支）、`FromTask`、`PlatformForJob`、`NewContext/FromContext`；`testdata/` 下是编译期伪造失败的对照包。
- `internal/platform/scope/` -- `scope.Scope` 类型别名（AC 的签名形态；别名不提供构造能力）。
- `internal/platform/db/` -- `Config`（含 Secret 口令、TLS、池限额缺省 `PoolDefaults`）、`OpenApp/OpenMaint/OpenMigrate`（三个互不兼容的池类型，`Pool` 接口含未导出方法）、`WithTx/WithTxOptions/TxFromContext/Executor`（事务经 context 传播，键含池标识）、`Q`（只接受编译期常量的 SQL 类型）、`FromBuilder`（构建器凭据，`internal/token`）、`ErrNotFound`/`MySQLErrorNumber`/`IsDuplicate`。
- `internal/platform/db/tenantdb/` -- `Table`（表名/列白名单）、`Select/Insert/Update/Delete` 构建器与 `Cond`；普通方法自动追加 `tenant_id = ?`，INSERT 强制写 Scope 租户，`*AcrossTenants` 方法族仅限平台范围。
- `scripts/gates/{spec.go,genlint.go,repo.go,data/platform-tables.txt,main.go}`、`.golangci.yml`（生成）、`Makefile`（`repo-check`）-- sqlx/database/sql/驱动只许出现在 db 与 repository 包（depguard）；Repository 内禁止 SQL 拼接、受限池与 Scope 构造入口的调用白名单（forbidigo）；`repo` 门禁检查 Repository 导出方法的 Scope 首参。
- `tests/mysqltest/` -- 独立临时 mysqld 的起停辅助（`--no-defaults`、独立 datadir/端口/socket、`--mysqlx=OFF`、`--log-bin` + trust=0、遗留实例回收、可用性开关）。
- `tests/integration/db/` -- 真实 MySQL 集成测试：会话 / 时间 / utf8mb4 / 预处理语句、事务传播与回滚、跨租户越权负向矩阵、三类账号权限负向、触发器权限（SET_USER_ID）。
- `tests/compilefail/` -- 用编译器断言「拼接 SQL / 伪造 Query / 混用池 / 自造 Pool」必须编译失败。
- `tests/gates/gates_selftest_test.go` -- 新增边界规则与 `repo` 门禁的正反自测。
- 文档：`docs/architecture/dev-environment.md`、`README.md`、`docs/database/README.md`（§5.1 逐表授权实测备注）、`internal/platform/doc.go`。

## Tasks & Acceptance

**Execution:**
- [x] `internal/platform/tenantscope`、`scope` -- Scope 与唯一构造入口（AC2）
- [x] `internal/platform/db` -- 连接 / 三类池 / 事务 / 常量 SQL（AC1、AC6）
- [x] `internal/platform/db/tenantdb` -- 租户查询构建器（AC3）
- [x] `tests/mysqltest` -- 独立临时 mysqld 辅助与可用性开关
- [x] `tests/integration/db` -- 真实 MySQL：越权负向矩阵（AC4）、三类账号权限负向（AC6）、会话与事务（AC1）
- [x] `scripts/gates`、`.golangci.yml`、`Makefile` -- 静态边界与 `repo` 门禁（AC5、AC6、AC2）
- [x] `tests/compilefail`、`tests/gates` -- 编译期与门禁的正反自测
- [x] 文档同步、`sprint-status.yaml`
- [x] 全部门禁 + 独立评审 + 本地提交

**Acceptance Criteria:** 见 `docs/bmad/epics.md` Story 1.5 的 AC1–AC6（原文为准）。

## Implementation Notes

见文末「开发记录」。

## Spec Change Log

## Review Triage Log

（见「开发记录 · 三路评审」。）

## Verification

**Commands:**
- `make lint`、`make security`、`make gate-selftest`、`make test`、`make race`、`make vuln`、`make contract-check`、`make build && make verify-build`

## 开发记录

**开发会话**：2026-11-17，bmad-build（非交互、完全自主）。环境：macOS（Darwin 25.6.0 arm64）、go1.27.4、MySQL 8.0.46（Homebrew，仅用独立临时实例）。

### 范围决策与偏差

- **范围以 AC1–AC6 为准**；迁移框架（goose、lint、`migrate` 子命令、版本门槛 8.0.16）属 Story 1.6，未触碰。`MigratePool.SQLDB()` 仅为 1.6 的 goose 预留（它需要 `*sql.DB`）。
- **`scope.Scope` 的落地**：类型本体与全部构造入口在 `internal/platform/tenantscope`（只有同包才能让字段对包外不可写，编译器即证明「包外无法构造非零 Scope」）；`internal/platform/scope` 是类型别名，使 Repository 签名写成 `scope.Scope`。构造入口 `FromPrincipal/FromTask/PlatformForJob/NewContext` 另由 forbidigo 限定调用路径（Handler/Service/Repository 不可调用，只能 `FromContext`）。`Principal` 为本包定义的最小接口（platform 不得依赖 auth 模块），由 Story 1.16/1.20 的主体类型实现。
- **SQL 只接受编译期常量**：`db.Q(sqlConst)` 的参数类型未导出，`fmt.Sprintf`/变量拼接无法通过编译（`tests/compilefail` 用真实编译器断言）；tenantdb 构建器经 `internal/token` 凭据生成文本。AC5 的「forbidigo 拦截拼接」作为第二道闸保留（Repository 内禁 `fmt.Sprint*`、`strings.Join/Replace*/Builder`）。
- **租户表清单未从 docs/database 生成**：文档对 T / T? / 平台级的标注是叙述性的，无法可靠解析（实测启发式漏判 usage_records、login_logs、sessions 等）。改为「默认所有 repository 导出方法/接口方法都要求 Scope 首参，平台级表显式登记」的反向清单（`scripts/gates/data/platform-tables.txt`，并有测试核对表名存在于 docs/database 且文档中无 `tenant_id` 列）。已回写 ADR-009 修订记录。
- **ClientFoundRows=true**：UPDATE 的受影响行数按「匹配行」计，使「0 行 = 没有匹配的行（越权/不存在）」成立，而不是「值没变」；有真实 MySQL 回归。
- **`ExecOne` 带 `LIMIT 1`**（评审后修改）：至多改动/删除一行，不会「先写入、后报错」。
- **池限额缺省为 `[ASSUMPTION]` 工程值**（app 25/10、maint 2/1、migrate 1/1），基线未规定，待性能基线以配置覆盖（UD-27）；`AppConfigFrom` 仅映射 server.yaml 已有的 database 节。**maint/migrate 凭据不进 server.yaml**（AA-7 未定稿）。
- **非回环主机必须显式设置 TLS**（评审后新增）：`server.yaml` 的 `database` 节暂无 `tls` 键，因此经 `AppConfigFrom` 连接非回环数据库目前会被校验拒绝——需要在后续 Story 扩展 config schema（`database.tls` 及池限额）后才能连远程库；本机/同机 MySQL（宝塔默认）不受影响。
- **设计发现（已写入 docs/database/README §5.1 与 ADR 备注，须由 1.6/1.7 遵循）**：README 原写 `lingedge_app: ... ON db.*` 且 `audit_logs`/`wallet_entries` 仅 SELECT/INSERT，二者在 MySQL 中不能同时成立（权限取并集；库级授权上做表级 REVOKE 报 ERROR 1147，已有回归）。结论：**app 必须在迁移内逐表授权**，与 maint 的表级授权同一机制。
- **受限入口的白名单**：`OpenMaint`（`internal/platform/db`、`internal/scheduler`、`cmd/lingedge-{server,worker,scheduler}`）、`OpenMigrate`（`internal/platform/db`、`internal/platform/migrate`、`cmd/lingedge-server`）、Scope 构造（`internal/platform`、`internal/tenant`、`internal/scheduler`、`cmd/lingedge-{server,worker,scheduler}`）。已知边界：`internal/platform/` 整体在 Scope 构造白名单内，待 1.12/1.14/1.20 落地后收窄；`lingedge-server` 同时承载请求路径与 scheduler/worker 角色，故在 maint 白名单内，是否收窄到具体文件随 1.13 装配评估。

### 交付物

`internal/platform/{tenantscope,scope,db,db/tenantdb}`；`tests/mysqltest`（临时实例辅助）、`tests/integration/db`（真实 MySQL 集成测试，30 余个用例）、`tests/compilefail`；`scripts/gates` 新增 `repo` 门禁与 depguard/forbidigo 规则生成、`Makefile` 的 `repo-check`；依赖 `jmoiron/sqlx v1.4.0`、`go-sql-driver/mysql v1.10.1`（间接 `filippo.io/edwards25519 v1.2.0`）；文档同步（dev-environment、README、docs/database §5.1、ADR-009、doc.go）。

### 临时实例辅助（tests/mysqltest）

`mysqld --no-defaults --initialize-insecure` 初始化独立临时 datadir，空闲回环端口 + datadir 内独立 socket，`--mysqlx=OFF`（不占本机 33060）、`--log-bin` 且 `log_bin_trust_function_creators=0`；`TestMain` 经 `mysqltest.Main` 在测试结束时 SIGTERM 关闭并删除目录；上次异常退出遗留的实例按「属主测试进程已死 + 命令行含该 datadir」精确回收（有单测，含「不误杀无关进程」）。开关：找不到 mysqld 8.0 → `t.Skip("等待环境验证: …")`；`LINGEDGE_MYSQL_TESTS=off` 显式跳过；`LINGEDGE_REQUIRE_MYSQL=1` 缺失即失败；`LINGEDGE_MYSQLD` 指定路径。**启动失败一律视为失败，不会被当作环境缺失而跳过。** 本机常驻 MySQL（端口 3306/33060，pid 25755）全程未触碰：验证后 `pgrep -fl mysqld` 仅剩其原有两个进程，`$TMPDIR` 下无 `lemy*` 残留。Redis 本 Story 不需要。

### 实际验证（本机 macOS arm64、go1.27.4）

- `make lint`：vet×2、gofmt、scaffold、arch、golangci-lint `0 issues`、shell、`repo-check` 通过。`make security` 通过。
- `make gate-selftest`：通过。新增正反自测——sqlx/database/sql/驱动出现在 service/handler/platform 其他包/cmd 被 depguard 拦截；repository 与 `internal/platform/db` 内合法；Repository 内 `fmt.Sprintf`/`strings.Join` 被 `[no-sql-concat]` 拦截；`db.OpenMaint/OpenMigrate`（含别名导入）在白名单外被 `[db-pool-maint]/[db-pool-migrate]` 拦截，scheduler 不得开 migrate 池；`tenantscope.FromTask/NewContext/PlatformForJob`（含别名）在 handler/service/repository 被 `[tenantscope-mint]` 拦截；`repo` 门禁 10 个负例（缺 Scope、Scope 非首位、自造同名类型、租户表豁免、AcrossTenants 缺 Scope/豁免、多表夹带、接口方法、`NewestSites`、包级函数）与合法写法不误伤。
- `make test`、`make race` 全部通过。其中真实 MySQL（8.0.46 临时实例，binlog=ON、trust=0）：
  - **跨租户越权负向矩阵**（`TestCrossTenantAccessIsDenied_RealMySQL`，10 个子用例）：租户 A 的 Scope 按 uid/按 id 读、UPDATE（ExecOne/按 id）、DELETE（ExecOne/按 id）、Count、`IN` 列表、`Or`/`Not` 试图并入他租户行——全部 0 行/`ErrNotFound`，且每步之后以 root 核对租户 B 数据未变；反向对照：B 自己的 Scope 可读写同一批行。另有：INSERT 忽略请求体 `tenant_id`、平台范围用于普通方法与零值 Scope 均在发出 SQL 前失败且数据未变、AcrossTenants 仅平台范围、ActingAs 仍限定目标租户、恶意值只是参数、复合外键对跨租户引用报 1452。**突变验证**：临时去掉构建器的租户条件，上述用例立即失败（10 个子用例 + INSERT + ActingAs），已还原。
  - **三类账号**（AC6）：app 对 `audit_logs`/`wallet_entries` UPDATE/DELETE/TRUNCATE → 1142，ALTER/DROP/CREATE → 1142；maint 可对 `audit_logs` REORGANIZE/DROP PARTITION，DROP 非分区表/改业务表/读业务表/写分区表行 → 1142；migrate 可 DDL 与建触发器；无全局 `SET_USER_ID` 的账号在 binlog=ON、trust=0 下建触发器 → 1419，授予后/trust=1 的第二个临时实例上成功；触发器使 `audit_logs` UPDATE 被 SIGNAL 拒绝。
  - **连接与事务**（AC1）：即使服务端全局 `time_zone='+08:00'`，池内会话仍是 `+00:00`/utf8mb4/`utf8mb4_0900_ai_ci`；非 UTC 的 `time.Time` 写入后库内为 UTC 墙钟、读回为 UTC；4 字节字符往返；`Com_stmt_execute` 递增（服务端预处理，非客户端插值）；事务跨「模块」共用、内层加入不提交、错误/panic/`Goexit` 回滚、不同池的事务互相独立、只读事务拒绝写、`SKIP LOCKED` 并发领取不重复；`ExecOne` 带 `LIMIT 1`。
- `make vuln`：No vulnerabilities found。`make contract-check`：全部通过（未改动契约）。`make build && make verify-build`：15 个产物 ok。

### 三路评审（Blind Hunter / Edge Case Hunter / Verification Gap，均为无上下文子代理）

处置汇总（均已修复并有回归测试，除注明 defer/驳回者）：

| 发现 | 判定 | 处置 |
|---|---|---|
| `ExecOne` 先写后报「多行」，非事务下已改多行 | high | patch：语句带 `LIMIT 1`，删除事后检查；单测 + 真实 MySQL |
| `ForUpdate`/`SKIP LOCKED` 在自动提交下无意义且会重复领取 | high | patch：`Executor.InTx()`，非事务执行器返回 `ErrTxRequired`；单测 + 真实 MySQL 并发领取 |
| `WithTx` 对 `runtime.Goexit`（如 `t.Fatal`）不回滚，事务悬空 | medium | patch：改为「仅提交成功才不回滚」的 defer；真实 MySQL 回归 |
| 执行包装器（Count/Update.Exec/Delete.Exec/AcrossTenants 变体）无有效 Scope 的非 MySQL 单测；`ExecOne` 0 行映射无测试 | medium | patch：`recordingExec` 表驱动单测 |
| `verifySession` 无超时（黑洞数据库使 Open 永久挂起）、排序规则只扫描不检查 | medium | patch：以连接超时包裹校验；检查 `utf8mb4_` 前缀 |
| 非回环主机默认明文传输 | medium | patch：`Validate` 要求显式 `tls`（`false` 为显式明文）；见上「偏差」 |
| 类型化 nil（nil 指针/切片）绕过 `ErrNilValue` 变成 `= NULL` | medium | patch：反射判空，`In` 元素同理 |
| `In` 超大列表触发 MySQL 1390 | low→medium | patch：`MaxInValues=10000` |
| `Select` 无行得到 nil 切片、对已有内容追加（与文档不符） | medium | patch：先重置为空切片；真实 MySQL 回归 |
| `FromPrincipal` 对持有 nil 指针的接口 panic；`ActorID()==0` 的平台/代租户 Scope 审计无法归因 | medium | patch：反射判空、拒绝 0；单测 |
| `TxFromContext(ctx, nil)` panic | low | patch：nil 守卫 |
| `NoTarget` 为可被任意包重赋值的包级变量 | medium | patch：改为函数 `NoTarget()` |
| `repo` 门禁：`New` 前缀（`NewestSites`）整体豁免；接口方法不检查；`//lingedge:` 无空格指令被剥离；多表值夹带；AcrossTenants+豁免无测试 | medium | patch：仅 `New`/`New<大写>` 构造函数豁免；检查接口方法；扫描注释原文；逐表核对；补 5 个负例与 1 个合法例 |
| `forge_named/forge_alias` 用了不存在的字段名，字段改名后仍「通过」；断言子串过弱（`internal`、`base`） | medium | patch：改用真实的未导出字段名，并把断言收紧为完整诊断片段 |
| mysqltest：失败后先删目录再读日志；启动失败被缓存后其余用例静默跳过；经符号链接找到的 mysqld 的 basedir 错误；无 `m.pid` 时遗留实例不回收 | medium | patch：先读日志；启动失败与环境缺失分开缓存（前者一律失败）；`EvalSymlinks`；改按命令行 datadir 扫描进程回收 |
| docs/database §5.1 表格仍写 `ON db.*`，与实现备注自相矛盾 | medium | patch：表格改为逐表授权；ADR-009 增修订记录 |
| `OpenMaint/OpenMigrate` 注释引用不存在的标记 `[db-restricted-pool]` | low | patch |
| `WithTxOptions` 不在 `Pool` 接口 | low | patch |
| CI（`.github/workflows/ci.yml`）未提供 MySQL、未设 `LINGEDGE_REQUIRE_MYSQL`，真实 MySQL 套件在无 MySQL 的 CI 上会跳过 | medium | **defer（等待环境验证）**：无 Docker 约束下 CI 需自行安装 MySQL 8.0，本机无法验证 workflow；跳过时有统一的「等待环境验证」标记，不会假装通过。须在 CI/验收机接入 MySQL 8.0 并设 `LINGEDGE_REQUIRE_MYSQL=1`（登记于下方遗留） |
| `AppConfigFrom` 仅映射基础字段，TLS/池限额无法经 server.yaml 配置；无慢查询日志/指标/请求 ID 透传 | low | defer：属 config schema 扩展与 1.12/1.13/监控 Story |
| 缺 JOIN、聚合、`Between`、原子表达式（`col = col + ?`）、批量 INSERT/upsert | low | defer：按需随首个需要它的 Story 扩展（原子表达式预计随 6.x 钱包/计量） |
| `Ne`/`Not` 对 NULL 行的三值逻辑 | low | patch（文档）：`Ne` 注释说明需 `Or(IsNull, Ne)`；不改语义 |
| 死锁/锁等待超时（1213/1205）的分类与重试助手；事务隔离级别选项；`ReadOnly` 加入已有事务被忽略；`ReadTimeout/WriteTimeout` 缺省无限 | low | defer：随任务运行时（1.14）按实际需要引入；`ReadOnly` 加入时忽略已在文档说明 |
| `maintPoolPaths` 含 `cmd/lingedge-server/`、`scopeMintPaths` 含整个 `internal/platform/` 过宽 | low | 部分接受：`cmd/` 收窄到 server/worker/scheduler；其余是过渡性路径白名单，已在 spec.go 与上文「偏差」写明收窄时机 |
| 平台级表豁免「不校验方法 SQL 只碰该表」；`users` 等是否确无 `tenant_id` | low | 驳回/部分接受：路径级门禁无法分析 SQL 语义，已写明为人工评审点；`TestPlatformTablesMatchDocs` 核对文档无 `tenant_id` 列（`users` 等通过） |
| SQL 拼接守卫「能被 `sqlx` 直调 + `+` 绕过」 | low | 驳回为设计取舍：repository 包按 ADR-009 §6 允许持有 sqlx；`db.Q` 保证经 Executor 的路径，直调 sqlx 属评审点，已在 ADR 备注写明 |
| 遗留 `Com_stmt_execute` 为全局计数、注释笔误、`_RealMySQL` 命名不一致 | low | 部分改进（注释与 1147 断言）；其余不改 |

### AC 对照

| AC | 结果 |
|---|---|
| 1 sqlx 封装：UTC、utf8mb4、parseTime、参数化；`TxFromContext` 传播、跨模块共用同一事务 | 通过（真实 MySQL 回归，含全局时区被改后的会话校验） |
| 2 `Scope` 只能经 `tenantscope.FromPrincipal(...)` 构造；包外无法构造非零 Scope | 通过（字段未导出；`testdata` 四种伪造手法编译失败 + 正向对照；构造入口另有 forbidigo 白名单） |
| 3 构建器自动追加 `tenant_id = ?`；INSERT 强制写 Scope 租户、忽略请求体；`Platform` 仅经 `AcrossTenants` | 通过（SQL 文本单测 + 执行包装器单测 + 真实 MySQL） |
| 4 真实 MySQL：租户 A 的 Scope 读写租户 B → 0 行/未找到（UPDATE/DELETE/按 uid 查询） | 通过（表驱动矩阵 + 突变验证） |
| 5 静态分析：repository 之外引用 `*sqlx.DB/*sqlx.Tx`、租户表方法缺 Scope 首参 → `make lint` 失败；字符串拼接 SQL 被 forbidigo 拦截 | 通过（depguard + `repo-check` + forbidigo，正反自测） |
| 6 三类账号独立连接池；app/maint/migrate 真实账号权限负向；maint/migrate 构造函数仅白名单包可调用 | 通过（三个不可互换的池类型；真实账号 1142/1419 等；forbidigo 白名单含别名导入） |

### DoD（基线 §25）

需求明确、代码完成、编译通过、单元测试（含 `-race`）与集成测试通过、文档更新、本机运行验证：满足。API 可调用、前端功能：本 Story 不适用。**等待环境验证**：CI 无 MySQL 时真实 MySQL 套件将跳过并标「等待环境验证」，须在 CI/验收机接入 MySQL 8.0 并设 `LINGEDGE_REQUIRE_MYSQL=1`；最低版本 8.0.16 属 Story 1.6 的等待环境验证条件，本 Story 未声称覆盖。

### 提交

本地提交（未 push）：哈希见 `git log`；`git add` 逐路径，未含 `_bmad/`、`dist/`、`bin/`、二进制；使用 `git -c user.name=… -c user.email=…`，未改 `git config`。

### 遗留与后续

- **Story 1.6 前置满足**：依赖 1.4、1.5。`db.OpenMigrate`/`MigratePool.SQLDB()` 与迁移白名单路径（`internal/platform/migrate/`、`cmd/lingedge-server/`）已就绪；临时实例辅助（binlog=ON、trust=0、可建 `SET_USER_ID` 账号）与账号/触发器实测可直接复用；1.6 须在迁移与 lint 中落实「app 逐表授权」与 maint 表级授权。
- CI 接入 MySQL 8.0 并设 `LINGEDGE_REQUIRE_MYSQL=1`（含 8.0.16 最低版本，1.6）；`server.yaml` 的 `database` 节增 `tls` 与池限额键；Scope 构造白名单随 1.12/1.14/1.20 收窄；死锁/锁等待重试助手随 1.14。
