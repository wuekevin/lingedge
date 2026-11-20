package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/lineage"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

const lineageUsage = `用法: lingedge-server lineage <show|bump> [--config FILE] [--json]

  show   输出当前 system_settings.deploy.lineage
  bump   单事务把 deploy.lineage 提升为 max(当前值+1, 当前 UNIX 秒) 并写审计；重复执行严格单调递增。
         供 Tools 在「数据库恢复」之后、启动 Server 之前调用（ADR-002 §12、tools-design §17.5）。
         设置项不存在（安装未完成）时拒绝。

使用 server.yaml 的 app 账号（database.*；口令经 LINGEDGE_DATABASE__PASSWORD 或 database.password_file），不需要迁移账号。
退出码: 0 成功；1 失败；2 用法错误
`

func lineageMain(args []string, stdout, stderr io.Writer, environ []string) int {
	if len(args) == 0 || (args[0] != "show" && args[0] != "bump") {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
			_, _ = fmt.Fprint(stdout, lineageUsage)
			return 0
		}
		_, _ = fmt.Fprint(stderr, lineageUsage)
		return 2
	}
	sub := args[0]
	fl := flag.NewFlagSet("lineage "+sub, flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "server.yaml 路径（缺省 $"+EnvConfig+"，再缺省 "+DefaultConfigPath+"）")
	asJSON := fl.Bool("json", false, "以 JSON 输出")
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	if fl.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "lineage %s: 多余的参数 %q\n", sub, fl.Arg(0))
		return 2
	}
	cfg, err := loadConfig(*cfgPath, environ, false)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "lineage: %v\n", err)
		return 1
	}
	if err := requireDatabase(cfg); err != nil {
		_, _ = fmt.Fprintf(stderr, "lineage: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	pool, err := db.OpenApp(ctx, db.AppConfigFrom(cfg.Database))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "lineage: 无法连接数据库: %v\n", err)
		return 1
	}
	defer func() { _ = pool.Close() }()
	clk := clock.System()
	svc, err := lineage.New(pool, clk, uid.NewGenerator(clk))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "lineage: %v\n", err)
		return 1
	}
	if sub == "show" {
		v, err := svc.Show(ctx)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "lineage show: %v\n", err)
			return 1
		}
		if *asJSON {
			_ = json.NewEncoder(stdout).Encode(map[string]int64{"lineage": v})
		} else {
			_, _ = fmt.Fprintf(stdout, "%d\n", v)
		}
		return 0
	}
	res, err := svc.Bump(ctx)
	if err != nil {
		if errors.Is(err, lineage.ErrNotInstalled) {
			_, _ = fmt.Fprintf(stderr, "lineage bump: %v\n", err)
		} else {
			_, _ = fmt.Fprintf(stderr, "lineage bump: 失败（事务已回滚，lineage 未改变）: %v\n", err)
		}
		return 1
	}
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(map[string]any{"before": res.Before, "after": res.After, "request_id": res.RequestID})
	} else {
		_, _ = fmt.Fprintf(stdout, "deploy.lineage: %d -> %d（request_id=%s）\n", res.Before, res.After, res.RequestID)
	}
	return 0
}

// requireDatabase 检查 database 节的必需项（lineage/迁移类命令不需要 Redis 与 server.env）。
func requireDatabase(c *config.Config) error {
	if c.Database.Host == "" || c.Database.Name == "" || c.Database.User == "" || !c.Database.Password.IsSet() {
		return errors.New("缺少数据库连接配置：需要 database.host/name/user 与口令（环境变量 LINGEDGE_DATABASE__PASSWORD 或 database.password_file）")
	}
	return nil
}
