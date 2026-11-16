# ADR-004 OpenResty 配置生成与注入防护（模板 + 白名单，禁止拼接）

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §1.3、§2.4、§10.2、§24；PRD FR-54、FR-62、FR-65、NFR-5、NFR-10、R-1；architecture AD-4、PoC-1/2｜阶段：M1

## 背景

基线 §1.3 与 §24 禁止直接拼接不可信用户输入生成 Nginx 指令；PRD FR-54 要求"模板白名单 + 校验 + 转义"，并以注入向量测试纳入 CI。仅靠"转义"在 Nginx 配置语言里风险很高（变量展开、`include`、`lua_` 指令、正则、`$`、换行、花括号、引号嵌套）。需要把攻击面降到可穷举。

## 决策

**核心思想：数据与代码分离——用户可控值永远不是 nginx 指令文本。**

1. **固定模板**：`edge/templates/*.tmpl`（Go `text/template`）**嵌入 `lingedge-node` 二进制**并随发行包签名（ADR-012）；**模板文本不从 Server 下发**，修订里只有数据。Lua 引擎代码 `edge/lua/le/*` 同样随发行包发布，不可由修订改变。
2. **极小的结构化渲染面**：只有下列值允许进入 `nginx.conf` 片段，且各自经 `pkg/edgeconfig/validate` 的**类型化渲染器**（不是通用字符串替换）：
   - `Hostname`（`server_name`，IDNA→punycode 后匹配 `^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`，总长≤253，通配符仅允许首标签 `*.`）；
   - `IP`/`CIDR`（经 `net/netip` 解析再格式化输出，不回显原串；**入站可信代理 CIDR 不再进入 nginx.conf**——可信代理判定在 Lua 引擎内完成，CIDR 仅作为 JSON 数据，见 ADR-006）、`Port`（1–65535 整数）、`SafeName`（`^[a-z0-9_]{1,48}$`，用于 upstream/zone/profile 名，**由 Server 生成的内部 ID 派生，不取用户字符串**）、`BoundedInt`（带上下限的整数，如 TTL 档位、超时、容量）、`FilePath`（仅由 Agent 以内部规则生成，不接受修订数据）。
   - 渲染器输出前再做**二次断言**：无 `\r\n`、`;`、`{`、`}`、`"`、`'`、`$`、`#`、反斜杠与空字节；失败即整次渲染失败（不降级为转义）。
   - **默认站点（未匹配 Host/SNI，MIN-12）为固定模板，不含任何用户值**：HTTP `default_server`：未匹配 Host → `return 444`；HTTPS `default_server`：`ssl_reject_handshake on`（未匹配 SNI 在握手阶段拒绝，nginx ≥ 1.19.4）；HTTP/2 连接复用场景（`$ssl_server_name` 与 `Host` 不属于同一站点证书所覆盖的主机名集合）→ Lua 返回 `421 Misdirected Request`。
3. **规则与高变动数据一律作为 JSON 数据**（`data/sites/<id>.json` 等），由固定 Lua 解释器在 `access_by_lua`/`header_filter_by_lua`/`balancer_by_lua` 阶段读取执行（ADR-005）：重定向目标、重写、Header 名/值、条件值、正则、错误页内容、源站选择全部不进入 nginx.conf。动态 Header 名用 `ngx.req.set_header(name, value)`，名称已在 Server 与 Agent 两侧校验。
4. **注入向量集**（FR-54）：换行、分号、花括号、`$` 变量、`include`/`lua_`/`content_by_lua`/`set_by_lua` 字样、`%00`、超长、Unicode 同形字、IDNA 边界、CRLF；对**每个渲染面类型**与**每个规则字段**构造；预期=拒绝，或作为 JSON 数据字符串无害存在；生成配置必须通过 `openresty -t` 且**不含被注入的指令**（用 token 化比对，而非子串）。纳入 CI（Go 侧渲染器 golden/模糊测试可在本机运行；`openresty -t` 与 Lua 端需 OpenResty，见 ADR-023）。
5. **缓存 TTL 档位**（PoC-1）：`proxy_cache_valid` 为静态指令，Lua 无法在上游响应后改写 TTL。方案：Server 编译时把站点缓存规则归并为有限个"TTL 档位"（每站点上限 16、节点上限 64），渲染器为每个档位生成命名 location `@le_cp_<n>`（仅整数参数），Lua 在 access 阶段决定档位后 `ngx.exec("@le_cp_<n>")`。备选（PoC 失败时）：内部回环 server 在 `header_filter` 注入 `X-Accel-Expires`（双跳）。
   **`ngx.exec` 的状态约束（IMP-03/M11）**：内部重定向会销毁 `ngx.ctx`，并从 server-rewrite 阶段重新进入 rewrite/access 阶段（若 `access_by_lua` 配在 server 级会被重复执行）。因此：
   - 跨 exec 的状态**只经预声明的 `ngx.var.le_*` 传递**（`le_client_ip`、`le_rule_hits`、`le_cache_policy_hit`、`le_cache_bypass`、`le_engine_done` 等）。变量用 `map` 指令预声明（不用 `set` 指令：`set` 在 server-rewrite 重入时会被重置）；**不依赖 `ngx.ctx` 跨 exec**；
   - `access_by_lua` 入口检查 `le_engine_done`：已为 `1` 则直接返回（命名 location 内重入不再重复执行规则）；首次执行结束时置 `1`；
   - **PoC-1 显式通过条件**：(i) `ngx.var.le_*` 跨 `ngx.exec` 保留；(ii) 命名 location 内 access 阶段不重复执行；(iii) 一个请求只产生一行访问日志且含 `rule_hits`/`cache_policy_hit`；(iv) TTL 档位命中、Range、缓存锁行为符合预期。任一不通过 → 回退双跳方案，并评估其额外延迟与日志重复（回环 server 不写访问日志，以 `le_hop` 标识去重）。
   - **渲染规模**：渲染的命名 location 数 = 站点配置实际用到的 **(TTL 档位 × 上游变体)** 组合数（上游变体见下条 6），节点上限 64（超限发布失败）。
