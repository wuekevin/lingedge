package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/pkg/version"
)

// BackupEnv 是备份/恢复/卸载的外部依赖装配（Env.Backup）。零值 = 生产：真实 mysqldump/mysql 与 lingedge-server 经 safeexec（各自独立的白名单 Runner），
// 口令由命令行文件/环境变量/终端取得。测试只替换「世界」的接缝（可执行文件路径、server.yaml 路径、故障注入点），不替换备份/恢复的业务逻辑。
type BackupEnv struct {
	// MySQLRunner/ServerRun 执行 mysqldump·mysql 与 lingedge-server；nil = 生产 Runner（backup.NewMySQLRunner / backup.NewServerRunner）。
	MySQLRunner service.Runner
	ServerRun   service.Runner
	// DumpBin/ClientBin 是 mysqldump/mysql 的绝对路径（空 = 裸名，在 safeexec 的固定目录里解析；--mysql-bin-dir 优先）。
	DumpBin, ClientBin string
	// ServerConfig 覆盖 lineage bump 使用的 server.yaml（主机路径）；ServerEnv 是传给 lingedge-server 子进程的额外环境变量。仅测试世界使用。
	ServerConfig string
	ServerEnv    []string
	// KDF 是加密备份的 argon2id 参数（零值取生产默认）；AllowedUIDs 是主密钥/凭据文件允许的属主（nil 取 {euid, 0}）。
	KDF         backup.KDFParams
	AllowedUIDs []uint32
	// Health/HealthTimeout/HealthInterval 覆盖恢复后的健康探测；FailAt 是故障注入点。
	Health                        func(ctx context.Context, url string) error
	HealthTimeout, HealthInterval time.Duration
	FailAt                        func(point string) error
	// ReadSecret 无回显读取一行口令（交互时）；生产为终端读取。
	ReadSecret func(label string) (string, error)
}

// EnvBackupPassFile 是备份口令文件的环境变量通道（没有命令行标志的 recover 路径使用）。
const EnvBackupPassFile = "LINGEDGE_BACKUP_PASSPHRASE_FILE"

// f7Notice 是醒目的主密钥保管提示（F-7；Story 1.41 AC2 的原文）。
const f7Notice = "数据库与私钥为密文，恢复必须有对应 master.key，请与数据库备份分开保管"

func dbFlags(fs *pflag.FlagSet) {
	fs.String("db-host", "", "数据库主机（缺省取 migrate.yaml / server.yaml；口令永远不经命令行）")
	fs.Int("db-port", 0, "数据库端口")
	fs.String("db-socket", "", "MySQL Unix 套接字路径（与 --db-host 互斥）")
	fs.String("db-name", "", "库名")
	fs.String("db-user", "", "迁移账号（缺省 "+backup.DefaultUser+"）")
	fs.String("db-tls", "", "TLS 模式 true|false|skip-verify|preferred")
	fs.String("mysql-bin-dir", "", "mysqldump/mysql 所在目录（绝对路径；缺省在 /usr/bin 等固定目录查找，宝塔常见 /www/server/mysql/bin）")
}

func (r *Run) mysqlTool() (*backup.MySQL, error) {
	be := r.env.Backup
	run := be.MySQLRunner
	if run == nil {
		rr, err := backup.NewMySQLRunner(nil)
		if err != nil {
			return nil, err
		}
		run = rr
	}
	m := &backup.MySQL{Run: run, DumpBin: be.DumpBin, ClientBin: be.ClientBin}
	return m, nil
}

