package registry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"time"
)

// Holder 是锁文件里的持有者信息（tools-design §16）。仅用于诊断显示：判活永远靠 flock，不靠这些字段。
type Holder struct {
	PID        int    `json:"pid"`
	RunID      string `json:"run_id"`
	Command    string `json:"command"`
	StartedAt  string `json:"started_at"`
	Host       string `json:"host"`
	StartTicks uint64 `json:"start_ticks,omitempty"` // Linux：/proc/<pid>/stat 的 starttime，使显示可区分 pid 复用
	Scope      string `json:"scope,omitempty"`       // 持有的是哪把锁（global|<component>）
}

// FileLock 是一把已持有的 flock 文件锁。Release 幂等；锁文件本身永不删除（删除会与并发 open 竞态产生两把「同名」锁）。
type FileLock struct {
	f    *os.File
	name string
	path string
}

// Name 返回锁名（"global" 或组件名）。
func (l *FileLock) Name() string { return l.name }

// Release 释放锁（解锁并关闭）。进程被杀时由内核代劳，无需调用。
func (l *FileLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	funlock(l.f)
	_ = l.f.Close()
	l.f = nil
}

// maxHolderBytes 限制读取锁文件诊断内容的大小（锁文件可能被他人篡改，不信任其内容）。
const maxHolderBytes = 4 << 10

// tryLock 对 path 取 LOCK_EX|LOCK_NB。
// 打开时不跟随符号链接，要求是属主可信的普通文件；加锁后复核路径仍指向同一 inode。
// 成功后清空并写入持有者信息；busy 时读取现有持有者信息（空则短暂重试，因为持有者可能刚拿到锁还没写完）返回 *LockedError。
func (r *Registry) tryLock(path, name string) (*FileLock, error) {
	if !platformSupported {
		return nil, &Error{Kind: KindUnsupported, Msg: "本平台不支持 flock 任务锁（Tools 仅支持 Linux）"}
	}
	for range 3 {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|noFollowFlag, 0o600)
		if err != nil {
			return nil, wrapErr(KindIO, err, "无法打开锁文件 %s", path)
		}
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, wrapErr(KindIO, err, "无法检查锁文件 %s", path)
		}
		if !fi.Mode().IsRegular() || !ownerTrusted(fi) || fi.Mode().Perm()&0o022 != 0 || nlink(fi) != 1 {
			_ = f.Close()
			return nil, &Error{Kind: KindPermission, Msg: "锁文件不是属主可信、单一硬链接、仅属主可写的普通文件", Path: path}
		}
		busy, err := flockTry(f)
		if err != nil {
			_ = f.Close()
			return nil, wrapErr(KindIO, err, "flock %s 失败", path)
		}
		if busy {
			h := readHolder(f)
			_ = f.Close()
			return nil, &LockedError{Name: name, Path: path, Holder: h}
		}
		if li, lerr := os.Lstat(path); lerr != nil || !os.SameFile(fi, li) {
			funlock(f)
			_ = f.Close()
			continue
		}
		h := r.holder(name)
		b, _ := json.Marshal(h)
		// 先清空再写：即便上一个持有者被 kill -9 留下了旧内容，新持有者拿到锁后也不会让争用方读到过期的信息
		// （争用方读到空内容会重试，见 readHolder）。
		if err := f.Truncate(0); err == nil {
			_, _ = f.WriteAt(append(b, '\n'), 0)
		}
		return &FileLock{f: f, name: name, path: path}, nil
	}
	return nil, newErr(KindIO, "锁文件 %s 持续被替换", path)
}

func readHolder(f *os.File) *Holder {
	for i := 0; i < 10; i++ {
		b, err := io.ReadAll(io.NewSectionReader(f, 0, maxHolderBytes))
		if err == nil && len(b) > 0 {
			var h Holder
			if json.Unmarshal(b, &h) == nil {
				return &h
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// WithGlobal 在全局状态锁（<Root>/lock）内运行 fn：这是读写 state.json/journal/trust 的短临界区（§16）。
// 锁以 LOCK_EX|LOCK_NB 获取，被占用时每 10ms 重试，最多等待 Options.GlobalWait（缺省 5s），超时返回 *LockedError（退出码 6）。
// 不可重入：fn 内再次调用 WithGlobal（含 UpdateState/LockedSequence）会等到超时失败——这是有意的，重入意味着调用方在临界区内套临界区。
func (r *Registry) WithGlobal(ctx context.Context, fn func() error) error {
	if err := r.L.Ensure(); err != nil {
		return err
	}
	lk, err := r.acquireGlobal(ctx)
	if err != nil {
		return err
	}
	defer lk.Release()
	return fn()
}

func (r *Registry) acquireGlobal(ctx context.Context) (*FileLock, error) {
	deadline := r.now().Add(r.opt.GlobalWait)
	for {
		lk, err := r.tryLock(r.L.GlobalLockFile(), "global")
		var le *LockedError
		if err == nil || !errors.As(err, &le) {
			return lk, err
		}
		if !r.now().Before(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// LockSet 是按字典序获取的一组组件锁。
type LockSet struct {
	locks []*FileLock
	names []string
}

// Names 返回已持有的组件名（字典序）。
func (s *LockSet) Names() []string { return append([]string(nil), s.names...) }

// Release 按获取的逆序释放。
func (s *LockSet) Release() {
	if s == nil {
		return
	}
	for i := len(s.locks) - 1; i >= 0; i-- {
		s.locks[i].Release()
	}
	s.locks = nil
}

// LockComponents 获取一组组件锁（LOCK_EX|LOCK_NB，不等待）：名字去重后按字典序依次获取，任何一把失败则释放已得的并返回该把的 *LockedError。
// 所有调用方都按同一全序获取，因此不会出现 A 持 x 等 y、B 持 y 等 x 的互等；非阻塞获取则让「冲突」立即以退出码 6 暴露而不是挂起。
func (r *Registry) LockComponents(components []string) (*LockSet, error) {
	names, err := normalizeComponents(components)
	if err != nil {
		return nil, err
	}
	if err := r.L.Ensure(); err != nil {
		return nil, err
	}
	set := &LockSet{names: names}
	for _, n := range names {
		if r.opt.onAcquire != nil {
			r.opt.onAcquire(n)
		}
		lk, err := r.tryLock(r.L.ComponentLockFile(n), n)
		if err != nil {
			set.Release()
			return nil, err
		}
		set.locks = append(set.locks, lk)
	}
	return set, nil
}

func normalizeComponents(components []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(components))
	for _, c := range components {
		if !ValidComponent(c) {
			return nil, newErr(KindInvalid, "组件名不合法: %q", c)
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, nil
}
