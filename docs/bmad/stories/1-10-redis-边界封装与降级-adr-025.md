---
title: 'Story 1.10：Redis 边界封装与降级（ADR-025）'
type: 'feature'
created: '2026-11-20'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'e222bc9'
context:
  - docs/bmad/epics.md（Story 1.10 全文，约 1043–1067 行）
  - docs/bmad/decisions-adopted.md（UD-21）
  - docs/architecture/adr/ADR-025-redis-boundary.md（及 ADR-003 §6、ADR-009 §10、ADR-019、ADR-022）
  - docs/architecture/architecture.md（§9、F-11）、docs/architecture/dev-environment.md
  - docs/bmad/stories/1-4-…、1-5-…（config/logx/Secret/Scope 与门禁约定）
---

## Intent

**Problem:** 限流/缓存/广播即将接入 Redis，但 Redis 是「可丢失」依赖（ADR-025）：若各模块各自用客户端，键名、TTL、脱敏、降级会各不相同，Redis 故障就会演变成停服或数据丢失。

**Approach:** 在 `internal/platform/redisx` 提供唯一的 Redis 封装层（键空间/TTL/脱敏/降级/自愈）与 `Limiter`/`Cache`/Pub-Sub 三类接口；用 depguard 规则限定 go-redis 只能出现在该包，用依赖图测试证明数据面零 Redis 依赖；用测试自己拉起的独立临时 redis-server 做真实故障注入，降级矩阵既有文档表（`docs/operations/redis-degradation.md`）又有对应测试。

## Boundaries & Constraints

**Always:** Redis 不是真相源，丢失只降级；键必带 TTL；值不含明文敏感信息；口令/TLS 材料不进日志与错误；Redis 不可达不阻止启动；测试只用独立临时实例（独立端口/目录、无持久化），结束即关闭并清理。

**Never:** 启动或改动本机常驻 Redis/MySQL；Docker；Mock/miniredis 冒充 Redis；数据面依赖 Redis；push；改 git config；改其他产品目录。

## Code Map

- `internal/platform/redisx/` -- `keys.go`（`Keyspace`/`Key`：唯一键名正则，租户键经 Scope + ULID）、`config.go`（`Config`、TLS 校验不可关闭、`FromConfig`）、`client.go`（`New` 不可达不失败、状态机 Up/Degraded、后台探测自愈、快速失败、`Set/Get/Del/TTL/Ping/Status`）、`limiter.go`（Redis Lua 固定窗口 → 进程内 → `DBFallback`）、`cache.go`（`GetOrLoad`/`Invalidate`）、`pubsub.go`（用途白名单、`Publish`、必须带 Poll 兜底的 `Subscribe`，panic 捕获）。
- `internal/platform/config/config.go` -- `redis.tls`、`redis.tls_ca_file`、`redis.tls_server_name`（含校验与单测）；`configs/server.example.yaml` 同步。
- `tests/redistest/` -- 独立临时 redis-server（Kill/Stop/Restart/Pause/Resume、TLS 证书运行时生成、requirepass、遗留实例回收）。
- `tests/integration/redis/` -- 真实 Redis 集成与故障注入测试。
- `scripts/gates/{spec.go,genlint.go}`、`.golangci.yml`（生成）、`tests/gates`、`tests/scaffold` -- `redis-only-in-redisx` 规则及正反自测；数据面依赖图测试。
- 文档：`docs/operations/redis-degradation.md`（降级矩阵）、`docs/bmad/traceability.md`（F-11 标注）、`internal/platform/doc.go`。

## Tasks & Acceptance

- [x] redisx 键空间/Client/Limiter/Cache/Pub-Sub（AC1、AC2、AC3、AC7）
- [x] config Redis TLS 字段与校验
- [x] redistest 独立临时实例与故障注入（AC4）
- [x] 集成测试：连通、TTL、命名空间、宕机/清空/重启/超时、口令与 TLS 脱敏、Pub/Sub 兜底、并发混沌
- [x] 门禁与依赖图：redis-only-in-redisx、数据面零依赖（AC5）
- [x] 文档：降级矩阵、traceability F-11（AC6）
- [x] sprint-status、开发记录、评审、本地提交

