package hostprobeit

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func TestProbeRedisRealInstances(t *testing.T) {
	ctx := context.Background()
	open := redistest.Start(t)
	ep := probe.Endpoint{Host: "127.0.0.1", Port: open.Port}
	wantVer := probe.ParseRedisServerVersion(redistest.Version())
	if wantVer == "" {
		t.Fatalf("无法取得 redis-server 版本: %q", redistest.Version())
	}
	st := probe.ProbeRedis(ctx, ep, probe.Creds{}, 0)
	if !st.Reachable || st.AuthRequired || st.Authenticated || st.Version != wantVer || st.Error != "" || st.Flavor == "" {
		t.Fatalf("无口令实例: %+v（期望版本 %s）", st, wantVer)
	}

	// 带口令的实例：口令在运行期随机生成
	pw := mysqltest.RandomSecret(t)
	locked := redistest.Start(t, redistest.Options{Password: pw})
	lep := probe.Endpoint{Host: "127.0.0.1", Port: locked.Port}
	// 1) 未提供凭据：可达、需要口令，无版本；不会尝试任何其他命令
	st = probe.ProbeRedis(ctx, lep, probe.Creds{}, 0)
	if !st.Reachable || !st.AuthRequired || st.Authenticated || st.Version != "" {
		t.Fatalf("需要口令且未提供: %+v", st)
	}
	// 2) 正确口令：认证并取到版本
	before := redisCommandstats(t, lep, pw)
	st = probe.ProbeRedis(ctx, lep, probe.Creds{RedisPassword: pw}, 0)
	if !st.Reachable || !st.AuthRequired || !st.Authenticated || st.Version != wantVer || st.Error != "" {
		t.Fatalf("正确口令: %+v", st)
	}
	after := redisCommandstats(t, lep, pw)
	// 3) 只读证明：探测期间服务器端执行过的命令只可能是 ping/auth/info（commandstats 的增量）
	for cmd, n := range after {
		if n > before[cmd] && cmd != "ping" && cmd != "auth" && cmd != "info" {
			t.Errorf("探测期间执行了非只读命令 %s（%d → %d）", cmd, before[cmd], n)
		}
	}
	// PING 在要求认证的服务器上被 NOAUTH 拒绝（计入 rejected_calls，不计 calls），所以只断言 AUTH 与 INFO 有增量
	if after["auth"] <= before["auth"] || after["info"] <= before["info"] {
		t.Errorf("commandstats 应记录到 AUTH/INFO: %v → %v", before, after)
	}
	// 4) 错误口令：AUTH 被拒绝；口令不出现在任何输出
	wrong := "wrong-" + pw
	st = probe.ProbeRedis(ctx, lep, probe.Creds{RedisPassword: wrong}, 0)
	if !st.Reachable || st.Authenticated || !strings.Contains(st.Error, "AUTH 被拒绝") {
		t.Fatalf("错误口令: %+v", st)
	}
	for _, p := range []string{pw, wrong} {
		if b, _ := json.Marshal(st); strings.Contains(string(b), p) {
			t.Fatalf("口令出现在结果里: %s", b)
		}
	}
	// 5) 不可达
	st = probe.ProbeRedis(ctx, probe.Endpoint{Host: "127.0.0.1", Port: closedPort(t)}, probe.Creds{}, time.Second)
	if st.Reachable || st.Error == "" {
		t.Fatalf("不可达: %+v", st)
	}
	// 6) 非 Redis 服务（回复 MySQL 握手包）：不当作 Redis
	notRedis := listenAnd(t, func(c net.Conn) {
		_, _ = c.Write([]byte{5, 0, 0, 0, 10, '8', '.', '0', 0})
		time.Sleep(200 * time.Millisecond)
	})
	if st := probe.ProbeRedis(ctx, notRedis, probe.Creds{}, time.Second); st.Reachable {
		t.Fatalf("非 Redis 服务不应判为可达: %+v", st)
	}
}
