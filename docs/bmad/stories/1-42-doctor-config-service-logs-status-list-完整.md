---
title: 'Story 1.42：doctor / config / service / logs / status / list（完整）'
type: 'feature'
created: '2026-11-24'
status: 'review'
route: 'dispatch'
baseline_commit: '5c6e4f6'
waiting_env_verification: true
context:
  - docs/bmad/epics.md Story 1.42（AC1–AC5）
  - docs/architecture/tools-design.md §3、§6、§15、§19；ADR-003 §3–§5、ADR-020、ADR-026、ADR-027
  - docs/bmad/stories/1-31-*.md、1-32-*.md、1-33-*.md、1-40-*.md、1-41-*.md
---

## Intent

**Problem:** `lingedge doctor|service|logs|config validate|show|edit` 全是「尚未实现」的占位（退出码 1）；`status`/`list` 只有 registry 记录的安装状态——没有真实 systemd 状态、没有健康探测、没有进程归属；`tools.yaml` 只有 `release`/`install` 两节被校验；1.40/1.41 留给本 Story 的 `doctor` 检查（静态前端深度检查、`backup.*`、`sys.time_sync`）没有载体。

**Approach:** 两个新包 + cli 的真实命令：`internal/tools/doctor`（检查项注册表 + 内置检查，World 全部可注入）、`internal/tools/cfgcheck`（带「键 + 行 + 列」的配置校验）；`service.Systemctl.Show`（`systemctl show` 固定属性集）；safeexec 的流式入口 `Runner.Stream`（`logs -f`、`config edit` 的编辑器）；`lingedge-server config check`（与 Server 启动同一个 `config.Parse`，Tools 不 import Server 的包）；`list/status/service status` 共用一份「真实状态 + 健康」视图。不造新机制：退出码表、envelope、registry 任务锁、probe/baota/service/backup 的既有能力全部复用。**无迁移、无 OpenAPI/proto 改动（无 CC-14）、不改 CF-6/contract-v1/迁移 000001–000018。**

## 逐条验收

- **AC1**：`doctor [--all|<组件>]` 按注册表输出 `pass|warn|fail|skip`（稳定 `id/category/message/hint`，`data.checks[]` 有 JSON Schema），有 `fail` → 退出码 3（data 仍在 envelope 里），`--strict` 把 warn 视同 fail；`--fix-perms` 仅 root，只修复 `master.key`/配置/节点 identity 的「权限宽于规格」与「属主不符」（chmod 经 `O_NOFOLLOW` 句柄、chown 用 `Lchown`、符号链接拒绝、绝不创建/读取内容、逐项输出并复检、`--dry-run` 只列计划）。
- **AC2**：检查项 sys.\* / port.\* / baota.\* / dep.\* / svc.\* / health.server / fs.\* / cfg.validate / state.drift / journal.incomplete / backup.recent / ver.\* / `cert.node_mtls`（共约 40 个 id，清单见 tools-design §19.1）；注册表可扩展（`Registry.Register`，后续 Story 追加 node/dns/disk，id 形态/前缀/唯一性由框架强制）；`cert.node_mtls` **剩余 < 总有效期 1/6 才告警**（续期窗口 1/3 内不告警，边界值逐点测试）；Server gRPC 叶证书与私有 CA（CA < 1 年告警）经 TLS 握手读取；`sys.time_sync`（`timedatectl` + 可选真实 SNTP + 时钟回拨判定）；`backup.master_key_separate`。
- **AC3**：`config validate [<c>|tools] [--file]`（问题指出键路径/行/列，不回显取值，非零退出 3）；`config show` 脱敏（结构化 + 逐行兜底）；`config edit` 编辑副本→校验→原子替换（`.bak` 同权限同属主）→可选重启（`--restart`/交互询问）。`tools.yaml` 完整 schema 定稿（新增 `log/backup/doctor` 节）。
- **AC4**：`service status|start|stop|restart`（未安装 → 14；写命令取组件锁、root）、`logs <c> [-f] [--since] [-n]`（`journalctl` 参数数组、输出逐行脱敏、PEM 整块隐去）。
- **AC5**：`status`/`list`/`service status` 汇总每个已装组件的**真实 systemd 状态**与**健康探测**；`--json` 可解析（schema）；`list` 输出端口/配置目录/进程归属（FR-58）；有不健康组件 → 退出码 11。

