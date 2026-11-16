# ADR-019 Scheduler / Worker：Server 内置角色、MySQL 任务队列与租约围栏

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §2.1、§4.2、§19；PRD FR-21、FR-29、FR-122、D-16、R-5；architecture AD-19、§9.4｜阶段：M1（内置）/ P3（独立二进制）

## 背景

基线 §2.1：Scheduler/Worker 小规模时作为 Server 内置模块，也须支持后续独立部署；FR-29 要求任务幂等、重启可恢复；FR-122 要求内置与独立两种模式行为一致且**任务不重复执行**（机制由架构定）。Task 同时是 API 的异步契约（ADR-017）与运维动作的白名单载体（FR-35）。

## 决策

1. **角色而非模块拆分**：`lingedge-server --roles=api,grpc,scheduler,worker`。`scheduler` 角色负责**触发**周期任务（入队）；`worker` 角色负责**消费**任务。独立二进制 `lingedge-scheduler`/`lingedge-worker`（P3）只是装配不同 `roles` 的同一份代码；内置与独立可同时存在，行为由租约保证一致（D-16、FR-122）。
2. **持久队列在 MySQL**（表 `tasks`，database/platform-identity §4.1）：状态 `pending|running|success|partial|failed|canceled`；`run_after`、`priority`、`attempts/max_attempts`、`lease_owner/lease_until`、`fence`、`idempotency_key`、`parent_task_id`。
3. **领取**：事务内 `SELECT … WHERE status='pending' AND run_after<=NOW(3) ORDER BY priority, id LIMIT n FOR UPDATE SKIP LOCKED`（MySQL 8.0 支持），将 `status='running'`、写 `lease_owner/lease_until`、`fence=fence+1`、`attempts=attempts+1`。多个 Worker 因此天然互斥。
4. **围栏**：Worker 写回结果（进度、状态、result）时必须 `WHERE id=? AND fence=?`；租约过期后被他人重新领取则 `fence` 不同，旧持有者的写回被拒绝（防「僵尸 Worker」覆盖）。执行期间每 `lease/3` 续约。
5. **恢复**：租约过期且仍 `running` 的任务由 `heartbeat.sweep`/回收逻辑置回 `pending`（`attempts` 未超限）或 `failed`（超限）；退避 `run_after = now + backoff(attempts)`。结论：**至少一次执行 + 幂等实现 = 效果恰好一次**；**每个任务类型的处理函数必须幂等**，并以 `idempotency_key`/业务键去重（如 `deployment.publish` 以 `deployment_id`）。
6. **周期任务的触发选主**：`job_locks`（`name, holder, fence, lease_until`）。`scheduler.leader` 租约持有者负责按 cron 表把周期任务（`rollup.aggregate`、`heartbeat.sweep`（心跳超时判定）、`retention.cleanup`、证书续期检查、`cms.publish_due`…）以**确定性幂等键**（如 `rollup.aggregate:<窗口id>`）入队；即使两个 Scheduler 因租约重叠同时入队，`uk_tasks_idem(type, idempotency_key)` 也保证只生成一条。单例任务（如 `migrate`）直接以 `job_locks` 租约互斥。
7. **扇出**：一个父任务（如 `deployment.publish`、`cache.purge_site`）生成子任务/逐节点记录（`deployment_targets`、`cache_task_nodes`），父任务状态由子任务汇总为 `success|partial|failed`。节点侧动作不由 Worker 直接执行，而是写入**逐节点任务队列 `node_tasks`**（`uid`=proto `NodeTask.task_uid`，带 `attempt/lease_until/deadline_at`，database/node-deploy §4.4）由 `TaskService.PullTasks` 下发——`tasks` 无 node 维度，故不能承载逐节点租约/回执；节点离线时保持 `pending` 并在上线后补执行（ADR-007）。**发布编排**：`deployment.publish` 经 `publish.dispatch` 任务按集群串行（ADR-002 §13），`publish.dispatch` 以 `clusters` 行锁互斥、以 `publish_queue` 行为幂等对象。
8. **Redis 仅作唤醒**（ADR-025）：入队后向 `le:<env>:task:wake` 发布（`<env>` 是环境占位符，实际键不含花括号，避免被 Redis Cluster 当作 hash tag，ADR-025 §4）；Worker 同时以固定间隔轮询（默认值待基线测试后确定），丢失唤醒不影响正确性。
9. **白名单任务类型**：`type` 来自代码中注册表，**清单以 OpenAPI `TaskType` 为唯一事实源**（本文与 architecture §9.2、`tasks` 表文档不再另列，避免与新增类型 `node.rollback`、`deployment.retry`、`realip.debug`、`secrets.rewrap`、`media.cleanup_orphans`、`server.cert_rotate`、`publish.dispatch`、`usage.derive` 等漂移，M14）；**不存在接受任意命令字符串的类型**（FR-35）。`payload_json` 不含敏感明文（引用 ID，敏感值运行时经 KeyProvider 解密）。
10. **取消**：`cancel_requested=1` 为协作式，处理函数在检查点读取；已完成的副作用不回滚，由业务自行定义补偿。
11. **租户与审计**：任务带 `tenant_id`、`request_id`、`created_by_user_id`；任务结果与状态变更写 `task_events`；Worker 以任务的租户 `Scope` 执行 Repository 调用（ADR-009），平台任务显式使用平台 Scope。

