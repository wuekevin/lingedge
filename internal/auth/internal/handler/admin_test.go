package handler

import (
	"net/http/httptest"
	"testing"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/rbac"
)

// /admin 管理路由的元数据必须合法、与权限注册表一致，且租户范围/再认证/幂等声明符合 ADR-018 §12。
func TestAdminMetasValidAndKnownPermissions(t *testing.T) {
	ms := AdminMetas()
	if len(ms) != 17 {
		t.Fatalf("应有 17 个 /admin 路由（含 Story 1.21 的 listLoginLogs）: %d", len(ms))
	}
	seen := map[string]bool{}
	reauth := 0
	for _, m := range ms {
		if p := m.Validate(); len(p) > 0 {
			t.Errorf("%s 元数据不合法: %v", m.OperationID, p)
		}
		if seen[m.OperationID] {
			t.Errorf("重复 %s", m.OperationID)
		}
		seen[m.OperationID] = true
		if m.TenantScope != httpx.ScopePlatform || m.Auth != httpx.AuthRequired || len(m.Permissions) == 0 {
			t.Errorf("%s: 管理路由必须是 platform 范围、需认证、声明权限", m.OperationID)
		}
		for _, p := range m.Permissions {
			if !rbac.Known(p) {
				t.Errorf("%s 引用了未注册的权限 %s", m.OperationID, p)
			}
		}
		if m.Method != "GET" && !m.Audit {
			t.Errorf("%s: 写操作必须声明 x-audit", m.OperationID)
		}
		if m.Reauth {
			reauth++
		}
		if !IsAdminOperation(m.OperationID) {
			t.Errorf("%s 应被识别为管理 operation", m.OperationID)
		}
	}
	if reauth != 8 { // createUser/deleteUser/setUserRoles/resetUserPassword + createRole/updateRole/deleteRole/setRolePermissions
		t.Errorf("x-reauth 的管理 operation 应为 8 个: %d", reauth)
	}
	if IsAdminOperation("listTasks") || IsAdminOperation("login") || IsAdminOperation("") {
		t.Error("非管理 operation 不得被识别为管理 operation")
	}
}

// 管理路由不是「自助」operation：不会被 WithSelfService 的最小授权放行。
func TestAdminOperationsAreNotSelfService(t *testing.T) {
	m := &Module{}
	for _, meta := range AdminMetas() {
		if m.SelfService(meta.OperationID) {
			t.Errorf("%s 不得是自助 operation", meta.OperationID)
		}
	}
}

// 授权钩子对外来主体（不是本模块产出的、无法装载权限集）fail-closed。
func TestAuthorizerRejectsForeignPrincipal(t *testing.T) {
	a := NewAuthorizer(nil)
	r := httptest.NewRequest("GET", "/x", nil)
	err := a.Authorize(r, httpx.RouteMeta{OperationID: "x", TenantScope: httpx.ScopePlatform, Permissions: []string{"user:read"}}, foreign{})
	if e, ok := apierr.As(err); !ok || e.Code != apierr.AuthForbidden {
		t.Errorf("外来主体应 403 auth.forbidden: %v", err)
	}
	// 公开 scope 不经授权：若被错误装配到授权钩子，同样拒绝。
	err = a.Authorize(r, httpx.RouteMeta{OperationID: "x", TenantScope: httpx.ScopePublic}, &domain.Principal{})
	if e, ok := apierr.As(err); !ok || e.Code != apierr.AuthForbidden {
		t.Errorf("public scope 应拒绝: %v", err)
	}
}

func TestTargetTenantHeader(t *testing.T) {
	good := "01M7QP5V346STC0AT56NME5NR3"
	cases := []struct {
		vals    []string
		present bool
		bad     bool
	}{
		{nil, false, false},
		{[]string{good}, true, false},
		{[]string{"nope"}, false, true},
		{[]string{good, good}, false, true},
		{[]string{""}, false, true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/x", nil)
		for _, v := range c.vals {
			r.Header.Add("X-Tenant-ID", v)
		}
		got, present, err := targetTenant(r)
		if (err != nil) != c.bad || present != c.present || (c.present && got != good) {
			t.Errorf("%v: got=%q present=%v err=%v", c.vals, got, present, err)
		}
	}
}
