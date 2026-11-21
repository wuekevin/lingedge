package registry

import (
	"context"
	"errors"
	"testing"
)

// Task.Extend（Story 1.33）：求解出依赖闭包之后追加组件锁；非阻塞；失败时已有的锁保持、新得的全部释放；OpenTx 之后不能再追加；journal 头登记全部组件。
func TestTaskExtend(t *testing.T) {
	r := New(Layout{Root: t.TempDir() + "/tools"}, Options{})
	ctx := context.Background()
	task, err := r.Begin(ctx, TaskRequest{Components: []string{"node"}})
	if err != nil {
		t.Fatal(err)
	}
	defer task.Release()
	other, err := r.LockComponents([]string{"openresty"})
	if err != nil {
		t.Fatal(err)
	}
	// openresty 被别人持有 → LockedError，原有的 node 锁不受影响，且不会残留 b/c 之类部分新锁。
	err = task.Extend([]string{"b-comp", "openresty", "a-comp"})
	var le *LockedError
	if !errors.As(err, &le) || le.Name != "openresty" {
		t.Fatalf("应为 LockedError(openresty): %v", err)
	}
	if got := task.Components(); len(got) != 1 || got[0] != "node" {
		t.Fatalf("失败的 Extend 不得改变任务的组件: %v", got)
	}
	if lk, err := r.LockComponents([]string{"a-comp", "b-comp"}); err != nil {
		t.Fatalf("失败的 Extend 应释放它已经拿到的锁: %v", err)
	} else {
		lk.Release()
	}
	other.Release()
	// 成功：字典序合并、重复项忽略。
	if err := task.Extend([]string{"openresty", "node", "a-comp"}); err != nil {
		t.Fatal(err)
	}
	if got := task.Components(); len(got) != 3 || got[0] != "a-comp" || got[1] != "node" || got[2] != "openresty" {
		t.Fatalf("组件: %v", got)
	}
	if _, err := r.LockComponents([]string{"a-comp"}); err == nil {
		t.Fatal("新加的锁应被持有")
	}
	if err := task.Extend([]string{"Bad Name"}); err == nil {
		t.Fatal("非法组件名应被拒绝")
	}
	j, err := task.OpenTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := r.ReadTx(j.ID())
	if err != nil || len(tx.Header.Components) != 3 {
		t.Fatalf("journal 头应登记全部 3 个组件: %+v %v", tx.Header.Components, err)
	}
	if err := task.Extend([]string{"late"}); err == nil {
		t.Fatal("OpenTx 之后不能再追加")
	}
	task.Release()
	if lk, err := r.LockComponents([]string{"a-comp", "node", "openresty"}); err != nil {
		t.Fatalf("Release 应释放全部锁: %v", err)
	} else {
		lk.Release()
	}
	if err := task.Extend([]string{"x"}); err == nil {
		t.Fatal("已释放的任务不能 Extend")
	}
}
