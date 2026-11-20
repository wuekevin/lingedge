# Redis 边界与降级矩阵（ADR-025，Story 1.10）

Redis 只存可重建/可丢失的数据；不可用、被清空、重启、超时，Server 都只降级、不丢数据、不停服。代码在 `internal/platform/redisx`（唯一允许 import `go-redis` 的包，由 depguard 规则 `redis-only-in-redisx` 保证），故障注入测试在 `tests/integration/redis`（连接测试自己拉起的独立临时 `redis-server`，不触碰本机常驻服务）。

> PRD 缺口 F-11：PRD 暂无对应 NFR，建议同步（见 UD-21）。本文的降级矩阵是该缺口的验收依据。

## 1. 降级矩阵与对应测试

| 功能 | Redis 不可用/被清空时的行为 | 封装接口 | 测试（`tests/integration/redis`，除注明外） |
|---|---|---|---|
| API/登录限流 | 回退到进程内固定窗口计数 + 可选 DB 兜底（`users.failed_login_count/locked_until` 由调用方实现 `DBFallback`）；DB 判定拒绝则拒绝；DB 兜底自身故障时仅采用进程内判定并告警；可能略宽松 | `Limiter` | `TestLimiterFaultInjection`（杀进程→降级→重启→回到 Redis）；单测 `TestLimiterDegradedLayers`、`TestLocalLimiterWindow`（`internal/platform/redisx`） |
| 会话/权限校验 | `Cache.GetOrLoad`：miss/故障一律直读真相源（MySQL），结果正确，延迟上升；读到后尽力回填；Redis 恢复或被清空后下一次 miss 自动重建 | `Cache` | `TestCacheFaultInjectionAndSelfHeal`（FLUSHALL、宕机、重启、自愈、真相源错误原样返回）、`TestTimeoutInjection`、单测 `TestCacheDegradedReadsSourceOfTruth` |
| 任务唤醒 | Pub/Sub 视为有损：订阅者必须提供轮询兜底（缺省拒绝订阅），Worker 轮询领取，任务不丢（ADR-019） | `Subscribe` + `Poll` | `TestPubsubLossyWithPollFallback`；单测 `TestSubscribeRequiresPollFallback` |
| 失效广播丢失（权限版本、节点证书吊销） | 靠短 TTL 自然过期 + 订阅者轮询对账；订阅启动时、每个轮询间隔、Redis 恢复时各对账一次 | `Subscribe` + `Poll` | 同上（宕机期间轮询持续、恢复时立即补一次、重启后订阅自动重建） |
| 告警去重 | 回退到 MySQL 告警状态（`alert_events`）；本 Story 只提供 `Cache`/键空间能力，告警模块随 Epic 6 接入 | `Cache` | 能力由 Cache 用例覆盖；接入用例待告警模块 Story |
| 边缘限流/CC | 不受影响（节点本地，数据面不依赖 Redis） | — | `tests/scaffold` 的 `TestDataPlaneHasNoRedisDependency`（依赖图不含 Redis）；门禁 `tests/gates` 自测 |

## 2. 启动与健康

- Server 启动时 Redis 不可达**不阻止启动**：`redisx.New` 返回降级状态的 Client 并记录告警（`TestStartWhileRedisDownThenRecover`）；配置错误（环境名、地址、CA 文件）仍然失败。
- `Client.Status()`（无网络请求）返回 `up|degraded`、起始时间、已脱敏的最近错误、累计降级次数，供 `/healthz/ready`（Story 1.12 接线）报告 `degraded` 而非失败；是否因 Redis 拒绝就绪由配置决定（默认不拒绝，1.12 实现）。
- 后台探测（缺省 2s）在 Redis 恢复后自动转回 `up`；降级期间调用**快速失败**（`ErrUnavailable`，不发网络请求），不被超时拖慢（`TestTimeoutInjection`：暂停进程后 20 次调用 <200ms）。调用方 `ctx` 取消不算 Redis 故障。

## 3. 键空间与写入约束

- 键名 `le:<env>:<模块>:<用途>:<标识>`，租户键 `le:<env>:<模块>:<用途>:t<tenant_uid>[:<标识>]`；**不使用花括号**（旧写法 `le:{env}:…` 在 Cluster 下是 hash tag）。唯一的正则在 `keys.go`，`Keyspace.Parse` 对旧写法返回 `ErrBadKey`（负向用例 `TestKeyNegatives`）。
- 非租户入口 `Keyspace.Key` 拒绝形如 `t<ULID>` 的标识段，租户键只能经 `TenantKey` 构造。
- 租户键必须持有租户范围的 `scope.Scope`（零值/平台范围被拒）并带合法 ULID 的 `tenant_uid`。已知边界：`Scope` 只携带内部数值 id，不含 uid，二者一致性由调用方的 Scope 来源保证。
- 写入必须带正 TTL（`ErrNoTTL`，不发出任何命令，`TestConnectivityTTLAndNamespace` 旁路核对）；值含明文敏感信息（经 `pkg/redact` 判定）返回 `ErrSensitiveValue`。
- Pub/Sub 用途穷尽清单：`cert_revoke`（节点证书吊销）、`perm_version`（权限版本失效）、`task_wakeup`（任务唤醒）；清单外用途被拒（`ErrBadPurpose`）。

- 限流计数键即使被外部去掉 TTL，下一次计数会恢复 TTL（`TestLimiterHealsKeyWithoutTTL`）。
- `Client.Close` 终止仍在运行的订阅（`TestCloseStopsSubscriptions`）。

## 4. 配置与脱敏

`redis.addr`、`redis.db`、`redis.password`（仅环境变量 `LINGEDGE_REDIS__PASSWORD` 或 `redis.password_file`，不得写在配置文件）、`redis.tls`、`redis.tls_ca_file`（绝对路径）、`redis.tls_server_name`。TLS 证书校验不可关闭（无跳过校验入口）。口令是 `config.Secret`：`Config` 的 `%v/%+v/%#v/String`、日志、`Status.LastError`、返回的错误都不含口令（`TestPasswordAuthAndRedaction`、`TestTLS`）。非回环地址上的明文连接是部署者的选择，建议启用 TLS。

## 5. 部署建议

`maxmemory-policy` 建议 `volatile-lru`（所有键都带 TTL）或 `allkeys-lru`；持久化（AOF/RDB）不作为正确性前提，系统在数据被清空后仍须正确。Redis 最低版本约束：封装只用通用命令（字符串/过期/Pub/Sub/Lua `EVAL`），不依赖 Redis 8 新模块；本机验证版本为 redis-server 8.10.1，其他小版本与 Linux 验收机上的版本**等待环境验证**。
