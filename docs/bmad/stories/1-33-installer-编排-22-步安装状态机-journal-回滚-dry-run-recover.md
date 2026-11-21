---
title: 'Story 1.33：installer 编排：22 步安装状态机 + journal 回滚 + dry-run + recover'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '54b7257'
waiting_env_verification: true
context:
  - docs/bmad/epics.md Story 1.33（AC1–AC6）
  - docs/architecture/tools-design.md §12（22 步与 journal）、§8、§10、§15、§16；docs/prd/LINGEDGE_V2_REQUIREMENTS.md §4.5（22 步权威）、FR-4/5/6、NFR-3、AC-A-2、AC-A-9、SM-C3
  - docs/bmad/stories/1-25-*、1-28-*、1-28b-*、1-29-*、1-30-*、1-31-*、1-31b-*、1-32-*（本 Story 接线的各库及其「属 1.33」遗留项）
---

## Intent

**Problem:** 1.25–1.32 把安装需要的库都做好了，但没有任何东西把它们串起来：`install`/`recover` 仍是「尚未实现」，没有 22 步状态机、没有「先写 undo 再动手」的步骤外壳、没有失败回滚与崩溃恢复，也没有人对「既有配置不同时绝不覆盖」「dry-run 零副作用」「撤销失败保留 journal」负责。前序 Story 留下的「属 1.33」接线项（下载配置、被动升级确认、`--set`、Plan 展示、`Collect`/`ListenersKnown`、master.key/server.yaml 生成、`daemon-reload` 时机、`recover` 本体……）也都悬着。

**Approach:** 新增 `internal/tools/installer`（`cli → installer → {registry, downloader+manifest, resolver, probe, service, security}`），把 §12.1 的 22 步实现为一个状态机：预检 1–9 只读求值，取包 10–13 经 `downloader.Acquire`（`DeferCommit` + `Finish`，`OnStage` 映射成四个 journal 步），物化 14–18 与激活 19–20 每步先写 undo（`Created` 在动手前由只读检查决定）再执行，第 21 步原子写 `state.json` 作提交点。失败逆序撤销（撤销幂等，可被打断后续做），退出码 3/4/7/8（预检）、5（校验）、10（激活后，状态已恢复）、12（撤销失败，保留 journal）、130（SIGINT）。`cli` 把 `install`/`recover` 接成真实命令，并把 `cli.ServiceError/RegistryError/ResolveError/DownloadError/VerifyError` 汇入同一个 `classify`。**组件规格 `ComponentSpec` 只定义接口，生产构建不含任何实现**（1.35 等 Story 登记）：未登记的组件 `install` 以 `install.component_spec_missing` 明确失败。**没有新增迁移（000001–000018 不变）、没有 OpenAPI/proto 变更、没有改 CF-6 冻结的 schema 文件与 contract-v1、没有新增依赖。**

## 逐条验收

