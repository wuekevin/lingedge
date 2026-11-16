package apierr

// 本文件的错误码清单与 HTTP 状态映射与 docs/api/README.md §5「错误码索引」逐项一致，由 codes_test.go 机器核对
// （增删改任何一项必须同时改文档并走契约变更说明，ADR-017 §10）。状态 0 表示该码不经 REST 状态行返回
// （task.failed 出现在 Task.error.code；enroll.token_invalid 仅 gRPC :9444 与 Tools）。

// Code 是稳定的小写点分错误码。
type Code string

const (
	// AuthUnauthenticated 未登录或会话/令牌失效。
	AuthUnauthenticated Code = "auth.unauthenticated"
	// AuthForbidden 已登录但缺少权限。
	AuthForbidden Code = "auth.forbidden"
	// AuthInvalidCredentials 邮箱或口令错误。
	AuthInvalidCredentials Code = "auth.invalid_credentials"
	// AuthReauthRequired L4 敏感操作要求近期再认证。
	AuthReauthRequired Code = "auth.reauth_required"
	// AuthTotpRequired 需要二次验证 / 验证码错误。
	AuthTotpRequired Code = "auth.totp_required"
	// AuthTotpInvalid 需要二次验证 / 验证码错误。
	AuthTotpInvalid Code = "auth.totp_invalid"
	// AuthCsrfInvalid 缺少或错误的 X-CSRF-Token。
	AuthCsrfInvalid Code = "auth.csrf_invalid"
	// AuthPasswordPolicy 口令不满足策略。
	AuthPasswordPolicy Code = "auth.password_policy"
	// AuthTokenRevoked API Token 已吊销。
	AuthTokenRevoked Code = "auth.token_revoked"
	// ValidationFailed 参数/请求体校验失败。
	ValidationFailed Code = "validation.failed"
	// ConfigValidationFailed 发布前配置/规则校验失败。
	ConfigValidationFailed Code = "config.validation_failed"
	// ResourceNotFound 不存在或不属于当前租户。
	ResourceNotFound Code = "resource.not_found"
	// ResourceConflict 唯一性、状态不允许、被引用。
	ResourceConflict Code = "resource.conflict"
	// ResourcePreconditionFailed If-Match 版本不匹配。
	ResourcePreconditionFailed Code = "resource.precondition_failed"
	// IdempotencyKeyReuse 同一幂等键用于不同请求体。
	IdempotencyKeyReuse Code = "idempotency.key_reuse"
	// RateLimitExceeded 触发限流。
	RateLimitExceeded Code = "rate_limit.exceeded"
	// FeatureDisabled 运行模式或阶段未启用该功能。
	FeatureDisabled Code = "feature.disabled"
	// TaskFailed Task 终态错误。
	TaskFailed Code = "task.failed"
	// TaskNotCancelable 任务已终态。
	TaskNotCancelable Code = "task.not_cancelable"
	// SiteDomainConflict 域名已被其他活动站点占用。
	SiteDomainConflict Code = "site.domain_conflict"
	// SiteDomainInvalid 域名格式非法。
	SiteDomainInvalid Code = "site.domain_invalid"
	// SiteDomainUnverified 商业模式下域名未通过所有权校验。
	SiteDomainUnverified Code = "site.domain_unverified"
	// OriginAddressBlocked 回源地址未通过 SSRF 校验。
	OriginAddressBlocked Code = "origin.address_blocked"
	// RuleInvalid 规则结构/字段/操作符不匹配。
	RuleInvalid Code = "rule.invalid"
	// RuleRegexRejected 正则不在 RE2 子集或超出长度/复杂度限制。
	RuleRegexRejected Code = "rule.regex_rejected"
	// RuleTypeUnavailable 规则类型在当前阶段未交付。
	RuleTypeUnavailable Code = "rule.type_unavailable"
	// RealipHeaderInvalid 自定义 Header 名语法非法。
	RealipHeaderInvalid Code = "realip.header_invalid"
	// RealipHeaderReserved 命中保留名/保留前缀/与内置 Header 重复。
	RealipHeaderReserved Code = "realip.header_reserved"
	// RealipTrustedProxyForbidden 可信代理范围被禁止。
	RealipTrustedProxyForbidden Code = "realip.trusted_proxy_forbidden"
	// RuleCacheKeyDimensionConflict 缓存键封闭规则被违反：规则读取了未入缓存键的维度却参与回源/改写/回源头。
	RuleCacheKeyDimensionConflict Code = "rule.cache_key_dimension_conflict"
	// RuleRegexAmbiguous 正则存在指数歧义。
	RuleRegexAmbiguous Code = "rule.regex_ambiguous"
	// RuleInterpolationInvalid 变量插值/重定向目标模板非法。
	RuleInterpolationInvalid Code = "rule.interpolation_invalid"
	// RuleRedirectTargetInvalid 变量插值/重定向目标模板非法。
	RuleRedirectTargetInvalid Code = "rule.redirect_target_invalid"
	// RuleLoopDetected 变量插值/重定向目标模板非法。
	RuleLoopDetected Code = "rule.loop_detected"
	// CacheDangerousNotAcknowledged 启用危险开关未提供 risk_ack。
	CacheDangerousNotAcknowledged Code = "cache.dangerous_not_acknowledged"
	// CachePurgeLimitExceeded 刷新 URL 数量超限。
	CachePurgeLimitExceeded Code = "cache.purge_limit_exceeded"
	// CacheFeatureUnavailable 目录/通配符失效在 PoC 通过前不可用。
	CacheFeatureUnavailable Code = "cache.feature_unavailable"
	// CertKeyMismatch 私钥与证书不匹配。
	CertKeyMismatch Code = "cert.key_mismatch"
	// CertChainInvalid 证书链无效或已过期。
	CertChainInvalid Code = "cert.chain_invalid"
	// CertInUse 证书仍被站点引用。
	CertInUse Code = "cert.in_use"
	// EnrollTokenInvalid 注册令牌无效/已用/过期。
	EnrollTokenInvalid Code = "enroll.token_invalid"
	// NodeClusterConflict 入群/绑定导致同节点主机名或集群级参数冲突。
	NodeClusterConflict Code = "node.cluster_conflict"
	// NodeNotOnline 目标节点不在线，无法执行调试/升级。
	NodeNotOnline Code = "node.not_online"
	// DeploymentNotPausable 部署当前状态不允许该操作。
	DeploymentNotPausable Code = "deployment.not_pausable"
	// DeploymentNotResumable 部署当前状态不允许该操作。
	DeploymentNotResumable Code = "deployment.not_resumable"
	// DeploymentNotRetryable 部署当前状态不允许该操作。
	DeploymentNotRetryable Code = "deployment.not_retryable"
	// SiteNoClusterBound 站点尚未被平台绑定到任何集群，无法发布。
	SiteNoClusterBound Code = "site.no_cluster_bound"
	// OriginGroupInconsistent 同一源站组内 protocol/tls_verify/sni 必须一致。
	OriginGroupInconsistent Code = "origin.group_inconsistent"
	// SetupTokenInvalid 安装向导令牌错误。
	SetupTokenInvalid Code = "setup.token_invalid"
	// SetupAlreadyInstalled 已存在 install.lock，向导接口永久不可用。
	SetupAlreadyInstalled Code = "setup.already_installed"
	// TenantQuotaExceeded 租户配额超限。
	TenantQuotaExceeded Code = "tenant.quota_exceeded"
	// InternalError 内部错误。
	InternalError Code = "internal.error"
)

