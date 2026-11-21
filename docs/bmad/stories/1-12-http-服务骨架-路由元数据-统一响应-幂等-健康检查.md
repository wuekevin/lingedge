---
title: 'Story 1.12：HTTP 服务骨架（路由元数据 / 统一响应 / 幂等 / 健康检查）'
type: 'feature'
created: '2026-11-21'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: 'd56fec8'
context:
  - docs/bmad/epics.md（Story 1.12 全文，1092–1117 行）
  - docs/architecture/adr/ADR-017-api-conventions.md（路由元数据、统一错误/分页/幂等、IMP-21）
  - docs/architecture/adr/ADR-018-authn-authz.md §2-3（会话/CSRF，钩子的位置）
  - docs/api/README.md、docs/api/openapi.yaml（x-* 扩展）、docs/bmad/review-architecture*.md 的 IMP-21 / M14
  - docs/bmad/stories/1-10-…、1-11-…（格式与门禁约定）；internal/platform/{apierr,requestid,logx,config,db,redisx,tenantscope}
---

## Intent

**Problem:** 之后 200 个 M1 接口若各自拼装中间件、错误体、分页与幂等，就会出现「忘了鉴权/租户/审计」的路由、不一致的错误码，以及与 OpenAPI 漂移的实现；Gin 又有已实测的 `:` 通配陷阱（IMP-21）。

**Approach:** 在 `internal/platform/httpx` 固定一条无法重排的中间件链与唯一的路由注册入口 `Router.Register(RouteMeta, handler)`：元数据缺失/非法、路径含 `:`、重复 operationId 或方法+路径、handler 为空、声明 `Idempotent` 却无存储，都使注册失败并让 `Handler()` 失败（启动失败）。认证/授权/租户 Scope/审计是接口钩子，默认实现是**安全拒绝**（不是放行）；具体实现属 1.16–1.21。同时提供统一响应与错误映射、分页助手、基于 MySQL `idempotency_keys` 的 `Idempotency-Key`、`/healthz` 与 `/healthz/ready`、`127.0.0.1:9180/metrics` 的 Prometheus 文本，以及 kin-openapi 响应契约测试助手；`lingedge-server routes` 导出真实注册路由清单，`make openapi-check` 据此校验「运行时路由 ⊆ openapi.yaml」并输出「M1 未实现 operation 清单」。

## Boundaries & Constraints

**Always:** 链序 RequestID → recover → 访问日志（脱敏）→ 安全头/CORS 白名单 → 认证钩子 → 授权钩子 → Scope 钩子 → 审计钩子；默认钩子拒绝；错误映射 400/404/412/409/422；幂等 ≤128 字符、24h、同键同体重放同键异体 422；Redis 不可用只降级；契约冲突走 `docs/bmad/contract-changes/`；不以 501 假实现充数；本机常驻 mysqld/redis 不动，测试只用自拉起的临时实例。

**Never:** Mock 冒充生产；改冻结迁移（migrations-m1）；启动/改动/终止本机常驻 mysqld/redis；Docker；push；改 git config；改其他产品目录。

## Code Map

- `internal/platform/httpx/` -- `meta.go`（`RouteMeta`、`Validate`、`ginPath`）、`hooks.go`（四个钩子接口、`Deny*` 默认实现、`MapScopeError`、`PrincipalFrom`/`MetaFrom`）、`middleware.go`（RequestID/recover/访问日志+指标/安全头+CORS/各钩子中间件）、`router.go`（`NewRouter`、`Register`、`Handler`、`Manifest`）、`respond.go`（`Fail`/`JSON`/`BindJSON`/`CheckIfMatch`/`MapError`）、`page.go`（page/page_size、cursor/limit、sort 白名单）、`idempotency.go`、`health.go`、`metrics.go`、`manifest.go`。
- `internal/platform/httpx/contracttest/` -- kin-openapi 契约助手：`Load/LoadRepo`、`ValidateResponse/AssertResponse`、`CheckRoutes`、`Unimplemented`。
- `internal/platform/idempotency/`（接口与常量）与 `idempotency/repository/`（MySQL 实现，平台级表 `idempotency_keys`，`lingedge:platform-table` 豁免登记于 `scripts/gates/data/platform-tables.txt`）。
- `internal/platform/apierr/` -- 增 `Conflict`/`PreconditionFailed`/`KeyReuse` 构造器。
- `cmd/lingedge-server/routes.go` -- `registerRoutes`（唯一装配点，当前无业务路由）与 `lingedge-server routes` 子命令。
- `scripts/contract/routes.go`（+ `cmd_openapi.go`）-- `openapi-check -routes`、`-require-m1-complete`、规则 `path-param-name`、`idempotency-422`、`route-*`；`Makefile` 的 `openapi-check` 接入（`M1_EXIT=1` 为阶段出口）。
- `scripts/gates/{spec,genlint}.go`、`.golangci.yml`、`tests/gates` -- 新门禁 `[gin-raw-engine]`（只有 httpx 可构造 Gin 引擎）+ 负向/正向自测。
- `tests/integration/httpx/` -- 真实临时 MySQL + 真实临时 Redis 的幂等与就绪集成测试；`tests/redistest` 增 `MainWith`。
- `docs/bmad/contract-changes/CC-2-openapi-gin-consistency.md`；`docs/api/openapi.yaml`（路径参数名统一、`retryDeployment` 补 422）。

