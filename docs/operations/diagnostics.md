# 诊断与日常运维：doctor / config / service / logs / status / list（Story 1.42）

> 面向运营者的手册。设计口径与逐条决定见 `docs/architecture/tools-design.md` §19.1；退出码见 `lingedge --help exit-codes`。所有命令都支持 `--json`（stdout 仅一个 `lingedge.cli/v1` 文档，有失败项时 `data` 仍在其中）。

## 1. 看状态：`status` / `service status` / `list`

| 命令 | 内容 | 退出码 |
|---|---|---|
| `lingedge status` | 每个已装组件的**真实 systemd 状态**（`systemctl show`）+ **健康探测**；`healthy` 为 `true/false/null`（信息不足） | 有不健康组件 → **11** |
| `lingedge service status [--all\|<组件>]` | 同上，限定组件 | 同上 |
| `lingedge list [--available [--channel stable\|beta]]` | 版本/状态/端口/配置目录/运行用户 + **进程归属**（端口上的真实监听者：pid/进程名/用户/是否属于本组件）；静态前端显示站点域名与 conf.d 软链状态；`--available` 另列出经验签的清单里的可装组件 | 无发行源 → 3；验签失败 → 5 |

健康探测：`server` = `GET /healthz`（Server 向导完成后加 `/healthz/ready`；向导阶段只要求存活）；静态前端 = 发布物与站点片段的深度检查（见下）；`node` 目前只有 systemd 状态（Agent 探针随 Epic 3）。

## 2. 诊断：`doctor [--all|<组件>] [--strict] [--fix-perms]`

输出每一项 `pass|warn|fail|skip`（稳定的 `id/category/message/hint`）。有 `fail` → 退出码 **3**；`--strict` 把 `warn` 视同 `fail`。没有 root 读不了的东西（journal、备份目录、配置、主密钥）报告 `skip` 并说明需要 root，**不猜测**。

| 类别 | 检查项 | 要点 |
|---|---|---|
| 系统 | `sys.os` `sys.arch` `sys.mem` `sys.disk` `sys.ulimit_nofile` `sys.time_sync` | 内存/磁盘优先用**清单声明的要求**（能获取清单时，低于要求 = fail），否则用 `tools.yaml` 的 `doctor.min_memory_mb/min_disk_mb`（缺省 256/512，**[ASSUMPTION]**）；`sys.time_sync` 见下 |
| 端口 | `port.<组件>.<端口>` | 端口被谁占用（pid/进程/用户）；ss 与 /proc 都不可用时不当作「无冲突」；53 端口给出 systemd-resolved 提示 |
| 宝塔/共存 | `baota.nginx_conflict` `baota.firewall` | 80/443 真实占用才是冲突（宝塔存在本身不是）；防火墙只读判定 ufw/firewalld 配置文件，云安全组需自行确认 |
| 依赖 | `dep.mysql` `dep.mysql.settings` `dep.mysql.pmax` `dep.redis` `dep.nodejs` `dep.openresty` | 版本来自真实握手；`dep.mysql.settings` 用迁移账号查询 utf8mb4/严格 sql_mode，`dep.mysql.pmax` 检查分区表的 pmax 兜底分区是否非空（预建分区没跟上写入，无凭据 = skip）；Redis 不可达是 warn（降级运行） |
| 服务 | `svc.<组件>.active\|enabled\|restarts`、`service.systemd_version\|credential_mode\|unit_new_pending` | NRestarts ≥ 3 warn（`doctor.restarts_warn`），`auto-restart` 循环 fail |
| 健康 | `health.server` `health.node` | `health.node` 如实 `skip`（Story 3.31） |
| 文件与权限 | `fs.master_key_perm` `fs.config_perm` `fs.identity_perm` `fs.release_owner.<c>` `fs.dirs` `fs.state_dir` | release 目录不得被运行用户/other 写入；状态目录位于网络文件系统 → warn |
| 静态前端 | `fe.<c>.release` `fe.<c>.nginx` | `current` 链接与版本一致、`index.html` 引用的哈希资源齐全、预压缩 `.gz` 与原文件逐字节一致；站点片段与安全头通过静态检查、conf.d 软链状态 |
| 证书 | `cert.node_mtls` `cert.server_leaf` `cert.private_ca` | 叶证书**剩余 < 总有效期 1/6**（约 15 天）才告警——Agent 在剩余 1/3 时自动续期，续期窗口内不告警；CA < 1 年告警；过期 fail |
| 配置 | `cfg.validate` | 等价 `config validate` |
| 安装一致性 | `state.drift` `journal.incomplete` | state.json 与磁盘/单元的双向核对；被中断的事务 → fail，提示 `lingedge recover` |
| 备份 | `backup.recent` `backup.master_key_separate` | 见下 |
| 版本与信任 | `ver.revoked` `ver.update_available` `trust.manifest_valid` | 需要 `release.base_url`（联网获取并验签清单）；`doctor.offline: true` 则 skip；命中吊销 = fail；有更新 = pass（消息列出） |

