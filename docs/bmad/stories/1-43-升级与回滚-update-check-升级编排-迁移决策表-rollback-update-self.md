---
title: 'Story 1.43：升级与回滚（update check / 升级编排 / 迁移决策表 / rollback / update self）'
type: 'feature'
created: '2026-11-24'
status: 'review'
route: 'dispatch'
baseline_commit: '87e5386'
waiting_env_verification: true
context:
  - docs/bmad/epics.md Story 1.43（AC1–AC9）
  - docs/architecture/tools-design.md §3、§9.5、§13.1、§16、§17.2–§17.5、§18.3
  - ADR-013；ADR-016；ADR-026 §4
  - docs/bmad/stories/1-33-*.md、1-35-*.md、1-40-*.md、1-41-*.md、1-42-*.md
---

## Intent

**Problem:** `lingedge update|rollback|update self|version --check` 全是「尚未实现」的占位（退出码 1）；`install` 遇到已装组件的升级只会提示「请走 update」；`internal/tools/updater` 与 `internal/tools/rollback` 是空的占位包；升级前备份、迁移决策表、失败自动回滚、含不可逆迁移的确认与「恢复备份 + 旧二进制」路径都不存在；1.40/1.41/1.42 把前端静态组件的升级与回滚、升级前强制备份、`rollback --from-backup`、`version --check`/`update check` 留给了本 Story。

**Approach:** 不造新机制：升级事务复用 1.33 的 journal/锁/取包（`downloader.Acquire` + `DeferCommit`）/撤销/`recover`，复用 1.41 的 `backup.Service`（升级前备份、恢复备份）与 1.35 的 server 规格；新增 `dbmigrate`（`lingedge-server migrate` 调用封装，口令只经环境变量）、`rollback`（决策表 + 回滚路径执行器）、`updater`（`update check` 比对 + `update self` 原子替换）与 `installer.Update/Rollback/UpdateSelf`、`cli` 真实命令。验证用**本机真实 `lingedge-server` 进程 + 临时 MySQL 8.0/Redis + 用 `go build -overlay` 往内嵌迁移集合里追加的真实 SQL 迁移（reversible/expand/irreversible/中途失败）**——不改任何冻结迁移与源码。**无迁移（000001–000018 不动）、无 OpenAPI/proto 改动（无 CC-14）、不改 CF-6/contract-v1。**

## 逐条验收

