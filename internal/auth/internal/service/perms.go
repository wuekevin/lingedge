package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
)

// 权限集缓存（Story 1.18、ADR-018 §8、ADR-025）。
//
// 一致性模型（读这一段再改代码）：
//   - 权威版本：users.permission_version 随会话一起读出（认证钩子每个请求都读会话，所以版本号是免费且权威的）。
//     权限缓存键含它：le:<env>:auth:perms:<user uid>:s<范围>:v<版本>（范围 0 = 平台，否则租户 id）。
//     权限/角色变更时同一事务内版本 +1，于是旧版本的缓存项在下一个请求就不可达——变更立即生效，不依赖任何广播。
//   - 缓存项 TTL ≤ 60 秒（ADR-025 §1）：旧版本的键靠它自然过期；这也是「库里的权限被带外修改而没有递增版本」时的陈旧上限。
//   - Pub/Sub（redisx.PurposePermVersion，有损）：变更提交后广播被波及的（用户, 范围, 旧版本），所有实例据此主动删除旧键——
//     这是内存回收，不是正确性依赖；广播丢失由 TTL 与下面的轮询兜底补上（redisx.Subscribe 强制要求提供轮询兜底）。
//   - 轮询兜底：周期性从 MySQL 读出最近被更新且版本>0 的用户，删除其上一版本的键。
//   - Redis 不可用：redisx.Cache 直读 MySQL（结果正确，只是更慢），Redis 恢复后下一次 miss 自动回填。
const (
	// MaxPermCacheTTL 是权限缓存 TTL 的上限（ADR-025 §1：会话/权限缓存 ≤60 秒）。
	MaxPermCacheTTL = 60 * time.Second
	// DefaultPermPollEvery 是轮询兜底的周期；对账窗口是两个周期，须不小于缓存 TTL 的一半才能覆盖整个 TTL（默认 30s×2=60s）。
	DefaultPermPollEvery = 30 * time.Second
	permPollLimit        = 500
	permMsgBatch         = 100
)

// permEntry 是缓存项：权限码（p）与 /auth/me 所需的角色（r）、菜单键（m）。V=2 起含 r/m（Story 1.22：/auth/me 改走本缓存）；
// 旧格式（无 V）的项只有 p，读取方遇到时回落到直读 MySQL，随 TTL（≤60 秒）自然淘汰。
type permEntry struct {
	P []string `json:"p"`
	R []string `json:"r,omitempty"`
	M []string `json:"m,omitempty"`
	V int      `json:"v,omitempty"`
}

const permEntryVersion = 2

func (s *Service) permKey(userUID string, scopeID, version uint64) (redisx.Key, error) {
	return s.ks.Key("auth", "perms", userUID, "s"+strconv.FormatUint(scopeID, 10), "v"+strconv.FormatUint(version, 10))
}

// LoadPermissions 取主体的有效权限集（平台主体：平台角色；租户主体：其当前租户内的租户角色，且只含 scope 为 tenant/both 的权限）。
func (s *Service) LoadPermissions(ctx context.Context, p *domain.Principal) (map[string]struct{}, error) {
	e, err := s.loadEntry(ctx, p)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(e.P))
	for _, c := range e.P {
		set[c] = struct{}{}
	}
	if p.Bearer {
		// Bearer 主体的有效权限 = 令牌 scopes ∩ 创建者**当前**权限（创建者被降权后令牌随之收缩；令牌 scopes 从不扩大权限）。
		for c := range set {
			if _, ok := p.TokenScopes[c]; !ok {
				delete(set, c)
			}
		}
	}
	return set, nil
}

// LoadAccess 返回 /auth/me 所需的角色、权限集与菜单键：与授权钩子同一缓存（键含 permission_version，TTL ≤60 秒，Redis 不可用直读 MySQL），
// 因此 /auth/me 看到的权限集与授权判定逐项一致（Story 1.18 遗留：此前 /auth/me 直读 MySQL）。
func (s *Service) LoadAccess(ctx context.Context, p *domain.Principal) (domain.Access, error) {
	e, err := s.loadEntry(ctx, p)
	if err != nil {
		return domain.Access{}, err
	}
	if e.V < permEntryVersion { // 旧格式缓存项没有角色/菜单键：直读一次
		return s.loadAccessDirect(ctx, p)
	}
	return domain.Access{Roles: nonNil(e.R), Permissions: nonNil(e.P), MenuKeys: nonNil(e.M)}, nil
}

func nonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func (s *Service) scopeOf(p *domain.Principal) (uint64, bool) {
	if p.IsPlatform() {
		return 0, true
	}
	if p.Tenant == nil {
		return 0, false // 没有租户上下文的租户主体：没有任何权限（fail-closed）
	}
	return p.Tenant.ID, true
}

func (s *Service) loadAccessDirect(ctx context.Context, p *domain.Principal) (domain.Access, error) {
	scopeID, ok := s.scopeOf(p)
	if !ok {
		return domain.Access{Roles: []string{}, Permissions: []string{}, MenuKeys: []string{}}, nil
	}
	e, err := s.queryEntry(ctx, p, scopeID)
	if err != nil {
		return domain.Access{}, apierr.Internal(err)
	}
	return domain.Access{Roles: nonNil(e.R), Permissions: nonNil(e.P), MenuKeys: nonNil(e.M)}, nil
}

