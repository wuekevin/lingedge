package service

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/auth/internal/domain"
	"github.com/wuekevin/lingedge/internal/auth/internal/repository"
	"github.com/wuekevin/lingedge/internal/platform/clock"
)

// 默认值是工程假设 [ASSUMPTION]（UD-27：会话滑动/绝对过期、登录节流窗口与阈值「待基线测试后确定」，均为可配置项，不虚构基线）。
// 它们只在 system_settings 没有对应键时生效；改值只需写 system_settings（15 秒内生效，无需重启）。
const (
	DefaultSessionIdleTTL     = 2 * time.Hour
	DefaultSessionAbsoluteTTL = 12 * time.Hour
	DefaultThrottleWindow     = 15 * time.Minute
	DefaultThrottleAccountMax = 10
	DefaultThrottleIPMax      = 30
	DefaultRunMode            = "self_hosted"
	// DefaultReauthWindow 是 L4 再认证窗口：UD-29 采纳值 300 秒（可配置）。
	DefaultReauthWindow = 300 * time.Second
	// DefaultMFAWindow 是「近期 MFA」窗口：基线与 UD 均未规定 [ASSUMPTION]，工程假设 15 分钟；可配置，改默认值只是配置改动。
	DefaultMFAWindow = 15 * time.Minute
	// API Token（Story 1.19）：基线与 UD 均未规定有效期 [ASSUMPTION]；工程假设 默认 90 天（创建时不给 expires_at）、系统上限 365 天——
	// 令牌不应默认永不过期。均为 system_settings 可配置项（UD-27 同口径），改默认值只是配置改动。
	DefaultAPITokenTTL    = 90 * 24 * time.Hour
	DefaultAPITokenMaxTTL = 365 * 24 * time.Hour

	settingsCacheTTL = 15 * time.Second
)

// 设置键（system_settings.setting_key；值为 JSON 数字/字符串）。
const (
	KeySessionIdleTTL     = "auth.session_idle_ttl_s"
	KeySessionAbsoluteTTL = "auth.session_absolute_ttl_s"
	KeyThrottleWindow     = "login.throttle_window_s"
	KeyThrottleAccountMax = "login.throttle_account_max"
	KeyThrottleIPMax      = "login.throttle_ip_max"
	KeyRunMode            = "system.run_mode"
	KeyReauthWindow       = "auth.reauth_window_s"
	KeyMFAWindow          = "auth.mfa_window_s"
	KeyTOTPRequiredAdmin  = "login.totp_required_for_admin"
	KeyAPITokenDefaultTTL = "auth.api_token_default_ttl_s"
	KeyAPITokenMaxTTL     = "auth.api_token_max_ttl_s"
)

type settingsCache struct {
	repo *repository.Repo
	clk  clock.Clock
	log  *zap.Logger

	mu      sync.Mutex
	val     domain.Settings
	expires time.Time
}

func newSettingsCache(repo *repository.Repo, clk clock.Clock, log *zap.Logger) *settingsCache {
	return &settingsCache{repo: repo, clk: clk, log: log}
}

func defaultSettings() domain.Settings {
	return domain.Settings{
		SessionIdleTTL: DefaultSessionIdleTTL, SessionAbsoluteTTL: DefaultSessionAbsoluteTTL,
		ThrottleWindow: DefaultThrottleWindow, ThrottleAccountMax: DefaultThrottleAccountMax, ThrottleIPMax: DefaultThrottleIPMax,
		RunMode: DefaultRunMode, ReauthWindow: DefaultReauthWindow, MFAWindow: DefaultMFAWindow,
		APITokenDefaultTTL: DefaultAPITokenTTL, APITokenMaxTTL: DefaultAPITokenMaxTTL,
	}
}

