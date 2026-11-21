---
title: 'Story 1.26：security 叶子包：安全解压 / 安全执行 / 脱敏 / 权限'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '3784ca84e2e3fbe777247de223e14736b12d049d'
context:
  - docs/bmad/epics.md Story 1.26（AC1–AC5）
  - docs/architecture/tools-design.md §18（安全解压算法、防命令注入、其余）、§15（unit 变量校验）、§5（日志脱敏）
  - docs/security/threat-model.md §9 G-4（路径穿越向量）、G-12（命令注入）
  - docs/bmad/stories/1-25-*、1-24-*（Story 文件格式与遗留登记口径）
---

## Intent

**Problem:** Tools 后续 Story（downloader 1.28、installer 1.33、updater、backup 等）都要解压归档、执行 systemctl/mysqldump 等外部命令、写日志与带权限的文件。若各处自行处理，路径穿越、命令注入与口令泄露会散落在每个调用点。`internal/tools/security` 目前只有占位 `doc.go`。

**Approach:** 实现一个只依赖标准库与 `pkg/redact` 的叶子包，使这三类风险在结构上被杜绝：`security.SafeExtract`（G-4）、子包 `security/safeexec`（Tools 里唯一允许创建进程的位置，G-12）、`ValidateUnitValue`、对 `pkg/redact` 的封装与权限助手。新增静态门禁 `gates extract` 让归档只能经 `SafeExtract` 解压，并把 G-4 向量集接入 `make security`。

## 逐条验收

