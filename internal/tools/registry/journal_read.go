package registry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// TxStatus 是事务重放后的状态。
type TxStatus string

// TxStatus 取值。
const (
	TxActive    TxStatus = "active" // 没有 commit/abort
	TxCommitted TxStatus = "committed"
	TxAborted   TxStatus = "aborted"
)

// Tx 是一个解析后的事务日志。
type Tx struct {
	ID      string
	Path    string
	Header  Record // open 记录；Empty 时为零值
	Records []Record
	Status  TxStatus
	Steps   []StepState // 按首次 begin 顺序
	// Empty：文件里没有任何有效记录（创建后、写入 open 前被杀）。
	Empty bool
	// TornTail：最后一行无法解析/不合法，已被忽略（kill -9/断电时最后一次追加可能只写了一半）。TornOffset 是该行起点。
	TornTail   bool
	TornOffset int64
	// UnterminatedTail：最后一条有效记录缺少换行（恢复后追加前须先补一个）。
	UnterminatedTail bool
	validEnd         int64
	state            *replay
}

// InFlight 返回处于 begun（已记 undo、未 done）的步骤，没有则 nil。恢复必须先撤销它。
func (t *Tx) InFlight() *StepState {
	if t.state == nil || t.state.inFlite == 0 {
		return nil
	}
	for i := range t.Steps {
		if t.Steps[i].Step == t.state.inFlite {
			return &t.Steps[i]
		}
	}
	return nil
}

// UndoPlan 是回滚计划：对每个已 begin 且尚未 undone 的步骤（含在途步骤——它可能执行了一半），按首次 begin 的逆序排列。
// 恢复者依次执行各步的 Undo（每个撤销动作必须幂等），每撤销一步调用 Journal.Undone。
func (t *Tx) UndoPlan() []StepState {
	var out []StepState
	for i := len(t.Steps) - 1; i >= 0; i-- {
		if t.Steps[i].Phase != PhaseUndone {
			out = append(out, t.Steps[i])
		}
	}
	return out
}

// ResumeFrom 返回 recover --resume 应从哪一步续跑：第一个不是 done 的步骤（已 begin 未 done 的那一步，需要幂等地重做）。没有则 0。
func (t *Tx) ResumeFrom() int {
	for _, s := range t.Steps {
		if s.Phase != PhaseDone {
			return s.Step
		}
	}
	return 0
}

// ParseJournal 容错地解析一个 journal。
//
// 规则：逐行（\n 分隔）解析并用与写入端相同的状态机校验。只有**最后一行**允许是坏的（撕裂的尾行：kill -9/断电时最后一次追加没写完或脏页部分落盘），
// 此时忽略它并置 TornTail；中间任何一行坏掉（JSON 非法、seq 不连续、违反次序、v 不受支持）说明文件被改动/损坏，返回 KindJournalCorrupt（同时返回
// 解析到的前缀，供诊断）。文件为空（或只有一行坏的）→ Empty。
func ParseJournal(rd io.Reader) (*Tx, error) {
	t := &Tx{state: newReplay(), Status: TxActive}
	br := bufio.NewReaderSize(io.LimitReader(rd, maxJournal+1), 64<<10)
	var off int64
	type pending struct {
		line  []byte
		start int64
		term  bool
		long  bool
	}
	var prev *pending
	flush := func(p *pending, last bool) error {
		rec, err := decodeLine(p.line, p.long)
		if err == nil {
			err = t.state.apply(rec)
		}
		if err != nil {
			if last {
				t.TornTail, t.TornOffset = true, p.start
				return nil
			}
			return &Error{Kind: kindOf(err), Msg: "journal 第 " + strconv.Itoa(len(t.Records)+1) + " 行损坏（不是尾行）", Cause: err}
		}
		t.Records = append(t.Records, rec)
		t.validEnd = p.start + int64(len(p.line))
		if p.term {
			t.validEnd++
		} else {
			t.UnterminatedTail = true
		}
		return nil
	}
	var perr error
	for {
		line, long, term, err := readLine(br)
		if err != nil && err != io.EOF { //nolint:errorlint // bufio 直接返回 io.EOF
			perr = wrapErr(KindIO, err, "读取 journal 失败")
			break
		}
		if len(line) == 0 && !term && err != nil { // EOF，没有残余数据
			break
		}
		cur := &pending{line: line, start: off, term: term, long: long}
		off += int64(len(line))
		if term {
			off++
		}
		if prev != nil {
			if e := flush(prev, false); e != nil {
				perr = e
				break
			}
		}
		prev = cur
		if err != nil {
			break
		}
	}
	if perr == nil && prev != nil {
		perr = flush(prev, true)
	}
	if off > maxJournal && perr == nil {
		perr = newErr(KindJournalCorrupt, "journal 超过 %d 字节", maxJournal)
	}
	t.finalize()
	if perr != nil {
		return t, perr
	}
	return t, nil
}

