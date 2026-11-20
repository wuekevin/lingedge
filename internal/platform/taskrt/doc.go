// Package taskrt 是任务运行时（Story 1.14、ADR-019、FR-21/29/35）：MySQL 持久队列（表 tasks）上的白名单任务类型注册表、
// 入队、Worker 领取/租约/围栏/回收，以及 Redis 唤醒（仅加速，丢失由轮询兜底）。
//
// 保证与边界：
//
//   - 白名单：任务类型只能来自 types.go 的 KnownTypes（= OpenAPI TaskType 25 项的镜像，CI 校验一致），且须经 Registry.Register
//     登记处理函数；未登记类型入队即拒（ErrTypeNotRegistered）。不存在「执行任意命令」的类型：名称含 exec/shell/cmd/script… 的类型被拒，
//     门禁 G-12 的 tasktype 规则对源码做同样的静态检查。
//   - 领取：事务内 SELECT … FOR UPDATE SKIP LOCKED，写 lease_owner/lease_until、fence=fence+1、attempts=attempts+1；时间以数据库 NOW(3) 为准（ADR-019）。
//   - 围栏：Worker 的一切写回（续约、进度、事件、终态、释放）都带 `id=? AND fence=? AND lease_owner=? AND status='running'`；
//     租约过期被别人重新领取后 fence 已变，僵尸 Worker 的写回返回 ErrFenceLost，不改变任何数据。
//   - 恢复：租约过期且仍 running 的任务被回收为 pending（退避 run_after）或超限 failed；Worker 每个轮询周期顺带执行回收，
//     1.15 的 Scheduler 还会把同一回收逻辑登记为内部周期作业 tasks.reap（内部周期作业不属 OpenAPI TaskType，AA-8）。
//   - 语义：至少一次执行 + 处理函数幂等 = 效果恰好一次；tasktest.AssertIdempotent 是随框架提供的幂等性测试助手。
//   - 取消：协作式（cancel_requested），处理函数在检查点调用 Run.Checkpoint；pending 任务取消即终态。
//   - 敏感信息：payload_json 只存引用（ID/uid），入队时 ValidatePayload 拒绝敏感键名/敏感值格式；API 输出再经 redact 清洗一遍。
package taskrt
