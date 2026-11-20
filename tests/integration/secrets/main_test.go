// Package secretsintegration 是 secretbox.RecordStore 真实 MySQL 实现的端到端测试（Story 1.11 遗留项）：
// 独立临时 mysqld 实例（tests/mysqltest）上应用全部内嵌迁移（000001–000013），以真实 lingedge_app 账号运行，
// 加解密一律走真实的 Box/FileKeyProvider，库一律是真实 MySQL，没有任何 Mock。
package secretsintegration

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
)

func TestMain(m *testing.M) {
	os.Exit(mysqltest.Main(m))
}
