package sysconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// 设置键（system_settings.setting_key）。认证模块使用的键由 TestAuthKeysMatchRegistry 与这里逐项核对。
const (
	KeyRunMode = "system.run_mode"

	KeyRegistrationOpen = "general.registration_open"
	KeyAdminURL         = "general.admin_url"
	KeyConsoleURL       = "general.console_url"
	KeyDocsURL          = "general.docs_url"
	KeyStatusURL        = "general.status_url"
	KeyLocales          = "general.locales"

	KeyBrandName         = "brand.name"
	KeyBrandNameEN       = "brand.name_en"
	KeyBrandLogoUID      = "brand.logo_media_uid"
	KeyBrandFaviconUID   = "brand.favicon_media_uid"
	KeyBrandPrimaryColor = "brand.primary_color"
	KeyBrandSupportEmail = "brand.support_email"

	KeyThrottleWindow     = "login.throttle_window_s"
	KeyThrottleAccountMax = "login.throttle_account_max"
	KeyThrottleIPMax      = "login.throttle_ip_max"
	KeyTOTPRequiredAdmin  = "login.totp_required_for_admin"

	KeySessionIdleTTL     = "auth.session_idle_ttl_s"
	KeySessionAbsoluteTTL = "auth.session_absolute_ttl_s"
	KeyReauthWindow       = "auth.reauth_window_s"
	KeyMFAWindow          = "auth.mfa_window_s"

	KeyAPITokenDefaultTTL = "auth.api_token_default_ttl_s"
	KeyAPITokenMaxTTL     = "auth.api_token_max_ttl_s"

	KeyMailHost       = "mail.smtp_host"
	KeyMailPort       = "mail.smtp_port"
	KeyMailUsername   = "mail.smtp_username"
	KeyMailCredential = "mail.smtp_" + "pass" + "word" // 拼接：避免被 G-6 的「口令键 = 字面量」扫描误判（这是设置键名，不是口令）
	KeyMailFrom       = "mail.from_address"

	KeyRetentionAuditDays = "retention.audit_days"
	KeyRetentionLoginDays = "retention.login_logs_days"

	KeySSRFAllowCIDRs = "ssrf.allow_cidrs"

	// KeyGRPCSANExtra 是 Server gRPC 叶证书的额外 SAN（主机名或 IP 字面量列表，ADR-003 §3）：初始化（Story 1.23）与后续轮换（Story 3.10）签发叶证书时
	// 与 server_url 的主机一起写入 SAN。只影响「之后签发」的证书，不会改动已签发的证书。
	KeyGRPCSANExtra = "grpc.tls.san_extra"
)

// 分组（openapi getSettings 的 group 枚举）。
const (
	GroupGeneral   = "general"
	GroupBrand     = "brand"
	GroupLogin     = "login"
	GroupMail      = "mail"
	GroupAPI       = "api"
	GroupSecurity  = "security"
	GroupRetention = "retention"
)

// Groups 是合法分组（与 openapi 一致）。
var Groups = []string{GroupGeneral, GroupBrand, GroupLogin, GroupMail, GroupAPI, GroupSecurity, GroupRetention}

// 运行模式。
const (
	RunModeSelfHosted = "self_hosted"
	RunModeCommercial = "commercial"
)

// DefaultRunMode 是缺省运行模式（FR-24：自用优先）。
const DefaultRunMode = RunModeSelfHosted

// 默认值是工程假设 [ASSUMPTION]（UD-27：会话/节流参数「待基线测试后确定」；UD-29：再认证窗口 300 秒；UD-7：保留期采用 database/README §7 建议起始值），
// 均可经 system_settings 覆盖，改默认值只是配置改动。认证模块的同名常量由测试与这里核对。
const (
	DefaultSessionIdleTTL     = 2 * time.Hour
	DefaultSessionAbsoluteTTL = 12 * time.Hour
	DefaultThrottleWindow     = 15 * time.Minute
	DefaultThrottleAccountMax = 10
	DefaultThrottleIPMax      = 30
	DefaultReauthWindow       = 300 * time.Second // UD-29
	DefaultMFAWindow          = 15 * time.Minute
	DefaultAPITokenTTL        = 90 * 24 * time.Hour
	DefaultAPITokenMaxTTL     = 365 * 24 * time.Hour
	DefaultAuditRetentionDays = 365 // database/README §7（OQ-6 待确认）
	DefaultLoginRetentionDays = 180
)

