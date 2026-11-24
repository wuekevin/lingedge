package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

// StateSchemaVersion 是本程序读写的 state.json schema 版本。
//
// 版本规则：只增不改——同一版本内字段只能新增可选字段是不允许的（解析是严格的，未知字段 = 损坏），任何结构变化都必须递增版本并在 migrate 里写好
// 「旧版本 → 当前」的内存迁移（下次 UpdateState 落盘时即升级）。读到比本程序所知更高的版本时失败关闭（KindStateUnsupported）：
// 旧程序若照常解析并回写，会静默丢掉新版本的字段。
//
// 版本 2（Story 1.31）：新增可选的顶层 host（host.baota：只读探测报告）。版本 1 的文件仍可读取（内存升级为 2，下次落盘时写成 2）；
// 版本 1 的文件里若出现 host 则是损坏（该字段在版本 1 不存在）。已发布的 1.29 程序读到版本 2 会失败关闭（KindStateUnsupported）且不改写——这是版本规则的预期行为。
//
// 版本 3（Story 1.43）：组件记录新增可选的 upgrade（最近一次成功升级：来源版本、迁移级别、备份文件、回滚路径），供 `rollback` 决定路径。
// 版本 1/2 的文件仍可读取（内存升级为 3，下次落盘时写成 3）；版本 1/2 的组件记录里若出现 upgrade 则是损坏（该字段自版本 3 起）。
// 已发布的 1.42 及更早程序读到版本 3 会失败关闭（KindStateUnsupported）且不改写——这是版本规则的预期行为。
const StateSchemaVersion = 3

const (
	maxStateBytes = 4 << 20
	// StateFileMode 是 state.json 的权限：普通用户可读以便 list/status；不含密钥所以可以公开读（见 validate 的泄漏检查）。
	StateFileMode os.FileMode = 0o644
)

// Component 是一个已装组件的记录（与 cli.ComponentInfo 同形，名字是 map 的键）。
type Component struct {
	Version     string `json:"version"`
	State       string `json:"state"`
	Unit        string `json:"unit"`
	RunUser     string `json:"run_user"`
	Ports       []int  `json:"ports"`
	ReleaseDir  string `json:"release_dir"`
	ConfigDir   string `json:"config_dir"`
	DataDir     string `json:"data_dir"`
	InstalledAt string `json:"installed_at"`
	PrevVersion string `json:"prev_version,omitempty"`
	LastTx      string `json:"last_tx,omitempty"` // 最近一次改动该组件的事务 ID
	// Upgrade 是最近一次成功升级的记录（版本 3 起）；回滚成功或重新安装后清除。
	Upgrade *UpgradeRecord `json:"upgrade,omitempty"`
}

// 升级后的回滚路径（UpgradeRecord.Path；tools-design §17.2 决策表）。
const (
	PathBinary        = "binary"         // 无迁移：切回旧 release
	PathMigrateDown   = "migrate_down"   // 全部 reversible：migrate down 到升级前 schema，再切回旧 release
	PathBinaryExpand  = "binary_expand"  // 含 expand（无不可逆）：只切回旧 release，不执行 down
	PathRestoreBackup = "restore_backup" // 含 irreversible：恢复升级前备份 + 旧版本二进制
)

// UpgradeRecord 是一次成功升级留下的、回滚所需的事实。不含任何口令/密钥。
type UpgradeRecord struct {
	From         string   `json:"from"`
	At           string   `json:"at"`
	Tx           string   `json:"tx"`
	Classes      []string `json:"classes"`
	Irreversible bool     `json:"irreversible"`
	Path         string   `json:"path"`
	// SchemaFrom/SchemaTo 是升级前/后的数据库迁移版本（仅 server 且数据库已初始化时记录，否则为 0）。
	SchemaFrom int64 `json:"schema_from,omitempty"`
	SchemaTo   int64 `json:"schema_to,omitempty"`
	// Backup 是升级前备份文件（server：含数据库；其他：空）；BackupEncrypted 标明恢复时需要口令。
	Backup          string `json:"backup,omitempty"`
	BackupEncrypted bool   `json:"backup_encrypted,omitempty"`
}

