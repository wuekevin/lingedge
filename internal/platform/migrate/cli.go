package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
)

// 迁移账号凭据的来源（AA-7 尚未定稿，见 docs/bmad/decisions-adopted.md §3；此处是过渡约定，定稿后只改本文件）：
// 口令绝不经命令行参数（会出现在 ps 里）；只经环境变量或口令文件。
const (
	EnvUser       = "LINGEDGE_MIGRATE_DB_USER"
	EnvDBPass     = "LINGEDGE_MIGRATE_DB_PASSWORD"
	EnvDBPassFile = "LINGEDGE_MIGRATE_DB_PASSWORD_FILE"
	// DefaultUser 是迁移账号的缺省名（database/README §5.1）。
	DefaultUser = "lingedge_migrate"
)

const usage = `用法: lingedge-server migrate <子命令> [选项]

子命令:
  plan      输出待执行迁移计划（JSON：序号、名称、分类、是否含 Down、has_irreversible）；不连库时列出全部内嵌迁移
  up        应用待执行迁移（逐个标记 dirty/校验和；--accept-irreversible 才允许不可逆迁移）
  down      回退迁移（仅 reversible；缺省 1 步，--steps N | --to N）
  status    显示版本、待执行迁移、dirty 与校验和状态（--json）
  recover   清除 dirty 标记（--version N；确认可重跑之后）
  lint      静态检查内嵌迁移（分类标注、命名、DDL 预检、逐表授权；--dir 检查指定目录）

连接选项（plan 不给任何连接选项时为离线模式）:
  --config FILE       server.yaml（取 database.host/port/name）
  --host H --port P   或 --socket PATH；--database NAME；--user NAME（缺省 ` + DefaultUser + `）；--tls true|false|skip-verify|preferred
  口令:               环境变量 ` + EnvDBPass + ` 或 ` + EnvDBPassFile + `（口令文件，绝对路径，权限 0400/0600）；用户名可用 ` + EnvUser + `
  --lock-timeout D    等待迁移互斥的时长（缺省 ` + "30s" + `）

退出码: 0 成功；1 失败；2 用法错误；3 lint 未通过；4 dirty；5 校验和不一致；6 数据库版本不受支持；7 迁移互斥超时；8 迁移账号预检失败；9 含不可逆迁移但未确认；10 数据库与二进制的迁移集合不一致（二进制比数据库旧/乱序/记录矛盾）
`

type connFlags struct {
	config, host, socket, database, user, tls string
	port                                      int
	lockTimeout                               time.Duration
}

func (c *connFlags) register(f *flag.FlagSet) {
	f.StringVar(&c.config, "config", "", "server.yaml 路径")
	f.StringVar(&c.host, "host", "", "数据库主机")
	f.IntVar(&c.port, "port", 0, "数据库端口")
	f.StringVar(&c.socket, "socket", "", "Unix socket 路径")
	f.StringVar(&c.database, "database", "", "库名")
	f.StringVar(&c.user, "user", "", "迁移账号（缺省 "+DefaultUser+"）")
	f.StringVar(&c.tls, "tls", "", "TLS 模式")
	f.DurationVar(&c.lockTimeout, "lock-timeout", DefaultLockTimeout, "等待迁移互斥的时长")
}

// validate 校验连接选项本身（不连库）。
func (c connFlags) validate() error {
	if c.lockTimeout < time.Second {
		return fmt.Errorf("--lock-timeout 不得小于 1s（GET_LOCK 以整秒计）：%s", c.lockTimeout)
	}
	if c.port < 0 || c.port > 65535 {
		return fmt.Errorf("--port 超出范围：%d", c.port)
	}
	return nil
}

