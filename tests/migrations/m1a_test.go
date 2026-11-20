package migrationstest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/migrations"
)

// ---- 静态：嵌入的真实迁移 ----

// 000001–000005 是真实迁移：序号/名称/分类/摘要正确，通过完整 lint（含 DDL 预检与 schema lint），仅 000004 含 DML，
// 且不含 irreversible（plan 的 has_irreversible=false，Tools 升级前无需强制备份确认）。
func TestM1AEmbeddedMigrationsAreLintCleanAndWellFormed(t *testing.T) {
	set, findings, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("lint: %s", f)
	}
	// 后续 Story（1.8/1.9）会继续追加 000006+：本测试只约束前五个（A 批），不随总数变化。
	if len(set.Migrations) < len(m1aFiles) {
		t.Fatalf("至少应有 %d 个迁移，实际 %d", len(m1aFiles), len(set.Migrations))
	}
	for i, m := range set.Migrations[:len(m1aFiles)] {
		if m.File != m1aFiles[i] || m.Version != int64(i+1) {
			t.Errorf("第 %d 个迁移 = %s（版本 %d），期望 %s", i+1, m.File, m.Version, m1aFiles[i])
		}
		if m.Class != migrate.ClassExpand {
			t.Errorf("%s 必须标 expand（AC1）: %s", m.File, m.Class)
		}
		if m.Summary == "" {
			t.Errorf("%s 缺少 lingedge:summary", m.File)
		}
		isSeed := m.File == "000004_seed_builtin.sql"
		var dml, ddl int
		for _, st := range m.Up {
			head := strings.ToUpper(strings.Fields(st.Text + " ")[0])
			switch head {
			case "INSERT", "UPDATE", "DELETE", "REPLACE":
				dml++
			case "CREATE", "ALTER", "DROP":
				ddl++
			}
		}
		if isSeed && (dml == 0 || ddl != 0) {
			t.Errorf("种子迁移必须仅含 DML：dml=%d ddl=%d", dml, ddl)
		}
		if !isSeed && dml != 0 {
			t.Errorf("%s 不得含 DML（种子只在 000004）：dml=%d", m.File, dml)
		}
	}
	// 计划（A 批）：全部为待执行、无不可逆。
	aSet, _, err := migrate.Load(toMapFS(fixtureFromEmbedded(t)))
	if err != nil {
		t.Fatal(err)
	}
	if p := migrate.BuildPlan(aSet, nil); p.Count != len(m1aFiles) || p.HasIrreversible || p.MaxClass != migrate.ClassExpand {
		t.Errorf("plan: %+v", p)
	}
}

// AC3：种子迁移不含任何口令、不创建任何用户；其它迁移不含任何数据写入。
func TestM1ASeedFileContainsNoCredentialsOrUsers(t *testing.T) {
	data, err := fs.ReadFile(migrations.FS, "000004_seed_builtin.sql")
	if err != nil {
		t.Fatal(err)
	}
	low := strings.ToLower(string(data))
	for _, bad := range []string{"password", "passwd", "secret", "token_hash", "totp", "identified by", "insert into users", "insert ignore into users", "into user_roles", "into sessions", "into api_tokens", "into tenant_members"} {
		if strings.Contains(low, bad) {
			t.Errorf("种子迁移不得含 %q（不含口令、不创建用户，管理员只由安装向导创建）", bad)
		}
	}
	for _, f := range m1aFiles {
		if f == "000004_seed_builtin.sql" {
			continue
		}
		b, _ := fs.ReadFile(migrations.FS, f)
		for _, ln := range strings.Split(string(b), "\n") {
			if u := strings.ToUpper(strings.TrimSpace(ln)); strings.HasPrefix(u, "INSERT ") || strings.HasPrefix(u, "REPLACE ") {
				t.Errorf("%s 不应含数据写入: %s", f, ln)
			}
		}
	}
}

// ---- 真实 MySQL：空库 up / 重放幂等 / 逐表授权 / 审计不可变 ----

func TestM1AUpOnEmptyDatabaseThenReplayIsIdempotent(t *testing.T) {
	ctx := ctxT(t)
	e, r := m1aUp(t)

	var business []string
	for _, tbl := range e.tables(t) {
		if !strings.HasPrefix(tbl, "schema_migrations") {
			business = append(business, tbl)
		}
	}
	want := append([]string(nil), m1aTables...)
	sort.Strings(want)
	if join(business) != join(want) {
		t.Fatalf("应恰好建出 21 张表\n实际 %v\n期望 %v", business, want)
	}
	wantVersions(t, e, "1", "2", "3", "4", "5")

	// 关键列：sessions.reauth_at（L4 再认证）；login_logs / task_events 为 C2u 带 uid；roles/user_roles 的 VIRTUAL 生成列。
	for _, c := range [][3]string{{"sessions", "reauth_at", "datetime"}, {"login_logs", "uid", "char"}, {"task_events", "uid", "char"},
		{"tenants", "owner_user_id", "bigint"}, {"audit_logs", "uid", "char"}} {
		if got := e.scalar(t, "SELECT DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?", e.dbName, c[0], c[1]); got != c[2] {
			t.Errorf("%s.%s 类型 = %q，期望 %s", c[0], c[1], got, c[2])
		}
	}
	for _, c := range [][3]string{{"roles", "uniq_key", "VIRTUAL GENERATED"}, {"user_roles", "tenant_key", "VIRTUAL GENERATED"},
		{"users", "active_email", "STORED GENERATED"}, {"tenants", "default_flag", "STORED GENERATED"}, {"signing_keys", "active_key", "STORED GENERATED"}} {
		if got := e.scalar(t, "SELECT EXTRA FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?", e.dbName, c[0], c[1]); got != c[2] {
			t.Errorf("%s.%s 的生成列类型 = %q，期望 %s", c[0], c[1], got, c[2])
		}
	}

	// 审计表分区：按月 RANGE 且含 pmax 兜底；触发器两个。
	parts := e.column(t, "SELECT PARTITION_NAME FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'audit_logs' ORDER BY PARTITION_ORDINAL_POSITION", e.dbName)
	if len(parts) != 15 || parts[0] != "p202611" || parts[len(parts)-2] != "p202712" || parts[len(parts)-1] != "pmax" {
		t.Errorf("audit_logs 分区 = %v（期望 p202611…p202712 + pmax）", parts)
	}
	if method := e.scalar(t, "SELECT DISTINCT PARTITION_METHOD FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'audit_logs'", e.dbName); method != "RANGE COLUMNS" {
		t.Errorf("分区方法 = %q", method)
	}
	if trg := e.column(t, "SELECT TRIGGER_NAME FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY TRIGGER_NAME", e.dbName); join(trg) != "trg_audit_logs_bd,trg_audit_logs_bu" {
		t.Errorf("触发器 = %v", trg)
	}

	// ---- 逐表授权（AC4）：lingedge_app 21 条表级授权；audit_logs 仅 SELECT, INSERT；lingedge_maint 仅 audit_logs ----
	app := e.grantsOf(t, migrate.AppUser)
	if len(app) != len(m1aTables) {
		t.Fatalf("app 的表级授权应恰为 %d 条，实际 %d: %v", len(m1aTables), len(app), app)
	}
	for _, tbl := range m1aTables {
		w := "Delete,Insert,Select,Update"
		if tbl == "audit_logs" {
			w = "Insert,Select"
		}
		if app[tbl] != w {
			t.Errorf("app 对 %s 的授权 = %q，期望 %q", tbl, app[tbl], w)
		}
	}
	if maint := e.grantsOf(t, migrate.MaintUser); len(maint) != 1 || maint["audit_logs"] != "Alter,Drop,Select" {
		t.Errorf("maint 只应有 audit_logs 的 SELECT, ALTER, DROP: %v", maint)
	}
	// 两个运行期账号没有任何库级/全局授权（最小权限：不能越过逐表授权）。
	for _, u := range []string{migrate.AppUser, migrate.MaintUser} {
		for _, g := range e.column(t, "SHOW GRANTS FOR '"+u+"'@'%'") {
			if strings.Contains(g, "ON *.*") && !strings.HasPrefix(g, "GRANT USAGE ON *.*") {
				t.Errorf("%s 不得有全局授权: %s", u, g)
			}
			if strings.Contains(g, "ON `"+e.dbName+"`.*") {
				t.Errorf("%s 不得有库级授权: %s", u, g)
			}
		}
	}

	// ---- 真实授权的最小权限负向用例 ----
	ap := e.appPool(t).Executor(ctx)
	tenantID := e.scalar(t, "SELECT id FROM `"+e.dbName+"`.tenants WHERE is_default = 1")
	if _, err := ap.Exec(ctx, db.Q("INSERT INTO audit_logs (uid, actor_type, actor_label, action, resource_type, result, request_id) VALUES (?, 'system', 'test', 'test.insert', 'test', 'success', ?)"), uid26(), uid26()); err != nil {
		t.Fatalf("app 应能 INSERT audit_logs: %v", err)
	}
	var auditRows int
	if err := ap.Get(ctx, &auditRows, db.Q("SELECT COUNT(*) FROM audit_logs")); err != nil || auditRows != 1 {
		t.Fatalf("app 应能 SELECT audit_logs: %d %v", auditRows, err)
	}
	denied := func(name string, q db.Query, args ...any) {
		t.Helper()
		_, err := ap.Exec(ctx, q, args...)
		if n, ok := mysqlErr(err); !ok || n != db.MySQLErrTableAccessDenied {
			t.Errorf("app %s 必须被表级授权拒绝（ERROR 1142）: %v", name, err)
		}
	}
	denied("UPDATE audit_logs", db.Q("UPDATE audit_logs SET action = 'tampered'"))
	denied("DELETE audit_logs", db.Q("DELETE FROM audit_logs"))
	denied("TRUNCATE audit_logs", db.Q("TRUNCATE TABLE audit_logs"))
	denied("DROP PARTITION", db.Q("ALTER TABLE audit_logs DROP PARTITION p202611"))
	denied("DROP TABLE users", db.Q("DROP TABLE users"))
	denied("ALTER TABLE users", db.Q("ALTER TABLE users ADD COLUMN evil INT NULL"))
	denied("CREATE TABLE", db.Q("CREATE TABLE evil (id INT)"))
	denied("DROP TRIGGER", db.Q("DROP TRIGGER trg_audit_logs_bu"))
	if _, err := ap.Exec(ctx, db.Q("SELECT * FROM schema_migrations")); err == nil {
		t.Error("app 不得读取迁移版本表")
	}
	// app 对业务表的 DML 照常可用（逐表授权没有过窄）。
	if _, err := ap.Exec(ctx, db.Q("INSERT INTO tenant_quotas (tenant_id, quota_key, limit_value) VALUES (?, 'sites', NULL)"), tenantID); err != nil {
		t.Errorf("app 应能写 tenant_quotas: %v", err)
	}
	if _, err := ap.Exec(ctx, db.Q("DELETE FROM tenant_quotas WHERE tenant_id = ?"), tenantID); err != nil {
		t.Errorf("app 应能删 tenant_quotas: %v", err)
	}

	// 迁移账号（持有 ALL）也不能改审计：触发器是第二道防线。
	_, err := e.admin(t).ExecContext(ctx, "UPDATE `"+e.dbName+"`.audit_logs SET action = 'x'")
	if n, ok := mysqlErr(err); !ok || n != errSignal || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("root UPDATE audit_logs 应被触发器拒绝（1644 immutable）: %v", err)
	}
	_, err = e.admin(t).ExecContext(ctx, "DELETE FROM `"+e.dbName+"`.audit_logs")
	if n, ok := mysqlErr(err); !ok || n != errSignal || !strings.Contains(err.Error(), "delete forbidden") {
		t.Errorf("root DELETE audit_logs 应被触发器拒绝（1644）: %v", err)
	}

	// ---- lingedge_maint：可对 audit_logs REORGANIZE / DROP PARTITION，不能碰其它表（AC4）----
	mx := e.maintPool(t).Executor(ctx)
	mdenied := func(name string, q db.Query) {
		t.Helper()
		_, err := mx.Exec(ctx, q)
		if n, ok := mysqlErr(err); !ok || n != db.MySQLErrTableAccessDenied {
			t.Errorf("maint %s 必须被拒绝（ERROR 1142）: %v", name, err)
		}
	}
	mdenied("DROP TABLE users", db.Q("DROP TABLE users"))
	mdenied("DROP TABLE tenants", db.Q("DROP TABLE tenants"))
	mdenied("DELETE FROM audit_logs", db.Q("DELETE FROM audit_logs"))
	mdenied("UPDATE audit_logs", db.Q("UPDATE audit_logs SET action = 'x'"))
	mdenied("INSERT audit_logs", db.Q("INSERT INTO audit_logs (uid) VALUES ('x')"))
	mdenied("SELECT users", db.Q("SELECT 1 FROM users"))
	mdenied("ALTER users", db.Q("ALTER TABLE users ADD COLUMN evil INT NULL"))
	if _, err := mx.Exec(ctx, db.Q("ALTER TABLE audit_logs REORGANIZE PARTITION pmax INTO (PARTITION p202801 VALUES LESS THAN ('2028-02-01 00:00:00'), PARTITION pmax VALUES LESS THAN (MAXVALUE))")); err != nil {
		t.Errorf("maint 应能 REORGANIZE pmax（滚动预建分区）: %v", err)
	}
	// 保留期清理：该行落在 p202611（当前年月之前的分区）；DROP PARTITION 不触发行级 DELETE 触发器，行随分区消失。
	if _, err := e.admin(t).ExecContext(ctx, "INSERT INTO `"+e.dbName+"`.audit_logs (uid, created_at, actor_type, actor_label, action, resource_type, result, request_id) VALUES (?, '2026-11-20 10:00:00.000', 'system', 'old', 'old.row', 'test', 'success', ?)", uid26(), uid26()); err != nil {
		t.Fatal(err)
	}
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.audit_logs PARTITION (p202611) WHERE action = 'old.row'"); got != "1" {
		t.Fatalf("p202611 应含该行: %s", got)
	}
	if _, err := mx.Exec(ctx, db.Q("ALTER TABLE audit_logs DROP PARTITION p202611")); err != nil {
		t.Errorf("maint 应能 DROP PARTITION（保留期清理，不受 DELETE 触发器影响）: %v", err)
	}
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.audit_logs WHERE action = 'old.row'"); got != "0" {
		t.Errorf("DROP PARTITION 后旧行应消失: %s", got)
	}

	// ---- 重放幂等（AC5）：Up 再次为空操作；把全部迁移的每条语句直接再执行一遍，库结构/授权/种子均不变 ----
	sigBefore := e.schemaSignature(t)
	seedBefore := e.seedCounts(t)
	if res, err := r.Up(ctx, migrate.UpOptions{}); err != nil || len(res.Applied) != 0 {
		t.Fatalf("第二次 Up 应为空操作: %+v %v", res, err)
	}
	c := e.conn(t)
	var replayed int
	for _, m := range r.Set().Migrations {
		for _, st := range m.Up {
			if strings.Contains(st.Lead, "lingedge:guard=") {
				continue
			}
			if _, err := c.ExecContext(ctx, st.Text); err != nil {
				t.Fatalf("%s 第 %d 行重放失败（迁移必须可重入）: %v\n%.120s", m.File, st.Line, err, st.Text)
			}
			replayed++
		}
	}
	if replayed < 40 {
		t.Fatalf("重放语句数异常: %d", replayed)
	}
	// 重放会重建触发器与授权；分区已被维护账号改动过（p202611 已 DROP、新增 p202801），CREATE TABLE IF NOT EXISTS 不会改回——这正是幂等所要的。
	sigAfter := e.schemaSignature(t)
	if sigAfter != sigBefore {
		t.Error("重放全部可重入语句后库结构与授权不得变化")
	}
	if seedAfter := e.seedCounts(t); seedAfter != seedBefore {
		t.Errorf("重放种子后行数不得变化: %s → %s", seedBefore, seedAfter)
	}

	// expand 迁移不提供数据库回退（切回旧二进制即可，ADR-013 §1）。
	_, err = r.Down(ctx, migrate.DownOptions{})
	var dn *migrate.ErrDownNotAllowed
	if !errors.As(err, &dn) || dn.File != "000005_keys_credentials.sql" {
		t.Errorf("expand 应拒绝回退: %v", err)
	}
	if p, _ := r.Plan(ctx); p.Count != 0 {
		t.Errorf("plan 应为空: %+v", p)
	}
}

