package main

// 本文件是模块依赖方向的唯一机器可读来源，逐条对应：
//   - docs/architecture/architecture.md §5.3 依赖图及其规则 1–5；
//   - docs/architecture/adr/ADR-001-modular-monolith.md §3（模块契约 = 根包）、§4（依赖方向）、§5（ports 清单）、§7（独立二进制边界）。
// .golangci.yml 中的 depguard 规则由本表经 `gates gen-lint` 机械生成，不设「临时豁免」；
// 依赖图变更须先改 ADR-001，再改本表，并重新生成。

const modulePath = "github.com/wuekevin/lingedge"

// serverModules 为 Server 内的业务/平台模块（基线 §20 的 internal/ 目录，不含 tools）。
var serverModules = []string{
	"platform",
	"auth", "tenant", "plugin", // L1
	"website", "origin", "cache", "ruleengine", "certificate", "waf", "security",
	"l4proxy", "dns", "billing", "cms", "seo", "media", "monitoring", "notification", // L2
	"node", "cluster", "scheduler", "reporter", // L3
}

// allowedDeps：模块 → 允许直接 import 的其他模块根包（architecture §5.3 图中的箭头）。
// 所有模块另外都可依赖 platform（含 platform/ports）与 pkg；platform 自身不依赖任何业务模块。
// 图中没有的边一律禁止；L2 之间互不依赖，除 origin/cache/ruleengine/certificate/waf/security → website（站点归属校验）；
// website、l4proxy、dns、billing → tenant；reporter → monitoring；node/cluster 不得 import 任何 L2 模块。
//
// 新增模块/二进制/ports 接口的步骤：先修订 ADR-001 与 architecture §5.3，再改本文件，运行 `make gen-lint`
// 重新生成 .golangci.yml，并同步 gates_test.go 中钉住的依赖图副本（防止规格被无意改动）。
var allowedDeps = map[string][]string{
	"platform":     nil,
	"auth":         nil,
	"tenant":       nil,
	"plugin":       nil,
	"website":      {"tenant"},
	"origin":       {"website"},
	"cache":        {"website"},
	"ruleengine":   {"website"},
	"certificate":  {"website"},
	"waf":          {"website"},
	"security":     {"website"},
	"l4proxy":      {"tenant"},
	"dns":          {"tenant"},
	"billing":      {"tenant"},
	"cms":          nil,
	"seo":          nil,
	"media":        nil,
	"monitoring":   nil,
	"notification": nil,
	"node":         nil,
	"cluster":      nil,
	"scheduler":    nil,
	"reporter":     {"monitoring"},
}

// 独立二进制：只依赖 pkg/、proto/、edge/，不得 import internal/*（ADR-001 §7、architecture §5.3 规则 4）。
var standaloneBinaries = []string{"lingedge-node", "lingedge-dns", "lingedge-reporter"}

// Server 装配类二进制：可 import 全部 Server 模块，但与 internal/tools 无编译依赖。
var serverBinaries = []string{"lingedge-server", "lingedge-scheduler", "lingedge-worker"}

// Tools CLI：可 import internal/tools 与 pkg，不得 import Server 业务模块（architecture §5.3 规则 5）。
const toolsBinary = "lingedge"

// 模块内实现包：仅允许 internal/<mod>/internal/{handler,service,repository,domain}（ADR-001 §3），
// testdouble 为 ADR-023 的测试替身包（须带 lingedge_testdouble 构建标签，由 testdouble 门禁检查）。
var implPackages = map[string]bool{"handler": true, "service": true, "repository": true, "domain": true, "testdouble": true}

// 模块根包允许的非测试 Go 文件：契约仅 api.go；doc.go 仅承载包注释。
var rootAllowedFiles = map[string]bool{"api.go": true, "doc.go": true}

// platform/ports：仅接口与 DTO、无实现（ADR-001 §5 ports 清单）。
// 此处只列出允许出现的接口名；各接口的方法集由其所属 Story 定义，清单变更须评审。
var portInterfaces = map[string]bool{
	"CertificateProvider": true,
	"SiteDirectory":       true,
	"NodeDirectory":       true,
	"NodeTaskDispatcher":  true,
	"OriginHealthSink":    true,
	"RollupSink":          true,
	"LogSink":             true,
	"LogQuery":            true,
	"OriginGroupLookup":   true,
	"CachePolicyLookup":   true,
	"PlanCatalog":         true,
}

