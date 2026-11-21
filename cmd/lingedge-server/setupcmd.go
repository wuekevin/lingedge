package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"

	"github.com/wuekevin/lingedge/cmd/lingedge-server/assembly"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/setup"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/migrations"
)

const setupUsage = `用法: lingedge-server setup --from-file <YAML> [--config FILE] [--json]

无头安装（供自动化，Story 1.24、ADR-026 §7）：按 YAML 完成与 Web 向导相同的全部步骤（MySQL → Redis → 数据库初始化 → 管理员 → 系统配置 → 密钥 → 完成），
每步幂等；Web 向导在 Server 无 install.lock 启动时自动进入 setup mode，不需要本命令。

  --from-file FILE  调用者属主、权限恰为 0600 的 YAML（含临时管理员/migrate/app/maint/Redis 凭据与管理员初始口令，口令字段可写成 *_file）。
                    **首次成功解析后进程清零并用 0 覆盖后删除该文件**（删除失败醒目警告，结果带 from_file_residual=true）；续做须重新提供文件。
  --config FILE     server.yaml（缺省 $` + EnvConfig + `，再缺省 ` + DefaultConfigPath + `）。请以 lingedge 用户运行（运行期凭据文件属主为运行用户）。
  --json            以 JSON 输出结果；自动生成的迁移口令只在结果里输出一次、不落盘。

退出码: 0 成功；1 失败（含已安装）；2 用法错误；3 from-file 权限/属主不符
`

func setupMain(args []string, stdout, stderr io.Writer, environ []string) int {
	fl := flag.NewFlagSet("setup", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "server.yaml 路径")
	file := fl.String("from-file", "", "无头安装的 YAML（0600，调用者属主）")
	asJSON := fl.Bool("json", false, "以 JSON 输出")
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		_, _ = fmt.Fprint(stdout, setupUsage)
		return 0
	}
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() > 0 || *file == "" {
		_, _ = fmt.Fprint(stderr, setupUsage)
		return 2
	}
	cfg, err := loadConfig(*cfgPath, environ, false)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "setup: 配置无效：%v\n", err)
		return 1
	}
	logger, err := logx.New(logx.Options{Level: cfg.Log.Level, Format: cfg.Log.Format, Output: stderr})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "setup: %v\n", err)
		return 1
	}
	defer func() { _ = logger.Sync() }()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	box, closeBox := openBoxQuiet(cfg)
	defer closeBox()
	if setup.DetectInstalled(ctx, cfg, box, logger, clock.System()) {
		_, _ = fmt.Fprintln(stderr, "setup: 系统已安装（install.lock 存在或数据库记录安装已完成）；安装向导不可用（410 setup.already_installed）")
		return 1
	}
	ff, residual, err := setup.LoadFromFile(*file)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "setup: %v\n", err)
		if errors.Is(err, setup.ErrFromFilePermission) {
			return 3
		}
		return 1
	}
	svc, err := setup.New(setup.Options{Config: cfg, Box: box, Logger: logger, Clock: clock.System(), IDs: uid.NewGenerator(clock.System()), Migrations: migrations.FS, NewAdmin: assembly.NewSetupAdmin,
		TokenHashPath: cfg.Secrets.SetupTokenHashFile})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "setup: %v\n", err)
		return 1
	}
	defer svc.Close()
	res := svc.RunHeadless(ctx, ff, residual)
	// from-file 的内存副本用后清空（字符串不可就地覆写，这里去掉引用；缓冲已在解析时清零）。
	*ff = setup.FromFile{}
	if *asJSON {
		_, _ = stdout.Write(append(res.JSON(), '\n'))
	} else {
		for _, st := range res.Steps {
			_, _ = fmt.Fprintf(stdout, "[%s] %s\n", st.Step, st.Message)
		}
		for _, w := range res.Warnings {
			_, _ = fmt.Fprintln(stderr, w)
		}
		if res.OK {
			_, _ = fmt.Fprintln(stdout, "安装完成。")
			if res.OneTimeSecrets != nil && res.OneTimeSecrets.MigratePassword != "" {
				_, _ = fmt.Fprintf(stdout, "迁移账号口令（自动生成，仅显示这一次，不会被保存）: %s\n请立即执行 `sudo lingedge-server migrate credential init`（交互输入该口令）保存到 root 侧凭据文件；来不及保存时用 `migrate credential reset` 重置。\n", res.OneTimeSecrets.MigratePassword)
			} else {
				_, _ = fmt.Fprintln(stdout, "升级前请执行 `sudo lingedge-server migrate credential init` 保存迁移账号口令（或之后用 credential reset）。")
			}
			_, _ = fmt.Fprintln(stdout, "临时数据库管理员账号（若使用了建账号模式）请自行删除；from-file 文件已被清零并删除。")
		} else {
			_, _ = fmt.Fprintf(stderr, "setup: 步骤 %s 失败: %s\n（各步骤幂等；from-file 文件已被清理，续做请重新提供）\n", res.Failed, res.Error)
		}
	}
	if !res.OK {
		return 1
	}
	return 0
}
