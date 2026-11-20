// Package auth 是认证模块（Story 1.16：登录 / 会话 / CSRF / 登录节流 / 登录日志；Story 1.17：TOTP / 恢复码 / 近期 MFA / L4 再认证；Story 1.18：RBAC——RBAC 授权钩子、/admin/users|roles|permissions 管理 API、权限集缓存；API Token 由 1.19 追加）。
// 根包只含对外契约 api.go（Module 接口、Options、纯委托的 New）；实现在 internal/auth/internal/{domain,repository,service,handler}。
package auth