func (r *Run) backupService(inv *invocation) (*backup.Service, *Error) {
	be := r.env.Backup
	ie := r.env.Install
	my, err := r.mysqlTool()
	if err != nil {
		return nil, newErr(ExitGeneral, "general.unexpected", "无法构造 MySQL 执行器："+err.Error(), "")
	}
	if inv != nil && inv.cmd.Flags().Lookup("mysql-bin-dir") != nil && inv.str("mysql-bin-dir") != "" {
		dir := inv.str("mysql-bin-dir")
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return nil, usageErr("usage.invalid_flag", "--mysql-bin-dir 必须是规范的绝对路径", "")
		}
		my.DumpBin, my.ClientBin = filepath.Join(dir, "mysqldump"), filepath.Join(dir, "mysql")
	}
	reg := r.registry()
	my.Scratch = reg.L.StagingDir()
	srun := be.ServerRun
	if srun == nil {
		rr, err := backup.NewServerRunner()
		if err != nil {
			return nil, newErr(ExitGeneral, "general.unexpected", "无法构造 lingedge-server 执行器："+err.Error(), "")
		}
		srun = rr
	}
	runner := ie.Runner
	if runner == nil {
		rr, err := service.SystemRunner()
		if err != nil {
			return nil, newErr(ExitGeneral, "general.unexpected", "无法构造 systemctl 执行器："+err.Error(), "")
		}
		runner = rr
	}
	own := ie.Own
	if own == nil {
		own = service.SystemOwnership()
	}
	return &backup.Service{
		Root: ie.Root, Reg: reg, MySQL: my, ServerRun: srun, ServerEnv: be.ServerEnv, ServerConfig: be.ServerConfig,
		Sys: &service.Systemctl{Run: runner, Booted: ie.Booted}, Own: own,
		Now: r.env.Now, Hostname: r.env.Hostname, Geteuid: r.env.Geteuid, ToolsVersion: version.Info("lingedge"),
		KDF: be.KDF, AllowedUIDs: be.AllowedUIDs, Health: be.Health, HealthTimeout: be.HealthTimeout, HealthInterval: be.HealthInterval, FailAt: be.FailAt,
	}, nil
}

func precond(code, msg, hint string) *Error {
	return &Error{Exit: ExitPrecondition, Code: code, Message: msg, Hint: hint}
}

// passphrase 取得备份口令：--passphrase-file ＞ 环境变量 LINGEDGE_BACKUP_PASSPHRASE_FILE ＞ 交互输入（无回显；创建时输入两次）。
// 非交互且没有文件 → 退出码 3（此时尚未改动任何东西）。口令永远不经命令行参数。
func (r *Run) passphrase(inv *invocation, creating bool) ([]byte, *Error) {
	path := ""
	if inv != nil && inv.cmd.Flags().Lookup("passphrase-file") != nil {
		path = inv.str("passphrase-file")
	}
	if path == "" {
		path = r.env.Getenv(EnvBackupPassFile)
	}
	if path != "" {
		b, err := backup.ReadPassphraseFile(path, r.env.Backup.AllowedUIDs)
		if err != nil {
			return nil, precond("precondition.passphrase_file", "无法使用口令文件："+err.Error(), "口令文件须是绝对路径、属主为 root、权限 0400/0600、非符号链接")
		}
		return b, nil
	}
	if r.interactive() && r.env.Backup.ReadSecret != nil {
		p1, err := r.env.Backup.ReadSecret("备份口令（无回显）: ")
		if err != nil || p1 == "" {
			return nil, precond("precondition.passphrase_required", "未读取到备份口令", "")
		}
		if creating {
			p2, err := r.env.Backup.ReadSecret("再输入一次以确认: ")
			if err != nil || p2 != p1 {
				return nil, precond("precondition.passphrase_mismatch", "两次输入的口令不一致", "")
			}
		}
		return []byte(p1), nil
	}
	return nil, precond("precondition.passphrase_required", "需要备份口令，而当前为非交互模式",
		"用 --passphrase-file <文件>（root 属主 0400/0600）或环境变量 "+EnvBackupPassFile+" 提供口令；口令不能出现在命令行里")
}

// resolveDBRaw 解析数据库连接（迁移账号；来源优先级见 backup.ResolveDB：环境变量 ＞ 口令文件 ＞ migrate.yaml ＞ 交互输入）。interactive=false 时永不提示。返回原始错误。
func (r *Run) resolveDBRaw(inv *invocation, interactive bool) (backup.DBConn, error) {
	var t backup.DBTarget
	if inv != nil && inv.cmd.Flags().Lookup("db-host") != nil {
		port, _ := inv.cmd.Flags().GetInt("db-port")
		t = backup.DBTarget{Host: inv.str("db-host"), Port: port, Socket: inv.str("db-socket"), Name: inv.str("db-name"), User: inv.str("db-user"), TLS: inv.str("db-tls")}
	}
	ce := backup.CredEnv{Getenv: r.env.Getenv, Explicit: t, AllowedUIDs: r.env.Backup.AllowedUIDs, ServerYAML: r.hostPath(backup.ServerConfigFile)}
	if p := r.env.Getenv(backup.EnvMigrateCfg); p != "" {
		ce.MigrateYAML, ce.MigrateYAMLExplicit = p, true
	} else {
		ce.MigrateYAML = r.hostPath(backup.DefaultMigrateYAML)
	}
	if interactive && r.interactive() {
		ce.ReadSecret = r.env.Backup.ReadSecret
	}
	return backup.ResolveDB(ce)
}

