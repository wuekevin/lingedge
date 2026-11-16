package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// repo-scope 门禁（Story 1.5 AC5、ADR-009 §1/§6）：Repository 包内每个导出的方法/函数，第一个业务参数必须是 scope.Scope。
//
//   - 「业务参数」指跳过开头的 context.Context 之后的第一个参数；类型须是 scope.Scope 或 tenantscope.Scope（按 import 路径解析，别名也认）；
//   - 导出的构造函数（名称以 New 开头）不受约束；
//   - 访问平台级表（无 tenant_id）的方法可在文档注释中写 `lingedge:platform-table=<表名>` 豁免，表名必须在
//     data/platform-tables.txt 清单内（租户表无法豁免）；
//   - 名称含 AcrossTenants 的方法同样必须以 Scope 为首参（平台范围 Scope；运行期由 tenantdb 校验）。
//
// 租户表清单不能从 docs/database 自动生成（文档对 T / T? / 平台级的标注是叙述性的），故采用「默认全部要求 Scope、
// 平台级表显式登记」的反向清单：漏登记只会多报，不会漏报。

// 允许 `// lingedge:…` 与 `//lingedge:…`（后者被 go/ast 的 Doc.Text() 当作指令剔除，故直接扫描注释原文）；值整体取出后逐项核对，逗号分隔的多表一并检查。
var rePlatformTable = regexp.MustCompile(`lingedge:platform-table=([A-Za-z0-9_,]+)`)

// isConstructorName：仅 `New` 或 `New<大写开头>` 的包级函数是构造函数（NewestSites 之类不是）。
func isConstructorName(name string) bool {
	if name == "New" {
		return true
	}
	return strings.HasPrefix(name, "New") && len(name) > 3 && name[3] >= 'A' && name[3] <= 'Z'
}

func platformTables() map[string]bool {
	m := map[string]bool{}
	for _, l := range loadLines("platform-tables.txt") {
		m[l] = true
	}
	return m
}

func runRepo(root string, _ options) ([]Finding, error) {
	var out []Finding
	fset := token.NewFileSet()
	plat := platformTables()
	err := walkText(root, func(rel string, isDir bool) bool {
		if isDir {
			return rel != "internal" && !strings.HasPrefix(rel, "internal/")
		}
		return !strings.HasSuffix(rel, ".go") || isTestFile(rel) || !inRepositoryPackage(rel)
	}, func(f textFile) {
		file, err := parser.ParseFile(fset, f.Rel, f.Content, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			out = append(out, Finding{File: f.Rel, Rule: "repo-unparseable", Msg: "无法解析 Go 文件，Scope 检查无法执行: " + err.Error()})
			return
		}
		out = append(out, checkRepoFile(fset, f.Rel, file, plat)...)
	})
	return out, err
}

var reRepoPath = regexp.MustCompile(repositoryPathRegex)

// inRepositoryPackage 判断文件是否位于 Repository 实现包（目录路径，不含文件名）。
func inRepositoryPackage(rel string) bool {
	i := strings.LastIndex(rel, "/")
	if i < 0 {
		return false
	}
	return reRepoPath.MatchString(rel[:i] + "/")
}

func checkRepoFile(fset *token.FileSet, rel string, file *ast.File, plat map[string]bool) []Finding {
	var out []Finding
	// 本地名 → import 路径，用于识别 Scope 与 context.Context（含别名）。
	locals := map[string]string{}
	for _, imp := range file.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		locals[name] = p
	}
	isSel := func(e ast.Expr, pkgPaths []string, typ string) bool {
		s, ok := e.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != typ {
			return false
		}
		id, ok := s.X.(*ast.Ident)
		return ok && contains(pkgPaths, locals[id.Name])
	}
	isCtx := func(e ast.Expr) bool { return isSel(e, []string{"context"}, "Context") }
	isScope := func(e ast.Expr) bool { return isSel(e, scopeTypeImports, "Scope") }

	for _, decl := range file.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
			// 接口里声明的导出方法同样是 Repository 契约：缺 Scope 的接口方法会让实现绕过规则。
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				it, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					continue
				}
				for _, m := range it.Methods.List {
					ft, ok := m.Type.(*ast.FuncType)
					if !ok || len(m.Names) == 0 || !m.Names[0].IsExported() {
						continue
					}
					if !firstBusinessParamIsScope(ft, isCtx, isScope) {
						out = append(out, Finding{File: rel, Line: fset.Position(m.Pos()).Line, Rule: "repo-scope",
							Msg: ts.Name.Name + "." + m.Names[0].Name + "：接口方法第一个业务参数（context.Context 之后）必须是 scope.Scope（ADR-009 §1）"})
					}
				}
			}
			continue
		}
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !fn.Name.IsExported() || (fn.Recv == nil && isConstructorName(fn.Name.Name)) {
			continue
		}
		pos := fset.Position(fn.Pos()).Line
		add := func(msg string) {
			out = append(out, Finding{File: rel, Line: pos, Rule: "repo-scope", Msg: fn.Name.Name + "：" + msg})
		}

		if fn.Doc != nil {
			var tables []string
			for _, c := range fn.Doc.List {
				if m := rePlatformTable.FindStringSubmatch(c.Text); m != nil {
					tables = append(tables, strings.Split(m[1], ",")...)
				}
			}
			if len(tables) > 0 {
				for _, tbl := range tables {
					if !plat[tbl] {
						add("豁免的表 " + tbl + " 不在平台级表清单（scripts/gates/data/platform-tables.txt）内；租户表不得豁免 Scope 首参（ADR-009 §1）")
					}
				}
				if strings.Contains(fn.Name.Name, "AcrossTenants") {
					add("名称含 AcrossTenants 的方法必须以 Scope 为首参（平台范围），不得用平台表豁免")
				}
				continue
			}
		}
		if !firstBusinessParamIsScope(fn.Type, isCtx, isScope) {
			add("Repository 的导出方法第一个业务参数（context.Context 之后）必须是 scope.Scope（ADR-009 §1）；访问平台级表的方法请在文档注释写 `lingedge:platform-table=<表名>`")
		}
	}
	return out
}

// firstBusinessParamIsScope：跳过开头的 context.Context 后，第一个参数是否为 Scope。
func firstBusinessParamIsScope(ft *ast.FuncType, isCtx, isScope func(ast.Expr) bool) bool {
	var params []ast.Expr
	for _, fld := range ft.Params.List {
		n := len(fld.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			params = append(params, fld.Type)
		}
	}
	i := 0
	if i < len(params) && isCtx(params[i]) {
		i++
	}
	return i < len(params) && isScope(params[i])
}
