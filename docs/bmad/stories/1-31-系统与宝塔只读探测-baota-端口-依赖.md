---
title: 'Story 1.31：系统与宝塔只读探测（baota / 端口 / 依赖）'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'fa27781'
waiting_env_verification: true
context:
  - docs/bmad/epics.md Story 1.31（AC1–AC5）
  - docs/architecture/tools-design.md §10.4、§10.5（resolver.System 契约）、§11（新增 §11.4）、docs/bmad/decisions-adopted.md（D-9、D-14、UD-18）、docs/prd/LINGEDGE_V2_REQUIREMENTS.md（FR-6、FR-7）
  - docs/bmad/stories/1-26-*、1-29-*、1-30-*（safeexec、registry 的 state.json、resolver.System）
---

## Intent

**Problem:** 1.30 的 resolver 只消费 `System` 输入，真正的采集没人做：安装前不知道本机 OS/内存/磁盘、谁占着 80/443/53、有没有宝塔与既有 Nginx/MySQL/Redis；MySQL/Redis 是否可达与版本也无从得知；doctor（1.42）将来也需要同一份结果。

**Approach:** 新增两个包。`internal/tools/baota`（叶子包，只依赖 `security`）对一个文件系统视图做**只读**探测，产出可序列化的 `baota.Report`；`internal/tools/probe` 在其上增加 `ss`/`/proc` 监听采集、内存/磁盘、MySQL/Redis/Node.js 探测、系统服务识别与冲突判定，产出 `probe.Result`，其中 `Result.System` **就是** `resolver.System`（字段一一对应，没有另造一套）。`registry` 的 `state.json` 升到 schema 2 以容纳 `host.baota`，写入走既有原子写。**没有新增迁移（000001–000018 不变）、没有 OpenAPI/proto 变更、没有改 CF-6 冻结的 schema、没有新增依赖。** 不接线 1.33 的编排，不实现 1.42 的 doctor。公共 API 与规则的完整说明见 tools-design §11.4。

## 逐条验收