// seedCounts 汇总种子相关表的行数，便于重放前后比较。
func (e *env) seedCounts(t testing.TB) string {
	t.Helper()
	var parts []string
	for _, tbl := range []string{"permissions", "roles", "role_permissions", "tenants", "users"} {
		parts = append(parts, tbl+"="+e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.`"+tbl+"`"))
	}
	return strings.Join(parts, " ")
}

// 篡改已应用的真实迁移被校验和拒绝（任一已应用文件）。
func TestM1ATamperedAppliedMigrationIsRejected(t *testing.T) {
	ctx := ctxT(t)
	e, _ := m1aUp(t)
	tampered := fixtureFromEmbedded(t)
	tampered["000001_identity_core.sql"].Data = append(tampered["000001_identity_core.sql"].Data, []byte("-- 悄悄加的一行注释\n")...)
	_, err := e.runner(t, toMapFS(tampered)).Up(ctx, migrate.UpOptions{})
	var cm *migrate.ErrChecksumMismatch
	if !errors.As(err, &cm) || cm.Version != 1 {
		t.Fatalf("改动已应用的 000001 必须被校验和拒绝: %v", err)
	}
}

// ---- 真实 MySQL：schema 约定（AC2，information_schema 实测）与种子一致性 ----

func TestM1ASchemaConventionsHoldOnRealDatabase(t *testing.T) {
	e, _ := m1aUp(t)
	db := e.dbName

	// 无 ENUM/SET/浮点/定点/TIMESTAMP/TIME 列。
	if bad := e.column(t, "SELECT CONCAT(TABLE_NAME, '.', COLUMN_NAME, ':', DATA_TYPE) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME NOT LIKE 'schema_migrations%' AND DATA_TYPE IN ('enum','set','float','double','decimal','timestamp','time')", db); len(bad) != 0 {
		t.Errorf("存在违反类型约定的列: %v", bad)
	}
	// 时间列一律 DATETIME(3)；date 仅允许（本批没有）。
	if bad := e.column(t, "SELECT CONCAT(TABLE_NAME, '.', COLUMN_NAME) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME NOT LIKE 'schema_migrations%' AND DATA_TYPE = 'datetime' AND DATETIME_PRECISION <> 3", db); len(bad) != 0 {
		t.Errorf("DATETIME 必须是 (3): %v", bad)
	}
	if n := e.scalar(t, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME NOT LIKE 'schema_migrations%' AND DATA_TYPE = 'datetime'", db); n == "0" {
		t.Error("应存在 DATETIME(3) 列")
	}
	// TEXT/JSON/BLOB 无任何 DEFAULT（字面量或表达式）。
	if bad := e.column(t, "SELECT CONCAT(TABLE_NAME, '.', COLUMN_NAME) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME NOT LIKE 'schema_migrations%' AND DATA_TYPE IN ('tinytext','text','mediumtext','longtext','tinyblob','blob','mediumblob','longblob','json') AND (COLUMN_DEFAULT IS NOT NULL OR EXTRA LIKE '%DEFAULT_GENERATED%')", db); len(bad) != 0 {
		t.Errorf("TEXT/JSON/BLOB 列不得带 DEFAULT: %v", bad)
	}
	// 全部 InnoDB、utf8mb4_0900_ai_ci、有主键。
	if bad := e.column(t, "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME NOT LIKE 'schema_migrations%' AND (ENGINE <> 'InnoDB' OR TABLE_COLLATION <> 'utf8mb4_0900_ai_ci')", db); len(bad) != 0 {
		t.Errorf("引擎/排序规则不符: %v", bad)
	}
	if bad := e.column(t, "SELECT t.TABLE_NAME FROM information_schema.TABLES t WHERE t.TABLE_SCHEMA = ? AND t.TABLE_NAME NOT LIKE 'schema_migrations%' AND NOT EXISTS (SELECT 1 FROM information_schema.TABLE_CONSTRAINTS c WHERE c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME AND c.CONSTRAINT_TYPE = 'PRIMARY KEY')", db); len(bad) != 0 {
		t.Errorf("缺主键: %v", bad)
	}

	// 所有指向 tenants(id) 的外键 ON DELETE RESTRICT（README §2.4）；所有外键无 ON UPDATE CASCADE/SET NULL。
	fks := e.column(t, "SELECT CONCAT(TABLE_NAME, '.', CONSTRAINT_NAME, ':', DELETE_RULE) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND REFERENCED_TABLE_NAME = 'tenants' ORDER BY 1", db)
	wantFK := []string{"api_tokens.fk_api_tokens_tenants:RESTRICT", "roles.fk_roles_tenants:RESTRICT", "sessions.fk_sessions_tenants:RESTRICT", "tasks.fk_tasks_tenants:RESTRICT",
		"tenant_members.fk_tenant_members_tenants:RESTRICT", "tenant_quotas.fk_tenant_quotas_tenants:RESTRICT", "user_roles.fk_user_roles_tenants:RESTRICT"}
	if join(fks) != join(wantFK) {
		t.Errorf("指向 tenants 的外键 = %v\n期望     %v", fks, wantFK)
	}
	if bad := e.column(t, "SELECT CONCAT(TABLE_NAME, '.', CONSTRAINT_NAME, ':', UPDATE_RULE) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND UPDATE_RULE IN ('CASCADE','SET NULL','SET DEFAULT')", db); len(bad) != 0 {
		t.Errorf("外键不得 ON UPDATE CASCADE/SET NULL（ERROR 3823 陷阱）: %v", bad)
	}
	// 分区表无外键（任一端）：audit_logs 既不引用也不被引用。
	if n := e.scalar(t, "SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND (TABLE_NAME = 'audit_logs' OR REFERENCED_TABLE_NAME = 'audit_logs')", db); n != "0" {
		t.Errorf("audit_logs 不得有外键: %s", n)
	}
	// 高频无外键表。
	for _, tbl := range []string{"login_logs", "event_outbox", "job_locks"} {
		if n := e.scalar(t, "SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND (TABLE_NAME = ? OR REFERENCED_TABLE_NAME = ?)", db, tbl, tbl); n != "0" {
			t.Errorf("%s 不得有外键: %s", tbl, n)
		}
	}

	// 租户表（tenant_id NOT NULL）：主键之外的所有索引以 tenant_id 打头；唯一例外是登记的 idx_tenant_members_user
	// 与外键自动建立的支撑索引（索引名 = 外键名，如 invited_by 的 fk_tenant_members_inviter）。
	tenantTbls := e.column(t, "SELECT TABLE_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND COLUMN_NAME = 'tenant_id' AND IS_NULLABLE = 'NO' AND TABLE_NAME NOT LIKE 'schema_migrations%' ORDER BY 1", db)
	if join(tenantTbls) != "tenant_members,tenant_quotas" {
		t.Fatalf("tenant_id NOT NULL 的租户表 = %v，期望 tenant_members,tenant_quotas", tenantTbls)
	}
	for _, tbl := range tenantTbls {
		bad := e.column(t, `SELECT DISTINCT CONCAT(s.INDEX_NAME, '(', s.COLUMN_NAME, ')') FROM information_schema.STATISTICS s
			WHERE s.TABLE_SCHEMA = ? AND s.TABLE_NAME = ? AND s.INDEX_NAME <> 'PRIMARY' AND s.SEQ_IN_INDEX = 1 AND s.COLUMN_NAME <> 'tenant_id'
			AND s.INDEX_NAME NOT IN (SELECT CONSTRAINT_NAME FROM information_schema.TABLE_CONSTRAINTS WHERE TABLE_SCHEMA = s.TABLE_SCHEMA AND TABLE_NAME = s.TABLE_NAME AND CONSTRAINT_TYPE = 'FOREIGN KEY')`, db, tbl)
		if tbl == "tenant_members" {
			if join(bad) != "idx_tenant_members_user(user_id)" {
				t.Errorf("tenant_members 的非 tenant_id 打头索引应仅为登记的 idx_tenant_members_user: %v", bad)
			}
		} else if len(bad) != 0 {
			t.Errorf("%s 存在未以 tenant_id 打头的索引: %v", tbl, bad)
		}
	}
	// 业务上有「站点/租户」维度的表：T? 表保留 tenant_id 可空 + 外键 RESTRICT（平台级数据 tenant_id=NULL）。
	for _, tbl := range []string{"roles", "user_roles", "sessions", "api_tokens", "tasks"} {
		if got := e.scalar(t, "SELECT IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = 'tenant_id'", db, tbl); got != "YES" {
			t.Errorf("%s.tenant_id 应为可空（T?：NULL = 平台级）: %q", tbl, got)
		}
	}
}

func TestM1ASeedDataMatchesOpenAPIAndRules(t *testing.T) {
	e, _ := m1aUp(t)
	oa := loadOpenAPI(t)

	// ---- 权限字典 = OpenAPI 全部 x-permissions + tenant:impersonate；scope 与 operation 的 x-tenant-scope 一致 ----
	type permRow struct{ scope, resource, action, module string }
	rows, err := e.admin(t).QueryContext(ctxT(t), "SELECT code, scope, resource, action, module FROM `"+e.dbName+"`.permissions")
	if err != nil {
		t.Fatal(err)
	}
	perms := map[string]permRow{}
	for rows.Next() {
		var code string
		var p permRow
		if err := rows.Scan(&code, &p.scope, &p.resource, &p.action, &p.module); err != nil {
			t.Fatal(err)
		}
		perms[code] = p
	}
	_ = rows.Close()
	for code, p := range oa.perms {
		got, ok := perms[code]
		if !ok {
			t.Errorf("OpenAPI 引用的权限 %s 未种子（ADR-018 §8：x-permissions 均须存在于种子）", code)
			continue
		}
		if want := p.derivedScope(); got.scope != want {
			t.Errorf("权限 %s 的 scope = %s，按 operation 的 x-tenant-scope 应为 %s", code, got.scope, want)
		}
		// 代租户（acting-as）规则：被 tenant/either 路由使用的权限 scope 必须是 tenant 或 both。
		if (p.scopes["tenant"] || p.scopes["either"]) && got.scope == "platform" {
			t.Errorf("权限 %s 被租户可达路由使用，scope 不得为 platform", code)
		}
	}
	var extra []string
	for code := range perms {
		if _, ok := oa.perms[code]; !ok {
			extra = append(extra, code)
		}
	}
	if join(extra) != "tenant:impersonate" {
		t.Errorf("种子中多于 OpenAPI 的权限应仅为 tenant:impersonate: %v", extra)
	}
	if p := perms["tenant:impersonate"]; p.scope != "platform" {
		t.Errorf("tenant:impersonate 的 scope 必须是 platform: %+v", p)
	}
	for code, p := range perms { // 一致性：code = resource:action（库内已有 CHECK，这里再验种子口径）
		if code != p.resource+":"+p.action || p.module == "" {
			t.Errorf("权限 %s 的 resource/action/module 不一致: %+v", code, p)
		}
	}

	// ---- 内置角色：6 个，tenant_id NULL，uid 为合法 ULID，内置标记 ----
	roleRows := e.column(t, "SELECT CONCAT(scope, '/', code, '/', is_builtin, '/', IF(tenant_id IS NULL, 'null', 'x'), '/', IF(uid REGEXP '^[0-9A-HJKMNP-TV-Z]{26}$', 'ulid', uid)) FROM roles ORDER BY scope, code")
	wantRoles := []string{"platform/platform_admin/1/null/ulid", "platform/platform_auditor/1/null/ulid", "platform/platform_operator/1/null/ulid",
		"tenant/site_operator/1/null/ulid", "tenant/tenant_owner/1/null/ulid", "tenant/tenant_viewer/1/null/ulid"}
	if join(roleRows) != join(wantRoles) {
		t.Errorf("内置角色 = %v\n期望 %v", roleRows, wantRoles)
	}
	grantsOf := func(role string) map[string]bool {
		out := map[string]bool{}
		for _, c := range e.column(t, "SELECT p.code FROM role_permissions rp JOIN roles r ON r.id = rp.role_id JOIN permissions p ON p.id = rp.permission_id WHERE r.code = ?", role) {
			out[c] = true
		}
		return out
	}
	// platform_admin：全部权限。
	admin := grantsOf("platform_admin")
	if len(admin) != len(perms) {
		t.Errorf("platform_admin 应含全部 %d 项权限，实际 %d", len(perms), len(admin))
	}
	// 租户角色只含 scope ∈ {tenant, both} 的权限（platform 权限永不进入租户角色，ADR-018 §8）。
	for _, role := range []string{"tenant_owner", "site_operator", "tenant_viewer"} {
		g := grantsOf(role)
		if len(g) == 0 {
			t.Errorf("%s 没有任何权限", role)
		}
		for code := range g {
			if perms[code].scope == "platform" {
				t.Errorf("租户角色 %s 不得持有 platform 权限 %s", role, code)
			}
		}
	}
	// tenant_owner = 全部 tenant/both 权限；tenant_viewer = 其中 action=read；site_operator ⊂ owner 且不含 site:delete。
	wantOwner, wantViewer := map[string]bool{}, map[string]bool{}
	for code, p := range perms {
		if p.scope != "platform" {
			wantOwner[code] = true
			if p.action == "read" && code != "credential:read" { // 只读成员不含第三方凭据，与平台运维/审计员口径一致
				wantViewer[code] = true
			}
		}
	}
	if got := grantsOf("tenant_owner"); join(sortedKeys(got)) != join(sortedKeys(wantOwner)) {
		t.Errorf("tenant_owner 授权集合不符（实际 %d 项，期望 %d 项）", len(got), len(wantOwner))
	}
	if got := grantsOf("tenant_viewer"); join(sortedKeys(got)) != join(sortedKeys(wantViewer)) {
		t.Errorf("tenant_viewer 授权集合不符（实际 %d 项，期望 %d 项）", len(got), len(wantViewer))
	}
	so := grantsOf("site_operator")
	for code := range so {
		if !wantOwner[code] {
			t.Errorf("site_operator 的权限 %s 不在 tenant_owner 内", code)
		}
	}
	if so["site:delete"] || !so["site:write"] || !so["config:publish"] || !so["token:manage"] {
		t.Errorf("site_operator 授权不符: site:delete=%v site:write=%v config:publish=%v token:manage=%v", so["site:delete"], so["site:write"], so["config:publish"], so["token:manage"])
	}
	// 审计员：只读；平台运维：含 impersonate 但无用户/角色/设置/租户写权限。
	for code := range grantsOf("platform_auditor") {
		if perms[code].action != "read" {
			t.Errorf("platform_auditor 不得有写权限 %s", code)
		}
	}
	op := grantsOf("platform_operator")
	for _, forbid := range []string{"user:write", "role:write", "setting:write", "tenant:manage", "tenant:member:manage", "credential:read", "site:delete"} {
		if op[forbid] {
			t.Errorf("platform_operator 不应持有 %s", forbid)
		}
	}
	if !op["tenant:impersonate"] || !op["config:publish"] || !op["node:manage"] {
		t.Error("platform_operator 应含 tenant:impersonate、config:publish、node:manage")
	}
	// tenant:impersonate 只授予 platform_admin 与 platform_operator。
	if holders := e.column(t, "SELECT r.code FROM role_permissions rp JOIN roles r ON r.id = rp.role_id JOIN permissions p ON p.id = rp.permission_id WHERE p.code = 'tenant:impersonate' ORDER BY 1"); join(holders) != "platform_admin,platform_operator" {
		t.Errorf("tenant:impersonate 的持有角色 = %v", holders)
	}

	// ---- 默认租户（AC1）：恰一个，is_default=1，未绑定 owner（由安装向导回填）----
	if got := e.scalar(t, "SELECT CONCAT(COUNT(*), '|', MIN(slug), '|', MIN(is_default), '|', MIN(default_flag), '|', MIN(status), '|', IF(MIN(owner_user_id) IS NULL, 'noowner', 'owner'), '|', IF(MIN(uid) REGEXP '^[0-9A-HJKMNP-TV-Z]{26}$', 'ulid', 'bad')) FROM `"+e.dbName+"`.tenants"); got != "1|default|1|1|active|noowner|ulid" {
		t.Errorf("默认租户 = %q", got)
	}

	// ---- AC3：不创建任何用户/会话/令牌/成员/配额/密钥 ----
	for _, tbl := range []string{"users", "user_recovery_codes", "user_roles", "sessions", "api_tokens", "tenant_members", "tenant_quotas", "signing_keys", "system_settings", "tasks", "audit_logs", "login_logs"} {
		if n := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.`"+tbl+"`"); n != "0" {
			t.Errorf("种子迁移之后 %s 应为空（管理员只由安装向导创建）: %s", tbl, n)
		}
	}

	// ---- tasks.type 的 CHECK 取值 == OpenAPI TaskType 枚举（唯一事实源）----
	clause := e.scalar(t, "SELECT cc.CHECK_CLAUSE FROM information_schema.CHECK_CONSTRAINTS cc WHERE cc.CONSTRAINT_SCHEMA = ? AND cc.CONSTRAINT_NAME = 'ck_tasks_type'", e.dbName)
	var got []string
	for _, m := range quoteRe.FindAllStringSubmatch(clause, -1) {
		got = append(got, m[1])
	}
	want := append([]string(nil), oa.taskType...)
	sort.Strings(got)
	sort.Strings(want)
	if join(got) != join(want) {
		t.Errorf("tasks.type 的 CHECK 取值与 OpenAPI TaskType 不一致\nCHECK   %v\nOpenAPI %v", got, want)
	}
}

