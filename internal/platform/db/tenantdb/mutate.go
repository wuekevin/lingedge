package tenantdb

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/scope"
)

// ---- INSERT ----

// InsertBuilder 构建 INSERT：tenant_id 强制取自 scope.TenantID，values 里的 tenant_id 被忽略（ADR-009 §2）。
type InsertBuilder struct {
	t      Table
	values map[string]any
}

// Insert 开始构建 INSERT。values 的键须在表白名单内（tenant_id 键被忽略）。
func (t Table) Insert(values map[string]any) InsertBuilder {
	return InsertBuilder{t: t, values: values}
}

// Build 生成 INSERT 文本与参数（列按字母序，tenant_id 在最前）。
func (b InsertBuilder) Build(sc scope.Scope) (db.Query, []any, error) {
	tid, err := tenantOf(sc)
	if err != nil {
		if errors.Is(err, ErrPlatformScope) {
			return db.Query{}, nil, ErrNoTenant
		}
		return db.Query{}, nil, err
	}
	keys := make([]string, 0, len(b.values))
	for k := range b.values {
		if k == TenantColumn {
			continue // 忽略请求体里的 tenant_id
		}
		if !b.t.known(k) {
			return db.Query{}, nil, fmt.Errorf("%w: 表 %s 的列 %q", ErrUnknownColumn, b.t.name, k)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return db.Query{}, nil, ErrNoValues
	}
	sort.Strings(keys)
	args := make([]any, 0, len(keys)+1)
	args = append(args, tid)
	for _, k := range keys {
		args = append(args, b.values[k])
	}
	text := "INSERT INTO " + quote(b.t.name) + " (" + quote(TenantColumn) + ", " + joinQuoted(keys) + ") VALUES (" +
		strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ") + ")"
	return mint(text), args, nil
}

