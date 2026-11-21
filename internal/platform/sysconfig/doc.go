// Package sysconfig 是系统设置、运行模式与能力注册表（Story 1.22，FR-24/FR-31/FR-36，NFR-27，ADR-018 §9/§11）。
//
//   - registry.go：system_settings 的键注册表（键、类型、范围/枚举/模式校验、默认值、分组、是否敏感、是否可经通用 PATCH 修改）。**唯一来源**：
//     认证模块读取的会话/节流/MFA/再认证/强制 TOTP 键与默认值经 TestAuthKeysMatchRegistry 与它逐项核对，不得各自漂移。
//   - capability.go：能力注册表（代码常量）：已交付模块（模块交付时登记）与高级能力（http3|l2_cache|dnssec|edge_compute，状态恒为 unverified，NFR-27）。
//   - service.go：类型化设置服务。写入 = schema 校验 → 事务内（变更 + 审计同一事务，IMP-19）；敏感键经 pkg/secretbox 信封加密落 value_enc（AAD = 表|列|uid），读取脱敏。
//   - gate.go：运行模式 → 功能开关（httpx.FeatureGate，位置：认证后、授权前）与 /auth/me 的 features。
//   - api.go：/admin/settings*、/admin/system/run-mode|version、/admin/installations、/public/site-info|version 的路由（经 httpx.Router.Register）。
//
// 运行模式切换不需要迁移：它只是 system_settings 里的一行（FR-24）。本实例写入后立即生效（缓存失效）；其他实例最迟 CacheTTL（默认 5 秒）内生效。
package sysconfig
