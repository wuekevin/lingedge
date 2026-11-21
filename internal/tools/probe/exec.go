package probe

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

// 本包创建进程的唯一入口是 readOnlyRun：safeexec.Runner（白名单、固定目录解析、参数数组、清洗环境、输出/超时上限）之上，
// 再加一道「只读 argv 表」——只有下表列出的程序与参数形态才会被执行，任何写入/控制类子命令（systemctl start/stop/…）在这里就被拒绝。

// ErrNotReadOnly 表示请求的命令不在只读表内。
var ErrNotReadOnly = errors.New("probe: 命令不在只读白名单内")

// ProbeAllowed 是本包 Runner 的可执行文件白名单。
var ProbeAllowed = []string{"ss", "systemctl", "nginx", "openresty", "mysqld", "mariadbd", "mysql", "redis-server", "node"}

// ProbeSearchDirs 是裸名的固定解析目录（与 safeexec 默认一致，不读取调用方 PATH）。
var ProbeSearchDirs = safeexec.DefaultSearchDirs

// NewRunner 构造探测用的 safeexec.Runner。dirs 为空取 ProbeSearchDirs（测试可传入本机真实程序所在目录，如 /opt/homebrew/bin）。
func NewRunner(dirs []string) (*safeexec.Runner, error) {
	if len(dirs) == 0 {
		dirs = ProbeSearchDirs
	}
	return safeexec.New(safeexec.Options{Allowed: ProbeAllowed, SearchDirs: dirs, MaxOutput: 4 << 20, Timeout: 15 * time.Second})
}

// 只读 argv 表：键为程序基名；值为允许的完整参数列表。
var readOnlyArgv = map[string][][]string{
	"ss":           {{"-H", "-lntup"}},
	"systemctl":    {{"list-units", "--type=service", "--all", "--no-legend", "--no-pager", "--plain"}},
	"nginx":        {{"-v"}},
	"openresty":    {{"-v"}},
	"mysqld":       {{"--version"}},
	"mariadbd":     {{"--version"}},
	"redis-server": {{"--version"}},
	"node":         {{"--version"}},
}

func argvAllowed(name string, args []string) bool {
	if name == "mysql" { // 认证探测的 argv 含端点与用户名，按形状校验而不是枚举
		return mysqlArgvAllowed(args)
	}
	for _, a := range readOnlyArgv[name] {
		if len(a) == len(args) {
			same := true
			for i := range a {
				if a[i] != args[i] {
					same = false
				}
			}
			if same {
				return true
			}
		}
	}
	return false
}

// readOnlyRun 执行只读命令。path 为白名单裸名或基名在白名单内的绝对路径。返回 stdout 与 stderr 的合并文本（nginx -v 写 stderr）。
func readOnlyRun(ctx context.Context, r *safeexec.Runner, path string, args []string, env ...string) (stdout, stderr []byte, err error) {
	name := filepath.Base(path)
	if !argvAllowed(name, args) {
		return nil, nil, fmt.Errorf("%w: %s %s", ErrNotReadOnly, name, security.RedactString(strings.Join(args, " ")))
	}
	res, err := r.Run(ctx, safeexec.Cmd{Path: path, Args: args, Env: env})
	return res.Stdout, res.Stderr, err
}

// VersionExec 实现 baota.Exec：经 safeexec 取版本。
type VersionExec struct{ Runner *safeexec.Runner }

// Version 执行 `<exe> -v|--version` 并解析版本。kind：nginx | mysqld | redis-server | node。
func (v VersionExec) Version(ctx context.Context, kind, exe string) (version, flavor string, err error) {
	var args []string
	switch kind {
	case "nginx":
		args = []string{"-v"}
	case "mysqld", "redis-server", "node":
		args = []string{"--version"}
	default:
		return "", "", fmt.Errorf("%w: kind=%s", ErrNotReadOnly, kind)
	}
	so, se, err := readOnlyRun(ctx, v.Runner, exe, args)
	if err != nil {
		return "", "", err
	}
	out := string(so) + "\n" + string(se)
	switch kind {
	case "nginx":
		version, flavor = ParseNginxV(out)
	case "mysqld":
		version, flavor = ParseMysqlVersion(out)
	case "redis-server":
		version = ParseRedisServerVersion(out)
		flavor = "redis"
	case "node":
		version = ParseNodeVersion(out)
		flavor = "nodejs"
	}
	if version == "" {
		return "", "", fmt.Errorf("probe: 无法从 %s 的输出解析版本", filepath.Base(exe))
	}
	return version, flavor, nil
}

var (
	reNginxV   = regexp.MustCompile(`(?m)^nginx version:\s*(nginx|openresty|tengine|angie)/(\S+)`)
	reMysqlVer = regexp.MustCompile(`(?m)\bVer\s+(\d[0-9A-Za-z.+_~-]*)`)
	reDistrib  = regexp.MustCompile(`\bDistrib\s+(\d[0-9A-Za-z.+_~-]*)`)
	reRedisV   = regexp.MustCompile(`\bv=(\d+(?:\.\d+){1,3}\S*)`)
	reRedisI   = regexp.MustCompile(`(?m)^redis_version:(\S+)`)
	reNodeV    = regexp.MustCompile(`(?m)^v(\d+\.\d+\.\d+\S*)\s*$`)
)

// ParseNginxV 解析 `nginx -v` 的输出（stderr）：`nginx version: nginx/1.24.0 (Ubuntu)`、`nginx version: openresty/1.21.4.1`。
func ParseNginxV(out string) (version, flavor string) {
	if m := reNginxV.FindStringSubmatch(out); m != nil {
		return m[2], m[1]
	}
	return "", ""
}

// ParseMysqlVersion 解析 `mysqld --version` / `mysql --version`：
//
//	/usr/sbin/mysqld  Ver 8.0.36-0ubuntu0.22.04.1 for Linux on x86_64 ((Ubuntu))
//	mariadbd  Ver 10.11.6-MariaDB-0+deb12u1 for debian-linux-gnu on x86_64
//	mysql  Ver 15.1 Distrib 10.5.23-MariaDB, for debian-linux-gnu (x86_64)   （MariaDB 客户端：版本取 Distrib）
//
// flavor：mysql | mariadb。
func ParseMysqlVersion(out string) (version, flavor string) {
	flavor = "mysql"
	if strings.Contains(strings.ToLower(out), "mariadb") {
		flavor = "mariadb"
	}
	if m := reDistrib.FindStringSubmatch(out); m != nil {
		return strings.TrimSuffix(m[1], ","), flavor
	}
	if m := reMysqlVer.FindStringSubmatch(out); m != nil {
		return m[1], flavor
	}
	return "", ""
}

// ParseRedisServerVersion 解析 `redis-server --version`（`Redis server v=7.0.15 sha=00000000:0 …`）。
func ParseRedisServerVersion(out string) string {
	if m := reRedisV.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// ParseNodeVersion 解析 `node --version`（`v20.11.0`），保持原样的 v 前缀由 resolver 归一；这里返回带 v 的原始串。
func ParseNodeVersion(out string) string {
	if m := reNodeV.FindStringSubmatch(out); m != nil {
		return "v" + m[1]
	}
	return ""
}
