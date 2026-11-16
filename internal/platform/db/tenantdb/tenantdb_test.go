package tenantdb_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/db/tenantdb"
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

var sites = tenantdb.MustTable("sites", "id", "uid", "name", "status", "deleted_at")

func tenantScope(t *testing.T, id uint64) scope.Scope {
	t.Helper()
	s, err := tenantscope.FromTask(id, 1)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func platformScope() scope.Scope { return tenantscope.PlatformForJob(1) }

func TestNewTableValidation(t *testing.T) {
	cases := []struct {
		name string
		tbl  string
		cols []string
		want error
	}{
		{"合法", "sites", []string{"id", "uid"}, nil},
		{"表名含大写", "Sites", []string{"id"}, tenantdb.ErrInvalidIdentifier},
		{"表名含反引号", "si`tes", []string{"id"}, tenantdb.ErrInvalidIdentifier},
		{"表名含空格与分号", "sites; drop", []string{"id"}, tenantdb.ErrInvalidIdentifier},
		{"列名非法", "sites", []string{"id", "na me"}, tenantdb.ErrInvalidIdentifier},
		{"声明 tenant_id", "sites", []string{"id", "tenant_id"}, tenantdb.ErrReservedColumn},
		{"无列", "sites", nil, tenantdb.ErrNoValues},
		{"重复列", "sites", []string{"id", "id"}, tenantdb.ErrInvalidIdentifier},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := tenantdb.NewTable(c.tbl, c.cols...)
			if (c.want == nil) != (err == nil) || (c.want != nil && !errors.Is(err, c.want)) {
				t.Fatalf("期望 %v，实际 %v", c.want, err)
			}
		})
	}
}

func TestSelectAddsTenantCondition(t *testing.T) {
	ten := tenantScope(t, 7)
	cases := []struct {
		name     string
		b        tenantdb.SelectBuilder
		wantSQL  string
		wantArgs []any
	}{
		{"全列、无条件仍带租户", sites.Select(),
			"SELECT `id`, `uid`, `name`, `status`, `deleted_at` FROM `sites` WHERE `tenant_id` = ?", []any{uint64(7)}},
		{"按 uid", sites.Select("id", "name").Where(tenantdb.Eq("uid", "01ABC")),
			"SELECT `id`, `name` FROM `sites` WHERE `tenant_id` = ? AND `uid` = ?", []any{uint64(7), "01ABC"}},
		{"Or 被括号包裹，无法逃出租户条件", sites.Select("id").Where(tenantdb.Or(tenantdb.Eq("uid", "a"), tenantdb.Eq("uid", "b"))),
			"SELECT `id` FROM `sites` WHERE `tenant_id` = ? AND (`uid` = ? OR `uid` = ?)", []any{uint64(7), "a", "b"}},
		{"软删除过滤 + 排序 + 分页", sites.Select("id").Where(tenantdb.IsNull("deleted_at")).OrderBy("id", tenantdb.Desc).Limit(20).Offset(40),
			"SELECT `id` FROM `sites` WHERE `tenant_id` = ? AND `deleted_at` IS NULL ORDER BY `id` DESC LIMIT ? OFFSET ?", []any{uint64(7), 20, 40}},
		{"IN", sites.Select("id").Where(tenantdb.In("status", "active", "paused")),
			"SELECT `id` FROM `sites` WHERE `tenant_id` = ? AND `status` IN (?, ?)", []any{uint64(7), "active", "paused"}},
		{"空 IN 恒假", sites.Select("id").Where(tenantdb.In("status")),
			"SELECT `id` FROM `sites` WHERE `tenant_id` = ? AND 1 = 0", []any{uint64(7)}},
		{"Not/And/Like", sites.Select("id").Where(tenantdb.Not(tenantdb.And(tenantdb.Like("name", "a%"), tenantdb.Ne("status", "x")))),
			"SELECT `id` FROM `sites` WHERE `tenant_id` = ? AND (NOT (`name` LIKE ? AND `status` <> ?))", []any{uint64(7), "a%", "x"}},
		{"可读 tenant_id 列", sites.Select("tenant_id", "id"),
			"SELECT `tenant_id`, `id` FROM `sites` WHERE `tenant_id` = ?", []any{uint64(7)}},
		{"SKIP LOCKED", sites.Select("id").Limit(1).ForUpdateSkipLocked(),
			"SELECT `id` FROM `sites` WHERE `tenant_id` = ? LIMIT ? FOR UPDATE SKIP LOCKED", []any{uint64(7), 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, args, err := c.b.Build(ten)
			if err != nil {
				t.Fatal(err)
			}
			if q.String() != c.wantSQL {
				t.Errorf("SQL 不符:\n got  %s\n want %s", q, c.wantSQL)
			}
			if !reflect.DeepEqual(args, c.wantArgs) {
				t.Errorf("参数不符: %#v 期望 %#v", args, c.wantArgs)
			}
		})
	}
}

