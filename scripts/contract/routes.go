package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// 运行时路由清单（Story 1.12 AC2/AC7）：lingedge-server 的路由注册层（internal/platform/httpx）导出的 JSON，
// 本文件据此校验「运行时路由集合 ⊆ openapi.yaml」、元数据与 x-* 一致，并产出「M1 未实现 operation 清单」。
// 清单格式见 httpx.ManifestEntry（字段名即 JSON 键）。

type routeEntry struct {
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
}

func readRoutes(path string) ([]routeEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rs []routeEntry
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rs); err != nil {
		return nil, fmt.Errorf("解析路由清单 %s: %w", path, err)
	}
	return rs, nil
}

// checkPathParamNames：同一 HTTP 方法下、同一路径位置的路径参数名必须一致。Gin（httprouter 基数树，每个方法一棵树）
// 要求同一位置只有一个通配符名，否则注册时 panic（如 GET /dns/zones/{dnsZoneId} 与 GET /dns/zones/{zoneId}/records 冲突）。
func checkPathParamNames(doc *openapi3.T) []finding {
	if doc.Paths == nil {
		return nil
	}
	type seen struct{ name, path string }
	at := map[string]seen{}
	var fs []finding
	reported := map[string]bool{}
	for _, o := range operations(doc) { // 已按路径、方法稳定排序
		segs := strings.Split(strings.TrimPrefix(o.Path, "/"), "/")
		prefix := o.Method + " "
		for _, s := range segs {
			if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
				name := s[1 : len(s)-1]
				if prev, ok := at[prefix]; ok && prev.name != name {
					key := o.Path + "|" + prev.path
					if !reported[key] {
						reported[key] = true
						fs = append(fs, finding{"path-param-name", o.Path, fmt.Sprintf("%s 的路径参数 {%s} 与 %s 在同一位置的 {%s} 不一致（Gin 同位置只允许一个通配符名，注册会 panic）", o.Method, name, prev.path, prev.name)})
					}
				} else if !ok {
					at[prefix] = seen{name, o.Path}
				}
				prefix += "/{}"
			} else {
				prefix += "/" + s
			}
		}
	}
	return fs
}

// checkRoutes 校验运行时路由清单 ⊆ OpenAPI 且元数据一致；返回违规与 M1 未实现清单。
func checkRoutes(doc *openapi3.T, routes []routeEntry) (fs []finding, unimplemented []string) {
	ops := map[string]opRef{}
	for _, o := range operations(doc) {
		ops[o.Op.OperationID] = o
	}
	have := map[string]bool{}
	seen := map[string]string{}
	for _, r := range routes {
		where := r.OperationID
		if strings.Contains(r.Path, ":") { // IMP-21 的注册层门禁（独立于 OpenAPI 文件本身的检查）
			fs = append(fs, finding{"route-path-colon", where, fmt.Sprintf("运行时路由路径含 ':': %s（IMP-21）", r.Path)})
		}
		if prev, dup := seen[r.OperationID]; dup {
			fs = append(fs, finding{"route-duplicate", where, "operationId 在路由清单中重复（" + prev + "）"})
		}
		seen[r.OperationID] = r.Method + " " + r.Path
		have[r.OperationID] = true
		o, ok := ops[r.OperationID]
		if !ok {
			fs = append(fs, finding{"route-not-in-openapi", where, fmt.Sprintf("运行时路由 %s %s 的 operationId 不在 openapi.yaml 中（路由集合必须 ⊆ 契约）", r.Method, r.Path)})
			continue
		}
		diff := func(field string, got, want any) {
			fs = append(fs, finding{"route-meta-mismatch", where, fmt.Sprintf("%s 与 openapi.yaml 不一致（代码 %v，契约 %v）", field, got, want)})
		}
		if r.Method != o.Method {
			diff("Method", r.Method, o.Method)
		}
		if r.Path != o.Path {
			diff("Path", r.Path, o.Path)
		}
		var perms []string
		if v, ok := o.Op.Extensions["x-permissions"].([]any); ok {
			for _, p := range v {
				if s, ok := p.(string); ok {
					perms = append(perms, s)
				}
			}
		}
		got := append([]string{}, r.Permissions...)
		sort.Strings(got)
		sort.Strings(perms)
		if strings.Join(got, ",") != strings.Join(perms, ",") {
			diff("Permissions", got, perms)
		}
		if s, _ := extString(o.Op, "x-tenant-scope"); r.TenantScope != s {
			diff("TenantScope", r.TenantScope, s)
		}
		if b, _ := extBool(o.Op, "x-audit"); r.Audit != b {
			diff("Audit", r.Audit, b)
		}
		if b, _ := extBool(o.Op, "x-reauth"); r.Reauth != b {
			diff("Reauth", r.Reauth, b)
		}
		if s, _ := extString(o.Op, "x-phase"); r.Phase != s {
			diff("Phase", r.Phase, s)
		}
		public := o.Op.Security != nil && len(*o.Op.Security) == 0
		if (r.Auth == "public") != public {
			diff("Auth", r.Auth, map[bool]string{true: "public", false: "required"}[public])
		}
		idem := false
		for _, p := range o.Op.Parameters {
			if p.Value != nil && p.Value.In == "header" && p.Value.Name == "Idempotency-Key" {
				idem = true
			}
		}
		if r.Idempotent != idem {
			diff("Idempotent", r.Idempotent, idem)
		}
	}
	for _, o := range operations(doc) {
		if s, _ := extString(o.Op, "x-phase"); s == "M1" && !have[o.Op.OperationID] {
			unimplemented = append(unimplemented, fmt.Sprintf("%s (%s %s)", o.Op.OperationID, o.Method, o.Path))
		}
	}
	sort.Strings(unimplemented)
	return fs, unimplemented
}

// checkIdempotencyResponses：声明了 Idempotency-Key 参数的 operation 必须声明 422 idempotency.key_reuse（引用
// components/responses/IdempotencyKeyReuse），反之 422 只允许出现在这些 operation 上（ADR-017 §3/§4：校验统一 400，422 是唯一例外）。
func checkIdempotencyResponses(doc *openapi3.T) []finding {
	var fs []finding
	for _, o := range operations(doc) {
		idem := false
		for _, p := range o.Op.Parameters {
			if p.Value != nil && p.Value.In == "header" && p.Value.Name == "Idempotency-Key" {
				idem = true
			}
		}
		var r422 *openapi3.ResponseRef
		if o.Op.Responses != nil {
			r422 = o.Op.Responses.Status(422)
		}
		switch {
		case idem && r422 == nil:
			fs = append(fs, finding{"idempotency-422", o.String(), "声明了 Idempotency-Key 却没有 422 idempotency.key_reuse 响应"})
		case idem && r422.Ref != "#/components/responses/IdempotencyKeyReuse":
			fs = append(fs, finding{"idempotency-422", o.String(), "422 响应必须引用 #/components/responses/IdempotencyKeyReuse（实际 " + r422.Ref + "）"})
		case !idem && r422 != nil:
			fs = append(fs, finding{"idempotency-422", o.String(), "未声明 Idempotency-Key 却声明了 422（校验失败统一 400，422 仅用于幂等键异体复用）"})
		}
	}
	return fs
}