// dbConn 解析数据库连接，把失败映射为 CLI 结构化错误。
func (r *Run) dbConn(inv *invocation) (*backup.DBConn, *Error) {
	conn, err := r.resolveDBRaw(inv, true)
	if err != nil {
		switch {
		case errors.Is(err, backup.ErrNoCredentials):
			return nil, precond("precondition.db_credentials", err.Error(),
				"任选其一：环境变量 "+backup.EnvDBPass+" 或 "+backup.EnvPassFile+"；先执行 `sudo lingedge-server migrate credential init`（写入 "+backup.DefaultMigrateYAML+"，root:root 0400）；在终端交互运行。此时尚未改动任何东西")
		case errors.Is(err, backup.ErrNoTarget):
			return nil, precond("precondition.db_target", "缺少数据库连接目标（host/socket 与库名）",
				"用 --db-host/--db-port 或 --db-socket 与 --db-name 指定，或先写好 "+backup.DefaultMigrateYAML+"（migrate credential init）")
		}
		return nil, precond("precondition.db_credentials", "无法确定数据库连接："+err.Error(), "")
	}
	return &conn, nil
}

// installedComponents 读取 state.json 里已装的 server/node。
func (r *Run) installedState() (registry.State, *Error) {
	st, _, err := r.registry().LoadState()
	if err != nil {
		return registry.State{}, RegistryError(err)
	}
	return st, nil
}

func notInstalled(c string) *Error {
	return &Error{Exit: ExitNotInstalled, Code: "not_installed.component", Message: "组件 " + c + " 未安装", Hint: "先安装：lingedge install " + c + "；lingedge list 查看已安装的组件"}
}

func (r *Run) pickComponent(inv *invocation) (string, string, *Error) {
	st, e := r.installedState()
	if e != nil {
		return "", "", e
	}
	if c := inv.str("component"); c != "" {
		rec, ok := st.Components[c]
		if !ok {
			return "", "", notInstalled(c)
		}
		return c, rec.Version, nil
	}
	var have []string
	for _, c := range []string{"server", "node"} {
		if _, ok := st.Components[c]; ok {
			have = append(have, c)
		}
	}
	switch len(have) {
	case 0:
		return "", "", &Error{Exit: ExitNotInstalled, Code: "not_installed.component", Message: "本机没有可备份的组件（server/node 都未安装）", Hint: "lingedge list 查看已安装的组件"}
	case 1:
		return have[0], st.Components[have[0]].Version, nil
	}
	return "", "", usageErr("usage.component_required", "本机同时安装了 server 与 node，请用 --component 指定要备份的组件", "lingedge backup create --component server|node")
}

