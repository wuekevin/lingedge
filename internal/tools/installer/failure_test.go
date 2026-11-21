package installer_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// AC4（状态机表驱动 + journal 故障注入）：在 journal 写 begin / done 的每一处注入失败，已完成的步骤必须逆序撤销，系统回到安装前的快照；
// 退出码分类：激活前失败保持原因的映射（步骤失败），激活后（19–21）为 ROLLED_BACK(10)。
func TestJournalFaultAtEveryStepRollsBack(t *testing.T) {
	type tc struct {
		step  int
		begin bool
		kind  installer.Kind
	}
	var cases []tc
	for n := 10; n <= 21; n++ {
		if n == 21 {
			cases = append(cases, tc{n, true, installer.KindRolledBack}) // 21 done 失败发生在提交点之后：见 TestFaultAfterCommitPointDoesNotRollBack
			continue
		}
		k := installer.KindStepFailed
		if installer.PhaseOf(n) == installer.PhaseActivate || installer.PhaseOf(n) == installer.PhaseCommit {
			k = installer.KindRolledBack
		}
		cases = append(cases, tc{n, true, k}, tc{n, false, k})
	}
	for _, c := range cases {
		where := "done"
		if c.begin {
			where = "begin"
		}
		t.Run(fmt.Sprintf("step%02d_%s", c.step, where), func(t *testing.T) {
			w := newW(t)
			before := sysSnap(t, w)
			var begins, dones []int
			if c.begin {
				begins = []int{c.step}
			} else {
				dones = []int{c.step}
			}
			o := doInstall(t, w, "node", []func(*installer.Deps){failAt(begins, dones)})
			if o.err == nil {
				t.Fatal("注入故障后安装不应成功")
			}
			var ie *installer.Error
			if !errors.As(o.err, &ie) || ie.Kind != c.kind || ie.Step != c.step || ie.SystemState != "rolled_back" {
				t.Fatalf("错误: %+v（期望 kind=%s step=%d rolled_back）", o.err, c.kind, c.step)
			}
			// 注入的是写 journal 的故障：回滚按磁盘上的 journal 撤销，系统回到安装前；journal 以 abort 结束并归档。
			if d := diffSnap(before, sysSnap(t, w)); d != "" {
				t.Fatalf("回滚后系统与安装前不一致:\n%s", d)
			}
			if len(activeJournals(t, w)) != 0 {
				t.Errorf("回滚成功后不应留有未完成的 journal: %v", activeJournals(t, w))
			}
		})
	}
}

// 提交点之后（第 21 步 done 之后）的 journal 故障不得回滚已提交的安装：状态已原子提交，journal 留给 recover --resume 收尾。
func TestFaultAfterCommitPointDoesNotRollBack(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(*installer.Deps)
	}{
		{"21 done 失败", failAt(nil, []int{21})}, {"22 begin 失败", failAt([]int{22}, nil)}, {"22 done 失败", failAt(nil, []int{22})},
		{"commit 失败", wrapJournal(func(j installer.Journal) installer.Journal { return &faultJournal{Journal: j, failCommit: true} })},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newW(t)
			o := doInstall(t, w, "server", []func(*installer.Deps){c.mut})
			if o.err != nil {
				t.Fatalf("提交点之后的 journal 故障不应让安装失败: %v", o.err)
			}
			st, _, _ := w.Registry("x").LoadState()
			if st.Components["server"].LastTx == "" {
				t.Fatal("状态应已提交")
			}
			var tok bool
			for _, a := range o.res.Access {
				tok = tok || a.Kind == "setup_token"
			}
			if !tok {
				t.Error("即使 journal 无法收尾，也必须交付只显示一次的 setup token")
			}
			if len(o.res.Warnings) == 0 {
				t.Error("应给出 journal 未收尾的警告")
			}
			// 下一次写命令得到 13（存在未完成事务）；recover --resume 只补写收尾记录并归档，不改动系统。
			if len(activeJournals(t, w)) != 1 {
				t.Fatalf("应留有 1 个未完成的 journal: %v", activeJournals(t, w))
			}
			after := sysSnap(t, w)
			in := w.NewInstaller("run-rec")
			r, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverResume})
			if err != nil || len(r.Transactions) != 1 || r.Transactions[0].Action != "finalized" {
				t.Fatalf("resume 应收尾: %+v %v", r, err)
			}
			if d := diffSnap(after, sysSnap(t, w)); d != "" {
				t.Fatalf("收尾不应改动系统:\n%s", d)
			}
			if len(activeJournals(t, w)) != 0 || len(archivedJournals(t, w)) != 1 {
				t.Fatalf("journal 应已归档")
			}
			ops := journalOps(t, archivedJournals(t, w)[0])
			if ops[len(ops)-1] != "commit" {
				t.Errorf("journal 应以 commit 结束: %v", ops)
			}
		})
	}
}

