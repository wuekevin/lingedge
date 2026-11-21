# LingEdge Tools（`lingedge`）CLI / TUI 交互设计

> 属于 `docs/ux/ux.md`。依据基线 §4（CLI/TUI）、§21（安装）；PRD FR-1…FR-15、D-16、R-3。
> 2026-11-16 依据架构评审修订：见 docs/bmad/architecture-revision-log.md（退出码、`--accept-irreversible`、`--force-reenroll`、setup 410、enroll 令牌用法）。
> **本文规定交互与呈现约定**；退出码**数值与子命令参数的最终表**以 Tools 技术设计 `docs/architecture/tools-design.md` §3/§4 为唯一事实源（2026-11-16 起本文镜像已与之一致）。
> 本文示例中的版本号、IP、路径均为文档示例，不代表真实发行数据。

## 1. 设计原则

1. **一个入口**：`lingedge`。无参数且在 TTY 中 → 进入交互 TUI；带子命令 → 直接执行（脚本友好）。不存在 `lingedge-updater`（FR-1）。
2. **交互与非交互等价**：TUI 里能做的每件事都有对应命令行与 `--yes`/参数形式；非交互模式**绝不弹出提示**，需要确认而无确认参数时直接失败（退出码见 §4）。
3. **不静默**：任何会覆盖既有网站、数据库、证书、Nginx 配置的动作必须停下并说明（FR-6、SM-C3）。TUI 以「冲突 → 选项」呈现，非交互以非零退出码 + 说明呈现。
4. **不夸大**：未发布的组件如实显示「尚未发布」，退出码区别于失败；绝不「假装安装成功」（FR-3）。
5. **可恢复**：安装/升级可中断；再次运行时检测上次状态，提供「继续 / 回滚 / 放弃」。
6. **机器可读与人类可读分离**：人类输出走 stderr/TTY 格式化，`--json` 的结构化结果走 stdout（§7）。
7. **敏感信息不落屏不落日志**：令牌、密码、私钥只以遮盖或「仅显示一次」形式出现；日志脱敏（FR-9）。
8. **语言**：随 `LANG` 选择 zh/en，可用 `--lang zh|en` 覆盖；`--json` 的字段名与枚举**不本地化**。

## 2. 交互菜单（`lingedge` 无参数）

```
 ◇ LingEdge Tools  v1.0.0                       服务器: srv-01 (Linux amd64)
 ──────────────────────────────────────────────────────────────
   1  安装组件
   2  查看已安装组件
   3  升级组件
   4  服务管理
   5  节点注册
   6  系统诊断
   7  配置管理
   8  备份恢复
   9  查看运行日志
  10  卸载组件
  11  查看版本
   0  退出
 ──────────────────────────────────────────────────────────────
  ↑↓ 选择   Enter 确认   数字 直达   ? 帮助   q/0 退出
```
- **编号与基线 §4.3 完全一致**（1–11、0）。数字键直达；支持 `↑↓`/`j k`；`?` 打开帮助；`Esc` 返回上级；`Ctrl+C` 安全中断（见 §8）。
- 顶部状态行显示：Tools 版本、主机、已安装组件数、是否检测到待处理的未完成任务（有则出现一行醒目提示「检测到未完成的安装任务 [继续/回滚]」）。
- 无 TTY 或终端过窄（<60 列）→ 不进入 TUI，打印帮助摘要与「请使用子命令或在交互终端中运行」，退出码 `2`（用法）。

## 3. 安装（多选安装 · 预检 · 进度 · 冲突）

### 3.1 多选安装界面（菜单 1）