func envMap(environ []string) map[string]string {
	m := map[string]string{}
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

func (c connFlags) wantsDB(env map[string]string) bool {
	return c.config != "" || c.host != "" || c.socket != "" || env["LINGEDGE_DATABASE__HOST"] != ""
}

// dbConfig 汇集连接配置：flag > 环境变量 > server.yaml。
func (c connFlags) dbConfig(environ []string, env map[string]string) (db.Config, error) {
	var cfg db.Config
	if c.config != "" {
		loaded, err := config.Load(c.config, config.Options{
			Environ:   environ,
			EnvIgnore: []string{EnvUser, EnvDBPass, EnvDBPassFile},
		})
		if err != nil {
			return cfg, err
		}
		cfg.Host, cfg.Port, cfg.Name = loaded.Database.Host, loaded.Database.Port, loaded.Database.Name
	}
	// 环境变量 LINGEDGE_DATABASE__*（与 server.yaml 覆盖语义一致）：--config 已由 config.Load 应用；无 --config 时在这里读取。
	if c.config == "" {
		if v := env["LINGEDGE_DATABASE__HOST"]; v != "" {
			cfg.Host = v
		}
		if v := env["LINGEDGE_DATABASE__PORT"]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return cfg, fmt.Errorf("LINGEDGE_DATABASE__PORT 不是合法端口")
			}
			cfg.Port = n
		}
		if v := env["LINGEDGE_DATABASE__NAME"]; v != "" {
			cfg.Name = v
		}
	}
	if c.host != "" {
		cfg.Host = c.host
	}
	if c.port != 0 {
		cfg.Port = c.port
	}
	if cfg.Socket == "" && c.socket == "" && cfg.Host != "" && cfg.Port == 0 {
		cfg.Port = 3306
	}
	if c.socket != "" {
		cfg.Socket, cfg.Host, cfg.Port = c.socket, "", 0
	}
	if c.database != "" {
		cfg.Name = c.database
	}
	if c.tls != "" {
		cfg.TLS = c.tls
	}
	switch {
	case c.user != "":
		cfg.User = c.user
	case env[EnvUser] != "":
		cfg.User = env[EnvUser]
	default:
		cfg.User = DefaultUser
	}
	switch {
	case env[EnvDBPass] != "" && env[EnvDBPassFile] != "":
		return cfg, fmt.Errorf("%s 与 %s 只能设置其一", EnvDBPass, EnvDBPassFile)
	case env[EnvDBPass] != "":
		cfg.Password = config.NewSecret(env[EnvDBPass])
	case env[EnvDBPassFile] != "":
		s, err := config.ReadSecretFile(env[EnvDBPassFile])
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", EnvDBPassFile, err)
		}
		cfg.Password = s
	default:
		return cfg, fmt.Errorf("缺少迁移账号口令：请设置 %s 或 %s（不提供默认口令，NFR-6）", EnvDBPass, EnvDBPassFile)
	}
	// 迁移进程需要同时持有：互斥连接 + goose 执行连接 + 状态读写；池缺省的 1 个连接会死锁。
	cfg.MaxOpenConns, cfg.MaxIdleConns = 4, 2
	return cfg, nil
}

