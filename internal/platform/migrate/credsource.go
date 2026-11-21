package migrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
	"golang.org/x/term"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/migrate/provision"
)

// lingedge_migrate 凭据的来源与 root 侧文件 migrate.yaml（ADR-026 §4、tools-design §13.1）。
//
// 来源优先级（口令）：环境变量 LINGEDGE_MIGRATE_DB_PASSWORD ＞ LINGEDGE_MIGRATE_DB_PASSWORD_FILE ＞ migrate.yaml ＞ 交互输入（仅 TTY，无回显；
// 非 TTY 退出码 3「前置条件」并说明如何提供）。
//
// 连接目标的信任规则（防止 lingedge 被攻陷后借 root 套取口令）：目标只来自 ① 命令行/环境变量显式值 ② migrate.yaml ③ root 属主的 server.yaml；
// **绝不**读取 runtime-credentials.json（lingedge 可写：被攻陷的 lingedge 可把 host 指向攻击者的 MySQL，让 root 侧把 migrate 口令发过去）。
// 本包不 import internal/platform/dbcred，由 TestMigrateNeverReadsRuntimeCredentials 与依赖扫描保证。
const (
	// DefaultMigrateYAML 是 root 侧凭据文件的缺省路径（root:root 0400）。
	DefaultMigrateYAML = "/etc/lingedge/server/migrate.yaml"
	// EnvMigrateYAML 覆盖缺省路径（测试与非标准部署）。
	EnvMigrateYAML = "LINGEDGE_MIGRATE_CONFIG"
	// EnvAdminUser/EnvAdminPass/EnvAdminPassFile：credential reset 的管理员（非交互供给，同样不经命令行）。
	EnvAdminUser     = "LINGEDGE_MIGRATE_ADMIN_USER"
	EnvAdminPass     = "LINGEDGE_MIGRATE_ADMIN_PASSWORD"
	EnvAdminPassFile = "LINGEDGE_MIGRATE_ADMIN_PASSWORD_FILE"
)

// ExitPrecondition 是「前置条件不满足」（如非交互且没有提供迁移口令）的退出码，与 Tools 退出码表的 3 一致。
const ExitPrecondition = 3

// ErrNoCredentials：没有任何口令来源可用（非 TTY 时）。
var ErrNoCredentials = errors.New("缺少迁移账号口令，且当前不是交互终端。请任选其一：" +
	"① 环境变量 " + EnvDBPass + " 或 " + EnvDBPassFile + "；② 先执行 `sudo lingedge-server migrate credential init`（写入 " + DefaultMigrateYAML + "，root:root 0400）；③ 在终端中交互运行")

// FileCreds 是 migrate.yaml 的内容（KnownFields）。
type FileCreds struct {
	Host     string        `yaml:"host"`
	Port     int           `yaml:"port"`
	Socket   string        `yaml:"socket"`
	Database string        `yaml:"database"`
	User     string        `yaml:"user"`
	Password config.Secret `yaml:"password"`
	TLS      string        `yaml:"tls"`
}

// promptFunc 读取一个无回显的口令；isTTY 报告是否可交互。均可被测试替换。
var (
	isTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	// readSecretPrompt 从终端读取无回显输入（提示写到 stderr）。
	readSecretPrompt = func(label string, stderr io.Writer) (string, error) {
		_, _ = fmt.Fprint(stderr, label)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		_, _ = fmt.Fprintln(stderr)
		if err != nil {
			return "", err
		}
		defer func() {
			for i := range b {
				b[i] = 0
			}
		}()
		return string(b), nil
	}
)

// migrateYAMLPath 返回 migrate.yaml 的路径，explicit 表示由环境变量显式指定（缺失即错误）。
func migrateYAMLPath(env map[string]string) (path string, explicit bool) {
	if p := env[EnvMigrateYAML]; p != "" {
		return p, true
	}
	return DefaultMigrateYAML, false
}

// loadMigrateYAML 读取并校验 migrate.yaml：普通文件、非符号链接、属主 = 运行用户或 root、权限无 group/other 位。
// 文件不存在时 (nil, nil)（explicit 为真则报错）。
func loadMigrateYAML(path string, explicit bool) (*FileCreds, error) {
	f, err := openNoFollow(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return nil, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("migrate.yaml 不存在")
		}
		return nil, fmt.Errorf("无法打开 migrate.yaml（符号链接或无权限均被拒绝）")
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("migrate.yaml 必须是普通文件")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("migrate.yaml 权限过宽（当前 %04o；应为 root:root 0400）", st.Mode().Perm())
	}
	if err := checkOwnerRootOrSelf(st); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, errors.New("读取 migrate.yaml 失败")
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var fc FileCreds
	if err := dec.Decode(&fc); err != nil {
		return nil, errors.New("migrate.yaml 解析失败或含未知字段")
	}
	if !fc.Password.IsSet() {
		return nil, errors.New("migrate.yaml 缺少 password")
	}
	return &fc, nil
}

