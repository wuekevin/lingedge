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
- `secrets.runtime_credentials_file` 为 Story 1.24 新增的可选键（CC-11）；在其落地前本节为目标形态。