func (c connFlags) open(ctx context.Context, environ []string, env map[string]string, fsys fs.FS, stderr io.Writer) (*Runner, func(), error) {
	cfg, err := c.dbConfig(environ, env)
	if err != nil {
		return nil, nil, err
	}
	pool, err := db.OpenMigrate(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	r, err := NewRunner(pool, fsys)
	if err != nil {
		_ = pool.Close()
		return nil, nil, err
	}
	r.LockTimeout = c.lockTimeout
	r.Logf = func(format string, a ...any) { _, _ = fmt.Fprintf(stderr, "migrate: "+format+"\n", a...) }
	return r, func() { _ = pool.Close() }, nil
}

// Main 是 `lingedge-server migrate` 的入口，返回进程退出码。fsys 为内嵌迁移（migrations.FS）。
func Main(args []string, stdout, stderr io.Writer, environ []string, fsys fs.FS) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = fmt.Fprint(stderr, usage)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	sub, rest := args[0], args[1:]
	env := envMap(environ)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "lingedge-server migrate %s: %v\n", sub, err)
		return ExitCode(err)
	}
	usageErr := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "lingedge-server migrate %s: %v\n用 `lingedge-server migrate help` 查看用法\n", sub, err)
		return ExitUsage
	}
	newFlags := func() *flag.FlagSet {
		f := flag.NewFlagSet("migrate "+sub, flag.ContinueOnError)
		f.SetOutput(stderr)
		return f
	}
	// parse 解析选项并拒绝多余的位置参数：Go 的 flag 遇到第一个非选项参数就停止解析，
	// 不拒绝的话 `up 5 --to 3` 会静默忽略 --to 而升级全部。
	parse := func(f *flag.FlagSet) bool {
		if err := f.Parse(rest); err != nil {
			return false
		}
		if f.NArg() > 0 {
			_, _ = fmt.Fprintf(stderr, "lingedge-server migrate %s: 多余的参数 %q（选项须写在子命令之后、且不接位置参数）\n", sub, f.Args())
			return false
		}
		return true
	}

	switch sub {
	case "lint":
		f := newFlags()
		dir := f.String("dir", "", "检查指定目录而不是内嵌迁移")
		if !parse(f) {
			return ExitUsage
		}
		target := fsys
		if *dir != "" {
			target = os.DirFS(*dir)
		}
		set, findings, err := Load(target)
		if err != nil {
			return fail(err)
		}
		if len(findings) > 0 {
			for _, fd := range findings {
				_, _ = fmt.Fprintln(stderr, fd)
			}
			_, _ = fmt.Fprintf(stderr, "lingedge-server migrate lint: %d 项发现\n", len(findings))
			return ExitLint
		}
		_, _ = fmt.Fprintf(stdout, "迁移 lint 通过：%d 个迁移\n", len(set.Migrations))
		return ExitOK

	case "plan":
		f := newFlags()
		var c connFlags
		c.register(f)
		offline := f.Bool("offline", false, "不连库，列出全部内嵌迁移")
		if !parse(f) {
			return ExitUsage
		}
		if err := c.validate(); err != nil {
			return usageErr(err)
		}
		if *offline || !c.wantsDB(env) {
			set, findings, err := Load(fsys)
			if err != nil {
				return fail(err)
			}
			if len(findings) > 0 {
				return fail(&ErrLint{Findings: findings})
			}
			return writePlan(BuildPlan(set, nil), stdout, fail)
		}
		r, closeFn, err := c.open(ctx, environ, env, fsys, stderr)
		if err != nil {
			return fail(err)
		}
		defer closeFn()
		if err := r.failIfLint(); err != nil {
			return fail(err)
		}
		p, err := r.Plan(ctx)
		if err != nil {
			return fail(err)
		}
		return writePlan(p, stdout, fail)

	case "status":
		f := newFlags()
		var c connFlags
		c.register(f)
		asJSON := f.Bool("json", false, "JSON 输出")
		if !parse(f) {
			return ExitUsage
		}
		if err := c.validate(); err != nil {
			return usageErr(err)
		}
		if !c.wantsDB(env) {
			return usageErr(errors.New("status 需要连接选项（--config 或 --host/--socket）"))
		}
		r, closeFn, err := c.open(ctx, environ, env, fsys, stderr)
		if err != nil {
			return fail(err)
		}
		defer closeFn()
		st, err := r.Status(ctx)
		if err != nil {
			return fail(err)
		}
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(st)
		} else {
			writeStatusText(stdout, st)
		}
		if len(r.Findings()) > 0 {
			_, _ = fmt.Fprintln(stderr, "警告：内嵌迁移未通过 lint（用 `migrate lint` 查看）；up/down 将拒绝执行")
		}
		if st.Err() != nil {
			_, _ = fmt.Fprintf(stderr, "lingedge-server migrate status: %v\n", st.Err())
			return ExitCode(st.Err())
		}
		return ExitOK

	case "up":
		f := newFlags()
		var c connFlags
		c.register(f)
		to := f.Int64("to", 0, "升级到该序号（含）")
		accept := f.Bool("accept-irreversible", false, "确认已备份，允许执行不可逆（irreversible）迁移")
		asJSON := f.Bool("json", false, "JSON 输出结果")
		if !parse(f) {
			return ExitUsage
		}
		if err := c.validate(); err != nil {
			return usageErr(err)
		}
		if *to < 0 {
			return usageErr(errors.New("--to 不得为负数"))
		}
		if !c.wantsDB(env) {
			return usageErr(errors.New("up 需要连接选项（--config 或 --host/--socket）"))
		}
		r, closeFn, err := c.open(ctx, environ, env, fsys, stderr)
		if err != nil {
			return fail(err)
		}
		defer closeFn()
		res, err := r.Up(ctx, UpOptions{To: *to, AcceptIrreversible: *accept})
		if err != nil {
			// 失败时仍给出已经应用成功的迁移（调用方据此知道库停在了哪里）。
			if res != nil && len(res.Applied) > 0 {
				writeResult(stdout, res, *asJSON, "升级")
			}
			return fail(err)
		}
		writeResult(stdout, res, *asJSON, "升级")
		return ExitOK

	case "down":
		f := newFlags()
		var c connFlags
		c.register(f)
		steps := f.Int("steps", 0, "回退个数（缺省 1）")
		to := f.Int64("to", 0, "回退到该序号（该序号保留）")
		all := f.Bool("all", false, "回退全部（与显式的 --to 0 等价）")
		asJSON := f.Bool("json", false, "JSON 输出结果")
		if !parse(f) {
			return ExitUsage
		}
		if err := c.validate(); err != nil {
			return usageErr(err)
		}
		toSet := false
		f.Visit(func(x *flag.Flag) {
			if x.Name == "to" {
				toSet = true
			}
		})
		stepsSet := *steps != 0
		if *steps < 0 || *to < 0 {
			return usageErr(errors.New("--steps/--to 不得为负数"))
		}
		modes := 0
		for _, b := range []bool{stepsSet, toSet, *all} {
			if b {
				modes++
			}
		}
		if modes > 1 {
			return usageErr(errors.New("--steps、--to、--all 只能指定其一"))
		}
		if toSet && *to == 0 { // 显式的 --to 0 即「回退全部」，不能静默退化成回退一步
			*all = true
		}
		if !c.wantsDB(env) {
			return usageErr(errors.New("down 需要连接选项（--config 或 --host/--socket）"))
		}
		r, closeFn, err := c.open(ctx, environ, env, fsys, stderr)
		if err != nil {
			return fail(err)
		}
		defer closeFn()
		res, err := r.Down(ctx, DownOptions{Steps: *steps, To: *to, All: *all})
		if err != nil {
			if res != nil && len(res.Applied) > 0 {
				writeResult(stdout, res, *asJSON, "回退")
			}
			return fail(err)
		}
		writeResult(stdout, res, *asJSON, "回退")
		return ExitOK

	case "recover":
		f := newFlags()
		var c connFlags
		c.register(f)
		ver := f.Int64("version", 0, "dirty 的迁移序号")
		if !parse(f) {
			return ExitUsage
		}
		if err := c.validate(); err != nil {
			return usageErr(err)
		}
		if *ver <= 0 {
			return usageErr(errors.New("recover 必须指定 --version N（dirty 的迁移序号，见 migrate status）"))
		}
		if !c.wantsDB(env) {
			return usageErr(errors.New("recover 需要连接选项（--config 或 --host/--socket）"))
		}
		r, closeFn, err := c.open(ctx, environ, env, fsys, stderr)
		if err != nil {
			return fail(err)
		}
		defer closeFn()
		if err := r.Recover(ctx, *ver); err != nil {
			return fail(err)
		}
		return ExitOK
	}
	return usageErr(fmt.Errorf("未知子命令 %q", sub))
}

