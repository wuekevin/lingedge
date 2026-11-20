// Package httpxintegration 是 Story 1.12 的端到端集成测试：REST 骨架（路由元数据 / 钩子链 / 幂等 / 健康检查）跑在
// 测试自拉起的独立临时 mysqld（tests/mysqltest，应用全部内嵌迁移 000001–000013、真实 lingedge_app/lingedge_migrate 账号）
// 与独立临时 redis-server（tests/redistest）之上；没有任何 Mock，用后清理。本机常驻 mysqld/redis 不受影响。
package httpxintegration

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func TestMain(m *testing.M) {
	os.Exit(redistest.MainWith(m, mysqltest.Main))
}