## Verification

`make lint security gate-selftest test race vuln contract-check build verify-build migrate-test` 全绿；`go test -race ./tests/integration/redis/` 连续多次通过。

## Spec Change Log

无（规格未变更）。

## Review Triage Log

见「开发记录 · 评审」。

## 开发记录

**开发会话**：2026-11-20，bmad-build（非交互、完全自主）。环境：macOS（Darwin 25.6.0 arm64）、go1.27.4、redis-server v=8.10.1（Homebrew，仅由 `tests/redistest` 拉起独立临时实例：回环随机端口、临时目录、`--save "" --appendonly no`）。开工前 `pgrep -fl redis` 无进程，本机常驻 mysqld PID 576/744；收尾核对两者不变，无 `leredis*` 临时目录残留。

### 范围

- `internal/platform/redisx`：键空间（唯一正则，花括号旧写法被拒；租户键经 Scope + ULID，非租户入口拒绝伪造租户段）；`Client`（不可达不失败、Up/Degraded 状态机、后台探测自愈、降级期间快速失败、ctx 取消不算故障、`Status()` 供 1.12 的 `/healthz/ready` 报告 degraded）；写入必带 TTL、值经 `pkg/redact` 判定含明文敏感信息则拒绝；`Limiter`（Redis Lua 固定窗口 → 进程内 → `DBFallback`）、`Cache`（miss/故障直读真相源，恢复后自动重建）、Pub/Sub（用途白名单 `cert_revoke|perm_version|task_wakeup`，`Subscribe` 缺轮询兜底即拒绝，启动/定时/恢复三时机对账，回调 panic 被捕获，`Client.Close` 终止订阅）。
- `config.Redis` 增 `tls/tls_ca_file/tls_server_name`（证书校验不可关闭）；门禁 `redis-only-in-redisx`（go-redis 只许出现在 redisx，测试/tests/scripts 豁免）；依赖图测试证明 `cmd/lingedge-node|dns|reporter|lingedge` 与 `pkg/...` 不含 go-redis/redisx。
- 新依赖：`github.com/redis/go-redis/v9 v9.23.0`（固定，ADR-025 记录版本；间接 `cespare/xxhash/v2 v2.3.0`、`go.uber.org/atomic v1.12.0`），`make vuln` 无漏洞。
- 文档：`docs/operations/redis-degradation.md`（降级矩阵与测试对照）、`docs/bmad/traceability.md` F-11 标注「PRD 暂无对应 NFR，建议同步（见 UD-21）」。

### 实际验证（真实 redis-server 8.10.1 独立临时实例）

- `go test -race ./tests/integration/redis/` 连续多次通过（约 8s/次）。用例：`TestConnectivityTTLAndNamespace`（读写/TTL；无 TTL 写入被拒且旁路确认未到达 Redis；库里全部键命名合规且 PTTL>0）、`TestStartWhileRedisDownThenRecover`（启动时 Redis 已停：不失败、告警日志、之后同端口拉起自动恢复）、`TestLimiterFaultInjection`（SIGKILL→降级放行/进程内限流/DB 兜底拒绝→重启回到 Redis）、`TestCacheFaultInjectionAndSelfHeal`（FLUSHALL、SIGKILL、重启后自愈，真相源变化无陈旧）、`TestTimeoutInjection`（SIGSTOP：端口通但不响应，调用在超时量级内返回并降级，之后 20 次调用 <200ms，SIGCONT 后自愈）、`TestChaosNoPanic`（8 协程并发 + 3 轮 Kill/Restart，调用方永远得到正确结果）、`TestPubsubLossyWithPollFallback`（宕机期间轮询持续、恢复时立即对账、重启后订阅自动重建、Close 后不再回调）、`TestPubsubHandlerPanicIsContained`、`TestPasswordAuthAndRedaction`（requirepass；正确/错误/缺失口令；口令不在应用日志、Redis 服务端日志、Status、错误、`Config` 的 `%v/%+v/%#v` 中出现）、`TestTLS`（运行时生成 CA/证书：配 CA 连通；缺 CA、ServerName 不符、明文连 TLS 端口均降级；口令不泄漏）、`TestTenantKeysIsolatedInRedis`、`TestLimiterHealsKeyWithoutTTL`、`TestCloseStopsSubscriptions`、`TestPublishSensitiveAndTenantForgery`。
- 单测（`internal/platform/redisx`）：键正则负向（花括号、空段、大写、环境不符、超长、伪造租户段、零/平台 Scope）、配置校验与脱敏、不可达启动降级与快速失败、限流分层与窗口、Cache 降级。
- 门禁：`make lint`、`security`、`gate-selftest`（含 redis-only-in-redisx 正反自测）、`test`、`race`、`vuln`、`contract-check`、`build`、`verify-build`、`migrate-test` 全绿（最终一轮结果见下）。

