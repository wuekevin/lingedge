# ADR-006 真实 IP 传递与入站可信代理

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §9、§8.3、§24；PRD FR-70..FR-74、NFR-11、NFR-17；architecture AD-6｜阶段：M1

## 背景

基线 §9（已确认强制需求）：用户在 LingEdge 中自定义**向源站传递真实客户端 IP 的 Header 名称**（方向 LingEdge→源站，**不是**让用户定义 LingEdge 从上游读取哪个 Header）。要求：名称合法、禁保留 Header、自定义 Header 覆盖访客伪造值、IP 来自可信识别结果、站点独立配置、可下发/回滚/调试。另：LingEdge 自身**入站**真实 IP 识别需独立的可信代理 IP/CIDR 机制。

## 决策

### 入站：可信客户端 IP 识别（阶段 `real_ip`）

1. 唯一可信来源 `$le_client_ip`（预声明的 `ngx.var.le_client_ip`，由引擎在阶段 1 计算；**不存于 `ngx.ctx`**——`ngx.exec` 会销毁 ctx，见 ADR-004 §5 / IMP-03）。其后所有使用真实 IP 的功能——访问控制、限速、日志、回源 Header——只读它。
2. 机制（**判定在 Lua 引擎内完成，不使用 nginx `realip` 模块，也不渲染 `set_real_ip_from`**）：`trusted_proxies`（CIDR 列表）为**集群级默认 + 站点级覆盖**（站点只能收紧或显式扩展为"本站点额外可信"，均作为 **JSON 数据**经修订下发，不是 nginx 指令；CIDR 数上限，待基线测试调整）。引擎调用纯函数 `le.realip.resolve(peer, xff, proxy_protocol_addr, trusted_cidrs)`：**仅当 TCP 对端（或 PROXY protocol 还原的对端）属于可信 CIDR 时才采信 `X-Forwarded-For` 链（自右向左剥离可信跳，取第一个不可信地址）；否则 `$le_client_ip` = TCP 对端地址**（FR-74）。因判定是数据驱动的纯函数，站点级覆盖不再受 nginx `server` 级 `realip` 与 `Host` 已知时机的限制，也便于单测与调试（§10）。
3. **PROXY protocol 仅为集群级设置**（`Cluster.settings` 的 `node_global` 键，平台写入）：启用后所有站点共用，监听端启用 `proxy_protocol`，Lua 读取 `$proxy_protocol_addr` 作为对端传入 `resolve`；F-3 冲突校验按节点全局参数逐键相等处理（ADR-020 §2）。**站点级只允许 `x_forwarded_for`**（OpenAPI `TrustedProxies` 站点级无 `header` 选择，IMP-11）。
4. 前置有其他 CDN 的场景（如上游 CDN 自带 `CF-Connecting-IP` 类头）：M1 仅支持通过 `X-Forwarded-For` 链与 PROXY protocol；自定义"入站真实 IP 头名"作为 P2 评估项（不在 M1）。
5. **可信代理范围约束**（Server 保存时校验，错误码 `realip.trusted_proxy_forbidden`，`details[].reason` 取 `prefix_too_short | loopback | multicast | reserved | self`）：拒绝 `0.0.0.0/0`、`::/0`，以及前缀短于 IPv4 /8、IPv6 /32 的范围；拒绝回环、多播、未指定、保留段，以及**节点自身服务/出口地址**（该范围内任何主机都能伪造客户端 IP，仅禁 `/0` 不够）；公网范围短于 /16 时给 UI 警告（可伪造风险）。

### 出站：向源站传递（阶段 `origin_headers`）

5. 站点配置项（`site_settings`，FR-70）。**命名以 OpenAPI 为事实源**（IMP-22/M15）：`xff_enabled`（bool，向源站传 `X-Forwarded-For`）、`xri_enabled`（bool，传 `X-Real-IP`）、`custom_enabled`（bool）、`custom_header`（自定义头名）、`xff_policy ∈ overwrite | append_trusted`。数据库列带 `real_ip_` 前缀：

   | OpenAPI（`RealIpSettings`） | DB（`site_settings`） |
   |---|---|
   | `xff_enabled` | `real_ip_xff_enabled` |
   | `xri_enabled` | `real_ip_xri_enabled` |
   | `custom_enabled` | `real_ip_custom_enabled` |
   | `custom_header` | `real_ip_custom_header` |
   | `xff_policy` | `xff_policy` |

   每个站点独立。（旧文的 `pass_x_forwarded_for/pass_x_real_ip/pass_custom_header/custom_header_name` 已废弃。）
