package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// 恢复（tools-design §17.5）。步骤即 journal 的步骤号；恢复前安全备份之后的每一步失败都回滚到该安全备份（文件 + 数据库 + master.key），
// 回滚由同一份实现在进程内（失败时）与 `recover --rollback`（崩溃后）执行。
const (
	StepRequest = 1 // 请求记录
	StepSafety  = 2 // 恢复前安全备份（不可撤销，也不需要撤销：它就是回滚的依据）
	StepStop    = 3 // 停服务；本步的 undo = 回滚到安全备份（文件、数据库、按需重启服务）
	StepKey     = 4 // 安装主密钥（仅指纹不匹配且提供了候选主密钥）
	StepFiles   = 5 // 恢复配置与服务数据
	StepDB      = 6 // 恢复数据库
	StepProbe   = 7 // 探针记录解密校验
	StepLineage = 8 // lingedge-server lineage bump（启动 Server 之前）
	StepStart   = 9 // 启动与健康检查
)

// StepNames 是步骤名（journal 与事件）。
var StepNames = map[int]string{StepRequest: "restore_request", StepSafety: "safety_backup", StepStop: "stop_service", StepKey: "master_key",
	StepFiles: "restore_files", StepDB: "restore_database", StepProbe: "probe_decrypt", StepLineage: "lineage_bump", StepStart: "start_and_health"}

// Journal 是 restore 事务用到的 journal 子集（*registry.Journal 满足）。
type Journal interface {
	Begin(step int, name string, undo registry.Undo) error
	Done(step int) error
	Undone(step int) error
	Commit() error
	Abort(reason string) error
	Archive() error
}

// RestoreKind 是恢复失败的类别（cli 据此映射退出码）。
type RestoreKind string

// RestoreKind 取值。
const (
	KindBadBackup        RestoreKind = "bad_backup"       // 结构/哈希/清单不合法 → 5
	KindNeedPassphrase   RestoreKind = "need_passphrase"  // 加密备份没给口令 → 3
	KindBadPassphrase    RestoreKind = "bad_passphrase"   // 口令错误或被篡改 → 5
	KindKeyMismatch      RestoreKind = "key_mismatch"     // 主密钥指纹不匹配或缺失 → 3
	KindSchemaNewer      RestoreKind = "schema_newer"     // 备份的 schema 比二进制新 → 3
	KindDBNotEmpty       RestoreKind = "db_not_empty"     // 目标库非空且未 --replace → 3
	KindPrecondition     RestoreKind = "precondition"     // 其他前置条件 → 3
	KindUsage            RestoreKind = "usage"            // 参数 → 2
	KindRolledBack       RestoreKind = "rolled_back"      // 失败，已回滚到安全备份 → 10
	KindRollbackFailed   RestoreKind = "rollback_failed"  // 失败，回滚也失败（journal 保留）→ 12
	KindFailedBeforeSafe RestoreKind = "failed_unchanged" // 安全备份阶段失败，系统未改动 → 1
	KindInternal         RestoreKind = "internal"
)

// RestoreError 是恢复的结构化失败。
type RestoreError struct {
	Kind RestoreKind
	Msg  string
	Hint string
	// Safety 是恢复前安全备份的路径（若已创建）。
	Safety string
	Step   int
	Err    error
}

func (e *RestoreError) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

// Unwrap 暴露底层错误。
func (e *RestoreError) Unwrap() error { return e.Err }

func rerr(kind RestoreKind, hint string, err error, format string, a ...any) *RestoreError {
	return &RestoreError{Kind: kind, Msg: fmt.Sprintf(format, a...), Hint: hint, Err: err}
}

// RestoreOptions 是 `backup restore` 的参数。
type RestoreOptions struct {
	// Component 可空（取备份里的组件）；给出时必须与备份一致。
	Component  string
	Archive    string
	Passphrase []byte
	Replace    bool
	DB         *DBConn
	// MasterKeyFile 是候选主密钥文件（本机 master.key 与备份不匹配时安装它；其指纹必须与备份匹配）。
	MasterKeyFile string
	// UseBackupMasterKey：安装备份里自带的 master.key（备份须是 --include-master-key 创建的）。
	UseBackupMasterKey bool
	// InstalledVersion 是本机组件当前版本（来自 state.json；空表示未知）。
	InstalledVersion string
	// SafetyDir 是安全备份目录（空取缺省备份目录）；SafetyPassphrase 非空则加密安全备份。
	SafetyDir        string
	SafetyPassphrase []byte
	// TxID 与 OpenJournal 来自写命令的 registry.Task（OpenJournal 即 Task.OpenTx）；journal 只在全部只读前置检查通过之后才打开——
	// 被拒绝的恢复（主密钥不匹配、schema 过新、目标库非空…）不会留下任何事务记录。DryRun 时可为空。J 仅供测试直接注入。
	TxID        string
	OpenJournal func() (Journal, error)
	J           Journal
	DryRun      bool
}

