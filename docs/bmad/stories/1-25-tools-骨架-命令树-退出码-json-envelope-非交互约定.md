---
title: 'Story 1.25：Tools 骨架：命令树 / 退出码 / JSON envelope / 非交互约定'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'd49c857'
context:
  - docs/bmad/epics.md Story 1.25（AC1–AC7，含 2026-11-16 修订的 AC7）
  - docs/architecture/tools-design.md §3（命令树与全局标志）、§4（退出码）、§5（输出与日志）、§6（JSON envelope）；ux/cli-tui.md §4、§7、§8
  - docs/bmad/architecture-revision-log.md MIN-04、docs/bmad/review-architecture-internal.md M16（标志名漂移）
  - docs/bmad/stories/1-24-*、1-22b-*（Story 文件格式与遗留登记口径）
---

## Intent

**Problem:** `cmd/lingedge` 只有脚手架阶段的 `--version`：没有命令树，退出码与 JSON 输出没有任何可被 Docs/TUI/验收脚本依赖的机器可读契约，标志命名在 ADR/UX/API 文档间漂移（MIN-04）。Tools 泳道后续 Story（1.26–1.45）都需要一个统一的「命令外壳」。

**Approach:** 在 `internal/tools/cli` 实现 Cobra 命令树与输出层：全局标志、退出码常量表（CF-5）、`lingedge.cli/v1` envelope 与 JSON Schema、非交互/确认/root 检查约定、脱敏日志；`cmd/lingedge/main.go` 只做 wiring。未交付的命令全量注册但 Hidden，调用时校验参数后以「尚未实现」失败（退出码 1），绝不假成功。新增 `gates flags` 门禁使旧标志名全仓 0 命中。

## 逐条验收

