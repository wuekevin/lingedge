---
title: 'Story 1.14：任务运行时（tasks 队列 / 租约 / 围栏 / 白名单任务类型）'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '97c7021'
context:
  - docs/bmad/epics.md（Story 1.14 全文，1142–1167 行）
  - docs/architecture/adr/ADR-019-scheduler-worker-tasks.md、docs/database/platform-identity.md §4.1（tasks 表）
  - docs/api/openapi.yaml（Task/TaskType、/tasks* 的 x-* 元数据）、docs/bmad/review-architecture*.md 的 M14 / AA-8
  - docs/bmad/stories/1-10…1-13-*.md（约定与遗留）；internal/platform/{httpx,idempotency,redisx,tenantscope,db,server,apierr}、cmd/lingedge-server/assembly.go
---

## Intent

**Problem:** 部署/缓存/证书/升级这类耗时操作都要「返回 Task ID、可查询、可恢复、可幂等」，但 1.13 的 worker 角色只是生命周期骨架：没有任务类型白名单、没有入队、没有 Worker 领取/租约/围栏/回收，任务中心的 4 个 REST 路由也不存在。

**Approach:** 新增 `internal/platform/taskrt`（类型注册表 + 入队 + Worker + Redis 唤醒）、`taskrt/repository`（`taskrt.Store` 的真实 MySQL 实现）、`taskrt/taskapi`（任务中心 REST，经 `httpx.Register`）、`taskrt/tasktest`（处理函数幂等性测试助手）；`server` 的 worker 角色换成真实运行时；门禁 G-12 增加 `tasktype` 规则。时间一律以数据库 `NOW(3)` 为准（ADR-019）。

## Boundaries & Constraints

**Always:** 任务类型只来自 OpenAPI `TaskType` 白名单且须登记处理函数；Worker 一切写回带围栏 `id=? AND fence=? AND lease_owner=? AND status='running'`；租户 Scope 下他租户任务表现为 404；认证/授权/Scope/审计钩子在生产装配里仍是安全拒绝默认（1.16–1.21 实现），测试用测试专用钩子驱动；测试只用自拉起的独立临时 MySQL/Redis；不改冻结迁移（000001–000014）。

**Never:** Mock 冒充生产；`sh -c`/任意命令类任务类型；在生产装配中放行任何受保护路由；实现 scheduler（1.15）；启动/改动/终止本机常驻 mysqld(576/744) 与 redis；Docker；push；改 git config。

## 核对结论：不需要契约变更

核对 `migrations/000002_platform_core.sql` 的 `tasks`/`task_events` 与 `lingedge_app` 授权：列（`status/priority/payload_json/result_json/error_code/error_message/progress/attempts/max_attempts/run_after/lease_owner/lease_until/fence/idempotency_key/parent_task_id/request_id/created_by_user_id/cancel_requested/started_at/finished_at`）、索引（`idx_tasks_claim(status,run_after,priority)`、`idx_tasks_lease(status,lease_until)`、`uk_tasks_idem(type,idempotency_key)`）、`ck_tasks_type`（25 项，与 OpenAPI 逐项一致，有测试）、`ck_tasks_status` 与 app 的 `SELECT,INSERT,UPDATE,DELETE`（tasks、task_events）均已满足本 Story，另需 `SELECT` 的 `tenants`/`users`（联查 uid）也已授权。**因此没有 `docs/bmad/contract-changes/` 新增项，也没有新迁移**；`contract-check`（迁移 14 个、哈希一致）不受影响。

## Code Map