// 注入面：值只出现在参数里，标识符来自白名单；恶意值不会进入 SQL 文本。
func TestHostileValuesNeverReachSQLText(t *testing.T) {
	evil := "x' OR '1'='1'; DROP TABLE sites; --"
	q, args, err := sites.Select("id").Where(tenantdb.Eq("uid", evil), tenantdb.In("status", evil, evil), tenantdb.Like("name", evil)).Build(tenantScope(t, 7))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q.String(), "DROP") || strings.Contains(q.String(), "OR '1'") {
		t.Fatalf("值泄入了 SQL 文本: %s", q)
	}
	if n := strings.Count(q.String(), "?"); n != len(args) {
		t.Fatalf("占位符数 %d 与参数数 %d 不一致", n, len(args))
	}
	for _, col := range []string{"id`; DROP TABLE x; --", "ID", "tenant_id", "nope", "uid = uid OR 1"} {
		if _, _, err := sites.Select("id").Where(tenantdb.Eq(col, 1)).Build(tenantScope(t, 7)); err == nil {
			t.Errorf("非法/未知/保留列 %q 必须被拒绝", col)
		}
	}
	if _, _, err := sites.Select("name; DROP").Build(tenantScope(t, 7)); !errors.Is(err, tenantdb.ErrUnknownColumn) {
		t.Errorf("SELECT 列表中的未知列必须被拒绝: %v", err)
	}
	if _, _, err := sites.Select("id").OrderBy("id; DROP", tenantdb.Asc).Build(tenantScope(t, 7)); !errors.Is(err, tenantdb.ErrUnknownColumn) {
		t.Errorf("ORDER BY 未知列必须被拒绝: %v", err)
	}
}

func TestConditionErrors(t *testing.T) {
	ten := tenantScope(t, 7)
	cases := []struct {
		name string
		c    tenantdb.Cond
		want error
	}{
		{"Eq nil", tenantdb.Eq("uid", nil), tenantdb.ErrNilValue},
		{"IN 含 nil", tenantdb.In("uid", "a", nil), tenantdb.ErrNilValue},
		{"空 And", tenantdb.And(), tenantdb.ErrEmptyCondition},
		{"空 Or", tenantdb.Or(), tenantdb.ErrEmptyCondition},
		{"嵌套中的未知列", tenantdb.Or(tenantdb.Eq("uid", "a"), tenantdb.Eq("zzz", "b")), tenantdb.ErrUnknownColumn},
		{"嵌套中的 tenant_id", tenantdb.And(tenantdb.Eq("tenant_id", 9)), tenantdb.ErrReservedColumn},
	}
	for _, c := range cases {
		if _, _, err := sites.Select("id").Where(c.c).Build(ten); !errors.Is(err, c.want) {
			t.Errorf("%s: 期望 %v，实际 %v", c.name, c.want, err)
		}
	}
	if _, _, err := sites.Select("id").Limit(-1).Build(ten); !errors.Is(err, tenantdb.ErrInvalidArgument) {
		t.Errorf("负 LIMIT: %v", err)
	}
	if _, _, err := sites.Select("id").Offset(3).Build(ten); !errors.Is(err, tenantdb.ErrInvalidArgument) {
		t.Errorf("OFFSET 无 LIMIT: %v", err)
	}
}

