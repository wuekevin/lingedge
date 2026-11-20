// Package taskapi 是任务中心的 REST 面（Story 1.14）：GET /tasks、/tasks/{taskId}、/tasks/{taskId}/events 与 POST /tasks/{taskId}/cancel，
// 全部经 httpx.Router.Register 注册，元数据与 docs/api/openapi.yaml 的 x-* 一致（make openapi-check 校验）。
//
// 认证/授权/Scope/审计仍由 httpx 的钩子完成；1.16–1.21 之前钩子是安全拒绝默认实现，因此这些路由在生产装配里一律 401/403。
// 处理函数只经 tenantscope.FromContext 取得中间件放入的 Scope，租户 Scope 下他租户的任务表现为 404（不泄漏存在性）。
//
// 本包还导出 Accepted：业务模块的耗时操作入队后用它返回 202 + Task + Location（ADR-017 §7）。
package taskapi
