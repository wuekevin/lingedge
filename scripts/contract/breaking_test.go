package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const defaultOasdiff = "go run github.com/oasdiff/oasdiff@v1.38.0" // 与 Makefile 的 OASDIFF_VERSION 一致；make 传入 OASDIFF 时以环境变量为准

// oasdiffBreaking 真实运行 oasdiff：base/cur 为内联文档，返回破坏性变更。
func oasdiffBreaking(t *testing.T, base, cur []byte) []change {
	t.Helper()
	dir := t.TempDir()
	bp, cp := filepath.Join(dir, "base.yaml"), filepath.Join(dir, "cur.yaml")
	if err := os.WriteFile(bp, base, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cp, cur, 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := readLines(filepath.Join(initialRoot, "scripts", "contract", "data", "oasdiff-severity-levels.txt"))
	if err != nil {
		t.Fatal(err)
	}
	sev := filepath.Join(dir, "sev.txt")
	if err := os.WriteFile(sev, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ch, err := runOasdiff(toolCmd("OASDIFF", defaultOasdiff), bp, cp, sev)
	if err != nil {
		t.Fatalf("oasdiff 运行失败: %v", err)
	}
	return ch
}

// baseDocForDiff：带必填请求体字段、响应字段、枚举的小契约，用于破坏性变更矩阵。
func baseDocForDiff() string {
	return `openapi: 3.1.0
info: {title: t, version: '1'}
paths:
  /things:
    post:
      operationId: createThing
      x-permissions: [thing:write]
      x-tenant-scope: tenant
      x-audit: true
      x-phase: M1
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [name]
              properties:
                name: {type: string, maxLength: 100}
                kind: {type: string, enum: [x, y]}
      responses:
        '201':
          description: ok
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Thing'}
  /things/other:
    get:
      operationId: getOther
      x-permissions: [thing:read]
      x-tenant-scope: tenant
      x-audit: false
      x-phase: M1
      responses:
        '200': {description: ok}
components:
  schemas:
    Status:
      type: string
      enum: [a, b]
    Thing:
      type: object
      required: [uid]
      properties:
        uid: {type: string}
        name: {type: string}
        count: {type: integer}
        status: {$ref: '#/components/schemas/Status'}
`
}

// 取消 x-reauth、取消审计、仅改路径参数名（线上无变化）三种情形。
func TestContractChangesReauthAuditAndRename(t *testing.T) {
	reauth := mustLoad(t, spec(map[string]string{"/act": opOpts{ID: "act", Reauth: true}.yaml("post")}, ""))
	plain := mustLoad(t, spec(map[string]string{"/act": opOpts{ID: "act"}.yaml("post")}, ""))
	found := func(chs []change, id string) bool {
		for _, c := range chs {
			if c.ID == id {
				return true
			}
		}
		return false
	}
	if !found(contractChanges(reauth, plain), "reauth-removed") {
		t.Error("取消 x-reauth 必须判为破坏")
	}
	audited := spec(map[string]string{"/act": strings.Replace(opOpts{ID: "act"}.yaml("post"), "x-audit: false", "x-audit: true", 1)}, "")
	if !found(contractChanges(mustLoad(t, audited), plain), "audit-removed") {
		t.Error("x-audit true→false 必须判为破坏")
	}
	// 路径参数改名不算移动。
	pp := func(name string) []byte {
		o := opOpts{ID: "get", Params: "        - name: " + name + "\n          in: path\n          required: true\n          schema: {type: string}\n"}
		return spec(map[string]string{"/things/{" + name + "}": o.yaml("get")}, "")
	}
	if chs := contractChanges(mustLoad(t, pp("siteId")), mustLoad(t, pp("id"))); len(chs) != 0 {
		t.Errorf("仅改路径参数名不应判破坏: %v", chs)
	}
}

// TestOasdiffMatrix 真实运行 oasdiff（固定版本）：破坏性变更必须被判定，向后兼容的新增必须放行（AC2）。
func TestOasdiffMatrix(t *testing.T) {
	base := baseDocForDiff()
	mut := func(old, new string) []byte {
		if !strings.Contains(base, old) {
			t.Fatalf("样例中找不到 %q", old)
		}
		return []byte(strings.Replace(base, old, new, 1))
	}
	breaking := map[string][]byte{
		"删除响应字段":         mut("        name: {type: string}\n        count", "        count"),
		"改字段类型":          mut("count: {type: integer}", "count: {type: string}"),
		"收紧校验 maxLength": mut("maxLength: 100", "maxLength: 10"),
		"请求体新增必填字段":      mut("required: [name]", "required: [name, kind]"),
		"删除 operation":   mut("  /things/other:\n    get:\n      operationId: getOther\n      x-permissions: [thing:read]\n      x-tenant-scope: tenant\n      x-audit: false\n      x-phase: M1\n      responses:\n        '200': {description: ok}\n", ""),
		"请求体删除枚举值":       mut("enum: [x, y]", "enum: [x]"),
		"删除请求体字段":        mut("                kind: {type: string, enum: [x, y]}\n", ""),
		"删除成功响应":         mut("        '201':\n          description: ok\n          content:\n            application/json:\n              schema: {$ref: '#/components/schemas/Thing'}", "        '202': {description: ok}"),
	}
	for name, cur := range breaking {
		if ch := oasdiffBreaking(t, []byte(base), cur); len(ch) == 0 {
			t.Errorf("%s: 应被判定为破坏性变更", name)
		}
	}
	compatible := map[string][]byte{
		"无变化":          []byte(base),
		"响应新增字段":       mut("        count: {type: integer}", "        count: {type: integer}\n        extra: {type: string}"),
		"请求体新增可选字段":    mut("kind: {type: string, enum: [x, y]}", "kind: {type: string, enum: [x, y]}\n                note: {type: string}"),
		"新增 operation": mut("components:", "  /things/third:\n    get:\n      operationId: getThird\n      x-permissions: [thing:read]\n      x-tenant-scope: tenant\n      x-audit: false\n      x-phase: M1\n      responses:\n        '200': {description: ok}\ncomponents:"),
		"响应新增枚举值（expand，AC9 同形态）": mut("enum: [a, b]", "enum: [a, b, c]"),
		"请求体新增枚举值":                mut("enum: [x, y]", "enum: [x, y, z]"),
	}
	for name, cur := range compatible {
		if ch := oasdiffBreaking(t, []byte(base), cur); len(ch) != 0 {
			t.Errorf("%s: 不应判为破坏性: %v", name, ch)
		}
	}
}

// 工具失败（文件无法加载）不得被当成「无变化」。
func TestOasdiffFailureIsNotSilentPass(t *testing.T) {
	_, err := runOasdiff(toolCmd("OASDIFF", defaultOasdiff), "/nonexistent/base.yaml", "/nonexistent/cur.yaml", "")
	if err == nil {
		t.Fatal("oasdiff 加载失败必须返回错误，不能静默通过")
	}
}

func TestContractChanges(t *testing.T) {
	mk := func(o opOpts, path string) []byte { return spec(map[string]string{path: o.yaml("post")}, "") }
	base := mustLoad(t, mk(opOpts{ID: "act", Scope: "tenant", Perms: "[a:read]"}, "/act"))
	cases := []struct {
		name string
		cur  []byte
		want string // 期望出现的变更 ID；空表示不应有变更
	}{
		{"无变化", mk(opOpts{ID: "act", Scope: "tenant", Perms: "[a:read]"}, "/act"), ""},
		{"operationId 改名", mk(opOpts{ID: "act2", Scope: "tenant", Perms: "[a:read]"}, "/act"), "operation-id-removed"},
		{"路径改名", mk(opOpts{ID: "act", Scope: "tenant", Perms: "[a:read]"}, "/act2"), "operation-moved"},
		{"新增必需权限", mk(opOpts{ID: "act", Scope: "tenant", Perms: "[a:read, a:admin]"}, "/act"), "permission-added"},
		{"放宽权限（不是破坏）", mk(opOpts{ID: "act", Scope: "tenant", Perms: "[]"}, "/act"), ""},
		{"租户范围变更", mk(opOpts{ID: "act", Scope: "platform", Perms: "[a:read]"}, "/act"), "tenant-scope-changed"},
		{"新增 x-reauth", mk(opOpts{ID: "act", Scope: "tenant", Perms: "[a:read]", Reauth: true}, "/act"), "reauth-added"},
		{"x-phase 变更", mk(opOpts{ID: "act", Scope: "tenant", Perms: "[a:read]", Phase: "P2"}, "/act"), "phase-changed"},
	}
	for _, c := range cases {
		got := contractChanges(base, mustLoad(t, c.cur))
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("%s: 不应有变更: %v", c.name, got)
		case c.want != "":
			found := false
			for _, g := range got {
				found = found || g.ID == c.want
			}
			if !found {
				t.Errorf("%s: 应检出 %s: %v", c.name, c.want, got)
			}
		}
	}
}

const readmeSample = `## 5. 错误码索引

| code | HTTP | 含义 |
|---|---|---|
| ` + "`auth.unauthenticated`" + ` | 401 | 未登录 |
| ` + "`auth.totp_required` / `auth.totp_invalid`" + ` | 401 | 二次验证 |
| ` + "`task.failed`" + ` | — | Task 终态错误 |
| ` + "`resource.not_found`" + ` | 404 | 不存在 |

## 6. 其他

| ` + "`not.an_error_code`" + ` | 200 | 不在错误码索引内 |
`

func TestErrorCodes(t *testing.T) {
	codes := parseErrorCodes(readmeSample)
	want := map[string]string{"auth.unauthenticated": "401", "auth.totp_required": "401", "auth.totp_invalid": "401", "task.failed": "—", "resource.not_found": "404"}
	if len(codes) != len(want) {
		t.Fatalf("解析结果 %v，期望 %v", codes, want)
	}
	for k, v := range want {
		if codes[k] != v {
			t.Errorf("%s = %q，期望 %q", k, codes[k], v)
		}
	}
	// 删除错误码
	if ch := errorCodeChanges(readmeSample, strings.Replace(readmeSample, "| `resource.not_found` | 404 | 不存在 |\n", "", 1)); len(ch) != 1 || ch[0].ID != "error-code-removed" {
		t.Errorf("删除错误码应判破坏: %v", ch)
	}
	// 改 HTTP 状态
	if ch := errorCodeChanges(readmeSample, strings.Replace(readmeSample, "| 404 |", "| 410 |", 1)); len(ch) != 1 || ch[0].ID != "error-code-status-changed" {
		t.Errorf("改 HTTP 状态应判破坏: %v", ch)
	}
	// 改名视为删除旧码
	if ch := errorCodeChanges(readmeSample, strings.Replace(readmeSample, "auth.unauthenticated", "auth.no_session", 1)); len(ch) != 1 {
		t.Errorf("改名应判破坏: %v", ch)
	}
	// 新增错误码向后兼容
	if ch := errorCodeChanges(readmeSample, strings.Replace(readmeSample, "| `task.failed`", "| `task.new_code` | 409 | 新 |\n| `task.failed`", 1)); len(ch) != 0 {
		t.Errorf("新增错误码不应判破坏: %v", ch)
	}
	// 真实 README 至少解析出 40 个错误码。
	if n := len(parseErrorCodes(readRepoFile(t, "docs/api/README.md"))); n < 40 {
		t.Errorf("真实 README 只解析出 %d 个错误码", n)
	}
}

func TestChangeNote(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "bmad", "contract-changes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	changes := []change{{ID: "operation-id-removed"}, {ID: "error-code-removed"}}
	good := "# CC-001\n- **影响范围**：POST /things 请求体\n- **是否破坏**：是（删除 operationId 与错误码：operation-id-removed、error-code-removed）\n- **关联 ADR**：ADR-017\n"
	if err := validateChangeNote(write("good.md", good), changes, nil); err != nil {
		t.Errorf("完整变更说明应放行: %v", err)
	}
	bad := map[string]string{
		"空文件":    "",
		"声明非破坏":  strings.Replace(good, "是否破坏**：是", "是否破坏**：否", 1),
		"无 ADR":  strings.Replace(good, "- **关联 ADR**：ADR-017\n", "- **关联 ADR**：N/A\n", 1),
		"无影响范围":  strings.Replace(good, "- **影响范围**：POST /things 请求体\n", "", 1),
		"未替换占位符": strings.Replace(good, "ADR-017", "ADR-017 <待填>", 1),
		"漏列变更类型": strings.Replace(good, "error-code-removed", "x", 1),
	}
	for name, body := range bad {
		if err := validateChangeNote(write(name+".md", body), changes, nil); err == nil {
			t.Errorf("%s: 不应放行", name)
		}
	}
	if err := validateChangeNote(filepath.Join(dir, "missing.md"), changes, nil); err == nil {
		t.Error("文件不存在不应放行")
	}
	// 目录限制：说明必须在 docs/bmad/contract-changes/ 下，且不能是模板/CF 记录。
	outside := filepath.Join(root, "elsewhere.md")
	_ = os.WriteFile(outside, []byte(good), 0o600)
	if err := validateChangeNote(outside, changes, nil); err == nil {
		t.Error("目录之外的说明不应放行")
	}
	if err := validateChangeNote(write("_template.md", good), changes, nil); err == nil {
		t.Error("模板文件名不应放行")
	}
	if err := validateChangeNote(write("CF-1.md", good), changes, nil); err == nil {
		t.Error("CF 评审记录不应放行")
	}
	// 基线中已存在的旧说明不得复用。
	if err := validateChangeNote(write("old.md", good), changes, func(rel string) bool { return strings.HasSuffix(rel, "/old.md") }); err == nil {
		t.Error("基线已有的旧说明不应放行")
	}
	// 仓库模板本身未填写，不得被当成有效变更说明。
	if err := validateChangeNote(filepath.Join(repoRoot(t), "docs/bmad/contract-changes/_template.md"), changes, nil); err == nil {
		t.Error("未填写的模板不应放行破坏性变更")
	}
}