func TestUpdateDeleteAddTenantCondition(t *testing.T) {
	ten := tenantScope(t, 7)
	q, args, err := sites.Update(map[string]any{"name": "n", "status": "paused"}).Where(tenantdb.Eq("uid", "u1")).Build(ten)
	if err != nil {
		t.Fatal(err)
	}
	if want := "UPDATE `sites` SET `name` = ?, `status` = ? WHERE `tenant_id` = ? AND `uid` = ?"; q.String() != want {
		t.Errorf("UPDATE:\n got  %s\n want %s", q, want)
	}
	if !reflect.DeepEqual(args, []any{"n", "paused", uint64(7), "u1"}) {
		t.Errorf("UPDATE 参数: %#v", args)
	}
	q, args, err = sites.Delete().Where(tenantdb.Eq("id", 5)).Build(ten)
	if err != nil {
		t.Fatal(err)
	}
	if want := "DELETE FROM `sites` WHERE `tenant_id` = ? AND `id` = ?"; q.String() != want {
		t.Errorf("DELETE:\n got  %s\n want %s", q, want)
	}
	if !reflect.DeepEqual(args, []any{uint64(7), 5}) {
		t.Errorf("DELETE 参数: %#v", args)
	}

	// 无条件的 UPDATE/DELETE（整租户误改误删）被拒绝。
	if _, _, err := sites.Update(map[string]any{"name": "n"}).Build(ten); !errors.Is(err, tenantdb.ErrNoCondition) {
		t.Errorf("无条件 UPDATE: %v", err)
	}
	if _, _, err := sites.Delete().Build(ten); !errors.Is(err, tenantdb.ErrNoCondition) {
		t.Errorf("无条件 DELETE: %v", err)
	}
	// 不能把行移到别的租户；未知列；空 SET。
	if _, _, err := sites.Update(map[string]any{"tenant_id": 9}).Where(tenantdb.Eq("id", 1)).Build(ten); !errors.Is(err, tenantdb.ErrReservedColumn) {
		t.Errorf("UPDATE SET tenant_id: %v", err)
	}
	if _, _, err := sites.Update(map[string]any{"zzz": 9}).Where(tenantdb.Eq("id", 1)).Build(ten); !errors.Is(err, tenantdb.ErrUnknownColumn) {
		t.Errorf("UPDATE 未知列: %v", err)
	}
	if _, _, err := sites.Update(nil).Where(tenantdb.Eq("id", 1)).Build(ten); !errors.Is(err, tenantdb.ErrNoValues) {
		t.Errorf("UPDATE 空 SET: %v", err)
	}
}

// AC3：INSERT 强制写入 scope.TenantID，忽略请求体里的 tenant_id。
func TestInsertForcesScopeTenantAndIgnoresBodyTenant(t *testing.T) {
	q, args, err := sites.Insert(map[string]any{"uid": "u1", "name": "n", "tenant_id": uint64(999)}).Build(tenantScope(t, 7))
	if err != nil {
		t.Fatal(err)
	}
	if want := "INSERT INTO `sites` (`tenant_id`, `name`, `uid`) VALUES (?, ?, ?)"; q.String() != want {
		t.Errorf("INSERT:\n got  %s\n want %s", q, want)
	}
	if !reflect.DeepEqual(args, []any{uint64(7), "n", "u1"}) {
		t.Errorf("INSERT 参数（tenant 必须是 scope 的 7，不是请求体的 999）: %#v", args)
	}
	for _, a := range args {
		if a == uint64(999) {
			t.Fatal("请求体的 tenant_id 不得进入参数")
		}
	}
	if _, _, err := sites.Insert(map[string]any{"tenant_id": 1}).Build(tenantScope(t, 7)); !errors.Is(err, tenantdb.ErrNoValues) {
		t.Errorf("只有 tenant_id 等于没有可写列: %v", err)
	}
	if _, _, err := sites.Insert(map[string]any{"zzz": 1}).Build(tenantScope(t, 7)); !errors.Is(err, tenantdb.ErrUnknownColumn) {
		t.Errorf("未知列: %v", err)
	}
	// 平台范围不能 INSERT（没有租户可写）。
	if _, _, err := sites.Insert(map[string]any{"name": "n"}).Build(platformScope()); !errors.Is(err, tenantdb.ErrNoTenant) {
		t.Errorf("平台范围 INSERT: %v", err)
	}
}

