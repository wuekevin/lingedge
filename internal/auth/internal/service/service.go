// Package service 是认证模块的业务逻辑（Story 1.16、ADR-018）：登录（argon2id、realm 隔离、会话固定防护、节流）、会话校验（滑动+绝对过期、CSRF）、
// 改密、会话与登录日志查询。
//
// 安全不变量（均有真实 MySQL/Redis 测试，见 tests/integration/auth）：
//   - 口令明文与会话令牌明文只存在于调用栈与 Cookie/请求里：不入库（库里只有哈希）、不入日志、不入 Redis；
//   - 登录对「用户不存在 / 口令错误 / realm 不符 / 账号停用」返回逐字节相同的 401 响应，且都做一次完整的 argon2id 计算（不存在账号用哑哈希），耗时不可区分；
//   - 节流按 identifier（realm+邮箱的哈希）与来源 IP 两个维度计数，不论账号是否存在；Redis 不可用时经 redisx.Limiter 降级为进程内窗口，
//     并叠加 users.failed_login_count/locked_until 的 DB 兜底（ADR-025）。
package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/auth/internal/repository"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// 登录响应状态。
const (
	StatusOK         = "ok"
	StatusMustChange = "must_change_password"
)

const (
	touchInterval    = time.Minute // 滑动续期最小间隔：避免每个请求都写库
	maxPasswordBytes = 1024        // 登录口令长度上限（防止超大输入的无谓计算）
	minNewPassword   = 12
	maxNewPassword   = 128
	lockedLogEvery   = time.Minute // 被锁定期间，同一 identifier 的 locked 日志至多每分钟一条
)

// Options 是 New 的参数。
type Options struct {
	Repo *repository.Repo
	// WithTx 划定事务边界（db.Pool.WithTx）：Service 决定哪些写入同事务，Repository 的方法经 ctx 加入该事务。
	WithTx func(ctx context.Context, fn func(ctx context.Context) error) error
	Redis  *redisx.Client
	Hasher *Hasher
	// Box 是信封加密（pkg/secretbox）：TOTP 密钥经它加密存储（Story 1.17）。
	Box   *secretbox.Box
	Clock clock.Clock
	IDs   *uid.Generator
	Log   *zap.Logger
	// PermCacheTTL 是权限集缓存的 TTL；<=0 取上限 60 秒，超过上限按上限（ADR-025 §1：会话/权限缓存 ≤60 秒）。
	PermCacheTTL time.Duration
	// PermPollEvery 是权限失效广播的轮询兜底周期；<=0 取默认 30 秒（测试可调小以验证兜底）。
	PermPollEvery time.Duration
}

// Service 是认证服务。
type Service struct {
	repo     *repository.Repo
	withTx   func(ctx context.Context, fn func(ctx context.Context) error) error
	redis    *redisx.Client
	limiter  *redisx.Limiter
	ks       redisx.Keyspace
	hasher   *Hasher
	box      *secretbox.Box
	clk      clock.Clock
	ids      *uid.Generator
	log      *zap.Logger
	settings *settingsCache
	dummy    string // 哑哈希：不存在/不合格账号也做一次同参数的 argon2id 计算

	permCache *redisx.Cache
	permTTL   time.Duration
	permPoll  time.Duration

	bgSem chan struct{}
	bgWG  sync.WaitGroup

	// touched 记录各 API Token 上次写 last_used_at 的时间（节流用，Story 1.19）。
	touchMu sync.Mutex
	touched map[uint64]time.Time
}

