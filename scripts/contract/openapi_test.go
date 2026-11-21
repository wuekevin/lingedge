package main

import (
	"strings"
	"testing"
)

// 合法基线：一个租户可见的 list operation（分页齐全）+ 一个平台 operation。
func goodSpec() []byte {
	page := "        - name: page\n          in: query\n          schema: {type: integer}\n        - name: page_size\n          in: query\n          schema: {type: integer}\n"
	return spec(map[string]string{
		"/things": opOpts{ID: "listThings", Params: page, RespYAML: "$ref: '#/components/schemas/ThingList'"}.yaml("get") +
			opOpts{ID: "createThing", Perms: "[thing:write]"}.yaml("post"),
		"/things/one": opOpts{ID: "getThing"}.yaml("get"),
	}, `    Thing:
      type: object
      properties:
        uid: {type: string}
        name: {type: string}
    ThingList:
      type: object
      properties:
        items:
          type: array
          items: {$ref: '#/components/schemas/Thing'}
        page: {type: integer}
`)
}

func TestGoodSpecPassesAllChecks(t *testing.T) {
	doc := mustLoad(t, goodSpec())
	if fs := runAllChecks(doc, nil); len(fs) != 0 {
		t.Fatalf("合法样例不应有违规: %v", fs)
	}
}

func TestRealOpenAPIPassesAllChecks(t *testing.T) {
	doc := mustLoad(t, []byte(readRepoFile(t, "docs/api/openapi.yaml")))
	pinned, err := readLines(repoRoot(t) + "/scripts/contract/data/x-reauth.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fs := runAllChecks(doc, pinned); len(fs) != 0 {
		t.Fatalf("真实契约应通过全部静态规则: %v", fs)
	}
	if n := len(operations(doc)); n != 486 {
		t.Errorf("operation 数 = %d，期望 486", n)
	}
}

// 校验器本身：非 3.1、非法结构必须被拒绝。
func TestLoadDocRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"openapi 3.0.3": strings.Replace(string(goodSpec()), "openapi: 3.1.0", "openapi: 3.0.3", 1),
		"缺 info":        strings.Replace(string(goodSpec()), "info:\n  title: t\n  version: '1'\n", "", 1),
		"悬空 $ref":       strings.Replace(string(goodSpec()), "#/components/schemas/Thing'", "#/components/schemas/Nope'", 1),
		"不是 YAML":       "paths: [",
	}
	for name, doc := range cases {
		if _, err := loadDoc([]byte(doc), name); err == nil {
			t.Errorf("%s: loadDoc 应失败", name)
		}
	}
}

func TestMetadataRequired(t *testing.T) {
	for _, omit := range []string{"x-permissions", "x-tenant-scope", "x-audit", "x-phase"} {
		data := spec(map[string]string{"/a": opOpts{ID: "a", Omit: omit}.yaml("get")}, "")
		fs := checkMetadata(mustLoad(t, data))
		if !hasRule(fs, "metadata") || !strings.Contains(fs[0].Msg, omit) {
			t.Errorf("缺少 %s 应被报告: %v", omit, fs)
		}
	}
	// 缺 operationId
	fs := checkMetadata(mustLoad(t, spec(map[string]string{"/a": opOpts{ID: ""}.yaml("get")}, "")))
	if len(fs) == 0 {
		t.Error("缺 operationId 应失败")
	}
	// operationId 重复：由校验器（loadDoc）拒绝。
	if _, err := loadDoc(spec(map[string]string{
		"/a": opOpts{ID: "dup"}.yaml("get"), "/b": opOpts{ID: "dup"}.yaml("get")}, ""), "dup"); err == nil {
		t.Error("operationId 重复应失败")
	}
	// 取值非法
	fs = checkMetadata(mustLoad(t, spec(map[string]string{"/a": opOpts{ID: "a", Scope: "everyone"}.yaml("get")}, "")))
	if len(fs) == 0 {
		t.Error("x-tenant-scope 非法取值应失败")
	}
}

func TestNoColonPaths(t *testing.T) {
	doc := mustLoad(t, spec(map[string]string{
		"/sites/s1/rules:validate":   opOpts{ID: "validateRules"}.yaml("post"),
		"/sites/s1/rule-validations": opOpts{ID: "ruleValidations"}.yaml("post"),
	}, ""))
	fs := checkNoColonPaths(doc)
	if len(fs) != 1 || !strings.Contains(fs[0].Where, "rules:validate") {
		t.Fatalf("应仅命中含冒号的路径: %v", fs)
	}
}