// AC3：Platform=true 只能经 AcrossTenants；零值 Scope 一律失败。
func TestScopeGuards(t *testing.T) {
	zero := scope.Scope{}
	plat := platformScope()
	ten := tenantScope(t, 7)

	for name, fn := range map[string]func(scope.Scope) error{
		"Select": func(s scope.Scope) error { _, _, err := sites.Select().Build(s); return err },
		"Update": func(s scope.Scope) error {
			_, _, err := sites.Update(map[string]any{"name": "n"}).Where(tenantdb.Eq("id", 1)).Build(s)
			return err
		},
		"Delete": func(s scope.Scope) error {
			_, _, err := sites.Delete().Where(tenantdb.Eq("id", 1)).Build(s)
			return err
		},
		"Insert": func(s scope.Scope) error { _, _, err := sites.Insert(map[string]any{"name": "n"}).Build(s); return err },
	} {
		if err := fn(zero); !errors.Is(err, tenantdb.ErrNoScope) {
			t.Errorf("%s: 零值 Scope 必须 ErrNoScope，实际 %v", name, err)
		}
		if name != "Insert" {
			if err := fn(plat); !errors.Is(err, tenantdb.ErrPlatformScope) {
				t.Errorf("%s: 平台范围用于普通方法必须 ErrPlatformScope，实际 %v", name, err)
			}
		}
		if err := fn(ten); err != nil {
			t.Errorf("%s: 租户范围应通过: %v", name, err)
		}
	}

	// AcrossTenants：要求平台范围，且不追加 tenant_id。
	q, args, err := sites.Select("id").Where(tenantdb.Eq("status", "active")).BuildAcrossTenants(plat)
	if err != nil || strings.Contains(q.String(), "tenant_id") || len(args) != 1 {
		t.Errorf("AcrossTenants 查询: %s %v %v", q, args, err)
	}
	if _, _, err := sites.Select("id").BuildAcrossTenants(ten); !errors.Is(err, tenantdb.ErrNotPlatform) {
		t.Errorf("租户 Scope 调 AcrossTenants 必须 ErrNotPlatform: %v", err)
	}
	if _, _, err := sites.Select("id").BuildAcrossTenants(zero); !errors.Is(err, tenantdb.ErrNoScope) {
		t.Errorf("零值 Scope 调 AcrossTenants 必须 ErrNoScope: %v", err)
	}
	if _, _, err := sites.Update(map[string]any{"name": "n"}).Where(tenantdb.Eq("id", 1)).BuildAcrossTenants(ten); !errors.Is(err, tenantdb.ErrNotPlatform) {
		t.Errorf("UpdateAcrossTenants 非平台: %v", err)
	}
	if _, _, err := sites.Delete().Where(tenantdb.Eq("id", 1)).BuildAcrossTenants(ten); !errors.Is(err, tenantdb.ErrNotPlatform) {
		t.Errorf("DeleteAcrossTenants 非平台: %v", err)
	}
}

// failingExec 在被调用时让测试失败：Scope 无效时不得向数据库发出任何语句。
type failingExec struct{ t *testing.T }

func (f failingExec) Exec(context.Context, db.Query, ...any) (db.Result, error) {
	f.t.Error("Scope 无效时不得执行任何语句（Exec）")
	return nil, errors.New("unexpected")
}
func (f failingExec) Get(context.Context, any, db.Query, ...any) error {
	f.t.Error("Scope 无效时不得执行任何语句（Get）")
	return errors.New("unexpected")
}
func (f failingExec) InTx() bool { return true }
func (f failingExec) Select(context.Context, any, db.Query, ...any) error {
	f.t.Error("Scope 无效时不得执行任何语句（Select）")
	return errors.New("unexpected")
}

func TestInvalidScopeNeverReachesExecutor(t *testing.T) {
	ex := failingExec{t}
	ctx := context.Background()
	zero := scope.Scope{}
	var dest struct{}
	var rows []struct{}
	if err := sites.Select().Get(ctx, ex, zero, &dest); !errors.Is(err, tenantdb.ErrNoScope) {
		t.Error(err)
	}
	if err := sites.Select().All(ctx, ex, platformScope(), &rows); !errors.Is(err, tenantdb.ErrPlatformScope) {
		t.Error(err)
	}
	if _, err := sites.Select().Count(ctx, ex, zero); !errors.Is(err, tenantdb.ErrNoScope) {
		t.Error(err)
	}
	if _, err := sites.Insert(map[string]any{"name": "n"}).Exec(ctx, ex, zero); !errors.Is(err, tenantdb.ErrNoScope) {
		t.Error(err)
	}
	if _, err := sites.Update(map[string]any{"name": "n"}).Where(tenantdb.Eq("id", 1)).Exec(ctx, ex, platformScope()); !errors.Is(err, tenantdb.ErrPlatformScope) {
		t.Error(err)
	}
	if err := sites.Delete().Where(tenantdb.Eq("id", 1)).ExecOne(ctx, ex, zero); !errors.Is(err, tenantdb.ErrNoScope) {
		t.Error(err)
	}
	tenant, _ := tenantscope.FromTask(3, 1)
	if err := sites.Select().GetAcrossTenants(ctx, ex, tenant, &dest); !errors.Is(err, tenantdb.ErrNotPlatform) {
		t.Error(err)
	}
	if _, err := sites.Select().CountAcrossTenants(ctx, ex, tenant); !errors.Is(err, tenantdb.ErrNotPlatform) {
		t.Error(err)
	}
	if _, err := sites.Delete().Where(tenantdb.Eq("id", 1)).ExecAcrossTenants(ctx, ex, tenant); !errors.Is(err, tenantdb.ErrNotPlatform) {
		t.Error(err)
	}
}