## 发现的缺口与矛盾（须披露；文档已修正）

tools-design §19/Story 原文**没有说明**（已写入 tools-design §19.1、epics 实现澄清、`docs/operations/diagnostics.md`）：

1. **`backup.master_key_separate` 的「记录」没有来源**。决定：`tools.yaml backup.custody_fingerprint`（运营者另存主密钥后写入指纹前缀），只提示、不读取不校验保管位置。
2. **`sys.mem/disk`「高于清单要求」只有联网验签后才有**（Tools 不内置数值）。决定：清单可用时以清单为准（低于 = fail）；否则退回 `tools.yaml` 的下限（缺省 256/512，**[ASSUMPTION]**）；`backup.recent` 阈值缺省 168h（**[ASSUMPTION]**，原文「默认待用户确认」）。
3. **「时钟偏移与回拨」没有可检测的定义**。决定：偏移来自 `timedatectl`/可选 SNTP；「回拨」= 当前时钟早于 Tools 自己记录过的时间点（state.json、信任库、journal）。**联调中真实触发**：伪时钟（2026-11-21）早于真实文件时间戳时 doctor 正确地判定回拨（`TestDoctorDetectsClockRewindAgainstRealFiles`）。
4. **Server 叶证书/CA 的来源**：Tools 不读数据库。决定：TLS 握手读取 `grpc.listen` 的证书链（gRPC 角色属 Epic 3，监听不存在 → skip，如实）；ADR-020 没有给出 `identity/` 的文件名，按内容识别（私钥块一律忽略）。
5. **`baota.firewall`「只读判断」的边界**：只能读 ufw/firewalld 的配置文件；iptables/nftables 无法只读判定 → warn 并说明；云安全组不可读（输出固定提醒）。
6. **`config validate server` 的校验来源**：Tools 不得 import `internal/platform/config`。决定：新增 `lingedge-server config check`（同一个 `config.Parse`），Tools 用键路径定位行列；二进制不可用时退回 `cfgcheck.CheckServerShape` 并**标注「部分校验」**；`tests/tools/serverconfig` 反射对照键集合、对照真实 `Parse` 的接受/拒绝。
7. **`tools.yaml` 其他节的校验行为变更**：1.33 对 release/install 之外的顶层节「忽略」；现全部严格校验并拒绝未知键（拼写错误不再被静默吞掉）。
8. **`service stop node` 的语义**：ADR-020 节点自治——只作用于 Agent，不停 OpenResty 数据面。
9. **`config edit --json`**：`--json` 隐含非交互（全局约定），编辑器需要终端 → 退出码 9（`--dry-run` 例外）。编辑器在 safeexec 固定的 `LC_ALL=C` 下运行（环境清洗不允许覆盖），文件内容按字节原样保留。
10. **`ver.update_available` 的级别**：「有更新」是 pass 且消息列出（避免 `--strict` 因可选更新而失败）；原文 §19 表里的 `trust.manifest_valid` 由同一份清单顺带产出。
11. **`docs/database/README.md` §5.1 把「`pmax` 非空的告警/doctor 检查随 1.42」挂到本 Story**，但 Story 原文与 §19 表都没有它。按该约定交付 `dep.mysql.pmax`（迁移账号逐表 `COUNT(*) … PARTITION (pmax)`，表名经标识符白名单）。
12. **信封脱敏按键名替换值**：doctor 的 `details` 键不得含敏感词（实现时把 `backups_with_master_key` 改为 `co_located_backups`，与 1.41 的同类问题一致）。
13. **G-6 门禁与测试夹具**：测试里的口令样例一律运行时拼接（`"hunter" + "2"`），门禁未豁免。
14. **SIGHUP 热加载不在本 Story**：原文与 tools-design 都没有它；它是 ADR-026「Deferred」的「热重载运行期口令」，前提是连接池整体替换（1.27b 已记录「仓库没有任何 SIGHUP 基础设施」）。归属：后续（ADR-026 的 P3/升级流程 Story，1.43 之后评估）；`config edit` 的重启走 `systemctl restart`（ADR-026 明文 M1 用 restart）。

