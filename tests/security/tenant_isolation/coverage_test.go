package tenantisolation

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// 覆盖登记（Story 1.20 AC5）：每个已注册、x-tenant-scope 为 tenant|either 的路由都必须在这里登记它的隔离覆盖方式，否则
// TestEveryRegisteredTenantRouteIsCovered 失败——**此后每个租户资源 Story 必须在本文件扩充登记（并补驱动），否则 DoD 不过**。
// 规则全文见 docs/testing/tenant-isolation-matrix.md。

// Kind 是覆盖方式。
type Kind string

const (
	// Driven：矩阵里有该资源的功能性隔离驱动（drive* 函数），对 {租户 A、租户 B、平台 acting-as、API Token} 四类身份逐一断言：
	// 列表只含本租户、他租户 id → 404 与「不存在」逐字节一致、响应字段 R-T1、kin-openapi 响应契约。驱动登记它实际发出的 (operation, 身份) 对，
	// 缺少任何一类身份即失败（不能只声明不覆盖）。
	Driven Kind = "driven"
	// FailClosed：路由在其依赖能力交付前被钩子安全拒绝（handler 不执行）；矩阵断言任何身份调用都被拒绝且不改变数据。
	// 依赖能力交付后该路由必须转为 Driven（届时断言「被拒绝」的测试会失败，迫使转换）。Story 1.21 已把唯一的 FailClosed 登记（cancelTask）转为 Driven；
	// 该类型保留给之后可能出现的、依赖尚未交付能力的路由。
	FailClosed Kind = "fail-closed"
	// Exempt：不是「按租户列举/寻址的资源」（自助 operation 只作用于调用者自己；令牌管理只作用于自己名下的令牌）。必须写明理由，并引用已存在的
	// 专项测试（Refs 里的测试函数必须真实存在，否则失败）。
	Exempt Kind = "exempt"
)

// TestRef 引用一个已有测试函数（目录相对仓库根）。
type TestRef struct{ Dir, Func string }

// Coverage 是一个 operation 的覆盖登记。
type Coverage struct {
	Kind     Kind
	Resource string // Driven/FailClosed：资源名（驱动的分组名）
	Reason   string // FailClosed/Exempt：理由
	Refs     []TestRef
}

const authIT = "tests/integration/auth"

const selfServiceReason = "自助 operation：只作用于调用者自己的身份/会话/登录日志（user_id 取自会话而非请求参数，x-permissions 为空），Scope 由主体自身推导并忽略 X-Tenant-ID（httpx.WithSelfService），不是按租户列举或寻址的资源"
const tokenReason = "令牌管理只作用于调用者自己名下的令牌（自有资源授权路径，不接受代租户）：他租户/他人/平台的令牌与不存在的一样 404；只能用 Cookie 会话"

// Registry 是覆盖登记表。
var Registry = map[string]Coverage{
	// 租户上下文（Story 1.20）。
	"getCurrentTenant": {Kind: Driven, Resource: "tenants/current"},
	// 任务中心（Story 1.14 的真实租户资源，tasks.tenant_id）。
	"listTasks":      {Kind: Driven, Resource: "tasks"},
	"getTask":        {Kind: Driven, Resource: "tasks"},
	"listTaskEvents": {Kind: Driven, Resource: "tasks"},
	"cancelTask":     {Kind: Driven, Resource: "tasks"}, // Story 1.21：审计交付后由 FailClosed 转为 Driven（取消 + 审计同事务，四类身份逐一驱动）
	// 审计日志（Story 1.21）：audit_logs.tenant_id 是被操作对象所属租户；租户主体（含 acting-as）只见自己租户且字段裁剪。
	"listAuditLogs": {Kind: Driven, Resource: "audit-logs"},
	"getAuditLog":   {Kind: Driven, Resource: "audit-logs"},
	// 令牌管理（Story 1.19）。
	"createApiToken": {Kind: Exempt, Reason: tokenReason, Refs: []TestRef{{authIT, "TestTenantTokenIsolation"}, {authIT, "TestAPITokenManagementOwnershipAndList"}}},
	"getApiToken":    {Kind: Exempt, Reason: tokenReason, Refs: []TestRef{{authIT, "TestTenantTokenIsolation"}, {authIT, "TestAPITokenManagementOwnershipAndList"}}},
	"listApiTokens":  {Kind: Exempt, Reason: tokenReason, Refs: []TestRef{{authIT, "TestTenantTokenIsolation"}, {authIT, "TestAPITokenManagementOwnershipAndList"}}},
	"revokeApiToken": {Kind: Exempt, Reason: tokenReason, Refs: []TestRef{{authIT, "TestTenantTokenIsolation"}, {authIT, "TestAPITokenRevocationIsImmediate"}}},
	"rotateApiToken": {Kind: Exempt, Reason: tokenReason, Refs: []TestRef{{authIT, "TestTenantTokenIsolation"}, {authIT, "TestAPITokenRotationChain"}}},
	// 自助 operation（Story 1.16/1.17）。
	"getMe":           {Kind: Exempt, Reason: selfServiceReason, Refs: []TestRef{{authIT, "TestAuthorizationMatrixHTTP"}, {authIT, "TestLoginSuccessSessionAndCookie"}}},
	"getCsrfToken":    {Kind: Exempt, Reason: selfServiceReason, Refs: []TestRef{{authIT, "TestCSRF"}}},
	"logout":          {Kind: Exempt, Reason: selfServiceReason, Refs: []TestRef{{authIT, "TestLoginSuccessSessionAndCookie"}}},
	"changePassword":  {Kind: Exempt, Reason: selfServiceReason, Refs: []TestRef{{authIT, "TestChangePasswordRevokesAllSessions"}}},
	"listSessions":    {Kind: Exempt, Reason: selfServiceReason, Refs: []TestRef{{authIT, "TestSessionsListAndRevoke"}}},
	"revokeSession":   {Kind: Exempt, Reason: selfServiceReason, Refs: []TestRef{{authIT, "TestSessionsListAndRevoke"}}},
	"listMyLoginLogs": {Kind: Exempt, Reason: selfServiceReason, Refs: []TestRef{{authIT, "TestMyLoginLogsOnlyOwn"}}},
	"reauth":          {Kind: Exempt, Reason: selfServiceReason, Refs: []TestRef{{authIT, "TestReauthRefreshesSession"}}},
	"setupTotp":       {Kind: Exempt, Reason: selfServiceReason, Refs: []TestRef{{authIT, "TestTOTPSetupEnableEncryptsSecretAndIssuesRecoveryCodes"}}},
	"enableTotp":      {Kind: Exempt, Reason: selfServiceReason, Refs: []TestRef{{authIT, "TestTOTPSetupEnableEncryptsSecretAndIssuesRecoveryCodes"}}},
	"disableTotp":     {Kind: Exempt, Reason: selfServiceReason, Refs: []TestRef{{authIT, "TestDisableTOTP"}}},
}