// mapBackupErr 把 backup 包的失败映射为 CLI 结构化错误。
func mapBackupErr(err error) *Error {
	var re *backup.RestoreError
	if errors.As(err, &re) {
		e := &Error{Message: re.Msg, Hint: re.Hint, Cause: re.Err, Step: 0}
		if re.Safety != "" {
			e.Details = map[string]any{"safety_backup": re.Safety}
		}
		switch re.Kind {
		case backup.KindKeyMismatch:
			e.Exit, e.Code = ExitPrecondition, "precondition.master_key_mismatch"
		case backup.KindSchemaNewer:
			e.Exit, e.Code = ExitPrecondition, "precondition.schema_newer"
		case backup.KindDBNotEmpty:
			e.Exit, e.Code = ExitPrecondition, "precondition.db_not_empty"
		case backup.KindNeedPassphrase:
			e.Exit, e.Code = ExitPrecondition, "precondition.passphrase_required"
		case backup.KindBadPassphrase:
			e.Exit, e.Code = ExitVerifyFailed, "verify.bad_passphrase"
		case backup.KindBadBackup:
			e.Exit, e.Code = ExitVerifyFailed, "verify.backup_invalid"
		case backup.KindPrecondition:
			e.Exit, e.Code = ExitPrecondition, "precondition.restore"
		case backup.KindUsage:
			e.Exit, e.Code = ExitUsage, "usage.restore"
		case backup.KindRolledBack:
			e.Exit, e.Code, e.SystemState = ExitRolledBack, "restore.rolled_back", "rolled_back"
		case backup.KindRollbackFailed:
			e.Exit, e.Code, e.SystemState = ExitRollbackFailed, "restore.rollback_failed", "needs_manual"
		case backup.KindFailedBeforeSafe:
			e.Exit, e.Code = ExitGeneral, "restore.safety_backup_failed"
		default:
			e.Exit, e.Code = ExitGeneral, "restore.failed"
		}
		return e
	}
	e := &Error{Cause: err, Message: "备份操作失败"}
	switch {
	case errors.Is(err, backup.ErrUsage):
		e.Exit, e.Code, e.Message = ExitUsage, "usage.backup", err.Error()
	case errors.Is(err, backup.ErrWeakPassphrase):
		e.Exit, e.Code, e.Message = ExitUsage, "usage.weak_passphrase", err.Error()
	case errors.Is(err, backup.ErrInsecureDir):
		e.Exit, e.Code, e.Message, e.Hint = ExitPrecondition, "precondition.backup_dir", err.Error(), "把 --output 指到仅 root 可进的目录（0700），或加 --encrypt"
	case errors.Is(err, backup.ErrMasterKeyMissing), errors.Is(err, backup.ErrMasterKeyInvalid):
		e.Exit, e.Code, e.Message, e.Hint = ExitPrecondition, "precondition.master_key", "主密钥文件不可用："+err.Error(), "确认 /etc/lingedge/server/master.key 存在、root:root 0400"
	case errors.Is(err, backup.ErrPassphraseRequired):
		e.Exit, e.Code, e.Message, e.Hint = ExitPrecondition, "precondition.passphrase_required", err.Error(), "用 --passphrase-file 提供口令"
	case errors.Is(err, backup.ErrBadPassphrase):
		e.Exit, e.Code, e.Message = ExitVerifyFailed, "verify.bad_passphrase", err.Error()
	case errors.Is(err, backup.ErrBadArchive), errors.Is(err, backup.ErrHashMismatch), errors.Is(err, backup.ErrCorrupt), errors.Is(err, backup.ErrBadManifest):
		e.Exit, e.Code, e.Message = ExitVerifyFailed, "verify.backup_invalid", err.Error()
	case errors.Is(err, safeexec.ErrNotFound), errors.Is(err, safeexec.ErrNotAllowed), errors.Is(err, safeexec.ErrUnsafeExe):
		e.Exit, e.Code, e.Message, e.Hint = ExitPrecondition, "precondition.dependency_missing", "找不到可用的 MySQL 客户端工具（mysqldump/mysql）", "安装 MySQL 客户端，或用 --mysql-bin-dir 指定其目录（须 root 属主、group/other 不可写）"
	case errors.Is(err, fs.ErrPermission):
		e.Exit, e.Code, e.Message, e.Hint = ExitPrecondition, "precondition.permission", "权限不足："+err.Error(), "请用 root 运行（sudo lingedge …）"
	case errors.Is(err, context.Canceled):
		return nil
	default:
		e.Exit, e.Code = ExitGeneral, "backup.failed"
	}
	return e
}

func warnsOf(ws []backup.Warning) []Warning {
	out := make([]Warning, 0, len(ws))
	for _, w := range ws {
		out = append(out, Warning{Code: w.Code, Message: w.Message})
	}
	return out
}

// ---- backup create ----