// RestoreResult 是恢复（或 --dry-run 计划）的结果。
type RestoreResult struct {
	Outcome        string      `json:"outcome"` // restored | planned
	Component      string      `json:"component"`
	Archive        string      `json:"archive"`
	BackupVersion  string      `json:"backup_version"`
	BackupHost     string      `json:"backup_host"`
	BackupCreated  string      `json:"backup_created_at"`
	SchemaBackup   int64       `json:"schema_backup"`
	SchemaBinary   int64       `json:"schema_binary,omitempty"`
	KeyCheck       string      `json:"key_check"` // match | rotated_superset | installed_from_file | installed_from_backup | not_applicable
	MasterKeySaved string      `json:"saved_key_file,omitempty"`
	Safety         string      `json:"safety_backup,omitempty"`
	Replace        bool        `json:"replace"`
	DBObjects      int         `json:"db_objects_before"`
	FilesRestored  []string    `json:"files_restored"`
	FilesKept      []string    `json:"files_kept"`
	Probe          ProbeResult `json:"probe"`
	LineageBefore  int64       `json:"lineage_before,omitempty"`
	LineageAfter   int64       `json:"lineage_after,omitempty"`
	BackupLineage  *int64      `json:"backup_lineage"`
	Health         string      `json:"health,omitempty"`
	Warnings       []Warning   `json:"warnings"`
}

type stagedArchive struct {
	dir string
	m   Manifest
	enc bool
}

func (st stagedArchive) path(e FileEntry) string {
	return filepath.Join(st.dir, filepath.FromSlash(e.Archive))
}

// unpack 完整校验归档并把内容解到暂存目录（校验通过之前不会碰系统）。
func (s *Service) unpack(archive string, pass []byte, dir string) (stagedArchive, error) {
	sink := func(e FileEntry) (io.WriteCloser, error) {
		p := filepath.Join(dir, filepath.FromSlash(e.Archive))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		return security.CreateFile(p, 0o600)
	}
	ar, err := ReadArchive(archive, pass, sink)
	if err != nil {
		return stagedArchive{}, err
	}
	return stagedArchive{dir: dir, m: ar.Manifest, enc: ar.Encrypted}, nil
}

func mapArchiveErr(err error) *RestoreError {
	switch {
	case errors.Is(err, ErrPassphraseRequired):
		return rerr(KindNeedPassphrase, "用 --passphrase-file <文件> 提供口令，或在终端交互输入", err, "这是加密备份，需要口令")
	case errors.Is(err, ErrBadPassphrase):
		return rerr(KindBadPassphrase, "确认口令无误；若口令正确，备份文件可能已损坏或被篡改（先运行 lingedge backup verify）", err, "无法解密备份")
	case errors.Is(err, ErrBadArchive), errors.Is(err, ErrHashMismatch), errors.Is(err, ErrCorrupt), errors.Is(err, ErrBadManifest):
		return rerr(KindBadBackup, "该备份不可用；未改动系统。请换用其他备份或重新取回该文件", err, "备份校验失败")
	}
	return rerr(KindInternal, "", err, "读取备份失败")
}

func (s *Service) loadKeyIf(path string) (KeySet, bool, error) {
	ks, err := LoadKeySet(path, s.AllowedUIDs)
	if errors.Is(err, ErrMasterKeyMissing) {
		return KeySet{}, false, nil
	}
	return ks, err == nil, err
}

type keyPlan struct {
	install bool
	source  string // 要安装的主密钥文件（主机路径）
	verdict string
}

