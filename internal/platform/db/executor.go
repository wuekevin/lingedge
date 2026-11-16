package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"

	"github.com/jmoiron/sqlx"
)

// Result 是写操作的结果（别名，使 Repository 不必 import database/sql）。
type Result = sql.Result

// Executor 是 Repository 执行 SQL 的唯一入口：连接池与事务都实现它，因此同一段 Repository 代码在事务内外行为一致。
// 所有查询必须是 Query（常量或受信任构建器生成），参数经 ? 占位符传递，由服务端预处理语句绑定（驱动 interpolateParams=false）。
type Executor interface {
	// Exec 执行写语句。
	Exec(ctx context.Context, q Query, args ...any) (Result, error)
	// Get 查询单行并扫描到 dest（struct 按 `db` 标签，或单列标量）；无行返回 ErrNotFound。
	Get(ctx context.Context, dest any, q Query, args ...any) error
	// Select 查询多行并扫描到 dest（指向切片的指针）；无行得到空切片、无错误。
	Select(ctx context.Context, dest any, q Query, args ...any) error
	// InTx 报告该执行器是否是事务（FOR UPDATE 等依赖事务的构建器据此拒绝在自动提交下使用）。
	InTx() bool
}

// sqlxRunner 是 *sqlx.DB 与 *sqlx.Tx 共有的方法子集。
type sqlxRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	GetContext(ctx context.Context, dest any, query string, args ...any) error
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
}

type runner struct {
	r    sqlxRunner
	inTx bool
}

func (e runner) InTx() bool { return e.inTx }

var _ Executor = runner{}
var _ sqlxRunner = (*sqlx.DB)(nil)
var _ sqlxRunner = (*sqlx.Tx)(nil)

func (e runner) Exec(ctx context.Context, q Query, args ...any) (Result, error) {
	res, err := e.r.ExecContext(ctx, q.text, args...)
	if err != nil {
		return nil, fmt.Errorf("db: exec: %w", err)
	}
	return res, nil
}

func (e runner) Get(ctx context.Context, dest any, q Query, args ...any) error {
	if err := e.r.GetContext(ctx, dest, q.text, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("db: get: %w", err)
	}
	return nil
}

func (e runner) Select(ctx context.Context, dest any, q Query, args ...any) error {
	// 无行时得到空切片而不是 nil（序列化为 [] 而非 null），且不会向调用方已有内容追加。
	if v := reflect.ValueOf(dest); v.Kind() == reflect.Pointer && !v.IsNil() && v.Elem().Kind() == reflect.Slice {
		v.Elem().Set(reflect.MakeSlice(v.Elem().Type(), 0, 0))
	}
	if err := e.r.SelectContext(ctx, dest, q.text, args...); err != nil {
		return fmt.Errorf("db: select: %w", err)
	}
	return nil
}
