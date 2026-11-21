package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/rbac"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// API Token（Story 1.19、ADR-018 §7、FR-25、NFR-14）。
//
// 令牌形如 le_<8 位前缀>_<随机>：前缀是 5 字节 CSPRNG 的小写 base32（8 个 [a-z2-7]，可检索、可在日志中出现），随机部分是 32 字节 CSPRNG 的 base64url（43 字符）。
// 明文只在创建/轮换响应里出现一次；库里只有 SHA-256(完整令牌) 的十六进制与前缀（令牌是高熵随机值，无需慢哈希；确定性哈希使按哈希查找成为可能）。
//
// 不变量（均有真实 MySQL/Redis 测试，见 tests/integration/auth/apitoken_*_test.go）：
//   - 明文不入库（含 idempotency_keys）、不入 Redis、不入日志（日志只记前缀）；
//   - 有效权限 = 令牌 scopes ∩ 创建者**当前**权限：创建者被禁用/删除 → 令牌 401；被降权 → 令牌随之收缩（权限集缓存键含 permission_version，立即生效）；
//   - 吊销立即失效：授权相关状态（吊销/过期/创建者状态/成员关系/权限版本）每个请求都从 MySQL 现读；Redis 里只缓存「哈希 → 令牌 id」这一不可变映射，
//     吊销时删除缓存键只是卫生措施，不是正确性依赖——所以 Redis 删除失败/不可用/被残留旧值都不会让已吊销的令牌继续有效；
//   - 过期、吊销、来源 IP 不在 allowed_cidrs、哈希不匹配、令牌不存在……一律同一个 401（响应不泄漏原因；原因只写服务端日志，不含明文）；
//   - 来源 IP 取自 httpx.ClientIPFrom（与访问日志/节流同一取值，遵守可信代理配置），伪造 X-Forwarded-For 不能绕过 allowed_cidrs。
const (
	apiTokenPrefixLen = 8
	apiTokenSecretLen = 43 // base64url(32 字节) 无填充
	apiTokenLen       = 3 + apiTokenPrefixLen + 1 + apiTokenSecretLen
	apiTokenSecretRaw = 32

	maxTokenScopes = 64
	maxTokenCIDRs  = 32
	maxTokenName   = 128

	// apiTokenCacheTTL 是「哈希 → 令牌 id」缓存的 TTL（ADR-025 §1：≤60 秒）。
	apiTokenCacheTTL = MaxPermCacheTTL
	// tokenTouchInterval 是 last_used_at 的最小写入间隔（每个令牌每进程至多每分钟一次异步写库）。
	tokenTouchInterval = time.Minute
	// tokenFailLogEvery：同一令牌的 token_auth_failed 日志至多每分钟一条（防止被盗令牌的持有者刷爆 login_logs）。
	tokenFailLogEvery = time.Minute
	maxTouchedEntries = 10000
	maxCreateAttempts = 5
)

const tokenManagePerm = "token:manage"

var prefixEnc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// GenerateAPIToken 生成新令牌：返回明文、前缀与库内哈希。
func GenerateAPIToken() (token, prefix, hash string, err error) {
	pb := make([]byte, 5)
	sb := make([]byte, apiTokenSecretRaw)
	if _, err := rand.Read(pb); err != nil {
		return "", "", "", fmt.Errorf("auth: 生成令牌前缀: %w", err)
	}
	if _, err := rand.Read(sb); err != nil {
		return "", "", "", fmt.Errorf("auth: 生成令牌随机部分: %w", err)
	}
	prefix = prefixEnc.EncodeToString(pb)
	token = "le_" + prefix + "_" + base64.RawURLEncoding.EncodeToString(sb)
	return token, prefix, HashAPIToken(token), nil
}

// HashAPIToken 是令牌的库内哈希：SHA-256(完整令牌) 的十六进制。
func HashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ParseAPIToken 校验令牌格式（长度、`le_` 前缀、前缀字符集、随机部分字符集）并返回前缀；不合格返回 ok=false（不做任何 I/O）。
func ParseAPIToken(s string) (prefix string, ok bool) {
	if len(s) != apiTokenLen || !strings.HasPrefix(s, "le_") || s[3+apiTokenPrefixLen] != '_' {
		return "", false
	}
	prefix = s[3 : 3+apiTokenPrefixLen]
	for i := 0; i < len(prefix); i++ {
		if !isPrefixChar(prefix[i]) {
			return "", false
		}
	}
	for i := 3 + apiTokenPrefixLen + 1; i < len(s); i++ {
		if !isSecretChar(s[i]) {
			return "", false
		}
	}
	return prefix, true
}

