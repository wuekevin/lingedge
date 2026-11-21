---
title: 'Story 1.6：Migration 框架（goose 嵌入 + 分类 lint + migrate 子命令）'
type: 'feature'
created: '2026-11-17'
status: 'in-review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '024463390c26ab212bbe348bebb99c4292233d1d'
context:
  - docs/bmad/epics.md（Story 1.6 全文，约 941–968 行）
  - docs/bmad/decisions-adopted.md（UD-2、AA-7）
  - docs/architecture/adr/ADR-016-migration-tool.md、ADR-013-upgrade-rollback-migration-policy.md
  - docs/database/README.md（§2.2、§5.1、§8、§8.1、§8.2、§9）、docs/database/verification/
  - docs/bmad/contract-changes/CF-1.md（迁移清单 CHECKSUMS 与 freeze-check：已发布迁移只追加）
  - docs/bmad/stories/1-5-db-基础设施与租户-scope-构建器.md（「app 须在迁移内逐表授权」发现、tests/mysqltest）
---

<frozen-after-approval reason="human-owned intent — do not modify unless human renegotiates">

## Intent

**Problem:** 迁移是升级与回滚承诺的载体，但仓库里没有统一的迁移执行器：没有强制的分类标注（reversible/expand/irreversible），MySQL 的非事务 DDL 一旦中途失败就留下无人知晓的半成品，已应用迁移被改动无人发现，多进程同时迁移没有互斥，已知会让 8.0 报错的 DDL 组合（生成列×外键、CHECK×外键、前向外键、分区表外键/pmax）只能在真实库上才暴露，Story 1.5 发现的「`lingedge_app` 必须逐表授权」也没有任何东西强制。

**Approach:** 以 goose v3 为库嵌入 `lingedge-server migrate plan|up|down|status|recover|lint`；在其外层自建迁移分类与结构 lint、DDL 预检（静态，与真实 MySQL 实测逐例比对）、逐迁移 dirty 标记与校验和、`GET_LOCK` + `job_locks` 租约互斥、版本门槛与账号预检；用独立临时 mysqld 上的真实集成测试（故障注入、进程被杀、篡改、并发、M1 骨架重放）验证，而不是 Mock。

## Boundaries & Constraints

**Always:** 迁移经 `lingedge_migrate` 账号执行；`schema_migrations` 为自定义版本表名；dirty / 校验和不一致 / lint 不通过 / 版本低于 8.0.16 一律拒绝执行（fail-closed）；失败的迁移不得被记为已应用；SQL 参数化；goose 版本固定且 `make vuln` 无漏洞；测试用临时实例必须独立 datadir/端口/socket 且用后关闭并删除；环境缺失（8.0.16）如实标「等待环境验证」。

**Never:** 触碰本机常驻 MySQL 实例；Docker；Mock 冒充生产；修改 `contract-v1` 已冻结内容（OpenAPI / proto / 已发布迁移 / `docs/database/verification`）；push；改 `git config`；改动 `SmartCDN`；在生产代码里加「检测到测试环境」的分支；越界实现 Story 1.7–1.9 的真实迁移（`migrations/` 只放框架，不放表）。

</frozen-after-approval>

## Code Map

