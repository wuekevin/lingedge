package authit

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/redisx"
)

// permKey 构造权限缓存键（与 service.permKey 同一格式：用户 uid / 范围 / 权限版本）。
func (s *stack) permKey(userUID string, scopeID, version int64) redisx.Key {
	s.t.Helper()
	k, err := s.rc.Keyspace().Key("auth", "perms", userUID, "s"+strconv.FormatInt(scopeID, 10), "v"+strconv.FormatInt(version, 10))
	if err != nil {
		s.t.Fatal(err)
	}
	return k
}

func (s *stack) cached(k redisx.Key) bool {
	s.t.Helper()
	_, ok, err := s.rc.Get(context.Background(), k)
	if err != nil && !errors.Is(err, redisx.ErrUnavailable) { // 故障注入后客户端尚未探测到恢复：视为「还没有缓存」
		s.t.Fatal(err)
	}
	return ok && err == nil
}

// AC4：权限变更生效——permission_version 递增、缓存键含版本、变更后下一个请求即按新权限判定；旧版本缓存键被清除；其他用户不受波及。
func TestPermissionChangeTakesEffect(t *testing.T) {
	s := newStack(t)
	adm := s.adminSession("platform_admin")
	s.freshReauth(adm)
	roleCode := s.customRole("platform", "user:read")
	u := s.adminSession(roleCode)
	bystander := s.adminSession(s.customRole("platform", "user:read"))
	rid := s.roleUID(roleCode)

	if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
		t.Fatalf("持有 user:read 应可读: %d %s", r.Status, r.Body)
	}
	if r := s.api(bystander, "GET", "/admin/users", nil); r.Status != 200 {
		t.Fatalf("旁观者应可读: %d", r.Status)
	}
	v0 := s.permVersion(u.u.ID)
	k0 := s.permKey(u.u.UID, 0, v0)
	if !s.cached(k0) {
		t.Fatalf("首次授权后权限集应已缓存在 Redis: %s", k0)
	}
	if ttl, ok, err := s.rc.TTL(context.Background(), k0); err != nil || !ok || ttl <= 0 || ttl > 60*time.Second {
		t.Fatalf("缓存 TTL 必须 ≤60 秒（ADR-025）: %v %v %v", ttl, ok, err)
	}
	bv := s.permVersion(bystander.u.ID)

	// 1) 角色权限变更：下一个请求立即 403；版本 +1；旧键被清除；旁观者的版本与缓存不变。
	if r := s.api(adm, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{}}); r.Status != 200 {
		t.Fatalf("清空角色权限: %d %s", r.Status, r.Body)
	}
	if r := s.api(u, "GET", "/admin/users", nil); r.Status != 403 || r.errCode(t) != "auth.forbidden" {
		t.Fatalf("权限被撤销后应立即 403: %d %s", r.Status, r.Body)
	}
	if v1 := s.permVersion(u.u.ID); v1 != v0+1 {
		t.Errorf("permission_version 应递增: %d → %d", v0, v1)
	}
	eventually(t, "旧版本缓存键被删除（写入方删除 + Pub/Sub 广播）", 3*time.Second, func() bool { return !s.cached(k0) })
	if s.permVersion(bystander.u.ID) != bv || !s.cached(s.permKey(bystander.u.UID, 0, bv)) {
		t.Errorf("无关用户的版本与缓存不应受影响")
	}
	// 2) 重新授予：立即 200。
	if r := s.api(adm, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{"user:read"}}); r.Status != 200 {
		t.Fatalf("恢复角色权限: %d %s", r.Status, r.Body)
	}
	if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
		t.Fatalf("恢复后应立即可读: %d %s", r.Status, r.Body)
	}
	// 3) 用户的角色授予变更：移除 → 403，再授予 → 200。
	if r := s.api(adm, "PUT", "/admin/users/"+u.u.UID+"/roles", map[string]any{"role_codes": []string{}}); r.Status != 200 {
		t.Fatalf("移除角色: %d %s", r.Status, r.Body)
	}
	if r := s.api(u, "GET", "/admin/users", nil); r.Status != 403 {
		t.Fatalf("移除角色后应立即 403: %d %s", r.Status, r.Body)
	}
	if r := s.api(adm, "PUT", "/admin/users/"+u.u.UID+"/roles", map[string]any{"role_codes": []string{roleCode}}); r.Status != 200 {
		t.Fatalf("授予角色: %d %s", r.Status, r.Body)
	}
	if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
		t.Fatalf("授予后应立即可读: %d %s", r.Status, r.Body)
	}
	// GET /auth/me 反映同一权限集。
	r := s.api(u, "GET", "/auth/me", nil)
	if perms, _ := r.JSON(t)["permissions"].([]any); len(perms) != 1 || perms[0] != "user:read" {
		t.Errorf("/auth/me 权限集: %s", r.Body)
	}
}