// New 创建 Service；会计算一次哑哈希（启动时的一次性开销）。
func New(ctx context.Context, o Options) (*Service, error) {
	switch {
	case o.Repo == nil, o.WithTx == nil, o.Redis == nil, o.Hasher == nil, o.IDs == nil, o.Box == nil:
		return nil, errors.New("auth/service: 缺少必需依赖")
	}
	if o.Clock == nil {
		o.Clock = clock.System()
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	dummy, err := o.Hasher.Hash(ctx, "dummy-"+mustRandomHex())
	if err != nil {
		return nil, fmt.Errorf("auth/service: 计算哑哈希: %w", err)
	}
	ttl := o.PermCacheTTL
	if ttl <= 0 || ttl > MaxPermCacheTTL {
		ttl = MaxPermCacheTTL
	}
	poll := o.PermPollEvery
	if poll <= 0 {
		poll = DefaultPermPollEvery
	}
	svc := &Service{
		permCache: redisx.NewCache(o.Redis), permTTL: ttl, permPoll: poll,
		repo: o.Repo, withTx: o.WithTx, redis: o.Redis, limiter: redisx.NewLimiter(o.Redis, nil), ks: o.Redis.Keyspace(), hasher: o.Hasher, box: o.Box,
		clk: o.Clock, ids: o.IDs, log: o.Log.Named("auth"), settings: newSettingsCache(o.Repo, o.Clock, o.Log), dummy: dummy,
		bgSem: make(chan struct{}, 64), touched: map[uint64]time.Time{},
	}
	if err := svc.startPermSubscription(ctx); err != nil {
		return nil, fmt.Errorf("auth/service: 订阅权限失效广播: %w", err)
	}
	return svc, nil
}

func mustRandomHex() string {
	_, h, err := NewSessionToken()
	if err != nil {
		panic(err)
	}
	return h
}

// RequestInfo 是请求侧信息（写登录日志与会话）。
type RequestInfo struct {
	IP        string
	UserAgent string
	RequestID string
}

func (r RequestInfo) ipBytes() []byte {
	ip := net.ParseIP(r.IP)
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip.To16()
}

// ErrUnauthenticated 是统一的 401。
func errUnauthenticated() error {
	return apierr.New(apierr.AuthUnauthenticated, "authentication required")
}

func errInvalidCredentials() error {
	return apierr.New(apierr.AuthInvalidCredentials, "invalid email or password")
}

// NormalizeEmail 小写并去首尾空白（users.email 约定为小写）。
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// ---- 节流键 ----

func hexSum(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:40]
}

func (s *Service) acctKey(realm domain.Realm, email string) (redisx.Key, error) {
	return s.ks.Key("auth", "login_acct", hexSum(string(realm), email))
}

func (s *Service) ipKey(ip string) (redisx.Key, error) {
	if ip == "" {
		ip = "unknown"
	}
	id := strings.NewReplacer(":", "_").Replace(ip)
	return s.ks.Key("auth", "login_ip", id)
}

type throttleState struct {
	acct, ip redisx.Key
	st       domain.Settings
}

func (s *Service) throttleFor(ctx context.Context, realm domain.Realm, email, ip string) (throttleState, error) {
	a, err := s.acctKey(realm, email)
	if err != nil {
		return throttleState{}, err
	}
	i, err := s.ipKey(ip)
	if err != nil {
		return throttleState{}, err
	}
	return throttleState{acct: a, ip: i, st: s.settings.get(ctx)}, nil
}

// blocked 判断（不计数）该 identifier/IP 当前是否已被锁定；u 非 nil 且 Redis 降级时叠加 DB 兜底。
func (s *Service) blocked(ctx context.Context, ts throttleState, u *domain.User) (time.Duration, bool, error) {
	var wait time.Duration
	blocked := false
	for _, c := range []struct {
		k redisx.Key
		n int
	}{{ts.acct, ts.st.ThrottleAccountMax}, {ts.ip, ts.st.ThrottleIPMax}} {
		d, err := s.limiter.Peek(ctx, c.k, c.n, ts.st.ThrottleWindow)
		if err != nil {
			return 0, false, err
		}
		if !d.Allowed {
			blocked = true
			wait = max(wait, d.RetryAfter)
		}
		if d.Degraded && u != nil && u.LockedUntil != nil && c.k == ts.acct {
			if left := u.LockedUntil.Sub(s.clk.Now()); left > 0 {
				blocked = true
				wait = max(wait, left)
			}
		}
	}
	return wait, blocked, nil
}

func rateLimited(wait time.Duration) error {
	if wait < time.Second {
		wait = time.Second
	}
	return apierr.New(apierr.RateLimitExceeded, "too many attempts, try again later").WithRetryAfter(wait)
}

