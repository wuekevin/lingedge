// Command contract 是 LingEdge 的契约工具链（Story 1.3）：OpenAPI 静态规则与破坏性变更检测、
// proto 符号核对、迁移哈希清单、CF-1 冻结前置复核。
//
// 它是独立的 Go module（scripts/contract/go.mod），第三方依赖（kin-openapi）不进入仓库根 go.mod；
// 外部二进制工具（buf、oasdiff）一律由 Makefile 以 `go run <pkg>@<固定版本>` 提供。
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "openapi-check":
		code = cmdOpenAPICheck(os.Args[2:])
	case "openapi-breaking":
		code = cmdOpenAPIBreaking(os.Args[2:])
	case "migrations-check":
		code = cmdMigrationsCheck(os.Args[2:])
	case "migrations-seal":
		code = cmdMigrationsSeal(os.Args[2:])
	case "freeze-check":
		code = cmdFreezeCheck(os.Args[2:])
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法: contract <openapi-check|openapi-breaking|migrations-check|migrations-seal|freeze-check> [flags]")
}
