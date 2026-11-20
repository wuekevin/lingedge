package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pathParams 生成路径参数声明（OpenAPI 要求每个路径参数都显式声明）。
func pathParams(names ...string) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString("        - name: " + n + "\n          in: path\n          required: true\n          schema: {type: string}\n")
	}
	return b.String()
}

func TestPathParamNamesConsistent(t *testing.T) {
	// 负向：同一位置两个不同的参数名（dns zones 的历史问题）。
	bad := mustLoad(t, spec(map[string]string{
		"/zones/{dnsZoneId}":      opOpts{ID: "getZone", Params: pathParams("dnsZoneId")}.yaml("get"),
		"/zones/{zoneId}/records": opOpts{ID: "listRecords", Params: pathParams("zoneId")}.yaml("get"),
		"/zones/{zoneId}/sync":    opOpts{ID: "syncZone", Params: pathParams("zoneId")}.yaml("post"),
		"/other/{a}/x":            opOpts{ID: "otherX", Params: pathParams("a")}.yaml("get"),
		"/other/{a}/y/{b}":        opOpts{ID: "otherY", Params: pathParams("a", "b")}.yaml("get"),
		"/third/{t}":              opOpts{ID: "thirdItem", Params: pathParams("t")}.yaml("get"),
		"/third/{t2}/items":       opOpts{ID: "thirdItems", Params: pathParams("t2")}.yaml("get"),
	}, ""))
	fs := checkPathParamNames(bad)
	if !hasRule(fs, "path-param-name") || len(fs) != 2 {
		t.Fatalf("应检出同位置参数名冲突: %v", fs)
	}
	joined := ""
	for _, f := range fs {
		joined += f.String() + "\n"
	}
	for _, want := range []string{"/zones/{zoneId}", "/third/{t2}"} {
		if !strings.Contains(joined, want) {
			t.Errorf("应指出 %s: %s", want, joined)
		}
	}
	// 正向：同位置同名、不同位置可以不同名；字面段与参数段并存不算冲突。
	good := mustLoad(t, spec(map[string]string{
		"/zones/{dnsZoneId}":         opOpts{ID: "getZone", Params: pathParams("dnsZoneId")}.yaml("get"),
		"/zones/{dnsZoneId}/records": opOpts{ID: "listRecords", Params: pathParams("dnsZoneId")}.yaml("get"),
		"/zones/export":              opOpts{ID: "exportZones"}.yaml("get"),
		"/sites/{siteId}/rules/{id}": opOpts{ID: "getRule", Params: pathParams("siteId", "id")}.yaml("get"),
		"/nodes/{nodeId}":            opOpts{ID: "getNode", Params: pathParams("nodeId")}.yaml("get"),
		// 不同方法是不同的基数树：同位置不同参数名不冲突（POST /x/{y}/test 与 GET /x/{z}）。
		"/chan/{chanId}":      opOpts{ID: "getChan", Params: pathParams("chanId")}.yaml("get"),
		"/chan/{channelId}/t": opOpts{ID: "testChan", Params: pathParams("channelId")}.yaml("post"),
	}, ""))
	if fs := checkPathParamNames(good); len(fs) != 0 {
		t.Fatalf("合法路径不应有违规: %v", fs)
	}
}

func routesSpec(t *testing.T) []byte {
	t.Helper()
	return spec(map[string]string{
		"/things":      opOpts{ID: "listThings", Perms: "[thing:read]"}.yaml("get") + opOpts{ID: "createThing", Perms: "[thing:write]"}.yaml("post"),
		"/things/{id}": opOpts{ID: "getThing", Params: pathParams("id")}.yaml("get") + opOpts{ID: "p2Thing", Phase: "P2", Params: pathParams("id")}.yaml("delete"),
	}, "")
}

func goodRoute() routeEntry {
	return routeEntry{OperationID: "listThings", Method: "GET", Path: "/things", Auth: "required", Permissions: []string{"thing:read"}, TenantScope: "tenant", Phase: "M1"}
}

