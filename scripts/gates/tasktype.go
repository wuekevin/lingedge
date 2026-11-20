package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
)

// 任务类型白名单门禁（G-12 的任务类型部分，Story 1.14 AC1/AC7、FR-35、NFR-9、M14）：
//
//   - tasktype-unknown：Go 源码里声明/登记/入队的任务类型字面量必须在 docs/api/openapi.yaml 的 components.schemas.TaskType 枚举内
//     （枚举是唯一事实源）。检查范围：internal/platform/taskrt/types.go 里的 `Type` 常量，以及全仓非测试 Go 代码中
//     `taskrt.Spec{Type: "…"}` / `taskrt.EnqueueRequest{Type: "…"}`（含 `taskrt.Type("…")` 包装）的字符串字面量。
//   - tasktype-command：类型名带「执行命令」语义（exec/shell/sh/bash/cmd/command/script/eval/spawn/popen/run 片段，或含空白/shell 元字符）。
//     任意命令执行类任务类型不得存在，哪怕有人把它同时加进了 OpenAPI 枚举。
//   - tasktype-spec：运行时存在但缺少 openapi.yaml 或 TaskType 枚举无法解析（不得因缺少事实源而静默放行）。
//
// 与 shell 门禁（`sh -c` 形态、exec.Command 拼接）一起构成「静态检查不存在 sh -c/任意命令类型」。
// 运行时的 Registry.Register 做同样的名称检查（taskrt.ForbiddenTypeReason）；两处片段表由 tasktype_test.go 与 registry_test.go 各自核对。

const (
	taskrtDir      = "internal/platform/taskrt"
	taskrtTypesRel = taskrtDir + "/types.go"
	openapiRel     = "docs/api/openapi.yaml"
)

var taskTypeForbiddenSegments = map[string]bool{
	"exec": true, "execute": true, "shell": true, "sh": true, "bash": true, "cmd": true, "command": true,
	"script": true, "eval": true, "spawn": true, "popen": true, "run": true,
}

var reTaskTypeName = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9_]*)+$`)

// taskTypeProblem 返回类型字面量的命令语义问题（空串表示没有）。
func taskTypeProblem(v string) string {
	if strings.ContainsAny(v, " \t\r\n;|&$`<>()\\\"'") {
		return "含空白或 shell 元字符"
	}
	if !reTaskTypeName.MatchString(v) {
		return "格式非法（须为 小写域.小写动作）"
	}
	for _, seg := range strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '_' }) {
		if taskTypeForbiddenSegments[seg] {
			return "含执行命令语义片段 " + seg
		}
	}
	return ""
}

// openapiTaskTypes 解析 openapi.yaml 的 TaskType 枚举（只依赖标准库：按缩进扫描 components.schemas.TaskType.enum）。
func openapiTaskTypes(content []byte) []string {
	lines := strings.Split(string(content), "\n")
	var out []string
	in, inEnum := false, false
	reItem := regexp.MustCompile(`^\s*-\s+'?"?([^'"\s]+)'?"?\s*$`)
	for _, ln := range lines {
		trim := strings.TrimSpace(ln)
		indent := len(ln) - len(strings.TrimLeft(ln, " "))
		switch {
		case !in:
			if ln == "    TaskType:" {
				in = true
			}
		case trim == "" || strings.HasPrefix(trim, "#"):
		case indent <= 4: // 下一个 schema
			return out
		case !inEnum:
			if trim == "enum:" {
				inEnum = true
			}
		default:
			if m := reItem.FindStringSubmatch(ln); m != nil {
				out = append(out, m[1])
			} else {
				return out
			}
		}
	}
	return out
}

func runTasktype(root string, _ options) ([]Finding, error) {
	var out []Finding
	var haveRuntime bool
	var spec map[string]bool
	var specErr string

	fset := token.NewFileSet()
	var pending []struct {
		rel  string
		file *ast.File
	}
	err := walkText(root, nil, func(f textFile) {
		switch {
		case f.Rel == openapiRel:
			spec = map[string]bool{}
			for _, v := range openapiTaskTypes(f.Content) {
				spec[v] = true
			}
		case strings.HasSuffix(f.Rel, ".go") && !isTestFile(f.Rel) && (strings.HasPrefix(f.Rel, "internal/") || strings.HasPrefix(f.Rel, "cmd/")):
			if strings.HasPrefix(f.Rel, taskrtDir+"/") {
				haveRuntime = true
			}
			file, err := parser.ParseFile(fset, f.Rel, f.Content, parser.SkipObjectResolution)
			if err != nil {
				out = append(out, Finding{File: f.Rel, Rule: "tasktype-unparseable", Msg: "无法解析 Go 文件，任务类型检查无法执行: " + err.Error()})
				return
			}
			pending = append(pending, struct {
				rel  string
				file *ast.File
			}{f.Rel, file})
		}
	})
	if err != nil {
		return nil, err
	}
	if !haveRuntime { // 任务运行时尚不存在：无事可检
		return out, nil
	}
	if len(spec) == 0 {
		specErr = "找不到 " + openapiRel + " 或其 TaskType 枚举无法解析：任务类型白名单没有事实源，门禁不得静默放行"
		out = append(out, Finding{File: openapiRel, Rule: "tasktype-spec", Msg: specErr})
	}
	check := func(rel string, pos token.Pos, v string, where string) {
		if p := taskTypeProblem(v); p != "" {
			out = append(out, Finding{File: rel, Line: fset.Position(pos).Line, Rule: "tasktype-command",
				Msg: "任务类型 " + strconv.Quote(v) + "（" + where + "）" + p + "（G-12/FR-35：不存在执行任意命令的任务类型）"})
			return
		}
		if specErr == "" && !spec[v] {
			out = append(out, Finding{File: rel, Line: fset.Position(pos).Line, Rule: "tasktype-unknown",
				Msg: "任务类型 " + strconv.Quote(v) + "（" + where + "）不在 OpenAPI TaskType 枚举内（枚举是唯一事实源，新增类型须先改契约，M14）"})
		}
	}
	for _, p := range pending {
		for _, d := range p.file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST || p.rel != taskrtTypesRel {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Type" {
					continue
				}
				for _, val := range vs.Values {
					if lit, ok := val.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, _ := strconv.Unquote(lit.Value)
						check(p.rel, lit.Pos(), v, "types.go 常量")
					}
				}
			}
		}
		inTaskrt := strings.HasPrefix(p.rel, taskrtDir+"/")
		ast.Inspect(p.file, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok || !isTaskrtLiteralType(cl.Type, inTaskrt) {
				return true
			}
			for _, el := range cl.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Type" {
					continue
				}
				val := kv.Value
				if call, ok := val.(*ast.CallExpr); ok && len(call.Args) == 1 { // taskrt.Type("…")
					val = call.Args[0]
				}
				if lit, ok := val.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					check(p.rel, lit.Pos(), v, "登记/入队字面量")
				}
			}
			return true
		})
	}
	return out, nil
}

// isTaskrtLiteralType 判断复合字面量类型是否为 taskrt.Spec / taskrt.EnqueueRequest（包内则为裸 Spec/EnqueueRequest）。
func isTaskrtLiteralType(e ast.Expr, inTaskrt bool) bool {
	switch t := e.(type) {
	case *ast.SelectorExpr:
		x, ok := t.X.(*ast.Ident)
		return ok && x.Name == "taskrt" && (t.Sel.Name == "Spec" || t.Sel.Name == "EnqueueRequest")
	case *ast.Ident:
		return inTaskrt && (t.Name == "Spec" || t.Name == "EnqueueRequest")
	}
	return false
}