// planKey 比对本机主密钥与备份；不匹配时只在提供了「指纹匹配的候选主密钥」时才计划安装它。
func (s *Service) planKey(m Manifest, o RestoreOptions, st stagedArchive) (keyPlan, error) {
	if m.MasterKeyFingerprint == "" && len(m.KEKFingerprints) == 0 {
		return keyPlan{verdict: "not_applicable"}, nil
	}
	cur, have, curErr := s.loadKeyIf(s.host(ServerMasterKey))
	if curErr != nil {
		have = false // 本机主密钥不可用（格式/权限/属主不符）：按「缺失」处理——只有候选主密钥能让恢复继续，旧文件照样保留
	}
	var mm *KeyMismatch
	if have {
		if mm = CheckKeyMatch(m, cur); mm == nil {
			if len(m.KEKIDsRequired) > 0 && m.MasterKeyFingerprint != cur.Overall {
				return keyPlan{verdict: "rotated_superset"}, nil
			}
			return keyPlan{verdict: "match"}, nil
		}
	} else {
		mm = &KeyMismatch{Missing: append([]string(nil), m.KEKIDsRequired...), Overall: len(m.KEKIDsRequired) == 0}
	}
	// 候选主密钥。
	var cand string
	verdict := ""
	switch {
	case o.MasterKeyFile != "":
		cand, verdict = o.MasterKeyFile, "installed_from_file"
	case o.UseBackupMasterKey:
		if !m.MasterKeyIncluded {
			return keyPlan{}, rerr(KindKeyMismatch, "该备份不含主密钥（创建时没有 --include-master-key）；请改用 --master-key-file", mm, "指定了 --use-backup-master-key，但备份里没有 master.key")
		}
		for _, e := range m.Files {
			if e.Kind == KindMasterKey {
				cand = st.path(e)
				if err := os.Chmod(cand, 0o400); err != nil {
					return keyPlan{}, err
				}
			}
		}
		verdict = "installed_from_backup"
	}
	if cand == "" && curErr != nil {
		return keyPlan{}, rerr(KindKeyMismatch, "修复本机 master.key（root:root 0400，格式见 docs/operations/secrets.md），或用 --master-key-file 提供备份对应的主密钥", curErr, "本机 master.key 不可用")
	}
	if cand == "" {
		return keyPlan{}, rerr(KindKeyMismatch,
			"恢复必须有对应的 master.key：从其保管处取回后用 --master-key-file <文件> 提供（Tools 会核对指纹并安装，旧文件保留为 master.key.pre-restore-*）；"+
				"若备份是 --include-master-key 创建的，可用 --use-backup-master-key", mm, "%s", mm.Error())
	}
	ck, ok, err := s.loadKeyIf(cand)
	if err != nil || !ok {
		return keyPlan{}, rerr(KindKeyMismatch, "候选主密钥须是 root 属主的 0400 文件，格式同 master.key", err, "候选主密钥不可用")
	}
	if mm2 := CheckKeyMatch(m, ck); mm2 != nil {
		return keyPlan{}, rerr(KindKeyMismatch, "请确认拿到的是备份创建时对应的 master.key", mm2, "候选主密钥与备份同样不匹配：%s", mm2.Error())
	}
	return keyPlan{install: true, source: cand, verdict: verdict}, nil
}