- **AC1**：`update check` 经签名清单列出可升级组件与迁移预告（`classes[]`、`has_irreversible`、`irreversible_count`、`backup_required`、`schema_from/to`），`blocked` 给出逐级升级路径，吊销的已装版本标注；`version --check` 同源；篡改的清单/包被拒（5；`TestUpdateRejectsTamperedManifestAndPackage`：篡改清单/签名/包、sequence 回放；`TestUpdateCheckRealHTTPSManifest`：真实 HTTPS 发行源 + 篡改签名 → 5）。
- **AC2**：`update <c>|--all` 按 §17.3 的 13 步执行（journal 步骤号即步骤号，`--dry-run/--plan-only` 与真实升级同源 `PlanUpdate`，对整棵根目录零变化；`TestPlanUpdateIsReadOnlyAndSameSource`）；兼容性由 resolver（`min_upgrade_from/requires/conflicts`）；取包经 `downloader.Acquire`；备份（server 含 DB，写完再 `Verify`）；`migrate plan`；停服务；`migrate up`；原子切 `current`（临时符号链接 + `rename`，并发读者永远读不到缺失）；启动；健康检查（server：`/healthz` + `/healthz/ready` + 数据库迁移版本 = 期望）；确认写 state。
- **AC3**：决策表四类 × 成功/失败（真实 MySQL + 真实进程：`TestUpdateServerDecisionTableSuccess`、`TestUpdateHealthFailureAutoRollbackPerClass`）；`reversible` 的 `down` 失败（迁移中途失败，库 dirty）升级为「恢复备份」（`TestUpdateMigrationFailureEscalatesToRestoreBackup`）；手动回滚里 down 失败**不会**静默改走恢复备份（`TestRevertDBNeverSilentlyEscalatesToRestoreUnlessAllowed`）。
- **AC4**：含 `irreversible`：升级前强制备份并验证可读；交互键入 `confirm-irreversible`，非交互 `--accept-irreversible`，否则退出码 9，**尚未改动任何东西**（整棵树快照比对、没有备份/staging/journal）；清单预告低估时以 `migrate plan` 为准（9，此时只留备份）；回滚路径＝恢复升级前备份 + 旧版本二进制，输出明示「备份点之后产生的数据将丢失」（`TestUpdateIrreversibleConfirmationAndRecoveryPath`）。
- **AC5**：构造的健康检查失败版本（真实进程因读不到配置而启动失败）→ 自动回滚、服务恢复、退出码 10；回滚失败 → 12 且保留 journal（`TestUpdateRollbackFailureExit12ThenRecover`、installer 层 `TestUpdateRollbackFailureKeepsJournalAndRecoverFinishes`）；回滚后复核版本号、配置、schema 版本与升级前一致，并自动跑健康检查 + doctor 子集（`rollback.verifyRolledBack`；`TestRollbackVerificationCatchesLyingUndo`：撤销谎报成功时复核仍能发现）。
- **AC6**：`rollback <c> [--to] [--from-backup]`：含不可逆迁移 → 拒绝简单回退并提示 `--from-backup <备份>`（3）；`--from-backup` 覆盖数据库，须主机名确认（9），恢复后 `lineage bump`（IMP-07）；`update self`：先写 `lingedge.new`、`--version` 自检、再 `rename`，保留 `.prev`（`TestUpdateSelf`、`updater.TestSwapBinary`）。
- **AC7**：`--offline` 与在线同一条验证链（`TestUpdateOnlineSameChainAsOffline`：在线经本地 HTTPS + CA，结果与离线一致）；成功确认后才清理超出保留数（缺省 2，`update.keep_releases` 不少于 2；`TestReleaseRetentionOnlyAfterSuccess`、live `TestUpdateNoMigrationHealthFailureAndRetention`）。
- **AC8**：含 `expand` 的回滚承诺限定为「升级失败的即时回滚」，输出与文档如实声明（§17.2 承诺文字原样展示；手动回滚警告 N-1 前提）；含不可逆迁移的恢复路径验证「恢复后 deploy.lineage 严格增大」（节点仍可接收新修订；节点侧环境验证属 1.45）；凡含「恢复数据库备份」的回滚路径均在启动旧版本 Server 之前由已切回的旧二进制执行 `lineage bump`（`backup.ApplyBackup`；mutation M31 验证）。
- **AC9**：迁移凭据来源四级优先级（环境变量 ＞ 口令文件 ＞ `migrate.yaml` ＞ 交互）、Tools 只经环境变量传给 `migrate` 直接子进程（`dbmigrate.TestPasswordOnlyViaEnvironmentNeverOnCommandLine`）、口令不入 journal/`state.json`/日志/输出（`TestUpdateMigrationCredentialSources` 对整棵根目录、journal 归档与 Tools 日志全树扫描）、非交互无凭据 → 3 且尚未改动任何东西、连接目标不读 `runtime-credentials.json`（`backup.ResolveDB`）、备份默认排除 `migrate.yaml`（1.41）。

## 已知项核对结果（挂到本 Story 的项）

| 项 | 结论 |
|---|---|
| `version --check` 与 `update check`（1.42 留下） | **做全**（AC1） |
| 升级前强制备份与 `rollback --from-backup` 编排（1.41 留下） | **做全**，复用 `backup.Service.Create/Verify` 与新增 `ApplyBackup`（恢复文件 + 清空重灌库 + 探针解密 + `lineage bump`，无启停/journal，供回滚复用） |
| 前端静态组件升级与回滚（1.40 留下） | **做全**：同一框架（无 DB、无进程、无备份支持 → 旧 release 保留作快照），原子切 `current`；`TestStaticFrontendUpdateAndRollback` |
| server ComponentSpec 升级 | **做全**：迁移 migrate 决策、lineage（仅恢复备份路径）、health（`/healthz/ready` + schema）；**`secrets.rewrap` 与移除 KEK 的命令不属本 Story**（原文无此要求；1.41/tools-design 已明确属 6.18），升级编排不触碰 `master.key` |

## 发现的缺口与矛盾（须披露；文档已修正）

