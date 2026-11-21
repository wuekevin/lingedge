package probe

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Unix socket 端点：存在性检查、属主信任判定、候选发现。全部只读（Lstat/Stat/读 /proc/net/unix），不连接、不发送任何数据。

// DefaultMySQLSockets 是 MySQL/MariaDB 常见 socket 位置（含宝塔 /www/server/mysql/mysql.sock）。
var DefaultMySQLSockets = []string{
	"/tmp/mysql.sock",
	"/var/lib/mysql/mysql.sock",
	"/www/server/mysql/mysql.sock",
	"/run/mysqld/mysqld.sock",
	"/var/run/mysqld/mysqld.sock",
}

// DefaultRedisSockets 是 Redis 常见 socket 位置。
var DefaultRedisSockets = []string{
	"/var/run/redis/redis.sock",
	"/run/redis/redis.sock",
	"/var/run/redis/redis-server.sock",
	"/run/redis/redis-server.sock",
	"/tmp/redis.sock",
	"/www/server/redis/redis.sock",
}

// maxSocketPath 取 macOS 的 sun_path 上限（104 含结尾 NUL），Linux 为 108。
const maxSocketPath = 103

var reSocketPath = regexp.MustCompile(`^/[A-Za-z0-9._+@/-]+$`)

// ValidateSocketPath 要求规范的绝对路径、限定字符集（不含空白与 shell 元字符、不以 - 开头的参数形态）、长度不超过 sun_path。
func ValidateSocketPath(p string) error {
	switch {
	case !reSocketPath.MatchString(p) || path.Clean(p) != p:
		return errors.New("probe: socket 路径必须是规范的绝对路径且只含 [A-Za-z0-9._+@/-]")
	case len(p) > maxSocketPath:
		return fmt.Errorf("probe: socket 路径过长（%d > %d）", len(p), maxSocketPath)
	}
	return nil
}

// SocketInfo 是对一个 unix socket 路径的只读检查结果。
type SocketInfo struct {
	Path     string // 声明的路径
	Resolved string // 符号链接解析后的真实路径（无链接时等于 Path）
	UID      int    // 属主 uid；未知为 -1
	Trusted  bool   // 属主是 root、当前用户，或 mysql/mariadb/redis 服务账号——只有受信的 socket 才会被发送凭据
}

// trustedSocketUsers 是被信任为「真实数据库服务」属主的账号名。
var trustedSocketUsers = map[string]bool{"mysql": true, "mariadb": true, "redis": true}

// socketUserDB 与 euidFunc 是 uid → 用户名的库与「当前用户」来源；包内变量，测试可替换。
var (
	socketUserDB = "/etc/passwd"
	euidFunc     = os.Geteuid
)

// CheckSocket 用 Lstat 检查 p：必须存在且是 socket。符号链接（如 /tmp/mysql.sock → /var/lib/mysql/mysql.sock）会解析一次，
// 解析结果也必须是 socket（不会「跟随到非 socket」）。不连接、不读写。
func CheckSocket(p string) (SocketInfo, error) {
	info := SocketInfo{Path: p, UID: -1}
	if err := ValidateSocketPath(p); err != nil {
		return info, err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return info, err
	}
	info.Resolved = p
	if fi.Mode()&os.ModeSymlink != 0 {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return info, fmt.Errorf("probe: %s 是无法解析的符号链接: %w", p, err)
		}
		if fi, err = os.Lstat(real); err != nil {
			return info, err
		}
		info.Resolved = real
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return info, fmt.Errorf("probe: %s 存在但不是 socket（%s）", p, fi.Mode().Type())
	}
	if uid, ok := fileOwnerUID(fi); ok {
		info.UID = uid
		info.Trusted = uid == 0 || uid == euidFunc() || trustedSocketUsers[uidName(uid)]
	}
	return info, nil
}

func uidName(uid int) string {
	b, err := os.ReadFile(socketUserDB)
	if err != nil {
		return ""
	}
	return ParsePasswd(b)[uid]
}

var (
	reMySQLSockName = regexp.MustCompile(`^(mysqld?|mariadb)(-\d+)?\.sock$`)
	reRedisSockName = regexp.MustCompile(`^redis(-server)?([_-]\d+)?\.sock$`)
)

// ParseProcNetUnix 解析 /proc/net/unix，返回处于监听状态的流式 socket 的路径（跳过抽象命名空间 @ 与无路径的）。
//
//	Num       RefCount Protocol Flags    Type St Inode Path
//	ffff…: 00000002 00000000 00010000 0001 01 21344 /run/mysqld/mysqld.sock
//
// 监听 = Flags 含 SO_ACCEPTCON(0x00010000) 且 Type=0001（SOCK_STREAM）。
func ParseProcNetUnix(b []byte) []string {
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || strings.HasPrefix(f[0], "Num") {
			continue
		}
		var flags int64
		if _, err := fmt.Sscanf(f[3], "%x", &flags); err != nil || flags&0x10000 == 0 || f[4] != "0001" {
			continue
		}
		p := strings.Join(f[7:], " ") // 路径可含空格
		if !strings.HasPrefix(p, "/") || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= 4096 {
			break
		}
	}
	return out
}

// discoverSockets 返回候选 socket：默认位置 + /proc/net/unix 里监听的、名字像 MySQL/Redis socket 的路径（去重、保持确定顺序）。
func discoverSockets(defaults []string, procRoot string, re *regexp.Regexp) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] && ValidateSocketPath(p) == nil {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range defaults {
		add(p)
	}
	if procRoot != "" {
		if b, err := os.ReadFile(filepath.Join(procRoot, "net", "unix")); err == nil {
			for _, p := range ParseProcNetUnix(b) {
				if re.MatchString(path.Base(p)) {
					add(p)
				}
			}
		}
	}
	return out
}