## Binds / Prevents / Rule

- 约束：所有产生异步工作的模块、Worker/Scheduler 实现、Tools 对独立 Scheduler/Worker 的安装。
- 防止：多实例重复执行；Worker 崩溃后任务丢失；僵尸 Worker 覆盖新结果；以 Redis 为唯一队列导致丢任务。
- 规则（AD-19）：**Scheduler/Worker 为 Server 内角色；任务持久化于 MySQL，租约 + 围栏令牌保证不重复执行；所有任务处理函数必须幂等。**

## 备选方案与权衡

- **Redis 队列（如 asynq 类库）**：否决——Redis 不是真相源（ADR-025），丢失即丢任务；且需要额外持久化运维。
- **外部调度/消息系统（Kafka、NATS、Temporal）**：否决——违背「第一阶段不拆微服务」与最少运维依赖（基线 §2.1）。
- **仅 Redis 分布式锁做选主**：否决——锁不带围栏，GC 停顿/时钟漂移可致双主；MySQL 租约 + `fence` 更可靠，且与数据同库可事务化。
- **每个任务 cron 直接在进程内执行（无队列）**：否决——重启丢失、无法扇出与重试、无审计。

## 后果

- 队列吞吐受 MySQL 限制；M1 的任务量（部署、刷新、续期、聚合）远低于其能力，规模变化时可经基线测试复核。
- `tasks` 表需定期清理（终态 90 天 `[ASSUMPTION]`，由 `retention.cleanup` 完成）。
- 租约时长与轮询间隔的默认值待基线测试后确定；时钟以数据库 `NOW(3)` 为准，避免节点间时钟差。
- 增加「处理函数幂等」的开发纪律，需在 Code Review 与测试中强制。

## 可被推翻处

若基线测试显示 MySQL 队列成为瓶颈：可把 `tasks` 的投递换成外部队列，但保留 MySQL 为状态真相；`job_locks` 与 API 契约不变。

## 验证

- 并发测试：N 个 Worker 竞争同一批任务，每个任务恰好被处理一次；kill -9 Worker 后任务被回收；僵尸 Worker 写回被围栏拒绝。
- 内置 + 独立实例同时运行时周期任务不重复（FR-122）；重复触发同一任务无重复副作用（FR-29）。
- 任务类型白名单测试：注册表之外的 `type` 创建被拒；静态检查不存在 `sh -c`/任意命令执行（NFR-9）。

## 修订记录 2026-11-16

- §7（BLK-06、H10）：逐节点投递改用 `node_tasks`（租约/attempt/回执），`tasks` 只承载 Server 侧作业；补 `publish.dispatch` 与发布串行。
- §8（MIN-06）：Redis 唤醒键 `le:<env>:task:wake`（占位符语义，去掉会被误当 hash tag 的花括号）。
- §9（M14）：任务类型清单改为引用 OpenAPI `TaskType`。
