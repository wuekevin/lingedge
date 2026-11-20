package httpxintegration

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

func ctxT(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// env 是一个用例的独立环境：随机库 + 全部迁移 + 真实 lingedge_app / migrate 账号。
type env struct {
	inst   *mysqltest.Instance
	adm    *sql.DB
	dbName string
	app    *db.AppPool
	maint  *db.MigratePool // 迁移账号池（仅用于迁移状态探测；app 账号对 schema_migrations 无授权）
	runner *migrate.Runner
}

func newEnv(t *testing.T) *env {
	t.Helper()
	inst := mysqltest.Start(t)
	e := &env{inst: inst, dbName: inst.NewDatabase(t)}
	appPW, maintPW, migPW := mysqltest.RandomSecret(t), mysqltest.RandomSecret(t), mysqltest.RandomSecret(t)
	migUser := mysqltest.RandomName("mig")
	for _, a := range []struct{ u, p string }{{migrate.AppUser, appPW}, {migrate.MaintUser, maintPW}, {migUser, migPW}} {
		inst.Exec(t, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", a.u, a.p))
		inst.DropUserOnCleanup(t, a.u)
	}
	inst.Exec(t, "GRANT ALL ON `"+e.dbName+"`.* TO '"+migUser+"'@'%' WITH GRANT OPTION")
	inst.Exec(t, "GRANT SET_USER_ID ON *.* TO '"+migUser+"'@'%'")

	cfg := func(user, pw string) db.Config {
		return db.Config{Host: inst.Host(), Port: inst.Port, Name: e.dbName, User: user, Password: config.NewSecret(pw), MaxOpenConns: 8, MaxIdleConns: 4}
	}
	var err error
	if e.maint, err = db.OpenMigrate(ctxT(t), cfg(migUser, migPW)); err != nil {
		t.Fatalf("OpenMigrate: %v", err)
	}
	t.Cleanup(func() { _ = e.maint.Close() })
	if e.runner, err = migrate.NewRunner(e.maint, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runner.Up(ctxT(t), migrate.UpOptions{}); err != nil {
		t.Fatalf("全部迁移 up: %v", err)
	}
	if e.app, err = db.OpenApp(ctxT(t), cfg(migrate.AppUser, appPW)); err != nil {
		t.Fatalf("OpenApp: %v", err)
	}
	t.Cleanup(func() { _ = e.app.Close() })
	e.adm = inst.Admin(t)
	e.adm.SetMaxOpenConns(3)
	return e
}

// root 以 root 执行语句（造数/旁路篡改，绕过被测路径）；§ 代表库名。
func (e *env) root(t testing.TB, q string, args ...any) {
	t.Helper()
	if _, err := e.adm.ExecContext(ctxT(t), strings.ReplaceAll(q, "§", "`"+e.dbName+"`"), args...); err != nil {
		t.Fatalf("root 执行失败: %v\n%s", err, q)
	}
}

func (e *env) rootInt(t testing.TB, q string, args ...any) int64 {
	t.Helper()
	var n sql.NullInt64
	if err := e.adm.QueryRowContext(ctxT(t), strings.ReplaceAll(q, "§", "`"+e.dbName+"`"), args...).Scan(&n); err != nil {
		t.Fatalf("root 查询失败: %v\n%s", err, q)
	}
	return n.Int64
}

// fakeClock 是可推进的时钟（clock.Clock 的测试实现；数据库里的过期判定也用它传参）。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: clock.System().Now()} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
