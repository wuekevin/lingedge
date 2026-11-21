package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 审计门禁（Story 1.21 AC1、FR-23、IMP-19；make audit-check）——「声明 Audit 但未记录」与「写 operation 缺 Audit 声明」都必须被发现：
//
//	audit-write-no-audit    openapi.yaml 里的写 operation（POST/PUT/PATCH/DELETE）没有 x-audit: true，且未在 data/audit-exempt.txt 登记（带理由）；
//	audit-exempt-stale      例外清单条目过期：operation 不存在、不是写 operation、已声明 x-audit: true，或缺少理由；
//	audit-catalog-openapi   audit.Catalog 登记的 operation 在 openapi.yaml 里不存在或没有 x-audit: true（目录与契约漂移）；
//	audit-const-orphan      audit.Action 常量没有登记到任何 operation，或 Catalog 引用了未声明的常量；
//	audit-declared-unrecorded  Catalog 登记的动作在业务代码里没有任何 `.Record(ctx, audit.ActionX, …)` 调用——声明了审计却没有记录者；
//	audit-record-unverifiable  `.Record(ctx, <非 audit.Action* 常量>, audit.Event{…})`：动作不是登记常量，门禁无法核对；
//	audit-record-unknown    Record 调用引用了 Catalog 里没有登记的动作常量。
//
// 运行时侧的对应检查：httpx 审计钩子（audit.Writer.Auditor）对「声明了 x-audit 的成功请求却没有审计行」报警计数；
// contracttest.CheckAuditCoverage 对真实路由集合做同样的元数据检查（cmd/lingedge-server 的 TestAuditCoverage）。
// 本门禁是纯文本/AST 解析（仅标准库），自测（audit_test.go）含负向样本。

const (
	auditCatalogRel = "internal/platform/audit/catalog.go"
	auditPkgDir     = "internal/platform/audit"
	auditExemptRel  = "scripts/gates/data/audit-exempt.txt"
)

type openAPIOp struct {
	ID     string
	Method string
	Audit  bool
	Line   int
}

var (
	reOpMethod = regexp.MustCompile(`^    (get|post|put|patch|delete):\s*$`)
	reOpAudit  = regexp.MustCompile(`^      x-audit:\s*(true|false)\s*$`)
)

