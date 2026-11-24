package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// 固定路径（tools-design §8、§17.1）。
const (
	// DefaultBackupDir 是备份的缺省目录（root 0700）。
	DefaultBackupDir = "/var/lib/lingedge/backups"
	// ServerMasterKey 是主密钥文件（root 属主；备份不含它，除非 --include-master-key 且加密）。
	ServerMasterKey = service.ServerConfigDir + "/master.key"
	// ServerMigrateYAML 是 root 侧迁移凭据文件（备份默认排除，ADR-026 §4）。
	ServerMigrateYAML = service.ServerConfigDir + "/migrate.yaml"
	// ServerConfigFile 是 server.yaml。
	ServerConfigFile = service.ServerConfigDir + "/server.yaml"
	// ServerCurrentBin 是 Server 当前版本的二进制（lineage bump / migrate plan 经它）。
	ServerCurrentBin = "/opt/lingedge/server/current/bin/lingedge-server"
	// RuntimeCredentialsRel 是运行期凭据文件（密文）相对 server 数据目录的路径（ADR-026 §3）。
	RuntimeCredentialsRel = "secrets/runtime-credentials.json"
	// filePrefix 是备份文件名前缀。
	filePrefix = "lingedge-backup-"
)

// Warning 是不致命的提示。
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Service 是备份/恢复的编排。字段都是「世界」的接缝（根目录前缀、外部命令执行器、系统控制）；业务逻辑永远是生产代码。
type Service struct {
	// Root 非空时 /etc /opt /var 路径前缀到该目录（测试用；生产为空）。
	Root string
	// Reg 用于读取 state.json 与定位 Tools 状态目录（暂存区在其下）。
	Reg *registry.Registry
	// MySQL 是 mysqldump/mysql 的封装（Run 为 NewMySQLRunner）。
	MySQL *MySQL
	// ServerRun 执行 lingedge-server（NewServerRunner）：migrate plan --offline、lineage bump。
	ServerRun service.Runner
	// ServerEnv 是传给 lingedge-server 子进程的额外环境变量（KEY=VALUE；不得含口令）。生产为空：Tools 自己补上主密钥路径。
	ServerEnv []string
	// ServerConfig 覆盖 lineage bump 使用的 server.yaml 路径（主机路径；空 = Root + ServerConfigFile）。仅供测试世界使用（其 server.yaml 里是未前缀的绝对路径）。
	ServerConfig string
	// Sys 控制 systemd 单元；Own 解析属主并 chown。
	Sys *service.Systemctl
	Own service.Ownership
	// Now/Hostname/Geteuid 是时钟、主机名与有效 uid；ToolsVersion 记入清单。
	Now          func() time.Time
	Hostname     func() (string, error)
	Geteuid      func() int
	ToolsVersion string
	// KDF 是加密备份的 argon2id 参数（零值取 DefaultKDF；测试用低成本参数）。
	KDF KDFParams
	// AllowedUIDs 是主密钥文件允许的属主（nil 取 {euid, 0}）。
	AllowedUIDs []uint32
	// Health 探测 Server 健康（默认真实 HTTP GET）；HealthTimeout/Interval 为等待窗口。
	Health                        func(ctx context.Context, url string) error
	HealthTimeout, HealthInterval time.Duration
	// FailAt 是故障注入点（仅测试；生产为 nil）：在每个步骤之后以 "after:<步骤名>" 调用，返回错误即模拟该步骤失败。
	FailAt func(point string) error
}

func (s *Service) host(virtual string) string {
	if s.Root == "" {
		return virtual
	}
	return filepath.Join(s.Root, virtual)
}

func (s *Service) virtual(host string) string {
	if s.Root == "" {
		return host
	}
	rel, err := filepath.Rel(s.Root, host)
	if err != nil || strings.HasPrefix(rel, "..") {
		return host
	}
	return "/" + filepath.ToSlash(rel)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) failAt(point string) error {
	if s.FailAt == nil {
		return nil
	}
	return s.FailAt(point)
}

// scratch 是暂存根目录：Tools 状态目录下的 staging（root 0700）。
func (s *Service) scratch() string { return s.Reg.L.StagingDir() }

