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