// AC4：Pub/Sub 失效（有损）与轮询兜底都能删除旧版本缓存键——不依赖写入方自己的删除。
func TestPermCachePubSubAndPollFallback(t *testing.T) {
	s := newStack(t, stackOpts{permPoll: 300 * time.Millisecond})
	u := s.operator("user:read")
	// 1) 广播：另一实例（这里用同一 Redis 的发布端）广播「某用户某范围的旧版本」→ 本实例订阅者删除对应键。
	planted := s.permKey(u.u.UID, 0, 90)
	if err := s.rc.Set(context.Background(), planted, `{"p":["user:read"]}`, time.Minute); err != nil {
		t.Fatal(err)
	}
	ch, err := s.rc.Keyspace().Channel("auth", redisx.PurposePermVersion)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := json.Marshal([]map[string]any{{"u": u.u.UID, "s": 0, "v": 90}})
	// 订阅在后台建立，发布可能早于订阅生效：重试发布直到键消失（Pub/Sub 本就有损，这正是它不是正确性依赖的原因）。
	eventually(t, "订阅者收到广播并删除旧键", 5*time.Second, func() bool {
		if err := s.rc.Publish(context.Background(), ch, string(msg)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(60 * time.Millisecond)
		return !s.cached(planted)
	})
	// 2) 轮询兜底：不发任何广播，用户权限版本刚刚变更（updated_at 在对账窗口内）→ 轮询删除其上一版本的键。
	s.e.root(t, "UPDATE §.users SET permission_version = permission_version + 1, updated_at = UTC_TIMESTAMP(3) WHERE id = ?", u.u.ID)
	ver := s.permVersion(u.u.ID)
	stale := s.permKey(u.u.UID, 0, ver-1)
	if err := s.rc.Set(context.Background(), stale, `{"p":["user:read"]}`, time.Minute); err != nil {
		t.Fatal(err)
	}
	eventually(t, "轮询兜底删除上一版本的缓存键（无广播）", 5*time.Second, func() bool { return !s.cached(stale) })
}

// AC4：TTL 兜底——库里的权限被带外修改且没有递增版本（也没有广播）时，缓存最迟在 TTL 内过期，变更最终生效。
func TestPermCacheTTLBackstop(t *testing.T) {
	s := newStack(t, stackOpts{permTTL: 2 * time.Second, permPoll: time.Hour})
	roleCode := s.customRole("platform", "user:read")
	u := s.adminSession(roleCode)
	if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
		t.Fatalf("应可读: %d %s", r.Status, r.Body)
	}
	k := s.permKey(u.u.UID, 0, s.permVersion(u.u.ID))
	if ttl, ok, _ := s.rc.TTL(context.Background(), k); !ok || ttl > 2*time.Second {
		t.Fatalf("缓存 TTL 应遵循配置（≤2s）: %v %v", ttl, ok)
	}
	// 带外撤销（不递增版本、不广播）。
	s.e.root(t, "DELETE FROM §.role_permissions WHERE role_id = (SELECT id FROM §.roles WHERE code = ? AND tenant_id IS NULL)", roleCode)
	if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
		t.Fatalf("TTL 内仍按缓存判定（证明缓存真实存在）: %d %s", r.Status, r.Body)
	}
	eventually(t, "TTL 过期后变更最终生效", 6*time.Second, func() bool {
		return s.api(u, "GET", "/admin/users", nil).Status == 403
	})
}

