// Package db 是 MySQL 访问层的基础设施（Story 1.5、ADR-001 §6、ADR-009、database/README §5.1）：
// 连接（UTC/utf8mb4/parseTime/参数化）、三类账号的独立连接池、经 context 传播的事务。
//
// 边界：
//   - 本包与各模块的 repository 包是仓库内仅有可以 import jmoiron/sqlx、database/sql、go-sql-driver 的地方（depguard，make lint）；
//     Service/Handler 只能通过 Repository 访问数据库，并用 WithTx 划定事务边界；
//   - 查询文本只能是编译期常量（Q）或 tenantdb 构建器的产物，运行期拼接的 SQL 无法通过编译；
//   - 租户条件由 tenantdb 构建器在 Scope 的约束下注入（ADR-009 §2）。
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// Pool 是三类连接池共有的能力。方法集含未导出方法，包外无法实现它，保证只有本包的三种池类型满足。
type Pool interface {
	// Role 返回该池使用的账号角色。
	Role() Role
	// Executor 返回执行器：ctx 中存在属于该池的事务则返回事务，否则返回池本身。Repository 一律经此取执行器。
	Executor(ctx context.Context) Executor
	// WithTx 在事务内运行 fn（见 pool.WithTx）。
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
	// WithTxOptions 是带选项的 WithTx。
	WithTxOptions(ctx context.Context, opts TxOptions, fn func(ctx context.Context) error) error
	Ping(ctx context.Context) error
	Stats() Stats
	Close() error

	base() *pool
}

// Stats 是连接池统计。
type Stats = sql.DBStats

type pool struct {
	role Role
	x    *sqlx.DB
}

// AppPool 是 app 账号的连接池（请求路径与 Worker 常规任务）。
type AppPool struct{ *pool }

// MaintPool 是 maint 账号的连接池（仅 retention.cleanup / partition.maintain；请求路径不得持有）。
type MaintPool struct{ *pool }

// MigratePool 是 migrate 账号的连接池（仅 lingedge-server migrate 与向导迁移步骤）。
type MigratePool struct{ *pool }

var (
	_ Pool = (*AppPool)(nil)
	_ Pool = (*MaintPool)(nil)
	_ Pool = (*MigratePool)(nil)
)

// OpenApp 建立 app 池。
func OpenApp(ctx context.Context, cfg Config) (*AppPool, error) {
	p, err := open(ctx, cfg, RoleApp)
	if err != nil {
		return nil, err
	}
	return &AppPool{p}, nil
}

// OpenMaint 建立 maint 池。调用点受静态规则限制：只有调度/Worker 装配与维护作业所在的白名单包可以调用（forbidigo [db-pool-maint]，
// 白名单见 scripts/gates/spec.go）；请求路径持有 maint 池即违规。
func OpenMaint(ctx context.Context, cfg Config) (*MaintPool, error) {
	p, err := open(ctx, cfg, RoleMaint)
	if err != nil {
		return nil, err
	}
	return &MaintPool{p}, nil
}

// OpenMigrate 建立 migrate 池。调用点受静态规则限制：只有 `lingedge-server migrate`（及迁移框架包）可以调用（forbidigo [db-pool-migrate]）。
func OpenMigrate(ctx context.Context, cfg Config) (*MigratePool, error) {
	p, err := open(ctx, cfg, RoleMigrate)
	if err != nil {
		return nil, err
	}
	return &MigratePool{p}, nil
}

// SQLDB 返回底层 *sql.DB，供 goose 等必须持有 *sql.DB 的迁移库使用（Story 1.6）。仅 migrate 池提供。
func (p *MigratePool) SQLDB() *sql.DB { return p.x.DB }

func open(ctx context.Context, cfg Config, role Role) (*pool, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("db: %s 池配置无效: %w", role, err)
	}
	connector, err := mysql.NewConnector(cfg.driverConfig())
	if err != nil {
		return nil, fmt.Errorf("db: %s 池配置无效: %w", role, err)
	}
	raw := sql.OpenDB(connector)
	l := cfg.limits(role)
	raw.SetMaxOpenConns(l.MaxOpenConns)
	raw.SetMaxIdleConns(l.MaxIdleConns)
	raw.SetConnMaxLifetime(l.ConnMaxLifetime)
	raw.SetConnMaxIdleTime(l.ConnMaxIdleTime)
	p := &pool{role: role, x: sqlx.NewDb(raw, "mysql")}
	vctx, cancel := context.WithTimeout(ctx, connectTimeout(cfg))
	defer cancel()
	if err := p.verifySession(vctx); err != nil {
		_ = p.x.Close()
		return nil, fmt.Errorf("db: %s 池连接校验失败: %w", role, err)
	}
	return p, nil
}

