---
title: 'Story 1.32：service 包：systemd Unit 渲染 / 运行用户 / 目录 / systemctl 封装'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'c7f949d'
waiting_env_verification: true
context:
  - docs/bmad/epics.md Story 1.32（AC1–AC5，含 2026-11-16 对 AC5 的修订口径）
  - docs/architecture/tools-design.md §15（单元模板）、§8（目录布局）、§21（待决：LoadCredential 最低版本）；docs/architecture/adr/ADR-024、ADR-020 §2（节点目录权威总表）、ADR-011 §2/§9
  - docs/bmad/review-architecture*.md（BLK-04、BLK-07、IMP-20、L24）、docs/prd/LINGEDGE_V2_REQUIREMENTS.md（FR-6、FR-12、FR-58、NFR-9、NFR-26）
  - docs/bmad/stories/1-26-*、1-29-*、1-31-*（security 助手与 safeexec、registry 的 journal/undo、probe 的 Runner 做法）
---

## Intent

**Problem:** 1.33 的安装编排需要「落地服务」的积木：把 systemd 单元渲染出来且不接受自由文本、创建运行用户与目录并且权限一次到位、安全地调用 `systemctl`、遇到管理员已改过的单元不覆盖，以及主密钥交付在老 systemd（无 `LoadCredential`）上的降级。每一件还都要能写进 journal 的 undo，让崩溃后的 recover 知道「哪些是本事务创建的」。

**Approach:** 实现 `internal/tools/service`。模板经 `go:embed` 内置，变量先过 `security.ValidateUnitValue`/`ValidateUnitPath` 再进模板；运行用户/目录/文件/单元安装/systemctl 都经 safeexec 与 `security` 的权限助手，每个会改系统的操作产出 `registry.UndoAction`（`Created` 记录「是否本事务创建」），`Undoer` 能执行它们；`LoadCredential` 门槛（≥247）与降级变体、版本解析与 doctor 检查函数一并实现（ADR-027）。本 Story **不编排**安装步骤（1.33）。

## 逐条验收