type backupCreateData struct {
	Component            string            `json:"component"`
	DryRun               bool              `json:"dry_run"`
	Path                 string            `json:"path,omitempty"`
	Size                 int64             `json:"size"`
	SHA256               string            `json:"sha256,omitempty"`
	Encrypted            bool              `json:"encrypted"`
	Database             string            `json:"database,omitempty"`
	MasterKeyFingerprint string            `json:"key_fingerprint,omitempty"` // 键名刻意不含 master_key：信封脱敏按键名替换，指纹本身不是机密
	KEKIDsRequired       []string          `json:"kek_ids_required"`
	DeployLineage        *int64            `json:"deploy_lineage"`
	SchemaVersion        int64             `json:"schema_version"`
	FileCount            int               `json:"file_count"`
	Files                []backup.PlanFile `json:"files"`
	Notice               string            `json:"notice,omitempty"`
}

func runBackupCreate(inv *invocation) (*Result, error) {
	r := inv.r
	comp, ver, e := r.pickComponent(inv)
	if e != nil {
		return nil, e
	}
	svc, e := r.backupService(inv)
	if e != nil {
		return nil, e
	}
	dry := r.opts.DryRun
	opts := backup.CreateOptions{Component: comp, Version: ver, OutputDir: inv.str("output"), Encrypt: inv.flag("encrypt"),
		IncludeLogs: inv.flag("include-logs"), IncludeMasterKey: inv.flag("include-master-key"), IncludeMigrateCred: inv.flag("include-migrate-credential"),
		IncludeIdentity: inv.flag("include-identity"), DryRun: dry}
	if !dry {
		if opts.Encrypt {
			p, e := r.passphrase(inv, true)
			if e != nil {
				return nil, e
			}
			if err := backup.CheckPassphrase(p); err != nil {
				return nil, mapBackupErr(err)
			}
			opts.Passphrase = p
		}
		if comp == "server" {
			conn, e := r.dbConn(inv)
			if e != nil {
				return nil, e
			}
			defer conn.Close()
			opts.DB = conn
		}
	}
	res, err := svc.Create(r.ctx, opts)
	if err != nil {
		return nil, mapBackupErr(err)
	}
	d := backupCreateData{Component: comp, DryRun: dry, Path: res.Path, Size: res.Size, SHA256: res.SHA256, Encrypted: res.Encrypted, Database: res.Database,
		MasterKeyFingerprint: res.MasterKeyFingerprint, KEKIDsRequired: res.KEKIDsRequired, DeployLineage: res.DeployLineage, SchemaVersion: res.SchemaVersion,
		FileCount: len(res.Files), Files: res.Files}
	if d.Files == nil {
		d.Files = []backup.PlanFile{}
	}
	if comp == "server" {
		d.Notice = f7Notice
	}
	return &Result{Data: d, Warnings: warnsOf(res.Warnings), Human: func(w io.Writer) {
		if dry {
			wln(w, "备份计划（--dry-run：未写入任何文件）")
		} else {
			wf(w, "备份已创建：%s\n", res.Path)
			wf(w, "  大小 %d 字节，SHA256 %s，加密：%v\n", res.Size, res.SHA256, res.Encrypted)
		}
		wf(w, "  组件 %s，%d 个文件", comp, len(res.Files))
		if res.Database != "" {
			wf(w, "，数据库 %s", res.Database)
		}
		wln(w)
		if !dry && comp == "server" {
			wf(w, "  主密钥指纹（sha256 前 8 字节）：%s\n  schema 版本 %d", backup.ShortFingerprint(res.MasterKeyFingerprint), res.SchemaVersion)
			if res.DeployLineage != nil {
				wf(w, "，deploy.lineage %d", *res.DeployLineage)
			}
			wf(w, "；所需 kek_id：%s\n", orDash(strings.Join(res.KEKIDsRequired, ",")))
		}
		if comp == "server" {
			wf(w, "\n!! %s !!\n", f7Notice)
		}
	}}, nil
}

// ---- backup list ----

// 输出里的键名避开 master_key/credential 等脱敏词（信封按键名把值替换成 ***；指纹与「是否含主密钥」不是机密）。
type listMeta struct {
	FormatVersion  int      `json:"format_version"`
	Purpose        string   `json:"purpose"`
	Component      string   `json:"component"`
	Version        string   `json:"version"`
	Host           string   `json:"host"`
	CreatedAt      string   `json:"created_at"`
	SchemaVersion  int64    `json:"schema_version"`
	DeployLineage  *int64   `json:"deploy_lineage"`
	KeyFingerprint string   `json:"key_fingerprint"`
	KEKIDsRequired []string `json:"kek_ids_required"`
	KeyIncluded    bool     `json:"key_included"`
}

