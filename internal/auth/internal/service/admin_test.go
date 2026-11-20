package service

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 服务端生成的一次性口令：满足口令策略、只含字母表内字符、每次不同、无易混淆字符。
func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if n := utf8.RuneCountInString(pw); n != genPasswordLen || n < minNewPassword {
			t.Fatalf("长度 %d", n)
		}
		for _, r := range pw {
			if !strings.ContainsRune(oneTimeCharset, r) {
				t.Fatalf("字符 %q 不在字母表内", r)
			}
		}
		if strings.ContainsAny(pw, "0O1lI") {
			t.Fatalf("含易混淆字符: %s", pw)
		}
		if err := validateNewPassword("p", pw, "a@example.test"); err != nil {
			t.Fatalf("生成的口令必须满足口令策略: %v", err)
		}
		if seen[pw] {
			t.Fatal("口令重复")
		}
		seen[pw] = true
	}
}

func TestValidateRolePermissionsScopeRule(t *testing.T) {
	if _, err := validateRolePermissions("tenant", []string{"site:read", "site:write", "site:read"}); err != nil {
		t.Errorf("tenant/both 权限应允许: %v", err)
	}
	for _, bad := range []string{"user:read", "tenant:impersonate", "node:manage", "no:such"} {
		if _, err := validateRolePermissions("tenant", []string{bad}); err == nil {
			t.Errorf("租户角色含 %s 必须被拒绝", bad)
		}
	}
	got, err := validateRolePermissions("platform", []string{"user:read", "site:write", "user:read"})
	if err != nil || len(got) != 2 {
		t.Errorf("平台角色可含任意已注册权限并去重: %v %v", got, err)
	}
	if _, err := validateRolePermissions("platform", []string{"no:such"}); err == nil {
		t.Error("未注册权限必须被拒绝")
	}
}
