package rollback

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/dbmigrate"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
)

const optRoot = "/opt/lingedge"

// Executor 执行回滚路径的动作。字段是「世界」的接缝（根目录前缀、二进制执行器、凭据来源）；逻辑永远是生产代码。
type Executor struct {
	// Root 非空时 /opt 路径前缀到该目录（测试用；生产为空）。
	Root string
	// Backup 用于「恢复升级前备份」；Mig 用于 migrate status/down。仅 server 的数据库路径需要。
	Backup *backup.Service
	Mig    *dbmigrate.Migrator
	// DB 取得迁移账号连接（来源优先级见 backup.ResolveDB；非交互且无凭据时返回错误——此时调用方须明确失败，绝不假装成功）。调用方负责 Close 返回的连接？
	// 不：Executor 在用完后 Close。
	DB func(ctx context.Context) (*backup.DBConn, error)
	// Passphrase 取得加密备份的口令（仅备份加密时调用）。
	Passphrase func(ctx context.Context) ([]byte, error)
	// Now 用于临时文件名唯一性（可为 nil）。
	Now func() time.Time
}

func (e *Executor) host(virtual string) string {
	if e.Root == "" {
		return virtual
	}
	return filepath.Join(e.Root, virtual)
}

// ReleasesDir/CurrentLink/ReleaseDir 返回（虚拟）路径。
func ReleasesDir(comp string) string { return optRoot + "/" + comp + "/releases" }
func CurrentLink(comp string) string { return optRoot + "/" + comp + "/current" }
func ReleaseDir(comp, version string) string {
	return ReleasesDir(comp) + "/" + version
}

// CurrentTarget 返回 current 链接的目标（相对形式 releases/<v>）；链接不存在返回 ""；不是符号链接是错误。
func (e *Executor) CurrentTarget(comp string) (string, error) {
	p := e.host(CurrentLink(comp))
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return "", fmt.Errorf("%s 不是符号链接，Tools 不会替换它", CurrentLink(comp))
	}
	return os.Readlink(p)
}

// CurrentVersion 返回 current 指向的版本号（releases/<v> 的 <v>）；无法识别返回 ""。
func (e *Executor) CurrentVersion(comp string) (string, error) {
	t, err := e.CurrentTarget(comp)
	if err != nil || t == "" {
		return "", err
	}
	if !strings.HasPrefix(t, "releases/") || strings.Contains(t[len("releases/"):], "/") {
		return "", nil
	}
	return strings.TrimPrefix(t, "releases/"), nil
}

// SwitchCurrent 原子地把 current 指向 releases/<version>（先建临时符号链接再 rename 覆盖：任何时刻读到的都是旧目标或新目标，绝不会是缺失/半状态）。
// 目标 release 必须是已存在的真实目录；返回切换前的目标（""=原先没有）。已经指向目标则幂等返回。
func (e *Executor) SwitchCurrent(comp, version string) (prev string, err error) {
	if !registry.ValidComponent(comp) {
		return "", fmt.Errorf("组件名不合法: %q", comp)
	}
	if _, perr := manifest.ParseVersion(version); perr != nil {
		return "", fmt.Errorf("版本号不是严格 SemVer: %q", version)
	}
	rel := e.host(ReleaseDir(comp, version))
	fi, err := os.Lstat(rel)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("release 目录 %s 不存在或不是真实目录，无法切换", ReleaseDir(comp, version))
	}
	prev, err = e.CurrentTarget(comp)
	if err != nil {
		return "", err
	}
	want := "releases/" + version
	if prev == want {
		return prev, nil
	}
	link := e.host(CurrentLink(comp))
	tmp := fmt.Sprintf("%s.new-%d", link, os.Getpid())
	_ = os.Remove(tmp)
	if err := os.Symlink(want, tmp); err != nil {
		return prev, err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return prev, err
	}
	if d, derr := os.Open(filepath.Dir(link)); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return prev, nil
}

