package redisx

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// 键与写入的拒绝原因。
var (
	ErrBadKey         = errors.New("redisx: 键名不合规（须为 le:<env>:<模块>:<用途>:<标识>，不使用花括号）")
	ErrNoTTL          = errors.New("redisx: 写入必须带正的 TTL（ADR-025 §4：所有键必须设置 TTL）")
	ErrSensitiveValue = errors.New("redisx: 值含明文敏感信息，拒绝写入 Redis（令牌只存哈希或不存）")
	ErrNoScope        = errors.New("redisx: 租户键需要租户范围的 Scope 与合法的 tenant_uid")
)

const (
	envPat     = `[a-z][a-z0-9-]{0,31}`
	segPat     = `[a-z][a-z0-9_]{0,31}`
	idPat      = `[A-Za-z0-9_.=-]{1,128}`
	maxKeyLen  = 256
	keyPrefix  = "le"
	tenantPref = "t"
)

// keyRe 是全系统唯一的键名正则：花括号、空段、空白、大写模块名一律不匹配。
var keyRe = regexp.MustCompile(`^` + keyPrefix + `:(` + envPat + `):(` + segPat + `):(` + segPat + `)(?::` + idPat + `)+$`)

var (
	envRe = regexp.MustCompile(`^` + envPat + `$`)
	segRe = regexp.MustCompile(`^` + segPat + `$`)
	idRe  = regexp.MustCompile(`^` + idPat + `$`)
	// tenantSegRe 匹配租户段（t + 26 位 ULID）。非租户键的标识段不得长成租户段，租户键只能经 TenantKey（Scope）构造。
	tenantSegRe = regexp.MustCompile(`^t[0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{26}$`)
)

// Key 是通过校验的键。字段未导出：包外只能经 Keyspace 构造，零值 Key 不可用于任何命令。
type Key struct{ s string }

// String 返回键名（不含值，可安全写入日志）。
func (k Key) String() string { return k.s }

// Valid 报告是否为合规键。
func (k Key) Valid() bool { return len(k.s) <= maxKeyLen && keyRe.MatchString(k.s) }

// Keyspace 构造某一环境下的键。
type Keyspace struct{ env string }

// NewKeyspace 校验环境名（与 config 的 server.env 同口径）。
func NewKeyspace(env string) (Keyspace, error) {
	if !envRe.MatchString(env) {
		return Keyspace{}, fmt.Errorf("redisx: 环境名 %q 不合法（小写字母开头，仅含小写字母/数字/连字符，最长 32 位）", env)
	}
	return Keyspace{env: env}, nil
}

// Env 返回环境名。
func (ks Keyspace) Env() string { return ks.env }

func (ks Keyspace) build(module, purpose string, tenant string, ids []string) (Key, error) {
	if ks.env == "" {
		return Key{}, fmt.Errorf("%w: 未初始化的 Keyspace", ErrBadKey)
	}
	if !segRe.MatchString(module) || !segRe.MatchString(purpose) {
		return Key{}, fmt.Errorf("%w: 模块/用途段 %q/%q 须匹配 %s", ErrBadKey, module, purpose, segPat)
	}
	parts := []string{keyPrefix, ks.env, module, purpose}
	if tenant != "" {
		parts = append(parts, tenant)
	}
	if len(ids) == 0 && tenant == "" {
		return Key{}, fmt.Errorf("%w: 缺少标识段", ErrBadKey)
	}
	for _, id := range ids {
		if !idRe.MatchString(id) {
			return Key{}, fmt.Errorf("%w: 标识段不合法（仅 A-Za-z0-9_.=-，1–128 位，不含冒号与花括号）", ErrBadKey)
		}
		parts = append(parts, id)
	}
	k := Key{s: strings.Join(parts, ":")}
	if !k.Valid() { // 双保险：拼出的结果必须通过唯一的正则
		return Key{}, fmt.Errorf("%w: %q", ErrBadKey, k.s)
	}
	return k, nil
}

// Key 构造非租户键 le:<env>:<module>:<purpose>:<id>[:<id>…]。
func (ks Keyspace) Key(module, purpose string, ids ...string) (Key, error) {
	for _, id := range ids {
		if tenantSegRe.MatchString(id) {
			return Key{}, fmt.Errorf("%w: 标识段形如租户段 t<tenant_uid>，租户键必须经 TenantKey（Scope）构造", ErrBadKey)
		}
	}
	return ks.build(module, purpose, "", ids)
}

// TenantKey 构造租户键 le:<env>:<module>:<purpose>:t<tenant_uid>[:<id>…]。
// 必须持有租户范围的 Scope（零值与平台范围被拒绝），tenantUID 须是该租户的 ULID（Scope 只带内部数值 id，
// 二者的一致性由调用方的 Scope 来源保证——Scope 本身只能经 tenantscope 白名单入口构造，ADR-009）。
func (ks Keyspace) TenantKey(sc scope.Scope, tenantUID, module, purpose string, ids ...string) (Key, error) {
	if sc.IsZero() || sc.IsPlatform() || sc.TenantID() == 0 {
		return Key{}, ErrNoScope
	}
	if !uid.Valid(tenantUID) {
		return Key{}, fmt.Errorf("%w: tenant_uid 不是合法 ULID", ErrNoScope)
	}
	return ks.build(module, purpose, tenantPref+tenantUID, ids)
}

// Parse 校验一个外部给出的键名字符串：必须匹配键名正则且环境段等于本 Keyspace 的环境
// （花括号旧写法 le:{env}:… 在此被拒绝）。
func (ks Keyspace) Parse(raw string) (Key, error) {
	k := Key{s: raw}
	m := keyRe.FindStringSubmatch(raw)
	if m == nil || !k.Valid() || m[1] != ks.env {
		return Key{}, fmt.Errorf("%w: %q", ErrBadKey, raw)
	}
	return k, nil
}