const leakSchemas = `    Inner:
      type: object
      properties:
        node_uid: {type: string}
    Outer:
      type: object
      properties:
        summary:
          type: object
          properties:
            healthy: {type: integer}
            total: {type: integer}
            offline: {type: integer}
        detail: {$ref: '#/components/schemas/Inner'}
    Loop:
      type: object
      properties:
        self: {$ref: '#/components/schemas/Loop'}
        ok: {type: string}
    Clean:
      type: object
      properties:
        summary:
          type: object
          properties:
            healthy: {type: integer}
            total: {type: integer}
`

func TestTenantLeakScan(t *testing.T) {
	ref := func(n string) string { return "$ref: '#/components/schemas/" + n + "'" }
	t.Run("经 $ref 递归命中 node_uid", func(t *testing.T) {
		doc := mustLoad(t, spec(map[string]string{"/x": opOpts{ID: "x", Scope: "tenant", RespYAML: ref("Outer")}.yaml("get")}, leakSchemas))
		if fs := checkTenantLeak(doc); len(fs) != 1 || !strings.Contains(fs[0].Msg, "node_uid") {
			t.Fatalf("期望命中 node_uid: %v", fs)
		}
	})
	t.Run("either 同样检查", func(t *testing.T) {
		doc := mustLoad(t, spec(map[string]string{"/x": opOpts{ID: "x", Scope: "either", RespYAML: ref("Outer")}.yaml("get")}, leakSchemas))
		if len(checkTenantLeak(doc)) == 0 {
			t.Fatal("either 应被检查")
		}
	})
	t.Run("platform 范围允许", func(t *testing.T) {
		doc := mustLoad(t, spec(map[string]string{"/x": opOpts{ID: "x", Scope: "platform", RespYAML: ref("Outer")}.yaml("get")}, leakSchemas))
		if fs := checkTenantLeak(doc); len(fs) != 0 {
			t.Fatalf("platform 不应被检查: %v", fs)
		}
	})
	t.Run("聚合计数允许，自引用环不死循环", func(t *testing.T) {
		doc := mustLoad(t, spec(map[string]string{
			"/c": opOpts{ID: "c", Scope: "tenant", RespYAML: ref("Clean")}.yaml("get"),
			"/l": opOpts{ID: "l", Scope: "tenant", RespYAML: ref("Loop")}.yaml("get"),
		}, leakSchemas))
		if fs := checkTenantLeak(doc); len(fs) != 0 {
			t.Fatalf("聚合计数与环不应误报: %v", fs)
		}
	})
	t.Run("请求参数名与 allOf/数组内的标识也命中", func(t *testing.T) {
		params := "        - name: cluster_uid\n          in: query\n          schema: {type: string}\n"
		doc := mustLoad(t, spec(map[string]string{
			"/p": opOpts{ID: "p", Scope: "tenant", Params: params}.yaml("get"),
			"/a": opOpts{ID: "a", Scope: "tenant", RespYAML: "type: array\nitems:\n  allOf:\n    - type: object\n      properties:\n        egress_node_ip: {type: string}\n        targets: {type: array, items: {type: string}}"}.yaml("get"),
		}, ""))
		fs := checkTenantLeak(doc)
		joined := ""
		for _, f := range fs {
			joined += f.Msg + ";"
		}
		for _, want := range []string{"cluster_uid", "egress_node_ip", "targets"} {
			if !strings.Contains(joined, want) {
				t.Errorf("应命中 %s: %v", want, fs)
			}
		}
	})
}

func TestReauthSetPinned(t *testing.T) {
	doc := mustLoad(t, spec(map[string]string{
		"/a": opOpts{ID: "a", Reauth: true}.yaml("post"),
		"/b": opOpts{ID: "b"}.yaml("post"),
	}, ""))
	if fs := checkReauthSet(doc, []string{"a"}); len(fs) != 0 {
		t.Fatalf("集合一致应通过: %v", fs)
	}
	if fs := checkReauthSet(doc, []string{"a", "b"}); len(fs) != 1 || !strings.Contains(fs[0].Msg, "取消") {
		t.Fatalf("契约取消了 b 的 x-reauth 应失败: %v", fs)
	}
	if fs := checkReauthSet(doc, nil); len(fs) != 1 || !strings.Contains(fs[0].Msg, "新增") {
		t.Fatalf("契约新增 a 的 x-reauth 但未登记应失败: %v", fs)
	}
}