// AuditCoverage 核对覆盖登记，返回全部问题（空表示通过）。registered 是已注册路由的元数据；refExists 判断引用的测试函数是否存在。
//   - 已注册且 x-tenant-scope 为 tenant|either 的路由必须有登记（新增租户资源未覆盖 → 失败）；
//   - 登记项必须对应已注册的 tenant|either 路由（陈旧/误登记 → 失败）；
//   - Driven 必须给出资源名；FailClosed/Exempt 必须给出理由，Exempt 必须至少引用一个存在的测试函数。
func AuditCoverage(registered []httpx.RouteMeta, reg map[string]Coverage, refExists func(TestRef) bool) []string {
	var p []string
	facing := map[string]httpx.RouteMeta{}
	for _, m := range registered {
		if m.Auth == httpx.AuthRequired && isTenantFacing(m.TenantScope) {
			facing[m.OperationID] = m
		}
	}
	for id, m := range facing {
		c, ok := reg[id]
		if !ok {
			p = append(p, fmt.Sprintf("%s %s %s（x-tenant-scope=%s）已注册但未在隔离矩阵登记：新增租户资源必须扩充 coverage_test.go 的 Registry（并补 Driven 驱动），见 docs/testing/tenant-isolation-matrix.md", id, m.Method, m.Path, m.TenantScope))
			continue
		}
		switch c.Kind {
		case Driven:
			if strings.TrimSpace(c.Resource) == "" {
				p = append(p, id+": Driven 登记必须给出资源名")
			}
		case FailClosed:
			if strings.TrimSpace(c.Reason) == "" || strings.TrimSpace(c.Resource) == "" {
				p = append(p, id+": FailClosed 登记必须给出资源名与理由")
			}
		case Exempt:
			if strings.TrimSpace(c.Reason) == "" {
				p = append(p, id+": Exempt 登记必须给出理由")
			}
			if len(c.Refs) == 0 {
				p = append(p, id+": Exempt 登记必须引用至少一个已有的专项测试")
			}
			for _, r := range c.Refs {
				if !refExists(r) {
					p = append(p, fmt.Sprintf("%s: 引用的测试 %s.%s 不存在", id, r.Dir, r.Func))
				}
			}
		default:
			p = append(p, fmt.Sprintf("%s: 未知登记类型 %q", id, c.Kind))
		}
	}
	for id := range reg {
		if _, ok := facing[id]; !ok {
			p = append(p, fmt.Sprintf("%s: 登记项没有对应的已注册 tenant|either 路由（陈旧或误登记；platform 路由不需要登记，由骨架行统一覆盖）", id))
		}
	}
	sort.Strings(p)
	return p
}

// testFuncExists 在 dir（相对仓库根）下的 *_test.go 里查找 `func <name>(`。
func testFuncExists(root string) func(TestRef) bool {
	return func(r TestRef) bool {
		re := regexp.MustCompile(`(?m)^func ` + regexp.QuoteMeta(r.Func) + `\(`)
		files, _ := filepath.Glob(filepath.Join(root, r.Dir, "*_test.go"))
		for _, f := range files {
			if b, err := os.ReadFile(f); err == nil && re.Match(b) {
				return true
			}
		}
		return false
	}
}
