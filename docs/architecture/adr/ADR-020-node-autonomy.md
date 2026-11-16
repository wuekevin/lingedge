# ADR-020 节点自治：本地持久化最近有效配置，主控离线不影响数据面

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §3.1、§10.2、§10.3；PRD FR-56、FR-57、FR-62、R-4、F-2；architecture AD-20、§8.3｜阶段：M1

## 背景

基线 §3.1：**主控暂时离线时，已发布配置的节点必须继续处理用户访问。** FR-57 进一步要求：停止 Server 或阻断网络期间访问成功率不下降；节点重启（主控仍离线）后用本地配置恢复服务；主控恢复后自动重连并补报。R-4 指出配置签名不得带会令离线节点停服的过期机制，且离线期间无法续期证书是已知限制。

## 决策

1. **数据面零控制面依赖**：请求路径上没有 Server、MySQL、Redis 调用。OpenResty 仅读取本地渲染产物与本地共享字典（ADR-004、ADR-022）。
2. **节点代与本地持久化布局（权威目录总表）**。节点只有**一个** OpenResty 进程、**一份** `nginx.conf`，所以渲染、回退、`current` 切换都以**节点代 `g<N>`** 为单位：`g<N>` = `{cluster_uid → (lineage, seq, manifest_sha256)}` 集合（各集群 `desired` 修订的并集，ADR-002 §10），由 Agent 把各集群修订的站点块与 `global` 块**合并渲染**成单一产物；`N` 为节点本地单调编号。`ReportApply` 仍按集群上报，但回退以节点代为单位。architecture §6、tools-design §15.2、node-protocol §7、ADR-004 §7、ADR-024 以本表为准：

   | 路径 | 属主/权限 | 内容 |
   |---|---|---|
   | `/var/lib/lingedge-node/identity/` | lingedge-node 0700 | 节点私钥、证书、CA 链、配置签名公钥集（keyset），文件 0600 |
   | `/var/lib/lingedge-node/generations/g<N>/` | lingedge-node，渲染后只读 | `generation.json`（集群→{lineage,seq,sha256}）、`conf/nginx.conf`、`conf/sites/*.conf`、`data/global.json`、`data/sites/<site_uid>.json`（规则/源站/缓存数据，Lua 读取） |
   | `/var/lib/lingedge-node/current` | 符号链接 | → `generations/g<N>`，`rename` 原子切换 |
   | `/var/lib/lingedge-node/state.json` | 0600 | `{current_gen, gens, clusters{uid→{lineage,seq,held_seq}}, epochs, markers, keyset_version, ca_bundle_version, 日志偏移检查点}`（原子写：临时文件→`fsync`→`rename`→`fsync` 目录） |
   | `/var/lib/lingedge-node/certs/<certificate_uid>/{fullchain,privkey}.pem` | 0600 | 经 `FetchCertificate` 取得，**不属于任何代**（证书独立于修订，ADR-002 §5） |
   | `/var/lib/lingedge-node/cache/` | lingedge-node，可写 | `proxy_cache_path`（绝对路径，不在代目录内） |
   | `/var/lib/lingedge-node/runtime/` | 可写 | nginx 临时目录（`client_body_temp` 等，绝对路径） |
   | `/var/lib/lingedge-node/staging/` | 可写，**不可信域** | 远程升级暂存（ADR-024 §7）；Agent 只写、root 侧经 dirfd 读 |
   | `/var/lib/lingedge-node/logs-spool/` | 可写 | 上传前的访问日志段与 rollup（限盘，ADR-008） |
   | `/var/log/lingedge-node/access/` | 可写 | OpenResty 访问日志（JSONL） |
   | `/run/lingedge-node/{openresty.pid,openresty-ctl.sock,agent.sock}` | 运行期 | pid 与两个 Unix socket（不开 TCP） |
   | `/opt/lingedge/node/current/edge/lua` | root 只读 | 固定 Lua 引擎，`lua_package_path` 写该绝对路径（随 Agent 发行包签名，不由修订下发） |
   | `/opt/lingedge/openresty/current` | root 只读 | OpenResty 二进制 |

   **统一 `-p/-c`**：预检 `openresty -t -p /var/lib/lingedge-node/generations/g<N>/ -c conf/nginx.conf`；生产（systemd `ExecStartPre/ExecStart`，tools-design §15.2）`-p /var/lib/lingedge-node/current/ -c conf/nginx.conf`。规则：`nginx.conf` 中凡**可写**路径（cache、temp、pid、日志）一律**绝对路径**且不在代目录内；只读引用（`conf/`、`data/`）相对前缀。两种前缀仅字符串不同、内容完全相同，故预检 `-t` 通过 ≡ 生产可启动（相对路径 `lua_package_path`/`data/` 解析不再分歧）。reload=向 master 发 `HUP`，nginx 按名重新打开 `current` 符号链接。Agent 的 `ReadWritePaths=/var/lib/lingedge-node /var/log/lingedge-node /run/lingedge-node` 已覆盖 cache/runtime/staging。