6. 处理逻辑（Lua，固定代码）：**先清除访客请求里的同名头**（`ngx.req.clear_header`），再写入验证后的 `$le_client_ip`：
   - `X-Real-IP` ← IP（开关开）；开关关闭时**也要清除访客自带的 `X-Real-IP`**，不向源站透传伪造值；
   - `X-Forwarded-For`：`overwrite` = 仅 IP；`append_trusted` = 仅当对端属于可信代理时保留其传来的链并追加 IP（否则等同 overwrite）；关闭时同样清除访客自带 XFF；
   - 自定义头（如 `Ling-Client-IP`）← IP；覆盖任何访客同名值。
7. **Header 名称校验**（Server 与 Agent 双侧，同源 `pkg/edgeconfig/validate`）：
   - 语法：`^[A-Za-z][A-Za-z0-9-]{0,63}$`（HTTP token 的保守子集；拒绝 `_` 以避免 nginx 默认丢弃含下划线头的行为分歧）；
   - 大小写不敏感地**不得**等于保留名：`Host Content-Length Content-Type Content-Encoding Transfer-Encoding Connection Upgrade Keep-Alive TE Trailer Cookie Set-Cookie Authorization Proxy-Authorization Proxy-Authenticate WWW-Authenticate Expect Range If-Match If-None-Match If-Modified-Since If-Unmodified-Since If-Range Via Forwarded X-Forwarded-Host X-Forwarded-Proto X-Forwarded-Port Accept-Encoding Origin Referer User-Agent`，**以及（MIN-03 扩充）** `Accept Accept-Language Cache-Control Pragma Date ETag Last-Modified Age Vary Location Content-Range Accept-Ranges X-HTTP-Method-Override X-Original-URL X-Rewrite-URL True-Client-IP CF-Connecting-IP X-Forwarded-Server`（其中 `Accept`、`Accept-Language` 是缓存键可选维度，自定义头覆盖会污染缓存键与源站内容协商）；`X-Real-IP`、`X-Forwarded-For` 另由专用开关控制（见下）；
   - 不得带保留前缀（不区分大小写）：`Proxy-`、`Sec-`、`X-LingEdge-`、`Ling-Internal-`；
   - 不得与 `X-Real-IP`、`X-Forwarded-For` 重名（它们有专用开关）；
   - 清单由 `pkg/edgeconfig` 常量维护，变更须走契约变更流程（ADR-017）。
8. **可信链不一致防护**：若集群/站点未配置任何可信代理，则 `$le_client_ip` 必为 TCP 对端；调试视图明确显示"识别来源：TCP 对端 / 经可信代理 X"。
9. **配置版本与回滚**：以上全部随站点块进入修订（ADR-002）；回滚旧版本即恢复旧 Header 名行为。
10. **调试（FR-73，重设计）**：Admin 的"真实 IP 调试"创建白名单任务 `debug_request`（node-protocol §9）。Agent **不再**向本机 OpenResty 发起请求、**不开**回环调试源站、生产请求路径**不含**"模拟对端/调试"分支：Agent 经本机 ctl socket（仅 `lingedge-node` 用户可达）的封闭操作 `debug_simulate`，调用与请求路径**同一批纯函数**——`le.realip.resolve(peer, xff, proxy_protocol_addr, trusted_cidrs)` 与 `le.origin_headers.build(…)`——其中 `simulated_peer_ip` 与伪造请求头仅是**函数入参**。返回识别到的 IP、命中的可信代理规则、最终出站头（Cookie/Authorization 脱敏，名称/数量/长度受白名单限制）。因此 ADR-023 §A.6（生产代码不得有 testing 分支）与 `pkg/ssrf` 的回环拒绝规则均不涉及，也无需任何 SSRF 豁免；"源站实际收到的"一致性由同一纯函数保证，并在验收中用真实源站 Nginx 日志（AC-E-3）核对。
11. **源站安全（NFR-17）**：提供"节点出口 IP 清单"（按集群，`nodes` 的出口 IP/IP 池）供用户在源站写 `set_real_ip_from`；回源默认校验源站 TLS，可显式关闭并告警；错误页与响应头不泄露源站地址（`proxy_hide_header` 固定列表 + 错误页统一）。

## Binds / Prevents / Rule