// ---- 真实 MySQL：约束 / 外键 / CHECK / 租户隔离相关约束的负向用例 ----

func TestM1AConstraintsOnRealDatabase(t *testing.T) {
	e, _ := m1aUp(t)
	c := e.conn(t)
	oa := loadOpenAPI(t)
	run := func(name string, fn func(c *cx)) {
		t.Run(name, func(t *testing.T) { inTx(t, e, c, fn) })
	}

	run("users：CHECK 与活动邮箱唯一", func(c *cx) {
		ins := "INSERT INTO users (uid, user_type, email, display_name, password_hash, status, totp_enabled, totp_secret_enc) VALUES (?, ?, ?, 'x', ?, ?, ?, ?)"
		c.ok(ins, uid26(), "platform", "ok@example.test", c.pw, "active", 0, nil)
		c.bad(errCheckViolated, ins, uid26(), "robot", "r@example.test", c.pw, "active", 0, nil)      // user_type
		c.bad(errCheckViolated, ins, uid26(), "tenant", "s@example.test", c.pw, "frozen", 0, nil)     // status
		c.bad(errCheckViolated, ins, uid26(), "tenant", "Upper@example.test", c.pw, "active", 0, nil) // 邮箱须小写
		c.bad(errCheckViolated, ins, uid26(), "tenant", "t@example.test", c.pw, "active", 1, nil)     // 启用 TOTP 须有密钥
		c.ok(ins, uid26(), "tenant", "t@example.test", c.pw, "active", 1, "v1:k:wrapped:nonce:ct")    // 有密钥则通过
		c.bad(errDup, ins, uid26(), "tenant", "ok@example.test", c.pw, "active", 0, nil)              // 活动邮箱唯一
	})
	run("users：软删除后邮箱可复用，uid 唯一", func(c *cx) {
		id := c.user("reuse@example.test")
		c.bad(errDup, "INSERT INTO users (uid, user_type, email, display_name, password_hash) VALUES (?, 'tenant', 'reuse@example.test', 'x', ?)", uid26(), c.pw)
		c.ok("UPDATE users SET deleted_at = NOW(3) WHERE id = ?", id)
		c.user("reuse@example.test") // active_email 为 NULL 的软删除行不占用唯一性
		if n := c.count("SELECT COUNT(*) FROM users WHERE email = 'reuse@example.test'"); n != 2 {
			t.Errorf("应有两行（一行已软删除）: %d", n)
		}
		u := uid26()
		c.ok("INSERT INTO users (uid, user_type, email, display_name, password_hash) VALUES (?, 'tenant', 'u1@example.test', 'x', ?)", u, c.pw)
		c.bad(errDup, "INSERT INTO users (uid, user_type, email, display_name, password_hash) VALUES (?, 'tenant', 'u2@example.test', 'x', ?)", u, c.pw)
	})
	run("users：必填列无默认值（口令哈希必须显式提供，不存在默认口令）", func(c *cx) {
		c.bad(errNoDefault, "INSERT INTO users (uid, user_type, email, display_name) VALUES (?, 'tenant', 'np@example.test', 'x')", uid26())
	})
	run("tenants：默认租户唯一、slug 格式、状态、owner 外键", func(c *cx) {
		c.bad(errDup, "INSERT INTO tenants (uid, name, slug, is_default) VALUES (?, 'second', 'second', 1)", uid26()) // 种子已有默认租户
		c.ok("INSERT INTO tenants (uid, name, slug, is_default) VALUES (?, 'ok', 'ok-1', 0)", uid26())
		c.ok("INSERT INTO tenants (uid, name, slug, is_default) VALUES (?, 'ok2', 'ok-2', 0)", uid26()) // 非默认可有多个（default_flag 为 NULL）
		for _, slug := range []string{"Bad", "-bad", "a_b", "has space", "x.y", ""} {
			c.bad(errCheckViolated, "INSERT INTO tenants (uid, name, slug) VALUES (?, 'n', ?)", uid26(), slug)
		}
		c.bad(1406, "INSERT INTO tenants (uid, name, slug) VALUES (?, 'n', ?)", uid26(), strings.Repeat("a", 64)) // 超长：VARCHAR(63)
		c.ok("INSERT INTO tenants (uid, name, slug) VALUES (?, 'n', ?)", uid26(), strings.Repeat("a", 63))
		c.bad(errDup, "INSERT INTO tenants (uid, name, slug) VALUES (?, 'n', 'ok-1')", uid26())
		c.bad(errCheckViolated, "INSERT INTO tenants (uid, name, slug, status) VALUES (?, 'n', 'st-1', 'gone')", uid26())
		c.bad(errNoRefRow, "INSERT INTO tenants (uid, name, slug, owner_user_id) VALUES (?, 'n', 'own-1', 999999999)", uid26())
		u := c.user("owner@example.test")
		tid := c.ok("INSERT INTO tenants (uid, name, slug, owner_user_id) VALUES (?, 'n', 'own-2', ?)", uid26(), u)
		c.ok("DELETE FROM users WHERE id = ?", u) // owner_user_id → SET NULL
		if !c.isNull("SELECT owner_user_id FROM tenants WHERE id = ?", tid) {
			t.Error("删除 owner 用户后 tenants.owner_user_id 应置 NULL")
		}
	})
	run("tenant_members / tenant_quotas：租户隔离相关约束", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		u1, u2 := c.user("m1@example.test"), c.user("m2@example.test")
		c.ok("INSERT INTO tenant_members (tenant_id, user_id) VALUES (?, ?)", t1, u1)
		c.ok("INSERT INTO tenant_members (tenant_id, user_id) VALUES (?, ?)", t2, u1) // 同一用户可属多个租户
		c.bad(errDup, "INSERT INTO tenant_members (tenant_id, user_id) VALUES (?, ?)", t1, u1)
		c.bad(errNoRefRow, "INSERT INTO tenant_members (tenant_id, user_id) VALUES (999999999, ?)", u2)
		c.bad(errNoRefRow, "INSERT INTO tenant_members (tenant_id, user_id) VALUES (?, 999999999)", u2)
		c.bad(errCheckViolated, "INSERT INTO tenant_members (tenant_id, user_id, status) VALUES (?, ?, 'banned')", t1, u2)
		c.bad(errNoRefRow, "INSERT INTO tenant_members (tenant_id, user_id, invited_by) VALUES (?, ?, 999999999)", t1, u2)
		c.bad(errRowIsRef, "DELETE FROM tenants WHERE id = ?", t1) // 外键 RESTRICT：有成员的租户不可物理删除
		c.ok("DELETE FROM users WHERE id = ?", u1)                 // 用户删除级联其成员关系
		if n := c.count("SELECT COUNT(*) FROM tenant_members WHERE user_id = ?", u1); n != 0 {
			t.Errorf("删除用户应级联删除成员关系: %d", n)
		}
		c.ok("DELETE FROM tenants WHERE id = ?", t1) // 成员已清空后可删

		c.ok("INSERT INTO tenant_quotas (tenant_id, quota_key, limit_value) VALUES (?, 'sites', 10)", t2)
		c.ok("INSERT INTO tenant_quotas (tenant_id, quota_key) VALUES (?, 'domains_per_site')", t2) // NULL = 不限
		t3 := c.tenant()
		c.ok("INSERT INTO tenant_quotas (tenant_id, quota_key, limit_value) VALUES (?, 'sites', 5)", t3) // 不同租户同键允许
		c.bad(errDup, "INSERT INTO tenant_quotas (tenant_id, quota_key, limit_value) VALUES (?, 'sites', 99)", t2)
		c.bad(errCheckViolated, "INSERT INTO tenant_quotas (tenant_id, quota_key, limit_value) VALUES (?, 'neg', -1)", t2)
		c.bad(errCheckViolated, "INSERT INTO tenant_quotas (tenant_id, quota_key, source) VALUES (?, 'src', 'magic')", t2)
		c.bad(errNoRefRow, "INSERT INTO tenant_quotas (tenant_id, quota_key) VALUES (999999999, 'sites')")
		c.bad(errRowIsRef, "DELETE FROM tenants WHERE id = ?", t2)
	})
	run("permissions：scope 与 code 一致性", func(c *cx) {
		ins := "INSERT INTO permissions (code, resource, action, scope, module) VALUES (?, ?, ?, ?, 'm')"
		c.ok(ins, "demo:read", "demo", "read", "both")
		c.bad(errDup, ins, "demo:read", "demo", "read", "tenant")
		c.bad(errCheckViolated, ins, "demo:write", "demo", "write", "everyone")
		c.bad(errCheckViolated, ins, "demo:other", "demo", "write", "tenant") // code 必须 = resource:action
		c.ok(ins, "demo:sub:read", "demo:sub", "read", "tenant")              // 三段权限串：resource 含冒号
	})
	run("roles / role_permissions / user_roles：T? 唯一与 RESTRICT", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		r1 := c.role("platform", nil, "custom_admin")
		c.bad(errDup, "INSERT INTO roles (uid, tenant_id, scope, code, name) VALUES (?, NULL, 'platform', 'custom_admin', 'dup')", uid26())   // NULL 租户也唯一（生成列 uniq_key）
		c.bad(errDup, "INSERT INTO roles (uid, tenant_id, scope, code, name) VALUES (?, NULL, 'platform', 'platform_admin', 'dup')", uid26()) // 与种子内置角色冲突
		c.ok("INSERT INTO roles (uid, tenant_id, scope, code, name) VALUES (?, NULL, 'tenant', 'custom_admin', 'tmpl')", uid26())             // scope 不同
		c.role("tenant", t1, "custom_admin")
		c.role("tenant", t2, "custom_admin") // 不同租户同名允许
		c.bad(errDup, "INSERT INTO roles (uid, tenant_id, scope, code, name) VALUES (?, ?, 'tenant', 'custom_admin', 'dup')", uid26(), t1)
		c.bad(errCheckViolated, "INSERT INTO roles (uid, tenant_id, scope, code, name) VALUES (?, ?, 'platform', 'bound', 'x')", uid26(), t1) // 平台角色不得带租户
		c.bad(errCheckViolated, "INSERT INTO roles (uid, tenant_id, scope, code, name) VALUES (?, NULL, 'galaxy', 'x', 'x')", uid26())
		c.bad(errNoRefRow, "INSERT INTO roles (uid, tenant_id, scope, code, name) VALUES (?, 999999999, 'tenant', 'orphan', 'x')", uid26())
		c.bad(errRowIsRef, "DELETE FROM tenants WHERE id = ?", t1) // 租户自定义角色引用租户：RESTRICT

		perm := c.ok("INSERT INTO permissions (code, resource, action, scope, module) VALUES ('demo:rp', 'demo', 'rp', 'both', 'm')")
		c.ok("INSERT INTO role_permissions (role_id, permission_id) VALUES (?, ?)", r1, perm)
		c.bad(errDup, "INSERT INTO role_permissions (role_id, permission_id) VALUES (?, ?)", r1, perm)
		c.bad(errNoRefRow, "INSERT INTO role_permissions (role_id, permission_id) VALUES (?, 999999999)", r1)

		u := c.platformUser()
		c.ok("INSERT INTO user_roles (user_id, role_id, tenant_id) VALUES (?, ?, NULL)", u, r1)
		c.bad(errDup, "INSERT INTO user_roles (user_id, role_id, tenant_id) VALUES (?, ?, NULL)", u, r1) // 平台级授予也唯一（tenant_key）
		rt := c.role("tenant", t2, "member")
		c.ok("INSERT INTO user_roles (user_id, role_id, tenant_id) VALUES (?, ?, ?)", u, rt, t2)
		c.ok("INSERT INTO user_roles (user_id, role_id, tenant_id) VALUES (?, ?, ?)", u, rt, t1) // 同一角色在不同租户内分别授予
		c.bad(errDup, "INSERT INTO user_roles (user_id, role_id, tenant_id) VALUES (?, ?, ?)", u, rt, t2)
		c.bad(errNoRefRow, "INSERT INTO user_roles (user_id, role_id, tenant_id) VALUES (?, ?, 999999999)", u, rt)
		r3 := c.role("platform", nil, "granted_by_probe")
		c.bad(errNoRefRow, "INSERT INTO user_roles (user_id, role_id, tenant_id, granted_by) VALUES (?, ?, NULL, 999999999)", u, r3)
		c.bad(errRowIsRef, "DELETE FROM tenants WHERE id = ?", t2) // user_roles 与 roles 都引用 t2

		c.ok("DELETE FROM roles WHERE id = ?", r1) // 角色删除级联 role_permissions 与 user_roles
		if n := c.count("SELECT COUNT(*) FROM role_permissions WHERE role_id = ?", r1); n != 0 {
			t.Errorf("角色删除应级联 role_permissions: %d", n)
		}
		if n := c.count("SELECT COUNT(*) FROM user_roles WHERE role_id = ?", r1); n != 0 {
			t.Errorf("角色删除应级联 user_roles: %d", n)
		}
		c.ok("DELETE FROM permissions WHERE id = ?", perm)
	})
	run("sessions：realm 与级联", func(c *cx) {
		u := c.user("s@example.test")
		tn := c.tenant()
		ins := "INSERT INTO sessions (token_hash, csrf_hash, user_id, realm, tenant_id, last_seen_at, expires_at, absolute_expires_at) VALUES (?, ?, ?, ?, ?, NOW(3), NOW(3), NOW(3))"
		h := func() string { return fmt.Sprintf("%064d", next()) }
		dupHash := h()
		c.ok(ins, dupHash, h(), u, "admin", nil)
		c.ok(ins, h(), h(), u, "console", tn)
		c.bad(errDup, ins, dupHash, h(), u, "admin", nil)
		c.bad(errCheckViolated, ins, h(), h(), u, "portal", nil)
		c.bad(errNoRefRow, ins, h(), h(), 999999999, "admin", nil)
		c.bad(errNoRefRow, ins, h(), h(), u, "console", 999999999)
		c.bad(errRowIsRef, "DELETE FROM tenants WHERE id = ?", tn)
		if n := c.count("SELECT COUNT(*) FROM sessions WHERE user_id = ? AND reauth_at IS NULL", u); n != 2 {
			t.Errorf("reauth_at 默认为空: %d", n)
		}
		c.ok("DELETE FROM users WHERE id = ?", u)
		if n := c.count("SELECT COUNT(*) FROM sessions WHERE user_id = ?", u); n != 0 {
			t.Errorf("用户删除应级联会话: %d", n)
		}
	})
	run("api_tokens：唯一、轮换自引用 SET NULL、租户 RESTRICT", func(c *cx) {
		u := c.platformUser()
		tn := c.tenant()
		ins := "INSERT INTO api_tokens (uid, tenant_id, user_id, name, token_prefix, token_hash, scopes_json, rotated_from_id) VALUES (?, ?, ?, 'n', ?, ?, JSON_ARRAY('site:read'), ?)"
		pfx := func() string { return fmt.Sprintf("%08d", next()%100000000) }
		h := func() string { return fmt.Sprintf("%064d", next()) }
		p1, h1 := pfx(), h()
		a := c.ok(ins, uid26(), nil, u, p1, h1, nil)
		b := c.ok(ins, uid26(), tn, u, pfx(), h(), a)
		c.bad(errDup, ins, uid26(), nil, u, p1, h(), nil)
		c.bad(errDup, ins, uid26(), nil, u, pfx(), h1, nil)
		c.bad(errNoRefRow, ins, uid26(), nil, u, pfx(), h(), 999999999)
		c.bad(errRowIsRef, "DELETE FROM tenants WHERE id = ?", tn)
		c.bad(errNoDefault, "INSERT INTO api_tokens (uid, user_id, name, token_prefix, token_hash) VALUES (?, ?, 'n', ?, ?)", uid26(), u, pfx(), h()) // scopes_json 必填
		c.ok("DELETE FROM api_tokens WHERE id = ?", a)
		if !c.isNull("SELECT rotated_from_id FROM api_tokens WHERE id = ?", b) {
			t.Error("被轮换的旧令牌删除后 rotated_from_id 应置 NULL")
		}
	})
	run("login_logs：C2u 无外键、uid 唯一", func(c *cx) {
		ins := "INSERT INTO login_logs (uid, user_id, tenant_id, realm, identifier, event) VALUES (?, ?, ?, 'admin', 'who@example.test', 'login_failed')"
		u := uid26()
		c.ok(ins, u, 999999999, 999999999) // 高频日志表无外键：引用完整性由写入路径保证
		c.bad(errDup, ins, u, nil, nil)
	})
	run("system_settings / system_installations", func(c *cx) {
		ins := "INSERT INTO system_settings (uid, setting_key, value_json, value_enc, group_name) VALUES (?, ?, ?, ?, 'general')"
		c.ok(ins, uid26(), "k.json", `{"a":1}`, nil)
		c.ok(ins, uid26(), "k.enc", nil, "v1:k:w:n:c")
		c.bad(errCheckViolated, ins, uid26(), "k.both", `{"a":1}`, "v1:k:w:n:c") // 二选一
		c.bad(errCheckViolated, ins, uid26(), "k.none", nil, nil)
		c.bad(errDup, ins, uid26(), "k.json", `{"b":2}`, nil)
		c.bad(errNoRefRow, "INSERT INTO system_settings (uid, setting_key, value_json, group_name, updated_by) VALUES (?, 'k.u', JSON_OBJECT(), 'g', 999999999)", uid26())

		si := "INSERT INTO system_installations (uid, installation_id, component, instance_name, host, version, status, installed_at) VALUES (?, ?, ?, ?, ?, '1.0.0', ?, NOW(3))"
		c.ok(si, uid26(), uid26(), "server", "main", "h1", "installed")
		c.ok(si, uid26(), uid26(), "server", "main", "h2", "installed") // 不同主机
		c.bad(errDup, si, uid26(), uid26(), "server", "main", "h1", "installed")
		c.bad(errCheckViolated, si, uid26(), uid26(), "node", "main", "h1", "exploded")
	})
	run("tasks / task_events：TaskType 白名单、状态、幂等、级联", func(c *cx) {
		for _, typ := range oa.taskType { // OpenAPI TaskType 的每个取值都可写入
			c.task(typ)
		}
		c.bad(errCheckViolated, "INSERT INTO tasks (uid, type, payload_json, request_id) VALUES (?, 'shell.exec', JSON_OBJECT(), ?)", uid26(), uid26()) // 不存在「执行任意命令」的任务类型
		c.bad(errCheckViolated, "INSERT INTO tasks (uid, type, status, payload_json, request_id) VALUES (?, 'node.upgrade', 'exploded', JSON_OBJECT(), ?)", uid26(), uid26())
		c.bad(errCheckViolated, "INSERT INTO tasks (uid, type, progress, payload_json, request_id) VALUES (?, 'node.upgrade', 101, JSON_OBJECT(), ?)", uid26(), uid26())
		c.bad(errNoDefault, "INSERT INTO tasks (uid, type, request_id) VALUES (?, 'node.upgrade', ?)", uid26(), uid26()) // payload_json 必填
		ins := "INSERT INTO tasks (uid, type, payload_json, request_id, idempotency_key) VALUES (?, 'cache.purge_url', JSON_OBJECT(), ?, ?)"
		c.ok(ins, uid26(), uid26(), "k1")
		c.bad(errDup, ins, uid26(), uid26(), "k1")                                                                                                              // (type, key) 唯一
		c.ok("INSERT INTO tasks (uid, type, payload_json, request_id, idempotency_key) VALUES (?, 'cache.prefetch', JSON_OBJECT(), ?, 'k1')", uid26(), uid26()) // 不同 type 同 key
		c.ok(ins, uid26(), uid26(), nil)
		c.ok(ins, uid26(), uid26(), nil) // NULL 键不受限
		tn := c.tenant()
		tk := c.ok("INSERT INTO tasks (uid, tenant_id, type, payload_json, request_id) VALUES (?, ?, 'cache.purge_site', JSON_OBJECT(), ?)", uid26(), tn, uid26())
		c.bad(errRowIsRef, "DELETE FROM tenants WHERE id = ?", tn)
		c.bad(errNoRefRow, "INSERT INTO tasks (uid, tenant_id, type, payload_json, request_id) VALUES (?, 999999999, 'cache.purge_site', JSON_OBJECT(), ?)", uid26(), uid26())
		child := c.ok("INSERT INTO tasks (uid, type, payload_json, request_id, parent_task_id) VALUES (?, 'cache.purge_url', JSON_OBJECT(), ?, ?)", uid26(), uid26(), tk)
		ev := "INSERT INTO task_events (uid, task_id, level, message) VALUES (?, ?, ?, 'm')"
		c.ok(ev, uid26(), child, "info")
		c.bad(errCheckViolated, ev, uid26(), child, "fatal")
		c.bad(errNoRefRow, ev, uid26(), 999999999, "info")
		u := c.user("creator@example.test")
		cb := c.ok("INSERT INTO tasks (uid, type, payload_json, request_id, created_by_user_id) VALUES (?, 'node.upgrade', JSON_OBJECT(), ?, ?)", uid26(), uid26(), u)
		c.ok("DELETE FROM users WHERE id = ?", u)
		if !c.isNull("SELECT created_by_user_id FROM tasks WHERE id = ?", cb) {
			t.Error("创建者用户删除后 created_by_user_id 应置 NULL")
		}
		c.ok("DELETE FROM tasks WHERE id = ?", tk) // 父任务删除级联子任务与事件
		if n := c.count("SELECT COUNT(*) FROM tasks WHERE id = ?", child); n != 0 {
			t.Error("父任务删除应级联子任务")
		}
		if n := c.count("SELECT COUNT(*) FROM task_events WHERE task_id = ?", child); n != 0 {
			t.Error("子任务删除应级联其事件")
		}
	})
	run("job_locks / idempotency_keys / event_outbox", func(c *cx) {
		jl := "INSERT INTO job_locks (name, holder, lease_until, acquired_at, updated_at) VALUES (?, 'h', NOW(3), NOW(3), NOW(3))"
		c.ok(jl, "scheduler.leader")
		c.bad(errDup, jl, "scheduler.leader")
		if got := c.count("SELECT fence FROM job_locks WHERE name = 'scheduler.leader'"); got != 0 {
			t.Errorf("fence 缺省 0: %d", got)
		}
		tk := c.task("cache.purge_url")
		ik := "INSERT INTO idempotency_keys (principal_key, idem_key, method, path, request_hash, task_id, expires_at) VALUES ('admin:1:0', ?, 'POST', '/x', ?, ?, NOW(3))"
		h := fmt.Sprintf("%064d", next())
		c.ok(ik, "k1", h, tk)
		c.bad(errDup, ik, "k1", h, nil)
		c.ok(ik, "k2", h, nil)
		c.bad(errNoRefRow, ik, "k3", h, 999999999)
		c.ok("DELETE FROM tasks WHERE id = ?", tk) // task_id → SET NULL，幂等记录保留
		if n := c.count("SELECT COUNT(*) FROM idempotency_keys WHERE idem_key = 'k1' AND task_id IS NULL"); n != 1 {
			t.Error("任务删除后幂等键应保留且 task_id 置 NULL")
		}
		ob := "INSERT INTO event_outbox (event_type, aggregate_type, aggregate_uid, payload_json, status) VALUES ('site.published', 'site', 'u', JSON_OBJECT(), ?)"
		c.ok(ob, "pending")
		c.ok(ob, "sent")
		c.bad(errCheckViolated, ob, "lost")
	})
	run("signing_keys：每个 purpose 至多一个 active", func(c *cx) {
		ins := "INSERT INTO signing_keys (uid, purpose, key_id, algorithm, public_key, private_key_enc, enc_key_id, status) VALUES (?, ?, ?, ?, ?, ?, 'kek1', ?)"
		key := func(purpose, status string) int64 {
			return c.ok(ins, uid26(), purpose, fmt.Sprintf("k%d", next()), "ed25519", []byte("pub"), "v1:kek1:w:n:c", status)
		}
		key("config_signing", "active")
		key("mtls_ca", "active") // 不同 purpose 各自可有一个 active
		key("grpc_server", "active")
		key("config_signing", "pending") // 非 active 不受限且可有多个
		key("config_signing", "pending")
		key("config_signing", "retiring")
		key("config_signing", "revoked")
		c.bad(errDup, ins, uid26(), "config_signing", "kdup", "ed25519", []byte("pub"), "v1:kek1:w:n:c", "active")
		c.bad(errCheckViolated, ins, uid26(), "tls_leaf", "kbad1", "ed25519", []byte("pub"), "v1:kek1:w:n:c", "pending")
		c.bad(errCheckViolated, ins, uid26(), "mtls_ca", "kbad2", "rsa2048", []byte("pub"), "v1:kek1:w:n:c", "pending")
		c.bad(errCheckViolated, ins, uid26(), "mtls_ca", "kbad3", "ed25519", []byte("pub"), "v1:kek1:w:n:c", "zombie")
		fixed := "kfixed"
		c.ok(ins, uid26(), "mtls_ca", fixed, "ecdsa_p256", []byte("pub"), "v1:kek1:w:n:c", "pending")
		c.bad(errDup, ins, uid26(), "mtls_ca", fixed, "ed25519", []byte("pub"), "v1:kek1:w:n:c", "pending") // key_id 唯一
		// 轮换：旧 active 转 retiring 后，新的才能 active。
		c.ok("UPDATE signing_keys SET status = 'retiring' WHERE purpose = 'mtls_ca' AND status = 'active'")
		key("mtls_ca", "active")
	})
	run("audit_logs：无外键、pmax 兜底、uid+分区键唯一、不可变触发器", func(c *cx) {
		ins := "INSERT INTO audit_logs (uid, created_at, actor_type, actor_user_id, actor_label, tenant_id, action, resource_type, result, request_id) VALUES (?, ?, 'user', 999999999, 'x', 888888888, 'site.create', 'site', 'success', ?)"
		u := uid26()
		c.ok(ins, u, "2026-11-25 08:00:00.123", uid26())       // 无外键：引用的用户/租户可以不存在（审计保留快照）
		c.ok(ins, uid26(), "2099-01-01 00:00:00.000", uid26()) // 越过全部月分区：落入 pmax，不报 ERROR 1526
		if n := c.count("SELECT COUNT(*) FROM audit_logs PARTITION (pmax)"); n != 1 {
			t.Errorf("2099 年的审计行应落入 pmax: %d", n)
		}
		c.bad(errDup, ins, u, "2026-11-25 08:00:00.123", uid26()) // (uid, created_at) 唯一
		c.bad(errSignal, "UPDATE audit_logs SET action = 'x' WHERE uid = ?", u)
		c.bad(errSignal, "DELETE FROM audit_logs WHERE uid = ?", u)
	})

	// 租户索引实测（EXPLAIN）：带 tenant_id 的列表查询走 tenant_id 打头索引；漏写租户条件则退化为全表扫描——正是该约定要让测试发现的现象。
	t.Run("EXPLAIN：租户条件走 tenant_id 打头索引", func(t *testing.T) {
		ctx := ctxT(t)
		tx, err := c.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		cc := &cx{t: t, x: tx, pw: "x"}
		for i := 0; i < 60; i++ {
			tn := cc.tenant()
			cc.ok("INSERT INTO tenant_quotas (tenant_id, quota_key, limit_value) VALUES (?, 'sites', 1), (?, 'domains_per_site', 2), (?, 'rules_per_site', 3), (?, 'api_tokens', 4), (?, 'members', 5)", tn, tn, tn, tn, tn)
			cc.ok("INSERT INTO tenant_members (tenant_id, user_id) VALUES (?, ?)", tn, cc.user(fmt.Sprintf("x%d@example.test", next())))
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		for _, tbl := range []string{"tenant_quotas", "tenant_members"} {
			if _, err := c.ExecContext(ctx, "ANALYZE TABLE `"+tbl+"`"); err != nil { // ANALYZE 返回结果集，用 Exec 丢弃即可
				t.Fatal(err)
			}
		}
		plan := func(q string, args ...any) string {
			var p string
			if err := c.QueryRowContext(ctx, "EXPLAIN FORMAT=TREE "+q, args...).Scan(&p); err != nil {
				t.Fatalf("%v\n%s", err, q)
			}
			return p
		}
		tn := e.scalar(t, "SELECT MIN(tenant_id) FROM `"+e.dbName+"`.tenant_quotas")
		if p := plan("SELECT limit_value FROM tenant_quotas WHERE tenant_id = ?", tn); !strings.Contains(p, "uk_tenant_quotas") || strings.Contains(p, "Table scan") {
			t.Errorf("tenant_quotas 的租户条件查询应走 uk_tenant_quotas: %s", p)
		}
		if p := plan("SELECT limit_value FROM tenant_quotas WHERE tenant_id = ? AND quota_key = 'sites'", tn); strings.Contains(p, "Table scan") {
			t.Errorf("tenant_quotas 的 (tenant_id, quota_key) 查询不应全表扫描（唯一键等值，常量查找）: %s", p)
		}
		if p := plan("SELECT joined_at FROM tenant_members WHERE tenant_id = ?", tn); !strings.Contains(p, "uk_tenant_members") || strings.Contains(p, "Table scan") {
			t.Errorf("tenant_members 的租户条件查询应走 uk_tenant_members: %s", p)
		}
		if p := plan("SELECT limit_value FROM tenant_quotas WHERE quota_key = 'sites'"); !strings.Contains(p, "Table scan") {
			t.Errorf("漏写 tenant_id 条件应退化为全表扫描（EXPLAIN 可发现）: %s", p)
		}
	})
}