func (u *UpgradeRecord) validate(name string) error {
	bad := func(f, why string) error {
		return newErr(KindStateInvalid, "组件 %s 的 upgrade.%s %s", name, f, why)
	}
	if !semverRE.MatchString(u.From) {
		return bad("from", "不是 semver: "+quote(u.From))
	}
	if err := requireTime("upgrade.at", u.At); err != nil {
		return err
	}
	if !ValidTxID(u.Tx) {
		return bad("tx", "不是合法的事务 ID")
	}
	switch u.Path {
	case PathBinary, PathMigrateDown, PathBinaryExpand, PathRestoreBackup:
	default:
		return bad("path", "不在 binary|migrate_down|binary_expand|restore_backup 之内: "+quote(u.Path))
	}
	for _, c := range u.Classes {
		if c != "reversible" && c != "expand" && c != "irreversible" {
			return bad("classes", "含未知迁移级别: "+quote(c))
		}
	}
	if u.SchemaFrom < 0 || u.SchemaTo < 0 {
		return bad("schema_from/schema_to", "不得为负")
	}
	if u.Backup != "" && (!filepath.IsAbs(u.Backup) || filepath.Clean(u.Backup) != u.Backup) {
		return bad("backup", "必须是规范的绝对路径: "+quote(u.Backup))
	}
	for f, v := range map[string]string{"from": u.From, "at": u.At, "tx": u.Tx, "path": u.Path, "backup": u.Backup} {
		if err := requireClean("组件 "+name+" 的 upgrade."+f, v); err != nil {
			return err
		}
	}
	return nil
}

// State 是 state.json 的内容。
type State struct {
	SchemaVersion int                  `json:"schema_version"`
	UpdatedAt     string               `json:"updated_at,omitempty"`
	Components    map[string]Component `json:"components"`
	// Host 是宿主机只读探测结果（版本 2 起）。nil = 从未探测。
	Host *Host `json:"host,omitempty"`
}

// Host 是 state.json 的 host 段。
type Host struct {
	Baota *baota.Report `json:"baota,omitempty"`
}

// NewState 返回当前版本的空状态。
func NewState() State {
	return State{SchemaVersion: StateSchemaVersion, Components: map[string]Component{}}
}