- `internal/platform/taskrt/types.go` -- `Type` 常量 = OpenAPI `TaskType` 25 项；`KnownTypes/IsKnown`；`ForbiddenTypeReason`（名称含 exec/shell/sh/bash/cmd/command/script/eval/spawn/popen/run 片段或格式非法即拒）；`Status`。
- `registry.go` -- `Registry.Register(Spec)`：不在白名单/带命令语义/重复/处理函数为空均拒；`Handler`/`Run`（`Progress`、`Event`、`Checkpoint`、`CancelRequested`；`NewRun` 供无库单测）。
- `enqueue.go` -- `Enqueuer.Enqueue(ctx, scope, EnqueueRequest)`：未登记类型 → `ErrTypeNotRegistered`；`ValidatePayload`；幂等键（同 type+key 返回既有任务，他租户占用不泄漏）；租户 Scope 强制 `tenant_id`；父任务须对调用方可见；事务内入队不提前唤醒（调用方提交后 `Wake`）。
- `payload.go` -- `ValidatePayload`（敏感键名/敏感值格式一律拒绝，只放行以 `_id/_uid/_ref` 结尾且值为引用的敏感键名）、`RedactJSON`（输出再脱敏）。
- `worker.go` -- `Worker`：领取（并发槽 + 周期回收）、每 `lease/3` 续约、处理函数 panic 捕获、结果/失败/取消/归还写回；Redis 唤醒订阅 + 固定间隔轮询；停机 `DrainTimeout` 后取消处理函数并 `Release`（退还 attempts）。
- `wake.go` -- `RedisWaker`（频道 `le:<env>:task:task_wakeup:all`，ADR-025 清单内用途，发布失败静默降级）、`NopWaker`。
- `store.go` + `repository/` -- `Store` 契约与 MySQL 实现：`Claim`（`FOR UPDATE SKIP LOCKED`、`fence+1`、`attempts+1`、`lease_until=NOW(3)+lease`）、`Renew/SetProgress/AddEvent/Complete/Retry/Release`（全部围栏）、`Reap`（过期租约 → pending 退避 / 超限 failed / 已请求取消则 canceled）、`Insert/Get/List/ListEvents/RequestCancel`（租户 Scope）。状态变更与 `task_events` 同事务。
- `taskapi/` -- `Register(r, store)`：`listTasks`/`getTask`/`listTaskEvents`/`cancelTask`；`Accepted(c, task)`（业务模块的 202 + Task + Location）；`View`：面向租户剔除 `cluster*/node*` 键（R-T1）、payload/result/事件再脱敏。
- `tasktest/` -- `AssertIdempotent`：顺序重投递 + 并发重叠执行后「可观测效果」必须不变（Observe 由调用方对真实库给出）。
- `internal/platform/server/server.go` -- worker 角色接入 `taskrt.Worker`（`Options.Tasks` 注册表；`Deps{Tasks,Enqueuer}` 交给 `Register`）；grpc/scheduler 仍是骨架。
- `cmd/lingedge-server/{routes.go,tasks.go,assembly.go}` -- `registerRoutes(r, deps)` 注册任务中心；`assembleTaskRegistry()` 是处理函数登记的唯一装配点；`assembleHooks()` 仍是零值（安全拒绝）。
- `scripts/gates/tasktype.go`（+ `make security`）-- G-12 任务类型规则；`tests/gates` 自测。
- 测试：`internal/platform/taskrt/**` 单测；`tests/integration/taskrt/`（真实 MySQL/Redis/子进程）；`tests/integration/server` 更新。

## 任务类型注册口径（AC1 / AC7）

