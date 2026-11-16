package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// initialRoot 在包初始化时（测试尚未切换 cwd）确定仓库根；部分测试会 chdir 进临时 git 仓库。
var initialRoot = func() string {
	root, err := filepath.Abs("../..")
	if err != nil {
		panic(err)
	}
	return root
}()

// repoRoot 返回仓库根（测试在 scripts/contract 下运行）。
func repoRoot(t *testing.T) string {
	t.Helper()
	root := initialRoot
	if _, err := os.Stat(filepath.Join(root, "docs", "api", "openapi.yaml")); err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}
	return root
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// opYAML 生成一个合法的 operation 片段（缩进 4 空格，置于 paths 下的方法键内）。
type opOpts struct {
	ID       string
	Scope    string
	Perms    string // 形如 "[a:b]"
	Reauth   bool
	Phase    string
	Params   string // 额外参数 YAML（缩进 8 空格，每项以 "- " 开头）
	RespYAML string // 200 响应 schema（缩进 18 空格起）；空则用 {type: object}
	Omit     string // 省略的扩展名
}

func (o opOpts) yaml(method string) string {
	if o.Scope == "" {
		o.Scope = "tenant"
	}
	if o.Perms == "" {
		o.Perms = "[thing:read]"
	}
	if o.Phase == "" {
		o.Phase = "M1"
	}
	resp := o.RespYAML
	if resp == "" {
		resp = "type: object"
	}
	var b strings.Builder
	b.WriteString("    " + method + ":\n")
	if o.ID != "" {
		b.WriteString("      operationId: " + o.ID + "\n")
	}
	ext := func(k, v string) {
		if o.Omit != k {
			b.WriteString("      " + k + ": " + v + "\n")
		}
	}
	ext("x-permissions", o.Perms)
	ext("x-tenant-scope", o.Scope)
	ext("x-audit", "false")
	ext("x-phase", o.Phase)
	if o.Reauth {
		b.WriteString("      x-reauth: true\n")
	}
	if o.Params != "" {
		b.WriteString("      parameters:\n" + o.Params)
	}
	b.WriteString("      responses:\n        '200':\n          description: ok\n          content:\n            application/json:\n              schema:\n")
	for _, l := range strings.Split(resp, "\n") {
		b.WriteString("                " + l + "\n")
	}
	return b.String()
}

// spec 组装一份最小 OpenAPI 3.1 文档：paths 为 path→已渲染的方法片段；components 为 schemas 下的原始 YAML（缩进 4 空格）。
func spec(paths map[string]string, schemas string) []byte {
	var b strings.Builder
	b.WriteString("openapi: 3.1.0\ninfo:\n  title: t\n  version: '1'\npaths:\n")
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		b.WriteString("  " + k + ":\n" + paths[k])
	}
	b.WriteString("components:\n  schemas:\n")
	if schemas == "" {
		schemas = "    Empty:\n      type: object\n"
	}
	b.WriteString(schemas)
	return []byte(b.String())
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func mustLoad(t *testing.T, data []byte) *openapi3.T {
	t.Helper()
	doc, err := loadDoc(data, "inline")
	if err != nil {
		t.Fatalf("样例文档应合法: %v\n%s", err, data)
	}
	return doc
}

func rules(fs []finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

func hasRule(fs []finding, rule string) bool {
	for _, f := range fs {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

// toolCmd 取外部工具命令：环境变量（Makefile 传入固定版本）优先，否则使用默认固定版本。
func toolCmd(env, def string) []string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return strings.Fields(v)
	}
	return strings.Fields(def)
}

func runTool(t *testing.T, argv []string, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(argv[0], append(append([]string{}, argv[1:]...), args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
