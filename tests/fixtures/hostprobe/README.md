# hostprobe 夹具（Story 1.31）

这些文件是 `internal/tools/probe` 与 `internal/tools/baota` 的解析器/探测测试的输入。**来源分两类，务必区分：**

## A. 本机真实命令输出（macOS 开发机，2026-11-21 采集）

| 文件 | 来源 |
|---|---|
| `real/nginx-v.stderr.txt` | 真实 `nginx -v`（stderr），nginx 1.31.8（Homebrew） |
| `real/mysqld-version.txt`、`real/mysql-client-version.txt` | 真实 `mysqld --version`、`mysql --version`，MySQL 8.0.46（Homebrew） |
| `real/redis-server-version.txt` | 真实 `redis-server --version`，Redis 8.10.1 |
| `real/redis-info-server.txt` | 真实 `redis-cli INFO server`（临时实例）；仅把 `os:`/`executable:` 两行改为 Linux 形态以免带入本机路径 |
| `real/node-version.txt` | 真实 `node --version` |
| `real/mysql-greeting-8.0.46.hex` | 真实 mysqld 握手包的十六进制（`xxd -p`），从临时实例的 TCP 端口原样捕获（其中的盐是一次性随机数） |

## B. 按真实格式构造，待 ENV-LNX / ENV-BT 以真机输出复验

macOS 上没有 `ss`、`/proc`、宝塔，下列样本是**按真实 Linux/宝塔格式严格构造**的，不是真机输出；在 ENV-LNX/ENV-BT 上用真机输出复验（Story 1.45）前，相关能力标「等待环境验证」：

| 路径 | 格式依据 |
|---|---|
| `ss/ss-lntup-root.txt`、`ss-lntup-nonroot.txt`、`ss-lntup-edge.txt` | iproute2 `ss -H -lntup`：IPv4/IPv6/`*` 通配、`127.0.0.53%lo` 接口后缀、`users:(("nginx",pid=…,fd=…),…)` 多进程、无权限时无 users 列、进程名含空格/引号/`\xNN` 转义 |
| `proc/net/{tcp,tcp6,udp,udp6}` | Linux 内核 `/proc/net/*` 原始格式：IPv4 为主机字节序 32 位整数（小端主机上 127.0.0.1 = `0100007F`），IPv6 为四个这样的字，端口大端十六进制，TCP 状态 `0A`=LISTEN，UDP 已绑定=`07` |
| `proc/fdmap.txt` + `proc/<pid>/{comm,status,cgroup}` | `/proc/<pid>/fd/<n> -> socket:[inode]` 的符号链接清单（测试时实体化到临时目录）、`comm`、`status` 的 `Uid:` 行、cgroup v2 的 `0::/system.slice/x.service` |
| `passwd` | `/etc/passwd` 格式 |
| `sys/meminfo`、`sys/meminfo-old-kernel`、`sys/mountinfo` | `/proc/meminfo`、`/proc/self/mountinfo` 格式（含 `\040` 转义、`ro`、overlay、nfs4、伪文件系统） |
| `systemctl/list-units.txt` | `systemctl list-units --type=service --all --no-legend --no-pager --plain` |
| `baota/*` | 宝塔常见默认布局（`/www/server/panel`、`data/port.pl`、`/etc/init.d/bt`、`/www/server/nginx|mysql|redis|php/<ver>|btwaf`、`panel/vhost/nginx/*.conf`、`panel/plugin/*`）。**面板版本来源文件（`class/common.py` 的 `version = '…'`）与各 `version.pl` 是按经验构造，须在 ENV-BT 真机核对**；vhost 样本按宝塔生成的站点配置风格编写（含 `#SSL-START` 注释、多 listen、`default_server`、正则/通配 server_name、畸形配置） |

`baota/*` 中不包含任何口令类文件。测试在运行时把「金丝雀」口令文件（`default.pl`、`default.db`、`redis.conf`、`my.cnf` 等）写进**临时副本**，以证明探测从不打开它们——这样仓库里也不存在固定口令。
