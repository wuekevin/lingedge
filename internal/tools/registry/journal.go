package registry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// JournalVersion 是 journal 行格式的版本（每条记录的 v 字段）。
const JournalVersion = 1

// 行长上限：写入端不会产生更长的行；读取端对超长行按「损坏行」处理（不无限读内存）。
const (
	maxWriteLine = 64 << 10
	maxReadLine  = 256 << 10
	maxJournal   = 64 << 20
)

// Op 是 journal 记录的类型（tools-design §12.3）。
type Op string

// Op 取值。open 是事务头（第一条记录，登记谁在做什么）；undone 记录恢复时「某步已被撤销」，使恢复本身可被中断并续做。
const (
	OpOpen   Op = "open"
	OpBegin  Op = "begin"
	OpDone   Op = "done"
	OpUndone Op = "undone"
	OpCommit Op = "commit"
	OpAbort  Op = "abort"
)

// UndoAction 是撤销一件事所需的信息。字段刻意只含路径/标志/哈希/名字，不含口令（写入时校验）。
// Action 的取值与解释属于各步骤的实现者（installer/service/…）；registry 只保证它被先落盘。
type UndoAction struct {
	Action  string            `json:"action"`            // 如 remove_path、restore_file、remove_dir_if_empty、restore_symlink、unlink_unit、userdel
	Path    string            `json:"path,omitempty"`    // 受影响的路径
	Created bool              `json:"created,omitempty"` // 是否由本事务创建（撤销时只删除本事务创建的东西）
	Backup  string            `json:"backup,omitempty"`  // 被替换内容的备份位置
	SHA256  string            `json:"sha256,omitempty"`  // 预期内容哈希（撤销前复核，防误删用户后来改过的文件）
	Prev    string            `json:"prev,omitempty"`    // 被替换前的值（如旧符号链接目标）
	Extra   map[string]string `json:"extra,omitempty"`
}

// Undo 是一步的撤销信息（journal 的 "undo" 对象）。
type Undo struct {
	Actions []UndoAction `json:"actions,omitempty"`
}

// Record 是 journal 的一行。
type Record struct {
	V    int    `json:"v"`
	Seq  int    `json:"seq"`
	TS   string `json:"ts"`
	Op   Op     `json:"op"`
	Step int    `json:"step,omitempty"`
	Name string `json:"name,omitempty"`
	Undo *Undo  `json:"undo,omitempty"`

	// 仅 open：
	Tx         string   `json:"tx,omitempty"`
	Command    string   `json:"command,omitempty"`
	RunID      string   `json:"run_id,omitempty"`
	Components []string `json:"components,omitempty"`
	PID        int      `json:"pid,omitempty"`
	Host       string   `json:"host,omitempty"`

	// 仅 abort：
	Reason string `json:"reason,omitempty"`
}

// StepPhase 是一步在重放后的阶段。
type StepPhase string

// StepPhase 取值。
const (
	PhaseBegun  StepPhase = "begun" // 已记 undo、尚未 done（可能已部分执行）
	PhaseDone   StepPhase = "done"
	PhaseUndone StepPhase = "undone" // 恢复已撤销
)

// StepState 是一步的当前状态。
type StepState struct {
	Step     int       `json:"step"`
	Name     string    `json:"name,omitempty"`
	Phase    StepPhase `json:"phase"`
	Undo     *Undo     `json:"undo,omitempty"`
	BeginSeq int       `json:"begin_seq"`
	Attempts int       `json:"attempts"` // begin 的次数（恢复续跑会重新 begin 同一步）
}

// replay 是 journal 的状态机：写入端在内存里维护它，读取端用同一份代码重放，两边对「合法序列」的定义不会漂移。
type replay struct {
	seq     int
	header  *Record
	steps   map[int]*StepState
	order   []int // 步骤首次 begin 的顺序
	final   Op    // "", commit 或 abort
	lastTS  string
	inFlite int // 当前 PhaseBegun 的步骤号，0 表示没有
}