- **AC1 命令树**：`commands.go` 按 tools-design §3 声明全部命令与标志（install/update[check|self]/service/logs/doctor/config/node enroll/backup/rollback/uninstall/recover，及 version/list/status），全局标志 `--json/--yes/-y/--non-interactive/--dry-run/--log-level/--log-file/--no-color/--config`（另有 `--progress`，tools-design §5）。已交付的只有 `version/list/status`（+ `help`）；其余 `Hidden`（`implemented()` 自动推导，父命令仅在有可见子命令时可见），`lingedge --help` 只列已交付命令。调用未交付命令的顺序：参数/用法校验（2、8）→ root 检查（3）→ 确认（9）→ `cli.not_implemented`（1，并指出交付 Story）。`TestHiddenCommandsAndNoFakeSuccess` 对 22 个未实现入口（含 `version --check`、`list --available`）断言「不以 0 退出」且 `errors[0].code == cli.not_implemented`。
- **AC2 退出码**：`exitcode.go` 常量表（0、1–14、130）；`lingedge --help exit-codes` 与 `lingedge help exit-codes` 输出该表（人类表格；`--json` 下为 `data.exit_codes`）。`TestExitTableMatchesDocs` 解析 tools-design §4 与 cli-tui §4.2 两张表，与 Go 常量表逐项（码+名称）一致；`TestEnvelopeSchemaEnumsMatchExitTable` 令 envelope schema 的枚举与常量表一致。130 的常量名由 `—` 定为 `SIGINT`（见「设计决定」）。
- **AC3 envelope**：`--json` 时 stdout 恰为一个单行 `lingedge.cli/v1` 文档（`ok/command/run_id/exit_code/exit_name/started_at/duration_ms/dry_run/data/warnings/errors`，`data` 失败时 `{}`，数组恒非 null）；Cobra 解析失败、`--help`、`help exit-codes` 同样只输出 envelope。提示与进度走 stderr NDJSON（`type:"notice"`，`--progress=none` 关闭）。schema 在 `internal/tools/cli/schema/`（`envelope.schema.json` + `data.{version,root-version,list,status,help,exit-codes}.schema.json`，`go:embed`）；`TestJSONSnapshotsAndSchemas` 对 11 个真实输出（6 成功 + 5 失败）既做 schema 校验又与 `testdata/golden/*.json` 快照逐字节比对；`TestEverySchema...` 保证每个已交付命令都有 data schema、每个 schema 都有归属；`TestSchemasRejectBadDocuments` 证明 schema 会拒绝坏文档。`run_id` 为 ULID，输出前对 `data/errors/warnings` 统一经 `pkg/redact`。
- **AC4 非交互**：stdin 非 TTY、`TERM=dumb`、`CI=true` 自动等价 `--non-interactive` 并在 stderr 提示一次（显式 `--non-interactive`/`--json` 不提示）；`--json` 即使有 TTY 也不提问。需要确认而未给显式标志 → 退出码 9：`confirmYes`（`--yes`）与 `confirmHostname`（`uninstall --purge` 必须 `--yes --confirm-name <本机主机名>`，不符 → 9）；`update --no-backup`（server 拒绝=2，其余需 `--yes`）、`rollback`、`backup restore`、`node enroll --force-reenroll`、`config setup-token --reset`、`uninstall` 均接入。交互分支（stdin 真实读取 y/yes/主机名）由 `TestInteractivePrompts` 覆盖；交互模式下对「尚未实现」的命令不征求确认（避免无意义的提问）。
- **AC5 root 与空状态**：需 root 的命令在非 root 时退出码 3，提示 `sudo lingedge <原参数>`（参数经 `redactArgs`，`--token` 在提示中为 `***`）；`TestNonRootHintsSudo` 覆盖 12 个需 root 入口。`version/list/status` 对无 `state.json` 返回空（`data.components=[]`）且普通用户可运行；`state.json` 存在时因其读取器属 Story 1.29，**如实**返回 `state.reader_unavailable`（退出码 1），不把已有状态当空展示。
- **AC6 校验与脱敏日志**：参数违规 → 退出码 2（组件名枚举、`--channel/--mode/--component` 枚举、`--version/--to` semver、`--set key=value`、`--setup-listen host:port`、`--server` URL（拒绝内嵌口令）、`--since` 时长、`-n` 正整数、互斥组合、缺参数；`TestUsageErrors` 34 个违规 + 7 个合法组合）。`scheduler|worker` → 8。日志 JSON Lines（`ts/level/run_id/step/msg/fields`，默认 `/var/log/lingedge/tools.log`，显式 `--log-file` 不可写=3、缺省不可写则静默禁用；文件 0640），argv 经 `redactArgs`（`--token X`/`--token=X`/`--set 敏感键=值`/`--*-password`，`*-file` 路径不算机密）+ `pkg/redact` 兜底。`tests/security/logscan_tools_test.go`（G-5）以运行期随机的不透明令牌/口令/DSN 口令经真实 `cli.Execute` 注入后扫描日志、stdout、stderr；**变异验证**：把 `redactArgs` 临时改为恒等返回，该测试与 `TestRedactArgs` 均失败，还原后通过。
- **AC7 旧标志名**：新增 `scripts/gates/flags.go`（`gates flags`，接入 `make security` 与 `gates all`，含自测与 `tests/gates` 的副本正/负向用例）：全仓文本文件 `--allow-rollback`、`--yes-irreversible`、`--reenroll`、`--allow-expired`（不含 `-manifest` 后缀者）0 命中。ADR-012 §1.6/M16、ADR-013 §2.5/M16 里点名旧标志的修订说明已改写为新名或不点名旧名；`cli-tui.md` 退出码表与 tools-design 逐项一致由测试断言；setup 完成后统一 `410 setup.already_installed` 已在 openapi/cli-tui/tools-design 一致（grep 复核，无需改动）。

## 改动清单（主要文件）

