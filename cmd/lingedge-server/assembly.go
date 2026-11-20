package main

import "github.com/wuekevin/lingedge/internal/platform/httpx"

// assembleHooks 是 REST 认证/授权/租户 Scope/审计四个钩子的唯一装配点（Story 1.12 约定，1.13 接入进程）。
//
// 在 Story 1.16–1.21 提供真实实现之前，这里保持零值 Hooks：httpx 对未装配的钩子使用**安全拒绝**默认实现
// （认证 401、授权/Scope 403、需要审计的路由 500——无法留痕就不执行），因此当前进程不可能放行任何受保护路由。
// 由各 Story 在此处逐个填入：
//
//	Authenticator  Story 1.16（会话/CSRF）、1.19（API Token）
//	Authorizer     Story 1.18（RBAC 权限串）
//	Scope          Story 1.20（租户 Scope 解析、acting-as）
//	Auditor        Story 1.21（审计，与业务同事务）
//
// 同一处还预留给周期作业登记：幂等键保留期清理（idempotency.Store.DeleteExpired）等内部周期作业的注册归 Story 1.14（任务运行时）
// 与 1.15（Scheduler 角色），本 Story 不登记、不伪造。
func assembleHooks() httpx.Hooks {
	return httpx.Hooks{}
}