func isServerModule(name string) bool {
	for _, m := range serverModules {
		if m == name {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- 数据库访问与租户 Scope 的静态边界（Story 1.5 AC5/AC6、ADR-009 §6、ADR-001 §9）----
//
// 以下白名单是「谁可以做什么」的唯一来源，.golangci.yml 中的相应 depguard/forbidigo 规则据此生成；
// 变更须先改 ADR-009/ADR-016 并评审，再改本表并 `make gen-lint`。

// repositoryPathRegex 匹配 Repository 实现包的路径（相对仓库根，以 / 分隔）：
// internal/<模块>/internal/repository（ADR-001 §3 布局），以及 platform 下自带 repository 子包的基础设施。
const repositoryPathRegex = `^internal/(?:[^/]+/)*repository(?:/|$)`

// sqlImportGlobsAllowed 是允许 import jmoiron/sqlx、database/sql、go-sql-driver/mysql 的位置（depguard 的 files 排除项，
// ${base-path} 由生成器补上）：db 基础设施包与全部 repository 包；测试与 tests/、scripts/ 另行豁免。
var sqlImportGlobsAllowed = []string{"internal/platform/db/**", "internal/**/repository/**"}

// sqlImportDenied 是上述位置之外禁止 import 的包（AC5：Repository 之外不得引用 *sqlx.DB/*sqlx.Tx）。
var sqlImportDenied = []struct{ pkg, why string }{
	{"github.com/jmoiron/sqlx", "只有 internal/platform/db 与 repository 包可以 import sqlx（*sqlx.DB/*sqlx.Tx 不得出现在 Repository 之外；ADR-009 §6、ADR-001 §9）"},
	{"database/sql", "只有 internal/platform/db 与 repository 包可以 import database/sql；Service/Handler 经 Repository 与 db.WithTx 访问数据库，用 db.ErrNotFound/db.Result 代替 sql 类型"},
	{"github.com/go-sql-driver/mysql", "只有 internal/platform/db 与 repository 包可以 import MySQL 驱动；需要错误号时用 db.MySQLErrorNumber/db.IsDuplicate"},
}

// 受限池构造函数的调用白名单（AC6）：路径正则（相对仓库根）。请求路径持有 maint/migrate 池即违规。
const (
	// maint 池：仅 retention.cleanup / partition.maintain（调度/Worker 模块与其装配入口）。
	maintPoolMarker = "[db-pool-maint]"
	maintPoolPaths  = `^(?:internal/platform/db/|internal/scheduler/|cmd/lingedge-(?:server|worker|scheduler)/)`
	// migrate 池：仅 `lingedge-server migrate` 与向导迁移步骤（迁移框架包与 Server 入口）。
	migratePoolMarker = "[db-pool-migrate]"
	migratePoolPaths  = `^(?:internal/platform/db/|internal/platform/migrate/|cmd/lingedge-server/)`
)

// Scope 构造入口（tenantscope.FromPrincipal/FromTask/PlatformForJob/NewContext）的调用白名单（AC2、ADR-009 §1、§8）。
// 已知边界：internal/platform/ 整体在白名单内（中间件与任务运行时将落在其下，Story 1.12/1.14/1.20 落地后应收窄到具体子包）；
// PlatformForJob 的「登记」目前是路径级的，调用点清单随 1.14/1.15 登记：
// Handler/Service/Repository 只能经 FromContext 取得中间件放入 context 的 Scope，不能自己造。
const (
	scopeMintMarker = "[tenantscope-mint]"
	scopeMintPaths  = `^(?:internal/platform/|internal/tenant/|internal/scheduler/|cmd/lingedge-(?:server|worker|scheduler)/)`
)

// sqlConcatMarker：Repository 包内禁止用格式化/字符串函数构造 SQL（NFR-9）。运行期拼接的 SQL 本来就无法通过 db.Q 的编译，
// 这条 forbidigo 规则是第二道闸，并给出明确的诊断。
const sqlConcatMarker = "[no-sql-concat]"

var sqlConcatForbid = []string{
	`^fmt\.(Sprintf|Sprint|Sprintln)$`,
	`^strings\.(Join|Replace|ReplaceAll|NewReplacer|Builder)$`,
}

// scopeTypeImports 是 Scope 类型可能出现的包路径（别名包与本体包）。
var scopeTypeImports = []string{
	modulePath + "/internal/platform/scope",
	modulePath + "/internal/platform/tenantscope",
}
