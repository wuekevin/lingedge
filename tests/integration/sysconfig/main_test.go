// Package sysconfigit 是 Story 1.22（系统设置、运行模式开关、能力注册表）的端到端集成测试：真实 sysconfig 模块 + 真实认证/租户/任务/审计模块 +
// httpx 固定中间件链（含功能开关）+ 真实钩子装配，跑在测试自拉起的独立临时 mysqld（应用全部内嵌迁移、真实 lingedge_app 账号）与独立临时
// redis-server 之上（tests/apistack）。没有任何 Mock；口令、令牌、密钥值均在测试内运行期生成（G-6：仓库内无固定口令）；本机常驻 mysqld/redis 不受影响。
package sysconfigit

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func TestMain(m *testing.M) {
	os.Exit(redistest.MainWith(m, mysqltest.Main))
}
