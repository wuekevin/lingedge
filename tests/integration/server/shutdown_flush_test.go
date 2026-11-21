package serverintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/auth"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/server"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// Story 1.16 遗留（Story 1.17 接入）：server 优雅停机流程要调用 auth Module.Flush，使异步的「DB 兜底失败计数」在 MySQL 关闭前落库。
// 本测试在进程内用真实 server.Run（真实 MySQL/Redis/主密钥）装配真实认证模块，其接线方式与 cmd/lingedge-server/assembly.go 相同：
// 经 Deps.OnShutdown 登记 Flush。断言：①停机钩子被调用；②调用时监听已关闭（在途请求已排空）而 MySQL 仍可用（钩子在资源关闭之前）；
// ③失败登录的异步计数在 Run 返回后已全部落库；④Deps 带有信封加密（Secrets）。
func TestShutdownFlushesAuthBackgroundWrites(t *testing.T) {
	e := newEnv(t, all)
	rs, err := server.ParseRoles("api")
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	lg, err := logx.New(logx.Options{Level: "info", Output: logs})
	if err != nil {
		t.Fatal(err)
	}
	cfg := e.loadConfig(t)

	ids := uid.NewGenerator(clock.System())
	email := strings.ToLower(ids.MustNew()) + "@example.test"
	e.root(t, "INSERT INTO §.users (uid, user_type, email, display_name, password_hash, status) VALUES (?, 'platform', ?, 'u', ?, 'active')",
		ids.MustNew(), email, "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$"+strings.Repeat("A", 43)) // 哈希格式合法即可：本测试只发错误口令

	var m auth.Module
	var hookRan, listenerClosedAtHook, dbUpAtHook bool
	var addr string
	opts := server.Options{
		Config: cfg, Roles: rs, Logger: lg,
		BuildHooks: func(d server.Deps) (httpx.Hooks, error) {
			if d.Secrets == nil || d.OnShutdown == nil {
				return httpx.Hooks{}, fmt.Errorf("Deps 应带 Secrets 与 OnShutdown")
			}
			var err error
			aw, aerr := audit.New(audit.Options{Pool: d.DB, IDs: d.IDs, Clock: d.Clock, Log: d.Logger})
			if aerr != nil {
				return httpx.Hooks{}, aerr
			}
			m, err = auth.New(context.Background(), auth.Options{Pool: d.DB, Redis: d.Redis, Clock: d.Clock, IDs: d.IDs, Log: d.Logger, Argon2: cfg.Auth.Argon2, Box: d.Secrets, Audit: aw})
			if err != nil {
				return httpx.Hooks{}, err
			}
			pool := d.DB
			d.OnShutdown("认证模块后台写入", func(ctx context.Context) error {
				hookRan = true
				_, cerr := http.Get("http://" + addr + "/healthz") //nolint:noctx // 探测监听是否已关闭
				listenerClosedAtHook = cerr != nil
				dbUpAtHook = pool.Ping(ctx) == nil
				m.Flush()
				return nil
			})
			return httpx.WithSelfService(httpx.Hooks{Authenticator: m.Authenticator(), Auditor: aw.Auditor()}, m.SelfService), nil
		},
		Register: func(r *httpx.Router, _ server.Deps) error { return m.Register(r) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan server.Info, 1)
	done := make(chan error, 1)
	opts.OnReady = func(i server.Info) { ready <- i }
	go func() { done <- server.Run(ctx, opts) }()
	t.Cleanup(cancel)
	var info server.Info
	select {
	case info = <-ready:
	case err := <-done:
		t.Fatalf("启动失败: %v\n%s", err, logs.String())
	case <-time.After(30 * time.Second):
		t.Fatal("启动超时")
	}
	addr = info.HTTPAddr

	const attempts = 5
	for i := 0; i < attempts; i++ {
		b, _ := json.Marshal(map[string]string{"realm": "admin", "email": email, "password": mysqltest.RandomSecret(t) + "Zz9"})
		resp, err := http.Post("http://"+addr+"/api/v1/auth/login", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("错误口令应 401: %d", resp.StatusCode)
		}
	}
	cancel() // 立即停机：异步的 DB 兜底计数可能仍在途
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("优雅停机应无错误: %v\n%s", err, logs.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("停机超时")
	}
	if !hookRan {
		t.Fatal("停机流程必须调用已登记的 auth Flush 钩子")
	}
	if !listenerClosedAtHook || !dbUpAtHook {
		t.Errorf("钩子应在监听排空之后、MySQL 关闭之前运行: listenerClosed=%v dbUp=%v", listenerClosedAtHook, dbUpAtHook)
	}
	if n := e.rootInt(t, "SELECT failed_login_count FROM §.users WHERE email = ?", email); n != attempts {
		t.Errorf("异步失败计数应在停机前全部落库: 期望 %d 实际 %d\n%s", attempts, n, logs.String())
	}
}