```
 安装组件 — 选择要安装的组件（可多选）
 ──────────────────────────────────────────────────────────────
  [x] Server        中央主控 API（内置 Scheduler / Worker）
  [x] Admin Web     管理员后台
  [ ] Console       用户控制台                  尚未发布 (P5)  ← 灰显，不可选
  [x] Website       官方网站（静态预渲染）
  [ ] Node          CDN 边缘节点                建议在边缘服务器上单独安装
  [ ] DNS           智能 DNS 服务               尚未发布 (P3)
  [ ] Reporter      区域监控探针                尚未发布 (P3)
  [ ] Docs          帮助与开发者文档            尚未发布 (P5)
  [ ] Status        服务状态页                  尚未发布 (P5)
 ──────────────────────────────────────────────────────────────
  已选 3 项   ↑↓ 移动   Space 勾选   a 全选可用项   Enter 继续   Esc 返回
```
- **列表项与基线 §4.3 一致**：Server / Admin Web / Console / Website / Node / DNS / Reporter / Docs / Status。**Scheduler 与 Worker 不在列表中**（内置于 Server，D-16）；命令行 `lingedge install scheduler|worker` 返回「已内置于 Server，无需单独安装」，退出码 `8`（与失败区分）。
- **未发布组件**灰显并标注阶段，**不可勾选**（防止假安装）；命令行安装则返回「组件尚未发布」退出码 `7`。
- 选择后「依赖分析」页：自动提示需要的前置（例：选 Admin Web/Website 需要 Server 或指向已有主控；选 Node 需要 OpenResty——将安装**独立 OpenResty**，不动宝塔 Nginx）。用户可看到**将安装什么、装到哪、占用哪些端口、需要多少磁盘/内存**，再确认。
- 同时选择 Node 与 Server：允许，但提示「生产环境建议将 Node 与主控分机部署」（不强制）。

### 3.2 预检与冲突（对应基线 §4.5 的 1–8 步）

```
 预检结果
  ✓ 操作系统   Debian 12 (amd64)
  ✓ 内存/磁盘   4.0 GiB / 38 GiB 可用  (所选组件需要 ≥ 1.5 GiB / 5 GiB)
  ✗ 端口冲突   80/tcp 已被 nginx (pid 1234, 宝塔) 占用  →  Node 默认需独占 80/443
  ✓ MySQL      检测到 8.0.x（宝塔），将作为外部数据库使用
  ⚠ Redis      未检测到。向导中需提供地址，或先安装 Redis 8
  ✓ 宝塔       检测到 BT-Panel 9.x（仅识别，LingEdge 不会修改其站点/Nginx 配置）

  端口冲突 — 请选择处理方式：
   1) 更换 Node 的监听端口（需自行把流量转发到新端口）
   2) 我会先自行释放 80/443，然后重试
   3) 取消安装
  （LingEdge 不会自动停止或修改宝塔 Nginx）
```
- 冲突页每项都要有**可行的选项**与**不会发生的事**（括号里的承诺）。选「自行处理」后提供「重新检测」。
- 非交互模式：遇冲突直接失败，退出码 `4`（PORT_CONFLICT），JSON 中 `error.code = "port.conflict"`，`error.details` 含端口与占用者。

### 3.3 安装进度（对应基线 §4.5 的 22 步）

```
 安装 Server                                          总进度 16/22  ████████████▒▒▒▒ 73%
 ──────────────────────────────────────────────────────────────
  ✓  1 OS 检测              ✓  9 版本选择              ✓ 17 安装 systemd Unit
  ✓  2 CPU 架构             ✓ 10 下载签名版本清单      ● 18 初始化配置
  ✓  3 内存与磁盘           ✓ 11 验证清单签名          ○ 19 启动服务
  ✓  4 端口冲突             ✓ 12 下载组件包            ○ 20 健康检查
  ✓  5 宝塔环境             ✓ 13 校验 SHA256 与签名    ○ 21 保存安装状态
  ✓  6 Nginx/OpenResty      ✓ 14 安全解压              ○ 22 输出访问/管理信息
  ✓  7 MySQL/Redis          ✓ 15 创建运行用户
  ✓  8 组件依赖分析         ✓ 16 创建配置目录
  当前：初始化配置 → /etc/lingedge/server.yaml（已存在配置不会被覆盖）
  已下载 lingedge-server_1.0.0_linux_amd64.tar.gz  12.4 MiB · SHA256 与签名 ✓
  日志：/var/log/lingedge/tools/install-20261116-1403.log
  Ctrl+C 将在当前步骤结束后安全中止（不会留下半成品）
```
- **22 步标签与基线 §4.5 顺序逐一对应**，步号固定（脚本/日志可引用）；`✓` 完成、`●` 进行中、`○` 待办、`✗` 失败、`↷` 跳过（说明原因）。
- 下载/校验步骤显示**真实**速度与字节；校验步骤明确写出「清单签名已验证（密钥指纹 SHA256:…）」「包 SHA256 与签名均通过」——这是供应链信任的可见确认。
- 中段步骤失败 → 立即停止并进入 §4 的错误呈现，同时显示**恢复选项**：`[r] 重试此步  [b] 回滚本次安装  [q] 保留现场退出`。
- **非 TTY 进度**：每步一行日志式输出（`[13/22] 校验 SHA256 与签名 … ok`），不做动画；`--json` 时 stderr 输出 NDJSON 进度事件（§7.3）。

