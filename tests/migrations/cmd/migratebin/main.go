// Command migratebin 是迁移测试台的辅助进程（仅 tests/migrations 使用）：与 lingedge-server 的 `migrate` 子命令是同一份实现
// （migrate.Main），区别只是迁移目录取自环境变量 LINGEDGE_TEST_MIGRATIONS_DIR，使测试能在迁移执行中途真正杀死一个独立进程
// （模拟崩溃/断电），而不污染生产二进制（生产二进制只执行嵌入的 migrations.FS，不接受任意目录）。
package main

import (
	"fmt"
	"os"

	"github.com/wuekevin/lingedge/internal/platform/migrate"
)

func main() {
	dir := os.Getenv("LINGEDGE_TEST_MIGRATIONS_DIR")
	if dir == "" {
		fmt.Fprintln(os.Stderr, "migratebin: 必须设置 LINGEDGE_TEST_MIGRATIONS_DIR")
		os.Exit(migrate.ExitUsage)
	}
	os.Exit(migrate.Main(os.Args[1:], os.Stdout, os.Stderr, os.Environ(), os.DirFS(dir)))
}
