package compilefail

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到 go.mod")
		}
		dir = parent
	}
}

func build(t *testing.T, pkg string) (string, error) {
	t.Helper()
	c := exec.Command("go", "build", "-o", os.DevNull, "./tests/compilefail/testdata/"+pkg)
	c.Dir = repoRoot(t)
	c.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	out, err := c.CombinedOutput()
	return string(out), err
}

func TestAPIMisuseFailsToCompile(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("找不到 go 工具链")
	}
	if out, err := build(t, "ok"); err != nil {
		t.Fatalf("正向对照必须编译通过: %v\n%s", err, out)
	}
	cases := []struct{ pkg, mention, why string }{
		{"bad_sprintf", "sqlConst", "fmt.Sprintf 拼接的 SQL 不是编译期常量"},
		{"bad_concat", "sqlConst", "变量拼接的 SQL 不是编译期常量"},
		{"bad_import_token", "use of internal package", "凭据包不可从 db 子树外导入"},
		{"bad_query_literal", "unexported", "不得用字面量构造 Query"},
		{"bad_pool_mixup", "cannot use m (variable of type *db.MaintPool) as *db.AppPool", "maint 池不能当 app 池用"},
		{"bad_pool_impl", "missing method base", "不能在包外实现 db.Pool"},
	}
	for _, c := range cases {
		t.Run(c.pkg, func(t *testing.T) {
			out, err := build(t, c.pkg)
			if err == nil {
				t.Fatalf("%s 必须编译失败，但通过了", c.why)
			}
			if !strings.Contains(out, c.mention) {
				t.Fatalf("编译错误应提及 %q（%s），实际:\n%s", c.mention, c.why, out)
			}
		})
	}
}
