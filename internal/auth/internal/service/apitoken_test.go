package service

import (
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/pkg/redact"
)

func TestGenerateAPITokenShapeAndEntropy(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		tok, prefix, hash, err := GenerateAPIToken()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(tok, "le_"+prefix+"_") || len(prefix) != 8 || len(tok) != apiTokenLen {
			t.Fatalf("令牌形态不符: %q (prefix %q)", tok, prefix)
		}
		if p, ok := ParseAPIToken(tok); !ok || p != prefix {
			t.Fatalf("生成的令牌应通过格式校验: %q", tok)
		}
		if hash != HashAPIToken(tok) || len(hash) != 64 {
			t.Fatalf("哈希应是 SHA-256 十六进制: %q", hash)
		}
		if seen[tok] || seen[prefix] {
			t.Fatalf("令牌/前缀不应重复")
		}
		seen[tok], seen[prefix] = true, true
		// 日志脱敏核心必须认得这种形态（G-5）：明文令牌出现在自由文本里会被遮蔽；单独的前缀不是秘密。
		if redact.String("authorization failed for "+tok) == "authorization failed for "+tok {
			t.Fatalf("redact 应识别令牌形态: %q", tok)
		}
	}
}

func TestParseAPITokenRejectsMalformed(t *testing.T) {
	good, _, _, _ := GenerateAPIToken()
	bad := []string{"", "le_", good[:len(good)-1], good + "x", "xx" + good[2:], strings.Replace(good, "le_", "LE_", 1),
		good[:3] + "ABCDEFGH" + good[11:], good[:3] + "abcdef01" + good[11:], good[:11] + "-" + good[12:],
		good[:len(good)-1] + "!", good[:len(good)-1] + " ", "Bearer " + good}
	for _, b := range bad {
		if _, ok := ParseAPIToken(b); ok {
			t.Errorf("不合格的令牌被接受: %q", b)
		}
	}
}

func TestNormalizeCIDRs(t *testing.T) {
	got, err := NormalizeCIDRs([]string{"10.1.2.3/8", "192.0.2.7", "2001:db8::1/32", "::ffff:198.51.100.9", "10.0.0.0/8", "2001:db8::/32 "})
	if err != nil {
		t.Fatal(err)
	}
	want := "10.0.0.0/8,192.0.2.7/32,198.51.100.9/32,2001:db8::/32"
	if strings.Join(got, ",") != want {
		t.Errorf("规范化结果 = %v，期望 %s", got, want)
	}
	for _, bad := range []string{"", "nope", "10.0.0.0/33", "10.0.0.0/-1", "0.0.0.0/0", "::/0", "fe80::1%eth0", "::ffff:10.0.0.0/64", "10.0.0.0/8/1", "300.1.1.1"} {
		if _, err := NormalizeCIDRs([]string{bad}); err == nil {
			t.Errorf("非法 CIDR 被接受: %q", bad)
		} else if e, ok := apierr.As(err); !ok || e.Code != apierr.ValidationFailed {
			t.Errorf("%q 应是 400 validation.failed: %v", bad, err)
		}
	}
	if _, err := NormalizeCIDRs(make([]string, 33)); err == nil {
		t.Error("超过 32 项应被拒绝")
	}
}

func TestIPAllowed(t *testing.T) {
	cidrs := []string{"10.0.0.0/8", "2001:db8::/32", "192.0.2.7/32"}
	for ip, want := range map[string]bool{
		"10.9.9.9": true, "11.0.0.1": false, "192.0.2.7": true, "192.0.2.8": false, "2001:db8:1::5": true, "2001:db9::1": false,
		"::ffff:10.1.1.1": true, // IPv4 映射的 IPv6 来源按 IPv4 比较
		"":                false, "not-an-ip": false, "10.0.0.1, 1.2.3.4": false,
	} {
		if got := ipAllowed(cidrs, ip); got != want {
			t.Errorf("ipAllowed(%q) = %v，期望 %v", ip, got, want)
		}
	}
}

func TestTokenDenyReasonOrderAndCoverage(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Second), now.Add(time.Hour)
	tid, ten, active := uint64(7), "t", "active"
	ok := func() domain.TokenAuthView {
		return domain.TokenAuthView{ExpiresAt: &future, UserStatus: domain.StatusActive, UserType: domain.UserTypePlatform}
	}
	tenantOK := func() domain.TokenAuthView {
		v := ok()
		v.UserType, v.TokenTenantID, v.MembershipOK, v.TenantUID, v.TenantStatus = domain.UserTypeTenant, &tid, true, &ten, &active
		return v
	}
	rv := now.Add(-time.Hour)
	cases := []struct {
		name string
		v    domain.TokenAuthView
		cidr []string
		ip   string
		want string
	}{
		{"平台令牌可用", ok(), nil, "", ""},
		{"租户令牌可用", tenantOK(), nil, "", ""},
		{"已吊销", func() domain.TokenAuthView { v := ok(); v.RevokedAt = &rv; return v }(), nil, "", "revoked"},
		{"恰好到期即过期", func() domain.TokenAuthView { v := ok(); v.ExpiresAt = &now; return v }(), nil, "", "expired"},
		{"已过期", func() domain.TokenAuthView { v := ok(); v.ExpiresAt = &past; return v }(), nil, "", "expired"},
		{"创建者停用", func() domain.TokenAuthView { v := ok(); v.UserStatus = "disabled"; return v }(), nil, "", "owner_unavailable"},
		{"创建者已删除", func() domain.TokenAuthView { v := ok(); v.UserDeletedAt = &past; return v }(), nil, "", "owner_unavailable"},
		{"平台令牌的创建者不是平台用户", func() domain.TokenAuthView { v := ok(); v.UserType = domain.UserTypeTenant; return v }(), nil, "", "owner_type_mismatch"},
		{"租户令牌的创建者不再是成员", func() domain.TokenAuthView { v := tenantOK(); v.MembershipOK = false; return v }(), nil, "", "tenant_unavailable"},
		{"租户已停用", func() domain.TokenAuthView { v := tenantOK(); s := "suspended"; v.TenantStatus = &s; return v }(), nil, "", "tenant_unavailable"},
		{"租户令牌的创建者是平台用户", func() domain.TokenAuthView { v := tenantOK(); v.UserType = domain.UserTypePlatform; return v }(), nil, "", "tenant_unavailable"},
		{"来源不在白名单", ok(), []string{"10.0.0.0/8"}, "192.0.2.1", "ip_not_allowed"},
		{"来源未知（fail-closed）", ok(), []string{"10.0.0.0/8"}, "", "ip_not_allowed"},
		{"来源在白名单", ok(), []string{"10.0.0.0/8"}, "10.1.1.1", ""},
	}
	for _, c := range cases {
		if got := tokenDenyReason(c.v, now, c.cidr, c.ip); got != c.want {
			t.Errorf("%s: 原因 = %q，期望 %q", c.name, got, c.want)
		}
	}
}
