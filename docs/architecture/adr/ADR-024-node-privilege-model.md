# ADR-024 Node 权限模型：非 root 运行、80/443 绑定与受限特权升级路径

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §4.6、§4.8、§10.2、§24；PRD FR-12、FR-58、FR-60、FR-62、NFR-14；architecture AD-24、F-9；[tools-design §15](../tools-design.md)｜阶段：M1

## 背景

FR-12 要求 Agent「以非 root 最小权限运行（受限于需要绑定的端口，机制由架构定）」。节点需要：绑定 80/443、在主控配置变更时**热重载** OpenResty、进程崩溃自愈、并支持经 Admin 触发的**远程升级**（FR-60）——而升级必须替换二进制并重启服务，这是特权操作。同时基线 §24 要求不得把节点控制权限暴露给租户，§4.8 要求最小权限与禁止未验证发行包。

## 决策

1. **运行用户**：`lingedge-node`（系统用户，无登录 shell，家目录无）运行 **Agent 与 OpenResty 两个 systemd 服务**；Server 用 `lingedge`；官网 SSR 用 `lingedge-web`；DNS 用 `lingedge-dns`。各用户目录互不可读（配置 0640/0600）。
2. **80/443 绑定**：`lingedge-openresty.service` 以 `User=lingedge-node` 运行，`AmbientCapabilities=CAP_NET_BIND_SERVICE` 且 `CapabilityBoundingSet=CAP_NET_BIND_SERVICE`——master 与 worker 都不是 root，仅有绑定低端口能力；`NoNewPrivileges=yes`。**不采用**全局 sysctl `net.ipv4.ip_unprivileged_port_start`（影响整机其他进程，与宝塔共存时不可接受）。UDP 443 不监听（D-7）。
3. **Agent 自身零能力**：`lingedge-node.service` `CapabilityBoundingSet=` 为空、`NoNewPrivileges=yes`、`ProtectSystem=strict`、`ReadWritePaths` 仅 `/var/lib/lingedge-node /var/log/lingedge-node /run/lingedge-node`（`cache/`、`runtime/`、`staging/`、`generations/` 均在 `/var/lib/lingedge-node` 内，目录总表见 ADR-020 §2）、`RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK`（Go `net.Interfaces()` 枚举本机地址需要 netlink）。**Agent 与 OpenResty 两个单元均不设 `MemoryDenyWriteExecute`**：MDWE 是 seccomp 过滤，会被 Agent 的子进程继承，而 Agent 要执行 `openresty -t`（LuaJIT/PCRE-JIT 需可写可执行内存）；若 Agent 带 MDWE，`-t` 会误报失败，或使“测试通过”所验证的运行模式（无 JIT）异于生产（有 JIT），削弱 FR-55 的“`nginx -t` 失败即保持旧版本”保证。Agent 是纯 Go 进程、无 JIT，放弃 MDWE 的代价有限；其余加固项保留（tools-design §15.2 单元文本为准）。
4. **重载与日志轮转**：OpenResty 的 master 由同一用户运行，`nginx.conf` 指定 `pid /run/lingedge-node/openresty.pid`；`-p/-c` 在预检与生产统一：预检 `-t -p …/generations/g<N>/ -c conf/nginx.conf`，生产（systemd `ExecStartPre/ExecStart`）`-p /var/lib/lingedge-node/current/ -c conf/nginx.conf`，可写路径一律绝对路径（ADR-020 §3）。Agent 读取 PID 文件向 master 发信号：`HUP`（热重载，前提 `openresty -t` 已通过）、`USR1`（重新打开日志，ADR-008）。**Agent 无需、也无法调用 `systemctl`**。`ExecReload=/bin/kill -HUP $MAINPID` 供管理员手工使用。
5. **自愈**：OpenResty 进程异常退出由 systemd `Restart=always`（`StartLimitIntervalSec=0`）拉起；Agent 监控进程与配置状态并上报事件（FR-62）。OpenResty 的 `ExecStartPre` 先 `-t` 校验当前代，配置不合法则拒绝启动，不依赖 Agent 在线（ADR-020）。Agent 自身亦 `Restart=always`。
6. **首次启动**：无配置代时不启动 OpenResty；`lingedge-openresty.path`（`PathExists=…/current/conf/nginx.conf`）在首个配置代出现时触发 service（tools-design §15.2）。
7. **远程升级的特权路径（FR-60，F-9；BLK-07/H8 重写）**：
   1. 收到 `NodeUpgrade` 任务后，Agent（非 root）将升级清单与包**下载并先行验证**到 `/var/lib/lingedge-node/staging/<component>-<version>/{manifest.json,manifest.json.sig,package.tar.gz}`（属 `lingedge-node`，**不可信域**），再写 `staging/upgrade.request`。
   2. **请求文件只含四个字段**：`{"schema":1,"component":"node|openresty","version":"x.y.z","task_uid":"<ULID>"}`（≤4 KiB）。**没有路径、没有 `staged_files[]`**：暂存文件的路径由 root 侧按 `component+version` 固定推导。`component` 只允许 `node` 或 `openresty`（proto `NodeUpgrade.component` 注释、OpenAPI `NodeUpgradeRequest.component`、threat-model §6.8 同步，不再写“固定 node”）。
   3. `lingedge-node-upgrade.path`（`PathExists=…/staging/upgrade.request`）触发 `lingedge-node-upgrade.service`（`Type=oneshot`，`User=root`，带加固项），执行 `/usr/local/bin/lingedge update node --apply-staged --non-interactive --json`——**即 Tools 的受限入口，不是独立 updater**（基线 §2.6）。
   4. root 侧**完全不信任 Agent 的校验**：对 staging 目录以 `O_DIRECTORY|O_NOFOLLOW` 取 dirfd，之后全部经 `openat(dirfd, name, O_NOFOLLOW)` 读取；每个文件 `fstat` 须为 `S_ISREG` 且 `st_nlink==1`（拒绝符号链接与硬链接）；**复制到 root 私有目录**后再用 Tools 内置 `keys.json` **重新**验证清单签名、sequence、`valid_until`、吊销、包 SHA256 与包内签名；版本须满足 `min_upgrade_from` 且严格高于当前（或显式回滚标记且存在于 `releases/`）；请求文件读取后 `unlinkat`，成败均删，防循环触发。
   5. 随后走标准升级流程（备份→原子切换 `current`→重启服务→健康检查→失败自动回滚，ADR-013）。
   6. **结果文件写入 root 属主目录**：`/var/lib/lingedge-node-upgrade/`（`root:root 0755`，由 Tools 安装 node 时创建），文件 `<task_uid>.result.json`（`root:root 0644`），先 `unlinkat` 同名旧文件，再以 `O_CREAT|O_EXCL|O_NOFOLLOW` 创建；Agent 只读它（或经 `lingedge status --json`）并经 `ReportTask` 上报。**root 进程不再向任何 Agent 属主目录写入文件**，因此被攻破的 Agent 无法借预置符号链接/硬链接诱使 root 覆盖任意文件。
   7. 因此远程升级要求节点上已安装 Tools（`/usr/local/bin/lingedge`）。