- **AC1 OS/架构/内存/磁盘与端口占用**：`runtime.GOOS/GOARCH`；Linux 内存取 `/proc/meminfo`（`ParseMeminfo`：`MemAvailable`，旧内核回退 `MemFree+Buffers+Cached` 并注明近似）、磁盘取 `/proc/self/mountinfo`（`ParseMountinfo`：`\040` 转义、`ro`（挂载选项或超级块选项）、伪文件系统过滤、同挂载点后者覆盖）+ `statfs`（`f_bavail×f_frsize`）。端口：`ss -H -lntup`（参数数组，经 safeexec）→ `ParseSS`（IPv4/IPv6/`*` 通配/回环、`127.0.0.53%lo` 接口后缀、`users:((…),…)` 多进程取最小 pid、进程名含空格/引号/逗号/括号/`\xNN`、无权限时无 users 列 → `Hidden`）；`ss` 不可用/失败回退 `ParseProcNet`（`/proc/net/{tcp,tcp6,udp,udp6}`：十六进制地址按**主机字节序**解码、IPv6 四个 32 位字、端口大端、TCP `0A`、UDP `07` 且远端全零）+ `/proc/<pid>/fd` 的 `socket:[inode]` 反查（同 inode 的 master/worker 全部列出）；两条路径都用 `/proc/<pid>/status`/`/etc/passwd` 补属主、`/proc/<pid>/cgroup` 补单元；无权限读他人 fd 时占用者 pid/name 不可见（`Hidden`，属主仍取 /proc/net 的 uid）并有降级说明。同地址/通配冲突由 `probe.Assess` 判定，语义复用 resolver 的 `AddrConflict`（`resolver/addr.go` 新增导出封装），同一端口的多个监听合并为一条冲突并列出占用者 pid/name/属主。**`ss` 与 `/proc` 都不可用时 `ListenersKnown=false` 且有警告，`Assess` 对每个端口返回「无法检测」——不会静默当作无冲突。**
- **AC2 宝塔与既有环境**：三类证据（`/www/server/panel`、`bt` 命令 `/usr/bin/bt|/usr/local/bin/bt|/etc/init.d/bt`、`data/port.pl`）**任两类**才 `detected`（三类 `high`、两类 `medium`），单类只列证据并说明不判定，`port.pl` 内容非法不计证据；输出置信度与证据列表、版本与 `version_source`、面板端口。识别 Nginx/OpenResty（`-v` 输出为准）、MySQL/MariaDB/Redis（仅识别：存在、版本、数据目录路径）、PHP（`/www/server/php/<NN>` → `N.N`，仅记录）、站点 vhost（只读解析 `server_name`/`listen`/ssl/`default_server`，含注释、引号、嵌套块、多 listen、IPv6、`unix:`、畸形配置标 `malformed` 且不 panic；`sites-enabled` 符号链接只在目标位于 `/etc/nginx/` 下时读取）、防火墙（ufw/firewalld，活跃状态由 systemctl 补全）、WAF/插件目录、系统服务（named/bind9/systemd-resolved/dnsmasq/nginx/mysqld/redis/防火墙，`ParseSystemctlUnits`）。结果经 `registry.RecordHostBaota` 写入 `state.json.host.baota`。
- **AC3 不阻止安装，仅真实冲突阻止**：`Assess` 只在端口/路径（既有且不属于 Tools 记录、或位于宝塔 `/www` 之下）/同名站点（精确、`*.x`、`.x`；正则与 `_` 不静态比较以免误报）冲突时给出冲突；宝塔存在本身不产生任何冲突（`TestAssessBaotaAlone…`）。不要求卸载宝塔；**不读取宝塔存储的 MySQL root 密码**（见下「只读证明」）。
- **AC4 MySQL/Redis 仅存在性/版本/连通性，不代装**：免凭据时 MySQL 只读服务器握手包（不发送任何认证数据；用真实 mysqld 捕获的握手包做夹具），Redis 只发 `PING`/（无口令时）`INFO server`；调用方提供 `Creds` 才认证：MySQL 经 safeexec 执行 `mysql` 客户端，**只执行 `SELECT VERSION()`**（口令经 `MYSQL_PWD`，不进 argv），Redis `AUTH`+`INFO server` 且口令只发往环回。不可达只警告并引导 Server 向导（`dep.mysql|redis.unreachable|absent|auth_required`），宝塔环境的提示明确「不读取宝塔存储的 MySQL root 密码」。
- **AC5 可被 doctor 与 resolver 复用**：`Result.System` 是 `resolver.System`（`TestSystemFeedsResolver`：探测结果直接喂 `resolver.Resolve`，宝塔存在不阻断、MySQL/Redis 版本满足、真实端口冲突阻断并带占用者）；`Result`/`Report` 全部可 JSON 往返（`TestCollectEndToEnd`、`TestRecordHostBaotaRoundTrip`）；`Result.Sockets`/`Deps`/`Baota` 与 `Assess` 是 doctor 的复用入口。

## 只读证明（测试）

- **不打开口令文件**：测试把「金丝雀」口令文件（`default.pl`、`default.db`、`other.db`、`mysql_root.pl`、`redis.conf`、`my.cnf`、`*.pem`、`root_pass_hint`…）写进夹具的**临时副本**（仓库里没有任何固定口令），用记录式 `FS`（包装真实 `OSFS`，只记录访问）探测：断言没有任何读取指向这些文件、序列化结果不含金丝雀串，且直接读取每一个都被 `readGuard` 拒绝；非敏感文件仍可读（防 guard 误伤）。
- **不写入**：探测前后对整棵文件树做快照（路径/模式/mtime/内容）逐项比较。`OSFS` 不跟随符号链接（`TestOSFSDoesNotFollowSymlinks`、`TestVhostSymlinkOutsideNginxIsNotFollowed`）。（1.31b：vhost 的 `include` 经 `Includer` 受控跟随，规则见 1.31b 与 tools-design §11.4。）
- **只读命令**：`readOnlyRun` 的 argv 表（`systemctl stop/restart/daemon-reload`、`ss -K`、`nginx -s stop`、`mysqld --initialize`、改写 mysql 语句/追加 `-p` 口令等全部在 safeexec 之前被拒绝）；`Collect` 经脚本化的外部程序验证 `ss`/`systemctl` 各只被以 `-H -lntup`/`list-units …` 调用一次。服务器端证明：真实临时 MySQL 的 `general_log` 里探测用户只执行了 `SELECT VERSION()`（至多外加客户端自带的只读 `select @@version_comment`），真实临时 Redis 的 `INFO commandstats` 增量只含 `auth`/`info`（`ping` 在 NOAUTH 下计入 rejected）。口令（正确/错误）不出现在任何结果、错误、警告里；向非环回地址的 Redis 端点即使被要求 `AUTH` 也不发送口令。

