package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

func lockedErr(t *testing.T, err error) *LockedError {
	t.Helper()
	var le *LockedError
	if !errors.As(err, &le) {
		t.Fatalf("期望 *LockedError，得到 %T: %v", err, err)
	}
	return le
}

func TestComponentLockExclusiveAndHolderInfo(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	a, err := r.Begin(ctx, TaskRequest{Components: []string{"server"}})
	if err != nil {
		t.Fatal(err)
	}
	// 同进程内另一次 Begin 同样被拒（flock 以打开文件描述为单位）：第二个任务退出码 6 的语义。
	r2 := New(r.L, Options{RunID: "run-other", Command: "update server", Hostname: func() (string, error) { return "edge-host-2", nil }})
	_, err = r2.Begin(ctx, TaskRequest{Components: []string{"server"}})
	le := lockedErr(t, err)
	if le.Name != "server" || le.Holder == nil {
		t.Fatalf("持有者信息缺失: %+v", le)
	}
	h := le.Holder
	if h.PID != os.Getpid() || h.RunID != "run-test-1" || h.Command != "install server" || h.Host != "edge-host-1" || h.StartedAt == "" || h.Scope != "server" {
		t.Errorf("持有者信息不对: %+v", h)
	}
	if !contains(le.Error(), "pid="+strconv.Itoa(os.Getpid())) || !contains(le.Error(), "run-test-1") {
		t.Errorf("错误文本缺少持有者: %s", le)
	}
	// 不同组件互不影响。
	b, err := r2.Begin(ctx, TaskRequest{Components: []string{"node"}})
	if err != nil {
		t.Fatalf("不同组件应可并行: %v", err)
	}
	b.Release()
	a.Release()
	a.Release() // 幂等
	c, err := r2.Begin(ctx, TaskRequest{Components: []string{"server"}})
	if err != nil {
		t.Fatalf("释放后应可获取: %v", err)
	}
	c.Release()
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestHolderCommandIsRedacted(t *testing.T) {
	r := newReg(t)
	r.opt.Command = "install server --set db.password=hunter2secret"
	a, err := r.Begin(context.Background(), TaskRequest{Components: []string{"server"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, _ := os.ReadFile(r.L.ComponentLockFile("server"))
	if contains(string(b), "hunter2secret") {
		t.Fatalf("锁文件含明文口令: %s", b)
	}
}

func TestLockOrderIsLexicographicAndAllOrNothing(t *testing.T) {
	r := newReg(t)
	var order []string
	r.opt.onAcquire = func(n string) { order = append(order, n) }
	s, err := r.LockComponents([]string{"node", "server", "node", "docs"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"docs", "node", "server"}) {
		t.Fatalf("获取顺序 %v，期望字典序且去重", order)
	}
	if !reflect.DeepEqual(s.Names(), []string{"docs", "node", "server"}) {
		t.Fatalf("Names=%v", s.Names())
	}
	s.Release()

	// 别人持有 node；我要 {server, docs, node}：按字典序先得 docs、server，在 node 失败 → 全部释放（all-or-nothing）。
	other, err := r.LockComponents([]string{"node"})
	if err != nil {
		t.Fatal(err)
	}
	order = nil
	_, err = r.LockComponents([]string{"server", "node", "docs"})
	if le := lockedErr(t, err); le.Name != "node" {
		t.Fatalf("应在 node 处失败，得到 %s", le.Name)
	}
	if !reflect.DeepEqual(order, []string{"docs", "node"}) {
		t.Fatalf("尝试顺序 %v，期望 docs 在 node 之前", order)
	}
	for _, c := range []string{"docs", "server"} {
		l, e := r.LockComponents([]string{c})
		if e != nil {
			t.Fatalf("失败的整体获取不得遗留 %s 的锁: %v", c, e)
		}
		l.Release()
	}
	other.Release()
}

func TestLockRejectsBadComponentNames(t *testing.T) {
	r := newReg(t)
	for _, n := range []string{"", "../etc", "Server", "a/b", "server.lock", "-x", "a b", string(make([]byte, 40))} {
		if _, err := r.LockComponents([]string{n}); err == nil {
			t.Errorf("组件名 %q 应被拒绝", n)
		}
	}
}

func TestLockFileSafetyChecks(t *testing.T) {
	r := newReg(t)
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	// 锁路径是符号链接：不跟随，拒绝。
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, r.L.ComponentLockFile("server")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LockComponents([]string{"server"}); err == nil {
		t.Fatal("符号链接锁文件应被拒绝")
	}
	if b, _ := os.ReadFile(target); string(b) != "v" {
		t.Fatalf("符号链接目标被改写: %q", b)
	}
	// 他人可写的锁文件：拒绝（否则任何人可抢占/伪造持有者信息）。
	p := r.L.ComponentLockFile("node")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := r.LockComponents([]string{"node"}); err == nil {
		t.Fatal("group/other 可写的锁文件应被拒绝")
	}
}

func TestKill9ReleasesLockNoStaleLock(t *testing.T) {
	r := newReg(t)
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	c := startChild(t, r.L.Root, "hold", "REGISTRY_COMPS=server,node")
	c.expect("READY")
	// 子进程活着：被拒，持有者信息指向子进程（pid/run_id/command）。
	_, err := r.Begin(context.Background(), TaskRequest{Components: []string{"server"}})
	h := lockedErr(t, err).Holder
	if h == nil || h.PID != c.pid() || h.RunID != "run-child-1" || h.Command != "install server" {
		t.Fatalf("持有者信息 %+v，期望 pid=%d", h, c.pid())
	}
	_, err = r.Begin(context.Background(), TaskRequest{Components: []string{"node", "server"}})
	_ = lockedErr(t, err)

	c.kill9() // SIGKILL：进程没有机会清理任何东西

	// 内核已释放：立即可获取，无需任何「陈旧锁清理」。锁文件里还留着已死进程的内容（说明判活不看它），新持有者覆盖之。
	stale, _ := os.ReadFile(r.L.ComponentLockFile("server"))
	if !contains(string(stale), strconv.Itoa(c.pid())) {
		t.Logf("提示：锁文件内容未保留旧持有者（%q），不影响结论", stale)
	}
	task, err := r.Begin(context.Background(), TaskRequest{Components: []string{"node", "server"}})
	if err != nil {
		t.Fatalf("kill -9 之后应立即可获取（无陈旧锁）: %v", err)
	}
	defer task.Release()
	now, _ := os.ReadFile(r.L.ComponentLockFile("server"))
	if contains(string(now), `"pid":`+strconv.Itoa(c.pid())) || !contains(string(now), `"pid":`+strconv.Itoa(os.Getpid())) {
		t.Fatalf("新持有者应覆盖旧信息: %s", now)
	}
}

func TestGlobalLockSerializesAcrossProcessesAndTimesOut(t *testing.T) {
	r := newReg(t)
	r.opt.GlobalWait = 150 * time.Millisecond
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	c := startChild(t, r.L.Root, "holdglobal")
	c.expect("READY")
	start := time.Now()
	err := r.WithGlobal(context.Background(), func() error { t.Fatal("不应进入临界区"); return nil })
	le := lockedErr(t, err)
	if le.Name != "global" || le.Holder == nil || le.Holder.PID != c.pid() {
		t.Fatalf("持有者 %+v", le)
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 3*time.Second {
		t.Errorf("等待时长 %v 不合理", d)
	}
	c.kill9()
	if err := r.WithGlobal(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("持有者被杀后全局锁应立即可得: %v", err)
	}
	// 上下文取消立即返回。
	ctx, cancel := context.WithCancel(context.Background())
	c2 := startChild(t, r.L.Root, "holdglobal")
	c2.expect("READY")
	r.opt.GlobalWait = time.Minute
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := r.WithGlobal(ctx, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，得到 %v", err)
	}
}

// 并发（-race）：多个 goroutine（各自独立的 Registry 与文件描述）经全局锁做读-改-写，不丢更新。
func TestUpdateStateConcurrentNoLostUpdates(t *testing.T) {
	r := newReg(t)
	r.opt.GlobalWait = 30 * time.Second
	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := New(r.L, Options{RunID: "g" + strconv.Itoa(i), Command: "test", GlobalWait: 30 * time.Second})
			if err := rr.UpdateState(context.Background(), func(s *State) error {
				s.Components["c"+strconv.Itoa(i)] = Component{Version: "1.0.0", State: "installed"}
				return nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	s, _, err := r.LoadState()
	if err != nil || len(s.Components) != n {
		t.Fatalf("丢失更新: %d/%d err=%v", len(s.Components), n, err)
	}
}

// 与 1.27/1.28 的关系：LockedSequence 满足 manifest.SequenceStore，可直接作为 Verifier.Seen。
var _ manifest.SequenceStore = LockedSequence{}

// 多个进程并发推进同一份 trust/state（它们各自持有不同组件的任务锁，组件锁帮不上忙）：
// 经 LockedSequence 后 sequence 只升不降，终值 = 全部写入中的最大值，且每个进程观察到的 Max 单调不减。
func TestLockedSequenceAcrossProcesses(t *testing.T) {
	r := newReg(t)
	if err := r.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	const procs = 6
	cs := make([]*child, procs)
	for i := range cs {
		cs[i] = startChild(t, r.L.Root, "seq", "SEQ_BASE="+strconv.Itoa(i*7))
	}
	for i, c := range cs {
		if l := c.line(); l != "OK" {
			t.Errorf("子进程 %d: %s", i, l)
		}
	}
	var want int64
	for i := 0; i < procs; i++ {
		for k := 1; k <= 150; k++ {
			if v := int64(i*7 + k*13%997 + 1); v > want {
				want = v
			}
		}
	}
	got, ok, err := (manifest.FileSequenceStore{Path: r.L.SequenceFile()}).Max("stable")
	if err != nil || !ok || got != want {
		t.Fatalf("终值 %d ok=%v err=%v，期望 %d", got, ok, err, want)
	}
}
