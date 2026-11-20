package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// `lingedge-server routes` 导出的是 registerRoutes 装配的真实路由集合。M1 当前没有已实现的业务路由，清单为空数组
// （而不是 501 占位路由）；`make openapi-check` 因此输出完整的「M1 未实现 operation 清单」。
func TestRoutesSubcommandExportsManifest(t *testing.T) {
	var out, errb bytes.Buffer
	if code := routesMain(nil, &out, &errb); code != 0 {
		t.Fatalf("routes 退出码 %d: %s", code, errb.String())
	}
	var m []map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("清单不是 JSON 数组: %v\n%s", err, out.String())
	}
	for _, e := range m {
		if e["operation_id"] == "" {
			t.Errorf("清单条目缺 operation_id: %v", e)
		}
	}

	p := filepath.Join(t.TempDir(), "routes.json")
	if code := routesMain([]string{"-o", p}, &out, &errb); code != 0 {
		t.Fatalf("-o 退出码 %d: %s", code, errb.String())
	}
	if b, err := os.ReadFile(p); err != nil || len(b) == 0 {
		t.Fatalf("应写出文件: %v", err)
	}
	if code := routesMain([]string{"-bogus"}, &out, &errb); code != 2 {
		t.Errorf("未知参数应为 2: %d", code)
	}
}