// recordFailure 对 identifier 与 IP 各计一次失败；u 非 nil 时同步 DB 兜底计数。返回本次失败后 identifier 维度是否刚好达到阈值。
func (s *Service) recordFailure(ctx context.Context, ts throttleState, u *domain.User) (reached bool, wait time.Duration, over bool, err error) {
	da, err := s.limiter.Allow(ctx, ts.acct, ts.st.ThrottleAccountMax, ts.st.ThrottleWindow)
	if err != nil {
		return false, 0, false, err
	}
	di, err := s.limiter.Allow(ctx, ts.ip, ts.st.ThrottleIPMax, ts.st.ThrottleWindow)
	if err != nil {
		return false, 0, false, err
	}
	if u != nil {
		// DB 兜底计数在请求路径之外异步写入：同步写会让「存在的账号」比「不存在的账号」多一次事务提交，耗时差是可被统计检测的账号枚举预言机
		// （实测约 1 ms、z≈7，见 tests/integration/auth 的 TestNoAccountEnumeration）。兜底层本就是尽力而为（ADR-025 §5）。
		s.background(func(ctx context.Context) {
			if derr := s.dbFailure(ctx, u.ID, ts.st); derr != nil {
				s.log.Warn("DB 兜底失败计数写入失败", zap.Error(derr)) // 不因兜底层故障而改变登录结果
			}
		})
	}
	over = !da.Allowed || !di.Allowed
	wait = max(da.RetryAfter, di.RetryAfter)
	return da.Allowed && da.Remaining == 0, wait, over, nil
}

// background 在后台（脱离请求 ctx、带超时）执行尽力而为的写入；并发数有上限，超限则丢弃并告警（不阻塞请求）。
func (s *Service) background(fn func(ctx context.Context)) {
	select {
	case s.bgSem <- struct{}{}:
	default:
		s.log.Warn("后台写入并发已满，丢弃一次 DB 兜底失败计数")
		return
	}
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		defer func() { <-s.bgSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		fn(ctx)
	}()
}

// Flush 等待已派发的后台写入完成（停机与测试用）。
func (s *Service) Flush() { s.bgWG.Wait() }

func (s *Service) dbFailure(ctx context.Context, userID uint64, st domain.Settings) error {
	return s.withTx(ctx, func(ctx context.Context) error {
		u, err := s.repo.FindUserByIDForUpdate(ctx, userID)
		if err != nil {
			return err
		}
		now := s.clk.Now()
		count, lu := u.FailedLoginCount+1, u.LockedUntil
		if lu != nil && !now.Before(*lu) { // 上一轮锁定已过期：重新计数
			count, lu = 1, nil
		}
		if int(count) >= st.ThrottleAccountMax {
			t := now.Add(st.ThrottleWindow)
			lu = &t
		}
		return s.repo.SetFailureState(ctx, userID, count, lu)
	})
}

// logLockedOnce 在锁定期间对同一 identifier 至多每分钟写一条 locked 日志（防止攻击者用 429 刷爆 login_logs）。
func (s *Service) logLockedOnce(ctx context.Context, ts throttleState, realm domain.Realm, email string, userID *uint64, ri RequestInfo, reason string) {
	k, err := s.ks.Key("auth", "locked_log", hexSum(string(realm), email))
	if err == nil {
		if d, derr := s.limiter.Allow(ctx, k, 1, lockedLogEvery); derr == nil && !d.Allowed {
			return
		}
	}
	s.writeLog(ctx, realm, email, domain.EventLocked, reason, userID, nil, ri)
}

func (s *Service) writeLog(ctx context.Context, realm domain.Realm, identifier, event, reason string, userID, tenantID *uint64, ri RequestInfo) {
	if err := s.insertLog(ctx, realm, identifier, event, reason, userID, tenantID, ri); err != nil {
		s.log.Error("写登录日志失败", zap.String("event", event), zap.Error(err))
	}
}

