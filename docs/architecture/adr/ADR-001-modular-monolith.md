# ADR-001 模块化单体与 MVC + Service + Repository

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §2.1、§3.1、§20；PRD FR-16、FR-29、D-1、D-16、NFR-4；architecture AD-1｜阶段：M1

## 背景

基线 §2.1 规定控制平面第一阶段为**模块化单体**，架构为 MVC + Service + Repository + Domain Modules；Node/DNS/Reporter 独立二进制；Scheduler/Worker 小规模内置、后续可独立；**不得无必要拆分微服务**。PRD D-1 无规模数据，故不为未知规模付复杂度。风险：单体长期演化为"大泥球"，或并行开发的多个模块互相穿透。

## 决策

1. **单一 Go 进程 `lingedge-server`、单一代码库、单一 MySQL 库**，按 Domain Module 切分（目录见 architecture §5.2）。
2. **模块内分层**：`handler`（Gin/grpc，对应 MVC 的 C 与视图 DTO）→ `service`（用例、事务边界、审计、事件）→ `repository`（sqlx，唯一接触 `*sqlx.DB/Tx`）→ MySQL；`domain` 为纯领域对象与规则。
3. **模块对外契约 = 根包 `internal/<mod>`**：该包**只含** `api.go`（`Service` 接口与 DTO）；实现全部放在 `internal/<mod>/internal/{handler,service,repository,domain}`，由 Go `internal` 规则在**包粒度**强制——其他模块只能 import `internal/<mod>` 根包（architecture §5.2 的目录与此一致；“`api.go` 是唯一可被 import 的文件”的说法作废，Go 以包为粒度无法限制文件，L21）。
4. **依赖方向**（architecture §5.3 图）：`platform/pkg ← auth,tenant,plugin ← website(站点聚合根) ← origin,cache,ruleengine,certificate,waf` 等；L2 之间互不依赖（仅指向 `website` 做站点归属校验）；`node/cluster`（修订、部署）不 import 任何 L2 业务模块，经 `pkg/edgeconfig.ConfigContributor` 依赖倒置汇编配置，装配在 `cmd/lingedge-server` 的 `wire` 层（手写，不引入 DI 框架）。
5. **跨模块协作**：同步 → 经 **`platform/ports` 接口**（见下表）调用对方 `Service`；异步/广播 → `platform/eventbus`（进程内）；需可靠投递（计费、通知）→ MySQL `event_outbox`（Worker 消费）。

   **ports 清单（IMP-15）**——依赖图只允许 L2 指向 `website`（站点归属校验）与 `platform`，但文档化的必需路径还包括 node→certificate/website/origin/monitoring/cache、cache→node、monitoring→website、ruleengine→origin/cache、cms→billing。为不留“临时豁免”，这些路径一律经定义在 `platform/ports`（仅接口与 DTO，无实现）的端口：**调用方只依赖 `platform/ports`，实现方在 `cmd/lingedge-server` 装配层注入**，因此编译期依赖图只增加“→ `platform/ports`”这一条所有模块都允许的边，depguard 规则可由此机械生成：

   | 端口 | 实现方 | 调用方 | 用途 |
   |---|---|---|---|
   | `CertificateProvider` | certificate | node（`FetchCertificate`） | 取证书链、解密私钥；判定“证书被该节点所属集群某站点引用”（联合 `SiteDirectory`） |
   | `SiteDirectory` | website | node、cache、monitoring、certificate、ruleengine、origin | 站点存在且属于租户、站点↔绑定集群、集群→站点列表与主机名集合、`site_uid`↔`site_id` |
   | `NodeDirectory` | node / cluster | cache、monitoring、scheduler、website（绑定校验） | 集群→节点、节点→集群、在线/维护状态 |
   | `NodeTaskDispatcher` | node | cache、website（真实 IP 调试） | 创建逐节点任务（`node_tasks`，ADR-019） |
   | `OriginHealthSink` | origin | node（`ReportOriginHealth`） | 写 `origin_health` |
   | `RollupSink` | monitoring | node（`ReportRollups`） | 写 `metric_rollups`/`rollup_windows`（ADR-008） |
   | `LogSink` / `LogQuery` | monitoring | node（`UploadAccessLogs`）、monitoring | 访问日志分片读写（ADR-008） |
   | `OriginGroupLookup` | origin | ruleengine | 校验规则动作引用的源站组属于该站点 |
   | `CachePolicyLookup` | cache | ruleengine（KCR 校验，ADR-005 §3.7） | 取缓存策略与键维度集合 |
   | `PlanCatalog` | billing | cms | 套餐只读目录（P5 前返回空集合） |
   | `ConfigContributor`（在 `pkg/edgeconfig`） | website/origin/cache/ruleengine/certificate/waf/l4proxy | node（修订构建器） | 汇编各自配置块（既有） |
