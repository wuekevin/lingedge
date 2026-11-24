// Package toolsinstall 是 Story 1.35 的联调：真实的 `lingedge install server`（cli.Execute → installer → 生产 server ComponentSpec）+ 本机构建的真实
// lingedge-server 二进制 + 本机临时 MySQL/Redis（tests/mysqltest、tests/redistest，ADR-026）+ 真实向导 HTTP 流程。
//
// 真实：Tools 的全部业务逻辑（状态机/journal/冲突/回滚、server.yaml/master.key/setup-token 生成、生产 Health 的 HTTP GET /healthz）、
// lingedge-server 进程（setup mode → 向导 → install.lock → 同进程切换正常模式）、MySQL、Redis、文件系统权限。
// 伪的：「操作系统」——伪 systemctl/useradd/chown（tests/toolsworld，根目录前缀到临时目录；本测试非 root）；伪 systemd 不会 fork 进程，
// 所以测试在生产 Start（伪 systemctl start）之后，按**已安装的真实单元文件里的 ExecStart/LoadCredential** 启动真实二进制（路径前缀到临时根；
// CREDENTIALS_DIRECTORY 由测试按 LoadCredential 的语义准备）。真实 systemd 下的行为（User=lingedge、ProtectSystem=strict、LoadCredential 真实注入）属 ENV-LNX，见 Story 1.35 文档。
package toolsinstall

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func TestMain(m *testing.M) {
	code := redistest.MainWith(m, mysqltest.Main)
	cleanupBuilt() // 删除本包构建的 lingedge-server 二进制（带测试迁移的 overlay 构建每个约 40 MB）
	os.Exit(code)
}