- **「本 Story 只注册内核所需类型」**：任务运行时内核自身不需要任何 OpenAPI `TaskType`——内部周期作业（`tasks.reap` 等）不属 TaskType（AA-8，1.15 负责）。因此**生产注册表当前为空**（`assembleTaskRegistry()`），运行时（入队/领取/租约/围栏/回收/取消）已完整可用并有真实库测试；空注册表时 Worker 空闲、不领取不回收，任何类型入队即被拒。各业务 Story 完成时在 `tasks.go` 登记其类型，并把类型追加到 `tasks_test.go` 的 `requiredRegistered`——该测试就是「M1 阶段所需类型在其 Story 完成时均已登记」（M14）的执行点。
- **一致性 CI**：`registry_test.go` 机器核对 `KnownTypes == OpenAPI TaskType`（25 项）与 `== 迁移 000002 的 ck_tasks_type`；`Register` 强制「已登记 ⊆ KnownTypes」；`tasks_test.go` 核对生产注册表 ⊆ OpenAPI 且含 `requiredRegistered`。
- **静态检查（门禁 G-12）**：`go run ./scripts/gates tasktype`（已接入 `make security`）——① 任务类型字面量（`types.go` 常量，及全仓非测试代码中 `taskrt.Spec/EnqueueRequest` 的 `Type`）必须在 OpenAPI 枚举内（`tasktype-unknown`）；② 名称带执行命令语义或含 shell 元字符即失败（`tasktype-command`）；③ 缺少 openapi 事实源不得放行（`tasktype-spec`）。负向样本：`scripts/gates/gates_test.go` 的 `TestTasktypeGate`（8 个负向 + 3 个正向）与 `tests/gates` 自测（clean→违规→清理后恢复）；运行时与门禁的片段表由 `TestTasktypeSegmentsMatchRuntime` 核对一致。`sh -c` 形态仍由既有 `shell` 门禁拦截。

## 默认值与基线数值（AC5「默认值待基线测试」）

| 参数 | 默认 | 依据 |
|---|---|---|
| 租约 `DefaultLease` | 30s（续约每 lease/3 = 10s） | 容忍连续丢失 2 次续约（DB 抖动/GC 停顿）仍不丢租约；kill -9 后最坏恢复延迟 ≈ lease + 1 个轮询周期 ≈ 32s |
| 轮询 `DefaultPollInterval` | 2s | Redis 唤醒丢失/不可用时，入队到被领取的额外延迟上界 ≈ 2s；每 Worker 每 2s 一次走 `idx_tasks_claim` 的空领取 |
| 并发 `DefaultConcurrency` | 4 | 每进程同时执行的任务数 |
| 退避 `DefaultBackoff` | 5s × 2^(attempts-1)，上限 5min | [ASSUMPTION] 工程缺省，基线未规定 |
| 排空 `DefaultDrainTimeout` | 25s（server 内取 min(25s, 停机预算×5/6)） | 小于 `--shutdown-timeout` 30s，留出归还与关闭资源的时间 |

**本机实测**（`TestPollingBaselineMeasurements`，macOS arm64、MySQL 8.0.46 临时实例、库内 3000 条终态任务，300 次采样）：空队列 `Claim`（含 BEGIN/SELECT…SKIP LOCKED/COMMIT）p50≈0.15ms、p95≈0.28ms、p99≈0.30ms；空回收 `Reap` p50≈0.15ms、p99≈0.28ms；Redis 唤醒→被领取 ≈ 27ms（观测粒度 25ms）。结论：空轮询成本为亚毫秒级，即使数十个 Worker 以 2s 间隔轮询也只有个位数 QPS，轮询间隔由「Redis 故障时的可接受额外延迟」决定而非 DB 成本。**这是本机数值，Linux 验收机与生产规模的基线复核属「等待环境验证」。**

## 取消与失败语义

- 取消（`POST /tasks/{id}/cancel`，200 + Task）：`pending` → 直接 `canceled`；`running` → 置 `cancel_requested`，处理函数在 `Checkpoint` 返回 `ErrCanceled` 后终态 `canceled`（续约会把标志同步给运行中的处理函数）；已终态 → 409 `task.not_cancelable`；他租户 → 404。已完成的副作用不回滚（ADR-019 §10）。
- 失败：可重试错误退避重试至 `max_attempts` 后 `failed`（`error_code=task.failed` + 脱敏的 `error_message`）；`taskrt.Permanent(code, err)` 直接 failed；panic 被捕获按失败处理；租约过期超限回收为 `failed`（`task.lease_expired`）。`partial` 由处理函数返回 `Result{Partial:true}`。
- Worker 停机：停止领取 → 排空 → 超时取消处理函数并 `Release`（回 pending，退还本次 attempts），不丢任务、不留租约。

## Tasks & Acceptance