// verifySession 确认连接会话真的是 UTC 与 utf8mb4（AC1）；不满足即拒绝启动，而不是带着错误的时区写数据。
func (p *pool) verifySession(ctx context.Context) error {
	var s struct {
		TimeZone  string `db:"tz"`
		Charset   string `db:"cs"`
		Collation string `db:"co"`
	}
	if err := p.x.GetContext(ctx, &s, "SELECT @@session.time_zone AS tz, @@session.character_set_connection AS cs, @@session.collation_connection AS co"); err != nil {
		return err
	}
	if s.TimeZone != "+00:00" && s.TimeZone != "UTC" {
		return fmt.Errorf("会话时区不是 UTC（%s）", s.TimeZone)
	}
	if s.Charset != "utf8mb4" || !strings.HasPrefix(s.Collation, "utf8mb4_") {
		return fmt.Errorf("会话字符集/排序规则不是 utf8mb4（%s/%s）", s.Charset, s.Collation)
	}
	return nil
}

func (p *pool) base() *pool { return p }

// Role 返回账号角色。
func (p *pool) Role() Role { return p.role }

// Ping 检查连通性。
func (p *pool) Ping(ctx context.Context) error { return p.x.PingContext(ctx) }

// Stats 返回连接池统计。
func (p *pool) Stats() Stats { return p.x.Stats() }

// Close 关闭连接池。
func (p *pool) Close() error { return p.x.Close() }

// ---- 事务（AC1：经 context 传播）----

type txKey struct{ p *pool }

type txState struct{ x *sqlx.Tx }

// TxFromContext 返回 ctx 中属于 p 的事务执行器。不同池的事务互相独立（键含池标识）：app 的事务绝不会被 maint 的 Repository 误用。
func TxFromContext(ctx context.Context, p Pool) (Executor, bool) {
	if p == nil {
		return nil, false
	}
	st, ok := ctx.Value(txKey{p.base()}).(*txState)
	if !ok || st == nil {
		return nil, false
	}
	return runner{r: st.x, inTx: true}, true
}

// Executor 见 Pool.Executor。
func (p *pool) Executor(ctx context.Context) Executor {
	if st, ok := ctx.Value(txKey{p}).(*txState); ok && st != nil {
		return runner{r: st.x, inTx: true}
	}
	return runner{r: p.x}
}

// TxOptions 是开启新事务的选项（加入已有事务时忽略）。
type TxOptions struct {
	ReadOnly bool
}

// WithTx 在事务内运行 fn，并把事务放入传给 fn 的 ctx：
//   - ctx 已含该池的事务时直接加入（不另开事务、不提交），使 Service 调用的多个模块共用同一事务（ADR-001 §6）；
//   - fn 返回 nil 则提交，返回错误或 panic 则回滚（panic 回滚后原样重新抛出）；
//   - 事务在 fn 返回后即结束，不得把 ctx 或执行器带出 fn。
func (p *pool) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return p.withTx(ctx, TxOptions{}, fn)
}

// WithTxOptions 是带选项的 WithTx。
func (p *pool) WithTxOptions(ctx context.Context, opts TxOptions, fn func(ctx context.Context) error) error {
	return p.withTx(ctx, opts, fn)
}

func (p *pool) withTx(ctx context.Context, opts TxOptions, fn func(ctx context.Context) error) (err error) {
	if st, ok := ctx.Value(txKey{p}).(*txState); ok && st != nil {
		return fn(ctx)
	}
	x, err := p.x.BeginTxx(ctx, &sql.TxOptions{ReadOnly: opts.ReadOnly})
	if err != nil {
		return fmt.Errorf("db: 开启事务: %w", err)
	}
	// fn 既可能正常返回、panic，也可能经 runtime.Goexit（如 t.Fatal）退出：只有「正常返回且提交成功」才算完成，其余一律回滚。
	committed := false
	defer func() {
		if !committed {
			_ = x.Rollback()
		}
	}()
	if err := fn(context.WithValue(ctx, txKey{p}, &txState{x})); err != nil {
		committed = true // 下面显式回滚并报告回滚错误，defer 不再重复
		if rbErr := x.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("db: 回滚: %w", rbErr))
		}
		return err
	}
	committed = true
	if err := x.Commit(); err != nil {
		return fmt.Errorf("db: 提交事务: %w", err)
	}
	return nil
}