func isPrefixChar(c byte) bool { return c >= 'a' && c <= 'z' || c >= '2' && c <= '7' }

func isSecretChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// TokenView 是对外的令牌元数据（不含哈希，更不含明文）。
type TokenView struct {
	UID            string
	Name           string
	Prefix         string
	Scopes         []string
	CIDRs          []string
	ExpiresAt      *time.Time
	LastUsedAt     *time.Time
	LastUsedIP     []byte
	RevokedAt      *time.Time
	TenantUID      *string
	RotatedFromUID *string
	CreatedAt      time.Time
}

func toTokenView(t domain.APIToken) TokenView {
	v := TokenView{UID: t.UID, Name: t.Name, Prefix: t.Prefix, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, LastUsedIP: t.LastUsedIP,
		RevokedAt: t.RevokedAt, TenantUID: t.TenantUID, RotatedFromUID: t.RotatedFromUID, CreatedAt: t.CreatedAt}
	_ = json.Unmarshal(t.ScopesJSON, &v.Scopes)
	if len(t.CIDRsJSON) > 0 {
		_ = json.Unmarshal(t.CIDRsJSON, &v.CIDRs)
	}
	if v.Scopes == nil {
		v.Scopes = []string{}
	}
	return v
}

// CreateTokenInput 是创建令牌的输入（expires_at 为空时取默认有效期）。
type CreateTokenInput struct {
	Name      string
	Scopes    []string
	CIDRs     []string
	ExpiresAt *time.Time
}

// owner 解析创建者的所有权边界：Cookie 会话的主体；平台主体 → 平台令牌（tenant_id NULL），租户主体 → 其当前租户的令牌。
// Bearer 主体（API Token 不能管理 API Token）与没有会话/租户上下文的主体一律拒绝（fail-closed）。
func tokenOwner(p *domain.Principal) (userID uint64, tenantID *uint64, err error) {
	if p == nil || p.Bearer || p.Session.ID == 0 {
		return 0, nil, apierr.New(apierr.AuthForbidden, "forbidden", apierr.Detail{Field: "Authorization", Reason: "API tokens are managed with a cookie session"})
	}
	if p.IsPlatform() {
		return p.User.ID, nil, nil
	}
	if p.Tenant == nil {
		return 0, nil, apierr.New(apierr.AuthForbidden, "forbidden")
	}
	id := p.Tenant.ID
	return p.User.ID, &id, nil
}

func (s *Service) validateTokenName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxTokenName {
		return "", apierr.Validation(apierr.Detail{Field: "name", Reason: "must be 1-128 characters"})
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", apierr.Validation(apierr.Detail{Field: "name", Reason: "must not contain control characters"})
		}
	}
	return name, nil
}

// validateScopes：scopes 必须是权限字典里的权限串，且是创建者**当前**权限的子集（防提权，FR-25）。
// 先报字典错误（400），再报越权（403）；token:manage 不能授予令牌（API Token 不能管理令牌）。
func (s *Service) validateScopes(ctx context.Context, p *domain.Principal, scopes []string) ([]string, error) {
	if len(scopes) == 0 || len(scopes) > maxTokenScopes {
		return nil, apierr.Validation(apierr.Detail{Field: "scopes", Reason: "must contain 1-64 permission strings"})
	}
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		if !rbac.Known(sc) {
			return nil, apierr.Validation(apierr.Detail{Field: "scopes", Reason: "contains an unknown permission string"})
		}
		if sc == tokenManagePerm {
			return nil, apierr.Validation(apierr.Detail{Field: "scopes", Reason: "token:manage cannot be granted to an API token"})
		}
		if _, dup := seen[sc]; dup {
			continue
		}
		seen[sc] = struct{}{}
		out = append(out, sc)
	}
	have, err := s.LoadPermissions(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, sc := range out {
		if _, ok := have[sc]; !ok {
			return nil, apierr.New(apierr.AuthForbidden, "forbidden", apierr.Detail{Field: "scopes", Reason: "scopes must be a subset of the creator's current permissions"})
		}
	}
	slices.Sort(out)
	return out, nil
}