func kindOf(err error) Kind {
	if e, ok := err.(*Error); ok && e.Kind == KindJournalUnsupported { //nolint:errorlint // 只看最外层
		return KindJournalUnsupported
	}
	return KindJournalCorrupt
}

func (t *Tx) finalize() {
	p := t.state
	t.Empty = len(t.Records) == 0
	if p.header != nil {
		t.Header = *p.header
		t.ID = p.header.Tx
	}
	switch p.final {
	case OpCommit:
		t.Status = TxCommitted
	case OpAbort:
		t.Status = TxAborted
	}
	for _, n := range p.order {
		t.Steps = append(t.Steps, *p.steps[n])
	}
}

// readLine 读一行：返回不含换行的内容、是否超长（超长部分被丢弃）、是否以 \n 结束。
func readLine(br *bufio.Reader) (line []byte, long, term bool, err error) {
	for {
		chunk, e := br.ReadSlice('\n')
		if !long {
			if len(line)+len(chunk) > maxReadLine+1 {
				long = true
				line = line[:0]
			} else {
				line = append(line, chunk...)
			}
		}
		switch e {
		case nil:
			if long {
				return nil, true, true, nil
			}
			return bytes.TrimSuffix(line, []byte("\n")), false, true, nil
		case bufio.ErrBufferFull:
			continue
		default:
			if long {
				return nil, true, false, e
			}
			return line, false, false, e
		}
	}
}

func decodeLine(line []byte, long bool) (Record, error) {
	if long {
		return Record{}, newErr(KindJournalCorrupt, "行过长")
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return Record{}, newErr(KindJournalCorrupt, "空行")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return Record{}, wrapErr(KindJournalCorrupt, err, "JSON 无法解析")
	}
	if _, err := dec.Token(); err != io.EOF {
		return Record{}, newErr(KindJournalCorrupt, "一行里有多个 JSON 值")
	}
	return rec, nil
}

// ReadTx 读取并解析 journal/<tx>.jsonl（也会在 archive/ 里找）。
func (r *Registry) ReadTx(id string) (*Tx, error) {
	if !ValidTxID(id) {
		return nil, newErr(KindInvalid, "事务 ID 不合法: %q", id)
	}
	for _, p := range []string{r.L.JournalFile(id), filepath.Join(r.L.ArchiveDir(), id+".jsonl")} {
		t, err := parseFile(p)
		if err != nil && isNotExist(unwrapPath(err)) {
			continue
		}
		return t, err
	}
	return nil, &Error{Kind: KindNotFound, Msg: "找不到事务 " + id}
}

func unwrapPath(err error) error {
	if e, ok := err.(*Error); ok && e.Kind == KindIO && e.Cause != nil { //nolint:errorlint // 只看最外层
		return e.Cause
	}
	return err
}

