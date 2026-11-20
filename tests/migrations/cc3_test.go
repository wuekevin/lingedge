package migrationstest

import (
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/migrations"
)

// CC-3（Story 1.13）：000014 只给 lingedge_app 授予 schema_migrations 的 SELECT——真实 MySQL 上验证
// 能读版本、读不到运行记录表、不能写版本表（最小权限：授权恰好是一条表级 SELECT，没有多给）。
func TestCC3AppReadsSchemaMigrationsOnly(t *testing.T) {
	e := newEnv(t)
	r := e.runner(t, migrations.FS)
	res, err := r.Up(ctxT(t), migrate.UpOptions{})
	if err != nil {
		t.Fatalf("全部（含 000014）迁移 up: %v", err)
	}
	if res.ToVersion != 17 {
		t.Fatalf("up 结果: %+v", res)
	}
	app := e.appPool(t)
	ctx := ctxT(t)
	ex := app.Executor(ctx)

	var max int64
	if err := ex.Get(ctx, &max, db.Q("SELECT MAX(version_id) FROM schema_migrations WHERE is_applied = 1")); err != nil || max != 17 {
		t.Fatalf("app 应能读到迁移版本 17（最新）: %v %d", err, max)
	}
	for _, c := range []struct {
		name string
		q    db.Query
	}{
		{"INSERT 版本表", db.Q("INSERT INTO schema_migrations (version_id, is_applied) VALUES (999, 1)")},
		{"UPDATE 版本表", db.Q("UPDATE schema_migrations SET is_applied = 0")},
		{"DELETE 版本表", db.Q("DELETE FROM schema_migrations")},
		{"DROP 版本表", db.Q("DROP TABLE schema_migrations")},
		{"读运行记录表", db.Q("SELECT * FROM schema_migrations_runs")},
	} {
		_, err := ex.Exec(ctx, c.q)
		if n, ok := mysqlErr(err); !ok || (n != 1142 && n != 1143) {
			t.Errorf("%s 应被拒绝（ERROR 1142/1143），实际 %v", c.name, err)
		}
	}
	// 授权恰为一条表级 SELECT。
	g := e.grantsOf(t, migrate.AppUser)
	got, ok := g["schema_migrations"]
	if !ok || strings.ToUpper(strings.TrimSpace(got)) != "SELECT" {
		t.Errorf("lingedge_app 对 schema_migrations 的授权应恰为 SELECT，实际 %q", got)
	}
	if _, has := g["schema_migrations_runs"]; has {
		t.Errorf("不得授权 schema_migrations_runs: %v", g["schema_migrations_runs"])
	}
	// 幂等重放。
	if res, err := r.Up(ctxT(t), migrate.UpOptions{}); err != nil || len(res.Applied) != 0 {
		t.Fatalf("重放 up 应为空操作: %v %+v", err, res)
	}
}
