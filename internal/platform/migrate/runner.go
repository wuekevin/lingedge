// Package migrate 是 LingEdge 的数据库迁移框架（Story 1.6、ADR-016、ADR-013）。
//
// 组成：
//   - source.go / lint.go / ddlcheck.go：迁移文件解析、分类与结构 lint、DDL 预检（纯静态，不连库）；
//   - plan.go：`migrate plan` 的 JSON 计划（含 has_irreversible，供 Tools 升级前展示）；
//   - runner.go / lock.go：以 goose v3 为库、经 migrate 账号执行 up / down / status / recover：
//     版本门槛（MySQL >= 8.0.16）、账号预检、迁移互斥（GET_LOCK + job_locks 租约）、`schema_migrations` 自定义版本表、
//     逐迁移 dirty 标记与校验和、reversible 之外拒绝回退、irreversible 须显式确认；
//   - cli.go：`lingedge-server migrate plan|up|down|status|recover|lint` 子命令。
//
// 边界：goose 与 database/sql 只出现在本包（depguard）；本包是 db.OpenMigrate 的白名单调用方之一。
// 迁移文件在 /migrations（嵌入为 migrations.FS）；已发布迁移只追加不得修改（CHECKSUMS，Story 1.3）。
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pressly/goose/v3"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

const (
	// VersionTable 是 goose 的版本表（自定义表名，ADR-016 §4）。
	VersionTable = "schema_migrations"
	// RunsTable 记录每次迁移的执行状态与校验和（dirty 判定、篡改检测）。
	RunsTable = "schema_migrations_runs"
	// DefaultLockTimeout 是等待迁移互斥的缺省时长。
	DefaultLockTimeout = 30 * time.Second
	// MinMySQL 是最低支持的 MySQL 版本（CHECK 自 8.0.16 起强制）。
	MinMySQL = "8.0.16"
)

const (
	statusRunning = "running"
	statusFailed  = "failed"
	statusApplied = "applied"
)

const createRunsSQL = "CREATE TABLE IF NOT EXISTS " + RunsTable + " (" +
	"version BIGINT UNSIGNED NOT NULL PRIMARY KEY, " +
	"name VARCHAR(128) NOT NULL, " +
	"class VARCHAR(16) NOT NULL, " +
	"checksum CHAR(64) NOT NULL, " +
	"direction VARCHAR(4) NOT NULL, " +
	"status VARCHAR(16) NOT NULL, " +
	"error TEXT NULL, " +
	"started_at DATETIME(3) NOT NULL, " +
	"finished_at DATETIME(3) NULL, " +
	"CONSTRAINT ck_schema_migrations_runs_status CHECK (status IN ('running','failed','applied')), " +
	"CONSTRAINT ck_schema_migrations_runs_direction CHECK (direction IN ('up','down'))" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci"

// Runner 在一个 migrate 池上执行迁移。零值不可用，用 NewRunner 创建。
type Runner struct {
	pool *db.MigratePool
	fsys fs.FS
	set  *Set
	lint []Finding

	// Clock 提供时间（默认系统时钟）；LockTimeout 为等待迁移互斥的时长（默认 DefaultLockTimeout）。
	Clock       clock.Clock
	LockTimeout time.Duration
	// HeartbeatInterval 是持锁连接的心跳/租约续约间隔（缺省 DefaultHeartbeatInterval）。
	HeartbeatInterval time.Duration
	// Logf 接收进度日志（默认丢弃）。不输出迁移的 SQL 文本；驱动错误信息可能含冲突的键值，迁移里本就不得含口令（G-6 扫描）。
	Logf func(format string, args ...any)
}

// NewRunner 解析并 lint fsys 中的迁移。lint 发现不阻止创建（plan/status 仍可用），但 Up/Down/Recover 会 fail-closed。
func NewRunner(pool *db.MigratePool, fsys fs.FS) (*Runner, error) {
	if pool == nil {
		return nil, errors.New("migrate: 缺少 migrate 连接池")
	}
	set, findings, err := Load(fsys)
	if err != nil {
		return nil, err
	}
	return &Runner{pool: pool, fsys: fsys, set: set, lint: findings, Clock: clock.System(), LockTimeout: DefaultLockTimeout}, nil
}

// Set 返回已解析的迁移集合。
func (r *Runner) Set() *Set { return r.set }

// Findings 返回 lint 发现。
func (r *Runner) Findings() []Finding { return r.lint }

func (r *Runner) logf(format string, a ...any) {
	if r.Logf != nil {
		r.Logf(format, a...)
	}
}

func (r *Runner) sqlDB() *sql.DB { return r.pool.SQLDB() }

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (r *Runner) failIfLint() error {
	if len(r.lint) > 0 {
		return &ErrLint{Findings: r.lint}
	}
	return nil
}