func (s *Service) insertLog(ctx context.Context, realm domain.Realm, identifier, event, reason string, userID, tenantID *uint64, ri RequestInfo) error {
	id, err := s.ids.New()
	if err != nil {
		return err
	}
	if len(identifier) > 255 {
		identifier = identifier[:255]
	}
	l := domain.LoginLog{UID: id, CreatedAt: s.clk.Now(), UserID: userID, TenantID: tenantID, Realm: string(realm), Identifier: identifier, Event: event, IP: ri.ipBytes()}
	if ri.UserAgent != "" {
		ua := trunc(ri.UserAgent, 512)
		l.UserAgent = &ua
	}
	if uid.Valid(ri.RequestID) {
		rid := ri.RequestID
		l.RequestID = &rid
	}
	if reason != "" {
		l.Reason = &reason
	}
	return s.repo.InsertLoginLog(ctx, l)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ---- 登录 ----

// LoginInput 是登录请求。
type LoginInput struct {
	Realm    domain.Realm
	Email    string
	Password string
	// ExistingToken 是请求携带的该 realm 旧会话 Cookie（若有）：登录成功后被吊销（会话固定防护）。
	ExistingToken string
	Req           RequestInfo
}

// LoginResult 是登录成功的结果。
type LoginResult struct {
	Status    string
	Realm     domain.Realm
	User      domain.User
	Tenant    *domain.Tenant
	Token     string // Cookie 值（明文，仅此一次）
	MFAToken  string // 仅 Status=totp_required：一次性 mfa_token（明文，仅此一次）
	CSRF      string
	ExpiresAt time.Time
	Absolute  time.Time
	CookieTTL time.Duration // Cookie 的 Max-Age：取绝对过期时长（服务端另按滑动过期判定）
}

// Login 校验凭据并建立会话。
func (s *Service) Login(ctx context.Context, in LoginInput) (*LoginResult, error) {
	if !in.Realm.Valid() {
		return nil, apierr.Validation(apierr.Detail{Field: "realm", Reason: "must be admin or console"})
	}
	email := NormalizeEmail(in.Email)
	if email == "" || len(email) > 255 || !strings.Contains(email, "@") {
		return nil, apierr.Validation(apierr.Detail{Field: "email", Reason: "must be a valid email address"})
	}
	if in.Password == "" || len(in.Password) > maxPasswordBytes {
		return nil, apierr.Validation(apierr.Detail{Field: "password", Reason: "must be 1-1024 bytes"})
	}
	ts, err := s.throttleFor(ctx, in.Realm, email, in.Req.IP)
	if err != nil {
		return nil, apierr.Internal(err)
	}

	user, err := s.repo.FindUserByEmail(ctx, email)
	var u *domain.User
	switch {
	case err == nil:
		u = &user
	case errors.Is(err, domain.ErrNotFound):
	default:
		return nil, apierr.Internal(err)
	}

	// 1) 已锁定：不再校验口令（不给暴力破解任何额外信息），统一 429。
	if wait, blocked, err := s.blocked(ctx, ts, u); err != nil {
		return nil, apierr.Internal(err)
	} else if blocked {
		var uidp *uint64
		if u != nil {
			uidp = &u.ID
		}
		s.logLockedOnce(ctx, ts, in.Realm, email, uidp, in.Req, "throttled")
		return nil, rateLimited(wait)
	}

	// 2) 校验口令：任何情形都做一次完整的 argon2id 计算（不存在/不合格账号用哑哈希），保证耗时与响应不可区分。
	eligible := u != nil && u.Status == domain.StatusActive && u.UserType == in.Realm.UserType()
	phc, reason := s.dummy, ""
	switch {
	case u == nil:
		reason = "unknown_user"
	case u.Status != domain.StatusActive:
		reason = "disabled"
	case u.UserType != in.Realm.UserType():
		reason = "realm_mismatch"
	default:
		phc = u.PasswordHash
	}
	ok, needsRehash, verr := s.hasher.Verify(ctx, in.Password, phc)
	if verr != nil {
		if errors.Is(verr, ErrBadHash) { // 库里的哈希损坏：按口令错误处理（绝不放行），并告警
			s.log.Error("用户口令哈希格式无效，按口令错误处理")
			ok = false
		} else {
			return nil, apierr.Internal(verr)
		}
	}
	if ok && !eligible {
		ok = false // 理论上哑哈希不可能匹配；防御性处理
	}
	if ok && reason == "" {
		// 控制台 realm 须有有效的租户成员关系。
		var tenant *domain.Tenant
		if in.Realm == domain.RealmConsole {
			t, err := s.repo.DefaultMembership(ctx, u.ID)
			switch {
			case err == nil:
				tenant = &t
			case errors.Is(err, domain.ErrNotFound):
				ok, reason = false, "no_membership"
			default:
				return nil, apierr.Internal(err)
			}
		}
		if ok {
			return s.finishLogin(ctx, in, ts, u, tenant, needsRehash)
		}
	}
	if reason == "" {
		reason = "bad_password"
	}
	return nil, s.failLogin(ctx, ts, in, email, u, reason)
}

func (s *Service) failLogin(ctx context.Context, ts throttleState, in LoginInput, email string, u *domain.User, reason string) error {
	var uidp *uint64
	if u != nil {
		uidp = &u.ID
	}
	reached, wait, over, err := s.recordFailure(ctx, ts, u)
	if err != nil {
		return apierr.Internal(err)
	}
	s.writeLog(ctx, in.Realm, email, domain.EventLoginFailed, reason, uidp, nil, in.Req)
	if over { // 并发请求越过了阈值：本次也按超限处理
		s.logLockedOnce(ctx, ts, in.Realm, email, uidp, in.Req, "throttled")
		return rateLimited(wait)
	}
	if reached {
		s.writeLog(ctx, in.Realm, email, domain.EventLocked, "threshold_reached", uidp, nil, in.Req)
	}
	return errInvalidCredentials()
}

func (s *Service) finishLogin(ctx context.Context, in LoginInput, ts throttleState, u *domain.User, tenant *domain.Tenant, needsRehash bool) (*LoginResult, error) {
	rehash := ""
	if needsRehash {
		h, err := s.hasher.Hash(ctx, in.Password)
		if err != nil {
			s.log.Warn("口令哈希升级失败（本次登录不受影响）", zap.Error(err))
		} else {
			rehash = h
		}
	}
	if u.TOTPEnabled {
		// 第二步（POST /auth/totp/verify）：口令正确但尚未建立会话，发放 5 分钟一次性的 mfa_token（Story 1.17）。
		// 不清零失败计数、不写 login_success：登录尚未完成，验证码的猜测仍受同一套节流约束。
		if rehash != "" {
			if err := s.repo.UpdatePasswordHash(ctx, u.ID, rehash); err != nil {
				s.log.Warn("口令哈希升级写入失败（本次登录不受影响）", zap.Error(err))
			}
		}
		tok, err := s.issueMFAChallenge(ctx, in.Realm, u, in.Req)
		if err != nil {
			return nil, apierr.Internal(err)
		}
		return &LoginResult{Status: StatusTOTPRequired, Realm: in.Realm, MFAToken: tok}, nil
	}
	return s.issueSession(ctx, sessionParams{Realm: in.Realm, User: u, Tenant: tenant, TS: ts, Req: in.Req, ExistingToken: in.ExistingToken, Rehash: rehash})
}

// sessionParams 是 issueSession 的输入：登录一步完成（无 TOTP）或第二步通过之后都经它建立会话。
type sessionParams struct {
	Realm         domain.Realm
	User          *domain.User
	Tenant        *domain.Tenant
	TS            throttleState
	Req           RequestInfo
	ExistingToken string
	Rehash        string
	// MFAVerifiedAt 非空表示本会话建立时已通过第二因子；LogReason 写入 login_success 的 reason（如 totp / recovery_code）。
	MFAVerifiedAt *time.Time
	LogReason     string
}

func (s *Service) issueSession(ctx context.Context, sp sessionParams) (*LoginResult, error) {
	u, tenant, ts, in := sp.User, sp.Tenant, sp.TS, sp.Req
	token, tokenHash, err := NewSessionToken()
	if err != nil {
		return nil, apierr.Internal(err)
	}
	_, raw, _ := HashSessionToken(token)
	csrf, csrfHash := DeriveCSRF(raw)
	sid, err := s.ids.New()
	if err != nil {
		return nil, apierr.Internal(err)
	}
	now := s.clk.Now()
	st := ts.st
	abs := now.Add(st.SessionAbsoluteTTL)
	exp := minTime(now.Add(st.SessionIdleTTL), abs)
	ns := domain.NewSession{UID: sid, TokenHash: tokenHash, CSRFHash: csrfHash, UserID: u.ID, Realm: sp.Realm, IP: in.ipBytes(),
		UserAgent: trunc(in.UserAgent, 512), Now: now, ExpiresAt: exp, AbsoluteExpiresAt: abs, MFAVerifiedAt: sp.MFAVerifiedAt}
	var tenantID *uint64
	if tenant != nil {
		ns.TenantID = &tenant.ID
		tenantID = &tenant.ID
	}
	err = s.withTx(ctx, func(ctx context.Context) error {
		if sp.ExistingToken != "" { // 会话固定防护：登录成功后旧会话作废，只认新建的会话
			if h, _, herr := HashSessionToken(sp.ExistingToken); herr == nil {
				if old, ferr := s.repo.FindSessionByTokenHash(ctx, h); ferr == nil {
					if _, rerr := s.repo.RevokeSession(ctx, old.ID, domain.RevokeRelogin, now); rerr != nil {
						return rerr
					}
				}
			}
		}
		if err := s.repo.CreateSession(ctx, ns); err != nil {
			return err
		}
		if err := s.repo.RecordLoginSuccess(ctx, u.ID, ns.IP, now, sp.Rehash); err != nil {
			return err
		}
		return s.insertLog(ctx, sp.Realm, u.Email, domain.EventLoginSuccess, sp.LogReason, &u.ID, tenantID, in)
	})
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if err := s.limiter.Reset(ctx, ts.acct); err != nil {
		s.log.Warn("清零登录失败计数失败", zap.Error(err))
	}
	res := &LoginResult{Status: StatusOK, Realm: sp.Realm, User: *u, Tenant: tenant, Token: token, CSRF: csrf, ExpiresAt: exp, Absolute: abs, CookieTTL: st.SessionAbsoluteTTL}
	if u.MustChange {
		res.Status = StatusMustChange
	}
	return res, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// ---- 会话校验 ----

// Authenticate 校验某 realm 的 Cookie 令牌，返回主体；任何失败一律 401（不区分原因）。
func (s *Service) Authenticate(ctx context.Context, realm domain.Realm, token string) (*domain.Principal, error) {
	hash, raw, err := HashSessionToken(token)
	if err != nil {
		return nil, errUnauthenticated()
	}
	v, err := s.repo.FindSessionByTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, errUnauthenticated()
		}
		return nil, apierr.Internal(err)
	}
	now := s.clk.Now()
	if v.Realm != string(realm) || v.RevokedAt != nil || !now.Before(v.ExpiresAt) || !now.Before(v.AbsoluteExpiresAt) ||
		v.UserDeletedAt != nil || v.UserStatus != domain.StatusActive || v.UserType != realm.UserType() {
		return nil, errUnauthenticated()
	}
	csrf, csrfHash := DeriveCSRF(raw)
	if csrfHash != v.CSRFHash {
		return nil, errUnauthenticated() // 完整性：库内哈希与令牌不匹配（被篡改）
	}
	p := &domain.Principal{Realm: realm, CSRFToken: csrf}
	if realm == domain.RealmConsole {
		if v.TenantID == nil || !v.MembershipOK || v.TenantUID == nil || v.TenantDeleted != nil || v.TenantStatus == nil || *v.TenantStatus != "active" {
			return nil, errUnauthenticated()
		}
		p.Tenant = &domain.Tenant{ID: *v.TenantID, UID: *v.TenantUID, Name: deref(v.TenantName), Slug: deref(v.TenantSlug), IsDefault: v.TenantDefault != nil && *v.TenantDefault}
	}
	p.Session = v
	p.User = domain.User{ID: v.UserID, UID: v.UserUID, UserType: v.UserType, Email: v.Email, DisplayName: v.DisplayName, Status: v.UserStatus,
		Locale: v.Locale, TOTPEnabled: v.TOTPEnabled, MustChange: v.MustChange}
	if now.Sub(v.LastSeenAt) >= touchInterval {
		st := s.settings.get(ctx)
		exp := minTime(now.Add(st.SessionIdleTTL), v.AbsoluteExpiresAt)
		if terr := s.repo.TouchSession(ctx, v.ID, now, exp); terr != nil {
			s.log.Warn("会话续期失败", zap.Error(terr))
		} else {
			p.Session.ExpiresAt = exp
		}
	}
	return p, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- 登出 / 会话管理 ----

// Logout 吊销当前会话。
func (s *Service) Logout(ctx context.Context, p *domain.Principal, ri RequestInfo) error {
	if p.Bearer || p.Session.ID == 0 {
		return errUnauthenticated()
	}
	now := s.clk.Now()
	return s.wrapInternal(s.withTx(ctx, func(ctx context.Context) error {
		if _, err := s.repo.RevokeSession(ctx, p.Session.ID, domain.RevokeLogout, now); err != nil {
			return err
		}
		return s.insertLog(ctx, p.Realm, p.User.Email, domain.EventLogout, "logout", &p.User.ID, tenantIDOf(p), ri)
	}))
}

func tenantIDOf(p *domain.Principal) *uint64 {
	if p.Tenant == nil {
		return nil
	}
	id := p.Tenant.ID
	return &id
}

func (s *Service) wrapInternal(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := apierr.As(err); ok {
		return err
	}
	return apierr.Internal(err)
}

// SessionItem 是对外的会话条目。
type SessionItem struct {
	Ref        string
	Realm      string
	IP         string
	UserAgent  *string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	Current    bool
}

// ListSessions 列出当前用户自己的有效会话。
func (s *Service) ListSessions(ctx context.Context, p *domain.Principal, page, size int) ([]SessionItem, int64, error) {
	now := s.clk.Now()
	total, err := s.repo.CountActiveSessions(ctx, p.User.ID, now)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	rows, err := s.repo.ListActiveSessions(ctx, p.User.ID, now, size, (page-1)*size)
	if err != nil {
		return nil, 0, apierr.Internal(err)
	}
	out := make([]SessionItem, 0, len(rows))
	for _, r := range rows {
		it := SessionItem{Realm: r.Realm, UserAgent: r.UserAgent, CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt, ExpiresAt: r.ExpiresAt,
			Current: r.ID == p.Session.ID, IP: ipString(r.IP)}
		if r.UID != nil {
			it.Ref = *r.UID
		}
		out = append(out, it)
	}
	return out, total, nil
}

func ipString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return net.IP(b).String()
}

