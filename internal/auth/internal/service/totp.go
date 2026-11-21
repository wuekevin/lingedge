package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"github.com/pquerna/otp/totp"
	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// TOTP / 恢复码 / 近期 MFA / L4 再认证（Story 1.17、ADR-018 §5/§12）。
//
// 设计要点：
//   - 密钥：pquerna/otp v1.5.0 生成 20 字节随机密钥（SHA-1/6 位/30 秒，所有主流验证器通用）；Base32 文本经 pkg/secretbox 信封加密存 users.totp_secret_enc，
//     AAD=users|totp_secret_enc|<用户 uid>（密文挪到别的行即解密失败）；明文只在内存与 setup 响应里出现一次。
//   - 重放：验证码匹配到 time-step 后，用 `UPDATE users SET totp_last_step=? WHERE … AND (totp_last_step IS NULL OR totp_last_step < ?)` 原子记录；
//     受影响行数为 0 即该窗口（或更早的窗口）已用过 → 拒绝。持久化在 MySQL，多实例一致，Redis 不参与。允许 ±1 个 time-step 漂移，±2 拒绝。
//   - 恢复码：10 个、各 80 位 CSPRNG 随机（Base32，16 字符，分 4 组展示），库内只存 SHA-256（带域前缀）。为什么不用 argon2：恢复码本身是高熵随机数
//     （2^80），离线穷举不可行，慢哈希只会给登录路径增加延迟而不增加安全性；需要确定性哈希才能按 (user_id, code_hash) 唯一索引原子地「用掉」一个码
//     （加盐的慢哈希会迫使逐行比对）。单次有效：UPDATE … SET used_at WHERE used_at IS NULL 原子完成。
//   - 失败一律计入 1.16 的节流（账号+IP 双维度）并写 login_logs（totp_failed / login_failed）；日志与审计不含口令、密钥、恢复码、验证码。
const (
	totpPeriod        = 30
	totpDigits        = 6
	totpIssuer        = "LingEdge"
	mfaChallengeTTL   = 5 * time.Minute
	mfaMaxAttempts    = 5
	recoveryCodeCount = 10
	recoveryCodeBytes = 10 // 80 位
)

// StatusTOTPRequired 是登录第一步通过、须提交第二因子时的 LoginResponse.status。
const StatusTOTPRequired = "totp_required"

var recoveryEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

func (s *Service) sealTOTP(ctx context.Context, userUID, secret string) (string, error) {
	return s.box.Seal(ctx, []byte(secret), secretbox.RecordContext{Table: "users", Column: "totp_secret_enc", UID: userUID})
}

func (s *Service) openTOTP(ctx context.Context, userUID, enc string) (string, error) {
	b, err := s.box.Open(ctx, enc, secretbox.RecordContext{Table: "users", Column: "totp_secret_enc", UID: userUID})
	if err != nil {
		return "", err
	}
	defer secretbox.Zero(b)
	return string(b), nil
}

