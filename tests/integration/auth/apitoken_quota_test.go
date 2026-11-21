package authit

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// Story 1.22b：令牌创建时的租户配额硬上限（tenant_quotas.api_tokens；通用配额强制与用量统计仍属 Story 6.9）。
// 语义：无配额行或 limit_value=NULL = 不限；有限额时「当前有效令牌（未吊销、未过期，租户内所有成员合计）>= 限额」→ 403 tenant.quota_exceeded；
// 平台令牌不受租户配额约束；轮换不增加有效数；并发创建不能突破上限。
func TestAPITokenCreateQuota(t *testing.T) {
	setQuota := func(s *stack, tenantID uint64, limit any) {
		s.e.root(t, "INSERT INTO §.tenant_quotas (tenant_id, quota_key, limit_value, source) VALUES (?, 'api_tokens', ?, 'manual') ON DUPLICATE KEY UPDATE limit_value = VALUES(limit_value)", tenantID, limit)
	}
	expectQuota := func(s *stack, ss session, what string) {
		t.Helper()
		r := s.as(ss, "POST", "/auth/api-tokens", tokBody(what, "site:read"))
		if r.Status != 403 || r.errCode(t) != "tenant.quota_exceeded" {
			t.Fatalf("%s：应 403 tenant.quota_exceeded: %d %s", what, r.Status, r.Body)
		}
		if got := string(r.Body); !strings.Contains(got, `"api_tokens"`) || strings.Contains(got, "limit") {
			t.Errorf("%s：错误体应指明配额键且不泄露限额数值: %s", what, got)
		}
	}

	t.Run("无配额行与 NULL 都是不限；租户之间互不影响；平台令牌不受租户配额约束", func(t *testing.T) {
		s := newTokStack(t)
		a, b := s.tenantSession("tenant_owner"), s.tenantSession("tenant_owner")
		for i := 0; i < 5; i++ {
			s.issue(a, tokBody("无配额行", "site:read"))
		}
		setQuota(s, a.u.TenantID, nil)
		s.issue(a, tokBody("NULL=不限", "site:read"))
		setQuota(s, a.u.TenantID, 1) // A 已远超 1：新建被拒
		expectQuota(s, a, "A 超限")
		s.issue(b, tokBody("B 不受 A 的配额影响", "site:read"))
		adm := s.adminSession("platform_admin")
		s.issue(adm, tokBody("平台令牌", "user:read"))
	})

	t.Run("限额 0 禁止创建；放开后可创建", func(t *testing.T) {
		s := newTokStack(t)
		a := s.tenantSession("tenant_owner")
		setQuota(s, a.u.TenantID, 0)
		expectQuota(s, a, "限额 0")
		setQuota(s, a.u.TenantID, 1)
		s.issue(a, tokBody("放开到 1", "site:read"))
		expectQuota(s, a, "已满")
	})

	t.Run("只数有效令牌：吊销与过期释放名额；同租户其他成员合计；轮换不受影响", func(t *testing.T) {
		s := newTokStack(t)
		a := s.tenantSession("tenant_owner")
		setQuota(s, a.u.TenantID, 2)
		first := s.issue(a, tokBody("甲", "site:read"))
		exp := s.clk.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
		short := s.issue(a, map[string]any{"name": "乙（1 小时）", "scopes": []string{"site:read"}, "expires_at": exp})
		expectQuota(s, a, "已满 2/2")

		// 同租户的另一位成员与创建者合计。
		u2 := s.seedUser("tenant", withNoMembership())
		u2.TenantID = a.u.TenantID
		s.e.root(t, "INSERT INTO §.tenant_members (tenant_id, user_id, status, is_owner, joined_at) VALUES (?, ?, 'active', 0, NOW(3))", u2.TenantID, u2.ID)
		s.grantTenantRole(u2, "tenant_owner")
		_, tk2, csrf2 := s.login(u2, freshIP())
		peer := session{u: u2, token: tk2, csrf: csrf2}
		expectQuota(s, peer, "同租户成员合计")

		// 轮换：新增一枚同时吊销旧的一枚，有效数不变，满额时仍可轮换。
		rot := s.as(a, "POST", "/auth/api-tokens/"+first.UID+"/rotate", nil)
		if rot.Status != 200 {
			t.Fatalf("满额时轮换应成功: %d %s", rot.Status, rot.Body)
		}
		expectQuota(s, a, "轮换后仍 2/2")

		// 吊销释放名额。
		if r := s.as(a, "DELETE", "/auth/api-tokens/"+short.UID, nil); r.Status != 204 {
			t.Fatalf("吊销: %d %s", r.Status, r.Body)
		}
		s.issue(peer, tokBody("成员占用释放的名额", "site:read"))
		expectQuota(s, a, "又满")

	})

	t.Run("过期释放名额", func(t *testing.T) {
		s := newTokStack(t)
		a := s.tenantSession("tenant_owner")
		setQuota(s, a.u.TenantID, 1)
		exp := s.clk.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
		s.issue(a, map[string]any{"name": "一小时", "scopes": []string{"site:read"}, "expires_at": exp})
		expectQuota(s, a, "未过期占用名额")
		s.clk.Advance(time.Hour + time.Second) // 与令牌认证使用同一时钟；会话 idle 超时前须仍有效
		s.issue(a, tokBody("过期后释放", "site:read"))
	})

	t.Run("并发创建不能突破上限", func(t *testing.T) {
		s := newTokStack(t)
		a := s.tenantSession("tenant_owner")
		const limit, attempts = 3, 10
		setQuota(s, a.u.TenantID, limit)
		var wg sync.WaitGroup
		codes := make([]int, attempts)
		for i := 0; i < attempts; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i] = s.as(a, "POST", "/auth/api-tokens", tokBody("并发", "site:read")).Status
			}()
		}
		wg.Wait()
		ok, denied := 0, 0
		for _, c := range codes {
			switch c {
			case 201:
				ok++
			case 403:
				denied++
			default:
				t.Errorf("意外状态码 %d（全部: %v）", c, codes)
			}
		}
		if ok != limit || denied != attempts-limit {
			t.Errorf("并发创建应恰好成功 %d 个、其余 403：成功 %d 拒绝 %d（%v）", limit, ok, denied, codes)
		}
		if n := s.e.rootInt(t, "SELECT COUNT(*) FROM §.api_tokens WHERE tenant_id = ? AND revoked_at IS NULL", a.u.TenantID); n != limit {
			t.Errorf("库内有效令牌数应为 %d: %d", limit, n)
		}
	})
}
