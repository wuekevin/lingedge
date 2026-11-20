package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/schedrt"
)

// ErrWrongRole：job_locks 租约走 app 账号（maint 仅限分区维护）。
var ErrWrongRole = errors.New("schedrt/repository: 只接受 app 连接池")

// Locks 实现 schedrt.LockStore（job_locks，平台级表）。
type Locks struct {
	pool   db.Pool
	seeded sync.Map // 已确认存在的租约行名称
}

var _ schedrt.LockStore = (*Locks)(nil)

// NewLocks 创建 LockStore。
func NewLocks(pool db.Pool) (*Locks, error) {
	if pool == nil || pool.Role() != db.RoleApp {
		return nil, ErrWrongRole
	}
	return &Locks{pool: pool}, nil
}

func micros(d time.Duration) int64 { return d.Microseconds() }

// ensureRow 保证租约行存在：补一行「已过期」的占位。必须在事务之外、先于条件 UPDATE 执行：
// 对不存在的主键先 UPDATE 再 INSERT 会让并发事务各持间隙锁后互相等待（实测 ERROR 1213 死锁）；
// 先自动提交地 INSERT IGNORE（并发安全），再对已存在的行做 UPDATE，则只有行锁、没有死锁。每个进程对每个名称只补一次。
func (l *Locks) ensureRow(ctx context.Context, name string) error {
	if _, ok := l.seeded.Load(name); ok {
		return nil
	}
	if _, err := l.pool.Executor(ctx).Exec(ctx, db.Q("INSERT IGNORE INTO job_locks (name, holder, fence, lease_until, acquired_at, updated_at) "+
		"VALUES (?, '', 0, NOW(3) - INTERVAL 1 DAY, NOW(3), NOW(3))"), name); err != nil {
		return err
	}
	l.seeded.Store(name, struct{}{})
	return nil
}

// Acquire 见 schedrt.LockStore。获取是单条条件 UPDATE（行锁内原子）：WHERE 在更新前的行上求值，因此两个并发获取者至多一个成功。
// SET 子句从左到右生效：acquired_at 必须排在 holder 之前，才能用旧 holder 判断「是否换主」；fence 经 LAST_INSERT_ID(expr) 带回，避免二次读取之间被他人改写。
//
// lingedge:platform-table=job_locks
func (l *Locks) Acquire(ctx context.Context, name, holder string, ttl time.Duration, allowSelf bool) (uint64, bool, error) {
	if name == "" || holder == "" || ttl <= 0 {
		return 0, false, errors.New("schedrt/repository: Acquire 参数非法")
	}
	if err := l.ensureRow(ctx, name); err != nil {
		return 0, false, fmt.Errorf("schedrt/repository: 初始化租约行 %s: %w", name, err)
	}
	self := 0
	if allowSelf {
		self = 1
	}
	var fence uint64
	var ok bool
	err := l.pool.WithTx(ctx, func(ctx context.Context) error {
		ex := l.pool.Executor(ctx)
		res, err := ex.Exec(ctx, db.Q("UPDATE job_locks SET acquired_at = IF(holder = ?, acquired_at, NOW(3)), fence = LAST_INSERT_ID(fence + 1), "+
			"lease_until = NOW(3) + INTERVAL ? MICROSECOND, updated_at = NOW(3), holder = ? "+
			"WHERE name = ? AND ((? = 1 AND holder = ?) OR lease_until <= NOW(3))"),
			holder, micros(ttl), holder, name, self, holder)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return err
		}
		ok = true
		return ex.Get(ctx, &fence, db.Q("SELECT LAST_INSERT_ID()"))
	})
	if err != nil {
		return 0, false, fmt.Errorf("schedrt/repository: 获取租约 %s: %w", name, err)
	}
	return fence, ok, nil
}

// Hold 见 schedrt.LockStore。
//
// lingedge:platform-table=job_locks
func (l *Locks) Hold(ctx context.Context, name, holder string, d time.Duration) (bool, error) {
	if d < 0 {
		return false, errors.New("schedrt/repository: Hold 时长非法")
	}
	res, err := l.pool.Executor(ctx).Exec(ctx, db.Q("UPDATE job_locks SET lease_until = NOW(3) + INTERVAL ? MICROSECOND, updated_at = NOW(3) WHERE name = ? AND holder = ?"),
		micros(d), name, holder)
	if err != nil {
		return false, fmt.Errorf("schedrt/repository: 更新租约 %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Release 见 schedrt.LockStore。
//
// lingedge:platform-table=job_locks
func (l *Locks) Release(ctx context.Context, name, holder string) error {
	if _, err := l.pool.Executor(ctx).Exec(ctx, db.Q("UPDATE job_locks SET lease_until = NOW(3), updated_at = NOW(3) WHERE name = ? AND holder = ?"), name, holder); err != nil {
		return fmt.Errorf("schedrt/repository: 释放租约 %s: %w", name, err)
	}
	return nil
}

// Get 读取租约行快照（测试与 doctor 观察用）；行不存在返回 db.ErrNotFound。
//
// lingedge:platform-table=job_locks
func (l *Locks) Get(ctx context.Context, name string) (schedrt.LeaseInfo, error) {
	var r struct {
		Name       string    `db:"name"`
		Holder     string    `db:"holder"`
		Fence      uint64    `db:"fence"`
		LeaseUntil time.Time `db:"lease_until"`
		Expired    bool      `db:"expired"`
	}
	if err := l.pool.Executor(ctx).Get(ctx, &r, db.Q("SELECT name, holder, fence, lease_until, lease_until <= NOW(3) AS expired FROM job_locks WHERE name = ?"), name); err != nil {
		return schedrt.LeaseInfo{}, err
	}
	return schedrt.LeaseInfo(r), nil
}