func isSixDigits(s string) bool {
	if len(s) != totpDigits {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// matchStep 在 ±1 个 time-step 内寻找与验证码匹配的步；多个匹配取最大（理论上不会发生）。
func matchStep(secret, code string, now time.Time) (uint64, bool) {
	cur := now.Unix() / totpPeriod
	best, found := int64(-1), false
	for off := int64(-1); off <= 1; off++ {
		c := cur + off
		if c < 0 {
			continue
		}
		ok, err := hotp.ValidateCustom(code, uint64(c), secret, hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err == nil && ok && c > best {
			best, found = c, true
		}
	}
	return uint64(best), found
}

func newRecoveryCode() (display, hash string, err error) {
	b := make([]byte, recoveryCodeBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	raw := recoveryEnc.EncodeToString(b) // 16 字符
	display = raw[0:4] + "-" + raw[4:8] + "-" + raw[8:12] + "-" + raw[12:16]
	return display, hashRecovery(raw), nil
}

func hashRecovery(normalized string) string {
	sum := sha256.Sum256([]byte("lingedge/recovery/v1\x00" + normalized))
	return hex.EncodeToString(sum[:])
}

// normalizeRecovery 把用户输入的恢复码规整为 16 位大写 Base32；格式不符返回 false。
func normalizeRecovery(in string) (string, bool) {
	r := strings.NewReplacer("-", "", " ", "")
	n := strings.ToUpper(r.Replace(strings.TrimSpace(in)))
	if len(n) != recoveryCodeBytes*8/5 {
		return "", false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
			return "", false
		}
	}
	return n, true
}

// 第二因子校验方式（login_logs.reason 与结构化日志用）。
const (
	methodTOTP     = "totp"
	methodRecovery = "recovery_code"
)

// checkSecondFactor 校验验证码：6 位数字按 TOTP（含重放防护），否则（allowRecovery 时）按恢复码（单次有效）。
// 成功时已「用掉」该 time-step / 恢复码；失败时 reason 是 login_logs 的原因（bad_code / replayed / bad_recovery_code）。
func (s *Service) checkSecondFactor(ctx context.Context, userID uint64, code string, allowRecovery bool) (method, reason string, ok bool, err error) {
	st, err := s.repo.GetTOTPState(ctx, userID, false)
	if err != nil {
		return "", "", false, err
	}
	if !st.Enabled || st.SecretEnc == nil {
		return "", "bad_code", false, nil
	}
	code = strings.TrimSpace(code)
	if isSixDigits(code) {
		secret, err := s.openTOTP(ctx, st.UID, *st.SecretEnc)
		if err != nil {
			return "", "", false, err
		}
		step, matched := matchStep(secret, code, s.clk.Now())
		if !matched {
			return methodTOTP, "bad_code", false, nil
		}
		advanced, err := s.repo.AdvanceTOTPStep(ctx, userID, step)
		if err != nil {
			return "", "", false, err
		}
		if !advanced {
			return methodTOTP, "replayed", false, nil
		}
		return methodTOTP, "", true, nil
	}
	if allowRecovery {
		if n, valid := normalizeRecovery(code); valid {
			used, err := s.repo.UseRecoveryCode(ctx, userID, hashRecovery(n), s.clk.Now())
			if err != nil {
				return "", "", false, err
			}
			if used {
				return methodRecovery, "", true, nil
			}
			return methodRecovery, "bad_recovery_code", false, nil
		}
	}
	return "", "bad_code", false, nil
}

// failAttempt 统一处理「失败的凭据尝试」：计入节流（账号+IP）、写 login_logs、按是否超限返回 429 或 fail。
func (s *Service) failAttempt(ctx context.Context, ts throttleState, realm domain.Realm, email string, u *domain.User, tenantID *uint64, ri RequestInfo, event, reason string, fail error) error {
	var uidp *uint64
	if u != nil {
		uidp = &u.ID
	}
	reached, wait, over, err := s.recordFailure(ctx, ts, u)
	if err != nil {
		return apierr.Internal(err)
	}
	s.writeLog(ctx, realm, email, event, reason, uidp, tenantID, ri)
	if over {
		s.logLockedOnce(ctx, ts, realm, email, uidp, ri, "throttled")
		return rateLimited(wait)
	}
	if reached {
		s.writeLog(ctx, realm, email, domain.EventLocked, "threshold_reached", uidp, tenantID, ri)
	}
	return fail
}

func errTOTPInvalid() error { return apierr.New(apierr.AuthTotpInvalid, "invalid verification code") }

func errCookieSessionRequired() error {
	return apierr.New(apierr.AuthForbidden, "a cookie session is required", apierr.Detail{Field: "session", Reason: "API tokens cannot perform this operation"})
}

func requireSession(p *domain.Principal) error {
	if p.Bearer || p.Session.ID == 0 {
		return errCookieSessionRequired()
	}
	return nil
}

// ---- 登录第二步 ----

// issueMFAChallenge 发放一次性 mfa_token（32 字节 CSPRNG，库存 SHA-256，5 分钟）。
func (s *Service) issueMFAChallenge(ctx context.Context, realm domain.Realm, u *domain.User, ri RequestInfo) (string, error) {
	token, hash, err := NewSessionToken()
	if err != nil {
		return "", err
	}
	now := s.clk.Now()
	if err := s.repo.CreateMFAChallenge(ctx, hash, u.ID, string(realm), ri.ipBytes(), now, now.Add(mfaChallengeTTL)); err != nil {
		return "", err
	}
	return token, nil
}

// VerifyInput 是登录第二步的输入。
type VerifyInput struct {
	MFAToken string
	Code     string
	// ExistingTokens 是请求携带的各 realm 旧会话 Cookie：挑战所属 realm 的那一个在登录成功后被吊销（会话固定防护）。
	ExistingTokens []ExistingToken
	Req            RequestInfo
}

// ExistingToken 是请求里某个 realm 的会话 Cookie 值。
type ExistingToken struct {
	Realm domain.Realm
	Token string
}

// VerifyLogin 校验 mfa_token 与验证码（TOTP 或恢复码），成功后建立会话（mfa_verified_at=now）。
func (s *Service) VerifyLogin(ctx context.Context, in VerifyInput) (*LoginResult, error) {
	code := strings.TrimSpace(in.Code)
	if len(code) < 6 || len(code) > 32 {
		return nil, apierr.Validation(apierr.Detail{Field: "code", Reason: "must be 6-32 characters"})
	}
	hash, _, err := HashSessionToken(in.MFAToken)
	if err != nil {
		return nil, apierr.Validation(apierr.Detail{Field: "mfa_token", Reason: "invalid token"})
	}
	invalid := func() error {
		return apierr.New(apierr.AuthUnauthenticated, "mfa challenge is invalid or expired", apierr.Detail{Field: "mfa_token", Reason: "start the login again"})
	}
	ch, err := s.repo.FindMFAChallenge(ctx, hash, false)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, invalid()
		}
		return nil, apierr.Internal(err)
	}
	now := s.clk.Now()
	if ch.ConsumedAt != nil || !now.Before(ch.ExpiresAt) || ch.Attempts >= mfaMaxAttempts {
		return nil, invalid()
	}
	realm := domain.Realm(ch.Realm)
	u, err := s.repo.FindUserByID(ctx, ch.UserID)
	if err != nil {
		return nil, invalid()
	}
	if u.Status != domain.StatusActive || u.UserType != realm.UserType() || !u.TOTPEnabled {
		return nil, invalid()
	}
	ts, err := s.throttleFor(ctx, realm, u.Email, in.Req.IP)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if wait, blocked, berr := s.blocked(ctx, ts, &u); berr != nil {
		return nil, apierr.Internal(berr)
	} else if blocked {
		s.logLockedOnce(ctx, ts, realm, u.Email, &u.ID, in.Req, "throttled")
		return nil, rateLimited(wait)
	}
	method, reason, ok, err := s.checkSecondFactor(ctx, u.ID, code, true)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if !ok {
		if berr := s.repo.BumpMFAAttempts(ctx, ch.ID); berr != nil {
			s.log.Warn("记录 mfa 挑战失败次数出错", zap.Error(berr))
		}
		return nil, s.failAttempt(ctx, ts, realm, u.Email, &u, nil, in.Req, domain.EventTOTPFailed, reason, errTOTPInvalid())
	}
	consumed, err := s.repo.ConsumeMFAChallenge(ctx, ch.ID, now)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if !consumed {
		return nil, invalid()
	}
	var tenant *domain.Tenant
	if realm == domain.RealmConsole {
		t, terr := s.repo.DefaultMembership(ctx, u.ID)
		if terr != nil {
			if errors.Is(terr, domain.ErrNotFound) {
				return nil, invalid()
			}
			return nil, apierr.Internal(terr)
		}
		tenant = &t
	}
	existing := ""
	for _, e := range in.ExistingTokens {
		if e.Realm == realm {
			existing = e.Token
		}
	}
	mfa := now
	return s.issueSession(ctx, sessionParams{Realm: realm, User: &u, Tenant: tenant, TS: ts, Req: in.Req, ExistingToken: existing, MFAVerifiedAt: &mfa, LogReason: method})
}

// ---- 启用 / 停用 ----

// TOTPSetup 是 setup 的结果：密钥只在此响应里出现一次。
type TOTPSetup struct {
	OTPAuthURI string
	Secret     string
}

// SetupTOTP 生成新的 TOTP 密钥并以「待确认」状态加密存储（需 enable 确认后才生效）。已启用返回 409。
func (s *Service) SetupTOTP(ctx context.Context, p *domain.Principal) (*TOTPSetup, error) {
	if err := requireSession(p); err != nil {
		return nil, err
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: totpIssuer, AccountName: p.User.Email, Period: totpPeriod, SecretSize: 20, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		return nil, apierr.Internal(err)
	}
	enc, err := s.sealTOTP(ctx, p.User.UID, key.Secret())
	if err != nil {
		return nil, apierr.Internal(err)
	}
	var set bool
	if err := s.withTx(ctx, func(ctx context.Context) error {
		var serr error
		if set, serr = s.repo.SetPendingTOTP(ctx, p.User.ID, enc); serr != nil || !set {
			return serr
		}
		// 审计与密钥落库同一事务；变更摘要不含密钥/otpauth URI。
		return s.audit.Record(ctx, audit.ActionTOTPSetup, audit.Event{ResourceType: audit.ResourceUser, ResourceUID: p.User.UID, Changes: map[string]any{"pending": true}})
	}); err != nil {
		return nil, s.wrapInternal(err)
	}
	if !set {
		return nil, apierr.New(apierr.ResourceConflict, "totp is already enabled", apierr.Detail{Field: "totp", Reason: "disable it first"})
	}
	return &TOTPSetup{OTPAuthURI: key.URL(), Secret: key.Secret()}, nil
}

// EnableTOTP 用首次验证码确认并启用 TOTP，一次性发放恢复码（明文只在此返回；库内只有哈希）。
func (s *Service) EnableTOTP(ctx context.Context, p *domain.Principal, code string, ri RequestInfo) ([]string, error) {
	if err := requireSession(p); err != nil {
		return nil, err
	}
	code = strings.TrimSpace(code)
	if !isSixDigits(code) {
		return nil, apierr.Validation(apierr.Detail{Field: "code", Reason: "must be a 6-digit code"})
	}
	ts, err := s.throttleFor(ctx, p.Realm, p.User.Email, ri.IP)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	u, err := s.repo.FindUserByID(ctx, p.User.ID)
	if err != nil {
		return nil, errUnauthenticated()
	}
	if wait, blocked, berr := s.blocked(ctx, ts, &u); berr != nil {
		return nil, apierr.Internal(berr)
	} else if blocked {
		s.logLockedOnce(ctx, ts, p.Realm, p.User.Email, &u.ID, ri, "throttled")
		return nil, rateLimited(wait)
	}
	var codes []string
	var failReason string
	now := s.clk.Now()
	err = s.withTx(ctx, func(ctx context.Context) error {
		st, err := s.repo.GetTOTPState(ctx, p.User.ID, true)
		if err != nil {
			return err
		}
		if st.Enabled {
			return apierr.New(apierr.ResourceConflict, "totp is already enabled", apierr.Detail{Field: "totp", Reason: "disable it first"})
		}
		if st.SecretEnc == nil {
			return apierr.New(apierr.ResourceConflict, "totp setup has not been started", apierr.Detail{Field: "totp", Reason: "call setup first"})
		}
		secret, err := s.openTOTP(ctx, st.UID, *st.SecretEnc)
		if err != nil {
			return err
		}
		step, ok := matchStep(secret, code, now)
		if !ok {
			failReason = "bad_code"
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
		if enabled, err := s.repo.EnableTOTP(ctx, p.User.ID, step); err != nil || !enabled {
			if err == nil {
				err = apierr.New(apierr.ResourceConflict, "totp is already enabled")
			}
			return err
		}
		if err := s.repo.ReplaceRecoveryCodes(ctx, p.User.ID, hashes); err != nil {
			return err
		}
		if err := s.repo.SetSessionMFA(ctx, p.Session.ID, now); err != nil {
			return err
		}
		// 审计与启用同一事务；只记数量，不含密钥/验证码/恢复码。
		return s.audit.Record(ctx, audit.ActionTOTPEnable, audit.Event{ResourceType: audit.ResourceUser, ResourceUID: p.User.UID, Changes: map[string]any{"recovery_codes_issued": len(codes)}})
	})
	if err != nil {
		return nil, s.wrapInternal(err)
	}
	if failReason != "" {
		return nil, s.failAttempt(ctx, ts, p.Realm, p.User.Email, &u, tenantIDOf(p), ri, domain.EventTOTPFailed, "enable_"+failReason, errTOTPInvalid())
	}
	s.log.Info("TOTP 已启用", zap.String("user_uid", p.User.UID), zap.String("request_id", ri.RequestID), zap.Int("recovery_codes", len(codes)))
	return codes, nil
}

// DisableTOTP 需要口令 + 当前验证码（或恢复码）。「强制管理员启用 TOTP」开启时平台用户不可停用。
func (s *Service) DisableTOTP(ctx context.Context, p *domain.Principal, password, code string, ri RequestInfo) error {
	if err := requireSession(p); err != nil {
		return err
	}
	if password == "" || len(password) > maxPasswordBytes {
		return apierr.Validation(apierr.Detail{Field: "password", Reason: "must be 1-1024 bytes"})
	}
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 32 {
		return apierr.Validation(apierr.Detail{Field: "code", Reason: "is required"})
	}
	if st := s.settings.get(ctx); st.TOTPRequiredForAdmin && p.IsPlatform() {
		return apierr.New(apierr.AuthForbidden, "totp is required for administrators", apierr.Detail{Field: "totp_required_for_admin", Reason: "disabling is not allowed while the policy is on"})
	}
	ts, err := s.throttleFor(ctx, p.Realm, p.User.Email, ri.IP)
	if err != nil {
		return apierr.Internal(err)
	}
	u, err := s.repo.FindUserByID(ctx, p.User.ID)
	if err != nil {
		return errUnauthenticated()
	}
	if !u.TOTPEnabled {
		return apierr.New(apierr.ResourceConflict, "totp is not enabled")
	}
	if wait, blocked, berr := s.blocked(ctx, ts, &u); berr != nil {
		return apierr.Internal(berr)
	} else if blocked {
		s.logLockedOnce(ctx, ts, p.Realm, p.User.Email, &u.ID, ri, "throttled")
		return rateLimited(wait)
	}
	ok, _, verr := s.hasher.Verify(ctx, password, u.PasswordHash)
	if verr != nil && !errors.Is(verr, ErrBadHash) {
		return apierr.Internal(verr)
	}
	if !ok {
		return s.failAttempt(ctx, ts, p.Realm, p.User.Email, &u, tenantIDOf(p), ri, domain.EventLoginFailed, "totp_disable_bad_password", errInvalidCredentials())
	}
	_, reason, ok, err := s.checkSecondFactor(ctx, u.ID, code, true)
	if err != nil {
		return apierr.Internal(err)
	}
	if !ok {
		return s.failAttempt(ctx, ts, p.Realm, p.User.Email, &u, tenantIDOf(p), ri, domain.EventTOTPFailed, "disable_"+reason, errTOTPInvalid())
	}
	err = s.withTx(ctx, func(ctx context.Context) error {
		if err := s.repo.DisableTOTP(ctx, u.ID); err != nil {
			return err
		}
		if err := s.repo.DeleteRecoveryCodes(ctx, u.ID); err != nil {
			return err
		}
		return s.audit.Record(ctx, audit.ActionTOTPDisable, audit.Event{ResourceType: audit.ResourceUser, ResourceUID: p.User.UID})
	})
	if err != nil {
		return s.wrapInternal(err)
	}
	if rerr := s.limiter.Reset(ctx, ts.acct); rerr != nil {
		s.log.Warn("清零登录失败计数失败", zap.Error(rerr))
	}
	s.log.Info("TOTP 已停用", zap.String("user_uid", p.User.UID), zap.String("request_id", ri.RequestID))
	return nil
}

// ---- 再认证（L4） ----

// ReauthResult 是 reauth 的结果。
type ReauthResult struct {
	ReauthAt  time.Time
	ExpiresAt time.Time
}

// Reauth 以口令（已启用 TOTP 时另需 6 位验证码）刷新当前会话的 reauth_at（并在含第二因子时刷新 mfa_verified_at）。
// 只有 Cookie 会话能再认证：Bearer API Token 不能满足。
func (s *Service) Reauth(ctx context.Context, p *domain.Principal, password, totpCode string, ri RequestInfo) (*ReauthResult, error) {
	if err := requireSession(p); err != nil {
		return nil, err
	}
	if password == "" || len(password) > maxPasswordBytes {
		return nil, apierr.Validation(apierr.Detail{Field: "password", Reason: "must be 1-1024 bytes"})
	}
	totpCode = strings.TrimSpace(totpCode)
	if totpCode != "" && !isSixDigits(totpCode) {
		return nil, apierr.Validation(apierr.Detail{Field: "totp_code", Reason: "must be a 6-digit code"})
	}
	ts, err := s.throttleFor(ctx, p.Realm, p.User.Email, ri.IP)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	u, err := s.repo.FindUserByID(ctx, p.User.ID)
	if err != nil {
		return nil, errUnauthenticated()
	}
	if u.TOTPEnabled && totpCode == "" { // 先提示需要验证码（不计失败：这是正常的两步交互）
		return nil, apierr.New(apierr.AuthTotpRequired, "second factor required", apierr.Detail{Field: "totp_code", Reason: "required when TOTP is enabled"})
	}
	if wait, blocked, berr := s.blocked(ctx, ts, &u); berr != nil {
		return nil, apierr.Internal(berr)
	} else if blocked {
		s.logLockedOnce(ctx, ts, p.Realm, p.User.Email, &u.ID, ri, "throttled")
		return nil, rateLimited(wait)
	}
	ok, _, verr := s.hasher.Verify(ctx, password, u.PasswordHash)
	if verr != nil && !errors.Is(verr, ErrBadHash) {
		return nil, apierr.Internal(verr)
	}
	if !ok {
		return nil, s.failAttempt(ctx, ts, p.Realm, p.User.Email, &u, tenantIDOf(p), ri, domain.EventLoginFailed, "reauth_bad_password", errInvalidCredentials())
	}
	now := s.clk.Now()
	var mfa *time.Time
	if u.TOTPEnabled {
		_, reason, good, cerr := s.checkSecondFactor(ctx, u.ID, totpCode, false)
		if cerr != nil {
			return nil, apierr.Internal(cerr)
		}
		if !good {
			return nil, s.failAttempt(ctx, ts, p.Realm, p.User.Email, &u, tenantIDOf(p), ri, domain.EventTOTPFailed, "reauth_"+reason, errTOTPInvalid())
		}
		mfa = &now
	}
	if err := s.withTx(ctx, func(ctx context.Context) error {
		if err := s.repo.SetSessionReauth(ctx, p.Session.ID, now, mfa); err != nil {
			return err
		}
		return s.audit.Record(ctx, audit.ActionAuthReauth, audit.Event{ResourceType: audit.ResourceSession, ResourceUID: p.Session.UID, Changes: map[string]any{"second_factor": mfa != nil}})
	}); err != nil {
		return nil, s.wrapInternal(err)
	}
	if rerr := s.limiter.Reset(ctx, ts.acct); rerr != nil {
		s.log.Warn("清零登录失败计数失败", zap.Error(rerr))
	}
	s.log.Info("会话已再认证", zap.String("user_uid", p.User.UID), zap.String("request_id", ri.RequestID))
	return &ReauthResult{ReauthAt: now, ExpiresAt: now.Add(s.settings.get(ctx).ReauthWindow)}, nil
}

// ---- 策略检查（由认证/授权钩子统一调用）----

// EnrollmentRequired 报告该主体是否被「强制管理员启用 TOTP」策略限制：开启时，未启用 TOTP 的平台用户只能访问启用流程。
func (s *Service) EnrollmentRequired(ctx context.Context, p *domain.Principal) bool {
	return p.IsPlatform() && !p.User.TOTPEnabled && s.settings.get(ctx).TOTPRequiredForAdmin
}

// CheckRecentMFA 对敏感操作（改密码、创建 API Token…）要求近期 MFA：已启用 TOTP 的账号，其会话的 mfa_verified_at 须在窗口内；
// 未启用 TOTP 的账号无第二因子可验证（由强制策略引导其启用）。Bearer 请求没有会话，无法满足（fail-closed）。
func (s *Service) CheckRecentMFA(ctx context.Context, p *domain.Principal) error {
	if !p.User.TOTPEnabled {
		return nil
	}
	if p.Bearer || p.Session.ID == 0 || p.Session.MFAVerifiedAt == nil || s.clk.Now().Sub(*p.Session.MFAVerifiedAt) > s.settings.get(ctx).MFAWindow {
		return apierr.New(apierr.AuthReauthRequired, "recent MFA required", apierr.Detail{Field: "mfa_verified_at", Reason: "verify again with POST /auth/reauth (password and totp_code)"})
	}
	return nil
}

// CheckReauth 是 L4 再认证检查：会话的 reauth_at 须在 auth.reauth_window_s 内（now - reauth_at ≤ 窗口），否则 403 auth.reauth_required。
// Bearer API Token 请求没有会话，永远不能满足（必须是 Cookie 会话）。
func (s *Service) CheckReauth(ctx context.Context, p *domain.Principal) error {
	denied := apierr.New(apierr.AuthReauthRequired, "reauthentication required", apierr.Detail{Field: "reauth_at", Reason: "call POST /auth/reauth first"})
	if p.Bearer || p.Session.ID == 0 || p.Session.ReauthAt == nil {
		return denied
	}
	if s.clk.Now().Sub(*p.Session.ReauthAt) > s.settings.get(ctx).ReauthWindow {
		return denied
	}
	return nil
}