- **AC1 状态机与事件**：`steps.go` 的 22 步表与 tools-design §12.1 逐行一致（`TestStepsMatchDesignDoc` 解析文档表）。每步由同一个外壳 `execution.step` 执行：`plan()`（只读，产出 undo）→ `journal.Begin`（fsync）→ 副作用 → `journal.Done`；journal 的 begin/done 严格交替（`TestInstallNodeHappyPath` 断言归档 journal 为 `open begin:1 done:1 … begin:22 done:22 commit`）。`installer.Event` 经 `Deps.Events` **非阻塞**投递（满/关闭/panic 都被吞掉并计数）：无人读取的无缓冲通道、已关闭的通道、容量 1 的慢消费者下，安装结果与文件系统与「没有 UI」逐项一致（`TestEventConsumerCannotAffectInstall`，含回滚路径）。
- **AC2 dry-run 与非交互**：`--dry-run` = `Installer.Plan`：不取锁、不开 journal、不建目录；对**整棵临时根目录**（含状态目录、锁、journal、staging、信任库、`/etc /opt /var`）前后逐项快照相等（`TestDryRunHasZeroSideEffects`：成功、端口阻断、配置冲突、规格缺失、noop、内置组件各一；cli 层 `TestInstallDryRunZeroSideEffectsAndSchema` 与 `FuzzInstallDryRunSet` 同样断言）。`--non-interactive` 遇冲突以退出码失败（既有内容不同 → 9，端口无法验证 → 4）。
- **AC3 冲突不覆盖（FR-6、SM-C3）**：预检末尾把「既有内容不同的配置/单元（站点同理）、既有目录权限宽于规格或属主不符、符号链接目标」列为冲突并停下；`--yes`/交互 y 之后既有文件**逐字节原样保留**，新内容写 `<文件>.new`；之后失败回滚时既有文件仍原样、`.new` 被撤销（`TestConflictsAreNeverOverwritten`，含既有 `master.key` 沿用）。目标 release 目录已存在且与已验证的包不一致 → 不覆盖，退出码 3。
- **AC4 逆序撤销与退出码**：`TestJournalFaultAtEveryStepRollsBack` 在 journal 的**每一步** begin/done 处注入故障（第 10–21 步，decorator 包真实 journal），回滚后系统整树快照与安装前相同；`TestComponentFailuresRollBack` 覆盖 useradd 失败、enable 失败、启动失败、健康失败（含伪 systemd 状态恢复、`daemon-reload`）；预判 `Created`：预先存在的运行用户不会被回滚删除。撤销失败 → `KindRollbackFailed`（12）且 journal 保留，排除故障后 `recover --rollback` 成功（`TestUndoFailureKeepsJournalAndRecoverFinishes`）；回滚中途 journal 写入失败同样 12（`TestRollbackWithBrokenJournalKeepsIt`）。**真实 SIGINT**：子进程（与生产相同的 `MainEnv` 信号处理）卡在启动步骤时收到 SIGINT → 完成当前步骤、回滚、退出码 130、系统与安装前一致（`TestInstallSIGINTRollsBackWithExit130`）；进程内取消在步骤 14–18 各点注入，断言该步完整到 done 且之后不再开始新步骤。`lingedge recover [--resume|--rollback]` 为真实命令。
- **AC5 提交点与 `list`**：第 21 步在一次原子写里落盘 `state.json`（组件记录 + `host.baota`）；提交前崩溃视为未完成（journal 回滚或续跑）。**提交点之后**的 journal 故障（21 done、22、commit）不回滚已提交的安装（`TestFaultAfterCommitPointDoesNotRollBack`：状态保留、setup token 仍交付、journal 留给 `recover --resume` 收尾）。`lingedge list` 读到持久化的安装状态（`TestInstallCommandEndToEnd`）。
- **AC6 ComponentSpec 与每步前后 kill**：`ComponentSpec`（`Name/Executables/ValidateSettings/InitConfig/Start/Health`）在 `installer/component.go` 文档化；假组件只在 `tests/toolsworld`（G-11）。`TestKill9AtEveryStepThenRecover`：**真实子进程**（测试二进制重新执行）在 **22 步 × 4 个点（before/begun/effect/done）= 88 个点**各 `kill -9` 一次，随后对两个相同的世界分别 `recover --rollback`（整树回到安装前）与 `recover --resume`（允许续跑的点完成安装且最终磁盘与不被打断的安装逐项一致；激活阶段/提交点未完成的点被 `recover.not_possible` 明确拒绝，随后回滚成功）。另有：回滚中途被 `kill -9` 再 recover（5 个撤销点）、续跑后再失败仍能完整回滚（在途步骤先撤销再重做，`Created` 重新判定）、空 journal 丢弃、CLI 层 kill → 13 → `recover` 的 9/0/3 各分支。

## 前序 Story「属 1.33」遗留接线项逐项处置