func newReplay() *replay { return &replay{steps: map[int]*StepState{}} }

// apply 校验并应用一条记录；违反协议返回 KindJournalProtocol。
func (p *replay) apply(rec Record) error {
	bad := func(format string, a ...any) error { return newErr(KindJournalProtocol, format, a...) }
	if rec.V != JournalVersion {
		return &Error{Kind: KindJournalUnsupported, Msg: "journal 记录版本 v=" + strconv.Itoa(rec.V) + " 不受支持"}
	}
	if rec.Seq != p.seq+1 {
		return bad("seq 不连续：期望 %d，得到 %d", p.seq+1, rec.Seq)
	}
	if p.final != "" {
		return bad("事务已 %s，其后不能再有记录", p.final)
	}
	if rec.TS != "" {
		if _, err := time.Parse(time.RFC3339Nano, rec.TS); err != nil {
			return bad("ts 不是 RFC3339")
		}
	}
	if p.header == nil {
		if rec.Op != OpOpen {
			return bad("第一条记录必须是 open，得到 %q", rec.Op)
		}
		if !ValidTxID(rec.Tx) {
			return bad("open 的 tx 不合法")
		}
		h := rec
		p.header = &h
		p.seq = rec.Seq
		p.lastTS = rec.TS
		return nil
	}
	switch rec.Op {
	case OpOpen:
		return bad("open 只能出现一次且位于第一行")
	case OpBegin:
		if rec.Step < 1 || rec.Step > 9999 {
			return bad("step 必须在 1..9999 内")
		}
		if p.inFlite != 0 && p.inFlite != rec.Step {
			return bad("第 %d 步尚未 done，不能开始第 %d 步", p.inFlite, rec.Step)
		}
		st := p.steps[rec.Step]
		if st == nil {
			st = &StepState{Step: rec.Step}
			p.steps[rec.Step] = st
			p.order = append(p.order, rec.Step)
		} else if st.Phase == PhaseDone {
			return bad("第 %d 步已 done，不能重复 begin", rec.Step)
		}
		st.Name, st.Phase, st.Undo, st.BeginSeq = rec.Name, PhaseBegun, rec.Undo, rec.Seq
		st.Attempts++
		p.inFlite = rec.Step
	case OpDone:
		st := p.steps[rec.Step]
		if st == nil || st.Phase != PhaseBegun {
			return bad("第 %d 步没有处于 begun 状态，不能 done", rec.Step)
		}
		st.Phase = PhaseDone
		p.inFlite = 0
	case OpUndone:
		st := p.steps[rec.Step]
		if st == nil {
			return bad("第 %d 步从未 begin，不能 undone", rec.Step)
		}
		st.Phase = PhaseUndone
		if p.inFlite == rec.Step {
			p.inFlite = 0
		}
	case OpCommit:
		if p.inFlite != 0 {
			return bad("第 %d 步尚未 done，不能 commit（请先完成该步，或 abort）", p.inFlite)
		}
		p.final = OpCommit
	case OpAbort:
		p.final = OpAbort
	default:
		return bad("未知 op %q", rec.Op)
	}
	p.seq = rec.Seq
	p.lastTS = rec.TS
	return nil
}

// Journal 是一个已打开的、可追加的事务日志。持有 journal 文件的 flock 直到 Commit/Abort/Close（或进程退出）——
// 这就是「此事务仍然活着」的判据（见 Registry.Unfinished）。方法并发安全。
type Journal struct {
	mu     sync.Mutex
	r      *Registry
	f      *os.File
	path   string
	p      *replay
	broken error // 一次写入失败后不再接受追加（磁盘上可能有撕裂的尾行；恢复端容错）
	closed bool
	lock   *FileLock
}

// ID 返回事务 ID。
func (j *Journal) ID() string { return j.p.header.Tx }

// Path 返回 journal 文件当前路径。
func (j *Journal) Path() string { j.mu.Lock(); defer j.mu.Unlock(); return j.path }

