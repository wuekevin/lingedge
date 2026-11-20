// Package httpx 是 REST 服务骨架（Story 1.12、ADR-017、ADR-018 §2-3）：路由元数据、固定的中间件链、统一响应与错误映射、
// 分页助手、Idempotency-Key、健康检查与 Prometheus 指标。
//
// 中间件链（顺序固定，由 Router 构造时写死，业务代码无法重排或绕过）：
//
//	全局：RequestID → recover → 访问日志（脱敏）→ 安全头/CORS 白名单
//	每条路由：认证钩子 → 授权钩子 → 租户 Scope 钩子 → 审计钩子 →（Idempotency-Key，仅声明 Idempotent 的路由）→ handler
//
// 钩子是接口（Authenticator/Authorizer/ScopeResolver/Auditor），具体实现属 Story 1.16–1.21。未注入时使用 Deny* 默认实现：
// 一律安全拒绝，而不是放行——因此在 1.16–1.21 落地之前，任何非公开路由都会得到 401/403/500，不存在「假放行」。
//
// 路由只能经 Router.Register(RouteMeta, handler) 注册：元数据缺失/非法、路径含 ':'（IMP-21）、重复 operationId 或方法+路径、
// handler 为空，都会使 Register 返回错误并让 Router.Handler() 失败（启动失败，NFR-4）。运行时路由集合与 docs/api/openapi.yaml
// 的 operationId 与 x-* 的一致性由 make openapi-check（scripts/contract）与 contracttest 子包校验。
package httpx
