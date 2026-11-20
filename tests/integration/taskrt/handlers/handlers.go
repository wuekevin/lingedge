// Package handlers 是任务运行时集成测试用的真实处理函数：它们把每次执行写入真实 MySQL 的 exec_log 表，
// 因此「每任务恰好一次」「kill -9 后重跑」等断言基于数据库里的事实而不是进程内计数。
// 同一份实现既在测试进程内的 Worker 中使用，也在被 kill -9 的 Worker 子进程（../workerproc）中使用。
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
)

// ExecLogDDL 是测试库里的执行记录表（§ 由测试替换为库名）（由测试以 root 创建并授权给 lingedge_app）。
const ExecLogDDL = "CREATE TABLE §.exec_log (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, task_uid CHAR(26) NOT NULL, owner VARCHAR(128) NOT NULL, " +
	"fence BIGINT UNSIGNED NOT NULL, attempt INT UNSIGNED NOT NULL, at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3), KEY idx_exec_task (task_uid)) ENGINE=InnoDB"

// Spec 是任务 payload 里的行为开关（payload 只含这些非敏感的整型/布尔量）。
type Spec struct {
	SleepMS          int  `json:"sleep_ms"`
	IgnoreCtx        bool `json:"ignore_ctx"`         // 睡眠期间忽略 ctx 取消（模拟不配合的处理函数/僵尸）
	FailUntilAttempt int  `json:"fail_until_attempt"` // attempts <= N 时返回可重试错误
	Permanent        bool `json:"permanent"`
	Panic            bool `json:"panic"`
	Partial          bool `json:"partial"`
	Loop             bool `json:"loop"` // 协作式取消演示：循环检查点直到被取消
	Leak             bool `json:"leak"` // 返回含敏感明文的错误，验证写入前脱敏
}

// Exec 返回测试处理函数。pool 为 app 池，owner 仅用于记录。
func Exec(pool db.Pool, owner string) taskrt.Handler {
	return func(ctx context.Context, run *taskrt.Run) (taskrt.Result, error) {
		var sp Spec
		if len(run.Task.Payload) > 0 {
			if err := json.Unmarshal(run.Task.Payload, &sp); err != nil {
				return taskrt.Result{}, taskrt.Permanent("task.bad_payload", err)
			}
		}
		if _, err := pool.Executor(ctx).Exec(ctx, db.Q("INSERT INTO exec_log (task_uid, owner, fence, attempt) VALUES (?, ?, ?, ?)"),
			run.Task.UID, owner, run.Task.Fence, run.Task.Attempts); err != nil {
			return taskrt.Result{}, err
		}
		switch {
		case sp.Panic:
			panic("handler exploded")
		case sp.Permanent:
			return taskrt.Result{}, taskrt.Permanent("test.permanent", errors.New("permanent failure"))
		case sp.Leak:
			return taskrt.Result{}, errors.New("upstream said password=hunter2 and Bearer abcdefghijklmnop")
		case int(run.Task.Attempts) <= sp.FailUntilAttempt:
			return taskrt.Result{}, errors.New("transient failure")
		}
		if sp.Loop {
			for i := 0; i < 400; i++ {
				if err := run.Checkpoint(ctx); err != nil {
					return taskrt.Result{}, err
				}
				_ = run.Progress(ctx, i%100)
				time.Sleep(25 * time.Millisecond)
			}
		}
		if sp.SleepMS > 0 {
			if sp.IgnoreCtx {
				time.Sleep(time.Duration(sp.SleepMS) * time.Millisecond)
			} else {
				select {
				case <-time.After(time.Duration(sp.SleepMS) * time.Millisecond):
				case <-ctx.Done():
					return taskrt.Result{}, context.Cause(ctx)
				}
			}
		}
		_ = run.Event(ctx, taskrt.LevelInfo, "handler done", map[string]any{"owner_ref": owner})
		return taskrt.Result{Partial: sp.Partial, Data: map[string]any{"by": owner, "attempt": run.Task.Attempts}}, nil
	}
}
