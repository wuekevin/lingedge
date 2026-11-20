package repository

import (
	"context"
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
)

const fenced = "WHERE id = ? AND fence = ? AND lease_owner = ? AND status = 'running'"

func micros(d time.Duration) int64 { return d.Microseconds() }

// Claim 见 taskrt.Store。FOR UPDATE SKIP LOCKED 使并发 Worker 天然互斥；每个被领取的任务 fence+1、attempts+1。
func (r *Repo) Claim(ctx context.Context, sc scope.Scope, owner string, types []taskrt.Type, n int, lease time.Duration) ([]taskrt.Task, error) {
	if err := requirePlatform(sc); err != nil {
		return nil, err
	}
	if n <= 0 || len(types) == 0 || owner == "" || lease <= 0 {
		return nil, errors.New("taskrt/repository: Claim 参数非法")
	}
	typeList := ""
	for i, t := range types {
		if i > 0 {
			typeList += ","
		}
		typeList += string(t)
	}
	var out []taskrt.Task
	err := r.pool.WithTx(ctx, func(ctx context.Context) error {
		ex := r.pool.Executor(ctx)
		var ids []uint64
		if err := ex.Select(ctx, &ids, db.Q("SELECT id FROM tasks WHERE status = 'pending' AND run_after <= NOW(3) AND FIND_IN_SET(type, ?) > 0 "+
			"ORDER BY priority, id LIMIT ? FOR UPDATE SKIP LOCKED"), typeList, n); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := ex.Exec(ctx, db.Q("UPDATE tasks SET status = 'running', lease_owner = ?, lease_until = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), "+
				"fence = fence + 1, attempts = attempts + 1, started_at = COALESCE(started_at, NOW(3)) WHERE id = ? AND status = 'pending'"), owner, micros(lease), id); err != nil {
				return err
			}
			var row taskRow
			if err := ex.Get(ctx, &row, db.Q("SELECT "+taskCols+taskFrom+"WHERE t.id = ?"), id); err != nil {
				return err
			}
			t := row.task()
			if err := r.event(ctx, ex, id, taskrt.LevelInfo, "task claimed", []byte(`{"attempt":`+itoa(uint64(t.Attempts))+`,"fence":`+itoa(t.Fence)+`}`)); err != nil {
				return err
			}
			out = append(out, t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// Renew 见 taskrt.Store。
func (r *Repo) Renew(ctx context.Context, sc scope.Scope, id, fence uint64, owner string, lease time.Duration) (bool, error) {
	if err := requirePlatform(sc); err != nil {
		return false, err
	}
	ex := r.pool.Executor(ctx)
	res, err := ex.Exec(ctx, db.Q("UPDATE tasks SET lease_until = DATE_ADD(NOW(3), INTERVAL ? MICROSECOND) "+fenced), micros(lease), id, fence, owner)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, taskrt.ErrFenceLost
	}
	var cr bool
	if err := ex.Get(ctx, &cr, db.Q("SELECT cancel_requested FROM tasks WHERE id = ?"), id); err != nil {
		return false, err
	}
	return cr, nil
}

// SetProgress 见 taskrt.Store。
func (r *Repo) SetProgress(ctx context.Context, sc scope.Scope, id, fence uint64, owner string, pct int) error {
	if err := requirePlatform(sc); err != nil {
		return err
	}
	res, err := r.pool.Executor(ctx).Exec(ctx, db.Q("UPDATE tasks SET progress = ? "+fenced), pct, id, fence, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return taskrt.ErrFenceLost
	}
	return nil
}

// AddEvent 见 taskrt.Store。围栏在 INSERT…SELECT 的 WHERE 里：不满足则插入 0 行。
func (r *Repo) AddEvent(ctx context.Context, sc scope.Scope, id, fence uint64, owner, level, message string, data []byte) error {
	if err := requirePlatform(sc); err != nil {
		return err
	}
	eid, err := r.ids.New()
	if err != nil {
		return err
	}
	var d any
	if data != nil {
		d = string(data)
	}
	res, err := r.pool.Executor(ctx).Exec(ctx, db.Q("INSERT INTO task_events (uid, task_id, level, message, data_json) "+
		"SELECT ?, t.id, ?, ?, CAST(? AS JSON) FROM tasks t WHERE t.id = ? AND t.fence = ? AND t.lease_owner = ? AND t.status = 'running'"),
		eid, level, message, d, id, fence, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return taskrt.ErrFenceLost
	}
	return nil
}

// Complete 见 taskrt.Store。
func (r *Repo) Complete(ctx context.Context, sc scope.Scope, id, fence uint64, owner string, f taskrt.Finish) error {
	if err := requirePlatform(sc); err != nil {
		return err
	}
	switch f.Status {
	case taskrt.StatusSuccess, taskrt.StatusPartial, taskrt.StatusFailed, taskrt.StatusCanceled:
	default:
		return errors.New("taskrt/repository: Complete 的状态必须是终态")
	}
	var result any
	if len(f.Result) > 0 {
		result = string(f.Result)
	}
	return r.pool.WithTx(ctx, func(ctx context.Context) error {
		ex := r.pool.Executor(ctx)
		res, err := ex.Exec(ctx, db.Q("UPDATE tasks SET status = ?, result_json = ?, error_code = NULLIF(?, ''), error_message = NULLIF(?, ''), "+
			"progress = IF(? = 'success', 100, progress), finished_at = NOW(3), lease_owner = NULL, lease_until = NULL "+fenced),
			string(f.Status), result, f.ErrorCode, f.ErrorMessage, string(f.Status), id, fence, owner)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return taskrt.ErrFenceLost
		}
		level := taskrt.LevelInfo
		if f.Status == taskrt.StatusFailed {
			level = taskrt.LevelError
		}
		msg := "task finished: " + string(f.Status)
		if f.ErrorMessage != "" {
			msg += ": " + f.ErrorMessage
		}
		if len(msg) > 1024 {
			msg = msg[:1024]
		}
		return r.event(ctx, ex, id, level, msg, nil)
	})
}

const backoffExpr = "DATE_ADD(NOW(3), INTERVAL CAST(LEAST(? * POW(2, GREATEST(attempts, 1) - 1), ?) AS UNSIGNED) MICROSECOND)"

// Retry 见 taskrt.Store。已请求取消的任务不再重试，直接 canceled。
func (r *Repo) Retry(ctx context.Context, sc scope.Scope, id, fence uint64, owner, errCode, errMsg string, b taskrt.BackoffConfig) error {
	if err := requirePlatform(sc); err != nil {
		return err
	}
	return r.pool.WithTx(ctx, func(ctx context.Context) error {
		ex := r.pool.Executor(ctx)
		res, err := ex.Exec(ctx, db.Q("UPDATE tasks SET status = IF(cancel_requested = 1, 'canceled', 'pending'), finished_at = IF(cancel_requested = 1, NOW(3), NULL), "+
			"run_after = "+backoffExpr+", error_code = NULLIF(?, ''), error_message = NULLIF(?, ''), lease_owner = NULL, lease_until = NULL "+fenced),
			micros(b.Base), micros(b.Max), errCode, errMsg, id, fence, owner)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return taskrt.ErrFenceLost
		}
		msg := "attempt failed, will retry: " + errMsg
		if len(msg) > 1024 {
			msg = msg[:1024]
		}
		return r.event(ctx, ex, id, taskrt.LevelWarn, msg, nil)
	})
}

// Release 见 taskrt.Store。
func (r *Repo) Release(ctx context.Context, sc scope.Scope, id, fence uint64, owner string) error {
	if err := requirePlatform(sc); err != nil {
		return err
	}
	return r.pool.WithTx(ctx, func(ctx context.Context) error {
		ex := r.pool.Executor(ctx)
		res, err := ex.Exec(ctx, db.Q("UPDATE tasks SET status = IF(cancel_requested = 1, 'canceled', 'pending'), finished_at = IF(cancel_requested = 1, NOW(3), NULL), "+
			"attempts = GREATEST(attempts, 1) - 1, run_after = NOW(3), lease_owner = NULL, lease_until = NULL "+fenced), id, fence, owner)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return taskrt.ErrFenceLost
		}
		return r.event(ctx, ex, id, taskrt.LevelInfo, "task released by worker shutdown", nil)
	})
}

