package contracttest

import (
	"fmt"
	"sort"

	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// 审计覆盖检查（Story 1.21 AC1、FR-23；make audit-check）——对**真实注册的路由集合**：
//
//   - 写 operation（POST/PUT/PATCH/DELETE）必须声明 Audit（x-audit: true），除非在例外清单（scripts/gates/data/audit-exempt.txt，带理由）里登记：
//     「写 operation 缺 Audit 声明」；
//   - 声明了 Audit 的 operation 必须在 audit.Catalog 登记审计动作：「声明 Audit 但没有记录者」（Catalog 里的动作是否真有 Record 调用由 `gates audit` 静态核对，
//     每个 operation 的审计行是否真的写入由各业务集成测试与审计钩子的运行期兜底核对）。
//
// 例外清单按 operationId 登记；这里只使用它的键（理由与过期核对由 `gates audit` 负责）。

// CheckAuditCoverage 返回全部问题（空表示通过）。registered 报告 operationId 是否在审计目录登记（生产传 audit.Registered）。
func CheckAuditCoverage(routes []httpx.RouteMeta, exempt map[string]string, registered func(operationID string) bool) []string {
	var problems []string
	write := func(m string) bool { return m == "POST" || m == "PUT" || m == "PATCH" || m == "DELETE" }
	for _, m := range routes {
		_, isExempt := exempt[m.OperationID]
		switch {
		case write(m.Method) && !m.Audit && !isExempt:
			problems = append(problems, fmt.Sprintf("%s %s %s：写 operation 没有声明 Audit（x-audit: true）；若确实不产生持久变更，须在 audit-exempt.txt 登记并写明理由", m.OperationID, m.Method, m.Path))
		case m.Audit && !registered(m.OperationID):
			problems = append(problems, fmt.Sprintf("%s %s %s：声明了 Audit，却没有在 audit.Catalog 登记审计动作（没有记录者）", m.OperationID, m.Method, m.Path))
		}
	}
	if len(routes) == 0 {
		problems = append(problems, "路由集合为空：审计覆盖检查不能空转通过")
	}
	sort.Strings(problems)
	return problems
}

// CheckAuditCoverageDefault 用生产的审计目录（audit.Registered）做检查。
func CheckAuditCoverageDefault(routes []httpx.RouteMeta, exempt map[string]string) []string {
	return CheckAuditCoverage(routes, exempt, audit.Registered)
}