| 来源 | 项 | 处置 |
|---|---|---|
| 1.28 | `--offline`、`--allow-expired-manifest`、`--channel` | **已做**：`install` 标志经 `Request` 进入预检与取包（`Verify` 与 `Acquire` 同一组参数）；过期清单仅离线放行并告警（`TestManifestExpiryAndChannel`）。 |
| 1.28 | `release.ca_file`、`release.base_url`、代理 | **已做**：tools.yaml 的 `release.*`（`installer.LoadToolsConfig`，严格校验）→ `downloader.Config`；在线安装用真实 HTTPS 测试服务器验证 base_url/ca_file/不可达代理/无 ca_file 的 TLS 失败/缺 base_url 的前置失败（`TestOnlineInstallWiresReleaseConfig`）。 |
| 1.28 | `Config.ResumeDir`、`Params.Executable`、版本选择与 Acquire 串联、`Finish` | **已做**：`release.resume_dir`（缺省 `<状态目录>/downloads`，0700；dry-run 不启用）；`Executable` 来自 `ComponentSpec.Executables`；resolver 选版本 → `Acquire` 精确版本；`finishAll` 嵌套 `Acquired.Finish`（失败不推进 sequence，同一份清单可重试，测试覆盖）。 |
| 1.28 | 进度输出 | **已做**：`installer.Event` + cli 的 stderr 文本/NDJSON。 |
| 1.29 | 写命令入口 Begin、显式 Abort、`recover` 本体 | **已做**：cli 写命令取锁+扫 journal（6/13）；依赖闭包经新增 `Task.Extend` 追加组件锁；可回滚失败路径显式 `Abort`+归档；`Recover` 驱动 `BeginRecovery/UndoPlan`。 |
| 1.30 | 展示 Plan、被动升级确认后重求解、steps 映射、`--set` | **已做**：人类可读 + `data.install`（schema，追加式）；确认后 `AcceptPassiveUpgrade` 重求解；`PlanStepMap` 映射并随输出给出 `step_map`；`ParseSet`（有 fuzz）→ `PortOverrides`/`Settings`；`InstalledVariants` 由 state 推断（website）。 |
| 1.31 | `Collect`、`ListenersKnown=false`、`RecordHostBaota`、`dep.nodejs.not_in_path` | **已做**：`Collect` 入预检；`ListenersKnown=false` 列为需确认冲突（非交互无 `--yes` → 4）；`host.baota` 随第 21 步同一次原子写；`NodeRuntime`（PATH 上为 `node`，否则非固定目录绝对路径）经 `ConfigInput/Runtime` 备好（website 组件本身属后续）。 |
| 1.32 | 安装顺序、journal 写入、recover 驱动 `Undoer`、master.key、server.yaml、`daemon-reload` 时机、LoadCredential 降级 | **已做**：15→16→17→18 顺序与 undo；installer 的 `Undoer` 组合 `service.Undoer`；`master.key`（`GenerateMasterKey`，`FileWriter` O_EXCL）、`server.yaml`（`RenderServerYAML`，**由真实 `config.Parse` 加载校验**，`tests/tools/serverconfig`）、`setup-token.hash`；单元落盘后 `daemon-reload` 再 `enable`，回滚撤销单元后再 `daemon-reload`；降级模式（systemd < 247）单元无 `LoadCredential=`、`master.key` 0440、`master_key_file` 填路径，< 235 失败。 |
| 1.25 | `cli.*Error` 接到真实命令；`install`/`recover` 从「尚未实现」改为真实执行；生产二进制不含 fake | **已做**：`classify` 汇总全部错误映射；`install`/`recover` 真实；`tests/security/toolsbinary` 对 `lingedge` 二进制做符号与字符串扫描（带负向样本）+ `ProductionSpecs()` 为空。 |

## 测试（真实，伪的只有「操作系统」与「组件」）

- **真实**：registry（锁/journal/state.json）、downloader（真实签名的离线 bundle、真实 HTTPS 服务器）、manifest 验签、resolver、service（经真实 `safeexec.Runner` 执行**伪二进制** `systemctl/id/getent/useradd/userdel`，状态落文件、跨进程可见）、真实文件系统上的权限/属主调用记录/符号链接拒绝/`.new`、**真实子进程**的 `kill -9` 与 SIGINT。`internal/tools/installer`、`cli` 的测试用 `tests/toolsworld`（伪操作系统 + 假组件规格，仅测试引用）。真实 Linux/systemd/真实组件 → 等待环境验证（见下）。
- 数量级：installer 40 个测试函数（含 88 点 kill 矩阵 ×2 模式、journal 故障 24 点、dry-run 7 路径）、cli 新增 9 个测试函数、registry/service/downloader 各增 1–2 个、`tests/security/toolsbinary` 与 `tests/tools/serverconfig` 两个跨界包；fuzz 4 个（`FuzzParseSet`、`FuzzParseToolsConfig`、`FuzzDecodeRequest`、`FuzzInstallDryRunSet`，后者走完整 dry-run 流水线并校验 envelope/schema/零副作用）。覆盖率：installer 80.9%、cli 91.1%、registry 85.5%、service 86.6%、downloader 88.6%。

## 变异验证（临时改动 → 测试必须失败 → 还原并逐字节比对）