func writePlan(p *Plan, w io.Writer, fail func(error) int) int {
	if err := p.WriteJSON(w); err != nil {
		return fail(err)
	}
	return ExitOK
}

func writeResult(w io.Writer, res *UpResult, asJSON bool, verb string) {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return
	}
	if len(res.Applied) == 0 {
		_, _ = fmt.Fprintf(w, "无需%s，当前版本 %06d\n", verb, res.ToVersion)
		return
	}
	for _, a := range res.Applied {
		_, _ = fmt.Fprintf(w, "%s %06d_%s（%s，%d ms）\n", verb, a.Version, a.Name, a.Class, a.Millis)
	}
	_, _ = fmt.Fprintf(w, "%s完成：%06d → %06d\n", verb, res.FromVersion, res.ToVersion)
}

func writeStatusText(w io.Writer, st *Status) {
	_, _ = fmt.Fprintf(w, "MySQL %s | 数据库版本 %06d | 已知最新 %06d | 待执行 %d\n", st.MySQLVersion, st.DBVersion, st.LatestKnown, st.Pending)
	if st.InProgress {
		_, _ = fmt.Fprintln(w, "另一个 migrate 正在运行（迁移互斥被持有）")
	}
	if st.Dirty != nil {
		_, _ = fmt.Fprintf(w, "DIRTY: %06d_%s（%s，%s）%s\n", st.Dirty.Version, st.Dirty.Name, st.Dirty.Direction, st.Dirty.Status, st.Dirty.Error)
	}
	for _, it := range st.Migrations {
		at := "-"
		if it.AppliedAt != nil {
			at = it.AppliedAt.UTC().Format(time.RFC3339)
		}
		_, _ = fmt.Fprintf(w, "  %06d  %-10s %-12s %-14s 校验和:%-9s %s\n", it.Version, it.State, it.Class, it.Name, it.ChecksumState, at)
	}
}
