package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/dbcred"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/migrate/provision"
)

const dbUsage = `用法: lingedge-server db credential rotate --role app|maint --admin-user U [--finish] [--config FILE] [--host H --port P] [--tls MODE]

轮换 lingedge_app / lingedge_maint 的运行期口令（ADR-026 §8，root 侧命令：需读 master.key 并重写 runtime-credentials.json）：
  阶段一（缺省）  生成新口令 → ALTER USER … IDENTIFIED BY <新> RETAIN CURRENT PASSWORD（MySQL ≥8.0.14 双口令）→ 原子重写运行期凭据文件。旧口令仍然有效。
                  随后请重启 Server（M1 用 systemctl restart）并确认健康检查通过。
  阶段二 --finish 确认新口令已生效后执行 ALTER USER … DISCARD OLD PASSWORD。任一步失败都不会丢弃旧口令；两个阶段都可重入。

管理员口令只经交互输入（TTY，无回显）或环境变量 ` + migrate.EnvAdminPass + ` / ` + migrate.EnvAdminPassFile + `；连接目标只取命令行与 root 属主的 --config server.yaml，
绝不取自 runtime-credentials.json（lingedge 可写，ADR-026 §4 信任规则）。
`

func dbMain(args []string, stdout, stderr io.Writer, environ []string) int {
	if len(args) < 2 || args[0] != "credential" || args[1] != "rotate" {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			_, _ = fmt.Fprint(stdout, dbUsage)
			return 0
		}
		_, _ = fmt.Fprint(stderr, dbUsage)
		return 2
	}
	fl := flag.NewFlagSet("db credential rotate", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "server.yaml 路径（root 运行时须属主 root）")
	host := fl.String("host", "", "数据库主机")
	port := fl.Int("port", 0, "数据库端口")
	socket := fl.String("socket", "", "MySQL Unix 套接字路径（与 --host/--port 互斥）")
	tls := fl.String("tls", "", "TLS 模式")
	role := fl.String("role", "", "app 或 maint")
	admin := fl.String("admin-user", "", "数据库管理员用户名")
	finish := fl.Bool("finish", false, "阶段二：丢弃旧口令")
	if err := fl.Parse(args[2:]); err != nil || fl.NArg() > 0 {
		return 2
	}
	if (*role != "app" && *role != "maint") || *admin == "" {
		_, _ = fmt.Fprint(stderr, dbUsage)
		return 2
	}
	env := migrate.EnvMap(environ)
	cfg, err := loadConfig(*cfgPath, environ, false)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db credential rotate: 配置无效：%v\n", err)
		return 1
	}
	if *cfgPath != "" {
		if err := migrate.CheckTrustedConfig(*cfgPath); err != nil {
			_, _ = fmt.Fprintf(stderr, "db credential rotate: %v\n", err)
			return 1
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	path := dbcred.Path(cfg)
	f, err := dbcred.Load(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db credential rotate: %v\n", err)
		return 1
	}
	appUser, maintUser := f.Users()
	user := appUser
	if *role == "maint" {
		user = maintUser
	}
	if user == "" {
		_, _ = fmt.Fprintf(stderr, "db credential rotate: 运行期凭据文件里没有 %s 账号\n", *role)
		return 1
	}
	// 连接目标：命令行 ＞ root 属主 server.yaml；绝不取自运行期凭据文件。
	tgt := provision.Target{Host: cfg.Database.Host, Port: cfg.Database.Port, TLS: cfg.Database.TLS}
	if cfg.Database.Socket != "" {
		tgt = provision.Target{Socket: cfg.Database.Socket}
	}
	if *socket != "" {
		tgt = provision.Target{Socket: *socket}
	}
	if *host != "" {
		tgt.Host, tgt.Socket = *host, ""
		if tgt.Port == 0 {
			tgt.Port = 3306
		}
	}
	if *port != 0 {
		tgt.Port = *port
	}
	if *tls != "" {
		tgt.TLS = *tls
	}
	if tgt.Host == "" && tgt.Socket == "" {
		_, _ = fmt.Fprintln(stderr, "db credential rotate: 缺少连接目标：需要 --host 或 --socket（或 --config 指向含 database.host/socket 的 root 属主 server.yaml）")
		return 1
	}
	apw, err := migrate.AdminSecret(env, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db credential rotate: %v\n", err)
		return migrate.ExitPrecondition
	}
	conn, err := provision.Open(ctx, tgt, *admin, apw, "")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db credential rotate: %v\n", err)
		return 1
	}
	defer conn.Close()
	if *finish {
		if err := conn.DiscardOldPassword(ctx, user); err != nil {
			_, _ = fmt.Fprintf(stderr, "db credential rotate: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "已丢弃 %s 的旧口令。\n", user)
		return 0
	}
	box, closeBox := openBoxQuiet(cfg)
	defer closeBox()
	if box == nil {
		_, _ = fmt.Fprintln(stderr, "db credential rotate: 主密钥不可用（需读取 master.key 才能加密写入新口令）")
		return 1
	}
	gen, err := migrate.GeneratePassword()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "db credential rotate: %v\n", err)
		return 1
	}
	newPW := config.NewSecret(gen)
	// 先验证能写文件（往返自检）再改数据库口令：任一步失败都不留下「口令已改、文件没写」的状态。
	if err := conn.ResetPassword(ctx, user, newPW, true); err != nil {
		_, _ = fmt.Fprintf(stderr, "db credential rotate: %v\n", err)
		return 1
	}
	if err := dbcred.ReplacePassword(ctx, path, box, dbcred.Role(*role), newPW); err != nil {
		_, _ = fmt.Fprintf(stderr, "db credential rotate: 数据库口令已改（旧口令仍有效，RETAIN CURRENT PASSWORD），但重写运行期凭据文件失败，请修复后重试本命令: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "%s 的新口令已写入 %s；旧口令仍有效。请重启 Server 并确认健康检查通过，然后执行 `lingedge-server db credential rotate --role %s --admin-user %s --finish` 丢弃旧口令。\n", user, path, *role, *admin)
	return 0
}
