package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// MySQL 访问（tools-design §17.1/§18.2）：只经 safeexec 调 mysqldump / mysql 客户端（参数数组，不经 shell）。
//
// 口令的唯一通道是 0600 的临时 --defaults-extra-file（位于 0700 的暂存目录，O_EXCL 创建，用后立即清零覆盖并删除）：
// 它不会出现在命令行（ps 不可见）、环境变量、日志或错误信息里。Tools 不用 Go 的 MySQL 驱动（只有 internal/platform 的 db/migrate/repository 包可以 import 它，
// scripts/gates 的依赖规则），所以查询也经 mysql 客户端的 --batch 模式（SQL 全部是本文件里的常量，唯一的插值是经正则校验的库名）。

// MySQLAllowed 是本包 MySQL Runner 的可执行文件白名单。
var MySQLAllowed = []string{"mysqldump", "mysql"}

// ServerBinAllowed 是调用 lingedge-server（lineage bump、migrate plan）的 Runner 白名单。
var ServerBinAllowed = []string{"lingedge-server"}

// NewMySQLRunner 构造只允许 mysqldump/mysql 的 Runner。dirs 为空取 safeexec.DefaultSearchDirs；也可在 Cmd.Path 里给出基名在白名单内的绝对路径（宝塔的 /www/server/mysql/bin 等）。
// 超时按大库放宽（默认 2 小时；调用方的 context 仍可随时取消）。
func NewMySQLRunner(dirs []string) (*safeexec.Runner, error) {
	if len(dirs) == 0 {
		dirs = safeexec.DefaultSearchDirs
	}
	return safeexec.New(safeexec.Options{Allowed: MySQLAllowed, SearchDirs: dirs, MaxOutput: 8 << 20, Timeout: 2 * time.Hour})
}

// NewServerRunner 构造只允许 lingedge-server 的 Runner（Cmd.Path 须是基名为 lingedge-server 的绝对路径）。
func NewServerRunner() (*safeexec.Runner, error) {
	return safeexec.New(safeexec.Options{Allowed: ServerBinAllowed, SearchDirs: safeexec.DefaultSearchDirs, MaxOutput: 1 << 20, Timeout: 5 * time.Minute})
}

var (
	reUserName = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,80}$`)
	reHostName = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,253}$`)
)

// DBTarget 是数据库连接目标（不含口令）。
type DBTarget struct {
	Host   string
	Port   int
	Socket string
	Name   string
	User   string
	TLS    string
}

// Validate 校验每个会进入选项文件/SQL 的字段（严格字符集，拒绝换行/引号/NUL）。
func (t DBTarget) Validate() error {
	if t.Socket == "" && t.Host == "" {
		return errors.New("缺少数据库连接目标（host 或 socket）")
	}
	if t.Socket != "" {
		if t.Host != "" || t.Port != 0 {
			return errors.New("socket 与 host/port 互斥")
		}
		if !filepath.IsAbs(t.Socket) || filepath.Clean(t.Socket) != t.Socket || strings.ContainsAny(t.Socket, "\x00\n\r\"'\\") {
			return errors.New("socket 必须是规范的绝对路径")
		}
	} else {
		if !reHostName.MatchString(t.Host) {
			return errors.New("host 含非法字符")
		}
		if t.Port < 0 || t.Port > 65535 {
			return errors.New("port 超出范围")
		}
	}
	if !reDBName.MatchString(t.Name) {
		return errors.New("库名只能含字母数字、下划线、$")
	}
	if !reUserName.MatchString(t.User) {
		return errors.New("用户名含非法字符")
	}
	switch t.TLS {
	case "", "true", "false", "skip-verify", "preferred":
	default:
		return errors.New("tls 只能是 true|false|skip-verify|preferred")
	}
	return nil
}

// DBConn 是一次连接所需的全部信息。Password 由调用方用后 Close 清零。
type DBConn struct {
	DBTarget
	Password []byte
}

// Close 清零口令。
func (c *DBConn) Close() { zero(c.Password); c.Password = nil }

