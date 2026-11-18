package migrate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

// 迁移互斥（Story 1.6 AC3、ADR-016 §7、ADR-019 §6）。
//
//   - 互斥本身永远由 MySQL `GET_LOCK` 提供（会话级、连接断开即释放，进程崩溃后无需等待租约过期）：首次迁移时
//     `job_locks` 表尚不存在，只能用它；表由 000002 创建之后，若只改用租约，则「用 GET_LOCK 起步的进程」与
//     「看到 job_locks 后改用租约的进程」之间会出现互斥缝隙。因此 GET_LOCK 始终先取、全程持有；
//   - `job_locks` 存在时，另外在名为 `migrate` 的行上记录租约（holder、fence+1、lease_until），使其他组件/doctor 能观察到
//     「迁移进行中」并获得递增的围栏值。租约是对 GET_LOCK 的补充而不是替代：持有 GET_LOCK 即证明没有其他存活的 migrate，
//     所以租约一律直接接管，不会因崩溃遗留的未过期租约而阻塞重启。

const (
	leaseName = "migrate"
	leaseTTL  = 10 * 60 // 秒；每个迁移之前与每个心跳都会续约
	// DefaultHeartbeatInterval 是持锁连接的心跳间隔。
	DefaultHeartbeatInterval = 30 * time.Second
)

type migrateLock struct {
	r      *Runner
	conn   *sql.Conn
	name   string
	holder string
	cols   leaseCols

	// mu 串行化对 conn 的使用（*sql.Conn 不是并发安全的）：心跳协程与迁移主流程共用这条连接。
	mu   sync.Mutex
	lost bool
	stop chan struct{}
	done chan struct{}
}

type leaseCols struct {
	ok         bool
	acquiredAt bool
	updatedAt  bool
}

func lockName(dbName string) string {
	h := sha256.Sum256([]byte(dbName))
	return "lingedge.migrate." + hex.EncodeToString(h[:6])
}

func newHolder() string {
	host, _ := os.Hostname()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("migrate@%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b))
}

func (r *Runner) acquire(ctx context.Context) (*migrateLock, error) {
	var dbName sql.NullString
	if err := r.sqlDB().QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dbName); err != nil || !dbName.Valid {
		return nil, fmt.Errorf("migrate: 无法确定当前数据库: %v", err)
	}
	conn, err := r.sqlDB().Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: 获取连接: %w", err)
	}
	l := &migrateLock{r: r, conn: conn, name: lockName(dbName.String), holder: newHolder(), stop: make(chan struct{}), done: make(chan struct{})}
	timeout := r.LockTimeout
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	var got sql.NullInt64
	// GET_LOCK 的超时单位是整秒：向上取整（500ms 不应变成「不等待」）。
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", l.name, int(math.Ceil(timeout.Seconds()))).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("migrate: 获取迁移互斥: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		_ = conn.Close()
		if !got.Valid {
			return nil, errors.New("migrate: 获取迁移互斥失败（GET_LOCK 返回 NULL）")
		}
		return nil, &ErrLockTimeout{Waited: timeout.String()}
	}
	l.touchLease(ctx)
	interval := r.HeartbeatInterval
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	go l.heartbeat(interval)
	return l, nil
}

// heartbeat 周期性地在持锁连接上校验互斥并续约：一次迁移可能持续很久，期间这条连接是空闲的，
// 若被服务端（wait_timeout）或中间代理回收，互斥会静默丢失、租约也会过期。
func (l *migrateLock) heartbeat(interval time.Duration) {
	defer close(l.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := l.check(c); err == nil {
				l.touchLease(c)
			}
			cancel()
		}
	}
}

// check 确认互斥仍由本连接持有；一旦发现丢失就记下，之后的 verify 一律失败。
func (l *migrateLock) check(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var held sql.NullBool
	if err := l.conn.QueryRowContext(ctx, "SELECT IS_USED_LOCK(?) = CONNECTION_ID()", l.name).Scan(&held); err != nil {
		l.lost = true
		return fmt.Errorf("migrate: 校验迁移互斥: %w", err)
	}
	if !held.Valid || !held.Bool {
		l.lost = true
		return errLockLost
	}
	return nil
}

