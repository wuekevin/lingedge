package repository

import (
	"context"
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// 错误。
var (
	ErrWrongRole = errors.New("taskrt/repository: 只接受 app 连接池")
	ErrNoScope   = errors.New("taskrt/repository: 缺少有效的 Scope")
)

// Repo 实现 taskrt.Store。
type Repo struct {
	pool db.Pool
	ids  *uid.Generator
}

var _ taskrt.Store = (*Repo)(nil)

// New 创建 Repository（app 池：Worker 常规任务与请求路径都用 app 账号，maint 池仅限 retention 类作业）。
func New(pool db.Pool, ids *uid.Generator) (*Repo, error) {
	if pool == nil || pool.Role() != db.RoleApp {
		return nil, ErrWrongRole
	}
	if ids == nil {
		return nil, errors.New("taskrt/repository: 缺少 uid 生成器")
	}
	return &Repo{pool: pool, ids: ids}, nil
}

// tenantFilter 把 Scope 转为查询条件：all=1 表示平台范围（不按租户过滤），否则按 tid 过滤。
func tenantFilter(sc scope.Scope) (all int, tid uint64, err error) {
	switch {
	case sc.IsZero():
		return 0, 0, ErrNoScope
	case sc.IsPlatform():
		return 1, 0, nil
	case sc.TenantID() == 0:
		return 0, 0, ErrNoScope
	}
	return 0, sc.TenantID(), nil
}

func requirePlatform(sc scope.Scope) error {
	if sc.IsZero() || !sc.IsPlatform() {
		return taskrt.ErrScope
	}
	return nil
}

type taskRow struct {
	ID              uint64     `db:"id"`
	UID             string     `db:"uid"`
	Type            string     `db:"type"`
	Status          string     `db:"status"`
	Priority        int        `db:"priority"`
	TenantID        *uint64    `db:"tenant_id"`
	TenantUID       *string    `db:"tenant_uid"`
	Payload         []byte     `db:"payload_json"`
	Result          []byte     `db:"result_json"`
	ErrorCode       *string    `db:"error_code"`
	ErrorMessage    *string    `db:"error_message"`
	Progress        int        `db:"progress"`
	Attempts        uint32     `db:"attempts"`
	MaxAttempts     uint32     `db:"max_attempts"`
	RunAfter        time.Time  `db:"run_after"`
	LeaseOwner      *string    `db:"lease_owner"`
	LeaseUntil      *time.Time `db:"lease_until"`
	Fence           uint64     `db:"fence"`
	IdempotencyKey  *string    `db:"idempotency_key"`
	ParentTaskID    *uint64    `db:"parent_task_id"`
	ParentTaskUID   *string    `db:"parent_uid"`
	RequestID       string     `db:"request_id"`
	CreatedByUserID *uint64    `db:"created_by_user_id"`
	CreatedByUID    *string    `db:"created_by_uid"`
	CancelRequested bool       `db:"cancel_requested"`
	CreatedAt       time.Time  `db:"created_at"`
	StartedAt       *time.Time `db:"started_at"`
	FinishedAt      *time.Time `db:"finished_at"`
}

func (r taskRow) task() taskrt.Task {
	return taskrt.Task{
		ID: r.ID, UID: r.UID, Type: taskrt.Type(r.Type), Status: taskrt.Status(r.Status), Priority: r.Priority,
		TenantID: r.TenantID, TenantUID: r.TenantUID, Payload: r.Payload, Result: r.Result,
		ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage, Progress: r.Progress, Attempts: r.Attempts, MaxAttempts: r.MaxAttempts,
		RunAfter: r.RunAfter, LeaseOwner: r.LeaseOwner, LeaseUntil: r.LeaseUntil, Fence: r.Fence, IdempotencyKey: r.IdempotencyKey,
		ParentTaskID: r.ParentTaskID, ParentTaskUID: r.ParentTaskUID, RequestID: r.RequestID,
		CreatedByUserID: r.CreatedByUserID, CreatedByUID: r.CreatedByUID, CancelRequested: r.CancelRequested,
		CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
}

const (
	taskCols = "t.id, t.uid, t.type, t.status, t.priority, t.tenant_id, tn.uid AS tenant_uid, t.payload_json, t.result_json, t.error_code, t.error_message, " +
		"t.progress, t.attempts, t.max_attempts, t.run_after, t.lease_owner, t.lease_until, t.fence, t.idempotency_key, t.parent_task_id, pt.uid AS parent_uid, " +
		"t.request_id, t.created_by_user_id, u.uid AS created_by_uid, t.cancel_requested, t.created_at, t.started_at, t.finished_at "
	taskFrom = "FROM tasks t LEFT JOIN tenants tn ON tn.id = t.tenant_id LEFT JOIN tasks pt ON pt.id = t.parent_task_id LEFT JOIN users u ON u.id = t.created_by_user_id "
	visible  = "(? = 1 OR t.tenant_id = ?)"
)

// Insert 见 taskrt.Store。
func (r *Repo) Insert(ctx context.Context, sc scope.Scope, n taskrt.NewTask) (taskrt.Task, bool, error) {
	all, tid, err := tenantFilter(sc)
	if err != nil {
		return taskrt.Task{}, false, err
	}
	var tenant any
	switch {
	case all == 0:
		tenant = tid // 租户 Scope：强制为 Scope 的租户，忽略调用方传入的 TenantID
	case n.TenantID != nil:
		tenant = *n.TenantID
	}
	var created bool
	var out taskrt.Task
	err = r.pool.WithTx(ctx, func(ctx context.Context) error {
		ex := r.pool.Executor(ctx)
		if n.ParentTaskID != nil { // 父任务必须对调用方可见（防止把子任务挂到他租户的任务下）
			var c int64
			if err := ex.Get(ctx, &c, db.Q("SELECT COUNT(*) FROM tasks t WHERE t.id = ? AND "+visible), *n.ParentTaskID, all, tid); err != nil {
				return err
			}
			if c == 0 {
				return db.ErrNotFound
			}
		}
		var runAfter, idem, parent, creator any
		if n.RunAfter != nil {
			runAfter = *n.RunAfter
		}
		if n.IdempotencyKey != nil {
			idem = *n.IdempotencyKey
		}
		if n.ParentTaskID != nil {
			parent = *n.ParentTaskID
		}
		if n.CreatedByUserID != nil {
			creator = *n.CreatedByUserID
		}
		res, err := ex.Exec(ctx, db.Q("INSERT INTO tasks (uid, tenant_id, type, status, priority, payload_json, max_attempts, run_after, idempotency_key, parent_task_id, request_id, created_by_user_id) "+
			"VALUES (?, ?, ?, 'pending', ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP(3)), ?, ?, ?, ?)"),
			n.UID, tenant, string(n.Type), n.Priority, string(n.Payload), n.MaxAttempts, runAfter, idem, parent, n.RequestID, creator)
		if err != nil {
			if db.IsDuplicate(err) && n.IdempotencyKey != nil {
				return errDup
			}
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		created = true
		var row taskRow
		if err := ex.Get(ctx, &row, db.Q("SELECT "+taskCols+taskFrom+"WHERE t.id = ?"), uint64(id)); err != nil {
			return err
		}
		out = row.task()
		return r.event(ctx, ex, out.ID, taskrt.LevelInfo, "task enqueued", nil)
	})
	if errors.Is(err, errDup) { // 事务已回滚；确定性入队：返回既有任务（仅当对调用方可见）
		var row taskRow
		gerr := r.pool.Executor(ctx).Get(ctx, &row, db.Q("SELECT "+taskCols+taskFrom+"WHERE t.type = ? AND t.idempotency_key = ? AND "+visible), string(n.Type), *n.IdempotencyKey, all, tid)
		if errors.Is(gerr, db.ErrNotFound) {
			return taskrt.Task{}, false, taskrt.ErrIdempotencyKeyTaken
		}
		if gerr != nil {
			return taskrt.Task{}, false, gerr
		}
		return row.task(), false, nil
	}
	if err != nil {
		return taskrt.Task{}, false, err
	}
	return out, created, nil
}

var errDup = errors.New("duplicate idempotency key")

// event 追加一条事件（调用方保证 id 有效；在事务内调用以与状态变更原子）。
func (r *Repo) event(ctx context.Context, ex db.Executor, taskID uint64, level, msg string, data []byte) error {
	id, err := r.ids.New()
	if err != nil {
		return err
	}
	var d any
	if data != nil {
		d = string(data)
	}
	_, err = ex.Exec(ctx, db.Q("INSERT INTO task_events (uid, task_id, level, message, data_json) VALUES (?, ?, ?, ?, ?)"), id, taskID, level, msg, d)
	return err
}

// Get 见 taskrt.Store。
func (r *Repo) Get(ctx context.Context, sc scope.Scope, taskUID string) (taskrt.Task, error) {
	all, tid, err := tenantFilter(sc)
	if err != nil {
		return taskrt.Task{}, err
	}
	var row taskRow
	if err := r.pool.Executor(ctx).Get(ctx, &row, db.Q("SELECT "+taskCols+taskFrom+"WHERE t.uid = ? AND "+visible), taskUID, all, tid); err != nil {
		return taskrt.Task{}, err
	}
	return row.task(), nil
}

const listWhere = "WHERE " + visible + " AND (? = '' OR t.type = ?) AND (? = '' OR t.status = ?) AND (? = '' OR t.request_id = ?) " +
	"AND (? = '' OR JSON_UNQUOTE(JSON_EXTRACT(t.payload_json, '$.resource_uid')) = ?) "

// List 见 taskrt.Store。
func (r *Repo) List(ctx context.Context, sc scope.Scope, f taskrt.Filter) ([]taskrt.Task, int64, error) {
	all, tid, err := tenantFilter(sc)
	if err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 200 || f.Offset < 0 {
		return nil, 0, errors.New("taskrt/repository: 分页参数非法")
	}
	ex := r.pool.Executor(ctx)
	typ, st := string(f.Type), string(f.Status)
	args := []any{all, tid, typ, typ, st, st, f.RequestID, f.RequestID, f.ResourceUID, f.ResourceUID}
	var total int64
	if err := ex.Get(ctx, &total, db.Q("SELECT COUNT(*) FROM tasks t "+listWhere), args...); err != nil {
		return nil, 0, err
	}
	args = append(args, f.Limit, f.Offset)
	var rows []taskRow
	if f.Desc {
		err = ex.Select(ctx, &rows, db.Q("SELECT "+taskCols+taskFrom+listWhere+"ORDER BY t.created_at DESC, t.id DESC LIMIT ? OFFSET ?"), args...)
	} else {
		err = ex.Select(ctx, &rows, db.Q("SELECT "+taskCols+taskFrom+listWhere+"ORDER BY t.created_at ASC, t.id ASC LIMIT ? OFFSET ?"), args...)
	}
	if err != nil {
		return nil, 0, err
	}
	out := make([]taskrt.Task, len(rows))
	for i := range rows {
		out[i] = rows[i].task()
	}
	return out, total, nil
}

type eventRow struct {
	UID       string    `db:"uid"`
	Level     string    `db:"level"`
	Message   string    `db:"message"`
	Data      []byte    `db:"data_json"`
	CreatedAt time.Time `db:"created_at"`
}

// ListEvents 见 taskrt.Store。
func (r *Repo) ListEvents(ctx context.Context, sc scope.Scope, taskUID string, offset, limit int) ([]taskrt.Event, int64, error) {
	all, tid, err := tenantFilter(sc)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 200 || offset < 0 {
		return nil, 0, errors.New("taskrt/repository: 分页参数非法")
	}
	ex := r.pool.Executor(ctx)
	var id uint64
	if err := ex.Get(ctx, &id, db.Q("SELECT t.id FROM tasks t WHERE t.uid = ? AND "+visible), taskUID, all, tid); err != nil {
		return nil, 0, err
	}
	var total int64
	if err := ex.Get(ctx, &total, db.Q("SELECT COUNT(*) FROM task_events WHERE task_id = ?"), id); err != nil {
		return nil, 0, err
	}
	var rows []eventRow
	if err := ex.Select(ctx, &rows, db.Q("SELECT uid, level, message, data_json, created_at FROM task_events WHERE task_id = ? ORDER BY id ASC LIMIT ? OFFSET ?"), id, limit, offset); err != nil {
		return nil, 0, err
	}
	out := make([]taskrt.Event, len(rows))
	for i, e := range rows {
		out[i] = taskrt.Event{UID: e.UID, Level: e.Level, Message: e.Message, Data: e.Data, CreatedAt: e.CreatedAt}
	}
	return out, total, nil
}

// RequestCancel 见 taskrt.Store。
func (r *Repo) RequestCancel(ctx context.Context, sc scope.Scope, taskUID string) (taskrt.Task, error) {
	all, tid, err := tenantFilter(sc)
	if err != nil {
		return taskrt.Task{}, err
	}
	err = r.pool.WithTx(ctx, func(ctx context.Context) error {
		ex := r.pool.Executor(ctx)
		var cur struct {
			ID     uint64 `db:"id"`
			Status string `db:"status"`
		}
		if err := ex.Get(ctx, &cur, db.Q("SELECT t.id, t.status FROM tasks t WHERE t.uid = ? AND "+visible+" FOR UPDATE"), taskUID, all, tid); err != nil {
			return err
		}
		switch taskrt.Status(cur.Status) {
		case taskrt.StatusPending: // 无人持有：直接终止
			if _, err := ex.Exec(ctx, db.Q("UPDATE tasks SET status = 'canceled', cancel_requested = 1, finished_at = NOW(3) WHERE id = ? AND status = 'pending'"), cur.ID); err != nil {
				return err
			}
			return r.event(ctx, ex, cur.ID, taskrt.LevelInfo, "task canceled before start", nil)
		case taskrt.StatusRunning: // 协作式：由执行方在检查点终止
			if _, err := ex.Exec(ctx, db.Q("UPDATE tasks SET cancel_requested = 1 WHERE id = ? AND status = 'running'"), cur.ID); err != nil {
				return err
			}
			return r.event(ctx, ex, cur.ID, taskrt.LevelInfo, "cancel requested", nil)
		default:
			return taskrt.ErrNotCancelable
		}
	})
	if err != nil {
		return taskrt.Task{}, err
	}
	return r.Get(ctx, sc, taskUID)
}
