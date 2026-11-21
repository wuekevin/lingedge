package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/wuekevin/lingedge/cmd/lingedge-server/assembly"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/server"
	"github.com/wuekevin/lingedge/internal/platform/setup"
)

// EnvConfig 是 --config 的环境变量形态（flag 优先）。
const EnvConfig = "LINGEDGE_CONFIG"

// DefaultConfigPath 是 Tools 安装的 server.yaml 位置（tools-design §13）。
const DefaultConfigPath = "/etc/lingedge/server/server.yaml"

// loadConfig 加载 server.yaml（flag > $LINGEDGE_CONFIG > 缺省路径）；runtime 为 true 时要求运行期必需项。
func loadConfig(path string, environ []string, runtime bool) (*config.Config, error) {
	if path == "" {
		for _, kv := range environ {
			if len(kv) > len(EnvConfig)+1 && kv[:len(EnvConfig)+1] == EnvConfig+"=" {
				path = kv[len(EnvConfig)+1:]
			}
		}
	}
	if path == "" {
		path = DefaultConfigPath
	}
	// LINGEDGE_MIGRATE_*（迁移账号/管理员凭据来源，ADR-026 §4）不是配置覆盖项。
	ignore := []string{EnvConfig, migrate.EnvUser, migrate.EnvDBPass, migrate.EnvDBPassFile, migrate.EnvMigrateYAML, migrate.EnvAdminUser, migrate.EnvAdminPass, migrate.EnvAdminPassFile}
	return config.Load(path, config.Options{Environ: environ, EnvIgnore: ignore, RequireRuntime: runtime})
}

// serveMain 解析运行参数并运行 Server，直到收到 SIGTERM/SIGINT 并优雅停机（退出码 0）。
func serveMain(args []string, stderr io.Writer, environ []string) int {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "server.yaml 路径（缺省 $"+EnvConfig+"，再缺省 "+DefaultConfigPath+"）")
	roles := fl.String("roles", "", "运行角色，逗号分隔：api,grpc,scheduler,worker（缺省全开）")
	timeout := fl.Duration("shutdown-timeout", server.DefaultShutdownTimeout, "优雅停机排空在途请求的最长等待")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "%s: 多余的参数 %q（子命令须放在最前面，如 `%s migrate status`）\n", name, fl.Arg(0), name)
		return 2
	}
	rs, err := server.ParseRoles(*roles)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 2
	}
	if *timeout <= 0 || *timeout > 10*time.Minute {
		_, _ = fmt.Fprintf(stderr, "%s: --shutdown-timeout 必须在 (0, 10m]\n", name)
		return 2
	}
	// 运行期必填项（数据库/Redis 连接目标与口令）在 setup mode 下尚不存在，且正常模式下可由运行期凭据文件合并提供（ADR-026）：
	// 加载时只做取值校验，必填性由 server.Run 在合并之后判定（ValidateRuntime）。
	cfg, err := loadConfig(*cfgPath, environ, false)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: 拒绝启动：配置无效（口令不得写在 server.yaml：显式通道为环境变量 LINGEDGE_DATABASE__PASSWORD / database.password_file，缺省由安装向导写入运行期凭据文件；参考 configs/server.example.yaml）\n%v\n", name, err)
		return 1
	}
	logger, err := logx.New(logx.Options{Level: cfg.Log.Level, Format: cfg.Log.Format, Output: stderr})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	defer func() { _ = logger.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() { // 第一个信号触发优雅停机；其后恢复默认行为，再来一个信号即强制终止。
		<-ctx.Done()
		stop()
	}()

	// setup mode 判定（Story 1.24 AC1）：无 install.lock 且无完成的 system_installations → 进入 setup mode；向导完成后在同一进程内切换为正常模式。
	box, closeBox := openBoxQuiet(cfg)
	installed := setup.DetectInstalled(ctx, cfg, box, logger, clock.System())
	closeBox()
	if !installed {
		done, err := server.RunSetup(ctx, server.SetupOptions{Config: cfg, Logger: logger, NewAdmin: assembly.NewSetupAdmin, ShutdownTimeout: *timeout})
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
			return 1
		}
		if !done { // 收到停机信号，安装未完成
			return 0
		}
		// 向导完成：沿用同一个信号 ctx（尚未被取消）继续以正常模式装配，不重新注册信号处理。
	}

	tasks, err := assembleTaskRegistry()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: 拒绝启动：任务类型注册表无效: %v\n", name, err)
		return 1
	}
	asm := assembly.New()
	err = server.Run(ctx, server.Options{
		Config: cfg, Roles: rs, Logger: logger,
		Register: asm.Register, BuildHooks: asm.BuildHooks, Tasks: tasks, Cron: registerCron, OpenMaint: openMaint, ShutdownTimeout: *timeout,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	return 0
}