// 真实组件失败路径：每一种都必须回到安装前，并映射到正确的错误分类。
func TestComponentFailuresRollBack(t *testing.T) {
	cases := []struct {
		name  string
		setup func(w *toolsworld.World)
		kind  installer.Kind
		step  int
	}{
		{"useradd 失败（步骤 15）", func(w *toolsworld.World) { w.SetCtl("fail-useradd", true) }, installer.KindStepFailed, 15},
		{"systemctl enable 失败（步骤 17）", func(w *toolsworld.World) { w.SetCtl("fail-enable", true) }, installer.KindStepFailed, 17},
		{"启动失败（步骤 19）", func(w *toolsworld.World) { w.SetCtl("fail-start", true) }, installer.KindRolledBack, 19},
		{"健康检查失败（步骤 20）", func(w *toolsworld.World) { w.SetCtl("fail-health-node", true) }, installer.KindRolledBack, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newW(t)
			before := sysSnap(t, w)
			c.setup(w)
			o := doInstall(t, w, "node", []func(*installer.Deps){func(d *installer.Deps) {
				d.Config.Install.HealthTimeout, d.Config.Install.HealthInterval = "30ms", "10ms"
			}})
			var ie *installer.Error
			if !errors.As(o.err, &ie) || ie.Kind != c.kind || ie.Step != c.step {
				t.Fatalf("错误: %v（期望 %s 第 %d 步）", o.err, c.kind, c.step)
			}
			if d := diffSnap(before, sysSnap(t, w)); d != "" {
				t.Fatalf("回滚后不一致:\n%s", d)
			}
			if w.FakeHas("users/lingedge-node") || w.FakeHas("active-lingedge-node.service") || w.FakeHas("enabled-lingedge-node.service") {
				t.Error("伪 systemd/用户状态应已恢复（用户已删除、服务已停止并取消启用）")
			}
			// 回滚时单元被撤销后做了 daemon-reload。
			if c.step >= 17 && !w.FakeHas("reloads") {
				t.Error("应执行过 daemon-reload")
			}
		})
	}
}

// 既有用户不会被回滚删除（Created=false 只在本事务创建时才撤销）。
func TestRollbackNeverDeletesPreExistingUser(t *testing.T) {
	w := newW(t)
	if err := os.WriteFile(filepath.Join(w.Fake, "users", "lingedge-node"), []byte("998\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.SetCtl("fail-start", true)
	o := doInstall(t, w, "node", nil)
	if kindOf(t, o.err) != installer.KindRolledBack {
		t.Fatalf("%v", o.err)
	}
	if !w.FakeHas("users/lingedge-node") {
		t.Fatal("预先存在的运行用户不应被回滚删除")
	}
}

// --no-rollback：健康检查失败保留现场，状态记为 failed，退出码 11 的 Kind。
func TestHealthFailureWithNoRollbackKeepsEverything(t *testing.T) {
	w := newW(t)
	w.SetCtl("fail-health-node", true)
	o := doInstall(t, w, "node", []func(*installer.Deps){func(d *installer.Deps) {
		d.Config.Install.HealthTimeout, d.Config.Install.HealthInterval = "30ms", "10ms"
	}},
		func(r *installer.Request) { r.NoRollback = true })
	if kindOf(t, o.err) != installer.KindHealthFailed {
		t.Fatalf("%v", o.err)
	}
	st, _, _ := w.Registry("x").LoadState()
	if st.Components["node"].State != "failed" {
		t.Fatalf("状态应为 failed: %+v", st.Components["node"])
	}
	if !w.Has("opt/lingedge/node/current") || len(activeJournals(t, w)) != 0 {
		t.Fatal("现场应保留且 journal 已提交归档")
	}
}

// 撤销失败 → KindRollbackFailed（退出码 12）且 journal 保留；排除故障后 recover --rollback 成功并恢复系统。
func TestUndoFailureKeepsJournalAndRecoverFinishes(t *testing.T) {
	w := newW(t)
	before := sysSnap(t, w)
	w.SetCtl("fail-start", true) // 触发回滚
	w.SetCtl("fail-userdel", true)
	o := doInstall(t, w, "node", nil)
	var ie *installer.Error
	if !errors.As(o.err, &ie) || ie.Kind != installer.KindRollbackFailed || ie.SystemState != "needs_manual" {
		t.Fatalf("应为 rollback_failed: %v", o.err)
	}
	if len(activeJournals(t, w)) != 1 {
		t.Fatalf("journal 必须保留: %v", activeJournals(t, w))
	}
	// 与 12 互斥的 10 不会出现；此时用户（fake）仍在，其余步骤（逆序，userdel 是 15 步，之前 16–19 已撤销）已撤销。
	if !w.FakeHas("users/lingedge-node") {
		t.Fatal("userdel 失败，用户应仍在")
	}
	w.SetCtl("fail-userdel", false)
	in := w.NewInstaller("run-rec")
	r, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback})
	if err != nil || len(r.Transactions) != 1 || r.Transactions[0].Action != "rolled_back" {
		t.Fatalf("recover: %+v %v", r, err)
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" {
		t.Fatalf("recover 后应回到安装前:\n%s", d)
	}
	if w.FakeHas("users/lingedge-node") {
		t.Error("recover 应删除本事务创建的用户")
	}
}

