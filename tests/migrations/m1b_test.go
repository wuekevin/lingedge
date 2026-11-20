package migrationstest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/migrations"
)

// Story 1.8：M1 迁移 B（000006–000011，真实迁移，来自 migrations.FS）的测试。
// 本批测试基于「A 批 + B 批」的内存集合（000001–000011）：Story 1.9 追加 000012+ 后，本文件的期望（表数、版本号）保持不变。

// m1bTables 是 000006–000011 创建的 28 张表（epics Story 1.8 AC1），按迁移文件分组。
var m1bTables = []string{
	// 000006 nodes_clusters（11）
	"regions", "isp_lines", "ip_pools", "nodes", "ip_pool_members", "clusters", "cluster_nodes", "enroll_tokens", "node_certificates", "node_events", "node_heartbeats",
	// 000007 sites_core（2）
	"site_groups", "sites",
	// 000008 certificates（2）
	"certificates", "certificate_deployments",
	// 000009 site_config（7）
	"site_domains", "site_settings", "origin_groups", "origins", "origin_health", "site_cluster_bindings", "node_tasks",
	// 000010 rules（3）
	"rule_templates", "site_rules", "rule_versions",
	// 000011 cache（3）
	"cache_policies", "cache_tasks", "cache_task_nodes",
}

var m1bFiles = []string{
	"000006_nodes_clusters.sql", "000007_sites_core.sql", "000008_certificates.sql",
	"000009_site_config.sql", "000010_rules.sql", "000011_cache.sql",
}

// m1bTenantTables 是 B 批中 tenant_id NOT NULL 的租户范围表（文档「通用列：… + T」）。
var m1bTenantTables = []string{
	"site_groups", "sites", "certificates", "certificate_deployments", "site_domains", "site_settings", "origin_groups", "origins",
	"origin_health", "site_cluster_bindings", "site_rules", "rule_versions", "cache_policies", "cache_tasks", "cache_task_nodes",
}

func m1abFiles() []string { return append(append([]string(nil), m1aFiles...), m1bFiles...) }

func m1abFixture(t testing.TB) map[string]*mapFile { return fixtureOf(t, m1abFiles()) }

// m1bUp 在新建空库上经框架执行 000001–000011（必须 0 错误）。
func m1bUp(t testing.TB) (*env, *migrate.Runner) {
	t.Helper()
	e := newEnv(t)
	r := e.runner(t, toMapFS(m1abFixture(t)))
	if f := r.Findings(); len(f) != 0 {
		t.Fatalf("真实迁移必须通过 lint: %v", f)
	}
	res, err := r.Up(ctxT(t), migrate.UpOptions{})
	if err != nil {
		t.Fatalf("空库全量 up 必须 0 错误: %v", err)
	}
	if len(res.Applied) != len(m1abFiles()) || res.ToVersion != int64(len(m1abFiles())) {
		t.Fatalf("up 结果: %+v", res)
	}
	return e, r
}

func m1abTables() []string {
	all := append(append([]string(nil), m1aTables...), m1bTables...)
	sort.Strings(all)
	return all
}

// ---- 静态：嵌入的真实迁移 ----

