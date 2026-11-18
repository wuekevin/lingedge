package migrationstest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
)

func wantVersions(t *testing.T, e *env, want ...string) {
	t.Helper()
	got := e.appliedVersions(t)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("goose 版本表已应用序号 = %v，期望 %v", got, want)
	}
}

// 空库全量升级（AC5）+ schema_migrations 自定义表名 + 逐表授权真实生效（Story 1.5 发现）。
func TestUpAppliesFullSetOnEmptyDatabase(t *testing.T) {
	e := newEnv(t)
	r := e.runner(t, fixtureFS(t, "good"))
	if f := r.Findings(); len(f) != 0 {
		t.Fatalf("夹具 good 应通过 lint: %v", f)
	}
	res, err := r.Up(ctxT(t), migrate.UpOptions{})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(res.Applied) != 5 || res.FromVersion != 0 || res.ToVersion != 5 {
		t.Fatalf("结果 = %+v", res)
	}
	wantVersions(t, e, "1", "2", "3", "4", "5")
	for _, tbl := range []string{"tenants", "widgets", "audit_logs", migrate.VersionTable, migrate.RunsTable} {
		if !e.hasTable(t, tbl) {
			t.Errorf("缺少表 %s", tbl)
		}
	}
	if e.hasTable(t, "goose_db_version") {
		t.Error("不应使用 goose 默认表名 goose_db_version")
	}
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.tenants WHERE uid = '01J00000000000000000000DEF'"); got != "1" {
		t.Errorf("种子迁移应写入默认租户: %s", got)
	}
	// 运行记录：每个迁移一行、状态 applied、校验和与文件一致。
	for _, m := range r.Set().Migrations {
		if got := e.scalar(t, "SELECT CONCAT(status, '/', direction, '/', checksum) FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = ?", m.Version); got != "applied/up/"+m.Checksum {
			t.Errorf("%s 运行记录 = %q", m.File, got)
		}
	}

	// 逐表授权：每张新表都有 lingedge_app 的表级授权；追加只写表仅 SELECT, INSERT；分区表 maint 的 SELECT, ALTER, DROP。
	app := e.grantsOf(t, migrate.AppUser)
	if app["widgets"] != "Delete,Insert,Select,Update" || app["tenants"] != "Delete,Insert,Select,Update" {
		t.Errorf("app 对业务表的授权 = %v", app)
	}
	if app["audit_logs"] != "Insert,Select" {
		t.Errorf("app 对 audit_logs 必须仅 SELECT, INSERT: %q", app["audit_logs"])
	}
	if _, ok := app[migrate.RunsTable]; ok {
		t.Error("框架自身的运行记录表不得授权给 app")
	}
	if maint := e.grantsOf(t, migrate.MaintUser); !reflect.DeepEqual(maint, map[string]string{"audit_logs": "Alter,Drop,Select"}) {
		t.Errorf("maint 只应有分区表 audit_logs 的 SELECT, ALTER, DROP: %v", maint)
	}

	// 授权的真实效果（三类账号权限，AC6 ⑤ 之外的运行期验证）。
	ctx := ctxT(t)
	ap := e.appPool(t)
	ex := ap.Executor(ctx)
	if _, err := ex.Exec(ctx, db.Q("INSERT INTO audit_logs (action) VALUES (?)"), "login"); err != nil {
		t.Fatalf("app 应能 INSERT audit_logs: %v", err)
	}
	_, err = ex.Exec(ctx, db.Q("UPDATE audit_logs SET action = ?"), "tampered")
	if n, ok := mysqlErr(err); !ok || n != db.MySQLErrTableAccessDenied {
		t.Errorf("app UPDATE audit_logs 应被表级授权拒绝（1142）: %v", err)
	}
	_, err = ex.Exec(ctx, db.Q("DELETE FROM audit_logs"))
	if n, ok := mysqlErr(err); !ok || n != db.MySQLErrTableAccessDenied {
		t.Errorf("app DELETE audit_logs 应被拒绝（1142）: %v", err)
	}
	if _, err := ex.Exec(ctx, db.Q("INSERT INTO widgets (tenant_id, name) VALUES (?, ?)"), 1, "w"); err != nil {
		t.Errorf("app 应能写业务表: %v", err)
	}
	_, err = ex.Exec(ctx, db.Q("ALTER TABLE widgets ADD COLUMN evil INT NULL"))
	if n, ok := mysqlErr(err); !ok || n != db.MySQLErrTableAccessDenied {
		t.Errorf("app 不得 ALTER（1142）: %v", err)
	}
	if _, err := ex.Exec(ctx, db.Q("SELECT * FROM "+migrate.RunsTable)); err == nil {
		t.Error("app 不得读取迁移运行记录表")
	}
	mx := e.maintPool(t).Executor(ctx)
	if _, err := mx.Exec(ctx, db.Q("ALTER TABLE audit_logs REORGANIZE PARTITION pmax INTO (PARTITION p202701 VALUES LESS THAN ('2027-02-01 00:00:00'), PARTITION pmax VALUES LESS THAN (MAXVALUE))")); err != nil {
		t.Errorf("maint 应能 REORGANIZE 分区: %v", err)
	}
	_, err = mx.Exec(ctx, db.Q("DROP TABLE widgets"))
	if n, ok := mysqlErr(err); !ok || n != db.MySQLErrTableAccessDenied {
		t.Errorf("maint 不能 DROP 非分区表（1142）: %v", err)
	}
	// 触发器随迁移创建（binlog=ON、trust=0 的实例上，迁移账号靠 SET_USER_ID 建成）。
	_, err = e.admin(t).ExecContext(ctx, "UPDATE `"+e.dbName+"`.audit_logs SET action = 'x'")
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("audit_logs 的不可变触发器应拒绝 UPDATE: %v", err)
	}
}

