package dbintegration

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
)

type sessionVars struct {
	TZ   string `db:"tz"`
	CS   string `db:"cs"`
	Coll string `db:"co"`
}

// AC1：连接会话为 UTC、utf8mb4；即便服务端全局时区被改成 +08:00，池内连接仍是 UTC。
func TestPoolSessionIsUTCAndUtf8mb4_RealMySQL(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	e.inst.Exec(t, "SET GLOBAL time_zone = '+08:00'")
	t.Cleanup(func() { e.inst.Exec(t, "SET GLOBAL time_zone = 'SYSTEM'") })

	pool, err := db.OpenApp(ctx, e.cfg(e.appUser, e.appPW)) // 新池 → 新连接，必然在全局时区改动之后
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	var v sessionVars
	if err := pool.Executor(ctx).Get(ctx, &v, db.Q("SELECT @@session.time_zone AS tz, @@session.character_set_connection AS cs, @@session.collation_connection AS co")); err != nil {
		t.Fatal(err)
	}
	if v.TZ != "+00:00" || v.CS != "utf8mb4" || v.Coll != "utf8mb4_0900_ai_ci" {
		t.Fatalf("会话变量不符: %+v", v)
	}
	// NOW() 随会话时区：必须与 UTC_TIMESTAMP 一致（全局 +08:00 不得渗入）。
	var diff int64
	if err := pool.Executor(ctx).Get(ctx, &diff, db.Q("SELECT ABS(TIMESTAMPDIFF(SECOND, NOW(), UTC_TIMESTAMP()))")); err != nil || diff > 2 {
		t.Fatalf("NOW() 必须是 UTC: diff=%d err=%v", diff, err)
	}
}

// AC1：parseTime（DATETIME → time.Time，UTC）与写入时的 UTC 归一化；utf8mb4 往返。
func TestTimeAndUnicodeRoundTrip_RealMySQL(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	ex := e.app.Executor(ctx)
	if _, err := ex.Exec(ctx, db.Q("INSERT INTO tenants (uid, name) VALUES (?, ?)"), "01J00000000000000000000TU1", "🙂 租户 ü"); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := ex.Get(ctx, &name, db.Q("SELECT name FROM tenants WHERE uid = ?"), "01J00000000000000000000TU1"); err != nil || name != "🙂 租户 ü" {
		t.Fatalf("utf8mb4 往返失败: %q %v", name, err)
	}
	if got := e.adminScalar(t, "SELECT CHAR_LENGTH(name) FROM `"+e.dbName+"`.tenants WHERE uid = ?", "01J00000000000000000000TU1"); got != "6" {
		t.Fatalf("按字符计长应为 6（4 字节字符未被截断）: %s", got)
	}

	// 非 UTC 时区的 time.Time 写入后，库里存的是 UTC 墙钟；读回为 UTC 的 time.Time。
	tid := e.seedTenantsOne(t)
	cst := time.FixedZone("CST", 8*3600)
	in := time.Date(2026, 11, 17, 20, 30, 15, 123_000_000, cst)
	if _, err := ex.Exec(ctx, db.Q("INSERT INTO sites (uid, tenant_id, name, created_at) VALUES (?, ?, ?, ?)"), "01J0000000000000000000SZ01", tid, "tz", in); err != nil {
		t.Fatal(err)
	}
	if got := e.adminScalar(t, "SELECT DATE_FORMAT(created_at, '%Y-%m-%d %H:%i:%s.%f') FROM `"+e.dbName+"`.sites WHERE uid = ?", "01J0000000000000000000SZ01"); got != "2026-11-17 12:30:15.123000" {
		t.Fatalf("库内应存 UTC 墙钟 12:30:15.123，实际 %s", got)
	}
	var out time.Time
	if err := ex.Get(ctx, &out, db.Q("SELECT created_at FROM sites WHERE uid = ?"), "01J0000000000000000000SZ01"); err != nil {
		t.Fatal(err)
	}
	if out.Location() != time.UTC || !out.Equal(in) {
		t.Fatalf("读回应为 UTC 且与写入时刻相同: %v vs %v", out, in)
	}
}

func (e *env) seedTenantsOne(t *testing.T) uint64 {
	a, _ := e.seedTenants(t)
	return a
}

// AC1：参数化——带参数的语句走服务端预处理（Com_stmt_execute 递增），恶意值只是数据。
func TestQueriesUseServerSidePreparedStatements_RealMySQL(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	count := func() string {
		var name, val string
		row := e.inst.Admin(t).QueryRowContext(ctx, "SHOW GLOBAL STATUS LIKE 'Com_stmt_execute'")
		if err := row.Scan(&name, &val); err != nil {
			t.Fatal(err)
		}
		return val
	}
	before := count() // 全局计数器：本包测试串行执行，且只断言「增加」，不断言精确增量
	var n int
	evil := "x'; DROP TABLE tenants; -- "
	if err := e.app.Executor(ctx).Get(ctx, &n, db.Q("SELECT COUNT(*) FROM tenants WHERE name = ?"), evil); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if after := count(); after == before {
		t.Fatalf("Com_stmt_execute 未增加（%s → %s）：参数应由服务端预处理语句绑定，而不是客户端插值", before, after)
	}
	if got := e.adminScalar(t, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = 'tenants'", e.dbName); got != "1" {
		t.Fatal("tenants 表不应被恶意值删除")
	}
}

