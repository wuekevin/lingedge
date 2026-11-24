---
title: 'Story 1.41：backup / restore / uninstall（含主密钥指纹与加密）'
type: 'feature'
created: '2026-11-24'
status: 'review'
route: 'dispatch'
baseline_commit: '1763624'
waiting_env_verification: true
context:
  - docs/bmad/epics.md Story 1.41（AC1–AC7）
  - docs/architecture/tools-design.md §3、§13.1、§16、§17.1、§17.5、§18.2；ADR-011 §3/§8；ADR-013 §4；ADR-026 §4
  - docs/bmad/stories/1-11-*.md、1-33-*.md、1-35-*.md、1-40-*.md
---

## Intent

**Problem:** `lingedge backup|uninstall` 全部是「尚未实现」的占位（退出码 1）；没有备份格式、没有加密、没有主密钥指纹，恢复没有任何校验与回滚，`uninstall` 不存在。1.11 留下的 `kek_ids_required` 数据源（`CheckKEKRemovable` 的 `BackupRequired`）也在等本 Story。

**Approach:** 一个 `internal/tools/backup` 包（备份格式 + 加密 + 指纹 + 恢复事务）、`installer.Uninstall`（复用 1.33 的 journal/锁/nginx 软链撤销）、`cli` 的真实命令。不造新机制：外部命令只经 safeexec（新增两个独立白名单 Runner，不放宽任何既有白名单）；journal 步骤、`recover --rollback`、退出码表、envelope/schema 全部沿用。验证用**本机临时 MySQL 8.0 + 本机构建的真实 `lingedge-server`**（`tests/mysqltest`，绝不碰常驻 mysqld）。**无迁移、无 OpenAPI/proto 改动（无 CC-14）、不改 CF-6/contract-v1/迁移 000001–000018。**

## 逐条验收

- **AC1**：`backup create`：配置（**不含** `master.key`/`migrate.yaml`/`setup-token.hash`）、`mysqldump --single-transaction --routines --triggers`（参数数组，凭据经 0600 的临时 `--defaults-extra-file`，用后清零并删除，命令行与环境变量里没有口令）、服务数据、Tools 状态；`BACKUP.json` 含组件/版本/`schema_version`/迁移级别历史/文件清单 + SHA256/`master_key_fingerprint`/`kek_fingerprints`/`kek_ids_required`/`deploy_lineage`/时间/Tools 版本；`--encrypt` = argon2id + AES-256-GCM 分块（截断/重排/伪造 final/篡改头部都认证失败）；未加密文件 0600、位于 0700 目录（`--output` 指到开放目录时拒绝）并警告。写完立即回读校验。
- **AC2**：成功后醒目输出「数据库与私钥为密文，恢复必须有对应 master.key，请与数据库备份分开保管」（人类输出 + `data.notice`）；`--include-master-key`（及 `--include-migrate-credential`/`--include-identity`）必须同时 `--encrypt`，否则退出码 2，且清单校验也拒绝「未加密备份含这些条目」。
- **AC3**：`backup list`（无需口令，展示每个备份所需的 `kek_id`）与 `backup verify`（结构/哈希/解密认证）只读，不改动系统。
- **AC4**：`backup restore`：主密钥指纹不匹配或缺失 → 退出码 3 并点名缺失/不同的 `kek_id`（此时尚未改动任何东西，连安全备份都不创建）；备份的 `schema_version` 大于当前二进制（`lingedge-server migrate plan --offline`）→ 退出码 3；目标库默认须为空，`--replace` 需 `--yes --confirm-name <主机名>`（或交互键入）；**先做「恢复前安全备份」**（同口令加密，写完回读校验）；恢复后对 `kek_ids_required` 的**每个** `kek_id` 解密一条探针记录；任一步失败回滚到安全备份（文件 + 数据库 + `master.key` + 服务状态），退出码 10，回滚失败 12 并保留 journal，`recover --rollback` 可重试。
- **AC5**：`uninstall <c>` 默认保留配置/数据/备份（只摘除单元、单元文件、conf.d 软链、`/opt/lingedge/<c>`、state 记录）；`--purge` 需交互输入主机名或 `--yes --confirm-name <主机名>`，否则退出码 9；静态前端没有数据目录；systemd 与 nginx 软链摘除；未安装 → 14。
- **AC6**：备份 → 清空测试库 → 恢复 → 逐表内容摘要一致（真实 MySQL，`TestBackupWipeRestoreRealMySQL`；排除运行中 Server 持续写入的 `job_locks` 与恢复后 lineage bump 必然追加的 `audit_logs`/`deploy.lineage` 行）。
- **AC7**：恢复在**启动 Server 之前**执行 `lingedge-server lineage bump`（真实二进制；输出的 `after` 必须严格大于 `before` 与备份记录的 `deploy_lineage`，且 ≥ 当前 UNIX 秒）；`BACKUP.json` 记录 `deploy_lineage`/`master_key_fingerprint`/`kek_ids_required`，`backup list` 展示所需 `kek_id`；`GuardKEKRemoval` 在 KEK 被保留备份需要时拒绝（无法解析的备份保守拒绝），除非 `--drop-backup-compat`，并与 `secretbox.CheckKEKRemovable` 联动；配置修订块在 MySQL 里随 `mysqldump` 备份，不另备份文件（IMP-16）。**「节点（含已应用更高 seq 者）仍能接收新修订」的端到端验收需要真实节点（Epic 3/Story 1.45，AC-J-5）——本 Story 验证的是它的充分条件：lineage 严格单调大于任何节点已应用值。**