func TestBuilderValueSemantics(t *testing.T) {
	base := sites.Select("id")
	a := base.Where(tenantdb.Eq("uid", "a"))
	b := base.Where(tenantdb.Eq("uid", "b"))
	qa, _, _ := a.Build(tenantScope(t, 1))
	qb, _, _ := b.Build(tenantScope(t, 1))
	qbase, _, _ := base.Build(tenantScope(t, 1))
	if qa.String() != qb.String() { // 文本相同，参数不同
		t.Fatal("两个分支的 SQL 文本应相同")
	}
	_, argsA, _ := a.Build(tenantScope(t, 1))
	_, argsB, _ := b.Build(tenantScope(t, 1))
	if argsA[1] != "a" || argsB[1] != "b" {
		t.Fatalf("分支互相污染: %v %v", argsA, argsB)
	}
	if strings.Contains(qbase.String(), "uid") {
		t.Fatalf("基础构建器被分支修改: %s", qbase)
	}
}

// recordingExec 记录执行的语句（有效 Scope 下验证执行包装器确实注入/省略租户条件）；返回预设的受影响行数。
type recordingExec struct {
	inTx     bool
	affected int64
	queries  []string
	args     [][]any
}

type fakeResult struct{ n int64 }

func (r fakeResult) LastInsertId() (int64, error) { return 1, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.n, nil }

func (r *recordingExec) rec(q db.Query, args []any) {
	r.queries = append(r.queries, q.String())
	r.args = append(r.args, args)
}
func (r *recordingExec) Exec(_ context.Context, q db.Query, args ...any) (db.Result, error) {
	r.rec(q, args)
	return fakeResult{r.affected}, nil
}
func (r *recordingExec) Get(_ context.Context, _ any, q db.Query, args ...any) error {
	r.rec(q, args)
	return nil
}
func (r *recordingExec) Select(_ context.Context, _ any, q db.Query, args ...any) error {
	r.rec(q, args)
	return nil
}
func (r *recordingExec) InTx() bool { return r.inTx }

func TestExecutionWrappersInjectTenantCondition(t *testing.T) {
	ctx := context.Background()
	ten, plat := tenantScope(t, 7), platformScope()
	hasTenant := func(q string) bool { return strings.Contains(q, "`tenant_id` = ?") }

	ex := &recordingExec{affected: 1}
	var n struct{}
	if _, err := sites.Select().Where(tenantdb.Eq("uid", "u")).Count(ctx, ex, ten); err != nil {
		t.Fatal(err)
	}
	if _, err := sites.Update(map[string]any{"name": "n"}).Where(tenantdb.Eq("id", 1)).Exec(ctx, ex, ten); err != nil {
		t.Fatal(err)
	}
	if _, err := sites.Delete().Where(tenantdb.Eq("id", 1)).Exec(ctx, ex, ten); err != nil {
		t.Fatal(err)
	}
	if err := sites.Select().Get(ctx, ex, ten, &n); err != nil {
		t.Fatal(err)
	}
	for i, q := range ex.queries {
		if !hasTenant(q) || ex.args[i][0] != uint64(7) && !strings.HasPrefix(q, "UPDATE") {
			t.Errorf("租户限定的语句必须带 tenant_id 条件且参数含租户 7: %s %v", q, ex.args[i])
		}
	}

	// AcrossTenants 变体：平台范围，不含租户条件。
	ex = &recordingExec{affected: 3}
	if _, err := sites.Select().CountAcrossTenants(ctx, ex, plat); err != nil {
		t.Fatal(err)
	}
	if err := sites.Select().GetAcrossTenants(ctx, ex, plat, &n); err != nil {
		t.Fatal(err)
	}
	if got, err := sites.Update(map[string]any{"name": "n"}).Where(tenantdb.Eq("id", 1)).ExecAcrossTenants(ctx, ex, plat); err != nil || got != 3 {
		t.Fatal(got, err)
	}
	if got, err := sites.Delete().Where(tenantdb.Eq("id", 1)).ExecAcrossTenants(ctx, ex, plat); err != nil || got != 3 {
		t.Fatal(got, err)
	}
	for _, q := range ex.queries {
		if hasTenant(q) {
			t.Errorf("AcrossTenants 语句不应含租户条件: %s", q)
		}
	}
}

