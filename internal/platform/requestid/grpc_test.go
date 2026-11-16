package requestid

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// 用真实 gRPC 服务端（grpc 自带的 health 服务，回环 TCP）验证：出站元数据 x-request-id、服务端入口读取/生成/拒绝非法值。
type captured struct {
	mu  sync.Mutex
	ids []string
}

func (c *captured) add(id string) { c.mu.Lock(); c.ids = append(c.ids, id); c.mu.Unlock() }
func (c *captured) count() int    { c.mu.Lock(); defer c.mu.Unlock(); return len(c.ids) }
func (c *captured) last(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.ids) == 0 {
		t.Fatal("服务端未捕获到 Request ID")
	}
	return c.ids[len(c.ids)-1]
}

func startHealthServer(t *testing.T, gen Generator, cap *captured) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("无法监听回环端口: %v", err)
	}
	record := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		id, _ := FromContext(ctx)
		cap.add(id)
		return h(ctx, req)
	}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(UnaryServerInterceptor(gen), record),
		grpc.ChainStreamInterceptor(StreamServerInterceptor(gen), func(s any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			id, _ := FromContext(ss.Context())
			cap.add(id)
			return h(s, ss)
		}),
	)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(UnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(StreamClientInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestGRPCRequestIDPropagation(t *testing.T) {
	cap := &captured{}
	conn := startHealthServer(t, testGen(), cap)
	client := healthpb.NewHealthClient(conn)

	// 1. 客户端上下文有 Request ID：元数据原样传到服务端。
	ctx, _ := NewContext(context.Background(), goodID)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := client.Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := cap.last(t); got != goodID {
		t.Errorf("服务端应收到客户端的 Request ID %q，实际 %q", goodID, got)
	}

	// 2. 客户端没有 Request ID：服务端生成合法 ULID。
	if _, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := cap.last(t); got == "" || got == goodID {
		t.Errorf("缺失时服务端应生成新的 Request ID，实际 %q", got)
	}

	// 3. 客户端手工塞入非法值：服务端丢弃并生成。
	bad := metadata.AppendToOutgoingContext(context.Background(), MetadataKey, "not-a-ulid-xyz")
	if _, err := client.Check(bad, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := cap.last(t); got == "not-a-ulid-xyz" || len(got) != 26 {
		t.Errorf("非法元数据值必须被替换，实际 %q", got)
	}

	// 4. 流式调用同样处理。
	sctx, scancel := context.WithTimeout(ctx, 5*time.Second)
	defer scancel()
	stream, err := client.Watch(sctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if got := cap.last(t); got != goodID {
		t.Errorf("流式调用应沿用 Request ID，实际 %q", got)
	}
}

func TestAppendToOutgoingReplacesAndNoopWithoutID(t *testing.T) {
	base := context.Background()
	if got := AppendToOutgoing(base); got != base {
		t.Error("上下文无 Request ID 时应原样返回")
	}
	ctx := metadata.AppendToOutgoingContext(base, MetadataKey, "01BX5ZZKBKACTAV9WEVGEMMVRZ", "other", "v")
	ctx, _ = NewContext(ctx, goodID)
	md, _ := metadata.FromOutgoingContext(AppendToOutgoing(ctx))
	if vals := md.Get(MetadataKey); len(vals) != 1 || vals[0] != goodID {
		t.Errorf("应覆盖为唯一值 %q，实际 %v", goodID, vals)
	}
	if md.Get("other")[0] != "v" {
		t.Error("其他元数据应保留")
	}
	if FromIncoming(base) != "" {
		t.Error("无入站元数据应返回空")
	}
}

func TestServerInterceptorGeneratorFailureIsInternalWithoutDetail(t *testing.T) {
	cap := &captured{}
	conn := startHealthServer(t, failGen{}, cap)
	_, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err == nil {
		t.Fatal("生成器失败应使调用失败")
	}
	if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "熵源") {
		t.Errorf("应为 Internal 且不泄漏原因: %v", err)
	}
	if cap.count() != 0 {
		t.Error("生成失败时不应进入 handler")
	}

	// 流式调用同样 fail-closed。
	stream, err := healthpb.NewHealthClient(conn).Watch(context.Background(), &healthpb.HealthCheckRequest{})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "熵源") {
		t.Errorf("流式调用应为 Internal 且不泄漏原因: %v", err)
	}
	if cap.count() != 0 {
		t.Error("流式生成失败时不应进入 handler")
	}
}