6. **源站解析与健康**（PoC-2）：回源主机名由 **Agent** 解析并经 `pkg/ssrf` 守卫校验（连接时校验解析结果，防 DNS 重绑定，NFR-10）；结果与健康状态经 Unix socket 控制接口写入 `lua_shared_dict`，`balancer_by_lua` 据此选择 peer；nginx.conf 只含 `upstream le_pool_<id> { server 0.0.0.1; balancer_by_lua_block { require("le.balancer").pick() } }`。PoC 失败的回退：Agent 渲染 IP 字面量 `upstream` 并做"仅 upstream 变更"的快速 reload。
   **逐源站 TLS/协议参数（IMP-10）**：`proxy_pass` 的 scheme 与 `proxy_ssl_verify` 是 location 级静态指令，`balancer_by_lua` 只能换 peer，不能换这些参数。约束与实现：
   - **同一源站组内所有启用源站的 `protocol`、有效 `tls_verify`（继承站点 `origin_tls_verify` 后）与 `sni` 必须一致**，否则保存时 `400 origin.group_inconsistent`；
   - 渲染器按**实际用到的 (TTL 档位 × 上游变体)** 组合生成命名 location，**上游变体 ∈ `http | https_verify | https_noverify`**（类型化、数量有界，仅整数/枚举参数）；
   - `host_header` 经 `proxy_set_header Host $le_upstream_host`（由 Lua 设置的预声明变量，Lua 侧已校验主机名）；`proxy_ssl_server_name on; proxy_ssl_name $le_ssl_name;`（变量，同样由 Lua 设置）。
   - **SSRF 白名单（IMP-10）**：`system_settings.ssrf.allow_cidrs`（平台写入、`setting:write` + 再认证、审计）作为 **`global` 块字段 `ssrf_allow_cidrs` 进入签名修订**；Server 保存源站时与 Agent 连接时（`pkg/ssrf`）使用同一份；变更需审计。
7. **验证与应用（节点代，BLK-04/H4）**：渲染单位是**节点代 `g<N>`**（该节点所属各集群当前 `desired` 修订的站点块合并渲染成**单一** `nginx.conf`，单一 `current` 符号链接，一个 OpenResty 进程；目录总表、节点全局参数、多集群候选代与回退规则见 **ADR-020 §2 目录总表**）。每个新代在 `generations/g<N>/` 渲染，用**与生产同一份 `-p/-c`** 预检：`openresty -t -p /var/lib/lingedge-node/generations/g<N>/ -c conf/nginx.conf`；生产（systemd `ExecStartPre/ExecStart`）为 `-p /var/lib/lingedge-node/current/ -c conf/nginx.conf`。**规则**：`nginx.conf` 中凡可写路径（`proxy_cache_path`、temp、pid、日志）一律为**绝对路径**且不在代目录内；只读引用（`conf/`、`data/`）才相对前缀；Lua 引擎路径写绝对路径（`/opt/lingedge/node/current/edge/lua`）。因此 `-t` 与生产仅前缀字符串不同、内容相同，`-t` 通过即等价于生产可启动。`-t` 通过后才切换 `current` 并 reload（ADR-002、ADR-020）。渲染前后记录配置哈希，健康检查回显哈希以确认生效。
8. **运行期数据通道**：站点缓存 epoch、URL 失效标记、源站解析/健康、限流/封禁（P4）经**本地 Unix socket** 控制接口（仅 `lingedge-node` 用户可访问）写入共享字典；该接口只接受**封闭操作集**（带结构化参数），不接受任意代码或配置文本。封闭操作集含 **`debug_simulate`**（真实 IP 调试，FR-73）：在 ctl 内调用与请求路径**同一批纯函数**（`le.realip.resolve`、`le.origin_headers.build`），对端地址等仅作为函数入参，**不发起任何请求、不开调试源站、不在生产请求路径增加调试分支**（ADR-006 §10）。
9. **TLS**：证书路径由 Agent 内部生成（`certs/<certificate_uid>/{fullchain,privkey}.pem`，0600）；SNI 选证采用静态 `server` 块 + `ssl_certificate` 路径（页面不含用户字符串）。HTTP/3：不监听 UDP 443（D-7）。

