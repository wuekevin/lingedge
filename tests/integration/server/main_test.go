// Package serverintegration 是 Story 1.13 的集成测试：lingedge-server 进程装配跑在测试自拉起的独立临时 mysqld（tests/mysqltest，
// 应用全部内嵌迁移 000001–000014、真实 lingedge_app/lingedge_migrate 账号）与独立临时 redis-server（tests/redistest）之上。
//
// 包含：真实子进程（`go build` 出的 lingedge-server 二进制）的启动/就绪通知/SIGTERM 优雅停机/启动拒绝，进程内 roles 矩阵，
// `lineage show|bump` 的真实 MySQL 测试，迁移版本探针（app 账号只读 schema_migrations）。没有任何 Mock；本机常驻 mysqld/redis 不受影响。
package serverintegration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

// serverBin 是 TestMain 构建出的真实 lingedge-server 二进制（与生产构建同一入口，不带 lingedge_testdouble 标签）。
var serverBin string

func TestMain(m *testing.M) {
	os.Exit(redistest.MainWith(m, func(m *testing.M) int {
		return mysqltest.Main(m)
	}))
}

// buildServer 首次使用时构建二进制（只构建一次）；构建失败直接让测试失败。
func buildServer(t testing.TB) string {
	t.Helper()
	if serverBin != "" {
		return serverBin
	}
	dir, err := os.MkdirTemp("", "lesrv")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "lingedge-server")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", bin, "./cmd/lingedge-server")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建 lingedge-server 失败: %v\n%s", err, out)
	}
	serverBin = bin
	return bin
}

func repoRoot(t testing.TB) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(wd, "..", "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal(fmt.Errorf("找不到仓库根: %w", err))
	}
	return root
}
