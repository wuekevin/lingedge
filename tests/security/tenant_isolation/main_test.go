// Package tenantisolation 是多租户隔离矩阵（Story 1.20、ADR-009 §11、NFR-8、G-2）：资源 × 操作 × 身份（租户 A/B、平台 acting-as、API Token）。
//
// 矩阵的骨架由 docs/api/openapi.yaml 的 x-tenant-scope 自动生成（skeleton_test.go）；已注册的 tenant|either 路由必须在覆盖登记里
// 出现（coverage_test.go），**新增租户资源未登记则测试失败**（规则见 docs/testing/tenant-isolation-matrix.md）。矩阵跑在测试自拉起的独立临时
// mysqld 与 redis-server 上，用真实进程内装配（tests/apistack：真实认证/租户模块、真实钩子链）与真实会话/令牌驱动，没有任何 Mock。
// 口令与令牌均在运行期生成（G-6：无固定口令、无豁免）；本机常驻 mysqld/redis 不受影响。
package tenantisolation

import (
	"os"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func TestMain(m *testing.M) {
	os.Exit(redistest.MainWith(m, mysqltest.Main))
}
