# ADR-008 访问日志与指标管线

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §17、§2.1；PRD FR-61、FR-113、FR-114、FR-115、D-5、OQ-10、反馈 F-10；architecture AD-8、PoC-6｜阶段：M1

## 背景

基线 §17.2：高流量访问日志必须采用**独立可扩展的采集存储机制，不全部写入 MySQL**。PRD D-5 采用三层：节点本地 JSONL → 断点续传上报 Server → 分片文件 + 聚合为 `metric_rollups`；日志写读经 `LogSink/LogQuery` 抽象以便后续接入外部存储。技术栈限定 MySQL/Redis，不新增运维依赖。

## 决策

1. **节点产出**：OpenResty 用 `access_log` + `log_format le_json escape=json` 写 JSON Lines 到 `/var/log/lingedge-node/access/access.log`；字段由固定模板渲染（不含用户值）：`ts, site_uid, host, method, uri(截断), status, bytes_sent, body_bytes_sent, req_bytes, ttfb_ms, req_time_ms, upstream_ms, cache_status, cache_policy_hit, client_ip, scheme, http_ver, ua(截断), referer(截断), rule_hits(采样), waf_action(P4), request_id`。URI/UA 截断与敏感查询参数脱敏（NFR-12）：查询串默认记录前缀 N 字节并遮蔽站点配置的敏感参数名。
2. **轮转**：由 Agent 控制——`rename` 当前文件为 `access-<segment_seq>.jsonl` + `openresty -s reopen`（信号 USR1）；`segment_seq` 为节点内单调。限盘：日志目录与 spool 有总量上限，超限丢弃最旧**已确认上传**之外的最旧段并上报 `LOG_SPOOL_OVERFLOW`（不写满系统盘，FR-61）。
3. **上传**：Agent 按 `(segment_seq, offset)` checkpoint 读取，按行边界切批，zstd 压缩，经 `TelemetryService.UploadAccessLogs`（客户端流）上传；**去重键 `(node_uid, segment_seq, start_offset, end_offset)`**，服务端返回 `ACCEPTED/DUPLICATE/REJECTED_OVERLAP/REJECTED_GAP/REJECTED_INVALID`；Agent 状态丢失时 `GetLogCheckpoint` 恢复；段完成且被确认后才删除段文件。至少一次投递 + 服务端去重 = 效果上恰好一次。
4. **Server 落盘与入库强校验**（IMP-08/H7）：按行解析 `site_uid`，追加写 `${data}/logs/access/<site_uid>/<YYYYMMDD>/<node_uid>-<segment_seq>.jsonl.zst`（按日、按站点、按节点分片；每个 chunk 是一个可追加的 zstd 帧）。**节点自报的 `site_uid` 不可信**（被攻破的节点 A4 可伪造以路径穿越或污染他租户日志/rollup/计量），写前必须：① `site_uid` 匹配 `^[0-9A-HJKMNP-TV-Z]{26}$`，格式非法 → **整批 `REJECTED_INVALID`**；② 站点必须属于**该节点所在集群当前绑定**的站点（查缓存的节点→集群→站点映射）；合法 ULID 但不属于 → 归入 `${data}/logs/access/_unattributed/<node_uid>/…`（`access_log_chunks.site_id=0`，仅平台可见，计 `LOG_SITE_MISMATCH` 事件，覆盖“站点刚被解绑”的合法尾部日志）；③ 最终路径 `realpath` 经 `filepath.Rel(logRoot, …)` 断言在日志根内。**chunk 按（批次, 站点, UTC 日期）拆分**（跨零点的批次拆成两行，`log_date` 并入去重键，避免同键两行冲突）；MySQL `access_log_chunks` 仅存**每个 chunk 一行**元数据（`tenant_id, site_id, node_id, log_date, segment_seq, start_offset, end_offset, file_offset, stored_length, storage_path, line_count, raw/stored_bytes, min_ts, max_ts, status_counts_json, sha256`，见 database/node-deploy §6.1），其中 `sha256` = **该 chunk 未压缩 JSONL 字节**的哈希，**不存任何请求行**。
5. **LogSink / LogQuery 抽象**：
   ```go
   type LogSink interface { Append(ctx, batch LogBatch) (committed Offsets, err error) }
   type LogQuery interface { Query(ctx, scope tenant.Scope, q LogFilter, cursor Cursor, limit int) (Page[LogLine], Cursor, error) }
   ```
   M1 实现 `FileLogSink/FileLogQuery`；`LogFilter` 支持站点（必填）、时间范围（必填）、状态码、客户端 IP（精确/前缀）、缓存状态、host/path 前缀。查询按时间范围用 `access_log_chunks` 选 chunk、解压顺序扫描过滤（**顺序扫描，无二级索引**；单次查询范围与返回行数有上限，延迟目标待基线测试）。查询强制租户 Scope（ADR-009）：站点必须属于调用者租户。外部存储（如 ClickHouse）的接入触发条件：基线测试表明写入/查询不达用户门槛（D-5/OQ-10）。