3. **保留代数**：保留最近 **3** 个节点代（可配，**不得少于 2**，以支持一次回退）；清理仅在新代 `HEALTHY` 确认后进行。
4. **原子持久化**：`state.json` 采用「写临时文件 → `fsync` 文件 → `rename` → `fsync` 目录」；`current` 切换用 `rename` 新符号链接覆盖旧链接。崩溃后启动恢复规则：以 `state.json` 为准；若指向的代目录不完整则回退上一代。
4a. **节点全局参数与入群冲突校验**（BLK-04(4)）：`proxy_cache_path` 集合、`lua_shared_dict` 容量、监听端口/HTTP2/TLS 基线、PROXY protocol 入站、日志格式版本、worker 参数是**节点级**指令，取自所属集群 `settings_json.node_global` 键。节点加入多个集群时这些键必须**逐键相等**；入群、站点绑定、修改集群 `node_global` 键时由 Server 校验，冲突即拒绝（`node.cluster_conflict`，F-3 扩展，同时校验主机名不重复与站点级可信代理）。Agent 渲染时再做一次断言（重复 `server_name`/键不一致即渲染失败）。
4b. **多集群应用流程**：Agent 把所有待应用的集群 `desired` 变更**合并成一次候选节点代** → 渲染 → `openresty -t` → 切换 `current` → reload → 健康检查；失败则整体回退 `g<N-1>`，再**逐集群单独重试**以定位出错集群，`ReportApply` 只对出错集群报 `FAILED`；回退以节点代为单位（不能只回退某一集群的站点块）。
5. **重启自启不依赖 Agent 在线**：`lingedge-openresty.service` 的 `ExecStartPre` 先执行 `openresty -t -p …/current/ -c conf/nginx.conf`，`ExecStart` 使用同一对 `-p/-c`；首个配置代出现前由 `lingedge-openresty.path` 触发启动（ADR-024、tools-design §15.2）。系统重启后：systemd 拉起 OpenResty → 按本地代服务；Agent 随后启动并尝试连接主控。
6. **离线行为**：Agent 使用指数退避（带抖动）重连；离线期间继续本地健康检查与源站检查；日志与 rollup 写本地 spool，超限丢弃最旧段并记事件（不写满系统盘，node-protocol §日志）。**离线节点不接受新配置，也不自行"推断"配置。**
7. **重连后顺序**：先 `Heartbeat` 补报 `applied` 集合与能力 → 取得 `desired` 与 `site_epoch` 增量（游标失效则全量重同步，node-protocol）→ 应用待处理任务（含离线期间积压的缓存失效，ADR-007）→ 补传日志/rollup（幂等去重，ADR-008）。
8. **签名无过期**（R-4）：配置修订签名不含有效期；节点验证只检查「签名有效 + `(lineage, seq)` **严格大于**已应用值」，因此长期离线后仍可恢复。anti-replay 靠 `(lineage, seq)` 单调（ADR-002 §12），不靠时间。
9. **证书类限制（已知限制，须在 Docs 与告警中声明，F-2）**：
   - 站点 TLS 证书若在节点离线期间到期，节点无法获取续期证书；Server 在到期前告警（FR-86）。
   - 节点 mTLS 身份证书有效期 90 天，Agent 在 2/3 周期自动续期（ADR-003）；**离线超过证书有效期后无法重新连接主控，必须重新 enroll**（新的一次性 Token）。数据面不受影响。`lingedge doctor` 对「证书剩余有效期不足 1/6（约 15 天，即续期窗口 1/3 已过半仍未续成）」告警——续期在剩余 1/3 时触发，故不能再以同一 1/3 阈值告警，否则正常续期窗口内每个节点都会告警（L24）。