- **AC1 SafeExtract**（`internal/tools/security/extract.go`、`errors.go`）：输入 tar 或 tar.gz 流，在同级 `dst.partial` 内解压，全部成功后 rename 为 `dst`；失败整体删除 `.partial`（目标或 `.partial` 已存在则拒绝且不动对方的数据）。仅允许 regular file/directory，符号链接、硬链接、字符/块设备、FIFO、全局 PAX 头一律 `ErrUnsupportedType`；路径经 `CleanEntryName`：拒绝空名、非法 UTF-8、NUL/控制字符/反斜杠、绝对路径、驱动器前缀、百分号编码（`%2e%2e`）、`..`/`.`/空段、单段 >255、整条/深度超限，另检测大小写冲突、重复条目、文件当目录；写入用 `os.Root`（逐级不跟随符号链接）并叠加词法落点检查与每个新目录的 `EvalSymlinks` realpath 复核（三层）；写后复核为单链接普通文件。权限：包内 mode 位完全忽略，目录 0755、文件 0644，仅 `Executable`（META 声明）集合得 0755，创建时 0600 + 显式 `Chmod`，不依赖 umask；属主留给调用方。限额 `Limits{MaxFiles,MaxTotalBytes,MaxFileBytes,MaxPathLen,MaxDepth}` 零值取默认、超出 `Abs*` 绝对上限则收紧；声明大小在写入前检查，实际写入由 `limitWriter` 流式检查，解压后的 tar 流总量由 `capReader` 封顶（含归档结束后的尾部垃圾，防压缩炸弹）。边写边算 SHA256，对照 `Checksums`（`ParseSHA256SUMS` 解析已验签的 `META/SHA256SUMS`）；缺失 `ErrMissing`、多余 `ErrExtra`（`Unchecked` 只豁免点名路径，如 SHA256SUMS 自身）、不符 `ErrChecksum`。调用方把任一失败映射为退出码 5（tools-design §18.1）。
- **AC2 向量集与 fuzz**：`tests/security/g4_pathtraversal_test.go`（G-4，接入 `make security`）对真实 `SafeExtract` + 真实文件系统投入 33 个向量（`../`、中段 `..`、多级穿越、绝对路径、`//`、反斜杠、驱动器前缀、`%2e%2e`/`%2E%2E%2F`/双重编码、控制字符、过深/过长、符号链接指向外部目录/绝对路径/先目录后链接逃逸/指向包内、硬链接（包内/系统文件）、字符/块设备、FIFO、超限条目数、声明过大的压缩炸弹、累计过大、归档后追加 64MiB 垃圾、清单多余/缺失/哈希不符、大小写冲突覆盖、文件当目录），断言每条被拒绝、错误类别正确、**整个沙箱（含父目录之外的受害者文件与兄弟目录）的目录树哈希快照在失败后与之前完全一致**；`TestG4PreplantedSymlinks`（预置 `release`/`release.partial` 符号链接不被跟随）；`TestG4ValidArchiveStaysInsideTarget` 作对照组防止「全拒绝」式假通过。细分用例在 `internal/tools/security/extract_test.go`（类型、限额边界「恰等于上限通过/多 1 字节失败」、炸弹、校验和、冲突、目标前置条件、context 取消、`CleanEntryName` 表驱动、`ParseSHA256SUMS`）。Fuzz：`FuzzSafeExtract`（任意字节×gzip 开关；不变量：失败后父目录无任何残留、成功后只有目录与 0644/0755 普通文件、哨兵与沙箱外无改动）、`FuzzCleanEntryName`、`FuzzValidateUnitValue`、`FuzzRunArgsInert`；种子语料在 `go test` 时作为回归运行，另做限时模糊（见门禁结果）。
- **AC3 safeexec**（`internal/tools/security/safeexec`）：`Run(ctx, Cmd{Path,Args[],Env,Stdin,Dir})` 只有参数数组入口；`Path` 为白名单（tools-design §18.2 的 12 个名字，`New` 拒绝 shell 解释器/`env`/`xargs`/`sudo` 等转调程序与含路径/空白的项）内的裸名（只在固定目录集合解析，**不读调用方 PATH**），或基名在白名单内的规范绝对路径；解析结果（EvalSymlinks 后）必须是普通可执行文件、group/other 不可写、无 setuid/setgid。环境：不继承父进程环境，固定 `PATH=/usr/sbin:/usr/bin:/sbin:/bin`、`LC_ALL=C`、`LANG=C`，调用方变量须 `KEY=VALUE`，拒绝 PATH/LC_ALL/LANG/IFS/ENV/BASH_ENV/LD_*/DYLD_*/BASH_FUNC_*。参数含 NUL/超长/过多被拒；输出每路 1MiB 封顶（`Truncated`）、默认 5 分钟超时、context 取消终止子进程；非零退出返回 `*ExitError`（argv 与 stderr 摘要已脱敏）。元字符注入测试：25 个向量（`;`、`&&`、`|`、`$()`、反引号、换行、引号逃逸、`${IFS}`、重定向、通配符、`-rf /`、`%n`、空串…）经真实 `printf` 原样回显、经真实 `id` 只得到「无此用户」，哨兵文件从未创建。`Resolve` 已有 G-12 静态门禁（`gates shell` 对本包同样生效；`forbidigo` 对 `os/exec` 的豁免仍只给 `^internal/tools/security/`）。
- **AC4 ValidateUnitValue / redact / 权限**：`ValidateUnitValue`（`unit.go`）仅 `[A-Za-z0-9_./:@-]`，拒绝空、`..`（连续点号，比「路径段为 ..」更严）、含 `/` 的值必须是规范绝对路径；`ValidateUnitPath` 要求绝对路径。拒绝清单测试含空白/换行/`%`（systemd specifier）/`$`/反引号/引号/`;`/控制字符/非 ASCII 等 50 项。脱敏（`redact.go`）是对 `pkg/redact` 的**封装而非第二套规则**（`TestRedactDelegatesToPkgRedact` 逐样本断言与 `pkg/redact` 输出一致）；Tools 特有的命令行脱敏 `RedactArgs` 由 Story 1.25 的 `internal/tools/cli/logger.go` 迁入，`cli.redactArgs` 现为一行委托，原 `TestRedactArgs` 与 G-5 `TestLogScanToolsCLI` 保持原样通过。权限助手（`perm.go`）：`CreateFile`（O_EXCL|O_NOFOLLOW、0600 创建后立即显式 chmod 为最终权限、拒绝 setuid/setgid/sticky）、`WriteFileAtomic`（临时文件→chmod→写→fsync→rename→fsync 目录，失败无残留）、`MkdirAllPerm`、`WithUmask077`；测试在 umask 000/022/077/277 下断言最终权限都精确等于请求值。
- **AC5 构建隔离**：平台相关代码按 build tag 隔离：`security/platform_unix.go`（`O_NOFOLLOW`、`Umask`、硬链接计数，`//go:build unix`）与 `platform_other.go`；`safeexec/platform_linux.go`（`//go:build linux`：`Setpgid` + `Pdeathsig: SIGKILL`，Tools 被杀时内核回收子进程）与 `platform_other.go`/`pgid_unix.go`/`pgid_other.go`。`GOOS=linux GOARCH=amd64|arm64` 的 `go build ./...` 与对 `internal/tools/...`、`tests/security`、`scripts/gates` 的 `go vet` 均通过，另验证 `GOOS=windows` 可编译 security 包。