## 偶发失败排查：`tests/release` 的 4 个用例（根因已定位并复现）

**症状**：整轮 `make race` 下 `TestFrontendPackagesInPublishedRelease` 等偶发一次失败（包内缺 `payload/public/index.html`），单独重跑与再次整轮均通过。

**根因（有证据）**：`frontend/admin/vite.config.ts` 的 `emptyOutDir: true` 会在 `vite build` 开始时清空 `frontend/dist/admin`。流水线 `scripts/release build` 在第 3 步校验 `-frontend-dist` 目录（`index.html`、mock 入口），随后编译 Go 目标（数十秒～数分钟），第 7 步才从**同一个活目录**打包——旧实现中间没有任何保护，测试 `distOrFixture` 又把活的 `frontend/dist/admin` 直接交给它。任何并发的前端构建（另一个门禁/开发者）落在这个窗口内，包就悄悄缺少 `index.html`，**流水线仍然成功**。
**复现**：新增回归用例 `TestFrontendDistMutatedDuringBuildStillYieldsIntactPackage`（Go 编译期间清空活目录再恢复）。在**旧实现**上运行：`build` 成功但 `stage/admin-web-0.1.0-any/payload/public/index.html: no such file or directory`——与观察到的症状逐字一致。
**加固**（不放宽任何断言）：① 流水线：build 一开始把前端目录**快照**到 `<out>/work/frontend/<组件>`，复制前后与快照三份目录摘要必须一致（否则明确失败「目录在复制快照期间被改动」，不采用状态不明的拷贝），校验与打包只读快照；打包后再确认 `payload/public/index.html` 存在且非空（`stagePackage` 的纵深防御，单测直接驱动）；② 测试：`distOrFixture` 先把真实 dist 复制到独立临时目录并核对复制前后摘要一致（不一致重试，持续变化则改用夹具并如实记录）；③ 回归用例 + `snapshotFrontend` 的单测（复制期间改动/清空/删除/新增、符号链接、缺目录）。修复后回归用例通过；原 4 个用例与 `TestFrontendDistGuards` 不变且通过。运行手册已补充（`docs/operations/release.md` §12）。**说明**：偶发那一次的具体并发进程无法回溯（当时只留下「单独重跑通过」），根因是按机制与复现定位的；修复后本机整轮 `make race` 通过（见下方门禁）。

## 交付物

