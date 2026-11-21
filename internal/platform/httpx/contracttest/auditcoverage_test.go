package contracttest_test

import (
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
)

func route(id, method string, audit bool) httpx.RouteMeta {
	return httpx.RouteMeta{OperationID: id, Method: method, Path: "/x/" + id, Auth: httpx.AuthRequired, Permissions: []string{"a:b"}, TenantScope: httpx.ScopeEither, Audit: audit, Phase: "M1"}
}

func TestCheckAuditCoverageNegativeSamples(t *testing.T) {
	reg := func(id string) bool { return id == "createThing" || id == "deleteThing" }
	exempt := map[string]string{"validateThing": "只读校验"}
	good := []httpx.RouteMeta{route("listThings", "GET", false), route("createThing", "POST", true), route("deleteThing", "DELETE", true), route("validateThing", "POST", false)}
	if p := contracttest.CheckAuditCoverage(good, exempt, reg); len(p) != 0 {
		t.Fatalf("合法路由集合不应有问题: %v", p)
	}
	cases := []struct {
		name   string
		routes []httpx.RouteMeta
		sub    string
	}{
		{"写 operation 缺 Audit 声明（POST）", append(good, route("updateThing", "POST", false)), "updateThing"},
		{"写 operation 缺 Audit 声明（PATCH/PUT/DELETE 同理）", append(good, route("patchThing", "PATCH", false), route("putThing", "PUT", false)), "patchThing"},
		{"声明 Audit 但没有记录者（未登记目录）", append(good, route("renameThing", "PATCH", true)), "renameThing"},
		{"空路由集合不得放行", nil, "空转"},
	}
	for _, c := range cases {
		p := contracttest.CheckAuditCoverage(c.routes, exempt, reg)
		if len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), c.sub) {
			t.Errorf("%s：应发现含 %q 的问题，实际 %v", c.name, c.sub, p)
		}
	}
	// 读 operation 声明 Audit 但未登记同样是问题；读 operation 不声明则无需登记。
	if p := contracttest.CheckAuditCoverage([]httpx.RouteMeta{route("getThing", "GET", true)}, nil, reg); len(p) != 1 {
		t.Errorf("读 operation 声明 Audit 但未登记应被发现: %v", p)
	}
}
