// Command schedproc 是集成测试用的「真实 Scheduler 子进程」：与生产同一份 schedrt.Runner（schedkit.Build），连接测试自拉起的真实 MySQL。
// 测试对它发送 SIGKILL（kill -9）做故障注入，观察另一实例接管。只在 tests/ 下构建，不属于任何发布产物。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/tests/integration/server/schedkit"
)

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "schedproc:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log, err := logx.New(logx.Options{Level: "info", Format: "json", Output: os.Stdout})
	if err != nil {
		return err
	}
	app, err := db.OpenApp(ctx, db.Config{Host: os.Getenv("SP_DB_HOST"), Port: envInt("SP_DB_PORT", 3306), Name: os.Getenv("SP_DB_NAME"),
		User: os.Getenv("SP_DB_USER"), Password: config.NewSecret(os.Getenv("SP_DB_PASS")), MaxOpenConns: 8, MaxIdleConns: 4})
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()
	r, err := schedkit.Build(schedkit.Cfg{
		App: app, Instance: os.Getenv("SP_INSTANCE"), Logger: log, Builtin: true,
		TTL: time.Duration(envInt("SP_TTL_MS", 2000)) * time.Millisecond, Tick: time.Duration(envInt("SP_TICK_MS", 100)) * time.Millisecond,
	})
	if err != nil {
		return err
	}
	return r.Run(ctx)
}