func (s *Service) queryEntry(ctx context.Context, p *domain.Principal, scopeID uint64) (permEntry, error) {
	var codes []string
	var err error
	if scopeID == 0 {
		codes, err = s.repo.PlatformPermissions(ctx, p.User.ID)
	} else {
		codes, err = s.repo.TenantPermissions(ctx, p.User.ID, scopeID)
	}
	if err != nil {
		return permEntry{}, err
	}
	roles, menus, err := s.repo.RolesAndMenus(ctx, p.User.ID, scopeID)
	if err != nil {
		return permEntry{}, err
	}
	return permEntry{P: nonNil(codes), R: nonNil(roles), M: nonNil(menus), V: permEntryVersion}, nil
}

func (s *Service) loadEntry(ctx context.Context, p *domain.Principal) (permEntry, error) {
	scopeID, ok := s.scopeOf(p)
	if !ok {
		return permEntry{P: []string{}, R: []string{}, M: []string{}, V: permEntryVersion}, nil
	}
	load := func(ctx context.Context) ([]byte, error) {
		e, err := s.queryEntry(ctx, p, scopeID)
		if err != nil {
			return nil, err
		}
		return json.Marshal(e)
	}
	var raw []byte
	key, kerr := s.permKey(p.User.UID, scopeID, p.Session.PermVersion)
	// Bearer 主体没有会话，但其创建者的 permission_version 随令牌一起现读（见 AuthenticateAPIToken），所以同样可以用版本化的缓存键。
	if (!p.Bearer && p.Session.ID == 0) || kerr != nil {
		if kerr != nil {
			s.log.Warn("权限缓存键构造失败，直读 MySQL", zap.Error(kerr))
		}
		var err error
		raw, err = load(ctx) // 没有会话（版本未知）的主体不缓存
		if err != nil {
			return permEntry{}, apierr.Internal(err)
		}
	} else {
		var err error
		raw, err = s.permCache.GetOrLoad(ctx, key, s.permTTL, load)
		if err != nil {
			return permEntry{}, apierr.Internal(err)
		}
	}
	var e permEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return permEntry{}, apierr.Internal(err)
	}
	return e, nil
}

// permMessage 是 Pub/Sub 广播的内容：被波及的（用户, 范围）与其「旧」版本（即需要删除的缓存键）。
type permMessage struct {
	U string `json:"u"`
	S uint64 `json:"s"`
	V uint64 `json:"v"`
}

// invalidatePermissions 在变更提交之后调用：删除旧版本的缓存键并广播（均为尽力而为；失败只记日志，正确性靠版本号与 TTL）。
func (s *Service) invalidatePermissions(ctx context.Context, affected []domain.AffectedAccess) {
	if len(affected) == 0 {
		return
	}
	msgs := make([]permMessage, 0, len(affected))
	for _, a := range affected {
		if a.Version == 0 {
			continue
		}
		old := a.Version - 1
		s.dropPermKey(ctx, a.UserUID, a.ScopeID, old)
		msgs = append(msgs, permMessage{U: a.UserUID, S: a.ScopeID, V: old})
	}
	ch, err := s.ks.Channel("auth", redisx.PurposePermVersion)
	if err != nil {
		s.log.Warn("权限失效频道构造失败", zap.Error(err))
		return
	}
	for len(msgs) > 0 {
		n := min(permMsgBatch, len(msgs))
		b, err := json.Marshal(msgs[:n])
		if err == nil {
			err = s.redis.Publish(ctx, ch, string(b))
		}
		if err != nil && !errors.Is(err, redisx.ErrUnavailable) {
			s.log.Warn("权限失效广播失败（缓存将靠版本号/TTL 收敛）", zap.Error(err))
		}
		msgs = msgs[n:]
	}
}

func (s *Service) dropPermKey(ctx context.Context, userUID string, scopeID, version uint64) {
	k, err := s.permKey(userUID, scopeID, version)
	if err != nil {
		return
	}
	if err := s.redis.Del(ctx, k); err != nil && !errors.Is(err, redisx.ErrUnavailable) {
		s.log.Warn("删除权限缓存键失败", zap.Error(err))
	}
}

// startPermSubscription 订阅权限失效广播；轮询兜底从 MySQL 对账。Redis 关闭（Client.Close）时订阅随之结束。
func (s *Service) startPermSubscription(ctx context.Context) error {
	ch, err := s.ks.Channel("auth", redisx.PurposePermVersion)
	if err != nil {
		return err
	}
	_, err = s.redis.Subscribe(ctx, ch, redisx.SubscribeOptions{
		Handler: func(ctx context.Context, payload string) {
			var msgs []permMessage
			if json.Unmarshal([]byte(payload), &msgs) != nil {
				return
			}
			for _, m := range msgs {
				s.dropPermKey(ctx, m.U, m.S, m.V)
			}
		},
		Poll: func(ctx context.Context) error {
			since := s.clk.Now().Add(-2 * s.permPoll)
			rows, err := s.repo.RecentlyChangedAccess(ctx, since, permPollLimit)
			if err != nil {
				return err
			}
			for _, a := range rows {
				if a.Version > 0 {
					s.dropPermKey(ctx, a.UserUID, a.ScopeID, a.Version-1)
				}
			}
			return nil
		},
		PollInterval: s.permPoll,
	})
	return err
}
