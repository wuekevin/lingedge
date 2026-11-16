# ADR-005 统一规则引擎 DSL、阶段映射与缓存隔离

- 状态：已接受（架构代理非交互决策；2026-11-16 评审修订）｜日期：2026-11-16｜关联：基线 §8、§11；PRD FR-63..FR-69、FR-76、D-15、NFR-5；architecture AD-5、PoC-4/5｜阶段：M1（执行器子集），P2/P3/P4 扩展

## 背景

基线 §8 要求**统一规则引擎**（非每种规则独立数据结构）：规则类型 12 种、条件字段 14 类、10 个操作符、AND/OR/NOT、13 个固定执行阶段、优先级/启用/作用范围/版本/命中统计/发布/回滚，并必须解决**缓存规则与动态内容隔离**（防跨用户泄漏）。

## 决策

### 1. 数据模型

- **一张表 `site_rules`** + `rule_versions`（不可变快照）+ `rule_templates`；新增规则类型**不需要新表**（FR-63）。规则 = **列 + 规则文档**：列 `rule_type`、`phase`（由 `rule_type` 派生、只读）、`name`、`priority`、`enabled`、`scope_json`（仅 `hosts`）、`published_version_id`、`current_version_id`；规则文档 `doc_json = {when, action}`（`rule_versions` 同构快照）。以 `docs/api/openapi.yaml`（`RuleDoc`/`RuleAction`）与 `docs/database/rules-cache-cert.md` 为事实源，本 ADR 不另立形态。
- **规则示例**（`rule_type=redirect`，`phase` 由类型派生为 `redirect`；`doc_json` 如下）：
  ```json
  {
    "when": { "all": [
      { "field": "path", "op": "starts_with", "value": "/old/" },
      { "not": { "field": "header", "name": "X-Debug", "op": "exists" } }
    ]},
    "action": { "redirect": { "status": 301, "strip_prefix": "/old/", "location": "/new/${path_suffix}", "preserve_query": true } }
  }
  ```
  `scope_json` 仅 `{"hosts":[…]}`；**不再有 `scope.path_prefix`**——前缀剥离由 `redirect.strip_prefix`（字面量）显式表达，引擎自动为其补一个 `path starts_with strip_prefix` 条件（IMP-02）。
- **条件树**：`all`（AND）/`any`（OR）/`not`/叶子 `{field, [name], op, value}`；**最大深度 6、叶子 ≤ 64**；字段：`host path uri query method header cookie user_agent referer client_ip cidr asn country region`；操作符：`eq ne contains starts_with ends_with regex in not_in exists ip_in_cidr`；字段×操作符合法矩阵由 `pkg/edgeconfig` 的表驱动定义并生成文档/前端提示。`asn/country/region` M1 仅保留数据模型，校验拒绝发布（D-15）。
- **动作参数变量**（`${…}`，仅白名单；不含任意 Header/Cookie 值插值，防反射型注入与缓存键污染；P2 评估）。统一变量表（Server 校验与 Lua 执行同源，golden 向量）：

  | 变量 | 来源 | 是否解码 | 输出编码 |
  |---|---|---|---|
  | `${host}` | 请求 Host（小写、IDNA→punycode、去端口） | 否 | 原样（已校验主机名字符集） |
  | `${path}` | §3.4 规范化后的 path | **否：保留百分号编码，保留字符不解码** | 规范化编码形式 |
  | `${path_suffix}` | `${path}` 去掉 `redirect.strip_prefix` 后的剩余（仅 redirect 可用） | 否 | 规范化编码形式 |
  | `${query}` | 原始查询串（不含 `?`） | 否 | 原编码形式 |
  | `${uri}` | `${path}` + （有查询时）`?` + `${query}` | 否 | 规范化编码形式 |
  | `${client_ip}` | `$le_client_ip`（ADR-006）规范文本 | — | IPv4 点分 / IPv6 压缩小写 |
  | `${scheme}` | `http` / `https` | — | 原样 |

  因所有插值都以**编码形式**输出，变量值内不可能出现 CR/LF 或其它控制字符（解码后的 CR/LF 只会以 `%0d%0a` 出现）；`/%2f%2fevil.com` 保持 `%2f` 不解码，`//evil.com` 经规范化合并为 `/evil.com`，因此不会因变量产生 scheme-relative 重定向。UX 文档中的 `{path_tail}` 一律改写为 `${path_suffix}`（单一占位语法）。