- `internal/tools/doctor/`：`doctor.go`（注册表、Finding、校验与脱敏）、`world.go`（可注入的 World）、`checks_sys|net|svc|fs|fe|cert|state.go`、`timesync.go`（timedatectl/真实 UDP SNTP/回拨判定）、平台文件。
- `internal/tools/cfgcheck/`：`yamlcheck.go`（语法/重复键/多文档/形状/类型 + 键路径→行列索引）、`tools.go`、`server.go`（结构退路 + 键集合导出）、`generic.go`（语法-only、Nginx 片段）。
- `internal/tools/cli/`：`cmd_doctor.go`、`cmd_config.go`、`cmd_service.go`（service + logs + 流式脱敏）、`cmd_read.go`（status/list 重写）、`inspect.go`（systemd/健康/进程/站点视图）、`ops.go`/`ops_sources.go`（`Env.Ops` 注入、World 装配、清单/MySQL 来源）、`opsutil.go`、`commands.go`/`root.go`/`spec.go`/`run.go`（命令接线；结果与错误并存；tools.yaml `log` 节缺省）、schema：`data.doctor|logs|config-validate|config-show|config-edit|service-status|service-start|stop|restart` 与 `status/list` 增量。
- `internal/tools/service`：`Systemctl.Show`、`RunUnits/StatusUnits/LogUnits`；`internal/tools/security`：`safeexec.Runner.Stream`、`WriteFileAtomicPre`；`internal/tools/probe`：`Options.ListenersOnly`；`internal/tools/installer/toolsconfig.go`：完整 schema + `Problems()`。
- `cmd/lingedge-server/configcmd.go`：`config check`。
- `scripts/release`：前端目录快照与一致性核对、包内 index.html 断言。
- 文档：tools-design §19.1 与修订记录、`docs/operations/diagnostics.md`（新）、`release.md` §12、`backup-restore.md`、`docs/database/README.md`、epics 实现澄清。

## 测试（全部真跑；伪的只有「操作系统」）

- `internal/tools/doctor`（覆盖率约 92%）：注册表（前缀/重复/越权 id/违规/panic/脱敏/扩展）；每个检查组表驱动并含边界值——节点证书 1/6 阈值逐点（90/31/30/16/15/14/1/过期）、自定义总期限、新旧叶证书并存取最晚、私钥块忽略；**真实 x509**；sys.time_sync（偏移/未同步/回拨/容差/来源出错）+ **真实 UDP SNTP 服务器**（偏移符号、origin 不匹配/stratum 0/非 server 模式/零时间戳被拒、无应答超时）；权限检查与 `--fix-perms`（真实文件 chmod、内容不变、范围外文件不动、符号链接拒绝、dry-run、非 root、属主经 Chown、缺失不重建）；静态前端深度（缺资源/gz 不一致/非法 gzip/current 错版本/越界引用/空 index、片段静态检查失败/缺文件/conf.d 软链 ours|foreign）；state.drift 七类偏差；journal；backup.\*；ver.\*（吊销/更新/预发布不算/即将过期/验签失败 fail/网络失败 warn/离线不取清单）；端口/宝塔/防火墙（ufw 规则、firewalld 端口段与服务）/依赖/MySQL 设置/pmax。
- `internal/tools/cfgcheck`：14 类 tools.yaml 破坏（键+行号）、语法/多文档/顶层非映射/超大、**问题文本从不回显取值**、server 结构校验、键集合、Locate 前缀回退、`sanitize` 抹取值、schema 覆盖每个节。
- `internal/tools/cli`（`ops_cmd_test.go`）：status/service status（真实伪 systemd 状态翻转 → 11）、service start/stop/restart（只停 Agent、state.json 更新、健康失败 → 11、dry-run、未安装 14、非 root 3、组件锁 6、静态前端 3）、logs（脱敏扫描：Bearer/DSN/lenr1/PEM/口令行，argv 数组、`--json`、`-f`）、`lineFilter` 边界、list（进程归属/站点/`--available`）、doctor（JSON/退出码/strict/组件过滤/真实漂移/`--fix-perms`）、**真实 TLS 握手取证书链**（含 TLS1.2 拒绝、明文端口、关闭端口）、**真实 HTTP** 的 health.server、config validate 破坏用例（键+行）/show 脱敏/edit（**真实编辑器进程**：成功/校验失败保留副本/重新编辑/无改动/非交互 9/非法编辑器/非 root/静态前端）、pmax 生产路径（伪 mysql 客户端 + 恶意表名）、tools.yaml `log` 节。
- `tests/integration/toolsinstall/ops_test.go`（**真实 lingedge-server 进程 + 临时 MySQL 8.0 + 临时 Redis + 真实 mysql 客户端 + 真实 HTTPS 发行源 + 真实 UDP SNTP**）：doctor 全量（health/依赖/真实 `config check`/真实 SNTP/真实验签清单/state/journal/备份新鲜度/主密钥记录/pmax 在真实分区表上 warn→pass）、时钟回拨对真实文件时间戳、`list --available` 的真实 HTTPS 验证链（篡改签名 → 5）、`config validate|edit server` 对真实二进制（问题带行号、被拒绝的修改不落盘）、status 对真实 server（停进程 → 11）。
- 其余：`cmd/lingedge-server`（`config check`）、`internal/tools/service`（Show/parseShow/单元映射）、`internal/tools/security`（`WriteFileAtomicPre`、`Stream` 的白名单/NUL/环境注入/退出码脱敏/不截断/取消）、`internal/tools/probe`（`ListenersOnly` 不连接 MySQL/Redis 端点——真实监听端验证）、`internal/tools/installer`（全部 tools.yaml 节）、`tests/tools/serverconfig`（键集合与真实 `Parse` 对照）、`scripts/release`（快照/阶段断言）、`tests/release`（回归用例）。