// NormalizeCIDRs 校验并规范化来源 IP 白名单：IPv4/IPv6 CIDR 或单个 IP（视为 /32、/128）；IPv4 映射的 IPv6 地址折叠为 IPv4；
// 不接受 /0（要不限制来源，省略该字段）、不接受带 zone 的地址；结果去重并排序。
func NormalizeCIDRs(in []string) ([]string, error) {
	if len(in) > maxTokenCIDRs {
		return nil, apierr.Validation(apierr.Detail{Field: "allowed_cidrs", Reason: "at most 32 entries"})
	}
	bad := func(reason string) error {
		return apierr.Validation(apierr.Detail{Field: "allowed_cidrs", Reason: reason})
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		var pf netip.Prefix
		if strings.Contains(raw, "/") {
			p, err := netip.ParsePrefix(raw)
			if err != nil {
				return nil, bad("contains an invalid CIDR")
			}
			pf = p
		} else {
			a, err := netip.ParseAddr(raw)
			if err != nil || a.Zone() != "" {
				return nil, bad("contains an invalid IP address")
			}
			pf = netip.PrefixFrom(a, a.BitLen())
		}
		if !pf.IsValid() || pf.Addr().Zone() != "" {
			return nil, bad("contains an invalid CIDR")
		}
		if pf.Addr().Is4In6() { // ::ffff:a.b.c.d/n 折叠为 IPv4（来源 IP 比较前同样折叠）
			if pf.Bits() < 96 {
				return nil, bad("IPv4-mapped IPv6 prefixes must be at least /96")
			}
			pf = netip.PrefixFrom(pf.Addr().Unmap(), pf.Bits()-96)
		}
		if pf.Bits() == 0 {
			return nil, bad("a /0 prefix allows every address; omit allowed_cidrs instead")
		}
		c := pf.Masked().String()
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	slices.Sort(out)
	return out, nil
}

// ipAllowed 报告来源 IP 是否落在任一 CIDR 内；来源未知/无法解析 → false（fail-closed）。
func ipAllowed(cidrs []string, ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, c := range cidrs {
		if pf, err := netip.ParsePrefix(c); err == nil && pf.Contains(a) {
			return true
		}
	}
	return false
}

func intersect(a []string, have map[string]struct{}) []string {
	out := make([]string, 0, len(a))
	for _, s := range a {
		if _, ok := have[s]; ok {
			out = append(out, s)
		}
	}
	return out
}

// expiryFor 计算新令牌的过期时间：未指定取默认有效期；指定的须在未来且不超过系统上限。
func (s *Service) expiryFor(ctx context.Context, now time.Time, want *time.Time) (time.Time, error) {
	st := s.settings.get(ctx)
	if want == nil {
		return now.Add(st.APITokenDefaultTTL), nil
	}
	if !want.After(now) {
		return time.Time{}, apierr.Validation(apierr.Detail{Field: "expires_at", Reason: "must be in the future"})
	}
	if want.After(now.Add(st.APITokenMaxTTL)) {
		return time.Time{}, apierr.Validation(apierr.Detail{Field: "expires_at", Reason: fmt.Sprintf("exceeds the system maximum of %d days", int64(st.APITokenMaxTTL/(24*time.Hour)))})
	}
	return *want, nil
}

// insertToken 生成令牌并写库；前缀（40 位随机）冲突时换一个重试。返回明文。
func (s *Service) insertToken(ctx context.Context, t domain.NewAPIToken) (token, uid string, err error) {
	for range maxCreateAttempts {
		var tok, prefix, hash string
		tok, prefix, hash, err = GenerateAPIToken()
		if err != nil {
			return "", "", err
		}
		uidv, uerr := s.ids.New()
		if uerr != nil {
			return "", "", uerr
		}
		t.UID, t.Prefix, t.Hash = uidv, prefix, hash
		if _, err = s.repo.InsertAPIToken(ctx, t); err == nil {
			return tok, uidv, nil
		}
		if !db.IsDuplicate(err) {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("auth: 生成唯一令牌前缀失败: %w", err)
}

// checkAPITokenQuota 在创建令牌的事务内核对租户配额键 api_tokens（Story 1.22b；只做令牌创建这一个硬上限）：
//   - 平台令牌（tenantID == nil）不属于任何租户，不受租户配额约束；
//   - 租户没有 api_tokens 配额行或 limit_value 为 NULL = 不限（默认租户无限制，docs/database/tenant-site.md；系统没有另设「默认上限」——那是产品决定，未被规定）；
//   - 有限额时：本租户当前有效令牌（未吊销且未过期，含所有成员创建的）数 >= 限额 → 403 tenant.quota_exceeded（限额 0 = 不允许创建）。
//     配额行被 FOR UPDATE 锁住，同一租户的并发创建串行化，上限是硬上限；
//   - 轮换不经此检查（新增一枚、同时吊销旧的一枚，有效数不变）。
//
// 与 Story 6.9 的关系：6.9 负责配额的通用强制与用量统计（sites/members/… 以及响应里的 used）；这里只接线令牌创建，不引入通用框架，
// 6.9 落地时可把 LockAPITokenQuota/CountActiveTenantAPITokens 收编进统一的配额检查，行为（NULL=不限、>=限额拒绝）保持一致。
// 租户取自请求 Scope（自助 operation 的 Scope 由主体自身推导），不取自任何请求参数；缺少 Scope 一律 fail-closed。
func (s *Service) checkAPITokenQuota(ctx context.Context, tenantID *uint64, now time.Time) error {
	if tenantID == nil {
		return nil
	}
	sc, ok := tenantscope.FromContext(ctx)
	if !ok || sc.IsPlatform() || sc.TenantID() != *tenantID {
		return apierr.Internal(errors.New("auth: 创建令牌缺少与创建者租户一致的 Scope"))
	}
	limit, err := s.repo.LockAPITokenQuota(ctx, sc)
	if err != nil {
		return err
	}
	if limit == nil {
		return nil
	}
	n, err := s.repo.CountActiveTenantAPITokens(ctx, sc, now)
	if err != nil {
		return err
	}
	if n >= *limit {
		return apierr.New(apierr.TenantQuotaExceeded, "api token quota exceeded", apierr.Detail{Field: "quota_key", Reason: "api_tokens"})
	}
	return nil
}

// CreateAPIToken 创建令牌（Cookie 会话；近期 MFA 由认证钩子先行检查）。返回元数据与**仅此一次**的明文。
func (s *Service) CreateAPIToken(ctx context.Context, p *domain.Principal, in CreateTokenInput, ri RequestInfo) (TokenView, string, error) {
	userID, tenantID, err := tokenOwner(p)
	if err != nil {
		return TokenView{}, "", err
	}
	name, err := s.validateTokenName(in.Name)
	if err != nil {
		return TokenView{}, "", err
	}
	scopes, err := s.validateScopes(ctx, p, in.Scopes)
	if err != nil {
		return TokenView{}, "", err
	}
	cidrs, err := NormalizeCIDRs(in.CIDRs)
	if err != nil {
		return TokenView{}, "", err
	}
	now := s.clk.Now()
	exp, err := s.expiryFor(ctx, now, in.ExpiresAt)
	if err != nil {
		return TokenView{}, "", err
	}
	nt := domain.NewAPIToken{Name: name, UserID: userID, TenantID: tenantID, ExpiresAt: &exp, Now: now}
	if nt.ScopesJSON, err = json.Marshal(scopes); err != nil {
		return TokenView{}, "", apierr.Internal(err)
	}
	if len(cidrs) > 0 {
		if nt.CIDRsJSON, err = json.Marshal(cidrs); err != nil {
			return TokenView{}, "", apierr.Internal(err)
		}
	}
	var token, newUID string
	// 令牌写入与审计同一事务（失败即整体回滚：不会留下「没有审计的令牌」）。变更摘要只含前缀/名称/作用域，绝不含令牌明文或哈希。
	err = s.withTx(ctx, func(ctx context.Context) error {
		if qerr := s.checkAPITokenQuota(ctx, tenantID, now); qerr != nil {
			return qerr
		}
		var ierr error
		if token, newUID, ierr = s.insertToken(ctx, nt); ierr != nil {
			return ierr
		}
		prefix, _ := ParseAPIToken(token)
		return s.audit.Record(ctx, audit.ActionAPITokenCreate, audit.Event{ResourceType: audit.ResourceAPIToken, ResourceUID: newUID,
			Changes: map[string]any{"prefix": prefix, "name": name, "scopes": scopes, "has_cidrs": len(cidrs) > 0, "expires_at": exp.UTC().Format(time.RFC3339)}})
	})
	if err != nil {
		return TokenView{}, "", s.wrapInternal(err)
	}
	prefix, _ := ParseAPIToken(token)
	row, err := s.repo.FindOwnedAPIToken(ctx, userID, tenantID, newUID)
	if err != nil {
		return TokenView{}, "", apierr.Internal(err)
	}
	// 结构化日志：只记前缀与 uid，不记明文，也不记哈希（审计记录已在上面的事务内写入 audit_logs）。
	s.log.Info("API Token 已创建", zap.String("prefix", prefix), zap.String("uid", row.UID),
		zap.String("owner_uid", p.User.UID), zap.Int("scopes", len(scopes)), zap.Bool("has_cidrs", len(cidrs) > 0), zap.String("request_id", ri.RequestID))
	return toTokenView(row), token, nil
}

// RotateAPIToken 轮换：产生新令牌（继承名称/来源限制/有效期时长；scopes 与创建者当前权限取交集）并**立即吊销旧令牌**（旧令牌没有宽限期：
// ADR-018 §7 与 openapi 均写「新令牌 + 吊销旧令牌」，宽限期需要额外的列与状态机，无此需求），新行记 rotated_from_id。已吊销的令牌不能轮换（404）。
func (s *Service) RotateAPIToken(ctx context.Context, p *domain.Principal, tokenUID string, ri RequestInfo) (TokenView, string, error) {
	userID, tenantID, err := tokenOwner(p)
	if err != nil {
		return TokenView{}, "", err
	}
	have, err := s.LoadPermissions(ctx, p)
	if err != nil {
		return TokenView{}, "", err
	}
	var (
		token, oldHash, oldPrefix string
		newUID                    string
	)
	err = s.withTx(ctx, func(ctx context.Context) error {
		old, err := s.repo.LockOwnedAPIToken(ctx, userID, tenantID, tokenUID)
		if err != nil {
			return err
		}
		if old.RevokedAt != nil {
			return domain.ErrNotFound
		}
		var oldScopes []string
		_ = json.Unmarshal(old.ScopesJSON, &oldScopes)
		scopes := intersect(oldScopes, have)
		if len(scopes) == 0 {
			return apierr.New(apierr.AuthForbidden, "forbidden", apierr.Detail{Field: "scopes", Reason: "the creator no longer holds any of the token's scopes"})
		}
		now := s.clk.Now()
		st := s.settings.get(ctx)
		life := st.APITokenDefaultTTL
		if old.ExpiresAt != nil && old.ExpiresAt.After(old.CreatedAt) {
			life = old.ExpiresAt.Sub(old.CreatedAt)
		}
		life = min(life, st.APITokenMaxTTL)
		exp := now.Add(life)
		nt := domain.NewAPIToken{Name: old.Name, UserID: userID, TenantID: tenantID, CIDRsJSON: old.CIDRsJSON, ExpiresAt: &exp, RotatedFromID: &old.ID, Now: now}
		if nt.ScopesJSON, err = json.Marshal(scopes); err != nil {
			return err
		}
		if token, newUID, err = s.insertToken(ctx, nt); err != nil {
			return err
		}
		if _, err := s.repo.RevokeAPIToken(ctx, old.ID, now); err != nil {
			return err
		}
		oldHash, oldPrefix = old.Hash, old.Prefix
		newPrefix, _ := ParseAPIToken(token)
		return s.audit.Record(ctx, audit.ActionAPITokenRotate, audit.Event{ResourceType: audit.ResourceAPIToken, ResourceUID: newUID,
			Changes: map[string]any{"prefix": newPrefix, "rotated_from_uid": tokenUID, "rotated_from_prefix": old.Prefix, "scopes": scopes}})
	})
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return TokenView{}, "", apierr.NotFound()
		}
		return TokenView{}, "", s.wrapInternal(err)
	}
	s.dropTokenKey(ctx, oldHash)
	row, err := s.repo.FindOwnedAPIToken(ctx, userID, tenantID, newUID)
	if err != nil {
		return TokenView{}, "", apierr.Internal(err)
	}
	newPrefix, _ := ParseAPIToken(token)
	s.log.Info("API Token 已轮换（旧令牌已吊销）", zap.String("prefix", newPrefix), zap.String("rotated_from_prefix", oldPrefix),
		zap.String("uid", newUID), zap.String("rotated_from_uid", tokenUID), zap.String("owner_uid", p.User.UID), zap.String("request_id", ri.RequestID))
	return toTokenView(row), token, nil
}

// RevokeAPIToken 吊销令牌：立即失效。先删缓存键、提交吊销、再删一次（第二次堵住「并发请求在吊销提交前回填」的窗口）；
// 删除失败（Redis 不可用）只记日志——授权状态每个请求都从 MySQL 现读，已吊销的令牌不会因缓存残留而继续有效。已吊销的令牌再吊销是幂等的（204）。
func (s *Service) RevokeAPIToken(ctx context.Context, p *domain.Principal, tokenUID string, ri RequestInfo) error {
	userID, tenantID, err := tokenOwner(p)
	if err != nil {
		return err
	}
	row, err := s.repo.FindOwnedAPIToken(ctx, userID, tenantID, tokenUID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return apierr.NotFound()
		}
		return apierr.Internal(err)
	}
	s.dropTokenKey(ctx, row.Hash)
	var changed bool
	if err := s.withTx(ctx, func(ctx context.Context) error {
		var rerr error
		if changed, rerr = s.repo.RevokeAPIToken(ctx, row.ID, s.clk.Now()); rerr != nil {
			return rerr
		}
		// 已吊销的再吊销是幂等的（204）：同样留痕（changed=false），避免「成功响应却无审计」的歧义。
		return s.audit.Record(ctx, audit.ActionAPITokenRevoke, audit.Event{ResourceType: audit.ResourceAPIToken, ResourceUID: row.UID,
			Changes: map[string]any{"prefix": row.Prefix, "changed": changed}})
	}); err != nil {
		return s.wrapInternal(err)
	}
	s.dropTokenKey(ctx, row.Hash)
	if changed {
		s.log.Info("API Token 已吊销", zap.String("prefix", row.Prefix), zap.String("uid", row.UID), zap.String("owner_uid", p.User.UID), zap.String("request_id", ri.RequestID))
	}
	return nil
}