### 评审（子代理三视角：盲审/边界/验证缺口）

| 发现 | 判定 | 处置 |
|---|---|---|
| 租户键可经 `Key("…","t"+uid)` 绕过 Scope；`TenantKey` 不核对 uid 与 Scope 的对应 | medium | patch：`Key` 拒绝形如租户段的标识段 + 负向用例；uid↔Scope 对应无法在封装层校验（Scope 不含 uid），已在文档登记为边界，由调用方 Scope 来源保证 |
| `Client.Close` 不终止订阅协程 | medium | patch：订阅纳入 wg 并随 stop 终止，`TestCloseStopsSubscriptions` |
| 限流计数键被去掉 TTL 后永久无 TTL | medium | patch：脚本在 `n==1` 或 `PTTL<0` 时重设 TTL，`TestLimiterHealsKeyWithoutTTL` |
| Redis 在两次探测间极快重启时订阅恢复补偿不触发 | medium→accept | 由下一个 PollInterval 补上，已在 `SubscribeOptions` 注释与文档说明（有损设计本意） |
| 限流脚本异常误触发降级状态 | low | patch：仅本次回退，不改状态 |
| DB 拒绝时缺 RetryAfter | low | patch |
| 无 singleflight/Handler 同步/TTL 无上限/仅拦 go-redis 的门禁/CA 追加系统根 | low | accept 并记录：缓存惊群与 Handler 异步化留待实际调用方出现时按需引入；其余为设计取舍 |
| 验证缺口：`FLUSHALL` 场景、Publish 敏感值、Close 含订阅、TTL 自愈、伪造租户段 | — | 均已补用例 |

### AC 对照

| AC | 结果 |
|---|---|
| 1 键名/必带 TTL/租户段经 Scope/值不含敏感信息 | 通过（单测 + 真实 Redis 旁路核对） |
| 2 不可达不阻止启动；`Status()` 供 ready 报告 degraded | 通过；`/healthz/ready` 接线属 1.12 |
| 3 `Limiter`/`Cache`/Pub-Sub 三类接口及降级测试 | 通过 |
| 4 停止/清空 Redis 后正确结果、无 panic、恢复自愈 | 通过（SIGKILL、重启、FLUSHALL、SIGSTOP 超时） |
| 5 数据面依赖图不含 Redis 客户端 | 通过（`TestDataPlaneHasNoRedisDependency` + depguard 规则）；`edge/` 目前无 Go 包，出现时测试自动纳入 |
| 6 F-11 标注 | 通过（traceability） |
| 7 键空间唯一、pub/sub 清单、消费者兜底、正则与负向用例 | 通过（清单外用途被拒；`Subscribe` 无兜底被拒；真实消费者由各业务 Story 接入） |

### DoD 与遗留

DoD（基线 §25）：除下述等待环境验证项外满足；前端/迁移/API 可调用不适用。**等待环境验证**：Linux 验收机上的 Redis 小版本与最低版本约束（ADR-025 §9 的「实现时核对」：封装只用通用命令与 Lua `EVAL`，本机仅验证 8.10.1）；Windows 不适用（`redistest` 为 `!windows`）。后续接入提示：1.12 用 `Status()` 接线 ready；登录节流须实现 `DBFallback`（users.failed_login_count/locked_until）；会话/权限缓存 TTL ≤60s。**Story 1.11 前置**：依赖 1.4，已满足。
