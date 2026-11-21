package signing

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 节点侧复用：本包只依赖标准库（import 路径首段不含 "."），不得出现 internal/*、第三方库或任何服务端专有依赖。
func TestOnlyStandardLibrary(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	n := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			n++
			if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") {
				t.Errorf("%s 引入了非标准库依赖 %s", f, p)
			}
		}
	}
	if n == 0 {
		t.Fatal("没有解析到任何 import，检查失效")
	}
}