- `migrations/embed.go` -- `migrations.FS`（`//go:embed *`）；目录内仅 `CHECKSUMS` 与本文件，真实迁移随 Story 1.7 落库。
- `internal/platform/migrate/` -- 迁移框架：`sqltok.go`（轻量词法/语句切分）、`source.go`（文件解析：goose 段/StatementBegin/文件头/Down 标记；`Load`）、`lint.go`（分类/命名/序号/幂等/DDL-DML 分文件/逐表授权）、`ddlcheck.go`（DDL 预检规则 + MySQL 错误号）、`plan.go`（plan JSON）、`runner.go`（版本门槛、账号探针、状态读取、up/down/recover、dirty/校验和）、`lock.go`（GET_LOCK + job_locks 租约）、`errors.go`（类型化错误与退出码）、`cli.go`（子命令、凭据来源）。
- `cmd/lingedge-server/main.go` -- `migrate` 子命令入口（其余子命令随 1.13）。
- `internal/platform/config/secretfile.go` -- `ReadSecretFile`（与 `*.password_file` 同一套检查）。
- `scripts/gates/{spec.go,genlint.go}`、`.golangci.yml`（生成）-- goose 只许出现在 `internal/platform/migrate`；该包可用 `database/sql`/sqlx。
- `scripts/contract/migrations.go` -- 新增迁移第一行须是合法 `-- lingedge:class=…`；`embed.go` 不是迁移。
- `Makefile` -- `migrate`、`migrate-test`（原为 `NOT_IMPLEMENTED(story 1.6)`）。
- `tests/migrations/` -- 迁移测试台（真实 MySQL）；`ddl-rules.sql`、`m1-fk-skeleton.sql`、`run_ddl_rules.py` 是 `docs/database/verification` 的逐字节副本；`cmd/migratebin` 供「杀进程」用例。
- `tests/fixtures/migrations/` -- 夹具：`good`、`extend/{expand,contract,tampered}`、`fault`、`crash`、`dml-atomic`、`lint-fail/<33 个用例>`。
- 文档：`docs/database/README.md`（§5.1、§8、§8.3、§9.1）、ADR-016 实现记录、`docs/architecture/dev-environment.md`、`README.md`、`internal/platform/doc.go`。

## Tasks & Acceptance

**Execution:**
- [x] `internal/platform/migrate` -- 解析/lint/DDL 预检/plan（AC1、AC2、AC6）
- [x] `internal/platform/migrate` -- runner：互斥、dirty、校验和、版本门槛、账号探针、up/down/status/recover（AC3、AC4、AC6）
- [x] `cmd/lingedge-server`、`migrations/embed.go` -- `migrate` 子命令与嵌入（AC1）
- [x] `tests/migrations`、`tests/fixtures/migrations` -- 测试台：空库全量、reversible 升降级、expand 兼容性、分类标注、DDL 规则 26 例、M1 骨架重放、故障注入（AC4、AC5、AC6）
- [x] `Makefile`（`migrate`、`migrate-test`）、门禁规则与自测、契约工具收紧
- [x] 文档同步、`sprint-status.yaml`
- [x] 全部门禁 + 三路评审 + 本地提交

**Acceptance Criteria:** 见 `docs/bmad/epics.md` Story 1.6 的 AC1–AC6（原文为准）。

## Verification

**Commands:**
- `make lint`、`make security`、`make gate-selftest`、`make test`、`make race`、`make vuln`、`make contract-check`、`make build && make verify-build`、`make migrate-test`

## Spec Change Log

## Review Triage Log

（三路评审的逐条处置见「开发记录 · 三路评审」。）

## 开发记录

**开发会话**：2026-11-17 至 2026-11-19，bmad-build（非交互、完全自主）。环境：macOS（Darwin 25.6.0 arm64）、go1.27.4、MySQL 8.0.46（Homebrew，仅用独立临时实例）。

### 范围决策与偏差

