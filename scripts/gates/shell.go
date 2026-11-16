package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// 命令注入门禁（G-12、NFR-9、基线 §24）：禁止 shell 拼接。
//   - 文本层：任何非文档/非测试文件中的 `sh -c`、`bash -c`、`cmd /c`、`powershell -Command` 形态；
//   - AST 层：非测试 Go 代码中 exec.Command/CommandContext 的命令名由字符串拼接/格式化构造、命令名含空白、
//     或以 shell 解释器为命令名。该检查对白名单入口 internal/tools/security 同样生效
//     （forbidigo 允许它使用 os/exec，但不允许它绕过「参数数组、无 shell」）。

var shellNames = `(?:/(?:usr/)?bin/)?(?:sh|bash|zsh|dash|ksh|csh|tcsh|fish)`

var shellTextRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b` + shellNames + `\s+-[a-z]*c\b`),
	// 参数数组形态，允许解释器与 -c 之间夹有其他选项（如 "bash","--noprofile","-c"）。
	regexp.MustCompile(`(?i)"` + shellNames + `"\s*,(?:\s*"-[^"]*"\s*,)*\s*"-[a-z]*c"`),
	regexp.MustCompile(`(?i)\b(?:cmd(?:\.exe)?\s+/c|powershell(?:\.exe)?\s+-command|pwsh(?:\.exe)?\s+-command|pwsh\s+-c)\b`),
	regexp.MustCompile(`(?i)"(?:cmd(?:\.exe)?|powershell(?:\.exe)?|pwsh(?:\.exe)?)"\s*,\s*"(?:/c|-command|-c)"`),
}

var reShellLiteral = regexp.MustCompile(`(?i)^` + shellNames + `$|^(?:cmd|powershell|pwsh)(?:\.exe)?$`)

// 允许出现在命令名位置的「路径计算」调用（不是字符串拼接成命令行）。
var safeNameCalls = map[string]bool{"filepath.Join": true, "filepath.Clean": true, "path.Join": true, "exec.LookPath": true}

func runShell(root string, _ options) ([]Finding, error) {
	var out []Finding
	fset := token.NewFileSet()
	err := walkText(root, func(rel string, isDir bool) bool {
		if isDir {
			return rel == "docs" || rel == "tests"
		}
		return isTestFile(rel) || strings.HasSuffix(rel, ".md")
	}, func(f textFile) {
		for _, re := range shellTextRes {
			for _, loc := range re.FindAllIndex(f.Content, -1) {
				out = append(out, Finding{File: f.Rel, Line: lineOf(f.Content, loc[0]), Rule: "shell-c",
					Msg: "禁止 shell -c 形态的命令执行（G-12/NFR-9）：用 exec 参数数组直接调用外部程序"})
			}
		}
		if !strings.HasSuffix(f.Rel, ".go") {
			return
		}
		file, err := parser.ParseFile(fset, f.Rel, f.Content, parser.SkipObjectResolution)
		if err != nil {
			out = append(out, Finding{File: f.Rel, Rule: "shell-unparseable", Msg: "无法解析 Go 文件，AST 检查无法执行: " + err.Error()})
			return
		}
		out = append(out, checkExecCalls(fset, f.Rel, file)...)
	})
	return out, err
}

func checkExecCalls(fset *token.FileSet, rel string, file *ast.File) []Finding {
	// 解析 os/exec 的本地名（含别名）
	local := ""
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "os/exec" {
			local = "exec"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
	}
	if local == "" || local == "_" {
		return nil
	}
	// 记录文件内字符串常量/变量的字面量值，用于识别「命令名放在变量里的 shell 解释器」。
	strVals := map[string][]string{}
	collect := func(name *ast.Ident, val ast.Expr) {
		if lit, ok := val.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if v, err := strconv.Unquote(lit.Value); err == nil {
				strVals[name.Name] = append(strVals[name.Name], v)
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.ValueSpec:
			for i, nm := range d.Names {
				if i < len(d.Values) {
					collect(nm, d.Values[i])
				}
			}
		case *ast.AssignStmt:
			for i, l := range d.Lhs {
				if id, ok := l.(*ast.Ident); ok && i < len(d.Rhs) {
					collect(id, d.Rhs[i])
				}
			}
		}
		return true
	})
	var out []Finding
	add := func(pos token.Pos, msg string) {
		out = append(out, Finding{File: rel, Line: fset.Position(pos).Line, Rule: "shell-exec", Msg: msg + "（G-12/NFR-9）"})
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var fnName string
		switch f := call.Fun.(type) {
		case *ast.SelectorExpr:
			if id, ok := f.X.(*ast.Ident); !ok || id.Name != local {
				return true
			}
			fnName = f.Sel.Name
		case *ast.Ident: // import . "os/exec"
			if local != "." {
				return true
			}
			fnName = f.Name
		default:
			return true
		}
		idx := -1
		switch fnName {
		case "Command":
			idx = 0
		case "CommandContext":
			idx = 1
		}
		if idx < 0 || len(call.Args) <= idx {
			return true
		}
		name := call.Args[idx]
		switch e := name.(type) {
		case *ast.BasicLit:
			if v, err := strconv.Unquote(e.Value); err == nil {
				if reShellLiteral.MatchString(v) {
					add(e.Pos(), "以 shell 解释器 "+v+" 作为命令名，禁止")
				} else if strings.ContainsAny(v, " \t") {
					add(e.Pos(), "命令名含空白/参数，命令行必须拆成参数数组")
				}
			}
		case *ast.Ident:
			for _, v := range strVals[e.Name] {
				if reShellLiteral.MatchString(v) {
					add(e.Pos(), "命令名变量 "+e.Name+" 取值为 shell 解释器 "+v+"，禁止")
				} else if strings.ContainsAny(v, " \t") {
					add(e.Pos(), "命令名变量 "+e.Name+" 含空白/参数，命令行必须拆成参数数组")
				}
			}
		case *ast.BinaryExpr:
			add(e.Pos(), "命令名由字符串拼接构造，禁止")
		case *ast.CallExpr:
			if fn := callName(e); !safeNameCalls[fn] {
				add(e.Pos(), "命令名由 "+fn+" 动态构造（字符串拼接/格式化），禁止；命令名须为常量或白名单查表结果")
			}
		}
		return true
	})
	return out
}

func callName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.SelectorExpr:
		if id, ok := f.X.(*ast.Ident); ok {
			return id.Name + "." + f.Sel.Name
		}
		return f.Sel.Name
	case *ast.Ident:
		return f.Name
	}
	return "<call>"
}
