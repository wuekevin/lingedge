// Package signingkeysit 是 Story 1.23（签名与 CA 密钥初始化，信任域②③）的集成测试：真实 signingkeys 服务 + 真实审计/认证/系统设置模块 + httpx 固定中间件链，
// 跑在测试自拉起的独立临时 mysqld（应用全部内嵌迁移、真实 lingedge_app 账号）与独立临时 redis-server 之上（tests/apistack）。没有任何 Mock；
// 主密钥、口令、令牌均在运行期随机生成（G-6：仓库内无固定口令）；本机常驻 mysqld/redis 不受影响。
package signingkeysit

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func TestMain(m *testing.M) {
	os.Exit(redistest.MainWith(m, mysqltest.Main))
}
