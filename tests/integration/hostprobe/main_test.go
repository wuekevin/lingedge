// Package hostprobeit 是 Story 1.31（系统与宝塔只读探测）的集成测试：probe 对**真实临时 mysqld 与 redis-server**（tests/mysqltest、tests/redistest，
// 独立端口与目录，本机常驻实例不受影响）做版本/连通性/认证探测，并证明探测期间服务器端只执行了只读命令；再把探测结果直接喂给真实的 resolver。
// 口令均在测试内运行期随机生成（G-6：仓库内无固定口令）。
package hostprobeit

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func TestMain(m *testing.M) { os.Exit(redistest.MainWith(m, mysqltest.Main)) }
