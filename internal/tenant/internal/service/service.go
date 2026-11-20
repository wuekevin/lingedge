// Package service 是租户模块的业务逻辑（Story 1.20）：租户读写、成员、配额（只存取）、租户目录。
//
// 约定：平台路由（/tenants*）以平台 Scope + 路径里的目标租户 uid 操作；不存在的租户一律 404（与非法 uid 同形）；
// 租户可见字段不含节点/集群内部标识（R-T1）——tenants.settings 由平台写入，写入时拒绝带内部标识的键（leakguard.ForbiddenKey）。
// 写操作的审计：audit 模块（Story 1.21）未实现，本模块不写 audit_logs，也不假装已写（过渡的结构化日志在审计钩子里）。
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/leakguard"
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/internal/tenant/internal/domain"
	"github.com/wuekevin/lingedge/internal/tenant/internal/repository"
)

// 输入上限（工程取值）。
const (
	maxNameRunes  = 128
	maxEmailBytes = 255
	maxSettings   = 16 << 10 // settings JSON 序列化后的字节上限
	maxRoleCodes  = 20
	maxQuotaValue = int64(1) << 53 // 与 JSON 数字的精确整数范围一致
)

// Options 是 New 的参数。
type Options struct {
	Repo   *repository.Repo
	WithTx func(ctx context.Context, fn func(ctx context.Context) error) error
	Clock  clock.Clock
}

// Service 是租户服务。
type Service struct {
	repo   *repository.Repo
	withTx func(ctx context.Context, fn func(ctx context.Context) error) error
	clk    clock.Clock
}

// New 创建租户服务。
func New(o Options) (*Service, error) {
	if o.Repo == nil || o.WithTx == nil {
		return nil, errors.New("tenant/service: 缺少 Repo 或 WithTx")
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	return &Service{repo: o.Repo, withTx: o.WithTx, clk: o.Clock}, nil
}

var _ httpx.TenantResolver = (*Service)(nil)

// ResolveTenant 实现 httpx.TenantResolver（租户目录）：每次现读，租户被停用/删除后下一个请求即生效，不缓存。
func (s *Service) ResolveTenant(ctx context.Context, tenantUID string) (httpx.TenantRef, error) {
	if !uid.Valid(tenantUID) {
		return httpx.TenantRef{}, httpx.ErrTenantNotFound
	}
	t, err := s.repo.FindTenantByUID(ctx, tenantUID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return httpx.TenantRef{}, httpx.ErrTenantNotFound
	case err != nil:
		return httpx.TenantRef{}, err
	}
	return httpx.TenantRef{ID: t.ID, UID: t.UID, Status: t.Status}, nil
}

func platformScope(sc scope.Scope) error {
	if !sc.IsPlatform() {
		return apierr.New(apierr.AuthForbidden, "forbidden") // 平台路由只会拿到平台 Scope；其他情形 fail-closed
	}
	return nil
}

// ---- 租户 ----

// Current 返回 Scope 所限定的租户（/tenants/current）。
func (s *Service) Current(ctx context.Context, sc scope.Scope) (domain.Tenant, error) {
	t, err := s.repo.GetScopedTenant(ctx, sc)
	if errors.Is(err, domain.ErrNotFound) {
		return t, apierr.NotFound()
	}
	return t, err
}

// Get 按 uid 取租户（平台）。
func (s *Service) Get(ctx context.Context, sc scope.Scope, tenantUID string) (domain.Tenant, error) {
	if err := platformScope(sc); err != nil {
		return domain.Tenant{}, err
	}
	return s.byUID(ctx, tenantUID)
}

func (s *Service) byUID(ctx context.Context, tenantUID string) (domain.Tenant, error) {
	if !uid.Valid(tenantUID) {
		return domain.Tenant{}, apierr.NotFound() // 非法 uid 与「不存在」同形
	}
	t, err := s.repo.FindTenantByUID(ctx, tenantUID)
	if errors.Is(err, domain.ErrNotFound) {
		return t, apierr.NotFound()
	}
	return t, err
}

// List 分页列出租户（平台）。
func (s *Service) List(ctx context.Context, sc scope.Scope, orderBy string, desc bool, limit, offset int) ([]domain.Tenant, int64, error) {
	if err := platformScope(sc); err != nil {
		return nil, 0, err
	}
	return s.repo.ListTenantsAcrossTenants(ctx, sc, orderBy, desc, limit, offset)
}

// UpdateInput 是 PATCH /tenants/{id} 的输入（nil = 不改）。
type UpdateInput struct {
	Name         *string
	ContactEmail *string
	Locale       *string
	Settings     map[string]any // nil = 不改；非 nil（含空对象）整体替换
}

// Update 更新租户（平台）。至少一个字段；校验失败 400；租户不存在 404。
func (s *Service) Update(ctx context.Context, sc scope.Scope, tenantUID string, in UpdateInput) (domain.Tenant, error) {
	if err := platformScope(sc); err != nil {
		return domain.Tenant{}, err
	}
	var upd domain.TenantUpdate
	var ds []apierr.Detail
	if in.Name == nil && in.ContactEmail == nil && in.Locale == nil && in.Settings == nil {
		ds = append(ds, apierr.Detail{Field: "body", Reason: "at least one field is required"})
	}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || utf8.RuneCountInString(n) > maxNameRunes || hasControl(n) {
			ds = append(ds, apierr.Detail{Field: "name", Reason: "must be 1-128 characters without control characters"})
		}
		upd.Name = &n
	}
	if in.ContactEmail != nil {
		e := strings.ToLower(strings.TrimSpace(*in.ContactEmail))
		if e != "" && !validEmail(e) {
			ds = append(ds, apierr.Detail{Field: "contact_email", Reason: "must be a valid email address (or empty to clear)"})
		}
		upd.ContactEmail = &e
	}
	if in.Locale != nil {
		if *in.Locale != "zh-CN" && *in.Locale != "en" {
			ds = append(ds, apierr.Detail{Field: "locale", Reason: "must be one of: zh-CN, en"})
		}
		upd.Locale = in.Locale
	}
	if in.Settings != nil {
		if bad := leakguard.ForbiddenKeysIn(in.Settings); len(bad) > 0 {
			ds = append(ds, apierr.Detail{Field: "settings", Reason: "must not contain node or cluster identifiers (R-T1)"})
		} else if b, err := json.Marshal(in.Settings); err != nil || len(b) > maxSettings {
			ds = append(ds, apierr.Detail{Field: "settings", Reason: "must be a JSON object of at most 16 KiB"})
		} else {
			upd.SettingsJSON = b
		}
	}
	if len(ds) > 0 {
		return domain.Tenant{}, apierr.Validation(ds...)
	}
	var out domain.Tenant
	err := s.withTx(ctx, func(ctx context.Context) error {
		t, err := s.byUID(ctx, tenantUID)
		if err != nil {
			return err
		}
		if _, err := s.repo.UpdateTenantAcrossTenants(ctx, sc, t.ID, upd); err != nil {
			return err
		}
		out, err = s.byUID(ctx, tenantUID)
		return err
	})
	return out, err
}

