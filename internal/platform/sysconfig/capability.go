package sysconfig

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// 能力注册表（Story 1.22、FR-31、NFR-27）：代码常量，**模块交付时登记**——前端据 /auth/me 的 features 裁剪菜单，不自行猜测；
// 未交付的模块不出现在 modules_delivered 里，高级能力在独立技术验证前恒为 unverified。

// 能力状态。
const (
	// StatusUnverified：尚未经独立技术验证（PoC/基线）——不得宣称可用（NFR-27）。
	StatusUnverified = "unverified"
	// StatusAvailable 仅用于「试图置为 available 被拒绝」：本版本没有任何能力具备被验证为可用的依据。
	StatusAvailable = "available"
)

// AdvancedCapabilities 是高级能力（P2/P6，须独立技术验证）：状态恒为 unverified。
var AdvancedCapabilities = []string{"http3", "l2_cache", "dnssec", "edge_compute"}

// ModuleInfo 是一个已交付模块：Name 是前端使用的模块键；Paths 声明它拥有的 REST 路径（精确匹配，或匹配 `<前缀>/…` 子路径）。
// 登记表与真实路由集合的一致性由 cmd/lingedge-server 的 TestModulesDeliveredMatchRoutes 核对：每个已注册路由恰好属于一个已交付模块，
// 每个已交付模块至少有一条已注册路由、且其路径下 openapi 中 M1 的 operation 全部已注册（不虚报、不漏登）。
type ModuleInfo struct {
	Name  string
	Paths []string
}

// DeliveredModules 是已交付模块登记表。新模块交付（其路由注册进 registerRoutes）的同一个 Story 必须在此追加，否则一致性测试失败。
var DeliveredModules = []ModuleInfo{
	{Name: "auth", Paths: []string{"/auth", "/admin/users", "/admin/roles", "/admin/permissions"}}, // 1.16–1.19：认证、会话、TOTP、RBAC、用户/角色、API Token
	{Name: "tenants", Paths: []string{"/tenants"}},                                                 // 1.20
	{Name: "tasks", Paths: []string{"/tasks"}},                                                     // 1.14
	{Name: "audit", Paths: []string{"/audit-logs", "/admin/login-logs"}},                           // 1.21
	{Name: "system", Paths: []string{"/admin/settings", "/admin/system/run-mode", "/admin/system/version", "/admin/installations", "/public/site-info", "/public/version", "/admin/system/signing-keys", "/public/setup"}}, // 1.22；/admin/system/signing-keys 为 1.23（签名密钥元数据，归属系统模块）；/public/setup 为 1.24（安装向导，正常模式下恒 410）
}

// ModulesDelivered 返回已交付模块键（排序）。
func ModulesDelivered() []string {
	out := make([]string, 0, len(DeliveredModules))
	for _, m := range DeliveredModules {
		out = append(out, m.Name)
	}
	sort.Strings(out)
	return out
}

// OwnerOf 返回拥有该路由路径（OpenAPI 模板，不含 /api/v1）的已交付模块；没有则 ok=false。
func OwnerOf(path string) (string, bool) {
	for _, m := range DeliveredModules {
		for _, p := range m.Paths {
			if path == p || strings.HasPrefix(path, p+"/") {
				return m.Name, true
			}
		}
	}
	return "", false
}

// CapabilityStatus 返回全部高级能力的状态：**恒为 unverified**（不读数据库——库里即使被带外写入了 available 也不生效，NFR-27）。
func CapabilityStatus() map[string]string {
	m := make(map[string]string, len(AdvancedCapabilities))
	for _, c := range AdvancedCapabilities {
		m[c] = StatusUnverified
	}
	return m
}

// FeaturesView 是 /auth/me 的 features：前端据此裁剪菜单（FR-31）。
type FeaturesView struct {
	RunMode          string            `json:"run_mode"`
	ModulesDelivered []string          `json:"modules_delivered"`
	CapabilityStatus map[string]string `json:"capability_status"`
}

// Feature 是路由声明的功能开关取值（RouteMeta.Feature / x-feature）。
const FeatureCommercial = "commercial"

// FeatureEnabled 判定功能在运行模式下是否启用；未知功能一律未启用（fail-closed）。
func FeatureEnabled(runMode, feature string) bool {
	switch feature {
	case FeatureCommercial:
		return runMode == RunModeCommercial
	}
	return false
}

// CheckModules 核对能力注册表与真实路由集合（防虚报、防漏登）：
//   - 每个已注册路由恰好属于一个已交付模块（路由存在而模块未登记 = 漏登，前端会看不到这个模块）；
//   - 每个已交付模块至少拥有一条已注册路由（登记了却没有路由 = 虚报）；
//   - 模块拥有的路径下，契约里 x-phase=M1 的 operation 必须全部已注册（模块只交付一半就不能整体登记）。
//
// registered 是已注册路由；contract 是契约里的全部 operation（operationId、路径、阶段）。返回全部问题（空表示一致）。
func CheckModules(modules []ModuleInfo, registered []httpx.RouteMeta, contract []ContractOp) []string {
	var p []string
	owner := func(path string) []string {
		var names []string
		for _, m := range modules {
			for _, pp := range m.Paths {
				if path == pp || strings.HasPrefix(path, pp+"/") {
					names = append(names, m.Name)
					break
				}
			}
		}
		return names
	}
	have := map[string]bool{}
	routesOf := map[string]int{}
	for _, r := range registered {
		have[r.OperationID] = true
		switch o := owner(r.Path); len(o) {
		case 0:
			p = append(p, fmt.Sprintf("已注册路由 %s %s 不属于任何已交付模块（模块交付时必须在 sysconfig.DeliveredModules 登记）", r.Method, r.Path))
		case 1:
			routesOf[o[0]]++
		default:
			p = append(p, fmt.Sprintf("已注册路由 %s %s 同时属于多个模块 %v", r.Method, r.Path, o))
		}
	}
	for _, m := range modules {
		if routesOf[m.Name] == 0 {
			p = append(p, fmt.Sprintf("已交付模块 %q 没有任何已注册路由（虚报）", m.Name))
		}
	}
	for _, c := range contract {
		if c.Phase != "M1" || have[c.OperationID] {
			continue
		}
		if o := owner(c.Path); len(o) > 0 {
			p = append(p, fmt.Sprintf("模块 %v 声称拥有 %s（%s，M1）但该 operation 尚未注册：不能把只交付了一半的模块登记为已交付", o, c.Path, c.OperationID))
		}
	}
	sort.Strings(p)
	return p
}

// ContractOp 是契约里的一个 operation（CheckModules 的输入）。
type ContractOp struct{ OperationID, Path, Phase string }