func optQuote(s string) (string, error) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("含控制字符")
		}
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`, nil
}

func (c DBConn) optionFile() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	pw, err := optQuote(string(c.Password))
	if err != nil {
		return nil, errors.New("数据库口令含控制字符，无法写入选项文件")
	}
	var b bytes.Buffer
	b.WriteString("[client]\n")
	fmt.Fprintf(&b, "user=%s\n", c.User)
	fmt.Fprintf(&b, "password=%s\n", pw)
	if c.Socket != "" {
		fmt.Fprintf(&b, "socket=%s\n", c.Socket)
	} else {
		fmt.Fprintf(&b, "host=%s\n", c.Host)
		if c.Port != 0 {
			fmt.Fprintf(&b, "port=%d\n", c.Port)
		}
		// 本机/远程 TCP：回环以外必须由调用方显式给 TLS 模式；与 server 侧一致的取值映射到客户端的 ssl-mode。
		switch c.TLS {
		case "true", "skip-verify":
			b.WriteString("ssl-mode=REQUIRED\n")
		case "false":
			b.WriteString("ssl-mode=DISABLED\n")
		case "preferred":
			b.WriteString("ssl-mode=PREFERRED\n")
		}
	}
	b.WriteString("default-character-set=utf8mb4\n")
	return b.Bytes(), nil
}

// MySQL 是 mysqldump/mysql 的调用封装。
type MySQL struct {
	Run service.Runner
	// DumpBin/ClientBin 是 mysqldump/mysql 的裸名或绝对路径（空取裸名，由 Runner 在固定目录里解析）。
	DumpBin, ClientBin string
	// Scratch 是暂存根目录（须由调用方保证 0700、仅 root 可进）；选项文件与转储的中间文件放在其子目录里。
	Scratch string
}

func (m *MySQL) bin(client bool) string {
	switch {
	case client && m.ClientBin != "":
		return m.ClientBin
	case client:
		return "mysql"
	case m.DumpBin != "":
		return m.DumpBin
	}
	return "mysqldump"
}

// withOptions 创建 0700 暂存子目录与 0600 选项文件，执行 fn，随后无论成败都清零覆盖并删除；删除失败本身就是错误（凭据残留不得被吞掉）。
func (m *MySQL) withOptions(conn DBConn, fn func(optFile string) error) (err error) {
	content, err := conn.optionFile()
	if err != nil {
		return err
	}
	defer zero(content)
	if err := security.MkdirAllPerm(m.Scratch, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(m.Scratch, "my-")
	if err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	opt := filepath.Join(dir, "client.cnf")
	defer func() {
		if cerr := shredAndRemove(opt, len(content)); cerr != nil && err == nil {
			err = cerr
		}
		if rerr := os.RemoveAll(dir); rerr != nil && err == nil {
			err = fmt.Errorf("backup: 清理凭据暂存目录失败: %w", rerr)
		}
		if _, serr := os.Lstat(dir); serr == nil && err == nil {
			err = errors.New("backup: 凭据暂存目录清理后仍然存在")
		}
	}()
	f, err := security.CreateFile(opt, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(content); werr != nil {
		_ = f.Close()
		return werr
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		return serr
	}
	if err := f.Close(); err != nil {
		return err
	}
	return fn(opt)
}

// shredAndRemove 先把内容覆盖为 0 再删除（尽力而为：日志型/写时复制文件系统无法保证，但缩小残留窗口；真正的保护是 0700 目录 + 立即删除）。
func shredAndRemove(path string, n int) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err == nil {
		_, _ = f.Write(make([]byte, n))
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup: 删除凭据选项文件失败: %w", err)
	}
	return nil
}

func (m *MySQL) clientArgs(opt string, extra ...string) []string {
	// --defaults-extra-file 必须是第一个参数。
	return append([]string{"--defaults-extra-file=" + opt}, extra...)
}

// Dump 以 `mysqldump --single-transaction --routines --triggers` 把库转储到 out（0600，已存在则报错）。
// --result-file 让转储直接写文件，不经 Runner 的输出缓冲（大库）。
func (m *MySQL) Dump(ctx context.Context, conn DBConn, out string) error {
	f, err := security.CreateFile(out, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return m.withOptions(conn, func(opt string) error {
		args := m.clientArgs(opt, "--single-transaction", "--routines", "--triggers", "--events", "--hex-blob", "--set-gtid-purged=OFF",
			"--default-character-set=utf8mb4", "--result-file="+out, conn.Name)
		if _, err := m.Run.Run(ctx, safeexec.Cmd{Path: m.bin(false), Args: args}); err != nil {
			return fmt.Errorf("mysqldump 失败: %w", err)
		}
		return nil
	})
}

// Query 执行只读查询并返回行（制表符分隔）。sql 必须是调用方的常量（或仅含已校验标识符的拼接）。
func (m *MySQL) Query(ctx context.Context, conn DBConn, sql string) ([][]string, error) {
	var rows [][]string
	err := m.withOptions(conn, func(opt string) error {
		args := m.clientArgs(opt, "--batch", "--raw", "--skip-column-names", "--database="+conn.Name, "-e", sql)
		res, err := m.Run.Run(ctx, safeexec.Cmd{Path: m.bin(true), Args: args})
		if err != nil {
			return fmt.Errorf("mysql 查询失败: %w", err)
		}
		if res.Truncated {
			return errors.New("mysql 查询输出超过上限")
		}
		for _, ln := range strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n") {
			if ln == "" {
				continue
			}
			rows = append(rows, strings.Split(ln, "\t"))
		}
		return nil
	})
	return rows, err
}

// ExecScript 把脚本经 stdin 交给 mysql 客户端执行。
func (m *MySQL) ExecScript(ctx context.Context, conn DBConn, script io.Reader) error {
	return m.withOptions(conn, func(opt string) error {
		args := m.clientArgs(opt, "--database="+conn.Name)
		if _, err := m.Run.Run(ctx, safeexec.Cmd{Path: m.bin(true), Args: args, Stdin: script}); err != nil {
			return fmt.Errorf("mysql 执行失败: %w", err)
		}
		return nil
	})
}

// Load 把转储文件灌入库。
func (m *MySQL) Load(ctx context.Context, conn DBConn, dumpFile string) error {
	f, err := os.Open(dumpFile)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return m.ExecScript(ctx, conn, f)
}

func qs(s string) string { return strings.ReplaceAll(s, "'", "''") }

func ident(s string) (string, error) {
	if strings.ContainsAny(s, "\x00\t\n\r") || s == "" {
		return "", fmt.Errorf("对象名含不支持的字符 %q", s)
	}
	return "`" + strings.ReplaceAll(s, "`", "``") + "`", nil
}