// table 是错误码 → HTTP 状态的唯一映射表（0=不经 REST 状态行）。
var table = map[Code]int{
	AuthUnauthenticated:           401,
	AuthForbidden:                 403,
	AuthInvalidCredentials:        401,
	AuthReauthRequired:            403,
	AuthTotpRequired:              401,
	AuthTotpInvalid:               401,
	AuthCsrfInvalid:               403,
	AuthPasswordPolicy:            400,
	AuthTokenRevoked:              401,
	ValidationFailed:              400,
	ConfigValidationFailed:        400,
	ResourceNotFound:              404,
	ResourceConflict:              409,
	ResourcePreconditionFailed:    412,
	IdempotencyKeyReuse:           422,
	RateLimitExceeded:             429,
	FeatureDisabled:               403,
	TaskFailed:                    0,
	TaskNotCancelable:             409,
	SiteDomainConflict:            409,
	SiteDomainInvalid:             400,
	SiteDomainUnverified:          409,
	OriginAddressBlocked:          400,
	RuleInvalid:                   400,
	RuleRegexRejected:             400,
	RuleTypeUnavailable:           400,
	RealipHeaderInvalid:           400,
	RealipHeaderReserved:          400,
	RealipTrustedProxyForbidden:   400,
	RuleCacheKeyDimensionConflict: 400,
	RuleRegexAmbiguous:            400,
	RuleInterpolationInvalid:      400,
	RuleRedirectTargetInvalid:     400,
	RuleLoopDetected:              400,
	CacheDangerousNotAcknowledged: 400,
	CachePurgeLimitExceeded:       400,
	CacheFeatureUnavailable:       403,
	CertKeyMismatch:               400,
	CertChainInvalid:              400,
	CertInUse:                     409,
	EnrollTokenInvalid:            0,
	NodeClusterConflict:           409,
	NodeNotOnline:                 409,
	DeploymentNotPausable:         409,
	DeploymentNotResumable:        409,
	DeploymentNotRetryable:        409,
	SiteNoClusterBound:            409,
	OriginGroupInconsistent:       400,
	SetupTokenInvalid:             401,
	SetupAlreadyInstalled:         410,
	TenantQuotaExceeded:           403,
	InternalError:                 500,
}
