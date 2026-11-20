package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/auth/internal/repository"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/rbac"
)

// 平台用户 / 角色 / 权限点管理（Story 1.18，/admin/users|roles|permissions）。
//
// 不变量（均有真实 MySQL 测试，见 tests/integration/auth/rbac_*_test.go）：
//   - 创建/重置口令：强制首次登录改密（must_change_password=1）；口令由调用方提供或由服务端生成，**生成的明文只经响应回显一次**，
//     库里只有 argon2id 哈希，任何日志都不含它（调用方 handler 另有幂等记录脱敏）。重置同时吊销目标的全部会话。
//   - 最后一个超级管理员（内置角色 platform_admin 的有效持有者）不得被删除、停用或移除该角色：所有可能减少它的操作先取得
//     platform_admin 角色行的事务锁再计数，因此并发请求被串行化——两个并发的「各自移除对方」只有一个成功。内置角色本身不可改/删/改权限。
//   - 防提权：授予角色（含创建用户时指定角色）、向角色加权限，要求操作者自己持有这些权限。
//   - 租户角色只能授予 scope 为 tenant 或 both 的权限（platform 权限永不进入租户角色）。
//   - 审计：audit_logs 的统一接入属 Story 1.21，**此处没有写 audit_logs**（待 1.21 补写）；这里只写不含敏感信息的结构化日志（操作、操作者 uid、目标 uid）。

// 管理操作的长度/格式限制。
const (
	maxDisplayName = 128
	maxRoleName    = 128
	maxRoleDesc    = 512
	maxEmail       = 255
	maxRoleCodes   = 32
	maxRolePerms   = 200
	genPasswordLen = 24
)

var (
	reRoleCode = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
	reEmail    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	reTimezone = regexp.MustCompile(`^[A-Za-z0-9_+/-]{1,48}$`)
)

// 口令生成字母表：去掉易混淆字符（0/O、1/l/I）。
const oneTimeCharset = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

// GeneratePassword 生成 genPasswordLen 位的一次性口令（crypto/rand + 拒绝采样，无模偏差）。
func GeneratePassword() (string, error) {
	n := big.NewInt(int64(len(oneTimeCharset)))
	b := make([]byte, genPasswordLen)
	for i := range b {
		v, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		b[i] = oneTimeCharset[v.Int64()]
	}
	return string(b), nil
}

func conflict(msg, field, reason string) error {
	return apierr.Conflict(msg, apierr.Detail{Field: field, Reason: reason})
}

func forbidden(msg, field, reason string) error {
	return apierr.New(apierr.AuthForbidden, msg, apierr.Detail{Field: field, Reason: reason})
}

func (s *Service) actorLog(op string, actor *domain.Principal, ri RequestInfo, fields ...zap.Field) {
	base := []zap.Field{zap.String("op", op), zap.String("actor_uid", actor.User.UID), zap.String("request_id", ri.RequestID), zap.Bool("audit_logs_written", false)}
	s.log.Info("RBAC 管理操作（audit_logs 待 Story 1.21 补写）", append(base, fields...)...)
}

// validateNewPassword：与改密同一策略（12–128 字符，不得等于邮箱）。
func validateNewPassword(field, pw, email string) error {
	if n := utf8.RuneCountInString(pw); n < minNewPassword || n > maxNewPassword || len(pw) > 4*maxNewPassword {
		return apierr.New(apierr.AuthPasswordPolicy, "password does not meet the policy",
			apierr.Detail{Field: field, Reason: fmt.Sprintf("must be %d-%d characters", minNewPassword, maxNewPassword)})
	}
	if strings.EqualFold(pw, email) {
		return apierr.New(apierr.AuthPasswordPolicy, "password does not meet the policy", apierr.Detail{Field: field, Reason: "must not be the email address"})
	}
	return nil
}

// ---- 视图 ----

// UserView 是 /admin/users 的用户视图。
type UserView struct {
	domain.AdminUser
	RoleCodes []string
}