// 重复执行幂等：再次 Up 无任何变更；plan/status 反映真实状态。
func TestUpTwiceIsNoopAndStatusPlanReflectState(t *testing.T) {
	e := newEnv(t)
	r := e.runner(t, fixtureFS(t, "good"))
	ctx := ctxT(t)

	p, err := r.Plan(ctx)
	if err != nil || p.Mode != "pending" || p.Count != 5 || p.DBVersion == nil || *p.DBVersion != 0 {
		t.Fatalf("空库 plan: %+v err=%v", p, err)
	}
	if e.hasTable(t, migrate.VersionTable) || e.hasTable(t, migrate.RunsTable) {
		t.Fatal("plan 是只读的，不得创建任何表")
	}
	if !strings.HasPrefix(p.MySQLVersion, "8.0.") {
		t.Errorf("plan 应带 MySQL 版本: %q", p.MySQLVersion)
	}

	if _, err := r.Up(ctx, migrate.UpOptions{To: 2}); err != nil {
		t.Fatal(err)
	}
	p, _ = r.Plan(ctx)
	if p.Count != 3 || *p.DBVersion != 2 || p.Migrations[0].Version != 3 {
		t.Fatalf("升到 2 之后 plan 只应剩 3..5: %+v", p)
	}
	sig := e.schemaSignature(t)

	res, err := r.Up(ctx, migrate.UpOptions{})
	if err != nil || len(res.Applied) != 3 {
		t.Fatalf("继续升级: %+v %v", res, err)
	}
	sig2 := e.schemaSignature(t)
	res, err = r.Up(ctx, migrate.UpOptions{})
	if err != nil || len(res.Applied) != 0 || res.ToVersion != 5 {
		t.Fatalf("第二次 Up 必须是空操作: %+v %v", res, err)
	}
	if e.schemaSignature(t) != sig2 || sig == sig2 {
		t.Fatal("重复 Up 不得改变库结构")
	}
	wantVersions(t, e, "1", "2", "3", "4", "5")

	st, err := r.Status(ctx)
	if err != nil || st.Err() != nil || st.Pending != 0 || st.DBVersion != 5 || st.Dirty != nil {
		t.Fatalf("status: %+v err=%v", st, err)
	}
	for _, it := range st.Migrations {
		if it.State != "applied" || it.ChecksumState != "ok" || it.AppliedAt == nil {
			t.Errorf("status 项: %+v", it)
		}
	}
	p, _ = r.Plan(ctx)
	if p.Count != 0 || p.HasIrreversible {
		t.Fatalf("全部应用后 plan 为空: %+v", p)
	}
}

