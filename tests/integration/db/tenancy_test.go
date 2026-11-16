package dbintegration

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/db/tenantdb"
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// 示例 Repository：与真实模块的写法一致（每个方法首个业务参数是 scope.Scope，查询只经 tenantdb 构建器）。
var (
	sitesTbl   = tenantdb.MustTable("sites", "id", "uid", "name", "status", "created_at", "deleted_at")
	domainsTbl = tenantdb.MustTable("site_domains", "id", "uid", "site_id", "hostname")
)

type siteRow struct {
	ID        uint64     `db:"id"`
	UID       string     `db:"uid"`
	Name      string     `db:"name"`
	Status    string     `db:"status"`
	CreatedAt time.Time  `db:"created_at"`
	DeletedAt *time.Time `db:"deleted_at"`
}

type siteRepo struct{ pool *db.AppPool }

func (r siteRepo) GetByUID(ctx context.Context, sc scope.Scope, uid string) (siteRow, error) {
	var s siteRow
	err := sitesTbl.Select().Where(tenantdb.Eq("uid", uid), tenantdb.IsNull("deleted_at")).Get(ctx, r.pool.Executor(ctx), sc, &s)
	return s, err
}

func (r siteRepo) GetByID(ctx context.Context, sc scope.Scope, id uint64) (siteRow, error) {
	var s siteRow
	err := sitesTbl.Select().Where(tenantdb.Eq("id", id)).Get(ctx, r.pool.Executor(ctx), sc, &s)
	return s, err
}

func (r siteRepo) List(ctx context.Context, sc scope.Scope) ([]siteRow, error) {
	var rows []siteRow
	err := sitesTbl.Select().Where(tenantdb.IsNull("deleted_at")).OrderBy("id", tenantdb.Asc).All(ctx, r.pool.Executor(ctx), sc, &rows)
	return rows, err
}

func (r siteRepo) Create(ctx context.Context, sc scope.Scope, values map[string]any) (int64, error) {
	return sitesTbl.Insert(values).Exec(ctx, r.pool.Executor(ctx), sc)
}

func (r siteRepo) RenameByUID(ctx context.Context, sc scope.Scope, uid, name string) error {
	return sitesTbl.Update(map[string]any{"name": name}).Where(tenantdb.Eq("uid", uid)).ExecOne(ctx, r.pool.Executor(ctx), sc)
}

func (r siteRepo) UpdateByID(ctx context.Context, sc scope.Scope, id uint64, status string) (int64, error) {
	return sitesTbl.Update(map[string]any{"status": status}).Where(tenantdb.Eq("id", id)).Exec(ctx, r.pool.Executor(ctx), sc)
}

func (r siteRepo) DeleteByUID(ctx context.Context, sc scope.Scope, uid string) error {
	return sitesTbl.Delete().Where(tenantdb.Eq("uid", uid)).ExecOne(ctx, r.pool.Executor(ctx), sc)
}

func (r siteRepo) DeleteByID(ctx context.Context, sc scope.Scope, id uint64) (int64, error) {
	return sitesTbl.Delete().Where(tenantdb.Eq("id", id)).Exec(ctx, r.pool.Executor(ctx), sc)
}

func (r siteRepo) Count(ctx context.Context, sc scope.Scope, conds ...tenantdb.Cond) (int64, error) {
	return sitesTbl.Select().Where(conds...).Count(ctx, r.pool.Executor(ctx), sc)
}

func (r siteRepo) ListAcrossTenants(ctx context.Context, sc scope.Scope) ([]siteRow, error) {
	var rows []siteRow
	err := sitesTbl.Select().OrderBy("id", tenantdb.Asc).AllAcrossTenants(ctx, r.pool.Executor(ctx), sc, &rows)
	return rows, err
}

func (r siteRepo) CreateDomain(ctx context.Context, sc scope.Scope, values map[string]any) (int64, error) {
	return domainsTbl.Insert(values).Exec(ctx, r.pool.Executor(ctx), sc)
}