- **AC1 模板与加固**：六个模板（`lingedge-server.service`、`lingedge-node.service`、`lingedge-openresty.service`、`lingedge-openresty.path`、`lingedge-node-upgrade.path/.service`）+ Server 的降级变体。golden（`internal/tools/service/testdata/golden/`）**由人工对照 tools-design §15/ADR-024 逐行写成**，没有 `-update` 开关，逐字节比对；另有**独立的属性测试**解析渲染结果的 INI 段逐条断言：`NoNewPrivileges`、`ProtectSystem=strict`、`LimitCORE=0`（Server）、`CapabilityBoundingSet`（Server/Agent 空，OpenResty 仅 `CAP_NET_BIND_SERVICE`）、Agent 与 OpenResty **均不设** `MemoryDenyWriteExecute`（Server 设）、`RestrictAddressFamilies` 含 `AF_NETLINK`（Agent）。所有变量先过 `ValidateUnitValue`（路径另过 `ValidateUnitPath`），再叠加账号正则（非 root）、角色白名单、主机名规则、`LimitNOFILE` 范围。注入向量表（换行注入 `ExecStartPre=`、`%n`/`%h` 等说明符、空格/制表符、引号、反斜杠、`$`/反引号、`..`、相对路径、NUL/控制字符/非 ASCII、超长）对**每个**字符串变量都被拒绝；两个 fuzz（`FuzzRenderServer`/`FuzzRenderNode`，各 30 秒、数百万次执行无失败）断言「只要渲染成功，指令集合与默认渲染完全相同且无 `%`」。另一条测试保证模板里出现的每个指令都登记了 systemd 引入版本，`MinSystemd` 恒等于最大值。
- **AC2 运行用户与目录**：`Provisioner.EnsureUser` 经 safeexec 的 `id -u`/`getent group` 查询，仅不存在才 `useradd --system --no-create-home --shell /usr/sbin/nologin`（组已存在 `-g` 复用，否则 `--user-group`），结果带 `Created`/`GroupCreated` 并经 `UserUndo` 写成 undo；既有 UID 0 拒绝、UID ≥ 1000 警告；非 root 在 `useradd` 前就明确失败。目录按 §8 与 ADR-020 §2 总表数据化（`ServerDirs`/`NodeDirs`/`UpgradeResultDir`），测试里的**期望表手写自文档**，不引用实现自证：配置 root:`<组>` 0750、数据/日志 0750、`identity/` `certs/` 0700、`staging/` 等；文件规格 0640（配置）、`master.key` root:root 0400（默认）。创建即最终权限：`security.MkdirAllPerm`（0700 → 显式 chmod）+ chown；`FileWriter.Create` 用 `security.CreateFile`（`O_EXCL`）、**先 chown 后写内容**（测试断言 chown 发生时文件大小为 0）、从不覆盖 `master.key`。测试在 umask `0000/0022/0077/0277` 下得到完全相同的权限。属主用可注入的 `Ownership`：非 root 测试记录 chown 调用并断言 uid/gid；另有对「当前用户」的真实 `os.Lchown` 测试（unix）。
- **AC3 systemctl 封装**：`Systemctl` 的 `DaemonReload/Enable/Disable/Start/Stop/Restart/IsActive/IsEnabled/Version` 全部固定形状的参数数组、经 safeexec，单元名只认 `AllUnits`（`--now`、空格、换行、`../` 等在进程创建前被拒绝，测试断言「被拒绝时无任何进程」）。`Installer.Install`：不存在 → `registry.EnsureFile` 创建（root:root 0644）；内容一致 → `unchanged`（幂等）；内容不同或目标是符号链接 → **不覆盖**，写 `<unit>.new` 并返回警告（`PendingNewUnits` 供 doctor）；`Enable`/`Start` 返回 undo（只有此前未启用/未运行才 `Created`）。
- **AC4 LoadCredential 降级**：`ParseVersion`（11 个发行版样本，含空白前缀、无括号形态、垃圾输入）、`MinSystemd=235`/`CredentialMinSystemd=247`（边界测试）、`SelectCredentialMode`、**降级变体**（不含 `LoadCredential=`，其余与默认变体逐键相同）、`SpecMasterKey`（降级 = root:`<组>` 0440）、`MasterKeyConfigValue`（默认留空/降级填路径）、doctor 检查函数 `CheckSystemdVersion`/`CheckCredentialConsistency`（fail/warn/pass）。决策与取舍写入 **ADR-027**（并回写 ADR-011/024、tools-design §15.1/§15.5/§21）。
- **AC5 修订后的节点权限模型**：见 AC1 的加固断言；Agent `ReadWritePaths=/var/lib/lingedge-node /var/log/lingedge-node /run/lingedge-node`；节点目录总表逐项断言；`lingedge-openresty.service` 的 `ExecStartPre`/`ExecStart` 由**同一个子模板**生成同一对 `-p /var/lib/lingedge-node/current/ -c conf/nginx.conf`（测试断言两者以同一串结尾）；升级结果目录 `/var/lib/lingedge-node-upgrade/` root:root 0755、不在 Agent 属主的数据目录之下、不在 Agent 目录计划里；Server 单元 `LimitCORE=0`（两个变体都保留）。**「Agent 单元内 `openresty -t` 与生产启动结果一致」须在 Linux+OpenResty 验收机验证——等待环境验证（ENV-LNX）。**

## 测试（真实，伪二进制仅验证 argv）