// reversible 升降级路径；expand 与 irreversible 拒绝回退，且「先整体校验再执行」，被拒时一个迁移都不回退。
func TestDownOnlyForReversibleAndIsAllOrNothing(t *testing.T) {
	e := newEnv(t)
	r := e.runner(t, fixtureFS(t, "good"))
	ctx := ctxT(t)
	if _, err := r.Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	full := e.createStmt(t, "widgets")
	if !strings.Contains(full, "`note`") || !strings.Contains(full, "idx_widgets_tenant_name") {
		t.Fatalf("前置：widgets 应含 note 列与名称索引:\n%s", full)
	}

	// 000005 是 reversible：回退成功，note 列消失，版本表与运行记录同步。
	res, err := r.Down(ctx, migrate.DownOptions{})
	if err != nil || len(res.Applied) != 1 || res.Applied[0].Version != 5 || res.ToVersion != 4 {
		t.Fatalf("回退 000005: %+v %v", res, err)
	}
	if strings.Contains(e.createStmt(t, "widgets"), "`note`") {
		t.Fatal("回退后 note 列应已删除")
	}
	wantVersions(t, e, "1", "2", "3", "4")
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = 5"); got != "0" {
		t.Fatalf("回退后运行记录应删除: %s", got)
	}

	// 000004 是 expand：拒绝。
	_, err = r.Down(ctx, migrate.DownOptions{})
	var dn *migrate.ErrDownNotAllowed
	if !errors.As(err, &dn) || dn.Class != migrate.ClassExpand || dn.File != "000004_seed_defaults.sql" {
		t.Fatalf("expand 迁移必须拒绝回退: %v", err)
	}
	// 回退到 1：路径上有 expand（000004、000003）→ 整体拒绝，000002（reversible）也不得被回退。
	_, err = r.Down(ctx, migrate.DownOptions{To: 1})
	if !errors.As(err, &dn) {
		t.Fatalf("路径上含 expand 必须整体拒绝: %v", err)
	}
	wantVersions(t, e, "1", "2", "3", "4")
	if !strings.Contains(e.createStmt(t, "widgets"), "idx_widgets_tenant_name") {
		t.Fatal("被拒绝的回退不得改动任何迁移（000002 的索引应仍在）")
	}
	if got := ExitOf(err); got != migrate.ExitFailure {
		t.Errorf("退出码 = %d", got)
	}

	// 升降级往返：升到 2 → 回退 2 → 再升，结构回到一致。
	e2 := newEnv(t)
	r2 := e2.runner(t, fixtureFS(t, "good"))
	if _, err := r2.Up(ctx, migrate.UpOptions{To: 2}); err != nil {
		t.Fatal(err)
	}
	before := e2.schemaSignature(t)
	if _, err := r2.Down(ctx, migrate.DownOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e2.createStmt(t, "widgets"), "idx_widgets_tenant_name") {
		t.Fatal("回退 000002 后索引应消失")
	}
	if _, err := r2.Up(ctx, migrate.UpOptions{To: 2}); err != nil {
		t.Fatal(err)
	}
	if after := e2.schemaSignature(t); after != before {
		t.Fatalf("升-降-升往返后结构应一致:\n--- before\n%s\n--- after\n%s", before, after)
	}
}

// ExitOf 是 migrate.ExitCode 的别名，便于断言。
func ExitOf(err error) int { return migrate.ExitCode(err) }