// IPString 把存储的 IP 字节转为文本（空则返回空串）。
func IPString(b []byte) string { return ipString(b) }

// RevokeSession 吊销当前用户自己的某个会话；current 表示被吊销的是当前会话。他人/不存在/已失效的会话统一 404。
func (s *Service) RevokeSession(ctx context.Context, p *domain.Principal, sessionUID string, ri RequestInfo) (current bool, err error) {
	if !uid.Valid(sessionUID) {
		return false, apierr.Validation(apierr.Detail{Field: "sessionId", Reason: "must be a valid uid"})
	}
	now := s.clk.Now()
	err = s.withTx(ctx, func(ctx context.Context) error {
		id, err := s.repo.RevokeUserSessionByUID(ctx, p.User.ID, sessionUID, domain.RevokeLogout, now)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return apierr.NotFound()
			}
			return err
		}
		current = id == p.Session.ID
		return s.insertLog(ctx, p.Realm, p.User.Email, domain.EventLogout, "session_revoked", &p.User.ID, tenantIDOf(p), ri)
	})
	return current, s.wrapInternal(err)
}

// ---- 登录日志 ----

// LogItem 是对外的登录日志条目。
type LogItem struct {
	domain.LoginLog
	IPText string
}

// ListLoginLogs 返回当前用户自己的登录事件（id 倒序游标）；next 为空表示到末尾。
func (s *Service) ListLoginLogs(ctx context.Context, p *domain.Principal, cursor string, limit int) (items []LogItem, next string, err error) {
	var before uint64
	if cursor != "" {
		b, derr := base64.RawURLEncoding.DecodeString(cursor)
		n, perr := strconv.ParseUint(strings.TrimPrefix(string(b), "id:"), 10, 64)
		if derr != nil || perr != nil || !strings.HasPrefix(string(b), "id:") || n == 0 {
			return nil, "", apierr.Validation(apierr.Detail{Field: "cursor", Reason: "invalid cursor"})
		}
		before = n
	}
	rows, err := s.repo.ListUserLoginLogs(ctx, p.User.ID, before, limit+1)
	if err != nil {
		return nil, "", apierr.Internal(err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
		next = base64.RawURLEncoding.EncodeToString([]byte("id:" + strconv.FormatUint(rows[len(rows)-1].ID, 10)))
	}
	items = make([]LogItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, LogItem{LoginLog: r, IPText: ipString(r.IP)})
	}
	return items, next, nil
}