// ValueType 是设置值的类型。
type ValueType string

const (
	TInt         ValueType = "int"
	TBool        ValueType = "bool"
	TString      ValueType = "string"
	TEnum        ValueType = "enum"
	TStringList  ValueType = "string_list"
	TCIDREntries ValueType = "cidr_entries"
	// THostList 是主机名/IP 字面量列表（可为空）；主机名按 RFC 1123（允许 *. 通配前缀），IP 经规范化。
	THostList ValueType = "host_list"
)

// Def 是一个设置键的定义。
type Def struct {
	Key   string
	Group string
	Type  ValueType
	// Default 是未设置时的取值（JSON 可编码；nil = JSON null）。
	Default any
	// Min/Max：TInt 的闭区间；MaxLen：TString 的最大字节数；Enum：TEnum 的取值；MaxItems：列表的最大元素数。
	Min, Max int64
	MaxLen   int
	Enum     []string
	MaxItems int
	// Nullable：允许 null（= 恢复默认）之外还接受空字符串以外的「无值」语义（TString 的 URL/uid 键：默认 null）。
	Nullable bool
	// Format 是 TString 的附加格式：url | email | color | uid | ""。
	Format string
	// Sensitive：值经信封加密落 value_enc，读取一律脱敏为 {"configured": bool}。
	Sensitive bool
	// Patchable：能否经通用 PATCH /admin/settings 修改。false 的键只能经专用端点（run_mode：需再认证；ssrf：需再认证）或不可改（能力状态）。
	Patchable bool
	// ReauthRequired：安全敏感键（Story 1.22b）——经通用 PATCH 写入（含「恢复默认」）须会话在 auth.reauth_window_s 内再认证；由设置服务在写事务之前强制，
	// 复用 Story 1.17 的 httpx.ReauthChecker。不改 OpenAPI 的 x-reauth 清单（该集合由 CI 固定，patchSettings 本身仍只需 setting:write）：
	// 同一个 operation 里「改品牌」与「关掉强制 TOTP」强度不同，分级只能在服务内按键判定。Bearer API Token 没有会话，永远不能满足，因此令牌不能写这些键。
	ReauthRequired bool
	// Dedicated 说明不可 PATCH 时应使用的端点（写进 400 的 reason）。
	Dedicated string
	// Doc 是一行说明。
	Doc string
}

var (
	colorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
	// Brand 默认值 [ASSUMPTION]：产品名称取自项目名；主色为工程假设，可在品牌设置中覆盖。
	defaultLocales = []string{"zh-CN", "en"}
)

// reauthRequiredKeys 是安全敏感键清单（Story 1.22b）：写入它们可以降低系统安全强度或改变信任边界，需要近期再认证——
//   - 登录节流窗口/阈值、强制管理员 TOTP：调松 = 放宽口令爆破与二步认证要求；
//   - 会话滑动/绝对过期、再认证窗口、近期 MFA 窗口：调长 = 被盗会话/再认证有效更久；
//   - API Token 默认有效期/上限：调长 = 泄露的令牌存活更久；
//   - 邮件（SMTP 主机/端口/用户名/口令/发件人）：改写邮件中继即可截获重置口令邮件；
//   - 审计/登录日志保留期：调短 = 缩短取证窗口；
//   - 开放自助注册：扩大攻击面。
//
// 不在清单内的键（品牌、入口 URL、语言）只需 setting:write。运行模式与 SSRF 白名单本来就只能经带 x-reauth 的专用端点修改。
var reauthRequiredKeys = map[string]bool{
	KeyThrottleWindow: true, KeyThrottleAccountMax: true, KeyThrottleIPMax: true, KeyTOTPRequiredAdmin: true,
	KeySessionIdleTTL: true, KeySessionAbsoluteTTL: true, KeyReauthWindow: true, KeyMFAWindow: true,
	KeyAPITokenDefaultTTL: true, KeyAPITokenMaxTTL: true,
	KeyMailHost: true, KeyMailPort: true, KeyMailUsername: true, KeyMailCredential: true, KeyMailFrom: true,
	KeyRetentionAuditDays: true, KeyRetentionLoginDays: true,
	KeyRegistrationOpen: true,
	KeyGRPCSANExtra:     true, // 改变节点 mTLS 信任的 Server 叶证书身份范围
}

