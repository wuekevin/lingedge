// Package requestid 实现 Request ID 的入口读取/生成与上下文传递（NFR-21、ADR-017 §6）。
//
// 信任边界：Request ID 由客户端提供，只用于关联日志/审计/Task，不得作为授权或唯一性依据；非 ULID 的上游 ID（如反代的 $request_id）
// 会被丢弃并重新生成（需要保留上游 ID 时由反代另设头并单独记录）。
//
// 约定：Request ID 是 ULID。入口（HTTP 中间件、gRPC 服务端拦截器）读取请求携带的 X-Request-ID / x-request-id，
// 合法（规范 ULID，允许小写输入并规整为大写）则沿用，缺失或非法一律丢弃并生成新的——避免把客户端任意字符串
// 写进日志（日志注入）、审计与 tasks.request_id（CHAR(26)）。之后经 context 取得：
//   - 日志：logx.WithContext(ctx, logger) 自动附 request_id 字段；
//   - 审计 / Task：用 FromContext(ctx) 取值写入 audit_logs / tasks.request_id；
//   - 出站 gRPC：UnaryClientInterceptor 或 AppendToOutgoing 把它放进元数据 x-request-id；
//   - 响应：SetResponseHeader 写回 X-Request-ID（每个响应必返回）。
package requestid

import (
	"context"
	"net/http"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

const (
	// Header 是 HTTP 请求/响应头名。
	Header = "X-Request-ID"
	// MetadataKey 是 gRPC 元数据键（元数据键一律小写）。
	MetadataKey = "x-request-id"
)

// Generator 生成新的 Request ID（*uid.Generator 满足该接口）。
type Generator interface {
	New() (string, error)
}

type ctxKey struct{}

// NewContext 返回携带 id 的子上下文。id 应来自 Resolve；传入非规范 ULID 会被拒绝（返回原 ctx 并报告 false），
// 以保证上下文里的值总是可安全写入日志与 CHAR(26) 列。
func NewContext(ctx context.Context, id string) (context.Context, bool) {
	n, ok := uid.Normalize(id)
	if !ok {
		return ctx, false
	}
	return context.WithValue(ctx, ctxKey{}, n), true
}

// FromContext 取出上下文里的 Request ID。
func FromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok && id != ""
}

// Resolve 决定一个入站请求使用的 Request ID：incoming 是规范 ULID（或其小写形式）则规整后沿用，否则由 gen 生成新的。
func Resolve(incoming string, gen Generator) (string, error) {
	if n, ok := uid.Normalize(incoming); ok {
		return n, nil
	}
	return gen.New()
}

// Ensure 在 ctx 中已有 Request ID 时原样返回，否则按 incoming/gen 解析并写入。
func Ensure(ctx context.Context, incoming string, gen Generator) (context.Context, string, error) {
	if id, ok := FromContext(ctx); ok {
		return ctx, id, nil
	}
	id, err := Resolve(incoming, gen)
	if err != nil {
		return ctx, "", err
	}
	ctx, _ = NewContext(ctx, id)
	return ctx, id, nil
}

// SetResponseHeader 把 id 写入响应头（覆盖已有值）。
func SetResponseHeader(h http.Header, id string) { h.Set(Header, id) }

// Middleware 是 net/http 形态的入口中间件：读取/生成 Request ID，写入请求上下文与响应头，再调用 next。
// 生成失败（crypto/rand 不可用）时按统一错误体返回 500（无可用 ID，request_id 为空、不带 X-Request-ID）且不调用 next；响应体不含原因。
func Middleware(gen Generator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, id, err := Ensure(r.Context(), r.Header.Get(Header), gen)
			if err != nil {
				apierr.Write(w, apierr.Internal(err), "")
				return
			}
			SetResponseHeader(w.Header(), id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
