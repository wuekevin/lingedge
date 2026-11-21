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