## 发现的缺口与矛盾（须披露；文档已修正）

tools-design §17.1/§17.5 与 Story 原文**没有说明**（已补 §17.6、epics 实现澄清、operations 手册）：

1. **数据库凭据从哪来**：只说「临时 `--defaults-extra-file`」。`lingedge_app`/`lingedge_maint` 刻意没有 dump/DDL/`SET_USER_ID` 权限，所以用迁移账号，来源与优先级同 §13.1；连接目标绝不取自 `runtime-credentials.json`（ADR-026 §4）。Tools 不能 import MySQL 驱动（架构门禁：只有 `internal/platform` 的 db/migrate/repository 包可以），所以查询经 `mysql --batch`。
2. **`master_key_fingerprint` 没有定义**，且「指纹不匹配就拒绝」与 KEK 轮换冲突（轮换后文件多了新 KEK，整体指纹必变，但旧备份仍可解）。定义：整体指纹 = `master.key` 文件内容 SHA-256（与 `lingedge install server` 输出的指纹同源，可 `sha256sum` 目视核对）；再记每个 `kek_id` 的单键指纹，恢复时按「备份所需的每个 `kek_id`」逐个比对。
3. **灾后重装怎么提供主密钥**（新机器的 `master.key` 是新生成的，与备份必然不匹配）：补 `--master-key-file`/`--use-backup-master-key`，旧文件保留为 `master.key.pre-restore-*`，Tools 绝不删除任何主密钥。
4. **`state.json` 在备份里的含义**：原文「Tools 状态：state.json、已装版本清单」包含，但恢复若覆盖它会让状态与磁盘上的 release 脱节 → 备份保留只读副本，恢复不覆盖。
5. **`runtime-credentials.json` 恢复后与 MySQL 账号口令的一致性**（原文未涉及）：本机已有且主密钥能解开 → 保留本机文件，避免用旧口令导致 `lineage bump` 连不上库；否则用备份里的。
6. **加密备份的 `backup list` 需要口令吗**：AC7 要求 list 展示每个备份所需 `kek_id`，而清单在加密流里 → 头部放明文且被认证（AAD）的 `meta`（不含机密）。
7. **崩溃后 `recover` 对 restore/uninstall 事务**：原 `recover --resume` 只认安装事务 → 对这两类事务拒绝并给指引；`--rollback` 对 restore 经 journal 里的 `restore_safety_backup` 动作回到安全备份，对 uninstall 只结束事务记录（卸载不可撤销，重跑即可）。
8. **G-4（归档只能在 `internal/tools/security` 内解析）与备份归档**：备份是 Tools 自己读写的 tar.gz，不是发行包解压；为不放宽门禁，tar.gz 读写放进 `security/tarstream.go`（只收普通文件、拒绝稀疏、条目名经 `CleanEntryName`）。
9. **G-6 门禁与命名**：常量名含 `password` 的环境变量名会被固定口令扫描命中；改用 `EnvDBPass`/`EnvBackupPassFile` 命名，**门禁未豁免**。
10. 信封脱敏按键名替换值：输出里的 `master_key_fingerprint`/`master_key_included` 会被打成 `***`；CLI 输出键改名为 `key_fingerprint`/`key_included`/`saved_key_file`（`BACKUP.json` 内保持规格里的字段名）。

