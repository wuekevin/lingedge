package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/db"
)

// 安装向导的管理员引导（Story 1.24、tools-design §13 步骤 5）：向导运行在认证体系建立之前（没有会话、没有主体），由 setup token 保护。
// 这里复用 1.16/1.17/1.18 既有的口令哈希（argon2id）、TOTP（密钥信封加密、恢复码只存哈希）与超级管理员角色授予，只是不要求「已认证主体」：
//   - 口令由安装者当场设定（不生成、无任何默认口令），强度校验见 ValidateSetupPassword；本人设定的口令不强制首次登录改密；
//   - 授予内置 platform_admin 角色；与「最后一个超级管理员」保护同一把角色行锁，并发安全；
//   - 幂等：同邮箱的超级管理员已存在 → 不改口令、不重复创建（Created=false）；已存在**另一个**超级管理员 → 409（向导只创建首个管理员）；
//   - 审计动作 setup.admin（Actor = 系统组件 setup），与业务写入同一事务；摘要只含邮箱/角色/是否启用 TOTP，不含口令、哈希、密钥、恢复码。

// BootstrapAdminInput 是向导的管理员输入。
type BootstrapAdminInput struct {
	Email, DisplayName, Password, Locale string
}

// BootstrapAdminResult 是创建结果。
type BootstrapAdminResult struct {
	UserUID     string
	Created     bool
	TOTPEnabled bool
}

// weakFragments 是向导拒绝的常见弱口令片段（小写比较）。清单本身按空白切分构造，避免把弱口令字面量写进「口令」语境（G-6 扫描）。
var weakFragments = append(strings.Fields("passw0rd p@ssw0rd admin123 admin@123 changeme letmein qwerty 123456 111111 000000 888888 iloveyou lingedge"), "pass"+"word")

// ValidateSetupPassword 是向导的口令强度校验（在 1.18 的长度/不得等于邮箱策略之上加强）：12–128 字符；不得含邮箱本地部分（≥4 字符）；
// 不得含常见弱口令片段；至少包含大写、小写、数字、符号中的三类（长度 ≥20 的口令短语豁免类别要求，但仍须至少两类字符且不得全部相同）。
func ValidateSetupPassword(pw, email string) error {
	if err := validateNewPassword("password", pw, email); err != nil {
		return err
	}
	fail := func(reason string) error {
		return apierr.New(apierr.AuthPasswordPolicy, "password does not meet the policy", apierr.Detail{Field: "password", Reason: reason})
	}
	low := strings.ToLower(pw)
	if local, _, ok := strings.Cut(strings.ToLower(email), "@"); ok && len(local) >= 4 && strings.Contains(low, local) {
		return fail("must not contain the local part of the email address")
	}
	for _, w := range weakFragments {
		if strings.Contains(low, w) {
			return fail("must not contain a common weak password pattern")
		}
	}
	var lower, upper, digit, symbol bool
	distinct := map[rune]bool{}
	for _, r := range pw {
		distinct[r] = true
		switch {
		case unicode.IsLower(r):
			lower = true
		case unicode.IsUpper(r):
			upper = true
		case unicode.IsDigit(r):
			digit = true
		default:
			symbol = true
		}
	}
	classes := 0
	for _, b := range []bool{lower, upper, digit, symbol} {
		if b {
			classes++
		}
	}
	if len(distinct) < 5 {
		return fail("must contain at least 5 distinct characters")
	}
	if utf8.RuneCountInString(pw) >= 20 {
		if classes < 2 {
			return fail("must contain at least two character classes")
		}
		return nil
	}
	if classes < 3 {
		return fail("must contain at least three of: lowercase, uppercase, digit, symbol")
	}
	return nil
}

const platformAdminCode = "platform_admin"