1. **expand 回滚与 Server 现行就绪探针矛盾**（tools-design §17.2 称旧二进制与新增结构兼容）：`MigrationProbeFromApp` 在「库里有二进制不认识的迁移」时 `/healthz/ready` 报 503，`migrate status` 也以退出码 10 拒绝。→ 回滚后该路径只要求存活探针 + schema 等于升级后版本（`healthy_live_only`），测试在缺口修复时提示恢复为 ready；N-1 就绪兼容属后续（Server 探针/发布流程）。已写入 tools-design §17.7、epics 实现澄清与 operations 手册。
2. **「尚未改动任何东西」的歧义**（§17.3 步骤 7 把退出码 9 放在备份之后）：精确化——9/3 发生时服务、数据库、`current`、配置、state、journal 均未改动；清单预告低估时备份已完成并保留（见 tools-design §17.7）。
3. **`installer.KindNotInstalled` 的错误码 `not_installed` 违反已发布的信封 schema**（要求点分小写码；1.41 起就是如此，因测试没有对该失败做 schema 校验而未发现）→ 重命名为 `not_installed.component`（退出码 14 不变）。
4. **tools-design §2 的依赖方向**（`updater→installer`）与升级复用 journal/取包/撤销的事实相反 → 编排落在 `installer`，文档已修正。
5. **真实服务的单元 `start` 幂等性**（测试世界的伪 systemd 一度重复拉起第二个进程抢端口）：伪 systemd 的 `start` 现与真实 systemd 一致（已在运行则不动）。

## [ASSUMPTION] 清单（缺省值类假设）

① 升级不重新渲染/覆盖 systemd 单元与配置（漂移由 doctor 后续检测）；② `update --all` 的逐个升级顺序为组件名字典序（现有可选组件互不依赖，依赖只发生在内部组件）；③ 升级前备份缺省不加密（`--passphrase-file`/`LINGEDGE_BACKUP_PASSPHRASE_FILE` 给出则加密）；④ `update.keep_releases` 缺省 2（AC7 原文）；⑤ node 升级后 Agent 首次 Heartbeat / `openresty -t` 的健康检查只做单元 active + 组件 Health（Epic 3 补全）；⑥ `rollback` 目标不是上一次升级的来源版本且 server 有库时要求 `--from-backup`（无法确认 schema 兼容）。

## 变更清单

- `internal/tools/{rollback,updater,dbmigrate}`（新）；`internal/tools/installer/{update_types,update_plan,update_run,update_fail,update_undo,rollback_run,self}.go`（新）、`{deps,undo,recover,engine,materialize,event,toolsconfig,errors,preflight}.go`（追加/小改：`Deps.Update/SelfPath/SelfRun`、撤销动作、recover 的 update/rollback 事务、`extractRelease` 抽出、事件 `op/total`、`update` 配置节）；`internal/tools/backup/{apply,manifest,mysql}.go`（`ApplyBackup`、用途 `pre-upgrade/pre-rollback`、`MySQL.SchemaVersion`）；`internal/tools/registry/state.go`（state 版本 3）；`internal/tools/cli/{cmd_update,cmd_read,cmd_install,cmd_backup,commands,installerr,run}.go` + 5 个 JSON schema。
- 测试：`internal/tools/{rollback,updater,dbmigrate}` 单元；`installer/{update,rollback_update,update_undo}_test.go`（假组件世界 + 真实签名 bundle：成功/故障注入 26 点/健康失败/回滚失败/recover/保留策略/静态前端/篡改/在线 HTTPS/self/复核）；`cli/update_cmd_test.go`；`tests/toolsworld/bundle.go`（`BuildBundle`）；`tests/integration/toolsinstall/update_*_test.go`（**真实 server + 临时 MySQL**：决策表四类成功、健康失败回滚四类、不可逆确认与恢复路径、迁移中途失败升级为恢复备份、凭据四级优先级与泄漏扫描、journal 故障注入 15 点、崩溃（模拟 kill -9）9 点 + 提交点之后 1 点的 recover、回滚失败 12、`update check` 真实 HTTPS）。

## 变异验证