// CreateJournal 创建新事务日志并写入 open 记录（fsync 文件与目录）。在全局锁内进行：
// 使 Unfinished 的扫描不会在「文件已创建、还没拿到 flock」的缝隙里把它误判为已死亡。
func (r *Registry) CreateJournal(ctx context.Context, tx string, components []string) (*Journal, error) {
	if !ValidTxID(tx) {
		return nil, newErr(KindInvalid, "事务 ID 不合法: %q", tx)
	}
	names, err := normalizeComponents(components)
	if err != nil {
		return nil, err
	}
	var j *Journal
	err = r.WithGlobal(ctx, func() error {
		path := r.L.JournalFile(tx)
		f, err := security.CreateFile(path, 0o600)
		if err != nil {
			return wrapErr(KindIO, err, "无法创建 journal %s", path)
		}
		busy, err := flockTry(f)
		if err != nil || busy {
			_ = f.Close()
			_ = os.Remove(path)
			return wrapErr(KindIO, err, "无法锁定新建的 journal %s", path)
		}
		j = &Journal{r: r, f: f, path: path, p: newReplay(), lock: &FileLock{f: f, name: "journal:" + tx, path: path}}
		host, _ := r.opt.Hostname()
		if err := j.appendLocked(Record{Op: OpOpen, Tx: tx, Command: cleanText(r.opt.Command), RunID: r.opt.RunID, Components: names, PID: r.opt.PID, Host: host}); err != nil {
			j.lock.Release()
			_ = os.Remove(path)
			return err
		}
		return r.syncDir(r.L.JournalDir())
	})
	if err != nil {
		return nil, err
	}
	return j, nil
}

func (r *Registry) fsync(f *os.File) error {
	if r.opt.syncHook != nil {
		return r.opt.syncHook(f)
	}
	return f.Sync()
}

func (r *Registry) syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return wrapErr(KindIO, err, "无法打开目录 %s 以 fsync", dir)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return wrapErr(KindIO, err, "目录 %s fsync 失败", dir)
	}
	return nil
}

// appendLocked 校验（协议 + 字段）→ 单次 write（O_APPEND）→ fsync。调用方持有 j.mu（或尚未共享 j）。
// 任何写入/fsync 失败都使日志进入 broken 状态：磁盘上的尾行可能撕裂，继续追加会把新记录粘在半行后面。
func (j *Journal) appendLocked(rec Record) error {
	if j.broken != nil {
		return wrapErr(KindJournalClosed, j.broken, "journal 之前的写入失败，不再接受追加")
	}
	if j.closed {
		return newErr(KindJournalClosed, "journal 已关闭")
	}
	rec.V = JournalVersion
	rec.Seq = j.p.seq + 1
	rec.TS = j.r.now().UTC().Format("2006-01-02T15:04:05.000Z")
	if err := checkRecord(rec); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return wrapErr(KindIO, err, "序列化 journal 记录失败")
	}
	b = append(b, '\n')
	if len(b) > maxWriteLine {
		return newErr(KindInvalid, "journal 记录过长（%d > %d）", len(b), maxWriteLine)
	}
	if err := j.p.apply(rec); err != nil { // 先在内存里走一遍状态机：非法序列不会落盘
		return err
	}
	n, err := j.f.Write(b)
	if err == nil && n != len(b) {
		err = os.ErrInvalid
	}
	if err == nil {
		err = j.r.fsync(j.f)
	}
	if err != nil {
		j.broken = err
		return wrapErr(KindIO, err, "写 journal 失败")
	}
	return nil
}

