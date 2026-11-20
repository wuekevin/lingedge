package serverintegration

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	schedrepo "github.com/wuekevin/lingedge/internal/platform/schedrt/repository"
)

// job_locks 租约的真实并发语义（真实 MySQL，lingedge_app 账号）。
func TestJobLocksSemantics(t *testing.T) {
	e := newEnv(t, all)
	locks, err := schedrepo.NewLocks(e.app)
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxT(t)

	// 并发获取同一名称：恰好一个成功。
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, err := locks.Acquire(ctx, "race.one", fmt.Sprintf("h%d", i), 5*time.Second, false); err != nil {
				t.Errorf("Acquire: %v", err)
			} else if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("16 个并发获取者应恰有 1 个成功: %d", wins.Load())
	}

	// 回归：大量并发的「首次获取」（行尚不存在）不得死锁（实测 ERROR 1213 的修复）。
	for round := 0; round < 3; round++ {
		var ok atomic.Int32
		for n := 0; n < 8; n++ {
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, got, err := locks.Acquire(ctx, fmt.Sprintf("fresh.%d.%d", round, n), fmt.Sprintf("g%d", g), 5*time.Second, false); err != nil {
						t.Errorf("并发首次获取失败: %v", err)
					} else if got {
						ok.Add(1)
					}
				}()
			}
		}
		wg.Wait()
		if ok.Load() != 8 {
			t.Fatalf("第 %d 轮：8 个名称各应恰有 1 个持有者: %d", round, ok.Load())
		}
	}

	// allowSelf：持有者续约成功且 fence 递增；allowSelf=false 时自己也不能在未过期期间重复获取（窗口租约语义）。
	f1, ok, _ := locks.Acquire(ctx, "self", "me", 5*time.Second, true)
	f2, ok2, _ := locks.Acquire(ctx, "self", "me", 5*time.Second, true)
	if !ok || !ok2 || f2 != f1+1 {
		t.Fatalf("续约: ok=%v/%v fence %d → %d", ok, ok2, f1, f2)
	}
	if _, ok, _ := locks.Acquire(ctx, "self", "me", 5*time.Second, false); ok {
		t.Fatal("allowSelf=false 时持有者在租约有效期内不得重复获取")
	}
	if _, ok, _ := locks.Acquire(ctx, "self", "other", 5*time.Second, true); ok {
		t.Fatal("他人持有期间不可获取")
	}

	// 过期接管：旧持有者随后的 Hold 失败，fence 递增。
	f1, _, _ = locks.Acquire(ctx, "expire", "old", 300*time.Millisecond, true)
	time.Sleep(500 * time.Millisecond)
	f2, ok, _ = locks.Acquire(ctx, "expire", "new", 5*time.Second, true)
	if !ok || f2 != f1+1 {
		t.Fatalf("过期后应可接管且 fence 递增: ok=%v %d→%d", ok, f1, f2)
	}
	if held, _ := locks.Hold(ctx, "expire", "old", time.Minute); held {
		t.Fatal("旧持有者不得再延长租约")
	}
	li, err := locks.Get(ctx, "expire")
	if err != nil || li.Holder != "new" || li.Expired {
		t.Fatalf("租约行: %+v %v", li, err)
	}

	// Release：立即可被接管；非持有者的 Release 无效。
	_ = locks.Release(ctx, "expire", "old")
	if li, _ := locks.Get(ctx, "expire"); li.Expired {
		t.Fatal("非持有者的 Release 不得影响租约")
	}
	_ = locks.Release(ctx, "expire", "new")
	if _, ok, _ := locks.Acquire(ctx, "expire", "third", time.Second, false); !ok {
		t.Fatal("Release 后应立即可被获取")
	}
	// Hold 可缩短：置为 0 即立即过期。
	if held, _ := locks.Hold(ctx, "expire", "third", 0); !held {
		t.Fatal("持有者 Hold 应成功")
	}
	if _, ok, _ := locks.Acquire(ctx, "expire", "fourth", time.Second, false); !ok {
		t.Fatal("Hold(0) 后应立即可被获取")
	}
}
