package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/idempotency"
)

// ErrWrongRole：只接受 app 连接池。
var ErrWrongRole = errors.New("idempotency/repository: 只接受 app 连接池")

// Options 是 Store 的可选参数（零值取默认）。
type Options struct {
	Clock         clock.Clock
	TTL           time.Duration // 默认 24h
	InFlightLease time.Duration // 默认 5min
}

// Store 实现 idempotency.Store。
type Store struct {
	pool  db.Pool
	clk   clock.Clock
	ttl   time.Duration
	lease time.Duration
}

var _ idempotency.Store = (*Store)(nil)

// New 创建 Store。
func New(pool db.Pool, o Options) (*Store, error) {
	if pool == nil || pool.Role() != db.RoleApp {
		return nil, ErrWrongRole
	}
	s := &Store{pool: pool, clk: o.Clock, ttl: o.TTL, lease: o.InFlightLease}
	if s.clk == nil {
		s.clk = clock.System()
	}
	if s.ttl <= 0 {
		s.ttl = idempotency.DefaultTTL
	}
	if s.lease <= 0 {
		s.lease = idempotency.DefaultInFlightLease
	}
	return s, nil
}

type row struct {
	ID        uint64    `db:"id"`
	Method    string    `db:"method"`
	Path      string    `db:"path"`
	Hash      string    `db:"request_hash"`
	Status    *int      `db:"response_status"`
	Body      []byte    `db:"response_body"`
	TaskID    *uint64   `db:"task_id"`
	CreatedAt time.Time `db:"created_at"`
	ExpiresAt time.Time `db:"expires_at"`
}

const maxAttempts = 4

// Begin 见 idempotency.Store。
//
// lingedge:platform-table=idempotency_keys
func (s *Store) Begin(ctx context.Context, r idempotency.Request) (idempotency.Outcome, error) {
	if err := validate(r); err != nil {
		return idempotency.Outcome{}, err
	}
	ex := s.pool.Executor(ctx)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		now := s.clk.Now()
		res, err := ex.Exec(ctx, db.Q("INSERT INTO idempotency_keys (created_at, principal_key, idem_key, method, path, request_hash, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)"),
			now, r.PrincipalKey, r.Key, r.Method, r.Path, r.Hash, now.Add(s.ttl))
		if err == nil {
			id, ierr := res.LastInsertId()
			if ierr != nil {
				return idempotency.Outcome{}, fmt.Errorf("idempotency: 取占位 id: %w", ierr)
			}
			return idempotency.Outcome{Result: idempotency.Acquired, ID: uint64(id)}, nil
		}
		if !db.IsDuplicate(err) {
			return idempotency.Outcome{}, fmt.Errorf("idempotency: 占位: %w", err)
		}

		var ex1 row
		err = ex.Get(ctx, &ex1, db.Q("SELECT id, method, path, request_hash, response_status, response_body, task_id, created_at, expires_at FROM idempotency_keys WHERE principal_key = ? AND idem_key = ?"), r.PrincipalKey, r.Key)
		if errors.Is(err, db.ErrNotFound) {
			continue // 刚被并发清理：重新占位
		}
		if err != nil {
			return idempotency.Outcome{}, fmt.Errorf("idempotency: 读取既有记录: %w", err)
		}
		if !ex1.ExpiresAt.After(now) {
			if _, err := ex.Exec(ctx, db.Q("DELETE FROM idempotency_keys WHERE id = ? AND expires_at <= ?"), ex1.ID, now); err != nil {
				return idempotency.Outcome{}, fmt.Errorf("idempotency: 清除过期记录: %w", err)
			}
			continue
		}
		if ex1.Method != r.Method || ex1.Path != r.Path || ex1.Hash != r.Hash {
			return idempotency.Outcome{Result: idempotency.Mismatch}, nil
		}
		if ex1.Status != nil {
			out := idempotency.Outcome{Result: idempotency.Replay, ID: ex1.ID, Status: *ex1.Status, Body: ex1.Body}
			if ex1.TaskID != nil {
				out.TaskID = *ex1.TaskID
			}
			return out, nil
		}
		// 同键同请求，仍在执行：租约未到期 → InFlight；已到期（执行方崩溃）→ CAS 接管。
		if ex1.CreatedAt.After(now.Add(-s.lease)) {
			return idempotency.Outcome{Result: idempotency.InFlight}, nil
		}
		res, err = ex.Exec(ctx, db.Q("UPDATE idempotency_keys SET created_at = ? WHERE id = ? AND response_status IS NULL AND created_at = ?"), now, ex1.ID, ex1.CreatedAt)
		if err != nil {
			return idempotency.Outcome{}, fmt.Errorf("idempotency: 接管陈旧占位: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return idempotency.Outcome{Result: idempotency.Acquired, ID: ex1.ID}, nil
		}
		// 别的请求抢先接管：回到循环重新判定（多半得到 InFlight）。
	}
	return idempotency.Outcome{}, errors.New("idempotency: 占位竞争未收敛")
}

// Complete 见 idempotency.Store。
//
// lingedge:platform-table=idempotency_keys
func (s *Store) Complete(ctx context.Context, id uint64, status int, body []byte, taskID uint64) error {
	var task any
	if taskID != 0 {
		task = taskID
	}
	res, err := s.pool.Executor(ctx).Exec(ctx, db.Q("UPDATE idempotency_keys SET response_status = ?, response_body = ?, task_id = ? WHERE id = ? AND response_status IS NULL"), status, body, task, id)
	if err != nil {
		return fmt.Errorf("idempotency: 写入首次响应: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return idempotency.ErrLost
	}
	return nil
}

// Abandon 见 idempotency.Store。
//
// lingedge:platform-table=idempotency_keys
func (s *Store) Abandon(ctx context.Context, id uint64) error {
	if _, err := s.pool.Executor(ctx).Exec(ctx, db.Q("DELETE FROM idempotency_keys WHERE id = ? AND response_status IS NULL"), id); err != nil {
		return fmt.Errorf("idempotency: 释放占位: %w", err)
	}
	return nil
}

// DeleteExpired 见 idempotency.Store。
//
// lingedge:platform-table=idempotency_keys
func (s *Store) DeleteExpired(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	res, err := s.pool.Executor(ctx).Exec(ctx, db.Q("DELETE FROM idempotency_keys WHERE expires_at <= ? LIMIT ?"), s.clk.Now(), limit)
	if err != nil {
		return 0, fmt.Errorf("idempotency: 清理过期记录: %w", err)
	}
	return res.RowsAffected()
}

func validate(r idempotency.Request) error {
	switch {
	case r.PrincipalKey == "" || len(r.PrincipalKey) > 64:
		return errors.New("idempotency: principal_key 非法（1–64 字符）")
	case r.Key == "" || len(r.Key) > idempotency.MaxKeyLen:
		return errors.New("idempotency: 幂等键非法（1–128 字符）")
	case r.Method == "" || len(r.Method) > 8:
		return errors.New("idempotency: method 非法")
	case r.Path == "" || len(r.Path) > idempotency.MaxPathLen:
		return errors.New("idempotency: path 非法（1–255 字符）")
	case len(r.Hash) != 64:
		return errors.New("idempotency: request_hash 必须是 64 字符的 SHA-256 十六进制")
	}
	return nil
}
