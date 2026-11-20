package rbac

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

func TestRegistryWellFormed(t *testing.T) {
	if n := len(All()); n != 81 {
		t.Fatalf("注册表应与种子一致共 81 项（含 tenant:impersonate），实际 %d", n)
	}
	seen := map[string]bool{}
	for _, p := range All() {
		if seen[p.Code] {
			t.Errorf("权限串重复: %s", p.Code)
		}
		seen[p.Code] = true
		if p.Code != p.Resource+":"+p.Action {
			t.Errorf("%s: code 必须等于 resource:action（%s:%s）", p.Code, p.Resource, p.Action)
		}
		switch p.Scope {
		case ScopePlatform, ScopeTenant, ScopeBoth:
		default:
			t.Errorf("%s: 非法 scope %q", p.Code, p.Scope)
		}
	}
	imp, ok := Lookup(Impersonate)
	if !ok || imp.Scope != ScopePlatform {
		t.Fatalf("权限字典必须含 scope=platform 的 %s: %+v", Impersonate, imp)
	}
}

func TestGrantable(t *testing.T) {
	for _, c := range []struct {
		code             string
		tenantRole, plat bool
	}{
		{"site:write", true, true},  // tenant
		{"site:read", true, true},   // both
		{"user:write", false, true}, // platform
		{"tenant:impersonate", false, true},
		{"no:such", false, false},
	} {
		if got := GrantableToTenantRole(c.code); got != c.tenantRole {
			t.Errorf("GrantableToTenantRole(%s)=%v", c.code, got)
		}
		if got := GrantableToPlatformRole(c.code); got != c.plat {
			t.Errorf("GrantableToPlatformRole(%s)=%v", c.code, got)
		}
	}
}

func has(codes ...string) func(string) bool {
	m := map[string]bool{}
	for _, c := range codes {
		m[c] = true
	}
	return func(c string) bool { return m[c] }
}