func (s *Service) userView(ctx context.Context, u domain.AdminUser) (UserView, error) {
	codes, err := s.repo.UserPlatformRoleCodes(ctx, u.ID)
	if err != nil {
		return UserView{}, apierr.Internal(err)
	}
	if codes == nil {
		codes = []string{}
	}
	return UserView{AdminUser: u, RoleCodes: codes}, nil
}

// ListUsers 分页列出平台用户。
func (s *Service) ListUsers(ctx context.Context, limit, offset int, sort string, desc bool) ([]UserView, int64, error) {
	rows, err := s.repo.ListPlatformUsers(ctx, limit, offset, sort, desc)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	total, err := s.repo.CountPlatformUsers(ctx)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	codes, err := s.repo.PlatformRoleCodesByUser(ctx)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	out := make([]UserView, 0, len(rows))
	for _, r := range rows {
		c := codes[r.ID]
		if c == nil {
			c = []string{}
		}
		out = append(out, UserView{AdminUser: r, RoleCodes: c})
	}
	return out, total, nil
}

// GetUser 取平台用户详情。
func (s *Service) GetUser(ctx context.Context, uid string) (UserView, error) {
	u, err := s.repo.FindPlatformUserByUID(ctx, uid)
	if err != nil {
		return UserView{}, notFoundErr(err)
	}
	return s.userView(ctx, u)
}

func notFoundErr(err error) error {
	if errors.Is(err, domain.ErrNotFound) {
		return apierr.NotFound()
	}
	return apierr.Internal(err)
}

// ---- 角色解析与防提权 ----

// resolvePlatformRoles 把 role_codes 解析成角色（须存在且为平台角色；重复项去重）。
func (s *Service) resolvePlatformRoles(ctx context.Context, field string, codes []string) ([]domain.Role, error) {
	if len(codes) > maxRoleCodes {
		return nil, apierr.Validation(apierr.Detail{Field: field, Reason: fmt.Sprintf("at most %d roles", maxRoleCodes)})
	}
	seen := map[string]bool{}
	var roles []domain.Role
	for _, c := range codes {
		if seen[c] {
			continue
		}
		seen[c] = true
		r, err := s.repo.FindPlatformRoleByCode(ctx, c)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, apierr.Validation(apierr.Detail{Field: field, Reason: "unknown platform role: " + trunc(c, 64)})
			}
			return nil, apierr.Internal(err)
		}
		roles = append(roles, r)
	}
	return roles, nil
}

// requireCanGrant 防提权：操作者须持有被授予角色所含的每一项权限，才能把该角色授予他人。
func (s *Service) requireCanGrant(ctx context.Context, actor *domain.Principal, roles []domain.Role) error {
	for _, r := range roles {
		perms, err := s.repo.RolePermissionCodes(ctx, r.ID)
		if err != nil {
			return apierr.Internal(err)
		}
		for _, p := range perms {
			if !actor.HasPermission(p) {
				return forbidden("cannot grant a role with permissions you do not hold", "role_codes", "role "+r.Code+" includes a permission you do not hold")
			}
		}
	}
	return nil
}

// ---- 用户 CRUD ----

// CreateUserInput 是创建平台用户的输入。InitialPassword 为空表示由服务端生成。
type CreateUserInput struct {
	Email, DisplayName, UserType, InitialPassword, Locale string
	RoleCodes                                             []string
}

