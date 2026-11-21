package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	"github.com/wuekevin/lingedge/internal/platform/server"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// TestAuditCoverage 是 Story 1.21 AC1 的 CI 门禁（make audit-check）：真实的路由集合（registerRoutes，与进程装配、lingedge-server routes 同一份）里，
// 写 operation 必须声明 Audit（例外须在 scripts/gates/data/audit-exempt.txt 登记并写明理由），声明了 Audit 的必须在 audit.Catalog 登记动作。
// 检查逻辑与负向样本见 internal/platform/httpx/contracttest（auditcoverage_test.go）；动作是否真有 Record 调用由 `gates audit` 静态核对。
func TestAuditCoverage(t *testing.T) {
	r, err := httpx.NewRouter(httpx.Options{IDs: uid.NewGenerator(clock.System()), ManifestOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := registerRoutes(r, server.Deps{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "gates", "data", "audit-exempt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	exempt := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			id, reason, _ := strings.Cut(l, " ")
			exempt[id] = reason
		}
	}
	if len(exempt) == 0 {
		t.Fatal("例外清单解析为空（格式变化？）")
	}
	for _, p := range contracttest.CheckAuditCoverageDefault(r.Routes(), exempt) {
		t.Error(p)
	}
}