### 3.4 完成输出（第 21–22 步）

```
 ✓ Server 安装完成                                   用时 2m 14s
  服务    lingedge-server  active (running)   健康检查 ✓
  版本    1.0.0 (build 7f3a1c2)
  Web 初始化（首次）：
     地址    http://203.0.113.5:8080/setup
     令牌    ak7k-9d2m-ph4q     ← 仅显示这一次，并已存入服务器的安全位置供找回
  下一步：在浏览器打开上述地址完成初始化；然后 lingedge install node
  详细日志：/var/log/lingedge/tools/install-20261116-1403.log
```
- **一次性初始化令牌**仅在终端输出（Web 向导要求输入，见 `screens.md` §9）；日志中脱敏为 `ak7k-****`。`[ARCH][ASSUMPTION]`
- 之后若忘记令牌且向导尚未完成：`lingedge config setup-token --reset`（L4 风格确认，需要 root）重新生成；向导完成后该命令返回「已完成初始化」；向导页面与 `/api/v1/public/setup/*` 接口此后一律返回 **410 `setup.already_installed`**（不再是 404）。`[ASSUMPTION]`
- 访问信息区**不会**输出任何默认账号密码（NFR-6）。

## 4. 错误呈现与退出码

### 4.1 错误的统一版式（TTY）

```
 ✗ 安装失败：步骤 11 验证清单签名
   发生了什么    版本清单的签名与内置公钥不匹配
   为什么        清单文件可能被篡改，或使用了未知的签名密钥（密钥指纹 SHA256:9ab4…）
   怎么办        1) 重新运行以重新下载（可能是传输损坏）
                 2) 若在离线环境，请确认使用了官方离线包及其签名文件
                 3) 仍失败请勿继续，并联系发行方
   系统状态      未做任何修改（失败发生在下载阶段，不会留下半成品）
   退出码 5      错误码 MANIFEST_SIGNATURE_INVALID（VERIFY_FAILED）
   日志          /var/log/lingedge/tools/install-20261116-1403.log   请求 ID: t_5d21
```
- 固定五段：**发生了什么 / 为什么 / 怎么办 / 系统状态 / 退出码+错误码+日志**。「系统状态」必须明确**是否留下修改、是否已恢复**，这是运维者最关心的信息。
- 色彩：错误红、警告黄、成功绿；**不只靠颜色**（带 `✗ ⚠ ✓` 与文字）；`NO_COLOR` 与 `--no-color` 全部关闭色彩。
- 绝不在错误中回显令牌/密码/私钥；路径与命令参数若含敏感值则打码。
- 不用堆栈跟踪作为默认输出；`--verbose` 或 `--debug` 才附加（并同样脱敏）。

### 4.2 退出码（**数值与名称以 `docs/architecture/tools-design.md` §4 为唯一事实源**；本表为呈现用镜像，2026-11-16 起与其逐项一致）