// ---- 版本门槛与预检 ----

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)

// CheckMySQLVersion 校验 SELECT VERSION() 的返回值：MySQL >= 8.0.16（MariaDB 与更低版本拒绝）。
func CheckMySQLVersion(v string) error {
	if strings.Contains(strings.ToLower(v), "mariadb") {
		return &ErrUnsupportedVersion{Version: v, Reason: "MariaDB 的 CHECK/生成列/外键语义与 MySQL 不同，不受支持"}
	}
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return &ErrUnsupportedVersion{Version: v, Reason: "无法识别的版本字符串"}
	}
	var n [3]int
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	if n[0] < 8 || (n[0] == 8 && n[1] == 0 && n[2] < 16) {
		return &ErrUnsupportedVersion{Version: v, Reason: "低于 " + MinMySQL}
	}
	return nil
}

// CheckVersion 读取并校验服务端版本，返回版本字符串。
func (r *Runner) CheckVersion(ctx context.Context) (string, error) {
	var v string
	if err := r.sqlDB().QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		return "", fmt.Errorf("migrate: 读取数据库版本: %w", err)
	}
	return v, CheckMySQLVersion(v)
}

// Preflight 在迁移开始之前探测迁移账号的两项必需能力，把「迁移到一半才失败」变成「迁移前拒绝」：
//   - GRANT OPTION（ADR-016 §6）：迁移内要执行逐表授权，账号需要 `ALL ON db.* WITH GRANT OPTION`；用探针表向自己授权来验证；
//   - 建触发器（AC6 ⑤）：binlog=ON 且 log_bin_trust_function_creators=0 时，需要 SUPER 或全局 SET_USER_ID，否则 ERROR 1419；
//     用探针触发器验证。
//
// 探针表/触发器/授权在探测后立即清理。
func (r *Runner) Preflight(ctx context.Context) error {
	var logBin, trust int
	if err := r.sqlDB().QueryRowContext(ctx, "SELECT @@log_bin, @@log_bin_trust_function_creators").Scan(&logBin, &trust); err != nil {
		return fmt.Errorf("migrate: 读取 binlog 设置: %w", err)
	}
	conn, err := r.sqlDB().Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: 获取连接: %w", err)
	}
	defer func() { _ = conn.Close() }()
	var suffix string
	if err := conn.QueryRowContext(ctx, "SELECT LOWER(HEX(RANDOM_BYTES(4)))").Scan(&suffix); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	tbl := "_lingedge_probe_" + suffix
	cleanup := func() {
		c := context.WithoutCancel(ctx)
		_, _ = conn.ExecContext(c, "DROP TRIGGER IF EXISTS `"+tbl+"_t`")
		_, _ = conn.ExecContext(c, "REVOKE SELECT ON `"+tbl+"` FROM CURRENT_USER()")
		_, _ = conn.ExecContext(c, "DROP TABLE IF EXISTS `"+tbl+"`")
	}
	defer cleanup()
	if _, err := conn.ExecContext(ctx, "CREATE TABLE `"+tbl+"` (id INT NOT NULL PRIMARY KEY) ENGINE=InnoDB"); err != nil {
		return &ErrPreflight{Reason: "迁移账号无法建表（需要库级 CREATE 权限）：" + err.Error(), Err: err}
	}
	if _, err := conn.ExecContext(ctx, "GRANT SELECT ON `"+tbl+"` TO CURRENT_USER()"); err != nil {
		return &ErrPreflight{Err: err, Reason: "迁移账号没有 GRANT OPTION，无法在迁移内执行逐表授权（" + err.Error() + "）。" +
			"迁移账号须为 `GRANT ALL ON <库>.* TO <迁移账号> WITH GRANT OPTION`（ADR-016 §6、database/README §5.1）"}
	}
	if logBin == 0 || trust == 1 {
		return nil // 建触发器不需要额外权限
	}
	if _, err := conn.ExecContext(ctx, "CREATE TRIGGER `"+tbl+"_t` BEFORE INSERT ON `"+tbl+"` FOR EACH ROW SET NEW.id = NEW.id"); err != nil {
		if n, ok := db.MySQLErrorNumber(err); ok && n == 1419 {
			return &ErrPreflight{Err: err, Reason: "服务器开启了 binlog（log_bin=ON）且 log_bin_trust_function_creators=0，迁移账号没有 SUPER 或全局 SET_USER_ID 权限，无法创建触发器（ERROR 1419）。" +
				"处置二选一：① 给迁移账号授予 `GRANT SET_USER_ID ON *.* TO <迁移账号>`；② 在 my.cnf 设置 log_bin_trust_function_creators=1 并重启 MySQL（database/README §5.1）"}
		}
		return &ErrPreflight{Reason: "迁移账号无法创建触发器：" + err.Error(), Err: err}
	}
	return nil
}

