package leakguard

import (
	"strings"
	"testing"
)

func rules(vs []Violation) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString(v.Path + ":" + v.Rule + ";")
	}
	return b.String()
}

// 反样本：每一种泄漏形态都必须被发现。
func TestScanDetectsLeaks(t *testing.T) {
	cases := []struct {
		name, body string
		o          Options
		want       string
	}{
		{"node_id", `{"node_id":"x"}`, Options{}, "$.node_id:key"},
		{"node_uid 嵌套数组", `{"items":[{"a":{"node_uid":"01"}}]}`, Options{}, "$.items[0].a.node_uid:key"},
		{"cluster_name", `{"cluster_name":"c1"}`, Options{}, "$.cluster_name:key"},
		{"cluster_uids", `{"cluster_uids":[]}`, Options{}, "$.cluster_uids:key"},
		{"egress_node_ip", `{"egress_node_ip":"1.2.3.4"}`, Options{}, "$.egress_node_ip:key"},
		{"targets", `{"targets":[]}`, Options{}, "$.targets:key"},
		{"驼峰 nodeId", `{"nodeId":"x"}`, Options{}, "$.nodeId:key"},
		{"节点列表 nodes", `{"nodes":[]}`, Options{}, "$.nodes:key"},
		{"集群对象 cluster", `{"cluster":{"x":1}}`, Options{}, "$.cluster:key"},
		{"已登记内部取值（主机名）", `{"message":"upstream edge-bj-01.internal failed"}`, Options{Internal: []string{"edge-bj-01.internal"}}, "$.message:internal-value"},
		{"内部取值大小写不敏感", `{"a":"EDGE-BJ-01"}`, Options{Internal: []string{"edge-bj-01"}}, "$.a:internal-value"},
		{"内部取值出现在键名", `{"edge-bj-01":1}`, Options{Internal: []string{"edge-bj-01"}}, "$.edge-bj-01(key):internal-value"},
		{"内部错误全文：MySQL", `{"error":{"message":"Error 1062 (23000): Duplicate entry"}}`, Options{}, "$.error.message:internal-error"},
		{"内部错误全文：栈", `{"m":"goroutine 12 [running]:"}`, Options{}, "$.m:internal-error"},
		{"内部错误全文：dial tcp", `{"m":"dial tcp 10.0.0.1:3306: connect: refused"}`, Options{}, "$.m:internal-error"},
		{"IP 值（开启时）", `{"a":"x 192.168.1.20 y"}`, Options{ForbidIPValues: true}, "$.a:ip"},
		{"IPv6 值（开启时）", `{"a":"2001:db8::1"}`, Options{ForbidIPValues: true}, "$.a:ip"},
		{"非 JSON 原文", `upstream edge-bj-01 down`, Options{Internal: []string{"edge-bj-01"}}, "$:internal-value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rules(Scan([]byte(c.body), c.o))
			if !strings.Contains(got, strings.TrimSuffix(c.want, ";")) {
				t.Fatalf("应发现违规 %q，实际 %q", c.want, got)
			}
		})
	}
}

// 正样本：合法的租户可见响应不得误报（含聚合计数）。
func TestScanAllowsLegitimateResponses(t *testing.T) {
	ok := []struct {
		name, body string
		o          Options
	}{
		{"租户对象", `{"uid":"01M7QP5V346STC0AT56NME5NR3","name":"默认租户","slug":"default","status":"active","is_default":true,"settings":{"theme":"dark"}}`, Options{Internal: []string{"edge-bj-01"}, ForbidIPValues: true}},
		{"聚合计数 summary.healthy/total/offline", `{"summary":{"healthy":3,"total":4,"offline":1},"status":"running"}`, Options{ForbidIPValues: true}},
		{"分页列表", `{"items":[],"page":1,"page_size":20,"total":0}`, Options{}},
		{"统一错误体", `{"error":{"code":"resource.not_found","message":"resource not found","message_key":"errors.resource.not_found","details":[],"request_id":"01M7QP5V346STC0AT56NME5NR3"}}`, Options{ForbidIPValues: true}},
		{"含 nodes 子串但键不同", `{"node":"","announce":"nodes are managed by the platform"}`, Options{}},
		{"空响应体", ``, Options{}},
		{"版本号不是 IP", `{"version":"1.20","ratio":"3.5"}`, Options{ForbidIPValues: true}},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			// 注意 "node":"" 命中键规则——该用例只用于证明「值」里出现 nodes 子串不误报，键 node 按规则本就违规，故单独断言。
			vs := Scan([]byte(c.body), c.o)
			if c.name == "含 nodes 子串但键不同" {
				if rules(vs) != "$.node:key;" {
					t.Fatalf("只应因裸键 node 违规：%s", rules(vs))
				}
				return
			}
			if len(vs) > 0 {
				t.Fatalf("合法响应不应违规：%s", rules(vs))
			}
		})
	}
}

func TestForbiddenKeyTable(t *testing.T) {
	for _, k := range []string{"node_id", "node_uid", "node_name", "cluster_uid", "cluster_name", "cluster_id", "cluster_uids", "egress_node", "egress_node_ip", "targets", "node", "nodes", "cluster", "clusters", "nodeId", "clusterUid", "egressNodeIp"} {
		if !ForbiddenKey(k) {
			t.Errorf("%q 应被禁止", k)
		}
	}
	for _, k := range []string{"uid", "name", "summary", "healthy", "total", "offline", "target", "tenant_uid", "owner_uid", "nodeless", "announce", "Node"} {
		if ForbiddenKey(k) {
			t.Errorf("%q 不应被禁止", k)
		}
	}
}

func TestForbiddenKeysIn(t *testing.T) {
	v := map[string]any{"a": map[string]any{"cluster_uid": 1}, "b": []any{map[string]any{"ok": 1, "node_id": 2}}}
	got := strings.Join(ForbiddenKeysIn(v), ",")
	if got != "$.a.cluster_uid,$.b[0].node_id" {
		t.Fatalf("got %s", got)
	}
}