func TestListPagination(t *testing.T) {
	list := "type: object\nproperties:\n  items:\n    type: array\n    items: {type: string}"
	q := func(names ...string) string {
		var b strings.Builder
		for _, n := range names {
			b.WriteString("        - name: " + n + "\n          in: query\n          schema: {type: integer}\n")
		}
		return b.String()
	}
	cases := []struct {
		name   string
		params string
		bad    bool
	}{
		{"无分页参数", "", true},
		{"仅 page", q("page"), true},
		{"page+page_size", q("page", "page_size"), false},
		{"cursor+limit", q("cursor", "limit"), false},
		{"cursor 缺 limit", q("cursor"), true},
	}
	for _, c := range cases {
		doc := mustLoad(t, spec(map[string]string{"/l": opOpts{ID: "l", Params: c.params, RespYAML: list}.yaml("get")}, ""))
		if got := len(checkListPagination(doc)) > 0; got != c.bad {
			t.Errorf("%s: 违规=%v，期望 %v", c.name, got, c.bad)
		}
	}
	// 非 list 的 GET（无 items）与非 GET 不检查。
	doc := mustLoad(t, spec(map[string]string{"/o": opOpts{ID: "o"}.yaml("get"), "/p": opOpts{ID: "p", RespYAML: list}.yaml("post")}, ""))
	if fs := checkListPagination(doc); len(fs) != 0 {
		t.Errorf("非 list operation 不应检查: %v", fs)
	}
}

func TestNoInternalID(t *testing.T) {
	bad := `    A:
      type: object
      properties:
        id: {type: integer, format: int64}
        uid: {type: string}
    B:
      type: object
      properties:
        inner:
          type: object
          properties:
            id: {type: integer}
    C:
      type: object
      properties:
        uid: {type: string}
        id: {type: string}
`
	doc := mustLoad(t, spec(map[string]string{"/a": opOpts{ID: "a"}.yaml("get")}, bad))
	fs := checkNoInternalID(doc)
	if len(fs) != 2 {
		t.Fatalf("应命中 A.id 与 B.inner.id 两处（C.id 为字符串不算）: %v", fs)
	}
	if fs := checkNoInternalID(mustLoad(t, goodSpec())); len(fs) != 0 {
		t.Fatalf("uid 形态不应违规: %v", fs)
	}
}

func TestTenantLeakScanExtendedKeywords(t *testing.T) {
	ref := func(n string) string { return "$ref: '#/components/schemas/" + n + "'" }
	schemas := `    PI:
      type: array
      prefixItems:
        - type: object
          properties:
            node_id: {type: string}
    IfThen:
      type: object
      if: {properties: {a: {type: string}}}
      then:
        properties:
          cluster_uid: {type: string}
    PatternP:
      type: object
      patternProperties:
        '^x':
          type: object
          properties:
            targets: {type: array, items: {type: string}}
    Contains:
      type: array
      contains:
        type: object
        properties:
          egress_node_ip: {type: string}
    Camel:
      type: object
      properties:
        nodeId: {type: string}
    ClusterRegion:
      type: object
      properties:
        cluster_region: {type: string}
    Unev:
      type: object
      unevaluatedProperties:
        type: object
        properties:
          node_name: {type: string}
`
	for _, name := range []string{"PI", "IfThen", "PatternP", "Contains", "Camel", "ClusterRegion", "Unev"} {
		doc := mustLoad(t, spec(map[string]string{"/x": opOpts{ID: "x", Scope: "tenant", RespYAML: ref(name)}.yaml("get")}, schemas))
		if len(checkTenantLeak(doc)) == 0 {
			t.Errorf("%s: 藏在 3.1 关键字/驼峰/cluster_* 里的内部标识应被命中", name)
		}
	}
	// 路径级参数与参数 content。
	pathLevel := "  /pl:\n    parameters:\n      - name: node_id\n        in: query\n        schema: {type: string}\n" + opOpts{ID: "pl", Scope: "tenant"}.yaml("get")
	data := strings.Replace(string(spec(map[string]string{"/pl": opOpts{ID: "pl", Scope: "tenant"}.yaml("get")}, "")), "  /pl:\n"+opOpts{ID: "pl", Scope: "tenant"}.yaml("get"), pathLevel, 1)
	if fs := checkTenantLeak(mustLoad(t, []byte(data))); len(fs) != 1 {
		t.Errorf("路径级 node_id 参数应被命中: %v", fs)
	}
	content := "        - name: filter\n          in: query\n          content:\n            application/json:\n              schema:\n                type: object\n                properties:\n                  cluster_uid: {type: string}\n"
	if fs := checkTenantLeak(mustLoad(t, spec(map[string]string{"/pc": opOpts{ID: "pc", Scope: "tenant", Params: content}.yaml("get")}, ""))); len(fs) != 1 {
		t.Errorf("参数 content 里的 cluster_uid 应被命中: %v", fs)
	}
}

func TestListRecognitionExtended(t *testing.T) {
	arr := "type: array\nitems: {type: string}"
	data := "type: object\nproperties:\n  data:\n    type: array\n    items: {type: string}"
	for name, resp := range map[string]string{"顶层数组": arr, "data 集合": data} {
		doc := mustLoad(t, spec(map[string]string{"/l": opOpts{ID: "l", RespYAML: resp}.yaml("get")}, ""))
		if len(checkListPagination(doc)) == 0 {
			t.Errorf("%s 应被识别为 list 并要求分页", name)
		}
	}
	// 带 charset 的媒体类型。
	y := strings.Replace(string(spec(map[string]string{"/l": opOpts{ID: "l", RespYAML: arr}.yaml("get")}, "")), "application/json:", "application/json; charset=utf-8:", 1)
	if len(checkListPagination(mustLoad(t, []byte(y)))) == 0 {
		t.Error("application/json; charset=utf-8 的 list 应被识别")
	}
}