// Names 返回按字典序排列的组件名。
func (s State) Names() []string {
	out := make([]string, 0, len(s.Components))
	for n := range s.Components {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// 组件状态取值（健康探测属 1.42，这里只记录安装生命周期结果）。
var validStates = map[string]bool{"installed": true, "running": true, "stopped": true, "failed": true}

var (
	semverRE  = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
	unitRE    = regexp.MustCompile(`^[A-Za-z0-9:_.@-]+\.(service|path)$`)
	runUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

// Validate 校验状态的全部约束；写入前与读取后都调用（手工改坏的文件在读取时即被发现，而不是在某次升级中途才爆）。
// 同时做泄漏检查：任何字符串值经 security 脱敏后若有变化，说明疑似密钥，返回 KindSecret（拒绝写入公开可读的文件）。
func (s State) Validate() error {
	if s.SchemaVersion != StateSchemaVersion {
		return newErr(KindStateInvalid, "schema_version 必须是 %d，收到 %d", StateSchemaVersion, s.SchemaVersion)
	}
	if s.Components == nil {
		return newErr(KindStateInvalid, "components 不能为空对象之外的 null")
	}
	if s.UpdatedAt != "" {
		if err := requireTime("updated_at", s.UpdatedAt); err != nil {
			return err
		}
	}
	if s.Host != nil && s.Host.Baota != nil {
		if err := s.Host.Baota.Validate(); err != nil {
			if strings.Contains(err.Error(), "疑似含密钥") {
				return wrapErr(KindSecret, err, "host.baota 疑似含密钥/口令，拒绝写入")
			}
			return wrapErr(KindStateInvalid, err, "host.baota 不合法")
		}
	}
	for _, name := range s.Names() {
		c := s.Components[name]
		if !ValidComponent(name) {
			return newErr(KindStateInvalid, "组件名不合法: %q", name)
		}
		if err := c.validate(name); err != nil {
			return err
		}
	}
	return nil
}

func (c Component) validate(name string) error {
	bad := func(f, why string) error { return newErr(KindStateInvalid, "组件 %s 的 %s %s", name, f, why) }
	if !semverRE.MatchString(c.Version) {
		return bad("version", "不是 semver: "+quote(c.Version))
	}
	if c.PrevVersion != "" && !semverRE.MatchString(c.PrevVersion) {
		return bad("prev_version", "不是 semver: "+quote(c.PrevVersion))
	}
	if !validStates[c.State] {
		return bad("state", "不在 installed|running|stopped|failed 之内: "+quote(c.State))
	}
	if c.Unit != "" && !unitRE.MatchString(c.Unit) {
		return bad("unit", "不合法: "+quote(c.Unit))
	}
	if c.RunUser != "" && !runUserRE.MatchString(c.RunUser) {
		return bad("run_user", "不合法: "+quote(c.RunUser))
	}
	seen := map[int]bool{}
	for _, p := range c.Ports {
		if p < 1 || p > 65535 || seen[p] {
			return bad("ports", "含非法或重复端口")
		}
		seen[p] = true
	}
	for f, p := range map[string]string{"release_dir": c.ReleaseDir, "config_dir": c.ConfigDir, "data_dir": c.DataDir} {
		if p != "" && (!filepath.IsAbs(p) || filepath.Clean(p) != p) {
			return bad(f, "必须是规范的绝对路径: "+quote(p))
		}
	}
	if c.InstalledAt != "" {
		if err := requireTime("installed_at", c.InstalledAt); err != nil {
			return err
		}
	}
	if c.LastTx != "" && !ValidTxID(c.LastTx) {
		return bad("last_tx", "不是合法的事务 ID")
	}
	if c.Upgrade != nil {
		if err := c.Upgrade.validate(name); err != nil {
			return err
		}
	}
	// 泄漏检查：逐个字符串值。路径/版本/时间戳这类值经脱敏后应当不变。
	for f, v := range map[string]string{
		"version": c.Version, "prev_version": c.PrevVersion, "state": c.State, "unit": c.Unit, "run_user": c.RunUser,
		"release_dir": c.ReleaseDir, "config_dir": c.ConfigDir, "data_dir": c.DataDir, "installed_at": c.InstalledAt, "last_tx": c.LastTx,
	} {
		if err := requireClean("组件 "+name+" 的 "+f, v); err != nil {
			return err
		}
	}
	return nil
}

func quote(s string) string {
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	b, _ := json.Marshal(s)
	return string(b)
}

func requireTime(field, v string) error {
	if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
		return newErr(KindStateInvalid, "%s 不是 RFC3339 时间: %s", field, quote(v))
	}
	return nil
}

// ParseState 解析并校验 state.json 的字节。
//
// 版本处理：先只取 schema_version——缺失/非整数/小于 1 → KindStateCorrupt；大于 StateSchemaVersion → KindStateUnsupported（不尝试解析其余内容）；
// 等于当前版本 → 严格解析（未知字段、尾随数据都是损坏）。更低的旧版本经 migrateState 升到当前。
func ParseState(b []byte) (State, error) {
	var head struct {
		SchemaVersion *int `json:"schema_version"`
	}
	if err := json.Unmarshal(b, &head); err != nil || head.SchemaVersion == nil {
		return State{}, wrapErr(KindStateCorrupt, err, "state.json 不是合法 JSON 或缺少 schema_version")
	}
	v := *head.SchemaVersion
	switch {
	case v < 1:
		return State{}, newErr(KindStateCorrupt, "schema_version=%d 不合法", v)
	case v > StateSchemaVersion:
		return State{}, &Error{Kind: KindStateUnsupported, Msg: "state.json 的 schema_version=" + strconv.Itoa(v) + " 高于本程序支持的 " + strconv.Itoa(StateSchemaVersion) + "，请升级 lingedge；本程序不会改写它",
			Details: map[string]any{"found": v, "supported": StateSchemaVersion}}
	}
	b, err := migrateState(v, b)
	if err != nil {
		return State{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s State
	if err := dec.Decode(&s); err != nil {
		return State{}, wrapErr(KindStateCorrupt, err, "state.json 解析失败（含未知字段或类型错误）")
	}
	if _, err := dec.Token(); err != io.EOF {
		return State{}, newErr(KindStateCorrupt, "state.json 在 JSON 值之后还有多余内容")
	}
	if s.Components == nil {
		s.Components = map[string]Component{}
	}
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	return s, nil
}

// migrateState 把版本 from 的字节升级到当前版本的字节。当前只有版本 1，没有需要迁移的旧版本；
// 未来递增版本时在此按 from 逐级转换，并为每一级补测试。
func migrateState(from int, b []byte) ([]byte, error) {
	for from < StateSchemaVersion {
		switch from {
		case 1: // 1 → 2：只是加了可选的 host；把 schema_version 改写为 2。版本 1 不应含 host。
			var m map[string]json.RawMessage
			if err := json.Unmarshal(b, &m); err != nil {
				return nil, wrapErr(KindStateCorrupt, err, "state.json 不是 JSON 对象")
			}
			if _, has := m["host"]; has {
				return nil, newErr(KindStateCorrupt, "schema_version=1 的 state.json 不应含 host（该字段自版本 2 起）")
			}
			m["schema_version"] = json.RawMessage("2")
			nb, err := json.Marshal(m)
			if err != nil {
				return nil, wrapErr(KindStateCorrupt, err, "无法迁移 state.json")
			}
			b = nb
		case 2: // 2 → 3：只是组件记录加了可选的 upgrade；版本 2 的组件记录不应含 upgrade。
			var m map[string]json.RawMessage
			if err := json.Unmarshal(b, &m); err != nil {
				return nil, wrapErr(KindStateCorrupt, err, "state.json 不是 JSON 对象")
			}
			var comps map[string]map[string]json.RawMessage
			if raw, ok := m["components"]; ok {
				if err := json.Unmarshal(raw, &comps); err != nil {
					return nil, wrapErr(KindStateCorrupt, err, "state.json 的 components 不是对象")
				}
			}
			for name, c := range comps {
				if _, has := c["upgrade"]; has {
					return nil, newErr(KindStateCorrupt, "schema_version<3 的 state.json 里组件 %s 不应含 upgrade（该字段自版本 3 起）", name)
				}
			}
			m["schema_version"] = json.RawMessage("3")
			nb, err := json.Marshal(m)
			if err != nil {
				return nil, wrapErr(KindStateCorrupt, err, "无法迁移 state.json")
			}
			b = nb
		default:
			return nil, newErr(KindStateCorrupt, "没有从 schema_version=%d 到 %d 的迁移", from, StateSchemaVersion)
		}
		from++
	}
	return b, nil
}

// RecordHostBaota 在全局锁内把只读探测报告写入 state.json 的 host.baota（读 → 改 → 校验 → 原子写，经既有 UpdateState）。
// 报告先 Scrub（遮蔽形似密钥的字符串），再由 Validate 兜底；其余字段（组件记录）原样保留。
func (r *Registry) RecordHostBaota(ctx context.Context, rep baota.Report) error {
	rep = rep.Scrub()
	return r.UpdateState(ctx, func(s *State) error {
		if s.Host == nil {
			s.Host = &Host{}
		}
		cp := rep
		s.Host.Baota = &cp
		return nil
	})
}

// MarshalState 校验并序列化（缩进 JSON + 换行；键序固定，便于 diff）。
func MarshalState(s State) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, wrapErr(KindIO, err, "序列化 state.json 失败")
	}
	return append(b, '\n'), nil
}

// LoadState 读取 state.json；文件不存在返回 (空状态, false, nil)。
// 不加锁：写入走原子 rename，读者看到的总是旧版或新版的完整文件。文件必须是普通文件（不跟随符号链接）、
// 属主为当前用户或 root、且 group/other 不可写（否则任何人都能篡改 root 命令信任的状态）。
func (r *Registry) LoadState() (State, bool, error) {
	path := r.L.StateFile()
	f, err := os.OpenFile(path, os.O_RDONLY|noFollowFlag, 0)
	if err != nil {
		if isNotExist(err) {
			return NewState(), false, nil
		}
		return State{}, false, &Error{Kind: KindIO, Msg: "无法读取 state.json", Path: path, Cause: err}
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return State{}, false, &Error{Kind: KindIO, Msg: "无法检查 state.json", Path: path, Cause: err}
	}
	if !fi.Mode().IsRegular() || !ownerTrusted(fi) || fi.Mode().Perm()&0o022 != 0 {
		return State{}, false, &Error{Kind: KindPermission, Msg: "state.json 不是属主可信且仅属主可写的普通文件，拒绝信任", Path: path}
	}
	b, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return State{}, false, &Error{Kind: KindIO, Msg: "无法读取 state.json", Path: path, Cause: err}
	}
	if len(b) > maxStateBytes {
		return State{}, false, &Error{Kind: KindStateCorrupt, Msg: "state.json 过大", Path: path}
	}
	s, err := ParseState(b)
	if err != nil {
		var e *Error
		if asError(err, &e) {
			e.Path = path
		}
		return State{}, false, err
	}
	return s, true, nil
}

// SaveState 校验、序列化并原子写 state.json（security.WriteFileAtomic：临时文件 + fsync + rename + 目录 fsync，0644）。
// 调用方负责持有全局锁（UpdateState 已经做了）；直接调用仅用于已持锁的场合与测试。
func (r *Registry) SaveState(s State) error {
	s.SchemaVersion = StateSchemaVersion
	s.UpdatedAt = r.now().UTC().Format("2006-01-02T15:04:05.000Z")
	b, err := MarshalState(s)
	if err != nil {
		return err
	}
	if len(b) > maxStateBytes {
		return newErr(KindStateInvalid, "state.json 超过 %d 字节", maxStateBytes)
	}
	if err := security.WriteFileAtomic(r.L.StateFile(), b, StateFileMode); err != nil {
		return &Error{Kind: KindIO, Msg: "原子写 state.json 失败（旧文件保持不变）", Path: r.L.StateFile(), Cause: err}
	}
	return nil
}

// UpdateState 在全局锁内「读 → fn 修改 → 校验 → 原子写」。fn 返回错误则不写。
// 并发（含跨进程）的多个 UpdateState 串行执行，不丢更新。
func (r *Registry) UpdateState(ctx context.Context, fn func(*State) error) error {
	return r.WithGlobal(ctx, func() error {
		s, _, err := r.LoadState()
		if err != nil {
			return err
		}
		if err := fn(&s); err != nil {
			return err
		}
		return r.SaveState(s)
	})
}

// CleanStaleTemps 删除 kill -9/断电遗留的 state.json 临时文件（.state.json.tmp-*）；在全局锁内进行，返回删除个数。
// 临时文件对读者不可见也不影响正确性，这只是回收空间。
func (r *Registry) CleanStaleTemps(ctx context.Context) (n int, err error) {
	err = r.WithGlobal(ctx, func() error {
		ms, gerr := filepath.Glob(filepath.Join(r.L.Root, ".state.json.tmp-*"))
		if gerr != nil {
			return gerr
		}
		for _, m := range ms {
			if fi, e := os.Lstat(m); e == nil && fi.Mode().IsRegular() && os.Remove(m) == nil {
				n++
			}
		}
		return nil
	})
	return n, err
}
