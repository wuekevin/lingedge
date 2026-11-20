package handler

import (
	"net/http"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/requestid"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// adminAuditor 是 /admin 管理路由的审计钩子过渡实现（Story 1.18）：**不是 audit_logs**。
//
// audit 模块（Story 1.21）尚未实现，写操作的 audit_logs 无法写入，也不假装已写：本钩子对「本模块的 /admin 管理 operation」只写一条不含
// 敏感信息的结构化日志（operation、操作者 id、HTTP 状态、request_id；明确标注 audit_logs_written=false），放行请求；
// 其他任何要求审计（x-audit: true）的 operation 仍委托给原钩子（缺省 DenyAuditor：500），不被放宽。
// 1.21 交付后用真实 Auditor 替换它（待 1.21 补写 audit_logs：actor / 目标租户 / 资源 / 变更摘要，禁含口令与密钥）。
type adminAuditor struct {
	inner httpx.Auditor
	log   *zap.Logger
}

var _ httpx.Auditor = adminAuditor{}

func (a adminAuditor) Begin(r *http.Request, meta httpx.RouteMeta) (func(httpx.AuditOutcome), error) {
	if !meta.Audit || !IsAdminOperation(meta.OperationID) {
		return a.inner.Begin(r, meta)
	}
	var actor uint64
	if p, ok := httpx.PrincipalFrom(r.Context()); ok {
		if dp, ok := p.(*domain.Principal); ok {
			actor = dp.ActorID()
		}
	}
	id, _ := requestid.FromContext(r.Context())
	scope := ""
	if s, ok := tenantscope.FromContext(r.Context()); ok {
		scope = s.String()
	}
	return func(o httpx.AuditOutcome) {
		a.log.Info("管理写操作（结构化日志；audit_logs 待 Story 1.21 补写）",
			zap.String("operation_id", meta.OperationID), zap.Uint64("actor_id", actor), zap.Int("status", o.Status),
			zap.Bool("replayed", o.Replayed), zap.Bool("panicked", o.Panicked), zap.String("scope", scope), zap.String("request_id", id),
			zap.Bool("audit_logs_written", false))
	}, nil
}