// ---- 读取库内状态 ----

type runRow struct {
	Version    int64
	Name       string
	Class      string
	Checksum   string
	Direction  string
	Status     string
	Error      sql.NullString
	StartedAt  time.Time
	FinishedAt sql.NullTime
}

type dbState struct {
	applied map[int64]time.Time // goose 版本表里已应用（>0）的序号
	max     int64
	runs    map[int64]*runRow
}

func tableExists(ctx context.Context, q querier, name string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?", name).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *Runner) readState(ctx context.Context, q querier) (*dbState, error) {
	st := &dbState{applied: map[int64]time.Time{}, runs: map[int64]*runRow{}}
	ok, err := tableExists(ctx, q, VersionTable)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if ok {
		// goose 版本表的结构（version_id、is_applied、tstamp）是其公开约定；这里只读，不经 Provider，避免 status/plan 建表。
		rows, err := q.QueryContext(ctx, "SELECT version_id, tstamp FROM "+VersionTable+" WHERE is_applied = 1 AND version_id > 0")
		if err != nil {
			return nil, fmt.Errorf("migrate: 读取版本表: %w", err)
		}
		for rows.Next() {
			var v int64
			var ts sql.NullTime
			if err := rows.Scan(&v, &ts); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("migrate: 读取版本表: %w", err)
			}
			st.applied[v] = ts.Time
			if v > st.max {
				st.max = v
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("migrate: 读取版本表: %w", err)
		}
	}
	ok, err = tableExists(ctx, q, RunsTable)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if ok {
		rs, err := q.QueryContext(ctx, "SELECT version, name, class, checksum, direction, status, error, started_at, finished_at FROM "+RunsTable+" ORDER BY version")
		if err != nil {
			return nil, fmt.Errorf("migrate: 读取 %s: %w", RunsTable, err)
		}
		defer func() { _ = rs.Close() }()
		for rs.Next() {
			var x runRow
			if err := rs.Scan(&x.Version, &x.Name, &x.Class, &x.Checksum, &x.Direction, &x.Status, &x.Error, &x.StartedAt, &x.FinishedAt); err != nil {
				return nil, fmt.Errorf("migrate: 读取 %s: %w", RunsTable, err)
			}
			st.runs[x.Version] = &x
		}
		if err := rs.Err(); err != nil {
			return nil, fmt.Errorf("migrate: 读取 %s: %w", RunsTable, err)
		}
	}
	return st, nil
}

func (st *dbState) dirty() *runRow {
	var vs []int64
	for v, x := range st.runs {
		if x.Status != statusApplied {
			vs = append(vs, v)
		}
	}
	if len(vs) == 0 {
		return nil
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i] < vs[j] })
	return st.runs[vs[0]]
}

func (st *dbState) appliedSet() map[int64]bool {
	out := make(map[int64]bool, len(st.applied))
	for v := range st.applied {
		out[v] = true
	}
	return out
}

func (x *runRow) toDirty() *ErrDirty {
	d := &ErrDirty{Version: x.Version, Name: x.Name, Direction: x.Direction, Status: x.Status}
	if x.Error.Valid {
		d.Cause = x.Error.String
	}
	return d
}

// verifyIntegrity 依次检查：dirty（skipDirty 时跳过，供 Recover 使用）、版本表与运行记录矛盾、数据库里有而二进制没有的迁移、校验和、乱序。
func (r *Runner) verifyIntegrity(st *dbState, skipDirty bool) error {
	if d := st.dirty(); d != nil && !skipDirty {
		return d.toDirty()
	}
	for v, run := range st.runs {
		if _, ok := st.applied[v]; run.Status == statusApplied && !ok {
			return &ErrInconsistent{Version: v, Detail: "的运行记录为 applied，但版本表里没有它"}
		}
	}
	var unknown []int64
	for v := range st.applied {
		if r.set.ByVersion(v) == nil {
			unknown = append(unknown, v)
		}
	}
	if len(unknown) > 0 {
		return &ErrUnknownApplied{Versions: unknown}
	}
	for _, m := range r.set.Migrations {
		if _, ok := st.applied[m.Version]; !ok {
			continue
		}
		run := st.runs[m.Version]
		switch {
		case run == nil:
			return &ErrChecksumMismatch{Version: m.Version, File: m.File, Actual: m.Checksum}
		case run.Checksum != m.Checksum:
			return &ErrChecksumMismatch{Version: m.Version, File: m.File, Recorded: run.Checksum, Actual: m.Checksum}
		}
	}
	var missing []int64
	for _, m := range r.set.Migrations {
		if _, ok := st.applied[m.Version]; !ok && m.Version < st.max {
			missing = append(missing, m.Version)
		}
	}
	if len(missing) > 0 {
		return &ErrOutOfOrder{Missing: missing, Max: st.max}
	}
	return nil
}