### `sys.time_sync`
三个来源合成一项：`timedatectl` 的同步标志；可选 SNTP 测量（`doctor.time_sync_servers`，缺省不联网）；**时钟回拨判定**——当前时钟早于 Tools 自己记录过的时间点（`state.json`、信任库、最近的事务日志）超过 `doctor.time_skew_fail`（5m）= fail。回拨会让清单 `valid_until`、证书有效期误判，并影响恢复后 `lineage bump` 的单调性（`backup-restore.md`）。

### `backup.recent` 与 `backup.master_key_separate`
- `backup.recent`：server/node 最近一次有效备份是否在 `backup.recent_max_age`（缺省 168h，**[ASSUMPTION]**）之内；Server 向导未完成不要求备份。
- `backup.master_key_separate`：**主密钥必须与数据库备份分开保管**。Tools 没有地方知道你把它放哪了，所以用 `tools.yaml` 记录：另存 `master.key` 后，把 doctor 提示的指纹前缀写进 `backup.custody_fingerprint`。备份里内置了主密钥（`--include-master-key`）→ warn；未记录 → warn；记录与当前主密钥不符（已轮换）→ warn。本项只提示，不读取、不校验保管位置，输出只含指纹前 16 位。

### `--fix-perms`
仅 root。**只修复** `master.key`、各组件配置（含配置目录、`migrate.yaml`）、节点 `identity/` 的「权限宽于规格」与「属主不符」；逐项输出并复检；**绝不创建、重建、删除、读取内容**；符号链接一律拒绝；`--dry-run` 只列出将做的。其他类别（数据/日志目录、release、状态目录）只给 chmod/chown 提示。

## 3. 配置：`config validate|show|edit`

目标是 `tools`（`tools.yaml`）或已装组件。

- `config validate [<组件>|tools] [--file <路径>]`：不带组件 = `tools.yaml` 与全部已装组件。**问题指出键路径与行/列，不回显取值**；有问题退出码 3。`server.yaml` 经 `lingedge-server config check`（与 Server 启动同一个解析器）校验；Server 二进制不可用时退回结构校验并**标注「部分校验」**。`node/dns/reporter` 的 schema 随 Epic 3 交付，现在只校验语法（同样标注）。
- `config show <组件|tools>`：脱敏输出（密码/令牌/私钥路径等敏感键与值里内嵌的令牌/PEM/DSN 口令一律 `***`），保留注释。
- `config edit <组件|tools> [--restart]`：编辑临时副本 → 校验（未通过则询问是否重新编辑；放弃时副本保留并告知路径，**不会被应用**）→ 原子替换并保留 `.bak`（同权限同属主）→ 可选重启。需要交互终端（`--json`/非交互 → 退出码 9；`--dry-run` 只给计划）。编辑器取 `$VISUAL`/`$EDITOR`，限 `vi|vim|nano|editor|emacs`（不支持带参数的命令行）。静态前端的站点片段由 Tools 生成，不可编辑。

### `tools.yaml`（`/etc/lingedge/tools/tools.yaml`，`--config` 可改）
```yaml
release:   {base_url, ca_file, proxy, resume_dir}      # 发行源（见 release.md）
install:   {health_timeout, health_interval}           # 健康等待窗口（install 与 service start/restart 共用）
log:       {level, file}                               # --log-level/--log-file 的缺省（命令行优先）
backup:    {dir, recent_max_age, custody_fingerprint}  # doctor backup.* 的目录、阈值与「主密钥已独立保管」记录
doctor:    {min_memory_mb, min_disk_mb, nofile_min, time_skew_warn, time_skew_fail, time_sync_servers, restarts_warn, offline}
```
全部顶层节与键严格校验：未知键、重复键、类型/取值非法一律拒绝（`lingedge config validate tools` 指出行列）。文件不存在 = 缺省配置（不是错误）。

## 4. 服务与日志：`service start|stop|restart`、`logs`

- `service start|stop|restart <组件>`（root，取组件锁）：作用于组件自己的服务单元。**`node` 只含 Agent**——OpenResty 数据面按节点自治（ADR-020）不随 Agent 停止。静态前端没有服务进程（退出码 3，提示由 Nginx 提供）；未安装 → 14。`start/restart` 之后在 `install.health_timeout` 内等待主单元 active 且健康，否则退出码 **11**（Tools 不会自动回滚一次手工操作）；成功后 `state.json` 的记录状态随之更新。`--dry-run` 只给计划。
- `logs <组件> [-f] [--since 10m] [-n 100]`：封装 `journalctl`（参数数组、不经 shell），**输出逐行脱敏**（令牌/口令/DSN 口令；PEM 块整块隐去；超长行截断）。`-f` 与 `--json` 互斥；node 同时显示 Agent 与 OpenResty 的日志。非 root 需在 `systemd-journal` 组才能读系统单元日志。

## 5. 等待环境验证（ENV-LNX / ENV-BT，Story 1.45）

真实 systemd 下的 `systemctl show` 输出与 `NRestarts` 语义、真实 journald 的 `logs`、root 属主下的真实 `--fix-perms`（chown）、真实宝塔上的 `baota.*`、真实 `ufw/firewalld` 配置文件、`timedatectl` 在各发行版的输出、节点真实证书文件布局（`identity/`）与 Server gRPC 叶证书/CA 的真实握手链。本机以真实进程/套接字/数据库验证了其余部分（见 `docs/bmad/stories/1-42-*.md`）。
