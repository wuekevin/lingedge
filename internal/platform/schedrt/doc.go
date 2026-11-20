// Package schedrt 是 Scheduler 角色的运行时（Story 1.15、ADR-019 §6、FR-29）：cron 注册表 + job_locks 租约选主 + 周期触发。
//
// 两类周期作业（口径见 AA-8）：
//
//   - 任务型（Registry.Task）：触发时以**确定性幂等键** `cron:<作业名>:<窗口起点 unix 秒>` 把 OpenAPI TaskType 入队，
//     交给 Worker 执行。即使两个 Scheduler 因租约重叠同时触发，tasks 的 uk_tasks_idem(type, idempotency_key) 也保证同一窗口只有一条任务。
//     后续 Story 的 heartbeat.sweep / rollup.aggregate / retention.cleanup / cms.publish_due 等用它登记。
//   - 内部型（Registry.Internal）：直接在持有者进程内执行的维护作业（tasks.reap、idempotency.cleanup、sessions.cleanup、partition.maintain）。
//     它们**不属于** TaskType 枚举（AA-8；tasks.type 的 CHECK 也不允许），不入 tasks 表；同一窗口只执行一次靠
//     `job_locks` 名为 `job.<作业名>` 的「窗口租约」：执行前独占获取，完成后把租约保持到下一个触发点（窗口内其他实例获取不到）。
//
// 选主：`scheduler.leader` 行（job_locks）。获取/续约原子（单条条件 UPDATE），每次成功 fence+1；持有者崩溃（kill -9）后租约自然过期，
// 另一实例在 LeaseTTL（+ 至多 LeaseTTL/3 的探测间隔）内接管；优雅停机时主动释放。重叠（旧持有者因停顿未及续约而新持有者已接管）的安全性
// 不依赖选主本身，而依赖上述幂等键/窗口租约。
//
// 时间：租约与窗口一律以数据库 NOW(3) 为准（与 tasks 一致）；触发点由注入的 Clock 与 Schedule 计算（UTC）。
// 重启/换主后只补偿「最近一个已到期窗口」一次（不回放错过的所有窗口）。
package schedrt