// GetAPIToken 取创建者自己的一个令牌（他人的/不存在的 → 404）。
func (s *Service) GetAPIToken(ctx context.Context, p *domain.Principal, tokenUID string) (TokenView, error) {
	userID, tenantID, err := tokenOwner(p)
	if err != nil {
		return TokenView{}, err
	}
	row, err := s.repo.FindOwnedAPIToken(ctx, userID, tenantID, tokenUID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return TokenView{}, apierr.NotFound()
		}
		return TokenView{}, apierr.Internal(err)
	}
	return toTokenView(row), nil
}

// ListAPITokens 分页列出创建者自己名下的令牌（平台主体：平台令牌；租户主体：其租户内自己创建的令牌）。
func (s *Service) ListAPITokens(ctx context.Context, p *domain.Principal, limit, offset int, orderBy string, desc bool) ([]TokenView, int64, error) {
	userID, tenantID, err := tokenOwner(p)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.CountOwnedAPITokens(ctx, userID, tenantID)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	rows, err := s.repo.ListOwnedAPITokens(ctx, userID, tenantID, orderBy, desc, limit, offset)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	out := make([]TokenView, 0, len(rows))
	for _, r := range rows {
		out = append(out, toTokenView(r))
	}
	return out, total, nil
}

// ---- Bearer 认证 ----