## Tasks & Acceptance

- [x] AC1 中间件链顺序与默认拒绝钩子（`TestHookChainOrder`、`TestHookChainShortCircuits`、`TestPublicRouteSkipsAuthHooks`、`TestDefaultHooksDenyNotAllow`、`TestAuthzAndScopeDenials`、`TestPanicRecoveredWithRequestIDAndAccessLog`、`TestAccessLogRedaction`、`TestSecurityHeadersAndCORS`）
- [x] AC2 `Register` 元数据校验/启动失败；运行时路由 ⊆ openapi；`make openapi-check` 输出 M1 未实现清单（`TestRouteMetaValidate`、`TestRegisterFailuresMakeHandlerFail`、`contracttest.TestEveryOperationRegistersCleanly`、`TestCheckRoutesNegativeSamples`、`scripts/contract` 的 `TestCheckRoutes`/`TestReportRoutesExitCodes`）
- [x] AC3 错误映射 400/404/412/409/422（`TestErrorMappingThroughRouter`、`apierr.TestStoryConstructorsMapToStatus`、`TestCheckIfMatch`）
- [x] AC4 分页助手（`TestParsePageBoundaries`、`TestParseCursorBoundaries`、`TestParseSortWhitelist`、`TestPageAndCursorResultShape`）
- [x] AC5 幂等（真实 MySQL：重放/异体 422/主体隔离/校验/失败不缓存/并发恰一次/执行中 409 与陈旧接管/24h 过期/同一 Task/存储不可用 fail-closed/57 个契约 operation 的 422 响应符合 OpenAPI）
- [x] AC6 `/healthz`、`/healthz/ready`（真实 MySQL + 迁移状态 + 真实 Redis 杀进程降级/自愈）、`127.0.0.1:9180/metrics` Prometheus 文本、kin-openapi 响应契约助手
- [x] AC7 IMP-21：OpenAPI 路径无 `:`（既有规则）+ 注册层拒绝 + 路由清单 `route-path-colon` + 回归用例（`TestIMP21ColonRegression`）；`TaskType` 25 项以 OpenAPI 为唯一事实源（沿用 1.3 的 freeze-check 口径，本 Story 不新增清单）
- [x] sprint-status、开发记录、本地提交

## Verification

`gofmt -l .` 为空；`make lint security gate-selftest race vuln contract-check build verify-build migrate-test` 全绿（结果见下「开发记录」）。

## Spec Change Log

- 2026-11-21：契约变更 CC-2（`docs/bmad/contract-changes/CC-2-openapi-gin-consistency.md`）：同位置路径参数名统一（17 个 P3/P4 operation，无 M1）、`retryDeployment` 补 `422`。均为非破坏性（oasdiff 对 `contract-v1` 无破坏性变更）。迁移与 proto 无改动；`contract-v1`/`migrations-m1` 未触碰。

## Review Triage Log

见「开发记录 · 自审」。

## 开发记录

**开发会话**：2026-11-21，bmad-build（非交互、完全自主）。环境：macOS（Darwin 25.6.0 arm64）、go1.27.4、本机 MySQL 8.0（Homebrew mysql@8.0，仅用于启动**自拉起的独立临时实例**）、redis-server 8.10.1。本机常驻 mysqld（PID 576/744）与 redis 全程未被启动、改动或终止；测试结束由 `mysqltest`/`redistest` 清理临时实例与账号。

### 范围与设计要点

