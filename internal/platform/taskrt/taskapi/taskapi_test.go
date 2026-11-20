package taskapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	"github.com/wuekevin/lingedge/internal/platform/taskrt/taskapi"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

func newRouter(t *testing.T, o httpx.Options) *httpx.Router {
	t.Helper()
	l, err := logx.New(logx.Options{Level: "info", Output: &strings.Builder{}})
	if err != nil {
		t.Fatal(err)
	}
	o.Logger, o.IDs = l, uid.NewGenerator(clock.System())
	r, err := httpx.NewRouter(o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// AC2：四个路由经 httpx.Register 注册，元数据与 openapi.yaml 的 x-* 完全一致，且 ManifestOnly 模式（store=nil）也能注册。
func TestRoutesMatchOpenAPI(t *testing.T) {
	spec := contracttest.LoadRepo(t)
	r := newRouter(t, httpx.Options{ManifestOnly: true})
	if err := taskapi.Register(r, nil); err != nil {
		t.Fatal(err)
	}
	routes := r.Routes()
	if len(routes) != 4 {
		t.Fatalf("应注册 4 个路由，实际 %d", len(routes))
	}
	if probs := spec.CheckRoutes(routes); len(probs) > 0 {
		t.Fatalf("路由元数据与 openapi.yaml 不一致:\n%s", strings.Join(probs, "\n"))
	}
}

// 生产装配里钩子是安全拒绝默认实现：任务中心路由对任何请求都是 401（认证钩子先拒绝），不会放行。
func TestDefaultDenyHooks(t *testing.T) {
	r := newRouter(t, httpx.Options{})
	if err := taskapi.Register(r, nil); err != nil {
		t.Fatal(err)
	}
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}
	const id = "01J8Z3Q9V6X2N4M5K7R8T0WABC"
	for _, c := range [][2]string{{"GET", "/api/v1/tasks"}, {"GET", "/api/v1/tasks/" + id}, {"GET", "/api/v1/tasks/" + id + "/events"}, {"POST", "/api/v1/tasks/" + id + "/cancel"}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c[0], c[1], nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: 默认钩子应 401，实际 %d", c[0], c[1], rec.Code)
		}
	}
}

func TestViewHidesPlatformIdentifiersFromTenants(t *testing.T) {
	tenantScope, err := tenantscope.FromTask(7, 1)
	if err != nil {
		t.Fatal(err)
	}
	task := taskrt.Task{
		UID: "01J8Z3Q9V6X2N4M5K7R8T0WABC", Type: taskrt.TypeCachePurgeURL, Status: taskrt.StatusSuccess, RequestID: "01J8Z3Q9V6X2N4M5K7R8T0WABD",
		Payload:   json.RawMessage(`{"site_uid":"x","cluster_uid":"c1","nested":{"node_uid":"n1","keep":1},"resource_type":"cache_task","resource_uid":"01J8Z3Q9V6X2N4M5K7R8T0WABE"}`),
		Result:    json.RawMessage(`{"nodes":[1,2],"done":true,"` + "pass" + `word":"p"}`),
		CreatedAt: time.Date(2026, 11, 21, 1, 2, 3, 456_000_000, time.UTC),
	}
	v := taskapi.View(task, tenantScope)
	b, _ := json.Marshal(v)
	s := string(b)
	for _, bad := range []string{"cluster_uid", "node_uid", `"nodes"`, `"p"`} {
		if strings.Contains(s, bad) {
			t.Errorf("租户视角不应含 %s: %s", bad, s)
		}
	}
	if !strings.Contains(s, `"keep":1`) || !strings.Contains(s, `"created_at":"2026-11-21T01:02:03.456Z"`) || v.Resource == nil || v.Resource.Type != "cache_task" {
		t.Errorf("视图内容不符: %s", s)
	}
	pv, _ := json.Marshal(taskapi.View(task, tenantscope.PlatformForJob(1)))
	if !strings.Contains(string(pv), "cluster_uid") || strings.Contains(string(pv), `"p"`) {
		t.Errorf("平台视角应保留 cluster_uid 但仍脱敏口令: %s", pv)
	}
}

// Story 1.20（R-T1，BLK-02）：租户视角还须剔除 egress_node*、targets、驼峰形态的内部标识，聚合计数 summary 保留；平台视角原样保留。
func TestViewHidesEgressNodeAndTargetsFromTenants(t *testing.T) {
	tenantScope, err := tenantscope.FromTask(7, 1)
	if err != nil {
		t.Fatal(err)
	}
	task := taskrt.Task{
		UID: "01J8Z3Q9V6X2N4M5K7R8T0WABC", Type: taskrt.TypeCachePurgeURL, Status: taskrt.StatusSuccess, RequestID: "01J8Z3Q9V6X2N4M5K7R8T0WABD",
		Payload: json.RawMessage(`{"egress_node_ip":"203.0.113.9","egress_node":"e","targets":["edge-1"],"clusters":[1],"nodeId":"n","summary":{"healthy":3,"total":4,"offline":1}}`),
	}
	s := string(mustJSON(t, taskapi.View(task, tenantScope)))
	for _, bad := range []string{"egress_node", "targets", `"clusters"`, "nodeId", "203.0.113.9", "edge-1"} {
		if strings.Contains(s, bad) {
			t.Errorf("租户视角不应含 %s: %s", bad, s)
		}
	}
	if !strings.Contains(s, `"summary":{"healthy":3,"offline":1,"total":4}`) {
		t.Errorf("聚合计数应保留: %s", s)
	}
	if pv := string(mustJSON(t, taskapi.View(task, tenantscope.PlatformForJob(1)))); !strings.Contains(pv, "egress_node_ip") || !strings.Contains(pv, "targets") {
		t.Errorf("平台视角应原样保留: %s", pv)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