// checkRecord 校验字段：自由文本脱敏、路径/哈希字段不得含密钥、undo 的哈希须是 64 位小写十六进制。
func checkRecord(rec Record) error {
	for _, c := range rec.Components {
		if !ValidComponent(c) {
			return newErr(KindInvalid, "open 含不合法的组件名: %q", c)
		}
	}
	for field, v := range map[string]string{"name": rec.Name, "run_id": rec.RunID, "host": rec.Host, "reason": rec.Reason} {
		if err := requireClean("journal."+field, v); err != nil {
			return err
		}
	}
	if rec.Undo == nil {
		return nil
	}
	if len(rec.Undo.Actions) > 256 {
		return newErr(KindInvalid, "单步 undo 动作过多")
	}
	for i, a := range rec.Undo.Actions {
		if a.Action == "" {
			return newErr(KindInvalid, "undo.actions[%d].action 不能为空", i)
		}
		if a.SHA256 != "" && !sha256RE.MatchString(a.SHA256) {
			return newErr(KindInvalid, "undo.actions[%d].sha256 不是 64 位小写十六进制", i)
		}
		for f, v := range map[string]string{"action": a.Action, "path": a.Path, "backup": a.Backup, "prev": a.Prev} {
			if err := requireClean("journal.undo."+f, v); err != nil {
				return err
			}
		}
		for k, v := range a.Extra {
			if err := requireClean("journal.undo.extra."+k, k+"="+v); err != nil {
				return err
			}
		}
	}
	return nil
}

// Begin 先记 undo 再动手（tools-design §12.3）：返回成功即表示 begin 记录（含 undo）已 fsync 到磁盘，调用方此后才可执行该步的副作用。
// undo 必须足以撤销该步可能产生的全部效果（包括执行到一半）。同一步可重复 begin（恢复续跑），已 done 的步不可。
func (j *Journal) Begin(step int, name string, undo Undo) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(Record{Op: OpBegin, Step: step, Name: name, Undo: &undo})
}

// Done 记录该步已完成。
func (j *Journal) Done(step int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(Record{Op: OpDone, Step: step})
}

// Undone 记录该步已被撤销（恢复/回滚用；撤销自身也是幂等的，所以此记录丢失只会导致多撤销一次）。
func (j *Journal) Undone(step int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked(Record{Op: OpUndone, Step: step})
}

// Commit 结束事务（成功）。仍有未 done 的步时拒绝。成功后 journal 关闭并释放 flock；归档见 Archive。
func (j *Journal) Commit() error { return j.finish(Record{Op: OpCommit}) }

// Abort 结束事务（放弃/已回滚）；reason 经脱敏。成功后 journal 关闭并释放 flock。
func (j *Journal) Abort(reason string) error {
	return j.finish(Record{Op: OpAbort, Reason: cleanText(reason)})
}

func (j *Journal) finish(rec Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.appendLocked(rec); err != nil {
		return err
	}
	j.closeLocked()
	return nil
}

// Close 不写结束记录地释放 flock 并关闭文件：事务保持「未完成」（Interrupted），留给 recover。
func (j *Journal) Close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.closeLocked()
}

func (j *Journal) closeLocked() {
	if j.closed {
		return
	}
	j.closed = true
	j.lock.Release() // 解锁并关闭 f
}

// Archive 把已结束（commit/abort）的 journal 移到 journal/archive/（不覆盖已存在的同名归档）。
// 在 Commit/Abort 之后调用；若进程在两者之间被杀，下一次 ArchiveFinished 会补做。
func (j *Journal) Archive() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.p.final == "" {
		return newErr(KindJournalProtocol, "事务尚未 commit/abort，不能归档")
	}
	dst, err := j.r.archiveFile(j.path)
	if err != nil {
		return err
	}
	j.path = dst
	return nil
}

func (r *Registry) archiveFile(path string) (string, error) {
	dst := filepath.Join(r.L.ArchiveDir(), filepath.Base(path))
	if _, err := os.Lstat(dst); err == nil {
		return "", &Error{Kind: KindConflict, Msg: "归档目标已存在，拒绝覆盖", Path: dst}
	}
	if err := os.Rename(path, dst); err != nil {
		return "", wrapErr(KindIO, err, "归档 %s 失败", path)
	}
	if err := r.syncDir(r.L.ArchiveDir()); err != nil {
		return "", err
	}
	if err := r.syncDir(r.L.JournalDir()); err != nil {
		return "", err
	}
	return dst, nil
}
