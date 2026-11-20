// Package tasktest 是任务处理函数的测试助手（Story 1.14 AC5：处理函数幂等性测试助手随框架提供）。
//
// 每个任务类型的处理函数都必须幂等（ADR-019 §5：至少一次执行 + 幂等 = 效果恰好一次）。AssertIdempotent 把同一个任务重复投递给处理函数——
// 先顺序两次，再并发多次（模拟租约过期回收后与旧持有者的重叠执行）——并断言「可观测效果」不变。可观测效果由调用方提供的 Observe 给出
// （通常是对真实数据库做一次查询并汇总），因此助手不依赖任何 Mock。
package tasktest

import (
	"context"
	"reflect"
	"sync"

	"github.com/wuekevin/lingedge/internal/platform/taskrt"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
)

// TB 是 AssertIdempotent 需要的 *testing.T/testing.TB 子集（非测试代码不得 import testing，ADR-023 §A.6，故在此声明接口；*testing.T 直接满足）。
type TB interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
}

// Scenario 描述一次幂等性检查。
type Scenario struct {
	// Task 是被重复投递的任务（须带 ID/UID/Type/Payload 等处理函数会读取的字段）。
	Task taskrt.Task
	// Scope 缺省：租户任务用 tenantscope.FromTask，平台任务用 PlatformForJob。
	Scope tenantscope.Scope
	// Observe 返回处理函数产生的可观测效果快照（如受影响表的行数/内容摘要）；必须可用 reflect.DeepEqual 比较。
	Observe func(ctx context.Context) (any, error)
	// Concurrent 是并发重叠执行的份数（缺省 4）。
	Concurrent int
}

// AssertIdempotent 对 h 做幂等性检查：执行 1 次得到效果 E1；再执行 1 次（重投递）效果必须仍为 E1；
// 再并发执行 Concurrent 次，效果必须仍为 E1，且每次执行都不得返回错误。
func AssertIdempotent(t TB, h taskrt.Handler, s Scenario) {
	t.Helper()
	if s.Observe == nil {
		t.Fatal("tasktest: 必须提供 Observe（否则无法判断效果是否重复）")
	}
	if s.Concurrent <= 0 {
		s.Concurrent = 4
	}
	ctx := context.Background()
	scp := s.Scope
	if scp.IsZero() {
		var err error
		if s.Task.TenantID != nil {
			scp, err = tenantscope.FromTask(*s.Task.TenantID, 0)
			if err != nil {
				t.Fatalf("tasktest: 构造租户 Scope: %v", err)
			}
		} else {
			scp = tenantscope.PlatformForJob(0)
		}
	}
	exec := func() (taskrt.Result, error) {
		run, _ := taskrt.NewRun(s.Task, scp)
		return h(ctx, run)
	}

	res1, err := exec()
	if err != nil {
		t.Fatalf("tasktest: 首次执行失败: %v", err)
	}
	e1, err := s.Observe(ctx)
	if err != nil {
		t.Fatalf("tasktest: Observe 失败: %v", err)
	}

	res2, err := exec()
	if err != nil {
		t.Fatalf("tasktest: 重投递执行失败（幂等的处理函数重复执行不应报错）: %v", err)
	}
	e2, err := s.Observe(ctx)
	if err != nil {
		t.Fatalf("tasktest: Observe 失败: %v", err)
	}
	if !reflect.DeepEqual(e1, e2) {
		t.Fatalf("tasktest: 处理函数不幂等：重投递后效果改变\n首次后: %#v\n重投递后: %#v", e1, e2)
	}
	if res1.Partial != res2.Partial {
		t.Fatalf("tasktest: 重投递的结果状态不一致（Partial %v → %v）", res1.Partial, res2.Partial)
	}

	var wg sync.WaitGroup
	errs := make([]error, s.Concurrent)
	for i := 0; i < s.Concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = exec()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("tasktest: 并发重叠执行 #%d 失败: %v", i, err)
		}
	}
	e3, err := s.Observe(ctx)
	if err != nil {
		t.Fatalf("tasktest: Observe 失败: %v", err)
	}
	if !reflect.DeepEqual(e1, e3) {
		t.Fatalf("tasktest: 处理函数不幂等：并发重叠执行后效果改变\n首次后: %#v\n并发后: %#v", e1, e3)
	}
}