## G-4 与 G-12 如何关联本 Story

- **G-4（路径穿越/越界写入）**：本 Story 是 Tools 侧的落点（traceability：G-4 → 1.26、1.28、2.11、3.34、6.19）。门禁由两部分组成并已接入 `make security`：① 动态——`tests/security/g4_pathtraversal_test.go` 向量集（含沙箱快照比对）；② 静态——新增 `gates extract`（`scripts/gates/extract.go`）：非测试 Go 代码除 `internal/tools/security/` 外不得 import `archive/tar`/`archive/zip`，使「归档只能经 SafeExtract」可被机器检查（含 `scripts/gates` 单测的 5 负向 6 正向与 `tests/gates` 副本端到端负向用例）。Makefile 的 `security` 目标文案与注释同步更新为「G-4 已接入（Tools 解压范围）」；节点 Agent 解包（3.34）、日志下载（2.11）、媒体上传（6.19）的 G-4 向量属各自 Story。
- **G-12（命令注入 + 任务类型白名单）**：G-12 有两半。**命令注入半**（`shell`、`forbidigo`，Story 1.2）早已把 `internal/tools/security` 定为唯一白名单入口——本 Story 就是那个入口的真实实现，`gates shell` 现对真实代码生效（0 发现），并以变异验证证明「改成经 `/bin/sh -c` 执行」会被向量测试检出。**任务类型白名单半**（`gates tasktype`、`taskrt.Registry`，Story 1.14，FR-35）约束的是 Server 任务注册表，Tools 没有任务队列，本 Story 不触及也无需修改；追溯表中「G-12 ← 1.26」应理解为命令注入半。`make security` 中 `tasktype` 照常运行且通过。

## 与「通常做法」不同的设计决定

- **`safeexec` 是 `security` 的子包**，而非 `security` 内的文件：epics AC3 写作 `safeexec.Run`，tools-design §18.2 写作 `security.Run`；子包同时满足两种引用的精神（`.golangci.yml` 的 forbidigo 豁免与 `gates shell` 路径都以 `internal/tools/security/` 为前缀，无需改动配置），且 `security` 本体保持无 `os/exec` 依赖。
- **SafeExtract 带 `ctx` 参数与 `Checksums` 必填**：缺校验表是调用错误（`ErrOptions`），而不是「跳过校验」；`.partial` 残留视为调用方的数据，拒绝而不删除（避免误删他人文件）。
- **一律拒绝百分号编码序列（`%XX`）**：发行包文件名不需要它；`100%.txt` 这类 `%` 后不接两个十六进制字符的名字仍允许。
- **ValidateUnitValue 对 `..` 取子串而非路径段**：略严于 tools-design 的措辞，`a..b` 也拒绝，换取无歧义。
- **`cli.redactArgs` 保留为委托**而没有删除，使 1.25 的测试与 G-5 用例不需任何改动。
- **gzip 作为 `ExtractOptions.Gzip` 开关**：tar 流总量上限作用于解压后的流，防止压缩比攻击；调用方（1.28）负责在此之前完成对包的签名/哈希验证（tools-design §9）。

