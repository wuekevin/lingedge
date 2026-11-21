---
title: 'Story 1.29：registry——安装状态 / journal / 任务锁'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'f384ff4'
context:
  - docs/bmad/epics.md Story 1.29（AC1–AC5）
  - docs/architecture/tools-design.md §8、§12.3、§16、§4（退出码 6/13）、docs/prd/LINGEDGE_V2_REQUIREMENTS.md（FR-4、FR-6）
  - docs/bmad/stories/1-25-*、1-26-*、1-27-*、1-28-*、1-28b-*（cli 骨架与 StateSource、security 叶子包、FileSequenceStore、Acquire）
---

## Intent

**Problem:** Tools 还没有「本机装了什么」的持久记录，也没有事务日志与任务锁：并发安装/升级无法被拒绝，进程被 `kill -9` 或断电后无法知道做到了哪一步、该撤销什么；1.27 的 `FileSequenceStore` 与 1.28 的 `Acquire` 都明确写着「无跨进程锁，靠 1.29 的全局任务锁保证」；1.25 的 `list/status` 在 state.json 存在时只能返回占位错误 `state.reader_unavailable`。

**Approach:** 新增 `internal/tools/registry`（只依赖标准库与 `security`，依赖图 registry → security）：state.json（schema 版本化、不含密钥、0644、经 `security.WriteFileAtomic` 原子写）、journal（每条 fsync、先记 undo 再动手、撕裂尾行容错、可归档、幂等落地助手）、flock 全局锁与组件锁（字典序、非阻塞、持有者信息、内核释放无陈旧锁）、写命令入口 `Begin`（取锁→扫描 journal→6/13）。`cli` 经 registry 读取 state.json（替换 1.25 占位）并给写命令接上 6/13。**没有新增迁移（000001–000018 不变）、没有 OpenAPI/proto 变更、没有改 CF-6 冻结的 schema 文件、没有新增依赖**（`oklog/ulid` 已在 go.mod）。不实现 1.33 的 22 步编排：只提供它直接可用的 API。

## 逐条验收

- **AC1 state.json**（`state.go`）：
  - 原子写：`SaveState` 校验→序列化→`security.WriteFileAtomic`（同目录临时文件、`fsync`、`rename`、目录 `fsync`），权限与 umask 无关（`umask 077` 下仍为 0644，测试覆盖）。读不加锁，因为 rename 保证读者看到旧版或新版的完整文件。**故障注入（全部真实）**：① 真实 `write(2)` 中途失败——子进程 `RLIMIT_FSIZE`=2048（忽略 SIGXFSZ）写 60 个组件的状态，`SaveState` 报错、旧文件字节不变、不留临时文件；② `rename` 失败（目标是非空目录）与目录不可写（root 下跳过）；③ 子进程持续 `UpdateState` 时被真实 `kill -9`，25 轮，期间读者线程在紧循环里 `LoadState`：永远读到完整文件、60 个组件版本号一致（不会混合新旧）、版本号单调不减；被杀后 `CleanStaleTemps` 回收 `.state.json.tmp-*`。
  - schema 版本化（`schema_version`，当前 1）：缺失/非整数/<1 → `state_corrupt`；**>1 → `state_unsupported`，失败关闭且绝不改写该文件**（`UpdateState` 同样拒绝，字节不变，测试断言）；=1 严格解析（未知字段、尾随数据都算损坏，避免旧程序静默丢字段后回写）；更低旧版本经 `migrateState` 内存迁移（目前没有旧版本，函数与规则已就位）。
  - 不含密钥：写入前对每个字符串值做 `security.RedactString` 比对，**脱敏后会变就拒绝写入**（`state_secret`/格式校验），而不是悄悄改写。注入式泄漏扫描：7 种密钥形态（`password=…`、`token=…`、`le_live_…`、`lenr1.…`、URL 内嵌口令、Bearer、`api_key:`）× 10 个字符串字段，全部被拒、磁盘旧文件字节不变、落盘文件不含任何注入串。
  - 读取安全：`O_NOFOLLOW`；须为普通文件、属主为当前用户或 root、group/other 不可写、≤4 MiB，否则 `permission`（符号链接/目录/他人可写均测试）。
  - 其余校验：组件名、semver、状态枚举（installed|running|stopped|failed）、unit/run_user 格式、端口范围与去重、目录须为规范绝对路径、时间 RFC3339、`last_tx` 为合法事务 ID。