- **真实**：golden/属性/注入/fuzz；真实文件系统（临时根目录）上的目录/文件创建、权限位、符号链接拒绝、`.new` 流程、undo 回放；真实 `registry` journal 往返（本包产出的 undo 写入真实 journal，`ReadTx().UndoPlan()` 读回并全部被 `Handles` 认领）；真实 `id`（本机 `/usr/bin/id`）经真实 safeexec 对 `root` 与不存在用户的解析；对当前用户的真实 `os.Lchown`。
- **伪二进制（须注明）**：本机是 macOS，没有 systemd，也不应创建系统用户。`fakebin_test.go` 在临时目录放 `/bin/sh` 脚本冒充 `systemctl`/`systemd-analyze`/`id`/`getent`/`useradd`/`userdel`，记录每次调用的 argv 并用状态文件模拟「已启用/已运行/用户存在」。它们经**真实的 `safeexec.Runner`**（白名单、固定目录解析、清洗环境）被执行，所以验证的是「本包构造的参数数组如何穿过 safeexec 到达进程」，**不验证** systemd/useradd 的真实行为——后者全部标 ENV-LNX。
- **跨平台**：Linux 专属（`sd_booted` 判定、ENV-LNX 门控测试）在 `*_linux.go`/`*_linux_test.go`；非 Linux 的 `systemdBooted()` 恒 false，`Systemctl` 明确返回 `not_systemd`（`TestSystemctlDefaultBootedPlatform` 在 macOS 上真实走这条降级）；属主读取在 `platform_unix.go`（linux、darwin）与 `platform_nounix.go`。`linux/amd64`、`linux/arm64` 交叉编译与 `go vet` 通过（另 windows/amd64 可编译）。
- 覆盖率 87.1%（`go test -cover`，`-race` 通过）。

## 变异验证（临时改动 → 测试必须失败 → 还原并逐字节比对）

仓库外的脚本（会话临时目录）逐条最小改动（保持可编译：用 `if false && cond` 而不是删除变量使用）→ 运行 `internal/tools/service` 与 `internal/tools/cli` 测试 → 立刻还原并比对 SHA-256，**72 条全部检出，无还原失败**：模板加固项（`NoNewPrivileges`、`LimitCORE`、`ProtectSystem`、`CapabilityBoundingSet`、MDWE 的增/删、`AF_NETLINK`、`ReadWritePaths`、OpenResty 的 `-p/-c` 分叉、`AmbientCapabilities`、`StartLimitIntervalSec` 位置、升级单元的路径参数/`MakeDirectory`）、校验（跳过 `ValidateUnitValue`、root 用户、未知/重复角色、主机名正则、`NOFILE` 范围、路径规则）、版本门槛与解析、目录（叶子权限被忽略、chown 缺失、既有权限不符被忽略、符号链接目录被接受、undo 的 Created/顺序）、规格（identity/升级目录/staging 的权限与属主、master.key 模式）、文件创建（权限/先 chown）、单元安装（覆盖而非 `.new`、不识别一致内容、undo 的 Created）、用户（uid 0、`--no-create-home`、shell、`Created` 标志、root 检查、组复用、用户名白名单）、撤销（忽略 `Created`、不复核哈希、不排除 Tools 状态目录、userdel 的 uid 守卫、跟随符号链接、非空目录报错）、systemctl（单元白名单、`booted` 检查、undo 的 Created、`alreadyEnabled`、多余 argv、enable `.service` 而非 `.path`）、白名单被放宽、cli 退出码映射。变异过程暴露并修正了测试自己的弱点：其中 7 条最初因「未使用变量」不能编译，改写成可编译形态后均被检出。

## 对既有测试/防护的改动（逐条）

- **没有放宽或豁免任何门禁**（G-4/5/6/8/11、depguard、forbidigo 的 exec 限制均未动；`.golangci.yml`、`scripts/gates` 均未改）。`safeexec.DefaultAllowed` **未改**：`getent` 不在其中，`service` 与 `probe` 一样构造自己的 `safeexec.Runner`（`service.Allowed` = `systemctl systemd-analyze id getent useradd userdel`，披露于此与 tools-design §15.5）。
- 既有生产代码仅一处改动：`internal/tools/cli/run.go` 两处各加一个分支（`isParseStageError` 与 `finish` 的错误分类），接入新文件 `cli/serviceerr.go`（`ServiceError`：3 = systemd 缺失/版本不足/需要 root/冲突/IO/目录不符，1 = 命令失败/撤销被拒/参数缺陷）。没有接线任何未实现的命令。既有 cli 测试原样通过。
- 文档：tools-design §15.1/§15.2/§15.5/§21/修订记录、ADR-011/ADR-024 追加实现记录、新增 ADR-027 与索引行。**未碰** OpenAPI/proto/迁移/CF-6/contract-v1。

## 与任务/通常做法不同的设计决定

