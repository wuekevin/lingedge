package tenantscope_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

type fakePrincipal struct {
	actor       uint64
	platform    bool
	tenant      uint64
	impersonate bool
}

func (p fakePrincipal) ActorID() uint64  { return p.actor }
func (p fakePrincipal) IsPlatform() bool { return p.platform }
func (p fakePrincipal) TenantID() uint64 { return p.tenant }
func (p fakePrincipal) HasPermission(perm string) bool {
	return perm == tenantscope.PermImpersonate && p.impersonate
}

func TestFromPrincipalMatrix(t *testing.T) {
	tenantUser := fakePrincipal{actor: 11, tenant: 7}
	admin := fakePrincipal{actor: 1, platform: true}
	adminImp := fakePrincipal{actor: 1, platform: true, impersonate: true}

	type want struct {
		tenant   uint64
		platform bool
		actingAs bool
		err      error
	}
	cases := []struct {
		name   string
		p      tenantscope.Principal
		route  tenantscope.RouteScope
		target tenantscope.Target
		want   want
	}{
		{"租户主体/tenant 路由", tenantUser, tenantscope.RouteTenant, tenantscope.NoTarget(), want{tenant: 7}},
		{"租户主体/either 路由", tenantUser, tenantscope.RouteEither, tenantscope.NoTarget(), want{tenant: 7}},
		{"租户主体指定自己的租户", tenantUser, tenantscope.RouteTenant, tenantscope.TargetTenant(7), want{tenant: 7}},
		{"租户主体指定他人租户（伪造 X-Tenant-ID）", tenantUser, tenantscope.RouteTenant, tenantscope.TargetTenant(8), want{err: tenantscope.ErrTenantMismatch}},
		{"租户主体访问平台路由", tenantUser, tenantscope.RoutePlatform, tenantscope.NoTarget(), want{err: tenantscope.ErrForbiddenRoute}},
		{"租户主体无租户（脏数据）", fakePrincipal{actor: 3}, tenantscope.RouteTenant, tenantscope.NoTarget(), want{err: tenantscope.ErrInvalidScope}},
		{"平台主体/平台路由", admin, tenantscope.RoutePlatform, tenantscope.NoTarget(), want{platform: true}},
		{"平台主体/平台路由带目标租户", admin, tenantscope.RoutePlatform, tenantscope.TargetTenant(7), want{err: tenantscope.ErrInvalidScope}},
		{"平台主体/either 不带租户 → 跨租户只读", admin, tenantscope.RouteEither, tenantscope.NoTarget(), want{platform: true}},
		{"平台主体/tenant 路由不带租户", admin, tenantscope.RouteTenant, tenantscope.NoTarget(), want{err: tenantscope.ErrTenantRequired}},
		{"平台主体代租户但无 impersonate 权限", admin, tenantscope.RouteTenant, tenantscope.TargetTenant(7), want{err: tenantscope.ErrImpersonationDenied}},
		{"平台主体 either 代租户但无权限", admin, tenantscope.RouteEither, tenantscope.TargetTenant(7), want{err: tenantscope.ErrImpersonationDenied}},
		{"平台主体代租户", adminImp, tenantscope.RouteTenant, tenantscope.TargetTenant(7), want{tenant: 7, actingAs: true}},
		{"目标租户 id 为 0", adminImp, tenantscope.RouteTenant, tenantscope.TargetTenant(0), want{err: tenantscope.ErrInvalidScope}},
		{"未知路由元数据", tenantUser, tenantscope.RouteScope("both"), tenantscope.NoTarget(), want{err: tenantscope.ErrInvalidScope}},
		{"持有 nil 指针的主体接口", (*fakePrincipal)(nil), tenantscope.RouteTenant, tenantscope.NoTarget(), want{err: tenantscope.ErrNoPrincipal}},
		{"操作者 id 为 0 的平台主体（审计无法归因）", fakePrincipal{platform: true, impersonate: true}, tenantscope.RouteTenant, tenantscope.TargetTenant(7), want{err: tenantscope.ErrInvalidScope}},
		{"空主体", nil, tenantscope.RouteTenant, tenantscope.NoTarget(), want{err: tenantscope.ErrNoPrincipal}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := tenantscope.FromPrincipal(c.p, c.route, c.target)
			if c.want.err != nil {
				if !errors.Is(err, c.want.err) {
					t.Fatalf("期望错误 %v，实际 %v（scope=%v）", c.want.err, err, s)
				}
				if !s.IsZero() {
					t.Fatalf("失败时必须返回零值 Scope，实际 %v", s)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if s.TenantID() != c.want.tenant || s.IsPlatform() != c.want.platform || s.ActingAs() != c.want.actingAs || s.IsZero() {
				t.Fatalf("scope 不符: %v，期望 %+v", s, c.want)
			}
			if c.p != nil && s.ActorID() != c.p.ActorID() {
				t.Fatalf("ActorID 不符: %d", s.ActorID())
			}
			// 不变量：平台范围无租户；租户范围非平台。
			if s.IsPlatform() && (s.TenantID() != 0 || s.ActingAs()) {
				t.Fatalf("平台范围不变量被破坏: %v", s)
			}
		})
	}
}

func TestFromTaskAndPlatformForJob(t *testing.T) {
	s, err := tenantscope.FromTask(9, 0)
	if err != nil || s.TenantID() != 9 || s.IsPlatform() || s.ActingAs() {
		t.Fatalf("FromTask: %v %v", s, err)
	}
	if z, err := tenantscope.FromTask(0, 1); !errors.Is(err, tenantscope.ErrInvalidScope) || !z.IsZero() {
		t.Fatalf("租户 0 必须被拒绝: %v %v", z, err)
	}
	p := tenantscope.PlatformForJob(5)
	if !p.IsPlatform() || p.TenantID() != 0 || p.ActorID() != 5 || p.IsZero() {
		t.Fatalf("PlatformForJob: %v", p)
	}
}

func TestZeroScopeAndString(t *testing.T) {
	var z tenantscope.Scope
	if !z.IsZero() || z.TenantID() != 0 || z.IsPlatform() {
		t.Fatal("零值 Scope 应为「没有 Scope」")
	}
	for _, s := range []tenantscope.Scope{z, tenantscope.PlatformForJob(1)} {
		if str := s.String(); str == "" || strings.ContainsAny(str, "\n") {
			t.Fatalf("String: %q", str)
		}
	}
}

func TestContextRoundTrip(t *testing.T) {
	if _, ok := tenantscope.FromContext(context.Background()); ok {
		t.Fatal("空 context 不应有 Scope")
	}
	s, _ := tenantscope.FromTask(3, 4)
	ctx := tenantscope.NewContext(context.Background(), s)
	got, ok := tenantscope.FromContext(ctx)
	if !ok || got != s {
		t.Fatalf("round trip: %v %v", got, ok)
	}
	// 放入零值等同于没有。
	if _, ok := tenantscope.FromContext(tenantscope.NewContext(context.Background(), tenantscope.Scope{})); ok {
		t.Fatal("零值 Scope 不得被当作有效 Scope")
	}
}

// 包外无法构造非零 Scope（AC2）：用真实编译器验证，而不是靠约定。
// testdata 下每个包是一种伪造手法，必须编译失败且诊断指向「未导出字段」；ok 包是正向对照，必须编译通过。
func TestForgingScopeOutsidePackageFailsToCompile(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("找不到 go 工具链，无法做编译期断言")
	}
	root := repoRoot(t)
	build := func(dir string) (string, error) {
		c := exec.Command("go", "build", "-o", os.DevNull, "./internal/platform/tenantscope/testdata/"+dir)
		c.Dir = root
		c.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
		out, err := c.CombinedOutput()
		return string(out), err
	}
	if out, err := build("ok"); err != nil {
		t.Fatalf("正向对照应编译通过: %v\n%s", err, out)
	}
	for _, dir := range []string{"forge_named", "forge_positional", "forge_assign", "forge_alias"} {
		t.Run(dir, func(t *testing.T) {
			out, err := build(dir)
			if err == nil {
				t.Fatalf("伪造 Scope 的代码必须编译失败，但通过了")
			}
			if !strings.Contains(out, "unexported") && !strings.Contains(out, "has no field or method tenantID") {
				t.Fatalf("编译失败的原因应与未导出字段有关，实际:\n%s", out)
			}
		})
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到 go.mod")
		}
		dir = parent
	}
}