- **链与钩子**：全局链 RequestID → recover → 访问日志（+指标）→ 安全头/CORS 由 `NewRouter` 写死；每条路由链 = `setMeta → 认证 → 授权 → Scope → 审计 →（幂等）→ handler`。公开路由（`Auth=public`）跳过认证/授权/Scope 但仍过审计钩子；认证钩子返回 `(nil,nil)` 或「装着 nil 指针的接口」一律 401；Scope 钩子返回零值 Scope 一律拒绝；未装配的钩子用 `Deny*`：认证 401、授权/Scope 403、审计对 `Audit=true` 的路由 500（无法留痕就不执行）。`MapScopeError` 把 `tenantscope` 哨兵错误映射为 403/404/400，供 1.20 复用。
- **元数据**：`RouteMeta` 在 AC 列出的 6 个字段之外增加 `Method/Path`（路由要挂树）、`Reauth`（x-reauth）、`Idempotent`（声明 Idempotency-Key 头）。`Permissions` 必须显式声明（`nil`=缺失，无权限用 `NoPermissions()`）；`Auth=public ⇔ TenantScope=public`；公开路由不得声明权限/Reauth/Idempotent（公开路由无主体，幂等键无法隔离，会泄漏他人响应）。路径：`{param}` 必须占满整段，拒绝 `:`、`*`、`?`、`#`、尾斜杠、空段、重复参数名。`Register` 同时捕获 Gin 通配符冲突的 panic 并转为错误。
- **运行时 ⊆ openapi**：`lingedge-server routes` 导出 `registerRoutes` 装配出的真实路由清单（JSON）；`scripts/contract openapi-check -routes` 校验 operationId ∈ openapi、Method/Path/Permissions/TenantScope/Audit/Reauth/Phase/Auth/Idempotent 与 x-* 一致、路径无 `:`、无重复，并输出「M1 未实现 operation 清单」（当前 200 个，不以 501 占位）；`make openapi-check M1_EXIT=1` 在清单非空时失败（阶段出口）。`contracttest.TestEveryOperationRegistersCleanly` 把契约全部 486 个 operation 按其 x-* 推导的元数据注册进真实 Gin 路由树。
- **幂等**：真相源 MySQL `idempotency_keys`（迁移 000002 已有，无需新表/无冻结迁移改动）；Redis 不参与（「仅可作加速」未使用，Redis 故障对幂等零影响）。流程：`Begin` 先 INSERT 占位 → 唯一键冲突读取既有行：过期则清除重占；方法/路径/请求哈希不同 → 422；已有响应 → 重放；仍在执行 → 409+`Retry-After`；执行中占位超过 5 分钟租约视为执行方崩溃，CAS 接管。请求哈希 = SHA-256(方法, 路径, 规范化查询, 请求体)；主体隔离键 `<platform|tenant>:<actor>:<tenant>`（租户取 Scope，代租户时为目标租户）。仅缓存 2xx 响应（含 `Location`/`ETag`/`Content-Type` 与 `tasks.id` 关联，故 202 重放返回同一 Task）；4xx/5xx/panic 释放占位，纠正后同键可重试。响应先缓冲、记录写入后再放行；记录写入失败只记错误日志（响应照常返回）。存储不可用时创建类请求 500，不绕过幂等保护执行。审计钩子对重放得到 `Replayed=true`，不应重复留痕。
- **健康**：`/healthz` 只证明进程存活；`/healthz/ready` 报告 database（Ping）、migrations（applied/latest/pending，经 `MigrationProbeFromRunner` 基于 `migrate.Runner.Status`）、redis（`redisx.Client.Status()`）。DB 或迁移不满足 → 503 `unavailable`；Redis 降级或未装配 → 200 `degraded`；依赖未装配（DB/迁移）一律失败，不会「默认就绪」。响应只含固定状态词与迁移版本号，不含错误原文（原文只进脱敏日志）。
- **指标**：自带 Prometheus 文本 0.0.4 输出（计数器/直方图/仪表，route 标签为路由模板、未命中记 `unmatched`，基数有界），`NewMetricsServer(addr)` 仅服务 `GET /metrics`，默认 `DefaultMetricsAddr=127.0.0.1:9180`；监听与关闭由 1.13 装配。
- **访问日志/脱敏**：只记路径（不记查询串、请求头、请求体），其余字段经 `logx` 脱敏核心；panic 值与栈经 `logx.PanicFields` 脱敏；访问日志用 defer 记录，panic 的请求也被记为 500。
- **CORS**：精确白名单（拒绝 `*`、`null`、通配、路径、userinfo），回显白名单源并带 `Vary: Origin`/`Allow-Credentials`，预检非白名单 403；安全头 nosniff/DENY/no-referrer/CSP `default-src 'none'; frame-ancestors 'none'`/`Cache-Control: no-store`。HSTS 由 Nginx 终止 TLS 时设置（本进程只在 127.0.0.1 后面）。
- **其他行为收紧**：不做尾斜杠/大小写重定向；405 折叠为 404（不泄漏路由存在性）；X-Forwarded-For 只信任 `127.0.0.1/::1`（`Options.TrustedProxies` 可改）；`BindJSON` 要求 `application/json`、体积受限（默认 1 MiB）、拒绝未知字段与多余内容、错误体不回显请求内容。 【Story 1.22b 已收口：生产装配现经 `server.trusted_proxies` 配置；见 1-22b-遗留项清理.md】
- **门禁**：新增 `[gin-raw-engine]`（只有 `internal/platform/httpx` 可 `gin.New/Default`，别名导入同样识别；`tests/gates` 含 3 个负向样本与 3 个合法写法）；`openapi-check` 新规则 `path-param-name`、`idempotency-422`、`route-*`，各带负向样本。

