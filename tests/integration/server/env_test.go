package serverintegration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func ctxT(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// env 是一个用例的独立环境：随机库 + 真实 lingedge_app / migrate 账号 + 独立临时 Redis + 主密钥文件 + server.yaml。
type env struct {
	inst    *mysqltest.Instance
	redis   *redistest.Instance
	adm     *sql.DB
	dbName  string
	appPW   string
	maintPW string
	migUser string
	migPW   string
	app     *db.AppPool
	runner  *migrate.Runner
	mig     *db.MigratePool

	dir     string // 临时目录（server.yaml、主密钥、NOTIFY_SOCKET）
	keyFile string
	cfgPath string
	ports   ports
}

type ports struct{ http, grpc, enroll, metrics int }

// 迁移范围：all = 全部内嵌迁移（含 000014）；m1Only = 仅 M1 冻结范围 000001–000013（模拟「库尚未升级到 000014」）。
const (
	all    = 14
	m1Only = 13
)

// migrationsThrough 返回只含序号 <= through 的内嵌迁移（through=all 时即内嵌 FS 本身）。
func migrationsThrough(t testing.TB, through int) fs.FS {
	t.Helper()
	if through >= all {
		return migrations.FS
	}
	out := fstest.MapFS{}
	ents, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range ents {
		n, _ := strconv.Atoi(strings.SplitN(en.Name(), "_", 2)[0])
		if !strings.HasSuffix(en.Name(), ".sql") || n < 1 || n > through {
			continue
		}
		b, err := fs.ReadFile(migrations.FS, en.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[en.Name()] = &fstest.MapFile{Data: b}
	}
	return out
}

// newEnv 建库、建账号，并应用 through 范围内的内嵌迁移（app 账号的库权限来自迁移，故至少需要 M1 范围）。
func newEnv(t *testing.T, through int) *env {
	t.Helper()
	inst := mysqltest.Start(t)
	e := &env{inst: inst, redis: redistest.Start(t), dbName: inst.NewDatabase(t)}
	e.appPW, e.migPW = mysqltest.RandomSecret(t), mysqltest.RandomSecret(t)
	e.maintPW = mysqltest.RandomSecret(t)
	maintPW := e.maintPW
	e.migUser = mysqltest.RandomName("mig")
	for _, a := range []struct{ u, p string }{{migrate.AppUser, e.appPW}, {migrate.MaintUser, maintPW}, {e.migUser, e.migPW}} {
		inst.Exec(t, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", a.u, a.p))
		inst.DropUserOnCleanup(t, a.u)
	}
	inst.Exec(t, "GRANT ALL ON `"+e.dbName+"`.* TO '"+e.migUser+"'@'%' WITH GRANT OPTION")
	inst.Exec(t, "GRANT SET_USER_ID ON *.* TO '"+e.migUser+"'@'%'")
	cfg := func(user, pw string) db.Config {
		return db.Config{Host: inst.Host(), Port: inst.Port, Name: e.dbName, User: user, Password: config.NewSecret(pw), MaxOpenConns: 8, MaxIdleConns: 4}
	}
	mig, err := db.OpenMigrate(ctxT(t), cfg(e.migUser, e.migPW))
	if err != nil {
		t.Fatalf("OpenMigrate: %v", err)
	}
	t.Cleanup(func() { _ = mig.Close() })
	if e.runner, err = migrate.NewRunner(mig, migrationsThrough(t, through)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runner.Up(ctxT(t), migrate.UpOptions{}); err != nil {
		t.Fatalf("迁移 up: %v", err)
	}
	e.mig = mig
	if e.app, err = db.OpenApp(ctxT(t), cfg(migrate.AppUser, e.appPW)); err != nil {
		t.Fatalf("OpenApp: %v", err)
	}
	t.Cleanup(func() { _ = e.app.Close() })
	e.adm = inst.Admin(t)
	e.adm.SetMaxOpenConns(3)

	// 短目录：Unix 数据报 socket 路径上限约 104 字节（macOS）。
	e.dir, err = os.MkdirTemp("", "lesv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(e.dir) })
	e.keyFile = filepath.Join(e.dir, "master.key")
	e.writeKey(t, 0o400)
	e.ports = ports{freePort(t), freePort(t), freePort(t), freePort(t)}
	e.writeConfig(t)
	return e
}

func (e *env) writeKey(t testing.TB, mode os.FileMode) {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(e.keyFile)
	if err := os.WriteFile(e.keyFile, []byte("k1:"+base64.RawURLEncoding.EncodeToString(b)+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.keyFile, mode); err != nil {
		t.Fatal(err)
	}
}

func (e *env) writeConfig(t testing.TB) {
	t.Helper()
	y := fmt.Sprintf(`server:
  env: test
  data_dir: %s
http:
  listen: 127.0.0.1:%d
grpc:
  listen: 127.0.0.1:%d
  enroll_listen: 127.0.0.1:%d
metrics:
  listen: 127.0.0.1:%d
database:
  host: %s
  port: %d
  name: %s
  user: %s
redis:
  addr: %s
log:
  level: info
  format: json
secrets:
  master_key_file: %s
`, filepath.Join(e.dir, "data"), e.ports.http, e.ports.grpc, e.ports.enroll, e.ports.metrics,
		e.inst.Host(), e.inst.Port, e.dbName, migrate.AppUser, e.redis.Addr(), e.keyFile)
	e.cfgPath = filepath.Join(e.dir, "server.yaml")
	if err := os.WriteFile(e.cfgPath, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
}

// environ 是子进程环境：口令只经环境变量（NFR-6）。
// 带 LINGEDGE_ 前缀的外部环境变量（如 LINGEDGE_REQUIRE_MYSQL）会被配置加载器当作未知覆盖项拒绝，故不带入子进程。
func (e *env) environ(extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LINGEDGE_") {
			out = append(out, kv)
		}
	}
	return append(append(out, "LINGEDGE_DATABASE__PASSWORD="+e.appPW), extra...)
}

func (e *env) loadConfig(t testing.TB) *config.Config {
	t.Helper()
	cfg, err := config.Load(e.cfgPath, config.Options{Environ: e.environ(), RequireRuntime: true})
	if err != nil {
		t.Fatalf("加载测试配置: %v", err)
	}
	return cfg
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

func (e *env) rootString(t testing.TB, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := e.adm.QueryRowContext(ctxT(t), strings.ReplaceAll(q, "§", "`"+e.dbName+"`"), args...).Scan(&s); err != nil {
		t.Fatalf("root 查询失败: %v\n%s", err, q)
	}
	return s.String
}

func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func eventually(t testing.TB, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("超时未满足: %s", what)
}

// upAll 在已有（仅 M1 范围的）库上补应用 000014：用全部内嵌迁移重建一个 runner（同一个 migrate 池）。
func (e *env) upAll(t testing.TB) (*migrate.UpResult, error) {
	r, err := migrate.NewRunner(e.mig, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	return r.Up(ctxT(t), migrate.UpOptions{})
}