// ---- 成员 ----

// Members 分页列出目标租户的成员（平台）。
func (s *Service) Members(ctx context.Context, sc scope.Scope, tenantUID, orderBy string, desc bool, limit, offset int) ([]domain.Member, int64, error) {
	if err := platformScope(sc); err != nil {
		return nil, 0, err
	}
	t, err := s.byUID(ctx, tenantUID)
	if err != nil {
		return nil, 0, err
	}
	return s.repo.ListMembers(ctx, sc, t.ID, orderBy, desc, limit, offset)
}

// AddMemberInput 是 POST /tenants/{id}/members 的输入。
type AddMemberInput struct {
	Email     string
	RoleCodes []string
}

// AddMember 把已有的租户用户加入目标租户并授予租户内角色（平台）。
// 目标用户必须已存在且为租户用户（本 Story 不创建用户账号：租户用户的创建属租户自助注册/邀请，P5）；已是有效成员 → 409。
// 授予角色与成员关系同事务，并递增用户的 permission_version（权限缓存键含版本：授予立即生效）。
func (s *Service) AddMember(ctx context.Context, sc scope.Scope, actorID uint64, tenantUID string, in AddMemberInput) (domain.Member, error) {
	if err := platformScope(sc); err != nil {
		return domain.Member{}, err
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	var ds []apierr.Detail
	if !validEmail(email) {
		ds = append(ds, apierr.Detail{Field: "email", Reason: "must be a valid email address"})
	}
	codes := dedupe(in.RoleCodes)
	if len(codes) > maxRoleCodes {
		ds = append(ds, apierr.Detail{Field: "role_codes", Reason: "too many role codes (max 20)"})
	}
	for _, c := range codes {
		if c == "" || len(c) > 64 {
			ds = append(ds, apierr.Detail{Field: "role_codes", Reason: "role codes must be 1-64 characters"})
			break
		}
	}
	if len(ds) > 0 {
		return domain.Member{}, apierr.Validation(ds...)
	}
	var out domain.Member
	err := s.withTx(ctx, func(ctx context.Context) error {
		t, err := s.byUID(ctx, tenantUID)
		if err != nil {
			return err
		}
		u, err := s.repo.FindUserForMembership(ctx, sc, email)
		if errors.Is(err, domain.ErrNotFound) || (err == nil && u.UserType != "tenant") {
			// 不存在 / 平台用户：同一个 400（调用方是平台主体，且不区分两种情形）。
			return apierr.Validation(apierr.Detail{Field: "email", Reason: "no such tenant user"})
		} else if err != nil {
			return err
		}
		roleIDs := make([]uint64, 0, len(codes))
		for _, c := range codes {
			id, err := s.repo.TenantRoleID(ctx, sc, c)
			if errors.Is(err, domain.ErrNotFound) {
				return apierr.Validation(apierr.Detail{Field: "role_codes", Reason: "unknown tenant role code"})
			} else if err != nil {
				return err
			}
			roleIDs = append(roleIDs, id)
		}
		now := s.clk.Now()
		m, err := s.repo.LockMember(ctx, sc, t.ID, u.ID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			if err := s.repo.InsertMember(ctx, sc, t.ID, u.ID, actorID, now); err != nil {
				if db.IsDuplicate(err) {
					return apierr.Conflict("user is already a member of this tenant")
				}
				return err
			}
		case err != nil:
			return err
		case m.Status == "active":
			return apierr.Conflict("user is already a member of this tenant")
		default:
			if err := s.repo.ReactivateMember(ctx, sc, m.ID, actorID, now); err != nil {
				return err
			}
		}
		for _, id := range roleIDs {
			if err := s.repo.GrantTenantRole(ctx, sc, t.ID, u.ID, id, actorID); err != nil {
				return err
			}
		}
		if err := s.repo.BumpPermissionVersion(ctx, sc, u.ID); err != nil {
			return err
		}
		out, err = s.repo.GetMember(ctx, sc, t.ID, u.ID)
		return err
	})
	return out, err
}

// ---- 配额（只存取；强制执行属 Story 6.9）----

// QuotaView 是一项配额（含默认值：未设置的键 limit=nil、source=default）。
type QuotaView struct {
	Key    string
	Limit  *int64
	Source string
}

// Quotas 返回目标租户的全部配额（字典内的每个键 + 已存储的其他键），按键升序。
func (s *Service) Quotas(ctx context.Context, sc scope.Scope, tenantUID string) ([]QuotaView, error) {
	if err := platformScope(sc); err != nil {
		return nil, err
	}
	t, err := s.byUID(ctx, tenantUID)
	if err != nil {
		return nil, err
	}
	return s.quotasOf(ctx, sc, t.ID)
}

func (s *Service) quotasOf(ctx context.Context, sc scope.Scope, tenantID uint64) ([]QuotaView, error) {
	rows, err := s.repo.ListQuotasAcrossTenants(ctx, sc, tenantID)
	if err != nil {
		return nil, err
	}
	by := map[string]QuotaView{}
	for _, k := range domain.QuotaKeys {
		by[k] = QuotaView{Key: k, Source: "default"}
	}
	for _, r := range rows {
		by[r.Key] = QuotaView{Key: r.Key, Limit: r.Limit, Source: r.Source}
	}
	out := make([]QuotaView, 0, len(by))
	for _, v := range by {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// SetQuotas 设置目标租户的配额：limits 的键必须在字典内，值为 null（不限）或 [0, 2^53]；至少一个键；全部成功或全部不写。
func (s *Service) SetQuotas(ctx context.Context, sc scope.Scope, tenantUID string, limits map[string]*int64) ([]QuotaView, error) {
	if err := platformScope(sc); err != nil {
		return nil, err
	}
	var ds []apierr.Detail
	if len(limits) == 0 {
		ds = append(ds, apierr.Detail{Field: "limits", Reason: "at least one quota is required"})
	}
	keys := make([]string, 0, len(limits))
	for k, v := range limits {
		keys = append(keys, k)
		switch {
		case !domain.KnownQuotaKey(k):
			ds = append(ds, apierr.Detail{Field: "limits." + k, Reason: "unknown quota key"})
		case v != nil && (*v < 0 || *v > maxQuotaValue):
			ds = append(ds, apierr.Detail{Field: "limits." + k, Reason: "must be null (unlimited) or an integer >= 0"})
		}
	}
	if len(ds) > 0 {
		sort.Slice(ds, func(i, j int) bool { return ds[i].Field < ds[j].Field })
		return nil, apierr.Validation(ds...)
	}
	sort.Strings(keys)
	var out []QuotaView
	err := s.withTx(ctx, func(ctx context.Context) error {
		t, err := s.byUID(ctx, tenantUID)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := s.repo.UpsertQuotaAcrossTenants(ctx, sc, t.ID, k, limits[k]); err != nil {
				return err
			}
		}
		out, err = s.quotasOf(ctx, sc, t.ID)
		return err
	})
	return out, err
}

// ---- 辅助 ----

// ParseMemberRoles 解析成员视图里的角色 code（JSON 数组，可为空）。
func ParseMemberRoles(raw []byte) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []string{}
	}
	var codes []string
	if json.Unmarshal(raw, &codes) != nil || codes == nil {
		return []string{}
	}
	sort.Strings(codes)
	return codes
}

func validEmail(e string) bool {
	if e == "" || len(e) > maxEmailBytes || strings.ContainsAny(e, " \t\r\n<>,;\"") {
		return false
	}
	a, err := mail.ParseAddress(e)
	return err == nil && a.Address == e && strings.Contains(e[strings.LastIndex(e, "@"):], ".")
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ParseSettings 解析 tenants.settings_json（NULL/非对象 → nil）。
func ParseSettings(raw []byte) map[string]any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}