func parseFile(path string) (*Tx, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|noFollowFlag, 0)
	if err != nil {
		return nil, &Error{Kind: KindIO, Msg: "无法打开 journal", Path: path, Cause: err}
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !ownerTrusted(fi) || fi.Mode().Perm()&0o022 != 0 {
		return nil, &Error{Kind: KindPermission, Msg: "journal 不是属主可信、仅属主可写的普通文件", Path: path, Cause: err}
	}
	t, perr := ParseJournal(f)
	t.Path = path
	if t.ID == "" {
		t.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	return t, perr
}

// journalLive 报告 journal 文件是否被某个存活的写者持有（flock 忙）。
func journalLive(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|noFollowFlag, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	busy, err := flockTry(f)
	if err != nil {
		return false, err
	}
	if !busy {
		funlock(f)
	}
	return busy, nil
}

// ListJournals 列出 journal/ 下（不含 archive/）的全部事务摘要，按 ID 排序。
// 全局锁内进行，使「刚创建、尚未拿到 flock」的事务不会被看到。
func (r *Registry) ListJournals(ctx context.Context) ([]TxInfo, error) {
	var out []TxInfo
	err := r.WithGlobal(ctx, func() error {
		ents, err := os.ReadDir(r.L.JournalDir())
		if err != nil {
			return wrapErr(KindIO, err, "无法读取 %s", r.L.JournalDir())
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".jsonl") || !ValidTxID(strings.TrimSuffix(name, ".jsonl")) {
				continue
			}
			path := filepath.Join(r.L.JournalDir(), name)
			info := TxInfo{ID: strings.TrimSuffix(name, ".jsonl"), Path: path}
			live, lerr := journalLive(path)
			if lerr != nil {
				info.Status, info.Problem = "corrupt", lerr.Error()
				out = append(out, info)
				continue
			}
			info.Live = live
			t, perr := parseFile(path)
			if t != nil {
				info.Command, info.RunID, info.Components = t.Header.Command, t.Header.RunID, t.Header.Components
				info.Steps, info.TornTail = len(t.Steps), t.TornTail
				switch {
				case t.Empty:
					info.Status = "empty"
				default:
					info.Status = string(t.Status)
				}
			}
			if perr != nil {
				info.Status, info.Problem = "corrupt", perr.Error()
			}
			out = append(out, info)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return nil
	})
	return out, err
}

// Unfinished 返回「已死亡且未结束」的事务：journal 未 commit/abort（含空文件与损坏文件）且没有存活的持有者。
// 存活的事务（另一个进程正在做的合法操作，flock 被占）不算。committed/aborted 的忽略（ArchiveFinished 负责归档）。
func (r *Registry) Unfinished(ctx context.Context) ([]TxInfo, error) {
	all, err := r.ListJournals(ctx)
	if err != nil {
		return nil, err
	}
	var out []TxInfo
	for _, t := range all {
		if t.Live || t.Status == string(TxCommitted) || t.Status == string(TxAborted) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// ArchiveFinished 把已 commit/abort 且无存活持有者的 journal 归档，返回归档个数。
func (r *Registry) ArchiveFinished(ctx context.Context) (int, error) {
	all, err := r.ListJournals(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range all {
		if t.Live || (t.Status != string(TxCommitted) && t.Status != string(TxAborted)) {
			continue
		}
		if _, err := r.archiveFile(t.Path); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ResumeJournal 以追加模式重新打开一个未结束的事务以便恢复：取该 journal 的 flock（别的恢复者/存活的写者持有则 *LockedError）、解析、
// 截掉撕裂的尾行（补齐缺失的换行）后 fsync，使后续追加不会粘在半行后面。返回可继续追加的 Journal 与解析结果。
// 事务已 commit/abort 返回 KindFinished；中间行损坏返回 KindJournalCorrupt（不动文件，需人工处理）。
func (r *Registry) ResumeJournal(ctx context.Context, id string) (*Journal, *Tx, error) {
	if !ValidTxID(id) {
		return nil, nil, newErr(KindInvalid, "事务 ID 不合法: %q", id)
	}
	path := r.L.JournalFile(id)
	var j *Journal
	var tx *Tx
	err := r.WithGlobal(ctx, func() error {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|noFollowFlag, 0)
		if err != nil {
			return &Error{Kind: KindIO, Msg: "无法打开 journal", Path: path, Cause: err}
		}
		busy, err := flockTry(f)
		if err != nil || busy {
			_ = f.Close()
			if busy {
				return &LockedError{Name: "journal:" + id, Path: path}
			}
			return wrapErr(KindIO, err, "无法锁定 %s", path)
		}
		lk := &FileLock{f: f, name: "journal:" + id, path: path}
		fail := func(e error) error { lk.Release(); return e }
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() || !ownerTrusted(fi) || fi.Mode().Perm()&0o022 != 0 {
			return fail(&Error{Kind: KindPermission, Msg: "journal 不是属主可信、仅属主可写的普通文件", Path: path, Cause: err})
		}
		t, perr := ParseJournal(io.NewSectionReader(f, 0, fi.Size()))
		t.Path = path
		if perr != nil {
			return fail(perr)
		}
		if t.Empty {
			return fail(newErr(KindJournalCorrupt, "journal 为空（没有 open 记录），请用 recover 直接丢弃该事务"))
		}
		if t.Status != TxActive {
			return fail(&Error{Kind: KindFinished, Msg: "事务已" + string(t.Status), Path: path})
		}
		if t.TornTail {
			if err := f.Truncate(t.TornOffset); err != nil {
				return fail(wrapErr(KindIO, err, "无法截掉撕裂的尾行"))
			}
			// 撕裂行之前的最后一条有效记录必然以 \n 结束（否则它自己就会被当作撕裂行），截断后无需补换行。
			t.UnterminatedTail = false
		}
		if t.UnterminatedTail {
			if _, err := f.Write([]byte("\n")); err != nil {
				return fail(wrapErr(KindIO, err, "无法补齐换行"))
			}
		}
		if t.TornTail || t.UnterminatedTail {
			if err := r.fsync(f); err != nil {
				return fail(wrapErr(KindIO, err, "fsync 失败"))
			}
		}
		j = &Journal{r: r, f: f, path: path, p: t.state, lock: lk}
		tx = t
		return nil
	})
	if err != nil {
		return nil, tx, err
	}
	return j, tx, nil
}