- **AC2 journal**（`journal.go`、`journal_read.go`、`idempotent.go`）：
  - 记录 `{"v":1,"seq":N,"ts":…,"op":open|begin|done|undone|commit|abort,…}`；**每条一次 `write`（O_APPEND）+ `fsync`**（测试用包装真实 `Sync` 的计数钩子证明 N 条记录恰好 N 次 fsync），文件创建时另 fsync 目录；文件 0600、目录 0700。
  - **先记 undo 再动手**：`Begin` 返回时，磁盘上（另一次 open 读到的内容）已有含 undo 的 begin 记录，此时副作用尚未发生（测试断言）。undo 字段写入前过泄漏检查；`sha256` 须为 64 位小写十六进制。
  - 次序约束（写入端与读取端共用同一状态机，不会漂移）：同时至多一步在途；`done` 对应在途 `begin`；已 done 的步不可重新 begin，在途步可（`recover --resume` 的幂等重做）；有在途步不可 `commit`；结束后不再有记录。**非法序列不落盘**（测试逐项断言文件大小不变，之后日志仍可用）。
  - 容错解析：**只有最后一行允许是坏的**（撕裂尾行，置 `TornTail`/`TornOffset`）；中间任何一行坏（JSON、未知字段、seq 跳号、次序违规、超长行）→ `journal_corrupt` 并返回已解析的前缀；`v>1` → `journal_unsupported`。测试在**每一个字节偏移**截断一个真实 journal，解析必须成功且结果恰为完整行的前缀（另有 NUL 串、半个 JSON、空行、未知字段等 6 种尾部损坏）；3 MiB 超长行不会撑爆内存。
  - 回滚/续跑：`Tx.UndoPlan()`（含在途步、按首次 begin 逆序）、`ResumeFrom()`、`InFlight()`；`BeginRecovery`/`ResumeJournal` 取组件锁与 journal 自身的 flock、截掉撕裂尾行（缺换行的有效尾记录补换行）后再追加；`undone` 记录使恢复可被中断续做。
  - 归档：`Journal.Archive()`（rename 到 `journal/archive/`，不覆盖同名归档，目录 fsync）与 `ArchiveFinished`（补做被中断的归档）。
  - **每步幂等**（`EnsureFile`/`EnsureDirAll`/`EnsureSymlink`）：不存在则创建（文件经 `security.WriteFileAtomic` 写临时文件再 `link(2)` 落名——目标已存在则失败，**不可能覆盖并发创建的同名文件**；16 个并发创建者恰好 1 个 `created=true`）；已存在且一致视为完成且 `created=false`（所以不会被记成「本事务创建」而在撤销时误删）；不一致（内容、权限、符号链接、目录）即 `conflict`，**不覆盖、不 chmod**。
- **AC3 flock 锁**（`lock.go`、`task.go`）：
  - 全局锁（`<root>/lock`）与组件锁（`locks/<component>.lock`），`LOCK_EX|LOCK_NB`；`LockComponents` 去重后**字典序**获取（`onAcquire` 钩子证明顺序），任一把失败则释放已得的（all-or-nothing，测试证明失败后其余组件仍可获取）。
  - 被占用 → `*LockedError`（CLI 退出码 6），带持有者 `{pid, run_id, command, started_at, host, scope}`（命令行经脱敏；Linux 另有 `start_ticks`）。持有者信息仅供显示，**判活只靠 flock，不用 pid 文件或 `kill -0`**。
  - **kill -9 无陈旧锁**：真实子进程持有 `server,node` 的锁，父进程被拒且持有者 pid 即子进程；`SIGKILL` 后立即可获取，新持有者覆盖旧信息。全局锁同理（并验证有界等待、`context` 取消）。
  - 锁文件安全：`O_NOFOLLOW`；须为属主可信、单一硬链接、仅属主可写的普通文件；加锁后复核 inode；永不删除锁文件。
  - 并发：24 个 goroutine（各自独立 Registry/文件描述）经全局锁做 `UpdateState` 读-改-写，不丢更新；`-race` 通过。