## 测试（真实、无 Mock）

- 解析器全部用夹具驱动。**真实采集（本机）**：`tests/fixtures/hostprobe/real/*`（真实 `nginx -v`、`mysqld --version`、`mysql --version`、`redis-server --version`、`redis-cli INFO server`、`node --version`、真实 mysqld 握手包十六进制）；真实二进制经 safeexec 执行并与直接运行同一程序的输出比对（`TestVersionExecRealBinaries`）；真实临时 MySQL（`tests/mysqltest`）与临时 Redis（`tests/redistest`，含 requirepass 实例）做版本/连通性/认证/错误口令/不可达/非协议端口/沉默服务器（超时）全覆盖，这类集成测试放在 `tests/integration/hostprobe/`（`make gate-selftest` 会把 `internal/` 拷成独立模块跑 lint，`internal/` 下的测试不得 import `tests/`，与仓库既有做法一致）；darwin 上真实内存/磁盘采集。**按真实格式构造、待 ENV-LNX/ENV-BT 复验**：`ss` 输出、`/proc/net/*`、`/proc/<pid>/*`、`meminfo`、`mountinfo`、`systemctl list-units`、宝塔目录树与 vhost 样本——来源与格式依据见 `tests/fixtures/hostprobe/README.md`（逐项标注）。
- **唯一的替身**：`ss`/`systemctl` 在 macOS 上不存在，`collect_test.go` 在临时目录放两个 shell 脚本（记录 argv 并输出夹具文本）作为这两个**外部程序**的替身，仅用于验证 Collect→safeexec 的接线与 argv；解析本身用的是夹具文本。另有 `stubExec`（baota 包）只验证「以 `-v` 输出为准切换 nginx/openresty」与「Exec 失败回退到文件证据」，`listenAnd`/非环回监听器是「协议不对/沉默/拒绝发送口令」这类真实服务器不会出现的对端。
- **fuzz**：`FuzzParseSS`、`FuzzParseProcNet`、`FuzzParseVhost`（各 15–20 秒、数百万次执行无失败；`FuzzParseState` 增加 host 种子）。
- **跨平台**：Linux 专属采集（`/proc/meminfo`、`mountinfo`+`statfs`、默认 `/proc` 根）在 `platform_linux.go`；`platform_linux_test.go`（真实 /proc 监听识别本进程）在本机仅编译（`go vet GOOS=linux amd64/arm64`），执行属 ENV-LNX；darwin/其他平台有明确降级实现，`linux/amd64`、`linux/arm64` 交叉编译通过。

## 变异验证（临时改动 → 测试必须失败 → 还原）