// Exec 执行 INSERT，返回自增 id。
func (b InsertBuilder) Exec(ctx context.Context, ex db.Executor, sc scope.Scope) (int64, error) {
	q, args, err := b.Build(sc)
	if err != nil {
		return 0, err
	}
	res, err := ex.Exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ---- UPDATE ----

// UpdateBuilder 构建租户限定的 UPDATE。
type UpdateBuilder struct {
	t     Table
	set   map[string]any
	where []Cond
}

// Update 开始构建 UPDATE。set 的键须在表白名单内，且不得是 tenant_id（行不能被移到别的租户）。
func (t Table) Update(set map[string]any) UpdateBuilder { return UpdateBuilder{t: t, set: set} }

// Where 追加条件。
func (b UpdateBuilder) Where(conds ...Cond) UpdateBuilder {
	b.where = append(append([]Cond(nil), b.where...), conds...)
	return b
}

func (b UpdateBuilder) build(tenant *uint64, limitOne bool) (db.Query, []any, error) {
	if len(b.where) == 0 {
		return db.Query{}, nil, ErrNoCondition
	}
	keys := make([]string, 0, len(b.set))
	for k := range b.set {
		switch {
		case k == TenantColumn:
			return db.Query{}, nil, fmt.Errorf("%w: SET 列 %s", ErrReservedColumn, k)
		case !b.t.known(k):
			return db.Query{}, nil, fmt.Errorf("%w: 表 %s 的列 %q", ErrUnknownColumn, b.t.name, k)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return db.Query{}, nil, ErrNoValues
	}
	sort.Strings(keys)
	args := make([]any, 0, len(keys)+len(b.where)+1)
	sets := make([]string, len(keys))
	for i, k := range keys {
		sets[i] = quote(k) + " = ?"
		args = append(args, b.set[k])
	}
	where, err := buildWhere(b.t, tenant, b.where, &args)
	if err != nil {
		return db.Query{}, nil, err
	}
	return mint("UPDATE " + quote(b.t.name) + " SET " + strings.Join(sets, ", ") + where + limitClause(limitOne)), args, nil
}

// Build 生成租户限定的 UPDATE 文本与参数。
func (b UpdateBuilder) Build(sc scope.Scope) (db.Query, []any, error) {
	tid, err := tenantOf(sc)
	if err != nil {
		return db.Query{}, nil, err
	}
	return b.build(&tid, false)
}

// BuildAcrossTenants 生成跨租户 UPDATE（平台范围）。
func (b UpdateBuilder) BuildAcrossTenants(sc scope.Scope) (db.Query, []any, error) {
	if err := requirePlatform(sc); err != nil {
		return db.Query{}, nil, err
	}
	return b.build(nil, false)
}

func execAffected(ctx context.Context, ex db.Executor, q db.Query, args []any) (int64, error) {
	res, err := ex.Exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// limitClause：ExecOne 使用 LIMIT 1，保证即使条件命中多行也至多改动/删除一行（不会「先写入、后报错」）。
func limitClause(one bool) string {
	if one {
		return " LIMIT 1"
	}
	return ""
}

func one(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return db.ErrNotFound
	}
	return nil
}

// Exec 执行 UPDATE，返回匹配的行数（驱动启用 clientFoundRows；越权时为 0）。
func (b UpdateBuilder) Exec(ctx context.Context, ex db.Executor, sc scope.Scope) (int64, error) {
	q, args, err := b.Build(sc)
	if err != nil {
		return 0, err
	}
	return execAffected(ctx, ex, q, args)
}

// ExecOne 至多更新 1 行（语句带 LIMIT 1）：0 行（不存在或属于其他租户）返回 db.ErrNotFound。条件应落在唯一键上。
func (b UpdateBuilder) ExecOne(ctx context.Context, ex db.Executor, sc scope.Scope) error {
	tid, err := tenantOf(sc)
	if err != nil {
		return err
	}
	q, args, err := b.build(&tid, true)
	if err != nil {
		return err
	}
	return one(execAffected(ctx, ex, q, args))
}

// ExecAcrossTenants 跨租户执行 UPDATE。
func (b UpdateBuilder) ExecAcrossTenants(ctx context.Context, ex db.Executor, sc scope.Scope) (int64, error) {
	q, args, err := b.BuildAcrossTenants(sc)
	if err != nil {
		return 0, err
	}
	return execAffected(ctx, ex, q, args)
}

// ---- DELETE ----

// DeleteBuilder 构建租户限定的 DELETE。
type DeleteBuilder struct {
	t     Table
	where []Cond
}

// Delete 开始构建 DELETE。
func (t Table) Delete() DeleteBuilder { return DeleteBuilder{t: t} }

// Where 追加条件。
func (b DeleteBuilder) Where(conds ...Cond) DeleteBuilder {
	b.where = append(append([]Cond(nil), b.where...), conds...)
	return b
}

func (b DeleteBuilder) build(tenant *uint64, limitOne bool) (db.Query, []any, error) {
	if len(b.where) == 0 {
		return db.Query{}, nil, ErrNoCondition
	}
	var args []any
	where, err := buildWhere(b.t, tenant, b.where, &args)
	if err != nil {
		return db.Query{}, nil, err
	}
	return mint("DELETE FROM " + quote(b.t.name) + where + limitClause(limitOne)), args, nil
}

// Build 生成租户限定的 DELETE 文本与参数。
func (b DeleteBuilder) Build(sc scope.Scope) (db.Query, []any, error) {
	tid, err := tenantOf(sc)
	if err != nil {
		return db.Query{}, nil, err
	}
	return b.build(&tid, false)
}

// BuildAcrossTenants 生成跨租户 DELETE（平台范围）。
func (b DeleteBuilder) BuildAcrossTenants(sc scope.Scope) (db.Query, []any, error) {
	if err := requirePlatform(sc); err != nil {
		return db.Query{}, nil, err
	}
	return b.build(nil, false)
}

// Exec 执行 DELETE，返回删除的行数（越权时为 0）。
func (b DeleteBuilder) Exec(ctx context.Context, ex db.Executor, sc scope.Scope) (int64, error) {
	q, args, err := b.Build(sc)
	if err != nil {
		return 0, err
	}
	return execAffected(ctx, ex, q, args)
}

// ExecOne 至多删除 1 行（语句带 LIMIT 1）：0 行返回 db.ErrNotFound。条件应落在唯一键上。
func (b DeleteBuilder) ExecOne(ctx context.Context, ex db.Executor, sc scope.Scope) error {
	tid, err := tenantOf(sc)
	if err != nil {
		return err
	}
	q, args, err := b.build(&tid, true)
	if err != nil {
		return err
	}
	return one(execAffected(ctx, ex, q, args))
}

// ExecAcrossTenants 跨租户执行 DELETE。
func (b DeleteBuilder) ExecAcrossTenants(ctx context.Context, ex db.Executor, sc scope.Scope) (int64, error) {
	q, args, err := b.BuildAcrossTenants(sc)
	if err != nil {
		return 0, err
	}
	return execAffected(ctx, ex, q, args)
}
