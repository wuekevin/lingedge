package registry

import (
	"context"
	"os"
	"sync"
)

// Task 是一次写命令（install/update/rollback/uninstall/restore/backup create/service restart）持有的资源：
// 组件锁（整个操作期间持有，§16）+ 事务 ID（同时用作 staging/<tx> 与 journal 的名字）+ 可选的事务日志。
type Task struct {
	r     *Registry
	locks *LockSet
	tx    string
	comps []string

	mu      sync.Mutex
	journal *Journal
	done    bool
}

// TaskRequest 描述要做的事。
type TaskRequest struct {
	// Components 是本次操作涉及的全部组件（会去重并按字典序加锁）。
	Components []string
}

// Begin 是写命令的启动入口（AC3、AC4）：
//
//  1. 组件锁按字典序 LOCK_EX|LOCK_NB 依次获取；任一把被占用 → *LockedError（退出码 6，含持有者 pid/run_id/command），已得的锁全部释放；
//  2. 持锁后扫描 journal：存在已死亡且未结束的事务 → *UnfinishedError（退出码 13，提示 lingedge recover），同样释放锁。
//     先锁后扫：真正在做同一组件的活事务一定持有组件锁，会在第 1 步就以 6 被拒，不会被当成「未完成」；
//     其他组件上的活事务（journal 被其持有者 flock）不算未完成。
//
// 返回的 Task 须在结束时 Release（defer）。
func (r *Registry) Begin(ctx context.Context, req TaskRequest) (*Task, error) {
	locks, err := r.LockComponents(req.Components)
	if err != nil {
		return nil, err
	}
	un, err := r.Unfinished(ctx)
	if err != nil {
		locks.Release()
		return nil, err
	}
	if len(un) > 0 {
		locks.Release()
		return nil, &UnfinishedError{Txs: un}
	}
	return &Task{r: r, locks: locks, tx: NewTxID(r.now()), comps: locks.Names()}, nil
}

// Check 做与 Begin 相同的检查（取锁 + 扫描 journal）但立即释放，不建立任务。
// 用于尚未实现的写命令：把「被占用/有未完成事务」的真实结果报告出来，而不假装成功。
func (r *Registry) Check(ctx context.Context, components []string) error {
	t, err := r.Begin(ctx, TaskRequest{Components: components})
	if err != nil {
		return err
	}
	t.Release()
	return nil
}

// TxID 返回事务 ID（downloader.Params.TxID 与 journal 同名）。
func (t *Task) TxID() string { return t.tx }

// Components 返回已持锁的组件（字典序）。
func (t *Task) Components() []string { return append([]string(nil), t.comps...) }

// StagingRoot 返回 downloader.Params.StagingRoot 应使用的目录（<Root>/staging；其下的 <tx>/ 由 downloader 创建）。
func (t *Task) StagingRoot() string { return t.r.L.StagingDir() }

// OpenTx 创建本任务的事务日志（open 记录已 fsync）。只能调用一次。
func (t *Task) OpenTx(ctx context.Context) (*Journal, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil, newErr(KindJournalClosed, "任务已释放")
	}
	if t.journal != nil {
		return nil, newErr(KindJournalProtocol, "本任务已打开事务日志")
	}
	j, err := t.r.CreateJournal(ctx, t.tx, t.comps)
	if err != nil {
		return nil, err
	}
	t.journal = j
	return j, nil
}

// Journal 返回已打开的事务日志（未打开则 nil）。
func (t *Task) Journal() *Journal { t.mu.Lock(); defer t.mu.Unlock(); return t.journal }

// Release 释放资源：事务日志若尚未 commit/abort 则仅关闭（保持未完成，留给 recover），然后按逆序释放组件锁。幂等。
func (t *Task) Release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.done = true
	if t.journal != nil {
		t.journal.Close()
	}
	t.locks.Release()
}

// BeginRecovery 是 lingedge recover 的入口：读取 journal 头得到涉及的组件、取这些组件锁、再取 journal 自己的 flock 并重新打开（截掉撕裂尾行）。
// 不做「存在未完成事务」检查（recover 就是为处理它）。事务已结束返回 KindFinished；journal 空/中间行损坏返回相应错误（见 ResumeJournal）。
func (r *Registry) BeginRecovery(ctx context.Context, tx string) (*Task, *Tx, error) {
	t0, err := r.ReadTx(tx)
	if err != nil {
		return nil, t0, err
	}
	if t0.Empty {
		return nil, t0, newErr(KindJournalCorrupt, "journal 为空（没有 open 记录）；用 DiscardEmpty 丢弃")
	}
	locks, err := r.LockComponents(t0.Header.Components)
	if err != nil {
		return nil, t0, err
	}
	j, t, err := r.ResumeJournal(ctx, tx)
	if err != nil {
		locks.Release()
		return nil, t, err
	}
	return &Task{r: r, locks: locks, tx: tx, comps: locks.Names(), journal: j}, t, nil
}

// DiscardEmpty 删除一个「已死亡且没有任何有效记录」的 journal（创建后、写入 open 前被杀：什么都没做过，无物可撤销）。
// 有有效记录、仍存活或不为空的一律拒绝。
func (r *Registry) DiscardEmpty(ctx context.Context, tx string) error {
	if !ValidTxID(tx) {
		return newErr(KindInvalid, "事务 ID 不合法: %q", tx)
	}
	return r.WithGlobal(ctx, func() error {
		path := r.L.JournalFile(tx)
		live, err := journalLive(path)
		if err != nil {
			return &Error{Kind: KindIO, Msg: "无法检查 journal", Path: path, Cause: err}
		}
		if live {
			return &LockedError{Name: "journal:" + tx, Path: path}
		}
		t, _ := parseFile(path)
		if t == nil || !t.Empty {
			return newErr(KindInvalid, "journal %s 不是空事务，拒绝丢弃", tx)
		}
		if err := os.Remove(path); err != nil {
			return &Error{Kind: KindIO, Msg: "删除失败", Path: path, Cause: err}
		}
		return r.syncDir(r.L.JournalDir())
	})
}