// ---- 当前身份 ----

// MeResult 是 /auth/me 所需的数据。
type MeResult struct {
	Access  domain.Access
	RunMode string
}

// Me 读取当前主体的角色/权限集与运行模式（只读；授权判定属 Story 1.18）。
func (s *Service) Me(ctx context.Context, p *domain.Principal) (MeResult, error) {
	var tid uint64
	if p.Tenant != nil {
		tid = p.Tenant.ID
	}
	acc, err := s.repo.UserAccess(ctx, p.User.ID, tid)
	if err != nil {
		return MeResult{}, apierr.Internal(err)
	}
	return MeResult{Access: acc, RunMode: s.settings.get(ctx).RunMode}, nil
}

// ---- 改密 ----

// ChangePassword 校验当前口令并设置新口令，同一事务内使该用户的全部会话失效（含当前会话）。
func (s *Service) ChangePassword(ctx context.Context, p *domain.Principal, current, newPassword string, ri RequestInfo) error {
	if p.Bearer || p.Session.ID == 0 {
		return errUnauthenticated()
	}
	if current == "" || len(current) > maxPasswordBytes {
		return apierr.Validation(apierr.Detail{Field: "current_password", Reason: "must be 1-1024 bytes"})
	}
	if newPassword == "" {
		return apierr.Validation(apierr.Detail{Field: "new_password", Reason: "is required"})
	}
	if n := utf8.RuneCountInString(newPassword); n < minNewPassword || n > maxNewPassword || len(newPassword) > 4*maxNewPassword {
		return apierr.New(apierr.AuthPasswordPolicy, "password does not meet the policy",
			apierr.Detail{Field: "new_password", Reason: fmt.Sprintf("must be %d-%d characters", minNewPassword, maxNewPassword)})
	}
	if newPassword == current {
		return apierr.New(apierr.AuthPasswordPolicy, "password does not meet the policy", apierr.Detail{Field: "new_password", Reason: "must differ from the current password"})
	}
	if strings.EqualFold(newPassword, p.User.Email) {
		return apierr.New(apierr.AuthPasswordPolicy, "password does not meet the policy", apierr.Detail{Field: "new_password", Reason: "must not be the email address"})
	}
	ts, err := s.throttleFor(ctx, p.Realm, p.User.Email, ri.IP)
	if err != nil {
		return apierr.Internal(err)
	}
	u, err := s.repo.FindUserByID(ctx, p.User.ID)
	if err != nil {
		return errUnauthenticated()
	}
	if wait, blocked, berr := s.blocked(ctx, ts, &u); berr != nil {
		return apierr.Internal(berr)
	} else if blocked {
		s.logLockedOnce(ctx, ts, p.Realm, p.User.Email, &u.ID, ri, "throttled")
		return rateLimited(wait)
	}
	ok, _, verr := s.hasher.Verify(ctx, current, u.PasswordHash)
	if verr != nil && !errors.Is(verr, ErrBadHash) {
		return apierr.Internal(verr)
	}
	if !ok {
		reached, wait, over, rerr := s.recordFailure(ctx, ts, &u)
		if rerr != nil {
			return apierr.Internal(rerr)
		}
		s.writeLog(ctx, p.Realm, p.User.Email, domain.EventLoginFailed, "password_change_bad_current", &u.ID, tenantIDOf(p), ri)
		if over {
			return rateLimited(wait)
		}
		if reached {
			s.writeLog(ctx, p.Realm, p.User.Email, domain.EventLocked, "threshold_reached", &u.ID, tenantIDOf(p), ri)
		}
		return errInvalidCredentials()
	}
	hash, err := s.hasher.Hash(ctx, newPassword)
	if err != nil {
		return apierr.Internal(err)
	}
	now := s.clk.Now()
	err = s.withTx(ctx, func(ctx context.Context) error {
		if err := s.repo.UpdatePassword(ctx, u.ID, hash, now); err != nil {
			return err
		}
		if _, err := s.repo.RevokeUserSessions(ctx, u.ID, domain.RevokeCredentialChange, now); err != nil {
			return err
		}
		return s.insertLog(ctx, p.Realm, p.User.Email, domain.EventLogout, "password_changed", &u.ID, tenantIDOf(p), ri)
	})
	if err != nil {
		return s.wrapInternal(err)
	}
	if rerr := s.limiter.Reset(ctx, ts.acct); rerr != nil {
		s.log.Warn("清零登录失败计数失败", zap.Error(rerr))
	}
	return nil
}
