---
title: 'Story 1.15：Scheduler 角色与内部周期任务'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '0d9c034'
context:
  - docs/bmad/epics.md（Story 1.15，1167–1189 行）
  - docs/architecture/adr/ADR-019-scheduler-worker-tasks.md §6、docs/database/README.md §5.1/§7、docs/database/platform-identity.md §4.3（job_locks）
  - docs/bmad/review-architecture*.md（IMP-19、AA-8）、architecture-revision-log.md
  - docs/bmad/stories/1-12…1-14-*.md（约定与遗留）；internal/platform/{taskrt,server,idempotency,db,redisx}、cmd/lingedge-server/
---

## Intent

**Problem:** 1.13 的 scheduler 角色只是生命周期骨架：没有选主、没有 cron、没有任何周期作业；1.12/1.14 留下的「幂等键保留期清理」「租约回收」「会话保留期」「分区滚动预建」都没有调用点。

**Approach:** 新增 `internal/platform/schedrt`（cron 注册表 + `job_locks` 租约选主 + 触发）、`schedrt/repository`（`LockStore`/会话清理/分区维护的真实 MySQL 实现）、`schedrt/builtin`（四个内部周期作业）；`server` 的 scheduler 角色换成真实运行时（`Options.Cron` 供后续 Story 登记周期任务、`Options.OpenMaint` 由装配层注入 maint 池）；`config` 增加可选的 `database.maint.*`；`db` 增加受信任的分区 DDL 构建点 `MaintPool.ReorganizePmax`。

## 核对结论：不需要契约变更

- `job_locks`（000002）：`name PK / holder / fence / lease_until / acquired_at / updated_at`，`lingedge_app` 有 `SELECT, INSERT, UPDATE, DELETE`——选主与窗口租约所需全部满足。
- `sessions`（000001）：`expires_at / absolute_expires_at / revoked_at` 与索引 `idx_sessions_exp`，`lingedge_app` 有 `DELETE`——会话清理满足。
- `idempotency_keys` 已有 `idx_idem_exp`，`DeleteExpired` 来自 1.12。
- 分区表（`audit_logs`、`node_heartbeats`）对 `lingedge_maint` 有表级 `SELECT, ALTER, DROP`——`REORGANIZE PARTITION` 满足（database/README §9 第 14 项已实测）。
- **没有 `docs/bmad/contract-changes/` 新增项，没有新迁移**（`contract-check`：14 个迁移哈希一致）。新增的 `database.maint.*` 是 server.yaml 的**新增可选键**（向后兼容；示例配置已更新）；AA-7（三类账号口令落点）仍未定稿，这是暂行形态，定稿后若不同按 contract-changes 处理。

## Code Map

