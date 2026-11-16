package tenantdb

import (
	"context"
	"fmt"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/scope"
)

// Direction 是排序方向。
type Direction int

const (
	Asc Direction = iota
	Desc
)

type orderTerm struct {
	col string
	dir Direction
}

// SelectBuilder 构建租户限定的 SELECT。值语义：每个链式方法返回新副本，可安全复用。
type SelectBuilder struct {
	t       Table
	cols    []string
	where   []Cond
	order   []orderTerm
	limit   int
	offset  int
	hasLim  bool
	hasOff  bool
	forUpd  bool
	skipLck bool
}

// Select 开始构建 SELECT。cols 为空表示表声明的全部列（从不使用 `SELECT *`，避免加列破坏扫描）；
// 允许列出 tenant_id（只读）。
func (t Table) Select(cols ...string) SelectBuilder { return SelectBuilder{t: t, cols: cols} }

// Where 追加条件（与租户条件及其他条件以 AND 连接）。
func (b SelectBuilder) Where(conds ...Cond) SelectBuilder {
	b.where = append(append([]Cond(nil), b.where...), conds...)
	return b
}

// OrderBy 追加排序列（列须在表白名单内）。
func (b SelectBuilder) OrderBy(col string, dir Direction) SelectBuilder {
	b.order = append(append([]orderTerm(nil), b.order...), orderTerm{col, dir})
	return b
}

// Limit 设置 LIMIT（参数化）。
func (b SelectBuilder) Limit(n int) SelectBuilder { b.limit, b.hasLim = n, true; return b }

// Offset 设置 OFFSET（参数化；MySQL 要求与 Limit 同用）。
func (b SelectBuilder) Offset(n int) SelectBuilder { b.offset, b.hasOff = n, true; return b }

// ForUpdate 追加 FOR UPDATE（须在事务内：Get/All 在非事务执行器上返回 ErrTxRequired）。
func (b SelectBuilder) ForUpdate() SelectBuilder { b.forUpd = true; return b }

// ForUpdateSkipLocked 追加 FOR UPDATE SKIP LOCKED（任务领取，ADR-019）。
func (b SelectBuilder) ForUpdateSkipLocked() SelectBuilder {
	b.forUpd, b.skipLck = true, true
	return b
}

func (b SelectBuilder) selectList() (string, error) {
	cols := b.cols
	if len(cols) == 0 {
		cols = b.t.cols
	}
	for _, c := range cols {
		if c != TenantColumn && !b.t.known(c) {
			return "", fmt.Errorf("%w: 表 %s 的列 %q", ErrUnknownColumn, b.t.name, c)
		}
	}
	return joinQuoted(cols), nil
}

func (b SelectBuilder) tail(args *[]any) (string, error) {
	var sb strings.Builder
	if len(b.order) > 0 {
		sb.WriteString(" ORDER BY ")
		for i, o := range b.order {
			if o.col != TenantColumn && !b.t.known(o.col) {
				return "", fmt.Errorf("%w: ORDER BY 列 %q", ErrUnknownColumn, o.col)
			}
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(quote(o.col))
			if o.dir == Desc {
				sb.WriteString(" DESC")
			} else {
				sb.WriteString(" ASC")
			}
		}
	}
	if b.hasLim {
		if b.limit < 0 {
			return "", fmt.Errorf("%w: LIMIT 不得为负", ErrInvalidArgument)
		}
		sb.WriteString(" LIMIT ?")
		*args = append(*args, b.limit)
		if b.hasOff {
			if b.offset < 0 {
				return "", fmt.Errorf("%w: OFFSET 不得为负", ErrInvalidArgument)
			}
			sb.WriteString(" OFFSET ?")
			*args = append(*args, b.offset)
		}
	} else if b.hasOff {
		return "", fmt.Errorf("%w: OFFSET 必须与 LIMIT 同用", ErrInvalidArgument)
	}
	if b.forUpd {
		sb.WriteString(" FOR UPDATE")
		if b.skipLck {
			sb.WriteString(" SKIP LOCKED")
		}
	}
	return sb.String(), nil
}