- 约束：Lua `realip`/`origin_headers` 模块（纯函数）、站点网络设置的校验与 UI、调试任务（`debug_simulate`）。
- 防止：访客伪造 `X-Real-IP`/XFF/自定义头欺骗源站；不可信链路被采信；保留头被覆盖导致请求走私/缓存污染；站点间 Header 名相互影响。
- 规则（AD-6）：**出站真实 IP Header 由 Lua 以验证后的 `$le_client_ip` 强制覆盖，并清除访客同名值；入站仅信任显式配置的可信代理 CIDR；Header 名按固定正则与保留清单校验。**

## 备选方案与权衡

- **用 nginx `proxy_set_header` 渲染自定义头名**：否决——头名进入配置文本，违反 ADR-004；改用 `ngx.req.set_header`。
- **允许下划线/任意 token 字符**：否决——nginx 对含 `_` 的头默认行为与源站不一致，易生歧义；保守子集即可满足 `Ling-Client-IP` 类需求。
- **默认信任 XFF**：否决——典型 IP 伪造入口。
- **使用 nginx `realip` 模块（`set_real_ip_from`/`real_ip_header`）判定入站可信代理**：否决（2026-11-16 修订）——判定落在 C 模块，无法以纯函数方式单测/调试，调试需要"模拟对端"请求分支；站点级覆盖受 `server` 级与 `Host` 时机限制；CIDR 进入 nginx 指令文本扩大渲染面。改为 Lua 纯函数 + JSON 数据。
- **仅 overwrite 一种 XFF 策略**：否决——前置合法代理的用户需要保留链；但 `append_trusted` 严格限于可信对端。

## 后果

- 关闭开关也会清除访客自带头，可能令依赖"透传访客 XFF"的源站行为变化；UI 需说明（UX 文档）。
- 入站可信代理判定由 Lua 纯函数完成（CIDR 匹配用基数树，每请求开销由 PoC-4/NFR-30 基线确认）；渲染面因此更小（无 `set_real_ip_from`）。

## 可被推翻处

保留名清单与前缀、Header 名字符集（是否允许下划线）、是否支持自定义入站头（P2）。

## 验证

- AC-E-1..E-7；对 `X-Real-IP`、`X-Forwarded-For`、自定义头分别做伪造测试；非可信地址伪造 XFF 不被采信、可信 CIDR 被采信；非法/保留名被拒；两个站点不同头名互不影响；修改头名→新修订→回滚。
- 单测：名称校验矩阵（含大小写、前缀、重复、扩充后的保留名）；Lua 单测：清除+覆盖逻辑；`le.realip.resolve` 表驱动向量（可信/不可信对端、多跳 XFF、IPv6、PROXY protocol 对端）；可信代理范围校验矩阵（`/0`、`/7`、回环、多播、保留段、节点自身地址）。
- 调试：`debug_simulate` 与请求路径共用同一函数的一致性测试（同输入同输出）；生产构建中不存在模拟对端分支（ADR-023 静态检查）。

## 修订记录 2026-11-16

评审驱动的原地修订（未改编号）：

- **IMP-11 / M11**：入站可信代理判定移入 Lua 引擎纯函数 `le.realip.resolve`，不使用 nginx `realip` 模块、不渲染 `set_real_ip_from`；PROXY protocol 仅为集群级 `node_global` 设置，站点级只允许 `x_forwarded_for`；增可信代理范围约束（最小前缀、禁用回环/多播/保留段/节点自身）与 `realip.trusted_proxy_forbidden` 的 `details.reason`；§10 调试重设计为 ctl `debug_simulate`（纯函数、对端为函数入参、不发请求、不开调试源站），消除与 ADR-023 §A.6 及 `pkg/ssrf` 回环规则的冲突。原因：原设计的站点级 `proxy_protocol` 在共享 listen 上不可实现，调试需要生产引擎存在可被请求影响的分支。
- **IMP-03**：§1 `$le_client_ip` 由 `ngx.ctx` 改为预声明的 `ngx.var.le_client_ip`（`ngx.exec` 销毁 ctx）。
- **IMP-22 / M15**：§5 站点配置字段名回写 OpenAPI（`xff_enabled/xri_enabled/custom_enabled/custom_header`）并给出与 DB `real_ip_*` 列的映射表。
- **MIN-03**：§7 保留 Header 清单扩充（`Accept`、`Accept-Language`、`Cache-Control`、`Pragma`、`Date`、`ETag`、`Last-Modified`、`Age`、`Vary`、`Location`、`Content-Range`、`Accept-Ranges`、`X-HTTP-Method-Override`、`X-Original-URL`、`X-Rewrite-URL`、`True-Client-IP`、`CF-Connecting-IP`、`X-Forwarded-Server`）。