type listEntry struct {
	Path              string    `json:"path"`
	Name              string    `json:"name"`
	Size              int64     `json:"size"`
	ModTime           string    `json:"mtime"`
	Encrypted         bool      `json:"encrypted"`
	Status            string    `json:"status"`
	Problem           string    `json:"problem,omitempty"`
	MetaAuthenticated bool      `json:"meta_authenticated"`
	Meta              *listMeta `json:"meta,omitempty"`
}

type backupListData struct {
	Dir     string      `json:"dir"`
	Backups []listEntry `json:"backups"`
}

func toListEntries(in []backup.ListEntry) []listEntry {
	out := make([]listEntry, 0, len(in))
	for _, b := range in {
		le := listEntry{Path: b.Path, Name: b.Name, Size: b.Size, ModTime: b.ModTime, Encrypted: b.Encrypted, Status: b.Status, Problem: b.Problem, MetaAuthenticated: b.MetaAuthenticated}
		if m := b.Meta; m != nil {
			ids := m.KEKIDsRequired
			if ids == nil {
				ids = []string{}
			}
			le.Meta = &listMeta{FormatVersion: m.FormatVersion, Purpose: m.Purpose, Component: m.Component, Version: m.Version, Host: m.Host, CreatedAt: m.CreatedAt,
				SchemaVersion: m.SchemaVersion, DeployLineage: m.DeployLineage, KeyFingerprint: m.MasterKeyFingerprint, KEKIDsRequired: ids, KeyIncluded: m.MasterKeyIncluded}
		}
		out = append(out, le)
	}
	return out
}

func runBackupList(inv *invocation) (*Result, error) {
	r := inv.r
	svc, e := r.backupService(inv)
	if e != nil {
		return nil, e
	}
	dir := inv.str("dir")
	shown := dir
	if shown == "" {
		shown = svc.DefaultDir()
	}
	list, err := svc.List(dir)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, precond("precondition.root_required", "无权读取备份目录 "+shown, "请用 root 运行：sudo lingedge backup list")
		}
		return nil, mapBackupErr(err)
	}
	return &Result{Data: backupListData{Dir: shown, Backups: toListEntries(list)}, Human: func(w io.Writer) {
		if len(list) == 0 {
			wf(w, "%s 里没有备份\n", shown)
			return
		}
		wf(w, "备份目录 %s\n", shown)
		for _, b := range list {
			wf(w, "\n%s  （%d 字节，%s，加密：%v）\n", b.Name, b.Size, b.ModTime, b.Encrypted)
			if b.Status != "ok" {
				wf(w, "  状态：损坏 —— %s\n", redactText(b.Problem))
				continue
			}
			if m := b.Meta; m != nil {
				auth := ""
				if !b.MetaAuthenticated {
					auth = "（头部摘要，解密校验前未认证）"
				}
				wf(w, "  %s %s，主机 %s，创建于 %s%s\n", m.Component, m.Version, m.Host, m.CreatedAt, auth)
				if m.Component == "server" {
					wf(w, "  所需 kek_id：%s；主密钥指纹 %s；schema %d", orDash(strings.Join(m.KEKIDsRequired, ",")), backup.ShortFingerprint(m.MasterKeyFingerprint), m.SchemaVersion)
					if m.DeployLineage != nil {
						wf(w, "；deploy.lineage %d", *m.DeployLineage)
					}
					wln(w)
				}
			}
		}
		wf(w, "\n结构检查只看头部与清单；完整校验（哈希/解密）请运行 lingedge backup verify <文件>\n")
	}}, nil
}

// ---- backup verify ----