// irreversible：未确认不执行（一个都不应用）；确认后执行；之后不可回退。
func TestIrreversibleRequiresAcceptanceAndCannotBeRolledBack(t *testing.T) {
	e := newEnv(t)
	fsys := fixtureFS(t, "good", "extend/contract")
	r := e.runner(t, fsys)
	ctx := ctxT(t)

	p, err := r.Plan(ctx)
	if err != nil || !p.HasIrreversible || p.MaxClass != migrate.ClassIrreversible || p.Count != 6 {
		t.Fatalf("plan 必须标出 has_irreversible: %+v %v", p, err)
	}
	_, err = r.Up(ctx, migrate.UpOptions{})
	var ir *migrate.ErrIrreversibleNotAccepted
	if !errors.As(err, &ir) || ir.Files[0] != "000006_drop_legacy_column.sql" {
		t.Fatalf("含不可逆迁移未确认必须拒绝: %v", err)
	}
	if migrate.ExitCode(err) != migrate.ExitIrreversible {
		t.Fatalf("退出码应为 %d: %d", migrate.ExitIrreversible, migrate.ExitCode(err))
	}
	wantVersions(t, e) // 一个迁移都没有应用
	if e.hasTable(t, "tenants") {
		t.Fatal("拒绝时不得应用任何迁移（含前面的 expand）")
	}

	// 只升到 5（不含不可逆）不需要确认。
	if _, err := r.Up(ctx, migrate.UpOptions{To: 5}); err != nil {
		t.Fatalf("不含不可逆的范围不需要确认: %v", err)
	}
	if _, err := r.Up(ctx, migrate.UpOptions{AcceptIrreversible: true}); err != nil {
		t.Fatalf("确认后应能执行: %v", err)
	}
	if strings.Contains(e.createStmt(t, "widgets"), "`legacy`") {
		t.Fatal("legacy 列应已删除")
	}
	_, err = r.Down(ctx, migrate.DownOptions{})
	var dn *migrate.ErrDownNotAllowed
	if !errors.As(err, &dn) || dn.Class != migrate.ClassIrreversible {
		t.Fatalf("irreversible 必须拒绝回退: %v", err)
	}
	if !strings.Contains(err.Error(), "恢复升级前备份") {
		t.Errorf("拒绝回退应指明恢复备份路径: %v", err)
	}
}

// 校验和被篡改须拒绝（AC3 之外的完整性要求）：已应用的迁移文件内容变化后，up/down/status 全部拒绝，且不动库。
func TestChecksumTamperIsRejected(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	if _, err := e.runner(t, fixtureFS(t, "good")).Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	before := e.schemaSignature(t)

	tampered := fixtureFS(t, "good", "extend/tampered")
	r := e.runner(t, tampered)
	if f := r.Findings(); len(f) != 0 {
		t.Fatalf("被篡改的夹具本身应能通过 lint（否则测的是 lint 而不是校验和）: %v", f)
	}
	for name, run := range map[string]func() error{
		"up":   func() error { _, err := r.Up(ctx, migrate.UpOptions{}); return err },
		"down": func() error { _, err := r.Down(ctx, migrate.DownOptions{}); return err },
	} {
		err := run()
		var cm *migrate.ErrChecksumMismatch
		if !errors.As(err, &cm) || cm.Version != 2 || cm.Recorded == "" || cm.Recorded == cm.Actual {
			t.Fatalf("%s: 期望校验和不一致错误，实际 %v", name, err)
		}
		if migrate.ExitCode(err) != migrate.ExitChecksum {
			t.Fatalf("%s: 退出码 = %d", name, migrate.ExitCode(err))
		}
	}
	if e.schemaSignature(t) != before || strings.Contains(e.createStmt(t, "widgets"), "idx_widgets_evil") {
		t.Fatal("篡改被拒绝时库结构不得变化，篡改的 SQL 不得被执行")
	}
	st, err := r.Status(ctx)
	if err != nil || st.Err() == nil || migrate.ExitCode(st.Err()) != migrate.ExitChecksum {
		t.Fatalf("status 应报告校验和问题: %+v %v", st, err)
	}
	var bad int
	for _, it := range st.Migrations {
		if it.ChecksumState == "mismatch" {
			bad++
			if it.Version != 2 {
				t.Errorf("只有 000002 被篡改: %+v", it)
			}
		}
	}
	if bad != 1 {
		t.Fatalf("status 应恰好标出 1 个 mismatch: %d", bad)
	}

	// 文件被删除（二进制没有数据库里已应用的迁移）同样拒绝。
	short := fixtureFS(t, "good")
	delete(short, "000005_widget_note_column.sql")
	_, err = e.runner(t, short).Up(ctx, migrate.UpOptions{})
	var ua *migrate.ErrUnknownApplied
	if !errors.As(err, &ua) || ua.Versions[0] != 5 {
		t.Fatalf("二进制缺少已应用迁移必须拒绝: %v", err)
	}
}

