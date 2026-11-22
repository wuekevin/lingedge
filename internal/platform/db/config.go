package db

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/wuekevin/lingedge/internal/platform/config"
)

// Role 是数据库账号角色（IMP-19、database/README §5.1）。三类账号对应三个互相独立的连接池与三个不可互换的 Go 类型。
type Role string

const (
	// RoleApp：请求路径与 Worker 常规任务（lingedge_app）。SELECT/INSERT/UPDATE/DELETE；对 audit_logs、wallet_entries 仅 SELECT、INSERT。
	RoleApp Role = "app"
	// RoleMaint：仅 retention.cleanup / partition.maintain 使用（lingedge_maint）。分区表表级 SELECT、ALTER、DROP；请求路径不得持有。
	RoleMaint Role = "maint"
	// RoleMigrate：仅 `lingedge-server migrate` 与安装向导的迁移步骤使用（lingedge_migrate）。
	RoleMigrate Role = "migrate"
)

// Config 描述一个连接池的目标与限额。口令用 config.Secret（任何格式化/日志/序列化都是 ***）；
// 三类账号各自持有一份 Config，彼此独立（口令落点见 ADR-026：app/maint 经 dbcred 合并进配置；migrate 只在内存或 root 侧 migrate.yaml）。
type Config struct {
	// 连接目标：Socket 非空时走 Unix 域套接字（与 Host/Port 互斥）。
	Host   string
	Port   int
	Socket string

	Name     string // 库名
	User     string
	Password config.Secret

	// TLS 取值：""（仅回环/Unix socket 允许）、"true"、"skip-verify"、"preferred"、"false"（显式明文）。
	// 非回环主机必须显式设置（校验强制），避免口令与租户数据在网络上明文传输。
	TLS string

	ConnectTimeout time.Duration // 0 表示使用 DefaultConnectTimeout
	ReadTimeout    time.Duration // 0 表示不限
	WriteTimeout   time.Duration // 0 表示不限

	// 连接池限额；0 表示使用该角色的工程缺省（见 PoolDefaults）。
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// DefaultConnectTimeout 是建立连接的缺省超时。
const DefaultConnectTimeout = 10 * time.Second

// PoolLimits 是连接池限额。
type PoolLimits struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// PoolDefaults 返回角色的工程缺省连接池限额。
//
// [ASSUMPTION] 数值为工程缺省，基线未规定，待性能基线报告后以配置覆盖（UD-27）；maint/migrate 的用途决定了它们天然很小
// （清理/迁移串行执行），app 的上限需随压测调整。ConnMaxLifetime 取低于常见 MySQL wait_timeout（8 小时）的值，防止使用被服务端关闭的连接。
func PoolDefaults(r Role) PoolLimits {
	switch r {
	case RoleMaint:
		return PoolLimits{MaxOpenConns: 2, MaxIdleConns: 1, ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute}
	case RoleMigrate:
		return PoolLimits{MaxOpenConns: 1, MaxIdleConns: 1, ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute}
	default:
		return PoolLimits{MaxOpenConns: 25, MaxIdleConns: 10, ConnMaxLifetime: 30 * time.Minute, ConnMaxIdleTime: 5 * time.Minute}
	}
}

// AppConfigFrom 由**合并后**的服务端配置（server.yaml 的 database 节 + 显式口令通道 + 运行期凭据文件，见 dbcred.Merge）生成 app 池配置。
// 调用方必须先经 dbcred.Merge（server.Run 与各子命令共用同一合并入口）；本函数只做字段映射。migrate 凭据不在此处（ADR-026）。
func AppConfigFrom(d config.Database) Config {
	return Config{Host: hostOf(d), Port: portOf(d), Socket: d.Socket, Name: d.Name, User: d.User, Password: d.Password, TLS: d.TLS}
}

// hostOf/portOf：配置了 socket 时连接目标只有 socket（host/port 清空，db.Config 要求互斥）。
func hostOf(d config.Database) string {
	if d.Socket != "" {
		return ""
	}
	return d.Host
}

func portOf(d config.Database) int {
	if d.Socket != "" {
		return 0
	}
	return d.Port
}

// MaintConfigFrom 由合并后的服务端配置生成 maint 池配置；维护账号未配置（用户名或口令缺失）时 ok=false。
// 连接目标（host/port/name）与 app 相同，仅账号与口令不同。
func MaintConfigFrom(d config.Database) (cfg Config, ok bool) {
	if !d.Maint.Configured() {
		return Config{}, false
	}
	return Config{Host: hostOf(d), Port: portOf(d), Socket: d.Socket, Name: d.Name, User: d.Maint.User, Password: d.Maint.Password, TLS: d.TLS}, true
}

var (
	reName = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)
	reUser = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,32}$`)
	reHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.\-]{0,251}[A-Za-z0-9])?$`)
)