type backupVerifyData struct {
	Path                 string   `json:"path"`
	OK                   bool     `json:"ok"`
	Encrypted            bool     `json:"encrypted"`
	Component            string   `json:"component"`
	Version              string   `json:"version"`
	Host                 string   `json:"host"`
	CreatedAt            string   `json:"created_at"`
	Purpose              string   `json:"purpose"`
	Files                int      `json:"files"`
	Bytes                int64    `json:"bytes"`
	SchemaVersion        int64    `json:"schema_version"`
	DeployLineage        *int64   `json:"deploy_lineage"`
	MasterKeyFingerprint string   `json:"key_fingerprint,omitempty"`
	KEKIDsRequired       []string `json:"kek_ids_required"`
	MasterKeyIncluded    bool     `json:"key_included"`
}

func runBackupVerify(inv *invocation) (*Result, error) {
	r := inv.r
	svc, e := r.backupService(inv)
	if e != nil {
		return nil, e
	}
	path := inv.args[0]
	enc, err := backup.IsEncrypted(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, usageErr("usage.file_not_found", "找不到备份文件 "+path, "")
		}
		return nil, mapBackupErr(err)
	}
	var pass []byte
	if enc {
		if pass, e = r.passphrase(inv, false); e != nil {
			return nil, e
		}
	}
	vr, err := svc.Verify(path, pass)
	if err != nil {
		return nil, mapBackupErr(err)
	}
	m := vr.Manifest
	d := backupVerifyData{Path: path, OK: true, Encrypted: vr.Encrypted, Component: m.Component, Version: m.Version, Host: m.Host, CreatedAt: m.CreatedAt, Purpose: m.Purpose,
		Files: vr.Files, Bytes: vr.Bytes, SchemaVersion: m.SchemaVersion, DeployLineage: m.DeployLineage, MasterKeyFingerprint: m.MasterKeyFingerprint,
		KEKIDsRequired: append([]string{}, m.KEKIDsRequired...), MasterKeyIncluded: m.MasterKeyIncluded}
	return &Result{Data: d, Human: func(w io.Writer) {
		wf(w, "备份校验通过：%s\n  %s %s（主机 %s，创建于 %s），%d 个文件，%d 字节，加密：%v\n", path, m.Component, m.Version, m.Host, m.CreatedAt, vr.Files, vr.Bytes, vr.Encrypted)
		if m.Component == "server" {
			wf(w, "  主密钥指纹 %s；所需 kek_id %s；schema %d\n", backup.ShortFingerprint(m.MasterKeyFingerprint), orDash(strings.Join(m.KEKIDsRequired, ",")), m.SchemaVersion)
		}
		wln(w, "  （只读校验，未改动系统）")
	}}, nil
}

// ---- backup restore ----

type backupRestoreData struct {
	*backup.RestoreResult
	Tx     string `json:"tx,omitempty"`
	DryRun bool   `json:"dry_run"`
}

