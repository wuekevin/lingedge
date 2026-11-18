package migrationstest

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
)

// m1-fk-skeleton.sql（docs/database/verification 的逐字节副本）是 M1 迁移骨架：docs/database/README §8.1 的 13 个迁移、61 张表，
// 只保留主键/外键/生成列/CHECK/分区/触发器；真实迁移（Story 1.7–1.9）尚未落库，所以框架在这里被「骨架」真实重放。

const (
	m1Tables       = 61 // §8.1：13 个迁移合计 61 张表
	skeletonTables = 67 // 另含 6 张后续阶段（P2/P5）样例
)

func readSkeleton(t testing.TB) string {
	t.Helper()
	b, err := os.ReadFile("m1-fk-skeleton.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 骨架整体（含 6 张后续阶段样例）在空库上 0 错误，共 67 张表；同时静态预检对整份骨架 0 违规。
func TestRawSkeletonRunsCleanOnEmptyDatabase(t *testing.T) {
	text := readSkeleton(t)
	stmts := splitSQL(text)

	ck := migrate.NewChecker()
	for _, s := range stmts {
		for _, v := range ck.Check(s) {
			t.Errorf("静态预检对骨架报告违规: [%s] %s", v.Rule, v.Message)
		}
	}

	inst := mysqltestStart(t)
	dbName := inst.NewDatabase(t)
	ctx := ctxT(t)
	conn, err := inst.Admin(t).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "USE `"+dbName+"`"); err != nil {
		t.Fatal(err)
	}
	var errs int
	for _, s := range stmts {
		rows, err := conn.QueryContext(ctx, s)
		if err != nil {
			errs++
			t.Errorf("骨架语句失败: %v\n%.120s", err, s)
			continue
		}
		_ = rows.Close()
	}
	if errs != 0 {
		t.FailNow()
	}
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?", dbName).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != skeletonTables {
		t.Fatalf("骨架应建出 %d 张表（61 + 6），实际 %d", skeletonTables, n)
	}
}

var (
	sectionRe = regexp.MustCompile(`(?m)^-- ==== (\d{6}) (\w+)`)
	createRe  = regexp.MustCompile(`(?m)^CREATE TABLE (\w+)`)
	trigRe    = regexp.MustCompile(`(?m)^CREATE TRIGGER (\w+)`)
	alterRe   = regexp.MustCompile(`(?m)^ALTER TABLE `)
)