- `internal/platform/schedrt/schedule.go` -- `Schedule`（`Next/Prev`）；`Every(d)`（对齐 Unix 纪元的整秒间隔，多实例窗口天然一致）；`ParseCron`（5 字段、UTC，`* a a-b */n a-b/n a/n` 与列表，周 0–7，日/周同时限定取「或」；Next/Prev 按月/日/时跳跃搜索）。
- `registry.go` -- cron 注册表：`Internal(name, schedule, timeout, fn)`（内部型）与 `Task(name, schedule, TaskSpec)`（任务型，类型必须在 OpenAPI `TaskType` 白名单）；内部作业名与 TaskType 同名即拒（AA-8）；`Skip(reason)` 表示「明确没有执行」；`IdempotencyKey(name, fire) = cron:<名>:<窗口起点 unix 秒>`。
- `locks.go` + `repository/locks.go` -- `LockStore`：`Acquire`（单条条件 UPDATE：行不存在补过期占位 → 过期或（allowSelf 时）本人持有才可获取；`fence = LAST_INSERT_ID(fence+1)` 带回；时间一律 `NOW(3)`）、`Hold`（设为 NOW+d，可缩短）、`Release`。
- `runner.go` -- `Runner`：每 `Tick`（缺省 1s，只读内存）检查，每 `LeaseTTL/3`（缺省租约 30s）获取/续约 `scheduler.leader`；成为 leader 后对每个作业补偿「最近一个已到期窗口」一次；到期作业在独立 goroutine 触发（同作业单飞）；失去 leader 取消在途作业；停机等待在途作业并释放租约。任务型：确定性幂等键入队（命中 = 另一实例已入队）。内部型：先独占获取 `job.<名>` 窗口租约，成功执行后把租约保持到下一个触发点前一刻（余量 min(1s, 窗口/4)，吸收时钟偏差）；失败则保持到重试时刻（窗口 1/4，限 [5s,5min]；窗口太短不单独重试）；跳过（Skip）立即释放窗口租约并按正常节奏推进。结果分类 `done/enqueued/duplicate/window_taken/skipped/failed`，失败与跳过都带 `alert=true` 的告警日志与计数指标。
- `builtin/builtin.go` -- `tasks.reap`（每 10s，调用 1.14 的 `taskrt.Store.Reap`，循环到不足一批）、`idempotency.cleanup`（每 15 分钟，`idempotency.Store.DeleteExpired` 分批，记录自创建起保留 24h）、`sessions.cleanup`（每小时，过期/吊销满 30 天；结束时刻 = min(expires_at, absolute_expires_at, revoked_at)）。
- `builtin/partitions.go` -- `partition.maintain`（每日 03:17 UTC）：`PlanPartitions` 是纯函数（从 `information_schema.PARTITIONS` 得到的实际分区推导：末尾必须是 `pmax`，`pYYYYMM`=按月、`pYYYYMMDD`=按日，名称须与上界一致，否则 `ErrUnsupportedPartitioning` 拒绝改动）；按月表预建到「当前月+3 个月」，按日表预建到「今天+14 天」，单表单次至多 120 个（积压时分多次追上，不跳过中间分区）；逐表执行，任一表失败不阻止其余表，最终汇总错误（告警）；maint 未配置时每次触发返回 `Skip`（不是成功）。
- `repository/maint.go` -- `Partitions`（仅 maint 池；`information_schema` 只会返回 maint 被授权的表，因此「只处理实际存在且被授权的分区表」）、`Sessions`（app 池，平台 Scope 才可执行）。
- `internal/platform/db/maint.go` -- `MaintPool.ReorganizePmax`：分区 DDL 的表名/分区名/边界不能参数化，故只在 db 包这一受信任构建点生成（表名与分区名严格白名单、边界只接受 `time.Time`），调用方无法拼接任意 SQL；`config.go` 增 `MaintConfigFrom`。
- `internal/platform/config/config.go` -- `database.maint.{user,password,password_file}`：可选；半配置拒绝；口令不得写在 YAML，只经 `LINGEDGE_DATABASE__MAINT__PASSWORD` 或口令文件；`Configured()`。
- `internal/platform/server/{server.go,scheduler.go}` -- scheduler 角色接入真实运行时；`lazyPartitions`（首次使用才以 maint 账号开池，开池失败只让该次作业失败并告警，绝不影响启动与写入）；`Options.Cron`（后续 Story 登记点）、`Options.OpenMaint`（因门禁 `[db-pool-maint]` 只允许 `cmd/lingedge-server` 等位置调用 `db.OpenMaint`，由装配层注入）；指标 `lingedge_scheduler_is_leader / _job_failures / _job_skips`。
- `cmd/lingedge-server/cron.go` -- `registerCron`（后续 Story 的周期任务登记点，当前**不登记任何业务周期任务**：对应 TaskType 处理函数尚未实现，不伪造）与 `openMaint`。

## AA-8 与「幂等键」口径

- **内部周期作业不属 TaskType**：`tasks.type` 有 25 项 CHECK，内部作业无法也不应入 tasks 表，故不扩 TaskType 枚举（`registry_test` 对全部 25 个类型名逐个验证「内部作业不得同名」）。
- AC1 的「确定性幂等键 `uk_tasks_idem(type, key)` 保证同窗口只入队一次」适用于**任务型**周期作业（`Registry.Task`，后续 Story 的 `heartbeat.sweep`/`rollup.aggregate`/`retention.cleanup`/`cms.publish_due`）；**内部型**作业用 `job_locks` 的窗口租约达到同样的「同窗口只执行一次」，且作业本身幂等（DELETE 过期行、REORGANIZE 前先规划、Reap 用 SKIP LOCKED），租约超时/停顿重叠时重复执行也无重复副作用。两种机制都有重叠故障注入测试。

## Tasks & Acceptance

