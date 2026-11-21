package auditapi

import (
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/audit/repository"
)

func u64(n uint64) *uint64 { return &n }
func str(s string) *string { return &s }

// Story 1.22b：面向租户的操作者标签——租户用户显示展示名（缺失时脱敏邮箱），平台操作者（含 acting-as 与其令牌）统一为 platform，节点为 system；平台视角保持快照原样。
func TestToViewActorLabels(t *testing.T) {
	const email = "alice.smith@example.com"
	base := repository.LogRow{UID: "u1", ActorType: "user", ActorLabel: email, ActorUID: str("01ACTOR"), IP: []byte{192, 0, 2, 1}, UserAgent: str("ua")}
	with := func(f func(*repository.LogRow)) repository.LogRow { r := base; f(&r); return r }
	cases := []struct {
		name         string
		row          repository.LogRow
		tenantFacing bool
		wantLabel    string
		wantType     string
		hideIdentity bool // actor_uid/ip/user_agent 被抹去
	}{
		{"租户用户 + 展示名", with(func(r *repository.LogRow) { r.ActorTenantID = u64(7); r.ActorDisplayName = str("张三") }), true, "张三", "user", false},
		{"租户用户 + 空白展示名 → 脱敏邮箱", with(func(r *repository.LogRow) { r.ActorTenantID = u64(7); r.ActorDisplayName = str("  ") }), true, "a***@example.com", "user", false},
		{"租户用户 + 用户行缺失 → 脱敏邮箱", with(func(r *repository.LogRow) { r.ActorTenantID = u64(7) }), true, "a***@example.com", "user", false},
		{"平台用户直接操作租户资源（无 acting-as）→ platform", with(func(r *repository.LogRow) { r.ActorDisplayName = str("Root 管理员") }), true, "platform", "user", true},
		{"平台用户（actor_tenant_id=0）→ platform", with(func(r *repository.LogRow) { r.ActorTenantID = u64(0) }), true, "platform", "user", true},
		{"acting-as → platform", with(func(r *repository.LogRow) { r.ActingAs = true; r.ActorTenantID = u64(7) }), true, "platform", "user", true},
		{"平台令牌 → platform", with(func(r *repository.LogRow) { r.ActorType = "api_token"; r.ActorLabel = "le_abcdefgh" }), true, "platform", "user", true},
		{"租户令牌保留前缀", with(func(r *repository.LogRow) { r.ActorType = "api_token"; r.ActorLabel = "le_abcdefgh"; r.ActorTenantID = u64(7) }), true, "le_abcdefgh", "api_token", false},
		{"节点 → system", with(func(r *repository.LogRow) { r.ActorType = "node"; r.ActorLabel = "edge-hk-01" }), true, "system", "system", false},
		{"系统组件保留", with(func(r *repository.LogRow) { r.ActorType = "system"; r.ActorLabel = "scheduler" }), true, "scheduler", "system", false},
		{"平台视角保留快照", with(func(r *repository.LogRow) { r.ActorTenantID = u64(7); r.ActorDisplayName = str("张三") }), false, email, "user", false},
	}
	for _, tc := range cases {
		v := ToView(tc.row, tc.tenantFacing)
		if v.ActorLabel != tc.wantLabel || v.ActorType != tc.wantType {
			t.Errorf("%s：label/type = %q/%q，期望 %q/%q", tc.name, v.ActorLabel, v.ActorType, tc.wantLabel, tc.wantType)
		}
		hidden := v.ActorUID == nil && v.IP == nil && v.UserAgent == nil
		if tc.tenantFacing && hidden != tc.hideIdentity {
			t.Errorf("%s：身份/来源是否被抹去 = %v，期望 %v", tc.name, hidden, tc.hideIdentity)
		}
		if tc.tenantFacing && strings.Contains(v.ActorLabel, "@example.com") && !strings.HasPrefix(v.ActorLabel, "a***@") {
			t.Errorf("%s：租户视角不得出现完整邮箱: %q", tc.name, v.ActorLabel)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{"alice@example.com": "a***@example.com", "李雷@例.中国": "李***@例.中国", "x@y": "x***@y", "no-at": "***", "@x.com": "***", "": "***"} {
		if got := MaskEmail(in); got != want {
			t.Errorf("MaskEmail(%q) = %q，期望 %q", in, got, want)
		}
	}
}