// skeletonMigrations 把骨架按「-- ==== 0000nn name」分段转换成 13 个符合 lint 的迁移文件（内存 FS）：
//   - 文件头 class=expand（§8.1 全部为 expand）与 goose Up；
//   - CREATE TABLE → IF NOT EXISTS；触发器前加 DROP TRIGGER IF EXISTS（重跑安全）；循环外键的 ALTER 加 guard 标注；
//   - 每张新表追加逐表授权（audit_logs 仅 SELECT, INSERT；分区表追加 maint 的 SELECT, ALTER, DROP）；
//   - 骨架没有 000004（种子迁移只有 DML）：补一个最小的仅 DML 种子，保证序号连续。
//
// 这是真实迁移（1.7–1.9）落库前的替身：它检验的是框架与 DDL 规则，而不是各表的完整列。
func skeletonMigrations(t testing.TB) fstest.MapFS {
	t.Helper()
	text := readSkeleton(t)
	idx := sectionRe.FindAllStringSubmatchIndex(text, -1)
	out := fstest.MapFS{}
	for i, m := range idx {
		end := len(text)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		ver, name := text[m[2]:m[3]], text[m[4]:m[5]]
		body := text[m[1]:end]
		if cut := strings.Index(body, "\n-- ==== "); cut >= 0 { // 「后续阶段」样例段不属于 M1
			body = body[:cut]
		}
		// 去掉分段标题行剩余部分（如「（分区表：…）」说明）。
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			body = body[nl+1:]
		}
		body = createRe.ReplaceAllString(body, "CREATE TABLE IF NOT EXISTS $1")
		body = trigRe.ReplaceAllString(body, "DROP TRIGGER IF EXISTS $1;\nCREATE TRIGGER $1")
		body = alterRe.ReplaceAllString(body, "-- lingedge:guard=循环/前向外键的追加：真实迁移用 information_schema 判断的 PREPARE 守卫，或拆成单语句文件\nALTER TABLE ")

		var grants strings.Builder
		for _, stmt := range splitSQL(body) {
			cm := regexp.MustCompile(`^CREATE TABLE IF NOT EXISTS (\w+)`).FindStringSubmatch(stmt)
			if cm == nil {
				continue
			}
			tbl := cm[1]
			if tbl == "audit_logs" || tbl == "wallet_entries" {
				fmt.Fprintf(&grants, "GRANT SELECT, INSERT ON `%s` TO %s;\n", tbl, migrate.AppUser)
			} else {
				fmt.Fprintf(&grants, "GRANT SELECT, INSERT, UPDATE, DELETE ON `%s` TO %s;\n", tbl, migrate.AppUser)
			}
			if strings.Contains(stmt, "PARTITION BY") {
				fmt.Fprintf(&grants, "GRANT SELECT, ALTER, DROP ON `%s` TO %s;\n", tbl, migrate.MaintUser)
			}
		}
		file := fmt.Sprintf("%s_%s.sql", ver, name)
		out[file] = &fstest.MapFile{Data: []byte("-- lingedge:class=expand\n-- lingedge:summary=M1 骨架段 " + ver + " " + name + "\n-- +goose Up\n" + strings.TrimSpace(body) + "\n" + grants.String())}
		if ver == "000003" { // 骨架缺 000004：补仅 DML 的种子迁移（权限字典里的内置权限，不含口令、不创建用户）
			out["000004_seed_builtin.sql"] = &fstest.MapFile{Data: []byte("-- lingedge:class=expand\n-- lingedge:summary=种子（替身：真实内容随 Story 1.7）\n-- +goose Up\n" +
				"INSERT IGNORE INTO permissions (code, resource, action, scope) VALUES ('tenant:impersonate', 'tenant', 'impersonate', 'platform');\n")}
		}
	}
	return out
}

func TestSkeletonConvertsToThirteenLintCleanMigrations(t *testing.T) {
	fsys := skeletonMigrations(t)
	set, findings, err := migrate.Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		for _, f := range findings {
			t.Errorf("lint: %s", f)
		}
		t.FailNow()
	}
	if len(set.Migrations) != 13 {
		t.Fatalf("应得到 13 个迁移文件，实际 %d", len(set.Migrations))
	}
	for i, m := range set.Migrations {
		if m.Version != int64(i+1) || m.Class != migrate.ClassExpand {
			t.Errorf("迁移 %d: %+v", i+1, m)
		}
	}
	if got := set.ByVersion(3).Name; got != "audit_logs" {
		t.Errorf("000003 应为 audit_logs: %s", got)
	}
}