func runBackupRestore(inv *invocation) (*Result, error) {
	r := inv.r
	path := inv.args[0]
	svc, e := r.backupService(inv)
	if e != nil {
		return nil, e
	}
	enc, err := backup.IsEncrypted(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, usageErr("usage.file_not_found", "找不到备份文件 "+path, "")
		}
		return nil, mapBackupErr(err)
	}
	var pass []byte
	if enc {
		if pass, e = r.passphrase(inv, false); e != nil {
			return nil, e
		}
	}
	// 先只读取清单，确定组件并确认它已安装（二进制由 install 负责，restore 只恢复配置与数据）。
	ar, err := backup.ReadManifest(path, pass)
	if err != nil {
		return nil, mapBackupErr(err)
	}
	comp := ar.Manifest.Component
	if c := inv.str("component"); c != "" && c != comp {
		return nil, usageErr("usage.restore", fmt.Sprintf("备份的组件是 %s，与 --component %s 不一致", comp, c), "")
	}
	st, e := r.installedState()
	if e != nil {
		return nil, e
	}
	rec, ok := st.Components[comp]
	if !ok {
		return nil, notInstalled(comp)
	}
	o := backup.RestoreOptions{Component: comp, Archive: path, Passphrase: pass, Replace: inv.flag("replace"), InstalledVersion: rec.Version,
		MasterKeyFile: inv.str("master-key-file"), UseBackupMasterKey: inv.flag("use-backup-master-key"), DryRun: r.opts.DryRun}
	if enc {
		o.SafetyPassphrase = pass // 恢复前安全备份与被恢复的备份使用同一口令
	}
	if comp == "server" {
		conn, e := r.dbConn(inv)
		if e != nil {
			return nil, e
		}
		defer conn.Close()
		o.DB = conn
	}
	if !r.opts.DryRun {
		if inv.task == nil {
			return nil, newErr(ExitGeneral, "general.unexpected", "restore 没有取得任务锁", "")
		}
		o.TxID = inv.task.TxID()
		o.OpenJournal = func() (backup.Journal, error) { return inv.task.OpenTx(r.ctx) }
	}
	res, err := svc.Restore(r.ctx, o)
	if err != nil {
		return nil, mapBackupErr(err)
	}
	d := backupRestoreData{RestoreResult: res, Tx: o.TxID, DryRun: r.opts.DryRun}
	return &Result{Data: d, Warnings: warnsOf(res.Warnings), Human: func(w io.Writer) {
		if r.opts.DryRun {
			wln(w, "恢复计划（--dry-run：备份已完整校验，其余只读检查已通过；未改动任何东西）")
		} else {
			wln(w, "恢复完成")
		}
		wf(w, "  备份 %s %s（主机 %s，创建于 %s），主密钥核对：%s\n", res.Component, res.BackupVersion, res.BackupHost, res.BackupCreated, res.KeyCheck)
		if res.Component == "server" {
			wf(w, "  schema：备份 %d / 当前二进制 %d；目标库原有对象 %d 个（--replace=%v）\n", res.SchemaBackup, res.SchemaBinary, res.DBObjects, res.Replace)
		}
		if !r.opts.DryRun {
			wf(w, "  恢复前安全备份：%s（保留；确认无误后可自行删除）\n", res.Safety)
			wf(w, "  已恢复 %d 个文件，保留本机 %d 个\n", len(res.FilesRestored), len(res.FilesKept))
			if res.Component == "server" {
				if res.Probe.Skipped {
					wf(w, "  探针解密：跳过（%s）\n", res.Probe.Reason)
				} else {
					wf(w, "  探针解密通过：%s\n", strings.Join(res.Probe.Probed, "、"))
				}
				wf(w, "  deploy.lineage：%d -> %d（节点仍可接收新修订）\n", res.LineageBefore, res.LineageAfter)
			}
			wf(w, "  健康检查：%s\n", res.Health)
			if res.MasterKeySaved != "" {
				wf(w, "\n!! 本机原 master.key 已保留为 %s（0400）。它可能仍是新近备份的密钥，请核对后与数据库备份分开保管；Tools 不会删除任何主密钥 !!\n", res.MasterKeySaved)
			}
		}
	}}, nil
}

// restoreSafety 实现 installer.Deps.RestoreSafety：`recover --rollback` 对崩溃的恢复事务执行「回到恢复前安全备份」。
// 数据库凭据与（安全备份加密时的）口令按与 restore 相同的来源取得（环境变量/migrate.yaml/server.yaml/交互）；取不到则失败并给出手工指引——绝不假装成功。
func (r *Run) restoreSafety(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
	svc, e := r.backupService(nil)
	if e != nil {
		return service.UndoResult{}, e
	}
	var pass []byte
	if backup.SafetyEncrypted(a) {
		p, pe := r.passphrase(nil, false)
		if pe != nil {
			return service.UndoResult{}, fmt.Errorf("恢复前安全备份是加密的，需要口令才能回滚（%s）；请设置环境变量 %s 后重试 recover --rollback", pe.Message, EnvBackupPassFile)
		}
		pass = p
	}
	var db *backup.DBConn
	if a.Extra[backup.ExtraComponent] == "server" {
		c, ce := r.dbConn(nil)
		if ce != nil {
			return service.UndoResult{}, fmt.Errorf("回滚恢复需要数据库迁移账号凭据：%s（%s）", ce.Message, ce.Hint)
		}
		defer c.Close()
		db = c
	}
	p, err := backup.ParseRollback(a, pass, db)
	if err != nil {
		return service.UndoResult{}, err
	}
	if err := svc.RollbackSafety(ctx, p); err != nil {
		return service.UndoResult{}, err
	}
	return service.UndoResult{Outcome: service.OutcomeDone, Detail: "已回滚到恢复前安全备份 " + filepath.Base(a.Path)}, nil
}