## 交付物

- `internal/tools/backup/`：`crypt.go`（加密容器）、`archive.go`、`manifest.go`（清单 + `validDest` 恢复目标白名单）、`keyfile.go`（指纹 + `CheckKeyMatch`）、`cred.go`（凭据/目标解析）、`mysql.go`（选项文件生命周期、dump/query/load/wipe）、`dbmeta.go`（schema/lineage/`kek_ids_required`/探针/KEK 守卫）、`service.go`（create/list/verify）、`restore.go` + `restore_support.go`（恢复事务、回滚、`UndoMasterKey`）。
- `internal/tools/installer/uninstall.go` + `recover.go`/`undo.go`/`deps.go`/`errors.go` 的追加（`KindNotInstalled`、`RestoreSafety`、restore/uninstall 的 recover 语义）。
- `internal/tools/cli/cmd_backup.go`、`cmd_uninstall.go`、`commands.go`（标志与确认）、`env.go`（`Env.Backup`）、schema：`data.backup-create|list|verify|restore`、`data.uninstall`。
- `internal/tools/security/tarstream.go`；`tests/mysqltest`：`Instance.BinDir()`。
- 文档：tools-design §17.6 与修订记录、`docs/operations/backup-restore.md`（新）、`secrets.md` §3.2、epics 1.41 实现澄清。

## 测试（全部真跑；伪的只有「操作系统」）

- `internal/tools/backup`：加密往返（0/1/整块±1/多块）、随机化、错误/弱口令、**每个字节翻转与每个截断点**、分块重排/重复、**截断 + 伪造 final 标志**、KDF 参数上限、meta 被认证；归档往返（明文/加密）、压缩流翻转、结构攻击（清单外条目含合规名称、缺失/重复/换内容/大小不符/符号链接/清单不在首位）；清单校验 19 例（越界目标、`..`、控制字符、未加密备份含主密钥…）；`WriteArchive` 拒绝非法清单与被改动的暂存文件、不覆盖；主密钥指纹（与 install 同源、注释/行序无关、轮换、缺失/不同/属主）；凭据解析优先级与信任（0400、符号链接、未知字段、不读 runtime-credentials、错误不含口令）；选项文件生命周期（第一个参数、0600、转义、命令行/环境变量无口令、用后清理含失败路径、控制字符口令拒绝）；mysqldump 参数与 Wipe 语句；KEK 守卫（含加密备份、无法解析的备份、与 `CheckKEKRemovable` 联动）。
- `internal/tools/installer/uninstall_test.go`：默认保留、purge 只删本组件、purge 的 master.key 警告、未安装/dry-run、静态前端（软链摘除 + nginx -t/reload、共用目录不删）、非 Tools 软链不动、中途失败 partial 并重跑完成、崩溃后 recover（rollback 说明/resume 拒绝）。
- `internal/tools/cli`：node 路径的 create/list/verify/restore（加密、身份私钥、口令来源：文件/环境变量/交互、dry-run、确认、组件不一致、口令错、未安装 14、越界清单目标被拒）、uninstall 命令（9/14/dry-run/默认/purge）、标志校验、schema 校验。
- `internal/tools/security/tarstream_test.go`：读写往返、7 类危险条目、尾部垃圾。
- `tests/integration/toolsinstall/backup_test.go`（**真实 `mysqldump`/`mysql` + 临时 MySQL 8.0 + 真实 `lingedge-server`（lineage bump、migrate plan、/healthz）+ 真实向导产生的数据**）：备份→清空→恢复一致（含探针、lineage、安全备份、journal 归档、凭据残留断言、泄漏扫描）；未加密规则/人类输出的 F-7 提示/dry-run；非空目标与 `--replace`（含备份之后新建的表被清掉）；**主密钥门禁**（不匹配/缺失/候选也不匹配/`--use-backup-master-key` 无来源/`--master-key-file` 安装/旧密钥保留 0400/备份自带主密钥）；schema 过新；**7 个故障注入点 × 回滚**；探针解密失败回滚；崩溃后 `recover --rollback`（缺目标时明确失败并保留 journal，补上后回滚成功）；uninstall 真实 server；`TestEncTargetsMatchMigrations`。