// ---- DDL 陷阱预检覆盖（AC6）：对真实迁移做变异，静态预检必须报出，且真实 MySQL 以文档记录的错误号拒绝 ----

// fixtureFromEmbedded 复制嵌入迁移中的 A 批（000001–000005）为可变的内存集合：本文件的测试只针对 A 批，
// 后续 Story 追加 000006+ 不会改变这里的期望（表数、授权数、版本序列）。
func fixtureFromEmbedded(t testing.TB) map[string]*mapFile { return fixtureOf(t, m1aFiles) }

// fixtureOf 从嵌入的真实迁移中取出指定文件，组成内存集合（可变异）。
func fixtureOf(t testing.TB, names []string) map[string]*mapFile {
	t.Helper()
	out := map[string]*mapFile{}
	for _, name := range names {
		b, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = &mapFile{Data: b}
	}
	return out
}

// firstFailure 在新建空库上按迁移顺序逐条执行全部 Up 语句（root 连接，不经框架），返回首个 MySQL 错误号（无错为 0）。
func firstFailure(t testing.TB, e *env, set *migrate.Set) (uint16, string) {
	t.Helper()
	ctx := context.Background()
	c := e.conn(t)
	for _, m := range set.Migrations {
		for _, st := range m.Up {
			if _, err := c.ExecContext(ctx, st.Text); err != nil {
				n, _ := mysqlErr(err)
				return n, fmt.Sprintf("%s:%d: %v", m.File, st.Line, err)
			}
		}
	}
	return 0, ""
}