- 新增：`internal/tools/cli/{exitcode,envelope,env,run,root,spec,commands,cmd_read,state,validate,confirm,logger,signal,schema,util}.go`、`internal/tools/cli/schema/*.json`（7 个）、`internal/tools/cli/testdata/{golden,examples}`；测试 `internal/tools/cli/{helpers,exitcode,envelope,behavior,validate,logging}_test.go`、`cmd/lingedge/main_test.go`（真实二进制）、`tests/security/logscan_tools_test.go`；`scripts/gates/flags.go`。
- 修改：`cmd/lingedge/main.go`（只剩 `cli.Main`）、`scripts/gates/{main.go,gates_test.go}`、`tests/gates/gates_selftest_test.go`、`Makefile`（`security` 增 `gates flags`）、`go.mod/go.sum`（`spf13/cobra v1.10.2`、`pflag` 提升为直接依赖；`go mod tidy` 另把 `pquerna/otp` 由 indirect 改为直接，属既有漂移）、文档：`docs/architecture/tools-design.md`（§4 的 130 命名 + 修订记录）、`docs/ux/cli-tui.md`（§4.2 同步、§7 改为引用 tools-design §5/§6 的 envelope）、`docs/ux/ux.md`（UX-17）、ADR-012/013（去掉对旧标志名的点名）。
- 既有测试/防护的改动（逐条）：① `tests/scaffold/scaffold_test.go`：`cmd/lingedge/main.go` 的 import 白名单由 `os,pkg/version` 改为 `os,internal/tools/cli`；`TestCmdsPrintRealVersion` 对 `lingedge` 的「无参数退出码 2 + 尚未交付」断言改为「无参数（管道）打印帮助、退出 0；未知命令退出 2」（`--version` 的输出断言保持不变且通过；其余 6 个二进制不变）。② `tests/gates/gates_selftest_test.go`：干净副本门禁清单增加 `flags`，并新增 2 个 `flags` 负向用例（只增不减）。③ `scripts/gates/main.go` 的 `gates` 表增加 `flags`（只增不减），`scripts/gates/gates_test.go` 增 `TestFlags`。**没有**放宽或豁免任何门禁：G-6（固定口令）、G-5（日志脱敏）、arch/depguard/forbidigo 规则均未改；我的测试曾被 G-6 门禁命中（含 `password=` 后紧跟引号字面量形态的拼接字符串与 URL 内嵌口令形态的字面量），已改为运行期随机值/拼接键名，未加豁免。

## 设计决定与说明

- **脱敏复用 `pkg/redact`，不在本 Story 建 `internal/tools/security`**：`pkg/redact` 本就是「Server、Tools、Node 共用」的脱敏引擎且 `internal/tools` 可 import `pkg/`；`internal/tools/security`（安全解压/safeexec/权限）属 1.26，届时的 `redact` 能力应直接复用 `pkg/redact`，避免两套脱敏规则。
- **退出码 130 命名为 `SIGINT`**：envelope 的 `exit_name` 必须有稳定名称，tools-design/cli-tui 原表为 `—`；已在两份文档同步（仅命名，码值不变）。错误码 `interrupt.sigint`。
- **`--help exit-codes` 的实现**：Cobra 会把 `--help` 之后的 `exit-codes` 当未知子命令而报错，故在 `Execute` 里把它改写为 `help exit-codes`（`rewriteHelpTopic`）；`lingedge help exit-codes`（cli-tui §8 的写法）等价。
- **未实现命令的退出码 1 / `cli.not_implemented`**：退出码 7（NOT_RELEASED）、14（NOT_INSTALLED）等有各自语义，不能借用；`install scheduler|worker` 的 8 是静态规则（D-16），本 Story 已真实实现。`install dns` 的 7 依赖清单状态，随 1.27/1.35。
- **`state.json` 存在时的行为**：不猜测 registry（1.29）的文件格式；存在即报 `state.reader_unavailable`（1）。在 1.29 之前没有任何组件会写这个文件，因此不影响真实使用，但也不会产生「读到了却显示为空」的假象。`StateSource` 接口与 `ComponentInfo` 已固定 `list` 的输出形状，`TestNonEmptyStateRendering` 用消费方侧的实现验证渲染与 schema（不声称 state.json 读取已实现）。
- **`status`**：本 Story 只汇总 registry 记录的安装状态（`name/version/state`）；systemd 状态与健康探测随 1.42 以新增字段补全（schema 只增不改）。`version --check`、`list --available` 隐藏并返回 `cli.not_implemented`（清单/下载属 1.27/1.28）。
- **`--config`**：显式指定的文件必须存在（否则 2），但 `tools.yaml` 的 schema 与加载属 1.42（`config validate`）；本 Story 不静默忽略也不假装解析。
- **`--dry-run` 不豁免 root 检查**：tools-design §1 的权限模型按命令划分，dry-run 的预检可能依赖 root 才能读的信息，故保持一致。`--plan-only` 等价 `--dry-run`（envelope `dry_run=true`）。
- **envelope 增加可选 `errors[].system_state`**（沿用 cli-tui 原 `error.system_state` 语义，只增字段）；`command` 对带组件的命令包含合法组件名（`install node`），非法输入不回显进 `command`。
- **ux/cli-tui §7 与 tools-design §6 原先矛盾**（`schema_version`/单个 `error` 对象/`--stream` stdout JSON Lines vs `schema`/`errors[]`/stderr NDJSON）：以 epics AC3 与 tools-design §6 为准，已回写 cli-tui §7 与 ux.md UX-17。

