package setupit

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wuekevin/lingedge/tests/redistest"
)

// redisRaw 以原生客户端对测试 Redis 执行一条命令（仅测试断言用），返回字符串结果。
func redisRaw(t testing.TB, in *redistest.Instance, pw string, args ...any) string {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: in.Addr(), Password: pw})
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v, err := c.Do(ctx, args...).Result()
	if err != nil && err != redis.Nil {
		t.Fatalf("redis %v: %v", args[0], err)
	}
	s, _ := v.(string)
	return s
}
