package requestid

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// FromIncoming 读取 gRPC 入站元数据里的 x-request-id（未校验，需经 Resolve 才可信）。
func FromIncoming(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get(MetadataKey); len(v) > 0 {
		return v[0]
	}
	return ""
}

// AppendToOutgoing 把上下文里的 Request ID 放入出站元数据 x-request-id（覆盖已有同名键，不产生重复值）。
// 上下文里没有 Request ID 时原样返回。
func AppendToOutgoing(ctx context.Context) context.Context {
	id, ok := FromContext(ctx)
	if !ok {
		return ctx
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(MetadataKey, id)
	return metadata.NewOutgoingContext(ctx, md)
}

// UnaryClientInterceptor 在每次出站一元调用上附带 x-request-id。
func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(AppendToOutgoing(ctx), method, req, reply, cc, opts...)
	}
}

// StreamClientInterceptor 在每次出站流式调用上附带 x-request-id。
func StreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(AppendToOutgoing(ctx), desc, cc, method, opts...)
	}
}

// UnaryServerInterceptor 在服务端入口读取/生成 Request ID 并写入 handler 的上下文。
func UnaryServerInterceptor(gen Generator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, _, err := Ensure(ctx, FromIncoming(ctx), gen)
		if err != nil {
			return nil, status.Error(codes.Internal, "internal error")
		}
		return handler(ctx, req)
	}
}

// StreamServerInterceptor 是流式调用的服务端对应物。
func StreamServerInterceptor(gen Generator) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, _, err := Ensure(ss.Context(), FromIncoming(ss.Context()), gen)
		if err != nil {
			return status.Error(codes.Internal, "internal error")
		}
		return handler(srv, &ctxStream{ServerStream: ss, ctx: ctx})
	}
}

type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }
