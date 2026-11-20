package service

import (
	"strings"
	"testing"
)

func TestValidEmail(t *testing.T) {
	for _, e := range []string{"a@b.test", "ops.team+x@example.co", "user_1@sub.example.org"} {
		if !validEmail(e) {
			t.Errorf("%q 应合法", e)
		}
	}
	for _, e := range []string{"", "x", "a@b", "a b@c.test", "<a@b.test>", "a@b.test,c@d.test", "\"a\"@b.test", strings.Repeat("a", 250) + "@b.test", "Name <a@b.test>"} {
		if validEmail(e) {
			t.Errorf("%q 应非法", e)
		}
	}
}

func TestParseMemberRolesAndSettings(t *testing.T) {
	if got := ParseMemberRoles([]byte(`["tenant_viewer","site_operator"]`)); strings.Join(got, ",") != "site_operator,tenant_viewer" {
		t.Errorf("角色应排序: %v", got)
	}
	for _, raw := range [][]byte{nil, []byte(""), []byte("null"), []byte("{bad")} {
		if got := ParseMemberRoles(raw); got == nil || len(got) != 0 {
			t.Errorf("%q 应得到空切片（非 nil）: %#v", raw, got)
		}
	}
	if ParseSettings(nil) != nil || ParseSettings([]byte("[1]")) != nil || ParseSettings([]byte(`{"a":1}`))["a"] != float64(1) {
		t.Error("ParseSettings: NULL/非对象 → nil，对象 → map")
	}
}

func TestDedupeAndControl(t *testing.T) {
	if got := dedupe([]string{"a", "b", "a", "b", "c"}); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("dedupe: %v", got)
	}
	if !hasControl("a\x00b") || !hasControl("a\x7fb") || hasControl("正常 name-1") {
		t.Error("hasControl")
	}
}