// RevertParams 是撤销升级的数据库部分所需的事实（也是 journal 里 update_db_revert 动作的内容；不含口令）。
type RevertParams struct {
	Component string
	// Path 是 registry.Path*。
	Path string
	// NewBin 是**新**release 的 lingedge-server（虚拟绝对路径）：migrate down/status 必须由认识这些迁移的二进制执行。
	NewBin string
	// PreSchema/TargetSchema：升级前 / 升级后（迁移全部完成时）的 schema 版本。
	PreSchema, TargetSchema int64
	Backup                  string
	BackupEncrypted         bool
	// AllowRestore 允许在需要时执行「恢复备份」：升级失败的自动回滚（journal 撤销）为 true；手动 `rollback` 不带 --from-backup 时为 false——
	// 此时数据库回退失败/状态不符会明确失败，绝不静默升级成会丢数据的恢复。
	AllowRestore bool
	// Force：即使数据库已经在 PreSchema 也执行恢复（手动 `rollback --from-backup`：用户明确要求用备份覆盖当前数据库）。
	Force bool
}

// RevertOutcome 描述撤销做了什么。
type RevertOutcome struct {
	// Action：none | migrate_down | restore_backup。
	Action string
	Detail string
	// SchemaAfter 是撤销之后数据库的 schema 版本（server 且有数据库时）。
	SchemaAfter int64
	Lineage     [2]int64
}

// RevertDB 把数据库恢复到升级前的状态，路径由迁移级别决定（§17.2），并按**数据库现状**自适应——撤销可能发生在 migrate up 之前/之中/之后的任意时刻（journal 回滚、recover --rollback 重试都会如此）：
//   - 库仍在升级前的 schema（且无 dirty）→ 什么都不用做；
//   - migrate_down 路径：down 到升级前 schema；down 失败（含 dirty）→ 升级为「恢复备份」（AC3）；
//   - binary_expand 路径：库已完整升到目标 schema 且无 dirty → 只切回旧二进制即可（旧二进制与新增结构兼容，不执行 down）；库处于中间状态/dirty → 恢复备份；
//   - restore_backup 路径：恢复备份。
//
// 恢复备份时 current 必须已指回旧 release（lineage bump 在启动旧版本 Server 之前由旧二进制执行）。幂等。
func (e *Executor) RevertDB(ctx context.Context, p RevertParams) (RevertOutcome, error) {
	out := RevertOutcome{Action: "none"}
	if p.Component != "server" || p.Path == registry.PathBinary || p.Path == "" {
		return out, nil
	}
	if e.DB == nil || e.Mig == nil {
		return out, errors.New("未装配数据库凭据通道或 migrate 执行器，无法回退数据库")
	}
	conn, err := e.DB(ctx)
	if err != nil {
		return out, fmt.Errorf("回退数据库需要迁移账号凭据：%w", err)
	}
	defer conn.Close()
	newBin := e.host(p.NewBin)
	st, serr := e.Mig.Status(ctx, newBin, conn)
	clean := serr == nil && st != nil && st.Problem == ""
	if !p.Force && clean && st.DBVersion == p.PreSchema {
		out.Detail, out.SchemaAfter = "数据库仍在升级前的 schema，无需回退", st.DBVersion
		return out, nil
	}
	switch p.Path {
	case registry.PathMigrateDown:
		if clean {
			if _, derr := e.Mig.Down(ctx, newBin, conn, p.PreSchema); derr == nil {
				if after, aerr := e.Mig.Status(ctx, newBin, conn); aerr == nil && after.Problem == "" && after.DBVersion == p.PreSchema {
					return RevertOutcome{Action: "migrate_down", Detail: fmt.Sprintf("migrate down 到升级前的 schema %d", p.PreSchema), SchemaAfter: after.DBVersion}, nil
				}
			}
		}
		// down 失败或回退后版本不符：升级为恢复备份。
	case registry.PathBinaryExpand:
		if clean && p.TargetSchema > 0 && st.DBVersion == p.TargetSchema {
			out.Detail, out.SchemaAfter = "expand 迁移与旧二进制兼容：只切回旧二进制，不执行 down", st.DBVersion
			return out, nil
		}
	case registry.PathRestoreBackup:
	default:
		return out, fmt.Errorf("未知的回滚路径 %q", p.Path)
	}
	return e.restore(ctx, p, conn, newBin)
}

