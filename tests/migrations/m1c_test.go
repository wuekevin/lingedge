package migrationstest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Story 1.9：M1 迁移 C（000012–000013，真实迁移，来自 migrations.FS）与 CC-1（跨租户外键库层强制）的测试。
// 本文件基于 000001–000013 全量（M1 冻结范围：13 个文件、61 张表）。

var m1cTables = []string{
	// 000012 config_revisions（9）
	"config_blocks", "config_revisions", "config_revision_blocks", "site_versions", "publish_queue", "deployments", "deployment_targets", "deployment_target_steps", "node_cluster_state",
	// 000013 observability（3）
	"metric_rollups", "rollup_windows", "access_log_chunks",
}

var m1cFiles = []string{"000012_config_revisions.sql", "000013_observability.sql"}

func m1AllFiles() []string { return append(m1abFiles(), m1cFiles...) }

func m1AllFixture(t testing.TB) map[string]*mapFile { return fixtureOf(t, m1AllFiles()) }

func m1AllTables() []string {
	all := append(m1abTables(), m1cTables...)
	sort.Strings(all)
	return all
}

// m1cUp 在新建空库上经框架执行 000001–000013（必须 0 错误）。
func m1cUp(t testing.TB) (*env, *migrate.Runner) {
	t.Helper()
	e := newEnv(t)
	r := e.runner(t, toMapFS(m1AllFixture(t)))
	if f := r.Findings(); len(f) != 0 {
		t.Fatalf("真实迁移必须通过 lint: %v", f)
	}
	res, err := r.Up(ctxT(t), migrate.UpOptions{})
	if err != nil {
		t.Fatalf("空库全量 up 必须 0 错误: %v", err)
	}
	if len(res.Applied) != len(m1AllFiles()) || res.ToVersion != 13 {
		t.Fatalf("up 结果: %+v", res)
	}
	return e, r
}

// ---- 静态：嵌入的真实迁移 ----