## 变异验证（临时去掉防线 → 测试必须失败 → 还原）

脚本逐条在源码里做最小改动、运行 `./internal/tools/security/... ./tests/security/`、立刻还原并 `diff -r` 确认字节一致。SafeExtract/路径/权限共 24 条，**全部被检出**：类型白名单放开；去掉百分号/绝对路径+空段/反斜杠/控制字符/过深/过长检查；去掉 `..` 段检查、去掉 `..`+词法落点检查（仅剩 `os.Root` 兜底）、再加上绕过 `os.Root`（改用普通 `os`）——最后这条使 G-4 向量集中 `../`、中段 `..`、多级穿越等向量真实越界写入而失败；去掉条目数限制、`capReader` 流总量上限、声明大小检查+实际写入限制、校验和比对、多余/缺失条目检查、失败时清理 `.partial`、大小写冲突检查、目标已存在检查；改用包内 mode 位、去掉文件 `Chmod`；`ValidateUnitValue` 放行 `%`/去掉 `..` 拒绝；权限助手去掉显式 chmod。safeexec 共 11 条，**全部被检出**：去掉白名单（裸名/绝对路径基名）、不要求规范绝对路径、继承父进程环境、放行 `LD_*`、不检查 group/other 可写与 setuid、不拒绝 NUL 参数、**改为 `sh -c` 拼接执行**、输出不设上限、错误不脱敏。
变异验证中发现并改进了三处弱测试（已修）：① 大小写冲突在 macOS（大小写不敏感）上会被文件系统「碰巧」拒绝，使变异不可检出——测试现要求错误必须来自大小写冲突逻辑（Linux 上文件系统不会救场）；② 「目标已存在」会被事后 `rename` 失败掩盖（Linux 上 rename 会静默替换空目录）——测试现要求在解压前以「已存在」拒绝；③ 多个单层变异因纵深防御被下一层挡住（如只去掉 `..` 检查，词法落点检查仍拒绝）：这是设计意图，因此另外组合去掉多层来证明最终防线（`os.Root`）与整套向量的有效性。

## 对既有测试/防护的改动（逐条）

- `internal/tools/cli/logger.go`：`redactArgs` 及其私有辅助（`sensitiveFlag`/`redactSet`）迁至 `security.RedactArgs`，`redactArgs` 改为一行委托；日志写入改用 `security.RedactString/RedactValue`（它们直接转调 `pkg/redact`，行为不变）。**没有修改任何 cli 测试**（`TestRedactArgs`、`TestLogScanToolsCLI` 原样通过）。
- `scripts/gates/main.go`：`gates` 表**增加** `extract`（只增不减）；`scripts/gates/gates_test.go` 增 `TestExtract`；`tests/gates/gates_selftest_test.go`：干净副本门禁清单增 `extract`、新增 1 个 `extract` 负向用例（只增不减）。
- `Makefile`：`security` 目标增 `$(GATES) extract`；注释与 `@echo` 文案把 G-4 从「未接入」改为「已接入（Tools 解压范围）」。
- **未**改动：`.golangci.yml`（forbidigo 对 `^internal/tools/security/` 的豁免本来就覆盖子包）、`docs/architecture/tools-design.md`、任何迁移（000001–000018 冻结）、G-5/G-6/G-11/arch/depguard 规则。**没有放宽或豁免任何门禁**：我的测试曾被 G-6 `passwords` 门禁命中（`password=` 后接字面量、URL 内嵌口令、`secret` 键后接字面量），已改为运行期随机值/拼接键名，未加任何豁免。

## 测试（真实、无 Mock）

全部使用真实文件系统、真实 tar/gzip、真实子进程（`printf`/`id`/`printenv`/`tr`/`sleep`，缺失时 `t.Skip` 并标明原因）；无 testdouble、无桩。`safeexec` 测试使用的非默认白名单仅为让测试能调用本机真实存在的小工具，生产默认白名单不受影响。