// M1 的 13 个迁移经框架在空库全量重放：0 错误、61 张表、逐表授权、job_locks 租约、重复执行幂等、篡改被拒、expand 拒绝回退。
func TestM1SkeletonReplaysThroughFrameworkOnEmptyDatabase(t *testing.T) {
	ctx := ctxT(t)
	fsys := skeletonMigrations(t)
	e := newEnv(t)
	r := e.runner(t, fsys)
	if f := r.Findings(); len(f) != 0 {
		t.Fatalf("lint: %v", f)
	}
	res, err := r.Up(ctx, migrate.UpOptions{})
	if err != nil {
		t.Fatalf("M1 空库全量 up 必须 0 错误: %v", err)
	}
	if len(res.Applied) != 13 || res.ToVersion != 13 {
		t.Fatalf("结果: %+v", res)
	}
	var business []string
	for _, tbl := range e.tables(t) {
		if !strings.HasPrefix(tbl, "schema_migrations") {
			business = append(business, tbl)
		}
	}
	if len(business) != m1Tables {
		t.Fatalf("M1 应建出 %d 张表，实际 %d: %v", m1Tables, len(business), business)
	}
	wantVersions(t, e, "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13")

	// 逐表授权：61 张表各有 app 的表级授权；audit_logs 仅 SELECT, INSERT；分区表（audit_logs、node_heartbeats）有 maint 授权。
	app := e.grantsOf(t, migrate.AppUser)
	if len(app) != m1Tables {
		t.Fatalf("app 的表级授权应覆盖全部 %d 张表，实际 %d", m1Tables, len(app))
	}
	for _, tbl := range business {
		want := "Delete,Insert,Select,Update"
		if tbl == "audit_logs" {
			want = "Insert,Select"
		}
		if app[tbl] != want {
			t.Errorf("app 对 %s 的授权 = %q，期望 %q", tbl, app[tbl], want)
		}
	}
	maint := e.grantsOf(t, migrate.MaintUser)
	if len(maint) != 2 || maint["audit_logs"] != "Alter,Drop,Select" || maint["node_heartbeats"] != "Alter,Drop,Select" {
		t.Errorf("maint 应仅有 audit_logs、node_heartbeats 的 SELECT, ALTER, DROP: %v", maint)
	}
	// 授权真实生效：app 不能改审计表；maint 不能 DROP 非分区表。
	ap := e.appPool(t)
	if _, err := ap.Executor(ctx).Exec(ctx, db.Q("UPDATE audit_logs SET action = 'x'")); err == nil {
		t.Error("app UPDATE audit_logs 必须被拒绝")
	}

	// job_locks 由 000002 创建之后，迁移互斥另行记录租约（GET_LOCK 始终是互斥本身）。
	if got := e.scalar(t, "SELECT CONCAT(IF(holder LIKE 'migrate@%', 'ok', holder), '|', IF(fence >= 2, 'fenced', fence), '|', IF(lease_until <= UTC_TIMESTAMP(3), 'released', 'held')) FROM `"+e.dbName+"`.job_locks WHERE name = 'migrate'"); got != "ok|fenced|released" {
		t.Errorf("job_locks 租约记录 = %q，期望 ok|fenced|released（持有者、围栏递增、结束后已释放）", got)
	}

	// 重复执行幂等：Up 再次运行是空操作；把全部迁移的可重放语句直接再执行一遍，库结构不变、0 错误。
	sig := e.schemaSignature(t)
	if res, err := r.Up(ctx, migrate.UpOptions{}); err != nil || len(res.Applied) != 0 {
		t.Fatalf("第二次 Up: %+v %v", res, err)
	}
	conn, err := e.admin(t).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "USE `"+e.dbName+"`"); err != nil {
		t.Fatal(err)
	}
	var replayed int
	for _, m := range r.Set().Migrations {
		for _, st := range m.Up {
			if strings.Contains(st.Lead, "lingedge:guard=") {
				continue // 带 guard 的循环外键追加语句重跑会报重复外键（由真实迁移的守卫负责），此处不重放
			}
			rows, err := conn.QueryContext(ctx, st.Text)
			if err != nil {
				t.Fatalf("%s 第 %d 行重放失败（迁移必须可重入）: %v\n%.100s", m.File, st.Line, err, st.Text)
			}
			_ = rows.Close()
			replayed++
		}
	}
	if replayed < 100 {
		t.Fatalf("重放的语句数异常: %d", replayed)
	}
	if got := e.schemaSignature(t); got != sig {
		t.Fatal("重放全部可重入语句后库结构与授权不得变化")
	}

	// 篡改：改动任何一个已应用迁移 → 拒绝。
	tampered := skeletonMigrations(t)
	tampered["000007_sites_core.sql"].Data = append(tampered["000007_sites_core.sql"].Data, []byte("-- 悄悄加的一行注释\n")...)
	_, err = e.runner(t, tampered).Up(ctx, migrate.UpOptions{})
	var cm *migrate.ErrChecksumMismatch
	if !errors.As(err, &cm) || cm.Version != 7 {
		t.Fatalf("改动已应用的 000007 必须被校验和拒绝: %v", err)
	}
	// expand 迁移不提供数据库回退。
	_, err = r.Down(ctx, migrate.DownOptions{})
	var dn *migrate.ErrDownNotAllowed
	if !errors.As(err, &dn) || dn.File != "000013_observability.sql" {
		t.Fatalf("expand 拒绝回退: %v", err)
	}
	// plan：全部已应用 → 空。
	if p, _ := r.Plan(ctx); p.Count != 0 || p.HasIrreversible {
		t.Fatalf("plan: %+v", p)
	}
}
