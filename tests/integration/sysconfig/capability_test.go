package sysconfigit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/leakguard"
	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// Story 1.22 AC3（FR-31、NFR-27）：/auth/me 返回权限集与 features{run_mode, modules_delivered[], capability_status{}}。
func TestMeFeaturesAndCapabilityRegistry(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	tn := w.SeedTenant("cap")
	tu := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(tu, "tenant_owner")
	ts := w.Login(tu)

	for name, s := range map[string]apistack.Session{"平台管理员": adm, "租户": ts} {
		r := s.Do("GET", "/auth/me", nil)
		w.Contract("getMe", "GET", "/auth/me", r)
		if r.Status != 200 {
			t.Fatalf("%s /auth/me: %d %s", name, r.Status, r.Body)
		}
		m := r.JSON(t)
		var f sysconfig.FeaturesView
		b, _ := json.Marshal(m["features"])
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatal(err)
		}
		if f.RunMode != "self_hosted" {
			t.Errorf("%s: run_mode=%s", name, f.RunMode)
		}
		if strings.Join(f.ModulesDelivered, ",") != strings.Join(sysconfig.ModulesDelivered(), ",") || len(f.ModulesDelivered) == 0 {
			t.Errorf("%s: modules_delivered=%v 应等于能力注册表 %v", name, f.ModulesDelivered, sysconfig.ModulesDelivered())
		}
		if len(f.CapabilityStatus) != 4 {
			t.Errorf("%s: capability_status 应含 4 个高级能力: %v", name, f.CapabilityStatus)
		}
		for _, c := range []string{"http3", "l2_cache", "dnssec", "edge_compute"} {
			if f.CapabilityStatus[c] != "unverified" {
				t.Errorf("%s: %s 应恒为 unverified: %v", name, c, f.CapabilityStatus)
			}
		}
		// capabilities（旧形态）与 features 同源。
		if strings.Join(toStrings(nested(t, m, "capabilities", "modules")), ",") != strings.Join(f.ModulesDelivered, ",") {
			t.Errorf("%s: capabilities.modules 应与 features.modules_delivered 同源", name)
		}
	}
	// 租户可见响应不得含内部标识（R-T1）：模块键、能力键都是通用词。
	in := w.SeedInternals()
	if v := leakguard.Scan(ts.Do("GET", "/auth/me", nil).Body, leakguard.Options{Internal: in.Strings()}); len(v) > 0 {
		t.Errorf("租户 /auth/me 泄漏内部标识: %v", v)
	}

	// modules_delivered 与真实路由一致（不虚报、不漏登、不半交付）：用本测试装配出的真实路由集合核对。
	var contract []sysconfig.ContractOp
	for _, id := range w.Spec.OperationIDs() {
		op, _ := w.Spec.Operation(id)
		contract = append(contract, sysconfig.ContractOp{OperationID: id, Path: op.Meta.Path, Phase: op.Meta.Phase})
	}
	if probs := sysconfig.CheckModules(sysconfig.DeliveredModules, w.Router.Routes(), contract); len(probs) > 0 {
		t.Errorf("modules_delivered 与真实路由不一致: %v", probs)
	}
	// 未交付的模块不得出现。
	for _, m := range []string{"sites", "nodes", "clusters", "certificates", "cache", "metrics", "billing", "dns", "waf", "cms"} {
		for _, d := range sysconfig.ModulesDelivered() {
			if d == m {
				t.Errorf("未交付的模块 %q 不得出现在 modules_delivered", m)
			}
		}
	}
}

// NFR-27：高级能力恒为 unverified，**试图置为 available 的写入口被拒绝**——经通用设置 API（PATCH /admin/settings）与服务层校验器两条路径。
func TestAdvancedCapabilityCannotBeSetAvailable(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	for _, c := range []string{"http3", "l2_cache", "dnssec", "edge_compute"} {
		key := "capability." + c + ".status"
		before := auditCount(w, "action = 'setting.update'")
		for _, v := range []any{"available", "unverified", "enabled", true, nil} {
			if v == nil {
				continue // null = 恢复默认：对只读键同样拒绝，见下
			}
			r := patch(adm, map[string]any{key: v})
			if r.Status != 400 || r.ErrCode(t) != "validation.failed" {
				t.Fatalf("%s=%v 的写入应被拒绝 400 validation.failed: %d %s", key, v, r.Status, r.Body)
			}
			if !strings.Contains(string(r.Body), "NFR-27") || !strings.Contains(string(r.Body), "values."+key) {
				t.Errorf("错误应指出字段与原因: %s", r.Body)
			}
		}
		if r := patch(adm, map[string]any{key: nil}); r.Status != 400 {
			t.Errorf("%s=null 也不得写入: %d", key, r.Status)
		}
		if settingRow(w, key) != 0 {
			t.Fatalf("%s 被拒绝后库里不得有行", key)
		}
		if auditCount(w, "action = 'setting.update'") != before {
			t.Error("被拒绝的写入不得产生审计")
		}
		// 与合法键混在同一批里：整批拒绝，合法键也不写。
		r := patch(adm, map[string]any{key: "available", "brand.name": "不应生效"})
		if r.Status != 400 || settingRow(w, "brand.name") != 0 {
			t.Fatalf("整批应原子拒绝: %d %s", r.Status, r.Body)
		}
	}
	// 服务层校验器同样拒绝（定义级：能力状态键的枚举只含 unverified）。
	d, _ := sysconfig.Lookup("capability.http3.status")
	if _, verr := d.Normalize(json.RawMessage(`"available"`)); verr == nil {
		t.Error("服务层校验器应拒绝 available")
	}
	// 带外写入（如被人直接改库）的 available 也不生效：/auth/me 与设置读取都仍是 unverified。
	w.Root("INSERT INTO §.system_settings (uid, setting_key, value_json, group_name) VALUES (?, 'capability.dnssec.status', CAST('\"available\"' AS JSON), 'general')", apistack.IDs.MustNew())
	me := adm.Do("GET", "/auth/me", nil)
	if got := nested(t, me.JSON(t), "features", "capability_status", "dnssec"); got != "unverified" {
		t.Fatalf("带外写入的 available 不得生效: %v", got)
	}
	g := adm.Do("GET", "/admin/settings?group=general", nil)
	if got := g.JSON(t)["capability.dnssec.status"]; got != "unverified" {
		t.Fatalf("设置读取同样恒为 unverified: %v", got)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, e := range v.([]any) {
		out = append(out, e.(string))
	}
	return out
}
