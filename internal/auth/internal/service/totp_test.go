package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/clock"
)

// testService 构造一个只含时钟与设置的 Service（CheckReauth / CheckRecentMFA / requireSession 这类纯策略检查不触及数据库）。
func testService(now time.Time, st domain.Settings) *Service {
	clk := clock.Func(func() time.Time { return now })
	c := &settingsCache{clk: clk, val: st, expires: now.Add(24 * time.Hour)}
	return &Service{clk: clk, settings: c}
}

func code(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func errCode(err error) apierr.Code {
	if e, ok := apierr.As(err); ok {
		return e.Code
	}
	return ""
}

// ±1 个 time-step 通过，±2 拒绝；返回匹配的 step。
func TestMatchStepSkew(t *testing.T) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "x", AccountName: "a@b.test", Period: 30, SecretSize: 20, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_015, 0)
	cur := uint64(now.Unix() / 30)
	for off, want := range map[int]bool{-3: false, -2: false, -1: true, 0: true, 1: true, 2: false, 3: false} {
		step, ok := matchStep(key.Secret(), code(t, key.Secret(), now.Add(time.Duration(off)*30*time.Second)), now)
		if ok != want {
			t.Errorf("漂移 %+d：期望通过=%v，实际 %v", off, want, ok)
		}
		if ok && step != uint64(int64(cur)+int64(off)) {
			t.Errorf("漂移 %+d：匹配的 step 应为 %d，实际 %d", off, int64(cur)+int64(off), step)
		}
	}
	if _, ok := matchStep(key.Secret(), "12345", now); ok {
		t.Error("长度不对的验证码不得通过")
	}
}

func TestSixDigitsAndRecoveryFormat(t *testing.T) {
	for in, want := range map[string]bool{"123456": true, "12345": false, "1234567": false, "12345a": false, "": false, "１２３４５６": false} {
		if isSixDigits(in) != want {
			t.Errorf("isSixDigits(%q) 应为 %v", in, want)
		}
	}
	seen := map[string]bool{}
	for range 200 {
		disp, hash, err := newRecoveryCode()
		if err != nil {
			t.Fatal(err)
		}
		n, ok := normalizeRecovery(disp)
		if !ok || hashRecovery(n) != hash || len(hash) != 64 || seen[hash] {
			t.Fatalf("恢复码 %q：规整/哈希/唯一性", disp)
		}
		seen[hash] = true
		if strings.Contains(hash, n) {
			t.Fatal("哈希不得含明文")
		}
		if n2, ok := normalizeRecovery(" " + strings.ToLower(disp) + " "); !ok || n2 != n {
			t.Errorf("大小写/空白容错: %q", disp)
		}
	}
	for _, bad := range []string{"", "AAAA-BBBB", "AAAA-BBBB-CCCC-DDD1", "AAAA-BBBB-CCCC-DDDD-EEEE", "0000-1111-8888-9999"} {
		if _, ok := normalizeRecovery(bad); ok {
			t.Errorf("%q 不应被当作恢复码", bad)
		}
	}
}

func principal(bearer bool, sessionID uint64, totpOn bool, mfa, reauth *time.Time) *domain.Principal {
	return &domain.Principal{Bearer: bearer, User: domain.User{TOTPEnabled: totpOn}, Session: domain.SessionView{ID: sessionID, MFAVerifiedAt: mfa, ReauthAt: reauth}}
}

