// Package contracttest 是 REST 契约测试助手（Story 1.12 AC6、ADR-017 §1/§11）：用 kin-openapi 加载 docs/api/openapi.yaml，
// 校验真实 HTTP 响应是否符合所声明 operation 的状态码、响应头与 body schema，并核对路由元数据与 x-* 的一致性。
//
// 只供测试使用（生产二进制不 import 它）。响应校验对「未在 OpenAPI 里声明的状态码」同样失败，防止实现悄悄偏离契约。
package contracttest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

var httpMethods = []string{"GET", "PUT", "POST", "DELETE", "PATCH"}

// Spec 是已加载并校验的 OpenAPI 文档。
type Spec struct {
	Doc *openapi3.T
	ops map[string]*Op
}

// Op 是一个 operation 及其由 x-* 推导出的期望元数据。
type Op struct {
	ID, Method, Path string
	Item             *openapi3.PathItem
	Operation        *openapi3.Operation
	Meta             httpx.RouteMeta
}

// Load 读取并校验 OpenAPI 3.1 文件。
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := LoadFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// LoadFromBytes 解析并校验内存中的 OpenAPI 3.1 文档（合成文档的自测用）。
func LoadFromBytes(data []byte) (*Spec, error) {
	path := "openapi"
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromData(data)
	if err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1.") {
		return nil, fmt.Errorf("%s: openapi 版本 %q 不是 3.1.x", path, doc.OpenAPI)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("%s 不是合法的 OpenAPI: %w", path, err)
	}
	s := &Spec{Doc: doc, ops: map[string]*Op{}}
	for p, item := range doc.Paths.Map() {
		for _, m := range httpMethods {
			op := item.GetOperation(m)
			if op == nil {
				continue
			}
			if op.OperationID == "" {
				return nil, fmt.Errorf("%s %s 缺少 operationId", m, p)
			}
			s.ops[op.OperationID] = &Op{ID: op.OperationID, Method: m, Path: p, Item: item, Operation: op, Meta: metaOf(m, p, op)}
		}
	}
	return s, nil
}

// TB 是测试对象的最小接口（*testing.T / *testing.B 满足）。本包是非测试代码，不得 import testing（ADR-023 §A.6）。
type TB interface {
	Helper()
	Error(args ...any)
	Fatal(args ...any)
}

// LoadRepo 从当前目录向上找到仓库根（含 go.mod）并加载 docs/api/openapi.yaml。
func LoadRepo(tb TB) *Spec {
	tb.Helper()
	dir, err := os.Getwd()
	if err != nil {
		tb.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			tb.Fatal("找不到仓库根（go.mod）")
		}
		dir = parent
	}
	s, err := Load(filepath.Join(dir, "docs", "api", "openapi.yaml"))
	if err != nil {
		tb.Fatal(err)
	}
	return s
}

func metaOf(method, path string, op *openapi3.Operation) httpx.RouteMeta {
	m := httpx.RouteMeta{OperationID: op.OperationID, Method: method, Path: path, Permissions: httpx.NoPermissions()}
	if v, ok := op.Extensions["x-permissions"].([]any); ok {
		for _, p := range v {
			if s, ok := p.(string); ok {
				m.Permissions = append(m.Permissions, s)
			}
		}
	}
	if s, ok := op.Extensions["x-tenant-scope"].(string); ok {
		m.TenantScope = httpx.TenantScope(s)
	}
	m.Audit, _ = op.Extensions["x-audit"].(bool)
	m.Reauth, _ = op.Extensions["x-reauth"].(bool)
	m.Phase, _ = op.Extensions["x-phase"].(string)
	m.Feature, _ = op.Extensions["x-feature"].(string)
	m.Auth = httpx.AuthRequired
	if op.Security != nil && len(*op.Security) == 0 { // security: []
		m.Auth = httpx.AuthPublic
	}
	for _, p := range op.Parameters {
		if p.Value != nil && p.Value.In == "header" && p.Value.Name == httpx.IdempotencyKeyHeader {
			m.Idempotent = true
		}
	}
	return m
}

