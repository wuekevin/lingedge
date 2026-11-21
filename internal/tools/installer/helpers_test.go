package installer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

func newW(t testing.TB) *toolsworld.World {
	t.Helper()
	w, err := toolsworld.NewWorld(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// 一次安装的结果。
type outcome struct {
	res *installer.Result
	err error
	in  *installer.Installer
}

func baseReq(w *toolsworld.World, comp string, mut ...func(*installer.Request)) installer.Request {
	req := installer.Request{Component: comp, Channel: "stable", Offline: w.Bundle, NonInteractive: true}
	for _, m := range mut {
		m(&req)
	}
	return req
}

// doInstall 走与 cli 相同的写命令入口：Begin（取锁 + 扫描 journal）→ Install。
func doInstall(t testing.TB, w *toolsworld.World, comp string, mutDeps []func(*installer.Deps), mut ...func(*installer.Request)) outcome {
	t.Helper()
	return doInstallCtx(t, t.Context(), w, comp, mutDeps, mut...)
}

func doInstallCtx(t testing.TB, ctx context.Context, w *toolsworld.World, comp string, mutDeps []func(*installer.Deps), mut ...func(*installer.Request)) outcome {
	t.Helper()
	in := w.NewInstaller("run-test", mutDeps...)
	task, err := in.Deps().Reg.Begin(ctx, registry.TaskRequest{Components: []string{comp}})
	if err != nil {
		return outcome{err: err, in: in}
	}
	defer task.Release()
	res, err := in.Install(ctx, task, baseReq(w, comp, mut...))
	return outcome{res: res, err: err, in: in}
}

func must(t testing.TB, o outcome) *installer.Result {
	t.Helper()
	if o.err != nil {
		t.Fatalf("install 失败: %v", o.err)
	}
	return o.res
}

func kindOf(t testing.TB, err error) installer.Kind {
	t.Helper()
	var ie *installer.Error
	if !errors.As(err, &ie) {
		t.Fatalf("不是 installer 错误: %T %v", err, err)
	}
	return ie.Kind
}

// snapshot 记录目录树的完整内容：相对路径 → 类型/权限/大小/SHA256/链接目标。skip 返回 true 的路径被忽略。
func snapshot(t testing.TB, root string, skip func(rel string) bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." || (skip != nil && skip(rel)) {
			if d.IsDir() && skip != nil && skip(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			tg, _ := os.Readlink(p)
			out[rel] = "L " + tg
		case info.IsDir():
			out[rel] = "D " + info.Mode().Perm().String()
		default:
			b, _ := os.ReadFile(p)
			h := sha256.Sum256(b)
			out[rel] = "F " + info.Mode().Perm().String() + " " + hex.EncodeToString(h[:8])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffSnap(a, b map[string]string) string {
	var lines []string
	for k, v := range a {
		if bv, ok := b[k]; !ok {
			lines = append(lines, "- "+k+" ("+v+")")
		} else if bv != v {
			lines = append(lines, "~ "+k+": "+v+" -> "+bv)
		}
	}
	for k, v := range b {
		if _, ok := a[k]; !ok {
			lines = append(lines, "+ "+k+" ("+v+")")
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// 「系统状态」快照：整棵 root，排除 Tools 自己的事务/锁/信任库/staging 簿记（journal 归档、锁文件、sequence 单调前进不回退）、伪 systemd 的 reload 计数与测试控制文件。
// state.json 例外——它必须回到原样（甚至不存在）。注意先取完整快照再过滤：对目录提前 SkipDir 会把 state.json 一并漏掉。
func sysSnap(t testing.TB, w *toolsworld.World) map[string]string {
	full := snapshot(t, w.Root, nil)
	out := map[string]string{}
	for k, v := range full {
		if k == "var/lib/lingedge/tools/state.json" {
			// state.json 按语义比较：「没有任何组件」与「文件不存在」等价（回滚会把本事务写入的记录删掉，留下空的组件表与只读探测的 host 段）；
			// 有组件时按组件名/版本/LastTx 归一（其余字段如 InstalledAt 在固定时钟下恒定）。
			b, err := os.ReadFile(filepath.Join(w.Root, k))
			if err != nil {
				continue
			}
			st, err := registry.ParseState(b)
			if err != nil {
				t.Fatalf("state.json 无法解析: %v", err)
			}
			if len(st.Components) == 0 {
				continue
			}
			var parts []string
			for _, n := range st.Names() {
				parts = append(parts, n+"@"+st.Components[n].Version+"/"+st.Components[n].State)
			}
			out[k] = "state " + strings.Join(parts, ",")
			continue
		}
		if k == "var/lib/lingedge/tools" || (strings.HasPrefix(k, "var/lib/lingedge/tools/") && k != "var/lib/lingedge/tools/state.json") ||
			k == "fake/reloads" || k == "bundle.tar" || k == "ctl" || strings.HasPrefix(k, "ctl/") {
			continue
		}
		out[k] = v
	}
	return out
}

// 完整快照（含簿记）：dry-run 的零变化断言用。
func fullSnap(t testing.TB, w *toolsworld.World) map[string]string { return snapshot(t, w.Root, nil) }

// journalOps 解析归档/活动的 journal，返回 op 序列字符串，如 "open begin:1 done:1 … commit"。
func journalOps(t testing.TB, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ln := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var r registry.Record
		if err := json.Unmarshal(ln, &r); err != nil {
			t.Fatalf("journal 行不是 JSON: %v", err)
		}
		switch r.Op {
		case registry.OpBegin, registry.OpDone, registry.OpUndone:
			out = append(out, string(r.Op)+":"+itoa(r.Step))
		default:
			out = append(out, string(r.Op))
		}
	}
	return out
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func archivedJournals(t testing.TB, w *toolsworld.World) []string {
	t.Helper()
	ms, _ := filepath.Glob(filepath.Join(w.Layout.ArchiveDir(), "*.jsonl"))
	return ms
}

func activeJournals(t testing.TB, w *toolsworld.World) []string {
	t.Helper()
	ms, _ := filepath.Glob(filepath.Join(w.Layout.JournalDir(), "*.jsonl"))
	return ms
}

// 事件收集。
type collector struct {
	mu  sync.Mutex
	evs []installer.Event
}

func (c *collector) run(ch <-chan installer.Event, done chan<- struct{}) {
	for e := range ch {
		c.mu.Lock()
		c.evs = append(c.evs, e)
		c.mu.Unlock()
	}
	close(done)
}

func withEvents(c *collector) (func(*installer.Deps), func()) {
	ch := make(chan installer.Event, 4096)
	done := make(chan struct{})
	go c.run(ch, done)
	return func(d *installer.Deps) { d.Events = ch }, func() { close(ch); <-done }
}

// 故障注入的 journal 包装：在第 n 次指定调用上返回错误（其余原样转发给真实 journal）。
type faultJournal struct {
	installer.Journal
	failBegin, failDone map[int]bool
	failCommit          bool
	failUndone          bool
	failAbort           bool
}

var errInjected = errors.New("injected journal fault")

func (f *faultJournal) Begin(step int, name string, u registry.Undo) error {
	if f.failBegin[step] {
		return errInjected
	}
	return f.Journal.Begin(step, name, u)
}

func (f *faultJournal) Done(step int) error {
	if f.failDone[step] {
		return errInjected
	}
	return f.Journal.Done(step)
}

func (f *faultJournal) Undone(step int) error {
	if f.failUndone {
		return errInjected
	}
	return f.Journal.Undone(step)
}

func (f *faultJournal) Commit() error {
	if f.failCommit {
		return errInjected
	}
	return f.Journal.Commit()
}

func (f *faultJournal) Abort(r string) error {
	if f.failAbort {
		return errInjected
	}
	return f.Journal.Abort(r)
}

func wrapJournal(mk func(installer.Journal) installer.Journal) func(*installer.Deps) {
	return func(d *installer.Deps) { d.WrapJournal = mk }
}

func failAt(begin, done []int) func(*installer.Deps) {
	return wrapJournal(func(j installer.Journal) installer.Journal {
		f := &faultJournal{Journal: j, failBegin: map[int]bool{}, failDone: map[int]bool{}}
		for _, n := range begin {
			f.failBegin[n] = true
		}
		for _, n := range done {
			f.failDone[n] = true
		}
		return f
	})
}