var errLockLost = errors.New("migrate: 迁移互斥已丢失（持锁连接被断开），为避免并发迁移已中止；请检查数据库连接后重试")

// verify 在每个迁移开始前确认互斥仍由本连接持有（含心跳已发现的丢失）。
func (l *migrateLock) verify(ctx context.Context) error {
	l.mu.Lock()
	lost := l.lost
	l.mu.Unlock()
	if lost {
		return errLockLost
	}
	return l.check(ctx)
}

// checkCols 判定 job_locks 是否可用于记录租约（列集与 platform-identity.md §4.3 一致）。
func (l *migrateLock) checkCols(ctx context.Context) {
	rows, err := l.conn.QueryContext(ctx, "SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'job_locks'")
	if err != nil {
		return
	}
	defer func() { _ = rows.Close() }()
	have := map[string]bool{}
	for rows.Next() {
		var c string
		if rows.Scan(&c) == nil {
			have[strings.ToLower(c)] = true
		}
	}
	l.cols = leaseCols{
		ok:         have["name"] && have["holder"] && have["fence"] && have["lease_until"],
		acquiredAt: have["acquired_at"],
		updatedAt:  have["updated_at"],
	}
}

// touchLease 在 job_locks 存在时记录/续约租约（尽力而为：失败只记日志，互斥由 GET_LOCK 保证）。
func (l *migrateLock) touchLease(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.checkCols(ctx)
	if !l.cols.ok {
		return
	}
	cols := "name, holder, fence, lease_until"
	vals := "?, ?, 1, DATE_ADD(NOW(3), INTERVAL ? SECOND)"
	upd := "fence = fence + 1, lease_until = VALUES(lease_until)"
	if l.cols.acquiredAt {
		cols += ", acquired_at"
		vals += ", NOW(3)"
		upd = "acquired_at = IF(holder = VALUES(holder), acquired_at, VALUES(acquired_at)), " + upd
	}
	if l.cols.updatedAt {
		cols += ", updated_at"
		vals += ", NOW(3)"
		upd += ", updated_at = VALUES(updated_at)"
	}
	upd += ", holder = VALUES(holder)" // holder 放最后：前面的表达式读到的是旧值
	q := "INSERT INTO job_locks (" + cols + ") VALUES (" + vals + ") ON DUPLICATE KEY UPDATE " + upd
	if _, err := l.conn.ExecContext(ctx, q, leaseName, l.holder, leaseTTL); err != nil {
		l.r.logf("警告：无法记录 job_locks 租约（不影响互斥）：%v", err)
	}
}

// release 释放互斥并让租约立即过期（保留行，fence 单调递增）。使用脱离取消的 ctx：Ctrl-C 之后也要释放。
func (l *migrateLock) release() {
	close(l.stop)
	<-l.done // 心跳协程退出后再使用连接
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if l.cols.ok {
		q := "UPDATE job_locks SET lease_until = NOW(3)"
		if l.cols.updatedAt {
			q += ", updated_at = NOW(3)"
		}
		_, _ = l.conn.ExecContext(c, q+" WHERE name = ? AND holder = ?", leaseName, l.holder)
	}
	var released sql.NullInt64
	_ = l.conn.QueryRowContext(c, "SELECT RELEASE_LOCK(?)", l.name).Scan(&released)
	_ = l.conn.Close()
}

// lockHeld 报告迁移互斥当前是否被某个 migrate 进程持有（只读命令用它区分「正在迁移」与「上次崩溃留下的 dirty」）。
func (r *Runner) lockHeld(ctx context.Context) (bool, error) {
	var db sql.NullString
	if err := r.sqlDB().QueryRowContext(ctx, "SELECT DATABASE()").Scan(&db); err != nil || !db.Valid {
		return false, fmt.Errorf("migrate: 无法确定当前数据库: %v", err)
	}
	var free sql.NullInt64
	if err := r.sqlDB().QueryRowContext(ctx, "SELECT IS_FREE_LOCK(?)", lockName(db.String)).Scan(&free); err != nil {
		return false, fmt.Errorf("migrate: %w", err)
	}
	return free.Valid && free.Int64 == 0, nil
}
