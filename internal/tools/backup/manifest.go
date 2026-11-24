package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// FormatVersion 是 BACKUP.json 的格式版本。
const FormatVersion = 1

// 条目类别（FileEntry.Kind）。
const (
	KindConfig     = "config"      // /etc/lingedge/<c>/ 下的配置
	KindData       = "data"        // 服务数据目录
	KindLog        = "log"         // 日志（仅 --include-logs）
	KindMasterKey  = "master_key"  // master.key（仅 --include-master-key 且加密）
	KindMigrateCr  = "migrate_cr"  // migrate.yaml（仅 --include-migrate-credential 且加密）
	KindIdentity   = "identity"    // node 身份私钥（仅 --include-identity 且加密）
	KindToolsState = "tools_state" // state.json 副本（只读参考，恢复不覆盖）
	KindDB         = "db"          // mysqldump 输出
)

// 备份用途。
const (
	PurposeManual     = "manual"
	PurposePreRestore = "pre-restore"
	// PurposePreUpgrade/PurposePreRollback 是 Story 1.43 的升级前备份与手动回滚前的安全备份（同样随 `backup list` 展示，Tools 不自动清理）。
	PurposePreUpgrade  = "pre-upgrade"
	PurposePreRollback = "pre-rollback"
)

// FileEntry 是清单里的一个文件。Archive 是包内路径（固定形态，与 Dest 无关，避免路径穿越）；Dest 是恢复时的绝对目标路径（虚拟根之内）。
type FileEntry struct {
	Archive string `json:"archive"`
	Dest    string `json:"dest,omitempty"`
	Kind    string `json:"kind"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Mode    uint32 `json:"mode"`
	Owner   string `json:"owner,omitempty"`
	Group   string `json:"group,omitempty"`
}

// MigrationRec 是迁移级别历史里的一项（来自 schema_migrations_runs，status=applied、direction=up）。
type MigrationRec struct {
	Version int64  `json:"version"`
	Name    string `json:"name"`
	Class   string `json:"class"`
}

// DBInfo 描述数据库转储。
type DBInfo struct {
	Name string `json:"name"`
	Dump string `json:"dump"` // 包内路径
}

// Manifest 是 BACKUP.json（tools-design §17.1）。
type Manifest struct {
	FormatVersion int    `json:"format_version"`
	Purpose       string `json:"purpose"`
	Component     string `json:"component"`
	Version       string `json:"version"`
	Host          string `json:"host"`
	CreatedAt     string `json:"created_at"`
	ToolsVersion  string `json:"tools_version"`

	SchemaVersion    int64          `json:"schema_version"`
	MigrationHistory []MigrationRec `json:"migration_history"`
	// DeployLineage 是备份时 system_settings 的 deploy.lineage（IMP-07）；无数据库或设置项不存在时为 null。
	DeployLineage *int64 `json:"deploy_lineage"`

	MasterKeyFingerprint string            `json:"master_key_fingerprint"`
	KEKFingerprints      map[string]string `json:"kek_fingerprints"`
	KEKIDsRequired       []string          `json:"kek_ids_required"`

	MasterKeyIncluded         bool `json:"master_key_included"`
	MigrateCredentialIncluded bool `json:"migrate_credential_included"`
	IdentityIncluded          bool `json:"identity_included"`
	LogsIncluded              bool `json:"logs_included"`
	Encrypted                 bool `json:"encrypted"`

	Database *DBInfo     `json:"database,omitempty"`
	Files    []FileEntry `json:"files"`
}

// Meta 是随加密头部明文保存的摘要（不含机密）：`backup list` 与「移除 KEK 前的核对」无需口令即可使用。
type Meta struct {
	FormatVersion        int      `json:"format_version"`
	Purpose              string   `json:"purpose"`
	Component            string   `json:"component"`
	Version              string   `json:"version"`
	Host                 string   `json:"host"`
	CreatedAt            string   `json:"created_at"`
	SchemaVersion        int64    `json:"schema_version"`
	DeployLineage        *int64   `json:"deploy_lineage"`
	MasterKeyFingerprint string   `json:"master_key_fingerprint"`
	KEKIDsRequired       []string `json:"kek_ids_required"`
	MasterKeyIncluded    bool     `json:"master_key_included"`
}

// Meta 由清单导出摘要。
func (m Manifest) Meta() Meta {
	ids := append([]string{}, m.KEKIDsRequired...)
	return Meta{FormatVersion: m.FormatVersion, Purpose: m.Purpose, Component: m.Component, Version: m.Version, Host: m.Host, CreatedAt: m.CreatedAt,
		SchemaVersion: m.SchemaVersion, DeployLineage: m.DeployLineage, MasterKeyFingerprint: m.MasterKeyFingerprint, KEKIDsRequired: ids, MasterKeyIncluded: m.MasterKeyIncluded}
}

var (
	reArchiveFile = regexp.MustCompile(`^files/[0-9]{6}$`)
	reSHA         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reOwner       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
	reVersionStr  = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)
	reHostStr     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)
)

// ManifestName 是包内清单的固定名；DumpName 是 mysqldump 输出的固定包内名。
const (
	ManifestName = "BACKUP.json"
	DumpName     = "db/dump.sql"
)

// 清单校验的上限。
const (
	maxManifestBytes = 16 << 20
	maxFiles         = 200000
)

// Validate 校验清单自身的结构与一致性（不读取归档内容）。
func (m Manifest) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrBadManifest, fmt.Sprintf(format, a...))
	}
	if m.FormatVersion != FormatVersion {
		return bad("不支持的 format_version %d", m.FormatVersion)
	}
	if m.Purpose != PurposeManual && m.Purpose != PurposePreRestore && m.Purpose != PurposePreUpgrade && m.Purpose != PurposePreRollback {
		return bad("purpose 非法")
	}
	if m.Component != "server" && m.Component != "node" {
		return bad("component 只能是 server 或 node")
	}
	if !reVersionStr.MatchString(m.Version) || !reHostStr.MatchString(m.Host) {
		return bad("version/host 非法")
	}
	if _, err := time.Parse(time.RFC3339, m.CreatedAt); err != nil {
		return bad("created_at 非法")
	}
	if len(m.Files) > maxFiles {
		return bad("文件过多")
	}
	for id := range m.KEKFingerprints {
		if !secretbox.ValidKEKID(id) || !reSHA.MatchString(m.KEKFingerprints[id]) {
			return bad("kek_fingerprints 非法")
		}
	}
	if m.MasterKeyFingerprint != "" && !reSHA.MatchString(m.MasterKeyFingerprint) {
		return bad("master_key_fingerprint 非法")
	}
	req := append([]string(nil), m.KEKIDsRequired...)
	if !sort.StringsAreSorted(req) {
		return bad("kek_ids_required 须升序")
	}
	for _, id := range req {
		if !secretbox.ValidKEKID(id) {
			return bad("kek_ids_required 含非法 kek_id")
		}
	}
	seen := map[string]bool{}
	seenDest := map[string]bool{}
	hasMaster, hasMig, hasIdent, hasLog, hasDump := false, false, false, false, false
	for _, f := range m.Files {
		if seen[f.Archive] {
			return bad("包内路径重复 %q", f.Archive)
		}
		seen[f.Archive] = true
		if !reSHA.MatchString(f.SHA256) || f.Size < 0 {
			return bad("%q 的 sha256/size 非法", f.Archive)
		}
		switch f.Kind {
		case KindDB:
			if f.Archive != DumpName || f.Dest != "" {
				return bad("数据库转储条目非法")
			}
			hasDump = true
			continue
		case KindMasterKey:
			hasMaster = true
		case KindMigrateCr:
			hasMig = true
		case KindIdentity:
			hasIdent = true
		case KindLog:
			hasLog = true
		case KindConfig, KindData, KindToolsState:
		default:
			return bad("未知条目类别 %q", f.Kind)
		}
		if !reArchiveFile.MatchString(f.Archive) {
			return bad("包内路径 %q 不合规", f.Archive)
		}
		if f.Mode > 0o777 {
			return bad("%q 的权限非法", f.Archive)
		}
		if f.Owner != "" && !reOwner.MatchString(f.Owner) || f.Group != "" && !reOwner.MatchString(f.Group) {
			return bad("%q 的属主非法", f.Archive)
		}
		if f.Kind != KindToolsState {
			if err := validDest(f.Dest, m.Component); err != nil {
				return bad("%q 的目标路径：%v", f.Archive, err)
			}
			if seenDest[f.Dest] {
				return bad("目标路径重复 %q", f.Dest)
			}
			seenDest[f.Dest] = true
		}
	}
	if (m.Database != nil) != hasDump {
		return bad("database 与转储条目不一致")
	}
	if m.Database != nil && (m.Database.Dump != DumpName || !reDBName.MatchString(m.Database.Name)) {
		return bad("database 段非法")
	}
	if m.MasterKeyIncluded != hasMaster || m.MigrateCredentialIncluded != hasMig || m.IdentityIncluded != hasIdent || m.LogsIncluded != hasLog {
		return bad("include 标志与条目不一致")
	}
	// 高敏感条目只允许存在于加密备份里（即便清单被改写也不放行）。
	if (hasMaster || hasMig || hasIdent) && !m.Encrypted {
		return bad("主密钥/迁移凭据/节点身份只能出现在加密备份中")
	}
	return nil
}

// ErrBadManifest：清单结构不合法。
var ErrBadManifest = errors.New("backup: BACKUP.json 不合法")

var reDBName = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)

// restoreRoots 返回某组件的备份/恢复允许触及的绝对路径前缀。恢复时不盲信清单：目标路径必须落在这些前缀内。
func restoreRoots(component string) []string {
	switch component {
	case "server":
		return []string{"/etc/lingedge/server", "/var/lib/lingedge/server", "/var/lib/lingedge/media", "/var/log/lingedge"}
	case "node":
		return []string{"/etc/lingedge/node", "/var/lib/lingedge-node", "/var/log/lingedge-node"}
	}
	return nil
}

func validDest(dest, component string) error {
	if dest == "" || !path.IsAbs(dest) || path.Clean(dest) != dest || strings.ContainsAny(dest, "\x00\\") {
		return errors.New("必须是规范的绝对路径")
	}
	for _, c := range dest {
		if c < 0x20 || c == 0x7f {
			return errors.New("含控制字符")
		}
	}
	for _, root := range restoreRoots(component) {
		if strings.HasPrefix(dest, root+"/") {
			return nil
		}
	}
	return errors.New("不在该组件允许恢复的目录内")
}

// MarshalManifest 序列化清单（确定性：文件按包内路径排序）。
func MarshalManifest(m Manifest) ([]byte, error) {
	if m.Files == nil {
		m.Files = []FileEntry{}
	}
	if m.MigrationHistory == nil {
		m.MigrationHistory = []MigrationRec{}
	}
	if m.KEKIDsRequired == nil {
		m.KEKIDsRequired = []string{}
	}
	if m.KEKFingerprints == nil {
		m.KEKFingerprints = map[string]string{}
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Archive < m.Files[j].Archive })
	return json.MarshalIndent(m, "", "  ")
}

// ParseManifest 严格解析（拒绝未知字段与尾部数据）并校验。
func ParseManifest(b []byte) (Manifest, error) {
	if len(b) > maxManifestBytes {
		return Manifest{}, fmt.Errorf("%w: 过大", ErrBadManifest)
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrBadManifest, err)
	}
	if dec.More() {
		return Manifest{}, fmt.Errorf("%w: 含多余数据", ErrBadManifest)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}
