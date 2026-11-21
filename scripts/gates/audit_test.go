package main

import (
	"strings"
	"testing"
)

func realAuditInput(t *testing.T) auditInput {
	t.Helper()
	in, err := loadAuditInput(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// 真实仓库必须通过（正向），且解析不能退化为空。
func TestAuditGateAcceptsRepository(t *testing.T) {
	in := realAuditInput(t)
	if fs := checkAudit(in); len(fs) != 0 {
		t.Fatalf("审计门禁应通过，实际:\n%v", fs)
	}
	ops := parseOpenAPIAudit(in.OpenAPI)
	writes, audited := 0, 0
	for _, o := range ops {
		if o.ID == "" {
			t.Errorf("第 %d 行的 operation 没有解析到 operationId", o.Line)
		}
		if o.Method != "GET" {
			writes++
			if o.Audit {
				audited++
			}
		}
	}
	if len(ops) < 400 || writes < 100 || audited < 100 {
		t.Fatalf("openapi 解析退化: operations=%d writes=%d audited=%d", len(ops), writes, audited)
	}
	cat, err := parseAuditCatalog(in.Catalog)
	if err != nil || len(cat.Ops) < 25 || len(cat.Consts) < 25 {
		t.Fatalf("目录解析退化: %v ops=%d consts=%d", err, len(cat.Ops), len(cat.Consts))
	}
	if len(in.Sources) < 50 {
		t.Fatalf("业务源码扫描退化: %d 个文件", len(in.Sources))
	}
}

func mutate(in auditInput, f func(*auditInput)) auditInput {
	cp := in
	cp.Sources = map[string]string{}
	for k, v := range in.Sources {
		cp.Sources[k] = v
	}
	f(&cp)
	return cp
}

// 负向样本：「声明 Audit 但未记录」与「写 operation 缺 Audit 声明」都必须被发现。
func TestAuditGateRejectsViolations(t *testing.T) {
	in := realAuditInput(t)
	cases := []struct {
		name string
		mut  func(*auditInput)
		rule string
		sub  string
	}{
		{"写 operation 缺 x-audit 声明", func(a *auditInput) {
			a.OpenAPI = strings.Replace(a.OpenAPI, "operationId: createRole\n", "operationId: createRole\n", 1)
			i := strings.Index(a.OpenAPI, "operationId: createRole")
			j := i + strings.Index(a.OpenAPI[i:], "x-audit: true")
			a.OpenAPI = a.OpenAPI[:j] + "x-audit: false" + a.OpenAPI[j+len("x-audit: true"):]
		}, "audit-write-no-audit", "createRole"},
		{"例外条目过期（operation 不存在）", func(a *auditInput) { a.Exempt += "noSuchOperation 随便写的理由\n" }, "audit-exempt-stale", "noSuchOperation"},
		{"例外条目缺理由", func(a *auditInput) { a.Exempt += "validateRule\n" }, "audit-exempt-stale", "缺少理由"},
		{"例外条目指向已声明审计的 operation", func(a *auditInput) { a.Exempt += "createUser 不该例外\n" }, "audit-exempt-stale", "createUser"},
		{"例外条目指向读 operation", func(a *auditInput) { a.Exempt += "listUsers 读操作\n" }, "audit-exempt-stale", "listUsers"},
		{"目录登记了 openapi 里没有 x-audit 的 operation", func(a *auditInput) {
			a.Catalog = strings.Replace(a.Catalog, `{"cancelTask", []Action{ActionTaskCancel}},`, `{"cancelTask", []Action{ActionTaskCancel}},`+"\n\t"+`{"listUsers", []Action{ActionUserCreate}},`, 1)
		}, "audit-catalog-openapi", "listUsers"},
		{"目录登记了不存在的 operation", func(a *auditInput) {
			a.Catalog = strings.Replace(a.Catalog, `{"cancelTask", []Action{ActionTaskCancel}},`, `{"cancelTask", []Action{ActionTaskCancel}},`+"\n\t"+`{"noSuchOperation", []Action{ActionTaskCancel}},`, 1)
		}, "audit-catalog-openapi", "noSuchOperation"},
		{"动作常量没有登记到任何 operation", func(a *auditInput) { a.Catalog += "\nconst ActionOrphan Action = \"x.orphan\"\n" }, "audit-const-orphan", "ActionOrphan"},
		{"声明了审计动作却没有记录者（删掉唯一的 Record 调用文件）", func(a *auditInput) {
			for rel, src := range a.Sources {
				if strings.Contains(src, "audit.ActionTaskCancel") {
					delete(a.Sources, rel)
				}
			}
		}, "audit-declared-unrecorded", "ActionTaskCancel"},
		{"Record 的动作不是登记常量", func(a *auditInput) {
			a.Sources["internal/x/y.go"] = "package x\n\nfunc f(w interface{}) {\n\tvar act audit.Action\n\tw.Record(ctx, act, audit.Event{ResourceType: \"x\"})\n}\n"
		}, "audit-record-unverifiable", "audit.Action*"},
		{"Record 引用了未声明的常量", func(a *auditInput) {
			a.Sources["internal/x/y.go"] = "package x\n\nfunc f() {\n\tw.Record(ctx, audit.ActionNope, audit.Event{ResourceType: \"x\"})\n}\n"
		}, "audit-record-unknown", "ActionNope"},
		{"无法解析的 Go 文件", func(a *auditInput) { a.Sources["internal/x/bad.go"] = "package x\n\nfunc (\n" }, "audit-record-unparseable", "无法解析"},
		{"openapi 解析为空不得放行", func(a *auditInput) { a.OpenAPI = "openapi: 3.1.0\n" }, "audit-openapi", "解析"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := checkAudit(mutate(in, c.mut))
			if !hasRule(fs, c.rule, c.sub) {
				t.Fatalf("应报 [%s] 含 %q，实际:\n%v", c.rule, c.sub, fs)
			}
		})
	}
	// 合法写法不误伤：新增一处合法的 Record 调用不应产生发现。
	ok := mutate(in, func(a *auditInput) {
		a.Sources["internal/x/ok.go"] = "package x\n\nfunc f() {\n\ts.audit.Record(ctx, audit.ActionTaskCancel, audit.Event{ResourceType: \"task\"})\n\tmetrics.Record(a, b, c)\n}\n"
	})
	if fs := checkAudit(ok); len(fs) != 0 {
		t.Fatalf("合法调用与无关的 Record 方法不应被误伤: %v", fs)
	}
}
