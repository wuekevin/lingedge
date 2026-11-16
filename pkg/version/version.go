// Package version 提供各二进制共享的构建版本信息与 --version 入口。
package version

import (
	"fmt"
	"io"
	"runtime"
)

// 由 Makefile 通过 -ldflags -X 注入；未注入时保持下列真实的「未知」取值。
var (
	Version = "0.0.0-dev"
	Commit  = "unknown"
)

// Info 返回 "<name> <version> (commit <commit>, <go>, <os>/<arch>)"。
func Info(name string) string {
	return fmt.Sprintf("%s %s (commit %s, %s, %s/%s)",
		name, Version, Commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// CLI 是脚手架阶段各 cmd 的唯一行为：仅支持 --version，返回进程退出码。
func CLI(name string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-version") {
		if _, err := fmt.Fprintln(stdout, Info(name)); err != nil {
			return 1
		}
		return 0
	}
	// 写 stderr 失败时无处可报，退出码语义不变。
	_, _ = fmt.Fprintf(stderr, "%s: 业务功能尚未交付，当前仅支持 --version\n", name)
	return 2
}