// parseOpenAPIAudit 提取每个 operation 的方法、operationId 与 x-audit。
func parseOpenAPIAudit(doc string) []openAPIOp {
	var ops []openAPIOp
	var cur *openAPIOp
	for i, l := range strings.Split(doc, "\n") {
		if m := reOpMethod.FindStringSubmatch(l); m != nil {
			ops = append(ops, openAPIOp{Method: strings.ToUpper(m[1]), Line: i + 1})
			cur = &ops[len(ops)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if m := reOpID.FindStringSubmatch(l); m != nil && cur.ID == "" {
			cur.ID = m[1]
		} else if m := reOpAudit.FindStringSubmatch(l); m != nil {
			cur.Audit = m[1] == "true"
		}
	}
	return ops
}

// parseAuditExempt 解析例外清单：operationId → 理由（理由为空的条目也返回，由检查报告）。
func parseAuditExempt(txt string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(txt, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		id, reason, _ := strings.Cut(l, " ")
		out[id] = strings.TrimSpace(reason)
	}
	return out
}

// auditCatalog 是从 catalog.go 的 AST 解析出的目录：常量名 → 值、operationId → 动作常量名。
type auditCatalog struct {
	Consts map[string]string
	Ops    map[string][]string
}

func parseAuditCatalog(src string) (auditCatalog, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, auditCatalogRel, src, 0)
	if err != nil {
		return auditCatalog{}, err
	}
	c := auditCatalog{Consts: map[string]string{}, Ops: map[string][]string{}}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, sp := range gd.Specs {
			vs, ok := sp.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if gd.Tok == token.CONST {
				for i, n := range vs.Names {
					if !strings.HasPrefix(n.Name, "Action") || i >= len(vs.Values) {
						continue
					}
					if bl, ok := vs.Values[i].(*ast.BasicLit); ok && bl.Kind == token.STRING {
						v, _ := strconv.Unquote(bl.Value)
						c.Consts[n.Name] = v
					}
				}
			}
			if gd.Tok == token.VAR && len(vs.Names) == 1 && vs.Names[0].Name == "Catalog" && len(vs.Values) == 1 {
				cl, ok := vs.Values[0].(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, e := range cl.Elts {
					el, ok := e.(*ast.CompositeLit)
					if !ok || len(el.Elts) < 2 {
						continue
					}
					idLit, ok := el.Elts[0].(*ast.BasicLit)
					if !ok {
						continue
					}
					id, _ := strconv.Unquote(idLit.Value)
					acts, ok := el.Elts[1].(*ast.CompositeLit)
					if !ok {
						continue
					}
					for _, a := range acts.Elts {
						if ident, ok := a.(*ast.Ident); ok {
							c.Ops[id] = append(c.Ops[id], ident.Name)
						}
					}
					if _, ok := c.Ops[id]; !ok {
						c.Ops[id] = nil
					}
				}
			}
		}
	}
	return c, nil
}

// recordCall 是一处 `.Record(ctx, audit.ActionX, audit.Event{…})` 调用。
type recordCall struct {
	File   string
	Line   int
	Action string // 常量名；空串 = 第二个实参不是 audit.Action* 常量
}

// findRecordCalls 在非测试 Go 源码里找审计记录调用：三个实参、方法名 Record、第三个实参是 audit.Event 复合字面量或第二个实参是 audit.Action* 选择表达式。
func findRecordCalls(rel, src string) ([]recordCall, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, 0)
	if err != nil {
		return nil, err
	}
	var out []recordCall
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Record" {
			return true
		}
		action := ""
		if s, ok := call.Args[1].(*ast.SelectorExpr); ok {
			if x, ok := s.X.(*ast.Ident); ok && x.Name == "audit" && strings.HasPrefix(s.Sel.Name, "Action") {
				action = s.Sel.Name
			}
		}
		isEvent := false
		if cl, ok := call.Args[2].(*ast.CompositeLit); ok {
			if s, ok := cl.Type.(*ast.SelectorExpr); ok {
				if x, ok := s.X.(*ast.Ident); ok && x.Name == "audit" && s.Sel.Name == "Event" {
					isEvent = true
				}
			}
		}
		if action != "" || isEvent {
			out = append(out, recordCall{File: rel, Line: fset.Position(call.Pos()).Line, Action: action})
		}
		return true
	})
	return out, nil
}

type auditInput struct {
	OpenAPI string
	Exempt  string
	Catalog string
	Sources map[string]string // 相对路径 → 内容（非测试 Go 源码，已排除审计包自身）
}