// installKey 保存旧主密钥并装入候选主密钥（调用前 journal 已记录 ActRestoreMasterKey）。
func (s *Service) installKey(source, tx string) (saved string, created bool, err error) {
	dest := s.host(ServerMasterKey)
	saved = dest + ".pre-restore-" + tx
	uid, gid := 0, 0
	fi, serr := os.Lstat(dest)
	switch {
	case serr == nil && fi.Mode().IsRegular():
		if u, g, ok := ownerIDs(fi); ok {
			uid, gid = int(u), int(g)
		}
		if err := os.Rename(dest, saved); err != nil {
			return "", false, err
		}
	case errors.Is(serr, os.ErrNotExist):
		created = true
		saved = ""
	default:
		return "", false, fmt.Errorf("本机 master.key 不是普通文件，拒绝替换")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return saved, created, err
	}
	defer zero(data)
	tmp := filepath.Join(filepath.Dir(dest), ".master.key.restore-"+randHex(4))
	f, err := security.CreateFile(tmp, 0o400)
	if err != nil {
		return saved, created, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return saved, created, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return saved, created, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return saved, created, err
	}
	if s.Own != nil {
		if err := s.Own.Chown(tmp, uid, gid); err != nil {
			_ = os.Remove(tmp)
			return saved, created, err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return saved, created, err
	}
	return saved, created, nil
}

// applyFiles 把清单里的文件写回。keepLocalRuntime 为 true 时保留本机现有的运行期凭据文件（它与当前 MySQL 账号一致，且主密钥仍能解它）。
// 返回已恢复与被保留的目标路径。
func (s *Service) applyFiles(st stagedArchive, keepLocalRuntime bool, kinds ...string) (restored, kept []string, err error) {
	want := map[string]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	rcDest := service.ServerDataDir + "/" + RuntimeCredentialsRel
	for _, e := range st.m.Files {
		switch e.Kind {
		case KindDB, KindToolsState:
			continue
		case KindMasterKey:
			continue // 主密钥只经 installKey 路径安装，绝不当作普通文件写回
		}
		if len(want) > 0 && !want[e.Kind] {
			continue
		}
		if err := validDest(e.Dest, st.m.Component); err != nil {
			return restored, kept, fmt.Errorf("清单目标路径不合法 %q: %v", e.Dest, err)
		}
		if keepLocalRuntime && e.Dest == rcDest && fileExists(s.host(e.Dest)) {
			kept = append(kept, e.Dest)
			continue
		}
		if err := s.writeFromStage(s.host(e.Dest), st.path(e), fs.FileMode(e.Mode), destOwner{e.Owner, e.Group}); err != nil {
			return restored, kept, fmt.Errorf("恢复 %s 失败: %w", e.Dest, err)
		}
		restored = append(restored, e.Dest)
	}
	return restored, kept, nil
}

// runtimeCredsDecryptable 判断本机运行期凭据文件的密文能否被（最终的）主密钥集解开：其 kek_id 全部在集合内。
func (s *Service) runtimeCredsDecryptable() (bool, error) {
	cur, have, err := s.loadKeyIf(s.host(ServerMasterKey))
	if err != nil || !have {
		return false, err
	}
	ids, err := runtimeCredKEKs(s.host(service.ServerDataDir + "/" + RuntimeCredentialsRel))
	if err != nil {
		return false, nil
	}
	for _, id := range ids {
		if _, ok := cur.FP[id]; !ok {
			return false, nil
		}
	}
	return len(ids) > 0, nil
}

// Restore 执行 `backup restore`（持有组件锁的调用方负责取锁与 OpenTx）。
func (s *Service) Restore(ctx context.Context, o RestoreOptions) (res *RestoreResult, err error) {
	stage, err := s.newStage("restore-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	// 1) 完整校验备份并解到暂存区：此前不碰系统。
	st, err := s.unpack(o.Archive, o.Passphrase, stage)
	if err != nil {
		return nil, mapArchiveErr(err)
	}
	m := st.m
	if o.Component != "" && o.Component != m.Component {
		return nil, rerr(KindUsage, "", nil, "备份的组件是 %s，与 --component %s 不一致", m.Component, o.Component)
	}
	comp := m.Component
	res = &RestoreResult{Outcome: "planned", Component: comp, Archive: o.Archive, BackupVersion: m.Version, BackupHost: m.Host, BackupCreated: m.CreatedAt,
		SchemaBackup: m.SchemaVersion, BackupLineage: m.DeployLineage, Replace: o.Replace, KeyCheck: "not_applicable", FilesRestored: []string{}, FilesKept: []string{}, Warnings: []Warning{}}
	if comp == "server" && o.DB == nil {
		return nil, rerr(KindUsage, "", nil, "恢复 server 需要数据库连接")
	}
	if m.Purpose == PurposePreRestore {
		res.Warnings = append(res.Warnings, Warning{"restore.safety_source", "正在恢复的是一份「恢复前安全备份」（等价于撤销此前的某次恢复）"})
	}

	// 2) 前置检查（只读）。
	var kp keyPlan
	if comp == "server" {
		binSchema, err := s.binarySchema(ctx)
		if err != nil {
			return nil, rerr(KindPrecondition, "确认 /opt/lingedge/server/current 指向已安装的 lingedge-server", err, "无法确定当前 Server 二进制的迁移级别")
		}
		res.SchemaBinary = binSchema
		if m.SchemaVersion > binSchema {
			return nil, rerr(KindSchemaNewer, "先升级 Server 二进制（lingedge update server）到不低于该备份的版本，再恢复", nil,
				"备份的 schema 版本 %d 比当前二进制支持的 %d 更新，拒绝恢复到旧二进制", m.SchemaVersion, binSchema)
		}
		if kp, err = s.planKey(m, o, st); err != nil {
			return nil, err
		}
		res.KeyCheck = kp.verdict
		objs, err := s.MySQL.ListObjects(ctx, *o.DB)
		if err != nil {
			return nil, rerr(KindPrecondition, "检查数据库连接目标与口令", err, "无法检查目标数据库")
		}
		res.DBObjects = objs.Count()
		if objs.Count() > 0 && !o.Replace {
			return nil, rerr(KindDBNotEmpty, "目标库默认必须为空；确需覆盖请加 --replace（会再要求键入主机名确认）。已有数据不会被改动", nil,
				"目标数据库 %s 非空（%d 个对象），拒绝恢复", o.DB.Name, objs.Count())
		}
	}
	if kp.install {
		res.Warnings = append(res.Warnings, Warning{"restore.master_key_replace", "将安装与备份对应的 master.key；本机现有文件保留为 master.key.pre-restore-*（0400），请核对后自行归档，Tools 不会删除任何主密钥"})
	}
	if o.DryRun {
		return res, nil
	}
	if o.TxID == "" || (o.J == nil && o.OpenJournal == nil) {
		return nil, rerr(KindInternal, "", nil, "restore 需要事务日志")
	}
	units := s.unitsFor(comp)
	if s.Sys == nil {
		return nil, rerr(KindPrecondition, "Tools 仅支持 Linux + systemd", nil, "无法控制服务：未配置 systemd")
	}
	if o.J == nil {
		jj, jerr := o.OpenJournal()
		if jerr != nil {
			return nil, rerr(KindInternal, "", jerr, "无法打开事务日志")
		}
		o.J = jj
	}

	// 3) journal：请求记录 → 安全备份。
	begun := []int{}
	j := o.J
	begin := func(step int, undo registry.Undo) error {
		if err := j.Begin(step, StepNames[step], undo); err != nil {
			return err
		}
		begun = append(begun, step)
		return nil
	}
	abortUnchanged := func(e *RestoreError) (*RestoreResult, error) {
		for i := len(begun) - 1; i >= 0; i-- {
			_ = j.Undone(begun[i])
		}
		_ = j.Abort("restore failed before any change: " + e.Msg)
		_ = j.Archive()
		return nil, e
	}
	if err := begin(StepRequest, registry.Undo{Actions: []registry.UndoAction{{Action: ActRestoreRequest, Extra: map[string]string{ExtraComponent: comp}}}}); err != nil {
		return nil, rerr(KindInternal, "", err, "写事务日志失败")
	}
	if err := j.Done(StepRequest); err != nil {
		return nil, rerr(KindInternal, "", err, "写事务日志失败")
	}

	if err := begin(StepSafety, registry.Undo{}); err != nil {
		return nil, rerr(KindInternal, "", err, "写事务日志失败")
	}
	safety, serr := s.Create(ctx, CreateOptions{Component: comp, Version: orDefault(o.InstalledVersion, m.Version), OutputDir: o.SafetyDir, Encrypt: len(o.SafetyPassphrase) > 0,
		Passphrase: o.SafetyPassphrase, DB: o.DB, Purpose: PurposePreRestore, IncludeLogs: false})
	if serr != nil {
		return abortUnchanged(&RestoreError{Kind: KindFailedBeforeSafe, Msg: "无法创建恢复前安全备份，恢复已中止，系统未改动", Hint: "排查磁盘空间/数据库连接后重试", Err: serr})
	}
	if _, verr := s.Verify(safety.Path, o.SafetyPassphrase); verr != nil {
		return abortUnchanged(&RestoreError{Kind: KindFailedBeforeSafe, Msg: "恢复前安全备份回读校验失败，恢复已中止，系统未改动", Err: verr, Safety: safety.Path})
	}
	res.Safety = safety.Path
	res.Warnings = append(res.Warnings, safety.Warnings...)
	if err := j.Done(StepSafety); err != nil {
		return nil, rerr(KindInternal, "", err, "写事务日志失败")
	}
	if err := s.failAt("after:safety_backup"); err != nil {
		return abortUnchanged(&RestoreError{Kind: KindFailedBeforeSafe, Msg: "恢复中止（故障注入）", Err: err, Safety: safety.Path})
	}

	// 4) 停服务。从这里开始任何失败都回滚到安全备份。
	wasActive, aerr := s.activeUnits(ctx, units)
	if aerr != nil {
		return abortUnchanged(&RestoreError{Kind: KindPrecondition, Msg: "无法查询服务状态，恢复已中止，系统未改动", Err: aerr, Safety: safety.Path})
	}
	safetyAct := registry.UndoAction{Action: ActRestoreSafety, Path: safety.Path, Extra: map[string]string{
		ExtraComponent: comp, ExtraUnits: unitsCSV(units), ExtraWasActive: fmt.Sprint(wasActive), ExtraEncrypted: fmt.Sprint(len(o.SafetyPassphrase) > 0)}}
	var savedKey string
	keyAct := registry.UndoAction{}
	fail := func(step int, e error) (*RestoreResult, error) {
		return s.failAndRollback(ctx, o, safetyAct, keyAct, begun, step, e, res)
	}
	if err := begin(StepStop, registry.Undo{Actions: []registry.UndoAction{safetyAct}}); err != nil {
		return nil, rerr(KindInternal, "", err, "写事务日志失败")
	}
	if err := s.Sys.Stop(ctx, units...); err != nil {
		return fail(StepStop, fmt.Errorf("停止服务失败: %w", err))
	}
	if err := j.Done(StepStop); err != nil {
		return nil, rerr(KindInternal, "", err, "写事务日志失败")
	}
	if err := s.failAt("after:stop_service"); err != nil {
		return fail(StepStop, err)
	}

	// 5) 主密钥。
	if kp.install {
		keyAct = registry.UndoAction{Action: ActRestoreMasterKey, Path: s.host(ServerMasterKey), Extra: map[string]string{
			ExtraSaved: s.host(ServerMasterKey) + ".pre-restore-" + o.TxID, ExtraCreated: fmt.Sprint(!fileExists(s.host(ServerMasterKey)))}}
		if err := begin(StepKey, registry.Undo{Actions: []registry.UndoAction{keyAct}}); err != nil {
			return fail(StepKey, err)
		}
		saved, _, err := s.installKey(kp.source, o.TxID)
		if err != nil {
			return fail(StepKey, fmt.Errorf("安装主密钥失败: %w", err))
		}
		savedKey = saved
		res.MasterKeySaved = savedKey
		if err := j.Done(StepKey); err != nil {
			return fail(StepKey, err)
		}
		if err := s.failAt("after:master_key"); err != nil {
			return fail(StepKey, err)
		}
	}

	// 6) 文件。
	if err := begin(StepFiles, registry.Undo{}); err != nil {
		return fail(StepFiles, err)
	}
	keep := false
	if comp == "server" {
		keep, _ = s.runtimeCredsDecryptable()
	}
	restored, kept, err := s.applyFiles(st, keep)
	if err != nil {
		return fail(StepFiles, err)
	}
	res.FilesRestored, res.FilesKept = nonNil(restored), nonNil(kept)
	for _, k := range kept {
		res.Warnings = append(res.Warnings, Warning{"restore.kept_runtime_credentials", k + " 保留了本机现有文件（与当前 MySQL 账号一致且主密钥可解密）；备份里的同名文件未覆盖"})
	}
	if err := j.Done(StepFiles); err != nil {
		return fail(StepFiles, err)
	}
	if err := s.failAt("after:restore_files"); err != nil {
		return fail(StepFiles, err)
	}

	// 7) 数据库（仅 server）。
	if comp == "server" {
		if err := begin(StepDB, registry.Undo{}); err != nil {
			return fail(StepDB, err)
		}
		if o.Replace {
			if err := s.MySQL.Wipe(ctx, *o.DB); err != nil {
				return fail(StepDB, fmt.Errorf("清空目标库失败: %w", err))
			}
		}
		var dump string
		for _, e := range m.Files {
			if e.Kind == KindDB {
				dump = st.path(e)
			}
		}
		if err := s.MySQL.Load(ctx, *o.DB, dump); err != nil {
			return fail(StepDB, fmt.Errorf("导入数据库转储失败: %w", err))
		}
		if err := j.Done(StepDB); err != nil {
			return fail(StepDB, err)
		}
		if err := s.failAt("after:restore_database"); err != nil {
			return fail(StepDB, err)
		}

		// 8) 探针解密。
		if err := begin(StepProbe, registry.Undo{}); err != nil {
			return fail(StepProbe, err)
		}
		pr, err := s.probe(ctx, *o.DB, m.KEKIDsRequired, s.host(ServerMasterKey), s.host(service.ServerDataDir+"/"+RuntimeCredentialsRel))
		if err != nil {
			return fail(StepProbe, err)
		}
		res.Probe = pr
		if err := j.Done(StepProbe); err != nil {
			return fail(StepProbe, err)
		}
		if err := s.failAt("after:probe_decrypt"); err != nil {
			return fail(StepProbe, err)
		}

		// 9) lineage bump（启动 Server 之前）。
		if err := begin(StepLineage, registry.Undo{}); err != nil {
			return fail(StepLineage, err)
		}
		before, after, err := s.bumpLineage(ctx)
		if err != nil {
			return fail(StepLineage, err)
		}
		res.LineageBefore, res.LineageAfter = before, after
		if m.DeployLineage != nil && after <= *m.DeployLineage {
			return fail(StepLineage, fmt.Errorf("lineage bump 之后的值 %d 未严格大于备份记录的 %d", after, *m.DeployLineage))
		}
		if err := j.Done(StepLineage); err != nil {
			return fail(StepLineage, err)
		}
		if err := s.failAt("after:lineage_bump"); err != nil {
			return fail(StepLineage, err)
		}
	}

	// 10) 启动与健康检查。
	if err := begin(StepStart, registry.Undo{}); err != nil {
		return fail(StepStart, err)
	}
	if _, err := s.Sys.Start(ctx, units...); err != nil {
		return fail(StepStart, fmt.Errorf("启动服务失败: %w", err))
	}
	health, err := s.checkHealth(ctx, comp, units, m)
	if err != nil {
		return fail(StepStart, err)
	}
	res.Health = health
	if err := j.Done(StepStart); err != nil {
		return fail(StepStart, err)
	}
	if err := s.failAt("after:start_and_health"); err != nil {
		return fail(StepStart, err)
	}
	if err := j.Commit(); err != nil {
		return nil, rerr(KindInternal, "", err, "事务日志无法提交（恢复本身已完成）")
	}
	_ = j.Archive()
	res.Outcome = "restored"
	return res, nil
}

func orDefault(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// checkHealth：server 探测 /healthz（备份含 install.lock，即已完成向导，再探 /healthz/ready）；node 的 Agent 心跳属 Epic 3，这里只确认单元处于 active。
func (s *Service) checkHealth(ctx context.Context, comp string, units []service.UnitName, m Manifest) (string, error) {
	if comp != "server" {
		for _, u := range units {
			st, err := s.Sys.IsActive(ctx, u)
			if err != nil || !isActiveState(st) {
				return "", fmt.Errorf("单元 %s 未处于 active（%s）", u, st)
			}
		}
		return "units_active (node 的 Agent 心跳检查属 Epic 3，等待环境验证)", nil
	}
	url, err := s.serverHealthURL("/healthz")
	if err != nil {
		return "", fmt.Errorf("无法确定健康检查地址: %w", err)
	}
	if err := s.waitHealthy(ctx, url); err != nil {
		return "", err
	}
	for _, e := range m.Files {
		if strings.HasSuffix(e.Dest, "/install.lock") {
			ready, err := s.serverHealthURL("/healthz/ready")
			if err != nil {
				return "", err
			}
			if err := s.waitHealthy(ctx, ready); err != nil {
				return "", fmt.Errorf("恢复后 /healthz/ready 未通过: %w", err)
			}
			return "healthz_ready", nil
		}
	}
	return "healthz", nil
}

// RollbackParams 是回滚到安全备份的输入（也是 journal 里 ActRestoreSafety 动作的内容）。
type RollbackParams struct {
	Safety     string
	Passphrase []byte
	Component  string
	Units      []service.UnitName
	WasActive  bool
	DB         *DBConn
}

// ParseRollback 把 journal 动作还原为参数（路径形状与单元名都经校验：journal 内容在恢复时不被盲信）。
func ParseRollback(a registry.UndoAction, pass []byte, db *DBConn) (RollbackParams, error) {
	if a.Action != ActRestoreSafety {
		return RollbackParams{}, errors.New("不是 restore_safety_backup 动作")
	}
	if !filepath.IsAbs(a.Path) || filepath.Clean(a.Path) != a.Path || !reBackupName.MatchString(filepath.Base(a.Path)) {
		return RollbackParams{}, errors.New("安全备份路径不合法，拒绝")
	}
	comp := a.Extra[ExtraComponent]
	if comp != "server" && comp != "node" {
		return RollbackParams{}, errors.New("安全备份动作的组件不合法")
	}
	units, err := parseUnits(a.Extra[ExtraUnits])
	if err != nil {
		return RollbackParams{}, err
	}
	return RollbackParams{Safety: a.Path, Passphrase: pass, Component: comp, Units: units, WasActive: a.Extra[ExtraWasActive] == "true", DB: db}, nil
}

// SafetyEncrypted 报告该动作的安全备份是否加密（recover 据此决定是否向用户要口令）。
func SafetyEncrypted(a registry.UndoAction) bool { return a.Extra[ExtraEncrypted] == "true" }

// RollbackSafety 把系统恢复到安全备份的状态：停服务 → 恢复文件 → 清空并重灌数据库 → 按需重启。幂等（可重复执行直到成功）。
func (s *Service) RollbackSafety(ctx context.Context, p RollbackParams) error {
	if s.Sys == nil {
		return errors.New("未配置 systemd 控制")
	}
	if len(p.Units) > 0 {
		_ = s.Sys.Stop(ctx, p.Units...) // 尽力而为：服务可能本来就是停止的
	}
	stage, err := s.newStage("rollback-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	st, err := s.unpack(p.Safety, p.Passphrase, stage)
	if err != nil {
		return fmt.Errorf("读取恢复前安全备份失败（%s）: %w", p.Safety, err)
	}
	if st.m.Component != p.Component {
		return errors.New("安全备份的组件与回滚请求不一致")
	}
	if _, _, err := s.applyFiles(st, false); err != nil {
		return err
	}
	if p.Component == "server" {
		if p.DB == nil {
			return errors.New("回滚 server 需要数据库连接")
		}
		if err := s.MySQL.Wipe(ctx, *p.DB); err != nil {
			return fmt.Errorf("回滚时清空数据库失败: %w", err)
		}
		for _, e := range st.m.Files {
			if e.Kind == KindDB {
				if err := s.MySQL.Load(ctx, *p.DB, st.path(e)); err != nil {
					return fmt.Errorf("回滚时导入安全备份的数据库转储失败: %w", err)
				}
			}
		}
	}
	if p.WasActive && len(p.Units) > 0 {
		if _, err := s.Sys.Start(ctx, p.Units...); err != nil {
			return fmt.Errorf("回滚后重启服务失败: %w", err)
		}
	}
	return nil
}

// failAndRollback 处理「安全备份之后」的失败：回滚（主密钥 → 安全备份），成功则 journal 以 abort 结束并返回 KindRolledBack；
// 回滚失败则保留 journal（recover --rollback 可重试）并返回 KindRollbackFailed。
func (s *Service) failAndRollback(ctx context.Context, o RestoreOptions, safetyAct, keyAct registry.UndoAction, begun []int, step int, cause error, res *RestoreResult) (*RestoreResult, error) {
	rctx := context.WithoutCancel(ctx)
	base := &RestoreError{Step: step, Safety: safetyAct.Path, Err: cause}
	var rbErr error
	if keyAct.Action != "" {
		if _, err := UndoMasterKey(keyAct); err != nil {
			rbErr = err
		}
	}
	if rbErr == nil {
		p, err := ParseRollback(safetyAct, o.SafetyPassphrase, o.DB)
		if err == nil {
			err = s.RollbackSafety(rctx, p)
		}
		rbErr = err
	}
	if rbErr != nil {
		base.Kind, base.Msg = KindRollbackFailed, fmt.Sprintf("恢复在第 %d 步（%s）失败，且回滚到恢复前安全备份也失败：%v", step, StepNames[step], rbErr)
		base.Hint = "事务日志已保留：排除原因后运行 lingedge recover --rollback 重试；安全备份在 " + safetyAct.Path
		return nil, base
	}
	for i := len(begun) - 1; i >= 0; i-- {
		_ = o.J.Undone(begun[i])
	}
	_ = o.J.Abort(fmt.Sprintf("restore failed at step %d (%s); rolled back to the pre-restore backup", step, StepNames[step]))
	_ = o.J.Archive()
	base.Kind = KindRolledBack
	base.Msg = fmt.Sprintf("恢复在第 %d 步（%s）失败，已回滚到恢复前的状态", step, StepNames[step])
	base.Hint = "系统（文件、数据库、master.key）已恢复到本次恢复之前；恢复前安全备份保留在 " + safetyAct.Path
	return nil, base
}