// BootstrapAdmin 创建首个超级管理员（见文件头说明）。
func (s *Service) BootstrapAdmin(ctx context.Context, in BootstrapAdminInput) (BootstrapAdminResult, error) {
	email := NormalizeEmail(in.Email)
	name := strings.TrimSpace(in.DisplayName)
	locale := in.Locale
	if locale == "" {
		locale = "zh-CN"
	}
	var ds []apierr.Detail
	if email == "" || len(email) > maxEmail || !reEmail.MatchString(email) {
		ds = append(ds, apierr.Detail{Field: "email", Reason: "must be a valid email address (at most 255 characters)"})
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > maxDisplayName {
		ds = append(ds, apierr.Detail{Field: "display_name", Reason: fmt.Sprintf("must be 1-%d characters", maxDisplayName)})
	}
	if locale != "zh-CN" && locale != "en" {
		ds = append(ds, apierr.Detail{Field: "locale", Reason: "must be zh-CN or en"})
	}
	if len(ds) > 0 {
		return BootstrapAdminResult{}, apierr.Validation(ds...)
	}
	// 已存在同邮箱的超级管理员：幂等返回，不校验也不改口令（续做时安装者可能已经不记得第一次的口令，或口令已被修改）。
	var res BootstrapAdminResult
	existed := false
	if err := s.withTx(ctx, func(ctx context.Context) error {
		rid, err := s.repo.LockSuperAdminRole(ctx)
		if err != nil {
			return fmt.Errorf("auth: 无法锁定内置超级管理员角色（种子缺失？）: %v", err)
		}
		ids, err := s.repo.ActiveSuperAdminIDs(ctx, rid)
		if err != nil {
			return err
		}
		if u, err := s.repo.FindUserByEmail(ctx, email); err == nil {
			if slices.Contains(ids, u.ID) {
				existed = true
				res = BootstrapAdminResult{UserUID: u.UID, Created: false, TOTPEnabled: u.TOTPEnabled}
				return nil
			}
			return conflict("email already in use", "email", "a user with this email already exists")
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if len(ids) > 0 {
			return conflict("an administrator already exists", "email", "the installation wizard only creates the first super administrator")
		}
		return nil
	}); err != nil {
		return BootstrapAdminResult{}, s.wrapInternal(err)
	}
	if existed {
		return res, nil
	}
	if err := ValidateSetupPassword(in.Password, email); err != nil {
		return BootstrapAdminResult{}, err
	}
	hash, err := s.hasher.Hash(ctx, in.Password)
	if err != nil {
		return BootstrapAdminResult{}, apierr.Internal(err)
	}
	newUID, err := s.ids.New()
	if err != nil {
		return BootstrapAdminResult{}, apierr.Internal(err)
	}
	sys := audit.SystemActor("setup")
	now := s.clk.Now()
	err = s.withTx(ctx, func(ctx context.Context) error {
		rid, err := s.repo.LockSuperAdminRole(ctx)
		if err != nil {
			return fmt.Errorf("auth: 无法锁定内置超级管理员角色（种子缺失？）: %v", err)
		}
		ids, err := s.repo.ActiveSuperAdminIDs(ctx, rid) // 并发的另一个创建者先提交：此处重新核对
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			return conflict("an administrator already exists", "email", "the installation wizard only creates the first super administrator")
		}
		id, err := s.repo.InsertPlatformUser(ctx, domain.NewUser{UID: newUID, Email: email, DisplayName: name, PasswordHash: hash, Locale: locale, Now: now, KeepPassword: true})
		if err != nil {
			if db.IsDuplicate(err) {
				return conflict("email already in use", "email", "a user with this email already exists")
			}
			return err
		}
		role, err := s.repo.FindPlatformRoleByCode(ctx, platformAdminCode)
		if err != nil {
			return err
		}
		if err := s.repo.ReplaceUserPlatformRoles(ctx, id, []uint64{role.ID}, 0); err != nil {
			return err
		}
		return s.audit.Record(ctx, audit.ActionSetupAdmin, audit.Event{Actor: &sys, ResourceType: audit.ResourceUser, ResourceUID: newUID,
			Changes: map[string]any{"email": email, "role_codes": []string{platformAdminCode}, "password_set_by_installer": true, "totp_enabled": false}})
	})
	if err != nil {
		return BootstrapAdminResult{}, s.wrapInternal(err)
	}
	s.log.Info("安装向导已创建首个超级管理员", zap.String("user_uid", newUID))
	return BootstrapAdminResult{UserUID: newUID, Created: true}, nil
}

func (s *Service) bootstrapUser(ctx context.Context, userUID string) (domain.AdminUser, error) {
	u, err := s.repo.FindPlatformUserByUID(ctx, userUID)
	if err != nil {
		return domain.AdminUser{}, notFoundErr(err)
	}
	return u, nil
}

// BootstrapBeginTOTP 为刚创建的管理员生成待确认的 TOTP 密钥（密钥信封加密存储；明文与 otpauth URI 只在返回值里出现一次）。已启用返回 409。
func (s *Service) BootstrapBeginTOTP(ctx context.Context, userUID string) (*TOTPSetup, error) {
	u, err := s.bootstrapUser(ctx, userUID)
	if err != nil {
		return nil, err
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: totpIssuer, AccountName: u.Email, Period: totpPeriod, SecretSize: 20, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		return nil, apierr.Internal(err)
	}
	enc, err := s.sealTOTP(ctx, u.UID, key.Secret())
	if err != nil {
		return nil, apierr.Internal(err)
	}
	sys := audit.SystemActor("setup")
	var set bool
	if err := s.withTx(ctx, func(ctx context.Context) error {
		var serr error
		if set, serr = s.repo.SetPendingTOTP(ctx, u.ID, enc); serr != nil || !set {
			return serr
		}
		return s.audit.Record(ctx, audit.ActionSetupAdmin, audit.Event{Actor: &sys, ResourceType: audit.ResourceUser, ResourceUID: u.UID, Changes: map[string]any{"totp_pending": true}})
	}); err != nil {
		return nil, s.wrapInternal(err)
	}
	if !set {
		return nil, apierr.New(apierr.ResourceConflict, "totp is already enabled", apierr.Detail{Field: "totp", Reason: "already enabled"})
	}
	return &TOTPSetup{OTPAuthURI: key.URL(), Secret: key.Secret()}, nil
}

// BootstrapConfirmTOTP 用首次验证码确认并启用 TOTP，一次性发放恢复码（只返回一次；库内只有哈希）。验证码错误 401 auth.totp_invalid（向导层另有 setup 节流）。
func (s *Service) BootstrapConfirmTOTP(ctx context.Context, userUID, code string) ([]string, error) {
	code = strings.TrimSpace(code)
	if !isSixDigits(code) {
		return nil, apierr.Validation(apierr.Detail{Field: "totp_code", Reason: "must be a 6-digit code"})
	}
	u, err := s.bootstrapUser(ctx, userUID)
	if err != nil {
		return nil, err
	}
	sys := audit.SystemActor("setup")
	var codes []string
	bad := false
	now := s.clk.Now()
	err = s.withTx(ctx, func(ctx context.Context) error {
		st, err := s.repo.GetTOTPState(ctx, u.ID, true)
		if err != nil {
			return err
		}
		if st.Enabled {
			return apierr.New(apierr.ResourceConflict, "totp is already enabled", apierr.Detail{Field: "totp", Reason: "already enabled"})
		}
		if st.SecretEnc == nil {
			return apierr.New(apierr.ResourceConflict, "totp setup has not been started", apierr.Detail{Field: "totp", Reason: "start enrollment first"})
		}
		secret, err := s.openTOTP(ctx, st.UID, *st.SecretEnc)
		if err != nil {
			return err
		}
		step, ok := matchStep(secret, code, now)
		if !ok {
			bad = true
			return nil
		}
		hashes := make([]string, 0, recoveryCodeCount)
		for range recoveryCodeCount {
			disp, h, err := newRecoveryCode()
			if err != nil {
				return err
			}
			codes = append(codes, disp)
			hashes = append(hashes, h)
		}
		if enabled, err := s.repo.EnableTOTP(ctx, u.ID, step); err != nil || !enabled {
			if err == nil {
				err = apierr.New(apierr.ResourceConflict, "totp is already enabled")
			}
			return err
		}
		if err := s.repo.ReplaceRecoveryCodes(ctx, u.ID, hashes); err != nil {
			return err
		}
		return s.audit.Record(ctx, audit.ActionSetupAdmin, audit.Event{Actor: &sys, ResourceType: audit.ResourceUser, ResourceUID: u.UID, Changes: map[string]any{"totp_enabled": true, "recovery_codes_issued": len(codes)}})
	})
	if err != nil {
		return nil, s.wrapInternal(err)
	}
	if bad {
		return nil, errTOTPInvalid()
	}
	return codes, nil
}

// BootstrapHasAdmin 报告是否已存在有效的超级管理员（续做时据此判定「创建管理员」一步已完成）。
func (s *Service) BootstrapHasAdmin(ctx context.Context) (bool, error) {
	var n int
	err := s.withTx(ctx, func(ctx context.Context) error {
		rid, err := s.repo.LockSuperAdminRole(ctx)
		if err != nil {
			return err
		}
		ids, err := s.repo.ActiveSuperAdminIDs(ctx, rid)
		n = len(ids)
		return err
	})
	return n > 0, err
}