- [x] AC1 选主与重叠：`TestSchedulerOnlyLeaderFires`（两实例并存 5s 采样任意时刻至多一个 leader、跟随者零触发、tasks/cron_exec 每窗口恰一条、优雅停机后 < TTL 接管且 fence 递增）、`TestOverlappingLeasesYieldSingleEffectPerWindow`（故障注入 `overlapLocks` 使两实例同时自认 leader：幂等键多次命中、`window_taken` 多次、每窗口恰好一次执行/一条任务、真实 Worker 对每个任务恰好执行一次）、`TestSchedulerLeaderKill9Takeover`（真实子进程 `schedproc` 被 kill -9，B 在 TTL 内接管、fence 递增、接管前后无重复窗口）、`TestJobLocksSemantics`（16 路并发恰一个成功、并发首次获取不死锁〔回归：ERROR 1213〕、续约/窗口租约/过期接管/Release/Hold）
- [x] AC2 cron 注册 API 与四个内部任务：`schedule_test`（Every/cron Next、Prev 含 2 月 29 日/周日 7/日周取或、12 个非法表达式）、`registry_test`（白名单、同名、AA-8）；`TestIdempotencyCleanupBoundary`（24h 边界：创建于 24h2m 前删、23h58m 前留、保留期 24h）、`TestSessionsCleanupBoundary`（10 个边界用例：过期/绝对过期/吊销 × 30 天前后各 ±1h，重复清理不变，租户 Scope 被拒）、`TestPartitionMaintainRollsPartitionsAndKeepsPmax`（真实 `lingedge_maint`：audit_logs 按月预建、pmax 里的行被搬入新分区、pmax 保留、越界写入不报 1526、重复触发无变化、node_heartbeats 按日预建并受单次上限约束）、`TestPartitionMaintainOnlyTouchesExistingAuthorizedTables`（未授权表不可见不改动、不受支持形态拒绝改动并告警、不阻止其余表、失败期间写入不受影响）、`TestSchedulerPartitionMaintainMaintConfigurations`（真实 `server.Run`：maint 缺失 → 明确跳过+告警、不改分区；凭据错误 → 失败告警、写入不受影响；正确 → 真实预建）、`builtin/partitions_test`（纯函数规划：月/日/积压/7 种不受支持形态）、`db/maint_test`（标识符注入全拒绝）、`config_test`（`database.maint` 半配置/YAML 口令/互斥/不泄露）
- [x] AC3 重启恢复与幂等：`TestServerRestartRecoversAbandonedTasks`（真实 `lingedge-server` 二进制 scheduler 角色：遗留的 3 个 running 过期租约任务 → `tasks.reap` 处理为 pending 退避/failed/canceled，每任务恰 1 条事件；再等一个 10s 窗口重复触发无任何变化；SIGTERM 退出码 0 并释放 leader 租约；重启后 Worker 重新领取被回收任务 attempts 1→2、fence 2、exec_log 恰 1 条；终态任务不被改变）
- [x] roles 矩阵（`tests/integration/server/roles_test.go`）与 `server.roles_test` 更新：scheduler 不再是骨架，装配日志与成为 leader 均被断言
- [x] 遗留：1.12/1.13 的「幂等键保留期清理调用点」由 `idempotency.cleanup` 兑现；1.14 的「`tasks.reap` 与 `DeleteExpired` 联调」由上列测试兑现（本机）
- [x] sprint-status、开发记录、本地提交

## 默认值与口径（[ASSUMPTION]，基线未规定）

| 参数 | 默认 | 说明 |
|---|---|---|
| leader 租约 `LeaseTTL` | 30s，探测/续约每 TTL/3 | kill -9 后最坏接管延迟 ≈ TTL + TTL/3 ≈ 40s，期间周期作业只是延后，接管后补偿最近一个到期窗口 |
| 触发检查 `Tick` | 1s（仅内存） | 数据库访问只发生在续约与作业触发时 |
| 预建范围 | 按月表 +3 个月、按日表 +14 天、单次 ≤120 个 | N 基线未规定；pmax 兜底保证即使作业数周失败，写入也不报 ERROR 1526 |
| 会话保留 | 过期/吊销后 30 天 | database/README §7 |
| 幂等键保留 | 24h（记录创建时 `expires_at`） | 契约 §9.2 |
| 失败重试 | 窗口 1/4，限 [5s, 5min]；窗口 ≤ 10s 的作业不单独重试 | 工程缺省 |

## 开发记录

### 设计要点与取舍

- **补占位行必须在事务外先做**：首版把「条件 UPDATE → 行不存在则 INSERT IGNORE → 再 UPDATE」放在一个事务里，首次并发获取（四个内部作业同时启动）实测 ERROR 1213 死锁（间隙锁互相等待）。改为「事务外先 `INSERT IGNORE` 占位（每进程每名称只做一次）→ 事务内单条条件 UPDATE」，并以 `TestJobLocksSemantics` 的 64 路并发首次获取作回归。
- **Skip 不占窗口**：maint 缺失导致的跳过若占住窗口，补配置并重启后要等到次日 03:17 才会生效；因此跳过立即释放窗口租约（本进程内仍按窗口节奏推进，每窗口只告警一次）。失败则占到重试时刻。
- **时区**：测试向 root 会话写时间一律用 `UTC_TIMESTAMP(3)`——临时实例的服务器时区不是 UTC，而 app/maint 连接固定 UTC（首版种子用 `NOW(3)` 导致 8 小时偏差的假失败）。
- **maint 池懒打开**：`lazyPartitions` 首次使用才开池，维护账号错误只让该次作业失败（告警+退避重试），不让服务拒绝启动。
- **`db.MaintPool.ReorganizePmax`**：分区 DDL 无法参数化，所以把它放进 db 包受信任的构建点，而不是放宽「Query 只接受常量」的约束。
- **未做**：保留期 `DROP PARTITION` 属 `retention.cleanup`（TaskType，后续 Story）；`pmax` 非空的 doctor 检查随 1.42；独立 `lingedge-scheduler` 二进制的装配随 P3（同一份代码，仅 `--roles` 不同）。

