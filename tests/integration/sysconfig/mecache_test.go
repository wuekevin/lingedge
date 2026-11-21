package sysconfigit

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/tests/apistack"
)

// 1.18 遗留：/auth/me 的权限集不再直读 MySQL，而是与授权钩子共用 permission_version 缓存（键含版本）。
// 证明方式（真实 MySQL + 真实 Redis）：缓存项在首个 /auth/me 之后出现在 Redis；带外（不递增 permission_version）修改角色权限时 /auth/me 仍返回缓存值
// （TTL ≤60 秒是陈旧上限）；递增 permission_version 后立即反映变化（旧版本键不可达）。
func TestMePermissionsServedFromVersionedCache(t *testing.T) {
	w := apistack.New(t)
	role := w.CustomRole("platform", "setting:read", "system:read")
	u := w.SeedUser("platform", nil)
	w.GrantPlatformRole(u, role)
	s := w.Login(u)

	perms := func() []string {
		r := s.Do("GET", "/auth/me", nil)
		if r.Status != 200 {
			t.Fatalf("/auth/me: %d %s", r.Status, r.Body)
		}
		return toStrings(r.JSON(t)["permissions"])
	}
	if got := strings.Join(perms(), ","); got != "setting:read,system:read" {
		t.Fatalf("权限集 = %s", got)
	}
	ver := w.RootInt("SELECT permission_version FROM §.users WHERE id = ?", u.ID)
	key, err := w.RC.Keyspace().Key("auth", "perms", u.UID, "s0", "v"+itoa(ver))
	if err != nil {
		t.Fatal(err)
	}
	raw, ok, err := w.RC.Get(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("/auth/me 之后权限缓存项应存在于 Redis（键 %s）: ok=%v err=%v", key, ok, err)
	}
	if !strings.Contains(raw, `"setting:read"`) || !strings.Contains(raw, `"r":["`+role+`"]`) {
		t.Errorf("缓存项应含权限码与角色: %s", raw)
	}
	me := s.Do("GET", "/auth/me", nil).JSON(t)
	if got := toStrings(me["roles"]); len(got) != 1 || got[0] != role {
		t.Errorf("roles = %v", got)
	}

	// 带外删除一条权限而不递增版本：/auth/me 仍是缓存值（证明走了缓存，而不是直读 MySQL）。
	w.Root("DELETE FROM §.role_permissions WHERE role_id = (SELECT id FROM §.roles WHERE code = ? AND tenant_id IS NULL) AND permission_id = (SELECT id FROM §.permissions WHERE code = 'system:read')", role)
	if got := strings.Join(perms(), ","); got != "setting:read,system:read" {
		t.Fatalf("未递增版本时应仍返回缓存的权限集（走缓存）: %s", got)
	}
	// 递增 permission_version（正常变更路径会同事务递增）：立即反映。
	w.Root("UPDATE §.users SET permission_version = permission_version + 1 WHERE id = ?", u.ID)
	if got := strings.Join(perms(), ","); got != "setting:read" {
		t.Fatalf("递增版本后应立即读到新权限集: %s", got)
	}

	// Redis 不可用时直读 MySQL（降级正确）。
	w.Redis.Stop()
	w.Root("DELETE FROM §.role_permissions WHERE role_id = (SELECT id FROM §.roles WHERE code = ? AND tenant_id IS NULL) AND permission_id = (SELECT id FROM §.permissions WHERE code = 'setting:read')", role)
	w.Root("UPDATE §.users SET permission_version = permission_version + 1 WHERE id = ?", u.ID)
	if got := perms(); len(got) != 0 {
		t.Fatalf("Redis 不可用时应直读 MySQL（权限已空）: %v", got)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