- [x] AC1 白名单：`TestRegistryWhitelist`、`TestKnownTypesHaveNoCommandSemantics`（单测）；`TestEnqueueRejectsUnregisteredTypes`（真实库：未登记/命令类类型入队被拒且不留行；直接 INSERT 命令类类型被 `ck_tasks_type` 拒）；`tasktype` 门禁 + 自测；`sh -c` 由 `shell` 门禁拦截
- [x] AC2 入队 202+Task+Location 与 4 个路由：`TestTaskCenterAPI`（真实库 + 测试专用钩子；全部响应经 kin-openapi 按契约校验：202+Location、列表过滤/分页/排序/400、详情/事件 404 越权、取消 200/403/401/404/409、平台跨租户与代租户）、`TestAcceptedWithIdempotencyKeyReplaysSameTask`（与 1.12 幂等联动，同一 Task）、`TestRoutesMatchOpenAPI`（元数据与 x-* 一致）、`TestTaskRoutesAreDenyByDefaultInRealAssembly`（真实 server 装配下默认钩子一律 401）
- [x] AC3 N Worker 竞争恰好一次：`TestClaimExactlyOnceStoreLevel`（12 路并发领取 120 个）、`TestWorkersProcessEachTaskExactlyOnce`（8 个 Worker×4 并发处理 150 个，exec_log 恰 150 条、attempts=fence=1）、`TestClaimOrderAndRunAfter`；续约 `TestLeaseRenewalKeepsTaskOwned`（处理 2.5s、租约 0.6s，旁观 Worker 持续回收也不被抢）
- [x] AC4 故障注入：`TestKill9WorkerTaskIsReclaimed`（真实子进程 kill -9 → 回收 → attempts=2、fence=2，至少一次执行）、`TestKill9ExhaustedAttemptsFail`（超限 failed）、`TestLeaseExpiryReapRequeuesWithBackoff`、`TestZombieWritebackRejectedByFence`（全部 6 种写回 + 冒用 owner 均 `ErrFenceLost` 且数据零变化）、`TestSIGSTOPZombieProcessCannotOverwrite`（真实僵尸进程 SIGSTOP→接手→SIGCONT，写回被围栏拒绝，结果仍来自接手者）、`TestGracefulShutdownReleasesTask`（SIGTERM 归还）
- [x] AC5 唤醒丢失：`TestRedisWakeAcceleratesClaim`（轮询 20s 时靠唤醒 27ms 领取；对照组无唤醒则 2s 仍 pending）、`TestLostWakeupFallsBackToPolling`、`TestRedisDownPollingStillClaims`（杀 Redis 后仍领取，恢复后自愈）；幂等性助手 `TestAssertIdempotentHelper`（幂等样本通过、「每次追加一行」样本被判失败、缺 Observe 失败）
- [x] AC6 状态与脱敏：`TestFailureSemantics`（重试至成功、超限 failed 含原因、永久失败、panic、错误信息脱敏、partial）、`TestEnqueueRejectsSensitivePayload` + `TestValidatePayload`（11 个负向 + 放行样本）、`TestTaskAPIRedactsStoredSensitiveValues`（库里已有敏感值也不外泄）、`TestViewHidesPlatformIdentifiersFromTenants`
- [x] AC7 注册表一致性：见「任务类型注册口径」（`TestKnownTypesEqualOpenAPITaskType`、`TestKnownTypesEqualMigrationCheck`、`TestTaskRegistryAssembly`）
- [x] 遗留：1.12/1.13 的「幂等键保留期清理 `idempotency.Store.DeleteExpired` 调用点」登记为 **Story 1.15（Scheduler 内部周期作业，与 `tasks.reap` 同类）**；1.14 不实现 scheduler，`assembly.go`/`idempotency.go` 注释已更正
- [x] sprint-status、开发记录、本地提交

## Verification

`gofmt -l .` 为空；`make lint security gate-selftest race vuln contract-check build verify-build migrate-test` 全绿（结果见「开发记录」）。

## Spec Change Log

- 无契约变更：OpenAPI/proto/迁移均未改动（见「核对结论」）。`Makefile` 的 `security` 目标追加 `tasktype` 门禁；`tests/gates` 自测副本追加 `docs/api/openapi.yaml`（tasktype 的事实源）。