// lint 未通过的迁移集合：Up/Down/Recover fail-closed，数据库完全不被触碰。
func TestUpRefusesWhenLintFails(t *testing.T) {
	e := newEnv(t)
	fsys := fixtureFS(t, "good")
	fsys["000006_bad.sql"] = fixtureFS(t, "lint-fail/create-table-not-idempotent")["000001_a.sql"]
	r := e.runner(t, fsys)
	_, err := r.Up(ctxT(t), migrate.UpOptions{})
	var le *migrate.ErrLint
	if !errors.As(err, &le) || migrate.ExitCode(err) != migrate.ExitLint {
		t.Fatalf("lint 失败应拒绝执行: %v", err)
	}
	if len(e.tables(t)) != 0 {
		t.Fatalf("lint 失败时库必须保持为空: %v", e.tables(t))
	}
}

// 乱序：序号低于已应用最大版本的迁移尚未应用 → 拒绝（迁移必须单调追加）；退出码 10（数据库与二进制的迁移集合不一致）。
func TestOutOfOrderMigrationIsRejected(t *testing.T) {
	ctx := ctxT(t)
	e := newEnv(t)
	r := e.runner(t, fixtureFS(t, "good"))
	if _, err := r.Up(ctx, migrate.UpOptions{To: 3}); err != nil {
		t.Fatal(err)
	}
	// 模拟「000002 是后合并进来的」：它在版本表与运行记录里都不存在，而 000003 已应用。
	e.inst.Exec(t, "DELETE FROM `"+e.dbName+"`."+migrate.VersionTable+" WHERE version_id = 2")
	e.inst.Exec(t, "DELETE FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = 2")
	before := e.schemaSignature(t)
	for name, run := range map[string]func() error{
		"up":   func() error { _, err := r.Up(ctx, migrate.UpOptions{}); return err },
		"down": func() error { _, err := r.Down(ctx, migrate.DownOptions{}); return err },
		"plan": func() error { _, err := r.Plan(ctx); return err },
	} {
		err := run()
		var oo *migrate.ErrOutOfOrder
		if !errors.As(err, &oo) || len(oo.Missing) != 1 || oo.Missing[0] != 2 || oo.Max != 3 {
			t.Fatalf("%s: 乱序必须被拒绝: %v", name, err)
		}
		if migrate.ExitCode(err) != migrate.ExitOutOfSync {
			t.Fatalf("%s: 退出码 = %d", name, migrate.ExitCode(err))
		}
	}
	if e.schemaSignature(t) != before {
		t.Fatal("乱序被拒绝时库结构不得变化")
	}
	if st, _ := r.Status(ctx); st.Err() == nil || migrate.ExitCode(st.Err()) != migrate.ExitOutOfSync {
		t.Fatalf("status 应报告乱序: %+v", st)
	}
}

// 版本表与运行记录矛盾（运行记录称已应用、版本表没有）：拒绝，而不是在 markRunning 处撞上主键冲突。
func TestInconsistentVersionTableAndRunsIsRejected(t *testing.T) {
	ctx := ctxT(t)
	e := newEnv(t)
	r := e.runner(t, fixtureFS(t, "good"))
	if _, err := r.Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	e.inst.Exec(t, "DELETE FROM `"+e.dbName+"`."+migrate.VersionTable+" WHERE version_id = 5")
	_, err := r.Up(ctx, migrate.UpOptions{})
	var ic *migrate.ErrInconsistent
	if !errors.As(err, &ic) || ic.Version != 5 || migrate.ExitCode(err) != migrate.ExitOutOfSync {
		t.Fatalf("矛盾状态必须被类型化地拒绝: %v", err)
	}
}