func TestM1ADDLTrapsAreCoveredByStaticPrecheckAndRealMySQL(t *testing.T) {
	type mutation struct {
		name     string
		mutate   func(files map[string]*mapFile)
		wantRule string
		wantErr  uint16 // 真实 MySQL 在建表/改表时即报出的错误号；0 = 建成后在写入时才报（见 probe）
		probe    string // 建成后的探针语句（须以 probeErr 失败）
		probeErr uint16
	}
	replace := func(file, old, new string) func(map[string]*mapFile) {
		return func(files map[string]*mapFile) {
			f := files[file]
			s := string(f.Data)
			if strings.Count(s, old) != 1 {
				t.Fatalf("变异锚点在 %s 中出现 %d 次（应为 1）: %q", file, strings.Count(s, old), old)
			}
			f.Data = []byte(strings.Replace(s, old, new, 1))
		}
	}
	muts := []mutation{
		{
			name: "CHECK 引用的外键列改为 ON DELETE SET NULL（roles.ck_roles_platform_scope 引用 tenant_id）",
			mutate: replace("000001_identity_core.sql", "CONSTRAINT fk_roles_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT",
				"CONSTRAINT fk_roles_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE SET NULL"),
			wantRule: migrate.RuleCheckFK, wantErr: 3823,
		},
		{
			name: "CHECK 引用的外键列改为 ON UPDATE CASCADE",
			mutate: replace("000001_identity_core.sql", "CONSTRAINT fk_roles_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT",
				"CONSTRAINT fk_roles_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT ON UPDATE CASCADE"),
			wantRule: migrate.RuleCheckFK, wantErr: 3823,
		},
		{
			name: "STORED 生成列的基列外键 CASCADE（user_roles.tenant_key 由 VIRTUAL 改 STORED，tenant FK 改 CASCADE）",
			mutate: func(files map[string]*mapFile) {
				replace("000001_identity_core.sql", "tenant_key BIGINT UNSIGNED AS (COALESCE(tenant_id, 0)) VIRTUAL NOT NULL", "tenant_key BIGINT UNSIGNED AS (COALESCE(tenant_id, 0)) STORED NOT NULL")(files)
				replace("000001_identity_core.sql", "CONSTRAINT fk_user_roles_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT",
					"CONSTRAINT fk_user_roles_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE")(files)
			},
			wantRule: migrate.RuleGeneratedFK, wantErr: 1215,
		},
		{
			name: "STORED 生成列的基列外键 CASCADE（roles.uniq_key 由 VIRTUAL 改 STORED，tenant FK 改 CASCADE）",
			mutate: func(files map[string]*mapFile) {
				replace("000001_identity_core.sql", "(CONCAT(scope, ':', COALESCE(tenant_id, 0), ':', code)) VIRTUAL NOT NULL", "(CONCAT(scope, ':', COALESCE(tenant_id, 0), ':', code)) STORED NOT NULL")(files)
				replace("000001_identity_core.sql", "CONSTRAINT fk_roles_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT",
					"CONSTRAINT fk_roles_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE")(files)
			},
			wantRule: migrate.RuleGeneratedFK, wantErr: 1215,
		},
		{
			name: "前向外键：tenants 先于 users 创建（tenants.owner_user_id → users）",
			mutate: func(files map[string]*mapFile) {
				s := string(files["000001_identity_core.sql"].Data)
				iu := strings.Index(s, "-- ---- users：")
				it := strings.Index(s, "-- ---- tenants：")
				ir := strings.Index(s, "-- ---- user_recovery_codes")
				files["000001_identity_core.sql"].Data = []byte(s[:iu] + s[it:ir] + s[iu:it] + s[ir:])
			},
			wantRule: migrate.RuleForwardFK, wantErr: 1824,
		},
		{
			name: "前向外键：platform_core 先于 identity_core（用户/租户尚不存在）",
			mutate: func(files map[string]*mapFile) {
				a, b := files["000001_identity_core.sql"], files["000002_platform_core.sql"]
				a.Data, b.Data = b.Data, a.Data
			},
			wantRule: migrate.RuleForwardFK, wantErr: 1824,
		},
		{
			name: "分区表出现外键（audit_logs.tenant_id → tenants）",
			mutate: replace("000003_audit_logs.sql", "  KEY idx_audit_logs_action (action, created_at)\n",
				"  KEY idx_audit_logs_action (action, created_at),\n  CONSTRAINT fk_audit_logs_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id)\n"),
			wantRule: migrate.RulePartitionFK, wantErr: 1506,
		},
		{
			name:     "分区表缺 pmax 兜底分区",
			mutate:   replace("000003_audit_logs.sql", "  PARTITION p202712 VALUES LESS THAN ('2028-01-01 00:00:00'),\n  PARTITION pmax VALUES LESS THAN (MAXVALUE)\n", "  PARTITION p202712 VALUES LESS THAN ('2028-01-01 00:00:00')\n"),
			wantRule: migrate.RulePartitionPmax,
			probe:    "INSERT INTO audit_logs (uid, created_at, actor_type, actor_label, action, resource_type, result, request_id) VALUES ('probe', '2099-01-01', 'system', 'x', 'x', 'x', 'success', 'x')",
			probeErr: errOutOfRange,
		},
	}
	for _, mu := range muts {
		t.Run(mu.name, func(t *testing.T) {
			files := fixtureFromEmbedded(t)
			mu.mutate(files)
			fsys := toMapFS(files)
			set, findings, err := migrate.Load(fsys)
			if err != nil {
				t.Fatal(err)
			}
			var rules []string
			hit := false
			for _, f := range findings {
				rules = append(rules, f.Rule)
				if f.Rule == mu.wantRule {
					hit = true
				}
			}
			if !hit {
				t.Fatalf("静态预检应报出 %s；实际 %v", mu.wantRule, rules)
			}
			// 静态预检的判定必须与真实 MySQL 一致：真实库确实以文档记录的错误号拒绝。
			e := newEnv(t)
			n, where := firstFailure(t, e, set)
			if mu.wantErr != 0 {
				if n != mu.wantErr {
					t.Fatalf("真实 MySQL 应以 ERROR %d 拒绝（预检规则 %s 的文档记录），实际 %d：%s", mu.wantErr, mu.wantRule, n, where)
				}
				return
			}
			if n != 0 {
				t.Fatalf("建表阶段不应失败: %d %s", n, where)
			}
			_, perr := e.conn(t).ExecContext(ctxT(t), mu.probe)
			if got, ok := mysqlErr(perr); !ok || got != mu.probeErr {
				t.Fatalf("探针应以 ERROR %d 失败: %v", mu.probeErr, perr)
			}
		})
	}
	// 对照：未变异的真实迁移在真实库上 0 错误（上面的失败确实来自变异，而不是别的原因）。
	t.Run("未变异的真实迁移 0 错误", func(t *testing.T) {
		set, findings, err := migrate.Load(migrations.FS)
		if err != nil || len(findings) != 0 {
			t.Fatalf("%v %v", err, findings)
		}
		if n, where := firstFailure(t, newEnv(t), set); n != 0 {
			t.Fatalf("真实迁移逐条执行应 0 错误: %d %s", n, where)
		}
	})
}