func checkAudit(in auditInput) []Finding {
	var out []Finding
	add := func(file string, line int, rule, f string, a ...any) {
		out = append(out, Finding{File: file, Line: line, Rule: rule, Msg: fmt.Sprintf(f, a...)})
	}
	ops := parseOpenAPIAudit(in.OpenAPI)
	if len(ops) == 0 {
		add(permsOpenAPIRel, 0, "audit-openapi", "未能从 openapi.yaml 解析出任何 operation（格式变化？门禁不能在解析为空时放行）")
	}
	byID := map[string]openAPIOp{}
	for _, o := range ops {
		byID[o.ID] = o
	}
	exempt := parseAuditExempt(in.Exempt)
	isWrite := func(m string) bool { return m == "POST" || m == "PUT" || m == "PATCH" || m == "DELETE" }

	// ① 写 operation 缺 Audit 声明。
	for _, o := range ops {
		if isWrite(o.Method) && !o.Audit {
			if _, ok := exempt[o.ID]; !ok {
				add(permsOpenAPIRel, o.Line, "audit-write-no-audit", "%s（%s）是写 operation，却没有 x-audit: true；若确实不产生持久变更，须在 %s 登记并写明理由", o.ID, o.Method, auditExemptRel)
			}
		}
	}
	for id, reason := range exempt {
		o, ok := byID[id]
		switch {
		case !ok:
			add(auditExemptRel, 0, "audit-exempt-stale", "例外 %s 不在 openapi.yaml 中（过期条目）", id)
		case !isWrite(o.Method):
			add(auditExemptRel, 0, "audit-exempt-stale", "例外 %s 不是写 operation（%s），无需例外", id, o.Method)
		case o.Audit:
			add(auditExemptRel, 0, "audit-exempt-stale", "例外 %s 已声明 x-audit: true，应删除该例外", id)
		}
		if reason == "" {
			add(auditExemptRel, 0, "audit-exempt-stale", "例外 %s 缺少理由", id)
		}
	}

	// ② 目录 ⇄ openapi、常量。
	cat, err := parseAuditCatalog(in.Catalog)
	if err != nil {
		add(auditCatalogRel, 0, "audit-catalog-parse", "无法解析 audit 目录: %v", err)
		return out
	}
	if len(cat.Ops) == 0 || len(cat.Consts) == 0 {
		add(auditCatalogRel, 0, "audit-catalog-parse", "未能从 catalog.go 解析出动作常量或目录（格式变化？门禁不能在解析为空时放行）")
		return out
	}
	used := map[string]bool{}
	for id, acts := range cat.Ops {
		o, ok := byID[id]
		switch {
		case !ok:
			add(auditCatalogRel, 0, "audit-catalog-openapi", "目录登记的 operation %s 不在 openapi.yaml 中", id)
		case !o.Audit:
			add(auditCatalogRel, 0, "audit-catalog-openapi", "目录登记的 operation %s 在 openapi.yaml 里没有 x-audit: true（契约与目录漂移）", id)
		}
		if len(acts) == 0 {
			add(auditCatalogRel, 0, "audit-const-orphan", "operation %s 在目录里没有任何动作", id)
		}
		for _, a := range acts {
			used[a] = true
			if _, ok := cat.Consts[a]; !ok {
				add(auditCatalogRel, 0, "audit-const-orphan", "目录引用了未声明的动作常量 %s（operation %s）", a, id)
			}
		}
	}
	seenVal := map[string]string{}
	for name, val := range cat.Consts {
		if !used[name] {
			add(auditCatalogRel, 0, "audit-const-orphan", "动作常量 %s（%q）没有登记到任何 operation", name, val)
		}
		if prev, dup := seenVal[val]; dup {
			add(auditCatalogRel, 0, "audit-const-orphan", "动作取值 %q 被 %s 与 %s 重复声明", val, prev, name)
		}
		seenVal[val] = name
	}

	// ③ 声明了动作却没有记录者。
	recorded := map[string]bool{}
	rels := make([]string, 0, len(in.Sources))
	for rel := range in.Sources {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		calls, err := findRecordCalls(rel, in.Sources[rel])
		if err != nil {
			add(rel, 0, "audit-record-unparseable", "无法解析 Go 文件: %v", err)
			continue
		}
		for _, c := range calls {
			switch {
			case c.Action == "":
				add(c.File, c.Line, "audit-record-unverifiable", "Record 的动作必须是 audit.Action* 常量（门禁据此核对「声明了审计就有记录者」）")
			case func() bool { _, ok := cat.Consts[c.Action]; return !ok }():
				add(c.File, c.Line, "audit-record-unknown", "Record 引用了未声明的动作常量 audit.%s", c.Action)
			default:
				if !used[c.Action] {
					add(c.File, c.Line, "audit-record-unknown", "动作 audit.%s 没有登记到 audit.Catalog 的任何 operation", c.Action)
				}
				recorded[c.Action] = true
			}
		}
	}
	for id, acts := range cat.Ops {
		for _, a := range acts {
			if _, ok := cat.Consts[a]; ok && !recorded[a] {
				add(auditCatalogRel, 0, "audit-declared-unrecorded", "operation %s 声明了审计动作 audit.%s，但业务代码里没有任何 .Record(ctx, audit.%s, …) 调用——声明了审计却没有记录", id, a, a)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Msg < out[j].Msg
	})
	return out
}

func runAudit(root string, _ options) ([]Finding, error) {
	in, err := loadAuditInput(root)
	if err != nil {
		return nil, err
	}
	return checkAudit(in), nil
}

func loadAuditInput(root string) (auditInput, error) {
	read := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		return string(b), err
	}
	var in auditInput
	var err error
	if in.OpenAPI, err = read(permsOpenAPIRel); err != nil {
		return in, err
	}
	if in.Exempt, err = read(auditExemptRel); err != nil {
		return in, err
	}
	if in.Catalog, err = read(auditCatalogRel); err != nil {
		return in, err
	}
	in.Sources = map[string]string{}
	err = walkText(root, func(rel string, isDir bool) bool {
		if isDir {
			return (rel != "internal" && !strings.HasPrefix(rel, "internal/")) || rel == auditPkgDir
		}
		return !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go")
	}, func(f textFile) { in.Sources[f.Rel] = string(f.Content) })
	return in, err
}
