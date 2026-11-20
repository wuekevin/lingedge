package authit

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

// env 是一个用例的独立 MySQL 环境：随机库 + 全部迁移 + 真实 lingedge_app 账号。
type env struct {
	inst   *mysqltest.Instance
	adm    *sql.DB
	dbName string
	app    *db.AppPool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	inst := mysqltest.Start(t)
	e := &env{inst: inst, dbName: inst.NewDatabase(t)}
	appPW, migPW := mysqltest.RandomSecret(t), mysqltest.RandomSecret(t)
	migUser := mysqltest.RandomName("mig")
	for _, a := range []struct{ u, p string }{{migrate.AppUser, appPW}, {migUser, migPW}} {
		inst.Exec(t, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", a.u, a.p))
		inst.DropUserOnCleanup(t, a.u)
	}
	inst.Exec(t, "GRANT ALL ON `"+e.dbName+"`.* TO '"+migUser+"'@'%' WITH GRANT OPTION")
	inst.Exec(t, "GRANT SET_USER_ID ON *.* TO '"+migUser+"'@'%'")
	// 迁移里的授权目标 lingedge_maint 也须存在。
	maintPW := mysqltest.RandomSecret(t)
	inst.Exec(t, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", migrate.MaintUser, maintPW))
	inst.DropUserOnCleanup(t, migrate.MaintUser)

	cfg := func(user, pw string) db.Config {
		return db.Config{Host: inst.Host(), Port: inst.Port, Name: e.dbName, User: user, Password: config.NewSecret(pw), MaxOpenConns: 8, MaxIdleConns: 4}
	}
	mig, err := db.OpenMigrate(ctxT(t), cfg(migUser, migPW))
	if err != nil {
		t.Fatalf("OpenMigrate: %v", err)
	}
	t.Cleanup(func() { _ = mig.Close() })
	runner, err := migrate.NewRunner(mig, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Up(ctxT(t), migrate.UpOptions{}); err != nil {
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

func (e *env) rootStr(t testing.TB, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := e.adm.QueryRowContext(ctxT(t), strings.ReplaceAll(q, "§", "`"+e.dbName+"`"), args...).Scan(&s); err != nil {
		t.Fatalf("root 查询失败: %v\n%s", err, q)
	}
	return s.String
}

// fakeClock 是可推进的时钟（clock.Clock 的测试实现；认证模块用它判定会话过期与写入时间戳）。
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

type colRef struct{ table, col string }

// columns 返回当前库内全部「可能承载文本」的列（char/varchar/text 族、json、enum/set、binary/blob 族）：全库明文扫描用。
func (e *env) columns(t testing.TB) []colRef {
	t.Helper()
	rows, err := e.adm.QueryContext(ctxT(t), `SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ?
		AND DATA_TYPE IN ('char','varchar','tinytext','text','mediumtext','longtext','json','enum','set','binary','varbinary','tinyblob','blob','mediumblob','longblob')
		ORDER BY TABLE_NAME, ORDINAL_POSITION`, e.dbName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []colRef
	for rows.Next() {
		var c colRef
		if err := rows.Scan(&c.table, &c.col); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}
