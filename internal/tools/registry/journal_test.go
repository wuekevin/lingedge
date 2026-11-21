package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func undoRemove(p string) Undo {
	return Undo{Actions: []UndoAction{{Action: "remove_path", Path: p, Created: true}}}
}

func openTx(t *testing.T, r *Registry, comps ...string) (*Task, *Journal) {
	t.Helper()
	task, err := r.Begin(context.Background(), TaskRequest{Components: comps})
	if err != nil {
		t.Fatal(err)
	}
	j, err := task.OpenTx(context.Background())
	if err != nil {
		task.Release()
		t.Fatal(err)
	}
	t.Cleanup(task.Release)
	return task, j
}

func TestJournalLifecycleAndFsyncEveryRecord(t *testing.T) {
	r := newReg(t)
	var syncs atomic.Int64
	r.opt.syncHook = func(f *os.File) error { syncs.Add(1); return f.Sync() } // 包装真实 fsync，只计数
	task, j := openTx(t, r, "server", "node")
	if fi, err := os.Stat(j.Path()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("journal 权限 %v err=%v，期望 0600", fi.Mode().Perm(), err)
	}
	steps := 0
	for i := 14; i <= 16; i++ {
		if err := j.Begin(i, "步骤"+strconv.Itoa(i), undoRemove("/opt/x/"+strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
		if err := j.Done(i); err != nil {
			t.Fatal(err)
		}
		steps += 2
	}
	if err := j.Commit(); err != nil {
		t.Fatal(err)
	}
	// open + 6 + commit = 8 条记录，每条一次 fsync（目录 fsync 另走 syncDir，不经 hook）。
	if got := syncs.Load(); got != int64(steps+2) {
		t.Fatalf("fsync 次数 %d，期望每条记录一次 = %d", got, steps+2)
	}
	tx, err := r.ReadTx(task.TxID())
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status != TxCommitted || len(tx.Records) != 8 || tx.TornTail || tx.Header.Command != "install server" || tx.Header.RunID != "run-test-1" ||
		!reflect.DeepEqual(tx.Header.Components, []string{"node", "server"}) || tx.Header.PID != os.Getpid() {
		t.Fatalf("解析结果不对: %+v", tx)
	}
	for i, rec := range tx.Records {
		if rec.Seq != i+1 || rec.V != 1 || rec.TS == "" {
			t.Errorf("记录 %d: %+v", i, rec)
		}
	}
	// 结束后：不再可写；归档后文件在 archive/ 且仍可读。
	if err := j.Begin(17, "x", Undo{}); kindOfErr(t, err) != KindJournalClosed {
		t.Fatalf("commit 之后不应再可写: %v", err)
	}
	if err := j.Archive(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.L.ArchiveDir(), task.TxID()+".jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.L.JournalFile(task.TxID())); !os.IsNotExist(err) {
		t.Fatal("归档后原文件应已移走")
	}
	if tx2, err := r.ReadTx(task.TxID()); err != nil || tx2.Status != TxCommitted {
		t.Fatalf("归档后应可读: %v", err)
	}
}

// 先记 undo 再动手：Begin 返回时，磁盘上（独立 open 读到的内容）已有包含 undo 的 begin 记录——此时副作用尚未发生。
func TestUndoIsDurableBeforeSideEffect(t *testing.T) {
	r := newReg(t)
	_, j := openTx(t, r, "server")
	victim := filepath.Join(t.TempDir(), "created-by-step")
	if err := j.Begin(14, "创建文件", undoRemove(victim)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Fatal("测试前提：副作用尚未发生")
	}
	raw, err := os.ReadFile(j.Path())
	if err != nil {
		t.Fatal(err)
	}
	last := raw[bytes.LastIndexByte(raw[:len(raw)-1], '\n')+1:]
	if !bytes.Contains(last, []byte(`"op":"begin"`)) || !bytes.Contains(last, []byte(victim)) || !bytes.Contains(last, []byte(`"created":true`)) {
		t.Fatalf("begin 记录尚未持久化 undo: %s", last)
	}
}

func TestJournalProtocolViolationsAreRejectedAndNotWritten(t *testing.T) {
	r := newReg(t)
	_, j := openTx(t, r, "server")
	size := func() int64 { fi, _ := os.Stat(j.Path()); return fi.Size() }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(j.Begin(1, "a", Undo{}))
	cases := []struct {
		name string
		fn   func() error
		kind Kind
	}{
		{"第 1 步未 done 就开始第 2 步", func() error { return j.Begin(2, "b", Undo{}) }, KindJournalProtocol},
		{"done 一个没 begin 的步骤", func() error { return j.Done(7) }, KindJournalProtocol},
		{"有在途步骤时 commit", func() error { return j.Commit() }, KindJournalProtocol},
		{"step=0", func() error { return j.Begin(0, "z", Undo{}) }, KindJournalProtocol},
		{"undone 从未 begin 的步骤", func() error { return j.Undone(9) }, KindJournalProtocol},
		{"undo 含口令", func() error {
			return j.Begin(1, "a", Undo{Actions: []UndoAction{{Action: "write", Path: "/etc/x", Extra: map[string]string{"dsn": "password=hunter2hunter2"}}}})
		}, KindSecret},
		{"undo 路径含令牌", func() error {
			return j.Begin(1, "a", Undo{Actions: []UndoAction{{Action: "x", Path: "/x?token=abcdef123456789"}}})
		}, KindSecret},
		{"name 含口令", func() error { return j.Begin(1, "password=hunter2hunter2", Undo{}) }, KindSecret},
		{"sha256 格式错", func() error { return j.Begin(1, "a", Undo{Actions: []UndoAction{{Action: "x", SHA256: "xyz"}}}) }, KindInvalid},
		{"action 为空", func() error { return j.Begin(1, "a", Undo{Actions: []UndoAction{{Path: "/x"}}}) }, KindInvalid},
		{"记录过长", func() error {
			return j.Begin(1, "a", Undo{Actions: []UndoAction{{Action: "x", Path: "/" + strings.Repeat("a", maxWriteLine)}}})
		}, KindInvalid},
	}
	for _, c := range cases {
		before := size()
		err := c.fn()
		if err == nil {
			t.Errorf("%s: 应被拒绝", c.name)
			continue
		}
		if k := kindOfErrSoft(err); k != c.kind {
			t.Errorf("%s: kind=%v，期望 %v（%v）", c.name, k, c.kind, err)
		}
		if size() != before {
			t.Errorf("%s: 被拒绝的记录不得落盘", c.name)
		}
		if c.kind != KindInvalid || c.name != "记录过长" {
			continue
		}
	}
	// 被拒绝之后日志仍可正常使用（序列号没有被消耗）。
	must(j.Done(1))
	must(j.Commit())
	tx, err := r.ReadTx(j.ID())
	if err != nil || tx.Status != TxCommitted {
		t.Fatalf("%v %+v", err, tx)
	}
	for _, rec := range tx.Records {
		if strings.Contains(fmt.Sprint(rec), "hunter2") {
			t.Fatal("密钥进入了 journal")
		}
	}
}

func TestJournalWriteFailureBreaksJournal(t *testing.T) {
	r := newReg(t)
	_, j := openTx(t, r, "server")
	_ = j.f.Close() // 真实的 I/O 故障：描述符失效，write(2) 返回 EBADF
	if err := j.Begin(1, "a", Undo{}); err == nil {
		t.Fatal("写失败应报错")
	}
	if err := j.Done(1); kindOfErr(t, err) != KindJournalClosed {
		t.Fatalf("写失败之后不应再接受追加（磁盘尾部可能撕裂）: %v", err)
	}
}

func TestJournalSecretInCommandIsRedacted(t *testing.T) {
	r := newReg(t)
	r.opt.Command = "install server --set db.password=hunter2secret"
	_, j := openTx(t, r, "server")
	b, _ := os.ReadFile(j.Path())
	if bytes.Contains(b, []byte("hunter2secret")) {
		t.Fatalf("命令行里的口令进入了 journal: %s", b)
	}
}

// ---- 解析的容错 ----

func sampleJournal(t *testing.T) []byte {
	t.Helper()
	r := newReg(t)
	_, j := openTx(t, r, "server")
	for i := 1; i <= 3; i++ {
		if err := j.Begin(i, "s"+strconv.Itoa(i), undoRemove("/tmp/p"+strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
		if err := j.Done(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Commit(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(j.Path())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 在每一个字节偏移处截断（模拟 kill -9/断电时最后一次追加只写了一部分）：解析必须成功，结果恰是完整行的前缀（可能多一条缺换行的有效记录）。
func TestParseToleratesTruncationAtEveryByteOffset(t *testing.T) {
	full := sampleJournal(t)
	want, err := ParseJournal(bytes.NewReader(full))
	if err != nil || len(want.Records) != 8 {
		t.Fatalf("完整文件应解析成功: %v", err)
	}
	for k := 0; k <= len(full); k++ {
		tx, err := ParseJournal(bytes.NewReader(full[:k]))
		if err != nil {
			t.Fatalf("截断于 %d/%d 字节：解析失败 %v", k, len(full), err)
		}
		complete := bytes.Count(full[:k], []byte("\n"))
		n := len(tx.Records)
		if n != complete && n != complete+1 {
			t.Fatalf("截断于 %d：记录数 %d，完整行 %d", k, n, complete)
		}
		if n == complete+1 && !tx.UnterminatedTail {
			t.Fatalf("截断于 %d：多出的一条记录应标记 UnterminatedTail", k)
		}
		if n < 8 && tx.Status != TxActive {
			t.Fatalf("截断于 %d：未走完的事务应是 active，得到 %s", k, tx.Status)
		}
		if tx.Empty != (n == 0) {
			t.Fatalf("截断于 %d：Empty=%v n=%d", k, tx.Empty, n)
		}
		for i, rec := range tx.Records {
			if !reflect.DeepEqual(rec, want.Records[i]) {
				t.Fatalf("截断于 %d：记录 %d 不是原记录的前缀", k, i)
			}
		}
		// 任意截断点，UndoPlan 都不会遗漏已 begin 的步骤。
		begun := 0
		for _, rec := range tx.Records {
			if rec.Op == OpBegin {
				begun++
			}
		}
		if len(tx.Steps) != begun {
			t.Fatalf("截断于 %d：步骤数 %d != begin 数 %d", k, len(tx.Steps), begun)
		}
	}
}

func TestParseTornTailGarbageVariants(t *testing.T) {
	full := sampleJournal(t)
	lines := bytes.SplitAfter(full, []byte("\n"))
	prefix := bytes.Join(lines[:5], nil) // open + begin1 done1 begin2 done2
	for name, tail := range map[string][]byte{
		"半个 JSON":         []byte(`{"v":1,"seq":6,"ts":"2026-11-21T12:00:00.000Z","op":"be`),
		"一串 NUL":          bytes.Repeat([]byte{0}, 300),
		"完整但乱码且有换行":       []byte("}{garbage\n"),
		"完整 JSON 但 seq 错": []byte(`{"v":1,"seq":99,"ts":"2026-11-21T12:00:00.000Z","op":"done","step":2}` + "\n"),
		"未知字段":            []byte(`{"v":1,"seq":6,"ts":"2026-11-21T12:00:00.000Z","op":"commit","x":1}` + "\n"),
		"空行":              []byte("\n"),
	} {
		tx, err := ParseJournal(bytes.NewReader(append(append([]byte{}, prefix...), tail...)))
		if err != nil {
			t.Errorf("%s: 尾部损坏应被容忍: %v", name, err)
			continue
		}
		if !tx.TornTail || len(tx.Records) != 5 || tx.Status != TxActive || tx.TornOffset != int64(len(prefix)) {
			t.Errorf("%s: %+v", name, tx)
		}
	}
}

func TestParseMidFileCorruptionIsAnError(t *testing.T) {
	full := sampleJournal(t)
	lines := bytes.SplitAfter(full, []byte("\n"))
	mut := func(i int, f func([]byte) []byte) []byte {
		cp := make([][]byte, len(lines))
		for k := range lines {
			cp[k] = append([]byte(nil), lines[k]...)
		}
		cp[i] = f(cp[i])
		return bytes.Join(cp, nil)
	}
	cases := map[string][]byte{
		"中间行被截断": mut(3, func(b []byte) []byte { return append(b[:len(b)/2], '\n') }),
		"中间行是乱码": mut(2, func(b []byte) []byte { return []byte("garbage\n") }),
		"seq 跳号": mut(4, func(b []byte) []byte { return bytes.Replace(b, []byte(`"seq":5`), []byte(`"seq":9`), 1) }),
		"次序违规：重复 done": mut(3, func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"op":"begin","step":2`), []byte(`"op":"done","step":1`), 1)
		}),
		"被改写的未知字段": mut(1, func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"op":"begin"`), []byte(`"op":"begin","evil":1`), 1)
		}),
	}
	for name, data := range cases {
		tx, err := ParseJournal(bytes.NewReader(data))
		if err == nil {
			t.Errorf("%s: 中间损坏应报错", name)
			continue
		}
		if k := kindOfErrSoft(err); k != KindJournalCorrupt {
			t.Errorf("%s: kind=%v", name, k)
		}
		if tx == nil {
			t.Errorf("%s: 出错时仍应返回已解析的前缀供诊断", name)
		}
	}
	// 更高的 journal 版本 → unsupported（不当作损坏、也不降级解析）。
	v2 := mut(2, func(b []byte) []byte { return bytes.Replace(b, []byte(`"v":1`), []byte(`"v":2`), 1) })
	if _, err := ParseJournal(bytes.NewReader(v2)); kindOfErrSoft(err) != KindJournalUnsupported {
		t.Errorf("v=2: %v", err)
	}
	// 事务结束后还有记录 → 损坏。
	if _, err := ParseJournal(bytes.NewReader(append(append([]byte{}, full...), full[:bytes.IndexByte(full, '\n')+1]...))); err == nil {
		// 最后一行允许是坏的（视为撕裂），所以追加两行才能区分。
		t.Log("单行多余数据被视为撕裂尾行，符合设计")
	}
	over := append(append([]byte{}, full...), []byte(`{"v":1,"seq":9,"ts":"2026-11-21T12:00:00.000Z","op":"done","step":1}`+"\n"+`{"v":1,"seq":10,"ts":"2026-11-21T12:00:00.000Z","op":"done","step":1}`+"\n")...)
	if _, err := ParseJournal(bytes.NewReader(over)); err == nil {
		t.Error("commit 之后的多条记录应报错")
	}
}

func TestParseHugeLineDoesNotBlowUp(t *testing.T) {
	full := sampleJournal(t)
	lines := bytes.SplitAfter(full, []byte("\n"))
	huge := append(bytes.Repeat([]byte("a"), 3<<20), '\n')
	data := bytes.Join([][]byte{lines[0], lines[1], huge, lines[3]}, nil)
	if _, err := ParseJournal(bytes.NewReader(data)); kindOfErrSoft(err) != KindJournalCorrupt {
		t.Fatalf("中间的超长行应报损坏: %v", err)
	}
	tx, err := ParseJournal(bytes.NewReader(append(append([]byte{}, bytes.Join(lines[:3], nil)...), huge...)))
	if err != nil || !tx.TornTail || len(tx.Records) != 3 {
		t.Fatalf("尾部超长行应视为撕裂: %v %+v", err, tx)
	}
}

func TestUndoPlanAndResumeFrom(t *testing.T) {
	r := newReg(t)
	_, j := openTx(t, r, "server")
	for _, s := range []struct {
		n    int
		done bool
	}{{14, true}, {15, true}, {16, false}} {
		if err := j.Begin(s.n, "s", undoRemove("/p/"+strconv.Itoa(s.n))); err != nil {
			t.Fatal(err)
		}
		if s.done {
			if err := j.Done(s.n); err != nil {
				t.Fatal(err)
			}
		}
	}
	j.Close()
	tx, err := r.ReadTx(j.ID())
	if err != nil {
		t.Fatal(err)
	}
	plan := tx.UndoPlan()
	var got []int
	for _, s := range plan {
		got = append(got, s.Step)
	}
	if !reflect.DeepEqual(got, []int{16, 15, 14}) { // 逆序，且含在途步骤 16
		t.Fatalf("UndoPlan=%v", got)
	}
	if f := tx.InFlight(); f == nil || f.Step != 16 || tx.ResumeFrom() != 16 {
		t.Fatalf("InFlight=%+v ResumeFrom=%d", f, tx.ResumeFrom())
	}
	if plan[0].Undo == nil || plan[0].Undo.Actions[0].Path != "/p/16" {
		t.Fatalf("undo 内容丢失: %+v", plan[0])
	}
}

// ---- 恢复：续写、未完成判定、归档 ----

func TestResumeJournalTruncatesTornTailAndAppends(t *testing.T) {
	r := newReg(t)
	task, j := openTx(t, r, "server")
	if err := j.Begin(1, "a", undoRemove("/p/1")); err != nil {
		t.Fatal(err)
	}
	path := j.Path()
	task.Release() // 释放 flock，保持未完成
	// 手工制造撕裂尾行（真实的 kill 撕裂见 kill_test）。
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"v":1,"seq":3,"ts":"2026-11-21T12:00:00.000Z","op":"do`)
	_ = f.Close()

	rt, tx, err := r.BeginRecovery(context.Background(), j.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Release()
	if !tx.TornTail || tx.InFlight() == nil {
		t.Fatalf("%+v", tx)
	}
	rj := rt.Journal()
	for _, s := range tx.UndoPlan() {
		if err := rj.Undone(s.Step); err != nil {
			t.Fatal(err)
		}
	}
	if err := rj.Abort("recovered"); err != nil {
		t.Fatal(err)
	}
	final, err := r.ReadTx(j.ID())
	if err != nil || final.TornTail || final.Status != TxAborted || len(final.Records) != 4 {
		t.Fatalf("恢复后应是干净的 aborted: %v %+v", err, final)
	}
}

func TestResumeUnterminatedValidTail(t *testing.T) {
	r := newReg(t)
	task, j := openTx(t, r, "server")
	if err := j.Begin(1, "a", undoRemove("/p/1")); err != nil {
		t.Fatal(err)
	}
	path := j.Path()
	task.Release()
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.TrimSuffix(b, []byte("\n")), 0o600); err != nil { // 最后一条记录完整，仅缺换行
		t.Fatal(err)
	}
	rt, tx, err := r.BeginRecovery(context.Background(), j.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Release()
	if !tx.UnterminatedTail || len(tx.Records) != 2 {
		t.Fatalf("%+v", tx)
	}
	if err := rt.Journal().Undone(1); err != nil {
		t.Fatal(err)
	}
	if err := rt.Journal().Abort("r"); err != nil {
		t.Fatal(err)
	}
	if final, err := r.ReadTx(j.ID()); err != nil || final.Status != TxAborted || len(final.Records) != 4 {
		t.Fatalf("追加不应粘在没有换行的尾行后: %v %+v", err, final)
	}
}

func TestUnfinishedDetectionGate13(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	// 活着的事务（别的组件上的合法操作）不算未完成。
	liveTask, _ := openTx(t, r, "node")
	t2, err := r.Begin(ctx, TaskRequest{Components: []string{"server"}})
	if err != nil {
		t.Fatalf("其他组件上有活事务时不应被拦截: %v", err)
	}
	t2.Release()
	// 同一组件：被组件锁拦成 6，而不是 13。
	_, err = r.Begin(ctx, TaskRequest{Components: []string{"node"}})
	_ = lockedErr(t, err)

	// 事务死亡（Close 不写结束记录 = 持有者消失，内核释放 flock）：任何写命令都被拦成 13，释放组件锁。
	deadID := liveTask.TxID()
	liveTask.Release()
	_, err = r.Begin(ctx, TaskRequest{Components: []string{"docs"}})
	var ue *UnfinishedError
	if !errors.As(err, &ue) || len(ue.Txs) != 1 || ue.Txs[0].ID != deadID || ue.Txs[0].Status != "active" || ue.Txs[0].Live {
		t.Fatalf("期望 *UnfinishedError(%s)，得到 %v", deadID, err)
	}
	if !strings.Contains(ue.Error(), "lingedge recover") || !reflect.DeepEqual(ue.Txs[0].Components, []string{"node"}) {
		t.Fatalf("%s %+v", ue, ue.Txs[0])
	}
	if l, e := r.LockComponents([]string{"docs"}); e != nil {
		t.Fatalf("被 13 拦下后不得遗留组件锁: %v", e)
	} else {
		l.Release()
	}
	// recover 之后（abort）放行，且已结束的事务可归档。
	rt, tx, err := r.BeginRecovery(ctx, deadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.UndoPlan()) != 0 {
		t.Fatal("该事务没有步骤")
	}
	if err := rt.Journal().Abort("recovered"); err != nil {
		t.Fatal(err)
	}
	rt.Release()
	ok, err := r.Begin(ctx, TaskRequest{Components: []string{"docs"}})
	if err != nil {
		t.Fatalf("abort 后应放行: %v", err)
	}
	ok.Release()
	if n, err := r.ArchiveFinished(ctx); err != nil || n != 1 {
		t.Fatalf("ArchiveFinished=%d %v", n, err)
	}
	if left, _ := r.ListJournals(ctx); len(left) != 0 {
		t.Fatalf("归档后 journal/ 应为空: %+v", left)
	}
	if err := r.Check(ctx, []string{"server"}); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyAndCorruptJournalsAreUnfinished(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	empty := "01JD5Y3C8Q5G7M0V1N2P3R4S5T"
	if err := os.WriteFile(r.L.JournalFile(empty), nil, 0o600); err != nil { // 创建后、写 open 前被杀
		t.Fatal(err)
	}
	err := r.Check(ctx, []string{"server"})
	var ue *UnfinishedError
	if !errors.As(err, &ue) || ue.Txs[0].Status != "empty" {
		t.Fatalf("空 journal 应算未完成: %v", err)
	}
	if _, _, err := r.BeginRecovery(ctx, empty); kindOfErrSoft(err) != KindJournalCorrupt {
		t.Fatalf("空 journal 不能续写: %v", err)
	}
	if err := r.DiscardEmpty(ctx, empty); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(ctx, []string{"server"}); err != nil {
		t.Fatal(err)
	}
	// 非空事务拒绝 DiscardEmpty。
	task, j := openTx(t, r, "tmp")
	j.Close()
	if err := r.DiscardEmpty(ctx, j.ID()); err == nil {
		t.Fatal("非空事务不得被丢弃")
	}
	// 中间行损坏：报 13（status=corrupt），且不被静默忽略。
	b, _ := os.ReadFile(j.Path())
	bad := bytes.Replace(b, []byte(`"op":"open"`), []byte(`"op":"opeX"`), 1)
	bad = append(bad, []byte(`{"v":1,"seq":2,"ts":"2026-11-21T12:00:00.000Z","op":"abort"}`+"\n")...)
	if err := os.WriteFile(j.Path(), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	task.Release()
	err = r.Check(ctx, []string{"server"})
	if !errors.As(err, &ue) || ue.Txs[0].Status != "corrupt" || ue.Txs[0].Problem == "" {
		t.Fatalf("损坏的 journal 应被报告而不是忽略: %v", err)
	}
}

func TestArchiveRefusesToOverwrite(t *testing.T) {
	r := newReg(t)
	_, j := openTx(t, r, "server")
	if err := j.Archive(); kindOfErr(t, err) != KindJournalProtocol {
		t.Fatalf("未结束不能归档: %v", err)
	}
	if err := j.Commit(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(r.L.ArchiveDir(), j.ID()+".jsonl")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := j.Archive(); kindOfErr(t, err) != KindConflict {
		t.Fatalf("归档目标已存在应拒绝覆盖: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "old" {
		t.Fatal("已有归档被覆盖")
	}
}

// -race：多个任务并发写各自的 journal；同一个 Journal 被多个 goroutine 同时追加（互斥保证 seq 连续、每行完整）。
func TestJournalConcurrentAppends(t *testing.T) {
	r := newReg(t)
	r.opt.GlobalWait = 30e9
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := New(r.L, Options{RunID: "g", Command: "x", GlobalWait: 30e9})
			task, err := rr.Begin(context.Background(), TaskRequest{Components: []string{"c" + strconv.Itoa(i)}})
			if err != nil {
				t.Error(err)
				return
			}
			defer task.Release()
			j, err := task.OpenTx(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			for s := 1; s <= 20; s++ {
				if err := j.Begin(s, "s", undoRemove("/p")); err != nil {
					t.Error(err)
					return
				}
				if err := j.Done(s); err != nil {
					t.Error(err)
					return
				}
			}
			_ = j.Commit()
		}()
	}
	wg.Wait()

	_, shared := openTx(t, r, "shared")
	var wg2 sync.WaitGroup
	var errs atomic.Int64
	for g := 0; g < 8; g++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			for k := 0; k < 25; k++ {
				// 并发追加：只有 Undone（对 begun 步骤合法）能安全地并发；先放一个 begun 步骤。
				if err := shared.Undone(1); err != nil {
					errs.Add(1)
				}
			}
		}()
	}
	if err := shared.Begin(1, "s", Undo{}); err != nil {
		t.Fatal(err)
	}
	wg2.Wait()
	_ = errs.Load() // 在 Begin 之前到达的 Undone 会被协议拒绝，这是预期；关键是文件仍严格有效
	tx, err := r.ReadTx(shared.ID())
	if err != nil || tx.TornTail {
		t.Fatalf("并发追加后 journal 应完整有效: %v", err)
	}
	for i, rec := range tx.Records {
		if rec.Seq != i+1 {
			t.Fatalf("seq 不连续于 %d", i)
		}
	}
	all, err := r.ListJournals(context.Background())
	if err != nil || len(all) != 9 {
		t.Fatalf("%d %v", len(all), err)
	}
}