8. **无任意命令执行**：Server 无法让节点执行任意命令；`TaskService` 的任务类型为白名单枚举，Agent 仅实现这些动作（FR-35、NFR-14）。`DebugRequest` 等任务只能发起受限的本机回环请求。
9. **文件权限**：证书私钥、`identity/` 0600；配置渲染产物 0640（OpenResty 同用户可读）；`/run/lingedge-node/openresty-ctl.sock`（Agent↔OpenResty 控制 socket）仅该用户可访问，不开 TCP。
10. **Tools 的特权边界**：安装/升级/卸载需要 root（创建用户、写 systemd 单元与 `/opt`）；`list/status/doctor` 可由普通用户运行（读 `state.json`，不含密钥）。

## Binds / Prevents / Rule

- 约束：tools-design 的 unit 模板、Agent 的重载/升级实现、安装器的用户与目录创建、安全审计。
- 防止：Agent 被攻破即获得 root；以 sysctl 放宽全机低端口限制；Agent 信任自己下载的包导致供应链绕过；staging 目录的符号链接/TOCTOU 提权。
- 规则（AD-24）：**Node 全部进程非 root；特权操作仅经 systemd 单元与 Tools 的受限入口，且特权侧重新验证一切。**

## 备选方案与权衡

- **OpenResty 以 root 启动再降权（master root，worker nobody）**：否决——master 常驻 root 扩大攻击面；`AmbientCapabilities` 已能满足绑定需求。
- **给 Agent `sudo`/polkit 以调用 `systemctl restart`**：否决——引入可被滥用的特权通道；path 单元 + root oneshot 的输入面更小且可审计。
- **独立 `lingedge-updater` 守护进程**：否决——基线明确不再开发独立 updater（§2.6）。
- **Agent 直接 setuid 升级器/自替换二进制**：否决——运行用户无权写 `/opt/lingedge`，且自替换难以回滚。