| 码 | 名称 | 含义 | 典型场景 |
|---|---|---|---|
| 0 | OK | 成功（含 `--dry-run` 计划成功、`doctor` 无失败项） | — |
| 1 | GENERAL | 未分类的运行时错误 | 意外失败（日志中含细节） |
| 2 | USAGE | 命令用法/参数错误；非 TTY 下请求交互 | 未知子命令、参数缺失、非法值 |
| 3 | PRECONDITION | 前置条件/预检失败 | 非 root、资源不足、依赖缺失、OS/架构不支持、`doctor` 有失败项 |
| 4 | PORT_CONFLICT | 端口冲突 | 80/443/53/9443 被占用 |
| 5 | VERIFY_FAILED | 校验/签名失败 | 清单签名无效、SHA256 不符、包被篡改、sequence 回退、清单过期、enroll 令牌无效 |
| 6 | LOCKED | 任务锁被占用 | 同一目标已有安装/升级任务 |
| 7 | NOT_RELEASED | 组件尚未发布 | MVP 阶段 `install dns` |
| 8 | BUILTIN | 已内置，无需安装 | `install scheduler|worker`（D-16） |
| 9 | CONFIRM_REQUIRED | 需要显式确认但处于非交互且未提供确认参数 | 不可逆迁移缺 `--accept-irreversible`、`--purge` 缺 `--confirm-name` |
| 10 | ROLLED_BACK | 升级/安装失败，**已自动回滚，状态已恢复为操作前** | 升级后健康检查失败 |
| 11 | HEALTH_FAILED | 健康检查失败（且未回滚或无需回滚） | `status` 发现组件不健康 |
| 12 | ROLLBACK_FAILED | 回滚本身失败，**需人工恢复**（保留 journal 与备份） | 回滚也失败 |
| 13 | INTERRUPTED | 存在未完成事务，需 `lingedge recover` | 上次被 kill/断电 |
| 14 | NOT_INSTALLED | 目标组件未安装 | `service start node` 而未装 |
| 130 | SIGINT | 用户中断（Ctrl+C），已按 journal 处理 | 已在安全点停止 |

- **10 与 12 的区分是关键 UX**：脚本/运维者必须能区分「失败但已恢复到可用状态」与「失败且需要人救」（二者互斥）。
- `doctor`/`config validate` 的专用语义：存在**警告**但无错误 → 退出码 `0` 且 JSON 里 `warnings` 非空；`--strict` 把警告升为失败（退出码 `1`）。`config validate` 失败时指出**文件、行号/键路径**（FR-15）。
- 退出码表在 Docs 中公布（FR-4 可验证项），并以 `lingedge help exit-codes` 在终端可查。

## 5. 升级、回滚与不可逆迁移（闭环 J）

### 5.1 `update check`
```
 $ lingedge update check
  组件        已装        可用        兼容性        迁移
  server      1.0.0      1.1.0      ✓ 兼容         含 1 个迁移（前向兼容）
  node        1.0.0      1.1.0      ✓ 兼容         —
  admin-web   1.0.0      1.1.0      ✓ 兼容         —
  ⚠ server 1.2.0 含不可逆迁移（0042_split_site_settings）→ 升级需要显式确认并先备份
  清单签名 ✓ 已验证（SHA256:…）
```
- 迁移分类用**三级标签**：`可逆` / `前向兼容` / `不可逆`（R-3、NFR-23），颜色：绿 / 黄 / 红 + 文字。

### 5.2 升级流程（对应基线 §4.7）
`检测版本 → 获取签名清单 → 检查兼容性 → 下载 → 校验 → 备份当前版本 → 检查数据库迁移 → 安装新版本 → 启动及健康检查 → 成功确认或回滚`，用与 §3.3 同样的步骤进度版式，步号固定。