// ---- schema lint 在真实迁移上的变异覆盖：每条规则都能拦住对真实迁移文本的违规改动 ----

func TestM1ASchemaLintCatchesViolationsInRealMigrations(t *testing.T) {
	cases := []struct {
		name, file, old, new, rule string
	}{
		{"ENUM", "000001_identity_core.sql", "status VARCHAR(16) NOT NULL DEFAULT 'active',\n  locale VARCHAR(8) NOT NULL DEFAULT 'zh-CN',\n  timezone", "status ENUM('active','disabled') NOT NULL DEFAULT 'active',\n  locale VARCHAR(8) NOT NULL DEFAULT 'zh-CN',\n  timezone", migrate.RuleSchemaEnum},
		{"TEXT DEFAULT", "000001_identity_core.sql", "totp_secret_enc TEXT NULL,", "totp_secret_enc TEXT NULL DEFAULT 'x',", migrate.RuleSchemaLOBDefault},
		{"JSON DEFAULT", "000002_platform_core.sql", "payload_json JSON NOT NULL,\n  result_json", "payload_json JSON NOT NULL DEFAULT (JSON_OBJECT()),\n  result_json", migrate.RuleSchemaLOBDefault},
		{"TIMESTAMP", "000001_identity_core.sql", "password_changed_at DATETIME(3) NULL,", "password_changed_at TIMESTAMP NULL,", migrate.RuleSchemaTime},
		{"DATETIME 无精度", "000002_platform_core.sql", "lease_until DATETIME(3) NOT NULL,\n  acquired_at", "lease_until DATETIME NOT NULL,\n  acquired_at", migrate.RuleSchemaTime},
		{"浮点", "000002_platform_core.sql", "attempts INT UNSIGNED NOT NULL DEFAULT 0,\n  max_attempts", "attempts DOUBLE NOT NULL DEFAULT 0,\n  max_attempts", migrate.RuleSchemaFloat},
		{"租户表索引未以 tenant_id 打头", "000001_identity_core.sql", "  KEY idx_tenant_members_user (user_id),\n", "  KEY idx_tenant_members_user (user_id),\n  KEY idx_tenant_members_status (status, tenant_id),\n", migrate.RuleSchemaTenantIndex},
		{"租户表缺 tenant_id（登记表）", "000001_identity_core.sql", "  tenant_id BIGINT UNSIGNED NOT NULL,\n  quota_key", "  tenant_id BIGINT UNSIGNED NULL,\n  quota_key", migrate.RuleSchemaTenantColumn},
		{"跨租户索引豁免被删除", "000001_identity_core.sql", "-- lingedge:cross-tenant-index=idx_tenant_members_user 登录后按用户反查所属租户（跨租户查询，同时支撑外键 fk_tenant_members_users）\n", "", migrate.RuleSchemaTenantIndex},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := fixtureFromEmbedded(t)
			f := files[c.file]
			s := string(f.Data)
			if strings.Count(s, c.old) != 1 {
				t.Fatalf("变异锚点出现 %d 次（应为 1）: %q", strings.Count(s, c.old), c.old)
			}
			f.Data = []byte(strings.Replace(s, c.old, c.new, 1))
			_, findings, err := migrate.Load(toMapFS(files))
			if err != nil {
				t.Fatal(err)
			}
			for _, fd := range findings {
				if fd.Rule == c.rule {
					return
				}
			}
			t.Fatalf("应报出 %s；实际 %v", c.rule, findings)
		})
	}
}

