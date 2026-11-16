package apierr

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到 go.mod")
		}
		dir = parent
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// readmeIndex 解析 docs/api/README.md §5 错误码索引：code → HTTP 状态（"—" 记 0）。
func readmeIndex(t *testing.T) map[string]int {
	t.Helper()
	text := string(repoFile(t, "docs/api/README.md"))
	start := strings.Index(text, "## 5. 错误码索引")
	end := strings.Index(text, "## 6.")
	if start < 0 || end < start {
		t.Fatal("README 中找不到 §5 错误码索引")
	}
	codeRe := regexp.MustCompile("`([a-z][a-z0-9_.]*)`")
	out := map[string]int{}
	for _, line := range strings.Split(text[start:end], "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) < 3 {
			t.Fatalf("表格行异常: %q", line)
		}
		status := 0
		if s := strings.TrimSpace(cells[1]); s != "—" {
			n, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("状态列不是数字或「—」: %q", line)
			}
			status = n
		}
		for _, m := range codeRe.FindAllStringSubmatch(cells[0], -1) {
			if _, dup := out[m[1]]; dup {
				t.Fatalf("README 错误码重复: %s", m[1])
			}
			out[m[1]] = status
		}
	}
	return out
}

// 映射表与文档逐项一致（双向）：文档有而代码无、代码有而文档无、状态不同，均失败。
func TestTableMatchesREADMEErrorCodeIndex(t *testing.T) {
	doc := readmeIndex(t)
	if len(doc) < 40 {
		t.Fatalf("解析到的错误码过少(%d)，解析逻辑可能失效", len(doc))
	}
	for code, st := range doc {
		got, ok := table[Code(code)]
		if !ok {
			t.Errorf("README 列出但代码缺失: %s", code)
			continue
		}
		if got != st {
			t.Errorf("%s: 代码状态 %d，README 状态 %d", code, got, st)
		}
	}
	for c, st := range table {
		if _, ok := doc[string(c)]; !ok {
			t.Errorf("代码登记但 README 未列出: %s（%d）", c, st)
		}
	}
}

func TestEveryCodeHasSyntaxAndExportedConstant(t *testing.T) {
	src := string(func() []byte {
		b, err := os.ReadFile("codes.go")
		if err != nil {
			t.Fatal(err)
		}
		return b
	}())
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\t[A-Z]\w* Code = "([^"]+)"$`).FindAllStringSubmatch(src, -1) {
		declared[m[1]] = true
	}
	for _, c := range Codes() {
		if !ValidSyntax(c) {
			t.Errorf("错误码语法不合法（须小写点分）: %q", c)
		}
		if !declared[string(c)] {
			t.Errorf("错误码 %q 没有对应的导出常量", c)
		}
	}
	for c := range declared { // 反向：声明了常量却没进映射表
		if !Known(Code(c)) {
			t.Errorf("常量 %q 未登记在映射表中", c)
		}
	}
	for _, bad := range []Code{"", "Validation.failed", "validation", "validation..failed", "validation.Failed", ".x", "x.", "auth.invalid credentials", "auth-forbidden.x"} {
		if ValidSyntax(bad) {
			t.Errorf("%q 不应通过语法检查", bad)
		}
	}
}