func TestNoInternalIDExtended(t *testing.T) {
	schemas := `    StrInt64:
      type: object
      properties:
        id: {type: string, format: int64}
    FK:
      type: object
      properties:
        tenant_id: {type: integer}
    AddlProps:
      type: object
      additionalProperties:
        type: object
        properties:
          id: {type: integer}
    PrefixI:
      type: array
      prefixItems:
        - type: object
          properties:
            id: {type: integer}
    UidOnly:
      type: object
      properties:
        tenant_uid: {type: string}
        id: {type: string}
`
	doc := mustLoad(t, spec(map[string]string{"/a": opOpts{ID: "a"}.yaml("get")}, schemas))
	fs := checkNoInternalID(doc)
	hit := map[string]bool{}
	for _, f := range fs {
		hit[f.Where] = true
	}
	for _, w := range []string{"#/components/schemas/StrInt64", "#/components/schemas/FK", "#/components/schemas/AddlProps{}", "#/components/schemas/PrefixI/prefixItems[0]"} {
		if !hit[w] {
			t.Errorf("应命中 %s: %v", w, fs)
		}
	}
	for w := range hit {
		if strings.Contains(w, "UidOnly") {
			t.Errorf("uid 形态不应命中: %s", w)
		}
	}
	// 整型 id 路径参数。
	p := opOpts{ID: "g", Params: "        - name: id\n          in: path\n          required: true\n          schema: {type: integer}\n"}
	if fs := checkNoInternalID(mustLoad(t, spec(map[string]string{"/things/{id}": p.yaml("get")}, ""))); len(fs) != 1 {
		t.Errorf("整型 id 路径参数应被命中: %v", fs)
	}
}

func TestFeatureTags(t *testing.T) {
	t.Run("billing 标签与显式清单的商业类 operation 缺 x-feature 被发现（负向样本）", func(t *testing.T) {
		doc := mustLoad(t, spec(map[string]string{
			"/b": opOpts{ID: "billed", Tags: "[billing]"}.yaml("get"),
			"/r": opOpts{ID: "register"}.yaml("post"),
			"/n": opOpts{ID: "plain"}.yaml("get"),
		}, ""))
		fs := checkFeatureTags(doc, []string{"register"})
		if len(fs) != 2 {
			t.Fatalf("应发现 billed 与 register 两处缺失，得到 %v", fs)
		}
		joined := fs[0].String() + fs[1].String()
		if !strings.Contains(joined, "GET /b") || !strings.Contains(joined, "POST /r") || strings.Contains(joined, "/n") {
			t.Fatalf("命中对象不对: %v", fs)
		}
	})
	t.Run("声明了 x-feature: commercial 通过（正向样本）", func(t *testing.T) {
		doc := mustLoad(t, spec(map[string]string{
			"/b": opOpts{ID: "billed", Tags: "[billing]", Feature: "commercial"}.yaml("get"),
			"/r": opOpts{ID: "register", Feature: "commercial"}.yaml("post"),
		}, ""))
		if fs := checkFeatureTags(doc, []string{"register"}); len(fs) != 0 {
			t.Fatalf("应通过: %v", fs)
		}
	})
	t.Run("未知取值被发现", func(t *testing.T) {
		doc := mustLoad(t, spec(map[string]string{"/x": opOpts{ID: "x", Feature: "premium"}.yaml("get")}, ""))
		if fs := checkFeatureTags(doc, nil); len(fs) != 1 {
			t.Fatalf("应发现未知取值: %v", fs)
		}
	})
	t.Run("真实契约：商业类 operation 全部已标注", func(t *testing.T) {
		doc := mustLoad(t, []byte(readRepoFile(t, "docs/api/openapi.yaml")))
		if fs := checkFeatureTags(doc, commercialOps); len(fs) != 0 {
			t.Fatalf("%v", fs)
		}
		have := map[string]bool{}
		billing := 0
		for _, o := range operations(doc) {
			have[o.Op.OperationID] = true
			for _, tg := range o.Op.Tags {
				if tg == commercialTag {
					billing++
				}
			}
		}
		for _, id := range commercialOps {
			if !have[id] {
				t.Errorf("商业入口清单里的 %s 不在契约中（清单陈旧）", id)
			}
		}
		if billing == 0 {
			t.Error("契约里没有 billing 标签的 operation：商业类判定失效")
		}
	})
}