- **前提偏差（如实记录）**：任务描述称「真实重放既有 13 个迁移文件」，但仓库里**没有任何真实迁移**——`migrations/` 只有 `CHECKSUMS` 与 `.gitkeep`，000001–000013 由 Story 1.7–1.9 创建。13 个迁移的唯一现成材料是 `docs/database/verification/m1-fk-skeleton.sql`（只保留主键/外键/生成列/CHECK/分区/触发器的规则验证骨架）。因此「M1 61 表空库 0 错误」的重放以**骨架按 §8.1 分段转换成的 13 个迁移文件**为对象（`tests/migrations/skeleton_test.go`：补 `IF NOT EXISTS`/`DROP TRIGGER IF EXISTS`/`guard` 标注/逐表授权；骨架没有 000004，补一个最小的仅 DML 种子替身）；转换后的 13 个文件通过完整 lint 并经框架在真实 MySQL 上 0 错误重放。这检验的是框架与 DDL 规则，**不等于**真实迁移已验证；真实迁移落库后必须同样通过本测试台（1.7–1.9 的 AC）。
- **`docs/database/verification` 没有搬走，而是在 `tests/migrations` 放了逐字节副本**：原件被 CF-1 冻结复核引用（`freeze-check` 读取 `m1-fk-skeleton.sql`，CF-1.md 记录其路径），搬走等于改动冻结基线；`TestRegressionAssetsAreIdenticalToFrozenDocs` 守住两份一致。AC6 的「用例与期望错误码不变」成立（`run_ddl_rules.py` 在新位置原样运行 26/26；另有 Go 版逐例断言）。
- **互斥与 `job_locks`**：AC3 写「首次无 `job_locks` 表时回退 `GET_LOCK`，之后用 `job_locks` 租约」。实现为 `GET_LOCK` 始终持有（互斥本身），`job_locks` 存在后另记录租约（holder、递增 fence、lease_until）。原因：`job_locks` 由 000002 创建——若「有表则只用租约」，用 GET_LOCK 起步的进程与之后改用租约的进程之间出现互斥缝隙。这是对 AC3 措辞的强化（一个超集），写入 ADR-016 实现记录。
- **未实现（有意延后）**：ADR-016 决策 4 的 `system_settings.schema.latest_class_irreversible_applied`——`system_settings` 随 Story 1.7（000002）才存在；`recover --mark-applied`（单语句 DDL 已生效而版本表未记录时的人工补记）。
- **AA-7 过渡（2026-11-21 已定稿，ADR-026：以下环境变量保留为最高优先级显式来源，仅追加 `migrate.yaml`/交互来源）**：迁移账号凭据只经环境变量 `LINGEDGE_MIGRATE_DB_USER`（缺省 `lingedge_migrate`）、`LINGEDGE_MIGRATE_DB_PASSWORD` 或 `LINGEDGE_MIGRATE_DB_PASSWORD_FILE`（权限检查同 `*.password_file`），口令不经命令行（有测试）；AA-7 定稿后只改 `cli.go` 的凭据来源。
- **`up` 的 `--accept-irreversible`**：ADR-013 把确认交互放在 Tools；服务端同样拒绝「含不可逆迁移而未确认」（退出码 9），纵深防御，且「一个迁移都不执行」。
- **goose v3.30.1（2026-11-17 查询的最新版）以 Provider 库形式使用**，`WithTableName("schema_migrations")`；goose 的 MVS 要求带动 `x/sys`、`x/text`、`genproto`、`multierr` 小版本上升（必要，`make vuln` 无漏洞）。goose 与 `database/sql` 只允许出现在 `internal/platform/migrate`（depguard 规则新增并有正反自测）。

### 交付物

`migrations/embed.go`；`internal/platform/migrate/`（词法与语句切分、文件解析与分类、lint、DDL 预检、plan、runner、互斥与心跳、错误与退出码、CLI；约 4800 行含测试）；`cmd/lingedge-server`（`migrate` 子命令）；`internal/platform/config/secretfile.go`；`scripts/gates`（goose 边界规则与自测）；`scripts/contract`（class 标注收紧、`embed.go` 不是迁移）；`Makefile`（`migrate`、`migrate-test`）；`tests/migrations`（真实 MySQL 测试台，36 个用例）与 `tests/fixtures/migrations`（good/extend/reversible/fault/crash/dml-atomic/down-fail 与 45 个 lint 负向夹具）；文档同步（database/README §5.1/§8/§8.3/§9.1、ADR-016 实现记录、dev-environment、README、decisions-adopted AA-7 过渡约定）。

### 迁移框架行为（摘要，细则见 docs/database/README §8.3）