func (s *Service) tokenKey(hash string) (redisx.Key, error) {
	return s.ks.Key("auth", "apitoken", hash)
}

func (s *Service) dropTokenKey(ctx context.Context, hash string) {
	k, err := s.tokenKey(hash)
	if err != nil {
		return
	}
	if err := s.redis.Del(ctx, k); err != nil && !errors.Is(err, redisx.ErrUnavailable) {
		s.log.Warn("删除 API Token 缓存键失败", zap.Error(err))
	}
}

// AuthenticateAPIToken 验证 Bearer 令牌并产出主体。所有失败（格式不对、不存在、过期、吊销、创建者不可用、来源 IP 不符……）都返回同一个 401；
// 原因只写服务端日志（只含前缀）。clientIP 必须来自 httpx.ClientIPFrom。
func (s *Service) AuthenticateAPIToken(ctx context.Context, token, clientIP string) (*domain.Principal, error) {
	if _, ok := ParseAPIToken(token); !ok {
		return nil, errUnauthenticated()
	}
	hash := HashAPIToken(token)
	id, err := s.tokenIDByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, errUnauthenticated()
		}
		return nil, apierr.Internal(err)
	}
	v, err := s.repo.FindTokenAuthView(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.dropTokenKey(ctx, hash)
			return nil, errUnauthenticated()
		}
		return nil, apierr.Internal(err)
	}
	if subtle.ConstantTimeCompare([]byte(v.TokenHash), []byte(hash)) != 1 {
		s.dropTokenKey(ctx, hash) // 缓存里的映射与库不一致（不应发生）：丢弃并拒绝
		return nil, errUnauthenticated()
	}
	now := s.clk.Now()
	var scopes, cidrs []string
	_ = json.Unmarshal(v.ScopesJSON, &scopes)
	if len(v.CIDRsJSON) > 0 {
		_ = json.Unmarshal(v.CIDRsJSON, &cidrs)
	}
	reason := tokenDenyReason(v, now, cidrs, clientIP)
	switch {
	case reason != "":
	case v.MustChange:
		reason = "must_change_password" // 与会话一致：须先改密
	case v.UserType == domain.UserTypePlatform && !v.TOTPEnabled && s.settings.get(ctx).TOTPRequiredForAdmin:
		reason = "totp_enrollment_required" // 与会话一致：强制管理员启用 TOTP 期间未启用者只能走启用流程
	}
	if reason != "" {
		s.dropTokenKey(ctx, hash) // 不再有用的映射顺手清掉（正确性不依赖它：状态每次现读）
		s.tokenDenied(ctx, v, reason, clientIP)
		return nil, errUnauthenticated()
	}
	set := make(map[string]struct{}, len(scopes))
	for _, sc := range scopes {
		set[sc] = struct{}{}
	}
	realm := domain.RealmAdmin
	p := &domain.Principal{Bearer: true, TokenID: v.TokenID, TokenPrefix: v.Prefix, TokenScopes: set}
	if v.TokenTenantID != nil {
		realm = domain.RealmConsole
		p.Tenant = &domain.Tenant{ID: *v.TokenTenantID, UID: deref(v.TenantUID), Name: deref(v.TenantName), Slug: deref(v.TenantSlug), IsDefault: v.TenantDefault != nil && *v.TenantDefault}
	}
	p.Realm = realm
	p.User = domain.User{ID: v.UserID, UID: v.UserUID, UserType: v.UserType, Email: v.Email, DisplayName: v.DisplayName, Status: v.UserStatus,
		Locale: v.Locale, TOTPEnabled: v.TOTPEnabled, MustChange: v.MustChange}
	// 创建者的权限版本随令牌一起现读：权限集缓存键含它，创建者被降权后的下一个请求就读到新权限（令牌随之收缩）。
	p.Session = domain.SessionView{UserID: v.UserID, UserUID: v.UserUID, Realm: string(realm), UserType: v.UserType, PermVersion: v.PermVersion}
	s.noteTokenUsed(v.TokenID, clientIP, now)
	return p, nil
}

