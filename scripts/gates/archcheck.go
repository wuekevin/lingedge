package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// runArch 检查 depguard 之外无法用 import 规则表达的结构约束（ADR-001 §3、§5）：
//  1. .golangci.yml 与依赖图规格一致（防手改/漂移）；
//  2. internal/ 与 cmd/ 下每个目录都已在依赖图中登记（新增模块/二进制必须先定边界）；
//  3. 模块根包仅含 api.go（及 doc.go 包注释），实现仅位于 internal/<mod>/internal/{handler,service,repository,domain}；
//  4. platform/ports 仅含接口与 DTO、无实现，且接口名在 ADR-001 §5 清单内。
func runArch(root string, opts options) ([]Finding, error) {
	var out []Finding

	fs, err := runGenLint(root, options{check: true})
	if err != nil {
		return nil, err
	}
	out = append(out, fs...)

	// 2a. internal/ 目录登记
	entries, err := os.ReadDir(filepath.Join(root, "internal"))
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !isServerModule(e.Name()) && e.Name() != "tools" && e.Name() != "testdouble" { // internal/testdouble 为 ADR-023 的共享替身位置
			out = append(out, Finding{File: "internal/" + e.Name(), Rule: "arch-unregistered-module",
				Msg: "未在依赖图登记的模块目录：先修订 ADR-001/architecture §5.3，再更新 scripts/gates/spec.go 并 `make gen-lint`"})
		}
	}
	// 2b. cmd/ 目录登记
	cmdEntries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		return nil, err
	}
	for _, e := range cmdEntries {
		if !e.IsDir() {
			continue
		}
		n := e.Name()
		if !contains(standaloneBinaries, n) && !contains(serverBinaries, n) && n != toolsBinary {
			out = append(out, Finding{File: "cmd/" + n, Rule: "arch-unregistered-binary",
				Msg: "未归类的二进制：须在 spec.go 指定其依赖边界（独立二进制 / Server 装配 / Tools）"})
		}
	}

	// 3. 模块根包与实现包布局
	for _, m := range serverModules {
		modDir := filepath.Join(root, "internal", m)
		if _, err := os.Stat(modDir); err != nil {
			continue
		}
		out = append(out, checkModuleLayout(root, m)...)
	}

	// 4. ports
	pf, err := checkPorts(root)
	if err != nil {
		return nil, err
	}
	out = append(out, pf...)

	// 5. 独立二进制的传递依赖不含 internal/*（含经 proto/、edge/ 间接引入的情形）
	if !opts.skipBuild {
		out = append(out, checkStandaloneDeps(root)...)
	}
	return out, nil
}

// checkStandaloneDeps：depguard 只看直接 import；这里用 go list -deps 补上传递依赖（ADR-001 §7）。
func checkStandaloneDeps(root string) []Finding {
	var out []Finding
	for _, b := range standaloneBinaries {
		if _, err := os.Stat(filepath.Join(root, "cmd", b)); err != nil {
			continue
		}
		deps, err := goCmd(root, "list", "-deps", "-f", "{{.ImportPath}}", "./cmd/"+b).CombinedOutput()
		if err != nil {
			out = append(out, Finding{File: "cmd/" + b, Rule: "arch-standalone-deps", Msg: "go list -deps 失败: " + strings.TrimSpace(string(deps))})
			continue
		}
		for _, p := range strings.Fields(string(deps)) {
			if strings.HasPrefix(p, modulePath+"/internal/") {
				out = append(out, Finding{File: "cmd/" + b, Rule: "arch-standalone-deps",
					Msg: "独立二进制的传递依赖含 " + p + "（只允许 pkg/、proto/、edge/；ADR-001 §7）"})
			}
		}
	}
	return out
}

// checkRootFileContent：doc.go 只承载包注释；api.go 只有接口与 DTO，不得含函数/方法实现或包级变量。
//
// 唯一的例外（Story 1.16 修订 ADR-001 §3）：api.go 可以导出**纯委托的构造函数**——函数名以 New 开头、无接收者、函数体只有一条
// `return <internal 包别名>.<函数>(…)`，且被调用的包位于本模块的 internal/<mod>/internal/… 下。原因：实现包位于
// internal/<mod>/internal/…，按 Go 的 internal 规则只有 internal/<mod> 子树内的代码能 import 它们，cmd/lingedge-server 的装配层
// （ADR-001 §4「手写 wire」）因此只能经根包拿到模块实例；没有这个出口，模块无法被装配。委托形态保证根包里没有任何实现逻辑。
func checkRootFileContent(root, mod, name string) []Finding {
	rel := "internal/" + mod + "/" + name
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.SkipObjectResolution)
	if err != nil {
		return []Finding{{File: rel, Rule: "arch-module-root", Msg: "无法解析: " + err.Error()}}
	}
	var out []Finding
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if name == "api.go" && isDelegatingConstructor(d, f, mod) {
				continue
			}
			out = append(out, Finding{File: rel, Line: fset.Position(d.Pos()).Line, Rule: "arch-module-root", Msg: "模块根包不得含函数/方法实现：" + d.Name.Name + "（实现放 internal/" + mod + "/internal/…，ADR-001 §3；唯一例外是纯委托的 New* 构造函数）"})
		case *ast.GenDecl:
			if d.Tok == token.VAR || (name == "doc.go" && d.Tok != token.IMPORT) {
				out = append(out, Finding{File: rel, Line: fset.Position(d.Pos()).Line, Rule: "arch-module-root", Msg: "模块根包 " + name + " 不得含包级变量（doc.go 仅承载包注释）"})
			}
		}
	}
	return out
}