// 授权矩阵（AC6）：主体类型 × x-tenant-scope × 有无 X-Tenant-ID × 有无 impersonate 权限 × 有无路由权限 × 读/写。
// 每一行的期望都是手写的（不是从实现推导）。
func TestAuthorizationMatrix(t *testing.T) {
	const (
		P = true  // platform 主体
		T = false // tenant 主体
	)
	pl, te, ei := tenantscope.RoutePlatform, tenantscope.RouteTenant, tenantscope.RouteEither
	type row struct {
		name     string
		platform bool
		route    tenantscope.RouteScope
		required []string
		held     []string
		readOnly bool
		target   bool
		own      bool
		wantMode Mode
		wantErr  error
	}
	rows := []row{
		// ---- 租户主体 ----
		{name: "租户主体访问 platform 路由 → 403", platform: T, route: pl, required: []string{"user:read"}, held: []string{"user:read"}, readOnly: true, wantErr: tenantscope.ErrForbiddenRoute},
		{name: "租户主体 tenant 路由 有权限 无目标 → 自己的租户", platform: T, route: te, required: []string{"site:write"}, held: []string{"site:write"}, wantMode: ModeOwnTenant},
		{name: "租户主体 tenant 路由 无权限 → 403", platform: T, route: te, required: []string{"site:write"}, held: []string{"site:read"}, wantErr: ErrMissingPermission},
		{name: "租户主体 either 路由 有权限 读 → 自己的租户", platform: T, route: ei, required: []string{"site:read"}, held: []string{"site:read"}, readOnly: true, wantMode: ModeOwnTenant},
		{name: "租户主体 either 路由 写 有权限 → 自己的租户", platform: T, route: ei, required: []string{"site:read"}, held: []string{"site:read"}, wantMode: ModeOwnTenant},
		{name: "租户主体 带自己的 X-Tenant-ID → 允许", platform: T, route: ei, required: []string{"site:read"}, held: []string{"site:read"}, readOnly: true, target: true, own: true, wantMode: ModeOwnTenant},
		{name: "租户主体 带他人的 X-Tenant-ID → 404（不泄漏存在性，且先于权限判定）", platform: T, route: ei, required: []string{"site:read"}, held: []string{"site:read"}, readOnly: true, target: true, own: false, wantErr: tenantscope.ErrTenantMismatch},
		{name: "租户主体 带他人的 X-Tenant-ID 且无权限 → 仍为 404", platform: T, route: te, required: []string{"site:read"}, target: true, own: false, wantErr: tenantscope.ErrTenantMismatch},
		{name: "租户主体持有 tenant:impersonate 也不能代租户（无此语义）", platform: T, route: te, required: []string{"site:write"}, held: []string{"site:write", Impersonate}, target: true, own: false, wantErr: tenantscope.ErrTenantMismatch},
		{name: "租户主体 路由无权限要求（自助）→ 允许", platform: T, route: ei, readOnly: true, wantMode: ModeOwnTenant},
		{name: "租户主体 多权限须全部具备：缺一 → 403", platform: T, route: te, required: []string{"site:write", "domain:write"}, held: []string{"site:write"}, wantErr: ErrMissingPermission},

		// ---- 平台主体 + platform 路由 ----
		{name: "平台主体 platform 路由 有权限 → 平台范围", platform: P, route: pl, required: []string{"user:read"}, held: []string{"user:read"}, readOnly: true, wantMode: ModePlatform},
		{name: "平台主体 platform 路由 无权限 → 403", platform: P, route: pl, required: []string{"user:read"}, held: []string{"role:read"}, readOnly: true, wantErr: ErrMissingPermission},
		{name: "平台主体 platform 路由 多权限缺一 → 403", platform: P, route: pl, required: []string{"user:write", "role:write"}, held: []string{"user:write"}, wantErr: ErrMissingPermission},
		{name: "平台主体 platform 路由 多权限齐全 → 允许", platform: P, route: pl, required: []string{"user:write", "role:write"}, held: []string{"user:write", "role:write"}, wantMode: ModePlatform},
		{name: "平台主体 platform 路由 带 X-Tenant-ID → 400（平台级资源不接受目标租户）", platform: P, route: pl, required: []string{"user:read"}, held: []string{"user:read", Impersonate}, readOnly: true, target: true, wantErr: tenantscope.ErrInvalidScope},
		{name: "平台主体 platform 路由 带 X-Tenant-ID 且无权限 → 403（先权限）", platform: P, route: pl, required: []string{"user:read"}, readOnly: true, target: true, wantErr: ErrMissingPermission},

		// ---- 平台主体 + either 路由 ----
		{name: "平台主体 either 读 无 X-Tenant-ID 有权限 → 跨租户只读", platform: P, route: ei, required: []string{"site:read"}, held: []string{"site:read"}, readOnly: true, wantMode: ModeCrossTenantRead},
		{name: "平台主体 either 读 无 X-Tenant-ID 无权限 → 403", platform: P, route: ei, required: []string{"site:read"}, readOnly: true, wantErr: ErrMissingPermission},
		{name: "平台主体 either 读 无 X-Tenant-ID 有 impersonate 无路由权限 → 403", platform: P, route: ei, required: []string{"site:read"}, held: []string{Impersonate}, readOnly: true, wantErr: ErrMissingPermission},
		{name: "平台主体 either 写 无 X-Tenant-ID 有权限 → 400（写须经 acting-as）", platform: P, route: ei, required: []string{"site:read"}, held: []string{"site:read"}, wantErr: tenantscope.ErrTenantRequired},
		{name: "平台主体 either 无权限要求 读 → 跨租户只读", platform: P, route: ei, readOnly: true, wantMode: ModeCrossTenantRead},
		{name: "平台主体 either 带 X-Tenant-ID 同时有 impersonate 与路由权限 → acting-as", platform: P, route: ei, required: []string{"site:read"}, held: []string{"site:read", Impersonate}, readOnly: true, target: true, wantMode: ModeActingAs},
		{name: "平台主体 either 写 带 X-Tenant-ID 同时有 → acting-as", platform: P, route: ei, required: []string{"site:read"}, held: []string{"site:read", Impersonate}, target: true, wantMode: ModeActingAs},
		{name: "平台主体 either 带 X-Tenant-ID 仅有路由权限 → 403（缺 impersonate）", platform: P, route: ei, required: []string{"site:read"}, held: []string{"site:read"}, readOnly: true, target: true, wantErr: tenantscope.ErrImpersonationDenied},
		{name: "平台主体 either 带 X-Tenant-ID 仅有 impersonate → 403（缺路由权限）", platform: P, route: ei, required: []string{"site:read"}, held: []string{Impersonate}, readOnly: true, target: true, wantErr: ErrMissingPermission},
		{name: "平台主体 either 带 X-Tenant-ID 两者皆无 → 403", platform: P, route: ei, required: []string{"site:read"}, readOnly: true, target: true, wantErr: tenantscope.ErrImpersonationDenied},
		{name: "平台主体 either 带 X-Tenant-ID 路由权限的 scope 为 platform → 403（acting-as 只认 tenant/both 权限）", platform: P, route: ei, required: []string{"user:read"}, held: []string{"user:read", Impersonate}, readOnly: true, target: true, wantErr: ErrMissingPermission},
		{name: "平台主体 either 带 X-Tenant-ID 路由权限未注册 → 403（fail-closed）", platform: P, route: ei, required: []string{"no:such"}, held: []string{"no:such", Impersonate}, readOnly: true, target: true, wantErr: ErrMissingPermission},

		// ---- 平台主体 + tenant 路由 ----
		{name: "平台主体 tenant 路由 无 X-Tenant-ID 有权限 → 400（只能经 acting-as）", platform: P, route: te, required: []string{"site:write"}, held: []string{"site:write", Impersonate}, wantErr: tenantscope.ErrTenantRequired},
		{name: "平台主体 tenant 路由 读 无 X-Tenant-ID 有权限 → 仍为 400（tenant 路由无跨租户只读）", platform: P, route: te, required: []string{"site:write"}, held: []string{"site:write"}, readOnly: true, wantErr: tenantscope.ErrTenantRequired},
		{name: "平台主体 tenant 路由 无 X-Tenant-ID 无权限 → 403", platform: P, route: te, required: []string{"site:write"}, wantErr: ErrMissingPermission},
		{name: "平台主体 tenant 路由 带 X-Tenant-ID 同时有 → acting-as", platform: P, route: te, required: []string{"site:write"}, held: []string{"site:write", Impersonate}, target: true, wantMode: ModeActingAs},
		{name: "平台主体 tenant 路由 带 X-Tenant-ID 缺 impersonate → 403", platform: P, route: te, required: []string{"site:write"}, held: []string{"site:write"}, target: true, wantErr: tenantscope.ErrImpersonationDenied},
		{name: "平台主体 tenant 路由 带 X-Tenant-ID 缺路由权限 → 403", platform: P, route: te, required: []string{"site:write"}, held: []string{Impersonate}, target: true, wantErr: ErrMissingPermission},
		{name: "平台主体 tenant 路由 多权限 acting-as 缺一 → 403", platform: P, route: te, required: []string{"site:write", "domain:write"}, held: []string{"site:write", Impersonate}, target: true, wantErr: ErrMissingPermission},

		// ---- 异常输入 ----
		{name: "无效路由类型 → 拒绝", platform: P, route: tenantscope.RouteScope("public"), required: []string{"x:y"}, held: []string{"x:y"}, wantErr: tenantscope.ErrInvalidScope},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			d := Decide(Input{Route: r.route, Required: r.required, Platform: r.platform, Has: has(r.held...), ReadOnly: r.readOnly, HasTarget: r.target, TargetIsOwn: r.own})
			if r.wantErr != nil {
				if !errors.Is(d.Err, r.wantErr) {
					t.Fatalf("期望错误 %v，得到 %+v", r.wantErr, d)
				}
				if d.Mode != "" {
					t.Errorf("拒绝时不得带 Mode: %q", d.Mode)
				}
				return
			}
			if d.Err != nil || d.Mode != r.wantMode {
				t.Fatalf("期望 Mode=%s，得到 %+v", r.wantMode, d)
			}
		})
	}
}

