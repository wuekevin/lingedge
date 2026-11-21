package httpx

import (
	"encoding/json"
	"sort"
)

// ManifestEntry 是路由清单（routes manifest）里的一项：`make openapi-check` 据此校验「运行时路由集合 ⊆ openapi.yaml」
// 与元数据和 x-* 的一致性，并输出「M1 未实现 operation 清单」。字段名即 JSON 键，scripts/contract 读取同一格式。
type ManifestEntry struct {
	OperationID string   `json:"operation_id"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Auth        string   `json:"auth"`
	Permissions []string `json:"permissions"`
	TenantScope string   `json:"tenant_scope"`
	Audit       bool     `json:"audit"`
	Reauth      bool     `json:"reauth"`
	Idempotent  bool     `json:"idempotent"`
	Phase       string   `json:"phase"`
	Feature     string   `json:"feature,omitempty"`
}

// Manifest 返回已注册路由的清单（按 operationId 升序）。
func (r *Router) Manifest() []ManifestEntry {
	routes := r.Routes()
	out := make([]ManifestEntry, 0, len(routes))
	for _, m := range routes {
		perms := append([]string{}, m.Permissions...)
		sort.Strings(perms)
		out = append(out, ManifestEntry{
			OperationID: m.OperationID, Method: m.Method, Path: m.Path, Auth: string(m.Auth), Permissions: perms,
			TenantScope: string(m.TenantScope), Audit: m.Audit, Reauth: m.Reauth, Idempotent: m.Idempotent, Phase: m.Phase, Feature: m.Feature,
		})
	}
	return out
}

// ManifestJSON 把清单编码为缩进 JSON。
func (r *Router) ManifestJSON() ([]byte, error) {
	b, err := json.MarshalIndent(r.Manifest(), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