// tokenDenyReason 返回令牌当前不可用的原因（空串 = 可用）。原因只用于服务端日志，绝不进响应。
func tokenDenyReason(v domain.TokenAuthView, now time.Time, cidrs []string, clientIP string) string {
	switch {
	case v.RevokedAt != nil:
		return "revoked"
	case v.ExpiresAt != nil && !now.Before(*v.ExpiresAt):
		return "expired"
	case v.UserDeletedAt != nil || v.UserStatus != domain.StatusActive:
		return "owner_unavailable"
	case v.TokenTenantID == nil && v.UserType != domain.UserTypePlatform:
		return "owner_type_mismatch" // 平台令牌只属于平台用户
	case v.TokenTenantID != nil && (v.UserType != domain.UserTypeTenant || !v.MembershipOK || v.TenantUID == nil || v.TenantDeleted != nil || v.TenantStatus == nil || *v.TenantStatus != "active"):
		return "tenant_unavailable"
	case len(cidrs) > 0 && !ipAllowed(cidrs, clientIP):
		return "ip_not_allowed"
	}
	return ""
}

// tokenIDByHash：Redis 缓存「哈希 → 令牌 id」（只缓存存在的令牌；不存在的不缓存，避免随机令牌撑大 Redis）。Redis 故障/缺失直读 MySQL。
func (s *Service) tokenIDByHash(ctx context.Context, hash string) (uint64, error) {
	load := func(ctx context.Context) ([]byte, error) {
		id, err := s.repo.FindAPITokenIDByHash(ctx, hash)
		if err != nil {
			return nil, err
		}
		return []byte(strconv.FormatUint(id, 10)), nil
	}
	k, kerr := s.tokenKey(hash)
	if kerr != nil {
		raw, err := load(ctx)
		if err != nil {
			return 0, err
		}
		return strconv.ParseUint(string(raw), 10, 64)
	}
	raw, err := s.permCache.GetOrLoad(ctx, k, apiTokenCacheTTL, load)
	if err != nil {
		return 0, err
	}
	id, perr := strconv.ParseUint(string(raw), 10, 64)
	if perr != nil { // 缓存值损坏：丢弃并直读
		s.dropTokenKey(ctx, hash)
		raw, err = load(ctx)
		if err != nil {
			return 0, err
		}
		return strconv.ParseUint(string(raw), 10, 64)
	}
	return id, nil
}