`plan`（JSON，含 `has_irreversible`，连库时先校验完整性）｜`up`（lint → 版本门槛 ≥ 8.0.16 → 账号预检（GRANT OPTION、SET_USER_ID/1419）→ 互斥 → dirty/未知/校验和/乱序校验 → 逐个标记 running → goose → applied；含不可逆须 `--accept-irreversible`）｜`down`（只回退 reversible，整体先校验，`--steps/--to/--all`）｜`status`（`--json`，退出码反映 dirty/校验和问题）｜`recover --version N`（防呆，不是绕过校验的通道）｜`lint`。退出码 0/1/2/3/4/5/6/7/8/9/10（10 = 数据库与二进制的迁移集合不一致）。

### 实际验证（本机 macOS arm64、go1.27.4；MySQL 8.0.46 独立临时实例）

- `make lint`（vet×2、gofmt、scaffold、arch、golangci-lint `0 issues`、shell、repo-check）、`make security`、`make gate-selftest`（含新增 goose 边界的正反自测）、`make test`、`make race`、`make vuln`（No vulnerabilities found）、`make contract-check`（含 `freeze-check OK … 迁移顺序与骨架一致（M1 61 表）`）、`make build && make verify-build`（15 个产物 ok）全部通过。
- `make migrate-test` 通过（内嵌迁移 lint ✓；`internal/platform/migrate` 44 个单测；`tests/migrations` 36 个用例，0 跳过）。关键事实：
  - **AC6 DDL 规则**：`ddl-rules.sql` 26/26 在真实库逐例通过，且静态预检的判定与真实库错误号逐例一致（`TestStaticDDLCheckAgreesWithExpectedMySQLResults` 不连库常驻；`TestDDLRulesOnRealMySQL` 连库）；原 `run_ddl_rules.py` 原样 26/26；骨架空库 0 错误、67 表；静态预检对整份骨架 0 违规。
  - **M1 重放**：骨架转成的 13 个迁移经框架空库 `up` 0 错误、61 张业务表、`lingedge_app` 61 条表级授权（`audit_logs` 仅 SELECT, INSERT）、`lingedge_maint` 仅 `audit_logs`/`node_heartbeats` 的 SELECT, ALTER, DROP、`job_locks` 租约已记录并在结束后过期；重放全部可重入语句库结构与授权不变；改动已应用的 000007 被校验和拒绝；expand 拒绝回退。
  - **AC5**：空库全量升级、reversible 升/降/升往返结构一致、`--steps/--to/--all` 三种回退、expand 兼容性（上一版本二进制的读写 SQL 在 expand 之后仍可用；contract 删列后必然出错，证明该测试有区分力）、分类标注 lint 负向 45 例（每例触发恰好其 EXPECT 规则）。
  - **AC3/AC4 故障注入（全部真实）**：中途失败（`CREATE TABLE … LIKE` 一个尚不存在的表，ERROR 1146）→ 版本表无记录、半成品真实存在、dirty、`up/down` 均拒绝并给出恢复指引、`recover` + 排除故障 + 重跑后与一次性执行**逐表逐授权等价**；**进程被 SIGKILL**（独立的 `migratebin` 进程，迁移执行到休眠语句时杀死）→ 运行记录停在 running，同样 dirty，恢复后等价；纯 DML 迁移失败整体回滚（0 行残留，ERROR 1048）；Down 自身失败（ERROR 1091）→ `failed|down`，`recover` 恢复为已应用；两个并发 `up` 合计恰好应用一次，另一 migrate 运行时第二个在超时后拒绝（退出码 7）；长迁移期间心跳使 fence 递增、租约有效。
  - **账号预检**：binlog=ON、trust=0 的实例上缺 `SET_USER_ID` 在迁移开始前即被拒绝（真实 ERROR 1419，库保持为空），授予后通过；trust=1 实例无需该权限；缺 GRANT OPTION 同样预检拒绝（探针表与探针授权已清理）。
  - **CLI 与二进制**：`migrate.Main` 全命令面（plan/up/status/down/recover/lint，含 `--config`、仅环境变量、口令文件权限 0644 被拒/0600 通过、错误口令不泄露、失败时仍输出已应用部分）；真实构建的 `lingedge-server` 二进制 `migrate lint/plan/up/status` 与退出码。
  - **突变验证**：临时禁用校验和比对、dirty 检查、授权 lint、DDL 规则 1，对应测试均立即失败，已还原。