// Reap 见 taskrt.Store。围栏不变：回收后的任务不再是 running，旧持有者的写回因 status/fence 不符被拒；重新领取时 fence 再 +1。
func (r *Repo) Reap(ctx context.Context, sc scope.Scope, limit int, b taskrt.BackoffConfig) (taskrt.ReapResult, error) {
	if err := requirePlatform(sc); err != nil {
		return taskrt.ReapResult{}, err
	}
	if limit <= 0 {
		return taskrt.ReapResult{}, errors.New("taskrt/repository: Reap limit 非法")
	}
	var res taskrt.ReapResult
	err := r.pool.WithTx(ctx, func(ctx context.Context) error {
		ex := r.pool.Executor(ctx)
		var rows []struct {
			ID       uint64 `db:"id"`
			Attempts uint32 `db:"attempts"`
			Max      uint32 `db:"max_attempts"`
			Cancel   bool   `db:"cancel_requested"`
		}
		if err := ex.Select(ctx, &rows, db.Q("SELECT id, attempts, max_attempts, cancel_requested FROM tasks WHERE status = 'running' AND lease_until < NOW(3) "+
			"ORDER BY lease_until LIMIT ? FOR UPDATE SKIP LOCKED"), limit); err != nil {
			return err
		}
		for _, t := range rows {
			switch {
			case t.Cancel:
				if _, err := ex.Exec(ctx, db.Q("UPDATE tasks SET status = 'canceled', finished_at = NOW(3), lease_owner = NULL, lease_until = NULL WHERE id = ? AND status = 'running'"), t.ID); err != nil {
					return err
				}
				res.Canceled++
				if err := r.event(ctx, ex, t.ID, taskrt.LevelWarn, "lease expired; cancel was requested, task canceled", nil); err != nil {
					return err
				}
			case t.Attempts >= t.Max:
				if _, err := ex.Exec(ctx, db.Q("UPDATE tasks SET status = 'failed', finished_at = NOW(3), error_code = 'task.lease_expired', "+
					"error_message = 'lease expired and max attempts exhausted', lease_owner = NULL, lease_until = NULL WHERE id = ? AND status = 'running'"), t.ID); err != nil {
					return err
				}
				res.Failed++
				if err := r.event(ctx, ex, t.ID, taskrt.LevelError, "lease expired; max attempts exhausted, task failed", nil); err != nil {
					return err
				}
			default:
				if _, err := ex.Exec(ctx, db.Q("UPDATE tasks SET status = 'pending', run_after = "+backoffExpr+", error_code = 'task.lease_expired', "+
					"error_message = 'lease expired; worker presumed dead, requeued', lease_owner = NULL, lease_until = NULL WHERE id = ? AND status = 'running'"),
					micros(b.Base), micros(b.Max), t.ID); err != nil {
					return err
				}
				res.Requeued++
				if err := r.event(ctx, ex, t.ID, taskrt.LevelWarn, "lease expired; worker presumed dead, requeued", nil); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return res, err
}