10. **状态上报即时性**：节点每次应用/回退都落盘后再上报，保证「实际生效版本」以节点本地为事实，Server 端 `node_cluster_state` 为其镜像（FR-56）。

## Binds / Prevents / Rule

- 约束：Agent 实现（状态管理、渲染、应用）、OpenResty systemd 单元、Tools 的 node 安装器、Docs 的已知限制说明。
- 防止：因主控/数据库/Redis 故障导致边缘停服；因签名过期或证书到期导致无法本地恢复；崩溃时 `current` 指向半成品。
- 规则（AD-20）：**节点自治——本地持久化最近 N 代配置与证书；与主控断开不影响数据面；配置签名不设过期。**

## 备选方案与权衡

- **节点每次请求回源主控取配置/校验**：否决——违反基线 §3.1 的离线要求并使主控成为单点。
- **配置带短期有效期、过期降级到"维护页"**：否决——正是 R-4 警告的自毁机制。
- **本地使用嵌入式数据库（SQLite/bbolt）存状态**：暂不采用——文件 + 原子 rename 足够（状态量小），避免 CGO 与额外依赖；若状态复杂度增长可复议。
- **仅保留 1 代配置**：否决——无法做本地回退；2 代为最小可行。

## 后果

- 节点磁盘占用增加（3 代 × 集群数）；渲染产物体积随站点数增长，须限盘并监控。
- 离线期间运营者无法对该节点做任何变更；告警策略须覆盖「节点离线时长」。
- 证书续期的离线盲区需要运营流程配合（提前续期/换证书）。

## 可被推翻处

若用户要求更长的 mTLS 证书有效期以减少重新注册（权衡：吊销窗口变大，依赖 Server 侧吊销列表）：只需调整 ADR-003 的有效期参数；保留代数可调。

## 验证

- 故障注入：停止 Server/断网/停 MySQL/停 Redis，持续访问成功率不下降（AC-C-8）；重启节点（主控离线）后恢复服务。
- 崩溃测试：在渲染、切换 `current`、写 `state.json` 的各步骤 kill Agent，重启后状态一致可服务。
- 离线较长时间后重连：补报与补传无重复；超过证书有效期后须重新 enroll 的路径测试。
- 以上均为真实 Linux 节点验证项，缺环境时标「等待环境验证」（ADR-023）。

## 修订记录 2026-11-16

- §2（BLK-04、H4）：新增**节点代 `g<N>`** 概念与**权威目录总表**（含 cache/runtime/staging/certs/lua 路径与权限）；统一预检与生产的 `-p/-c`；可写路径一律绝对。architecture §6、tools-design §15.2、node-protocol §7、ADR-004 §7、ADR-024 同步引用本表。原因：旧文三处布局互相矛盾（`/opt` 下 nginx.conf、`generations/<cluster>/<seq>` 按集群分代、`runtime/` 前缀），多集群并集无法合成单份配置，且 `-t` 与生产前缀不同使预检不等价。
- §4a/§4b（BLK-04(4)、F-3）：节点全局参数 `node_global` 逐键相等的入群校验；多集群候选代整体回退后逐集群重试。
- §3/§8（L24）：保留“节点代”；签名校验改用 `(lineage, seq)` 严格大于（ADR-002 §12）。
- §9（L24）：`doctor` 节点证书告警阈值改为剩余 <1/6（续期 1/3 触发，避免常态告警）。