- **插值后校验**（发布期对模板做静态校验，请求期对结果做断言）：
  1. `redirect.location` 模板必须以**单个 `/`** 开头，或以**字面量** `http(s)://<字面主机>/` 开头；变量只允许出现在该前缀之后；
  2. 插值结果不得含控制字符（<0x20、0x7f）、不得含反斜杠，不得以 `//` 或 `/\` 开头，长度 ≤ 4096；
  3. 请求期断言失败 → 不发生重定向，返回固定的 500 类错误页并累计计数 `rule_interpolation_rejected`（日志 `rule_hits` 不记该规则命中）；
  4. Header 值（`origin_request_header`/`response_header`）同样套用 2；Header 名按 ADR-006 §7。

### 2. 类型 → 阶段映射（固定 13 阶段；**(rule_type → phase) 合法表**）

阶段顺序（基线 §8.3 逐一映射）：`1 real_ip → 2 access_control → 3 waf_cc → 4 redirect → 5 rewrite → 6 cache_rule → 7 cache_lookup → 8 origin_rule → 9 origin_headers → 10 upstream_response → 11 cache_write → 12 response_headers → 13 respond`。

`phase` 是由 `rule_type` **派生的只读属性**（OpenAPI `RuleCreate` 不再接收 `phase`；非法组合不可表达）。合法表如下：

| 规则类型 | 派生阶段（唯一） | 阶段（M/P） | 备注 |
|---|---|---|---|
| （真实 IP 识别） | real_ip | M1 | 非规则，引擎固定；见 ADR-006 |
| access_control | access_control | M1（IP/CIDR/Path/Header 等不依赖 GeoIP 的条件） | 动作 allow/deny（含 `deny{status,message_key}`，引擎拒绝时的自定义页面由此表达） |
| WAF/CC（引擎内置） | waf_cc | P4 | M1 空阶段，保留顺序位置 |
| rate_limit | waf_cc | P4 | |
| hotlink（防盗链） | access_control | P4 | 基于 Referer |
| redirect | redirect | M1 | |
| url_rewrite | rewrite | M1 | `ngx.req.set_uri(uri,false)` |
| request_header | rewrite（子阶段 after-rewrite） | M1 | 修改客户端请求头（受 §3.7 KCR 约束） |
| cache_rule | cache_rule | M1 | 决定 cache 模式、TTL 档位、key 维度 |
| （缓存查询） | cache_lookup | M1 | nginx `proxy_cache` 固定，不接受规则 |
| origin_rule | origin_rule | M1 | 回源协议/Host/端口/源站组选择/路径改写（受 §3.7 KCR 约束） |
| origin_request_header | origin_headers | M1 | 回源请求头（受 §3.7 KCR 约束） |
| error_page | upstream_response（**单一阶段**） | M1 | 源站 4xx/5xx 的自定义页面（受 §3.7 KCR 约束）；原“engine 拒绝时 respond 阶段”并入 access_control 的 deny 动作，不再单列 |
| （缓存写入） | cache_write | M1 | nginx 固定；TTL 由 cache_rule 档位决定 |
| response_header | response_headers | M1 | `header_filter_by_lua` |
| page_rule | 编译期展开为多阶段子规则 | P2 | 不产生独立执行语义 |

- **执行位置与物理顺序**：阶段 1–6、8–9 在单个 `access_by_lua_block`（`le.engine.run_request_phases()`）按序执行；`upstream_response` 在 `header_filter_by_lua` 的最前、`response_headers` 紧随其后；`respond` 为终止性响应（redirect/deny/error_page）。**逻辑阶段 7（cache_lookup）在 nginx 的 content 阶段由 `proxy_cache` 执行，物理上晚于全部 `access_by_lua` 规则（阶段 1–6、8–9）**；因此 `origin_rule`/`origin_headers` 的决策与 §3.7 的 `le_cache_bypass` 标志在查缓存之前已经确定。同阶段内按 `priority` 升序（数字小者先执行，值相同按规则 ID）；`stop_on_match` 动作（如 redirect/deny）终止后续阶段。
- 命中统计：每条规则命中经 `lua_shared_dict` 计数，由 Agent 汇入 1 分钟 rollup 的 `rule_hits`（有基数上限，ADR-008）；日志 `rule_hits` 字段采样记录规则 ID。

### 3. 缓存隔离（FR-69，RFC 9111 保守语义）

1. **默认可缓存判定**（全部满足才缓存）：方法 GET/HEAD；状态码在站点可缓存集合内；响应**无 `Set-Cookie`**；响应无 `Cache-Control: private|no-store`；请求带 `Authorization` 时仅当响应含 `Cache-Control: public`/`s-maxage` 才可缓存；`Vary` 仅允许 `Accept-Encoding` 及被纳入缓存键维度的头，`Vary: *` 或含未纳入维度的头 → 不缓存。
2. **缓存规则不能违反上述默认**，除非站点管理员在 `cache_policies` 上**显式**开启对应"危险开关"（M1 仅两个，与 DB/OpenAPI 同名）：`allow_cache_set_cookie`（允许缓存带 `Set-Cookie` 的响应）、`allow_cache_authorization`（允许缓存带 `Authorization` 的请求）；需二次确认（`risk_ack_by/risk_ack_at`）、写审计、UI 风险提示、并在配置里带 `unsafe_overrides` 标记便于巡检（FR-69）。**M1 没有 `ignore_vary` 开关**。边界澄清（IMP-22）：
   - `cache_rule.action.cache.mode=override` **只覆盖 TTL**，**不绕过**第 1 条的任何保护（`private/no-store`、`Set-Cookie`、`Authorization`、`Vary`）；绕过只能经策略的危险开关；
   - `cache_policies.ignore_origin_cache_control=1` **仅忽略 TTL 类指令**（`max-age`、`s-maxage`、`Expires`），`Cache-Control: private|no-store|no-cache` 与 `Vary: *` 保护始终有效。
3. **缓存键** = `site_id : site_epoch : url_ver : scheme(按策略) : host : normalized_uri : query 维度 : header 维度 : cookie 维度`（该维度集合即 §3.7 的 `keydims(P)`，**冻结**；新增维度须改本 ADR）。Query 策略（DB/OpenAPI 取值）：`ignore_all | include_all | include_whitelist | exclude_list`（白名单/排除列表的参数名排序规范化）；Cookie 策略：`ignore | include_named | bypass_on_named`；Header 维度仅允许白名单头（`vary_headers`，如 `Accept-Encoding` 归一化、`Accept-Language` 可选）。`site_epoch`、`url_ver` 见 ADR-007（均为 `(lineage, value)` 比较，不进入 `keydims` 语义）。
4. **规范化 URI**：合并斜杠/点段、百分号编码规范化（大写十六进制、不解码保留字符）、拒绝非法编码；规范化函数在 Server 与 Lua 两侧同源测试（golden 向量）。
5. **缓存键维度变更是配置变更**：随修订下发；变更不自动清缓存（旧键自然不再命中）。
6. 请求头修改规则（`request_header`）**不得**修改被纳入缓存键的维度或 `Authorization/Cookie` 而不重算 key：引擎在 key 计算前执行 `request_header`；若规则触及 key 维度，其**读取集**同样受 §3.7 约束（原“编译期 warning”升级为 §3.7 的发布失败/强制 bypass）。
7. **缓存键封闭规则 KCR（BLK-03）**：见下。

#### 3.7 缓存键封闭规则 KCR（Key-Closure Rule）

**问题**：若某规则的条件或动作值依赖了缓存键之外的维度（例：`Cookie beta=1` → 回源到 beta 源站；`UA contains Mobile` → 改写路径；回源头 `X-Client: ${client_ip}`），不同访客得到不同的源站响应，却共享同一缓存对象——这就是基线 §8.3/FR-69 要杜绝的跨用户泄漏，且不经过任何“危险开关”。

**定义**
- `keydims(P)`：生效缓存策略的键维度集合——若 `cache_rule.action.cache.cache_key` 内联则取其，否则取其引用或默认的 `cache_policies`；成员为：`host`、规范化 `uri`(path)、`query` 维度（`query_policy`：`ignore_all`→∅；`include_all`→全部参数；`include_whitelist/exclude_list`→按白名单/排除后的参数）、`header` 维度（`vary_headers` 白名单）、`cookie` 维度（`cookie_policy=include_named/bypass_on_named` 所列名字）、`scheme`（`scheme_in_key=1`）。**不在其中**：`method`、`user_agent`、`referer`、`client_ip/cidr/asn/country/region`、未入键的 header/cookie/query 参数。
- `reads(r)`：规则 r 的条件叶子字段（含 header/cookie/query 参数名，`any/not` 内一并计入，语法级保守取并集）∪ 动作值里插值的变量（如 `${client_ip}`、`${scheme}`）∪ `HeaderOp` 取值来源。
- 作用对象：落在阶段 `rewrite`（`request_header`、`url_rewrite`）、`origin_rule`、`origin_headers`、`upstream_response`（`error_page`）的规则。`redirect`/`access_control`/`respond` 终止请求、`response_headers` 在 `cache_write` 之后，均不改变缓存对象，故**豁免**。

**规则**：对每一对 (r, P)——P 为可与 r 同时命中的、非 `bypass` 的缓存策略——**必须 `reads(r) ⊆ keydims(P)`**。否则：
1. **默认 → 发布失败**：`400 config.validation_failed`，`details[].reason = rule.cache_key_dimension_conflict`（指出规则、未入键维度、策略）；
2. 规则可在动作内显式声明 `cache_interaction: "bypass"`（默认 `"key"`）：编译产物给该规则附 `forces_bypass`，引擎在其条件命中时置 `ngx.var.le_cache_bypass=1`（渲染 `proxy_cache_bypass $le_cache_bypass; proxy_no_cache $le_cache_bypass;`），**命中的请求既不读也不写缓存**；未命中请求的行为不依赖该维度，因此不会串用；
3. `redirect`/`access_control` 等终止型规则不受限；`cache_rule` 自身的条件允许读任意维度（它只影响 TTL/策略选择，不影响源站响应内容），但其选出的 P 的键维度即上述 `keydims`。

**物理顺序**：origin 类规则与 `le_cache_bypass` 在 `access_by_lua` 内于 nginx 查缓存之前确定（见 §2 “执行位置与物理顺序”），故 bypass 的实现不依赖 `ngx.exec` 之后的状态。

**验收项（V-1..V-3，写入验证章节与 threat-model G-10）**
- **V-1 单元（Go，本机可跑）**：reads×keydims 矩阵（cookie/UA/header/query/client_ip/method 逐项）、`any/not` 嵌套、变量插值、内联 `cache_key` 覆盖、`cache_interaction=bypass` 放行；
- **V-2 集成（OpenResty，等待环境验证）**：`origin_rule when cookie beta=1` + `cookie_policy=ignore` → 发布被拒；改 `include_named:[beta]` → 发布通过，有/无 beta 的请求命中不同缓存对象且互不串；`cache_interaction=bypass` → beta=1 请求 `X-Cache-Status: BYPASS` 且不产生缓存对象；`UA contains Mobile → url_rewrite` 同理；回源头 `X-Client: ${client_ip}` → 被拒（或 bypass）；
- **V-3 泄漏测试（AC-C-7 扩展）**：AC-C-7a 两个不同会话 Cookie 的访客请求同一动态 URL 不得互串；AC-C-7b 条件化 `origin_rule`/`url_rewrite`（Cookie、UA 条件）场景同样不得互串；AC-C-7c 回源头含 `${client_ip}` 的规则不得导致跨 IP 串用。全部纳入 CI（OpenResty 环境）。

错误码 `rule.cache_key_dimension_conflict` 入 OpenAPI `RuleValidationIssue` 的 code 枚举与 `docs/api/README.md` 错误码索引；`RuleAction` 的 `origin`/`rewrite`/`request_headers`/`origin_request_headers`/`error_page` 增可选字段 `cache_interaction ∈ {key,bypass}`。

### 4. 正则（FR-65、PoC-5）

- 方言 = **RE2 兼容子集**：Server 用 Go `regexp/syntax` 解析校验（拒绝回溯引用、环视）；长度 ≤256、捕获组 ≤16。**“RE2 子集”本身不等于“无灾难回溯”**（`(a+)+$` 在 RE2 语法内合法，但在 PCRE 回溯引擎上指数爆炸），故不再依赖“嵌套量词启发式”，改为**歧义检测**：对解析后的 NFA 做指数歧义度（EDA）检测（Weber–Seidl 算法），存在 EDA 的正则一律拒绝（`rule.regex_rejected`）；多项式歧义靠下述输入长度上限约束。
- **输入长度上限**：匹配前对输入截断——path/uri ≤ 2048 B，header 值 ≤ 1024 B，user_agent ≤ 512 B；因此最坏为 O(n²) 且 n 有界。
- **执行**：节点用 `ngx.re`（PCRE JIT）并设 `lua_regex_match_limit`/`lua_regex_cache_max_entries`；超限计数 `regex_limit_exceeded` 事件。
- **超限语义（`on_limit`，每个 `regex` 叶子可带，规则级默认）**：`access_control` 规则默认 **`fail_closed`**（超限＝拒绝该请求，避免攻击者构造超限输入绕过 deny 规则）；其它规则类型默认 `no_match`。
- **共享上限**：`lua_regex_match_limit` 是 nginx 作用域级指令，全部租户共享——因此 PoC-5 验收增加“恶意输入下的 p99 延迟与对同节点其他租户的影响”；PCRE JIT 与解释执行下 match limit 的实际生效差异**待 PoC-5 实测**（保留为待确认项）。
- 优先推荐 `starts_with/ends_with/contains/in` 等非正则操作符（UI 提示）。

### 5. 校验与发布（FR-68）

发布前校验：结构（JSON schema）、字段×操作符合法、(rule_type→phase) 合法、正则（含 EDA 检测与 `on_limit`）、Header 名（ADR-006）、动作变量与插值模板（§1）、重定向目标（站内相对路径或 `http(s)` 绝对 URL，且满足 §1 插值后校验；禁止 `javascript:`/`data:`；循环检测：对规则做符号可达分析，保守拒绝明显的自循环）、**缓存键封闭规则 §3.7（`rule.cache_key_dimension_conflict`）**、可信代理 CIDR（ADR-006 §2）、数量上限（每站点规则 ≤ 500、每集群档位 ≤ 64，待基线测试调整）。

## Binds / Prevents / Rule

- 约束：规则 Service/编译器（Server）、Lua 引擎（Node）、Admin/Console 规则编辑器的字段/操作符矩阵、缓存策略模块。
- 防止：每种规则一套结构；阶段顺序在各处理解不一致；缓存规则**或条件化回源/改写规则**导致跨用户泄漏；正则语义/ReDoS 分歧与 fail-open；用户值变成代码或造成开放重定向。
- 规则（AD-5）：**所有规则共用一张表、一套条件/动作 IR、固定 13 阶段；缓存默认保守，覆盖必须显式、审计、风险提示；影响源站响应的规则读取集必须 ⊆ 缓存键维度，否则发布失败或强制 bypass（KCR）。**

## 备选方案与权衡

- **每类规则独立表/结构**：否决（基线 §8 明确禁止）。
- **嵌入通用脚本（Lua/JS 片段）让用户编写**：否决——等同开放任意代码，违反 NFR-5 与威胁模型。
- **规则编译为 nginx `map/if/rewrite`**：未选——表达力受限、跨阶段统一困难、注入面更大；热点类型后续可选择性编译（PoC-4）。
- **完整 PCRE**：否决——ReDoS/语义差异风险；以 RE2 子集 + EDA 检测换确定性（反馈 F-6）。
- **Cookie 缺省参与缓存键**：否决——命中率极低；缺省"忽略 Cookie + 绝不缓存 Set-Cookie 响应"更安全且实用。
- **KCR 的另一实现——把“回源决策指纹”（源站组、改写后 URI、回源头值哈希）并入缓存键**：未选为主机制——`${client_ip}` 类变量会使缓存键逐访客碎片化（命中率趋零），且让隐式行为取代显式校验；保留为 P2 评估项。

## 后果

- 解释执行带来每请求 CPU 开销，规则数量/条件数需上限，待 NFR-30 基线报告确认。
- 缓存默认保守会降低命中率（反指标 SM-C2：不得为命中率放宽默认）。
- 需要在 Server（Go）与 Node（Lua）两侧实现字段/操作符/规范化/变量插值，**golden 向量**保证一致。
- KCR 使部分“看似合理”的组合（如 UA 条件回源且缓存按默认策略）在发布时被拒，需要在 UI 中给出可操作的修复提示（把维度加入缓存键，或声明 `cache_interaction=bypass`）。

## 可被推翻处

变量插值范围、规则数量上限、`request_header` 子阶段放置、KCR 的默认处置（拒绝 vs 自动 bypass）；若用户要求支持完整 PCRE，须同时接受 ReDoS 风险与匹配上限降级策略。

## 验证

- 每个字段×适用操作符单测；AND/OR/NOT 真值表；Go↔Lua 一致性 golden 向量（路径规范化、正则、IP/CIDR、变量插值与插值后校验）。
- 阶段顺序集成测试（同时命中多阶段规则）；(rule_type→phase) 非法组合不可表达的契约测试。
- **KCR：V-1（Go 矩阵单测）、V-2（OpenResty 集成）、V-3（AC-C-7a/b/c 泄漏测试，纳入 CI）**；两个不同会话 Cookie 请求同一动态 URL，用户 B 不得收到 A 的响应；条件化回源/改写规则场景同样适用。
- 正则：EDA 样本集（`(a+)+$`、`(a|a)*` 等）被拒；PoC-5 差分语料 + 恶意输入下 p99 与跨租户影响；`access_control` 超限 fail_closed 测试。
- 非法规则无法发布；`unsafe_overrides` 出现必有审计记录。

## 修订记录 2026-11-16

评审驱动的原地修订（未改编号）：

- **BLK-03**：新增 §3.7 缓存键封闭规则 KCR（`reads(r) ⊆ keydims(P)`，否则 `rule.cache_key_dimension_conflict` 发布失败，或规则显式 `cache_interaction=bypass`），原 §3.6 的“编译期 warning”升级为 error/bypass；补 V-1..V-3 与 AC-C-7a/b/c；明确 `le_cache_bypass` 的物理顺序。
- **IMP-02**：§1 增变量表（来源/是否解码/输出编码）与插值后校验；`${path_suffix}` 由显式 `redirect.strip_prefix` 定义，删除 `scope.path_prefix`；统一占位语法为 `${…}`。
- **IMP-22 / M15**：§1 规则形态回写为“列 + `doc_json={when,action}`”；§2 以 (rule_type→phase) 合法表改写，`error_page` 单一阶段 `upstream_response`；§3 回写 DB/OpenAPI 命名（`include_whitelist/exclude_list`、`allow_cache_set_cookie/allow_cache_authorization`，无 `ignore_vary`），并界定 `mode=override` 与 `ignore_origin_cache_control` 不绕过 private/no-store 等保护。
- **IMP-01**：§4 以 EDA 歧义检测替代“嵌套量词启发式”，增输入长度上限、`on_limit`（`access_control` 默认 fail_closed）、PoC-5 增“恶意输入下 p99 与跨租户影响”，JIT 与 match limit 差异保留待实测。
- **IMP-03**：§2 澄清逻辑阶段 7（nginx content 阶段 `proxy_cache`）与 `access_by_lua` 的物理顺序。
- 原因：原文把 `origin_rule`/`origin_headers` 与缓存同处 access 阶段却未约束其读取维度，条件化回源会造成跨用户缓存泄漏；规则文档与枚举与 OpenAPI/DB 漂移；正则启发式不完备且超限 fail-open。