// CreateUser 创建平台用户：must_change_password=1；返回用户视图与（仅当由服务端生成时的）一次性明文口令。
func (s *Service) CreateUser(ctx context.Context, actor *domain.Principal, in CreateUserInput, ri RequestInfo) (UserView, string, error) {
	email := NormalizeEmail(in.Email)
	name := strings.TrimSpace(in.DisplayName)
	var ds []apierr.Detail
	if in.UserType != domain.UserTypePlatform {
		ds = append(ds, apierr.Detail{Field: "user_type", Reason: "only platform users are managed here"})
	}
	if email == "" || len(email) > maxEmail || !reEmail.MatchString(email) {
		ds = append(ds, apierr.Detail{Field: "email", Reason: "must be a valid email address (at most 255 characters)"})
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > maxDisplayName {
		ds = append(ds, apierr.Detail{Field: "display_name", Reason: fmt.Sprintf("must be 1-%d characters", maxDisplayName)})
	}
	locale := in.Locale
	if locale == "" {
		locale = "zh-CN"
	}
	if locale != "zh-CN" && locale != "en" {
		ds = append(ds, apierr.Detail{Field: "locale", Reason: "must be zh-CN or en"})
	}
	if len(ds) > 0 {
		return UserView{}, "", apierr.Validation(ds...)
	}
	generated := false
	pw := in.InitialPassword
	if pw == "" {
		var err error
		if pw, err = GeneratePassword(); err != nil {
			return UserView{}, "", apierr.Internal(err)
		}
		generated = true
	} else if err := validateNewPassword("initial_password", pw, email); err != nil {
		return UserView{}, "", err
	}
	roles, err := s.resolvePlatformRoles(ctx, "role_codes", in.RoleCodes)
	if err != nil {
		return UserView{}, "", err
	}
	if len(roles) > 0 {
		if !actor.HasPermission("role:write") {
			return UserView{}, "", forbidden("assigning roles requires role:write", "role_codes", "role:write is required to assign roles")
		}
		if err := s.requireCanGrant(ctx, actor, roles); err != nil {
			return UserView{}, "", err
		}
	}
	hash, err := s.hasher.Hash(ctx, pw)
	if err != nil {
		return UserView{}, "", apierr.Internal(err)
	}
	newUID, err := s.ids.New()
	if err != nil {
		return UserView{}, "", apierr.Internal(err)
	}
	now := s.clk.Now()
	err = s.withTx(ctx, func(ctx context.Context) error {
		id, err := s.repo.InsertPlatformUser(ctx, domain.NewUser{UID: newUID, Email: email, DisplayName: name, PasswordHash: hash, Locale: locale, Now: now})
		if err != nil {
			if db.IsDuplicate(err) {
				return conflict("email already in use", "email", "a user with this email already exists")
			}
			return err
		}
		ids := make([]uint64, 0, len(roles))
		for _, r := range roles {
			ids = append(ids, r.ID)
		}
		return s.repo.ReplaceUserPlatformRoles(ctx, id, ids, actor.User.ID)
	})
	if err != nil {
		return UserView{}, "", s.wrapInternal(err)
	}
	u, err := s.repo.FindPlatformUserByUID(ctx, newUID)
	if err != nil {
		return UserView{}, "", notFoundErr(err)
	}
	v, err := s.userView(ctx, u)
	if err != nil {
		return UserView{}, "", err
	}
	s.actorLog("user.create", actor, ri, zap.String("target_uid", newUID), zap.Strings("role_codes", in.RoleCodes), zap.Bool("password_generated", generated))
	if !generated {
		pw = ""
	}
	return v, pw, nil
}

// UpdateUserInput 是更新平台用户的输入（nil 表示不改）。
type UpdateUserInput struct {
	DisplayName, Status, Locale, Timezone *string
}

// lockSuperAdminAndTarget 在事务内先锁超级管理员角色行、再锁目标用户行（固定顺序，无死锁），返回角色 id 与目标。
func (s *Service) lockSuperAdminAndTarget(ctx context.Context, uid string) (uint64, domain.AdminUser, error) {
	rid, err := s.repo.LockSuperAdminRole(ctx)
	if err != nil {
		return 0, domain.AdminUser{}, fmt.Errorf("auth: 无法锁定内置超级管理员角色（种子缺失？）: %v", err) // 不包装哨兵：这是服务端不一致，不是 404
	}
	u, err := s.repo.FindPlatformUserByUIDForUpdate(ctx, uid)
	return rid, u, err
}

// guardLastSuperAdmin：目标是有效超级管理员且是最后一个时拒绝（409）。须在持有角色锁的事务内调用。
func (s *Service) guardLastSuperAdmin(ctx context.Context, roleID uint64, target domain.AdminUser, what string) error {
	ids, err := s.repo.ActiveSuperAdminIDs(ctx, roleID)
	if err != nil {
		return err
	}
	if slices.Contains(ids, target.ID) && len(ids) <= 1 {
		return conflict("the last super administrator cannot be "+what, "user", "at least one active platform_admin must remain")
	}
	return nil
}

// UpdateUser 更新展示名/状态/语言/时区；停用会吊销目标的全部会话；停用最后一个超级管理员被拒绝。
func (s *Service) UpdateUser(ctx context.Context, actor *domain.Principal, uid string, in UpdateUserInput, ri RequestInfo) (UserView, error) {
	var ds []apierr.Detail
	if in.DisplayName != nil {
		v := strings.TrimSpace(*in.DisplayName)
		in.DisplayName = &v
		if n := utf8.RuneCountInString(v); n < 1 || n > maxDisplayName {
			ds = append(ds, apierr.Detail{Field: "display_name", Reason: fmt.Sprintf("must be 1-%d characters", maxDisplayName)})
		}
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "disabled" {
		ds = append(ds, apierr.Detail{Field: "status", Reason: "must be active or disabled"})
	}
	if in.Locale != nil && *in.Locale != "zh-CN" && *in.Locale != "en" {
		ds = append(ds, apierr.Detail{Field: "locale", Reason: "must be zh-CN or en"})
	}
	if in.Timezone != nil && !reTimezone.MatchString(*in.Timezone) {
		ds = append(ds, apierr.Detail{Field: "timezone", Reason: "must be an IANA time zone name"})
	}
	if len(ds) > 0 {
		return UserView{}, apierr.Validation(ds...)
	}
	now := s.clk.Now()
	var target domain.AdminUser
	err := s.withTx(ctx, func(ctx context.Context) error {
		rid, u, err := s.lockSuperAdminAndTarget(ctx, uid)
		if err != nil {
			return notFoundErr(err)
		}
		target = u
		disabling := in.Status != nil && *in.Status == "disabled" && u.Status == domain.StatusActive
		if disabling {
			if err := s.guardLastSuperAdmin(ctx, rid, u, "disabled"); err != nil {
				return err
			}
		}
		if err := s.repo.UpdateUserProfile(ctx, u.ID, in.DisplayName, in.Status, in.Locale, in.Timezone); err != nil {
			return err
		}
		if in.Status != nil && *in.Status == "disabled" {
			if _, err := s.repo.RevokeUserSessions(ctx, u.ID, domain.RevokeAdmin, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return UserView{}, s.wrapInternal(err)
	}
	s.actorLog("user.update", actor, ri, zap.String("target_uid", uid), zap.Bool("status_changed", in.Status != nil))
	return s.GetUser(ctx, target.UID)
}

// DeleteUser 软删除平台用户：吊销其全部会话、清除其角色授予；删除最后一个超级管理员被拒绝。
func (s *Service) DeleteUser(ctx context.Context, actor *domain.Principal, uid string, ri RequestInfo) error {
	now := s.clk.Now()
	err := s.withTx(ctx, func(ctx context.Context) error {
		rid, u, err := s.lockSuperAdminAndTarget(ctx, uid)
		if err != nil {
			return notFoundErr(err)
		}
		if err := s.guardLastSuperAdmin(ctx, rid, u, "deleted"); err != nil {
			return err
		}
		if err := s.repo.SoftDeleteUser(ctx, u.ID, now); err != nil {
			return err
		}
		if _, err := s.repo.RevokeUserSessions(ctx, u.ID, domain.RevokeAdmin, now); err != nil {
			return err
		}
		if err := s.repo.ReplaceUserPlatformRoles(ctx, u.ID, nil, 0); err != nil {
			return err
		}
		return s.repo.BumpUserPermissionVersion(ctx, u.ID)
	})
	if err != nil {
		return s.wrapInternal(err)
	}
	s.actorLog("user.delete", actor, ri, zap.String("target_uid", uid))
	return nil
}

// SetUserRoles 整体设置用户的平台角色：移除最后一个超级管理员的 platform_admin 被拒绝；新增的角色受防提权约束。
func (s *Service) SetUserRoles(ctx context.Context, actor *domain.Principal, uid string, codes []string, ri RequestInfo) (UserView, error) {
	roles, err := s.resolvePlatformRoles(ctx, "role_codes", codes)
	if err != nil {
		return UserView{}, err
	}
	var target domain.AdminUser
	var affected []domain.AffectedAccess
	err = s.withTx(ctx, func(ctx context.Context) error {
		rid, u, err := s.lockSuperAdminAndTarget(ctx, uid)
		if err != nil {
			return notFoundErr(err)
		}
		target = u
		cur, err := s.repo.UserPlatformRoleIDs(ctx, u.ID)
		if err != nil {
			return err
		}
		want := make([]uint64, 0, len(roles))
		var added []domain.Role
		for _, r := range roles {
			want = append(want, r.ID)
			if !slices.Contains(cur, r.ID) {
				added = append(added, r)
			}
		}
		if err := s.requireCanGrant(ctx, actor, added); err != nil {
			return err
		}
		if slices.Contains(cur, rid) && !slices.Contains(want, rid) {
			if err := s.guardLastSuperAdmin(ctx, rid, u, "demoted"); err != nil {
				return err
			}
		}
		if err := s.repo.ReplaceUserPlatformRoles(ctx, u.ID, want, actor.User.ID); err != nil {
			return err
		}
		if err := s.repo.BumpUserPermissionVersion(ctx, u.ID); err != nil {
			return err
		}
		affected, err = s.repo.UserAccessScopes(ctx, u.ID)
		return err
	})
	if err != nil {
		return UserView{}, s.wrapInternal(err)
	}
	s.invalidatePermissions(ctx, affected)
	s.actorLog("user.set_roles", actor, ri, zap.String("target_uid", uid), zap.Strings("role_codes", codes))
	return s.GetUser(ctx, target.UID)
}

// UnlockUser 解除锁定：清除 DB 失败计数/锁定截止与 Redis 账号维度节流计数；状态为 locked 的恢复为 active。
func (s *Service) UnlockUser(ctx context.Context, actor *domain.Principal, uid string, ri RequestInfo) (UserView, error) {
	var u domain.AdminUser
	err := s.withTx(ctx, func(ctx context.Context) error {
		var err error
		if u, err = s.repo.FindPlatformUserByUIDForUpdate(ctx, uid); err != nil {
			return notFoundErr(err)
		}
		return s.repo.UnlockUser(ctx, u.ID)
	})
	if err != nil {
		return UserView{}, s.wrapInternal(err)
	}
	s.resetThrottle(ctx, u.Email)
	s.actorLog("user.unlock", actor, ri, zap.String("target_uid", uid))
	return s.GetUser(ctx, uid)
}

func (s *Service) resetThrottle(ctx context.Context, email string) {
	k, err := s.acctKey(domain.RealmAdmin, email)
	if err != nil {
		return
	}
	if err := s.limiter.Reset(ctx, k); err != nil {
		s.log.Warn("清零登录节流计数失败", zap.Error(err))
	}
}

// ResetPassword 强制重置口令：newPassword 为空时由服务端生成（返回一次性明文）；目标 must_change_password=1，全部会话被吊销。
func (s *Service) ResetPassword(ctx context.Context, actor *domain.Principal, uid, newPassword string, ri RequestInfo) (string, error) {
	u, err := s.repo.FindPlatformUserByUID(ctx, uid)
	if err != nil {
		return "", notFoundErr(err)
	}
	generated := false
	pw := newPassword
	if pw == "" {
		if pw, err = GeneratePassword(); err != nil {
			return "", apierr.Internal(err)
		}
		generated = true
	} else if err := validateNewPassword("new_password", pw, u.Email); err != nil {
		return "", err
	}
	hash, err := s.hasher.Hash(ctx, pw)
	if err != nil {
		return "", apierr.Internal(err)
	}
	now := s.clk.Now()
	err = s.withTx(ctx, func(ctx context.Context) error {
		cur, err := s.repo.FindPlatformUserByUIDForUpdate(ctx, uid)
		if err != nil {
			return notFoundErr(err)
		}
		if err := s.repo.ResetPassword(ctx, cur.ID, hash, now); err != nil {
			return err
		}
		_, err = s.repo.RevokeUserSessions(ctx, cur.ID, domain.RevokeAdmin, now)
		return err
	})
	if err != nil {
		return "", s.wrapInternal(err)
	}
	s.resetThrottle(ctx, u.Email)
	s.actorLog("user.reset_password", actor, ri, zap.String("target_uid", uid), zap.Bool("password_generated", generated))
	if !generated {
		pw = ""
	}
	return pw, nil
}

// RevokeUserSessions 吊销目标的全部会话。
func (s *Service) RevokeUserSessions(ctx context.Context, actor *domain.Principal, uid string, ri RequestInfo) error {
	u, err := s.repo.FindPlatformUserByUID(ctx, uid)
	if err != nil {
		return notFoundErr(err)
	}
	if _, err := s.repo.RevokeUserSessions(ctx, u.ID, domain.RevokeAdmin, s.clk.Now()); err != nil {
		return apierr.Internal(err)
	}
	s.actorLog("user.revoke_sessions", actor, ri, zap.String("target_uid", uid))
	return nil
}

// ---- 角色 ----

// RoleView 是角色视图。
type RoleView struct {
	domain.Role
	Permissions []string
}

func (s *Service) roleView(ctx context.Context, r domain.Role) (RoleView, error) {
	perms, err := s.repo.RolePermissionCodes(ctx, r.ID)
	if err != nil {
		return RoleView{}, apierr.Internal(err)
	}
	if perms == nil {
		perms = []string{}
	}
	return RoleView{Role: r, Permissions: perms}, nil
}

// ListRoles 分页列出模板角色及其权限。
func (s *Service) ListRoles(ctx context.Context, limit, offset int) ([]RoleView, int64, error) {
	rows, err := s.repo.ListRoles(ctx, limit, offset)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	total, err := s.repo.CountRoles(ctx)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	all, err := s.repo.AllRolePermissionCodes(ctx)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	out := make([]RoleView, 0, len(rows))
	for _, r := range rows {
		p := all[r.ID]
		if p == nil {
			p = []string{}
		}
		out = append(out, RoleView{Role: r, Permissions: p})
	}
	return out, total, nil
}

// GetRole 取角色详情。
func (s *Service) GetRole(ctx context.Context, uid string) (RoleView, error) {
	r, err := s.repo.FindRoleByUID(ctx, uid)
	if err != nil {
		return RoleView{}, notFoundErr(err)
	}
	return s.roleView(ctx, r)
}

// validateRolePermissions 校验权限串：去重、须在注册表内；租户角色只能含 scope 为 tenant/both 的权限。
func validateRolePermissions(scope string, perms []string) ([]string, error) {
	if len(perms) > maxRolePerms {
		return nil, apierr.Validation(apierr.Detail{Field: "permissions", Reason: fmt.Sprintf("at most %d permissions", maxRolePerms)})
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		if seen[p] {
			continue
		}
		seen[p] = true
		if !rbac.Known(p) {
			return nil, apierr.Validation(apierr.Detail{Field: "permissions", Reason: "unknown permission: " + trunc(p, 96)})
		}
		if scope == domain.ScopeTenantStr && !rbac.GrantableToTenantRole(p) {
			return nil, apierr.Validation(apierr.Detail{Field: "permissions", Reason: "tenant roles can only include tenant or both scope permissions: " + p})
		}
		out = append(out, p)
	}
	slices.Sort(out)
	return out, nil
}

// requireHolds 防提权：操作者须持有 perms 中的每一项。
func requireHolds(actor *domain.Principal, perms []string) error {
	for _, p := range perms {
		if !actor.HasPermission(p) {
			return forbidden("cannot grant a permission you do not hold", "permissions", "you do not hold "+p)
		}
	}
	return nil
}

// CreateRoleInput 是创建角色的输入。
type CreateRoleInput struct {
	Scope, Code, Name string
	Description       *string
	Permissions       []string
}

// CreateRole 创建（非内置）模板角色。
func (s *Service) CreateRole(ctx context.Context, actor *domain.Principal, in CreateRoleInput, ri RequestInfo) (RoleView, error) {
	var ds []apierr.Detail
	if in.Scope != domain.ScopePlatform && in.Scope != domain.ScopeTenantStr {
		ds = append(ds, apierr.Detail{Field: "scope", Reason: "must be platform or tenant"})
	}
	if !reRoleCode.MatchString(in.Code) {
		ds = append(ds, apierr.Detail{Field: "code", Reason: "must match ^[a-z][a-z0-9_]{1,63}$"})
	}
	name := strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > maxRoleName {
		ds = append(ds, apierr.Detail{Field: "name", Reason: fmt.Sprintf("must be 1-%d characters", maxRoleName)})
	}
	if in.Description != nil && utf8.RuneCountInString(*in.Description) > maxRoleDesc {
		ds = append(ds, apierr.Detail{Field: "description", Reason: fmt.Sprintf("at most %d characters", maxRoleDesc)})
	}
	if len(ds) > 0 {
		return RoleView{}, apierr.Validation(ds...)
	}
	perms, err := validateRolePermissions(in.Scope, in.Permissions)
	if err != nil {
		return RoleView{}, err
	}
	if err := requireHolds(actor, perms); err != nil {
		return RoleView{}, err
	}
	uid, err := s.ids.New()
	if err != nil {
		return RoleView{}, apierr.Internal(err)
	}
	err = s.withTx(ctx, func(ctx context.Context) error {
		id, err := s.repo.InsertRole(ctx, uid, in.Scope, in.Code, name, in.Description)
		if err != nil {
			if db.IsDuplicate(err) {
				return conflict("role code already in use", "code", "a role with this code already exists in this scope")
			}
			return err
		}
		return s.repo.ReplaceRolePermissions(ctx, id, perms)
	})
	if err != nil {
		return RoleView{}, s.wrapInternal(err)
	}
	s.actorLog("role.create", actor, ri, zap.String("role_uid", uid), zap.String("scope", in.Scope))
	return s.GetRole(ctx, uid)
}

func builtinImmutable(r domain.Role) error {
	if r.IsBuiltin {
		return conflict("built-in roles are immutable", "role", "built-in roles cannot be changed or deleted")
	}
	return nil
}

// UpdateRole 更新名称/描述；内置角色不可改。
func (s *Service) UpdateRole(ctx context.Context, actor *domain.Principal, uid string, name, description *string, ri RequestInfo) (RoleView, error) {
	var ds []apierr.Detail
	if name != nil {
		v := strings.TrimSpace(*name)
		name = &v
		if n := utf8.RuneCountInString(v); n < 1 || n > maxRoleName {
			ds = append(ds, apierr.Detail{Field: "name", Reason: fmt.Sprintf("must be 1-%d characters", maxRoleName)})
		}
	}
	if description != nil && utf8.RuneCountInString(*description) > maxRoleDesc {
		ds = append(ds, apierr.Detail{Field: "description", Reason: fmt.Sprintf("at most %d characters", maxRoleDesc)})
	}
	if len(ds) > 0 {
		return RoleView{}, apierr.Validation(ds...)
	}
	err := s.withTx(ctx, func(ctx context.Context) error {
		r, err := s.repo.FindRoleByUIDForUpdate(ctx, uid)
		if err != nil {
			return notFoundErr(err)
		}
		if err := builtinImmutable(r); err != nil {
			return err
		}
		return s.repo.UpdateRole(ctx, r.ID, name, description)
	})
	if err != nil {
		return RoleView{}, s.wrapInternal(err)
	}
	s.actorLog("role.update", actor, ri, zap.String("role_uid", uid))
	return s.GetRole(ctx, uid)
}

// DeleteRole 删除角色；内置角色与仍有持有者的角色不可删。
func (s *Service) DeleteRole(ctx context.Context, actor *domain.Principal, uid string, ri RequestInfo) error {
	err := s.withTx(ctx, func(ctx context.Context) error {
		r, err := s.repo.FindRoleByUIDForUpdate(ctx, uid)
		if err != nil {
			return notFoundErr(err)
		}
		if err := builtinImmutable(r); err != nil {
			return err
		}
		n, err := s.repo.CountRoleHolders(ctx, r.ID)
		if err != nil {
			return err
		}
		if n > 0 {
			return conflict("role is still assigned", "role", "remove the role from its holders before deleting it")
		}
		return s.repo.DeleteRole(ctx, r.ID)
	})
	if err != nil {
		return s.wrapInternal(err)
	}
	s.actorLog("role.delete", actor, ri, zap.String("role_uid", uid))
	return nil
}

// SetRolePermissions 整体设置角色权限：内置角色不可改；租户角色只能含 tenant/both 权限；新增的权限受防提权约束；
// 同事务内使该角色所有持有者的 permission_version + 1，提交后清缓存并广播。
func (s *Service) SetRolePermissions(ctx context.Context, actor *domain.Principal, uid string, perms []string, ri RequestInfo) (RoleView, error) {
	var affected []domain.AffectedAccess
	err := s.withTx(ctx, func(ctx context.Context) error {
		r, err := s.repo.FindRoleByUIDForUpdate(ctx, uid)
		if err != nil {
			return notFoundErr(err)
		}
		if err := builtinImmutable(r); err != nil {
			return err
		}
		want, err := validateRolePermissions(r.Scope, perms)
		if err != nil {
			return err
		}
		cur, err := s.repo.RolePermissionCodes(ctx, r.ID)
		if err != nil {
			return err
		}
		var added []string
		for _, p := range want {
			if !slices.Contains(cur, p) {
				added = append(added, p)
			}
		}
		if err := requireHolds(actor, added); err != nil {
			return err
		}
		if err := s.repo.ReplaceRolePermissions(ctx, r.ID, want); err != nil {
			if errors.Is(err, repository.ErrUnknownPermission) {
				return apierr.Validation(apierr.Detail{Field: "permissions", Reason: "unknown permission"})
			}
			return err
		}
		affected, err = s.repo.BumpRolePermissionVersion(ctx, r.ID)
		return err
	})
	if err != nil {
		return RoleView{}, s.wrapInternal(err)
	}
	s.invalidatePermissions(ctx, affected)
	s.actorLog("role.set_permissions", actor, ri, zap.String("role_uid", uid), zap.Int("affected_users", len(affected)))
	return s.GetRole(ctx, uid)
}

// ---- 权限点字典 ----

// ListPermissions 分页列出权限点字典。
func (s *Service) ListPermissions(ctx context.Context, limit, offset int) ([]domain.Permission, int64, error) {
	rows, err := s.repo.ListPermissions(ctx, limit, offset)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	total, err := s.repo.CountPermissions(ctx)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	return rows, total, nil
}