// 撤销是幂等的：回滚中途失败（只撤销了前几步），再次 recover 从未完成处继续，最终与安装前一致；已 undone 的步骤不再重复。
func TestUndoIsIdempotentAfterPartialRollback(t *testing.T) {
	w := newW(t)
	before := sysSnap(t, w)
	w.SetCtl("fail-start", true)
	calls := 0
	failAfter := func(n int) func(*installer.Deps) {
		return func(d *installer.Deps) {
			d.WrapUndoer = func(u installer.UndoApplier) installer.UndoApplier {
				return undoFn(func(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
					calls++
					if calls > n {
						return service.UndoResult{}, errors.New("injected undo failure")
					}
					return u.Apply(ctx, a)
				})
			}
		}
	}
	o := doInstall(t, w, "node", []func(*installer.Deps){failAfter(5)})
	if kindOf(t, o.err) != installer.KindRollbackFailed {
		t.Fatalf("%v", o.err)
	}
	if diffSnap(before, sysSnap(t, w)) == "" {
		t.Fatal("回滚只做了一半，系统不应已完全恢复")
	}
	w.SetCtl("fail-start", false)
	in := w.NewInstaller("run-rec")
	if _, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback}); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" {
		t.Fatalf("recover 后应回到安装前:\n%s", d)
	}
	// 再来一次 recover 是空操作。
	r, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback})
	if err != nil || len(r.Transactions) != 0 {
		t.Fatalf("没有未完成事务时应为空: %+v %v", r, err)
	}
}

type undoFn func(ctx context.Context, a registry.UndoAction) (service.UndoResult, error)

func (f undoFn) Apply(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
	return f(ctx, a)
}

// 回滚时 journal 写入失败：其余步骤仍尽力撤销，journal 保留，退出码 12。
func TestRollbackWithBrokenJournalKeepsIt(t *testing.T) {
	w := newW(t)
	w.SetCtl("fail-start", true)
	o := doInstall(t, w, "node", []func(*installer.Deps){wrapJournal(func(j installer.Journal) installer.Journal { return &faultJournal{Journal: j, failUndone: true} })})
	if kindOf(t, o.err) != installer.KindRollbackFailed {
		t.Fatalf("%v", o.err)
	}
	if len(activeJournals(t, w)) != 1 {
		t.Fatal("journal 必须保留")
	}
	// recover 复核（撤销幂等）后正常收尾。
	w.SetCtl("fail-start", false)
	in := w.NewInstaller("run-rec")
	if _, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback}); err != nil {
		t.Fatal(err)
	}
	if len(activeJournals(t, w)) != 0 {
		t.Fatal("recover 后 journal 应已归档")
	}
}

// SIGINT 语义（进程内 context 取消）：物化阶段的原子步骤（14–18）与提交点（21）在收到取消后仍完整执行到 done（不在半步中退出），
// 之后才回滚，Kind=Interrupted（退出码 130），系统恢复。步骤 17 的 daemon-reload/enable 经带 ctx 的外部命令——若步骤不是原子的，取消会让它半途失败。
func TestCancelDuringMaterializeCompletesStepThenRollsBack(t *testing.T) {
	for _, n := range []int{14, 15, 16, 17, 18} {
		t.Run(fmt.Sprintf("步骤%d", n), func(t *testing.T) {
			w := newW(t)
			before := sysSnap(t, w)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var ran []string
			begun := fmt.Sprintf("begun:%d", n)
			o := doInstallCtx(t, ctx, w, "node", []func(*installer.Deps){func(d *installer.Deps) {
				d.Hook = func(p string) {
					ran = append(ran, p)
					if p == begun { // 该步刚写完 begin：此刻「收到 SIGINT」
						cancel()
					}
				}
			}})
			var ie *installer.Error
			if !errors.As(o.err, &ie) || ie.Kind != installer.KindInterrupted || ie.SystemState != "rolled_back" {
				t.Fatalf("应为 interrupted: %v", o.err)
			}
			joined := strings.Join(ran, " ")
			if !strings.Contains(joined, fmt.Sprintf("done:%d", n)) || strings.Contains(joined, fmt.Sprintf("before:%d", n+1)) {
				t.Fatalf("步骤 %d 应完整执行到 done 并在之后停止: %s", n, joined)
			}
			if d := diffSnap(before, sysSnap(t, w)); d != "" {
				t.Fatalf("回滚后不一致:\n%s", d)
			}
		})
	}
}
