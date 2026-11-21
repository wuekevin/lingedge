package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 清单 schema（CF-6，tools-design §9.2）。解析规则：
//   - 重复键、超深嵌套、顶层非对象一律拒绝（签名覆盖的是原始字节，解析必须无歧义）；
//   - **未知可选字段忽略；未知必需字段拒绝**：「必需」由对象内的 `critical` 数组声明——producer 把「接收方必须理解」的字段名写进
//     `critical`（X.509 critical 扩展的做法），解析方若不认识其中任一名字即拒绝整份清单；未在 `critical` 中的未知字段被忽略。
//     `critical` 可出现在顶层、component、version 条目、variant、artifact 五个层级；
//   - `schema_version` 不是 1、必需的已知字段缺失、枚举/格式非法同样拒绝。

// SchemaVersion 是本实现理解的清单 schema 版本。
const SchemaVersion = 1

const (
	maxManifestBytes = 8 << 20
	maxJSONDepth     = 24
)

// Manifest 是已通过 schema 校验的清单（验签之后才会得到它，见 Verifier.Verify）。
type Manifest struct {
	SchemaVersion   int
	Channel         string
	Sequence        int64
	GeneratedAt     time.Time
	ValidUntil      time.Time
	RevokedVersions []RevokedVersion
	Components      map[string]Component
	KeyRotation     *KeyRotation
}

type rawManifest struct {
	SchemaVersion   int                  `json:"schema_version"`
	Channel         string               `json:"channel"`
	Sequence        int64                `json:"sequence"`
	GeneratedAt     string               `json:"generated_at"`
	ValidUntil      string               `json:"valid_until"`
	RevokedVersions []RevokedVersion     `json:"revoked_versions"`
	Components      map[string]Component `json:"components"`
	KeyRotation     *KeyRotation         `json:"key_rotation"`
}

// RevokedVersion 是 revoked_versions 的一项。
type RevokedVersion struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Reason    string `json:"reason,omitempty"`

	ver Version
}

// 组件状态。
const (
	CompAvailable  = "available"
	CompUnreleased = "unreleased"
	CompBuiltin    = "builtin"
)

// Component 是 components 中的一项。
type Component struct {
	Status      string         `json:"status"`
	Description string         `json:"description,omitempty"`
	Internal    bool           `json:"internal,omitempty"`
	BuiltinIn   string         `json:"builtin_in,omitempty"`
	Versions    []VersionEntry `json:"versions"`
}

// VersionEntry 是组件的一个发行版本。
type VersionEntry struct {
	Version             string             `json:"version"`
	ReleasedAt          string             `json:"released_at,omitempty"`
	MinUpgradeFrom      string             `json:"min_upgrade_from,omitempty"`
	NotesURL            string             `json:"notes_url,omitempty"`
	Requires            *Requires          `json:"requires,omitempty"`
	Conflicts           []Conflict         `json:"conflicts,omitempty"`
	Ports               []Port             `json:"ports,omitempty"`
	Migrations          *MigrationsNotice  `json:"migrations,omitempty"`
	ConfigSchemaVersion int                `json:"config_schema_version,omitempty"`
	Variants            map[string]Variant `json:"variants"`

	Parsed Version `json:"-"`
}

// Requires 是版本/变体的依赖声明；各约束值都是 ParseConstraint 的语法子集。
type Requires struct {
	System     *SystemReq        `json:"system,omitempty"`
	External   map[string]string `json:"external,omitempty"`
	Components map[string]string `json:"components,omitempty"`
	Runtime    map[string]string `json:"runtime,omitempty"`
	Peer       map[string]string `json:"peer,omitempty"`
}

// SystemReq 是系统要求（数值由发行方填写，Tools 不内置）。
type SystemReq struct {
	OS          []string `json:"os,omitempty"`
	Arch        []string `json:"arch,omitempty"`
	MinMemoryMB int64    `json:"min_memory_mb,omitempty"`
	MinDiskMB   int64    `json:"min_disk_mb,omitempty"`
}