## 变异验证

对关键防线逐条临时改动 → 对应测试必须失败 → 还原，共 **41 条**，**全部检出**：主密钥权限检查恒通过、符号链接不拒绝、`--fix-perms` 不要求 root、dry-run 仍修复、节点证书阈值 1/6→1/3、CA 阈值、时钟回拨不判定、SNTP 不核对 origin、state.drift 不核对 current、journal 不算死亡事务、backup.recent 计入安全备份、主密钥同处备份被忽略、静态前端不核对 gz/引用资源、注册表不校验前缀/不脱敏、`--strict` 失效、防火墙判定恒通过、端口被占也通过、日志不脱敏/PEM 不隐去、`service stop node` 同时停数据面、status 忽略 systemd 未运行、start 后不等健康、config show 不脱敏、edit 不校验/不写 .bak/编辑器白名单失效/stdin 被吞、tools.yaml 允许未知节/行号失效/回显取值、server 口令规则失效、Show 属性不校验、Stream 不做白名单、原子写 pre 被忽略、流水线快照不核对/不使用快照/不核对包内 index.html、pmax 非空不告警/表名不经白名单。其中 **5 条初次未检出**，据此补强后检出：问题文本回显取值（补 `sanitize` 直接单测）、流水线包内 index.html 断言（它在正常路径上不可达，补直接驱动 `stagePackage` 的单测）、pmax 表名白名单（补伪 mysql 客户端 + 恶意表名用例）、另有 2 条是变异写法无效（编译失败）后重写。

## 对既有测试/防护的改动（逐条，均为随功能交付的必要更新，无放宽）