// resolveCreds 按优先级取得迁移账号口令；返回的 config.Secret 由调用方使用后丢弃。
func resolveCreds(env map[string]string, fc *FileCreds, stderr io.Writer) (config.Secret, error) {
	switch {
	case env[EnvDBPass] != "" && env[EnvDBPassFile] != "":
		return config.Secret{}, fmt.Errorf("%s 与 %s 只能设置其一", EnvDBPass, EnvDBPassFile)
	case env[EnvDBPass] != "":
		return config.NewSecret(env[EnvDBPass]), nil
	case env[EnvDBPassFile] != "":
		s, err := config.ReadSecretFile(env[EnvDBPassFile])
		if err != nil {
			return config.Secret{}, fmt.Errorf("%s: %w", EnvDBPassFile, err)
		}
		return s, nil
	case fc != nil && fc.Password.IsSet():
		return fc.Password, nil
	case isTTY():
		pw, err := readSecretPrompt("迁移账号口令（无回显）: ", stderr)
		if err != nil || pw == "" {
			return config.Secret{}, errors.New("未读取到口令")
		}
		return config.NewSecret(pw), nil
	}
	return config.Secret{}, ErrNoCredentials
}

// ---- credential init|reset ----

const credUsage = `用法: lingedge-server migrate credential <init|reset> [选项]

  init    把 lingedge_migrate 的连接目标与口令写入 root 侧凭据文件（缺省 ` + DefaultMigrateYAML + `，root:root 0400，O_EXCL，已存在则拒绝，--force 覆盖）。
          先真实连接并预检（GRANT OPTION、触发器权限），失败不写文件。口令来自交互输入（TTY，无回显）或 ` + EnvDBPass + ` / ` + EnvDBPassFile + `。
  reset   用管理员凭据对 lingedge_migrate 执行 ALTER USER 生成新的随机口令并写入凭据文件（口令丢失/续做重置/定期轮换）。
          管理员用户 --admin-user（或 ` + EnvAdminUser + `），口令交互输入或 ` + EnvAdminPass + ` / ` + EnvAdminPassFile + `。

选项: --host H --port P --database NAME [--user NAME] [--tls MODE] [--file PATH] [--force]
      （目标取自命令行/环境变量，其次 root 属主的 server.yaml（--config）；绝不读取 runtime-credentials.json，ADR-026 §4）
`

func credentialMain(args []string, stdout, stderr io.Writer, environ []string, env map[string]string, fsys fsFS) int {
	if len(args) == 0 || (args[0] != "init" && args[0] != "reset") {
		_, _ = fmt.Fprint(stderr, credUsage)
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			return ExitOK
		}
		return ExitUsage
	}
	sub := args[0]
	f := newFlagSet("migrate credential "+sub, stderr)
	var c connFlags
	c.register(f)
	file := f.String("file", "", "凭据文件路径（缺省 "+DefaultMigrateYAML+" 或 $"+EnvMigrateYAML+"）")
	force := f.Bool("force", false, "覆盖已存在的凭据文件")
	adminUser := f.String("admin-user", "", "（reset）数据库管理员用户名")
	if err := f.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	if f.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "lingedge-server migrate credential %s: 多余的参数 %q\n", sub, f.Args())
		return ExitUsage
	}
	if err := c.validate(); err != nil {
		_, _ = fmt.Fprintf(stderr, "lingedge-server migrate credential %s: %v\n", sub, err)
		return ExitUsage
	}
	target := *file
	if target == "" {
		target, _ = migrateYAMLPath(env)
	}
	if _, err := os.Lstat(target); err == nil && !*force {
		_, _ = fmt.Fprintf(stderr, "lingedge-server migrate credential %s: %s 已存在，拒绝覆盖（确认后加 --force）\n", sub, target)
		return ExitFailure
	}
	ctx, stop := notifyContext()
	defer stop()
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "lingedge-server migrate credential %s: %v\n", sub, err)
		if errors.Is(err, ErrNoCredentials) {
			return ExitPrecondition
		}
		return ExitCode(err)
	}
	// 目标：显式 + server.yaml；不读取已有的 migrate.yaml 作为目标来源（reset 时它可能正是要被替换的文件）。
	cfg, err := c.target(environ, env)
	if err != nil {
		return fail(err)
	}
	if cfg.Host == "" || cfg.Name == "" {
		return fail(errors.New("缺少连接目标：需要 --host/--database（或 --config 指向 root 属主的 server.yaml）"))
	}
	if cfg.Port == 0 {
		cfg.Port = 3306
	}
	user := cfg.User
	var pw config.Secret
	switch sub {
	case "init":
		pw, err = resolveCreds(env, nil, stderr)
		if err != nil {
			return fail(err)
		}
		cfg.Password = pw
		if err := verifyMigrateAccount(ctx, cfg, fsys); err != nil {
			return fail(err)
		}
	case "reset":
		au := *adminUser
		if au == "" {
			au = env[EnvAdminUser]
		}
		if au == "" {
			return fail(errors.New("reset 需要 --admin-user（或 " + EnvAdminUser + "）"))
		}
		apw, err := adminSecret(env, stderr)
		if err != nil {
			return fail(err)
		}
		gen, err := GeneratePassword()
		if err != nil {
			return fail(err)
		}
		pw = config.NewSecret(gen)
		conn, err := provision.Open(ctx, provision.Target{Host: cfg.Host, Port: cfg.Port, TLS: cfg.TLS}, au, apw, "")
		if err != nil {
			return fail(err)
		}
		err = conn.ResetPassword(ctx, user, pw, false)
		conn.Close()
		if err != nil {
			return fail(err)
		}
		cfg.Password = pw
		if err := verifyMigrateAccount(ctx, cfg, fsys); err != nil {
			return fail(fmt.Errorf("口令已重置，但新口令的连接/预检失败，凭据文件未写入: %w", err))
		}
	}
	if err := writeMigrateYAML(target, FileCreds{Host: cfg.Host, Port: cfg.Port, Database: cfg.Name, User: user, Password: pw, TLS: cfg.TLS}, *force || sub == "reset"); err != nil {
		return fail(err)
	}
	_, _ = fmt.Fprintf(stdout, "已写入 %s（lingedge_migrate 凭据，0400）。口令未输出。\n", target)
	return ExitOK
}

