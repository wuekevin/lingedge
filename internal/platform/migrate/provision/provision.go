// Package provision 是安装向导的「MySQL 步骤」所需的数据库管理操作（Story 1.24、ADR-026 §5、tools-design §13）：
//
//   - 事实探测与预检：版本 ≥8.0.16（非 MariaDB）、utf8mb4 与 utf8mb4_0900_ai_ci、sql_mode（严格模式）、@@log_bin 与 @@log_bin_trust_function_creators；
//   - 「建账号模式」：用临时管理员凭据创建专用库与三类账号（lingedge_app / lingedge_maint / lingedge_migrate，主机部分 '%'，与迁移里
//     不带主机的 GRANT 一致）并授予 lingedge_migrate `ALL ON db.* WITH GRANT OPTION` 与全局 `SET_USER_ID`；app/maint 的逐表授权由迁移 SQL 完成
//     （database/README §5.1「实现备注」：库级授权无法对 audit_logs/wallet_entries 收窄，故这里**不**授予 app/maint 任何库级权限）；
//   - 目标库必须为空（FR-6，不静默覆盖）；
//   - 口令重置（续做：ALTER USER，ADR-026 §6）。
//
// 它位于 internal/platform/migrate 之下：只有 db、migrate 与 repository 包可以 import database/sql 与 MySQL 驱动（门禁 no-sql-outside-repository）。
//
// 口令纪律：口令只作为 config.Secret/[]byte 传入；SQL 字面量只经唯一的转义函数 literal() 构造；返回的错误不含 SQL 文本（驱动错误里的语法片段可能回显口令），
// 只含 MySQL 错误号与（已替换掉全部已知口令的）服务端消息；连接对象从不被日志化。管理员连接用后由调用方 Close。
package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/wuekevin/lingedge/internal/platform/config"
)

// 账号名（迁移里写死的授权对象，迁移 lint 只允许授权给这两个账号）与缺省迁移账号名。
const (
	AppUser     = "lingedge_app"
	MaintUser   = "lingedge_maint"
	MigrateUser = "lingedge_migrate"
	// AccountHost 是三类账号的主机部分：迁移 SQL 里不带主机的 GRANT 等价于 '%'（database/README §5.1）。
	AccountHost = "%"
)

// MinVersion 是最低 MySQL 版本（CHECK 约束自此版本起才强制执行）。
const MinVersion = "8.0.16"

var (
	reDB   = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)
	reUser = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,32}$`)
	reHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.\-]{0,251}[A-Za-z0-9])?$`)
)

// Target 是 MySQL 连接目标。
type Target struct {
	Host string
	Port int
	// Socket 非空时走 Unix 域套接字（与 Host/Port 互斥，CC-13）；套接字连接无需 TLS。
	Socket string
	// TLS：""（仅回环）、true、false、skip-verify、preferred。
	TLS string
}

func (t Target) validate() error {
	if t.Socket != "" {
		if t.Host != "" || t.Port != 0 {
			return errors.New("provision: socket 与 host/port 只能设置其一")
		}
		if !ValidSocketPath(t.Socket) {
			return errors.New("provision: socket 必须是规范化的绝对路径（无 NUL/换行，最长 100 字节）")
		}
		if t.TLS != "" && t.TLS != "false" {
			return errors.New("provision: Unix 套接字连接不使用 TLS（tls 须留空或 false）")
		}
		return nil
	}
	if t.Host == "" || (net.ParseIP(t.Host) == nil && !reHost.MatchString(t.Host)) {
		return errors.New("provision: host 必须是主机名或 IP")
	}
	if t.Port < 1 || t.Port > 65535 {
		return errors.New("provision: port 必须在 1–65535")
	}
	switch t.TLS {
	case "", "true", "false", "skip-verify", "preferred":
	default:
		return errors.New("provision: tls 必须是 true|false|skip-verify|preferred 之一或留空")
	}
	if t.TLS == "" && !isLoopback(t.Host) {
		return errors.New("provision: 非回环主机必须显式设置 tls（口令与数据不得在网络上明文传输；确需明文请显式设为 false）")
	}
	return nil
}

// ValidSocketPath 报告 p 是否是可接受的 MySQL Unix 套接字路径：规范化绝对路径、无 NUL/换行、长度 ≤100（sun_path 上限约 104–108）。
func ValidSocketPath(p string) bool {
	return p != "" && len(p) <= 100 && filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsAny(p, "\x00\r\n")
}

func isLoopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ValidDatabaseName / ValidUserName 供上层在构造请求前做同口径的校验。
func ValidDatabaseName(s string) bool { return reDB.MatchString(s) }
func ValidUserName(s string) bool     { return reUser.MatchString(s) }

// Error 是对 MySQL 错误的脱敏包装：只含阶段、错误号与消息（消息里的已知口令已替换为 ***），绝不含 SQL 文本。
type Error struct {
	Op     string
	Number uint16 // 0 = 非 MySQL 服务端错误（网络/握手）
	Msg    string
}

func (e *Error) Error() string {
	if e.Number != 0 {
		return fmt.Sprintf("provision: %s 失败（MySQL 错误 %d：%s）", e.Op, e.Number, e.Msg)
	}
	return fmt.Sprintf("provision: %s 失败（%s）", e.Op, e.Msg)
}

// MySQLNumber 返回错误链里的 MySQL 错误号（无则 0）。
func MySQLNumber(err error) uint16 {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Number
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

// Conn 是一个到 MySQL 的小连接池（≤2 连接）。用完必须 Close。
type Conn struct {
	db          *sql.DB
	noBackslash bool
	secrets     []string
}

// Open 以 user/pw 连接 target；database 可为空（建库前的管理员连接）。口令不进入任何错误文本。
func Open(ctx context.Context, t Target, user string, pw config.Secret, database string) (*Conn, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	if !reUser.MatchString(user) {
		return nil, errors.New("provision: 用户名必须是 1–32 位的字母/数字/_/./-")
	}
	if database != "" && !reDB.MatchString(database) {
		return nil, errors.New("provision: 库名必须是 MySQL 标识符（字母/数字/_/$，最长 64）")
	}
	if !pw.IsSet() {
		return nil, errors.New("provision: 必须提供口令（不存在默认口令，NFR-6）")
	}
	c := mysql.NewConfig()
	c.User, c.Passwd, c.DBName = user, pw.Reveal(), database
	if t.Socket != "" {
		c.Net, c.Addr = "unix", t.Socket
	} else {
		c.Net, c.Addr = "tcp", net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	}
	c.Collation = "utf8mb4_0900_ai_ci"
	c.Timeout, c.ReadTimeout, c.WriteTimeout = 10*time.Second, 120*time.Second, 120*time.Second
	c.MultiStatements, c.InterpolateParams = false, false
	if t.TLS != "" {
		c.TLSConfig = t.TLS
	}
	conn, err := mysql.NewConnector(c)
	if err != nil {
		return nil, &Error{Op: "构造连接", Msg: "配置无效"}
	}
	sdb := sql.OpenDB(conn)
	sdb.SetMaxOpenConns(2)
	sdb.SetMaxIdleConns(1)
	sdb.SetConnMaxLifetime(5 * time.Minute)
	out := &Conn{db: sdb, secrets: []string{pw.Reveal()}}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := sdb.PingContext(pctx); err != nil {
		_ = sdb.Close()
		return nil, out.wrap("连接", err)
	}
	var mode string
	if err := sdb.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&mode); err == nil {
		out.noBackslash = hasMode(mode, "NO_BACKSLASH_ESCAPES")
	}
	return out, nil
}

// Close 关闭连接池并清除已知口令引用。
func (c *Conn) Close() {
	if c == nil {
		return
	}
	_ = c.db.Close()
	c.secrets = nil
}

// Remember 登记需要从错误消息里抹掉的口令（如即将写进 CREATE USER 的新口令）。
func (c *Conn) remember(secrets ...string) {
	for _, s := range secrets {
		if s != "" {
			c.secrets = append(c.secrets, s)
		}
	}
}

func (c *Conn) wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	var me *mysql.MySQLError
	msg := "网络或握手失败"
	var num uint16
	if errors.As(err, &me) {
		num, msg = me.Number, me.Message
	} else if errors.Is(err, context.DeadlineExceeded) {
		msg = "超时"
	} else if errors.Is(err, context.Canceled) {
		msg = "已取消"
	}
	for _, s := range c.secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return &Error{Op: op, Number: num, Msg: msg}
}

func hasMode(mode, want string) bool {
	for _, m := range strings.Split(mode, ",") {
		if strings.EqualFold(strings.TrimSpace(m), want) {
			return true
		}
	}
	return false
}