### 5.3 不可逆迁移的确认（TUI）
```
 ⛔ 此升级包含不可逆数据库迁移
 ──────────────────────────────────────────────────────────────
  server 1.1.0 → 1.2.0   迁移 0042_split_site_settings（不可逆）

  这意味着：
   • 升级失败时 **不能** 仅靠换回旧版本程序来恢复。
   • 回滚路径是「恢复升级前的备份 + 换回旧版本程序」。
   • 备份点之后产生的数据（新站点、新配置、日志上报）将会丢失。

  Tools 将先创建备份（数据库 + 配置）：
     /var/backups/lingedge/pre-upgrade-1.1.0-20261116-1420/   预计 1.2 GiB
  备份完成并校验通过后才会继续。

  请输入  confirm-irreversible  以确认（或 Esc 取消）: _
```
- 要求输入**固定的 ASCII 确认语**（避免输入法差异），不接受仅按 Enter。
- 顺序固定：警告 → **备份（强制，不可跳过）并校验** → 确认 → 升级。**确认语在备份成功之后才被接受**；备份失败则中止，退出码 `1`（GENERAL）并说明。
- 非交互：必须同时带 `--yes` 与 `--accept-irreversible`（统一标志名，以 tools-design §4/§17 为准）；缺少则退出码 `9`（CONFIRM_REQUIRED）；备份仍然强制。
- 升级失败自动回滚时，输出：
```
 ✗ 升级失败：健康检查未通过（lingedge-server 启动后 30 秒内 /healthz 返回 503）
   系统状态   已自动回滚：已恢复升级前备份并换回 1.1.0 · 服务已恢复 ✓ · 数据库状态与升级前一致
   提醒       备份点（14:20:05）之后到失败之间的数据已丢失：无（升级期间主控处于维护只读）
   退出码 10  ROLLED_BACK
```
  → 「提醒」一栏**具体说明数据损失**（量化到时间点），不使用笼统措辞。回滚也失败时输出 `NEEDS_MANUAL` 版式并给出手动恢复步骤与备份路径。
- `lingedge rollback node`：对节点回滚到上一版本；若上一版本之间存在不可逆迁移（仅适用主控侧），显示 §5.3 同样的限制说明。

## 6. 其他命令的输出样式

### 6.1 `lingedge status` / `list` / `service status --all`
```
 组件          版本    状态            端口       运行用户       健康
 server       1.0.0   ● running       8080       lingedge       ✓
 admin-web    1.0.0   ● served(nginx) 80         www            ✓
 website      1.0.0   ● served(nginx) 8081       www            ✓
 node         1.0.0   ○ not installed
 dns          —       尚未发布
```
- 状态词：`running` / `stopped` / `failed` / `not installed` / `尚未发布`。`●` 绿、`○` 灰、`✗` 红，且带文字。

### 6.2 `lingedge doctor --all`
- 分组检查（系统环境、端口、依赖、服务、证书到期、配置有效性、宝塔共存），每项 `✓/⚠/✗` + 一句话 + 「修复建议」；末尾汇总 `通过 12 · 警告 2 · 失败 0`。
- 证书到期、单 NS DNS 冗余风险（P3）等以 **⚠ 警告** 出现，不使 `doctor` 失败，除非 `--strict`。
- **只读**：`doctor` 不修改系统；修复动作需另行执行并确认。

### 6.3 `lingedge node enroll`
```
 $ lingedge node enroll
  注册令牌（输入不回显，前缀 lenr1.；令牌内已含主控地址与 CA 指纹）: ********
  ✓ 已连接主控 https://admin.example.com:9444，CA 指纹 SHA256:…  请核对此指纹是否与 Admin「节点注册」页一致 [y/N]: y
  ✓ 节点身份证书已签发并保存（/etc/lingedge/node/identity，权限 0600）
  ✓ 首次心跳  ✓ 配置已同步  ● 节点在线
```
- **令牌用隐藏输入**，避免进入 shell 历史；也支持 `--token-file <文件>` 与 stdin（推荐，Admin 页面展示的命令即此形式）；命令行 `--token` 虽可用，但令牌会出现在 `ps` 与 shell 历史，打印风险提示且 Admin 页面不再展示该用法。节点已有身份需重新注册（如证书过期）使用 `--force-reenroll`。
- 首次连接展示**主控证书指纹**并要求确认（令牌内的 CA 指纹用于钉扎，目视核对防中间人），非交互时令牌自带指纹，无需另给 `--server-fingerprint`。`[ASSUMPTION]`

