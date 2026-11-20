// Package tenant 是租户模块（Story 1.20、ADR-009、FR-19）：租户、租户成员与租户配额的数据模型与读写（/tenants*），以及租户目录
// （httpx.TenantResolver，供 Scope 钩子把 X-Tenant-ID 解析为真实且可用的租户）。
//
// 配额只存取：强制执行属 Story 6.9，本模块不在任何路径上「假装强制」。租户可见响应不含节点/集群内部标识（R-T1）。
package tenant