## 测试（真实、无 Mock）

- `internal/tools/cli`：命令树、校验、确认、日志、envelope 均为生产代码；测试只决定进程环境量（TTY 与否、有效 UID、主机名、时钟、`state.json`/日志所在的真实临时目录）。非空 `StateSource` 用例与 `fixedState` 只是 `list/status/version` 渲染与 schema 的消费方侧验证。
- `cmd/lingedge/main_test.go`：真实编译二进制，管道（无 TTY）+ `TERM=dumb` 下断言进程退出码（0/2/3/8）与 envelope 一致、stdout 恰一个 JSON 文档、无参数打印帮助且只有一次自动非交互提示。
- 手工 PTY 冒烟（本机，一次性，未纳入自动化）：用 Python `pty` 在真实伪终端下运行 `lingedge install nope`（红色 ✗、版式五段、退出码 2）、`lingedge list`（无提示）、`lingedge`（⚠ 菜单未交付 + 帮助）、`TERM=dumb`（自动非交互提示）。
- 参数注入向量：10 个 shell 元字符向量（`;`/`&&`/`|`/`$()`/反引号/换行/引号逃逸/`${IFS}`/路径穿越/`-rf`）分别作为组件名、`--set` 值、`--version` 值，断言哨兵文件未被创建、输出仍为合法单 JSON、日志逐行合法 JSON。

## 门禁结果

提交前在本机（macOS arm64，临时 mysqld 8.0.46 / redis 由 tests/mysqltest、tests/redistest 自启并清理，未触碰常驻 mysqld/6390 redis）同步执行：`gofmt -l .` 无输出；`make lint`（0 issues）、`make security`（含新 `gates flags`）、`make gate-selftest`、`make race`（`-race -timeout 30m ./...`，后台运行并等待结束，退出码 0、61 个包 ok、无 DATA RACE）、`make vuln`（No vulnerabilities found）、`make contract-check`、`make build`、`make verify-build`、`make migrate-test` 全部通过。

## 遗留与风险（只列确属后续 Story 或需特定环境的）

- **等待环境验证（ENV-LNX）**：① 以真实 root 在 Linux 上运行（本机仅验证「非 root → 3」的真实进程路径与注入 `Geteuid()==0` 的 root 路径）；② 缺省日志 `/var/log/lingedge/tools.log`（root 0640，目录属主/权限）与 `/etc/lingedge/tools/tools.yaml`、`/var/lib/lingedge/tools/state.json` 的真实路径行为；③ 真实 SIGINT（Ctrl-C）下的 130：本机仅验证「被取消的 context → 130」（`Main` 把 SIGINT 绑定到该 context，当前没有长任务可在进程级复现）。
- **Deferred（确属后续 Story）**：交互主菜单/多选/计划确认/进度/不可逆确认键入短语（1.44，TUI）；全部业务命令的实现（install server 1.35、静态前端 1.40、backup/restore/uninstall 1.41、doctor/config/service/logs/status/list 完整版 1.42、update/rollback/update self 1.43、安装编排与 `recover` 1.33、`node enroll` 在 Epic 3）；`state.json` 读写与任务锁（1.29）；清单/下载（1.27/1.28，`version --check`、`list --available`、退出码 5/7）；端口探测（1.31，退出码 4）；回滚/journal 相关退出码 10–13 的生产者（1.33/1.43）；`tools.yaml` 的 schema 与 `config validate`（1.42）；`internal/tools/security`（1.26）；命令树完整性检查（1.45，epics 已指定）；Docs 站点发布退出码表（6.13）。
- **已知限制（如实）**：`.golangci.yml` 与依赖图未改（`internal/tools/cli` 只依赖 `pkg/*` 与第三方库）。~~`--json` 判定依赖对 argv 的预扫描~~ **已在 Story 1.27b 关闭**：解析失败后改用 pflag 自身语义（同一命令的完整标志集、`--` 终止、取值消耗、`strconv.ParseBool`、后者覆盖前者）重扫，见 `jsonAfterParseFailure` 与 `TestJSONEnvelopeOnParseFailureMatchesPflag`。