## Binds / Prevents / Rule

- 约束：Agent 渲染器、`edge/templates`、`edge/lua`、规则编译器（Server）、注入测试。
- 防止：任何用户值经字符串拼接进入 Nginx 配置；通过修订下发 Lua/模板代码；DNS 重绑定回源 SSRF；渲染失败时"尽力转义继续"。
- 规则（AD-4）：**`nginx.conf` 只由固定模板渲染，渲染面仅限类型化白名单值；用户规则一律作为数据，由固定 Lua 解释；校验失败即整次失败，不降级为转义。**

## 备选方案与权衡

- **在 Go 里对所有用户值做转义后拼接指令**：否决——Nginx 语义面大，转义规则难以穷尽，一旦遗漏即配置注入（RCE 级别，`lua_` 指令）。
- **`include` 按站点用户片段**：否决——等同开放任意配置。
- **运行期用 `loadstring` 把规则编译成 Lua 源码**：否决——把用户值变成可执行代码；改为"数据 → 闭包表分派"，不 `loadstring` 任何用户派生字符串。
- **Lua 解释 vs 渲染为 `map/if`**：选 Lua 解释（灵活、可统一 13 阶段）；性能由 PoC-4 基线验证，热点类型可后续编译为 `map`。
- **Agent 之外由 Server 渲染完整 nginx.conf 下发**：否决——Server 被攻破即可下发任意配置；模板固定在节点侧降低 Server 失陷的爆炸半径（配合签名）。

## 后果

- 运行期数据通道与 Lua 引擎成为关键组件，需要 OpenResty 真机测试；本机无 OpenResty 时相关 Story 标「等待环境验证」（ADR-023）。
- 动态 Header 名等设计要求 Server 与 Agent **双侧校验**（深度防御，规则相同、同源于 `pkg/edgeconfig/validate`）。
- TTL 档位与命名 location 设计受 PoC-1 约束，可能回退到双跳方案。

## 可被推翻处

若 PoC-1/2 失败，按文中备选回退；若用户要求"高级用户自定义 Nginx 片段"：**不接受**（违反基线 §1.3）。

## 验证

- 注入向量集纳入 CI；渲染器模糊测试；`nginx -t` 通过且指令 token 比对无新增（FR-54 AC、AC-D-7）。
- 生产构建中不存在 `include` 来自修订数据的路径（静态检查）。
- PoC-1/2 报告（OpenResty 真机）作为 M1 出口物之一。

## 修订记录 2026-11-16

评审驱动的原地修订（未改编号）：

- **IMP-03 / M11**：§5 增 `ngx.exec` 状态约束——内部重定向销毁 `ngx.ctx` 并重入 rewrite/access 阶段，故跨 exec 状态只经 `map` 预声明的 `ngx.var.le_*`、`le_engine_done` 防重入；PoC-1 增显式通过条件，失败回退双跳并评估延迟与日志去重。
- **IMP-10 / M11**：§6 约束同一源站组内 `protocol/tls_verify/sni` 一致（`origin.group_inconsistent`），按 (TTL 档位×上游变体) 组合渲染命名 location（节点上限 64），`host_header`/`proxy_ssl_name` 用 Lua 设置的变量；SSRF 白名单 `ssrf.allow_cidrs` 作为 `global` 块 `ssrf_allow_cidrs` 进入签名修订。原因：`proxy_ssl_verify` 等为静态指令，原文未给出承载与混合协议的处理。
- **BLK-04 / H4**：§7 改为“节点代 + 统一 `-p/-c`”（不再“每代 `-t -p <该代前缀>`、生产用另一前缀”），明确可写路径绝对、只读相对，引用 ADR-020 §2 目录总表。
- **MIN-12**：§2 固定模板增 `default_server`（HTTP 444、HTTPS `ssl_reject_handshake`、HTTP/2 连接复用 421）。
- **IMP-11（关联）**：入站可信代理判定移入 Lua（ADR-006），渲染面 `CIDR` 不再用于 `set_real_ip_from`；§8 运行期数据通道增 `debug_simulate` 封闭操作。