func (b SelectBuilder) build(tenant *uint64) (db.Query, []any, error) {
	list, err := b.selectList()
	if err != nil {
		return db.Query{}, nil, err
	}
	var args []any
	where, err := buildWhere(b.t, tenant, b.where, &args)
	if err != nil {
		return db.Query{}, nil, err
	}
	tail, err := b.tail(&args)
	if err != nil {
		return db.Query{}, nil, err
	}
	return mint("SELECT " + list + " FROM " + quote(b.t.name) + where + tail), args, nil
}

func (b SelectBuilder) buildCount(tenant *uint64) (db.Query, []any, error) {
	if len(b.order) > 0 || b.hasLim || b.hasOff || b.forUpd {
		return db.Query{}, nil, fmt.Errorf("%w: Count 不接受 OrderBy/Limit/Offset/ForUpdate", ErrInvalidArgument)
	}
	var args []any
	where, err := buildWhere(b.t, tenant, b.where, &args)
	if err != nil {
		return db.Query{}, nil, err
	}
	return mint("SELECT COUNT(*) FROM " + quote(b.t.name) + where), args, nil
}

// Build 生成租户限定的查询文本与参数（供单测与调试；执行请用 Get/All/Count）。
func (b SelectBuilder) Build(sc scope.Scope) (db.Query, []any, error) {
	tid, err := tenantOf(sc)
	if err != nil {
		return db.Query{}, nil, err
	}
	return b.build(&tid)
}

// BuildAcrossTenants 生成跨租户查询（要求平台范围）。
func (b SelectBuilder) BuildAcrossTenants(sc scope.Scope) (db.Query, []any, error) {
	if err := requirePlatform(sc); err != nil {
		return db.Query{}, nil, err
	}
	return b.build(nil)
}

// Get 查询租户内的单行并扫描到 dest；无匹配（含他租户的行）返回 db.ErrNotFound。
func (b SelectBuilder) Get(ctx context.Context, ex db.Executor, sc scope.Scope, dest any) error {
	q, args, err := b.Build(sc)
	if err != nil {
		return err
	}
	if b.forUpd && !ex.InTx() {
		return ErrTxRequired
	}
	return ex.Get(ctx, dest, q, args...)
}

// All 查询租户内的多行（dest 为指向切片的指针）。
func (b SelectBuilder) All(ctx context.Context, ex db.Executor, sc scope.Scope, dest any) error {
	q, args, err := b.Build(sc)
	if err != nil {
		return err
	}
	if b.forUpd && !ex.InTx() {
		return ErrTxRequired
	}
	return ex.Select(ctx, dest, q, args...)
}

// Count 统计租户内匹配的行数。
func (b SelectBuilder) Count(ctx context.Context, ex db.Executor, sc scope.Scope) (int64, error) {
	tid, err := tenantOf(sc)
	if err != nil {
		return 0, err
	}
	q, args, err := b.buildCount(&tid)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := ex.Get(ctx, &n, q, args...); err != nil {
		return 0, err
	}
	return n, nil
}

// GetAcrossTenants 跨租户查询单行（平台范围；Service 层须已校验权限串，ADR-009 §2）。
func (b SelectBuilder) GetAcrossTenants(ctx context.Context, ex db.Executor, sc scope.Scope, dest any) error {
	q, args, err := b.BuildAcrossTenants(sc)
	if err != nil {
		return err
	}
	if b.forUpd && !ex.InTx() {
		return ErrTxRequired
	}
	return ex.Get(ctx, dest, q, args...)
}

// AllAcrossTenants 跨租户查询多行。
func (b SelectBuilder) AllAcrossTenants(ctx context.Context, ex db.Executor, sc scope.Scope, dest any) error {
	q, args, err := b.BuildAcrossTenants(sc)
	if err != nil {
		return err
	}
	if b.forUpd && !ex.InTx() {
		return ErrTxRequired
	}
	return ex.Select(ctx, dest, q, args...)
}

// CountAcrossTenants 跨租户统计行数。
func (b SelectBuilder) CountAcrossTenants(ctx context.Context, ex db.Executor, sc scope.Scope) (int64, error) {
	if err := requirePlatform(sc); err != nil {
		return 0, err
	}
	q, args, err := b.buildCount(nil)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := ex.Get(ctx, &n, q, args...); err != nil {
		return 0, err
	}
	return n, nil
}