// get 返回当前设置；读取失败时沿用上一次（或默认）值并告警，不让设置读取故障阻断登录。
func (c *settingsCache) get(ctx context.Context) domain.Settings {
	now := c.clk.Now()
	c.mu.Lock()
	if now.Before(c.expires) {
		v := c.val
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()

	next := defaultSettings()
	raw, err := c.repo.ReadSettings(ctx)
	if err != nil {
		c.log.Warn("读取认证系统设置失败：沿用上一次/默认值", zap.Error(err))
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.val == (domain.Settings{}) {
			c.val = next
		}
		c.expires = now.Add(2 * time.Second) // 故障期间不要每个请求都打库
		return c.val
	}
	next.SessionIdleTTL = secs(c.log, raw, KeySessionIdleTTL, next.SessionIdleTTL, time.Minute, 30*24*time.Hour)
	next.SessionAbsoluteTTL = secs(c.log, raw, KeySessionAbsoluteTTL, next.SessionAbsoluteTTL, 5*time.Minute, 90*24*time.Hour)
	next.ThrottleWindow = secs(c.log, raw, KeyThrottleWindow, next.ThrottleWindow, time.Second, 24*time.Hour)
	next.ThrottleAccountMax = count(c.log, raw, KeyThrottleAccountMax, next.ThrottleAccountMax)
	next.ThrottleIPMax = count(c.log, raw, KeyThrottleIPMax, next.ThrottleIPMax)
	if v, ok := raw[KeyRunMode]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil && (s == "self_hosted" || s == "commercial") {
			next.RunMode = s
		}
	}
	next.ReauthWindow = secs(c.log, raw, KeyReauthWindow, next.ReauthWindow, 10*time.Second, 24*time.Hour)
	next.MFAWindow = secs(c.log, raw, KeyMFAWindow, next.MFAWindow, 30*time.Second, 24*time.Hour)
	if v, ok := raw[KeyTOTPRequiredAdmin]; ok {
		var b bool
		if json.Unmarshal(v, &b) == nil {
			next.TOTPRequiredForAdmin = b
		} else {
			c.log.Warn("系统设置取值无效，使用默认值", zap.String("key", KeyTOTPRequiredAdmin))
		}
	}
	next.APITokenMaxTTL = secs(c.log, raw, KeyAPITokenMaxTTL, next.APITokenMaxTTL, time.Hour, 3650*24*time.Hour)
	next.APITokenDefaultTTL = secs(c.log, raw, KeyAPITokenDefaultTTL, next.APITokenDefaultTTL, time.Hour, 3650*24*time.Hour)
	if next.APITokenDefaultTTL > next.APITokenMaxTTL { // 默认有效期不得超过系统上限
		next.APITokenDefaultTTL = next.APITokenMaxTTL
	}
	if next.SessionIdleTTL > next.SessionAbsoluteTTL { // 滑动过期不得长于绝对过期
		next.SessionIdleTTL = next.SessionAbsoluteTTL
	}
	c.mu.Lock()
	c.val, c.expires = next, now.Add(settingsCacheTTL)
	c.mu.Unlock()
	return next
}

func number(raw map[string]json.RawMessage, key string) (int64, bool) {
	v, ok := raw[key]
	if !ok {
		return 0, false
	}
	var n json.Number
	if json.Unmarshal(v, &n) != nil {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	return i, err == nil
}

func secs(log *zap.Logger, raw map[string]json.RawMessage, key string, def, lo, hi time.Duration) time.Duration {
	n, ok := number(raw, key)
	if !ok {
		if _, present := raw[key]; present {
			log.Warn("系统设置取值无效，使用默认值", zap.String("key", key))
		}
		return def
	}
	d := time.Duration(n) * time.Second
	if d < lo || d > hi {
		log.Warn("系统设置超出允许范围，使用默认值", zap.String("key", key))
		return def
	}
	return d
}

func count(log *zap.Logger, raw map[string]json.RawMessage, key string, def int) int {
	n, ok := number(raw, key)
	if !ok {
		if _, present := raw[key]; present {
			log.Warn("系统设置取值无效，使用默认值", zap.String("key", key))
		}
		return def
	}
	if n < 1 || n > 1_000_000 {
		log.Warn("系统设置超出允许范围，使用默认值", zap.String("key", key))
		return def
	}
	return int(n)
}
