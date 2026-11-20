package serverintegration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/server"
)

type runningServer struct {
	info   server.Info
	cancel context.CancelFunc
	done   chan error
	logs   *syncBuf
}

// runInProcess 在进程内用 server.Run 起一个真实装配（真实 MySQL/Redis），返回 OnReady 报告的信息。
func runInProcess(t *testing.T, e *env, roles string) *runningServer {
	t.Helper()
	rs, err := server.ParseRoles(roles)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	lg, err := logx.New(logx.Options{Level: "info", Output: logs})
	if err != nil {
		t.Fatal(err)
	}
	cfg := e.loadConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan server.Info, 1)
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, server.Options{Config: cfg, Roles: rs, Logger: lg, OnReady: func(i server.Info) { ready <- i }})
	}()
	t.Cleanup(cancel)
	select {
	case info := <-ready:
		return &runningServer{info: info, cancel: cancel, done: done, logs: logs}
	case err := <-done:
		t.Fatalf("启动失败: %v\n%s", err, logs.String())
	case <-time.After(30 * time.Second):
		t.Fatal("启动超时")
	}
	return nil
}

func (r *runningServer) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("优雅停机应无错误: %v\n%s", err, r.logs.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("停机超时")
	}
}

// AC1：roles 矩阵——各角色子集只装配其角色：api 才监听 HTTP；metrics 恒在；grpc/scheduler/worker 仅生命周期骨架（日志标明由哪个 Story 实现）。
func TestRolesMatrix(t *testing.T) {
	e := newEnv(t, all)
	httpAddr := "127.0.0.1:" + strconv.Itoa(e.ports.http)
	cases := []struct {
		roles   string
		wantAPI bool
		skel    []string
	}{
		{"api", true, nil},
		{"worker", false, []string{"worker"}},
		{"scheduler", false, []string{"scheduler"}},
		{"grpc", false, []string{"grpc"}},
		{"api,worker", true, []string{"worker"}},
		{"scheduler,worker", false, []string{"scheduler", "worker"}},
		{"", true, []string{"grpc", "scheduler", "worker"}}, // 默认全开
		{"api,grpc,scheduler,worker", true, []string{"grpc", "scheduler", "worker"}},
	}
	for _, c := range cases {
		name := c.roles
		if name == "" {
			name = "(默认)"
		}
		t.Run(name, func(t *testing.T) {
			r := runInProcess(t, e, c.roles)
			if r.info.MetricsAddr == "" {
				t.Fatal("metrics 对所有角色常开")
			}
			if code, body, _ := httpGet(t, "http://"+r.info.MetricsAddr+"/metrics"); code != 200 || !strings.Contains(string(body), "lingedge_redis_up") {
				t.Fatalf("metrics: %d", code)
			}
			if c.wantAPI {
				if r.info.HTTPAddr != httpAddr {
					t.Fatalf("api 应监听 %s: %q", httpAddr, r.info.HTTPAddr)
				}
				if code, _, _ := httpGet(t, "http://"+httpAddr+"/healthz"); code != 200 {
					t.Fatalf("/healthz: %d", code)
				}
			} else {
				if r.info.HTTPAddr != "" || !dialRefused(httpAddr) {
					t.Fatalf("未启用 api 角色时不得监听 HTTP: %q", r.info.HTTPAddr)
				}
			}
			for _, rl := range []string{"grpc", "scheduler", "worker"} {
				want := false
				for _, s := range c.skel {
					want = want || s == rl
				}
				logged := strings.Contains(r.logs.String(), `"role":"`+rl+`"`) && strings.Contains(r.logs.String(), "仅生命周期骨架")
				if want != logged {
					t.Errorf("角色 %s 骨架装配 = %v，期望 %v\n%s", rl, logged, want, r.logs.String())
				}
			}
			if !strings.Contains(r.logs.String(), "Story 1.14") && containsRole(c.skel, "worker") {
				t.Errorf("worker 骨架日志应标明由 Story 1.14 实现")
			}
			r.stop(t)
			if !dialRefused(r.info.MetricsAddr) {
				t.Error("停机后 metrics 端口应关闭")
			}
			if c.wantAPI && !dialRefused(httpAddr) {
				t.Error("停机后 API 端口应关闭")
			}
		})
	}
}

func containsRole(list []string, r string) bool {
	for _, x := range list {
		if x == r {
			return true
		}
	}
	return false
}

// 迁移版本探针（CC-3）：app 账号只读 schema_migrations——已迁移库就绪；未迁移库（app 无授权）fail-closed 为 503/unknown。
func TestReadyMigrationProbeFailClosedWhenUnmigrated(t *testing.T) {
	e := newEnv(t, m1Only)
	r := runInProcess(t, e, "api")
	defer r.stop(t)
	resp, err := http.Get("http://" + r.info.HTTPAddr + "/healthz/ready")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var rep httpx.ReadyReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 503 || rep.Checks["migrations"].Status != "unknown" {
		t.Fatalf("未迁移的库应 503 且 migrations=unknown: %d %s", resp.StatusCode, b)
	}
	if strings.Contains(string(b), "Error") || strings.Contains(string(b), "denied") {
		t.Errorf("响应不得含错误原文: %s", b)
	}
	// 迁移后恢复就绪（探针随库状态变化，无需重启）。
	if _, err := e.upAll(t); err != nil {
		t.Fatal(err)
	}
	eventually(t, "迁移后就绪", 10*time.Second, func() bool {
		resp, err := http.Get("http://" + r.info.HTTPAddr + "/healthz/ready")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == 200
	})
}