// ADR-017 §3 / architecture §9.2 点名的映射：独立于 README 解析再钉一遍，防止两边同时被改坏。
func TestStatusMappingADR017(t *testing.T) {
	cases := []struct {
		code Code
		want int
	}{
		{ValidationFailed, 400}, {ConfigValidationFailed, 400}, {AuthPasswordPolicy, 400},
		{AuthUnauthenticated, 401}, {AuthInvalidCredentials, 401}, {AuthTotpRequired, 401}, {AuthTotpInvalid, 401}, {AuthTokenRevoked, 401}, {SetupTokenInvalid, 401},
		{AuthForbidden, 403}, {AuthReauthRequired, 403}, {AuthCsrfInvalid, 403}, {FeatureDisabled, 403}, {TenantQuotaExceeded, 403},
		{ResourceNotFound, 404},
		{ResourceConflict, 409}, {SiteDomainConflict, 409}, {TaskNotCancelable, 409}, {NodeClusterConflict, 409},
		{SetupAlreadyInstalled, 410},
		{ResourcePreconditionFailed, 412},
		{IdempotencyKeyReuse, 422},
		{RateLimitExceeded, 429},
		{InternalError, 500},
		{"does.not_exist", 500}, // 未登记 → 500
		{TaskFailed, 500},       // 无 HTTP 状态行 → 500
		{EnrollTokenInvalid, 500},
	}
	for _, tc := range cases {
		if got := Status(tc.code); got != tc.want {
			t.Errorf("Status(%s)=%d，期望 %d", tc.code, got, tc.want)
		}
	}
	if Known("does.not_exist") || !Known(TaskFailed) || HasHTTPStatus(TaskFailed) || !HasHTTPStatus(ResourceNotFound) {
		t.Error("Known/HasHTTPStatus 判断不符")
	}
	// 423 / 422 校验：契约禁止用 423 与「校验失败用 422」。
	for c, st := range table {
		if st == 423 {
			t.Errorf("%s 不得使用 423（登录节流统一 429）", c)
		}
		if st == 422 && c != IdempotencyKeyReuse {
			t.Errorf("%s 不得使用 422（唯一例外是 idempotency.key_reuse）", c)
		}
	}
}

// 响应体形状与 OpenAPI 的 Error / ErrorDetail / ErrorResponse 对齐：字段集合相等，required 均被无条件输出。
func TestBodyShapeMatchesOpenAPISchemas(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
				Required   []string       `yaml:"required"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(repoFile(t, "docs/api/openapi.yaml"), &doc); err != nil {
		t.Fatalf("解析 openapi.yaml: %v", err)
	}
	keys := func(m map[string]any) map[string]bool {
		out := map[string]bool{}
		for k := range m {
			out[k] = true
		}
		return out
	}
	sch := doc.Components.Schemas
	errSchema, detSchema, respSchema := sch["Error"], sch["ErrorDetail"], sch["ErrorResponse"]

	// 以真实序列化结果（含全部可选字段）取 JSON 键集合。
	full := Render(New(ValidationFailed, "m", Detail{Field: "f", Reason: "r", Code: "c"}), "01ARZ3NDEKTSV4RRFFQ69G5FAV").Body
	got := jsonKeys(t, full)
	if want := keys(respSchema.Properties); !equalSets(got, want) {
		t.Errorf("ErrorResponse 键 %v，OpenAPI %v", got, want)
	}
	inner := jsonKeys(t, full.Error)
	if want := keys(errSchema.Properties); !equalSets(inner, want) {
		t.Errorf("Error 键 %v，OpenAPI %v", inner, want)
	}
	det := jsonKeys(t, full.Error.Details[0])
	if want := keys(detSchema.Properties); !equalSets(det, want) {
		t.Errorf("ErrorDetail 键 %v，OpenAPI %v", det, want)
	}
	// 最小错误体（无 details、无字段级 code）里 OpenAPI 标 required 的键必须仍然存在。
	min := jsonKeys(t, Render(New(ResourceNotFound, "m"), "01ARZ3NDEKTSV4RRFFQ69G5FAV").Body.Error)
	for _, r := range errSchema.Required {
		if !min[r] {
			t.Errorf("Error.required 的 %q 在最小错误体中缺失", r)
		}
	}
	// AC3 额外要求 message_key 与 details 总是输出（OpenAPI 未标 required，但前端依赖）。
	for _, k := range []string{"message_key", "details"} {
		if !min[k] {
			t.Errorf("%s 必须总是输出", k)
		}
	}
	minDetail := jsonKeys(t, Detail{Reason: "r"})
	for _, r := range detSchema.Required {
		if !minDetail[r] {
			t.Errorf("ErrorDetail.required 的 %q 缺失", r)
		}
	}
}