// Objects 返回库中的对象：表/视图/例程/事件。
type Objects struct {
	Tables   []string
	Views    []string
	Routines [][2]string // {名, PROCEDURE|FUNCTION}
	Events   []string
}

// Count 是对象总数。
func (o Objects) Count() int { return len(o.Tables) + len(o.Views) + len(o.Routines) + len(o.Events) }

// ListObjects 列出库内全部对象（information_schema）。
func (m *MySQL) ListObjects(ctx context.Context, conn DBConn) (Objects, error) {
	var o Objects
	db := qs(conn.Name)
	rows, err := m.Query(ctx, conn, "SELECT table_name, table_type FROM information_schema.tables WHERE table_schema = '"+db+"' ORDER BY table_name")
	if err != nil {
		return o, err
	}
	for _, r := range rows {
		if len(r) != 2 {
			return o, errors.New("information_schema.tables 输出格式异常")
		}
		if r[1] == "VIEW" {
			o.Views = append(o.Views, r[0])
		} else {
			o.Tables = append(o.Tables, r[0])
		}
	}
	if rows, err = m.Query(ctx, conn, "SELECT routine_name, routine_type FROM information_schema.routines WHERE routine_schema = '"+db+"' ORDER BY routine_name"); err != nil {
		return o, err
	}
	for _, r := range rows {
		if len(r) != 2 || (r[1] != "PROCEDURE" && r[1] != "FUNCTION") {
			return o, errors.New("information_schema.routines 输出格式异常")
		}
		o.Routines = append(o.Routines, [2]string{r[0], r[1]})
	}
	if rows, err = m.Query(ctx, conn, "SELECT event_name FROM information_schema.events WHERE event_schema = '"+db+"' ORDER BY event_name"); err != nil {
		return o, err
	}
	for _, r := range rows {
		o.Events = append(o.Events, r[0])
	}
	return o, nil
}

// Wipe 删除库内全部对象但保留库本身（及其上的授权）。仅在 --replace 且主机名二次确认之后调用。
func (m *MySQL) Wipe(ctx context.Context, conn DBConn) error {
	o, err := m.ListObjects(ctx, conn)
	if err != nil {
		return err
	}
	if o.Count() == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("SET FOREIGN_KEY_CHECKS=0;\n")
	add := func(kind, name string) error {
		id, err := ident(name)
		if err != nil {
			return err
		}
		sb.WriteString("DROP " + kind + " IF EXISTS " + id + ";\n")
		return nil
	}
	for _, v := range o.Views {
		if err := add("VIEW", v); err != nil {
			return err
		}
	}
	for _, t := range o.Tables {
		if err := add("TABLE", t); err != nil {
			return err
		}
	}
	for _, r := range o.Routines {
		if err := add(r[1], r[0]); err != nil {
			return err
		}
	}
	for _, e := range o.Events {
		if err := add("EVENT", e); err != nil {
			return err
		}
	}
	sb.WriteString("SET FOREIGN_KEY_CHECKS=1;\n")
	if err := m.ExecScript(ctx, conn, strings.NewReader(sb.String())); err != nil {
		return err
	}
	after, err := m.ListObjects(ctx, conn)
	if err != nil {
		return err
	}
	if after.Count() != 0 {
		return fmt.Errorf("清空后库内仍有 %d 个对象", after.Count())
	}
	return nil
}

// tableExists 判断表是否存在。
func (m *MySQL) tableExists(ctx context.Context, conn DBConn, table string) (bool, error) {
	rows, err := m.Query(ctx, conn, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = '"+qs(conn.Name)+"' AND table_name = '"+qs(table)+"'")
	if err != nil {
		return false, err
	}
	return len(rows) == 1 && len(rows[0]) == 1 && rows[0][0] != "0", nil
}

func atoi64(s string) (int64, error) { return strconv.ParseInt(strings.TrimSpace(s), 10, 64) }

// SchemaVersion 返回库已应用的最大迁移版本（经 mysql 客户端读 goose 版本表 schema_migrations；与具体 lingedge-server 二进制无关，旧二进制也能用来核对被新迁移推进过的库）。
// 版本表不存在（库尚未初始化）返回 0。
func (m *MySQL) SchemaVersion(ctx context.Context, conn DBConn) (int64, error) {
	has, err := m.tableExists(ctx, conn, "schema_migrations")
	if err != nil {
		return 0, err
	}
	if !has {
		return 0, nil
	}
	rows, err := m.Query(ctx, conn, "SELECT COALESCE(MAX(version_id), 0) FROM schema_migrations WHERE is_applied = 1")
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, errors.New("schema_migrations 的查询结果无法识别")
	}
	return atoi64(rows[0][0])
}