仓库外脚本逐条最小改动，运行对应测试，**立即还原并断言字节一致**。共 29 条，**全部检出**（其中 5 条最初存活或不可编译，已改进测试/变异后检出）：先写 undo 的顺序（begin 挪到副作用之后）、回滚正序、冲突检查关闭、dry-run 取全局锁（创建状态目录）、覆盖而非 `.new`、master.key 被覆盖、提交点之后回滚、事件发送阻塞、取包即提交 sequence、预判 `Created` 恒真、续跑允许进入激活阶段、续跑不先撤销在途步骤、`state_forget` 空操作（最初存活：测试快照漏掉了 state.json——**测试自身的缺陷**：对目录提前 `SkipDir` 把 state.json 一并跳过，已改为先取完整快照再过滤，并修正 `state_forget` 回滚时凭空创建空 state.json 的真实瑕疵）、SIGINT 不再保护原子步骤（最初存活：取消点测试不够，改为在 14–18 各步取消并断言该步到 done）、`ListenersKnown=false` 被忽略、清单在预检与取包之间变化的比对被删（新增测试）、被动升级不重新求解、安装静默执行升级、取包阶段 journal 步被跳过、Undoer 不校验 `Created`/路径、依赖闭包不追加锁、事件里夹带 setup token、`Ensurer.Plan` 丢祖先、`Task.Extend` 不更新组件表、`OnStage` 漏调用、`cli` 把 10 映射成 12、dry-run 被改成真实执行、步骤表标题漂移、server.yaml 总是填 `master_key_file`。

## 对既有测试/防护的改动（逐条）

- **没有放宽或豁免任何门禁**：G-4/5/6/8/11、depguard、forbidigo、`.golangci.yml`、`scripts/gates` 均未改；`safeexec.DefaultAllowed` 未改。`tests/toolsworld` 在 `tests/` 下（G-11 允许），`make lint security` 均通过。
- **`internal/tools/cli` 既有测试因 `install`/`recover` 从桩变为真实而必须更新（逐条）**：① `behavior_test.go`：「未实现命令」清单与 `--help` 隐藏断言去掉 `install`/`recover`，把它们加入「已交付命令可见」；`TestConfirmationRules` 的「无确认命令作对照」由 `recover` 改用仍是桩的 `update node`。② `registry_test.go`：作为「写命令桩」的 `install server/node` 改用 `update server/node`（锁、13、不创建状态目录等通用网关断言不变）；`recover` 在存在未完成事务时的断言由「尚未实现=1」改为「非交互无标志 → 9（需要明确方式），不被 13 拦截」。③ `exitcode_test.go`、`envelope_test.go`：「未实现命令 → 1」样例由 `install server` 改为 `update server`；新增 `err-install-no-keys`（本构建未内置发行公钥 → 5）快照。④ `validate_test.go`：合法组合里的 `install …` 改为「通过校验进入清单验证 → 5」（新增 `TestInstallValidCombosPassValidation`）；`TestShellMetacharactersAreLiteral` 的 `--set` 值向量期望由 1 改为 5（含换行/控制字符的值被 `ParseSet` 拒绝 → 2，值依旧只当数据、哨兵文件断言不变）。⑤ `verifyerr_test.go`：`--offline … --allow-expired-manifest` 的「落到尚未实现」对 `update` 保持，对 `install` 改为 bundle 不存在 → 2/5。⑥ golden：`help-root.json`（`install`/`recover` 现已出现在 `--help`）、`err-not-implemented.json`（样例命令改为 `update server`）重新生成并人工核对；其余 golden 未变。
- **`cli.Env.withDefaults`**：`TrustDir` 缺省由常量 `DefaultTrustDir` 改为 `<StatePath 目录>/trust`（生产两者相同；使 installer 的 `registry.Layout` 与 `Env.NewVerifier` 指向同一处）。
- **`cli.Run.finish` 重构**：把 switch 提成 `classify`（行为等价，`isParseStageError` 复用它）并加入 `InstallError` 一支；`spec.go` 对结构化错误透传而不是包成 `general.unexpected`。
- **对 1.29/1.32/1.28 包的追加式改动（均有测试）**：`registry.Task.Extend`（+`extend_test.go`）、`service.Provisioner.Exists`、`service.Ensurer.Plan/PlanUndo`、`service.IsManagedPath`（+`dirplan_test.go`）、`downloader.Params.OnStage`（+`onstage_test.go`）。
- 文档：tools-design §3、§12.4（新增）、§15.5、修订记录；本文件；sprint-status。**未碰** OpenAPI/proto/迁移/CF-6/contract-v1；退出码表（§4、cli-tui §4.2）与 envelope schema 未改，一致性测试原样通过。