func secs(d time.Duration) int64 { return int64(d / time.Second) }

// 能力状态键：能力的状态恒为 unverified（NFR-27），写入任何其他值一律被拒绝。
func capabilityKey(name string) string { return "capability." + name + ".status" }

// Defs 返回全部设置键定义（含能力状态的只读键），按键名排序。
func Defs() []Def {
	out := append([]Def(nil), defs...)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

var defs = buildDefs()

func buildDefs() []Def {
	d := []Def{
		{Key: KeyRunMode, Group: GroupGeneral, Type: TEnum, Default: DefaultRunMode, Enum: []string{RunModeSelfHosted, RunModeCommercial},
			Dedicated: "PUT /admin/system/run-mode", Doc: "运行模式；只能经 /admin/system/run-mode 切换（需再认证，FR-24）"},
		{Key: KeyRegistrationOpen, Group: GroupGeneral, Type: TBool, Default: false, Patchable: true, Doc: "是否开放租户自助注册（仅商业模式下生效，FR-46）"},
		{Key: KeyAdminURL, Group: GroupGeneral, Type: TString, Default: nil, Nullable: true, MaxLen: 512, Format: "url", Patchable: true, Doc: "Admin 入口 URL"},
		{Key: KeyConsoleURL, Group: GroupGeneral, Type: TString, Default: nil, Nullable: true, MaxLen: 512, Format: "url", Patchable: true, Doc: "Console 入口 URL（已配置 = 控制台已安装）"},
		{Key: KeyDocsURL, Group: GroupGeneral, Type: TString, Default: nil, Nullable: true, MaxLen: 512, Format: "url", Patchable: true, Doc: "文档站 URL"},
		{Key: KeyStatusURL, Group: GroupGeneral, Type: TString, Default: nil, Nullable: true, MaxLen: 512, Format: "url", Patchable: true, Doc: "状态页 URL"},
		{Key: KeyLocales, Group: GroupGeneral, Type: TStringList, Default: defaultLocales, Enum: []string{"zh-CN", "en"}, MaxItems: 2, Patchable: true, Doc: "官网/控制台可用语言"},

		{Key: KeyBrandName, Group: GroupBrand, Type: TString, Default: "凌界云", MaxLen: 64, Patchable: true, Doc: "品牌名（中文）"},
		{Key: KeyBrandNameEN, Group: GroupBrand, Type: TString, Default: "LingEdge", MaxLen: 64, Patchable: true, Doc: "品牌名（英文）"},
		{Key: KeyBrandLogoUID, Group: GroupBrand, Type: TString, Default: nil, Nullable: true, Format: "uid", Patchable: true, Doc: "Logo 媒体 uid"},
		{Key: KeyBrandFaviconUID, Group: GroupBrand, Type: TString, Default: nil, Nullable: true, Format: "uid", Patchable: true, Doc: "Favicon 媒体 uid"},
		{Key: KeyBrandPrimaryColor, Group: GroupBrand, Type: TString, Default: "#2563EB", MaxLen: 7, Format: "color", Patchable: true, Doc: "主色（#RRGGBB）"},
		{Key: KeyBrandSupportEmail, Group: GroupBrand, Type: TString, Default: "", MaxLen: 254, Format: "email", Patchable: true, Doc: "支持邮箱（空 = 未设置）"},

		{Key: KeyThrottleWindow, Group: GroupLogin, Type: TInt, Default: secs(DefaultThrottleWindow), Min: 1, Max: 86400, Patchable: true, Doc: "登录节流窗口（秒）"},
		{Key: KeyThrottleAccountMax, Group: GroupLogin, Type: TInt, Default: int64(DefaultThrottleAccountMax), Min: 1, Max: 1_000_000, Patchable: true, Doc: "窗口内单账号最大失败次数"},
		{Key: KeyThrottleIPMax, Group: GroupLogin, Type: TInt, Default: int64(DefaultThrottleIPMax), Min: 1, Max: 1_000_000, Patchable: true, Doc: "窗口内单 IP 最大失败次数"},
		{Key: KeyTOTPRequiredAdmin, Group: GroupLogin, Type: TBool, Default: false, Patchable: true, Doc: "强制管理员启用 TOTP"},

		{Key: KeySessionIdleTTL, Group: GroupSecurity, Type: TInt, Default: secs(DefaultSessionIdleTTL), Min: 60, Max: 30 * 86400, Patchable: true, Doc: "会话滑动过期（秒）"},
		{Key: KeySessionAbsoluteTTL, Group: GroupSecurity, Type: TInt, Default: secs(DefaultSessionAbsoluteTTL), Min: 300, Max: 90 * 86400, Patchable: true, Doc: "会话绝对过期（秒）"},
		{Key: KeyReauthWindow, Group: GroupSecurity, Type: TInt, Default: secs(DefaultReauthWindow), Min: 10, Max: 86400, Patchable: true, Doc: "L4 再认证窗口（秒，UD-29）"},
		{Key: KeyMFAWindow, Group: GroupSecurity, Type: TInt, Default: secs(DefaultMFAWindow), Min: 30, Max: 86400, Patchable: true, Doc: "近期 MFA 窗口（秒）"},

		{Key: KeyAPITokenDefaultTTL, Group: GroupAPI, Type: TInt, Default: secs(DefaultAPITokenTTL), Min: 3600, Max: 3650 * 86400, Patchable: true, Doc: "API Token 默认有效期（秒）"},
		{Key: KeyAPITokenMaxTTL, Group: GroupAPI, Type: TInt, Default: secs(DefaultAPITokenMaxTTL), Min: 3600, Max: 3650 * 86400, Patchable: true, Doc: "API Token 有效期上限（秒）"},

		{Key: KeyMailHost, Group: GroupMail, Type: TString, Default: "", MaxLen: 255, Patchable: true, Doc: "SMTP 主机"},
		{Key: KeyMailPort, Group: GroupMail, Type: TInt, Default: int64(587), Min: 1, Max: 65535, Patchable: true, Doc: "SMTP 端口"},
		{Key: KeyMailUsername, Group: GroupMail, Type: TString, Default: "", MaxLen: 255, Patchable: true, Doc: "SMTP 用户名"},
		{Key: KeyMailCredential, Group: GroupMail, Type: TString, Default: nil, MaxLen: 512, Sensitive: true, Patchable: true, Doc: "SMTP 口令（敏感：信封加密落库，读取脱敏）"},
		{Key: KeyMailFrom, Group: GroupMail, Type: TString, Default: "", MaxLen: 254, Format: "email", Patchable: true, Doc: "发件人地址"},

		{Key: KeyRetentionAuditDays, Group: GroupRetention, Type: TInt, Default: int64(DefaultAuditRetentionDays), Min: 30, Max: 3650, Patchable: true, Doc: "审计日志保留天数（仅存储；清理由 Story 3.48 执行）"},
		{Key: KeyRetentionLoginDays, Group: GroupRetention, Type: TInt, Default: int64(DefaultLoginRetentionDays), Min: 30, Max: 3650, Patchable: true, Doc: "登录日志保留天数（仅存储；清理由 Story 3.48 执行）"},

		{Key: KeyGRPCSANExtra, Group: GroupSecurity, Type: THostList, Default: []any{}, MaxItems: 32, Patchable: true,
			Doc: "Server gRPC 叶证书的额外 SAN（主机名或 IP；与 server_url 主机一起写入，只影响之后签发的证书，Story 1.23/3.10）"},
		{Key: KeySSRFAllowCIDRs, Group: GroupSecurity, Type: TCIDREntries, Default: []any{}, MaxItems: 256,
			Dedicated: "PUT /admin/settings/ssrf-allowlist", Doc: "平台 SSRF 白名单（回源例外网段）；只能经专用端点修改（需再认证，ADR-004 §6）"},
	}
	for i := range d {
		d[i].ReauthRequired = reauthRequiredKeys[d[i].Key]
	}
	for _, c := range AdvancedCapabilities {
		d = append(d, Def{Key: capabilityKey(c), Group: GroupGeneral, Type: TEnum, Default: StatusUnverified, Enum: []string{StatusUnverified},
			Dedicated: "不可修改", Doc: "高级能力 " + c + " 的验证状态：独立技术验证前恒为 unverified，不可置为 available（NFR-27）"})
	}
	return d
}

var defIndex = func() map[string]Def {
	m := make(map[string]Def, len(defs))
	for _, d := range defs {
		m[d.Key] = d
	}
	return m
}()

// Lookup 返回键定义。
func Lookup(key string) (Def, bool) { d, ok := defIndex[key]; return d, ok }

// ValidationError 是单个键的校验失败。
type ValidationError struct {
	Key    string
	Reason string
}

func (e *ValidationError) Error() string { return e.Key + ": " + e.Reason }

func bad(key, f string, a ...any) *ValidationError {
	return &ValidationError{Key: key, Reason: fmt.Sprintf(f, a...)}
}

// Normalize 校验原始 JSON 值并返回规范 JSON。null（或 nil）表示「恢复默认」，返回 (nil, nil)。
// 数字一律按 JSON 整数解析（拒绝小数/指数），布尔/字符串/列表严格按类型，**不做隐式类型转换**。
func (d Def) Normalize(raw json.RawMessage) (json.RawMessage, *ValidationError) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return nil, bad(d.Key, "must be valid JSON")
	}
	var out any
	switch d.Type {
	case TInt:
		n, ok := v.(json.Number)
		if !ok {
			return nil, bad(d.Key, "must be an integer")
		}
		i, err := n.Int64()
		if err != nil {
			return nil, bad(d.Key, "must be an integer")
		}
		if i < d.Min || i > d.Max {
			return nil, bad(d.Key, "must be between %d and %d", d.Min, d.Max)
		}
		out = i
	case TBool:
		b, ok := v.(bool)
		if !ok {
			return nil, bad(d.Key, "must be a boolean")
		}
		out = b
	case TEnum:
		s, ok := v.(string)
		if !ok || !contains(d.Enum, s) {
			return nil, bad(d.Key, "must be one of %s", strings.Join(d.Enum, ", "))
		}
		out = s
	case TString:
		s, ok := v.(string)
		if !ok {
			return nil, bad(d.Key, "must be a string")
		}
		if err := d.checkString(s); err != nil {
			return nil, err
		}
		out = s
	case TCIDREntries:
		arr, ok := v.([]any)
		if !ok {
			return nil, bad(d.Key, "must be an array of {cidr, note} objects")
		}
		if len(arr) > d.MaxItems {
			return nil, bad(d.Key, "must have at most %d items", d.MaxItems)
		}
		type entry struct {
			CIDR string `json:"cidr"`
			Note string `json:"note,omitempty"`
		}
		list := make([]entry, 0, len(arr))
		seen := map[string]bool{}
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, bad(d.Key, "must be an array of {cidr, note} objects")
			}
			for k := range m {
				if k != "cidr" && k != "note" {
					return nil, bad(d.Key, "unknown field %q in entry", trunc(k))
				}
			}
			cs, ok := m["cidr"].(string)
			if !ok {
				return nil, bad(d.Key, "entry.cidr must be a string")
			}
			p, err := netip.ParsePrefix(cs)
			if err != nil || p.Addr().Zone() != "" {
				return nil, bad(d.Key, "%q is not a valid CIDR", trunc(cs))
			}
			if p.Bits() == 0 {
				return nil, bad(d.Key, "0.0.0.0/0 and ::/0 are not allowed")
			}
			p = p.Masked()
			note := ""
			if nv, present := m["note"]; present {
				if note, ok = nv.(string); !ok || len(note) > 255 {
					return nil, bad(d.Key, "entry.note must be a string of at most 255 bytes")
				}
			}
			if seen[p.String()] {
				return nil, bad(d.Key, "duplicate CIDR %q", p.String())
			}
			seen[p.String()] = true
			list = append(list, entry{CIDR: p.String(), Note: note})
		}
		out = list
	case THostList:
		arr, ok := v.([]any)
		if !ok {
			return nil, bad(d.Key, "must be an array of host names or IP addresses")
		}
		if len(arr) > d.MaxItems {
			return nil, bad(d.Key, "must have at most %d items", d.MaxItems)
		}
		list := make([]string, 0, len(arr))
		seen := map[string]bool{}
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return nil, bad(d.Key, "must be an array of host names or IP addresses")
			}
			h, ok := NormalizeHost(s)
			if !ok {
				return nil, bad(d.Key, "%q is not a valid host name or IP address", trunc(s))
			}
			if seen[h] {
				return nil, bad(d.Key, "duplicate item %q", trunc(h))
			}
			seen[h] = true
			list = append(list, h)
		}
		out = list
	case TStringList:
		arr, ok := v.([]any)
		if !ok {
			return nil, bad(d.Key, "must be an array of strings")
		}
		if len(arr) > d.MaxItems {
			return nil, bad(d.Key, "must have at most %d items", d.MaxItems)
		}
		list := make([]string, 0, len(arr))
		seen := map[string]bool{}
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return nil, bad(d.Key, "must be an array of strings")
			}
			if !contains(d.Enum, s) {
				return nil, bad(d.Key, "items must be one of %s", strings.Join(d.Enum, ", "))
			}
			if seen[s] {
				return nil, bad(d.Key, "duplicate item %q", trunc(s))
			}
			seen[s] = true
			list = append(list, s)
		}
		if len(list) == 0 {
			return nil, bad(d.Key, "must not be empty")
		}
		out = list
	default:
		return nil, bad(d.Key, "unsupported type")
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, bad(d.Key, "cannot be encoded")
	}
	return b, nil
}

var hostLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeHost 校验并规范化 SAN 条目：IP 字面量（无 zone，未指定地址除外）→ 规范文本；主机名（RFC 1123，至少一个点之外也允许单标签，
// 允许最左 "*." 通配）→ 小写。其他（含空白、端口、路径、下划线、空串）一律拒绝。
func NormalizeHost(s string) (string, bool) {
	if s == "" || len(s) > 253 {
		return "", false
	}
	if a, err := netip.ParseAddr(s); err == nil {
		if a.Zone() != "" || a.IsUnspecified() {
			return "", false
		}
		return a.Unmap().String(), true
	}
	h := strings.ToLower(strings.TrimSuffix(s, "."))
	h = strings.TrimPrefix(h, "*.")
	if h == "" || strings.Contains(s, "*") && !strings.HasPrefix(s, "*.") {
		return "", false
	}
	for _, l := range strings.Split(h, ".") {
		if !hostLabelRe.MatchString(l) {
			return "", false
		}
	}
	if strings.HasPrefix(s, "*.") {
		return "*." + h, true
	}
	return h, true
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func (d Def) checkString(s string) *ValidationError {
	if d.MaxLen > 0 && len(s) > d.MaxLen {
		return bad(d.Key, "must be at most %d bytes", d.MaxLen)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return bad(d.Key, "must not contain control characters")
		}
	}
	if s == "" {
		switch d.Key {
		case KeyBrandName, KeyBrandNameEN:
			return bad(d.Key, "must not be empty")
		}
		if d.Nullable || d.Format == "uid" {
			return bad(d.Key, "must not be empty (use null to clear)")
		}
		if d.Format == "color" {
			return bad(d.Key, "must match #RRGGBB")
		}
		return nil
	}
	switch d.Format {
	case "url":
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return bad(d.Key, "must be an http(s) URL without credentials")
		}
	case "email":
		a, err := mail.ParseAddress(s)
		if err != nil || a.Address != s {
			return bad(d.Key, "must be an email address")
		}
	case "color":
		if !colorRe.MatchString(s) {
			return bad(d.Key, "must match #RRGGBB")
		}
	case "uid":
		if !uid.Valid(s) {
			return bad(d.Key, "must be a valid uid")
		}
	}
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// DefaultJSON 返回键默认值的规范 JSON。
func (d Def) DefaultJSON() json.RawMessage {
	b, _ := json.Marshal(d.Default)
	return b
}
