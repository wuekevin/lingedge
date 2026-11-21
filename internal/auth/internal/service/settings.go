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
	"github.com/wuekevin/lingedge/internal/platform/sysconfig"
)

// 键与默认值的**唯一来源**是 internal/platform/sysconfig 的键注册表（Story 1.22）：这里只是别名，认证模块读取时的取值范围也取自注册表
// （bounds/countBounds），因此经 PATCH /admin/settings 写入的合法值一定会被这里接受，不会出现「写得进、读时被当作无效回退默认」。
// 默认值是工程假设 [ASSUMPTION]（UD-27：会话滑动/绝对过期、登录节流窗口与阈值「待基线测试后确定」；UD-29：再认证窗口 300 秒），
// 只在 system_settings 没有对应键时生效；改值只需写 system_settings（15 秒内生效，无需重启）。
const (
	DefaultSessionIdleTTL     = sysconfig.DefaultSessionIdleTTL
	DefaultSessionAbsoluteTTL = sysconfig.DefaultSessionAbsoluteTTL
	DefaultThrottleWindow     = sysconfig.DefaultThrottleWindow
	DefaultThrottleAccountMax = sysconfig.DefaultThrottleAccountMax
	DefaultThrottleIPMax      = sysconfig.DefaultThrottleIPMax
	DefaultRunMode            = sysconfig.DefaultRunMode
	DefaultReauthWindow       = sysconfig.DefaultReauthWindow
	DefaultMFAWindow          = sysconfig.DefaultMFAWindow
	DefaultAPITokenTTL        = sysconfig.DefaultAPITokenTTL
	DefaultAPITokenMaxTTL     = sysconfig.DefaultAPITokenMaxTTL

	settingsCacheTTL = 15 * time.Second
)

// 设置键（system_settings.setting_key；值为 JSON 数字/字符串）。
const (
	KeySessionIdleTTL     = sysconfig.KeySessionIdleTTL
	KeySessionAbsoluteTTL = sysconfig.KeySessionAbsoluteTTL
	KeyThrottleWindow     = sysconfig.KeyThrottleWindow
	KeyThrottleAccountMax = sysconfig.KeyThrottleAccountMax
	KeyThrottleIPMax      = sysconfig.KeyThrottleIPMax
	KeyRunMode            = sysconfig.KeyRunMode
	KeyReauthWindow       = sysconfig.KeyReauthWindow
	KeyMFAWindow          = sysconfig.KeyMFAWindow
	KeyTOTPRequiredAdmin  = sysconfig.KeyTOTPRequiredAdmin
	KeyAPITokenDefaultTTL = sysconfig.KeyAPITokenDefaultTTL
	KeyAPITokenMaxTTL     = sysconfig.KeyAPITokenMaxTTL
)

// bounds 返回注册表里某个「秒」类型键的取值范围。
func bounds(key string) (lo, hi time.Duration) {
	d, ok := sysconfig.Lookup(key)
	if !ok {
		panic("auth: 设置键未在 sysconfig 注册表登记: " + key)
	}
	return time.Duration(d.Min) * time.Second, time.Duration(d.Max) * time.Second
}

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
	next.SessionIdleTTL = secs(c.log, raw, KeySessionIdleTTL, next.SessionIdleTTL)
	next.SessionAbsoluteTTL = secs(c.log, raw, KeySessionAbsoluteTTL, next.SessionAbsoluteTTL)
	next.ThrottleWindow = secs(c.log, raw, KeyThrottleWindow, next.ThrottleWindow)
	next.ThrottleAccountMax = count(c.log, raw, KeyThrottleAccountMax, next.ThrottleAccountMax)
	next.ThrottleIPMax = count(c.log, raw, KeyThrottleIPMax, next.ThrottleIPMax)
	if v, ok := raw[KeyRunMode]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil && (s == "self_hosted" || s == "commercial") {
			next.RunMode = s
		}
	}
	next.ReauthWindow = secs(c.log, raw, KeyReauthWindow, next.ReauthWindow)
	next.MFAWindow = secs(c.log, raw, KeyMFAWindow, next.MFAWindow)
	if v, ok := raw[KeyTOTPRequiredAdmin]; ok {
		var b bool
		if json.Unmarshal(v, &b) == nil {
			next.TOTPRequiredForAdmin = b
		} else {
			c.log.Warn("系统设置取值无效，使用默认值", zap.String("key", KeyTOTPRequiredAdmin))
		}
	}
	next.APITokenMaxTTL = secs(c.log, raw, KeyAPITokenMaxTTL, next.APITokenMaxTTL)
	next.APITokenDefaultTTL = secs(c.log, raw, KeyAPITokenDefaultTTL, next.APITokenDefaultTTL)
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

func secs(log *zap.Logger, raw map[string]json.RawMessage, key string, def time.Duration) time.Duration {
	lo, hi := bounds(key)
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