// ADR-016 决策 4（Story 1.6 延后项，随 system_settings 落库补齐）：成功应用 irreversible 迁移后，runner 把最近一次不可逆迁移
// 记入 system_settings 的 schema.latest_class_irreversible_applied；expand 迁移不写；之后再有不可逆迁移则更新同一行。
func TestM1AIrreversibleMigrationIsRecordedInSystemSettings(t *testing.T) {
	ctx := ctxT(t)
	e, _ := m1aUp(t)
	key := migrate.IrreversibleSettingKey
	if n := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.system_settings WHERE setting_key = ?", key); n != "0" {
		t.Fatalf("只应用了 expand 迁移，不应有不可逆记录: %s", n)
	}
	files := fixtureFromEmbedded(t)
	files["000006_drop_quota_subscription.sql"] = &mapFile{Data: []byte("-- lingedge:class=irreversible\n-- lingedge:summary=测试：删除列（不可逆）\n-- +goose Up\nALTER TABLE tenant_quotas DROP COLUMN subscription_id;\n")}
	r := e.runner(t, toMapFS(files))
	if f := r.Findings(); len(f) != 0 {
		t.Fatalf("lint: %v", f)
	}
	if _, err := r.Up(ctx, migrate.UpOptions{}); !errors.As(err, new(*migrate.ErrIrreversibleNotAccepted)) {
		t.Fatalf("未确认不可逆迁移应被拒: %v", err)
	}
	if n := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.system_settings WHERE setting_key = ?", key); n != "0" {
		t.Fatalf("被拒绝的升级不应写入记录: %s", n)
	}
	if _, err := r.Up(ctx, migrate.UpOptions{AcceptIrreversible: true}); err != nil {
		t.Fatal(err)
	}
	get := func() string {
		return e.scalar(t, "SELECT CONCAT(JSON_UNQUOTE(JSON_EXTRACT(value_json, '$.version')), '|', JSON_UNQUOTE(JSON_EXTRACT(value_json, '$.name')), '|', IF(JSON_UNQUOTE(JSON_EXTRACT(value_json, '$.applied_at')) REGEXP '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z$', 'ts', 'badts'), '|', group_name, '|', IF(uid REGEXP '^[0-9A-HJKMNP-TV-Z]{26}$', 'ulid', 'baduid'), '|', IF(value_enc IS NULL, 'plain', 'enc')) FROM `"+e.dbName+"`.system_settings WHERE setting_key = ?", key)
	}
	if got := get(); got != "6|drop_quota_subscription|ts|general|ulid|plain" {
		t.Fatalf("不可逆记录 = %q", got)
	}
	// 自愈：记录被误删（或「迁移已提交但进程在写记录前被杀」）后，下一次 up（即使没有待执行迁移）按运行记录补写。
	if _, err := e.admin(t).ExecContext(ctx, "DELETE FROM `"+e.dbName+"`.system_settings WHERE setting_key = ?", key); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != "6|drop_quota_subscription|ts|general|ulid|plain" {
		t.Fatalf("up 应自愈补写不可逆记录: %q", got)
	}
	// 再来一个不可逆迁移：更新同一行，不新增。
	files["000007_drop_quota_source.sql"] = &mapFile{Data: []byte("-- lingedge:class=irreversible\n-- +goose Up\nALTER TABLE tenant_quotas DROP COLUMN source;\n")}
	r2 := e.runner(t, toMapFS(files))
	if _, err := r2.Up(ctx, migrate.UpOptions{AcceptIrreversible: true}); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != "7|drop_quota_source|ts|general|ulid|plain" {
		t.Fatalf("更新后的不可逆记录 = %q", got)
	}
	if n := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.system_settings WHERE setting_key = ?", key); n != "1" {
		t.Fatalf("应只有一行: %s", n)
	}
	// recover 路径：SQL 与版本表都已完成、只缺 applied 标记（进程在 markApplied 前被杀）→ recover 补记 applied 的同时补写记录。
	if _, err := e.admin(t).ExecContext(ctx, "UPDATE `"+e.dbName+"`."+migrate.RunsTable+" SET status = 'running', finished_at = NULL WHERE version = 7"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.admin(t).ExecContext(ctx, "DELETE FROM `"+e.dbName+"`.system_settings WHERE setting_key = ?", key); err != nil {
		t.Fatal(err)
	}
	if err := r2.Recover(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != "7|drop_quota_source|ts|general|ulid|plain" {
		t.Fatalf("recover 补记 applied 后应补写不可逆记录: %q", got)
	}
	if got := e.scalar(t, "SELECT status FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = 7"); got != "applied" {
		t.Fatalf("recover 后运行记录 = %q", got)
	}
}

// 记录的对账不依赖外部状态：没有 system_settings 的库（例如只有框架表）上 up 不受影响（表缺失时静默跳过）。
func TestIrreversibleSyncIsSkippedWithoutSystemSettings(t *testing.T) {
	ctx := ctxT(t)
	e := newEnv(t)
	files := toMapFS(map[string]*mapFile{
		"000001_widgets.sql":     {Data: []byte("-- lingedge:class=expand\n-- +goose Up\nCREATE TABLE IF NOT EXISTS widgets (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, legacy VARCHAR(8) NULL) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON `widgets` TO lingedge_app;\n")},
		"000002_drop_legacy.sql": {Data: []byte("-- lingedge:class=irreversible\n-- +goose Up\nALTER TABLE widgets DROP COLUMN legacy;\n")},
	})
	r := e.runner(t, files)
	if f := r.Findings(); len(f) != 0 {
		t.Fatalf("lint: %v", f)
	}
	if _, err := r.Up(ctx, migrate.UpOptions{AcceptIrreversible: true}); err != nil {
		t.Fatalf("没有 system_settings 时不可逆迁移照常应用: %v", err)
	}
	wantVersions(t, e, "1", "2")
	if e.hasTable(t, "system_settings") {
		t.Error("框架不得创建 system_settings")
	}
}