### 6.4 `uninstall`、`backup`
- `uninstall node`：列出将删除/保留的内容；**默认保留用户数据**，只有 `--purge` 才删除，且 `--purge` 需输入组件名确认（非交互需 `--yes --purge`）。
- `backup create`：显示备份范围（配置 + 数据库）、大小、校验和与保存路径；备份文件权限受限；`backup restore` 在 L3 强度确认（会覆盖当前数据），并先对当前状态做一次安全备份。

## 7. JSON 输出约定

> **以 `docs/architecture/tools-design.md` §5/§6 为唯一事实源**（2026-11-21，Story 1.25 起与实现一致；此前本节的 `schema_version`/单个 `error` 对象/`--stream` 写法已废止）。schema 文件：`internal/tools/cli/schema/`（`envelope.schema.json` + 每命令 `data.<命令>.schema.json`），CI 以快照测试校验真实输出。

### 7.1 通则
- 所有命令支持 `--json`：**stdout 仅输出一个 JSON 文档**；人类可读日志、警告、一次性提示与进度事件一律走 stderr（进度/提示为 NDJSON，`--progress=none` 可关闭）；`--json` 自动关闭色彩与交互（等价 `--non-interactive`）。`--json --help` 的帮助文本放在 `data.help`。
- 字段名 `snake_case`；时间用 RFC 3339 UTC（毫秒）；枚举值为稳定英文标识；**不本地化**；字段**只增不删不改义**（前向兼容），版本标识为 `schema: "lingedge.cli/v1"`。
- `ok == (exit_code == 0)`；`exit_code`/`exit_name` 取自 §4.2；失败时 `data` 为 `{}`，错误在 `errors[]`。

### 7.2 结果文档形状
```json
{
  "schema": "lingedge.cli/v1",
  "ok": true,
  "command": "status",
  "run_id": "01JD5Y3C8Q5G7M0V1N2P3R4S5T",
  "exit_code": 0,
  "exit_name": "OK",
  "started_at": "2026-11-16T14:03:11.000Z",
  "duration_ms": 25,
  "dry_run": false,
  "data": { "components": [ { "name": "server", "version": "1.0.0", "state": "running" } ] },
  "warnings": [ { "code": "cert.expiring", "message": "…" } ],
  "errors": []
}
```
失败：
```json
{
  "schema": "lingedge.cli/v1",
  "ok": false,
  "command": "install node",
  "run_id": "01JD5Y3C8Q5G7M0V1N2P3R4S5T",
  "exit_code": 4,
  "exit_name": "PORT_CONFLICT",
  "started_at": "2026-11-16T14:03:11.000Z",
  "duration_ms": 1830,
  "dry_run": false,
  "data": {},
  "warnings": [],
  "errors": [
    {
      "code": "port.conflict",
      "message": "TCP 80 已被进程 nginx(pid 1234) 占用",
      "step": 4,
      "hint": "释放端口或使用 --set 指定其他端口",
      "system_state": "unchanged",
      "details": { "port": 80, "proto": "tcp", "holder": "nginx", "pid": 1234 }
    }
  ]
}
```
- `errors[].code` 为稳定小写点分标识；`errors[].system_state` ∈ `unchanged | rolled_back | partial | needs_manual`（可选，对应 §4.2 的 9/10/11 语义；本 Story 交付的错误恒为 `unchanged`）。
- **绝不**在 JSON 中包含令牌、密码、私钥；envelope 在输出前对 `data/errors/warnings` 统一脱敏；需要输出的凭据类字段只给指纹/末四位。