func TestM1CEmbeddedMigrationsAreLintCleanAndWellFormed(t *testing.T) {
	set, findings, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("lint: %s", f)
	}
	if len(set.Migrations) != 13 {
		t.Fatalf("M1 冻结范围应恰为 13 个迁移文件，实际 %d", len(set.Migrations))
	}
	createRe := regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+IF\s+NOT\s+EXISTS\s+(\w+)`)
	var createdC, createdAll []string
	for i, m := range set.Migrations {
		if m.Class != migrate.ClassExpand {
			t.Errorf("%s 必须标 expand（AC2）: %s", m.File, m.Class)
		}
		if len(m.Down) != 0 {
			t.Errorf("%s 为 expand，不得提供 Down", m.File)
		}
		for _, st := range m.Up {
			if mm := createRe.FindStringSubmatch(st.Text); mm != nil {
				createdAll = append(createdAll, mm[1])
				if i >= len(m1abFiles()) {
					createdC = append(createdC, mm[1])
				}
			}
			if i >= len(m1abFiles()) {
				if f := strings.Fields(st.Text); len(f) > 0 {
					switch strings.ToUpper(f[0]) {
					case "INSERT", "UPDATE", "DELETE", "REPLACE":
						t.Errorf("%s 不得含 DML: 第 %d 行", m.File, st.Line)
					}
				}
			}
		}
	}
	for i, f := range m1cFiles {
		m := set.Migrations[len(m1abFiles())+i]
		if m.File != f || m.Version != int64(len(m1abFiles())+i+1) {
			t.Errorf("迁移 = %s（版本 %d），期望 %s", m.File, m.Version, f)
		}
		if m.Summary == "" {
			t.Errorf("%s 缺少 lingedge:summary", m.File)
		}
	}
	want := append([]string(nil), m1cTables...)
	sort.Strings(want)
	sort.Strings(createdC)
	if len(createdC) != 12 || join(createdC) != join(want) {
		t.Errorf("C 批应恰好 CREATE 12 张表\n实际 %v\n期望 %v", createdC, want)
	}
	sort.Strings(createdAll)
	if len(createdAll) != 61 || join(createdAll) != join(m1AllTables()) {
		t.Errorf("M1 应恰好 61 张表\n实际 %v\n期望 %v", createdAll, m1AllTables())
	}
	plan := migrate.BuildPlan(set, nil)
	if plan.Count != 13 || plan.HasIrreversible || plan.MaxClass != migrate.ClassExpand {
		t.Errorf("plan: %+v（应 13 个、全部 expand、irreversible=0）", plan)
	}
	// 000012 内追加的三条循环外键各带守卫。
	var s12 strings.Builder
	for _, st := range set.Migrations[11].Up {
		s12.WriteString(st.Text + "\n")
	}
	for _, fk := range []string{"fk_publish_queue_deployments", "fk_clusters_confirmed_revision", "fk_sites_last_deployment"} {
		if !strings.Contains(s12.String(), "CONSTRAINT_NAME = '"+fk+"'") {
			t.Errorf("000012 缺少循环外键 %s 的守卫", fk)
		}
	}
}

// AC6：全表核对——对照 docs/database 逐表勾选（表节标记 M1、README §4 总索引与 §4.1 补充清单），迁移创建的 61 张表与文档一致。
func TestM1CAllTablesMatchDatabaseDocsChecklist(t *testing.T) {
	files, err := filepath.Glob("../../docs/database/*.md")
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到 docs/database: %v", err)
	}
	headerRe := regexp.MustCompile("^### [0-9.]+ `([a-z_]+)`.*\\*\\*(补充 · )?M1")
	docM1 := map[string]string{}
	var readme string
	var cms strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(f) == "README.md" {
			readme = string(b)
		}
		if filepath.Base(f) == "cms.md" {
			cms.Write(b)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if m := headerRe.FindStringSubmatch(line); m != nil {
				docM1[m[1]] = filepath.Base(f)
			}
		}
	}
	mig := map[string]bool{}
	for _, tbl := range m1AllTables() {
		mig[tbl] = true
	}
	for tbl := range docM1 {
		if !mig[tbl] {
			t.Errorf("文档表节标记 M1 的表 %s 未被 M1 迁移创建", tbl)
		}
	}
	for tbl := range mig {
		if _, ok := docM1[tbl]; !ok {
			t.Errorf("迁移创建的表 %s 在 docs/database 没有标记 M1 的表节", tbl)
		}
	}
	if len(docM1) != 61 {
		t.Errorf("文档标记 M1 的表节应为 61，实际 %d", len(docM1))
	}
	// README §4 总索引的 M1 行与 §4.1 补充表清单：并集覆盖全部 61 张迁移表，且索引 M1 行都已建。
	idx, rest, _ := strings.Cut(readme, "### 4.1")
	sup, _, _ := strings.Cut(rest, "## 5.")
	indexM1 := map[string]bool{}
	for _, m := range regexp.MustCompile(`\|\s*([a-z_]+)\s*\|\s*[a-z-]+\s*\|\s*M1`).FindAllStringSubmatch(idx, -1) {
		indexM1[m[1]] = true
	}
	supSet := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(sup, -1) {
		supSet[m[1]] = true
	}
	for tbl := range indexM1 {
		if !mig[tbl] {
			t.Errorf("README §4 索引标 M1 的表 %s 未建", tbl)
		}
	}
	for tbl := range mig {
		if !indexM1[tbl] && !supSet[tbl] {
			t.Errorf("表 %s 既不在 README §4 的 M1 索引也不在 §4.1 补充清单", tbl)
		}
	}
	// 补充表（epics AC6）已同时出现在文档与迁移中；P2 的 website_preview_links 仅在文档（不在 M1 迁移）。
	for _, tbl := range []string{"node_tasks", "deployment_target_steps", "publish_queue", "site_versions", "rollup_windows"} {
		if !mig[tbl] || !supSet[tbl] {
			t.Errorf("补充表 %s 必须同时出现在迁移与文档补充清单", tbl)
		}
	}
	if !strings.Contains(cms.String(), "website_preview_links") || mig["website_preview_links"] {
		t.Error("website_preview_links 应只在文档（P2）而不在 M1 迁移")
	}
	// 基线 §18 的 80 张核心表：README §4 总索引的表（M1 + P2–P5）共 80 行减补充 = 核对到的核心表数不少于 80 的口径由文档自身维护；
	// 此处仅断言 P2–P5 表一律未在 M1 创建。
	laterRe := regexp.MustCompile(`\|\s*([a-z_]+)\s*\|\s*[a-z-]+\s*\|\s*P[2-6]`)
	for _, m := range laterRe.FindAllStringSubmatch(idx, -1) {
		if mig[m[1]] {
			t.Errorf("P2–P5 阶段表 %s 不得出现在 M1 迁移中", m[1])
		}
	}
}

// ---- 真实 MySQL：空库 up、重放幂等、授权 ----

func TestM1CUpOnEmptyDatabaseThenReplayIsIdempotent(t *testing.T) {
	ctx := ctxT(t)
	e, r := m1cUp(t)

	var business []string
	for _, tbl := range e.tables(t) {
		if !strings.HasPrefix(tbl, "schema_migrations") {
			business = append(business, tbl)
		}
	}
	if want := m1AllTables(); len(business) != 61 || join(business) != join(want) {
		t.Fatalf("M1 应恰好 61 张表\n实际 %v\n期望 %v", business, want)
	}
	wantVersions(t, e, "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13")

	// 触发器：audit_logs 2 + rule_versions 2 + config_blocks 2 + config_revisions 2 + config_revision_blocks 1。
	wantTrg := "trg_audit_logs_bd,trg_audit_logs_bu,trg_config_blocks_bd,trg_config_blocks_bu,trg_config_revisions_bd,trg_config_revisions_bu,trg_config_revision_blocks_bu,trg_rule_versions_bd,trg_rule_versions_bu"
	if trg := e.column(t, "SELECT TRIGGER_NAME FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY TRIGGER_NAME", e.dbName); join(trg) != wantTrg {
		t.Errorf("触发器 = %v", trg)
	}
	// 循环外键：此刻全部带 FK（动作符合文档）。
	for _, c := range [][4]string{
		{"publish_queue", "fk_publish_queue_deployments", "deployments", "SET NULL"},
		{"clusters", "fk_clusters_confirmed_revision", "config_revisions", "RESTRICT"},
		{"sites", "fk_sites_last_deployment", "deployments", "SET NULL"},
		{"site_rules", "fk_site_rules_current_version", "rule_versions", "SET NULL"},
		{"site_rules", "fk_site_rules_published_version", "rule_versions", "SET NULL"},
		{"deployments", "fk_deployments_queue", "publish_queue", "SET NULL"},
	} {
		got := e.scalar(t, "SELECT CONCAT(REFERENCED_TABLE_NAME, ':', DELETE_RULE) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND TABLE_NAME = ? AND CONSTRAINT_NAME = ?", e.dbName, c[0], c[1])
		if got != c[2]+":"+c[3] {
			t.Errorf("%s.%s = %q，期望 %s:%s", c[0], c[1], got, c[2], c[3])
		}
	}
	// 高频表无外键（000013）；块内容在 MySQL（MEDIUMBLOB，AC5）；时间列统一 DATETIME(3)；唯一键含 log_date。
	if n := e.scalar(t, "SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ? AND (TABLE_NAME IN ('metric_rollups','rollup_windows','access_log_chunks') OR REFERENCED_TABLE_NAME IN ('metric_rollups','rollup_windows','access_log_chunks'))", e.dbName); n != "0" {
		t.Errorf("000013 的高频表不得有外键: %s", n)
	}
	if got := e.scalar(t, "SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'config_blocks' AND COLUMN_NAME = 'content'", e.dbName); got != "mediumblob" {
		t.Errorf("config_blocks.content = %s，期望 mediumblob（块内容存 MySQL）", got)
	}
	for _, c := range [][2]string{{"metric_rollups", "window_start"}, {"rollup_windows", "window_start"}} {
		if got := e.scalar(t, "SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?", e.dbName, c[0], c[1]); got != "datetime(3)" {
			t.Errorf("%s.%s = %s，期望 datetime(3)", c[0], c[1], got)
		}
	}
	if cols := e.scalar(t, "SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'access_log_chunks' AND INDEX_NAME = 'uk_alc_dedupe'", e.dbName); cols != "node_id,segment_seq,start_offset,end_offset,site_id,log_date" {
		t.Errorf("uk_alc_dedupe 列 = %s（须含 log_date，IMP-08）", cols)
	}
	// queued_key 为 STORED，基列 site_id 外键 RESTRICT（README §2.2）。
	if got := e.scalar(t, "SELECT EXTRA FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'publish_queue' AND COLUMN_NAME = 'queued_key'", e.dbName); got != "STORED GENERATED" {
		t.Errorf("publish_queue.queued_key = %q", got)
	}
	// block_key 与 manifest blocks[] 一一对应：列存在且无并列 global 列（IMP-16/L20）。
	if n := e.scalar(t, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME IN ('config_revisions','config_revision_blocks') AND COLUMN_NAME IN ('global_block_id','global_sha256','blob_path')", e.dbName); n != "0" {
		t.Error("修订表不得有并列 global 块列或 blob 文件路径列")
	}
	// alert_events.open_key（P2）不属 M1：本迁移集中不得出现该表（AC5 的口径由后续阶段迁移落实，骨架已验证 VARCHAR(255)）。
	if e.hasTable(t, "alert_events") {
		t.Error("alert_events 属 P2，不得在 M1 创建")
	}

	// ---- 逐表授权：app 对 61 张表各一条；仅 audit_logs 追加只写；maint 只有两张分区表 ----
	app := e.grantsOf(t, migrate.AppUser)
	if len(app) != 61 {
		t.Fatalf("app 的表级授权应恰为 61 条，实际 %d", len(app))
	}
	for _, tbl := range m1AllTables() {
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

	// ---- 真实账号的最小权限负向用例（C 批表）----
	ap := e.appPool(t).Executor(ctx)
	denied := func(name string, q db.Query) {
		t.Helper()
		_, err := ap.Exec(ctx, q)
		if n, ok := mysqlErr(err); !ok || n != db.MySQLErrTableAccessDenied {
			t.Errorf("app %s 必须被表级授权拒绝（ERROR 1142）: %v", name, err)
		}
	}
	denied("DROP TABLE config_revisions", db.Q("DROP TABLE config_revisions"))
	denied("TRUNCATE config_blocks", db.Q("TRUNCATE TABLE config_blocks"))
	denied("ALTER TABLE deployments", db.Q("ALTER TABLE deployments ADD COLUMN evil INT NULL"))
	denied("DROP FOREIGN KEY clusters.confirmed", db.Q("ALTER TABLE clusters DROP FOREIGN KEY fk_clusters_confirmed_revision"))
	denied("ALTER metric_rollups PARTITION", db.Q("ALTER TABLE metric_rollups PARTITION BY HASH(id) PARTITIONS 2"))
	denied("DROP TABLE access_log_chunks", db.Q("DROP TABLE access_log_chunks"))
	denied("DROP TRIGGER config_revisions", db.Q("DROP TRIGGER trg_config_revisions_bu"))
	denied("CREATE TABLE", db.Q("CREATE TABLE evil_c (id INT)"))
	denied("GRANT", db.Q("GRANT ALL ON config_blocks TO lingedge_app"))
	mx := e.maintPool(t).Executor(ctx)
	for name, q := range map[string]db.Query{
		"DROP PARTITION metric_rollups": db.Q("ALTER TABLE metric_rollups DROP PARTITION p0"),
		"ALTER deployments":             db.Q("ALTER TABLE deployments ADD COLUMN evil INT NULL"),
		"SELECT config_revisions":       db.Q("SELECT 1 FROM config_revisions"),
		"DELETE access_log_chunks":      db.Q("DELETE FROM access_log_chunks"),
		"DROP TABLE config_blocks":      db.Q("DROP TABLE config_blocks"),
		"TRUNCATE metric_rollups":       db.Q("TRUNCATE TABLE metric_rollups"),
		"ALTER publish_queue DROP FK":   db.Q("ALTER TABLE publish_queue DROP FOREIGN KEY fk_publish_queue_deployments"),
		"INSERT rollup_windows":         db.Q("INSERT INTO rollup_windows (node_id, window_start, window_seconds, generation, content_sha256) VALUES (1, NOW(3), 60, 1, 'x')"),
	} {
		if _, err := mx.Exec(ctx, q); func() bool { n, ok := mysqlErr(err); return !ok || n != db.MySQLErrTableAccessDenied }() {
			t.Errorf("maint %s 必须被拒绝（ERROR 1142）: %v", name, err)
		}
	}

	// app 对不可变表：可 INSERT；UPDATE/DELETE 虽有表级权限仍被触发器拒绝；仅在 purge 事务内可 DELETE（清理任务路径）。
	c := e.conn(t)
	var clusterID, revID int64
	{
		res, err := c.ExecContext(ctx, "INSERT INTO clusters (uid, name, settings_json) VALUES (?, 'priv-c', JSON_OBJECT())", uid26())
		if err != nil {
			t.Fatal(err)
		}
		clusterID, _ = res.LastInsertId()
	}
	ins := db.Q("INSERT INTO config_revisions (uid, cluster_id, lineage, seq, kind, schema_version, manifest_canonical, manifest_sha256, signature, key_id, block_count, total_size, request_id) VALUES (?, ?, 1, 1, 'publish', 1, 'm', ?, 'sig', 'k1', 0, 0, ?)")
	if _, err := ap.Exec(ctx, ins, uid26(), clusterID, fmt.Sprintf("%064d", 1), uid26()); err != nil {
		t.Fatalf("app 应能 INSERT config_revisions: %v", err)
	}
	if err := c.QueryRowContext(ctx, "SELECT id FROM config_revisions WHERE cluster_id = ?", clusterID).Scan(&revID); err != nil {
		t.Fatal(err)
	}
	for s, q := range map[string]db.Query{
		"UPDATE config_revisions": db.Q("UPDATE config_revisions SET key_id = 'x'"),
		"DELETE config_revisions": db.Q("DELETE FROM config_revisions"),
	} {
		if _, err := ap.Exec(ctx, q); err == nil {
			t.Errorf("app 的 %s 应被拒绝", s)
		} else if n, ok := mysqlErr(err); !ok || n != errSignal {
			t.Errorf("app 的 %s 必须被不可变触发器拒绝（1644）: %v", s, err)
		}
	}
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.config_revisions WHERE id = ?", revID); got != "1" {
		t.Fatalf("被拒绝的 DELETE 不得删除修订: %s", got)
	}
	// purge 路径：同一事务内置会话变量后 DELETE 才被放行。
	ap2 := e.appPool(t)
	if err := ap2.WithTx(ctx, func(txctx context.Context) error {
		x := ap2.Executor(txctx)
		if _, err := x.Exec(txctx, db.Q("SET @lingedge_purge = 1")); err != nil {
			return err
		}
		_, err := x.Exec(txctx, db.Q("DELETE FROM config_revisions"))
		_, _ = x.Exec(txctx, db.Q("SET @lingedge_purge = 0")) // 清理任务须在事务末尾复位，防止池化连接带着放行标志
		return err
	}); err != nil {
		t.Fatalf("purge 事务内 app 应能 DELETE: %v", err)
	}
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.config_revisions"); got != "0" {
		t.Errorf("purge 后修订应被删除: %s", got)
	}

	// ---- 重放幂等：Up 再次为空操作；C 批每条语句（含 PREPARE 守卫、触发器重建、GRANT）重放一遍，结构/授权/触发器/外键不变 ----
	sigBefore := e.schemaSignature(t)
	trgQ := "SELECT CONCAT(TRIGGER_NAME, ':', ACTION_TIMING, ':', EVENT_MANIPULATION) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY 1"
	trgBefore := e.column(t, trgQ, e.dbName)
	fkQ := "SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = ?"
	fkBefore := e.scalar(t, fkQ, e.dbName)
	if res, err := r.Up(ctx, migrate.UpOptions{}); err != nil || len(res.Applied) != 0 {
		t.Fatalf("第二次 Up 应为空操作: %+v %v", res, err)
	}
	rc := e.conn(t)
	var replayed, guards int
	for _, m := range r.Set().Migrations[len(m1abFiles()):] {
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
	if replayed < 40 || guards != 3 {
		t.Fatalf("重放语句数/守卫数异常: %d / %d", replayed, guards)
	}
	if e.schemaSignature(t) != sigBefore {
		t.Error("重放全部 C 批语句后库结构与授权不得变化")
	}
	if trgAfter := e.column(t, trgQ, e.dbName); join(trgAfter) != join(trgBefore) {
		t.Errorf("重放后触发器变化: %v → %v", trgBefore, trgAfter)
	}
	if fkAfter := e.scalar(t, fkQ, e.dbName); fkAfter != fkBefore {
		t.Errorf("重放后外键数变化: %s → %s", fkBefore, fkAfter)
	}
	// expand 迁移不提供数据库回退（降级对 reversible 才适用；本集合 irreversible=0 且全 expand，故无可降级步）。
	_, err := r.Down(ctx, migrate.DownOptions{})
	var dn *migrate.ErrDownNotAllowed
	if !errors.As(err, &dn) || dn.File != "000013_observability.sql" {
		t.Errorf("expand 应拒绝回退: %v", err)
	}
	if p, _ := r.Plan(ctx); p.Count != 0 {
		t.Errorf("plan 应为空: %+v", p)
	}
}

// ---- 租户索引（AC5/README §2.6）：C 批租户表的列表查询走 tenant_id 打头索引 ----

func TestM1CTenantListQueriesUseTenantLeadingIndexes(t *testing.T) {
	e, _ := m1cUp(t)
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
		b := cc.ok("INSERT INTO config_blocks (sha256, kind, schema_version, size_raw, size_stored, content) VALUES (?, 'global', 1, 1, 1, 'x')", hash())
		for j := 0; j < 2; j++ {
			s := cc.ok("INSERT INTO sites (uid, tenant_id, name) VALUES (?, ?, ?)", uid26(), tn, fmt.Sprintf("s%d", next()))
			cc.ok("INSERT INTO site_versions (uid, tenant_id, site_id, version_no, block_id) VALUES (?, ?, ?, 1, ?)", uid26(), tn, s, b)
			cc.ok("INSERT INTO metric_rollups (granularity, window_start, dim_type, dim_id, tenant_id, metric, value) VALUES ('1m', NOW(3), 'site', ?, ?, 'requests', 1)", s, tn)
			cc.ok(`INSERT INTO access_log_chunks (tenant_id, site_id, node_id, log_date, segment_seq, start_offset, end_offset, storage_path, file_offset, stored_length,
				line_count, raw_bytes, stored_bytes, min_ts, max_ts, sha256) VALUES (?, ?, 1, '2026-11-20', ?, 0, 1, 'p', 0, 1, 1, 1, 1, NOW(3), NOW(3), ?)`, tn, s, next(), hash())
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	leading := func(tbl, idx string) string {
		return e.scalar(t, "SELECT COLUMN_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ? AND SEQ_IN_INDEX = 1", e.dbName, tbl, idx)
	}
	usingRe := regexp.MustCompile(`using (\w+)`)
	for _, tbl := range []string{"site_versions", "metric_rollups", "access_log_chunks"} {
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
	// 非 tenant 打头的索引都已显式登记豁免（schema lint 已保证，这里再从真实库断言：租户表所有非主键索引要么 tenant_id 打头，要么在登记清单内）。
	allowed := map[string]bool{
		"site_versions.uk_site_versions_uid": true, "site_versions.uk_site_versions": true, "site_versions.idx_site_versions_block": true, "site_versions.idx_site_versions_user": true,
		"metric_rollups.uk_metric_rollups": true, "metric_rollups.idx_metric_rollups_time": true,
		"access_log_chunks.uk_alc_dedupe": true, "access_log_chunks.idx_alc_node": true, "access_log_chunks.idx_alc_date": true,
	}
	for _, r := range e.column(t, `SELECT CONCAT(TABLE_NAME, '.', INDEX_NAME) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = ? AND SEQ_IN_INDEX = 1 AND COLUMN_NAME <> 'tenant_id'
		AND INDEX_NAME <> 'PRIMARY' AND TABLE_NAME IN ('site_versions', 'metric_rollups', 'access_log_chunks')`, e.dbName) {
		if !allowed[r] {
			t.Errorf("租户表索引 %s 未以 tenant_id 打头且未登记豁免", r)
		}
	}
}

// ---- DDL 陷阱预检覆盖（C 批）：对真实迁移变异，静态预检必须报出，且真实 MySQL 以文档记录的错误号拒绝 ----

func TestM1CDDLTrapsAreCoveredByStaticPrecheckAndRealMySQL(t *testing.T) {
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
	muts := []struct {
		name     string
		mutate   func(map[string]*mapFile)
		wantRule string
		wantErr  uint16
	}{
		{
			name: "CHECK + FK 动作：publish_queue.tenant_id 改 SET NULL（被 ck_publish_queue_site_tenant 引用）",
			mutate: replace("000012_config_revisions.sql", "CONSTRAINT fk_publish_queue_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT",
				"CONSTRAINT fk_publish_queue_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE SET NULL"),
			wantRule: migrate.RuleCheckFK, wantErr: 3823,
		},
		{
			name: "STORED 生成列的基列外键 CASCADE：publish_queue.queued_key（基列 site_id）",
			mutate: replace("000012_config_revisions.sql", "CONSTRAINT fk_publish_queue_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT",
				"CONSTRAINT fk_publish_queue_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE CASCADE"),
			wantRule: migrate.RuleGeneratedFK, wantErr: 1215,
		},
		{
			name: "前向外键：publish_queue 内联引用尚未创建的 deployments（循环外键应先建后 ALTER）",
			mutate: replace("000012_config_revisions.sql", "  CONSTRAINT fk_publish_queue_users FOREIGN KEY (requested_by) REFERENCES users (id) ON DELETE SET NULL\n",
				"  CONSTRAINT fk_publish_queue_users FOREIGN KEY (requested_by) REFERENCES users (id) ON DELETE SET NULL,\n  CONSTRAINT fk_publish_queue_inline FOREIGN KEY (deployment_id) REFERENCES deployments (id) ON DELETE SET NULL\n"),
			wantRule: migrate.RuleForwardFK, wantErr: 1824,
		},
		{
			name: "前向外键：deployments（引用 publish_queue）先于 publish_queue 创建",
			mutate: func(files map[string]*mapFile) {
				s := string(files["000012_config_revisions.sql"].Data)
				iq := strings.Index(s, "-- ---- publish_queue：")
				id := strings.Index(s, "-- ---- deployments：")
				it := strings.Index(s, "-- ---- deployment_targets：")
				files["000012_config_revisions.sql"].Data = []byte(s[:iq] + s[id:it] + s[iq:id] + s[it:])
			},
			wantRule: migrate.RuleForwardFK, wantErr: 1824,
		},
		{
			name: "前向外键：config_revision_blocks 先于其引用的 config_blocks 创建",
			mutate: func(files map[string]*mapFile) {
				s := string(files["000012_config_revisions.sql"].Data)
				ib := strings.Index(s, "-- ---- config_blocks：")
				ir := strings.Index(s, "-- ---- config_revisions：")
				irb := strings.Index(s, "-- ---- config_revision_blocks：")
				isv := strings.Index(s, "-- ---- site_versions：")
				files["000012_config_revisions.sql"].Data = []byte(s[:ib] + s[ir:irb] + s[irb:isv] + s[ib:ir] + s[isv:])
			},
			wantRule: migrate.RuleForwardFK, wantErr: 1824,
		},
	}
	for _, mu := range muts {
		t.Run(mu.name, func(t *testing.T) {
			files := m1AllFixture(t)
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
			n, where := firstFailure(t, newEnv(t), set)
			if n != mu.wantErr {
				t.Fatalf("真实 MySQL 应以 ERROR %d 拒绝（预检规则 %s 的文档记录），实际 %d：%s", mu.wantErr, mu.wantRule, n, where)
			}
		})
	}
	// CC-1 的陷阱：证书外键改回 SET NULL 与复合外键冲突，真实 MySQL 报 ERROR 1830（静态预检无此规则，由本用例与 ddl-rules.sql 的 composite-fk-setnull-notnull 守护）。
	t.Run("复合外键 SET NULL 与 tenant_id NOT NULL 冲突（CC-1 回退即失败）", func(t *testing.T) {
		files := m1AllFixture(t)
		replace("000009_site_config.sql", "CONSTRAINT fk_site_domains_certificates FOREIGN KEY (tenant_id, certificate_id) REFERENCES certificates (tenant_id, id) ON DELETE RESTRICT",
			"CONSTRAINT fk_site_domains_certificates FOREIGN KEY (tenant_id, certificate_id) REFERENCES certificates (tenant_id, id) ON DELETE SET NULL")(files)
		set, _, err := migrate.Load(toMapFS(files))
		if err != nil {
			t.Fatal(err)
		}
		if n, where := firstFailure(t, newEnv(t), set); n != 1830 {
			t.Fatalf("应以 ERROR 1830 失败，实际 %d：%s", n, where)
		}
	})
	t.Run("未变异的 000001–000013 真实迁移逐条执行 0 错误", func(t *testing.T) {
		set, findings, err := migrate.Load(toMapFS(m1AllFixture(t)))
		if err != nil || len(findings) != 0 {
			t.Fatalf("%v %v", err, findings)
		}
		if n, where := firstFailure(t, newEnv(t), set); n != 0 {
			t.Fatalf("真实迁移逐条执行应 0 错误: %d %s", n, where)
		}
	})
}

// ---- schema lint 在 C 批真实迁移上的变异覆盖 ----

func TestM1CSchemaLintCatchesViolationsInRealMigrations(t *testing.T) {
	cases := []struct {
		name, file, old, new, rule string
	}{
		{"TEXT/BLOB 之外：JSON DEFAULT", "000012_config_revisions.sql", "batch_plan_json JSON NULL,", "batch_plan_json JSON NULL DEFAULT (JSON_OBJECT()),", migrate.RuleSchemaLOBDefault},
		{"TIMESTAMP", "000013_observability.sql", "window_start DATETIME(3) NOT NULL,\n  dim_type", "window_start TIMESTAMP NOT NULL,\n  dim_type", migrate.RuleSchemaTime},
		{"DATETIME 无精度", "000013_observability.sql", "window_start DATETIME(3) NOT NULL,\n  window_seconds", "window_start DATETIME NOT NULL,\n  window_seconds", migrate.RuleSchemaTime},
		{"ENUM", "000012_config_revisions.sql", "kind VARCHAR(20) NOT NULL,\n  expected_config_version", "kind ENUM('site_publish','site_rollback','cluster_rollback') NOT NULL,\n  expected_config_version", migrate.RuleSchemaEnum},
		{"租户表索引未以 tenant_id 打头", "000012_config_revisions.sql", "  KEY idx_site_versions_tenant (tenant_id, site_id, created_at),\n", "  KEY idx_site_versions_tenant (tenant_id, site_id, created_at),\n  KEY idx_site_versions_status_only (status, tenant_id),\n", migrate.RuleSchemaTenantIndex},
		{"唯一键豁免被删除", "000013_observability.sql", "-- lingedge:cross-tenant-index=uk_alc_dedupe 至少一次投递 + 服务端去重键（节点分段区间 + site + log_date，跨租户写入路径）\n", "", migrate.RuleSchemaTenantIndex},
		{"租户表缺 tenant_id（登记表 site_versions）", "000012_config_revisions.sql", "  tenant_id BIGINT UNSIGNED NOT NULL,\n  site_id BIGINT UNSIGNED NOT NULL,\n  version_no", "  tenant_id BIGINT UNSIGNED NULL,\n  site_id BIGINT UNSIGNED NOT NULL,\n  version_no", migrate.RuleSchemaTenantColumn},
		{"新表缺逐表授权（site_versions 的 GRANT 被删）", "000012_config_revisions.sql", "GRANT SELECT, INSERT, UPDATE, DELETE ON site_versions TO lingedge_app;\n", "", "grant-app"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := m1AllFixture(t)
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

// AC4（CF-2）：000001–000013 已登记到 migrations/CHECKSUMS，且哈希与当前文件一致（封存后任何改动都会使本用例与 make migrations-check 失败）。
func TestM1CFrozenMigrationsAreSealedInChecksums(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/CHECKSUMS")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && !strings.HasPrefix(line, "#") {
			listed[f[1]] = f[0]
		}
	}
	for _, name := range m1AllFiles() {
		b, err := os.ReadFile(filepath.Join("../../migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := listed[name]; got != hex.EncodeToString(sum[:]) {
			t.Errorf("%s 未登记或哈希与文件不一致（CF-2 封存）: %q", name, got)
		}
	}
}