// ExecOne：语句带 LIMIT 1（至多改动一行，不会先写后报错）；0 行 → ErrNotFound。
func TestExecOneLimitsToOneRowAndMapsZeroToNotFound(t *testing.T) {
	ctx := context.Background()
	ten := tenantScope(t, 7)
	for _, affected := range []int64{1, 0} {
		ex := &recordingExec{affected: affected}
		uerr := sites.Update(map[string]any{"name": "n"}).Where(tenantdb.Eq("uid", "u")).ExecOne(ctx, ex, ten)
		derr := sites.Delete().Where(tenantdb.Eq("uid", "u")).ExecOne(ctx, ex, ten)
		for _, q := range ex.queries {
			if !strings.HasSuffix(q, " LIMIT 1") || !strings.Contains(q, "`tenant_id` = ?") {
				t.Errorf("ExecOne 语句必须带租户条件与 LIMIT 1: %s", q)
			}
		}
		if affected == 1 && (uerr != nil || derr != nil) {
			t.Errorf("命中 1 行应成功: %v %v", uerr, derr)
		}
		if affected == 0 && (!errors.Is(uerr, db.ErrNotFound) || !errors.Is(derr, db.ErrNotFound)) {
			t.Errorf("0 行应为 ErrNotFound: %v %v", uerr, derr)
		}
	}
}

func TestForUpdateRequiresTransaction(t *testing.T) {
	ctx := context.Background()
	ten := tenantScope(t, 7)
	var rows []struct{}
	if err := sites.Select("id").ForUpdateSkipLocked().Limit(1).All(ctx, &recordingExec{}, ten, &rows); !errors.Is(err, tenantdb.ErrTxRequired) {
		t.Fatalf("自动提交下 FOR UPDATE 必须被拒绝: %v", err)
	}
	if err := sites.Select("id").ForUpdate().Get(ctx, &recordingExec{}, ten, &rows); !errors.Is(err, tenantdb.ErrTxRequired) {
		t.Fatalf("Get 同理: %v", err)
	}
	ex := &recordingExec{inTx: true}
	if err := sites.Select("id").ForUpdate().All(ctx, ex, ten, &rows); err != nil {
		t.Fatalf("事务内应允许: %v", err)
	}
	if len(ex.queries) != 1 {
		t.Fatal("事务内应执行")
	}
}

func TestTypedNilAndInListLimit(t *testing.T) {
	ten := tenantScope(t, 7)
	var np *string
	var nb []byte
	for name, c := range map[string]tenantdb.Cond{
		"类型化 nil 指针":  tenantdb.Eq("uid", np),
		"nil 切片":      tenantdb.Gt("uid", nb),
		"IN 含类型化 nil": tenantdb.In("uid", "a", np),
	} {
		if _, _, err := sites.Select("id").Where(c).Build(ten); !errors.Is(err, tenantdb.ErrNilValue) {
			t.Errorf("%s 应 ErrNilValue: %v", name, err)
		}
	}
	vals := make([]any, tenantdb.MaxInValues+1)
	for i := range vals {
		vals[i] = i
	}
	if _, _, err := sites.Select("id").Where(tenantdb.In("id", vals...)).Build(ten); !errors.Is(err, tenantdb.ErrInvalidArgument) {
		t.Errorf("超大 IN 列表应被拒绝: %v", err)
	}
	if _, _, err := sites.Select("id").Where(tenantdb.In("id", vals[:tenantdb.MaxInValues]...)).Build(ten); err != nil {
		t.Errorf("上限内应通过: %v", err)
	}
}