func TestM1BEmbeddedMigrationsAreLintCleanAndWellFormed(t *testing.T) {
	set, findings, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("lint: %s", f)
	}
	if len(set.Migrations) < len(m1abFiles()) {
		t.Fatalf("至少应有 %d 个迁移，实际 %d", len(m1abFiles()), len(set.Migrations))
	}
	createRe := regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+IF\s+NOT\s+EXISTS\s+(\w+)`)
	var created []string
	for i, m := range set.Migrations[len(m1aFiles):len(m1abFiles())] {
		if m.File != m1bFiles[i] || m.Version != int64(len(m1aFiles)+i+1) {
			t.Errorf("迁移 = %s（版本 %d），期望 %s（版本 %d）", m.File, m.Version, m1bFiles[i], len(m1aFiles)+i+1)
		}
		if m.Class != migrate.ClassExpand {
			t.Errorf("%s 必须标 expand（AC1）: %s", m.File, m.Class)
		}
		if m.Summary == "" {
			t.Errorf("%s 缺少 lingedge:summary", m.File)
		}
		for _, st := range m.Up {
			switch strings.ToUpper(strings.Fields(st.Text + " ")[0]) {
			case "INSERT", "UPDATE", "DELETE", "REPLACE":
				t.Errorf("%s 不得含 DML（种子只在 000004）: 第 %d 行", m.File, st.Line)
			}
			if mm := createRe.FindStringSubmatch(st.Text); mm != nil {
				created = append(created, mm[1])
			}
		}
	}
	want := append([]string(nil), m1bTables...)
	sort.Strings(want)
	sort.Strings(created)
	if len(created) != 28 || join(created) != join(want) {
		t.Errorf("B 批应恰好 CREATE 28 张表\n实际 %v\n期望 %v", created, want)
	}
	// 计划（A+B 批）：全部 expand、无不可逆。
	abSet, _, err := migrate.Load(toMapFS(m1abFixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	if p := migrate.BuildPlan(abSet, nil); p.Count != len(m1abFiles()) || p.HasIrreversible || p.MaxClass != migrate.ClassExpand {
		t.Errorf("plan: %+v", p)
	}
}

// 逐表勾选：每张 B 批表在 docs/database 中都有对应的表节（### x.y `table`），文档与迁移一致（AC6 的全表核对在 1.9 汇总）。
func TestM1BEveryTableIsDocumentedInDatabaseDocs(t *testing.T) {
	var docs strings.Builder
	files, err := filepath.Glob("../../docs/database/*.md")
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到 docs/database: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		docs.Write(b)
	}
	for _, tbl := range m1bTables {
		re := regexp.MustCompile("(?m)^### [0-9.]+ `" + tbl + "`")
		if !re.MatchString(docs.String()) {
			t.Errorf("docs/database 缺少 %s 的表节", tbl)
		}
	}
}

// ---- 真实 MySQL：空库 up、重放幂等、授权 ----

func TestM1BUpOnEmptyDatabaseThenReplayIsIdempotent(t *testing.T) {
	ctx := ctxT(t)
	e, r := m1bUp(t)

	var business []string
	for _, tbl := range e.tables(t) {
		if !strings.HasPrefix(tbl, "schema_migrations") {
			business = append(business, tbl)
		}
	}
	if want := m1abTables(); len(business) != 49 || join(business) != join(want) {
		t.Fatalf("A+B 应恰好 49 张表\n实际 %v\n期望 %v", business, want)
	}
	wantVersions(t, e, "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11")

	// 生成列类型（README §2.2）：STORED 的基列无 CASCADE/SET NULL 外键；T? 表用 VIRTUAL。
	for _, c := range [][3]string{{"nodes", "active_name", "STORED GENERATED"}, {"sites", "active_name", "STORED GENERATED"},
		{"site_domains", "active_hostname", "STORED GENERATED"}, {"origin_groups", "default_flag", "STORED GENERATED"},
		{"cache_policies", "default_flag", "STORED GENERATED"}, {"site_rules", "active_name", "STORED GENERATED"},
		{"rule_templates", "tenant_key", "VIRTUAL GENERATED"}} {
		if got := e.scalar(t, "SELECT EXTRA FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?", e.dbName, c[0], c[1]); got != c[2] {
			t.Errorf("%s.%s 的生成列类型 = %q，期望 %s", c[0], c[1], got, c[2])
		}
	}
	// 心跳分区：按日 RANGE COLUMNS，含 pmax；无外键。
	parts := e.column(t, "SELECT PARTITION_NAME FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'node_heartbeats' ORDER BY PARTITION_ORDINAL_POSITION", e.dbName)
	if len(parts) != 9 || parts[0] != "p20261120" || parts[len(parts)-2] != "p20261127" || parts[len(parts)-1] != "pmax" {
		t.Errorf("node_heartbeats 分区 = %v（期望 p20261120…p20261127 + pmax）", parts)
	}
	if n := e.scalar(t, "SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND (TABLE_NAME = 'node_heartbeats' OR REFERENCED_TABLE_NAME = 'node_heartbeats')", e.dbName); n != "0" {
		t.Errorf("node_heartbeats 不得有外键: %s", n)
	}
	// 触发器：audit_logs 两个 + rule_versions 两个。
	if trg := e.column(t, "SELECT TRIGGER_NAME FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY TRIGGER_NAME", e.dbName); join(trg) != "trg_audit_logs_bd,trg_audit_logs_bu,trg_rule_versions_bd,trg_rule_versions_bu" {
		t.Errorf("触发器 = %v", trg)
	}
	// 循环外键：先建表后 ALTER ADD CONSTRAINT（SET NULL）；尚未能建的两条（目标表在 000012）此刻是无 FK 普通列。
	for _, fk := range []string{"fk_site_rules_current_version", "fk_site_rules_published_version"} {
		got := e.scalar(t, "SELECT CONCAT(REFERENCED_TABLE_NAME, ':', DELETE_RULE) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND TABLE_NAME = 'site_rules' AND CONSTRAINT_NAME = ?", e.dbName, fk)
		if got != "rule_versions:SET NULL" {
			t.Errorf("%s = %q，期望 rule_versions:SET NULL", fk, got)
		}
	}
	for _, c := range [][2]string{{"clusters", "confirmed_revision_id"}, {"sites", "last_deployment_id"}, {"certificates", "acme_account_id"}} {
		if n := e.scalar(t, "SELECT COUNT(*) FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ? AND REFERENCED_TABLE_NAME IS NOT NULL", e.dbName, c[0], c[1]); n != "0" {
			t.Errorf("%s.%s 此刻应为无 FK 普通列（外键随目标表所在迁移追加）", c[0], c[1])
		}
		if got := e.scalar(t, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?", e.dbName, c[0], c[1]); got != "1" {
			t.Errorf("%s.%s 列缺失", c[0], c[1])
		}
	}
	// cache_tasks 无 cluster_id（epoch 为站点级，IMP-04）。
	if n := e.scalar(t, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'cache_tasks' AND COLUMN_NAME = 'cluster_id'", e.dbName); n != "0" {
		t.Error("cache_tasks 不得有 cluster_id")
	}

	// ---- 逐表授权：app 对 49 张表各一条；仅 audit_logs 为追加只写；maint 仅 audit_logs 与 node_heartbeats ----
	app := e.grantsOf(t, migrate.AppUser)
	if len(app) != 49 {
		t.Fatalf("app 的表级授权应恰为 49 条，实际 %d: %v", len(app), app)
	}
	for _, tbl := range m1abTables() {
		w := "Delete,Insert,Select,Update"
		if tbl == "audit_logs" {
			w = "Insert,Select"
		}
		if app[tbl] != w {
			t.Errorf("app 对 %s 的授权 = %q，期望 %q", tbl, app[tbl], w)
		}
	}
	maint := e.grantsOf(t, migrate.MaintUser)
	if len(maint) != 2 || maint["audit_logs"] != "Alter,Drop,Select" || maint["node_heartbeats"] != "Alter,Drop,Select" {
		t.Errorf("maint 只应有 audit_logs 与 node_heartbeats 的 SELECT, ALTER, DROP: %v", maint)
	}
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

	// ---- 真实授权的最小权限负向用例（B 批表）----
	ap := e.appPool(t).Executor(ctx)
	denied := func(name string, q db.Query) {
		t.Helper()
		_, err := ap.Exec(ctx, q)
		if n, ok := mysqlErr(err); !ok || n != db.MySQLErrTableAccessDenied {
			t.Errorf("app %s 必须被表级授权拒绝（ERROR 1142）: %v", name, err)
		}
	}
	denied("DROP TABLE sites", db.Q("DROP TABLE sites"))
	denied("DROP TABLE node_heartbeats", db.Q("DROP TABLE node_heartbeats"))
	denied("TRUNCATE node_heartbeats", db.Q("TRUNCATE TABLE node_heartbeats"))
	denied("DROP PARTITION node_heartbeats", db.Q("ALTER TABLE node_heartbeats DROP PARTITION p20261120"))
	denied("REORGANIZE PARTITION node_heartbeats", db.Q("ALTER TABLE node_heartbeats REORGANIZE PARTITION pmax INTO (PARTITION pmax VALUES LESS THAN (MAXVALUE))"))
	denied("ALTER TABLE certificates", db.Q("ALTER TABLE certificates ADD COLUMN evil INT NULL"))
	denied("ALTER TABLE site_rules DROP FK", db.Q("ALTER TABLE site_rules DROP FOREIGN KEY fk_site_rules_current_version"))
	denied("CREATE TABLE", db.Q("CREATE TABLE evil_b (id INT)"))
	denied("DROP TRIGGER rule_versions", db.Q("DROP TRIGGER trg_rule_versions_bu"))
	denied("GRANT", db.Q("GRANT ALL ON nodes TO lingedge_app"))
	// app 对 B 批业务表的 DML 照常可用（逐表授权没有过窄）。
	if _, err := ap.Exec(ctx, db.Q("INSERT INTO regions (uid, code, name) VALUES (?, 'cn-east', 'East')"), uid26()); err != nil {
		t.Errorf("app 应能写 regions: %v", err)
	}
	if _, err := ap.Exec(ctx, db.Q("DELETE FROM regions WHERE code = 'cn-east'")); err != nil {
		t.Errorf("app 应能删 regions: %v", err)
	}
	// rule_versions 即使对 app 有表级 UPDATE/DELETE，也被触发器拒绝（第二道防线）。
	c := e.conn(t)
	tenantID := e.scalar(t, "SELECT id FROM `"+e.dbName+"`.tenants WHERE is_default = 1")
	var siteID, ruleID int64
	{
		res, err := c.ExecContext(ctx, "INSERT INTO sites (uid, tenant_id, name) VALUES (?, ?, 'priv-site')", uid26(), tenantID)
		if err != nil {
			t.Fatal(err)
		}
		siteID, _ = res.LastInsertId()
		res, err = c.ExecContext(ctx, "INSERT INTO site_rules (uid, tenant_id, site_id, rule_type, phase, name, doc_json, doc_sha256) VALUES (?, ?, ?, 'redirect', 'redirect', 'r', JSON_OBJECT(), ?)", uid26(), tenantID, siteID, fmt.Sprintf("%064d", 1))
		if err != nil {
			t.Fatal(err)
		}
		ruleID, _ = res.LastInsertId()
	}
	if _, err := ap.Exec(ctx, db.Q("INSERT INTO rule_versions (uid, tenant_id, site_id, rule_id, version_no, rule_type, phase, priority, enabled, doc_json, doc_sha256) VALUES (?, ?, ?, ?, 1, 'redirect', 'redirect', 1000, 1, JSON_OBJECT(), ?)"),
		uid26(), tenantID, siteID, ruleID, fmt.Sprintf("%064d", 1)); err != nil {
		t.Fatalf("app 应能 INSERT rule_versions: %v", err)
	}
	for s, q := range map[string]db.Query{"UPDATE": db.Q("UPDATE rule_versions SET priority = 1"), "DELETE": db.Q("DELETE FROM rule_versions")} {
		_, err := ap.Exec(ctx, q)
		if n, ok := mysqlErr(err); !ok || n != errSignal {
			t.Errorf("app 的 %s rule_versions 必须被不可变触发器拒绝（1644）: %v", s, err)
		}
	}

	// ---- lingedge_maint：可对 node_heartbeats REORGANIZE / DROP PARTITION，不能碰其它表 ----
	mx := e.maintPool(t).Executor(ctx)
	mdenied := func(name string, q db.Query) {
		t.Helper()
		_, err := mx.Exec(ctx, q)
		if n, ok := mysqlErr(err); !ok || n != db.MySQLErrTableAccessDenied {
			t.Errorf("maint %s 必须被拒绝（ERROR 1142）: %v", name, err)
		}
	}
	mdenied("DROP TABLE nodes", db.Q("DROP TABLE nodes"))
	mdenied("ALTER nodes", db.Q("ALTER TABLE nodes ADD COLUMN evil INT NULL"))
	mdenied("SELECT sites", db.Q("SELECT 1 FROM sites"))
	mdenied("DELETE node_heartbeats", db.Q("DELETE FROM node_heartbeats"))
	mdenied("INSERT node_heartbeats", db.Q("INSERT INTO node_heartbeats (node_id, received_at, agent_version, openresty_state) VALUES (1, NOW(3), 'x', 'running')"))
	mdenied("UPDATE node_heartbeats", db.Q("UPDATE node_heartbeats SET agent_version = 'x'"))
	mdenied("ALTER site_rules", db.Q("ALTER TABLE site_rules DROP FOREIGN KEY fk_site_rules_current_version"))
	if _, err := mx.Exec(ctx, db.Q("ALTER TABLE node_heartbeats REORGANIZE PARTITION pmax INTO (PARTITION p20261128 VALUES LESS THAN ('2026-11-29 00:00:00'), PARTITION pmax VALUES LESS THAN (MAXVALUE))")); err != nil {
		t.Errorf("maint 应能 REORGANIZE pmax（滚动预建分区）: %v", err)
	}
	if _, err := c.ExecContext(ctx, "INSERT INTO node_heartbeats (node_id, received_at, agent_version, openresty_state) VALUES (1, '2026-11-20 10:00:00.000', 'old', 'running')"); err != nil {
		t.Fatal(err)
	}
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.node_heartbeats PARTITION (p20261120)"); got != "1" {
		t.Fatalf("p20261120 应含该行: %s", got)
	}
	if _, err := mx.Exec(ctx, db.Q("ALTER TABLE node_heartbeats DROP PARTITION p20261120")); err != nil {
		t.Errorf("maint 应能 DROP PARTITION（保留期清理）: %v", err)
	}
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.node_heartbeats WHERE agent_version = 'old'"); got != "0" {
		t.Errorf("DROP PARTITION 后旧行应消失: %s", got)
	}

	// ---- 重放幂等：Up 再次为空操作；把 B 批每条语句（含 PREPARE 守卫）再执行一遍，库结构/授权不变 ----
	sigBefore := e.schemaSignature(t)
	trgBefore := e.column(t, "SELECT CONCAT(TRIGGER_NAME, ':', ACTION_TIMING, ':', EVENT_MANIPULATION) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY 1", e.dbName)
	fkBefore := e.scalar(t, "SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ?", e.dbName)
	if res, err := r.Up(ctx, migrate.UpOptions{}); err != nil || len(res.Applied) != 0 {
		t.Fatalf("第二次 Up 应为空操作: %+v %v", res, err)
	}
	rc := e.conn(t)
	var replayed, guards int
	for _, m := range r.Set().Migrations[len(m1aFiles):] {
		for _, st := range m.Up {
			if strings.Contains(st.Lead, "lingedge:guard=") {
				guards++
			}
			if _, err := rc.ExecContext(ctx, st.Text); err != nil {
				t.Fatalf("%s 第 %d 行重放失败（迁移必须可重入）: %v\n%.120s", m.File, st.Line, err, st.Text)
			}
			replayed++
		}
	}
	if replayed < 60 || guards != 2 {
		t.Fatalf("重放语句数/守卫数异常: %d / %d", replayed, guards)
	}
	if sigAfter := e.schemaSignature(t); sigAfter != sigBefore {
		t.Error("重放全部 B 批语句后库结构与授权不得变化")
	}
	if trgAfter := e.column(t, "SELECT CONCAT(TRIGGER_NAME, ':', ACTION_TIMING, ':', EVENT_MANIPULATION) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY 1", e.dbName); join(trgAfter) != join(trgBefore) {
		t.Errorf("重放后触发器变化: %v → %v", trgBefore, trgAfter)
	}
	if fkAfter := e.scalar(t, "SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ?", e.dbName); fkAfter != fkBefore {
		t.Errorf("重放后外键数变化: %s → %s", fkBefore, fkAfter)
	}

	// expand 迁移不提供数据库回退。
	_, err := r.Down(ctx, migrate.DownOptions{})
	var dn *migrate.ErrDownNotAllowed
	if !errors.As(err, &dn) || dn.File != "000011_cache.sql" {
		t.Errorf("expand 应拒绝回退: %v", err)
	}
	if p, _ := r.Plan(ctx); p.Count != 0 {
		t.Errorf("plan 应为空: %+v", p)
	}
}

// ---- 真实 MySQL：schema 约定 ----

func TestM1BSchemaConventionsHoldOnRealDatabase(t *testing.T) {
	e, _ := m1bUp(t)
	dbn := e.dbName

	if bad := e.column(t, "SELECT CONCAT(TABLE_NAME, '.', COLUMN_NAME, ':', DATA_TYPE) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME NOT LIKE 'schema_migrations%' AND DATA_TYPE IN ('enum','set','float','double','decimal','timestamp','time')", dbn); len(bad) != 0 {
		t.Errorf("存在违反类型约定的列: %v", bad)
	}
	if bad := e.column(t, "SELECT CONCAT(TABLE_NAME, '.', COLUMN_NAME) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME NOT LIKE 'schema_migrations%' AND DATA_TYPE = 'datetime' AND DATETIME_PRECISION <> 3", dbn); len(bad) != 0 {
		t.Errorf("DATETIME 必须是 (3): %v", bad)
	}
	if bad := e.column(t, "SELECT CONCAT(TABLE_NAME, '.', COLUMN_NAME) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME NOT LIKE 'schema_migrations%' AND DATA_TYPE IN ('tinytext','text','mediumtext','longtext','tinyblob','blob','mediumblob','longblob','json') AND (COLUMN_DEFAULT IS NOT NULL OR EXTRA LIKE '%DEFAULT_GENERATED%')", dbn); len(bad) != 0 {
		t.Errorf("TEXT/JSON/BLOB 列不得带 DEFAULT: %v", bad)
	}
	if bad := e.column(t, "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME NOT LIKE 'schema_migrations%' AND (ENGINE <> 'InnoDB' OR TABLE_COLLATION <> 'utf8mb4_0900_ai_ci')", dbn); len(bad) != 0 {
		t.Errorf("引擎/排序规则不符: %v", bad)
	}
	if bad := e.column(t, "SELECT t.TABLE_NAME FROM information_schema.TABLES t WHERE t.TABLE_SCHEMA = ? AND t.TABLE_NAME NOT LIKE 'schema_migrations%' AND NOT EXISTS (SELECT 1 FROM information_schema.TABLE_CONSTRAINTS c WHERE c.TABLE_SCHEMA = t.TABLE_SCHEMA AND c.TABLE_NAME = t.TABLE_NAME AND c.CONSTRAINT_TYPE = 'PRIMARY KEY')", dbn); len(bad) != 0 {
		t.Errorf("缺主键: %v", bad)
	}
	// 所有指向 tenants(id) 的外键 ON DELETE RESTRICT；所有外键无 ON UPDATE CASCADE/SET NULL。
	if bad := e.column(t, "SELECT CONCAT(TABLE_NAME, '.', CONSTRAINT_NAME, ':', DELETE_RULE) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND REFERENCED_TABLE_NAME = 'tenants' AND DELETE_RULE <> 'RESTRICT'", dbn); len(bad) != 0 {
		t.Errorf("指向 tenants 的外键必须 RESTRICT: %v", bad)
	}
	for _, tbl := range append([]string{"node_tasks", "rule_templates"}, m1bTenantTables...) {
		n := e.scalar(t, "SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND TABLE_NAME = ? AND REFERENCED_TABLE_NAME = 'tenants'", dbn, tbl)
		if n != "1" {
			t.Errorf("%s 应有且仅有一条指向 tenants 的外键: %s", tbl, n)
		}
	}
	if bad := e.column(t, "SELECT CONCAT(TABLE_NAME, '.', CONSTRAINT_NAME, ':', UPDATE_RULE) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND UPDATE_RULE IN ('CASCADE','SET NULL','SET DEFAULT')", dbn); len(bad) != 0 {
		t.Errorf("外键不得 ON UPDATE CASCADE/SET NULL: %v", bad)
	}
	// 站点子表的复合外键 (tenant_id, site_id) → sites(tenant_id, id)（F-3 / AC3）。
	for _, tbl := range []string{"certificates", "site_domains", "site_settings", "origin_groups", "origins", "site_cluster_bindings", "node_tasks", "site_rules", "rule_versions", "cache_policies", "cache_tasks"} {
		cols := e.scalar(t, `SELECT GROUP_CONCAT(k.COLUMN_NAME ORDER BY k.ORDINAL_POSITION) FROM information_schema.KEY_COLUMN_USAGE k
			JOIN information_schema.REFERENTIAL_CONSTRAINTS r ON r.CONSTRAINT_SCHEMA = k.TABLE_SCHEMA AND r.CONSTRAINT_NAME = k.CONSTRAINT_NAME AND r.TABLE_NAME = k.TABLE_NAME
			WHERE k.TABLE_SCHEMA = ? AND k.TABLE_NAME = ? AND r.REFERENCED_TABLE_NAME = 'sites' AND r.UNIQUE_CONSTRAINT_NAME = 'uk_sites_tenant_id'`, dbn, tbl)
		if cols != "tenant_id,site_id" {
			t.Errorf("%s 应有复合外键 (tenant_id, site_id) → sites(tenant_id, id): %q", tbl, cols)
		}
	}
	if got := e.scalar(t, "SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'sites' AND INDEX_NAME = 'uk_sites_tenant_id' AND NON_UNIQUE = 0", dbn); got != "tenant_id,id" {
		t.Errorf("sites 应含 UNIQUE(tenant_id,id): %q", got)
	}
	if got := e.scalar(t, "SELECT CONCAT(r.REFERENCED_TABLE_NAME, ':', r.UNIQUE_CONSTRAINT_NAME, ':', r.DELETE_RULE) FROM information_schema.REFERENTIAL_CONSTRAINTS r WHERE r.CONSTRAINT_SCHEMA = ? AND r.TABLE_NAME = 'origins' AND r.CONSTRAINT_NAME = 'fk_origins_origin_groups'", dbn); got != "origin_groups:uk_origin_groups_site_id:CASCADE" {
		t.Errorf("origins 应含 (site_id, origin_group_id) → origin_groups(site_id, id): %q", got)
	}
	if got := e.scalar(t, "SELECT DELETE_RULE FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND TABLE_NAME = 'cache_policies' AND CONSTRAINT_NAME = 'fk_cache_policies_users'", dbn); got != "RESTRICT" {
		t.Errorf("cache_policies.risk_ack_by 外键应为 RESTRICT: %q", got)
	}
	// 高频/分区表无外键；C2u 表有 uid 唯一。
	for _, tbl := range []string{"node_events", "node_tasks"} {
		if got := e.scalar(t, "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = 'uid' AND NON_UNIQUE = 0 AND SEQ_IN_INDEX = 1", dbn, tbl); got != "1" {
			t.Errorf("%s 应为 C2u（uid CHAR(26) UNIQUE）: %s", tbl, got)
		}
	}

	// 租户表（tenant_id NOT NULL）：非主键索引以 tenant_id 打头；唯一例外是迁移文本里登记的 cross-tenant-index 与外键支撑索引。
	tenantTbls := e.column(t, "SELECT TABLE_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND COLUMN_NAME = 'tenant_id' AND IS_NULLABLE = 'NO' AND TABLE_NAME NOT LIKE 'schema_migrations%' ORDER BY 1", dbn)
	wantTenant := append([]string{"tenant_members", "tenant_quotas"}, m1bTenantTables...)
	sort.Strings(wantTenant)
	if join(tenantTbls) != join(wantTenant) {
		t.Fatalf("tenant_id NOT NULL 的租户表 = %v，期望 %v", tenantTbls, wantTenant)
	}
	registered := map[string]bool{}
	for _, f := range m1bFiles {
		b, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`lingedge:cross-tenant-index=(\w+)`).FindAllStringSubmatch(string(b), -1) {
			registered[m[1]] = true
		}
	}
	for _, tbl := range m1bTenantTables {
		for _, idx := range e.column(t, `SELECT DISTINCT s.INDEX_NAME FROM information_schema.STATISTICS s
			WHERE s.TABLE_SCHEMA = ? AND s.TABLE_NAME = ? AND s.INDEX_NAME <> 'PRIMARY' AND s.SEQ_IN_INDEX = 1 AND s.COLUMN_NAME <> 'tenant_id'
			AND NOT (s.NON_UNIQUE = 0 AND (SELECT COUNT(*) FROM information_schema.STATISTICS u WHERE u.TABLE_SCHEMA = s.TABLE_SCHEMA AND u.TABLE_NAME = s.TABLE_NAME AND u.INDEX_NAME = s.INDEX_NAME) = 1 AND s.COLUMN_NAME = 'uid')
			AND s.INDEX_NAME NOT IN (SELECT CONSTRAINT_NAME FROM information_schema.TABLE_CONSTRAINTS WHERE TABLE_SCHEMA = s.TABLE_SCHEMA AND TABLE_NAME = s.TABLE_NAME AND CONSTRAINT_TYPE = 'FOREIGN KEY')`, dbn, tbl) {
			if !registered[idx] {
				t.Errorf("%s.%s 未以 tenant_id 打头且未登记 cross-tenant-index 豁免", tbl, idx)
			}
		}
	}
	// 平台级/T? 表：无 tenant_id NOT NULL。
	for _, tbl := range []string{"regions", "isp_lines", "ip_pools", "nodes", "ip_pool_members", "clusters", "cluster_nodes", "enroll_tokens", "node_certificates", "node_events", "node_heartbeats"} {
		if n := e.scalar(t, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = 'tenant_id'", dbn, tbl); n != "0" {
			t.Errorf("平台级表 %s 不得有 tenant_id", tbl)
		}
	}
	for _, tbl := range []string{"rule_templates", "node_tasks"} {
		if got := e.scalar(t, "SELECT IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = 'tenant_id'", dbn, tbl); got != "YES" {
			t.Errorf("%s.tenant_id 应为可空（T?）: %q", tbl, got)
		}
	}
}

// ---- 真实 MySQL：约束负向用例 ----

func TestM1BConstraintsOnRealDatabase(t *testing.T) {
	e, _ := m1bUp(t)
	c := e.conn(t)
	oa := loadOpenAPI(t)
	run := func(name string, fn func(c *cx)) {
		t.Run(name, func(t *testing.T) { inTx(t, e, c, fn) })
	}
	hash := func() string { return fmt.Sprintf("%064d", next()) }
	node := func(c *cx) int64 {
		return c.ok("INSERT INTO nodes (uid, name) VALUES (?, ?)", uid26(), fmt.Sprintf("n%d", next()))
	}
	cluster := func(c *cx) int64 {
		return c.ok("INSERT INTO clusters (uid, name, settings_json) VALUES (?, ?, JSON_OBJECT())", uid26(), fmt.Sprintf("c%d", next()))
	}
	site := func(c *cx, tenantID int64) int64 {
		return c.ok("INSERT INTO sites (uid, tenant_id, name) VALUES (?, ?, ?)", uid26(), tenantID, fmt.Sprintf("s%d", next()))
	}
	host := func() string { return fmt.Sprintf("h%d.example.test", next()) }
	cert := func(c *cx, tenantID int64, siteID any) int64 {
		return c.ok(`INSERT INTO certificates (uid, tenant_id, site_id, name, source, primary_domain, domains_json, serial, issuer, not_before, not_after,
			fingerprint_sha256, key_algorithm, chain_pem, private_key_enc, enc_key_id) VALUES (?, ?, ?, 'c', 'upload', 'a.example.test', JSON_ARRAY('a.example.test'), ?, 'iss',
			'2026-01-01', '2027-01-01', ?, 'ecdsa_p256', 'PEM', 'v1:k:w:n:c', 'k1')`, uid26(), tenantID, siteID, fmt.Sprintf("s%d", next()), hash())
	}
	domain := func(c *cx, tenantID, siteID int64, h string) int64 {
		return c.ok("INSERT INTO site_domains (uid, tenant_id, site_id, hostname) VALUES (?, ?, ?, ?)", uid26(), tenantID, siteID, h)
	}
	ogroup := func(c *cx, tenantID, siteID int64, isDefault int) int64 {
		return c.ok("INSERT INTO origin_groups (uid, tenant_id, site_id, name, is_default) VALUES (?, ?, ?, ?, ?)", uid26(), tenantID, siteID, fmt.Sprintf("g%d", next()), isDefault)
	}
	nodeTask := func(c *cx, nodeID int64) int64 {
		return c.ok("INSERT INTO node_tasks (uid, node_id, kind, payload_json, request_id) VALUES (?, ?, 'cert_reload', JSON_OBJECT(), ?)", uid26(), nodeID, uid26())
	}
	rule := func(c *cx, tenantID, siteID int64, name string) int64 {
		return c.ok("INSERT INTO site_rules (uid, tenant_id, site_id, rule_type, phase, name, doc_json, doc_sha256) VALUES (?, ?, ?, 'redirect', 'redirect', ?, JSON_OBJECT(), ?)", uid26(), tenantID, siteID, name, hash())
	}
	ver := func(c *cx, tenantID, siteID, ruleID int64, no int) int64 {
		return c.ok(`INSERT INTO rule_versions (uid, tenant_id, site_id, rule_id, version_no, rule_type, phase, priority, enabled, doc_json, doc_sha256)
			VALUES (?, ?, ?, ?, ?, 'redirect', 'redirect', 1000, 1, JSON_OBJECT(), ?)`, uid26(), tenantID, siteID, ruleID, no, hash())
	}
	policy := func(c *cx, tenantID, siteID int64, isDefault int) int64 {
		return c.ok("INSERT INTO cache_policies (uid, tenant_id, site_id, name, is_default, cacheable_statuses_json) VALUES (?, ?, ?, ?, ?, JSON_ARRAY(200))", uid26(), tenantID, siteID, fmt.Sprintf("p%d", next()), isDefault)
	}

	// ======== 000006 nodes_clusters ========
	run("regions / isp_lines / ip_pools：唯一、自引用外键", func(c *cx) {
		r1 := c.ok("INSERT INTO regions (uid, code, name) VALUES (?, 'cn-east', 'East')", uid26())
		c.bad(errDup, "INSERT INTO regions (uid, code, name) VALUES (?, 'cn-east', 'dup')", uid26())
		c.bad(errNoRefRow, "INSERT INTO regions (uid, code, name, parent_id) VALUES (?, 'cn-x', 'x', 999999999)", uid26())
		r2 := c.ok("INSERT INTO regions (uid, code, name, parent_id) VALUES (?, 'cn-sh', 'SH', ?)", uid26(), r1)
		c.ok("DELETE FROM regions WHERE id = ?", r1) // parent_id → SET NULL
		if !c.isNull("SELECT parent_id FROM regions WHERE id = ?", r2) {
			t.Error("父区域删除后 parent_id 应置 NULL")
		}
		c.ok("INSERT INTO isp_lines (uid, code, name) VALUES (?, 'telecom', 'T')", uid26())
		c.bad(errDup, "INSERT INTO isp_lines (uid, code, name) VALUES (?, 'telecom', 'T2')", uid26())
		c.ok("INSERT INTO ip_pools (uid, name, ip_family) VALUES (?, 'pool-a', 'v4')", uid26())
		c.bad(errDup, "INSERT INTO ip_pools (uid, name, ip_family) VALUES (?, 'pool-a', 'v6')", uid26())
		c.bad(errCheckViolated, "INSERT INTO ip_pools (uid, name, ip_family) VALUES (?, 'pool-b', 'v5')", uid26())
	})
	run("nodes：状态值域、活动名称唯一、软删除复用、区域外键 SET NULL", func(c *cx) {
		id := c.ok("INSERT INTO nodes (uid, name) VALUES (?, 'edge-1')", uid26())
		if got := c.count("SELECT COUNT(*) FROM nodes WHERE id = ? AND status = 'pending_enroll'", id); got != 1 {
			t.Error("nodes.status 默认应为 pending_enroll")
		}
		c.bad(errDup, "INSERT INTO nodes (uid, name) VALUES (?, 'edge-1')", uid26())
		c.bad(errCheckViolated, "INSERT INTO nodes (uid, name, status) VALUES (?, 'edge-2', 'exploded')", uid26())
		c.ok("UPDATE nodes SET deleted_at = NOW(3) WHERE id = ?", id)
		c.ok("INSERT INTO nodes (uid, name) VALUES (?, 'edge-1')", uid26()) // 软删除后可复用
		c.bad(errNoDefault, "INSERT INTO nodes (uid) VALUES (?)", uid26())
		rg := c.ok("INSERT INTO regions (uid, code, name) VALUES (?, 'r1', 'r')", uid26())
		isp := c.ok("INSERT INTO isp_lines (uid, code, name) VALUES (?, 'i1', 'i')", uid26())
		n := c.ok("INSERT INTO nodes (uid, name, region_id, isp_line_id) VALUES (?, 'edge-3', ?, ?)", uid26(), rg, isp)
		c.bad(errNoRefRow, "INSERT INTO nodes (uid, name, region_id) VALUES (?, 'edge-4', 999999999)", uid26())
		c.ok("DELETE FROM regions WHERE id = ?", rg)
		c.ok("DELETE FROM isp_lines WHERE id = ?", isp)
		if !c.isNull("SELECT region_id FROM nodes WHERE id = ?", n) || !c.isNull("SELECT isp_line_id FROM nodes WHERE id = ?", n) {
			t.Error("区域/线路删除后 nodes 外键应置 NULL")
		}
	})
	run("ip_pool_members：address 全局唯一、级联与 SET NULL", func(c *cx) {
		p1 := c.ok("INSERT INTO ip_pools (uid, name, ip_family) VALUES (?, 'p1', 'v4')", uid26())
		p2 := c.ok("INSERT INTO ip_pools (uid, name, ip_family) VALUES (?, 'p2', 'v4')", uid26())
		n := node(c)
		ins := "INSERT INTO ip_pool_members (ip_pool_id, node_id, address, role, status) VALUES (?, ?, ?, ?, ?)"
		m := c.ok(ins, p1, n, "203.0.113.1", "serving", "active")
		c.bad(errDup, ins, p2, nil, "203.0.113.1", "serving", "active") // 地址全局唯一，不可属于两个池
		c.bad(errCheckViolated, ins, p1, nil, "203.0.113.2", "bogus", "active")
		c.bad(errCheckViolated, ins, p1, nil, "203.0.113.3", "serving", "bogus")
		c.bad(errNoRefRow, ins, 999999999, nil, "203.0.113.4", "serving", "active")
		c.ok("DELETE FROM nodes WHERE id = ?", n)
		if !c.isNull("SELECT node_id FROM ip_pool_members WHERE id = ?", m) {
			t.Error("节点删除后成员 node_id 应置 NULL")
		}
		c.ok("DELETE FROM ip_pools WHERE id = ?", p1) // 池删除级联成员
		if c.count("SELECT COUNT(*) FROM ip_pool_members WHERE id = ?", m) != 0 {
			t.Error("池删除应级联删除成员")
		}
	})
	run("clusters / cluster_nodes：唯一、RESTRICT、值域；confirmed_revision_id 暂为无 FK 列", func(c *cx) {
		cl := cluster(c)
		c.bad(errNoDefault, "INSERT INTO clusters (uid, name) VALUES (?, 'nosettings')", uid26()) // settings_json 无默认
		c.bad(errCheckViolated, "INSERT INTO clusters (uid, name, status, settings_json) VALUES (?, 'bad', 'x', JSON_OBJECT())", uid26())
		c.ok("UPDATE clusters SET confirmed_revision_id = 999999999 WHERE id = ?", cl) // 000012 前无 FK
		n := node(c)
		ins := "INSERT INTO cluster_nodes (cluster_id, node_id, serving_state) VALUES (?, ?, ?)"
		c.ok(ins, cl, n, "active")
		c.bad(errDup, ins, cl, n, "active")
		c.bad(errCheckViolated, ins, cl, node(c), "zombie")
		c.ok(ins, cluster(c), n, "standby") // 一节点可入多集群（F-3）
		c.bad(errRowIsRef, "DELETE FROM clusters WHERE id = ?", cl)
		c.bad(errRowIsRef, "DELETE FROM nodes WHERE id = ?", n)
	})
	run("enroll_tokens：仅存 64 位十六进制哈希、一次性、角色值域", func(c *cx) {
		n := node(c)
		ins := "INSERT INTO enroll_tokens (uid, node_id, role, token_hash, expires_at) VALUES (?, ?, ?, ?, '2030-01-01')"
		h := hash()
		c.ok(ins, uid26(), n, "node", h)
		c.bad(errDup, ins, uid26(), n, "node", h)
		c.bad(errCheckViolated, ins, uid26(), n, "node", "short")
		c.bad(errCheckViolated, ins, uid26(), n, "node", strings.Repeat("G", 64))
		c.bad(errCheckViolated, ins, uid26(), n, "node", strings.ToUpper(strings.Repeat("ab", 32))) // 须小写十六进制（区分大小写）
		c.bad(errCheckViolated, ins, uid26(), n, "root", hash())
		c.bad(errNoRefRow, ins, uid26(), 999999999, "node", hash())
		c.bad(errNoDefault, "INSERT INTO enroll_tokens (uid, node_id, token_hash) VALUES (?, ?, ?)", uid26(), n, hash()) // expires_at 必填
		u := c.user("enroller@example.test")
		tk := c.ok("INSERT INTO enroll_tokens (uid, node_id, token_hash, expires_at, created_by) VALUES (?, ?, ?, '2030-01-01', ?)", uid26(), n, hash(), u)
		c.ok("DELETE FROM users WHERE id = ?", u)
		if !c.isNull("SELECT created_by FROM enroll_tokens WHERE id = ?", tk) {
			t.Error("创建者删除后 created_by 应置 NULL")
		}
		c.ok("DELETE FROM nodes WHERE id = ?", n) // 令牌随节点级联
		if c.count("SELECT COUNT(*) FROM enroll_tokens WHERE node_id = ?", n) != 0 {
			t.Error("节点删除应级联删除其令牌")
		}
	})
	run("node_certificates：主体与 node_id 一致、有效期、序列号唯一", func(c *cx) {
		n := node(c)
		ins := "INSERT INTO node_certificates (subject_type, subject_id, node_id, serial, fingerprint_sha256, issuer_key_id, not_before, not_after) VALUES (?, ?, ?, ?, ?, 'ca1', ?, ?)"
		ser := fmt.Sprintf("ser%d", next())
		c.ok(ins, "node", n, n, ser, hash(), "2026-01-01", "2026-02-01")
		c.bad(errDup, ins, "node", n, n, ser, hash(), "2026-01-01", "2026-02-01")
		c.bad(errCheckViolated, ins, "node", n, nil, fmt.Sprintf("ser%d", next()), hash(), "2026-01-01", "2026-02-01") // node 主体必有 node_id
		c.bad(errCheckViolated, ins, "dns", 1, n, fmt.Sprintf("ser%d", next()), hash(), "2026-01-01", "2026-02-01")    // 非 node 主体不得有 node_id
		c.ok(ins, "dns", 1, nil, fmt.Sprintf("ser%d", next()), hash(), "2026-01-01", "2026-02-01")
		c.bad(errCheckViolated, ins, "robot", 1, nil, fmt.Sprintf("ser%d", next()), hash(), "2026-01-01", "2026-02-01")
		c.bad(errCheckViolated, ins, "node", n, n, fmt.Sprintf("ser%d", next()), hash(), "2026-02-01", "2026-01-01") // not_before < not_after
		c.bad(errNoRefRow, ins, "node", 1, 999999999, fmt.Sprintf("ser%d", next()), hash(), "2026-01-01", "2026-02-01")
		c.ok("DELETE FROM nodes WHERE id = ?", n) // 证书台账随节点级联（CHECK 引用的 FK 为 CASCADE，可建）
		if c.count("SELECT COUNT(*) FROM node_certificates WHERE node_id = ?", n) != 0 {
			t.Error("节点删除应级联删除证书台账")
		}
	})
	run("node_events：uid 幂等去重、severity、级联", func(c *cx) {
		n := node(c)
		ins := "INSERT INTO node_events (uid, node_id, event_type, severity, message) VALUES (?, ?, 'online', ?, 'm')"
		u := uid26()
		c.ok(ins, u, n, "info")
		c.bad(errDup, ins, u, n, "info")
		c.ok("INSERT IGNORE INTO node_events (uid, node_id, event_type, message) VALUES (?, ?, 'online', 'm')", u, n) // Agent 重放：静默去重
		if c.count("SELECT COUNT(*) FROM node_events WHERE uid = ?", u) != 1 {
			t.Error("INSERT IGNORE 应去重")
		}
		c.bad(errCheckViolated, ins, uid26(), n, "fatal")
		c.bad(errNoRefRow, ins, uid26(), 999999999, "info")
		c.ok("DELETE FROM nodes WHERE id = ?", n)
		if c.count("SELECT COUNT(*) FROM node_events WHERE uid = ?", u) != 0 {
			t.Error("节点删除应级联删除事件")
		}
	})
	run("node_heartbeats：无外键、pmax 兜底、CHECK", func(c *cx) {
		ins := "INSERT INTO node_heartbeats (node_id, received_at, agent_version, openresty_state, cpu_permille) VALUES (?, ?, 'v1', ?, ?)"
		c.ok(ins, 999999999, "2026-11-22 08:00:00.123", "running", 500) // 无外键：节点可以不存在
		c.ok(ins, 1, "2020-01-01 00:00:00.000", "running", nil)         // 早于首个分区：落入 p20261120
		c.ok(ins, 1, "2099-01-01 00:00:00.000", "unknown", 1000)        // 越过全部分区：落入 pmax，不报 ERROR 1526
		if n := c.count("SELECT COUNT(*) FROM node_heartbeats PARTITION (pmax)"); n != 1 {
			t.Errorf("2099 年的心跳应落入 pmax: %d", n)
		}
		c.bad(errCheckViolated, ins, 1, "2026-11-22 08:00:00.000", "exploded", 1)
		c.bad(errCheckViolated, ins, 1, "2026-11-22 08:00:00.000", "running", 1001)
		c.bad(errNoDefault, "INSERT INTO node_heartbeats (node_id, received_at, openresty_state) VALUES (1, NOW(3), 'running')") // agent_version 必填
	})

	// ======== 000007 sites_core ========
	run("site_groups：租户内名称唯一，跨租户可同名；租户 RESTRICT", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		ins := "INSERT INTO site_groups (uid, tenant_id, name) VALUES (?, ?, ?)"
		c.ok(ins, uid26(), t1, "g")
		c.ok(ins, uid26(), t2, "g")
		c.bad(errDup, ins, uid26(), t1, "g")
		c.bad(errNoRefRow, ins, uid26(), 999999999, "x")
		c.bad(errRowIsRef, "DELETE FROM tenants WHERE id = ?", t1)
	})
	run("sites：活动名称唯一（租户内）、软删除复用、版本 CHECK、SET NULL 外键", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		ins := "INSERT INTO sites (uid, tenant_id, name) VALUES (?, ?, ?)"
		s := c.ok(ins, uid26(), t1, "shop")
		c.ok(ins, uid26(), t2, "shop") // 他租户同名可
		c.bad(errDup, ins, uid26(), t1, "shop")
		c.ok("UPDATE sites SET deleted_at = NOW(3) WHERE id = ?", s)
		c.ok(ins, uid26(), t1, "shop") // 软删除后可复用
		c.bad(errCheckViolated, "INSERT INTO sites (uid, tenant_id, name, status) VALUES (?, ?, 'x', 'boom')", uid26(), t1)
		c.bad(errCheckViolated, "INSERT INTO sites (uid, tenant_id, name, config_version, published_config_version) VALUES (?, ?, 'v', 1, 5)", uid26(), t1) // 已发布版本不得超前
		c.ok("INSERT INTO sites (uid, tenant_id, name, config_version, published_config_version) VALUES (?, ?, 'v2', 5, 5)", uid26(), t1)
		c.bad(errNoRefRow, "INSERT INTO sites (uid, tenant_id, name) VALUES (?, 999999999, 'x')", uid26())
		c.ok("UPDATE sites SET last_deployment_id = 999999999 WHERE id = ?", s) // 000012 前无 FK
		g := c.ok("INSERT INTO site_groups (uid, tenant_id, name) VALUES (?, ?, 'grp')", uid26(), t1)
		s2 := c.ok("INSERT INTO sites (uid, tenant_id, name, group_id) VALUES (?, ?, 'in-grp', ?)", uid26(), t1, g)
		c.ok("DELETE FROM site_groups WHERE id = ?", g)
		if !c.isNull("SELECT group_id FROM sites WHERE id = ?", s2) {
			t.Error("分组删除后 sites.group_id 应置 NULL")
		}
		u := c.user("publisher@example.test")
		s3 := c.ok("INSERT INTO sites (uid, tenant_id, name, published_by) VALUES (?, ?, 'pub', ?)", uid26(), t1, u)
		c.ok("DELETE FROM users WHERE id = ?", u)
		if !c.isNull("SELECT published_by FROM sites WHERE id = ?", s3) {
			t.Error("发布者删除后 published_by 应置 NULL")
		}
		c.bad(errRowIsRef, "DELETE FROM tenants WHERE id = ?", t1)
	})

	// ======== 000008 certificates ========
	run("certificates：值域、私钥无默认、租户/站点复合外键", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		s1, s2 := site(c, t1), site(c, t2)
		cert(c, t1, s1)
		cert(c, t1, nil) // 租户级证书（不绑站点）
		c.bad(errNoRefRow, `INSERT INTO certificates (uid, tenant_id, site_id, name, source, primary_domain, domains_json, serial, issuer, not_before, not_after,
			fingerprint_sha256, key_algorithm, chain_pem, private_key_enc, enc_key_id) VALUES (?, ?, ?, 'c', 'upload', 'a', JSON_ARRAY(), 's', 'i', '2026-01-01', '2027-01-01', ?, 'rsa2048', 'P', 'e', 'k')`,
			uid26(), t1, s2, hash()) // 他租户站点：复合外键拒绝
		c.bad(errNoRefRow, `INSERT INTO certificates (uid, tenant_id, site_id, name, source, primary_domain, domains_json, serial, issuer, not_before, not_after,
			fingerprint_sha256, key_algorithm, chain_pem, private_key_enc, enc_key_id) VALUES (?, 999999999, NULL, 'c', 'upload', 'a', JSON_ARRAY(), 's', 'i', '2026-01-01', '2027-01-01', ?, 'rsa2048', 'P', 'e', 'k')`,
			uid26(), hash())
		bad := func(want uint16, set string, args ...any) {
			c.bad(want, `INSERT INTO certificates (uid, tenant_id, name, source, status, primary_domain, domains_json, serial, issuer, not_before, not_after, fingerprint_sha256,
				key_algorithm, chain_pem, private_key_enc, enc_key_id, auto_renew) VALUES (?, ?, 'c', ?, ?, 'a', JSON_ARRAY(), 's', 'i', ?, ?, ?, ?, 'P', 'e', 'k', ?)`, args...)
			_ = set
		}
		bad(errCheckViolated, "source", uid26(), t1, "manual", "active", "2026-01-01", "2027-01-01", hash(), "rsa2048", 0)
		bad(errCheckViolated, "status", uid26(), t1, "upload", "zombie", "2026-01-01", "2027-01-01", hash(), "rsa2048", 0)
		bad(errCheckViolated, "algo", uid26(), t1, "upload", "active", "2026-01-01", "2027-01-01", hash(), "des", 0)
		bad(errCheckViolated, "validity", uid26(), t1, "upload", "active", "2027-01-01", "2026-01-01", hash(), "rsa2048", 0)
		bad(errCheckViolated, "auto_renew", uid26(), t1, "upload", "active", "2026-01-01", "2027-01-01", hash(), "rsa2048", 1) // 仅 ACME 可自动续期
		c.ok(`INSERT INTO certificates (uid, tenant_id, name, source, primary_domain, domains_json, serial, issuer, not_before, not_after, fingerprint_sha256,
			key_algorithm, chain_pem, private_key_enc, enc_key_id, auto_renew) VALUES (?, ?, 'c', 'acme', 'a', JSON_ARRAY(), 's', 'i', '2026-01-01', '2027-01-01', ?, 'rsa2048', 'P', 'e', 'k', 1)`, uid26(), t1, hash())
		// private_key_enc 无默认：漏写即报错（不会静默存空）。
		c.bad(errNoDefault, `INSERT INTO certificates (uid, tenant_id, name, source, primary_domain, domains_json, serial, issuer, not_before, not_after, fingerprint_sha256,
			key_algorithm, chain_pem, enc_key_id) VALUES (?, ?, 'c', 'upload', 'a', JSON_ARRAY(), 's', 'i', '2026-01-01', '2027-01-01', ?, 'rsa2048', 'P', 'k')`, uid26(), t1, hash())
		c.bad(errRowIsRef, "DELETE FROM sites WHERE id = ?", s1) // 证书引用站点：RESTRICT
	})
	run("certificate_deployments：幂等键唯一、外键 RESTRICT", func(c *cx) {
		t1 := c.tenant()
		ce := cert(c, t1, nil)
		n, cl := node(c), cluster(c)
		ins := "INSERT INTO certificate_deployments (tenant_id, certificate_id, cert_version, node_id, cluster_id, status) VALUES (?, ?, ?, ?, ?, ?)"
		c.ok(ins, t1, ce, 1, n, cl, "pending")
		c.bad(errDup, ins, t1, ce, 1, n, cl, "applied") // (certificate_id, cert_version, node_id) 唯一
		c.ok(ins, t1, ce, 2, n, cl, "pending")          // 新版本可再部署到同一节点
		c.bad(errCheckViolated, ins, t1, ce, 3, n, cl, "teleported")
		c.bad(errNoRefRow, ins, t1, 999999999, 1, n, cl, "pending")
		c.bad(errNoRefRow, ins, t1, ce, 4, n, 999999999, "pending")
		c.bad(errRowIsRef, "DELETE FROM certificates WHERE id = ?", ce)
		c.bad(errRowIsRef, "DELETE FROM nodes WHERE id = ?", n)
		c.bad(errRowIsRef, "DELETE FROM clusters WHERE id = ?", cl)
	})

	// ======== 000009 site_config ========
	run("site_domains：活动主机名全局唯一（他租户冲突）、软删除复用、格式与验证状态", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		s1, s2 := site(c, t1), site(c, t2)
		d := domain(c, t1, s1, "www.example.test")
		c.bad(errDup, "INSERT INTO site_domains (uid, tenant_id, site_id, hostname) VALUES (?, ?, ?, 'www.example.test')", uid26(), t2, s2) // 他租户同域名冲突
		c.bad(errDup, "INSERT INTO site_domains (uid, tenant_id, site_id, hostname) VALUES (?, ?, ?, 'www.example.test')", uid26(), t1, s1) // 同租户同站点亦冲突
		c.ok("UPDATE site_domains SET deleted_at = NOW(3) WHERE id = ?", d)                                                                 // 软删除释放
		d2 := domain(c, t2, s2, "www.example.test")                                                                                         // 他租户可复用
		if got := c.count("SELECT COUNT(*) FROM site_domains WHERE id = ? AND verification_status = 'pending'", d2); got != 1 {
			t.Error("verification_status 默认应为 pending（MIN-11）")
		}
		c.ok("INSERT INTO site_domains (uid, tenant_id, site_id, hostname, verification_status, verification_method) VALUES (?, ?, ?, ?, 'exempt', 'admin_exempt')", uid26(), t1, s1, host())
		c.ok("INSERT INTO site_domains (uid, tenant_id, site_id, hostname, verification_status) VALUES (?, ?, ?, ?, 'verified')", uid26(), t1, s1, host())
		c.ok("INSERT INTO site_domains (uid, tenant_id, site_id, hostname, is_wildcard) VALUES (?, ?, ?, '*.wild.example.test', 1)", uid26(), t1, s1)
		insH := "INSERT INTO site_domains (uid, tenant_id, site_id, hostname) VALUES (?, ?, ?, ?)"
		for _, h := range []string{"Upper.example.test", "localhost", "under_score.example.test", "-lead.example.test", "a..b.example.test", "x.y", "has space.example.test"} {
			c.bad(errCheckViolated, insH, uid26(), t1, s1, h)
		}
		c.bad(errCheckViolated, "INSERT INTO site_domains (uid, tenant_id, site_id, hostname, verification_status) VALUES (?, ?, ?, ?, 'maybe')", uid26(), t1, s1, host())
		c.bad(errCheckViolated, "INSERT INTO site_domains (uid, tenant_id, site_id, hostname, cname_status) VALUES (?, ?, ?, ?, 'maybe')", uid26(), t1, s1, host())
	})
	run("站点子表复合外键：子行租户与父站点不一致在库层不可能", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		s1, s2 := site(c, t1), site(c, t2)
		c.bad(errNoRefRow, "INSERT INTO site_domains (uid, tenant_id, site_id, hostname) VALUES (?, ?, ?, ?)", uid26(), t1, s2, host())
		c.bad(errNoRefRow, "INSERT INTO site_domains (uid, tenant_id, site_id, hostname) VALUES (?, ?, ?, ?)", uid26(), t2, s1, host())
		c.bad(errNoRefRow, "INSERT INTO site_settings (uid, tenant_id, site_id, proxy_json) VALUES (?, ?, ?, JSON_OBJECT())", uid26(), t1, s2)
		c.bad(errNoRefRow, "INSERT INTO origin_groups (uid, tenant_id, site_id, name) VALUES (?, ?, ?, 'g')", uid26(), t1, s2)
		c.bad(errNoRefRow, "INSERT INTO site_cluster_bindings (tenant_id, site_id, cluster_id) VALUES (?, ?, ?)", t1, s2, cluster(c))
		c.bad(errNoRefRow, "INSERT INTO site_rules (uid, tenant_id, site_id, rule_type, phase, name, doc_json, doc_sha256) VALUES (?, ?, ?, 'redirect', 'redirect', 'r', JSON_OBJECT(), ?)", uid26(), t1, s2, hash())
		c.bad(errNoRefRow, "INSERT INTO cache_policies (uid, tenant_id, site_id, name, cacheable_statuses_json) VALUES (?, ?, ?, 'p', JSON_ARRAY())", uid26(), t1, s2)
		c.bad(errNoRefRow, "INSERT INTO cache_tasks (uid, tenant_id, site_id, kind, request_id) VALUES (?, ?, ?, 'purge_url', ?)", uid26(), t1, s2, uid26())
		c.bad(errNoRefRow, "INSERT INTO node_tasks (uid, node_id, tenant_id, site_id, kind, payload_json, request_id) VALUES (?, ?, ?, ?, 'cache_purge_url', JSON_OBJECT(), ?)", uid26(), node(c), t1, s2, uid26())
		// 对照：租户一致则通过。
		domain(c, t1, s1, host())
		c.ok("INSERT INTO site_settings (uid, tenant_id, site_id, proxy_json) VALUES (?, ?, ?, JSON_OBJECT())", uid26(), t1, s1)
		c.ok("INSERT INTO cache_tasks (uid, tenant_id, site_id, kind, request_id) VALUES (?, ?, ?, 'purge_url', ?)", uid26(), t1, s1, uid26())
	})
	run("site_domains：证书引用为复合外键 RESTRICT（CF-2 前决策，见 CC-1）；site_settings 1:1 与 CHECK", func(c *cx) {
		t1 := c.tenant()
		s1 := site(c, t1)
		ce := cert(c, t1, nil)
		d := domain(c, t1, s1, host())
		c.ok("UPDATE site_domains SET certificate_id = ? WHERE id = ?", ce, d)
		c.bad(errNoRefRow, "UPDATE site_domains SET certificate_id = 999999999 WHERE id = ?", d)
		c.bad(errRowIsRef, "DELETE FROM certificates WHERE id = ?", ce) // 被域名绑定的证书不可物理删除（须先解绑；证书本身软删除）
		c.ok("UPDATE site_domains SET certificate_id = NULL WHERE id = ?", d)
		c.ok("DELETE FROM certificates WHERE id = ?", ce)
		ins := "INSERT INTO site_settings (uid, tenant_id, site_id, proxy_json) VALUES (?, ?, ?, JSON_OBJECT())"
		st := c.ok(ins, uid26(), t1, s1)
		c.bad(errDup, ins, uid26(), t1, s1)                                                                                             // 站点 1:1
		c.bad(errCheckViolated, "UPDATE site_settings SET real_ip_custom_enabled = 1 WHERE id = ?", st)                                 // 启用自定义头须给头名
		c.bad(errCheckViolated, "UPDATE site_settings SET real_ip_custom_enabled = 1, real_ip_custom_header = '1bad' WHERE id = ?", st) // 头名格式
		c.ok("UPDATE site_settings SET real_ip_custom_enabled = 1, real_ip_custom_header = 'X-Real-Client' WHERE id = ?", st)
		c.bad(errCheckViolated, "UPDATE site_settings SET tls_min_version = 'SSLv3' WHERE id = ?", st)
		c.bad(errCheckViolated, "UPDATE site_settings SET xff_policy = 'trust_all' WHERE id = ?", st)
		c.bad(errNoRefRow, "UPDATE site_settings SET default_certificate_id = 999999999 WHERE id = ?", st)
		c.bad(errNoDefault, "INSERT INTO site_settings (uid, tenant_id, site_id) VALUES (?, ?, ?)", uid26(), t1, site(c, t1)) // proxy_json 无默认
	})
	run("origin_groups / origins：默认组唯一、复合外键禁止跨站点/跨租户绑定（MIN-13）", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		s1, s1b, s2 := site(c, t1), site(c, t1), site(c, t2)
		g1 := ogroup(c, t1, s1, 1)
		c.bad(errDup, "INSERT INTO origin_groups (uid, tenant_id, site_id, name, is_default) VALUES (?, ?, ?, 'second-default', 1)", uid26(), t1, s1) // 每站点至多一个默认组
		ogroup(c, t1, s1, 0)
		ogroup(c, t1, s1, 0)         // 非默认可有多个
		g1b := ogroup(c, t1, s1b, 1) // 另一站点有自己的默认组
		g2 := ogroup(c, t2, s2, 1)
		c.ok("INSERT INTO origin_groups (uid, tenant_id, site_id, name) VALUES (?, ?, ?, 'dupname')", uid26(), t1, s1b)
		c.bad(errDup, "INSERT INTO origin_groups (uid, tenant_id, site_id, name) VALUES (?, ?, ?, 'dupname')", uid26(), t1, s1b) // 站点内源站组名唯一
		c.bad(errCheckViolated, "INSERT INTO origin_groups (uid, tenant_id, site_id, name, lb_policy) VALUES (?, ?, ?, 'lb', 'random_magic')", uid26(), t1, s1)
		ins := "INSERT INTO origins (uid, tenant_id, site_id, origin_group_id, address, port) VALUES (?, ?, ?, ?, ?, ?)"
		o := c.ok(ins, uid26(), t1, s1, g1, "203.0.113.10", 8080)
		c.bad(errNoRefRow, ins, uid26(), t1, s1, g1b, "203.0.113.11", 80) // 站点 s1 绑定了 s1b 的源站组
		c.bad(errNoRefRow, ins, uid26(), t1, s1, g2, "203.0.113.12", 80)  // 绑定了他租户的源站组
		c.bad(errNoRefRow, ins, uid26(), t2, s1, g1, "203.0.113.13", 80)  // 租户与站点不一致
		c.bad(errNoRefRow, ins, uid26(), t1, s1, 999999999, "x", 80)
		c.bad(errCheckViolated, ins, uid26(), t1, s1, g1, "203.0.113.14", 0)
		c.bad(1264, ins, uid26(), t1, s1, g1, "203.0.113.14", 65536) // SMALLINT UNSIGNED 上限
		c.ok(ins, uid26(), t1, s1, g1, "203.0.113.14", 65535)
		c.bad(errCheckViolated, "INSERT INTO origins (uid, tenant_id, site_id, origin_group_id, address, port, weight) VALUES (?, ?, ?, ?, 'a', 80, 1001)", uid26(), t1, s1, g1)
		c.bad(errCheckViolated, "INSERT INTO origins (uid, tenant_id, site_id, origin_group_id, address, port, protocol) VALUES (?, ?, ?, ?, 'a', 80, 'ftp')", uid26(), t1, s1, g1)
		c.bad(errCheckViolated, "INSERT INTO origins (uid, tenant_id, site_id, origin_group_id, address, port, role) VALUES (?, ?, ?, ?, 'a', 80, 'spare')", uid26(), t1, s1, g1)
		// origin_health：按 (源站, 节点) 唯一；随源站级联。
		n := node(c)
		oh := "INSERT INTO origin_health (tenant_id, origin_id, node_id, status) VALUES (?, ?, ?, ?)"
		c.ok(oh, t1, o, n, "up")
		c.bad(errDup, oh, t1, o, n, "down")
		c.bad(errCheckViolated, oh, t1, o, node(c), "sideways")
		c.bad(errNoRefRow, oh, t1, 999999999, n, "up")
		c.ok("DELETE FROM origin_groups WHERE id = ?", g1) // 源站随源站组级联，健康记录随源站级联
		if c.count("SELECT COUNT(*) FROM origins WHERE id = ?", o) != 0 || c.count("SELECT COUNT(*) FROM origin_health WHERE origin_id = ?", o) != 0 {
			t.Error("源站组删除应级联删除源站与其健康记录")
		}
	})
	run("site_cluster_bindings：唯一、状态值域、集群 RESTRICT", func(c *cx) {
		t1 := c.tenant()
		s1 := site(c, t1)
		cl := cluster(c)
		ins := "INSERT INTO site_cluster_bindings (tenant_id, site_id, cluster_id, status) VALUES (?, ?, ?, ?)"
		b := c.ok(ins, t1, s1, cl, "active")
		c.bad(errDup, ins, t1, s1, cl, "active")
		c.bad(errCheckViolated, ins, t1, s1, cluster(c), "paused")
		c.bad(errNoRefRow, ins, t1, s1, 999999999, "active")
		c.bad(errRowIsRef, "DELETE FROM clusters WHERE id = ?", cl)
		c.ok("DELETE FROM sites WHERE id = ?", s1) // 绑定随站点级联
		if c.count("SELECT COUNT(*) FROM site_cluster_bindings WHERE id = ?", b) != 0 {
			t.Error("站点删除应级联删除绑定")
		}
	})
	run("node_tasks：kind/status 值域与 OpenAPI TaskType 无冲突、租约与 attempt、归属", func(c *cx) {
		n := node(c)
		t1 := c.tenant()
		s1 := site(c, t1)
		ins := "INSERT INTO node_tasks (uid, node_id, tenant_id, site_id, kind, status, attempt, max_attempts, payload_json, request_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, JSON_OBJECT(), ?)"
		for _, k := range []string{"cache_purge_url", "cache_purge_site", "cache_purge_prefix", "cache_prefetch", "node_upgrade", "node_rollback", "cert_reload", "debug_request"} {
			c.ok(ins, uid26(), n, t1, s1, k, "pending", 0, 5, uid26())
		}
		for _, k := range []string{"exec", "shell", "run_command", "node_exec", ""} {
			c.bad(errCheckViolated, ins, uid26(), n, t1, s1, k, "pending", 0, 5, uid26()) // 封闭枚举，无 exec 类
		}
		for _, s := range []string{"pending", "leased", "running", "success", "failed", "skipped", "expired", "canceled"} {
			c.ok(ins, uid26(), n, nil, nil, "cert_reload", s, 0, 5, uid26())
		}
		c.bad(errCheckViolated, ins, uid26(), n, nil, nil, "cert_reload", "lost", 0, 5, uid26())
		c.bad(errCheckViolated, ins, uid26(), n, nil, nil, "cert_reload", "pending", 6, 5, uid26()) // attempt <= max_attempts
		c.bad(errCheckViolated, ins, uid26(), n, nil, s1, "cert_reload", "pending", 0, 5, uid26())  // 有站点必有租户（MATCH SIMPLE 绕过防护）
		c.bad(errNoRefRow, ins, uid26(), n, t1, 999999999, "cert_reload", "pending", 0, 5, uid26())
		c.bad(errNoRefRow, ins, uid26(), 999999999, nil, nil, "cert_reload", "pending", 0, 5, uid26())
		c.bad(errNoDefault, "INSERT INTO node_tasks (uid, node_id, kind, payload_json) VALUES (?, ?, 'cert_reload', JSON_OBJECT())", uid26(), n) // request_id 必填
		u := uid26()
		c.ok("INSERT INTO node_tasks (uid, node_id, kind, payload_json, request_id) VALUES (?, ?, 'node_upgrade', JSON_OBJECT(), ?)", u, n, uid26())
		c.bad(errDup, "INSERT INTO node_tasks (uid, node_id, kind, payload_json, request_id) VALUES (?, ?, 'node_upgrade', JSON_OBJECT(), ?)", u, n, uid26())
		parent := c.task("cache.purge_url")
		nt := c.ok("INSERT INTO node_tasks (uid, node_id, task_id, kind, payload_json, request_id) VALUES (?, ?, ?, 'cache_purge_url', JSON_OBJECT(), ?)", uid26(), n, parent, uid26())
		c.ok("DELETE FROM tasks WHERE id = ?", parent) // 父任务删除：SET NULL
		if !c.isNull("SELECT task_id FROM node_tasks WHERE id = ?", nt) {
			t.Error("父任务删除后 node_tasks.task_id 应置 NULL")
		}
		c.bad(errRowIsRef, "DELETE FROM nodes WHERE id = ?", n) // 节点有任务：RESTRICT
		// kind 值域与 OpenAPI TaskType 对齐：缓存/升级/回滚类 kind 的点号形式都是 TaskType 的成员（node-deploy §4.4）。
		have := map[string]bool{}
		for _, tt := range oa.taskType {
			have[tt] = true
		}
		for _, k := range []string{"cache_purge_url", "cache_purge_site", "cache_purge_prefix", "cache_prefetch", "node_upgrade", "node_rollback"} {
			if dotted := strings.Replace(k, "_", ".", 1); !have[dotted] {
				t.Errorf("OpenAPI TaskType 缺少 %s（node_tasks.kind=%s）；TaskType=%v", dotted, k, oa.taskType)
			}
		}
	})

	// ======== 000010 rules ========
	run("rule_templates：T? 唯一（平台与租户同名互不冲突）、类型值域", func(c *cx) {
		t1 := c.tenant()
		ins := "INSERT INTO rule_templates (uid, tenant_id, name, rule_type, phase, doc_json) VALUES (?, ?, ?, ?, 'redirect', JSON_OBJECT())"
		c.ok(ins, uid26(), nil, "tpl", "redirect")
		c.bad(errDup, ins, uid26(), nil, "tpl", "redirect") // 平台级同名（tenant_key=0）唯一
		c.ok(ins, uid26(), t1, "tpl", "redirect")           // 租户级同名可
		c.bad(errDup, ins, uid26(), t1, "tpl", "redirect")
		c.bad(errCheckViolated, ins, uid26(), nil, "x", "exec_shell")
		c.bad(errNoRefRow, ins, uid26(), 999999999, "x", "redirect")
		c.bad(errRowIsRef, "DELETE FROM tenants WHERE id = ?", t1)
	})
	run("site_rules：单表承载全部规则类型、type→phase 映射、活动名唯一、模板 SET NULL", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		s1, s2 := site(c, t1), site(c, t2)
		ins := "INSERT INTO site_rules (uid, tenant_id, site_id, rule_type, phase, name, doc_json, doc_sha256) VALUES (?, ?, ?, ?, ?, ?, JSON_OBJECT(), ?)"
		pairs := [][2]string{{"redirect", "redirect"}, {"url_rewrite", "rewrite"}, {"request_header", "rewrite"}, {"cache_rule", "cache_rule"}, {"origin_rule", "origin_rule"},
			{"origin_request_header", "origin_headers"}, {"error_page", "upstream_response"}, {"response_header", "response_headers"}, {"access_control", "access_control"},
			{"hotlink", "access_control"}, {"rate_limit", "waf_cc"}, {"page_rule", "respond"}}
		for _, p := range pairs {
			c.ok(ins, uid26(), t1, s1, p[0], p[1], "r-"+p[0], hash())
		}
		c.bad(errCheckViolated, ins, uid26(), t1, s1, "redirect", "rewrite", "mismatch", hash()) // 类型与阶段不匹配
		c.bad(errCheckViolated, ins, uid26(), t1, s1, "cache_rule", "redirect", "mismatch2", hash())
		c.bad(errCheckViolated, ins, uid26(), t1, s1, "rate_limit", "access_control", "mismatch3", hash())
		c.bad(errCheckViolated, ins, uid26(), t1, s1, "mystery", "redirect", "badtype", hash())
		c.bad(errCheckViolated, ins, uid26(), t1, s1, "redirect", "nowhere", "badphase", hash())
		c.bad(errDup, ins, uid26(), t1, s1, "redirect", "redirect", "r-redirect", hash()) // 站点内活动名唯一
		c.ok(ins, uid26(), t1, s1, "redirect", "redirect", "r-redirect-2", hash())
		c.ok(ins, uid26(), t2, s2, "redirect", "redirect", "r-redirect", hash()) // 他站点同名可
		id := c.ok(ins, uid26(), t1, s1, "redirect", "redirect", "reuse", hash())
		c.ok("UPDATE site_rules SET deleted_at = NOW(3) WHERE id = ?", id)
		c.ok(ins, uid26(), t1, s1, "redirect", "redirect", "reuse", hash()) // 软删除后可复用
		c.bad(errCheckViolated, "UPDATE site_rules SET priority = 100001 WHERE id = ?", id)
		tp := c.ok("INSERT INTO rule_templates (uid, name, rule_type, phase, doc_json) VALUES (?, 'tp', 'redirect', 'redirect', JSON_OBJECT())", uid26())
		r := c.ok("INSERT INTO site_rules (uid, tenant_id, site_id, rule_type, phase, name, doc_json, doc_sha256, template_id) VALUES (?, ?, ?, 'redirect', 'redirect', 'withtpl', JSON_OBJECT(), ?, ?)", uid26(), t1, s1, hash(), tp)
		c.ok("DELETE FROM rule_templates WHERE id = ?", tp)
		if !c.isNull("SELECT template_id FROM site_rules WHERE id = ?", r) {
			t.Error("模板删除后 site_rules.template_id 应置 NULL")
		}
	})
	run("rule_versions：不可变触发器、版本号唯一、循环外键 SET NULL", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		s1, s2 := site(c, t1), site(c, t2)
		r := rule(c, t1, s1, "r1")
		v1 := ver(c, t1, s1, r, 1)
		c.bad(errDup, `INSERT INTO rule_versions (uid, tenant_id, site_id, rule_id, version_no, rule_type, phase, priority, enabled, doc_json, doc_sha256)
			VALUES (?, ?, ?, ?, 1, 'redirect', 'redirect', 1, 1, JSON_OBJECT(), ?)`, uid26(), t1, s1, r, hash()) // (rule_id, version_no) 唯一
		v2 := ver(c, t1, s1, r, 2)
		// 租户隔离：版本的租户/站点必须与站点一致。
		c.bad(errNoRefRow, `INSERT INTO rule_versions (uid, tenant_id, site_id, rule_id, version_no, rule_type, phase, priority, enabled, doc_json, doc_sha256)
			VALUES (?, ?, ?, ?, 3, 'redirect', 'redirect', 1, 1, JSON_OBJECT(), ?)`, uid26(), t1, s2, r, hash())
		c.bad(errNoRefRow, `INSERT INTO rule_versions (uid, tenant_id, site_id, rule_id, version_no, rule_type, phase, priority, enabled, doc_json, doc_sha256)
			VALUES (?, ?, ?, 999999999, 1, 'redirect', 'redirect', 1, 1, JSON_OBJECT(), ?)`, uid26(), t1, s1, hash())
		// 不可变：任何 UPDATE 被拒；DELETE 仅 @lingedge_purge=1。
		c.bad(errSignal, "UPDATE rule_versions SET priority = 1 WHERE id = ?", v1)
		c.bad(errSignal, "UPDATE rule_versions SET change_note = 'x' WHERE id = ?", v1)
		c.bad(errSignal, "UPDATE rule_versions SET doc_sha256 = ? WHERE id = ?", hash(), v1)
		c.bad(errSignal, "DELETE FROM rule_versions WHERE id = ?", v1)
		c.ok("SET @lingedge_purge = 0")
		c.bad(errSignal, "DELETE FROM rule_versions WHERE id = ?", v1)
		c.ok("SET @lingedge_purge = 1")
		// 循环外键：site_rules.current/published_version_id → rule_versions（ALTER 追加）。
		c.bad(errNoRefRow, "UPDATE site_rules SET current_version_id = 999999999 WHERE id = ?", r)
		c.ok("UPDATE site_rules SET current_version_id = ?, published_version_id = ? WHERE id = ?", v2, v1, r)
		c.bad(errRowIsRef, "DELETE FROM site_rules WHERE id = ?", r) // 有版本引用：RESTRICT
		c.ok("DELETE FROM rule_versions WHERE id = ?", v2)           // purge 下可删；被引用的指针 SET NULL
		if !c.isNull("SELECT current_version_id FROM site_rules WHERE id = ?", r) {
			t.Error("版本删除后 site_rules.current_version_id 应置 NULL")
		}
		c.ok("SET @lingedge_purge = NULL")
		// 回滚链：rollback_of_version_id 自引用 SET NULL。
		v3 := c.ok(`INSERT INTO rule_versions (uid, tenant_id, site_id, rule_id, version_no, rule_type, phase, priority, enabled, doc_json, doc_sha256, rollback_of_version_id)
			VALUES (?, ?, ?, ?, 9, 'redirect', 'redirect', 1, 1, JSON_OBJECT(), ?, ?)`, uid26(), t1, s1, r, hash(), v1)
		c.ok("SET @lingedge_purge = 1")
		c.ok("DELETE FROM rule_versions WHERE id = ?", v1)
		if !c.isNull("SELECT rollback_of_version_id FROM rule_versions WHERE id = ?", v3) {
			t.Error("被回滚版本删除后 rollback_of_version_id 应置 NULL")
		}
		c.ok("SET @lingedge_purge = NULL")
	})

	// ======== 000011 cache ========
	run("cache_policies：默认策略唯一、epoch 仅默认策略、危险开关须确认、ack 外键 RESTRICT", func(c *cx) {
		t1, t2 := c.tenant(), c.tenant()
		s1, s2 := site(c, t1), site(c, t2)
		p := policy(c, t1, s1, 1)
		policy(c, t1, s1, 0)
		policy(c, t1, s1, 0)
		c.bad(errDup, "INSERT INTO cache_policies (uid, tenant_id, site_id, name, is_default, cacheable_statuses_json) VALUES (?, ?, ?, 'second-default', 1, JSON_ARRAY())", uid26(), t1, s1)
		policy(c, t2, s2, 1) // 他站点各有默认
		c.ok("UPDATE cache_policies SET cache_epoch = 3, epoch_bumped_at = NOW(3) WHERE id = ?", p)
		np := policy(c, t1, s1, 0)
		c.bad(errCheckViolated, "UPDATE cache_policies SET cache_epoch = 1 WHERE id = ?", np) // 命名策略不承载 epoch
		c.bad(errCheckViolated, "UPDATE cache_policies SET default_ttl_s = 100, max_ttl_s = 10 WHERE id = ?", p)
		c.bad(errCheckViolated, "UPDATE cache_policies SET mode = 'yolo' WHERE id = ?", p)
		c.bad(errCheckViolated, "UPDATE cache_policies SET query_policy = 'yolo' WHERE id = ?", p)
		c.bad(errCheckViolated, "UPDATE cache_policies SET cookie_policy = 'yolo' WHERE id = ?", p)
		c.bad(errCheckViolated, "UPDATE cache_policies SET range_mode = 'yolo' WHERE id = ?", p)
		// 危险开关（缓存 Set-Cookie / Authorization）须带风险确认人与时间。
		c.bad(errCheckViolated, "UPDATE cache_policies SET allow_cache_set_cookie = 1 WHERE id = ?", p)
		c.bad(errCheckViolated, "UPDATE cache_policies SET allow_cache_authorization = 1, risk_ack_at = NOW(3) WHERE id = ?", p)
		u := c.user("acker@example.test")
		c.bad(errNoRefRow, "UPDATE cache_policies SET allow_cache_set_cookie = 1, risk_ack_by = 999999999, risk_ack_at = NOW(3) WHERE id = ?", p)
		c.ok("UPDATE cache_policies SET allow_cache_set_cookie = 1, risk_ack_by = ?, risk_ack_at = NOW(3) WHERE id = ?", u, p)
		c.bad(errRowIsRef, "DELETE FROM users WHERE id = ?", u) // RESTRICT（SET NULL 与 CHECK 冲突，ERROR 3823）
		if c.count("SELECT COUNT(*) FROM cache_policies WHERE id = ? AND risk_ack_by = ?", p, u) != 1 {
			t.Error("被 RESTRICT 拒绝删除后确认人应保留")
		}
	})
	run("cache_tasks / cache_task_nodes：值域、逐节点回执唯一、级联与 SET NULL", func(c *cx) {
		t1 := c.tenant()
		s1 := site(c, t1)
		ins := "INSERT INTO cache_tasks (uid, tenant_id, site_id, kind, status, request_id) VALUES (?, ?, ?, ?, ?, ?)"
		for _, k := range []string{"purge_url", "purge_site", "prefetch", "purge_prefix", "purge_wildcard"} {
			c.ok(ins, uid26(), t1, s1, k, "pending", uid26())
		}
		c.bad(errCheckViolated, ins, uid26(), t1, s1, "purge_everything", "pending", uid26())
		c.bad(errCheckViolated, ins, uid26(), t1, s1, "purge_url", "exploded", uid26())
		c.bad(errNoDefault, "INSERT INTO cache_tasks (uid, tenant_id, site_id, kind) VALUES (?, ?, ?, 'purge_url')", uid26(), t1, s1) // request_id 必填
		ct := c.ok(ins, uid26(), t1, s1, "purge_url", "pending", uid26())
		n := node(c)
		nt := nodeTask(c, n)
		cn := "INSERT INTO cache_task_nodes (tenant_id, cache_task_id, node_id, node_task_id, status) VALUES (?, ?, ?, ?, ?)"
		row := c.ok(cn, t1, ct, n, nt, "pending")
		c.bad(errDup, cn, t1, ct, n, nil, "pending") // 每任务每节点一条回执
		for _, s := range []string{"pending", "sent", "success", "failed", "skipped_offline", "expired"} {
			c.ok(cn, t1, ct, node(c), nil, s)
		}
		c.bad(errCheckViolated, cn, t1, ct, node(c), nil, "running")
		c.bad(errNoRefRow, cn, t1, 999999999, node(c), nil, "pending")
		c.bad(errNoRefRow, cn, t1, ct, 999999999, nil, "pending")
		c.bad(errNoRefRow, cn, t1, ct, node(c), 999999999, "pending")
		c.bad(errNoRefRow, cn, 999999999, ct, node(c), nil, "pending")
		c.ok("DELETE FROM node_tasks WHERE id = ?", nt) // 投递记录清理：回执保留，node_task_id 置 NULL
		if !c.isNull("SELECT node_task_id FROM cache_task_nodes WHERE id = ?", row) {
			t.Error("node_tasks 删除后 cache_task_nodes.node_task_id 应置 NULL")
		}
		c.bad(errRowIsRef, "DELETE FROM nodes WHERE id = ?", n)
		c.ok("DELETE FROM cache_tasks WHERE id = ?", ct) // 回执随任务头级联
		if c.count("SELECT COUNT(*) FROM cache_task_nodes WHERE cache_task_id = ?", ct) != 0 {
			t.Error("缓存任务删除应级联删除回执")
		}
		parent := c.task("cache.purge_url")
		ct2 := c.ok("INSERT INTO cache_tasks (uid, tenant_id, site_id, task_id, kind, request_id) VALUES (?, ?, ?, ?, 'purge_site', ?)", uid26(), t1, s1, parent, uid26())
		c.ok("DELETE FROM tasks WHERE id = ?", parent)
		if !c.isNull("SELECT task_id FROM cache_tasks WHERE id = ?", ct2) {
			t.Error("父任务删除后 cache_tasks.task_id 应置 NULL")
		}
	})
}

// ---- 租户索引实测（EXPLAIN）：带 tenant_id 的列表查询走 tenant_id 打头索引 ----

func TestM1BTenantListQueriesUseTenantLeadingIndexes(t *testing.T) {
	e, _ := m1bUp(t)
	c := e.conn(t)
	ctx := ctxT(t)
	hash := func() string { return fmt.Sprintf("%064d", next()) }
	var firstTenant int64
	for i := 0; i < 80; i++ {
		tx, err := c.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		cc := &cx{t: t, x: tx, pw: "x"}
		tn := cc.tenant()
		if firstTenant == 0 {
			firstTenant = tn
		}
		for j := 0; j < 2; j++ {
			s := cc.ok("INSERT INTO sites (uid, tenant_id, name) VALUES (?, ?, ?)", uid26(), tn, fmt.Sprintf("s%d", next()))
			cc.ok("INSERT INTO site_domains (uid, tenant_id, site_id, hostname) VALUES (?, ?, ?, ?)", uid26(), tn, s, fmt.Sprintf("h%d.example.test", next()))
			g := cc.ok("INSERT INTO origin_groups (uid, tenant_id, site_id, name) VALUES (?, ?, ?, 'g')", uid26(), tn, s)
			cc.ok("INSERT INTO origins (uid, tenant_id, site_id, origin_group_id, address, port) VALUES (?, ?, ?, ?, 'o.example.test', 80)", uid26(), tn, s, g)
			cc.ok("INSERT INTO site_rules (uid, tenant_id, site_id, rule_type, phase, name, doc_json, doc_sha256) VALUES (?, ?, ?, 'redirect', 'redirect', 'r', JSON_OBJECT(), ?)", uid26(), tn, s, hash())
			cc.ok("INSERT INTO cache_policies (uid, tenant_id, site_id, name, cacheable_statuses_json) VALUES (?, ?, ?, 'p', JSON_ARRAY(200))", uid26(), tn, s)
			cc.ok("INSERT INTO cache_tasks (uid, tenant_id, site_id, kind, request_id) VALUES (?, ?, ?, 'purge_url', ?)", uid26(), tn, s, uid26())
			cc.ok(`INSERT INTO certificates (uid, tenant_id, site_id, name, source, primary_domain, domains_json, serial, issuer, not_before, not_after, fingerprint_sha256,
				key_algorithm, chain_pem, private_key_enc, enc_key_id) VALUES (?, ?, ?, 'c', 'upload', 'a', JSON_ARRAY(), 's', 'i', '2026-01-01', '2027-01-01', ?, 'rsa2048', 'P', 'e', 'k')`, uid26(), tn, s, hash())
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	leading := func(tbl, idx string) string {
		return e.scalar(t, "SELECT COLUMN_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ? AND SEQ_IN_INDEX = 1", e.dbName, tbl, idx)
	}
	usingRe := regexp.MustCompile(`using (\w+)`)
	for _, tbl := range []string{"sites", "site_domains", "origin_groups", "origins", "site_rules", "cache_policies", "cache_tasks", "certificates"} {
		if _, err := c.ExecContext(ctx, "ANALYZE TABLE `"+tbl+"`"); err != nil {
			t.Fatal(err)
		}
		var p string
		if err := c.QueryRowContext(ctx, "EXPLAIN FORMAT=TREE SELECT id FROM `"+tbl+"` WHERE tenant_id = ?", firstTenant).Scan(&p); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		m := usingRe.FindStringSubmatch(p)
		if strings.Contains(p, "Table scan") || m == nil || leading(tbl, m[1]) != "tenant_id" {
			t.Errorf("%s 的租户条件查询应走 tenant_id 打头索引: %s", tbl, p)
		}
	}
	// 漏写 tenant_id 条件（按无索引列过滤）则退化为全表扫描——正是该约定要让测试发现的现象。
	var q string
	if err := c.QueryRowContext(ctx, "EXPLAIN FORMAT=TREE SELECT id FROM sites WHERE description = 'x'").Scan(&q); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, "Table scan") {
		t.Errorf("漏写租户条件应退化为全表扫描（EXPLAIN 可发现）: %s", q)
	}
}

// ---- DDL 陷阱预检覆盖（AC6，B 批）：对真实迁移做变异，静态预检必须报出，且真实 MySQL 以文档记录的错误号拒绝 ----

func TestM1BDDLTrapsAreCoveredByStaticPrecheckAndRealMySQL(t *testing.T) {
	type mutation struct {
		name     string
		mutate   func(files map[string]*mapFile)
		wantRule string
		wantErr  uint16
		probe    string
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
			name: "CHECK + FK 动作：cache_policies.risk_ack_by 改 SET NULL（被 ck_cache_policies_dangerous 引用）",
			mutate: replace("000011_cache.sql", "FOREIGN KEY (risk_ack_by) REFERENCES users (id) ON DELETE RESTRICT",
				"FOREIGN KEY (risk_ack_by) REFERENCES users (id) ON DELETE SET NULL"),
			wantRule: migrate.RuleCheckFK, wantErr: 3823,
		},
		{
			name: "CHECK + FK 动作：node_certificates.node_id 改 SET NULL（被 ck_node_certificates_subject 引用）",
			mutate: replace("000006_nodes_clusters.sql", "FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE CASCADE\n) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON node_certificates",
				"FOREIGN KEY (node_id) REFERENCES nodes (id) ON DELETE SET NULL\n) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON node_certificates"),
			wantRule: migrate.RuleCheckFK, wantErr: 3823,
		},
		{
			name: "CHECK + FK 动作：node_tasks.tenant_id 改 SET NULL（被 ck_node_tasks_site_tenant 引用）",
			mutate: replace("000009_site_config.sql", "CONSTRAINT fk_node_tasks_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT",
				"CONSTRAINT fk_node_tasks_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE SET NULL"),
			wantRule: migrate.RuleCheckFK, wantErr: 3823,
		},
		{
			name: "CHECK + FK 动作：node_tasks 复合外键 (tenant_id, site_id) 加 ON UPDATE CASCADE（被 ck_node_tasks_site_tenant 引用）",
			mutate: replace("000009_site_config.sql", "CONSTRAINT fk_node_tasks_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT",
				"CONSTRAINT fk_node_tasks_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT ON UPDATE CASCADE"),
			wantRule: migrate.RuleCheckFK, wantErr: 3823,
		},
		{
			name: "STORED 生成列的基列外键 CASCADE：origin_groups.default_flag（基列 site_id）",
			mutate: replace("000009_site_config.sql", "CONSTRAINT fk_origin_groups_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT",
				"CONSTRAINT fk_origin_groups_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE CASCADE"),
			wantRule: migrate.RuleGeneratedFK, wantErr: 1215,
		},
		{
			name: "STORED 生成列的基列外键 CASCADE：cache_policies.default_flag（基列 site_id）",
			mutate: replace("000011_cache.sql", "CONSTRAINT fk_cache_policies_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT",
				"CONSTRAINT fk_cache_policies_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE CASCADE"),
			wantRule: migrate.RuleGeneratedFK, wantErr: 1215,
		},
		{
			name: "前向外键：certificates（000008）晚于 site_domains（000009）创建",
			mutate: func(files map[string]*mapFile) {
				a, b := files["000008_certificates.sql"], files["000009_site_config.sql"]
				a.Data, b.Data = b.Data, a.Data
			},
			wantRule: migrate.RuleForwardFK, wantErr: 1824,
		},
		{
			name: "前向外键：循环外键直接内联（site_rules 内联引用尚未创建的 rule_versions）",
			mutate: replace("000010_rules.sql", "CONSTRAINT fk_site_rules_rule_templates FOREIGN KEY (template_id) REFERENCES rule_templates (id) ON DELETE SET NULL\n",
				"CONSTRAINT fk_site_rules_rule_templates FOREIGN KEY (template_id) REFERENCES rule_templates (id) ON DELETE SET NULL,\n  CONSTRAINT fk_site_rules_inline FOREIGN KEY (current_version_id) REFERENCES rule_versions (id) ON DELETE SET NULL\n"),
			wantRule: migrate.RuleForwardFK, wantErr: 1824,
		},
		{
			name: "前向外键：sites（000007）先于 site_groups 创建",
			mutate: func(files map[string]*mapFile) {
				s := string(files["000007_sites_core.sql"].Data)
				ig := strings.Index(s, "-- ---- site_groups：")
				is := strings.Index(s, "-- ---- sites：")
				files["000007_sites_core.sql"].Data = []byte(s[:ig] + s[is:] + "\n" + s[ig:is])
			},
			wantRule: migrate.RuleForwardFK, wantErr: 1824,
		},
		{
			name: "分区表出现外键（node_heartbeats.node_id → nodes）",
			mutate: replace("000006_nodes_clusters.sql", "  CONSTRAINT ck_node_heartbeats_cpu CHECK (cpu_permille IS NULL OR cpu_permille <= 1000)\n",
				"  CONSTRAINT ck_node_heartbeats_cpu CHECK (cpu_permille IS NULL OR cpu_permille <= 1000),\n  CONSTRAINT fk_node_heartbeats_nodes FOREIGN KEY (node_id) REFERENCES nodes (id)\n"),
			wantRule: migrate.RulePartitionFK, wantErr: 1506,
		},
		{
			name:     "分区表缺 pmax 兜底分区（node_heartbeats）",
			mutate:   replace("000006_nodes_clusters.sql", "  PARTITION p20261127 VALUES LESS THAN ('2026-11-28 00:00:00'),\n  PARTITION pmax VALUES LESS THAN (MAXVALUE)\n", "  PARTITION p20261127 VALUES LESS THAN ('2026-11-28 00:00:00')\n"),
			wantRule: migrate.RulePartitionPmax,
			probe:    "INSERT INTO node_heartbeats (node_id, received_at, agent_version, openresty_state) VALUES (1, '2099-01-01', 'x', 'running')",
			probeErr: errOutOfRange,
		},
	}
	for _, mu := range muts {
		t.Run(mu.name, func(t *testing.T) {
			files := m1abFixture(t)
			mu.mutate(files)
			set, findings, err := migrate.Load(toMapFS(files))
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
	t.Run("未变异的 A+B 真实迁移逐条执行 0 错误", func(t *testing.T) {
		set, findings, err := migrate.Load(toMapFS(m1abFixture(t)))
		if err != nil || len(findings) != 0 {
			t.Fatalf("%v %v", err, findings)
		}
		if n, where := firstFailure(t, newEnv(t), set); n != 0 {
			t.Fatalf("真实迁移逐条执行应 0 错误: %d %s", n, where)
		}
	})
}

// ---- schema lint 在 B 批真实迁移上的变异覆盖 ----

func TestM1BSchemaLintCatchesViolationsInRealMigrations(t *testing.T) {
	cases := []struct {
		name, file, old, new, rule string
	}{
		{"ENUM", "000007_sites_core.sql", "status VARCHAR(16) NOT NULL DEFAULT 'active',\n  description VARCHAR(512) NULL,\n  config_version", "status ENUM('active','disabled') NOT NULL DEFAULT 'active',\n  description VARCHAR(512) NULL,\n  config_version", migrate.RuleSchemaEnum},
		{"TEXT DEFAULT", "000008_certificates.sql", "private_key_enc MEDIUMTEXT NOT NULL,", "private_key_enc MEDIUMTEXT NULL DEFAULT 'x',", migrate.RuleSchemaLOBDefault},
		{"JSON DEFAULT", "000009_site_config.sql", "proxy_json JSON NOT NULL,", "proxy_json JSON NOT NULL DEFAULT (JSON_OBJECT()),", migrate.RuleSchemaLOBDefault},
		{"TIMESTAMP", "000007_sites_core.sql", "published_at DATETIME(3) NULL,", "published_at TIMESTAMP NULL,", migrate.RuleSchemaTime},
		{"DATETIME 无精度", "000007_sites_core.sql", "purge_requested_at DATETIME(3) NULL,", "purge_requested_at DATETIME NULL,", migrate.RuleSchemaTime},
		{"浮点", "000009_site_config.sql", "port SMALLINT UNSIGNED NOT NULL,\n  protocol", "port DOUBLE NOT NULL,\n  protocol", migrate.RuleSchemaFloat},
		{"租户表索引未以 tenant_id 打头", "000007_sites_core.sql", "  KEY idx_sites_group (tenant_id, group_id),\n", "  KEY idx_sites_group (tenant_id, group_id),\n  KEY idx_sites_status_only (status, tenant_id),\n", migrate.RuleSchemaTenantIndex},
		{"租户表缺 tenant_id（登记表）", "000008_certificates.sql", "  tenant_id BIGINT UNSIGNED NOT NULL,\n  certificate_id", "  tenant_id BIGINT UNSIGNED NULL,\n  certificate_id", migrate.RuleSchemaTenantColumn},
		{"跨租户索引豁免被删除", "000008_certificates.sql", "-- lingedge:cross-tenant-index=idx_certificates_expiry 平台侧到期扫描与续期告警按 (status, not_after) 跨租户查询\n", "", migrate.RuleSchemaTenantIndex},
		{"活动域名唯一索引豁免被删除", "000009_site_config.sql", "-- lingedge:cross-tenant-index=uk_site_domains_active_host 活动主机名全局唯一、跨租户冲突检测（他租户占用即冲突，FR-48）；软删除后释放\n", "", migrate.RuleSchemaTenantIndex},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := m1abFixture(t)
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