- **模板的一处更正（须披露）**：tools-design §15.2 原文把 `StartLimitIntervalSec=0` 放在 `lingedge-openresty.service` 的 `[Service]`。systemd ≥ 230 该指令属于 `[Unit]`，放在 `[Service]` 会被忽略（`Restart=always` 的崩溃循环将触发默认限流并停止拉起，违背 FR-62）。golden 与文档均已更正到 `[Unit]`；其余内容逐字保持。
- **降级方案选了第三条路（ADR-027）**：任务与 §21 列的是「root 读取后降权」或 `EnvironmentFile` 0400。二者都需要 Server 代码改动（环境变量路径/以 root 启动再 setuid），且前者与「全部进程非 root」冲突。`pkg/secretbox` 已支持「配置的 `master_key_file`，0400/0440，属主为 root 或运行用户」，所以降级 = **`root:<运行组> 0440` 的密钥文件 + `secrets.master_key_file`**，无需任何 Server 改动；代价（运行组可读）与告警写在 ADR 的「后果」。可被推翻。
- **版本数字来自 systemd NEWS，而非仓库文档**：tools-design §21/ADR-011 §9 只写「实现时核对」，没有给数字。`LoadCredential` ≥247、加固单元 ≥235、各指令引入版本（`DirectiveSince`）是我核对的结果，真实发行版复验登记 ENV-LNX；`MinSystemd` 由测试约束为表中最大值，防漂移。
- **未复用 probe 的系统服务探测**：`probe` 当前不含 systemd 版本探测（只有 `systemctl list-units` 识别服务）；版本解析放在 `service`（`systemctl --version`），doctor（1.42）直接调用 `CheckSystemdVersion`，避免为一个函数改 `probe` 的只读 argv 表。
- **`FileWriter` 没有 `Replace`**：安装步骤只需要「不存在才创建、从不覆盖」；替换既有配置属于 1.33 的备份/journal 流程，那里才有旧内容可备份。

## 等待环境验证（ENV-LNX；代码已完整，待验收机复验，汇总于 Story 1.45）

1. 真实 `systemctl`（`daemon-reload/enable/start/stop/restart/is-active/is-enabled`）的输出词汇与退出码（本机伪二进制只验证 argv）；`systemd-analyze verify` 对 7 个渲染单元的真实结果（门控测试 `TestEnvLNXRealSystemctlReadOnly`：`LINGEDGE_ENV_LNX=1` 在 Linux 验收机运行，只读）。
2. `LoadCredential` 在 ≥247 上的真实行为（`lingedge` 用户不能直接读 `master.key`、进程能读 `$CREDENTIALS_DIRECTORY/master.key`）；<247（RHEL 8/Ubuntu 20.04）上降级变体以 `root:lingedge 0440` 启动成功；各发行版 `systemctl --version` 真实输出与 `DirectiveSince` 版本表复核。
3. 真实 `useradd/userdel/id/getent`（含 RHEL 系 `/usr/sbin/nologin`、`useradd` 对同名组的行为）；以 root 对 `lingedge`/`lingedge-node` 的真实 `chown`（本机只验证调用与对当前用户的真实 `lchown`）。
4. **Agent 单元内 `openresty -t` 与 OpenResty 单元启动结果一致**（AC5，IMP-20；需 Linux + OpenResty）。
5. `lingedge-openresty.path` 在首个配置代出现时真实触发、崩溃循环下 `StartLimitIntervalSec=0` 的拉起行为；`lingedge-node-upgrade.path` 的 `PathExists` 触发。

## 属后续 Story（不在本 Story 范围）

- 1.33：安装编排（调用顺序、journal 写入与 recover 驱动本包的 `Undoer`、`master.key` 生成步骤 18 与 `server.yaml` 生成、`daemon-reload` 时机）、把 `cli.ServiceError` 接到真实命令。
- 1.42：doctor 汇总（`CheckSystemdVersion`/`CheckCredentialConsistency`/`VerifyDirs`/`PendingNewUnits` 已提供）与 `--fix-perms`。
- §15.4 其他单元（`lingedge-website`/`lingedge-dns`/`lingedge-reporter`，P2/P3 起）。