func mustScope(t *testing.T, tenant uint64) scope.Scope {
	t.Helper()
	s, err := tenantscope.FromTask(tenant, 100)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type tenantFixture struct {
	e                *env
	repo             siteRepo
	tenantA, tenantB uint64
	a1, a2, b1, b2   siteRow
	scopeA, scopeB   scope.Scope
	bUIDs, bIDs      []any
}

func newTenantFixture(t *testing.T) *tenantFixture {
	t.Helper()
	e := newEnv(t)
	f := &tenantFixture{e: e, repo: siteRepo{e.app}}
	f.tenantA, f.tenantB = e.seedTenants(t)
	f.scopeA, f.scopeB = mustScope(t, f.tenantA), mustScope(t, f.tenantB)
	ctx := ctxT(t)
	mk := func(sc scope.Scope, uid, name string) siteRow {
		if _, err := f.repo.Create(ctx, sc, map[string]any{"uid": uid, "name": name}); err != nil {
			t.Fatalf("创建 %s: %v", name, err)
		}
		r, err := f.repo.GetByUID(ctx, sc, uid)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	f.a1 = mk(f.scopeA, "01J0000000000000000000SA01", "a-site-1")
	f.a2 = mk(f.scopeA, "01J0000000000000000000SA02", "a-site-2")
	f.b1 = mk(f.scopeB, "01J0000000000000000000SB01", "b-site-1")
	f.b2 = mk(f.scopeB, "01J0000000000000000000SB02", "b-site-2")
	f.bUIDs, f.bIDs = []any{f.b1.UID, f.b2.UID}, []any{f.b1.ID, f.b2.ID}
	return f
}

// 以 root 核对租户 B 的数据没有被租户 A 的任何越权操作改动。
func (f *tenantFixture) assertBUntouched(t *testing.T) {
	t.Helper()
	q := "SELECT CONCAT(COUNT(*), '|', GROUP_CONCAT(name ORDER BY id), '|', GROUP_CONCAT(status ORDER BY id), '|', COALESCE(SUM(deleted_at IS NOT NULL),0)) FROM `" + f.e.dbName + "`.sites WHERE tenant_id = ?"
	if got, want := f.e.adminScalar(t, q, f.tenantB), "2|b-site-1,b-site-2|active,active|0"; got != want {
		t.Fatalf("租户 B 的数据被改动: got %q want %q", got, want)
	}
}

// AC4：以租户 A 的 Scope 读写租户 B 的行 → 0 行受影响/未找到（表驱动：UPDATE / DELETE / 按 uid 查询 / 按 id 查询 / 计数 / 列表）。
func TestCrossTenantAccessIsDenied_RealMySQL(t *testing.T) {
	f := newTenantFixture(t)
	ctx := ctxT(t)
	r, sa := f.repo, f.scopeA

	type result struct {
		rows int64 // 受影响/返回的行数
		err  error
	}
	// got 把「读到了一行」记为 1 行（读到他租户的行即越权）。
	got := func(_ siteRow, err error) result {
		if err != nil {
			return result{err: err}
		}
		return result{rows: 1}
	}
	ops := []struct {
		name string
		run  func() result
	}{
		{"按 uid 查询他租户的行", func() result { return got(r.GetByUID(ctx, sa, f.b1.UID)) }},
		{"按 id 查询他租户的行", func() result { return got(r.GetByID(ctx, sa, f.b1.ID)) }},
		{"UPDATE（按 uid，ExecOne）他租户的行", func() result { return result{err: r.RenameByUID(ctx, sa, f.b1.UID, "pwned")} }},
		{"UPDATE（按 id）他租户的行", func() result { n, err := r.UpdateByID(ctx, sa, f.b1.ID, "suspended"); return result{rows: n, err: err} }},
		{"DELETE（按 uid，ExecOne）他租户的行", func() result { return result{err: r.DeleteByUID(ctx, sa, f.b1.UID)} }},
		{"DELETE（按 id）他租户的行", func() result { n, err := r.DeleteByID(ctx, sa, f.b2.ID); return result{rows: n, err: err} }},
		{"Count 他租户的 uid", func() result {
			n, err := r.Count(ctx, sa, tenantdb.Eq("uid", f.b1.UID))
			return result{rows: n, err: err}
		}},
		{"Count 他租户的 id 列表（IN）", func() result {
			n, err := r.Count(ctx, sa, tenantdb.In("id", f.bIDs...))
			return result{rows: n, err: err}
		}},
		{"Or 条件试图把他租户的行并入结果", func() result {
			n, err := r.Count(ctx, sa, tenantdb.Or(tenantdb.Eq("uid", f.b1.UID), tenantdb.Eq("uid", f.b2.UID)))
			return result{rows: n, err: err}
		}},
		{"Not 条件试图取反出他租户的行", func() result {
			n, err := r.Count(ctx, sa, tenantdb.Not(tenantdb.In("id", f.a1.ID, f.a2.ID)))
			return result{rows: n, err: err}
		}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			res := op.run()
			switch {
			case res.err != nil && !errors.Is(res.err, db.ErrNotFound):
				t.Fatalf("越权应表现为「未找到」，得到其他错误: %v", res.err)
			case res.err == nil && res.rows != 0:
				t.Fatalf("越权必须 0 行受影响/返回，实际 %d", res.rows)
			}
			f.assertBUntouched(t)
		})
	}

	// 列表只含本租户的行。
	rows, err := r.List(ctx, sa)
	if err != nil || len(rows) != 2 || rows[0].UID != f.a1.UID || rows[1].UID != f.a2.UID {
		t.Fatalf("List 只应返回租户 A 的行: %+v %v", rows, err)
	}

	// 反向对照：租户 B 自己的 Scope 能读写同一批行，证明上面的「未找到」来自租户条件而不是数据不存在。
	if got, err := r.GetByUID(ctx, f.scopeB, f.b1.UID); err != nil || got.Name != "b-site-1" {
		t.Fatalf("B 读自己的行: %+v %v", got, err)
	}
	if err := r.RenameByUID(ctx, f.scopeB, f.b1.UID, "b-renamed"); err != nil {
		t.Fatalf("B 改自己的行: %v", err)
	}
	if n, err := r.UpdateByID(ctx, f.scopeB, f.b2.ID, "paused"); err != nil || n != 1 {
		t.Fatalf("B 按 id 改自己的行: %d %v", n, err)
	}
	// 同值 UPDATE 仍按「匹配行」计 1（clientFoundRows），否则「0 行」无法区分「越权」与「值未变」。
	if n, err := r.UpdateByID(ctx, f.scopeB, f.b2.ID, "paused"); err != nil || n != 1 {
		t.Fatalf("同值 UPDATE 应计为匹配 1 行: %d %v", n, err)
	}
	if err := r.DeleteByUID(ctx, f.scopeB, f.b1.UID); err != nil {
		t.Fatalf("B 删自己的行: %v", err)
	}
	// 租户 A 的数据同样不受 B 的操作影响。
	if rows, err := r.List(ctx, sa); err != nil || len(rows) != 2 {
		t.Fatalf("A 的数据不应受影响: %+v %v", rows, err)
	}
}

// 被软删除的行对本租户也不可见；越权访问软删除行同样「未找到」。
func TestSoftDeletedRowsAreFilteredByRepository(t *testing.T) {
	f := newTenantFixture(t)
	ctx := ctxT(t)
	f.e.inst.Exec(t, "UPDATE `"+f.e.dbName+"`.sites SET deleted_at = UTC_TIMESTAMP(3) WHERE id = ?", f.a1.ID)
	if _, err := f.repo.GetByUID(ctx, f.scopeA, f.a1.UID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("软删除的行应被过滤: %v", err)
	}
	if rows, _ := f.repo.List(ctx, f.scopeA); len(rows) != 1 {
		t.Fatalf("List 应过滤软删除行: %d", len(rows))
	}
}

// AC3：INSERT 强制写入 scope.TenantID，请求体里的 tenant_id 被忽略。
func TestInsertIgnoresTenantIDFromRequestBody_RealMySQL(t *testing.T) {
	f := newTenantFixture(t)
	ctx := ctxT(t)
	id, err := f.repo.Create(ctx, f.scopeA, map[string]any{"uid": "01J0000000000000000000SA03", "name": "forged", "tenant_id": f.tenantB})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.e.adminScalar(t, "SELECT tenant_id FROM `"+f.e.dbName+"`.sites WHERE id = ?", id); got != strconv.FormatUint(f.tenantA, 10) {
		t.Fatalf("行必须归属租户 A(%d)，实际 %s（请求体的 tenant_id 不得生效）", f.tenantA, got)
	}
	if _, err := f.repo.GetByUID(ctx, f.scopeB, "01J0000000000000000000SA03"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("租户 B 不得看到该行: %v", err)
	}
}

// AC3：Platform 范围只能经 AcrossTenants；零值 Scope 一律失败，且失败时数据未被改动。
func TestPlatformAndZeroScopeRules_RealMySQL(t *testing.T) {
	f := newTenantFixture(t)
	ctx := ctxT(t)
	platform := tenantscope.PlatformForJob(1)

	if _, err := f.repo.GetByUID(ctx, platform, f.a1.UID); !errors.Is(err, tenantdb.ErrPlatformScope) {
		t.Fatalf("平台范围用于普通方法必须失败: %v", err)
	}
	if err := f.repo.DeleteByUID(ctx, platform, f.a1.UID); !errors.Is(err, tenantdb.ErrPlatformScope) {
		t.Fatalf("平台范围不得经普通 DELETE 跨租户删除: %v", err)
	}
	if _, err := f.repo.Create(ctx, platform, map[string]any{"uid": "01J0000000000000000000SP01", "name": "x"}); !errors.Is(err, tenantdb.ErrNoTenant) {
		t.Fatalf("平台范围不得 INSERT: %v", err)
	}
	var zero scope.Scope
	if _, err := f.repo.List(ctx, zero); !errors.Is(err, tenantdb.ErrNoScope) {
		t.Fatalf("零值 Scope 必须失败: %v", err)
	}
	if err := f.repo.DeleteByUID(ctx, zero, f.a1.UID); !errors.Is(err, tenantdb.ErrNoScope) {
		t.Fatalf("零值 Scope 的 DELETE 必须失败: %v", err)
	}
	// AcrossTenants：平台范围可见全部租户的行；租户范围调用则失败。
	rows, err := f.repo.ListAcrossTenants(ctx, platform)
	if err != nil || len(rows) != 4 {
		t.Fatalf("AcrossTenants 应返回 4 行: %d %v", len(rows), err)
	}
	if _, err := f.repo.ListAcrossTenants(ctx, f.scopeA); !errors.Is(err, tenantdb.ErrNotPlatform) {
		t.Fatalf("租户范围调用 AcrossTenants 必须失败: %v", err)
	}
	if got := f.e.adminScalar(t, "SELECT COUNT(*) FROM `"+f.e.dbName+"`.sites"); got != "4" {
		t.Fatalf("失败的调用不得改动数据，行数 %s", got)
	}
}

// 管理员代租户（ActingAs）仍被限定到目标租户。
func TestActingAsIsStillTenantLimited_RealMySQL(t *testing.T) {
	f := newTenantFixture(t)
	ctx := ctxT(t)
	sc, err := tenantscope.FromPrincipal(adminPrincipal{}, tenantscope.RouteTenant, tenantscope.TargetTenant(f.tenantA))
	if err != nil || !sc.ActingAs() {
		t.Fatalf("FromPrincipal: %v %v", sc, err)
	}
	rows, err := f.repo.List(ctx, sc)
	if err != nil || len(rows) != 2 {
		t.Fatalf("代租户 A 只应看到 A 的 2 行: %d %v", len(rows), err)
	}
	if _, err := f.repo.GetByUID(ctx, sc, f.b1.UID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("代租户 A 不得读到 B 的行: %v", err)
	}
}

type adminPrincipal struct{}

func (adminPrincipal) ActorID() uint64             { return 1 }
func (adminPrincipal) IsPlatform() bool            { return true }
func (adminPrincipal) TenantID() uint64            { return 0 }
func (adminPrincipal) HasPermission(p string) bool { return p == tenantscope.PermImpersonate }

// 恶意值全部作为参数传递：不会改变语义、不会删表。
func TestHostileValuesAreInertParameters_RealMySQL(t *testing.T) {
	f := newTenantFixture(t)
	ctx := ctxT(t)
	for _, evil := range []string{
		"x' OR '1'='1", "'; DROP TABLE sites; --", "\\' OR 1=1 #", "01J0000000000000000000SB01' OR tenant_id <> 0 -- ", "%", "_",
	} {
		if _, err := f.repo.GetByUID(ctx, f.scopeA, evil); !errors.Is(err, db.ErrNotFound) {
			t.Errorf("恶意 uid %q 应当找不到: %v", evil, err)
		}
		if n, err := f.repo.Count(ctx, f.scopeA, tenantdb.Eq("name", evil)); err != nil || n != 0 {
			t.Errorf("恶意 name %q: %d %v", evil, n, err)
		}
	}
	// 字面量 LIKE 通配符不会越过租户边界：% 匹配全部本租户行，但不含他租户。
	if n, err := f.repo.Count(ctx, f.scopeA, tenantdb.Like("name", "%")); err != nil || n != 2 {
		t.Errorf("LIKE '%%' 应只匹配本租户 2 行: %d %v", n, err)
	}
	// 恶意值可原样存入并读回。
	evil := "'; DROP TABLE sites; --"
	if _, err := f.repo.Create(ctx, f.scopeA, map[string]any{"uid": "01J0000000000000000000SA09", "name": evil}); err != nil {
		t.Fatal(err)
	}
	got, err := f.repo.GetByUID(ctx, f.scopeA, "01J0000000000000000000SA09")
	if err != nil || got.Name != evil {
		t.Fatalf("恶意值应被原样存取: %+v %v", got, err)
	}
	if got := f.e.adminScalar(t, "SELECT COUNT(*) FROM `"+f.e.dbName+"`.sites"); got != "5" {
		t.Fatalf("表应仍在且行数为 5: %s", got)
	}
}

// 纵深防御（ADR-009 §3）：即便 Repository 被误用，复合外键也使「租户 A 的子行指向租户 B 的站点」在库层不可能（ERROR 1452）。
func TestCompositeForeignKeyBlocksCrossTenantReference_RealMySQL(t *testing.T) {
	f := newTenantFixture(t)
	ctx := ctxT(t)
	_, err := f.repo.CreateDomain(ctx, f.scopeA, map[string]any{"uid": "01J0000000000000000000DA01", "site_id": f.b1.ID, "hostname": "evil.example.com"})
	if n, ok := db.MySQLErrorNumber(err); !ok || n != db.MySQLErrNoReferencedRow {
		t.Fatalf("跨租户引用必须被复合外键拒绝（1452），实际: %v", err)
	}
	if _, err := f.repo.CreateDomain(ctx, f.scopeA, map[string]any{"uid": "01J0000000000000000000DA02", "site_id": f.a1.ID, "hostname": "ok.example.com"}); err != nil {
		t.Fatalf("同租户引用应成功: %v", err)
	}
	// 唯一键冲突可被识别（Repository 外无需 import 驱动）。
	_, err = f.repo.CreateDomain(ctx, f.scopeA, map[string]any{"uid": "01J0000000000000000000DA03", "site_id": f.a1.ID, "hostname": "ok.example.com"})
	if !db.IsDuplicate(err) {
		t.Fatalf("重复主机名应为唯一键冲突: %v", err)
	}
}

// ExecOne 带 LIMIT 1：即便条件（误）命中多行，也至多改动/删除一行，不会「先写入、后报错」。
func TestExecOneAffectsAtMostOneRow_RealMySQL(t *testing.T) {
	f := newTenantFixture(t)
	ctx := ctxT(t)
	// 两行同名（name 无唯一约束）：按 name 条件命中 2 行。
	if _, err := f.repo.Create(ctx, f.scopeA, map[string]any{"uid": "01J0000000000000000000SA05", "name": "dup"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Create(ctx, f.scopeA, map[string]any{"uid": "01J0000000000000000000SA06", "name": "dup"}); err != nil {
		t.Fatal(err)
	}
	ex := f.repo.pool.Executor(ctx)
	if err := sitesTbl.Update(map[string]any{"status": "paused"}).Where(tenantdb.Eq("name", "dup")).ExecOne(ctx, ex, f.scopeA); err != nil {
		t.Fatal(err)
	}
	if got := f.e.adminScalar(t, "SELECT COUNT(*) FROM `"+f.e.dbName+"`.sites WHERE name = 'dup' AND status = 'paused'"); got != "1" {
		t.Fatalf("ExecOne(UPDATE) 应只改 1 行，实际 %s", got)
	}
	if err := sitesTbl.Delete().Where(tenantdb.Eq("name", "dup")).ExecOne(ctx, ex, f.scopeA); err != nil {
		t.Fatal(err)
	}
	if got := f.e.adminScalar(t, "SELECT COUNT(*) FROM `"+f.e.dbName+"`.sites WHERE name = 'dup'"); got != "1" {
		t.Fatalf("ExecOne(DELETE) 应只删 1 行，剩余应为 1，实际 %s", got)
	}
}

// FOR UPDATE SKIP LOCKED：事务内可用且并发领取不重复；自动提交下被拒绝。
func TestForUpdateSkipLockedNeedsTxAndSkipsLockedRows_RealMySQL(t *testing.T) {
	f := newTenantFixture(t)
	ctx := ctxT(t)
	claim := sitesTbl.Select("id").Where(tenantdb.Eq("status", "active")).OrderBy("id", tenantdb.Asc).Limit(1).ForUpdateSkipLocked()
	var rows []siteRow
	if err := claim.All(ctx, f.e.app.Executor(ctx), f.scopeA, &rows); !errors.Is(err, tenantdb.ErrTxRequired) {
		t.Fatalf("自动提交下必须拒绝: %v", err)
	}
	var first, second []siteRow
	err := f.e.app.WithTx(ctx, func(ctx context.Context) error {
		if err := claim.All(ctx, f.e.app.Executor(ctx), f.scopeA, &first); err != nil {
			return err
		}
		// 另一个独立事务（全新 context，另一连接）在第一个事务持锁期间领取：应跳过已锁行，拿到另一行。
		other := ctxT(t)
		return f.e.app.WithTx(other, func(other context.Context) error {
			return claim.All(other, f.e.app.Executor(other), f.scopeA, &second)
		})
	})
	if err != nil || len(first) != 1 || len(second) != 1 {
		t.Fatalf("两次领取各应得到 1 行: %v %v %v", first, second, err)
	}
	if first[0].ID == second[0].ID {
		t.Fatalf("SKIP LOCKED 下两个事务不得领取到同一行: %d", first[0].ID)
	}
}