// ---- plan / status（只读，不加锁、不建表）----

// Plan 连库生成「待执行」计划（只读：不创建任何表）。
func (r *Runner) Plan(ctx context.Context) (*Plan, error) {
	ver, err := r.CheckVersion(ctx)
	if err != nil {
		return nil, err
	}
	st, err := r.readState(ctx, r.sqlDB())
	if err != nil {
		return nil, err
	}
	// plan 是升级前的校验（ADR-013 §2：先 plan 再 up）：库处于 dirty/校验和不一致/乱序时 up 必然拒绝，plan 不应给出一份看似正常的计划。
	// 例外：另一个 migrate 正在运行时 running 记录是「进行中」而不是 dirty。
	if d := st.dirty(); d != nil {
		if held, herr := r.lockHeld(ctx); herr == nil && held {
			return nil, &ErrMigrationRunning{Version: d.Version}
		}
	}
	if err := r.verifyIntegrity(st, false); err != nil {
		return nil, err
	}
	p := BuildPlan(r.set, st.appliedSet())
	p.MySQLVersion = ver
	v := st.max
	p.DBVersion = &v
	return p, nil
}

// StatusItem 是 status 中的一行。
type StatusItem struct {
	Version   int64      `json:"version"`
	Name      string     `json:"name,omitempty"`
	Class     Class      `json:"class,omitempty"`
	State     string     `json:"state"` // applied | pending | running（另一个 migrate 进行中）| dirty | unknown（数据库有而二进制没有）
	AppliedAt *time.Time `json:"applied_at,omitempty"`
	Checksum  string     `json:"checksum,omitempty"`
	// ChecksumState：ok | mismatch | untracked | n/a
	ChecksumState string `json:"checksum_state"`
}

// Status 是迁移状态快照。
type Status struct {
	MySQLVersion string     `json:"mysql_version"`
	DBVersion    int64      `json:"db_version"`
	LatestKnown  int64      `json:"latest_version"`
	Pending      int        `json:"pending"`
	Dirty        *DirtyInfo `json:"dirty,omitempty"`
	// InProgress 为 true 表示另一个 migrate 正在运行（running 记录不是 dirty）。
	InProgress bool         `json:"in_progress,omitempty"`
	Migrations []StatusItem `json:"migrations"`
	// Problem 汇总第一个完整性问题的文字（空表示健康）。
	Problem string `json:"problem,omitempty"`
	err     error
}