## 与任务/通常做法不同的设计决定

- **安装流程不执行已装组件的升级**（含被动升级）：确认→重求解的接线与展示是完整的，但执行升级需要备份、迁移决策与健康回滚，属 1.43 的 `update`；计划含 `upgrade` 即 `precondition.upgrade_required`（3）并指向 `lingedge update`。
- **信任状态在安装成功后才提交**（`DeferCommit` + `Finish`），而不是 §12.1 第 11 步「验签后立即写 sequence」：失败的安装不推进 sequence，同一份清单可重试（1.28b 的设计目的）；sequence 仍只进不退。
- **预检 1–9 在 journal 之外求值**，通过后在 journal 里补记（无副作用）：预检失败（3/4/7/8）因此不开 journal、不留任何痕迹；`recover --resume` 看到完整的 22 步进度。
- **取包 10–13 复用 `Acquire` 的四个阶段**（新增 `OnStage` 回调，nil 无影响）而不是拆开下载器；多组件时步骤号只前进不后退，其 staging 由这些步的 undo 整体覆盖。续跑一律重新取包并完整重验。
- **一步内的 undo 按记录顺序执行**（与 `service.DirUndo` 的既有约定一致），由记录者排成撤销顺序；`UndoPlan` 本身仍是步骤的逆序。
- **提交点之后的 journal 故障不回滚**；`recover --resume` 只收尾。
- **`ListenersKnown=false` 当作需确认的冲突**（非交互无 `--yes` → 4），而不是直接阻断或直接放行。
- **install 对 website 单元模板缺失明确失败**（`service.AllUnits` 没有 `lingedge-website.service`），不跳过。
- **tools.yaml 只校验 `release`/`install` 两节**，其余顶层节忽略（1.42 的 schema）；`release.base_url` 的清单路径 `<base>/<channel>/manifest.json` 是 [ASSUMPTION]（UD-3 发行源布局未定）。
- **健康检查窗口缺省 60s/间隔 1s** 是 [ASSUMPTION]（tools-design §12.1 写「待基线测试后确定」），可经 tools.yaml 调整。
- **setup token 在 `--json` 下经 `data.access[].value`（`secret:true`）交付**，因 envelope 的脱敏只按键名与已知前缀工作；它不进日志、事件或任何文件（测试明文扫描）。

## 等待环境验证（代码已完整可用，待 Linux 验收机复验，汇总于 1.45）

1. 真实 Linux + systemd 上的完整安装（`useradd/chown/systemctl` 的真实行为、root 属主与权限位、`/run` 与 `sd_booted`），含降级与 LoadCredential 两条路径（ENV-LNX）。
2. 真实组件（Server/Node/OpenResty…）的 `Start`/`Health` 与真实包（1.35/Epic 3；UD-3 真实发行公钥与发行源）。
3. 真实 `ss`/`/proc` 端口占用、宝塔环境（ENV-BT）下的冲突处置；真实 SIGINT 在真实 TTY 下的交互确认。
4. 在线安装对真实发行源（CDN/镜像、企业代理）的行为。

## 属后续 Story（依据）

- **1.35**（epics 1.35）：`ComponentSpec` 的 server 实现与 `ProductionSpecs` 登记、`/healthz` 健康检查、向导联调与 `install.lock`、`config setup-token --reset`、MySQL/Redis 的警告细节（AC2）。
- **1.40**（epics 1.40）：静态前端的 Nginx 片段与站点同名冲突（`probe.Intent.Names`）；website 的 `lingedge-website.service` 模板与 Node 运行时使用。
- **1.41**：`uninstall`、备份（本 Story 的 journal 备份位置字段 `Backup` 暂未使用）。
- **1.42**：`tools.yaml` 的完整 schema 与 `config validate`；doctor/`status`/`list` 对 `host`、`NodeInstalls`、`PendingNewUnits`、`state.drift` 的展示；`--fix-perms`（本 Story 对既有目录权限不符只列为冲突、不修复）。
- **1.43**：升级执行（含被动升级的实际升级）、`rollback`、`update self`、`--apply-staged`。
- **1.44**：无组件名的多选安装、计划确认页、进度视图（本 Story 的 `Event` 通道与 `data.install.plan` 已就绪）。
- **1.45**：Epic 1 验收与 ENV-LNX/ENV-BT 真机复验。