// isDelegatingConstructor 判断 d 是否是「纯委托的构造函数」：名称 New 或 New<大写>…、无接收者、非泛型、函数体恰为单条
// `return <包别名>.<函数>(…)`，且该包别名对应 modulePath/internal/<mod>/internal/… 下的包。
func isDelegatingConstructor(d *ast.FuncDecl, f *ast.File, mod string) bool {
	if d.Recv != nil || !isConstructorName(d.Name.Name) || d.Body == nil || len(d.Body.List) != 1 {
		return false
	}
	ret, ok := d.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	prefix := modulePath + "/internal/" + mod + "/internal/"
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !strings.HasPrefix(p, prefix) {
			continue
		}
		local := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			local = imp.Name.Name
		}
		if local == id.Name {
			return true
		}
	}
	return false
}

func checkModuleLayout(root, mod string) []Finding {
	var out []Finding
	modDir := filepath.Join(root, "internal", mod)

	rootEntries, _ := os.ReadDir(modDir)
	for _, e := range rootEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || isTestFile(e.Name()) {
			continue
		}
		if !rootAllowedFiles[e.Name()] {
			out = append(out, Finding{File: "internal/" + mod + "/" + e.Name(), Rule: "arch-module-root",
				Msg: "模块根包只允许 api.go（Service 接口与 DTO）与 doc.go；实现必须放在 internal/" + mod + "/internal/{handler,service,repository,domain}（ADR-001 §3）"})
			continue
		}
		out = append(out, checkRootFileContent(root, mod, e.Name())...)
	}
	if mod == "platform" { // platform 为基础设施容器，子包自由布局
		return out
	}

	seen := map[string]bool{}
	_ = filepath.WalkDir(modDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		dirRel, _ := filepath.Rel(modDir, filepath.Dir(p))
		dirRel = filepath.ToSlash(dirRel)
		if dirRel == "." || seen[dirRel] {
			return nil
		}
		for _, seg := range strings.Split(dirRel, "/") { // Go 工具链忽略的目录
			if seg == "testdata" || strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "_") {
				return nil
			}
		}
		seen[dirRel] = true
		parts := strings.Split(dirRel, "/")
		ok := false
		switch {
		case parts[len(parts)-1] == "testdouble" && parts[0] == "internal":
			ok = true
		case len(parts) == 2 && parts[0] == "internal" && implPackages[parts[1]]:
			ok = true
		}
		if !ok {
			out = append(out, Finding{File: "internal/" + mod + "/" + dirRel, Rule: "arch-module-layout",
				Msg: "模块实现包只允许位于 internal/" + mod + "/internal/{handler,service,repository,domain}（及 testdouble）（ADR-001 §3）"})
		}
		return nil
	})
	return out
}

func checkPorts(root string) ([]Finding, error) {
	dir := filepath.Join(root, "internal", "platform", "ports")
	rel := "internal/platform/ports"
	if _, err := os.Stat(dir); err != nil {
		return []Finding{{File: rel, Rule: "arch-ports", Msg: "缺少 internal/platform/ports（ADR-001 §5：跨模块必需调用的唯一通道）"}}, nil
	}
	var out []Finding
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !isTestFile(e.Name()) {
			names = append(names, e.Name())
		}
		if e.IsDir() {
			out = append(out, Finding{File: rel + "/" + e.Name(), Rule: "arch-ports", Msg: "ports 不设子包（仅接口与 DTO）"})
		}
	}
	sort.Strings(names)
	fset := token.NewFileSet()
	for _, n := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
		if err != nil {
			out = append(out, Finding{File: rel + "/" + n, Rule: "arch-ports", Msg: "无法解析: " + err.Error()})
			continue
		}
		add := func(pos token.Pos, msg string) {
			out = append(out, Finding{File: rel + "/" + n, Line: fset.Position(pos).Line, Rule: "arch-ports", Msg: msg})
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				add(d.Pos(), "ports 仅含接口与 DTO，不得有函数/方法实现："+d.Name.Name)
			case *ast.GenDecl:
				if d.Tok == token.VAR {
					add(d.Pos(), "ports 不得声明包级变量（无实现、无状态）")
				}
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts := spec.(*ast.TypeSpec)
					if ts.Assign.IsValid() {
						add(ts.Pos(), "ports 不得使用类型别名（"+ts.Name.Name+"）：别名可把实现类型引入端口包")
					}
					if _, isIface := ts.Type.(*ast.InterfaceType); isIface && !portInterfaces[ts.Name.Name] {
						add(ts.Pos(), fmt.Sprintf("接口 %s 不在 ADR-001 §5 ports 清单内（清单变更须评审并修订 ADR 与 spec.go；含未导出接口）", ts.Name.Name))
					}
				}
			}
		}
	}
	return out, nil
}