- `pgrep -fl mysqld` 仅剩本机原有两个进程（PID 25651/25755，全程未变）；`$TMPDIR` 下无 `lemy*` 残留。

### 三路评审（Blind Hunter / Edge Case Hunter / Verification Gap，均为无上下文子代理）

处置汇总（均已修复并有回归测试，除注明 defer/驳回者）：

| 发现 | 判定 | 处置 |
|---|---|---|
| 旧写法 `-- lingedge:class expand`（无 `=`）通过 lint，Class 为空，绕过分类规则与不可逆确认 | high | patch：第一行必须严格匹配 `class=<值>`（含前导空白）；单测 + 夹具 |
| `recover` 对 down 方向忽略版本表已删除该版本：重置为 applied 后下次 up 撞主键冲突，永久卡死 | medium | patch：down 且 goose 已无记录 → 删除运行记录；真实库回归 |
| 运行记录为 applied 而版本表没有：`markRunning` 撞主键 | medium | patch：`ErrInconsistent`（退出码 10）+ 回归 |
| `recover`（up 已落库只缺标记）覆盖原校验和，把被篡改文件「洗白」；且 recover 不做完整性校验 | high | patch：保持原校验和并核对文件；其余完整性问题照常拒绝；回归（篡改被拒、记录不变） |
| `ErrUnknownApplied`/`ErrOutOfOrder` 退出码为通用失败，Tools 无法区分「二进制比数据库旧」 | medium | patch：退出码 10；乱序/二进制旧/记录矛盾各有真实库回归 |
| 持锁连接无心跳：长迁移期间可能被回收而静默丢锁；租约只在迁移之前续一次 | medium | patch：心跳协程（缺省 30 秒，可配）校验并续约，丢锁即中止；真实库回归（fence 递增） |
| 单语句 DDL 已生效而版本表未记录时，「重跑等价」的说法过强 | medium | patch：恢复指引与文档如实说明；`recover --mark-applied` 延后（见下） |
| expand 规则与可重入规则互相矛盾：视图/过程/函数/事件的 `DROP … IF EXISTS` 被判破坏 | medium | patch：紧邻同名 CREATE 的 DROP 是重建守卫；孤立 DROP 仍判破坏；单测 |
| GRANT lint 漏洞：`TO lingedge_app, other_user`、`WITH GRANT OPTION`、裸词 `user@host`；REVOKE/CREATE USER/SET GLOBAL/USE 等完全不检查 | high | patch：每条 GRANT 只授一个账号、禁尾随子句、识别裸词主机；新增 `forbidden-statement` 规则；夹具与单测 |
| 可执行注释 `/*! … */` 被词法层丢弃，其中的 DROP/外键开关对规则不可见 | medium | patch：`executable-comment` 规则；单测 + 夹具 |
| lint 按任意分号切分，goose 按行尾分号切分（同一行多语句、字符串内行尾分号）：lint 看到的不是执行的 | medium | patch：标出并报 `syntax` 规则；单测 + 夹具 |
| expand 不检查 `ADD COLUMN … NOT NULL` 无默认值与 `DELETE` | medium | patch：并入 `class-expand`；单测 + 夹具 |
| 预检不验证 GRANT OPTION（ADR-016 决策 6），缺失时迁移到第一条 GRANT 才失败并 dirty | medium | patch：探针表向自己授权；清理探针授权；真实库回归 |
| `plan` 不校验完整性：dirty/篡改时给出看似正常的计划 | medium | patch：plan 先校验；另一个 migrate 运行时返回 `ErrMigrationRunning`（退出码 7），status 标 `in_progress`；回归 |
| `LINGEDGE_DATABASE__HOST` 使 `wantsDB` 为真却不被 `dbConfig` 读取；`--host` 无 `--port` 时端口为 0 | medium | patch：读取 `LINGEDGE_DATABASE__*`、缺省 3306；`--config` 与仅环境变量的真实库回归 |
| 多余位置参数使其后的选项被静默忽略（`up 5 --to 3` 升级全部） | high | patch：任何多余位置参数即用法错误（退出码 2）；单测 |
| `down --to 0` 与文档「= --all」不符、负数/互斥选项无校验 | medium | patch：显式 `--to 0` = 全部；负数与互斥组合拒绝；CLI 真实库回归 |
| `--lock-timeout` 小于 1 秒被截断为 0/默认 30 秒 | medium | patch：校验 ≥ 1s，`GET_LOCK` 向上取整；单测 |
| `markFailed` 按字节截断错误文本，中文被切断后严格模式 UPDATE 失败、失败原因丢失 | medium | patch：按字符截断；单测 |
| Ctrl-C 恰在 goose 提交之后、`markApplied` 之前：已完整应用的迁移被标 dirty | medium | patch：脱离取消的 ctx 写入标记 |
| 失败的 up/down 的 CLI 丢弃已成功的部分结果 | low→medium | patch：失败时仍输出已应用部分（含 `--json`）；真实库回归 |
| 大写 `.SQL` 被契约工具登记为已发布、却永远不被 lint/执行；Go 迁移文件同理 | medium | patch：`name` 规则报出；夹具 |
| `CREATE TABLE … LIKE` 浅拷贝，对副本的 ALTER 污染源表模型 | medium | patch：深拷贝；单测 |
| `ALTER` 未知表被登记为已知（拼错表名使后续外键绕过前向外键检查）；`RENAME` 不更新模型；`RENAME TABLE` 后新名无授权 | medium | patch：不登记占位；模型跟踪 RENAME；改名后要求授权；单测 + 夹具 |
| 乱序迁移检测、`Down` 的 `--steps/--to/--all` 成功路径、`recover` 的各分支、失败的 `down` 均无测试 | medium | patch：真实库回归（乱序、三种回退方式、失败的 down、两种丢标记场景） |
| `--config` 路径无测试（`EnvIgnore` 被删也全绿） | medium | patch：真实库回归（`--config`、仅环境变量、未知覆盖仍被拒） |
| 版本门槛只测了字符串解析，`begin/Plan/Status` 的接线未在不受支持服务器上验证 | medium | **defer（等待环境验证）**：本机无 8.0.15/MariaDB，且不得为测试在生产代码加注入口；已登记于 `environment_verification`（8.0.16 与当前 8.0.x 各跑一遍，含版本门槛的真实拒绝） |
| 仓库 CI 不运行 `make migrate-test`，真实库套件在无 MySQL 的 CI 上会跳过 | medium | **defer（等待环境验证）**：CI 尚未在远端运行、无法验证 MySQL 安装步骤；与 Story 1.5 同一遗留，已登记 |
| `recover --mark-applied`（单语句 DDL 已生效而版本表未记录的人工补记） | low | **defer**：当前恢复指引如实说明（确认对象就位后从备份恢复或修正重跑）；随首个需要它的 Story |
| `ALTER … PARTITION BY` 于已有表需 maint 授权、`CONVERT TO/ENGINE` 不属 expand、未命名外键的 `DROP FOREIGN KEY` 建模 | low | **defer**：lint 的静态近似边界，已在 `ddlcheck.go` 头注释写明局限；真实库的空库全量 `up` 兜底 |
| `VALUES()` 在 8.0.20+ 已弃用 | low | **驳回**：行别名语法自 8.0.19 起才有，而最低支持版本是 8.0.16；仅产生弃用警告 |
| `go.mod` 带动的间接依赖升级 | low | **驳回**：goose 的 go.mod 最低版本要求（MVS）所致，`make vuln` 无漏洞 |
| `migrate lint` 在零迁移上通过 | low | **驳回**：设计如此（真实迁移随 1.7 落库；`TestGoodFixturesAreLintClean` 与负向夹具守住规则本身） |