func connectTimeout(c Config) time.Duration {
	if c.ConnectTimeout > 0 {
		return c.ConnectTimeout
	}
	return DefaultConnectTimeout
}

// isLocalHost 报告主机是否为回环（localhost 或回环 IP）。
func isLocalHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Validate 校验配置；错误文本不含口令。
func (c Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf("db: "+format, a...)) }
	switch {
	case c.Socket != "" && (c.Host != "" || c.Port != 0):
		add("socket 与 host/port 只能设置其一")
	case c.Socket == "" && c.Host == "":
		add("必须设置 host 或 socket")
	case c.Socket == "" && (c.Port < 1 || c.Port > 65535):
		add("port 必须在 1–65535")
	case c.Socket == "" && net.ParseIP(c.Host) == nil && !reHost.MatchString(c.Host):
		add("host 必须是主机名或 IP")
	}
	if !reName.MatchString(c.Name) {
		add("库名必须是 MySQL 标识符（字母/数字/_/$，最长 64）")
	}
	if !reUser.MatchString(c.User) {
		add("用户名必须是 1–32 位的字母/数字/_/./-")
	}
	if !c.Password.IsSet() {
		add("必须提供口令（不存在默认口令，NFR-6）")
	}
	switch c.TLS {
	case "", "true", "false", "skip-verify", "preferred":
	default:
		add("tls 必须是 true|false|skip-verify|preferred 之一或留空")
	}
	if c.Socket == "" && c.Host != "" && !isLocalHost(c.Host) && c.TLS == "" {
		add("非回环主机必须显式设置 tls（true|skip-verify|preferred），确需明文传输请显式设为 false")
	}
	if c.MaxOpenConns < 0 || c.MaxIdleConns < 0 || c.ConnMaxLifetime < 0 || c.ConnMaxIdleTime < 0 || c.ConnectTimeout < 0 || c.ReadTimeout < 0 || c.WriteTimeout < 0 {
		add("连接池与超时参数不得为负")
	}
	if c.MaxOpenConns > 0 && c.MaxIdleConns > c.MaxOpenConns {
		add("max_idle_conns 不得大于 max_open_conns")
	}
	return errors.Join(errs...)
}

func (c Config) limits(role Role) PoolLimits {
	l := PoolDefaults(role)
	if c.MaxOpenConns > 0 {
		l.MaxOpenConns = c.MaxOpenConns
		if c.MaxIdleConns == 0 && l.MaxIdleConns > l.MaxOpenConns {
			l.MaxIdleConns = l.MaxOpenConns
		}
	}
	if c.MaxIdleConns > 0 {
		l.MaxIdleConns = c.MaxIdleConns
	}
	if c.ConnMaxLifetime > 0 {
		l.ConnMaxLifetime = c.ConnMaxLifetime
	}
	if c.ConnMaxIdleTime > 0 {
		l.ConnMaxIdleTime = c.ConnMaxIdleTime
	}
	return l
}

// driverConfig 生成 go-sql-driver 配置（AC1）：
//   - UTC：Loc=UTC 并在每个新连接上 SET time_zone='+00:00'；
//   - utf8mb4：握手排序规则 utf8mb4_0900_ai_ci（database/README §2.2）；
//   - parseTime：DATETIME 扫描为 time.Time；
//   - 参数化：InterpolateParams=false，带参数的语句走服务端预处理（参数永不拼入 SQL 文本）；MultiStatements=false；
//   - ClientFoundRows：UPDATE 的「受影响行数」按匹配行计数而非变更行计数，使「0 行 = 没有匹配的行」成立（越权判定依赖它）。
func (c Config) driverConfig() *mysql.Config {
	d := mysql.NewConfig()
	d.User = c.User
	d.Passwd = c.Password.Reveal()
	d.DBName = c.Name
	if c.Socket != "" {
		d.Net, d.Addr = "unix", c.Socket
	} else {
		d.Net, d.Addr = "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	}
	d.Collation = "utf8mb4_0900_ai_ci"
	d.ParseTime = true
	d.Loc = time.UTC
	d.InterpolateParams = false
	d.MultiStatements = false
	d.ClientFoundRows = true
	d.CheckConnLiveness = true
	d.AllowAllFiles = false
	d.AllowCleartextPasswords = false
	d.AllowOldPasswords = false
	d.Timeout = connectTimeout(c)
	d.ReadTimeout, d.WriteTimeout = c.ReadTimeout, c.WriteTimeout
	if c.TLS != "" {
		d.TLSConfig = c.TLS
	}
	d.Params = map[string]string{"time_zone": "'+00:00'"}
	return d
}