### 契约冲突与处理（CC-2）

契约测试把 486 个 operation 注册进真实 Gin 路由树时暴露两类**契约自身**的问题，均已按 `contract-changes` 流程处理（见 CC-2，非破坏性，oasdiff 对 `contract-v1` 无破坏性变更）：① P3/P4 的 3 组路径（`/dns/zones`、`/l4/applications`、`/waf/rulesets`）同一位置使用了不同的路径参数名，Gin 注册会 panic（17 个 operation，均非 M1）；② `retryDeployment`（M1）声明了 `Idempotency-Key` 却缺 `422 idempotency.key_reuse`，由幂等集成测试发现。`make openapi-check` 因此新增了对应的永久规则，对改名前的 openapi 副本报 8 处违规。

### 自审

| 发现 | 严重度 | 处理 |
|---|---|---|
| `tenantscope` 铸造白名单 `scopeMintPaths` 仍是 `internal/platform/` 整体（`httpx` 调用 `tenantscope.NewContext`） | low | accept：1.12 spec.go 注释早已登记「应在 1.12/1.14/1.20 落地后收窄」；收窄需连同 1.14（任务运行时）与 1.20（Scope 钩子实现）一起评估，避免现在就改出误伤，已列入遗留 |
| 幂等记录写入失败后 handler 副作用已发生 | medium | 响应照常返回并记错误日志；同键重放会在租约（5 分钟）到期前得到 409、之后被视为新请求重新执行——这是「至少一次」边界，需要强保证的创建类 handler 应使用任务幂等键（`tasks.uk_tasks_idem`，1.14 约定）；已在文档登记 |
| 陈旧占位接管后，原执行方若并未真的崩溃（只是慢于 5 分钟）会造成同键两次执行 | medium | accept 并登记：租约取 5 分钟（远大于正常请求时长），`Complete` 以 `response_status IS NULL` 做 CAS，先完成者写入；集成测试 `TestInFlightConflictAndStaleTakeover` 覆盖 |
| `lingedge_app` 对 `schema_migrations` 没有 SELECT 授权（版本表由 migrate 账号维护，迁移文件没有授权语句），app 池无法直接做「迁移版本」就绪检查 | medium | 不改冻结迁移：就绪探针做成接口（`MigrationProbe`），真实实现 `MigrationProbeFromRunner` 基于 migrate 池；由 1.13 决定在进程内如何持有/缓存（建议：启动时与按需低频探测，且 migrate 凭据只读不写），或经 contract-changes 为 app 增加该表只读授权。已列入遗留 |
| 公开路由 + 幂等键会泄漏他人响应 | high（设计期拦截） | `Register` 直接拒绝（`TestRegisterFailuresMakeHandlerFail`） |
| 默认钩子若「空实现返回 nil」会成为假放行 | high（设计期拦截） | 默认实现一律拒绝，`TestDefaultHooksDenyNotAllow` 证明未装配时 handler 永不执行 |
| 同位置路径参数名不一致、`retryDeployment` 缺 422 | medium | 见 CC-2，永久门禁化 |

### 新增依赖

`github.com/gin-gonic/gin v1.12.0`（IMP-21 实测版本）与 `github.com/getkin/kin-openapi v0.153.0`（ADR-017 §1 的契约校验库，仅 `httpx/contracttest` 与测试使用，生产路径不 import；版本与 `scripts/contract` 一致）。govulncheck 首跑发现 gin 的传递依赖有已知漏洞：`quic-go v0.59.0`（GO-2026-5676，被可达调用）与 `golang.org/x/crypto v0.58.0`（GO-2026-5932，未调用），已分别升到 `quic-go v0.59.1`、`x/crypto v0.59.0`，`make vuln` 回到 No vulnerabilities found。