func TestDecideFailsClosedWithoutHasFunc(t *testing.T) {
	d := Decide(Input{Route: tenantscope.RoutePlatform, Platform: true})
	if !errors.Is(d.Err, ErrMissingPermission) || d.Mode != "" {
		t.Fatalf("Has 为 nil 必须拒绝（即使路由无权限要求）: %+v", d)
	}
}

// 全组合不变量（192 种）：不论组合如何，下列安全属性必须成立——它们独立于上面的手写表，防止决策表遗漏某个组合时放行。
func TestAuthorizationInvariantsExhaustive(t *testing.T) {
	routes := []tenantscope.RouteScope{tenantscope.RoutePlatform, tenantscope.RouteTenant, tenantscope.RouteEither}
	bools := []bool{false, true}
	n := 0
	for _, platform := range bools {
		for _, route := range routes {
			for _, target := range bools {
				for _, own := range bools {
					for _, imp := range bools {
						for _, perm := range bools {
							for _, ro := range bools {
								n++
								var held []string
								if imp {
									held = append(held, Impersonate)
								}
								if perm {
									held = append(held, "site:read")
								}
								d := Decide(Input{Route: route, Required: []string{"site:read"}, Platform: platform, Has: has(held...), ReadOnly: ro, HasTarget: target, TargetIsOwn: own})
								name := fmt.Sprintf("platform=%v route=%s target=%v own=%v imp=%v perm=%v ro=%v", platform, route, target, own, imp, perm, ro)
								if (d.Err == nil) == (d.Mode == "") {
									t.Fatalf("%s: Err 与 Mode 必须恰有一个: %+v", name, d)
								}
								if d.Err != nil {
									continue
								}
								// 放行的前提：必须持有路由权限（本组合路由声明了权限）。
								if !perm {
									t.Fatalf("%s: 未持有路由权限却被放行: %+v", name, d)
								}
								switch d.Mode {
								case ModeOwnTenant:
									if platform || route == tenantscope.RoutePlatform || (target && !own) {
										t.Fatalf("%s: 租户范围只能给租户主体、非 platform 路由、且目标是自己的租户: %+v", name, d)
									}
								case ModePlatform:
									if !platform || route != tenantscope.RoutePlatform || target {
										t.Fatalf("%s: 平台范围只能给平台主体访问 platform 路由且不带目标: %+v", name, d)
									}
								case ModeCrossTenantRead:
									if !platform || route != tenantscope.RouteEither || target || !ro {
										t.Fatalf("%s: 跨租户只读只能是平台主体、either 路由、无目标、只读方法: %+v", name, d)
									}
								case ModeActingAs:
									if !platform || route == tenantscope.RoutePlatform || !target || !imp {
										t.Fatalf("%s: acting-as 只能是平台主体、非 platform 路由、带目标且持有 impersonate: %+v", name, d)
									}
								default:
									t.Fatalf("%s: 未知 Mode %q", name, d.Mode)
								}
							}
						}
					}
				}
			}
		}
	}
	if n != 192 {
		t.Fatalf("组合数 %d", n)
	}
}
