package serverintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/cmd/lingedge-server/assembly"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/server"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// Story 1.22b：server.trusted_proxies 经配置文件进入生产装配（server.Run → httpx.Router），REST 来源 IP 的唯一来源：
//   - 缺省（仅 127.0.0.1/::1）：来自本机反代的 X-Forwarded-For 被采信，登录日志记录的是 XFF 链上的真实来源；
//   - 配置成别的反代地址：本机直连的对端不再可信，伪造的 X-Forwarded-For 被忽略，登录日志记录的是对端地址 127.0.0.1。
//
// 登录日志与登录节流、API Token 的 allowed_cidrs 取同一个值（httpx.ClientIPFrom / c.ClientIP()）；后两者的行为见 tests/integration/auth。
func TestTrustedProxiesFromConfigDeterminesSourceIP(t *testing.T) {
	const spoofed = "198.51.100.7"
	for _, tc := range []struct {
		name, yamlLine, wantIP string
	}{
		{"缺省仅信任本机反代：XFF 被采信", "", spoofed},
		{"显式信任本机反代：XFF 被采信", "  trusted_proxies: ['127.0.0.1', '::1']\n", spoofed},
		{"信任的是另一台反代：本机来源的伪造 XFF 被忽略", "  trusted_proxies: ['203.0.113.0/24']\n", "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, all)
			b, err := os.ReadFile(e.cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			nb := bytes.Replace(b, []byte("server:\n  env: test\n"), []byte("server:\n  env: test\n"+tc.yamlLine), 1)
			if tc.yamlLine != "" && bytes.Equal(b, nb) {
				t.Fatal("测试配置里没有 server 节（格式变化？）")
			}
			if err := os.WriteFile(e.cfgPath, nb, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := e.loadConfig(t) // 经 YAML → config.Load：同时验证配置键被接受
			rs, _ := server.ParseRoles("api")
			logs := &syncBuf{}
			lg, err := logx.New(logx.Options{Level: "info", Output: logs})
			if err != nil {
				t.Fatal(err)
			}
			asm := assembly.New()
			ctx, cancel := context.WithCancel(context.Background())
			ready := make(chan server.Info, 1)
			done := make(chan error, 1)
			go func() {
				done <- server.Run(ctx, server.Options{Config: cfg, Roles: rs, Logger: lg, BuildHooks: asm.BuildHooks, Register: asm.Register,
					OnReady: func(i server.Info) { ready <- i }})
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(30 * time.Second):
				}
			})
			var info server.Info
			select {
			case info = <-ready:
			case err := <-done:
				t.Fatalf("启动失败: %v\n%s", err, logs.String())
			case <-time.After(30 * time.Second):
				t.Fatal("启动超时")
			}

			ids := uid.NewGenerator(clock.System())
			email := strings.ToLower(ids.MustNew()) + "@example.test"
			body, _ := json.Marshal(map[string]string{"realm": "admin", "email": email, "password": mysqltest.RandomSecret(t) + "Zz9"})
			req, _ := http.NewRequest(http.MethodPost, "http://"+info.HTTPAddr+"/api/v1/auth/login", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Forwarded-For", spoofed)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Fatalf("未知账号登录应 401: %d", resp.StatusCode)
			}
			// 登录日志是同步写入的（不是异步兜底）；记录的 ip 即路由器解析出的来源 IP。
			got := e.rootString(t, "SELECT INET6_NTOA(ip) FROM §.login_logs WHERE identifier = ? ORDER BY id DESC LIMIT 1", email)
			if got != tc.wantIP {
				t.Errorf("登录日志来源 IP = %q，期望 %q", got, tc.wantIP)
			}
			if !strings.Contains(logs.String(), `"client_ip":"`+tc.wantIP+`"`) {
				t.Errorf("访问日志的 client_ip 应与登录日志同源 %q\n%s", tc.wantIP, logs.String())
			}
		})
	}
}