// AC4：Redis 不可用时直读 MySQL——从未可用；以及运行中被杀（真实故障注入）并在恢复后自动回填缓存。
func TestPermissionsWithoutRedisAndFaultInjection(t *testing.T) {
	check := func(t *testing.T, s *stack) {
		t.Helper()
		adm := s.adminSession("platform_admin")
		s.freshReauth(adm)
		roleCode := s.customRole("platform", "user:read")
		u := s.adminSession(roleCode)
		rid := s.roleUID(roleCode)
		if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
			t.Fatalf("Redis 不可用时应直读 MySQL 并放行: %d %s", r.Status, r.Body)
		}
		if r := s.api(adm, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{}}); r.Status != 200 {
			t.Fatalf("Redis 不可用时权限变更仍可用: %d %s", r.Status, r.Body)
		}
		if r := s.api(u, "GET", "/admin/users", nil); r.Status != 403 {
			t.Fatalf("Redis 不可用时变更也必须立即生效: %d %s", r.Status, r.Body)
		}
		if r := s.api(adm, "PUT", "/admin/users/"+u.u.UID+"/roles", map[string]any{"role_codes": []string{"platform_auditor"}}); r.Status != 200 {
			t.Fatalf("改用户角色: %d %s", r.Status, r.Body)
		}
		if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
			t.Fatalf("授予新角色后应立即可读: %d %s", r.Status, r.Body)
		}
	}
	t.Run("从未可用", func(t *testing.T) { check(t, newStack(t, stackOpts{noRedis: true})) })
	t.Run("运行中被杀并恢复", func(t *testing.T) {
		s := newStack(t)
		adm := s.adminSession("platform_admin")
		s.freshReauth(adm)
		roleCode := s.customRole("platform", "user:read")
		u := s.adminSession(roleCode)
		rid := s.roleUID(roleCode)
		if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
			t.Fatalf("预热: %d", r.Status)
		}
		k := s.permKey(u.u.UID, 0, s.permVersion(u.u.ID))
		if !s.cached(k) {
			t.Fatalf("预热后应已缓存")
		}
		s.redis.Kill() // 真实故障注入：SIGKILL，缓存与订阅一并丢失
		if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
			t.Fatalf("Redis 被杀后应直读 MySQL: %d %s", r.Status, r.Body)
		}
		if r := s.api(adm, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{}}); r.Status != 200 {
			t.Fatalf("Redis 被杀后权限变更仍可用: %d %s", r.Status, r.Body)
		}
		if r := s.api(u, "GET", "/admin/users", nil); r.Status != 403 {
			t.Fatalf("Redis 被杀后变更必须立即生效: %d %s", r.Status, r.Body)
		}
		s.redis.Restart(t)
		if r := s.api(adm, "PUT", "/admin/roles/"+rid+"/permissions", map[string]any{"permissions": []string{"user:read"}}); r.Status != 200 {
			t.Fatalf("Redis 恢复后: %d %s", r.Status, r.Body)
		}
		nk := s.permKey(u.u.UID, 0, s.permVersion(u.u.ID))
		eventually(t, "Redis 恢复后缓存自动回填（无需人工干预）", 10*time.Second, func() bool {
			if r := s.api(u, "GET", "/admin/users", nil); r.Status != 200 {
				t.Fatalf("恢复后应可读: %d %s", r.Status, r.Body)
			}
			return s.cached(nk)
		})
	})
}