### 实际验证

提交前最后一次完整运行 `make lint security gate-selftest race vuln contract-check build verify-build migrate-test`，退出码 0：

- `gofmt -l .` 为空；`make lint`：vet、scaffold、arch-check、golangci-lint（0 issues）、shell-check、repo-check 全部通过（`idempotency_keys` 以 `lingedge:platform-table` 登记，新门禁 `[gin-raw-engine]` 已进 `.golangci.yml`）。
- `make security`：G-5/G-6/G-11/G-12/品牌通过（其余 G-x 仍未接入，同 1.11 的声明）。
- `make gate-selftest`：`scripts/gates` 与 `tests/gates` 通过，含新增的 gin 裸引擎 3 个负向样本 + 3 个合法写法。
- `make race`（`-race`）：全部包 ok，含 `internal/platform/httpx`（钩子链/元数据/分页/错误映射/健康/指标）、`httpx/contracttest`（486 个 operation 全量注册、响应契约助手）、`tests/integration/httpx`（真实临时 MySQL + Redis）、`cmd/lingedge-server`（routes 子命令）。
- `make vuln`：No vulnerabilities found。
- `make contract-check`：`scripts/contract` 单测（含新规则的负向样本）通过；`openapi-check OK`（路径 307、operation 486；路由清单 OK：运行时已注册 0 条，均在 openapi.yaml 中且元数据一致；**M1 未实现 operation 清单 200 个**）；`openapi-breaking OK`（相对 contract-v1 无破坏性变更）；`proto-check`；`migrations-check OK`（13 个哈希一致）；`freeze-check OK`（M1 operation 200、路径含冒号 0）。`make openapi-check M1_EXIT=1` 按预期失败（清单非空）。
- `make build`、`make verify-build`、`make migrate-test`：通过（迁移 lint、迁移包单测、`tests/migrations` 真实 MySQL 临时实例）。
- 收尾核对：本机常驻 mysqld（PID 744，及 576 的 mysqld_safe）与 redis 未被触碰；临时实例/账号由 `mysqltest`/`redistest` 清理。

### AC 对照

| AC | 结果 |
|---|---|
| 1 中间件链固定顺序，钩子为接口、默认拒绝 | 通过（顺序轨迹断言 + 短路 + 公开路由 + 默认拒绝 + panic/脱敏/CORS） |
| 2 Register 元数据缺失启动失败；运行时 ⊆ openapi；M1 未实现清单 | 通过（清单 200 个；`M1_EXIT=1` 阶段出口开关已验证会失败） |
| 3 错误映射 | 通过（400/404/412/409/422 经真实 Gin 路由与统一错误体） |
| 4 分页助手 | 通过（默认值、边界、非法值、深翻页上限、sort 白名单、响应形状） |
| 5 Idempotency-Key | 通过（真实 MySQL，含并发与过期） |
| 6 健康检查 / metrics / 契约助手 | 通过（真实 MySQL + Redis；Prometheus 文本经格式正则逐行校验） |
| 7 IMP-21 | 通过（路径无 `:` 的三层门禁 + 回归；OpenAPI 路径已是改名后的 `rule-validations`/`certificate-parses`） |

### DoD 与遗留

DoD（基线 §25）：除下述等待环境验证项外满足；前端功能可用、数据库迁移完成（使用 1.7）不适用。**等待环境验证**：① Linux 验收机/最低 MySQL 8.0.16 上重跑 `tests/integration/httpx`（本机仅验证 8.0.x 临时实例与 Redis 8.10.1）；② 真实 Nginx 反代（`X-Forwarded-For` 采信、TLS 终止与 HSTS）与真实 Prometheus 抓取 `127.0.0.1:9180/metrics`；③ 真实的认证/授权/Scope/审计钩子装配后的整链端到端（1.16–1.21 提供实现后补）。**遗留/后续**：1.13 负责监听 `HTTP.Listen` 与 `metrics.listen`、装配 `Hooks`、选择迁移探针的持有方式、把 `redisx.Client` 与 `db.AppPool` 接入 `HealthOptions`、调用 `idempotency.Store.DeleteExpired` 的保留期作业（1.14/1.15）；各业务 Story 在 `cmd/lingedge-server/routes.go` 的 `registerRoutes` 追加注册；M1 阶段出口使用 `make openapi-check M1_EXIT=1`。
