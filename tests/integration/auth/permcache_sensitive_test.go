package authit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/rbac"
	"github.com/wuekevin/lingedge/pkg/redact"
)

// misjudgedCodes 枚举权限目录里「被 redact.String 对其 JSON 缓存形态误判」的全部权限码（Story 1.39b 复现用，不写死清单）。
func misjudgedCodes(t *testing.T) []string {
	t.Helper()
	var bad []string
	for _, p := range rbac.All() {
		b, _ := json.Marshal(map[string]any{"p": []string{p.Code}, "v": 2})
		if redact.String(string(b)) != string(b) {
			bad = append(bad, p.Code)
		}
	}
	return bad
}

// Story 1.39b：权限码是非机密的结构化数据。目录里被 redact 启发式误判的权限码（含 token:manage）
// 不得让权限缓存无法回填，也不得每个请求打一条 warn。
func TestPermCacheBackfillsEvenWithRedactLookalikeCodes(t *testing.T) {
	bad := misjudgedCodes(t)
	if len(bad) == 0 {
		t.Fatal("预期目录里存在被 redact 误判的权限码（token:manage 等），否则本回归失去意义")
	}
	t.Logf("redact 对缓存形态误判的权限码: %v", bad)
	s := newStack(t)
	u, _ := s.opWithTokenManage("user:read")
	for i := 0; i < 3; i++ {
		if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
			t.Fatalf("应可读: %d %s", r.Status, r.Body)
		}
	}
	k := s.permKey(u.u.UID, 0, s.permVersion(u.u.ID))
	if !s.cached(k) {
		t.Fatalf("含 token:manage 的权限集应已回填 Redis: %s", k)
	}
	v, _, _ := s.rc.Get(context.Background(), k)
	if !strings.Contains(v, "token:manage") {
		t.Errorf("缓存值应含权限码: %s", v)
	}
	if ttl, ok, err := s.rc.TTL(context.Background(), k); err != nil || !ok || ttl <= 0 || ttl > 60*time.Second {
		t.Errorf("TTL 必须 ≤60 秒: %v %v %v", ttl, ok, err)
	}
	if out := s.logs.String(); strings.Contains(out, "缓存值含敏感信息") || strings.Contains(out, "跳过回填") {
		t.Errorf("不应有「缓存值含敏感信息」warn:\n%s", out)
	}
}

// Story 1.39b：含 token:manage 的权限集回填后，权限变更仍立即生效且旧键被清除，新版本再次回填；全程无敏感信息 warn。
func TestPermCacheInvalidationWithTokenManage(t *testing.T) {
	s := newStack(t)
	adm := s.adminSession("platform_admin")
	s.freshReauth(adm)
	u, roleCode := s.opWithTokenManage("user:read")
	rid := s.roleUID(roleCode)
	if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
		t.Fatalf("应可读: %d %s", r.Status, r.Body)
	}
	v0 := s.permVersion(u.u.ID)
	k0 := s.permKey(u.u.UID, 0, v0)
	if !s.cached(k0) {
		t.Fatalf("应已回填: %s", k0)
	}
	// 撤销 user:read（保留 token:manage）：下一个请求立即 403；旧键删除；新版本键回填且仍含 token:manage。
	if r := s.api(adm, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{"token:manage"}}); r.Status != 200 {
		t.Fatalf("变更角色权限: %d %s", r.Status, r.Body)
	}
	if r := s.api(u, "GET", "/admin/users", nil); r.Status != 403 {
		t.Fatalf("撤销后应立即 403: %d %s", r.Status, r.Body)
	}
	if v1 := s.permVersion(u.u.ID); v1 != v0+1 {
		t.Errorf("版本应递增: %d → %d", v0, v1)
	}
	eventually(t, "旧版本缓存键被删除", 3*time.Second, func() bool { return !s.cached(k0) })
	k1 := s.permKey(u.u.UID, 0, s.permVersion(u.u.ID))
	if !s.cached(k1) {
		t.Fatalf("新版本权限集应回填: %s", k1)
	}
	if v, _, _ := s.rc.Get(context.Background(), k1); !strings.Contains(v, "token:manage") || strings.Contains(v, "user:read") {
		t.Errorf("新缓存值应只含 token:manage: %s", v)
	}
	// /auth/me 与缓存一致。
	r := s.api(u, "GET", "/auth/me", nil)
	if perms, _ := r.JSON(t)["permissions"].([]any); len(perms) != 1 || perms[0] != "token:manage" {
		t.Errorf("/auth/me 权限集: %s", r.Body)
	}
	if out := s.logs.String(); strings.Contains(out, "缓存值含敏感信息") {
		t.Errorf("不应有敏感信息 warn:\n%s", out)
	}
}
