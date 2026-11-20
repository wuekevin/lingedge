package taskrtintegration

import (
	"context"
	"fmt"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	"github.com/wuekevin/lingedge/internal/platform/taskrt/tasktest"
)

// recordingTB 把 Fatalf 转为可断言的 panic，用来验证 AssertIdempotent 对「不幂等」的处理函数确实会失败（负向样本）。
type recordingTB struct {
	testing.TB
	failed string
}

func (r *recordingTB) Helper() {}
func (r *recordingTB) Fatalf(f string, a ...any) {
	r.failed = fmt.Sprintf(f, a...)
	panic(r)
}
func (r *recordingTB) Fatal(a ...any) { r.Fatalf("%v", a...) }

func expectFailure(t *testing.T, f func(tb testing.TB)) string {
	t.Helper()
	rec := &recordingTB{TB: t}
	func() {
		defer func() {
			if r := recover(); r != nil && r != any(rec) {
				panic(r)
			}
		}()
		f(rec)
	}()
	return rec.failed
}

// AC5：处理函数幂等性测试助手随框架提供。对真实数据库做断言：幂等的处理函数（以任务 uid 去重）通过；
// 「每次执行都追加一行」的不幂等处理函数被助手判定失败。
func TestAssertIdempotentHelper(t *testing.T) {
	e := newEnv(t)
	e.root(t, "CREATE TABLE §.effects (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, task_uid CHAR(26) NOT NULL, UNIQUE KEY uk_task (task_uid)) ENGINE=InnoDB")
	e.root(t, "CREATE TABLE §.effects_dup (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, task_uid CHAR(26) NOT NULL) ENGINE=InnoDB")
	e.root(t, "GRANT SELECT, INSERT, UPDATE ON §.effects TO 'lingedge_app'@'%'")
	e.root(t, "GRANT SELECT, INSERT ON §.effects_dup TO 'lingedge_app'@'%'")
	task := taskrt.Task{UID: e.ids.MustNew(), Type: taskrt.TypeRetentionCleanup, Payload: []byte(`{}`)}

	idempotent := func(ctx context.Context, run *taskrt.Run) (taskrt.Result, error) {
		_, err := e.app.Executor(ctx).Exec(ctx, db.Q("INSERT INTO effects (task_uid) VALUES (?) ON DUPLICATE KEY UPDATE task_uid = task_uid"), run.Task.UID)
		return taskrt.Result{}, err
	}
	notIdempotent := func(ctx context.Context, run *taskrt.Run) (taskrt.Result, error) {
		_, err := e.app.Executor(ctx).Exec(ctx, db.Q("INSERT INTO effects_dup (task_uid) VALUES (?)"), run.Task.UID)
		return taskrt.Result{}, err
	}
	observe := func(table string) func(ctx context.Context) (any, error) {
		return func(ctx context.Context) (any, error) {
			var n int64
			var err error
			if table == "effects" {
				err = e.app.Executor(ctx).Get(ctx, &n, db.Q("SELECT COUNT(*) FROM effects"))
			} else {
				err = e.app.Executor(ctx).Get(ctx, &n, db.Q("SELECT COUNT(*) FROM effects_dup"))
			}
			return n, err
		}
	}

	tasktest.AssertIdempotent(t, idempotent, tasktest.Scenario{Task: task, Observe: observe("effects"), Concurrent: 6})
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.effects"); n != 1 {
		t.Fatalf("幂等处理函数应恰好 1 行，实际 %d", n)
	}
	msg := expectFailure(t, func(tb testing.TB) {
		tasktest.AssertIdempotent(tb, notIdempotent, tasktest.Scenario{Task: task, Observe: observe("effects_dup")})
	})
	if msg == "" {
		t.Fatal("助手必须判定「每次执行追加一行」的处理函数不幂等")
	}
	t.Logf("负向样本被正确拒绝: %s", msg)
	if msg := expectFailure(t, func(tb testing.TB) { tasktest.AssertIdempotent(tb, idempotent, tasktest.Scenario{Task: task}) }); msg == "" {
		t.Fatal("缺少 Observe 必须失败（否则无法判断效果）")
	}
}