// 二进制比数据库旧（数据库已应用二进制不认识的迁移）：退出码 10，Tools 据此区分「该换二进制/回滚」与普通失败。
func TestUnknownAppliedHasDedicatedExitCode(t *testing.T) {
	ctx := ctxT(t)
	e := newEnv(t)
	if _, err := e.runner(t, fixtureFS(t, "good")).Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	old := fixtureFS(t, "good")
	delete(old, "000005_widget_note_column.sql")
	_, err := e.runner(t, old).Up(ctx, migrate.UpOptions{})
	if migrate.ExitCode(err) != migrate.ExitOutOfSync {
		t.Fatalf("二进制比数据库旧应返回 %d: %v (%d)", migrate.ExitOutOfSync, err, migrate.ExitCode(err))
	}
}

// plan 是升级前的校验：dirty / 校验和不一致时不应给出一份看似正常的计划。
func TestPlanRefusesWhenDatabaseIsNotHealthy(t *testing.T) {
	ctx := ctxT(t)
	e := newEnv(t)
	good := fixtureFS(t, "good")
	r := e.runner(t, good)
	if _, err := r.Up(ctx, migrate.UpOptions{To: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runner(t, fixtureFS(t, "good", "extend/tampered")).Plan(ctx); !errors.As(err, new(*migrate.ErrChecksumMismatch)) {
		t.Fatalf("校验和不一致时 plan 必须拒绝: %v", err)
	}
	e.inst.Exec(t, "UPDATE `"+e.dbName+"`."+migrate.RunsTable+" SET status = 'failed', error = 'x' WHERE version = 2")
	if _, err := r.Plan(ctx); !errors.As(err, new(*migrate.ErrDirty)) {
		t.Fatalf("dirty 时 plan 必须拒绝: %v", err)
	}
}

// Down 的各种选择方式在真实库上逐一验证：--steps N、--to N（保留 N）、--all。
func TestDownSelectionOptions(t *testing.T) {
	ctx := ctxT(t)
	e := newEnv(t)
	r := e.runner(t, fixtureFS(t, "reversible"))
	if f := r.Findings(); len(f) != 0 {
		t.Fatalf("夹具 reversible 应通过 lint: %v", f)
	}
	up := func() {
		t.Helper()
		if _, err := r.Up(ctx, migrate.UpOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	up()
	wantVersions(t, e, "1", "2", "3", "4")

	res, err := r.Down(ctx, migrate.DownOptions{Steps: 2})
	if err != nil || len(res.Applied) != 2 || res.Applied[0].Version != 4 || res.Applied[1].Version != 3 || res.ToVersion != 2 {
		t.Fatalf("--steps 2: %+v %v", res, err)
	}
	wantVersions(t, e, "1", "2")
	up()

	res, err = r.Down(ctx, migrate.DownOptions{To: 1}) // 保留 1，回退 4、3、2
	if err != nil || len(res.Applied) != 3 || res.ToVersion != 1 {
		t.Fatalf("--to 1: %+v %v", res, err)
	}
	wantVersions(t, e, "1")
	if !e.hasTable(t, "rv_a") || e.hasTable(t, "rv_b") {
		t.Fatalf("--to 1 应保留 rv_a、删除 rv_b: %v", e.tables(t))
	}
	up()

	res, err = r.Down(ctx, migrate.DownOptions{All: true})
	if err != nil || len(res.Applied) != 4 || res.ToVersion != 0 {
		t.Fatalf("--all: %+v %v", res, err)
	}
	wantVersions(t, e)
	if e.hasTable(t, "rv_a") || e.hasTable(t, "rv_b") {
		t.Fatalf("--all 应删除全部业务表: %v", e.tables(t))
	}
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`."+migrate.RunsTable); got != "0" {
		t.Fatalf("全部回退后运行记录应清空: %s", got)
	}
	// 再次 down：无事可做。
	if res, err := r.Down(ctx, migrate.DownOptions{}); err != nil || len(res.Applied) != 0 {
		t.Fatalf("无可回退迁移: %+v %v", res, err)
	}
	up() // 回退后可重新升级（授权在 DROP TABLE 后仍在，CREATE 后同样可用）
	wantVersions(t, e, "1", "2", "3", "4")
}
