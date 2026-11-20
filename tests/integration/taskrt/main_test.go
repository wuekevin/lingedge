// Package taskrtintegration 是 Story 1.14 的集成测试：任务运行时跑在测试自拉起的独立临时 mysqld（tests/mysqltest，全部内嵌迁移、真实
// lingedge_app/lingedge_migrate 账号）与独立临时 redis-server（tests/redistest）之上，没有任何 Mock。
//
// 覆盖：并发领取恰好一次、租约续约、kill -9 真实 Worker 子进程后的回收、SIGSTOP 僵尸 Worker 的围栏拒绝、Redis 唤醒丢失后的轮询兜底、
// 白名单/幂等入队/payload 脱敏、协作式取消、任务中心 API（租户 Scope、越权 404、202+Location、OpenAPI 契约）。
// 本机常驻 mysqld/redis 不受影响（临时实例随测试清理）。
package taskrtintegration

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

// workerBin 是 TestMain 之后首次使用时构建的 workerproc 二进制（与生产同一份 taskrt.Worker）。
var workerBin string

func TestMain(m *testing.M) {
	os.Exit(redistest.MainWith(m, func(m *testing.M) int { return mysqltest.Main(m) }))
}

func buildWorker(t testing.TB) string {
	t.Helper()
	if workerBin != "" {
		return workerBin
	}
	dir, err := os.MkdirTemp("", "lewp")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "workerproc")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", bin, "./tests/integration/taskrt/workerproc")
	cmd.Dir = filepath.Join("..", "..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建 workerproc 失败: %v\n%s", err, out)
	}
	workerBin = bin
	return bin
}