func (s *Service) newStage(prefix string) (string, error) {
	if err := security.MkdirAllPerm(s.scratch(), 0o700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(s.scratch(), prefix)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// DefaultDir 返回缺省备份目录（主机路径）。
func (s *Service) DefaultDir() string { return s.host(DefaultBackupDir) }

// ---- 创建 ----

// CreateOptions 是 `backup create` 的参数。
type CreateOptions struct {
	Component string
	// Version 是组件当前版本（来自 state.json）。
	Version string
	// OutputDir 是（主机）目录；空取缺省目录。
	OutputDir                                                          string
	Encrypt                                                            bool
	Passphrase                                                         []byte
	IncludeLogs, IncludeMasterKey, IncludeMigrateCred, IncludeIdentity bool
	// DB 是数据库连接（仅 server）；Create 不关闭它。
	DB      *DBConn
	Purpose string
	DryRun  bool
}

// PlanFile 是计划里的一个文件。
type PlanFile struct {
	Dest string `json:"dest"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

// CreateResult 是创建结果。
type CreateResult struct {
	Path                 string     `json:"path"`
	Size                 int64      `json:"size"`
	SHA256               string     `json:"sha256"`
	Encrypted            bool       `json:"encrypted"`
	Manifest             Manifest   `json:"-"`
	Files                []PlanFile `json:"files"`
	Database             string     `json:"database,omitempty"`
	MasterKeyFingerprint string     `json:"master_key_fingerprint,omitempty"`
	KEKIDsRequired       []string   `json:"kek_ids_required"`
	DeployLineage        *int64     `json:"deploy_lineage"`
	SchemaVersion        int64      `json:"schema_version"`
	Warnings             []Warning  `json:"warnings"`
	DryRun               bool       `json:"dry_run"`
}

// 创建时的错误。
var (
	ErrInsecureDir = errors.New("backup: 未加密备份所在目录必须仅属主可访问（0700）")
	ErrUsage       = errors.New("backup: 参数组合不合法")
)

type plannedFile struct {
	host, dest, kind string
	mode             fs.FileMode
	owner, group     string
	size             int64
}

func ownerNames(fi os.FileInfo) (string, string) {
	uid, gid, ok := ownerIDs(fi)
	if !ok {
		return "", ""
	}
	var on, gn string
	if u, err := user.LookupId(strconv.Itoa(int(uid))); err == nil && reOwner.MatchString(u.Username) {
		on = u.Username
	}
	if g, err := user.LookupGroupId(strconv.Itoa(int(gid))); err == nil && reOwner.MatchString(g.Name) {
		gn = g.Name
	}
	return on, gn
}

// collect 列出要备份的文件（不读内容）。
func (s *Service) collect(component string, o CreateOptions) ([]plannedFile, []Warning, error) {
	var out []plannedFile
	var warns []Warning
	type root struct{ virt, kind string }
	var roots []root
	switch component {
	case "server":
		roots = []root{{service.ServerConfigDir, KindConfig}, {service.ServerDataDir, KindData}, {"/var/lib/lingedge/media", KindData}}
		if o.IncludeLogs {
			roots = append(roots, root{service.ServerLogDir, KindLog})
		}
	case "node":
		roots = []root{{service.NodeConfigDir, KindConfig}, {service.NodeDataDir, KindData}}
		if o.IncludeLogs {
			roots = append(roots, root{service.NodeLogDir, KindLog})
		}
	default:
		return nil, nil, fmt.Errorf("%w: 组件 %q", ErrUsage, component)
	}
	identityDir := service.NodeDataDir + "/identity"
	for _, r := range roots {
		base := s.host(r.virt)
		if _, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			virt := s.virtual(p)
			if d.IsDir() {
				if component == "node" && virt == service.NodeDataDir+"/staging" {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				warns = append(warns, Warning{"backup.symlink_skipped", "符号链接不进入备份：" + virt})
				return nil
			}
			if !d.Type().IsRegular() {
				warns = append(warns, Warning{"backup.special_skipped", "非普通文件不进入备份：" + virt})
				return nil
			}
			kind := r.kind
			switch {
			case component == "server" && virt == service.ServerConfigDir+"/setup-token.hash":
				return nil // 安装向导令牌哈希：一次性，恢复后需重新生成，不备份
			case component == "server" && virt == ServerMasterKey:
				if !o.IncludeMasterKey {
					return nil
				}
				kind = KindMasterKey
			case component == "server" && virt == ServerMigrateYAML:
				if !o.IncludeMigrateCred {
					return nil
				}
				kind = KindMigrateCr
			case component == "node" && strings.HasPrefix(virt, identityDir+"/"):
				if !o.IncludeIdentity {
					return nil
				}
				kind = KindIdentity
			case strings.HasSuffix(virt, ".partial"):
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			on, gn := ownerNames(fi)
			out = append(out, plannedFile{host: p, dest: virt, kind: kind, mode: fi.Mode().Perm(), owner: on, group: gn, size: fi.Size()})
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("备份：遍历 %s 失败: %w", r.virt, err)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dest < out[j].dest })
	return out, warns, nil
}

func (s *Service) hostLabel() (string, error) {
	h := "localhost"
	if s.Hostname != nil {
		v, err := s.Hostname()
		if err != nil {
			return "", err
		}
		h = v
	}
	h = strings.ToLower(h)
	if !reHostStr.MatchString(h) {
		h = "host"
	}
	return h, nil
}

func (s *Service) fileName(host, component string, enc bool, seq int) string {
	n := fmt.Sprintf("%s%s-%s-%s", filePrefix, host, component, s.now().UTC().Format("20060102T150405Z"))
	if seq > 1 {
		n += "-" + strconv.Itoa(seq)
	}
	n += ".tar.gz"
	if enc {
		n += ".enc"
	}
	return n
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Create 执行 `backup create`（持有组件锁的调用方负责取锁）。
func (s *Service) Create(ctx context.Context, o CreateOptions) (*CreateResult, error) {
	if o.Purpose == "" {
		o.Purpose = PurposeManual
	}
	if o.Component != "server" && o.Component != "node" {
		return nil, fmt.Errorf("%w: 组件只能是 server 或 node", ErrUsage)
	}
	if !o.DryRun && o.Encrypt != (len(o.Passphrase) > 0) {
		return nil, fmt.Errorf("%w: --encrypt 与口令必须同时提供", ErrUsage)
	}
	if (o.IncludeMasterKey || o.IncludeMigrateCred || o.IncludeIdentity) && !o.Encrypt {
		return nil, fmt.Errorf("%w: 主密钥/迁移凭据/节点身份只能放进加密备份（需同时 --encrypt）", ErrUsage)
	}
	if o.IncludeMasterKey && o.Component != "server" || o.IncludeMigrateCred && o.Component != "server" || o.IncludeIdentity && o.Component != "node" {
		return nil, fmt.Errorf("%w: 该 include 选项不适用于组件 %s", ErrUsage, o.Component)
	}
	if !o.DryRun && o.Component == "server" && o.DB == nil {
		return nil, fmt.Errorf("%w: server 备份需要数据库连接", ErrUsage)
	}
	if o.Encrypt && !o.DryRun {
		if err := CheckPassphrase(o.Passphrase); err != nil {
			return nil, err
		}
	}
	host, err := s.hostLabel()
	if err != nil {
		return nil, err
	}
	outDir := o.OutputDir
	if outDir == "" {
		outDir = s.DefaultDir()
	}
	files, warns, err := s.collect(o.Component, o)
	if err != nil {
		return nil, err
	}
	res := &CreateResult{Encrypted: o.Encrypt, KEKIDsRequired: []string{}, Warnings: warns, DryRun: o.DryRun}
	for _, f := range files {
		res.Files = append(res.Files, PlanFile{Dest: f.dest, Kind: f.kind, Size: f.size})
	}
	if o.Component == "server" && o.DB != nil {
		res.Database = o.DB.Name
	}
	if !o.Encrypt {
		res.Warnings = append(res.Warnings, Warning{"backup.unencrypted", "备份未加密：含数据库内容（敏感值为密文）与配置；文件 0600，位于仅 root 可进的目录——请勿与 master.key 存放在一起，传输/离机存放前请加密（--encrypt）"})
	}
	if o.DryRun {
		return res, nil
	}
	// 输出目录：缺失则以 0700 创建；已存在且对 group/other 开放 → 未加密备份拒绝，加密备份只告警。
	if err := security.MkdirAllPerm(outDir, 0o700); err != nil {
		return nil, fmt.Errorf("备份：无法创建输出目录: %w", err)
	}
	if fi, err := os.Stat(outDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("备份：输出路径 %s 不是目录", outDir)
	} else if fi.Mode().Perm()&0o077 != 0 {
		if !o.Encrypt {
			return nil, fmt.Errorf("%w（%s 当前 %04o）", ErrInsecureDir, outDir, fi.Mode().Perm())
		}
		res.Warnings = append(res.Warnings, Warning{"backup.dir_open", fmt.Sprintf("输出目录 %s 对 group/other 开放（%04o）；备份已加密，仍建议收紧为 0700", outDir, fi.Mode().Perm())})
	}

	stage, err := s.newStage("backup-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	m := Manifest{FormatVersion: FormatVersion, Purpose: o.Purpose, Component: o.Component, Version: o.Version, Host: host, CreatedAt: s.now().UTC().Format(time.RFC3339),
		ToolsVersion: s.ToolsVersion, MigrationHistory: []MigrationRec{}, KEKFingerprints: map[string]string{}, KEKIDsRequired: []string{},
		MasterKeyIncluded: o.IncludeMasterKey, MigrateCredentialIncluded: o.IncludeMigrateCred, IdentityIncluded: o.IncludeIdentity, LogsIncluded: o.IncludeLogs, Encrypted: o.Encrypt}
	var staged []StagedFile
	n := 0
	stageFile := func(src, dest, kind string, mode fs.FileMode, owner, group string) error {
		n++
		arch := fmt.Sprintf("files/%06d", n)
		dst := filepath.Join(stage, fmt.Sprintf("%06d", n))
		sum, size, err := copyHashed(src, dst)
		if err != nil {
			return fmt.Errorf("备份：读取 %s 失败: %w", dest, err)
		}
		e := FileEntry{Archive: arch, Dest: dest, Kind: kind, Size: size, SHA256: sum, Mode: uint32(mode), Owner: owner, Group: group}
		m.Files = append(m.Files, e)
		staged = append(staged, StagedFile{Entry: e, Path: dst})
		return nil
	}

	if o.Component == "server" {
		keyPath := s.host(ServerMasterKey)
		ks, err := LoadKeySet(keyPath, s.AllowedUIDs)
		switch {
		case err == nil:
			m.MasterKeyFingerprint, m.KEKFingerprints = ks.Overall, ks.FP
			res.MasterKeyFingerprint = ks.Overall
		case errors.Is(err, ErrMasterKeyMissing) && o.Purpose == PurposePreRestore:
			// 恢复前安全备份：本机没有 master.key（灾后重装前）时不记指纹——这份备份只用于回滚到「无主密钥」的状态，不参与指纹比对。
			res.Warnings = append(res.Warnings, Warning{"backup.no_master_key", "本机没有 master.key：恢复前安全备份不记录主密钥指纹"})
		default:
			return nil, fmt.Errorf("读取主密钥以记录指纹失败（备份必须记录 master_key_fingerprint）: %w", err)
		}
		meta, err := s.dbMeta(ctx, *o.DB, s.host(service.ServerDataDir+"/"+RuntimeCredentialsRel))
		if err != nil {
			return nil, err
		}
		m.SchemaVersion, m.MigrationHistory, m.DeployLineage, m.KEKIDsRequired = meta.schema, meta.history, meta.lineage, meta.keks
		if err := s.failAt("after:db_meta"); err != nil {
			return nil, err
		}
		dump := filepath.Join(stage, "dump.sql")
		if err := s.MySQL.Dump(ctx, *o.DB, dump); err != nil {
			return nil, err
		}
		sum, size, err := hashFile(dump)
		if err != nil {
			return nil, err
		}
		m.Database = &DBInfo{Name: o.DB.Name, Dump: DumpName}
		e := FileEntry{Archive: DumpName, Kind: KindDB, Size: size, SHA256: sum, Mode: 0o600}
		m.Files = append(m.Files, e)
		staged = append(staged, StagedFile{Entry: e, Path: dump})
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := stageFile(f.host, f.dest, f.kind, f.mode, f.owner, f.group); err != nil {
			return nil, err
		}
	}
	// Tools 状态（只读参考，恢复不覆盖）。
	if sp := s.Reg.L.StateFile(); fileExists(sp) {
		if err := stageFile(sp, "", KindToolsState, 0o644, "", ""); err != nil {
			return nil, err
		}
	}
	if err := s.failAt("after:stage"); err != nil {
		return nil, err
	}

	final, partial := "", ""
	for seq := 1; seq < 1000; seq++ {
		cand := filepath.Join(outDir, s.fileName(host, o.Component, o.Encrypt, seq))
		if _, err := os.Lstat(cand); errors.Is(err, os.ErrNotExist) {
			final = cand
			break
		}
	}
	if final == "" {
		return nil, errors.New("备份：无法选出不冲突的文件名")
	}
	partial = filepath.Join(outDir, "."+filepath.Base(final)+".partial-"+randHex(4))
	if err := WriteArchive(partial, m, staged, WriteOptions{Passphrase: o.Passphrase, KDF: s.KDF}); err != nil {
		return nil, err
	}
	if err := os.Rename(partial, final); err != nil {
		_ = os.Remove(partial)
		return nil, err
	}
	// 写完立即回读校验：备份不可读等于没有备份。
	ar, err := ReadArchive(final, o.Passphrase, nil)
	if err != nil {
		_ = os.Remove(final)
		return nil, fmt.Errorf("备份写出后回读校验失败（已删除该备份）: %w", err)
	}
	sum, size, err := hashFile(final)
	if err != nil {
		return nil, err
	}
	res.Path, res.Size, res.SHA256, res.Manifest = final, size, sum, ar.Manifest
	res.KEKIDsRequired, res.DeployLineage, res.SchemaVersion = ar.Manifest.KEKIDsRequired, ar.Manifest.DeployLineage, ar.Manifest.SchemaVersion
	return res, nil
}

func fileExists(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	return hashReader(f)
}

func copyHashed(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = in.Close() }()
	out, err := security.CreateFile(dst, 0o600)
	if err != nil {
		return "", 0, err
	}
	h := newHash()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ---- 列出 / 校验 ----

// ListEntry 是 `backup list` 的一项。
type ListEntry struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	ModTime   string `json:"mtime"`
	Encrypted bool   `json:"encrypted"`
	// Status：ok（结构可读）| invalid（结构损坏，Problem 说明）。
	Status  string `json:"status"`
	Problem string `json:"problem,omitempty"`
	// MetaAuthenticated：加密备份的摘要来自未认证的头部（解密校验前不可当作事实，仅用于展示与保守核对）。
	MetaAuthenticated bool  `json:"meta_authenticated"`
	Meta              *Meta `json:"meta,omitempty"`
}

var reBackupName = regexp.MustCompile(`^lingedge-backup-.+\.tar\.gz(\.enc)?$`)

// List 列出目录里的备份并做结构检查（不读全部内容、不需要口令、不改动任何东西）。dir 为空取缺省目录。目录不存在返回空列表。
func (s *Service) List(dir string) ([]ListEntry, error) {
	if dir == "" {
		dir = s.DefaultDir()
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []ListEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []ListEntry{}
	for _, de := range ents {
		if !reBackupName.MatchString(de.Name()) || !de.Type().IsRegular() {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(dir, de.Name())
		le := ListEntry{Path: p, Name: de.Name(), Size: fi.Size(), ModTime: fi.ModTime().UTC().Format(time.RFC3339), Status: "ok"}
		meta, authenticated, enc, perr := peekMeta(p)
		le.Encrypted, le.Meta, le.MetaAuthenticated = enc, meta, authenticated
		if perr != nil {
			le.Status, le.Problem = "invalid", perr.Error()
		}
		out = append(out, le)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func peekMeta(path string) (*Meta, bool, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, false, err
	}
	defer func() { _ = f.Close() }()
	var hdr [4]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, false, false, errors.New("文件过短")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, false, false, err
	}
	if string(hdr[:]) == encMagic {
		raw, err := PeekMeta(f)
		if err != nil {
			return nil, false, true, err
		}
		m, err := parseMeta(raw)
		return m, false, true, err
	}
	a, err := ReadManifest(path, nil)
	if err != nil {
		return nil, false, false, err
	}
	mt := a.Manifest.Meta()
	return &mt, true, false, nil
}

// VerifyResult 是 `backup verify` 的结果。
type VerifyResult struct {
	Path      string   `json:"path"`
	Encrypted bool     `json:"encrypted"`
	Manifest  Manifest `json:"-"`
	Files     int      `json:"files"`
	Bytes     int64    `json:"bytes"`
}

// Verify 完整校验备份：结构、清单、每个条目的 SHA256，加密备份同时校验认证（解密）。不改动系统。
func (s *Service) Verify(path string, pass []byte) (*VerifyResult, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s 不是普通文件", ErrBadArchive, path)
	}
	ar, err := ReadArchive(path, pass, nil)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, e := range ar.Manifest.Files {
		total += e.Size
	}
	return &VerifyResult{Path: path, Encrypted: ar.Encrypted, Manifest: ar.Manifest, Files: len(ar.Manifest.Files), Bytes: total}, nil
}

// IsEncrypted 报告文件是否是加密备份容器（只读头 4 字节）。
func IsEncrypted(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	var hdr [4]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false, nil
	}
	return string(hdr[:]) == encMagic, nil
}