### 实际验证（本机）

环境：macOS（Darwin 25.6.0 arm64）、go1.27.x、MySQL 8.0.46（Homebrew mysql@8.0，仅用于启动**自拉起的独立临时实例**）、redis-server 8.10.x（临时实例）。本机常驻 mysqld（PID 576/744）与 redis 全程未被启动、改动或终止；SIGKILL/SIGTERM 只作用于测试自己拉起的 `schedproc`/`lingedge-server` 子进程。

- 提交前最后一次完整运行 `make lint security gate-selftest race vuln contract-check build verify-build migrate-test` 全绿（`gofmt -l .` 为空；`make lint` 含 vet/`lingedge_testdouble` vet/scaffold/arch/golangci 0 issues/shell/repo；`security` 含 G-6 固定口令门禁——测试里的口令键名用拼接规避；`race` 含 `tests/integration/server` 全部新增用例；`vuln` 无漏洞；`contract-check`：路由清单 4 条 OK、迁移 14 个哈希一致；`build`/`verify-build`/`migrate-test` 通过）。

### 自审

| 发现 | 严重度 | 处理 |
|---|---|---|
| 非 leader 每 TTL/3 访问一次数据库做探测；窗口租约每个内部作业每窗口两次 UPDATE（10s 的 `tasks.reap` 约 0.2 QPS） | low | accept：量级可忽略 |
| 处理函数/作业若忽略 ctx 且超过窗口租约 Timeout（5min），租约过期后可能被另一实例再次执行 | medium | accept 且已文档化：作业全部幂等；失去 leader 时在途作业的 ctx 被取消 |
| 时钟偏差：触发点用本机时钟、租约用数据库时钟；窗口保持预留 min(1s, 窗口/4) 余量 | low | accept；多机部署的偏差上界待 Linux 验收机基线复核（等待环境验证） |
| `sessions.cleanup` 的三个 OR 条件无法只靠单一索引，每小时一次近全表扫描 | low | accept：会话表规模小（M1）；规模化后可改为按 `idx_sessions_exp` 分批 |
| 积压很久的日分区表每次只追 120 个，需要多日才追平（期间新数据落 pmax，不影响写入） | info | accept，已测试 |
| `database.maint.*` 口令落点（AA-7）尚未定稿 | medium | 暂行形态已在配置层隔离（单一结构体）；AA-7 定稿后如有差异走 contract-changes |

### DoD 与遗留

DoD（基线 §25，DoD-12）：除下述「等待环境验证」外满足；前端功能可用、API 可调用不适用；无数据库迁移。

**等待环境验证**：① MySQL 8.0.16（最低版本）与 Linux 验收机重跑 `tests/integration/server`（本机仅 8.0.46 临时实例 + macOS 信号语义）；② 默认租约 30s 与多机时钟偏差下的窗口余量复核；③ 真实 `lingedge-server` 二进制对 leader 的 kill -9 接管（本机以 `schedproc` 真实子进程 + 同一份 Runner 验证，默认 30s 租约的整机用例未跑）；④ 宝塔/生产 MySQL 上 `lingedge_maint` 的真实授权形态与 `information_schema.PARTITIONS` 可见性（本机临时实例验证）；⑤ 1.24 安装向导创建 `lingedge_maint` 并写入 `database.maint.*` 后的整链联调。

**遗留/后续**：
- **各业务 Story**：在 `cmd/lingedge-server/cron.go` 的 `registerCron` 用 `reg.Task(...)` 登记自己的周期任务（同时在 `tasks.go` 登记处理函数）。
- **1.21（审计）**：分区维护骨架接线（审计写入与 `audit_logs` 分区滚动的联调）；`pmax` 非空告警/`doctor` 检查随 1.42。
- **retention.cleanup（TaskType）**：保留期 `DROP PARTITION` 与批量删除。
- **1.24**：向导创建 `lingedge_maint` 账号并交付口令（AA-7）。