6. **指标 rollup**：Agent 在本地对**已 flush 的日志**按 `window_seconds=60` 对齐窗口计算 rollup（站点级与节点级；字段：请求数、状态码分布(2xx/3xx/4xx/5xx+分状态码 Top)、`bytes_out/bytes_in`、缓存状态计数(HIT/MISS/EXPIRED/BYPASS/STALE…)、回源请求数/字节/延迟直方图、规则命中、WAF 事件(P4)）并经 `ReportRollups` 上报；节点系统指标（CPU/内存/磁盘/连接数/OpenResty 状态）随 Heartbeat。窗口 id 确定性 + `generation` 更正（node-protocol §11）；Agent 还产出 `ttfb_ms_sum` 与 `bandwidth_peak_bps`（按日志秒级 `ts` 与 `bytes_sent` 以 1 秒分桶取窗口内峰值，聚合 `max`），使 DB 的 metric 词表全部有生产者。
7. **Server 写 `metric_rollups`**：幂等 upsert（窗口判定经 `rollup_windows`：键 `(node_id, site_id, window_start, window_seconds)`；**更高 `generation` → 替换；同代同值 → DUPLICATE；同代异值 → CONFLICT，保留首次并记事件，不静默覆盖**，与 proto 语义一致，DB 事实源见 dns-scheduling-security §5.1）；Scheduler 任务做降采样 1m→5m→1h→1d，并按保留策略清理（database README §7）。**同一份 rollup 供图表、告警、计量复用**（P5 的 `usage_records` 由其幂等派生，ADR-010）。
8. **对账**：Server 比对 rollup 的 `requests` 与对应日志 chunk 的 `line_count`（同窗口范围），差异超阈值产生事件（`ROLLUP_LOG_MISMATCH`），用于发现丢日志（FR-113 AC 的误差容限待基线测试）。
9. **MySQL 负载**：写入量与请求数**不成正比**，只与（站点数 × 节点数 × 窗口数）及 chunk 数成正比（FR-114 AC）。
10. **其他日志类别**（FR-115）：管理操作/登录/部署/升级/计费走 `audit_logs`/`login_logs`/`node_events` 等表；WAF 事件（P4）`security_events` 限采样与聚合；DNS 查询日志（P3）走同一 `LogSink` 抽象。
11. **Server 自身日志**：Zap JSON 到 stdout/文件（`/var/log/lingedge/`），Request ID 贯穿；脱敏中间件（NFR-12）。

## Binds / Prevents / Rule

- 约束：Node Agent 日志 shipper、Server `monitoring` 模块、LogSink/LogQuery 实现、计量（P5）、Admin 统计页。
- 防止：把每请求日志写入 MySQL；各处自己定义窗口/幂等键导致对账不一致；日志丢失无法发现；日志查询绕过租户隔离。
- 规则（AD-8）：**访问日志：节点 JSONL → Agent 断点续传 → Server 分片文件；MySQL 只存 chunk 元数据与 rollup；读写走 LogSink/LogQuery。**

## 备选方案与权衡

- **直接写 MySQL（分区表）**：否决——违反基线；高流量下写放大与保留清理成本。
- **经 Redis Stream/队列缓冲**：否决——引入对 Redis 的强依赖与丢失风险；日志可靠性应由本地文件+偏移提供。
- **Kafka/ClickHouse/ELK**：M1 否决（新增运维依赖，基线限定栈）；抽象接口保留后续接入。
- **Agent 之外由 OpenResty `log_by_lua` 直接 HTTP 上报**：否决——请求路径引入外部依赖，违背"数据面不依赖控制面"。
- **服务端侧从日志计算 rollup**：可行但延迟与 CPU 集中；选 Agent 侧本地计算并以日志对账。

## 后果

- 顺序扫描查询在大范围/高基数下慢；M1 限制查询范围并明确边界（反馈 F-10）。
- 日志为节点本地 → Server 单副本文件，需纳入备份策略与磁盘告警（备份/保留：database README §7）。
- Agent 的窗口计算依赖日志 flush 时序，需在 PoC-6 做故障注入（kill/断网/轮转）验证。

## 可被推翻处

保留期默认（OQ-7/OQ-10）、窗口长度（60s）、是否引入外部日志存储（基线测试后）、日志字段集。

## 验证

- 压测期间 MySQL 写入量不随请求数线性增长；断开主控后本地缓冲、恢复后补传无重复/缺失（PoC-6、FR-114 AC）。
- rollup 与压测工具/日志计数在容差内一致；对账事件注入测试。
- 查询租户隔离测试（越权站点日志返回 404）。
- 入库强校验：伪造 `site_uid`（`../..`、他租户站点、非 ULID）→ `REJECTED_INVALID` / `_unattributed`，无越界写；批次跨 UTC 零点产生两行且去重键不冲突；`REPLACED/CONFLICT` 语义与 proto 一致。

## 修订记录 2026-11-16

- §4（IMP-08、H7）：`site_uid` 入库强校验（ULID 正则 + 站点属于该节点所在集群 + realpath 断言，非法整批 `REJECTED_INVALID`，合法但未绑定 → `_unattributed`）；chunk 拆分改为（批次, 站点, 日期），`log_date` 并入唯一键；`sha256` 明确为 chunk 未压缩 JSONL 的哈希；新增 `file_offset/stored_length`。原因：原文逐行信任节点自报站点并按其拼路径，`uk_alc_dedupe` 缺 `log_date` 使跨零点批次触发唯一键冲突，`sha256` 对象未定义。
- §6/§7（IMP-09）：rollup 同代冲突语义统一（CONFLICT 保留首次）；Agent 产出 `ttfb_ms_sum`/`bandwidth_peak_bps`；`rollup_windows` 承载幂等判定。