## 门禁结果

提交前在本机（macOS arm64，go1.27.4；临时 mysqld/redis 由 tests/mysqltest、tests/redistest 自启并清理，未触碰常驻 mysqld 576/744 与 127.0.0.1:6390 的 redis；`df -h /` 剩余 25GiB）同步执行：`gofmt -l .` 无输出；`make lint`（vet、scaffold、arch、golangci 0 issues、shell、repo）、`make security`（testdouble/passwords/brand/shell/tasktype/trustdomain/flags/**extract** + `tests/security/...` 含 G-4 向量集与租户隔离矩阵）、`make gate-selftest`（含新增 extract 正/负向）、`make race`（`-race`，前台等待至结束：63 个包 ok、无 DATA RACE/FAIL，`internal/tools/security` 与 `safeexec` 均含其中）、`make vuln`（No vulnerabilities found）、`make contract-check`、`make build`、`make verify-build`、`make migrate-test` 全部退出码 0。另：`GOOS=linux GOARCH=amd64|arm64 go build ./...` 与 `go vet`（internal/tools、tests/security、scripts/gates）通过。
限时模糊（本机）：`FuzzSafeExtract` 60s（约 1.5 万次执行，新增语料 14 条）、`FuzzCleanEntryName` 20s、`FuzzValidateUnitValue` 20s、`FuzzRunArgsInert` 15s，均无失败，未产生 `testdata/fuzz` 失败语料（种子语料在常规 `go test` 中作为回归运行）。

## 遗留与风险（只列确属后续 Story 或需特定环境的）

- **等待环境验证（ENV-LNX）**：① Linux 文件系统语义——大小写敏感文件系统上的冲突行为（逻辑检查已在 macOS 验证，真实 Linux 未跑）、`os.Root` 底层 `openat2(RESOLVE_BENEATH)` 路径、`O_NOFOLLOW`、权限位与 root 属主（本机是非 root 单用户）；② `safeexec` 的 `Pdeathsig`（Tools 被 SIGKILL 时子进程被内核回收）与白名单程序（`systemctl`/`journalctl` 等）在真实发行版固定目录中的解析；③ root 用户下 `WriteFileAtomic`/`CreateFile` 的属主与 `/etc`、`/var/lib` 路径行为。Linux 测试二进制已确认可交叉编译（amd64/arm64），但未在 Linux 上执行。
- **Deferred（确属后续 Story）**：downloader 与离线 bundle 完整校验链调用 `SafeExtract`（1.28，含对包的验签/哈希先于解压）；manifest 验签与 `META/SHA256SUMS(.sig)` 的产生（1.27）；unit 模板渲染调用 `ValidateUnitValue`（1.32）；各业务命令使用 `safeexec`（1.31/1.32/1.33/1.41/1.42）；节点 Agent 解包（3.34）、日志下载（2.11）、媒体上传（6.19）的 G-4 向量；发布流水线构建期消除包内符号链接并加包内无链接扫描（1.34）。
- **已知限制（如实）**：`FuzzSafeExtract` 的 fuzz 进度行偶见 `0/sec`（Story 1.27b 复跑 25 秒观察到：中途也会出现，随后恢复，最终 PASS、无失败语料）——这是 Go fuzz 协调器按采样间隔汇总执行数、且 worker 在最小化新发现输入/每次执行都做真实文件系统解压时的正常表现，不是挂死；`SafeExtract` 不设置属主（留给调用方 chown）；PAX 全局头（如 `git archive` 产物）被拒绝；tar 名称无法携带 NUL（tar 格式本身截断），NUL 向量由 `CleanEntryName` 单测与 fuzz 覆盖，而不在 tar 向量集中；`safeexec` 不解释参数语义（选项注入如以 `-` 开头的值由调用方的类型化校验负责，tools-design §18.2 第 2 条）。
