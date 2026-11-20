// Package authit 是 Story 1.16（认证 I：登录 / 会话 / CSRF / 登录节流 / 登录日志）的端到端集成测试：
// 真实的认证模块 + httpx 固定中间件链 + 真实钩子装配（httpx.WithSelfService），跑在测试自拉起的独立临时 mysqld
// （tests/mysqltest，应用全部内嵌迁移 000001–000017、真实 lingedge_app 账号）与独立临时 redis-server（tests/redistest）之上。
// 没有任何 Mock；用后清理；本机常驻 mysqld/redis 不受影响。口令在测试内随机生成并用真实 argon2id 哈希（G-6：仓库内无固定口令）。
package authit

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func TestMain(m *testing.M) {
	os.Exit(redistest.MainWith(m, mysqltest.Main))
}