// Bearer API Token 请求没有会话，永远不能满足再认证与近期 MFA（即便时间戳字段「看起来」新鲜）。
func TestBearerCannotSatisfyReauthOrRecentMFA(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	s := testService(now, domain.Settings{ReauthWindow: 300 * time.Second, MFAWindow: 900 * time.Second})
	fresh := now
	ctx := context.Background()

	bearer := principal(true, 0, true, &fresh, &fresh)
	if err := s.CheckReauth(ctx, bearer); errCode(err) != apierr.AuthReauthRequired {
		t.Errorf("Bearer 不能满足再认证: %v", err)
	}
	if err := s.CheckRecentMFA(ctx, bearer); errCode(err) != apierr.AuthReauthRequired {
		t.Errorf("Bearer 不能满足近期 MFA: %v", err)
	}
	// 即便被标成 Bearer 的主体带有会话 id，也不能满足。
	if err := s.CheckReauth(ctx, principal(true, 7, false, &fresh, &fresh)); errCode(err) != apierr.AuthReauthRequired {
		t.Errorf("Bearer（带会话 id）不能满足再认证: %v", err)
	}
	for name, fn := range map[string]func() error{
		"setup":   func() error { _, e := s.SetupTOTP(ctx, bearer); return e },
		"enable":  func() error { _, e := s.EnableTOTP(ctx, bearer, "123456", RequestInfo{}); return e },
		"disable": func() error { return s.DisableTOTP(ctx, bearer, "x", "123456", RequestInfo{}) },
		"reauth":  func() error { _, e := s.Reauth(ctx, bearer, "x", "", RequestInfo{}); return e },
	} {
		if err := fn(); errCode(err) != apierr.AuthForbidden {
			t.Errorf("Bearer 不能执行 %s: %v", name, err)
		}
	}
	// Cookie 会话：新鲜则通过。
	if err := s.CheckReauth(ctx, principal(false, 1, false, nil, &fresh)); err != nil {
		t.Errorf("窗口内应通过: %v", err)
	}
	if err := s.CheckRecentMFA(ctx, principal(false, 1, true, &fresh, nil)); err != nil {
		t.Errorf("近期 MFA 应通过: %v", err)
	}
}

// 窗口边界：now - reauth_at ≤ 窗口 通过；多 1 毫秒拒绝；从未再认证拒绝；未启用 TOTP 的账号不要求近期 MFA。
func TestWindowBoundaries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	st := domain.Settings{ReauthWindow: 300 * time.Second, MFAWindow: 900 * time.Second}
	ctx := context.Background()
	at := func(ago time.Duration) *time.Time { v := now.Add(-ago); return &v }
	s := testService(now, st)
	for ago, want := range map[time.Duration]bool{0: true, 299 * time.Second: true, 300 * time.Second: true, 300*time.Second + time.Millisecond: false, time.Hour: false} {
		if got := s.CheckReauth(ctx, principal(false, 1, false, nil, at(ago))) == nil; got != want {
			t.Errorf("再认证 %v 前：期望通过=%v", ago, want)
		}
	}
	if s.CheckReauth(ctx, principal(false, 1, false, nil, nil)) == nil {
		t.Error("从未再认证应拒绝")
	}
	for ago, want := range map[time.Duration]bool{900 * time.Second: true, 900*time.Second + time.Millisecond: false} {
		if got := s.CheckRecentMFA(ctx, principal(false, 1, true, at(ago), nil)) == nil; got != want {
			t.Errorf("近期 MFA %v 前：期望通过=%v", ago, want)
		}
	}
	if s.CheckRecentMFA(ctx, principal(false, 1, true, nil, nil)) == nil {
		t.Error("启用了 TOTP 但会话没有 mfa_verified_at 应拒绝")
	}
	if s.CheckRecentMFA(ctx, principal(false, 1, false, nil, nil)) != nil {
		t.Error("未启用 TOTP 的账号不要求近期 MFA")
	}
}

func TestEnrollmentRequiredPolicy(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	ctx := context.Background()
	platform := &domain.Principal{User: domain.User{UserType: domain.UserTypePlatform}}
	tenant := &domain.Principal{User: domain.User{UserType: domain.UserTypeTenant}}
	on, off := testService(now, domain.Settings{TOTPRequiredForAdmin: true}), testService(now, domain.Settings{})
	if !on.EnrollmentRequired(ctx, platform) || off.EnrollmentRequired(ctx, platform) || on.EnrollmentRequired(ctx, tenant) {
		t.Error("仅「策略开启 ∧ 平台用户 ∧ 未启用 TOTP」受限")
	}
	platform.User.TOTPEnabled = true
	if on.EnrollmentRequired(ctx, platform) {
		t.Error("已启用 TOTP 的管理员不受限")
	}
}
