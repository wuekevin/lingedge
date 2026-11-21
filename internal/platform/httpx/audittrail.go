package httpx

import (
	"context"
	"sync/atomic"
)

// 审计「已记录」登记（Story 1.21）：审计中间件在调用审计钩子之前，为每个请求在上下文里放一个计数器；审计记录器
// （internal/platform/audit）在业务事务内写入 audit_logs 行后调用 NoteAudited 登记一次。请求结束时计数交给审计钩子的 end（AuditOutcome.Recorded），
// 用来发现「声明了 x-audit: true 的成功请求却没有任何审计行」。

type auditTrailKey struct{}

type auditTrail struct{ n atomic.Int32 }

// NoteAudited 登记「本请求已写入一条审计记录」；对不经审计中间件的上下文（后台作业、单元测试）是空操作。
func NoteAudited(ctx context.Context) {
	if t, ok := ctx.Value(auditTrailKey{}).(*auditTrail); ok && t != nil {
		t.n.Add(1)
	}
}

type userAgentKey struct{}

// UserAgentFrom 取出本请求的 User-Agent（由路由器登记，供审计记录器使用；不经路由器时返回空串）。
func UserAgentFrom(ctx context.Context) string {
	s, _ := ctx.Value(userAgentKey{}).(string)
	return s
}

func withUserAgent(ctx context.Context, ua string) context.Context {
	return context.WithValue(ctx, userAgentKey{}, ua)
}
