package dbintegration

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// 临时 mysqld 实例由 mysqltest 在首次使用时启动，Main 返回前关闭并删除 datadir。
func TestMain(m *testing.M) {
	os.Exit(mysqltest.Main(m))
}