func (e *Executor) restore(ctx context.Context, p RevertParams, conn *backup.DBConn, newBin string) (RevertOutcome, error) {
	out := RevertOutcome{Action: "restore_backup"}
	if !p.AllowRestore {
		return out, errors.New("数据库无法按该路径回退，需要恢复备份，但本次操作不允许自动改走「恢复备份」（会丢失备份点之后的数据）；请改用 lingedge rollback --from-backup <备份文件>")
	}
	if p.Backup == "" {
		return out, errors.New("需要恢复备份，但没有升级前备份可用")
	}
	if e.Backup == nil {
		return out, errors.New("未装配备份服务，无法恢复备份")
	}
	var pass []byte
	if p.BackupEncrypted {
		if e.Passphrase == nil {
			return out, errors.New("升级前备份是加密的，需要口令才能恢复（设置环境变量 LINGEDGE_BACKUP_PASSPHRASE_FILE 指向口令文件后重试 recover --rollback）")
		}
		var err error
		if pass, err = e.Passphrase(ctx); err != nil {
			return out, fmt.Errorf("升级前备份是加密的，无法取得口令：%w", err)
		}
	}
	res, err := e.Backup.ApplyBackup(ctx, backup.ApplyOptions{Component: p.Component, Archive: p.Backup, Passphrase: pass, DB: conn})
	if err != nil {
		return out, err
	}
	out.Lineage = [2]int64{res.LineageBefore, res.LineageAfter}
	out.Detail = fmt.Sprintf("已恢复升级前备份 %s（数据库 schema %d；deploy.lineage %d → %d）", filepath.Base(p.Backup), res.SchemaBackup, res.LineageBefore, res.LineageAfter)
	after, aerr := e.Mig.Status(ctx, newBin, conn)
	if aerr != nil || after.Problem != "" {
		return out, fmt.Errorf("恢复备份后数据库迁移状态异常（%v）", aerr)
	}
	if after.DBVersion != res.SchemaBackup {
		return out, fmt.Errorf("恢复备份后数据库 schema 版本为 %d，与备份记录的 %d 不一致", after.DBVersion, res.SchemaBackup)
	}
	out.SchemaAfter = after.DBVersion
	return out, nil
}

// PruneReleases 清理超出保留数的旧 release：保留语义版本最新的 keep 个（keep 不足 2 按 2），并且永远保留 current 的目标与 protect 里列出的版本。
// 只处理名字是严格 SemVer 的真实目录（.partial 与其他杂物不动）。返回被删除的版本。必须在升级**确认成功之后**才调用（AC7）。
func (e *Executor) PruneReleases(comp string, keep int, protect ...string) ([]string, error) {
	if keep < 2 {
		keep = 2
	}
	dir := e.host(ReleasesDir(comp))
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type rel struct {
		name string
		v    manifest.Version
	}
	var rels []rel
	for _, en := range ents {
		if !en.IsDir() {
			continue
		}
		v, perr := manifest.ParseVersion(en.Name())
		if perr != nil || v.String() != en.Name() {
			continue
		}
		rels = append(rels, rel{en.Name(), v})
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i].v.Compare(rels[j].v) > 0 })
	protected := map[string]bool{}
	for _, p := range protect {
		protected[p] = true
	}
	if cur, cerr := e.CurrentVersion(comp); cerr == nil && cur != "" {
		protected[cur] = true
	}
	var removed []string
	for i, r := range rels {
		if i < keep || protected[r.name] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, r.name)); err != nil {
			return removed, err
		}
		removed = append(removed, r.name)
	}
	return removed, nil
}

// SchemaVersion 经 mysql 客户端读取库已应用的最大迁移版本（回滚后的 schema 复核；不依赖任何一个 lingedge-server 二进制认识全部迁移）。
func (e *Executor) SchemaVersion(ctx context.Context, conn *backup.DBConn) (int64, error) {
	if e.Backup == nil || e.Backup.MySQL == nil || conn == nil {
		return 0, errors.New("未装配 mysql 客户端或数据库连接")
	}
	return e.Backup.MySQL.SchemaVersion(ctx, *conn)
}