见最终报告（对关键防线逐条临时改动 → 对应测试必须失败 → 还原；存活项已补强）。清单：未知迁移级别不按不可逆、不可逆不需确认、expand 路径写成 binary、MergeClasses 不取并集、不可逆不拒绝简单回退、切换 current 非原子、清理不保护 current、保留数不钳到 2、AllowRestore 门禁失效、expand 路径总是恢复备份、保留 .prev 失效、自检被跳过、min_upgrade_from 不拦、口令进命令行、up 恒带 --accept-irreversible、不可逆确认被绕过、回滚复核不查 current、recover 恒认为已提交、撤销不重启服务、手动回滚不拒绝不可逆、不做升级前备份、state 接受未知回滚路径、撤销动作路径不校验、提交点之前允许 --resume、self 允许降级、恢复备份不 lineage bump、恢复备份不清空数据库。

## 对既有测试/防护的改动（逐条，均为随功能交付的必要更新，无放宽）

| 文件 | 改动 |
|---|---|
| `internal/tools/registry/{state_test,fuzz_test}.go` | state 版本 2→3：既有「未来版本」用例的版本号由 3 改为 4（语义不变：失败关闭、不改写）；v1→v2 迁移用例的期望版本由 2 改为 3；新增 v3 用例 |
| `internal/tools/cli/behavior_test.go` | update/rollback/`version --check` 自 1.43 起已实现：从「未实现」清单移到「已交付」清单；交互确认用例的预期由「落到未实现 1」改为「落到命令本身：node 未安装 14 / 交互 n → 9 且确实提了问」（**反而更严格**：证明确认真的发生了）；未实现命令清单改用仍未实现的 `update node --apply-staged` 与 `node enroll` |
| `internal/tools/cli/{exitcode,envelope,validate,verifyerr,registry}_test.go` | 同上：「未实现的写命令」占位改用 `update node --apply-staged`；`update` 的合法组合改为落到命令本身（14/0）；锁/journal 用例的「走到未实现 = 1」改为「走到命令本身 = 14」（锁与 13/6 的断言原样保留）；`TestWriteCommandStubDoesNotCreateStateDir` 用合成的桩命令（`commandSpecs` 测试挂钩）继续覆盖 gate 对「尚未实现的写命令」的行为；未来 schema 版本号 3→4；错误码 `not_installed` → `not_installed.component`；`TestEverySchemaHasOwnerAndEveryVisibleCommandHasSchema` 增强为「可运行的父命令（update）同样必须有 schema」 |
| `internal/tools/cli/testdata/golden/{help-root,err-not-implemented}.json` | 重新生成：help 列出 update/rollback；err-not-implemented 改用 `update node --apply-staged` |
| `internal/tools/cli/schema/data.{version,backup-list,backup-verify}.schema.json` | 追加 `check` 属性 / `purpose` 枚举两项（向后兼容的放宽**仅限新增字段/新增枚举值**，既有字段与必填项不变） |
| `tests/integration/toolsinstall/{live,backup}_test.go` | 追加：`liveServer` 的 `external/failVersions/launch`（`start` 幂等）、`runOpt` 的 runner/journal/hook/tty 接缝；既有用例行为不变 |
| `scripts/gates`、`.golangci.yml`、`safeexec.DefaultAllowed`、`SafeExtract` 限额、冻结契约 | **未改动** |

## 属后续 Story / 代码已完成待环境验证

**代码已完成，待环境验证（ENV-LNX / ENV-BT，汇总于 1.45；闭环 J 不得整体标通过）**：真实 systemd 下 `systemctl stop/start` 的优雅停机与 `Pdeathsig`；root 属主下真实 `chown`/0400 文件属主检查；真实宝塔 MySQL 的 `mysqldump/mysql/migrate` 路径与权限；真实发行源（UD-3）与真实发布密钥下的清单/包验证；node 组件的真实升级（Agent 首次 Heartbeat、`openresty -t`、配置代未受影响）；全新 Linux 机器上的升级/回滚演练与「恢复后节点仍可接收新修订」（需节点环境，AC8/1.45-AC9）。

**属后续 Story**：`secrets.rewrap` 与移除 KEK 的执行命令（6.18）；节点升级路径 `lingedge-node-upgrade.path/.service` 与 `update node --apply-staged`（Epic 3，ADR-024）；Server 就绪探针对 expand 迁移的 N-1 兼容与发布流程的 N-1 测试（见缺口 1）；TUI 里的升级确认页/不可逆确认页（1.44）。

## 门禁结果

见 sprint-status 的 `done_locally` 与最终报告（以最后一次完整运行为准）。