| 文件 | 改动 |
|---|---|
| `internal/tools/cli/behavior_test.go` | service/logs/doctor/`config validate\|show\|edit` 与 `list --available` 自 1.42 起已实现：从「未实现」清单移到「已交付」清单（`--help` 可见项追加 service/logs/doctor）；`version --check`、`update`、`rollback`、`node enroll` 仍在未实现清单且期望不变（退出码 1/`cli.not_implemented`）；`TestNonEmptyStateRendering` 的第二个组件由 docs 改为 reporter 并注入「无 systemd」（status 现在做真实健康探测，伪造的 docs 没有发布物会如实报告不健康）——断言（schema、组件数、人类输出）不变 |
| `internal/tools/cli/registry_test.go` | `TestListStatusFromRealRegistry`：status 在该夹具下（server 未监听、docs 无发布物）按新语义退出 11，改为断言 11 且 data 仍符合 schema；list/version 仍断言 0 |
| `internal/tools/cli/validate_test.go` | 合法组合 `logs node --since 10m -n 5` 通过校验后落到命令本身（node 未安装 → 14，原为落到「未实现」1）；新增专门用例；其余用法错误用例（退出码 2）不变 |
| `internal/tools/cli/helpers_test.go` | 新增 `noSystemd()` 夹具选项 |
| `internal/tools/cli/testdata/golden/{help-root,status-empty}.json` | 重新生成：help 列出 service/logs/doctor；status 的 data 新增 `summary`（只增字段）|
| `internal/tools/cli/schema/data.{status,list}.schema.json` | 追加字段（`healthy/systemd/health/summary`，`process/site/available/manifest`）；新增 9 个命令的 schema |
| `internal/tools/installer/set_test.go` | `TestParseToolsConfig` 的文案更新（log 节不再「忽略」而是严格校验，同一份输入仍合法）；新增 `TestToolsConfigAllSections`（拒绝项只增） |
| `internal/tools/installer/toolsconfig.go` | `Validate` 保持「返回第一个问题」，改由 `Problems()` 提供全部；错误文案由「release.base_url 不合法（…）」改为「release.base_url 必须是…」（既有测试只断言错误种类，全部通过）；路径要求增加「规范形式」（`filepath.Clean` 相等）——这是**收紧** |
| `internal/tools/security/safeexec`、`security/perm.go`、`probe/collect.go`、`service/systemctl.go|units.go` | 追加：`Runner.Stream`、`WriteFileAtomicPre`、`Options.ListenersOnly`、`Show/RunUnits…`；`Run` 重构为 `prepare` + 执行，行为不变（既有测试全绿） |
| `tests/toolsworld/world.go` | 伪 systemctl 增 `show`、调用日志（写在 `ctl/`，既有的树快照比对不受影响）；新增伪 journalctl/timedatectl/openresty |
| `scripts/release`、`tests/release` | 见上「偶发失败排查」；既有断言全部保留 |
| `scripts/gates`、`.golangci.yml`、`safeexec.DefaultAllowed`、`SafeExtract` 限额、tools 白名单、G-6 | **未改动、未豁免** |

## 属后续 Story / 代码已完成待环境验证

**代码已完成，待环境验证（ENV-LNX / ENV-BT，汇总于 1.45）**：真实 systemd 下 `systemctl show` 的输出格式与 `NRestarts` 语义、真实 journald 的 `logs`（含非 root 的 `systemd-journal` 组）、root 属主下真实 `--fix-perms` 的 chown、真实宝塔上的 `baota.*`（含 ufw/firewalld 的真实配置文件位置与格式）、`timedatectl` 在各发行版的输出、节点真实 `identity/` 文件布局与 Server gRPC 叶证书/CA 的真实握手链（本机用真实 TLS 套接字与真实 x509 验证了逻辑）、真实 systemd 下 `config edit --restart` 的重启与健康等待、`service stop/start` 对真实 Agent 的行为。

**属后续 Story**：`health.node`（Agent 连通/心跳/配置代 `nginx -t`）与 `node.*`/`dns.*`/`disk.*` 检查（3.31、4.x，注册表已就绪）；`node/dns/reporter` 配置 schema（Epic 3，现只校验语法并如实标注）；`version --check` 与 `update check` 的清单比对（1.43）；TUI 的状态/日志/配置页（1.44）；SIGHUP 热加载（ADR-026 Deferred，见上）；`doctor` 对 Server 叶证书/CA 的数据库侧到期告警（Admin 系统页）；真实发行密钥与发行源（UD-3）。

## 门禁（本机，顺序同步运行，无并行门禁）

`gofmt -l .` 无输出；`make lint security gate-selftest vuln contract-check build verify-build migrate-test frontend-check` 退出码 0（首轮 `freeze-check` 因 tools-design 一行提到 `partition.maintain` 未标注「内部周期作业」失败，已改文案，不是豁免，重跑全绿）；`make race` 退出码 0（82 个包 ok）；未涉及浏览器/前端源码，`make e2e` 未运行。结束时无本任务遗留进程（常驻 mysqld、他项目的 redis/qemu 不属本任务）。