## 变异验证

对关键防线逐条临时改动 → 对应测试必须失败 → 还原：加密 flag 不进 AAD、meta 不进 AAD、选项文件 0644、选项文件不删、口令进命令行、指纹比对恒通过、单键指纹不比较、恢复目标不校验、未加密备份允许含主密钥、归档不校验哈希、归档允许清单外条目、KEK 守卫忽略加密备份/不保守处理无法解析的备份、卸载默认删配置、purge 误删其他组件备份、不分辨非 Tools 软链、purge 删共用 nginx 目录、恢复不检查 schema、不做 lineage bump、探针形同虚设、回滚不重灌数据库/不恢复主密钥、指纹门禁关闭、目标库非空也恢复、`--replace` 不清空、备份不加密——**全部检出**。其中 5 条**初次存活**，据此补强后检出：flag 不进 AAD（补「截断 + 伪造 final」用例）、口令类参数进命令行（补参数内容断言）、清单外条目（原用例的非法名称被更早的路径检查拦下，补名称合规的用例）、探针（我的变异写法无效，重做）、`--replace` 不清空（补「备份之后新建的表必须消失」）。

## 对既有测试/防护的改动（逐条，均为随功能交付的必要更新，无放宽）

| 文件 | 改动 |
|---|---|
| `internal/tools/cli/behavior_test.go` | backup/uninstall 自 1.41 起已实现：从「未实现」清单移到「已交付」清单；交互确认用例的占位命令由 `backup restore` 改为仍未实现的 `rollback`；`uninstall node --purge --yes --confirm-name` 的期望由 1（未实现）改为 14（确认通过后落到命令本身，node 未安装）。**其余期望（9/2/3）不变** |
| `internal/tools/cli/install_cmd_test.go` | 退出码映射穷尽表追加 `KindNotInstalled → 14` |
| `internal/tools/cli/testdata/golden/{help-root,err-confirm}.json` | 重新生成：help 列出 backup/uninstall；err-confirm 的 `duration_ms` 因命令已实现、写命令入口改走真实 `Begin`（多读几次时钟）而变（输出的 exit_code/errors 不变） |
| `internal/tools/installer/{deps,undo,recover,engine,errors}.go` | 追加：`Deps.RestoreSafety`、restore/uninstall 的撤销动作、`KindNotInstalled`、recover 对这两类事务的语义；既有安装路径行为不变（既有测试全绿） |
| `scripts/gates`、`.golangci.yml`、`safeexec.DefaultAllowed`、`SafeExtract` 限额、tools 白名单 | **未改动** |

## 属后续 Story / 代码已完成待环境验证

**代码已完成，待环境验证（ENV-LNX / ENV-BT，汇总于 1.45）**：真实 systemd 下 `systemctl stop/start` 与 `Pdeathsig` 行为；root 属主下真实 `chown`/0400 文件属主检查（本机测试进程非 root，chown 为记录型伪实现，其余文件操作与权限位都是真实的）；真实宝塔 MySQL 的 `mysqldump`/`mysql` 路径与权限（`--mysql-bin-dir`）；node 组件的真实数据布局与 Agent 首次心跳（本机以 toolsworld 的假 node 规格验证文件与单元编排）；全新机器上的灾后重装恢复演练；「恢复后节点仍能接收新修订」的真实节点验收（AC-J-5，需 Epic 3 的节点）。

**属后续 Story**：移除 KEK 的执行命令与 `secrets.rewrap` 任务接线（6.18；本 Story 交付数据源与 `GuardKEKRemoval`）；`doctor` 的 `backup.recent`/`backup.master_key_separate`/`sys.time_sync`（1.42）；升级前强制备份与不可逆迁移的恢复路径编排、`rollback --from-backup`、`--include-migrate-credential` 在升级中的使用（1.43，会调用本 Story 的 `backup.Service.Create/Restore`）；TUI 备份/卸载页（1.44）；备份保留策略/自动清理与定时备份（不在本 Story 范围，运维手册已说明手工清理）。

## 门禁结果

见 sprint-status 的 `done_locally` 与最终报告（以最后一次完整运行为准）。
