package main

import (
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// 归档解压入口门禁（G-4 静态部分，Story 1.26、tools-design §18.1）：归档只能经 internal/tools/security.SafeExtract 解压，
// 因此非测试 Go 代码里，除 internal/tools/security/ 之外，不得 import archive/tar 或 archive/zip
// （自己解析归档就意味着自己处理路径穿越/链接/炸弹——这些恰是 SafeExtract 集中防住的）。
// 测试代码（构造恶意样本）与 tests/、scripts/ 下的开发期工具不受限。

const extractAllowedDir = "internal/tools/security/"

var archiveImports = map[string]bool{"archive/tar": true, "archive/zip": true}

func runExtract(root string, _ options) ([]Finding, error) {
	var out []Finding
	fset := token.NewFileSet()
	err := walkText(root, func(rel string, isDir bool) bool {
		if isDir {
			return rel == "docs" || rel == "tests" || rel == "scripts"
		}
		return isTestFile(rel) || !strings.HasSuffix(rel, ".go")
	}, func(f textFile) {
		if strings.HasPrefix(f.Rel, extractAllowedDir) {
			return
		}
		file, err := parser.ParseFile(fset, f.Rel, f.Content, parser.ImportsOnly|parser.SkipObjectResolution)
		if err != nil {
			out = append(out, Finding{File: f.Rel, Rule: "extract-unparseable", Msg: "无法解析 Go 文件，import 检查无法执行: " + err.Error()})
			return
		}
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); archiveImports[p] {
				out = append(out, Finding{File: f.Rel, Line: fset.Position(imp.Pos()).Line, Rule: "extract-archive",
					Msg: "禁止直接 import " + p + "（G-4）：归档解压只能经 internal/tools/security.SafeExtract"})
			}
		}
	})
	return out, err
}
