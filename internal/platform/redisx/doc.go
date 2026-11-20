// Package redisx 是 Server 唯一允许使用 Redis 客户端（go-redis/v9）的位置（Story 1.10、ADR-025）。
//
// Redis 不是真相源：它只存可重建/可丢失的数据（限流计数、读缓存、失效广播、任务唤醒、短期去重），
// 不可用或被清空时只降级、不丢数据、不停服。本包把这条边界落到代码：
//
//   - 键空间（keys.go）：全部键为 le:<env>:<模块>:<用途>:<标识>，租户段 t<tenant_uid> 经 scope.Scope 构造；
//     写入必须带 TTL（无 TTL 返回 ErrNoTTL，且不发出任何命令）；值含明文敏感信息被拒绝（ErrSensitiveValue）；
//   - 连接与降级（client.go）：New 在 Redis 不可达时不失败，而以 Degraded 状态返回并告警；后台探测恢复后自愈；
//     降级期间调用快速失败（ErrUnavailable），不被超时拖慢；调用方的 ctx 取消不算 Redis 故障；
//   - 三类接口（limiter.go / cache.go / pubsub.go）：Limiter（Redis→进程内→DB 兜底）、Cache（miss/故障→直读真相源，
//     恢复后自动重建）、Pub/Sub（有损；订阅者必须提供轮询兜底，否则拒绝订阅）；
//   - 配置（config.go）：口令是 config.Secret，TLS 证书校验不可关闭；任何错误/日志都不含口令。
//
// 降级矩阵（与 ADR-025 §5 对应，测试见 tests/integration/redis）：
//
//	API/登录限流  → 进程内限流 + DB 兜底（可能略宽松，告警）
//	会话/权限校验 → Cache 直读真相源（延迟上升，结果正确）
//	任务唤醒      → 订阅者轮询兜底，任务不丢
//	失效广播丢失  → 依赖 TTL 与订阅者轮询自然收敛
//	边缘限流/CC   → 不受影响（节点本地，数据面不依赖 Redis）
package redisx