// literal 是构造 SQL 字符串字面量的唯一入口（ADR-026 §10）：单引号翻倍；未启用 NO_BACKSLASH_ESCAPES 时反斜杠翻倍。
// 含 NUL 的值被拒绝（MySQL 字面量不能携带 NUL）。
func (c *Conn) literal(s string) (string, error) {
	if strings.ContainsRune(s, 0) {
		return "", errors.New("provision: 值不得含 NUL")
	}
	if !c.noBackslash {
		s = strings.ReplaceAll(s, `\`, `\\`)
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'", nil
}

func (c *Conn) account(user string) (string, error) {
	if !reUser.MatchString(user) {
		return "", errors.New("provision: 非法的账号名")
	}
	h, _ := c.literal(AccountHost)
	u, _ := c.literal(user)
	return u + "@" + h, nil
}

func (c *Conn) exec(ctx context.Context, op, stmt string) error {
	_, err := c.db.ExecContext(ctx, stmt)
	return c.wrap(op, err)
}

// Facts 是服务器事实。
type Facts struct {
	Version        string
	LogBin         bool
	TrustFunctions bool
	SQLMode        string
	CharsetServer  string
	HasCollation   bool // utf8mb4_0900_ai_ci 可用
}

// Facts 读取服务器事实。
func (c *Conn) Facts(ctx context.Context) (Facts, error) {
	var f Facts
	var logBin, trust int
	if err := c.db.QueryRowContext(ctx, "SELECT VERSION(), @@log_bin, @@log_bin_trust_function_creators, @@GLOBAL.sql_mode, @@character_set_server").
		Scan(&f.Version, &logBin, &trust, &f.SQLMode, &f.CharsetServer); err != nil {
		return f, c.wrap("读取服务器事实", err)
	}
	f.LogBin, f.TrustFunctions = logBin == 1, trust == 1
	var n int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.COLLATIONS WHERE COLLATION_NAME = 'utf8mb4_0900_ai_ci'").Scan(&n); err != nil {
		return f, c.wrap("读取排序规则", err)
	}
	f.HasCollation = n > 0
	return f, nil
}

// Finding 是一项预检结论。
type Finding struct {
	Key      string `json:"key"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity"` // info|warn|error
	Message  string `json:"message"`
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)

// CheckVersion 校验 VERSION()：MySQL ≥ 8.0.16，MariaDB 与低版本拒绝。
func CheckVersion(v string) error {
	if strings.Contains(strings.ToLower(v), "mariadb") {
		return errors.New("不支持 MariaDB（CHECK/生成列/外键语义与 MySQL 不同）")
	}
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return errors.New("无法识别的版本字符串")
	}
	var n [3]int
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	if n[0] < 8 || (n[0] == 8 && n[1] == 0 && n[2] < 16) {
		return fmt.Errorf("MySQL %s 低于最低版本 %s：设计依赖 CHECK 约束，8.0.16 起才强制执行", v, MinVersion)
	}
	return nil
}

// Check 对事实给出逐项结论（环境检查与预检共用）。
func (f Facts) Check() []Finding {
	var out []Finding
	add := func(key string, ok bool, sev, msg string) {
		out = append(out, Finding{Key: key, OK: ok, Severity: sev, Message: msg})
	}
	if err := CheckVersion(f.Version); err != nil {
		add("mysql.version", false, "error", err.Error())
	} else {
		add("mysql.version", true, "info", "MySQL "+f.Version+" ≥ "+MinVersion)
	}
	if !f.HasCollation {
		add("mysql.utf8mb4", false, "error", "服务器不支持 utf8mb4_0900_ai_ci 排序规则（需要 MySQL 8.0 的 utf8mb4）")
	} else {
		add("mysql.utf8mb4", true, "info", "utf8mb4 / utf8mb4_0900_ai_ci 可用（服务器默认字符集 "+f.CharsetServer+"，专用库将显式创建为 utf8mb4）")
	}
	if !hasMode(f.SQLMode, "STRICT_TRANS_TABLES") && !hasMode(f.SQLMode, "STRICT_ALL_TABLES") {
		add("mysql.sql_mode", false, "error", "全局 sql_mode 未启用严格模式（STRICT_TRANS_TABLES/STRICT_ALL_TABLES）：非法值会被静默截断，设计依赖严格模式")
	} else {
		add("mysql.sql_mode", true, "info", "sql_mode 已启用严格模式")
	}
	switch {
	case !f.LogBin:
		add("mysql.binlog", true, "info", "log_bin=OFF：建触发器不需要额外权限")
	case f.TrustFunctions:
		add("mysql.binlog", true, "info", "log_bin=ON 且 log_bin_trust_function_creators=1：建触发器不需要额外权限")
	default:
		add("mysql.binlog", true, "warn", "log_bin=ON 且 log_bin_trust_function_creators=0：迁移账号需要全局 SET_USER_ID（或 SUPER）才能建触发器，将用探针触发器验证")
	}
	return out
}

// DatabaseExists 报告库是否存在。
func (c *Conn) DatabaseExists(ctx context.Context, name string) (bool, error) {
	if !reDB.MatchString(name) {
		return false, errors.New("provision: 非法库名")
	}
	var n int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name).Scan(&n); err != nil {
		return false, c.wrap("检查库是否存在", err)
	}
	return n > 0, nil
}