## 后果

- 低端口绑定依赖 systemd Ambient 能力，需要较新的 systemd（版本要求实现时核对）；不使用 systemd 的发行版不在支持范围（基线要求 systemd）。
- 升级路径多一层 root 侧重新验证，实现与测试更重，但换取更小信任面。
- 远程升级依赖 Tools 已安装，须在节点安装流程中保证。

## 可被推翻处

若用户接受 Agent 以 root 运行：可简化升级（直接升级），但违背 FR-12 与最小权限；应同时在威胁模型中扩大 Agent 被攻破的爆炸半径评估。若某发行版的 systemd 不支持 Ambient 能力，可退而使用 `setcap cap_net_bind_service` 于独立 OpenResty 二进制（文件能力），影响 tools-design 的安装器。

## 验证

- 安装后检查：进程用户非 root、`CapEff` 仅含 `CAP_NET_BIND_SERVICE`（OpenResty）/ 空（Agent）；`doctor` 输出端口、目录、进程归属（FR-58）。
- 升级：构造 staging 中含符号链接/`../`/非白名单 component/篡改签名的请求，root 侧一律拒绝且无越界写入；失败升级自动回滚（AC-J-6）。
- 升级（补充）：staging 中把 `package.tar.gz` 换成指向 `/etc/shadow` 的符号链接或硬链接（`nlink>1`）、在 `/var/lib/lingedge-node-upgrade/` 之外预置结果文件符号链接——root 侧拒绝，且无任何写入落在 Agent 属主目录；请求含 `staged_files`/路径字段或 `component` 不在白名单 → 拒绝。
- 在 Agent 单元（无 MDWE）内执行 `openresty -t`，其结果须与 OpenResty 单元启动前检查一致（IMP-20）。
- 重载不中断存量连接（持续并发探测）；kill OpenResty 后被自动拉起（FR-62）。
- 上述需在 Linux + systemd 验收机验证，缺环境标「等待环境验证」（ADR-023）。

## 修订记录 2026-11-16

- **BLK-07 / H8（升级特权路径）**：§7 重写。原设计中 root 进程把 `upgrade.result` 写进 Agent 属主目录（0644），而只对“读取请求文件”做了 `O_NOFOLLOW`，被攻破的 Agent 可预置符号链接/硬链接诱使 root 覆盖任意文件；三处文档对请求内容与 component 的描述互相矛盾（threat-model“仅版本号”、ADR/tools“component+staged_files[]”、proto“固定 node”）。现统一：请求仅 `component+version+task_uid`，暂存路径由 root 固定推导，读取经 dirfd+`O_NOFOLLOW`+`nlink==1`，结果文件写 root 属主目录 `/var/lib/lingedge-node-upgrade/`（`O_EXCL`）；`component ∈ {node, openresty}`。
- **IMP-20**：§3 Agent 单元去掉 `MemoryDenyWriteExecute`（会被子进程 `openresty -t` 继承）并说明理由；L3 验收增“在 Agent 单元内执行 `openresty -t` 与生产启动结果一致”（待 Linux 验收机，标「等待环境验证」）。
- **L24**：§3 `RestrictAddressFamilies` 增 `AF_NETLINK`（Go `net.Interfaces()` 需要，否则 `reported_ips` 失败）。
- **BLK-04 / H4**：§4 补充 `-p/-c` 统一与目录总表引用（ADR-020 §2/§3）。