脚本逐条最小改动（保持可编译）→ 运行对应测试 → 立刻还原并逐字节比对（脚本内字节与 sha256 双校验，无还原失败）。72 条，**70 条检出**：宝塔判定（要求三类/单类判定/high 降级/port.pl 范围）、`readGuard` 失效/各层规则（`.db` 后缀、redis.conf）、探测偷读 default.pl、OSFS 跟随符号链接、vhost 符号链接目标不限、注释/ssl/default_server/IPv6/缺分号/server 数与嵌套上限、Validate 泄漏检查/Scrub、PHP 版本映射、WAF 目录、openresty 判别；`ss`（主进程取值、Hidden、`\xNN`、接口后缀、非监听状态）、`/proc/net`（字节序、端口序、UDP 远端、TCP 状态）、fd 映射降级、属主回落、cgroup 单元、meminfo 回退、mountinfo（伪文件系统/转义/ro）、只读 argv 表（含 systemctl 放行 stop、mysql 语句可替换、口令进 argv、用户名不校验）、服务识别（not-found/失败标记）、握手包（前缀/协议版本）、Redis（口令发往非环回、白名单放行 SET、错误口令泄露）、sanitize、MySQL 认证语句改写、`ListenersKnown` 恒真、不回退 `/proc`、ss 结果不补全、同端口不合并、宝塔 nginx 路径映射、不可达不警告、`Assess`（未知不保守、/www 规则、既有路径、OwnedByUs、通配域名、正则名、协议归一、地址冲突恒假）、registry（v1 含 host、host 不校验、不 Scrub、不升到 2）。
- **未检出 2 条（等价变异，如实披露）**：`buildSite` 里对 `location` 块的 `continue`——循环只遍历 server 块的直接子指令，`location` 自身的名字本就不是 listen/server_name，该分支是纵深防御；`socketOwners` 的 `!want[ino]` 过滤——只影响内存占用，结果 map 只会被「想要的」inode 读取。
- 变异过程暴露并**修正了测试自己的弱点**（均已补强再验证）：`.db` 后缀与 `default.db` 两层规则互相掩盖（加入只被后缀规则覆盖的 `other.db` 与只被子串规则覆盖的 `root_pass_hint` 金丝雀）；符号链接测试的目标在探测根之外所以「跟随也读不到」（改为目标在根内的 `/srv/evil.conf`）；深嵌套用例被 token 上限先拦住（降到 5 万层）；UDP 远端检查、`ro` 的两处来源、MySQL 协议版本 9、正则 server_name、`tcp6` 的我方协议归一各补了针对性用例。

## 对既有测试/防护的改动（逐条）

- **没有修改任何门禁规则、白名单或 lint 配置**（depguard 禁止 `database/sql`/MySQL 驱动出现在 repository 之外，所以认证探测走 safeexec 的 `mysql` 客户端；`scaffold` 的容器运行时扫描命中了我自己夹具里的字眼，改的是夹具而不是门禁）。safeexec 的 `DefaultAllowed` 未改：探测用 `probe.NewRunner` 构造自己的 Runner（白名单见 tools-design §11.4）。
- **`state.json` schema 1 → 2（版本规则要求）带来 4 处既有测试的必要改动**，全部是把「未来版本」从 2 改为 3，语义不变：`internal/tools/registry/state_test.go`（`TestParseStateSchemaVersioning` 的未来版本用例、`TestUnsupportedFutureVersionIsNeverRewritten` 的文件内容 `schema_version:2` → `3`；`TestStateRoundTripPermsAndMissing` 的 `SchemaVersion != 1` → `!= StateSchemaVersion`）、`internal/tools/cli/registry_test.go`（`TestStateErrorsMapToExitCodes` 的「未来 schema」文件 `2` → `3`）、`internal/tools/registry/fuzz_test.go`（种子 `2` → `3`，另加版本 2 的种子与带 host 的种子）。其余既有 state 测试（版本 1 的各种非法/损坏用例）原样通过，证明版本 1 仍可读取。
- 生产代码的既有文件改动：`registry/state.go`（版本 2、`Host`、迁移 1→2、`RecordHostBaota`）、`resolver/addr.go`（仅新增导出封装 `AddrConflict`/`NormProto`，`Resolve` 行为不变，`resolver` 全部测试原样通过）。`internal/tools/baota/doc.go` 由占位改为正式包说明。

## 与任务/通常做法不同的设计决定