// tokenDenied 记录一次「令牌有效匹配但被拒绝」：结构化日志（只含前缀与原因）+ login_logs（token_auth_failed，同一令牌至多每分钟一条）。
func (s *Service) tokenDenied(ctx context.Context, v domain.TokenAuthView, reason, clientIP string) {
	s.log.Warn("API Token 认证被拒绝", zap.String("prefix", v.Prefix), zap.String("reason", reason), zap.String("client_ip", clientIP))
	if k, err := s.ks.Key("auth", "tok_fail_log", strconv.FormatUint(v.TokenID, 10)); err == nil {
		if d, derr := s.limiter.Allow(ctx, k, 1, tokenFailLogEvery); derr == nil && !d.Allowed {
			return
		}
	}
	realm := domain.RealmAdmin
	if v.TokenTenantID != nil {
		realm = domain.RealmConsole
	}
	uidv := v.UserID
	s.writeLog(ctx, realm, "le_"+v.Prefix, domain.EventTokenAuthFailed, reason, &uidv, v.TokenTenantID, RequestInfo{IP: clientIP})
}

// noteTokenUsed 节流地异步更新 last_used_at/last_used_ip：每个令牌每进程至多每分钟写一次，且不阻塞请求（后台写入，并发有上限）。
func (s *Service) noteTokenUsed(id uint64, ip string, now time.Time) {
	s.touchMu.Lock()
	if last, ok := s.touched[id]; ok && now.Sub(last) < tokenTouchInterval {
		s.touchMu.Unlock()
		return
	}
	if len(s.touched) >= maxTouchedEntries {
		clear(s.touched)
	}
	s.touched[id] = now
	s.touchMu.Unlock()
	ipb := RequestInfo{IP: ip}.ipBytes()
	s.background(func(ctx context.Context) {
		if err := s.repo.TouchAPITokenUsed(ctx, id, now, ipb); err != nil {
			s.log.Warn("API Token last_used_at 写入失败", zap.Error(err))
		}
	})
}
