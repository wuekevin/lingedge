// lingedge-server 是 Server 单进程入口（Story 1.13）：按 --roles 装配 api/grpc/scheduler/worker，并提供运维子命令。
//
// 用法：
//
//	lingedge-server --config FILE [--roles=api,grpc,scheduler,worker]   运行 Server（systemd Type=notify；默认全开）
//	lingedge-server migrate plan|up|down|status|recover|lint ...         迁移框架（Story 1.6）
//	lingedge-server healthz [--config FILE | --addr HOST:PORT] [--ready] 探测本机 Server 的 /healthz（/healthz/ready）
//	lingedge-server lineage show|bump --config FILE                      部署谱系（ADR-002 §12；备份恢复后、启动 Server 前执行 bump）
//	lingedge-server routes [-o FILE]                                     导出 REST 路由清单（Story 1.12，make openapi-check 使用）
//	lingedge-server version | --version                                   版本信息
//
// `setup`（Web 安装向导 / setup mode）在 Story 1.24 之前**不注册**：现在运行 `lingedge-server setup` 与任何未知子命令一样以退出码 2 报错，
// 不假装可用。
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/pkg/version"
)

const name = "lingedge-server"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Environ()))
}

const usageText = `用法:
  lingedge-server --config FILE [--roles=api,grpc,scheduler,worker]
  lingedge-server <子命令> [选项]

子命令: migrate  healthz  lineage  routes  version
`

// run 是可测试的入口，返回进程退出码（0 成功；1 运行失败；2 用法错误）。
func run(args []string, stdout, stderr io.Writer, environ []string) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usageText)
		return 2
	}
	// 形如 --config/--roles/--version 的参数：运行 Server（systemd ExecStart 形态）。
	if strings.HasPrefix(args[0], "-") {
		if args[0] == "--version" || args[0] == "-version" {
			return version.CLI(name, args, stdout, stderr)
		}
		if args[0] == "-h" || args[0] == "--help" || args[0] == "-help" {
			_, _ = fmt.Fprint(stdout, usageText)
			return 0
		}
		return serveMain(args, stderr, environ)
	}
	switch args[0] {
	case "migrate":
		return migrate.Main(args[1:], stdout, stderr, environ, migrations.FS)
	case "routes":
		return routesMain(args[1:], stdout, stderr)
	case "healthz":
		return healthzMain(args[1:], stdout, stderr, environ)
	case "lineage":
		return lineageMain(args[1:], stdout, stderr, environ)
	case "version":
		_, _ = fmt.Fprintln(stdout, version.Info(name))
		return 0
	case "help":
		_, _ = fmt.Fprint(stdout, usageText)
		return 0
	}
	// 含 setup：Story 1.24 之前不注册。
	_, _ = fmt.Fprintf(stderr, "%s: 未知子命令 %q\n%s", name, args[0], usageText)
	return 2
}