- **AC4 启动写命令前扫描 journal**（`Registry.Begin`）：**先锁后扫**——同组件上的活事务一定持有组件锁，在第一步就以 6 被拒；扫描时「存在已死亡且未结束的事务」→ `*UnfinishedError`（退出码 13，提示 `lingedge recover`）并释放已取的锁。「死亡」由 journal 文件自身的 flock 判定（活事务持有它，被杀的由内核释放），所以另一个组件上正在进行的合法事务**不会**被误判成 13（测试覆盖）；空 journal（创建后、写 open 前被杀）与中间损坏的 journal 同样算未完成。创建与扫描都在全局锁内，扫描不会撞见「已创建、尚未 flock」的缝隙。CLI：写命令（install/update/rollback/uninstall/backup create|restore/service start|stop|restart）在 root 检查之后、确认之前经 `cmdSpec.Locks` 取锁并扫描；`--dry-run` 与读命令不取锁；`recover` 不被自己拦截。
- **AC5 每步 kill 注入**（`kill_test.go`，真实子进程 + 真实 `SIGKILL`）：子进程执行 3 步事务（每步 `EnsureFile` 创建真实文件），在 `opened`、每步的 `begun/effected/done`、`committed`、`archived` 共 12 个点上通知并阻塞，父进程 `kill -9`。每个点之后：journal 可解析；写命令被拦成 13（`committed/archived` 之后不拦）；磁盘上存在的每个文件都被 `UndoPlan` 的 undo 动作覆盖（**journal 足以恢复**）；随后用真实恢复流程（`BeginRecovery`→按计划逆序撤销→`Undone`→`Abort`）把磁盘恢复到事务之前，再放行、归档。另有追加循环在随机时刻被杀 40 轮，每轮可解析、可续写、恢复后严格有效。网络文件系统锁告警留给 doctor（1.42）：本 Story 提供 `registry.FSKind`（linux statfs 魔数 / darwin f_fstypename），不据此拒绝运行。

## 与「通常做法」不同的设计决定

- **state 目录权限**：tools-design §8 原写「root 0700（state.json 0644 便于普通用户 list/status）」自相矛盾——0700 目录会挡住普通用户读 state.json。落地为：`<root>` **0711**（只给穿越权限、不可列目录）、`state.json` 0644、`journal/ locks/ staging/ trust/` 0700、锁与 journal 文件 0600；`Layout.Ensure` 对已存在目录不改权限，权限过宽/属主不对/符号链接是错误（修复归 `doctor --fix-perms`）。§8 已回写。
- **journal 增加两种 op**：`open`（事务头，登记 command/run_id/components/pid）与 `undone`（恢复进度）。epics 只列了 begin/done/commit/abort；头记录是判定「涉及哪些组件」与恢复时取组件锁所必需，`undone` 使恢复可中断续做。`undo` 保持对象形态 `{"actions":[…]}`（一步可能创建多个东西）。
- **「存活」不用 pid**：未完成事务的存活由 journal 自身的 flock 判定。这比「组件锁是否被占」更准确（不依赖头里的组件表），也让「活事务」与「死事务」在任何时刻都可区分，且与 §16「不使用 pid 文件 + kill -0」一致。
- **全局锁有界等待**：epics 写 `LOCK_EX|LOCK_NB`。全局锁是短临界区，立即失败会让并发的 `UpdateState` 无谓地得 6；因此每次获取仍是 `LOCK_NB`，但被占用时每 10ms 重试至 5s（可配），超时才 `LockedError`。**组件锁永不等待**（冲突即 6）。
- **`LockedSequence` 与组件锁并存**：`trust/state` 被不同组件的并发任务共用，组件锁帮不上忙，所以 `FileSequenceStore` 必须经全局锁访问；这是 1.27「靠 1.29 的全局任务锁」的落点。`LockedSequence` 在 registry 里按结构定义 `SequenceStore`（registry 不依赖 manifest），测试用编译期断言证明它满足 `manifest.SequenceStore`。
- **`Task` 把「取锁+扫描」与「开事务」分开**：`Begin`（取锁、扫描）→ `Task.OpenTx`（建 journal）→ 步骤 → `Commit/Abort`；`Task.TxID()` 同时是 `staging/<tx>` 名与 journal 名。未实现的写命令只做 `Check`（取锁、扫描、立即释放），并且仅在状态目录已存在时才做——没装过任何东西的机器上，未实现的命令不应为「假装检查」而创建 `/var/lib/lingedge/tools`。
- **泄漏检查是「拒绝」而非「改写」**：undo 的路径/哈希若被改写，撤销就会失真；所以疑似密钥一律拒绝落盘，而不是脱敏后写入。自由文本（命令行、原因）则脱敏后写入。
- **未知字段严格**：同一 schema 版本内未知字段 = 损坏，而不是忽略。

## 变异验证（临时去掉关键检查 → 测试必须失败 → 还原）