// AdminSecret 取得数据库管理员口令（credential reset 与 db credential rotate 共用）：环境变量 LINGEDGE_MIGRATE_ADMIN_PASSWORD / _FILE，或 TTY 交互（无回显）。
func AdminSecret(env map[string]string, stderr io.Writer) (config.Secret, error) {
	return adminSecret(env, stderr)
}

// CheckTrustedConfig 校验 --config 指向的 server.yaml 可作为连接目标来源（root 运行时须属主 root 且不被 group/other 写入，ADR-026 §4）。
func CheckTrustedConfig(path string) error { return checkRootOwnedConfig(path) }

// EnvMap 把 KEY=VALUE 环境切片转为映射。
func EnvMap(environ []string) map[string]string { return envMap(environ) }

func adminSecret(env map[string]string, stderr io.Writer) (config.Secret, error) {
	switch {
	case env[EnvAdminPass] != "" && env[EnvAdminPassFile] != "":
		return config.Secret{}, fmt.Errorf("%s 与 %s 只能设置其一", EnvAdminPass, EnvAdminPassFile)
	case env[EnvAdminPass] != "":
		return config.NewSecret(env[EnvAdminPass]), nil
	case env[EnvAdminPassFile] != "":
		return config.ReadSecretFile(env[EnvAdminPassFile])
	case isTTY():
		pw, err := readSecretPrompt("数据库管理员口令（无回显）: ", stderr)
		if err != nil || pw == "" {
			return config.Secret{}, errors.New("未读取到管理员口令")
		}
		return config.NewSecret(pw), nil
	}
	return config.Secret{}, errors.New("缺少管理员口令，且当前不是交互终端：请设置 " + EnvAdminPass + " 或 " + EnvAdminPassFile)
}

// verifyMigrateAccount 真实连接并做账号预检（GRANT OPTION/触发器）；失败返回错误（调用方不写文件）。
func verifyMigrateAccount(ctx context.Context, cfg db.Config, fsys fsFS) error {
	cfg.MaxOpenConns, cfg.MaxIdleConns = 4, 2
	pool, err := db.OpenMigrate(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()
	r, err := NewRunner(pool, fsys)
	if err != nil {
		return err
	}
	if _, err := r.CheckVersion(ctx); err != nil {
		return err
	}
	return r.Preflight(ctx)
}

// GeneratePassword 生成高熵随机口令（24 字节 crypto/rand，base64url，32 字符）。向导与 credential reset 共用。
func GeneratePassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("随机数: %w", err)
	}
	s := base64.RawURLEncoding.EncodeToString(b)
	for i := range b {
		b[i] = 0
	}
	return s, nil
}

// writeMigrateYAML 原子写入 0400 凭据文件：同目录临时文件 + fsync；不覆盖时用 link（目标已存在则失败），覆盖时 rename。
func writeMigrateYAML(path string, fc FileCreds, overwrite bool) error {
	type out struct {
		Host     string `yaml:"host"`
		Port     int    `yaml:"port,omitempty"`
		Database string `yaml:"database"`
		User     string `yaml:"user"`
		Password string `yaml:"password"`
		TLS      string `yaml:"tls,omitempty"`
	}
	b, err := yaml.Marshal(out{Host: fc.Host, Port: fc.Port, Database: fc.Database, User: fc.User, Password: fc.Password.Reveal(), TLS: fc.TLS})
	if err != nil {
		return err
	}
	defer func() {
		for i := range b {
			b[i] = 0
		}
	}()
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".migrate-yaml-*")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	name := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(name) }
	if err := tmp.Chmod(0o400); err != nil {
		cleanup()
		return fmt.Errorf("chmod: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		cleanup()
		return fmt.Errorf("写入: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close: %w", err)
	}
	if overwrite {
		if err := os.Rename(name, path); err != nil {
			_ = os.Remove(name)
			return fmt.Errorf("rename: %w", err)
		}
		return nil
	}
	defer func() { _ = os.Remove(name) }()
	if err := os.Link(name, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s 已存在，拒绝覆盖", path)
		}
		return fmt.Errorf("link: %w", err)
	}
	return nil
}
