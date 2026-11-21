// Package setupit 是 Story 1.24（Server setup mode 与 Web 安装向导后端）的集成测试：真实 setup 服务 + 真实 HTTP 处理器（BuildHandler）+ 真实迁移（全部 18 个）
// + 真实认证/审计/系统设置/签名密钥模块，跑在测试自拉起的独立临时 mysqld（log_bin=ON、log_bin_trust_function_creators=0，生产形态）与独立临时 redis-server 之上。
// 没有任何 Mock；口令、令牌、主密钥全部在运行期随机生成（G-6：仓库内无固定口令）；本机常驻 mysqld/redis 不受影响。
//
// 「临时数据库管理员」用一个测试实例里新建的全权限账号（GRANT ALL ON *.* WITH GRANT OPTION）扮演——等价于生产里的 root/宝塔管理员；实例的 root 本身没有口令且只允许 socket，
// 无法按向导的方式（TCP + 口令）连接，改写它的口令会破坏同包其他用例。三类账号是全局对象，所以本包的用例串行执行（不使用 t.Parallel），每个用例前后清理它们。
package setupit

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func TestMain(m *testing.M) {
	os.Exit(redistest.MainWith(m, mysqltest.Main))
}
