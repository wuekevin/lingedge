package main

import "github.com/wuekevin/lingedge/internal/platform/taskrt"

// assembleTaskRegistry 是任务类型注册表（白名单，Story 1.14 AC1/AC7）的唯一装配点。
//
// 口径：只登记「已有真实处理函数」的类型；未登记的类型入队即被拒绝，Worker 也只领取已登记类型。
// 本 Story（任务运行时内核）本身没有任何必须由内核执行的 OpenAPI TaskType——内部周期作业（tasks.reap 等）不属 TaskType（AA-8，1.15 负责），
// 因此这里当前不登记任何类型：运行时已就绪（入队/领取/租约/围栏/回收/取消全部可用且有测试），各业务 Story 在完成时在此追加登记，例如：
//
//	reg.Register(taskrt.Spec{Type: taskrt.TypeCachePurgeURL, Handler: cache.PurgeURLHandler(...)})
//
// 「x-phase: M1 所需类型在其 Story 完成时均已登记」（M14）的执行点是 tasks_test.go 的 requiredRegistered：
// 每个业务 Story 完成时把自己负责的类型加进该清单（并在这里登记），测试即保证「清单内类型必已登记」且「已登记 ⊆ OpenAPI TaskType」。
func assembleTaskRegistry() (*taskrt.Registry, error) {
	return taskrt.NewRegistry(), nil
}
