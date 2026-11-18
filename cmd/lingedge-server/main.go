package main

import (
	"os"

	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/pkg/version"
)

func main() {
	// `lingedge-server migrate ...`：迁移框架入口（Story 1.6）；迁移文件嵌入在二进制里（migrations.FS）。
	// 其余子命令（roles、setup mode 等）随 Story 1.13 装配进来。
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		os.Exit(migrate.Main(os.Args[2:], os.Stdout, os.Stderr, os.Environ(), migrations.FS))
	}
	os.Exit(version.CLI("lingedge-server", os.Args[1:], os.Stdout, os.Stderr))
}
