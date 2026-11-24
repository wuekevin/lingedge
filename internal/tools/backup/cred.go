package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// 迁移级数据库凭据的来源（ADR-026 §4、tools-design §13.1；与 lingedge-server migrate 一致）。
//
// 口令优先级：环境变量 LINGEDGE_MIGRATE_DB_PASSWORD ＞ LINGEDGE_MIGRATE_DB_PASSWORD_FILE ＞ root 侧 migrate.yaml（root:root 0400）＞ 交互输入（仅 TTY，无回显）；
// 非交互且都没有 → ErrNoCredentials（退出码 3，且此时尚未改动任何东西）。口令绝不经命令行参数。
// 连接目标优先级：命令行显式值 ＞ migrate.yaml ＞ root 属主的 server.yaml 的 database 段；
// **绝不**读取 runtime-credentials.json（`lingedge` 可写：被攻陷后可把 host 指向攻击者的 MySQL，让 root 侧把迁移口令发过去）。
// 为什么备份/恢复用迁移账号：mysqldump 需要 SELECT/SHOW VIEW/TRIGGER/EVENT/ROUTINE，恢复需要 DDL 与 SET_USER_ID（binlog 下建触发器），app/maint 账号刻意没有这些权限。

// 凭据相关环境变量（与 lingedge-server migrate 同名）。
const (
	EnvUser       = "LINGEDGE_MIGRATE_DB_USER"
	EnvDBPass     = "LINGEDGE_MIGRATE_DB_PASSWORD"
	EnvPassFile   = "LINGEDGE_MIGRATE_DB_PASSWORD_FILE"
	EnvMigrateCfg = "LINGEDGE_MIGRATE_CONFIG"
	// 连接目标的环境变量通道（recover 等没有命令行标志的路径使用；优先级低于命令行、高于 migrate.yaml）。
	EnvHost   = "LINGEDGE_MIGRATE_DB_HOST"
	EnvPort   = "LINGEDGE_MIGRATE_DB_PORT"
	EnvSocket = "LINGEDGE_MIGRATE_DB_SOCKET"
	EnvName   = "LINGEDGE_MIGRATE_DB_NAME"
	// DefaultUser 是迁移账号缺省名。
	DefaultUser = "lingedge_migrate"
	// DefaultMigrateYAML 是 root 侧凭据文件缺省路径。
	DefaultMigrateYAML = "/etc/lingedge/server/migrate.yaml"
)

// 凭据错误。
var (
	ErrNoCredentials = errors.New("缺少迁移账号口令，且当前不是交互终端")
	ErrNoTarget      = errors.New("缺少数据库连接目标")
)

// CredEnv 是凭据解析的输入。
type CredEnv struct {
	Getenv func(string) string
	// ReadSecret 读取无回显输入（仅交互时非 nil）。
	ReadSecret func(label string) (string, error)
	// MigrateYAML/ServerYAML 是（已加虚拟根前缀的）文件路径；空则跳过该来源。
	MigrateYAML, ServerYAML string
	// MigrateYAMLExplicit：路径由环境变量显式指定（缺失即错误）。
	MigrateYAMLExplicit bool
	// Explicit 是命令行显式给出的目标（零值字段表示未给）。
	Explicit DBTarget
	// AllowedUIDs 是凭据文件允许的属主（nil 取 {当前 euid, 0}）。
	AllowedUIDs []uint32
}

