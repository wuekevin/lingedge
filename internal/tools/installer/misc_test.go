package installer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// AC1：UI 不影响业务。阻塞（无人读的无缓冲通道）、已关闭的通道、慢消费者、nil 通道——安装结果与系统最终状态与「没有 UI」完全一致。
func TestEventConsumerCannotAffectInstall(t *testing.T) {
	baseline := func() (map[string]string, *installer.Result) {
		w := newW(t)
		res := must(t, doInstall(t, w, "node", nil))
		return sysSnap(t, w), res
	}
	want, wantRes := baseline()
	norm := func(m map[string]string) map[string]string { // LastTx 是随机 ULID，安装结果比较的是文件系统，状态文件内容按存在性比较
		out := map[string]string{}
		for k, v := range m {
			if strings.HasPrefix(k, "bin/") { // 伪二进制脚本里嵌着各自世界的绝对路径
				continue
			}
			if k == "var/lib/lingedge/tools/state.json" {
				v = "F state"
			}
			out[k] = v
		}
		return out
	}
	cases := map[string]func() (chan installer.Event, func()){
		"无人读取的无缓冲通道": func() (chan installer.Event, func()) { return make(chan installer.Event), func() {} },
		"已关闭的通道": func() (chan installer.Event, func()) {
			c := make(chan installer.Event, 4)
			close(c)
			return c, func() {}
		},
		"容量为 1 的慢消费者": func() (chan installer.Event, func()) {
			c := make(chan installer.Event, 1)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range c {
					time.Sleep(15 * time.Millisecond)
				}
			}()
			return c, func() { close(c); wg.Wait() }
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			w := newW(t)
			ch, stop := mk()
			done := make(chan outcome, 1)
			go func() {
				done <- doInstall(t, w, "node", []func(*installer.Deps){func(d *installer.Deps) { d.Events = ch }})
			}()
			select {
			case o := <-done:
				stop()
				res := must(t, o)
				if res.Outcome != wantRes.Outcome || len(res.Components) != len(wantRes.Components) {
					t.Fatalf("结果不同: %+v vs %+v", res, wantRes)
				}
				if d := diffSnap(norm(want), norm(sysSnap(t, w))); d != "" {
					t.Fatalf("文件系统结果与无 UI 时不同:\n%s", d)
				}
				if o.in.EventsDropped() == 0 && name != "容量为 1 的慢消费者" {
					t.Error("应记录被丢弃的事件数")
				}
			case <-time.After(60 * time.Second):
				t.Fatal("安装被事件通道拖住了（UI 影响了业务）")
			}
		})
	}
	// 失败路径同样不受影响：回滚在无人读取的通道下照常完成。
	t.Run("回滚路径", func(t *testing.T) {
		w := newW(t)
		before := sysSnap(t, w)
		w.SetCtl("fail-start", true)
		o := doInstall(t, w, "node", []func(*installer.Deps){func(d *installer.Deps) { d.Events = make(chan installer.Event) }})
		if kindOf(t, o.err) != installer.KindRolledBack || diffSnap(before, sysSnap(t, w)) != "" {
			t.Fatalf("%v", o.err)
		}
	})
}

// 并发：同组件的第二个安装 → LockedError（退出码 6）；依赖闭包里的组件被别人持有时，追加锁失败同样得 6，且不留任何副作用。
func TestConcurrentInstallIsLocked(t *testing.T) {
	w := newW(t)
	holder := w.NewInstaller("holder").Deps().Reg
	t1, err := holder.Begin(t.Context(), registry.TaskRequest{Components: []string{"node"}})
	if err != nil {
		t.Fatal(err)
	}
	o := doInstall(t, w, "node", nil)
	var le *registry.LockedError
	if !errors.As(o.err, &le) || le.Name != "node" {
		t.Fatalf("应为 LockedError(node): %v", o.err)
	}
	t1.Release()
	// openresty 被另一个任务持有：node 的依赖闭包追加锁时失败（6），事务尚未开始，没有 journal、没有副作用。
	t2, err := holder.Begin(t.Context(), registry.TaskRequest{Components: []string{"openresty"}})
	if err != nil {
		t.Fatal(err)
	}
	before := sysSnap(t, w)
	o = doInstall(t, w, "node", nil)
	if !errors.As(o.err, &le) || le.Name != "openresty" {
		t.Fatalf("应为 LockedError(openresty): %v", o.err)
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" || len(activeJournals(t, w)) != 0 {
		t.Fatalf("追加锁失败不应有副作用:\n%s\n%v", d, activeJournals(t, w))
	}
	t2.Release()
	must(t, doInstall(t, w, "node", nil)) // 释放后可以安装
}