### AC 对照

| AC | 结果 |
|---|---|
| 1 goose v3 嵌入 `embed.FS`；`lingedge-server migrate plan\|up\|down\|status`；`plan` 输出 JSON（序号、名称、分类、是否含 Down、`has_irreversible`） | 通过（真实 `lingedge-server` 二进制与 `migrate.Main` 的真实库回归；`recover`/`lint` 为补充子命令） |
| 2 迁移 lint：缺 class / expand 含 DROP·RENAME·MODIFY / reversible 无可执行 Down / irreversible 的 Down 非空无原因 → 失败；命名六位序号 | 通过（45 个负向夹具恰好触发各自规则；另有幂等、DDL/DML 分文件、逐表授权等规则） |
| 3 迁移互斥（GET_LOCK + job_locks 租约）；`schema_migrations` 自定义表名；dirty 拒绝并给恢复指引 | 通过（并发/超时/心跳真实库回归；dirty 的退出码与指引有断言；互斥实现为 AC 措辞的超集，见偏差） |
| 4 中断重跑等价；回填拆 DDL/DML | 通过（中途失败与进程被杀两种真实故障注入，重跑与一次性执行逐表逐授权等价；DML 迁移事务内整体回滚；lint 强制分文件） |
| 5 `make migrate-test`：空库全量、reversible 升降级、expand 兼容性、分类标注检查；fixtures 仅在 `tests/fixtures/` | 通过 |
| 6 DDL 预检五项（生成列×FK、CHECK×FK、前向 FK、分区表无 FK 且含 pmax、迁移账号探针）+ `ddl-rules.sql`/`run_ddl_rules.py`/`m1-fk-skeleton.sql` 回归 + 版本门槛 8.0.16 | 通过（本机 8.0.46）；**8.0.16 最低版本空库全量 `up` 标「等待环境验证」**，本机 8.0.46 通过不代替该项 |