type migrateFile struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Socket   string `yaml:"socket"`
	Database string `yaml:"database"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	TLS      string `yaml:"tls"`
}

type serverFile struct {
	Database struct {
		Host   string `yaml:"host"`
		Port   int    `yaml:"port"`
		Socket string `yaml:"socket"`
		Name   string `yaml:"name"`
		TLS    string `yaml:"tls"`
	} `yaml:"database"`
}

func (e CredEnv) uids() []uint32 {
	if e.AllowedUIDs != nil {
		return e.AllowedUIDs
	}
	u := uint32(os.Geteuid())
	if u == 0 {
		return []uint32{0}
	}
	return []uint32{u, 0}
}

// readTrusted 读取凭据/配置文件：O_NOFOLLOW、普通文件、无 group/other 位（strict）或无 group/other 写位（!strict）、属主可信、≤64 KiB。
func (e CredEnv) readTrusted(path string, strict bool) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, errors.New("无法打开（符号链接或无权限均被拒绝）")
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("必须是普通文件")
	}
	if strict && st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("权限过宽（当前 %04o；应为 root:root 0400）", st.Mode().Perm())
	}
	if !strict && st.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("不得对 group/other 可写（当前 %04o）", st.Mode().Perm())
	}
	uid, _, ok := ownerIDs(st)
	if !ok {
		return nil, errors.New("无法确定属主")
	}
	trusted := false
	for _, a := range e.uids() {
		trusted = trusted || uid == a
	}
	if !trusted {
		return nil, errors.New("属主不可信（应为 root）")
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	if err != nil || len(b) > 64<<10 {
		return nil, errors.New("读取失败或文件过大")
	}
	return b, nil
}

func readSecretFile(path string, e CredEnv) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("口令文件必须是绝对路径")
	}
	b, err := e.readTrusted(path, true)
	if err != nil {
		return nil, err
	}
	b = []byte(strings.TrimRight(string(b), "\r\n"))
	if len(b) == 0 {
		return nil, errors.New("口令文件为空")
	}
	return b, nil
}

// ResolveDB 解析连接目标与迁移级口令。返回的 DBConn 由调用方用后 Close（清零口令）。
func ResolveDB(e CredEnv) (DBConn, error) {
	get := e.Getenv
	if get == nil {
		get = func(string) string { return "" }
	}
	var mf *migrateFile
	if e.MigrateYAML != "" {
		b, err := e.readTrusted(e.MigrateYAML, true)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if e.MigrateYAMLExplicit {
				return DBConn{}, fmt.Errorf("%s 指定的 migrate.yaml 不存在", EnvMigrateCfg)
			}
		case err != nil:
			return DBConn{}, fmt.Errorf("migrate.yaml 不可用：%w", err)
		default:
			var f migrateFile
			dec := yaml.NewDecoder(strings.NewReader(string(b)))
			dec.KnownFields(true)
			if derr := dec.Decode(&f); derr != nil {
				zero(b)
				return DBConn{}, errors.New("migrate.yaml 解析失败或含未知字段")
			}
			zero(b)
			mf = &f
		}
	}
	var sf *serverFile
	if e.ServerYAML != "" {
		b, err := e.readTrusted(e.ServerYAML, false)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return DBConn{}, fmt.Errorf("server.yaml 不可用：%w", err)
		default:
			var f serverFile
			if derr := yaml.Unmarshal(b, &f); derr != nil {
				return DBConn{}, errors.New("server.yaml 解析失败")
			}
			sf = &f
		}
	}

	// 目标：命令行 ＞ 环境变量 ＞ migrate.yaml ＞ server.yaml。host/port 与 socket 互斥，按来源整组选取。
	t := e.Explicit
	if t.Host == "" && t.Socket == "" {
		switch {
		case get(EnvHost) != "":
			t.Host = get(EnvHost)
			if p := get(EnvPort); p != "" && t.Port == 0 {
				n, err := strconv.Atoi(p)
				if err != nil {
					return DBConn{}, fmt.Errorf("%s 不是整数", EnvPort)
				}
				t.Port = n
			}
		case get(EnvSocket) != "":
			t.Socket = get(EnvSocket)
		}
	}
	if t.Name == "" {
		t.Name = get(EnvName)
	}
	if t.Host == "" && t.Socket == "" {
		switch {
		case mf != nil && (mf.Host != "" || mf.Socket != ""):
			t.Host, t.Port, t.Socket = mf.Host, mf.Port, mf.Socket
		case sf != nil && (sf.Database.Host != "" || sf.Database.Socket != ""):
			t.Host, t.Port, t.Socket = sf.Database.Host, sf.Database.Port, sf.Database.Socket
		}
	}
	if t.Host != "" && t.Socket == "" && t.Port == 0 {
		t.Port = 3306
	}
	if t.Name == "" {
		switch {
		case mf != nil && mf.Database != "":
			t.Name = mf.Database
		case sf != nil && sf.Database.Name != "":
			t.Name = sf.Database.Name
		}
	}
	if t.User == "" {
		switch {
		case get(EnvUser) != "":
			t.User = get(EnvUser)
		case mf != nil && mf.User != "":
			t.User = mf.User
		default:
			t.User = DefaultUser
		}
	}
	if t.TLS == "" {
		switch {
		case mf != nil && mf.TLS != "":
			t.TLS = mf.TLS
		case sf != nil && sf.Database.Socket == "" && sf.Database.TLS != "":
			t.TLS = sf.Database.TLS
		}
	}
	if t.Host == "" && t.Socket == "" || t.Name == "" {
		return DBConn{}, ErrNoTarget
	}
	if err := t.Validate(); err != nil {
		return DBConn{}, fmt.Errorf("数据库连接目标不合法：%w", err)
	}

	// 口令。
	var pw []byte
	switch {
	case get(EnvDBPass) != "" && get(EnvPassFile) != "":
		return DBConn{}, fmt.Errorf("%s 与 %s 只能设置其一", EnvDBPass, EnvPassFile)
	case get(EnvDBPass) != "":
		pw = []byte(get(EnvDBPass))
	case get(EnvPassFile) != "":
		b, err := readSecretFile(get(EnvPassFile), e)
		if err != nil {
			return DBConn{}, fmt.Errorf("%s: %w", EnvPassFile, err)
		}
		pw = b
	case mf != nil && mf.Password != "":
		pw = []byte(mf.Password)
	case e.ReadSecret != nil:
		s, err := e.ReadSecret("数据库迁移账号 " + t.User + " 的口令（无回显）: ")
		if err != nil || s == "" {
			return DBConn{}, errors.New("未读取到数据库口令")
		}
		pw = []byte(s)
	default:
		return DBConn{}, ErrNoCredentials
	}
	return DBConn{DBTarget: t, Password: pw}, nil
}

// ReadPassphraseFile 读取口令文件（绝对路径、O_NOFOLLOW、无 group/other 位、属主 root 或当前用户；去掉结尾换行）。
func ReadPassphraseFile(path string, allowedUIDs []uint32) ([]byte, error) {
	return readSecretFile(path, CredEnv{AllowedUIDs: allowedUIDs})
}
