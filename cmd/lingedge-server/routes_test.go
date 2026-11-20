package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// `lingedge-server routes` 导出的是 registerRoutes 装配的真实路由集合。Story 1.14 起含任务中心 4 个 operation、Story 1.16 起含认证 8 个 operation，
// 其余 M1 业务路由未实现（不用 501 占位）；`make openapi-check` 据此输出「M1 未实现 operation 清单」。
func TestRoutesSubcommandExportsManifest(t *testing.T) {
	var out, errb bytes.Buffer
	if code := routesMain(nil, &out, &errb); code != 0 {
		t.Fatalf("routes 退出码 %d: %s", code, errb.String())
	}
	var m []map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("清单不是 JSON 数组: %v\n%s", err, out.String())
	}
	got := map[string]bool{}
	for _, e := range m {
		got[e["operation_id"].(string)] = true
	}
	for _, id := range []string{"listTasks", "getTask", "listTaskEvents", "cancelTask",
		"login", "logout", "getMe", "getCsrfToken", "changePassword", "listSessions", "revokeSession", "listMyLoginLogs",
		"listUsers", "createUser", "getUser", "updateUser", "deleteUser", "setUserRoles", "unlockUser", "resetUserPassword", "revokeUserSessions",
		"listRoles", "createRole", "getRole", "updateRole", "deleteRole", "setRolePermissions", "listPermissions"} {
		if !got[id] {
			t.Errorf("清单缺少路由 %s", id)
		}
	}
	if len(m) != 33 {
		t.Errorf("当前应恰有 33 条路由（任务中心 4 + 认证 13 + 用户/角色/权限管理 16），实际 %d", len(m))
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