// OperationIDs 返回全部 operationId（升序）。
func (s *Spec) OperationIDs() []string {
	ids := make([]string, 0, len(s.ops))
	for id := range s.ops {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Operation 按 operationId 取 operation。
func (s *Spec) Operation(id string) (*Op, bool) { op, ok := s.ops[id]; return op, ok }

// Expected 返回按 x-* 推导的期望元数据（升序 operationId）。
func (s *Spec) Expected() []httpx.RouteMeta {
	out := make([]httpx.RouteMeta, 0, len(s.ops))
	for _, id := range s.OperationIDs() {
		out = append(out, s.ops[id].Meta)
	}
	return out
}

// CheckRoutes 校验运行时路由集合 ⊆ OpenAPI，且元数据与 x-* 一致；返回全部问题（空表示通过）。
func (s *Spec) CheckRoutes(routes []httpx.RouteMeta) []string {
	var p []string
	for _, r := range routes {
		want, ok := s.ops[r.OperationID]
		if !ok {
			p = append(p, fmt.Sprintf("%s: operationId 不在 openapi.yaml 中", r.OperationID))
			continue
		}
		w := want.Meta
		diff := func(field string, got, exp any) {
			p = append(p, fmt.Sprintf("%s: %s 与 openapi.yaml 不一致（代码 %v，契约 %v）", r.OperationID, field, got, exp))
		}
		if r.Method != w.Method {
			diff("Method", r.Method, w.Method)
		}
		if r.Path != w.Path {
			diff("Path", r.Path, w.Path)
		}
		if r.Auth != w.Auth {
			diff("Auth", r.Auth, w.Auth)
		}
		if r.TenantScope != w.TenantScope {
			diff("TenantScope", r.TenantScope, w.TenantScope)
		}
		if r.Audit != w.Audit {
			diff("Audit", r.Audit, w.Audit)
		}
		if r.Reauth != w.Reauth {
			diff("Reauth", r.Reauth, w.Reauth)
		}
		if r.Idempotent != w.Idempotent {
			diff("Idempotent", r.Idempotent, w.Idempotent)
		}
		if r.Phase != w.Phase {
			diff("Phase", r.Phase, w.Phase)
		}
		if r.Feature != w.Feature {
			diff("Feature", r.Feature, w.Feature)
		}
		if a, b := sortedCopy(r.Permissions), sortedCopy(w.Permissions); strings.Join(a, ",") != strings.Join(b, ",") {
			diff("Permissions", a, b)
		}
	}
	return p
}

// Unimplemented 返回 x-phase=phase 且未出现在 routes 中的 operationId（升序，「M1 未实现 operation 清单」）。
func (s *Spec) Unimplemented(routes []httpx.RouteMeta, phase string) []string {
	have := map[string]bool{}
	for _, r := range routes {
		have[r.OperationID] = true
	}
	var out []string
	for _, id := range s.OperationIDs() {
		if s.ops[id].Meta.Phase == phase && !have[id] {
			out = append(out, id)
		}
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// ValidateResponse 校验响应是否符合 operationId 所声明的契约：状态码必须已声明；有 body schema 时 body 必须通过；
// 声明为 required 的响应头以及 X-Request-ID（每个响应必返回）必须存在。
func (s *Spec) ValidateResponse(operationID string, req *http.Request, status int, header http.Header, body []byte) error {
	op, ok := s.ops[operationID]
	if !ok {
		return fmt.Errorf("operationId %q 不在 openapi.yaml 中", operationID)
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: req,
			Route:   &routers.Route{Spec: s.Doc, Path: op.Path, PathItem: op.Item, Method: op.Method, Operation: op.Operation},
		},
		Status:  status,
		Header:  header,
		Body:    io.NopCloser(bytes.NewReader(body)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		return fmt.Errorf("%s 的 %d 响应不符合契约: %w", operationID, status, err)
	}
	if header.Get("X-Request-ID") == "" {
		return fmt.Errorf("%s 的 %d 响应缺少 X-Request-ID（每个响应必返回）", operationID, status)
	}
	if resp := op.Operation.Responses.Status(status); resp != nil && resp.Value != nil {
		for name, h := range resp.Value.Headers {
			if h.Value != nil && h.Value.Required && header.Get(name) == "" {
				return fmt.Errorf("%s 的 %d 响应缺少必需的响应头 %s", operationID, status, name)
			}
		}
	}
	return nil
}

// AssertResponse 校验 *http.Response（读取并恢复 body）；httptest.ResponseRecorder 经 rec.Result() 传入。
func (s *Spec) AssertResponse(t TB, operationID string, req *http.Request, resp *http.Response) {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	if err := s.ValidateResponse(operationID, req, resp.StatusCode, resp.Header, b); err != nil {
		t.Error(err)
	}
}