// 写命令入口 Begin：存在已死亡的未完成事务 → UnfinishedError（13）。
func TestUnfinishedJournalBlocksInstall(t *testing.T) {
	w := newW(t)
	reg := w.NewInstaller("x").Deps().Reg
	task, err := reg.Begin(t.Context(), registry.TaskRequest{Components: []string{"node"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := task.OpenTx(t.Context()); err != nil {
		t.Fatal(err)
	}
	task.Release() // 未 commit/abort 就关闭：保持未完成
	o := doInstall(t, w, "node", nil)
	var ue *registry.UnfinishedError
	if !errors.As(o.err, &ue) {
		t.Fatalf("应为 UnfinishedError: %v", o.err)
	}
}

// 设置校验：未知键被组件 schema 拒绝（KindSettings → 2），不进入任何副作用；通过的设置进入配置文件。
func TestSettingsValidatedBySpec(t *testing.T) {
	w := newW(t)
	before := fullSnap(t, w)
	_, err := plan(t, w, "node", nil, func(r *installer.Request) { r.Settings = map[string]string{"bogus": "1"} })
	if kindOf(t, err) != installer.KindSettings {
		t.Fatalf("%v", err)
	}
	if d := diffSnap(before, fullSnap(t, w)); d != "" {
		t.Fatal(d)
	}
	must(t, doInstall(t, w, "node", nil, func(r *installer.Request) { r.Settings = map[string]string{"greeting": "hello"} }))
	if b, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/node/node.yaml")); !strings.Contains(string(b), "greeting: hello") {
		t.Fatalf("node.yaml: %q", b)
	}
	// server 的内建键。
	w2 := newW(t)
	_, err = plan(t, w2, "server", nil, func(r *installer.Request) { r.Settings = map[string]string{"env": "Bad Env"} })
	if kindOf(t, err) != installer.KindSettings {
		t.Fatalf("%v", err)
	}
	must(t, doInstall(t, w2, "server", nil, func(r *installer.Request) {
		r.Settings = map[string]string{"env": "staging", "trusted-proxies": "10.0.0.0/8, 127.0.0.1"}
	}))
	b, _ := os.ReadFile(filepath.Join(w2.Root, "etc/lingedge/server/server.yaml"))
	if !strings.Contains(string(b), "env: staging") || !strings.Contains(string(b), "'10.0.0.0/8', '127.0.0.1'") {
		t.Fatalf("server.yaml:\n%s", b)
	}
}

// --skip-start：只物化，不启动；状态记为 installed。
func TestSkipStart(t *testing.T) {
	w := newW(t)
	res := must(t, doInstall(t, w, "node", nil, func(r *installer.Request) { r.SkipStart = true }))
	if w.FakeHas("active-lingedge-node.service") || !w.FakeHas("enabled-lingedge-node.service") {
		t.Error("应已启用但未启动")
	}
	if res.Components[0].State != "installed" {
		t.Errorf("状态: %+v", res.Components)
	}
}

// 步骤表与设计文档 tools-design §12.1 的 22 步逐行一致（编号、阶段、标题）。
func TestStepsMatchDesignDoc(t *testing.T) {
	b, err := os.ReadFile("../../../docs/architecture/tools-design.md")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\| (\d{1,2}) \| ([^|]+) \| ([PFMAC]) \|`)
	rows := re.FindAllStringSubmatch(string(b), -1)
	if len(rows) != 22 {
		t.Fatalf("设计文档 §12.1 应有 22 行，解析到 %d", len(rows))
	}
	phase := map[string]installer.Phase{"P": installer.PhasePreflight, "F": installer.PhaseFetch, "M": installer.PhaseMaterialize, "A": installer.PhaseActivate, "C": installer.PhaseCommit}
	for i, r := range rows {
		s := installer.Steps[i]
		if r[1] != itoa(s.N) || s.N != i+1 || phase[r[3]] != s.Phase || !strings.HasPrefix(s.Title, strings.TrimSpace(r[2])) {
			t.Errorf("第 %d 行不一致：文档=%v 代码=%+v", i+1, r[1:], s)
		}
		if installer.StepName(s.N) != s.Name || installer.PhaseOf(s.N) != s.Phase {
			t.Errorf("StepName/PhaseOf 不一致: %d", s.N)
		}
	}
}

// Undoer 不盲信 journal：路径形状不符、未知动作一律拒绝；Created=false 不动；目标已不存在视为完成。
func TestUndoerDoesNotTrustJournal(t *testing.T) {
	w := newW(t)
	in := w.NewInstaller("x")
	u := &installer.Undoer{Root: w.Root, Reg: in.Deps().Reg}
	ctx := context.Background()
	bad := []registry.UndoAction{
		{Action: "remove_release", Path: "/etc/passwd", Created: true},
		{Action: "remove_release", Path: "/opt/lingedge/../etc", Created: true},
		{Action: "unlink_current", Path: "/opt/lingedge/node/releases/x", Created: true},
		{Action: "remove_opt_dir", Path: "/opt", Created: true},
		{Action: "remove_staging", Path: "/var/lib/lingedge/tools", Created: true},
		{Action: "remove_staging", Path: filepath.Join(w.Layout.StagingDir(), "..", "state.json")},
		{Action: "state_forget", Extra: map[string]string{"component": "../x", "tx": "short"}},
		{Action: "rm_rf_everything", Path: "/"},
	}
	for _, a := range bad {
		if _, err := u.Apply(ctx, a); err == nil {
			t.Errorf("应拒绝 %+v", a)
		}
	}
	// Created=false → noop；不存在 → absent。
	rel := "/opt/lingedge/node/releases/0.1.0"
	if err := os.MkdirAll(filepath.Join(w.Root, rel), 0o755); err != nil {
		t.Fatal(err)
	}
	if r, err := u.Apply(ctx, registry.UndoAction{Action: "remove_release", Path: rel}); err != nil || r.Outcome != "noop" {
		t.Fatalf("%v %v", r, err)
	}
	if !w.Has("opt/lingedge/node/releases/0.1.0") {
		t.Fatal("Created=false 不得删除")
	}
	if r, err := u.Apply(ctx, registry.UndoAction{Action: "remove_release", Path: rel, Created: true}); err != nil || r.Outcome != "done" {
		t.Fatalf("%v %v", r, err)
	}
	if r, err := u.Apply(ctx, registry.UndoAction{Action: "remove_release", Path: rel, Created: true}); err != nil || r.Outcome != "absent" {
		t.Fatalf("幂等: %v %v", r, err)
	}
	// 符号链接冒充目录：不跟随、不删除。
	_ = os.MkdirAll(filepath.Join(w.Root, "opt/lingedge/node/releases"), 0o755)
	keep := filepath.Join(w.Root, "keep")
	_ = os.MkdirAll(keep, 0o755)
	_ = os.Symlink(keep, filepath.Join(w.Root, rel))
	if r, err := u.Apply(ctx, registry.UndoAction{Action: "remove_release", Path: rel, Created: true}); err != nil || r.Outcome != "skipped" {
		t.Fatalf("符号链接: %v %v", r, err)
	}
	if !w.Has("keep") {
		t.Fatal("符号链接指向的目录被删了")
	}
}

var _ = toolsworld.T0
