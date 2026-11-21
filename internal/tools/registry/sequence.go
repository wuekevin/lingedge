package registry

import "context"

// SequenceStore 与 manifest.SequenceStore 结构相同（registry 不依赖 manifest，依赖图 registry → security）。
type SequenceStore interface {
	Max(channel string) (int64, bool, error)
	Advance(channel string, seq int64) error
}

// LockedSequence 把一个 SequenceStore（通常是 manifest.FileSequenceStore{Path: Layout.SequenceFile()}）包进全局锁：
// 每次 Max/Advance 都在全局状态锁的短临界区内完成。
//
// 为什么需要：FileSequenceStore.Advance 是「读 → 比较 → 原子写」，原子写只保证文件不会被写坏，不保证两个进程的读-改-写不丢更新——
// 进程 A、B 同时读到 5，A 写 7，B 写 6，sequence 就回退了（回滚防护被绕过）。1.27/1.28 约定「无跨进程锁，靠 1.29 的全局任务锁」，这里就是落点。
// 持有组件锁（Task）并不够，因为不同组件的任务可以并发，却共用同一份 trust/state。
// 不可重入：不要在 WithGlobal/UpdateState 的回调里使用。
type LockedSequence struct {
	R     *Registry
	Inner SequenceStore
	Ctx   context.Context //nolint:containedctx // 满足 SequenceStore 无 ctx 的接口；缺省 Background
}

func (l LockedSequence) ctx() context.Context {
	if l.Ctx != nil {
		return l.Ctx
	}
	return context.Background()
}

// Max 实现 SequenceStore。
func (l LockedSequence) Max(channel string) (n int64, ok bool, err error) {
	err = l.R.WithGlobal(l.ctx(), func() error { n, ok, err = l.Inner.Max(channel); return err })
	return n, ok, err
}

// Advance 实现 SequenceStore。
func (l LockedSequence) Advance(channel string, seq int64) error {
	return l.R.WithGlobal(l.ctx(), func() error { return l.Inner.Advance(channel, seq) })
}
