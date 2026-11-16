package tenantscope

import "context"

type ctxKey struct{}

// NewContext 把 Scope 放入 context；只应由 tenantscope 中间件/任务运行时调用（受 [tenantscope-mint] 规则约束）。
func NewContext(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext 取出中间件放入的 Scope；不存在或为零值时 ok=false（调用方应拒绝请求，不得继续）。
func FromContext(ctx context.Context) (Scope, bool) {
	s, ok := ctx.Value(ctxKey{}).(Scope)
	if !ok || s.IsZero() {
		return Scope{}, false
	}
	return s, true
}
