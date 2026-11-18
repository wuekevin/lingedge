package migrationstest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

const fixturesDir = "../fixtures/migrations"

func ctxT(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// fixtureFS 读取 tests/fixtures/migrations/<layer> 下的文件并叠加（后者覆盖前者）。
func fixtureFS(t testing.TB, layers ...string) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	for _, l := range layers {
		ents, err := os.ReadDir(filepath.Join(fixturesDir, l))
		if err != nil {
			t.Fatalf("读取夹具 %s: %v", l, err)
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(fixturesDir, l, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	return out
}

// runtimeAccounts：lingedge_app / lingedge_maint 是迁移里写死的授权对象（lint 要求），每个临时实例各建一次。
type runtimeAccounts struct{ appPW, maintPW string }

var (
	rtMu sync.Mutex
	rt   = map[*mysqltest.Instance]*runtimeAccounts{}
)

func ensureRuntimeAccounts(t testing.TB, inst *mysqltest.Instance) *runtimeAccounts {
	t.Helper()
	rtMu.Lock()
	defer rtMu.Unlock()
	if a, ok := rt[inst]; ok {
		return a
	}
	a := &runtimeAccounts{appPW: mysqltest.RandomSecret(t), maintPW: mysqltest.RandomSecret(t)}
	// 口令是运行期生成的十六进制串（无引号风险）；CREATE USER 不支持占位符。
	inst.Exec(t, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", migrate.AppUser, a.appPW))
	inst.Exec(t, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", migrate.MaintUser, a.maintPW))
	rt[inst] = a
	return a
}

type env struct {
	inst   *mysqltest.Instance
	adm    *sql.DB // 懒创建并复用：mysqltest.Admin 每次调用都会新建连接池，反复调用会耗尽 max_connections
	dbName string
	user   string
	pw     string
	rt     *runtimeAccounts
}

// newEnv 建随机库与迁移账号：ALL ON db.* WITH GRANT OPTION + 全局 SET_USER_ID（database/README §5.1）。
func newEnv(t testing.TB, opts ...mysqltest.Options) *env {
	return newEnvWith(t, true, opts...)
}

func newEnvWith(t testing.TB, setUserID bool, opts ...mysqltest.Options) *env {
	t.Helper()
	inst := mysqltest.Start(t, opts...)
	e := &env{inst: inst, dbName: inst.NewDatabase(t), user: mysqltest.RandomName("mig"), pw: mysqltest.RandomSecret(t)}
	e.rt = ensureRuntimeAccounts(t, inst)
	inst.Exec(t, fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", e.user, e.pw))
	inst.DropUserOnCleanup(t, e.user)
	inst.Exec(t, "GRANT ALL ON `"+e.dbName+"`.* TO '"+e.user+"'@'%' WITH GRANT OPTION")
	if setUserID {
		inst.Exec(t, "GRANT SET_USER_ID ON *.* TO '"+e.user+"'@'%'")
	}
	return e
}

// admin 返回以 root 连接本临时实例的共享连接池（限制并发连接数）。
func (e *env) admin(t testing.TB) *sql.DB {
	t.Helper()
	if e.adm == nil {
		e.adm = e.inst.Admin(t)
		e.adm.SetMaxOpenConns(3)
	}
	return e.adm
}

func (e *env) cfg(user, pw string) db.Config {
	return db.Config{Host: e.inst.Host(), Port: e.inst.Port, Name: e.dbName, User: user, Password: config.NewSecret(pw), MaxOpenConns: 4, MaxIdleConns: 2}
}

func (e *env) runner(t testing.TB, fsys fs.FS) *migrate.Runner {
	t.Helper()
	pool, err := db.OpenMigrate(ctxT(t), e.cfg(e.user, e.pw))
	if err != nil {
		t.Fatalf("OpenMigrate: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	r, err := migrate.NewRunner(pool, fsys)
	if err != nil {
		t.Fatal(err)
	}
	r.LockTimeout = 20 * time.Second
	return r
}

func (e *env) appPool(t testing.TB) *db.AppPool {
	t.Helper()
	p, err := db.OpenApp(ctxT(t), e.cfg(migrate.AppUser, e.rt.appPW))
	if err != nil {
		t.Fatalf("OpenApp: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func (e *env) maintPool(t testing.TB) *db.MaintPool {
	t.Helper()
	p, err := db.OpenMaint(ctxT(t), e.cfg(migrate.MaintUser, e.rt.maintPW))
	if err != nil {
		t.Fatalf("OpenMaint: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// scalar 以 root 查询单个值（绕过被测路径，直接核对库内真实状态）。
func (e *env) scalar(t testing.TB, query string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := e.admin(t).QueryRowContext(ctxT(t), query, args...).Scan(&s); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("查询失败: %v\n%s", err, query)
	}
	return s.String // 无行时为空串
}

func (e *env) tables(t testing.TB) []string {
	t.Helper()
	rows, err := e.admin(t).QueryContext(ctxT(t), "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME", e.dbName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func (e *env) hasTable(t testing.TB, name string) bool {
	t.Helper()
	return e.scalar(t, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", e.dbName, name) == "1"
}

var autoIncRe = regexp.MustCompile(` AUTO_INCREMENT=\d+`)

// createStmt 返回 SHOW CREATE TABLE 的规范化文本（去掉 AUTO_INCREMENT 计数）。
func (e *env) createStmt(t testing.TB, table string) string {
	t.Helper()
	var name, ddl string
	if err := e.admin(t).QueryRowContext(ctxT(t), "SHOW CREATE TABLE `"+e.dbName+"`.`"+table+"`").Scan(&name, &ddl); err != nil {
		t.Fatalf("SHOW CREATE TABLE %s: %v", table, err)
	}
	return autoIncRe.ReplaceAllString(ddl, "")
}

// grantsOf 返回某账号在本库的表级授权：表名 → 权限集合（规范化排序）。
func (e *env) grantsOf(t testing.TB, user string) map[string]string {
	t.Helper()
	rows, err := e.admin(t).QueryContext(ctxT(t), "SELECT Table_name, Table_priv FROM mysql.tables_priv WHERE Db = ? AND User = ?", e.dbName, user)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var tbl, priv string
		if err := rows.Scan(&tbl, &priv); err != nil {
			t.Fatal(err)
		}
		ps := strings.Split(priv, ",")
		sort.Strings(ps)
		out[tbl] = strings.Join(ps, ",")
	}
	return out
}

// schemaSignature 汇总库内（除迁移框架自身表外）全部表的规范化 DDL 与表级授权，用于「重跑 == 一次性执行」的等价比较。
func (e *env) schemaSignature(t testing.TB) string {
	t.Helper()
	var b strings.Builder
	for _, tbl := range e.tables(t) {
		if strings.HasPrefix(tbl, "schema_migrations") {
			continue
		}
		fmt.Fprintf(&b, "== %s\n%s\n", tbl, e.createStmt(t, tbl))
	}
	for _, u := range []string{migrate.AppUser, migrate.MaintUser} {
		g := e.grantsOf(t, u)
		var keys []string
		for k := range g {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "grant %s %s %s\n", u, k, g[k])
		}
	}
	return b.String()
}

func (e *env) appliedVersions(t testing.TB) []string {
	t.Helper()
	if !e.hasTable(t, migrate.VersionTable) {
		return nil
	}
	rows, err := e.admin(t).QueryContext(ctxT(t), "SELECT version_id FROM `"+e.dbName+"`."+migrate.VersionTable+" WHERE is_applied = 1 AND version_id > 0 ORDER BY version_id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d", v))
	}
	return out
}

func mysqlErr(err error) (uint16, bool) { return db.MySQLErrorNumber(err) }