脚本逐条最小改动（保持可编译：`if false`/`&& false`），运行对应测试，立刻还原；结束后逐文件与变异前**字节一致**（脚本内 `filecmp`，另有 `LockedSequence` 一条手工 `cmp`）。26 条，**26 条全部检出**（括号内为首个失败的测试）：锁忙检测（`TestComponentLockExclusiveAndHolderInfo`）；组件锁不排序、多锁失败不释放（`TestLockOrderIsLexicographicAndAllOrNothing`）；Begin 不扫描 journal、扫描把活事务当未完成、journal 结束后不释放 flock（`TestUnfinishedDetectionGate13`/`TestKill9AtEveryJournalPoint`）；journal 不 fsync（`TestJournalLifecycleAndFsyncEveryRecord`）；状态机不检查在途步骤、undo 不查密钥（`TestJournalProtocolViolationsAreRejectedAndNotWritten`）；解析不容忍尾行（`TestParseToleratesTruncationAtEveryByteOffset`）；解析不检查 seq 连续（`TestParseMidFileCorruptionIsAnError`）；恢复不截撕裂尾行（`TestResumeJournalTruncatesTornTailAndAppends`）；state 不拒绝未来版本、允许未知字段（`TestParseStateSchemaVersioning`）；state 不做泄漏检查（`TestStateRefusesSecrets`）；state 权限 0600（`TestStateRoundTripPermsAndMissing`）；读取不检查 group 可写（`TestLoadStateRejectsUnsafeFiles`）；锁文件不查权限（`TestLockFileSafetyChecks`）；`EnsureFile`/`EnsureSymlink` 不一致时当作完成（`TestEnsureFileIdempotentAndNeverOverwrites`/`TestEnsureDirAllAndSymlink`）；归档覆盖已有文件（`TestArchiveRefusesToOverwrite`）；`LockedSequence.Advance` 不加全局锁（`TestLockedSequenceAcrossProcesses`，该条依赖真实竞争，手工连跑 5 次 5 次都失败）；cli 锁冲突不映射 6、未完成不映射 13、写命令不取锁、dry-run 也取锁（`TestWriteCommandLockedExit6AndKill9Releases`/`TestWriteCommandUnfinishedExit13`）。
- 变异过程中**自己的脚本缺陷**：首版有 4 条因 `sort`/变量被闲置而编译失败（「检出」但无效），已改写为可编译变异后重跑，上表为重跑结果。
- 说明：真实 `kill -9` 在单次 `write(2)` 之后不会留下撕裂行（进程被杀不丢页缓存），40 轮实测 0 轮撕裂；撕裂尾行是断电/内核崩溃的现象，因此由「每字节截断」测试与手工构造的尾部损坏覆盖，**真正的断电撕裂需 ENV-LNX 验证**。

## 对既有测试/防护的改动（逐条）

- **`internal/tools/cli/behavior_test.go` 的 `TestStateFilePresentIsNotSilentlyEmpty` 改了两处**：期望的错误码 `state.reader_unavailable` → `state.corrupt`，并更新注释。原因：该占位错误是 1.25 在读取器缺位时的临时行为，本 Story 交付了读取器，占位随之消失；同一份测试输入（缺 `schema_version` 的 `{"components":{}}`）现在被严格解析为损坏，退出码仍为 1，「state.json 存在但读不懂时绝不当作空」的语义不变。测试里「父路径是文件 → 退出码 3」一段未改且仍通过。
- `internal/tools/cli/state.go`：`FileState` 改为经 registry 读取；`ComponentInfo`/`StateSource` 形状不变（`TestNonEmptyStateRendering` 与 7 个 schema 快照未改，仍通过）。
- `internal/tools/cli/run.go`：`finish` 增 `RegistryError` 分支，`isParseStageError` 把 registry 错误排除在解析阶段之外（与 manifest/downloader 同处理）。
- `internal/tools/cli/spec.go`/`commands.go`：`cmdSpec` 增 `Locks`；9 个写命令声明锁定的组件；未声明 `Locks` 的命令（含 `recover` 与全部读命令）行为不变。退出码表（0/1–14/130）、envelope schema、`--help` 输出**未变**（`exitcode_test` 的文档一致性、help/envelope 快照仍通过）。
- 新增文件：`internal/tools/registry/*`、`internal/tools/cli/registryerr.go`、`registry_test.go`。
- 门禁 G-4/5/6/8/11、release-keys 的规则与白名单**一字未改**，无豁免。`golangci-lint` 对测试里 5 处未检查返回值的报告已就地修正（没有 `nolint`）。`docs/architecture/tools-design.md`（§8/§12.3/§16 与修订记录）、`docs/bmad/decisions-adopted.md` §1.1、`docs/bmad/sprint-status.yaml` 已同步；`docs/bmad/stories/1-25-*`、`1-28-*`、`1-28b-*` 中「state.json 读取器属 1.29」「1.29 任务锁」的遗留项在本文件落地，未改动其正文。
- 没有新增迁移、没有 OpenAPI/proto 变更、`contract-v1` 不受影响；新增 `FuzzParseJournal`、`FuzzParseState`（各 20 秒无失败，`testdata/fuzz` 无新增文件）。