### DoD（基线 §25）

需求明确、代码完成、编译通过、数据库迁移框架完成（真实迁移属 1.7–1.9）、单元测试（含 `-race`）与集成测试通过、关键异常测试（中途失败/崩溃/篡改/并发/权限）通过、文档更新、本机运行验证：满足。API 可调用、前端功能：本 Story 不适用。**等待环境验证**：8.0.16 最低版本；CI 接入 MySQL 8.0。

### 提交

本地提交（未 push）：哈希见 `git log`；`git add` 逐路径，未含 `_bmad/`、`dist/`、`bin/`、二进制；使用 `git -c user.name=… -c user.email=…`，未改 `git config`。

### 遗留与后续

- **Story 1.7 前置满足**：依赖 1.6。1.7 起真实迁移放入 `migrations/`，须通过 `make migrate-test`（lint 含逐表授权、DDL 预检、重入）；其 AC2 的 schema lint（无 ENUM、TEXT/JSON/BLOB 无 DEFAULT 字面量、`DATETIME(3)`、租户表索引以 `tenant_id` 打头、无浮点金额）可在同一 lint 框架（`lint.go`）追加；`system_settings.schema.latest_class_irreversible_applied` 随 000002 落库后补；`job_locks` 的真实列须含 `name/holder/fence/lease_until`（可有 `acquired_at/updated_at`）以启用租约记录。
- 8.0.16 与当前 8.0.x 各跑一遍 `make migrate-test`；CI 接入 MySQL 8.0 并设 `LINGEDGE_REQUIRE_MYSQL=1`；AA-7 定稿后调整 `cli.go` 的凭据来源；`recover --mark-applied` 视需要。