// Conflict 声明与某组件版本范围互斥。
type Conflict struct {
	Component string `json:"component"`
	Version   string `json:"version,omitempty"`
}

// Port 是组件占用端口的预告（供 resolver 做端口矩阵）。
type Port struct {
	Proto       string `json:"proto"`
	Port        int    `json:"port"`
	Bind        string `json:"bind,omitempty"`
	Purpose     string `json:"purpose,omitempty"`
	Conditional bool   `json:"conditional,omitempty"`
}

// MigrationsNotice 是迁移预告（以 `lingedge-server migrate plan --json` 的实际结果为准，取更严格者）。
type MigrationsNotice struct {
	SchemaFrom        string   `json:"schema_from,omitempty"`
	SchemaTo          string   `json:"schema_to,omitempty"`
	Classes           []string `json:"classes,omitempty"`
	IrreversibleCount int      `json:"irreversible_count"`
	BackupRequired    bool     `json:"backup_required"`
}

// Variant 是版本的一个变体（server 的 default，website 的 static/ssr）。
type Variant struct {
	Artifacts map[string]Artifact `json:"artifacts"`
	Requires  *Requires           `json:"requires,omitempty"`
}

// Artifact 是某平台的包（platform ∈ linux-amd64 | linux-arm64 | any）。
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"` // 小写 hex，64
	Size   int64  `json:"size"`   // 下载上限（字节），≥1
}

// KeyRotation 是清单携带的公钥轮换段（§9.1）。
type KeyRotation struct {
	Keys         []Key         `json:"keys"`
	Endorsements []Endorsement `json:"endorsements"`
}

// Endorsement 是旧 active key 对新公钥的背书。
type Endorsement struct {
	KeyID      string `json:"key_id"`      // 被背书的新公钥
	EndorsedBy string `json:"endorsed_by"` // 背书者（旧 active key）
	Sig        string `json:"sig"`         // base64，对 KeyRotationInput(新公钥)
}

var (
	reComponent = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	rePlatform  = regexp.MustCompile(`^(any|linux-(amd64|arm64))$`)
	reSHA256    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reVariant   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// 各层级已知字段（`critical` 校验的依据；新增字段须同步 schema/ 与 CF-6 文档）。
var (
	knownTop       = set("schema_version", "channel", "sequence", "generated_at", "valid_until", "revoked_versions", "components", "key_rotation", "critical")
	knownComponent = set("status", "description", "internal", "builtin_in", "versions", "critical")
	knownVersion   = set("version", "released_at", "min_upgrade_from", "notes_url", "requires", "conflicts", "ports", "migrations", "config_schema_version", "variants", "critical")
	knownVariant   = set("artifacts", "requires", "critical")
	knownArtifact  = set("url", "sha256", "size", "critical")
)

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func schemaErr(format string, a ...any) *Error { return newErr(KindSchema, format, a...) }

// scanJSON 拒绝重复键、超深嵌套与多余内容。
func scanJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := scanValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err == nil {
		return fmt.Errorf("JSON 文档后有多余内容")
	}
	return nil
}

func scanValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= maxJSONDepth {
		return fmt.Errorf("嵌套超过 %d 层", maxJSONDepth)
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string)
			if seen[key] {
				return fmt.Errorf("重复的键 %q", trimTo(key, 32))
			}
			seen[key] = true
			if err := scanValue(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := scanValue(dec, depth+1); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token()
	return err
}

// checkCritical 校验对象的 critical 数组：每个名字必须是该层已知字段。
func checkCritical(where string, obj map[string]any, known map[string]bool) error {
	c, present := obj["critical"]
	if !present {
		return nil
	}
	arr, ok := c.([]any)
	if !ok {
		return schemaErr("%s.critical 必须是字符串数组", where)
	}
	for _, e := range arr {
		name, ok := e.(string)
		if !ok {
			return schemaErr("%s.critical 必须是字符串数组", where)
		}
		if !known[name] {
			return schemaErr("%s 声明了本版本 Tools 不认识的必需字段 %q（升级 Tools 后重试）", where, trimTo(name, 48)).with("critical_field", trimTo(name, 48))
		}
	}
	return nil
}