## 开发记录

**开发会话**：2026-11-21，bmad-build（非交互、完全自主）。环境：macOS（Darwin 25.6.0 arm64）、go1.27.4、MySQL 8.0.46（Homebrew mysql@8.0，仅用于启动**自拉起的独立临时实例**）、redis-server 8.10.1。本机常驻 mysqld（PID 576/744）与 redis 全程未被启动、改动或终止；SIGKILL/SIGSTOP/SIGCONT 只作用于测试自己拉起的 `workerproc` 子进程与临时 redis。

### 设计要点与取舍

- **围栏的完整含义**：写回条件是 `id AND fence AND lease_owner AND status='running'`。回收（Reap）不改 fence（只把 running 改回 pending/终态），所以旧持有者在「已回收未重领」窗口因 `status` 不符被拒，重领后因 `fence` 不符被拒；`fence` 在每次领取时 +1。驱动启用 `clientFoundRows`，「0 行 = 条件不满足」成立（相同值的 UPDATE 不会被误判为围栏丢失）。
- **回收放在 Worker 里**：Worker 每个轮询周期顺带 `Reap`（走 `idx_tasks_lease`，空转 ≈0.15ms），所以仅有 worker 角色的部署也能从 kill -9 恢复，不依赖 Scheduler。1.15 的 `tasks.reap` 内部周期作业调用同一个 `Store.Reap`，两者并发安全（`FOR UPDATE SKIP LOCKED`）。
- **FIND_IN_SET 取代动态 IN 列表**：`db.Q` 只接受编译期常量，已登记类型是运行期集合，所以用 `FIND_IN_SET(type, ?)` 传逗号串（类型名格式校验保证不含逗号）；`idx_tasks_claim` 仍由 `status/run_after` 命中。
- **事务内入队**：`Enqueuer.InTx` 探测调用方事务，事务内入队不立即唤醒（提交前唤醒无意义），由调用方提交后 `Wake`；唤醒丢了也由轮询兜底。
- **payload 拒绝而非静默脱敏**：入队方带明文说明调用方有缺陷，应暴露；API 输出再经 `redact` 一遍作为纵深防御（历史数据、处理函数写出的 result 也不外泄）。命名惯例：对敏感对象的引用用 `*_uid/*_id/*_ref`（如 `credential_uid`），运行时经 KeyProvider/Repository 取明文。
- **`resource` 约定**：Task.resource 与 `resource_uid` 过滤取自 payload 的 `resource_type`/`resource_uid`（tasks 表没有资源列，不为此改冻结迁移）。
- **生产注册表为空**：见上「任务类型注册口径」。测试登记 `retention.cleanup`/`cache.purge_url` 只存在于测试注册表。
- **`tasktest` 不 import `testing`**：非测试代码不得 import `testing`（depguard，ADR-023 §A.6），故声明最小接口 `tasktest.TB`，`*testing.T` 直接满足。
- **门禁 G-6 对测试里的敏感样本也生效**：测试里的敏感样本一律运行期拼接（`"pass"+"word"` 之类），不在仓库里留固定口令字面量。

### 实际验证（本机）