// TableCount 返回库内表/视图数。
func (c *Conn) TableCount(ctx context.Context, name string) (int, error) {
	var n int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?", name).Scan(&n); err != nil {
		return 0, c.wrap("检查库是否为空", err)
	}
	return n, nil
}

// DatabaseCharset 返回库的缺省字符集。
func (c *Conn) DatabaseCharset(ctx context.Context, name string) (string, error) {
	var cs string
	if err := c.db.QueryRowContext(ctx, "SELECT DEFAULT_CHARACTER_SET_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name).Scan(&cs); err != nil {
		return "", c.wrap("读取库字符集", err)
	}
	return cs, nil
}

// InProgressInstall 报告库里是否存在「本向导中断留下的、尚未完成」的安装状态：schema_migrations 与 system_installations 都存在，
// 且 system_installations 里没有任何 install_locked_at 非空的行。用于续做时放行「非空」的目标库（ADR-026 §6）。
func (c *Conn) InProgressInstall(ctx context.Context, name string) (bool, error) {
	if !reDB.MatchString(name) {
		return false, errors.New("provision: 非法库名")
	}
	var n int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME IN ('schema_migrations','system_installations')", name).Scan(&n); err != nil {
		return false, c.wrap("检查安装状态", err)
	}
	if n < 2 {
		return false, nil
	}
	var locked int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+name+"`.system_installations WHERE install_locked_at IS NOT NULL").Scan(&locked); err != nil {
		return false, c.wrap("读取安装状态", err)
	}
	return locked == 0, nil
}

// CreateDatabase 创建专用库（utf8mb4）。库已存在时不做任何事（调用方已先检查为空）。
func (c *Conn) CreateDatabase(ctx context.Context, name string) error {
	if !reDB.MatchString(name) {
		return errors.New("provision: 非法库名")
	}
	return c.exec(ctx, "创建库", "CREATE DATABASE IF NOT EXISTS `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
}

// AccountSpec 是建账号模式要创建的三类账号。
type AccountSpec struct {
	Database                 string
	AppUser, MaintUser       string
	MigrateUser              string
	AppPass, MaintPass       config.Secret
	MigratePass              config.Secret
	SkipMaint                bool // 不创建 maint 账号
	AllowPreexistingAccounts bool // 续做：账号已存在（须仅持有目标库权限）时重置口令而不是拒绝
}

// ErrAccountInUse：同名账号已存在且持有目标库之外的权限（可能属于别的系统），拒绝改写。
var ErrAccountInUse = errors.New("provision: 同名账号已存在且持有目标库之外的权限，拒绝改写（账号是全局对象，迁移 SQL 里的授权对象固定为 lingedge_app/lingedge_maint）")

// SetUserIDResult 说明全局 SET_USER_ID 授权是否成功（管理员无此授权能力不是致命错误：由后续触发器探针决定是否阻断）。
type SetUserIDResult struct {
	Granted bool
	Reason  string
}

// CreateAccounts 创建三类账号并授予 lingedge_migrate 的权限（幂等：账号已存在则在安全前提下重置口令）。口令仅经 literal() 进入语句文本。
func (c *Conn) CreateAccounts(ctx context.Context, s AccountSpec) (SetUserIDResult, error) {
	var res SetUserIDResult
	if !reDB.MatchString(s.Database) {
		return res, errors.New("provision: 非法库名")
	}
	type acct struct {
		user string
		pw   config.Secret
	}
	list := []acct{{s.AppUser, s.AppPass}, {s.MigrateUser, s.MigratePass}}
	if !s.SkipMaint {
		list = append(list, acct{s.MaintUser, s.MaintPass})
	}
	for _, a := range list {
		c.remember(a.pw.Reveal())
	}
	for _, a := range list {
		if !a.pw.IsSet() {
			return res, errors.New("provision: 缺少账号口令")
		}
		acc, err := c.account(a.user)
		if err != nil {
			return res, err
		}
		exists, err := c.userExists(ctx, a.user)
		if err != nil {
			return res, err
		}
		if exists {
			if !s.AllowPreexistingAccounts {
				return res, fmt.Errorf("%w: %s", ErrAccountInUse, a.user)
			}
			if err := c.accountOnlyForDB(ctx, acc, s.Database); err != nil {
				return res, fmt.Errorf("%w: %s", err, a.user)
			}
		}
		lit, err := c.literal(a.pw.Reveal())
		if err != nil {
			return res, err
		}
		verb := "CREATE USER"
		if exists {
			verb = "ALTER USER"
		}
		if err := c.exec(ctx, verb+" "+a.user, verb+" "+acc+" IDENTIFIED BY "+lit); err != nil {
			return res, err
		}
	}
	mig, _ := c.account(s.MigrateUser)
	if err := c.exec(ctx, "授予 "+s.MigrateUser+" 库级权限", "GRANT ALL PRIVILEGES ON `"+strings.ReplaceAll(s.Database, "_", `\_`)+"`.* TO "+mig+" WITH GRANT OPTION"); err != nil {
		return res, err
	}
	if err := c.exec(ctx, "授予 "+s.MigrateUser+" SET_USER_ID", "GRANT SET_USER_ID ON *.* TO "+mig); err != nil {
		res.Reason = err.Error()
	} else {
		res.Granted = true
	}
	return res, nil
}

func (c *Conn) userExists(ctx context.Context, user string) (bool, error) {
	var n int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.user WHERE User = ? AND Host = ?", user, AccountHost).Scan(&n); err != nil {
		return false, c.wrap("检查账号是否存在", err)
	}
	return n > 0, nil
}