func asObj(v any) map[string]any { m, _ := v.(map[string]any); return m }

// checkCriticalTree 沿清单的固定层级逐层检查 critical。
func checkCriticalTree(root map[string]any) error {
	if err := checkCritical("manifest", root, knownTop); err != nil {
		return err
	}
	for cname, cv := range asObj(root["components"]) {
		comp := asObj(cv)
		if comp == nil {
			return schemaErr("components.%s 必须是对象", trimTo(cname, 32))
		}
		if err := checkCritical("components."+cname, comp, knownComponent); err != nil {
			return err
		}
		vers, _ := comp["versions"].([]any)
		for i, ve := range vers {
			vo := asObj(ve)
			if vo == nil {
				return schemaErr("components.%s.versions[%d] 必须是对象", trimTo(cname, 32), i)
			}
			where := fmt.Sprintf("components.%s.versions[%d]", cname, i)
			if err := checkCritical(where, vo, knownVersion); err != nil {
				return err
			}
			for vn, vv := range asObj(vo["variants"]) {
				variant := asObj(vv)
				if variant == nil {
					return schemaErr("%s.variants.%s 必须是对象", where, trimTo(vn, 32))
				}
				if err := checkCritical(where+".variants."+vn, variant, knownVariant); err != nil {
					return err
				}
				for pn, av := range asObj(variant["artifacts"]) {
					art := asObj(av)
					if art == nil {
						return schemaErr("%s.variants.%s.artifacts.%s 必须是对象", where, trimTo(vn, 32), trimTo(pn, 32))
					}
					if err := checkCritical(where+".variants."+vn+".artifacts."+pn, art, knownArtifact); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func requireFields(where string, obj map[string]any, names ...string) error {
	for _, n := range names {
		if _, ok := obj[n]; !ok {
			return schemaErr("%s 缺少必需字段 %q", where, n)
		}
	}
	return nil
}

func parseTime(where, s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, schemaErr("%s 不是 RFC 3339 时间: %q", where, trimTo(s, 40))
	}
	return t, nil
}

// ParseManifest 解析并校验清单字节（不验签；验签见 Verifier.Verify，它先验签再调用本函数）。
func ParseManifest(b []byte) (*Manifest, error) {
	if len(b) == 0 || len(b) > maxManifestBytes {
		return nil, schemaErr("清单大小非法（%d 字节，上限 %d）", len(b), maxManifestBytes)
	}
	if err := scanJSON(b); err != nil {
		return nil, &Error{Kind: KindSchema, Msg: "清单不是无歧义的 JSON", Cause: err}
	}
	var tree any
	if err := json.Unmarshal(b, &tree); err != nil {
		return nil, &Error{Kind: KindSchema, Msg: "清单不是合法 JSON", Cause: err}
	}
	root := asObj(tree)
	if root == nil {
		return nil, schemaErr("清单顶层必须是 JSON 对象")
	}
	if err := requireFields("manifest", root, "schema_version", "channel", "sequence", "generated_at", "valid_until", "revoked_versions", "components"); err != nil {
		return nil, err
	}
	if v, ok := root["schema_version"].(float64); !ok || v != SchemaVersion {
		return nil, schemaErr("不支持的 schema_version %v（本版本 Tools 只理解 %d）", root["schema_version"], SchemaVersion)
	}
	if err := checkCriticalTree(root); err != nil {
		return nil, err
	}
	var rm rawManifest
	if err := json.Unmarshal(b, &rm); err != nil {
		return nil, &Error{Kind: KindSchema, Msg: "清单字段类型不符", Cause: err}
	}
	m := &Manifest{
		SchemaVersion: rm.SchemaVersion, Channel: rm.Channel, Sequence: rm.Sequence,
		RevokedVersions: rm.RevokedVersions, Components: rm.Components, KeyRotation: rm.KeyRotation,
	}
	if m.Channel != "stable" && m.Channel != "beta" {
		return nil, schemaErr("channel 必须是 stable|beta，收到 %q", trimTo(m.Channel, 16))
	}
	if m.Sequence < 1 {
		return nil, schemaErr("sequence 必须是 ≥1 的整数，收到 %d", m.Sequence)
	}
	var err error
	if m.GeneratedAt, err = parseTime("generated_at", rm.GeneratedAt); err != nil {
		return nil, err
	}
	if m.ValidUntil, err = parseTime("valid_until", rm.ValidUntil); err != nil {
		return nil, err
	}
	if !m.ValidUntil.After(m.GeneratedAt) {
		return nil, schemaErr("valid_until 必须晚于 generated_at")
	}
	if m.RevokedVersions == nil || m.Components == nil {
		return nil, schemaErr("revoked_versions 与 components 不得为 null")
	}
	for i := range m.RevokedVersions {
		rv := &m.RevokedVersions[i]
		if !reComponent.MatchString(rv.Component) {
			return nil, schemaErr("revoked_versions[%d].component 非法", i)
		}
		if rv.ver, err = ParseVersion(rv.Version); err != nil {
			return nil, err
		}
		if len(rv.Reason) > 512 {
			return nil, schemaErr("revoked_versions[%d].reason 过长", i)
		}
	}
	names := make([]string, 0, len(m.Components))
	for n := range m.Components {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := m.Components[n]
		if !reComponent.MatchString(n) {
			return nil, schemaErr("组件名 %q 非法", trimTo(n, 32))
		}
		if err := validateComponent(n, &c); err != nil {
			return nil, err
		}
		m.Components[n] = c
	}
	if m.KeyRotation != nil {
		if err := validateRotationShape(m.KeyRotation); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func validateComponent(name string, c *Component) error {
	switch c.Status {
	case CompAvailable:
		if c.Versions == nil {
			return schemaErr("components.%s：available 组件必须有 versions 数组（可为空）", name)
		}
	case CompUnreleased:
		if c.Versions != nil {
			return schemaErr("components.%s：unreleased 组件不得有 versions", name)
		}
	case CompBuiltin:
		if c.Versions != nil {
			return schemaErr("components.%s：builtin 组件不得有 versions", name)
		}
		if !reComponent.MatchString(c.BuiltinIn) {
			return schemaErr("components.%s：builtin 组件必须有合法的 builtin_in", name)
		}
	default:
		return schemaErr("components.%s.status 必须是 available|unreleased|builtin，收到 %q", name, trimTo(c.Status, 16))
	}
	seen := map[string]bool{}
	for i := range c.Versions {
		v := &c.Versions[i]
		where := fmt.Sprintf("components.%s.versions[%d]", name, i)
		pv, err := ParseVersion(v.Version)
		if err != nil {
			return err
		}
		v.Parsed = pv
		if seen[pv.String()] {
			return schemaErr("%s：版本 %s 重复", where, pv.String())
		}
		seen[pv.String()] = true
		if err := validateVersionEntry(where, v); err != nil {
			return err
		}
	}
	return nil
}

func validateVersionEntry(where string, v *VersionEntry) error {
	if v.ReleasedAt != "" {
		if _, err := parseTime(where+".released_at", v.ReleasedAt); err != nil {
			return err
		}
	}
	if v.MinUpgradeFrom != "" {
		if _, err := ParseVersion(v.MinUpgradeFrom); err != nil {
			return err
		}
	}
	if v.NotesURL != "" {
		if err := checkHTTPS(where+".notes_url", v.NotesURL); err != nil {
			return err
		}
	}
	if err := validateRequires(where+".requires", v.Requires); err != nil {
		return err
	}
	for i, c := range v.Conflicts {
		if !reComponent.MatchString(c.Component) {
			return schemaErr("%s.conflicts[%d].component 非法", where, i)
		}
		if c.Version != "" {
			if _, err := ParseConstraint(c.Version); err != nil {
				return err
			}
		}
	}
	for i, p := range v.Ports {
		if (p.Proto != "tcp" && p.Proto != "udp") || p.Port < 1 || p.Port > 65535 {
			return schemaErr("%s.ports[%d] 的 proto/port 非法", where, i)
		}
	}
	if m := v.Migrations; m != nil && m.IrreversibleCount < 0 {
		return schemaErr("%s.migrations.irreversible_count 不得为负", where)
	}
	if v.ConfigSchemaVersion < 0 {
		return schemaErr("%s.config_schema_version 不得为负", where)
	}
	if len(v.Variants) == 0 {
		return schemaErr("%s 必须至少有一个 variant（含 artifacts）", where)
	}
	for vn, va := range v.Variants {
		vw := where + ".variants." + vn
		if !reVariant.MatchString(vn) {
			return schemaErr("%s：variant 名非法", vw)
		}
		if err := validateRequires(vw+".requires", va.Requires); err != nil {
			return err
		}
		if len(va.Artifacts) == 0 {
			return schemaErr("%s 必须至少有一个 artifact", vw)
		}
		for pn, a := range va.Artifacts {
			aw := vw + ".artifacts." + pn
			if !rePlatform.MatchString(pn) {
				return schemaErr("%s：platform 必须是 linux-amd64|linux-arm64|any", aw)
			}
			if err := checkHTTPS(aw+".url", a.URL); err != nil {
				return err
			}
			if !reSHA256.MatchString(a.SHA256) {
				return schemaErr("%s.sha256 必须是 64 位小写十六进制", aw)
			}
			if a.Size < 1 || a.Size > 8<<30 {
				return schemaErr("%s.size 必须在 [1, 8GiB] 内（下载上限）", aw)
			}
		}
	}
	return nil
}

func validateRequires(where string, r *Requires) error {
	if r == nil {
		return nil
	}
	for _, m := range []map[string]string{r.External, r.Components, r.Runtime, r.Peer} {
		for k, c := range m {
			if !reComponent.MatchString(k) {
				return schemaErr("%s 含非法依赖名 %q", where, trimTo(k, 32))
			}
			if _, err := ParseConstraint(c); err != nil {
				return err
			}
		}
	}
	if s := r.System; s != nil && (s.MinMemoryMB < 0 || s.MinDiskMB < 0) {
		return schemaErr("%s.system 的 min_* 不得为负", where)
	}
	return nil
}

func checkHTTPS(where, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || strings.ContainsAny(raw, " \t\r\n") || u.User != nil {
		return schemaErr("%s 必须是不含凭据的 https URL", where)
	}
	return nil
}

// RevokedVersion 报告 component@version 是否在 revoked_versions 中（按 SemVer 优先级比较，忽略构建元数据）。
func (m *Manifest) RevokedVersion(component string, v Version) (reason string, revoked bool) {
	for _, rv := range m.RevokedVersions {
		if rv.Component == component && rv.ver.Compare(v) == 0 {
			return rv.Reason, true
		}
	}
	return "", false
}

// CheckTarget 在选定安装/升级目标版本时调用（§9.5 第 4 步）：命中 revoked_versions → KindVersionRevoked。
func (m *Manifest) CheckTarget(component, version string) error {
	v, err := ParseVersion(version)
	if err != nil {
		return err
	}
	if reason, rev := m.RevokedVersion(component, v); rev {
		return newErr(KindVersionRevoked, "%s %s 已被发行方吊销，不可作为安装/升级目标", component, v.String()).
			with("component", component).with("version", v.String()).with("reason", trimTo(reason, 128))
	}
	return nil
}