// DirtyInfo 描述 dirty 的迁移。
type DirtyInfo struct {
	Version   int64  `json:"version"`
	Name      string `json:"name"`
	Direction string `json:"direction"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// Err 返回 status 发现的第一个完整性错误（dirty/校验和/未知/乱序），健康时为 nil；用于决定退出码。
func (s *Status) Err() error { return s.err }

// Status 只读地汇总迁移状态；完整性问题不会使调用失败，而是体现在返回值里（Err/Problem）。
func (r *Runner) Status(ctx context.Context) (*Status, error) {
	ver, verr := r.CheckVersion(ctx)
	if verr != nil && ver == "" {
		return nil, verr
	}
	st, err := r.readState(ctx, r.sqlDB())
	if err != nil {
		return nil, err
	}
	out := &Status{MySQLVersion: ver, DBVersion: st.max, Migrations: []StatusItem{}}
	if verr != nil {
		out.err = verr
		out.Problem = verr.Error()
	}
	if d := st.dirty(); d != nil {
		if held, herr := r.lockHeld(ctx); herr == nil && held {
			out.InProgress = true // 另一个 migrate 正在运行：running 记录是进行中，不是 dirty
		} else {
			out.Dirty = &DirtyInfo{Version: d.Version, Name: d.Name, Direction: d.Direction, Status: d.Status, Error: d.Error.String}
		}
	}
	if !out.InProgress {
		if ierr := r.verifyIntegrity(st, false); ierr != nil && out.err == nil {
			out.err, out.Problem = ierr, ierr.Error()
		}
	}
	seen := map[int64]bool{}
	for _, m := range r.set.Migrations {
		seen[m.Version] = true
		out.LatestKnown = m.Version
		it := StatusItem{Version: m.Version, Name: m.Name, Class: m.Class, State: "pending", Checksum: m.Checksum, ChecksumState: "n/a"}
		if ts, ok := st.applied[m.Version]; ok {
			t := ts
			it.State, it.AppliedAt = "applied", &t
			switch run := st.runs[m.Version]; {
			case run == nil:
				it.ChecksumState = "untracked"
			case run.Checksum != m.Checksum:
				it.ChecksumState = "mismatch"
			default:
				it.ChecksumState = "ok"
			}
		} else {
			out.Pending++
		}
		if run := st.runs[m.Version]; run != nil && run.Status != statusApplied {
			it.State = "dirty"
			if out.InProgress {
				it.State = "running"
			}
		}
		out.Migrations = append(out.Migrations, it)
	}
	for v, ts := range st.applied {
		if !seen[v] {
			t := ts
			out.Migrations = append(out.Migrations, StatusItem{Version: v, State: "unknown", AppliedAt: &t, ChecksumState: "n/a"})
		}
	}
	sort.Slice(out.Migrations, func(i, j int) bool { return out.Migrations[i].Version < out.Migrations[j].Version })
	return out, nil
}

// ---- up / down / recover ----

// UpOptions 控制 Up。
type UpOptions struct {
	// To 非零时只升级到该序号（含）。
	To int64
	// AcceptIrreversible 为 true 才允许执行 irreversible 迁移（ADR-013 §2；Tools 在用户确认并完成备份后传入）。
	AcceptIrreversible bool
}

// AppliedMigration 是一次执行的结果。
type AppliedMigration struct {
	Version  int64         `json:"version"`
	Name     string        `json:"name"`
	Class    Class         `json:"class"`
	Duration time.Duration `json:"-"`
	Millis   int64         `json:"duration_ms"`
}

// UpResult 是 Up 的结果。
type UpResult struct {
	FromVersion int64              `json:"from_version"`
	ToVersion   int64              `json:"to_version"`
	Applied     []AppliedMigration `json:"applied"`
}

func (r *Runner) provider() (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectMySQL, r.sqlDB(), r.fsys,
		goose.WithTableName(VersionTable),
		goose.WithLogger(goose.NopLogger()),
		goose.WithDisableGlobalRegistry(true))
}

func (r *Runner) begin(ctx context.Context) (*migrateLock, error) {
	if err := r.failIfLint(); err != nil {
		return nil, err
	}
	if _, err := r.CheckVersion(ctx); err != nil {
		return nil, err
	}
	if err := r.Preflight(ctx); err != nil {
		return nil, err
	}
	lock, err := r.acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := r.sqlDB().ExecContext(ctx, createRunsSQL); err != nil {
		lock.release()
		return nil, fmt.Errorf("migrate: 创建 %s: %w", RunsTable, err)
	}
	return lock, nil
}

// Up 应用待执行迁移（逐个：标记 running → goose 执行 → 标记 applied；失败则标记 failed 并停止）。
func (r *Runner) Up(ctx context.Context, opts UpOptions) (*UpResult, error) {
	lock, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer lock.release()

	st, err := r.readState(ctx, r.sqlDB())
	if err != nil {
		return nil, err
	}
	if err := r.verifyIntegrity(st, false); err != nil {
		return nil, err
	}
	defer r.syncIrreversibleSetting(ctx) // 含无待执行迁移与失败返回：以运行记录对账（ADR-016 决策 4）
	res := &UpResult{FromVersion: st.max, ToVersion: st.max, Applied: []AppliedMigration{}}
	var pending []*Migration
	for _, m := range r.set.Migrations {
		if _, ok := st.applied[m.Version]; ok {
			continue
		}
		if opts.To > 0 && m.Version > opts.To {
			break
		}
		pending = append(pending, m)
	}
	if len(pending) == 0 {
		r.logf("没有待执行的迁移（当前版本 %06d）", st.max)
		return res, nil
	}
	if !opts.AcceptIrreversible {
		var files []string
		for _, m := range pending {
			if m.Class == ClassIrreversible {
				files = append(files, m.File)
			}
		}
		if len(files) > 0 {
			return nil, &ErrIrreversibleNotAccepted{Files: files}
		}
	}
	prov, err := r.provider()
	if err != nil {
		return nil, err
	}
	for _, m := range pending {
		if err := lock.verify(ctx); err != nil {
			return res, err
		}
		lock.touchLease(ctx)
		if err := r.markRunning(ctx, m, "up"); err != nil {
			return res, err
		}
		start := r.Clock.Now()
		r.logf("应用 %s（%s）……", m.File, m.Class)
		if _, err := prov.ApplyVersion(ctx, m.Version, true); err != nil {
			r.markFailed(ctx, m, err)
			return res, fmt.Errorf("迁移 %s 失败，已标记 dirty（不会继续后续迁移）：%w", m.File, err)
		}
		// 迁移已落库：即使此刻被 Ctrl-C 取消，标记也要落盘，否则一个已完整应用的迁移会被误判为 dirty。
		if err := detached(ctx, func(c context.Context) error { return r.markApplied(c, m) }); err != nil {
			return res, err
		}
		d := clock.Since(r.Clock, start)
		res.Applied = append(res.Applied, AppliedMigration{Version: m.Version, Name: m.Name, Class: m.Class, Duration: d, Millis: d.Milliseconds()})
		res.ToVersion = m.Version
		r.logf("已应用 %s（%d ms）", m.File, d.Milliseconds())
	}
	return res, nil
}

// IrreversibleSettingKey 是 system_settings 中记录「最近一次已应用的不可逆迁移」的键（ADR-016 决策 4，辅助 doctor 判断
// 「回滚 = 恢复备份」是否仍可简单切回旧二进制）。值为 JSON：{"version":N,"name":"…","applied_at":"…Z"}。
const IrreversibleSettingKey = "schema.latest_class_irreversible_applied"

// syncIrreversibleSetting 让 system_settings 的 IrreversibleSettingKey 与运行记录表对账：以运行记录里最近一次「已应用」的
// irreversible 迁移为准，缺失或落后则写入。它在每次 up 结束（含无待执行迁移与失败返回）与 recover 补记 applied 之后调用，所以
// 「迁移已提交但进程在写记录前被杀」「irreversible 迁移早于 system_settings 应用」「记录被误删」都会在下一次 up/recover 自愈，
// 而不依赖某一次调用恰好成功。它是辅助信息，不是迁移成败的依据：表缺失或结构不符时静默跳过；探测或写入失败只记警告。
func (r *Runner) syncIrreversibleSetting(ctx context.Context) {
	_ = detached(ctx, func(c context.Context) error {
		warn := func(err error) {
			r.logf("警告：未能同步 %s（不影响迁移结果，下次 up/recover 会再次尝试）：%v", IrreversibleSettingKey, err)
		}
		var cols int
		if err := r.sqlDB().QueryRowContext(c, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'system_settings' "+
			"AND COLUMN_NAME IN ('uid','setting_key','value_json','value_enc','group_name')").Scan(&cols); err != nil {
			warn(err)
			return nil
		}
		if cols != 5 {
			return nil // 表尚未创建（000002 之前）或结构不符
		}
		var uniqueKey int
		if err := r.sqlDB().QueryRowContext(c, "SELECT COUNT(*) FROM (SELECT INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'system_settings' "+
			"AND NON_UNIQUE = 0 GROUP BY INDEX_NAME HAVING COUNT(*) = 1 AND MAX(COLUMN_NAME) = 'setting_key') k").Scan(&uniqueKey); err != nil {
			warn(err)
			return nil
		}
		if uniqueKey == 0 {
			return nil // 没有 setting_key 的唯一键：ON DUPLICATE KEY 不会生效，写入会产生重复行
		}
		var ver int64
		err := r.sqlDB().QueryRowContext(c, "SELECT version FROM "+RunsTable+" WHERE class = 'irreversible' AND direction = 'up' AND status = 'applied' ORDER BY version DESC LIMIT 1").Scan(&ver)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			warn(err)
			return nil
		}
		var cur sql.NullInt64
		err = r.sqlDB().QueryRowContext(c, "SELECT CAST(JSON_EXTRACT(value_json, '$.version') AS SIGNED) FROM system_settings WHERE setting_key = ?", IrreversibleSettingKey).Scan(&cur)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			warn(err)
			return nil
		}
		if cur.Valid && cur.Int64 >= ver {
			return nil
		}
		id, err := uid.NewGenerator(r.Clock).New()
		if err != nil {
			warn(err)
			return nil
		}
		if _, err := r.sqlDB().ExecContext(c, "INSERT INTO system_settings (uid, setting_key, value_json, group_name) "+
			"SELECT ?, ?, JSON_OBJECT('version', version, 'name', name, 'applied_at', DATE_FORMAT(finished_at, '%Y-%m-%dT%H:%i:%s.%fZ')), 'general' FROM "+RunsTable+" WHERE version = ? "+
			"ON DUPLICATE KEY UPDATE value_json = VALUES(value_json), value_enc = NULL", id, IrreversibleSettingKey, ver); err != nil {
			warn(err)
		}
		return nil
	})
}

// DownOptions 控制 Down。
type DownOptions struct {
	// Steps 为回退的迁移个数（缺省 1）；To 非零时回退到该序号（该序号保留，To=0 表示回退全部——须同时 All=true）。
	Steps int
	To    int64
	All   bool
}

// Down 回退迁移：只允许 reversible（ADR-013 §3）；先整体校验再逐个回退。
func (r *Runner) Down(ctx context.Context, opts DownOptions) (*UpResult, error) {
	lock, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer lock.release()

	st, err := r.readState(ctx, r.sqlDB())
	if err != nil {
		return nil, err
	}
	if err := r.verifyIntegrity(st, false); err != nil {
		return nil, err
	}
	var applied []int64
	for v := range st.applied {
		applied = append(applied, v)
	}
	sort.Slice(applied, func(i, j int) bool { return applied[i] > applied[j] }) // 从新到旧
	var plan []*Migration
	switch {
	case opts.To > 0 || opts.All:
		for _, v := range applied {
			if v > opts.To {
				plan = append(plan, r.set.ByVersion(v))
			}
		}
	default:
		n := opts.Steps
		if n <= 0 {
			n = 1
		}
		for _, v := range applied {
			if len(plan) >= n {
				break
			}
			plan = append(plan, r.set.ByVersion(v))
		}
	}
	res := &UpResult{FromVersion: st.max, ToVersion: st.max, Applied: []AppliedMigration{}}
	if len(plan) == 0 {
		r.logf("没有可回退的迁移")
		return res, nil
	}
	for _, m := range plan { // 先整体校验：任何一个不是 reversible 就一个都不执行
		if m.Class != ClassReversible {
			return nil, &ErrDownNotAllowed{File: m.File, Class: m.Class}
		}
		if !m.HasDown {
			return nil, &ErrDownNotAllowed{File: m.File, Class: m.Class}
		}
	}
	prov, err := r.provider()
	if err != nil {
		return nil, err
	}
	for _, m := range plan {
		if err := lock.verify(ctx); err != nil {
			return res, err
		}
		lock.touchLease(ctx)
		if err := r.markRunning(ctx, m, "down"); err != nil {
			return res, err
		}
		start := r.Clock.Now()
		r.logf("回退 %s ……", m.File)
		if _, err := prov.ApplyVersion(ctx, m.Version, false); err != nil {
			r.markFailed(ctx, m, err)
			return res, fmt.Errorf("回退 %s 失败，已标记 dirty：%w", m.File, err)
		}
		if err := detached(ctx, func(c context.Context) error {
			_, err := r.sqlDB().ExecContext(c, "DELETE FROM "+RunsTable+" WHERE version = ?", m.Version)
			return err
		}); err != nil {
			return res, fmt.Errorf("migrate: 清理 %s 记录（回退已完成，运行记录停在 running，请 recover）: %w", RunsTable, err)
		}
		d := clock.Since(r.Clock, start)
		res.Applied = append(res.Applied, AppliedMigration{Version: m.Version, Name: m.Name, Class: m.Class, Duration: d, Millis: d.Milliseconds()})
		r.logf("已回退 %s（%d ms）", m.File, d.Milliseconds())
	}
	res.ToVersion = 0
	gone := map[int64]bool{}
	for _, a := range res.Applied {
		gone[a.Version] = true
	}
	for v := range st.applied {
		if !gone[v] && v > res.ToVersion {
			res.ToVersion = v
		}
	}
	return res, nil
}

// Recover 清除 dirty 标记（操作者确认可重跑之后）：
//   - up 方向：goose 版本表里没有该序号 → 删除记录，下次 up 重新执行（迁移带幂等守卫）；
//     已有该序号（SQL 全部完成、仅最后的标记更新丢失）→ 直接标记 applied；
//   - down 方向：迁移仍记为已应用 → 恢复为 applied，下次 down 重试。
func (r *Runner) Recover(ctx context.Context, version int64) error {
	lock, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer lock.release()

	st, err := r.readState(ctx, r.sqlDB())
	if err != nil {
		return err
	}
	// 除 dirty 的那个迁移之外，其余完整性问题（校验和、未知迁移、版本表矛盾）照常拒绝：recover 不是绕过校验的通道。
	if err := r.verifyIntegrity(st, true); err != nil {
		return err
	}
	d := st.dirty()
	if d == nil {
		return errors.New("migrate: 没有处于 dirty 状态的迁移，无需 recover")
	}
	if d.Version != version {
		return fmt.Errorf("migrate: dirty 的迁移是 %06d_%s，不是 %06d；请用 --version %d", d.Version, d.Name, version, d.Version)
	}
	_, inGoose := st.applied[d.Version]
	m := r.set.ByVersion(d.Version)
	switch {
	case d.Direction == "up" && !inGoose:
		// SQL 没有完整执行（或没有执行）：删除运行记录，下次 up 重新执行（文件在此期间被修复也没关系：它从未被记为已应用）。
		_, err = r.sqlDB().ExecContext(ctx, "DELETE FROM "+RunsTable+" WHERE version = ?", d.Version)
		r.logf("已清除 %06d_%s 的 dirty 标记：下次 up 将重新执行它", d.Version, d.Name)
	case d.Direction == "up" && inGoose:
		// SQL 已完整执行且版本表已记录，只是最后的标记更新丢了。必须保持 markRunning 时记录的校验和，并核对文件未被改动，
		// 否则 recover 会把被篡改文件的哈希「洗白」成已应用的校验和。
		if m == nil {
			return fmt.Errorf("migrate: 迁移 %06d 已在数据库中应用，但当前二进制没有对应文件，无法恢复", d.Version)
		}
		if m.Checksum != d.Checksum {
			return &ErrChecksumMismatch{Version: m.Version, File: m.File, Recorded: d.Checksum, Actual: m.Checksum}
		}
		_, err = r.sqlDB().ExecContext(ctx, "UPDATE "+RunsTable+" SET direction = 'up', status = 'applied', error = NULL, finished_at = NOW(3) WHERE version = ?", d.Version)
		r.logf("%06d_%s 的 SQL 已完整执行（版本表已记录），仅补记 applied", d.Version, d.Name)
		if err == nil {
			r.syncIrreversibleSetting(ctx)
		}
	case !inGoose: // down 方向，回退的 SQL 已完整执行且版本表记录已被 goose 删除，只是运行记录没来得及清理
		_, err = r.sqlDB().ExecContext(ctx, "DELETE FROM "+RunsTable+" WHERE version = ?", d.Version)
		r.logf("%06d_%s 的回退已完整执行（版本表已无该记录），清除运行记录", d.Version, d.Name)
	default: // down 方向，回退没有完成：迁移仍记为已应用
		_, err = r.sqlDB().ExecContext(ctx, "UPDATE "+RunsTable+" SET direction = 'up', status = 'applied', error = NULL, finished_at = NOW(3) WHERE version = ?", d.Version)
		r.logf("已清除 %06d_%s 的 dirty 标记：迁移仍记为已应用，可重试 down", d.Version, d.Name)
	}
	return err
}

// ---- 运行记录 ----

func (r *Runner) markRunning(ctx context.Context, m *Migration, dir string) error {
	var err error
	if dir == "up" {
		_, err = r.sqlDB().ExecContext(ctx, "INSERT INTO "+RunsTable+" (version, name, class, checksum, direction, status, started_at) VALUES (?, ?, ?, ?, 'up', 'running', NOW(3))",
			m.Version, m.Name, string(m.Class), m.Checksum)
	} else {
		_, err = r.sqlDB().ExecContext(ctx, "UPDATE "+RunsTable+" SET direction = 'down', status = 'running', error = NULL, started_at = NOW(3), finished_at = NULL WHERE version = ?", m.Version)
	}
	if err != nil {
		return fmt.Errorf("migrate: 记录 %s 开始执行: %w", m.File, err)
	}
	return nil
}

func (r *Runner) markApplied(ctx context.Context, m *Migration) error {
	_, err := r.sqlDB().ExecContext(ctx, "INSERT INTO "+RunsTable+" (version, name, class, checksum, direction, status, started_at, finished_at) VALUES (?, ?, ?, ?, 'up', 'applied', NOW(3), NOW(3)) "+
		"ON DUPLICATE KEY UPDATE direction = 'up', status = 'applied', error = NULL, checksum = VALUES(checksum), finished_at = NOW(3)",
		m.Version, m.Name, string(m.Class), m.Checksum)
	if err != nil {
		return fmt.Errorf("migrate: 记录 %s 完成（迁移 SQL 已执行并写入版本表，运行记录停在 running，请 recover）: %w", m.File, err)
	}
	return nil
}

// markFailed 在 ctx 可能已取消（Ctrl-C）时仍要落盘 dirty 标记，故用脱离取消的 ctx。
func (r *Runner) markFailed(ctx context.Context, m *Migration, cause error) {
	msg := truncateRunes(cause.Error(), 2000)
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := r.sqlDB().ExecContext(c, "UPDATE "+RunsTable+" SET status = 'failed', error = ?, finished_at = NOW(3) WHERE version = ?", msg, m.Version); err != nil {
		r.logf("警告：无法记录失败状态（运行记录保持 running，同样视为 dirty）：%v", err)
	}
}

// truncateRunes 按字符（不是字节）截断：错误文本含中文时按字节截断会产生非法 UTF-8，严格模式下的 UPDATE 会失败，失败原因就丢了。
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	rs := []rune(s)
	return string(rs[:max]) + "…"
}

// detached 在脱离取消、带超时的 ctx 上运行 fn，用于「事实已发生、必须落盘」的记录写入。
func detached(ctx context.Context, fn func(ctx context.Context) error) error {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	return fn(c)
}