6. **事务**：边界在 Service 方法；事务经 `context` 传递（`platform/db.WithTx`），子调用复用；禁止 Handler 与 Repository 直接开事务。
7. **独立二进制边界**：`cmd/lingedge-node|dns|reporter` 只依赖 `pkg/`、`proto/`、`edge/`；**不得 import `internal/*` Server 业务包**。`internal/tools` 与 Server 无编译依赖。
8. **运行角色**：`--roles=api,grpc,scheduler,worker`；独立 `lingedge-scheduler/worker` 仅装配不同角色（ADR-019）。
9. **强制手段**：`depguard`/`go-arch-lint`（实现时核对选型）把上述规则写成 CI 规则；Repository 外出现 `sqlx.DB` 使用即失败（与 ADR-009 共用）。

## Binds / Prevents / Rule

- 约束：所有 Server 内模块、`cmd/*` 装配、CI；`platform/ports` 的接口清单变更须评审（它是模块间契约）。
- 防止：模块互相穿透数据表；Node/DNS 二进制把 Server 业务代码拖进来导致体积与攻击面扩大；并行开发的多个 Story 各自在 Handler 里写 SQL/事务。
- 规则（AD-1）：**模块只经 `api.go` 的 Service 接口或事件互访；依赖单向；SQL 只在 Repository；独立二进制不 import Server 业务模块。**

## 备选方案与权衡

- **微服务（每模块一服务）**：否决——违背基线"不得无必要拆分"；运维复杂度与分布式事务成本不匹配 D-1。
- **单体 + 扁平包（不设模块边界）**：否决——并行开发易漂移，后续拆分 Scheduler/Worker/DNS 成本高。
- **DDD 六边形 + 严格 CQRS**：否决——超出基线指定的 MVC+Service+Repository，学习与样板成本高；保留 `ports` 做依赖倒置已足够。
- **使用 DI 框架（wire/fx）**：暂不采用，手写装配更易审查；若装配代码膨胀可后续引入（可推翻）。

## 后果

- 需要维护 `api.go` 与 DTO 转换，样板增多；换来可检查的边界。
- `node` 模块通过 Contributor 注册汇编配置，调试链路比直接调用长；用"配置汇编单元测试 + golden 文件"补偿。
- 单体意味着整体发布；以 expand-contract 迁移与 Tools 的升级回滚（ADR-013）降低风险。

## 可被推翻处

用户给出明确规模目标（D-1）并要求早期拆分日志/调度/DNS 时，可把对应角色拆为独立二进制（已有边界）；不影响本 ADR 的依赖规则。

## 验证

- CI 架构规则：违反依赖图、Repository 外使用 `*sqlx.DB`、`cmd/lingedge-node` import `internal/*` 业务包均使构建失败。
- 单进程启动即可跑通 MVP 全部流程（FR-16 AC）。

## 修订记录 2026-11-16

- §3（L21）：对外契约改为**根包 `internal/<mod>`（仅 `api.go`）+ 实现在 `internal/<mod>/internal/…`**，与 architecture §5.2 统一；删除“`api.go` 是唯一可被 import 的文件”的不可执行说法。
- §5（IMP-15）：新增 **ports 清单**（CertificateProvider、SiteDirectory、NodeDirectory、NodeTaskDispatcher、OriginHealthSink、RollupSink、LogSink/LogQuery、OriginGroupLookup、CachePolicyLookup、PlanCatalog）及“调用方只依赖 `platform/ports`、实现在装配层注入”的规则。原因：原依赖图禁止 node→L2、L2↔L2，但 gRPC `FetchCertificate`、`ReportOriginHealth`、`ReportRollups`、缓存任务派发、`LogQuery` 站点校验、规则引用源站/策略、CMS 读套餐都必需跨模块调用，depguard 按原图生成会使这些路径无法编译，只能临时豁免，边界规则形同虚设。
