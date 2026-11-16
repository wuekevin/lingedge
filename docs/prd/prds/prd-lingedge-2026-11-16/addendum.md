# Addendum: LingEdge PRD 补充材料

> 本文保存对下游文档（架构、Epic、UX）有用、但不适合放进 PRD 主叙述的深度内容。均为 PRD 阶段的**候选/建议**，不是已冻结的架构决定；冲突时以基线与 PRD 为准。

## A. 访问日志管线候选设计（对应 D-5、FR-114）

1. 节点：OpenResty 以 JSON Lines 写本地文件，按大小/时间轮转，总量受限盘。
2. 上报：Agent（或独立采集器）按文件偏移做断点续传，批量压缩，经 mTLS 通道上报；以「节点 ID + 文件 + 偏移区间」作为去重键，保证至少一次投递 + 服务端去重。
3. Server 接收：按「站点/日期」分片写压缩文件，并维护轻量索引（时间范围、状态码、IP 前缀等）支撑基础筛选；聚合器按固定时间窗口生成 `metric_rollups`（请求数、字节、状态码分布、缓存状态），同一份 rollup 供图表、告警、计费复用。
4. 抽象：`LogSink`（写）与 `LogQuery`（读）接口；评估外部存储（如 ClickHouse）的触发条件：基线测试显示文件方案的写入吞吐或查询时延不达用户设定的门槛。
5. 未决：原始日志保留期、租户访问日志查询的时延与最大时间范围（OQ-10）。

## B. 缓存失效候选方案（对应 D-6、FR-79）

- **整站逻辑失效**：缓存键包含「站点缓存版本号」，递增即令旧对象不可命中，旧对象由淘汰机制清理。MVP 采用。
- **前缀/目录失效（候选）**：为路径前缀维护版本号表，缓存键纳入「所属前缀版本号」；需解决前缀层级与匹配开销。
- **缓存键索引（候选）**：节点维护缓存键索引以精确枚举匹配对象；需评估内存与一致性成本。
- **集群级**：失效任务经异步 Task 分发，每节点回执；节点离线时的补偿（上线后补执行）。
- 禁止：不受控地扫描/删除缓存目录。PoC 判据见 FR-79。

## C. Migration 三级分类（对应 R-3、NFR-23）

| 级别 | 定义 | 升级与回滚 |
|---|---|---|
| 可逆 | 提供经测试的降级脚本 | 二进制与数据库可一起回退 |
| 前向兼容（expand-contract） | 新结构只「增」，旧二进制仍可运行；「删」延后到后续版本 | 回退二进制即可；清理步骤单独发布 |
| 不可逆 | 破坏性变更或数据转换 | 升级前强制备份并要求显式确认；回滚=恢复备份+旧二进制，备份点之后数据丢失，须明示 |

## D. 保留 Header 名候选清单（对应 FR-72，最终清单由架构确定）

Host、Content-Length、Content-Type、Transfer-Encoding、Connection、Upgrade、Keep-Alive、TE、Trailer、Cookie、Set-Cookie、Authorization、Proxy-Authorization、Proxy-Authenticate、WWW-Authenticate、Expect、Range（视场景）、以及 `X-Forwarded-Host`、`X-Forwarded-Proto` 等平台自用 Header。

## E. 三个信任域（对应 D-12）

| 信任域 | 保护对象 | 公钥/信任锚分发 | 轮换要点 |
|---|---|---|---|
| 发行清单签名 | 组件包、版本清单 | 内置于 Tools，支持多公钥与吊销列表 | 新旧并存的重叠期；私钥离线保管 |
| 配置版本签名 | 配置版本 | 节点注册时下发公钥 | 重叠期内新旧均可验证；签名不设会使离线节点停服的过期（R-4） |
| 节点 mTLS CA | 节点与主控通信身份 | 私有 CA 根证书随 enroll 下发 | 节点证书可吊销；与签名密钥分离 |

## F. 阶段与基线 Epic 的对应

| PRD 阶段 | 基线 Epic |
|---|---|
| MVP | Epic 1 全部；Epic 3 核心；Epic 2 框架 |
| P2 | Epic 2 余下（CMS）；Epic 3 补强（ACME、缓存失效、通知） |
| P3 | Epic 4 |
| P4 | Epic 5 |
| P5 | Epic 6 |
| P6 | 基线 §22 末段「高级能力」 |

## G. 交给下游的重点

- **架构（bmad-architecture）**：ADR：签名算法与密钥保管（OQ-8）、日志管线、配置下发 gRPC 协议、Scheduler/Worker 的锁/选举、OpenResty 模板白名单与转义机制、缓存失效 PoC、ERD、OpenAPI 草案、契约冻结流程（D-11）。
- **UX（bmad-ux）**：Admin 一级菜单与「未交付模块不渲染」的呈现；官网深蓝科技风；「网络设置 → 真实 IP 传递」页的调试视图；Migration 不可逆升级的确认交互（Tools TUI）。
- **Epic 与 Story（bmad-create-epics-and-stories）**：沿用基线 Epic 1–6；按 §10 将 Epic 2 与 Epic 3 的内容切分到 MVP/P2；每个 Story 带 NFR-3 的 DoD 与「等待环境验证」状态。