## 测试（真实、无 Mock）

registry 包 44 个顶层测试（含子测试 64 项）：真实文件系统、真实 `flock`、真实子进程（测试二进制自我再执行，`SIGKILL`）、真实 `RLIMIT_FSIZE` 写失败、真实 `FileSequenceStore`；仅有的两个「钩子」（`onAcquire` 观察加锁顺序、`syncHook` 包装真实 `Sync` 计数）都不替代被测行为。cli 8 个新增/改动测试：真实子进程持有锁、真实 journal 死亡现场，经完整命令树得到退出码 6/13。`-race` 下 `go test -race -count=3 ./internal/tools/registry` 通过。

**一次真实的测试缺陷（已修）**：第一次 `make race` 中 `TestKill9DuringAppendLoopLeavesParseableJournal` 在负载下失败（「记录太少 1」）——测试假设被杀时 journal 至少已有 2 条记录，而子进程可能在打印 `STARTED`（open 已 fsync）之后、写下第一个步骤之前就被杀。这是测试自身的错误假设，不是被测代码的缺陷；已改为断言「至少有 open 记录且头与事务 ID 一致」。修后 `-race -count=6` 与两路并行加压各 4 轮通过。

## 门禁结果

见提交说明与交付报告（`gofmt -l .` 无输出；`make lint security gate-selftest race vuln contract-check build verify-build migrate-test` 与 linux/amd64、arm64 交叉编译及 vet；windows 仅 vet 以验证非 unix 桩可编译）。

## 遗留与风险（只列确属后续 Story 或需特定环境的）

- **代码已完成，等待环境验证（ENV-LNX，→ 1.45）**：
  - 真实 `/var/lib/lingedge/tools` root 目录布局（0711/0700 与 root 属主检查在真实 root 下的行为；本机以当前用户验证，root 才有的分支——`uid==0` 视为可信属主——未被真实执行）；
  - Linux 上的 flock 语义（同进程多 open 冲突、`kill -9` 释放）——代码走同一 `syscall.Flock`，macOS 已测，Linux 未实跑；
  - `/proc/<pid>/stat` 的 `start_ticks`（`proc_linux.go`，解析函数 `parseProcStat` 已单测，读取在 Linux 才执行）与 `fsKind` 的 statfs 魔数表（`fsinfo_linux.go`，仅交叉编译验证；macOS 分支已测）；
  - 断电（而非 kill）造成的撕裂尾行与 fsync 的真实持久性；
  - 网络文件系统上的 flock 行为（只识别并交给 doctor）。
- **属后续 Story**：1.33（22 步编排：按本 Story 的 `Begin → OpenTx → Begin/Done 每步 → Commit/Archive`，`downloader.Params{StagingRoot: task.StagingRoot(), TxID: task.TxID(), DeferCommit: true}` + `LockedSequence`，每步 undo 动作的具体取值与执行器、`recover --resume/--rollback` 命令本体——本 Story 只提供 `BeginRecovery`/`UndoPlan`/`ResumeFrom`）；1.42（doctor：对 `FSKind` 网络文件系统告警、`--fix-perms` 修复目录权限、`status` 的 systemd/健康探测）；1.33/1.41/1.43 各自把写命令的 `Run` 接到 `inv.task`；stale `.ensure-*`/`.state.json.tmp-*` 临时文件的定期回收（`CleanStaleTemps` 已有，由 doctor/recover 调用）。
- **已知限制**：`UpdateState` 的 `UpdatedAt` 取注入时钟；`Task.Release` 对未 commit 的 journal 只 Close（保持未完成），因此命令路径上任何错误返回而忘记 `Abort` 都会让下一个写命令得到 13——这是设计（宁可要求 recover，也不静默丢弃事务），1.33 的编排须在可回滚的失败路径上显式 `Abort`。
