package server

import (
	"strings"
	"testing"
)

func TestParseRoles(t *testing.T) {
	ok := map[string]string{
		"":                          "api,grpc,scheduler,worker", // 默认全开
		"api":                       "api",
		"worker":                    "worker",
		"worker,api":                "api,worker", // 规范顺序
		"api, grpc ,scheduler":      "api,grpc,scheduler",
		"api,grpc,scheduler,worker": "api,grpc,scheduler,worker",
	}
	for in, want := range ok {
		rs, err := ParseRoles(in)
		if err != nil || rs.String() != want {
			t.Errorf("ParseRoles(%q) = %q, %v；期望 %q", in, rs, err, want)
		}
	}
	bad := map[string]string{
		"bogus":     "未知角色",
		"api,bogus": "未知角色",
		"API":       "未知角色", // 大小写敏感
		"api,,":     "空项",
		",":         "空项",
		" ":         "空项",
		"api,api":   "重复",
		"all":       "未知角色",
	}
	for in, frag := range bad {
		if _, err := ParseRoles(in); err == nil || !strings.Contains(err.Error(), frag) {
			t.Errorf("ParseRoles(%q) 应报错含 %q: %v", in, frag, err)
		}
	}
}

func TestRoleSetHas(t *testing.T) {
	rs, _ := ParseRoles("api,worker")
	if !rs.Has(RoleAPI) || !rs.Has(RoleWorker) || rs.Has(RoleGRPC) || rs.Has(RoleScheduler) {
		t.Fatalf("Has 不符: %v", rs)
	}
	if got := DefaultRoles().List(); len(got) != 4 {
		t.Fatalf("默认全开: %v", got)
	}
	// 每个骨架角色都登记了实现它的 Story（api 与 worker 是真实实现：worker 自 Story 1.14 起接入任务运行时）。
	for _, r := range AllRoles {
		if r != RoleAPI && r != RoleWorker && skeletonOwner[r] == "" {
			t.Errorf("角色 %s 缺少实现 Story 登记", r)
		}
	}
}
