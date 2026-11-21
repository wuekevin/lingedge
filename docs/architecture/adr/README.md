# LingEdge ADR 索引

> 返回 [architecture.md](../architecture.md)。ADR 由架构代理在**非交互**条件下依据基线与 PRD 作出，均可被用户推翻；每篇「可被推翻处」说明影响范围。编号 = architecture.md §2 的 `AD-n`。状态均为「已接受（架构代理非交互决策）」，用户推翻后改为「已被 ADR-0xx 取代」，**不得重新编号**。

| ADR | 标题 | 关键词 | 阶段 | 关联 PRD |
|---|---|---|---|---|
| [001](ADR-001-modular-monolith.md) | 模块化单体与 MVC + Service + Repository | 依赖方向、api.go、ConfigContributor | M1 | FR-16、D-1 |
| [002](ADR-002-immutable-config-revisions.md) | 配置修订不可变、签名、灰度与回滚 | 集群级修订、内容寻址块、回滚即新修订 | M1 | FR-55/56、R-4、F-3 |
| [003](ADR-003-node-protocol-mtls-enroll.md) | Node↔Server 协议、mTLS 身份与 enroll | gRPC、私有 CA、一次性 Token、90 天证书 | M1 | FR-52、D-12、F-2 |
| [004](ADR-004-openresty-config-generation.md) | OpenResty 配置生成与注入防护 | 固定模板、类型化渲染面、数据驱动 Lua | M1 | FR-54、NFR-5 |
| [005](ADR-005-rule-engine-dsl.md) | 统一规则引擎 DSL、阶段映射、缓存隔离 | 单表、13 阶段、RFC 9111 保守缓存、RE2 子集 | M1 | FR-63..69、F-6 |
| [006](ADR-006-real-ip.md) | 真实 IP 传递与入站可信代理 | 覆盖访客值、保留 Header 清单、可信 CIDR | M1 | FR-70..74 |
| [007](ADR-007-cache-invalidation.md) | 缓存失效 | site_epoch、url_ver、满载升级、目录 PoC | M1/P2 | FR-76/78/79、D-6、F-4 |
| [008](ADR-008-access-log-and-metrics-pipeline.md) | 访问日志与指标管线 | JSONL、断点续传、chunk 元数据、rollup | M1 | FR-114、D-5、F-10 |
| [009](ADR-009-tenant-isolation.md) | 多租户隔离强制点 | Repository Scope、404、隔离矩阵 | M1 | FR-19、NFR-8 |
| [010](ADR-010-money-and-metering-idempotency.md) | 金额与计量幂等 | 整数最小单位、幂等键、钱包只写 | M1 预留/P5 | FR-107..109 |
| [011](ADR-011-secrets-and-kms.md) | 密钥与证书私钥加密、主密钥 | 信封加密、KeyProvider、AAD | M1 | NFR-12/13、F-7 |
| [012](ADR-012-release-signing.md) | Tools 发行包签名与三个信任域 | Ed25519 + SHA256、离线私钥 | M1 | FR-9、D-12、OQ-8 |
| [013](ADR-013-upgrade-rollback-migration-policy.md) | 升级回滚与不可逆 Migration 策略 | 三级分类、备份、回滚=恢复备份 | M1 | FR-8、R-3 |
| [014](ADR-014-frontend-structure.md) | 前端工程结构（Admin/Console 隔离） | pnpm workspace、realm 隔离 | M1 | FR-37 |
| [015](ADR-015-website-ssr-ssg.md) | 官网 SSR / 静态 | M1 SSG、P2 SSR、Node.js 依赖声明 | M1/P2 | FR-14、D-8、F-5 |
| [016](ADR-016-migration-tool.md) | Migration 工具选型 | goose v3、class 标注、非事务 DDL | M1 | FR-22 |
| [017](ADR-017-api-conventions.md) | API 约定与契约冻结 | OpenAPI-first、错误体、分页、Task | M1 | FR-20/21、D-11 |
| [018](ADR-018-authn-authz.md) | 认证与授权 | 会话 realm、CSRF、TOTP、API Token、RBAC | M1 | FR-17/18/25 |
| [019](ADR-019-scheduler-worker-tasks.md) | Scheduler/Worker 内置与任务租约 | tasks 队列、job_locks、围栏令牌 | M1 | FR-29、FR-122、D-16 |
| [020](ADR-020-node-autonomy.md) | 节点自治（主控离线继续服务） | 本地代、OpenResty ExecStartPre | M1 | FR-57 |
| [021](ADR-021-scheduling-debounce-dns.md) | 调度防抖与 DNS 架构（预案） | 联合判定、迟滞 | P3 | FR-91、D-9 |
| [022](ADR-022-edge-rate-limit.md) | 边缘限流本地优先（预案） | 令牌桶、近似全局、Redis 不在路径 | P4 | FR-98、R-9 |
| [023](ADR-023-test-doubles-and-env-verification.md) | 测试替身隔离与等待环境验证 | build tag、分层测试、无 OpenResty 策略 | M1 | NFR-1/3、R-11 |
| [024](ADR-024-node-privilege-model.md) | Node 进程权限模型与升级特权路径 | 非 root、CAP_NET_BIND_SERVICE、.path 触发 | M1 | FR-12/60、F-9 |
| [025](ADR-025-redis-boundary.md) | Redis 使用边界 | 非真相源、降级 | M1 | F-11 |
| [026](ADR-026-database-credential-handoff.md) | 数据库凭据交接与生命周期（AA-7） | 向导内存持有 migrate 口令、runtime-credentials.json、migrate.yaml root 0400、轮换 | M1 | FR-11、NFR-6、NFR-12 |

## 依赖版本说明

各 ADR 提及的库版本均为 **2026-11-16 查询到的 registry 最新稳定值**，仅供参考；实际版本以 `go.sum` / lockfile 在实现时核对并固定（见 [dev-environment.md](../dev-environment.md)）。

## 提出新 ADR / 推翻已有 ADR

1. 新文件 `ADR-0NN-<slug>.md`（沿用模板：背景、决策、Binds/Prevents/Rule、备选方案、后果、可被推翻处、验证）。
2. 被取代的 ADR 状态改为「已被 ADR-0NN 取代」并链接；同步更新 `architecture.md` §2 的 AD 表与受影响文档（数据库、OpenAPI、proto）。
3. 契约类变更（OpenAPI/proto/迁移）依 ADR-017 的冻结与破坏检测流程。

## 2026-11-16 修订说明

依据两份独立评审（`docs/bmad/review-architecture.md`、`review-architecture-internal.md`）对 ADR-001…ADR-025 做了**原地修订**（编号不变），每篇被改的 ADR 末尾有「修订记录 2026-11-16」。逐条评审 ID → 处理结果 → 改动文件见 [`docs/bmad/architecture-revision-log.md`](../../bmad/architecture-revision-log.md)。被修订的 ADR：001、002、003、004、005、006、007、008、009、010、011、012、013、014、015、016、017、018、019、020、022（仅 §6 同步）、023、024、025；ADR-021（P3 预案）经核对无需改动，见修订日志。

## 2026-11-21 补充

新增 ADR-026（AA-7，UD-31，默认采纳、可推翻）：数据库凭据交接。同时在 ADR-016、ADR-011 末尾追加修订记录；tools-design §13.1 为其落点。
