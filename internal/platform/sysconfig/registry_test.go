package sysconfig

import (
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

func norm(t *testing.T, key, raw string) (string, *ValidationError) {
	t.Helper()
	d, ok := Lookup(key)
	if !ok {
		t.Fatalf("键 %s 未登记", key)
	}
	b, err := d.Normalize(json.RawMessage(raw))
	return string(b), err
}

func TestNormalizeValidAndInvalid(t *testing.T) {
	good := []struct{ key, in, want string }{
		{KeyThrottleWindow, `900`, `900`},
		{KeyTOTPRequiredAdmin, `true`, `true`},
		{KeyBrandPrimaryColor, `"#aBc123"`, `"#aBc123"`},
		{KeyBrandName, `"凌界云"`, `"凌界云"`},
		{KeyBrandSupportEmail, `"ops@example.test"`, `"ops@example.test"`},
		{KeyBrandSupportEmail, `""`, `""`},
		{KeyConsoleURL, `"https://console.example.test/"`, `"https://console.example.test/"`},
		{KeyLocales, `["en","zh-CN"]`, `["en","zh-CN"]`},
		{KeyRunMode, `"commercial"`, `"commercial"`},
		{KeySSRFAllowCIDRs, `[{"cidr":"10.1.2.3/8","note":"内网"}]`, `[{"cidr":"10.0.0.0/8","note":"内网"}]`},
		{KeyMailPort, `465`, `465`},
	}
	for _, c := range good {
		got, err := norm(t, c.key, c.in)
		if err != nil || got != c.want {
			t.Errorf("%s %s: 得到 %q %v，期望 %q", c.key, c.in, got, err, c.want)
		}
	}
	// null / 空 = 恢复默认。
	if got, err := norm(t, KeyBrandName, `null`); got != "" || err != nil {
		t.Errorf("null 应表示恢复默认: %q %v", got, err)
	}
	bad := []struct{ key, in string }{
		{KeyThrottleWindow, `0`}, {KeyThrottleWindow, `86401`}, {KeyThrottleWindow, `"900"`}, {KeyThrottleWindow, `1.5`}, {KeyThrottleWindow, `1e3`}, {KeyThrottleWindow, `true`},
		{KeyTOTPRequiredAdmin, `"true"`}, {KeyTOTPRequiredAdmin, `1`},
		{KeyBrandPrimaryColor, `"red"`}, {KeyBrandPrimaryColor, `"#12345"`}, {KeyBrandPrimaryColor, `""`},
		{KeyBrandName, `""`}, {KeyBrandName, `"` + strings.Repeat("长", 30) + `"`}, {KeyBrandName, "\"a\\u0000b\""},
		{KeyBrandSupportEmail, `"not-an-email"`}, {KeyBrandSupportEmail, `"Ops <ops@example.test>"`},
		{KeyBrandLogoUID, `"not-a-uid"`}, {KeyBrandLogoUID, `""`},
		{KeyConsoleURL, `"ftp://x.example.test"`}, {KeyConsoleURL, `"https://operator@x.example.test"`}, {KeyConsoleURL, `"javascript:alert(1)"`},
		{KeyLocales, `[]`}, {KeyLocales, `["fr"]`}, {KeyLocales, `["en","en"]`}, {KeyLocales, `"en"`},
		{KeyRunMode, `"enterprise"`}, {KeyRunMode, `1`},
		{KeySSRFAllowCIDRs, `[{"cidr":"0.0.0.0/0"}]`}, {KeySSRFAllowCIDRs, `[{"cidr":"::/0"}]`}, {KeySSRFAllowCIDRs, `[{"cidr":"nope"}]`},
		{KeySSRFAllowCIDRs, `[{"cidr":"10.0.0.0/8"},{"cidr":"10.1.1.1/8"}]`}, {KeySSRFAllowCIDRs, `[{"cidr":"10.0.0.0/8","extra":1}]`}, {KeySSRFAllowCIDRs, `{}`},
		{KeyMailPort, `70000`}, {KeyThrottleWindow, `{`}, {KeyThrottleWindow, `1 2`},
	}
	for _, c := range bad {
		if got, err := norm(t, c.key, c.in); err == nil {
			t.Errorf("%s %s 应被拒绝，得到 %q", c.key, c.in, got)
		}
	}
}

// 每个键的默认值自身必须通过该键的校验（默认值不得违反 schema），且键、分组一致、无重复。
func TestRegistryDefaultsAreValid(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range Defs() {
		if seen[d.Key] {
			t.Errorf("键 %s 重复", d.Key)
		}
		seen[d.Key] = true
		if !contains(Groups, d.Group) {
			t.Errorf("%s: 分组 %q 不在 openapi 枚举内", d.Key, d.Group)
		}
		if d.Default != nil {
			if _, err := d.Normalize(d.DefaultJSON()); err != nil && !(d.Sensitive) {
				t.Errorf("%s: 默认值 %s 不通过自身校验: %v", d.Key, d.DefaultJSON(), err)
			}
		}
		if d.Doc == "" {
			t.Errorf("%s: 缺说明", d.Key)
		}
		if !d.Patchable && d.Dedicated == "" {
			t.Errorf("%s: 不可 PATCH 的键必须说明专用端点或原因", d.Key)
		}
	}
	for _, k := range []string{KeyRunMode, KeySSRFAllowCIDRs} {
		if d, _ := Lookup(k); d.Patchable {
			t.Errorf("%s 不得经通用 PATCH 修改（须经需再认证的专用端点）", k)
		}
	}
	if d, _ := Lookup(KeyBrandLogoUID); d.Default != nil {
		t.Error("logo 默认应为 null")
	}
	if d, _ := Lookup(KeyBrandLogoUID); true {
		if _, err := d.Normalize(json.RawMessage(`"` + "01M7QP5V346STC0AT56NME5NR3" + `"`)); err != nil || !uid.Valid("01M7QP5V346STC0AT56NME5NR3") {
			t.Errorf("合法 uid 应通过: %v", err)
		}
	}
}

// NFR-27：能力状态键只接受 unverified，且恒不可 PATCH；高级能力恒为 unverified。
func TestCapabilityStatusIsAlwaysUnverified(t *testing.T) {
	if len(AdvancedCapabilities) != 4 {
		t.Fatalf("高级能力应为 http3|l2_cache|dnssec|edge_compute: %v", AdvancedCapabilities)
	}
	for _, c := range AdvancedCapabilities {
		if CapabilityStatus()[c] != StatusUnverified {
			t.Errorf("%s 应为 unverified", c)
		}
		d, ok := Lookup(capabilityKey(c))
		if !ok || d.Patchable {
			t.Errorf("%s 状态键应已登记且不可 PATCH", c)
		}
		if _, err := d.Normalize(json.RawMessage(`"available"`)); err == nil {
			t.Errorf("%s: 校验器应拒绝 available", c)
		}
		if _, err := d.Normalize(json.RawMessage(`"unverified"`)); err != nil {
			t.Errorf("%s: unverified 应合法: %v", c, err)
		}
	}
	// 即使库里被带外写入 available，读取也不生效。
	sn := snapshot{vals: map[string]json.RawMessage{capabilityKey("http3"): json.RawMessage(`"available"`)}}
	if got := sn.value(nopLog(), capabilityKey("http3")); got != StatusUnverified {
		t.Errorf("带外写入的 available 不得生效: %v", got)
	}
}

func TestFeatureEnabled(t *testing.T) {
	if FeatureEnabled(RunModeSelfHosted, FeatureCommercial) || !FeatureEnabled(RunModeCommercial, FeatureCommercial) {
		t.Error("commercial 仅在商业模式启用")
	}
	if FeatureEnabled(RunModeCommercial, "unknown_feature") || FeatureEnabled("", FeatureCommercial) {
		t.Error("未知功能/未知模式一律未启用（fail-closed）")
	}
}

func TestCheckModulesNegativeSamples(t *testing.T) {
	route := func(id, path string) httpx.RouteMeta {
		return httpx.RouteMeta{OperationID: id, Method: "GET", Path: path}
	}
	mods := []ModuleInfo{{Name: "a", Paths: []string{"/a"}}}
	if p := CheckModules(mods, []httpx.RouteMeta{route("x", "/a/1")}, []ContractOp{{"x", "/a/1", "M1"}}); len(p) != 0 {
		t.Fatalf("一致应通过: %v", p)
	}
	if p := CheckModules(mods, nil, nil); len(p) != 1 || !strings.Contains(p[0], "虚报") {
		t.Errorf("登记了却没有路由应判虚报: %v", p)
	}
	if p := CheckModules(mods, []httpx.RouteMeta{route("x", "/a/1"), route("y", "/b/1")}, nil); len(p) != 1 || !strings.Contains(p[0], "不属于任何已交付模块") {
		t.Errorf("有路由未登记应判漏登: %v", p)
	}
	if p := CheckModules(mods, []httpx.RouteMeta{route("x", "/a/1")}, []ContractOp{{"x", "/a/1", "M1"}, {"z", "/a/2", "M1"}}); len(p) != 1 || !strings.Contains(p[0], "尚未注册") {
		t.Errorf("模块路径下仍有未注册的 M1 operation 应判半交付: %v", p)
	}
	if p := CheckModules(mods, []httpx.RouteMeta{route("x", "/a/1")}, []ContractOp{{"x", "/a/1", "M1"}, {"z", "/a/2", "P2"}}); len(p) != 0 {
		t.Errorf("非 M1 的未实现不算半交付: %v", p)
	}
	dup := []ModuleInfo{{Name: "a", Paths: []string{"/a"}}, {Name: "b", Paths: []string{"/a/x"}}}
	if p := CheckModules(dup, []httpx.RouteMeta{route("x", "/a/x")}, nil); len(p) == 0 {
		t.Error("同时属于多个模块应被发现")
	}
	// 前缀按路径段匹配：/auth 不得吞掉 /authx。
	if owner, ok := OwnerOf("/authx/1"); ok {
		t.Errorf("前缀必须按路径段匹配: %s", owner)
	}
	if owner, ok := OwnerOf("/auth/login"); !ok || owner != "auth" {
		t.Errorf("OwnerOf(/auth/login) = %q %v", owner, ok)
	}
}

func nopLog() *zap.Logger { return zap.NewNop() }

// Story 1.22b：安全敏感键清单里的每个键都必须是已注册、可经通用 PATCH 修改的键（防拼写错误让分级静默失效）；
// 品牌/入口 URL/语言不在清单内；运行模式与 SSRF 白名单不经 PATCH（它们的专用端点本来就有 x-reauth）。
func TestReauthRequiredKeysAreRegisteredAndPatchable(t *testing.T) {
	for k := range reauthRequiredKeys {
		d, ok := Lookup(k)
		if !ok || !d.Patchable || !d.ReauthRequired {
			t.Errorf("安全敏感键 %q 必须已注册、Patchable 且 ReauthRequired 被置位: %+v ok=%v", k, d, ok)
		}
	}
	for _, d := range Defs() {
		if d.ReauthRequired && !reauthRequiredKeys[d.Key] {
			t.Errorf("%s 的 ReauthRequired 不在清单内", d.Key)
		}
	}
	for _, k := range []string{KeyBrandName, KeyBrandNameEN, KeyBrandLogoUID, KeyBrandFaviconUID, KeyBrandPrimaryColor, KeyBrandSupportEmail, KeyAdminURL, KeyConsoleURL, KeyDocsURL, KeyStatusURL, KeyLocales} {
		if reauthRequiredKeys[k] {
			t.Errorf("%s 不应要求再认证", k)
		}
	}
	// 与认证模块读取的会话/节流/MFA/令牌键对齐：这些键全部是安全敏感键。
	for _, k := range []string{KeyThrottleWindow, KeyThrottleAccountMax, KeyThrottleIPMax, KeyTOTPRequiredAdmin, KeySessionIdleTTL, KeySessionAbsoluteTTL, KeyReauthWindow, KeyMFAWindow, KeyAPITokenDefaultTTL, KeyAPITokenMaxTTL} {
		if !reauthRequiredKeys[k] {
			t.Errorf("%s 应是安全敏感键", k)
		}
	}
}

// grpc.tls.san_extra（Story 1.23）：主机名/IP 列表，可为空；规范化、去重、拒绝非法。
func TestGRPCSANExtraNormalize(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`[]`, `[]`},
		{`["Edge.Example.TEST","10.0.0.5","::1","*.node.example.test"]`, `["edge.example.test","10.0.0.5","::1","*.node.example.test"]`},
		{`["::ffff:10.0.0.5"]`, `["10.0.0.5"]`},
	} {
		if got, err := norm(t, KeyGRPCSANExtra, c.in); err != nil || got != c.want {
			t.Errorf("%s -> %s %v, want %s", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{`"a.example.test"`, `[1]`, `[""]`, `["a b.test"]`, `["a.test:9443"]`, `["https://a.test"]`, `["a_b.test"]`, `["0.0.0.0"]`, `["fe80::1%eth0"]`,
		`["a.test","A.test"]`, `["x*.test"]`, `["-a.test"]`, `["a..test"]`} {
		if _, err := norm(t, KeyGRPCSANExtra, in); err == nil {
			t.Errorf("%s 应被拒绝", in)
		}
	}
	big := make([]string, 33)
	for i := range big {
		big[i] = "h" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".test"
	}
	b, _ := json.Marshal(big)
	if _, err := norm(t, KeyGRPCSANExtra, string(b)); err == nil {
		t.Error("超过 32 项应被拒绝")
	}
	d, _ := Lookup(KeyGRPCSANExtra)
	if !d.Patchable || !d.ReauthRequired {
		t.Errorf("san_extra 应可 PATCH 且需再认证: %+v", d)
	}
}