- **`probe` 与 `baota` 分两个包**：`registry` 要持有 `baota.Report`（`state.json.host.baota`），而 `probe` 要 import `resolver`（→ `registry`）；`baota` 若 import `resolver` 会成环，所以 `baota` 做叶子包，`resolver.System` 的映射放在 `probe`。
- **MySQL 认证探测用 `mysql` 客户端而非驱动**：depguard 禁止 Tools 以外 import 驱动，也不应放宽。代价：认证探测依赖 `mysql` 客户端存在；缺失时退回免凭据握手并在 `Error` 说明。口令走 `MYSQL_PWD`（8.0 弃用但仍支持，只对本用户/root 可见），不走 `-p`（ps 可见）。
- **Redis 用手写 RESP（只 `PING`/`AUTH`/`INFO server`）而非 `redis-cli`**：命令集在发送前强制只读；没有客户端依赖；`redis-cli` 的输出样本仍作为解析夹具。
- **免凭据 MySQL 探测的副作用**：握手后不认证即断开会计入服务器 `Aborted_connects`/`max_connect_errors`，所以每次 `Collect` 每个端点只探一次（已在文档说明）。
- **`schema_version` 升到 2 而不是在 v1 里追加字段**：版本规则明令「同版本内不允许新增字段」。后果：已发布的 1.29 程序读 v2 会失败关闭（预期）。
- **`Assess` 对同一端口的多个监听合并为一条冲突**（`0.0.0.0` 与 `::` 各一个 socket 很常见），`details.occupants` 列全部；resolver 对每个监听地址各出一条阻断是其既有行为，未改。
- **cli 未展示探测结果**：任务里「如需展示」——`status` 的 data schema 严格且 `StateSource` 接口被既有测试注入，展示 host.baota 属 doctor（1.42）的范围，避免越界。
- **`ss` 的 `*` 通配保持原文**（resolver 已把 `*` 视为全通配，保守）。

## 门禁结果

见提交说明与交付报告（`gofmt -l .` 无输出；`make lint security gate-selftest race vuln contract-check build verify-build migrate-test` 与 linux/amd64、arm64 交叉编译）。

## 遗留与风险

- **代码已完成、待环境验证（ENV-LNX / ENV-BT，Story 1.45）**：真实 Linux 上的 `ss -H -lntup` 与 `/proc/net/*`、`/proc/<pid>/fd` 输出与夹具的一致性（夹具是按真实格式构造）；`/proc/meminfo`、`mountinfo`、`statfs` 的真实值与 `platform_linux_test.go`（本机只编译）；真实 `systemctl list-units` 输出；cgroup v1/v2 单元推导；**真实宝塔面板**：目录布局、`port.pl`、`bt` 命令位置、面板版本来源文件（`class/common.py` 的 `version`、`config/version.pl`）与各 `version.pl`、vhost 生成样式、WAF/插件目录名；真实 Linux 机上的 MySQL/Redis 版本探测（`mysqld --version` 等）与非 root 运行时的降级行为。
- **属后续 Story**：1.33 编排接线（调用 `Collect`、展示 Plan、在预检里处理 `ListenersKnown=false` 与 `Assess` 的结果、把 `RecordHostBaota` 放进安装步骤）；1.42 doctor（复用 `Result`/`Report`，展示 `host.baota`、对 `state.drift` 等）；Server 向导（1.39/向导族）负责 MySQL/Redis 的配置引导与凭据收集——本 Story 只提供 `Creds` 入口与只读探测；`status`/`list` 展示 host 信息（归 1.42）。
- **已在 1.31b 处理（见 `docs/bmad/stories/1-31b-遗留项清理.md`）**：站点 vhost 的 `include` 受控跟随、MySQL/Redis unix socket 端点探测、nvm/fnm/volta/n/asdf/宝塔 Node 项目管理器等非固定目录的 Node.js 识别。
- **已知限制**：`Result.System.Listeners` 对 `*`/`0.0.0.0`/`::` 的双栈语义沿用 resolver 的保守处理（`ss` 不给 `IPV6_V6ONLY`）。（原「站点解析不处理 `include`」「MySQL 的 unix socket 端点不探测」两项已在 1.31b 解除。）