- 单测：`taskrt`（白名单/命令类拒绝、KnownTypes==OpenAPI==迁移 CHECK、payload 脱敏 11 个负向）、`taskapi`（元数据==openapi x-*、默认钩子 401、租户视角剔除 cluster*/node*）、`cmd/lingedge-server`（注册表装配、路由清单恰 4 条）、`scripts/gates`（tasktype 8 负向 + 3 正向 + 片段表一致）。
- 集成（`tests/integration/taskrt`，真实临时 MySQL/Redis + 真实 `workerproc` 子进程，无 Mock；`-race` 连跑两次通过，约 60s/次）：见 AC 对照。`tests/integration/server` 更新 roles 矩阵（worker 不再是骨架）并新增「真实装配下任务路由默认 401」。
- 提交前最后一次完整运行 `make lint security gate-selftest race vuln contract-check build verify-build migrate-test`，退出码 0：`gofmt -l .` 为空；`make lint`（vet 含 `lingedge_testdouble` 标签、scaffold、arch-check、golangci-lint 0 issues、shell-check、repo-check）全部通过；`make security`（含新增 `gates tasktype`）通过；`make gate-selftest`（`scripts/gates`、`tests/gates`）通过；`make race` 全部包 ok（含 `tests/integration/taskrt`、`tests/integration/server`、`tests/migrations`）；`make vuln`：No vulnerabilities found；`make contract-check`：路由清单 OK（运行时已注册 4 条路由，均在 openapi.yaml 中且元数据一致）、openapi-breaking OK、migrations-check OK（14 个迁移哈希一致，仅追加）、freeze-check OK；`make build`、`verify-build`、`migrate-test` 通过。

### 自审

| 发现 | 严重度 | 处理 |
|---|---|---|
| Worker 在 HTTP 监听排空期间仍会继续领取新任务（worker 组件在监听关闭之后才停止） | low | accept：不影响正确性（任务持久、停机时归还）；如需更早停止领取可在 1.15 之后把 worker 停止提前到监听关闭之前 |
| 处理函数若忽略 ctx 且超过租约，会在新持有者接手后继续执行副作用（僵尸执行） | medium | accept 且已文档化：围栏只能阻止「写回」而不能阻止「副作用」——这正是处理函数必须幂等、副作用须以业务键去重的原因（ADR-019 §5）；`tasktest.AssertIdempotent` 提供重叠执行检查 |
| `FIND_IN_SET` 无法用 `type` 做索引过滤 | low | accept：`idx_tasks_claim(status,run_after,priority)` 已能把扫描限制在到期 pending 行；实测空领取 p99≈0.3ms |
| 平台任务 `tenant_id` 为 NULL，`tenantdb` 构建器（要求租户列）不适用，Repository 用常量 SQL + 显式可见性条件（`(? = 1 OR t.tenant_id = ?)`） | low | accept：跨租户越权有真实库负向测试覆盖（详情/事件/取消/幂等键/父任务） |
| 列表的 `sort` 白名单目前只有 `created_at` | info | accept：与 OpenAPI「可排序字段因资源而异」一致，后续按需追加 |
| 生产注册表为空，Worker 空闲 | info | 设计如此，见注册口径；不伪造处理函数 |

### DoD 与遗留

DoD（基线 §25，DoD-12）：除下述「等待环境验证」外满足；前端功能可用不适用；无数据库迁移。

**等待环境验证**：① MySQL 8.0.16（最低版本）与 Linux 验收机上重跑 `tests/integration/taskrt`、`tests/integration/server`（本机仅 8.0.46 临时实例 + macOS 信号语义）；② 默认租约 30s / 轮询 2s 在 Linux 与生产规模下的基线复核（本机数值仅作参考）；③ 1.16–1.21 真实认证/授权/Scope/审计钩子装配后，任务中心 4 路由（含取消的 `x-audit`）的整链端到端；④ 1.15 接入 Scheduler 后 `tasks.reap` 与幂等键保留期清理的联调。

**遗留/后续**：
- **1.15**：Scheduler 角色与内部周期作业——含 `tasks.reap`（调用 `Store.Reap`）与 **`idempotency.Store.DeleteExpired` 的调用点**（1.12/1.13 遗留，在此登记）；周期作业用确定性幂等键入队（`Enqueuer` 已支持）。
- **各业务 Story**：在 `cmd/lingedge-server/tasks.go` 登记自己的 TaskType 处理函数，并把类型追加到 `tasks_test.go` 的 `requiredRegistered`（M14）；入队后用 `taskapi.Accepted` 返回 202；处理函数用 `tasktest.AssertIdempotent` 写幂等性测试。
- **1.16–1.21**：在 `assembleHooks()` 填入真实钩子；任务中心路由届时按 `x-permissions`（`task:read`/`task:cancel`）与 `x-audit` 生效。
