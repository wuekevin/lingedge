package service

import (
	"context"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

// Runner 是本包创建外部进程的唯一入口，*safeexec.Runner 实现了它（Run 之外没有任何其他创建进程的路径：
// 全仓 forbidigo 禁止 os/exec，本包不 import 它）。
type Runner interface {
	Run(ctx context.Context, c safeexec.Cmd) (safeexec.Result, error)
}

// Allowed 是本包 Runner 的可执行文件白名单：systemctl / systemd-analyze（单元管理与校验）、
// id / getent（查询用户与组）、useradd / userdel（创建与撤销运行用户）。
// safeexec.DefaultAllowed 不含 getent，所以与 probe 一样构造自己的 Runner，而不是放宽 safeexec 的默认白名单。
var Allowed = []string{"systemctl", "systemd-analyze", "id", "getent", "useradd", "userdel"}

// NewRunner 构造经 safeexec 的 Runner。dirs 为空取 safeexec.DefaultSearchDirs（测试可传入放置伪二进制的目录）。
func NewRunner(dirs []string) (*safeexec.Runner, error) {
	if len(dirs) == 0 {
		dirs = safeexec.DefaultSearchDirs
	}
	return safeexec.New(safeexec.Options{Allowed: Allowed, SearchDirs: dirs, MaxOutput: 1 << 20, Timeout: 2 * time.Minute})
}

// SystemRunner 构造生产用 Runner（固定搜索目录）。
func SystemRunner() (*safeexec.Runner, error) { return NewRunner(nil) }

// WebServerAllowed 是静态前端安装（Story 1.40）创建 conf.d 软链后做 `nginx -t` 与 `nginx -s reload` 的可执行文件白名单。
// 与 Allowed 分开构造 Runner（而不是放宽 Allowed 或 safeexec.DefaultAllowed）：systemctl/useradd 的 Runner 看不到 nginx，反之亦然。
var WebServerAllowed = []string{"nginx", "openresty"}

// NewWebServerRunner 构造只允许 nginx/openresty 的 Runner。dirs 为空取 safeexec.DefaultSearchDirs。
func NewWebServerRunner(dirs []string) (*safeexec.Runner, error) {
	if len(dirs) == 0 {
		dirs = safeexec.DefaultSearchDirs
	}
	return safeexec.New(safeexec.Options{Allowed: WebServerAllowed, SearchDirs: dirs, MaxOutput: 1 << 20, Timeout: 2 * time.Minute})
}

// SystemWebServerRunner 构造生产用 Web 服务器 Runner（固定搜索目录）。
func SystemWebServerRunner() (*safeexec.Runner, error) { return NewWebServerRunner(nil) }
