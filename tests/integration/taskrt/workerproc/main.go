// Command workerproc 是集成测试用的「真实 Worker 子进程」：与生产同一份 taskrt.Worker 代码，连接测试自拉起的真实 MySQL/Redis，
// 登记 retention.cleanup 测试处理函数（handlers.Exec）。测试对它发送 SIGKILL（kill -9）/SIGSTOP/SIGCONT 做故障注入。
// 只在 tests/ 下构建，不属于任何发布产物。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	taskrepo "github.com/wuekevin/lingedge/internal/platform/taskrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/tests/integration/taskrt/handlers"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "workerproc:", err)
		os.Exit(1)
	}
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log, err := logx.New(logx.Options{Level: "info", Format: "json", Output: os.Stdout})
	if err != nil {
		return err
	}
	app, err := db.OpenApp(ctx, db.Config{Host: os.Getenv("WP_DB_HOST"), Port: envInt("WP_DB_PORT", 3306), Name: os.Getenv("WP_DB_NAME"),
		User: os.Getenv("WP_DB_USER"), Password: config.NewSecret(os.Getenv("WP_DB_PASS")), MaxOpenConns: 8, MaxIdleConns: 4})
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()
	ids := uid.NewGenerator(clock.System())
	store, err := taskrepo.New(app, ids)
	if err != nil {
		return err
	}
	owner := os.Getenv("WP_OWNER")
	reg := taskrt.NewRegistry()
	if err := reg.Register(taskrt.Spec{Type: taskrt.TypeRetentionCleanup, Handler: handlers.Exec(app, owner)}); err != nil {
		return err
	}
	var rc *redisx.Client
	if addr := os.Getenv("WP_REDIS"); addr != "" {
		if rc, err = redisx.New(ctx, redisx.Config{Addr: addr, Env: "test"}, redisx.Options{Logger: log}); err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()
	}
	w, err := taskrt.NewWorker(taskrt.WorkerConfig{
		Store: store, Registry: reg, Logger: log, Redis: rc, Owner: owner,
		Lease:        time.Duration(envInt("WP_LEASE_MS", 30000)) * time.Millisecond,
		PollInterval: time.Duration(envInt("WP_POLL_MS", 200)) * time.Millisecond,
		Concurrency:  envInt("WP_CONC", 2),
		Backoff:      taskrt.BackoffConfig{Base: 100 * time.Millisecond, Max: time.Second},
		DrainTimeout: 3 * time.Second,
		Scope:        tenantscope.PlatformForJob(0),
	})
	if err != nil {
		return err
	}
	fmt.Println("workerproc ready", owner)
	return w.Run(ctx)
}