// accountOnlyForDB：账号的全部授权都只作用于目标库（或仅 USAGE/SET_USER_ID）；否则返回 ErrAccountInUse。
func (c *Conn) accountOnlyForDB(ctx context.Context, acc, database string) error {
	rows, err := c.db.QueryContext(ctx, "SHOW GRANTS FOR "+acc)
	if err != nil {
		return c.wrap("读取账号授权", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return c.wrap("读取账号授权", err)
		}
		up := strings.ToUpper(g)
		switch {
		case strings.Contains(up, " ON *.* TO "):
			// 全局授权：只允许 USAGE 与 SET_USER_ID（向导自己授予的）。
			priv := strings.TrimSpace(strings.TrimPrefix(up[:strings.Index(up, " ON *.* TO ")], "GRANT "))
			for _, p := range strings.Split(priv, ",") {
				if p = strings.TrimSpace(p); p != "USAGE" && p != "SET_USER_ID" {
					return ErrAccountInUse
				}
			}
		case strings.Contains(g, "`"+database+"`."), strings.Contains(g, "`"+strings.ReplaceAll(database, "_", `\_`)+"`."):
		default:
			return ErrAccountInUse
		}
	}
	return c.wrap("读取账号授权", rows.Err())
}

// ResetPassword 对已存在的账号执行 ALTER USER … IDENTIFIED BY（续做重置、轮换）；retainCurrent 为真时附 RETAIN CURRENT PASSWORD（MySQL ≥8.0.14 双口令）。
func (c *Conn) ResetPassword(ctx context.Context, user string, pw config.Secret, retainCurrent bool) error {
	acc, err := c.account(user)
	if err != nil {
		return err
	}
	c.remember(pw.Reveal())
	lit, err := c.literal(pw.Reveal())
	if err != nil {
		return err
	}
	stmt := "ALTER USER " + acc + " IDENTIFIED BY " + lit
	if retainCurrent {
		stmt += " RETAIN CURRENT PASSWORD"
	}
	return c.exec(ctx, "重置 "+user+" 口令", stmt)
}

// DiscardOldPassword 丢弃账号的旧口令（轮换第二阶段，ADR-026 §8）。
func (c *Conn) DiscardOldPassword(ctx context.Context, user string) error {
	acc, err := c.account(user)
	if err != nil {
		return err
	}
	return c.exec(ctx, "丢弃 "+user+" 旧口令", "ALTER USER "+acc+" DISCARD OLD PASSWORD")
}

// Ping 在不选库的前提下验证账号能登录（app/maint 在迁移前没有任何库权限，选库会 ERROR 1044）。
func (c *Conn) Ping(ctx context.Context) error {
	var one int
	return c.wrap("登录校验", c.db.QueryRowContext(ctx, "SELECT 1").Scan(&one))
}
