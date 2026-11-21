package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// httpMethods 是 OpenAPI Path Item 中可承载 operation 的方法。
var httpMethods = []string{"GET", "PUT", "POST", "DELETE", "OPTIONS", "HEAD", "PATCH", "TRACE"}

// requiredExtensions 是每个 operation 必须声明的扩展字段（FR-20、NFR-22）。
var requiredExtensions = []string{"x-permissions", "x-tenant-scope", "x-audit", "x-phase"}

// opRef 把 operation 与其路径、方法绑在一起，便于报告。
type opRef struct {
	Path   string
	Method string
	Op     *openapi3.Operation
	Item   *openapi3.PathItem
}

func (o opRef) String() string { return o.Method + " " + o.Path }

// operations 按路径、方法稳定排序返回全部 operation。
func operations(doc *openapi3.T) []opRef {
	var out []opRef
	if doc.Paths == nil {
		return nil
	}
	for path, item := range doc.Paths.Map() {
		for _, m := range httpMethods {
			if op := item.GetOperation(m); op != nil {
				out = append(out, opRef{Path: path, Method: m, Op: op, Item: item})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// loadDoc 解析并校验 OpenAPI 文档；返回前先确认声明的是 3.1.x。
func loadDoc(data []byte, fromPath string) (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromData(data)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", fromPath, err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1.") {
		return nil, fmt.Errorf("%s 声明的 openapi 版本为 %q，须为 3.1.x", fromPath, doc.OpenAPI)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("%s 不是合法的 OpenAPI 3.1: %w", fromPath, err)
	}
	return doc, nil
}

// extString 读取字符串型扩展；缺失或类型不符返回 ok=false。
func extString(op *openapi3.Operation, key string) (string, bool) {
	v, ok := op.Extensions[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func extBool(op *openapi3.Operation, key string) (bool, bool) {
	v, ok := op.Extensions[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// finding 是一条规则违规。
type finding struct {
	Rule  string
	Where string
	Msg   string
}

func (f finding) String() string { return fmt.Sprintf("[%s] %s: %s", f.Rule, f.Where, f.Msg) }

var tenantScopes = map[string]bool{"platform": true, "tenant": true, "either": true, "public": true}

// checkMetadata：每个 operation 具备 operationId 与四个扩展，且取值合法；operationId 全局唯一（AC1）。
func checkMetadata(doc *openapi3.T) []finding {
	var fs []finding
	seen := map[string]string{}
	for _, o := range operations(doc) {
		if o.Op.OperationID == "" {
			fs = append(fs, finding{"metadata", o.String(), "缺少 operationId"})
		} else if prev, dup := seen[o.Op.OperationID]; dup {
			fs = append(fs, finding{"metadata", o.String(), fmt.Sprintf("operationId %q 与 %s 重复", o.Op.OperationID, prev)})
		} else {
			seen[o.Op.OperationID] = o.String()
		}
		for _, k := range requiredExtensions {
			if _, ok := o.Op.Extensions[k]; !ok {
				fs = append(fs, finding{"metadata", o.String(), "缺少 " + k})
			}
		}
		if v, ok := o.Op.Extensions["x-permissions"]; ok {
			if _, isList := v.([]any); !isList {
				fs = append(fs, finding{"metadata", o.String(), "x-permissions 必须是列表"})
			}
		}
		if s, ok := o.Op.Extensions["x-tenant-scope"]; ok {
			if str, isStr := s.(string); !isStr || !tenantScopes[str] {
				fs = append(fs, finding{"metadata", o.String(), fmt.Sprintf("x-tenant-scope 取值非法: %v（应为 platform|tenant|either|public）", s)})
			}
		}
		if _, ok := o.Op.Extensions["x-audit"]; ok {
			if _, isBool := extBool(o.Op, "x-audit"); !isBool {
				fs = append(fs, finding{"metadata", o.String(), "x-audit 必须是布尔值"})
			}
		}
		if _, ok := o.Op.Extensions["x-phase"]; ok {
			if s, isStr := extString(o.Op, "x-phase"); !isStr || s == "" {
				fs = append(fs, finding{"metadata", o.String(), "x-phase 必须是非空字符串"})
			}
		}
	}
	return fs
}

// checkNoColonPaths：路径不得含 ':'（IMP-21：Gin 把冒号后当通配参数）。
func checkNoColonPaths(doc *openapi3.T) []finding {
	var fs []finding
	if doc.Paths == nil {
		return nil
	}
	for path := range doc.Paths.Map() {
		if strings.Contains(path, ":") {
			fs = append(fs, finding{"path-colon", path, "路径含 ':'（IMP-21）"})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].Where < fs[j].Where })
	return fs
}

// R-T1：租户可见 schema 不得含 node/cluster 内部标识（BLK-02；docs/api/README.md §2：`node_*`、`cluster_*`、`egress_node*`、`targets`）。
// 聚合计数（summary.healthy 等）允许。同时拦截驼峰写法（nodeId、clusterUid）。
var tenantLeakRe = regexp.MustCompile(`^(node_.*|cluster_.*|egress_node.*|targets|(node|cluster|egressNode)[A-Z].*)$`)

// leakyName 判断属性/参数名是否属于 R-T1 禁止的内部标识。
func leakyName(name string) bool { return tenantLeakRe.MatchString(name) }

// child 是 schema 的一个子 schema；Prop 非空表示它是名为 Prop 的属性。
type child struct {
	Prop string
	Ref  *openapi3.SchemaRef
	Path string
}

// children 枚举 schema 的全部子 schema（含 OpenAPI 3.1 / JSON Schema 2020-12 的关键字），防止扫描器漏掉藏在
// prefixItems、patternProperties、if/then/else、dependentSchemas、contains、propertyNames、unevaluated*、contentSchema 里的字段。
func children(s *openapi3.Schema) []child {
	var out []child
	named := func(m openapi3.Schemas, prefix string, isProp bool) {
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			c := child{Ref: m[n], Path: prefix + n}
			if isProp {
				c.Prop = n
			}
			out = append(out, c)
		}
	}
	list := func(refs openapi3.SchemaRefs, label string) {
		for i, r := range refs {
			out = append(out, child{Ref: r, Path: fmt.Sprintf("%s[%d]", label, i)})
		}
	}
	one := func(r *openapi3.SchemaRef, label string) {
		if r != nil {
			out = append(out, child{Ref: r, Path: label})
		}
	}
	named(s.Properties, ".", true)
	named(s.PatternProperties, "/patternProperties/", false)
	named(s.DependentSchemas, "/dependentSchemas/", false)
	named(s.Defs, "/$defs/", false)
	one(s.Items, "[]")
	one(s.AdditionalProperties.Schema, "{}")
	list(s.AllOf, "/allOf")
	list(s.OneOf, "/oneOf")
	list(s.AnyOf, "/anyOf")
	list(s.PrefixItems, "/prefixItems")
	one(s.Not, "/not")
	one(s.Contains, "/contains")
	one(s.PropertyNames, "/propertyNames")
	one(s.If, "/if")
	one(s.Then, "/then")
	one(s.Else, "/else")
	one(s.ContentSchema, "/contentSchema")
	one(s.UnevaluatedItems.Schema, "/unevaluatedItems")
	one(s.UnevaluatedProperties.Schema, "/unevaluatedProperties")
	return out
}

// walker 递归跟随 $ref 遍历 schema，ref 级去重防止环。
type walker struct {
	seen  map[string]bool
	visit func(path string, name string)
}

func (w *walker) schema(ref *openapi3.SchemaRef, path string) {
	if ref == nil {
		return
	}
	if ref.Ref != "" {
		if w.seen[ref.Ref] {
			return
		}
		w.seen[ref.Ref] = true
	}
	if ref.Value == nil {
		return
	}
	for _, c := range children(ref.Value) {
		if c.Prop != "" {
			w.visit(path, c.Prop)
		}
		w.schema(c.Ref, path+c.Path)
	}
}

type schemaRoot struct {
	where string
	ref   *openapi3.SchemaRef
}

// operationParams 返回 operation 生效的全部参数（路径级 + 操作级）。
func operationParams(o opRef) openapi3.Parameters {
	var ps openapi3.Parameters
	if o.Item != nil {
		ps = append(ps, o.Item.Parameters...)
	}
	return append(ps, o.Op.Parameters...)
}

// operationSchemas 返回 operation 请求/响应涉及的全部 schema 根（含路径级与操作级参数的 schema/content）；
// paramName 回调收到每个参数名。
func operationSchemas(o opRef, paramName func(where, name string)) []schemaRoot {
	var out []schemaRoot
	add := func(where string, ref *openapi3.SchemaRef) { out = append(out, schemaRoot{where, ref}) }
	for _, p := range operationParams(o) {
		if p == nil || p.Value == nil {
			continue
		}
		if paramName != nil {
			paramName(o.String()+" 参数", p.Value.Name)
		}
		add(o.String()+" 参数 "+p.Value.Name, p.Value.Schema)
		for mt, m := range p.Value.Content {
			add(o.String()+" 参数 "+p.Value.Name+" "+mt, m.Schema)
		}
	}
	if rb := o.Op.RequestBody; rb != nil && rb.Value != nil {
		for mt, m := range rb.Value.Content {
			add(o.String()+" 请求体 "+mt, m.Schema)
		}
	}
	if o.Op.Responses != nil {
		for code, r := range o.Op.Responses.Map() {
			if r == nil || r.Value == nil {
				continue
			}
			for mt, m := range r.Value.Content {
				add(o.String()+" 响应 "+code+" "+mt, m.Schema)
			}
		}
	}
	return out
}

// checkTenantLeak：x-tenant-scope 为 tenant|either 的 operation，请求/响应 schema（递归跟随 $ref）与参数名
// 不得含 node/cluster 内部标识（AC1②、R-T1）。
func checkTenantLeak(doc *openapi3.T) []finding {
	var fs []finding
	for _, o := range operations(doc) {
		scope, _ := extString(o.Op, "x-tenant-scope")
		if scope != "tenant" && scope != "either" {
			continue
		}
		reported := map[string]bool{}
		report := func(where, name string) {
			key := where + "|" + name
			if !reported[key] {
				reported[key] = true
				fs = append(fs, finding{"tenant-leak", where, fmt.Sprintf("租户可见契约含内部标识 %q（R-T1）", name)})
			}
		}
		w := &walker{seen: map[string]bool{}}
		var cur string
		w.visit = func(_ string, name string) {
			if leakyName(name) {
				report(cur, name)
			}
		}
		roots := operationSchemas(o, func(where, name string) {
			if leakyName(name) {
				report(where, name)
			}
		})
		for _, r := range roots {
			cur = r.where
			w.schema(r.ref, "")
		}
	}
	return fs
}

// reauthOps 返回声明 x-reauth: true 的 operationId 集合（排序）。
func reauthOps(doc *openapi3.T) []string {
	var ids []string
	for _, o := range operations(doc) {
		if b, ok := extBool(o.Op, "x-reauth"); ok && b {
			ids = append(ids, o.Op.OperationID)
		}
	}
	sort.Strings(ids)
	return ids
}

// diffSets 返回 want 与 got 的差异（sorted）：missing 在 want 不在 got；extra 在 got 不在 want。
func diffSets(want, got []string) (missing, extra []string) {
	w, g := map[string]bool{}, map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	for _, s := range got {
		g[s] = true
	}
	for s := range w {
		if !g[s] {
			missing = append(missing, s)
		}
	}
	for s := range g {
		if !w[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return
}

// checkReauthSet：x-reauth 集合由 CI 固定（AC1③、IMP-12）。基线文件逐行一个 operationId，改动须经评审与变更说明。
func checkReauthSet(doc *openapi3.T, pinned []string) []finding {
	missing, extra := diffSets(pinned, reauthOps(doc))
	var fs []finding
	for _, id := range missing {
		fs = append(fs, finding{"x-reauth", id, "基线要求 x-reauth: true，契约中已缺失或被取消（降低再认证要求须走 ADR）"})
	}
	for _, id := range extra {
		fs = append(fs, finding{"x-reauth", id, "契约新增 x-reauth: true，但未登记到固定集合 scripts/contract/data/x-reauth.txt"})
	}
	return fs
}

// commercialTag 与 commercialOps 定义「商业类 operation」（FR-24）：billing 标签下的全部 operation，加上不在 billing 标签下的商业入口
// （租户自助注册、官网套餐展示、商业模式创建租户）。它们必须声明 `x-feature: commercial`——自用模式下运行时 403 feature.disabled。
// 新增商业类入口（不带 billing 标签）时须把 operationId 加到这里，否则「未标注就不受保护」。
const commercialTag = "billing"

var commercialOps = []string{"register", "listPublicPlans", "createTenant"}

// knownFeatures 是 x-feature 的合法取值（运行时由 sysconfig.FeatureEnabled 解释；未知功能一律视为未启用）。
var knownFeatures = map[string]bool{"commercial": true}

// checkFeatureTags（Story 1.22）：商业类 operation 必须声明 x-feature: commercial；x-feature 取值必须已知（清单陈旧由真实契约的单测 TestFeatureTags 核对）。
func checkFeatureTags(doc *openapi3.T, ids []string) []finding {
	var fs []finding
	explicit := map[string]bool{}
	for _, id := range ids {
		explicit[id] = true
	}
	for _, o := range operations(doc) {
		raw, present := o.Op.Extensions["x-feature"]
		feat, isStr := raw.(string)
		if present && (!isStr || !knownFeatures[feat]) {
			fs = append(fs, finding{"x-feature", o.String(), fmt.Sprintf("x-feature 取值 %v 非法（已知：commercial）", raw)})
			continue
		}
		commercial := explicit[o.Op.OperationID]
		for _, t := range o.Op.Tags {
			if t == commercialTag {
				commercial = true
			}
		}
		if commercial && feat != "commercial" {
			fs = append(fs, finding{"x-feature", o.String(), "商业类 operation（billing 标签或商业入口清单）必须声明 x-feature: commercial，否则自用模式下不受 feature.disabled 保护"})
		}
	}
	return fs
}

// listResponseSchemas 返回 operation 全部 2xx JSON 响应 schema（媒体类型按前缀 application/json 匹配，如带 charset）。
func listResponseSchemas(op *openapi3.Operation) []*openapi3.Schema {
	var out []*openapi3.Schema
	if op.Responses == nil {
		return nil
	}
	for code, r := range op.Responses.Map() {
		if !strings.HasPrefix(code, "2") || r == nil || r.Value == nil {
			continue
		}
		for mt, m := range r.Value.Content {
			if strings.HasPrefix(strings.ToLower(mt), "application/json") && m.Schema != nil && m.Schema.Value != nil {
				out = append(out, m.Schema.Value)
			}
		}
	}
	return out
}

// allProperties 把 schema 的直接属性与 allOf 组合出的属性合并（浅层，足够识别 {items,...} 形态）。
func allProperties(s *openapi3.Schema, depth int) map[string]*openapi3.SchemaRef {
	out := map[string]*openapi3.SchemaRef{}
	if s == nil || depth > 6 {
		return out
	}
	for k, v := range s.Properties {
		out[k] = v
	}
	for _, sub := range s.AllOf {
		for k, v := range allProperties(sub.Value, depth+1) {
			out[k] = v
		}
	}
	return out
}

// listCollectionKeys 是列表响应中承载集合的属性名；顶层数组同样视为 list。
var listCollectionKeys = []string{"items", "data", "records", "results"}

// isListOperation：GET 且任一 2xx JSON 响应体为数组，或为含集合属性（items/data/records/results）数组的对象（FR-20 的 list 类 operation）。
func isListOperation(o opRef) bool {
	if o.Method != "GET" {
		return false
	}
	isArr := func(r *openapi3.SchemaRef) bool {
		return r != nil && r.Value != nil && r.Value.Type != nil && r.Value.Type.Includes("array")
	}
	for _, s := range listResponseSchemas(o.Op) {
		if s.Type != nil && s.Type.Includes("array") {
			return true
		}
		props := allProperties(s, 0)
		for _, k := range listCollectionKeys {
			if isArr(props[k]) {
				return true
			}
		}
	}
	return false
}

// queryParamNames 汇总 operation 的 query 参数名（已解析 $ref；路径级参数一并计入）。
func queryParamNames(o opRef) map[string]bool {
	names := map[string]bool{}
	for _, p := range operationParams(o) {
		if p != nil && p.Value != nil && p.Value.In == "query" {
			names[p.Value.Name] = true
		}
	}
	return names
}

// checkListPagination：list 类 operation 必须声明分页参数——页码式 page+page_size 或游标式 cursor+limit（AC8、FR-20）。
func checkListPagination(doc *openapi3.T) []finding {
	var fs []finding
	for _, o := range operations(doc) {
		if !isListOperation(o) {
			continue
		}
		q := queryParamNames(o)
		pageStyle := q["page"] && q["page_size"]
		cursorStyle := q["cursor"] && q["limit"]
		if !pageStyle && !cursorStyle {
			fs = append(fs, finding{"list-pagination", o.String(), "list 类 operation 未声明分页参数（page+page_size 或 cursor+limit）"})
		}
	}
	return fs
}

// internalIDName 判断属性/参数名是否可能是内部自增主键：`id` 或 `*_id`（uid 形态的 `*_uid` 不算）。
func internalIDName(name string) bool { return name == "id" || strings.HasSuffix(name, "_id") }

// isIntegerSchema：整型/数值，或声明了 int32/int64 格式（即便 type 写成了 string）。
func isIntegerSchema(ref *openapi3.SchemaRef) bool {
	if ref == nil || ref.Value == nil {
		return false
	}
	v := ref.Value
	if v.Format == "int64" || v.Format == "int32" {
		return true
	}
	return v.Type != nil && (v.Type.Includes("integer") || v.Type.Includes("number"))
}

// checkNoInternalID：对外 schema 不得暴露内部整型 id（L22）。扫描 components.schemas 全部 schema（递归含 3.1 关键字）、
// operation 内联 schema，以及整型的 `id`/`*_id` 参数。资源标识一律 uid（ULID）。
func checkNoInternalID(doc *openapi3.T) []finding {
	var fs []finding
	scan := func(root *openapi3.SchemaRef, where string) {
		seen := map[*openapi3.Schema]bool{}
		var rec func(ref *openapi3.SchemaRef, path string)
		rec = func(ref *openapi3.SchemaRef, path string) {
			if ref == nil || ref.Value == nil || seen[ref.Value] {
				return
			}
			seen[ref.Value] = true
			for _, c := range children(ref.Value) {
				if c.Prop != "" && internalIDName(c.Prop) && isIntegerSchema(c.Ref) {
					fs = append(fs, finding{"internal-id", where + path, fmt.Sprintf("对外 schema 暴露内部整型标识 %q（L22，应使用 uid）", c.Prop)})
				}
				rec(c.Ref, path+c.Path)
			}
		}
		rec(root, "")
	}
	names := make([]string, 0)
	if doc.Components != nil {
		for n := range doc.Components.Schemas {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		scan(doc.Components.Schemas[n], "#/components/schemas/"+n)
	}
	for _, o := range operations(doc) {
		for _, p := range operationParams(o) {
			if p != nil && p.Value != nil && internalIDName(p.Value.Name) && isIntegerSchema(p.Value.Schema) {
				fs = append(fs, finding{"internal-id", o.String() + " 参数 " + p.Value.Name, "整型 id 参数暴露内部标识（L22，应使用 uid）"})
			}
		}
		for _, r := range operationSchemas(o, nil) {
			if r.ref != nil && r.ref.Ref == "" {
				scan(r.ref, r.where)
			}
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].Where < fs[j].Where })
	return fs
}

// openapi3T 是 *openapi3.T 的别名，便于其它文件不重复 import。
type openapi3T = openapi3.T

type stats struct {
	Paths  int
	Ops    int
	Phases map[string]int
}

func statsOf(doc *openapi3.T) stats {
	st := stats{Phases: map[string]int{}}
	if doc.Paths != nil {
		st.Paths = len(doc.Paths.Map())
	}
	for _, o := range operations(doc) {
		st.Ops++
		p, _ := extString(o.Op, "x-phase")
		st.Phases[p]++
	}
	return st
}

// PhaseSummary 形如 "M1=200 P2=128 …"。
func (s stats) PhaseSummary() string {
	keys := make([]string, 0, len(s.Phases))
	for k := range s.Phases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, s.Phases[k]))
	}
	return strings.Join(parts, " ")
}