// 连接池限额按角色应用；显式配置覆盖缺省。
func TestPoolLimitsApplied_RealMySQL(t *testing.T) {
	e := newEnv(t)
	if got := e.app.Stats().MaxOpenConnections; got != db.PoolDefaults(db.RoleApp).MaxOpenConns {
		t.Errorf("app 池缺省上限: %d", got)
	}
	if got := e.maint.Stats().MaxOpenConnections; got != db.PoolDefaults(db.RoleMaint).MaxOpenConns {
		t.Errorf("maint 池缺省上限: %d", got)
	}
	if got := e.migrate.Stats().MaxOpenConnections; got != db.PoolDefaults(db.RoleMigrate).MaxOpenConns {
		t.Errorf("migrate 池缺省上限: %d", got)
	}
	c := e.cfg(e.appUser, e.appPW)
	c.MaxOpenConns, c.MaxIdleConns = 3, 2
	p, err := db.OpenApp(ctxT(t), c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if got := p.Stats().MaxOpenConnections; got != 3 {
		t.Errorf("显式上限应为 3: %d", got)
	}
	for _, r := range []struct {
		p    db.Pool
		role db.Role
	}{{e.app, db.RoleApp}, {e.maint, db.RoleMaint}, {e.migrate, db.RoleMigrate}} {
		if r.p.Role() != r.role {
			t.Errorf("角色不符: %s vs %s", r.p.Role(), r.role)
		}
	}
}

// Unix 域套接字连接，以及错误口令/不存在的库被拒绝且错误里没有口令。
func TestOpenSocketAndFailures_RealMySQL(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c := db.Config{Socket: e.inst.Socket, Name: e.dbName, User: e.appUser, Password: config.NewSecret(e.appPW)}
	p, err := db.OpenApp(ctx, c)
	if err != nil {
		t.Fatalf("socket 连接: %v", err)
	}
	if err := p.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()

	wrong := e.cfg(e.appUser, "wrong-"+e.appPW)
	if _, err := db.OpenApp(ctx, wrong); err == nil || strings.Contains(err.Error(), e.appPW) || strings.Contains(err.Error(), "wrong-") {
		t.Fatalf("错误口令必须失败且错误不含口令: %v", err)
	}
	nodb := e.cfg(e.appUser, e.appPW)
	nodb.Name = "no_such_db_xyz"
	if _, err := db.OpenApp(ctx, nodb); err == nil {
		t.Fatal("不存在的库/无权限的库必须失败")
	}
}

// ---- 事务（AC1：经 context 传播，跨模块共用同一事务）----

func (e *env) tenantCount(t *testing.T) string {
	return e.adminScalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.tenants")
}

// 两个「模块」各自从 ctx 取执行器写入，共用一个事务：提交后两处写入同时可见。
func TestTxPropagatesAcrossModules_Commit(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	moduleA := func(ctx context.Context) error {
		_, err := e.app.Executor(ctx).Exec(ctx, db.Q("INSERT INTO tenants (uid, name) VALUES (?, ?)"), "01J00000000000000000000X01", "x1")
		return err
	}
	moduleB := func(ctx context.Context) error {
		// 模块 B 不另开事务：WithTx 加入 ctx 里已有的事务。
		return e.app.WithTx(ctx, func(ctx context.Context) error {
			_, err := e.app.Executor(ctx).Exec(ctx, db.Q("INSERT INTO tenants (uid, name) VALUES (?, ?)"), "01J00000000000000000000X02", "x2")
			return err
		})
	}
	err := e.app.WithTx(ctx, func(ctx context.Context) error {
		if _, ok := db.TxFromContext(ctx, e.app); !ok {
			t.Error("事务内 TxFromContext 应返回事务")
		}
		if err := moduleA(ctx); err != nil {
			return err
		}
		if err := moduleB(ctx); err != nil {
			return err
		}
		// 提交前其他连接看不到未提交的写入，事务内自己能看到。
		if got := e.tenantCount(t); got != "0" {
			t.Errorf("提交前外部连接不应看到写入: %s", got)
		}
		var n int
		if err := e.app.Executor(ctx).Get(ctx, &n, db.Q("SELECT COUNT(*) FROM tenants")); err != nil || n != 2 {
			t.Errorf("事务内应读到自己的写入: %d %v", n, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.tenantCount(t); got != "2" {
		t.Fatalf("提交后应有 2 行: %s", got)
	}
	if _, ok := db.TxFromContext(ctx, e.app); ok {
		t.Fatal("事务外的 ctx 不应携带事务")
	}
}

// 内层模块返回错误 → 整个事务回滚（含外层与其他模块已写入的数据）；加入事务的内层不会自行提交。
func TestTxRollbackOnErrorAndInnerJoinDoesNotCommit(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	boom := errors.New("boom")
	err := e.app.WithTx(ctx, func(ctx context.Context) error {
		if _, err := e.app.Executor(ctx).Exec(ctx, db.Q("INSERT INTO tenants (uid, name) VALUES (?, ?)"), "01J00000000000000000000X01", "outer"); err != nil {
			return err
		}
		inner := e.app.WithTx(ctx, func(ctx context.Context) error {
			if _, err := e.app.Executor(ctx).Exec(ctx, db.Q("INSERT INTO tenants (uid, name) VALUES (?, ?)"), "01J00000000000000000000X02", "inner"); err != nil {
				return err
			}
			return nil
		})
		if inner != nil {
			return inner
		}
		if got := e.tenantCount(t); got != "0" {
			t.Errorf("内层 WithTx 返回后不应已提交: %s", got)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("应返回原始错误: %v", err)
	}
	if got := e.tenantCount(t); got != "0" {
		t.Fatalf("回滚后不应残留任何行: %s", got)
	}
}

func TestTxRollbackOnPanicAndRepanics(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	func() {
		defer func() {
			if r := recover(); r != "kaboom" {
				t.Fatalf("panic 应原样重新抛出: %v", r)
			}
		}()
		_ = e.app.WithTx(ctx, func(ctx context.Context) error {
			if _, err := e.app.Executor(ctx).Exec(ctx, db.Q("INSERT INTO tenants (uid, name) VALUES (?, ?)"), "01J00000000000000000000X01", "p"); err != nil {
				t.Fatal(err)
			}
			panic("kaboom")
		})
	}()
	if got := e.tenantCount(t); got != "0" {
		t.Fatalf("panic 后应回滚: %s", got)
	}
	// 连接已归还且可继续使用。
	if err := e.app.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

// 不同池的事务互相独立：app 的事务绝不会被 maint 的 Repository 取到；只读事务拒绝写入。
func TestTxIsolationBetweenPoolsAndReadOnly(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	err := e.app.WithTx(ctx, func(ctx context.Context) error {
		if _, ok := db.TxFromContext(ctx, e.maint); ok {
			t.Error("maint 池不得看到 app 池的事务")
		}
		if _, ok := db.TxFromContext(ctx, e.migrate); ok {
			t.Error("migrate 池不得看到 app 池的事务")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = e.app.WithTxOptions(ctx, db.TxOptions{ReadOnly: true}, func(ctx context.Context) error {
		_, err := e.app.Executor(ctx).Exec(ctx, db.Q("INSERT INTO tenants (uid, name) VALUES (?, ?)"), "01J00000000000000000000X09", "ro")
		return err
	})
	if err == nil {
		t.Fatal("只读事务内的写入必须失败")
	}
	if got := e.tenantCount(t); got != "0" {
		t.Fatalf("%s", got)
	}
}

func TestGetReturnsErrNotFoundAndSelectEmpty(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	var name string
	if err := e.app.Executor(ctx).Get(ctx, &name, db.Q("SELECT name FROM tenants WHERE uid = ?"), "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("无行应为 ErrNotFound: %v", err)
	}
	var names []string
	if err := e.app.Executor(ctx).Select(ctx, &names, db.Q("SELECT name FROM tenants")); err != nil || len(names) != 0 {
		t.Fatalf("Select 无行应得空切片: %v %v", names, err)
	}
}

// Select 无行得到非 nil 空切片（序列化为 [] 而非 null），且不会向调用方已有内容追加。
func TestSelectReturnsNonNilEmptySliceAndResetsDest(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	var none []string
	if err := e.app.Executor(ctx).Select(ctx, &none, db.Q("SELECT name FROM tenants")); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("应为非 nil 空切片: %#v %v", none, err)
	}
	e.seedTenants(t)
	reused := []string{"stale"}
	if err := e.app.Executor(ctx).Select(ctx, &reused, db.Q("SELECT name FROM tenants ORDER BY id")); err != nil || len(reused) != 2 || reused[0] != "tenant-a" {
		t.Fatalf("不得在已有内容上追加: %v %v", reused, err)
	}
}

// 经 runtime.Goexit 退出（如 t.Fatal）的事务也必须回滚，不得悬空持锁。
func TestTxRollsBackOnGoexit(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.app.WithTx(ctx, func(ctx context.Context) error {
			if _, err := e.app.Executor(ctx).Exec(ctx, db.Q("INSERT INTO tenants (uid, name) VALUES (?, ?)"), "01J00000000000000000000G01", "g"); err != nil {
				t.Error(err)
			}
			runtime.Goexit()
			return nil
		})
	}()
	<-done
	if got := e.tenantCount(t); got != "0" {
		t.Fatalf("Goexit 后应回滚: %s", got)
	}
	// 连接未被悬空事务占用：池（上限 25）内仍可正常使用。
	if err := e.app.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}