func TestCheckRoutes(t *testing.T) {
	doc := mustLoad(t, routesSpec(t))

	// 正向：一致 → 无违规；M1 未实现清单恰为未注册的 M1 operation（P2 的不计入）。
	fs, todo := checkRoutes(doc, []routeEntry{goodRoute()})
	if len(fs) != 0 {
		t.Fatalf("一致的路由不应有违规: %v", fs)
	}
	if len(todo) != 2 || !strings.HasPrefix(todo[0], "createThing ") || !strings.HasPrefix(todo[1], "getThing ") {
		t.Fatalf("M1 未实现清单 = %v，期望 createThing/getThing（P2 不计）", todo)
	}
	// 全部注册后清单为空。
	all := []routeEntry{goodRoute(),
		{OperationID: "createThing", Method: "POST", Path: "/things", Auth: "required", Permissions: []string{"thing:write"}, TenantScope: "tenant", Phase: "M1"},
		{OperationID: "getThing", Method: "GET", Path: "/things/{id}", Auth: "required", Permissions: []string{"thing:read"}, TenantScope: "tenant", Phase: "M1"}}
	if fs, todo := checkRoutes(doc, all); len(fs) != 0 || len(todo) != 0 {
		t.Fatalf("全部注册: %v %v", fs, todo)
	}

	// 负向样本：每一种偏离都必须被检出。
	mut := func(name, rule string, f func(r *routeEntry)) {
		t.Helper()
		r := goodRoute()
		f(&r)
		fs, _ := checkRoutes(doc, []routeEntry{r})
		if !hasRule(fs, rule) {
			t.Errorf("%s: 应触发 %s，实际 %v", name, rule, fs)
		}
	}
	mut("operationId 不在契约", "route-not-in-openapi", func(r *routeEntry) { r.OperationID = "ghost" })
	mut("方法不一致", "route-meta-mismatch", func(r *routeEntry) { r.Method = "PUT" })
	mut("路径不一致", "route-meta-mismatch", func(r *routeEntry) { r.Path = "/stuff" })
	mut("权限不一致", "route-meta-mismatch", func(r *routeEntry) { r.Permissions = []string{"thing:write"} })
	mut("缺权限", "route-meta-mismatch", func(r *routeEntry) { r.Permissions = nil })
	mut("租户范围不一致", "route-meta-mismatch", func(r *routeEntry) { r.TenantScope = "platform" })
	mut("审计不一致", "route-meta-mismatch", func(r *routeEntry) { r.Audit = true })
	mut("再认证不一致", "route-meta-mismatch", func(r *routeEntry) { r.Reauth = true })
	mut("阶段不一致", "route-meta-mismatch", func(r *routeEntry) { r.Phase = "P2" })
	mut("认证不一致", "route-meta-mismatch", func(r *routeEntry) { r.Auth = "public" })
	mut("幂等不一致", "route-meta-mismatch", func(r *routeEntry) { r.Idempotent = true })
	// IMP-21：运行时路由路径含 ':'（独立于 openapi 文件检查）。
	mut("路由路径含冒号", "route-path-colon", func(r *routeEntry) { r.Path = "/things/{id}/rules:validate" })
	// 重复。
	if fs, _ := checkRoutes(doc, []routeEntry{goodRoute(), goodRoute()}); !hasRule(fs, "route-duplicate") {
		t.Errorf("重复 operationId 应被检出: %v", fs)
	}
}

func writeRoutes(t *testing.T, rs []routeEntry) string {
	t.Helper()
	b, err := json.Marshal(rs)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "routes.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReportRoutesExitCodes(t *testing.T) {
	doc := mustLoad(t, routesSpec(t))
	// 空清单：一致，但 M1 有未实现项；普通模式通过（输出清单），阶段出口模式失败。
	empty := writeRoutes(t, []routeEntry{})
	if code := reportRoutes(doc, empty, false); code != 0 {
		t.Errorf("普通模式应通过: %d", code)
	}
	if code := reportRoutes(doc, empty, true); code != 1 {
		t.Errorf("阶段出口模式在清单非空时应失败: %d", code)
	}
	full := writeRoutes(t, []routeEntry{goodRoute(),
		{OperationID: "createThing", Method: "POST", Path: "/things", Auth: "required", Permissions: []string{"thing:write"}, TenantScope: "tenant", Phase: "M1"},
		{OperationID: "getThing", Method: "GET", Path: "/things/{id}", Auth: "required", Permissions: []string{"thing:read"}, TenantScope: "tenant", Phase: "M1"}})
	if code := reportRoutes(doc, full, true); code != 0 {
		t.Errorf("清单为空时阶段出口应通过: %d", code)
	}
	// 清单里有契约外的路由：失败。
	if code := reportRoutes(doc, writeRoutes(t, []routeEntry{{OperationID: "ghost", Method: "GET", Path: "/ghost"}}), false); code != 1 {
		t.Errorf("契约外路由应失败: %d", code)
	}
	// 清单文件缺失/格式错误/含未知字段：2。
	if code := reportRoutes(doc, filepath.Join(t.TempDir(), "none.json"), false); code != 2 {
		t.Errorf("缺失清单应为 2: %d", code)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`[{"operation_id":"a","surprise":1}]`), 0o600)
	if code := reportRoutes(doc, bad, false); code != 2 {
		t.Errorf("未知字段应为 2: %d", code)
	}
}

func TestIdempotencyResponsesRule(t *testing.T) {
	idemParam := "        - name: Idempotency-Key\n          in: header\n          schema: {type: string}\n"
	resp422 := "      responses:\n        '422':\n          $ref: '#/components/responses/IdempotencyKeyReuse'\n"
	mk := func(withParam bool, with422 bool) string {
		o := opOpts{ID: "createThing"}
		if withParam {
			o.Params = idemParam
		}
		y := o.yaml("post")
		if with422 {
			y = strings.Replace(y, "      responses:\n", resp422, 1)
		}
		return y
	}
	doc := func(y string) []finding {
		src := string(spec(map[string]string{"/things": y}, ""))
		src += "  responses:\n    IdempotencyKeyReuse:\n      description: reuse\n"
		d := mustLoad(t, []byte(src))
		return checkIdempotencyResponses(d)
	}
	if fs := doc(mk(true, true)); len(fs) != 0 {
		t.Errorf("带键且带 422 应通过: %v", fs)
	}
	if fs := doc(mk(false, false)); len(fs) != 0 {
		t.Errorf("无键无 422 应通过: %v", fs)
	}
	if fs := doc(mk(true, false)); !hasRule(fs, "idempotency-422") {
		t.Errorf("带键缺 422 应被检出: %v", fs)
	}
	if fs := doc(mk(false, true)); !hasRule(fs, "idempotency-422") {
		t.Errorf("无键却有 422 应被检出: %v", fs)
	}
}
