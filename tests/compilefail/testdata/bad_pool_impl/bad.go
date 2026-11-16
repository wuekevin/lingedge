// Package bad_pool_impl：包外不能自造满足 db.Pool 的类型（接口含未导出方法）。
package bad_pool_impl

import (
	"context"

	"github.com/wuekevin/lingedge/internal/platform/db"
)

type fake struct{}

func (fake) Role() db.Role                                             { return db.RoleApp }
func (fake) Executor(context.Context) db.Executor                      { return nil }
func (fake) WithTx(context.Context, func(context.Context) error) error { return nil }
func (fake) WithTxOptions(context.Context, db.TxOptions, func(context.Context) error) error {
	return nil
}
func (fake) Ping(context.Context) error { return nil }
func (fake) Stats() db.Stats            { return db.Stats{} }
func (fake) Close() error               { return nil }

var _ db.Pool = fake{}
