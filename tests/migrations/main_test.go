package migrationstest

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// 临时 mysqld 实例由 mysqltest 在首次使用时启动，Main 返回前关闭并删除 datadir（不触碰本机常驻 MySQL）。
func TestMain(m *testing.M) {
	os.Exit(mysqltest.Main(m))
}
