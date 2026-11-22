# Server 配置：监听与可信反代（Story 1.22b）

配置文件见 `configs/server.example.yaml`；任何键都可用环境变量 `LINGEDGE_<节>__<键>`（双下划线分隔层级，全大写）覆盖。配置错误只报键路径与规则，不回显取值。

## `metrics.listen`：只允许回环

- 默认 `127.0.0.1:9180`（architecture §7、tools-design §10.4：指标端口仅本机可达，由本机 Prometheus/Agent 抓取）。
- **必须是回环 IP 字面量**（`127.0.0.0/8` 或 `::1`）。通配（`:9180`、`0.0.0.0`、`::`）、公网/内网 IP、主机名一律在配置加载时拒绝启动（`metrics.listen: 必须是回环地址…`）；`server.Run` 在触碰任何基础设施之前再校验一次（防止绕过 `config.Parse` 直接构造配置）。
- 架构文档没有「受控的非回环」场景，因此**没有放开开关**。需要远程抓取时，请在本机用 Prometheus/Nginx 转发，而不是让指标端口监听非回环地址。

## `server.trusted_proxies`：REST 来源 IP 的唯一来源

- IP 或 CIDR 列表，缺省 `['127.0.0.1', '::1']`（同机 Nginx 反代）；环境变量 `LINGEDGE_SERVER__TRUSTED_PROXIES` 用逗号分隔。
- 只有 **TCP 对端**属于这些范围时才采信 `X-Forwarded-For`，并取链上**最右侧的非可信地址**；对端不可信时该头被完全忽略（`X-Real-IP`、`Forwarded` 从不被采信）。
- 它是来源 IP 的唯一来源：访问日志、登录日志与登录节流（IP 维度）、API Token 的 `allowed_cidrs`、`audit_logs.ip` 都经路由器登记的同一个值（`httpx.ClientIPFrom`），不存在第二处解析 `X-Forwarded-For` 的代码。
- 校验：拒绝空列表、未指定地址、`/0`、过短前缀（IPv4 须 ≥ /8，IPv6 须 ≥ /32，与 ADR-006 §5 同口径）、zone、空白、IPv4 映射的 IPv6 CIDR。范围过大会让不可信主机伪造来源 IP，不要把整个内网/公网段写进来。
- Nginx 在另一台机器上时，把那台机器的地址写进来（保留本机项或去掉由部署决定）；真实反代下的取值验证属「等待环境验证」。

## 数据库与 Redis 口令的来源（AA-7；ADR-026）

- `server.yaml` **不含口令**（含则拒绝启动，不变）。口令来源按优先级：环境变量 `LINGEDGE_DATABASE__PASSWORD` / `LINGEDGE_DATABASE__MAINT__PASSWORD` / `LINGEDGE_REDIS__PASSWORD` ＞ 对应 `*.password_file`（绝对路径、权限 0400/0440/0600/0640）＞ **运行期凭据文件** `secrets.runtime_credentials_file`（缺省 `<server.data_dir>/secrets/runtime-credentials.json`，`lingedge:lingedge` 0600，口令为 secretbox 密文，由安装向导写入）。前两者是显式覆盖通道（运维托管/容器场景），Story 1.15 的 `database.maint.*` 键保持不变。
- DB 连接目标（host/port/name/tls）与 Redis 地址：`server.yaml`/环境变量显式给出则以其为准，否则取运行期凭据文件中的非密字段。
- `lingedge_migrate` 口令**不**在这里：见 `docs/operations/secrets.md` §8（root:root 0400 的 `migrate.yaml`，仅 root 运行的 `lingedge-server migrate` 读取；其连接目标不取自运行期凭据文件）。
- **`database.socket`（CC-13，Story 1.37）**：MySQL 经 Unix 域套接字连接（规范化绝对路径、最长 100 字节），与 `database.host` 互斥（设置后 `port` 被忽略；套接字不经网络，无需 `tls`）；环境变量 `LINGEDGE_DATABASE__SOCKET`。安装向导的 `socket` 字段把该目标写入运行期凭据文件（`database.socket`，与 host/port 互斥），Server 运行期（app/maint 池）、向导迁移窗口、`migrate credential init|reset`（`--socket`，写入 `migrate.yaml` 的 `socket`）与 `db credential rotate`（`--socket` 或 server.yaml 的 `database.socket`）均走同一连接目标。本机已用真实临时 mysqld 的 socket 验证全部代码路径；宝塔/发行版 MySQL 的套接字文件对 `lingedge` 用户的可访问性仍属 ENV-BT 等待环境验证。
- `secrets.runtime_credentials_file` 为 Story 1.24 新增的可选键（CC-11），`database.tls`（`true|false|skip-verify|preferred`，缺省空；非回环主机须显式设置）同为 1.24 新增的可选键（CC-12）；**已实现**：`internal/platform/dbcred.Merge` 是唯一合并入口（`server.Run` 与 `lineage` 等子命令共用），只解密当前角色需要的字段（maint 口令仅 scheduler 角色）；文件缺失时沿用显式通道，文件存在但无效（符号链接/权限/属主/版本/未知字段/主密钥不匹配）一律拒绝启动。
- **setup mode**：无 `install.lock`（`<server.data_dir>/install.lock`）且数据库里没有完成的 `system_installations` 时 `lingedge-server --config …` 进入 setup mode（仅 `/setup` 静态页与 `/api/v1/public/setup/*`，其余 API 返回 503 `setup.mode_active`；默认监听 `http.listen` 的缺省值 `127.0.0.1:8080`，非回环监听时携带口令的接口须经 TLS，否则 `400 setup.insecure_transport`）。此时 `server.yaml` 不需要数据库/Redis 配置，也不需要口令。向导完成后同一进程内切换为正常模式。