### 7.3 进度事件（stderr，NDJSON）
`--json` 时 stderr 每行一个事件（`--progress=none` 关闭），便于脚本与 CI 跟踪长任务；结果仍只在 stdout 的 envelope 中：
```
{"type":"notice","code":"cli.non_interactive_auto","message":"…","ts":"2026-11-16T14:03:10.000Z"}
{"type":"step","step":13,"total":22,"name":"verify_package","state":"ok","ts":"2026-11-16T14:03:11.000Z"}
{"type":"progress","name":"download","bytes":13002342,"total_bytes":13002342}
```
- 事件类型 `notice` 已由 Story 1.25 交付；`step`/`progress` 随 installer/downloader Story 交付。

## 8. 非交互、任务锁与中断恢复

- **非交互判定**：stdin 非 TTY，或 `--non-interactive`，或 `CI=true`。此时不弹提示；缺少必要输入 → 退出码 `2`；缺少必要确认 → 退出码 `9`。
- **确认参数**：`--yes`（通用确认，**不**覆盖不可逆迁移与 `--purge` 这类需要专用开关的危险项）、`--accept-irreversible`、`--purge`（需 `--confirm-name`）。
- **任务锁**：同一目标并发执行第二个安装/升级 → 立即失败，退出码 `6`（LOCKED），输出「已有任务在运行（pid …，开始于 …，命令 …）」与 `lingedge status --tasks` 的查看方式；**陈旧锁**（进程已不存在）自动识别并提示「检测到上次异常退出的任务，可继续或回滚」，不要求手工删锁文件。
- **中断（Ctrl+C）**：只在**安全点**停止——正在下载则丢弃临时文件；正在写入系统文件则完成当前原子步骤后停止；提示「已安全中止，可再次运行以继续或 `lingedge rollback` 回滚」。第二次 Ctrl+C 才强制退出并明确警告可能的残留。
- **失败恢复**（FR-4 可验证项）：安装中途被 kill 后再次运行：显示「上次安装在步骤 18 中断」+ `[c] 继续  [b] 回滚  [q] 退出`，且**不破坏既有数据**。
- **日志**：默认写入 `/var/log/lingedge/tools/<task>-<时间>.log`（脱敏）；`-v/--verbose` 增加屏幕细节；`--log-file` 自定义；日志路径始终在结束输出中给出。

## 9. 可访问性与终端兼容

- **`--plain`**：关闭 TUI，使用纯文本行式交互（编号选择：`请输入要安装的组件编号，逗号分隔`）；读屏器与不支持光标控制的终端使用。检测到 `TERM=dumb` 或无 TTY 时自动启用。
- **不仅靠颜色**；符号有 ASCII 回退（`[x]`/`[ ]`、`OK`/`FAIL`/`WARN`），`--ascii` 或 `LANG` 非 UTF-8 时启用。
- 键盘全部可达；鼠标仅为可选增强。
- 宽度自适应：最小 60 列；进度表在窄屏下折叠为单列。
- 无动画依赖：进度的每次状态变化都是一行可读的文本（读屏友好）。
- 颜色遵守 `NO_COLOR`；高对比：使用终端 16 色基础调色板，不依赖 256 色或真彩色。

## 10. 基线覆盖对照

| 基线要求 | 本文位置 |
|---|---|
| §4.3 交互菜单 11 项 + 退出、多选复选框 | §2、§3.1 |
| §4.4 交互/无交互、退出码、JSON、详细日志、参数验证、任务锁、进度、失败恢复 | §4、§7、§8、§3.3 |
| §4.5 22 步安装 | §3.3 |
| §4.6 宝塔兼容、不要求卸载、独立 OpenResty、明示端口与归属 | §3.2 |
| §4.7 升级流程与不可逆迁移 | §5 |
| §4.8 Token/密码不进日志、不默认删数据、安全卸载 | §1、§4.1、§6.4、§7.2 |
| §21.1 `install.lock`、无默认管理员密码 | §3.4、`screens.md` §9 |
| D-16 Scheduler/Worker 内置 | §3.1、退出码 8 |
