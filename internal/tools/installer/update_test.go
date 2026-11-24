package installer_test

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/rollback"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.43：升级编排（13 步、journal 撤销、决策表无库路径、保留策略、手动回滚、recover）。
// 世界：真实临时目录树 + 伪 systemctl + 真实签名的离线 bundle（toolsworld.BuildBundle，生产的清单/包格式与验签链）+ 假组件规格；
// 被测的 PlanUpdate/Update/Rollback、journal、撤销、registry、downloader、resolver、backup 都是生产代码。有数据库的路径（migrate 决策表四类）见 tests/integration/toolsinstall。

func nodeV(w *toolsworld.World, ver string) []toolsworld.BundleComp {
	return []toolsworld.BundleComp{
		{Name: "node", Version: ver, Requires: map[string]any{"components": map[string]string{"openresty": ">=0.1.0"}},
			Ports: []map[string]any{{"proto": "tcp", "port": 9100, "bind": "127.0.0.1", "purpose": "agent"}},
			Files: []toolsworld.PkgFile{{Name: "bin/lingedge-node", Body: "#!node-binary-" + ver + "\n", Exec: true}}},
		{Name: "openresty", Version: "0.1.0", Internal: true, Files: []toolsworld.PkgFile{{Name: "bin/openresty", Body: "#!openresty-v1\n", Exec: true}}},
	}
}

// newUpdW 建立世界并安装 node 0.1.0（openresty 随依赖安装）。
func newUpdW(t *testing.T) *toolsworld.World {
	t.Helper()
	w := newW(t)
	must(t, doInstall(t, w, "node", nil))
	return w
}

func bundleFor(t *testing.T, w *toolsworld.World, name string, seq int64, comps []toolsworld.BundleComp, rev ...toolsworld.RevokedVersion) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name+".tar")
	if err := w.BuildBundle(p, seq, comps, rev); err != nil {
		t.Fatal(err)
	}
	return p
}

func updEnv(w *toolsworld.World) *installer.UpdateEnv {
	r, err := service.NewRunner([]string{w.Bin})
	if err != nil {
		panic(err)
	}
	svc := &backup.Service{Root: w.Root, Reg: w.Registry("upd"), Sys: &service.Systemctl{Run: r, Booted: func() bool { return true }}, Own: w.Own,
		Now: func() time.Time { return toolsworld.T0 }, Hostname: func() (string, error) { return "edge-host-1", nil }, Geteuid: func() int { return 0 }, ToolsVersion: "test",
		KDF: backup.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}}
	return &installer.UpdateEnv{Backup: svc, Rollback: &rollback.Executor{Root: w.Root, Backup: svc}, BackupDir: filepath.Join(w.Root, "var/lib/lingedge/backups")}
}

type updOut struct {
	res *installer.UpdateResult
	err error
	in  *installer.Installer
}

func doUpdate(t *testing.T, w *toolsworld.World, comp, bundle string, mutDeps []func(*installer.Deps), mut ...func(*installer.UpdateRequest)) updOut {
	t.Helper()
	deps := append([]func(*installer.Deps){func(d *installer.Deps) {
		d.Update = updEnv(w)
		d.Config.Install.HealthTimeout, d.Config.Install.HealthInterval = "60ms", "10ms"
	}}, mutDeps...)
	in := w.NewInstaller("run-upd", deps...)
	task, err := in.Deps().Reg.Begin(t.Context(), registry.TaskRequest{Components: []string{comp}})
	if err != nil {
		return updOut{err: err, in: in}
	}
	defer task.Release()
	req := installer.UpdateRequest{Component: comp, Channel: "stable", Offline: bundle, NonInteractive: true}
	for _, m := range mut {
		m(&req)
	}
	res, err := in.Update(t.Context(), task, req)
	return updOut{res: res, err: err, in: in}
}

func mustUpd(t *testing.T, o updOut) *installer.UpdateResult {
	t.Helper()
	if o.err != nil {
		t.Fatalf("update 失败: %v", o.err)
	}
	return o.res
}

func recDeps(w *toolsworld.World) func(*installer.Deps) {
	return func(d *installer.Deps) {
		d.Update = updEnv(w)
		d.Config.Install.HealthTimeout, d.Config.Install.HealthInterval = "60ms", "10ms"
	}
}

func instErr(t *testing.T, err error) *installer.Error {
	t.Helper()
	var ie *installer.Error
	if !errors.As(err, &ie) {
		t.Fatalf("不是 installer 错误: %T %v", err, err)
	}
	return ie
}

// updSnap 是升级测试的「系统状态」：sysSnap 之外再排除备份目录（备份是证据，失败后也保留）与 staging。
func updSnap(t *testing.T, w *toolsworld.World) map[string]string {
	out := map[string]string{}
	for k, v := range sysSnap(t, w) {
		if strings.HasPrefix(k, "var/lib/lingedge/backups") {
			continue
		}
		out[k] = v
	}
	return out
}

func curTarget(t *testing.T, w *toolsworld.World, comp string) string {
	t.Helper()
	l, err := os.Readlink(filepath.Join(w.Root, "opt/lingedge", comp, "current"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func compState(t *testing.T, w *toolsworld.World, comp string) registry.Component {
	t.Helper()
	st, _, err := w.Registry("x").LoadState()
	if err != nil {
		t.Fatal(err)
	}
	return st.Components[comp]
}

func TestUpdateNodeHappyPath(t *testing.T) {
	w := newUpdW(t)
	b2 := bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0"))
	res := mustUpd(t, doUpdate(t, w, "node", b2, nil))
	if res.Outcome != installer.OutcomeUpdated || res.From != "0.1.0" || res.To != "0.2.0" || res.Path != registry.PathBinary {
		t.Fatalf("结果: %+v", res)
	}
	if got := curTarget(t, w, "node"); got != "releases/0.2.0" {
		t.Fatalf("current=%s", got)
	}
	c := compState(t, w, "node")
	if c.Version != "0.2.0" || c.PrevVersion != "0.1.0" || c.Upgrade == nil || c.Upgrade.From != "0.1.0" || c.Upgrade.Path != registry.PathBinary || c.Upgrade.Backup == "" || c.LastTx != res.Tx {
		t.Fatalf("state: %+v %+v", c, c.Upgrade)
	}
	if !w.FakeHas("active-lingedge-node.service") {
		t.Error("升级后 node 应在运行")
	}
	// 升级前备份存在、已校验可读；旧 release 保留。
	if !res.BackupVerified || res.Backup == "" {
		t.Errorf("备份: %+v", res)
	}
	if _, err := os.Stat(res.Backup); err != nil {
		t.Errorf("备份文件: %v", err)
	}
	if !w.Has("opt/lingedge/node/releases/0.1.0") || !w.Has("opt/lingedge/node/releases/0.2.0") {
		t.Error("旧 release 应保留（保留数 2）")
	}
	// journal 已提交并归档，13 步齐全。
	if len(activeJournals(t, w)) != 0 {
		t.Fatal("成功后不应有活动 journal")
	}
	arc, _ := filepath.Glob(filepath.Join(w.Layout.ArchiveDir(), res.Tx+"*"))
	if len(arc) != 1 {
		t.Fatalf("归档 journal: %v", arc)
	}
	ops := strings.Join(journalOps(t, arc[0]), " ")
	for n := 1; n <= installer.UpTotalSteps; n++ {
		if !strings.Contains(ops, fmt.Sprintf("begin:%d done:%d", n, n)) {
			t.Errorf("journal 缺第 %d 步: %s", n, ops)
		}
	}
	if !strings.HasSuffix(ops, "commit") {
		t.Errorf("journal 未以 commit 结束: %s", ops)
	}
	// 冒烟：升级后 doctor 之外的二进制内容确为新版本。
	if b, _ := os.ReadFile(filepath.Join(w.Root, "opt/lingedge/node/current/bin/lingedge-node")); string(b) != "#!node-binary-0.2.0\n" {
		t.Errorf("current 的二进制不是新版本: %q", b)
	}
}

// --dry-run/--plan-only 与真实升级同源：PlanUpdate 对整棵根目录零变化。
func TestPlanUpdateIsReadOnlyAndSameSource(t *testing.T) {
	w := newUpdW(t)
	b2 := bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0"))
	before := fullSnap(t, w)
	in := w.NewInstaller("run-plan", recDeps(w))
	plan, err := in.PlanUpdate(t.Context(), installer.UpdateRequest{Component: "node", Channel: "stable", Offline: b2, NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != "upgrade" || plan.From != "0.1.0" || plan.To != "0.2.0" || len(plan.Steps) != 13 || !plan.Backup.Planned || plan.Decision.Path != registry.PathBinary {
		t.Fatalf("计划: %+v", plan)
	}
	if d := diffSnap(before, fullSnap(t, w)); d != "" {
		t.Fatalf("PlanUpdate 不得有任何变化:\n%s", d)
	}
	// 同源：真实升级的结果与计划一致。
	res := mustUpd(t, doUpdate(t, w, "node", b2, nil))
	if res.From != plan.From || res.To != plan.To || res.Path != plan.Decision.Path {
		t.Fatalf("计划与结果不一致: %+v vs %+v", plan, res)
	}
}

func TestUpdateNoopAndRefusals(t *testing.T) {
	w := newUpdW(t)
	// 已是目标版本：no-op，不动任何东西。
	same := bundleFor(t, w, "same", 11, nodeV(w, "0.1.0"))
	before := updSnap(t, w)
	r := mustUpd(t, doUpdate(t, w, "node", same, nil))
	if r.Outcome != installer.OutcomeAlreadyCurrent {
		t.Fatalf("%+v", r)
	}
	// 未安装的组件。
	o := doUpdate(t, w, "server", same, nil)
	if o.err == nil || instErr(t, o.err).Kind != installer.KindNotInstalled {
		t.Fatalf("未安装: %v", o.err)
	}
	if d := diffSnap(before, updSnap(t, w)); d != "" {
		t.Fatalf("no-op 与拒绝都不得改动任何东西:\n%s", d)
	}
	// min_upgrade_from：目标要求已装版本 ≥ 0.1.5，当前 0.1.0 → 须逐级升级（resolver 拒绝，系统未改动）。
	comps := nodeV(w, "0.3.0")
	comps[0].MinUpgradeFrom = "0.1.5"
	o = doUpdate(t, w, "node", bundleFor(t, w, "min", 12, comps), nil)
	var re *resolver.Error
	if o.err == nil || !errors.As(o.err, &re) {
		t.Fatalf("min_upgrade_from 应被 resolver 拒绝: %v", o.err)
	}
	// 降级：--version 指向低于已装的版本 → 不支持降级。
	mustUpd(t, doUpdate(t, w, "node", bundleFor(t, w, "v2", 13, nodeV(w, "0.2.0")), nil))
	o = doUpdate(t, w, "node", bundleFor(t, w, "v1again", 14, nodeV(w, "0.1.0")), nil, func(r *installer.UpdateRequest) { r.Version = "0.1.0" })
	if o.err == nil || !strings.Contains(o.err.Error(), "降级") {
		t.Fatalf("降级应被拒绝: %v", o.err)
	}
}

// 失败的升级不留半状态：每个 journal 写入点注入失败（begin 与 done），系统回到升级前；第 13 步（提交点）之后不回滚。
func TestUpdateJournalFaultAtEveryStep(t *testing.T) {
	type tc struct {
		step  int
		begin bool
	}
	var cases []tc
	for n := 1; n <= installer.UpTotalSteps; n++ {
		cases = append(cases, tc{n, true}, tc{n, false})
	}
	for _, c := range cases {
		where := "done"
		if c.begin {
			where = "begin"
		}
		t.Run(fmt.Sprintf("step%02d_%s", c.step, where), func(t *testing.T) {
			w := newUpdW(t)
			b2 := bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0"))
			before := updSnap(t, w)
			var begins, dones []int
			if c.begin {
				begins = []int{c.step}
			} else {
				dones = []int{c.step}
			}
			o := doUpdate(t, w, "node", b2, []func(*installer.Deps){failAt(begins, dones)})
			if c.step == installer.UpStepCommit && !c.begin {
				// 提交点：state.json 已原子写入，之后 journal 的故障不得回滚已提交的升级。
				if o.err != nil || compState(t, w, "node").Version != "0.2.0" {
					t.Fatalf("提交点之后的故障不应让升级失败: %v", o.err)
				}
				return
			}
			if o.err == nil {
				t.Fatal("注入故障后升级不应成功")
			}
			ie := instErr(t, o.err)
			want := installer.KindStepFailed
			if c.step >= installer.UpStepStop {
				want = installer.KindRolledBack
			}
			if ie.Kind != want {
				t.Fatalf("错误分类 %s，期望 %s：%v", ie.Kind, want, o.err)
			}
			if d := diffSnap(before, updSnap(t, w)); d != "" {
				t.Fatalf("回滚后系统与升级前不一致:\n%s", d)
			}
			if len(activeJournals(t, w)) != 0 {
				t.Errorf("回滚成功后不应留有未完成的 journal: %v", activeJournals(t, w))
			}
			if !w.FakeHas("active-lingedge-node.service") {
				t.Error("回滚后旧版本应在运行")
			}
		})
	}
}

// 构造一个健康检查失败的新版本：自动回滚、服务恢复（旧版本健康）、与升级前一致；状态不变。
func TestUpdateHealthFailureRollsBack(t *testing.T) {
	w := newUpdW(t)
	b2 := bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0"))
	w.SetCtl("fail-health-node-0.2.0", true)
	before := updSnap(t, w)
	o := doUpdate(t, w, "node", b2, nil)
	ie := instErr(t, o.err)
	if ie.Kind != installer.KindRolledBack || ie.SystemState != "rolled_back" {
		t.Fatalf("应为 rolled_back: %v", o.err)
	}
	if d := diffSnap(before, updSnap(t, w)); d != "" {
		t.Fatalf("回滚后系统与升级前不一致:\n%s", d)
	}
	rb, _ := ie.Details["rollback"].(installer.RollbackReport)
	if rb.Version != "0.1.0" || rb.Health != "healthy" {
		t.Fatalf("回滚复核报告: %+v", ie.Details["rollback"])
	}
	// 备份保留（证据）。
	if ents, _ := os.ReadDir(filepath.Join(w.Root, "var/lib/lingedge/backups")); len(ents) != 1 {
		t.Errorf("升级前备份应保留: %v", ents)
	}
	// 信任库 sequence 不因失败的升级而推进（Finish 失败时不提交）。
	b, _ := os.ReadFile(w.Layout.SequenceFile())
	if strings.Contains(string(b), `"stable": 11`) || strings.Contains(string(b), `"stable":11`) {
		t.Errorf("失败的升级不得推进 sequence: %s", b)
	}
	// --no-rollback：保留现场，状态记为 failed，退出 KindHealthFailed 之外的结果带 failed 状态。
	w.SetCtl("fail-health-node-0.2.0", true)
	r := mustUpd(t, doUpdate(t, w, "node", b2, nil, func(r *installer.UpdateRequest) { r.NoRollback = true }))
	if c := compState(t, w, "node"); c.State != "failed" || c.Version != "0.2.0" || c.Upgrade == nil {
		t.Fatalf("--no-rollback 应保留现场且记为 failed: %+v", c)
	}
	_ = r
}

// 回滚本身失败：KindRollbackFailed、journal 保留；修复后 recover --rollback 回到升级前（含复核），再 recover 为空操作。
func TestUpdateRollbackFailureKeepsJournalAndRecoverFinishes(t *testing.T) {
	w := newUpdW(t)
	b2 := bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0"))
	before := updSnap(t, w)
	w.SetCtl("fail-health-node-0.2.0", true) // 触发回滚
	calls := 0
	o := doUpdate(t, w, "node", b2, []func(*installer.Deps){func(d *installer.Deps) {
		d.WrapUndoer = func(u installer.UndoApplier) installer.UndoApplier {
			return undoFn(func(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
				calls++
				if a.Action == installer.ActUpdateSwitchCurrent {
					return service.UndoResult{}, errors.New("injected undo failure")
				}
				return u.Apply(ctx, a)
			})
		}
	}})
	ie := instErr(t, o.err)
	if ie.Kind != installer.KindRollbackFailed || ie.SystemState != "needs_manual" {
		t.Fatalf("应为 rollback_failed: %v", o.err)
	}
	if len(activeJournals(t, w)) != 1 {
		t.Fatalf("journal 必须保留: %v", activeJournals(t, w))
	}
	if curTarget(t, w, "node") != "releases/0.2.0" {
		t.Fatal("切回失败，current 应仍在新版本")
	}
	w.SetCtl("fail-health-node-0.2.0", false) // 修复：旧版本本来就健康；这里去掉故障注入
	in := w.NewInstaller("run-rec", recDeps(w))
	r, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback})
	if err != nil || len(r.Transactions) != 1 || r.Transactions[0].Action != "rolled_back" {
		t.Fatalf("recover: %+v %v", r, err)
	}
	if d := diffSnap(before, updSnap(t, w)); d != "" {
		t.Fatalf("recover 后应回到升级前:\n%s", d)
	}
	if r, err = in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback}); err != nil || len(r.Transactions) != 0 {
		t.Fatalf("再次 recover 应为空: %+v %v", r, err)
	}
}

// 提交点之后被中断（state.json 已写入、journal 未收尾）：recover 两种模式都只补写收尾，绝不回滚已提交的升级。
func TestRecoverFinalizesCommittedUpdate(t *testing.T) {
	for _, mode := range []installer.RecoverMode{installer.RecoverRollback, installer.RecoverResume} {
		t.Run(string(mode), func(t *testing.T) {
			w := newUpdW(t)
			b2 := bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0"))
			o := doUpdate(t, w, "node", b2, []func(*installer.Deps){failAt(nil, []int{13})})
			if o.err != nil {
				t.Fatal(o.err)
			}
			if len(activeJournals(t, w)) != 1 {
				t.Fatal("journal 应保持未完成")
			}
			in := w.NewInstaller("run-rec", recDeps(w))
			r, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: mode})
			if err != nil || len(r.Transactions) != 1 || r.Transactions[0].Action != "finalized" {
				t.Fatalf("recover: %+v %v", r, err)
			}
			if compState(t, w, "node").Version != "0.2.0" || curTarget(t, w, "node") != "releases/0.2.0" {
				t.Fatal("已提交的升级不得被回滚")
			}
		})
	}
}

// 提交点之前被中断的升级：--resume 被拒绝（状态不确定），--rollback 回到升级前。
func TestRecoverUncommittedUpdateRefusesResume(t *testing.T) {
	w := newUpdW(t)
	b2 := bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0"))
	before := updSnap(t, w)
	w.SetCtl("fail-health-node-0.2.0", true)
	o := doUpdate(t, w, "node", b2, []func(*installer.Deps){wrapJournal(func(j installer.Journal) installer.Journal { return &faultJournal{Journal: j, failUndone: true} })})
	if instErr(t, o.err).Kind != installer.KindRollbackFailed {
		t.Fatalf("%v", o.err)
	}
	w.SetCtl("fail-health-node-0.2.0", false)
	in := w.NewInstaller("run-rec", recDeps(w))
	if _, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverResume}); err == nil || instErr(t, err).Kind != installer.KindRecover {
		t.Fatalf("--resume 应被拒绝: %v", err)
	}
	if _, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback}); err != nil {
		t.Fatal(err)
	}
	if d := diffSnap(before, updSnap(t, w)); d != "" {
		t.Fatalf("recover 后应回到升级前:\n%s", d)
	}
}

// AC-J-2：篡改清单/包被拒（与安装同一条验证链）；失败后系统不变、sequence 不推进、staging 清理。
func TestUpdateRejectsTamperedManifestAndPackage(t *testing.T) {
	w := newUpdW(t)
	good := bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0"))
	names, bodies := bundleEntries(t, good)
	before := updSnap(t, w)
	check := func(name string, mut func(map[string][]byte)) error {
		bs := map[string][]byte{}
		for k, v := range bodies {
			bs[k] = append([]byte(nil), v...)
		}
		mut(bs)
		p := filepath.Join(t.TempDir(), name+".tar")
		writeBundleFrom(t, p, names, bs)
		o := doUpdate(t, w, "node", p, nil)
		if o.err == nil {
			t.Fatalf("%s：被篡改的 bundle 不得升级成功", name)
		}
		if d := diffSnap(before, updSnap(t, w)); d != "" {
			t.Fatalf("%s：被拒后系统必须不变:\n%s", name, d)
		}
		if ents, _ := os.ReadDir(w.Layout.StagingDir()); len(ents) != 0 {
			t.Errorf("%s：staging 应已清理: %v", name, ents)
		}
		return o.err
	}
	var me *manifest.Error
	err := check("manifest", func(b map[string][]byte) { b["manifest.json"][len(b["manifest.json"])/2] ^= 0x01 })
	if !errors.As(err, &me) {
		t.Errorf("篡改清单应为 manifest 校验错误: %v", err)
	}
	err = check("signature", func(b map[string][]byte) { s := b["manifest.json.sig"]; s[len(s)-8] ^= 0x01 })
	if !errors.As(err, &me) {
		t.Errorf("篡改签名应为 manifest 校验错误: %v", err)
	}
	var de *downloader.Error
	err = check("package", func(b map[string][]byte) {
		for n := range b {
			if strings.HasPrefix(n, "packages/lingedge-node-0.2.0") {
				b[n][len(b[n])/2] ^= 0x01
			}
		}
	})
	if !errors.As(err, &de) || de.Kind != downloader.KindSHA256 {
		t.Errorf("篡改包应为 sha256_mismatch: %v", err)
	}
	if _, ok, _ := (manifest.FileSequenceStore{Path: w.Layout.SequenceFile()}).Max("stable"); !ok {
		t.Fatal("安装时已记录 sequence")
	}
	if seq, _, _ := (manifest.FileSequenceStore{Path: w.Layout.SequenceFile()}).Max("stable"); seq != 10 {
		t.Errorf("被拒的升级不得推进 sequence（应仍为 10）: %d", seq)
	}
	// 回滚攻击：sequence 低于已见的清单被拒（旧清单重放）。
	mustUpd(t, doUpdate(t, w, "node", good, nil))
	replay := bundleFor(t, w, "replay", 9, nodeV(w, "0.3.0"))
	o := doUpdate(t, w, "node", replay, nil)
	if !errors.As(o.err, &me) {
		t.Fatalf("低 sequence 的清单应被拒绝（回滚攻击）: %v", o.err)
	}
}

// AC7：--offline 与在线走同一条验证链；在线升级经 HTTPS（release.base_url + ca_file）取清单与包，结果与离线一致。
func TestUpdateOnlineSameChainAsOffline(t *testing.T) {
	var routes map[string][]byte
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if b, ok := routes[r.URL.Path]; ok {
			_, _ = rw.Write(b)
			return
		}
		http.NotFound(rw, r)
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	base := "https://" + srv.Listener.Addr().String()
	w, err := toolsworld.NewWorldBase(t.TempDir(), base)
	if err != nil {
		t.Fatal(err)
	}
	must(t, doInstall(t, w, "node", nil))
	v2 := bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0"))
	names, bodies := bundleEntries(t, v2)
	routes = map[string][]byte{"/stable/manifest.json": bodies["manifest.json"], "/stable/manifest.json.sig": bodies["manifest.json.sig"]}
	for _, n := range names {
		if rest, ok := strings.CutPrefix(n, "packages/"); ok {
			routes["/dl/"+rest] = bodies[n]
		}
	}
	ca := filepath.Join(w.Root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	online := []func(*installer.Deps){func(d *installer.Deps) { d.Config.Release.BaseURL, d.Config.Release.CAFile = base, ca }}
	// 没有 ca_file：TLS 校验失败（无 --insecure），系统不变。
	before := updSnap(t, w)
	o := doUpdate(t, w, "node", "", []func(*installer.Deps){func(d *installer.Deps) { d.Config.Release.BaseURL = base }}, func(r *installer.UpdateRequest) { r.Offline = "" })
	var de *downloader.Error
	if !errors.As(o.err, &de) || de.Kind != downloader.KindTLS {
		t.Fatalf("应为 tls_untrusted: %v", o.err)
	}
	if d := diffSnap(before, updSnap(t, w)); d != "" {
		t.Fatalf("TLS 失败后系统应不变:\n%s", d)
	}
	res := mustUpd(t, doUpdate(t, w, "node", "", online, func(r *installer.UpdateRequest) { r.Offline = "" }))
	if res.To != "0.2.0" || curTarget(t, w, "node") != "releases/0.2.0" {
		t.Fatalf("在线升级: %+v", res)
	}
}

// update self（AC6）：同一条验证链取 tools 包 → 写 lingedge.new 并 --version 自检 → rename，保留 .prev；自检不符/包被篡改 → 目标不变。
func TestUpdateSelf(t *testing.T) {
	w := newW(t)
	selfDir := t.TempDir()
	target := filepath.Join(selfDir, "lingedge")
	script := func(ver string) string { return "#!/bin/sh\necho \"lingedge " + ver + " (commit abc, test)\"\n" }
	if err := os.WriteFile(target, []byte(script("0.1.0")), 0o755); err != nil {
		t.Fatal(err)
	}
	tools := func(ver, reported string) []toolsworld.BundleComp {
		return []toolsworld.BundleComp{{Name: "tools", Version: ver, Files: []toolsworld.PkgFile{{Name: "bin/lingedge", Body: script(reported), Exec: true}}}}
	}
	mk := func(seq int64, ver, reported string) string {
		return bundleFor(t, w, ver+reported, seq, tools(ver, reported))
	}
	self := func(b string, mut ...func(*installer.SelfRequest)) (*installer.SelfResult, error) {
		in := w.NewInstaller("run-self", func(d *installer.Deps) {
			d.Update = &installer.UpdateEnv{ToolsVersion: "0.1.0"}
			d.SelfPath = func() (string, error) { return target, nil }
		})
		req := installer.SelfRequest{Channel: "stable", Offline: b}
		for _, m := range mut {
			m(&req)
		}
		return in.UpdateSelf(t.Context(), req)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	// --dry-run：不动任何东西。
	r, err := self(mk(11, "0.2.0", "0.2.0"), func(r *installer.SelfRequest) { r.DryRun = true })
	if err != nil || r.Outcome != "planned" || r.To != "0.2.0" || read(target) != script("0.1.0") {
		t.Fatalf("dry-run: %+v %v", r, err)
	}
	// 自检失败：包里的二进制报告的版本不是清单声明的 0.3.0 → 拒绝，目标不变，没有 .new/.prev。
	if _, err := self(mk(12, "0.3.0", "0.9.9")); err == nil || !strings.Contains(err.Error(), "自检") {
		t.Fatalf("自检不符应被拒绝: %v", err)
	}
	if read(target) != script("0.1.0") || w.Has(strings.TrimPrefix(target, w.Root)) && false {
		t.Fatal("自检失败后目标必须不变")
	}
	for _, n := range []string{"lingedge.new", "lingedge.prev"} {
		if _, err := os.Stat(filepath.Join(selfDir, n)); err == nil {
			t.Errorf("%s 不应存在", n)
		}
	}
	if seq, _, _ := (manifest.FileSequenceStore{Path: w.Layout.SequenceFile()}).Max("stable"); seq != 0 {
		t.Errorf("失败的 self 升级不得推进 sequence: %d", seq)
	}
	// 成功。
	r, err = self(mk(13, "0.2.0", "0.2.0"))
	if err != nil || r.Outcome != "updated" || r.From != "0.1.0" || r.To != "0.2.0" || !strings.Contains(r.SelfCheck, "0.2.0") {
		t.Fatalf("self: %+v %v", r, err)
	}
	if read(target) != script("0.2.0") || read(target+".prev") != script("0.1.0") {
		t.Fatal("目标应是新版本，.prev 应是旧版本")
	}
	if _, err := os.Stat(target + ".new"); err == nil {
		t.Error(".new 应已被 rename")
	}
	if ents, _ := os.ReadDir(w.Layout.StagingDir()); len(ents) != 0 {
		t.Errorf("staging 应已清理: %v", ents)
	}
	// 已是最新（当前版本以 UpdateEnv.ToolsVersion 为准：这里仍声明 0.1.0，所以 0.2.0 是升级；声明为 0.2.0 后同版本为 no-op、更低版本被拒）。
	selfAt := func(cur, b string) (*installer.SelfResult, error) {
		in := w.NewInstaller("run-self2", func(d *installer.Deps) {
			d.Update = &installer.UpdateEnv{ToolsVersion: cur}
			d.SelfPath = func() (string, error) { return target, nil }
		})
		return in.UpdateSelf(t.Context(), installer.SelfRequest{Channel: "stable", Offline: b})
	}
	if r, err := selfAt("0.2.0", mk(14, "0.2.0", "0.2.0")); err != nil || r.Outcome != "already_up_to_date" {
		t.Fatalf("同版本应为 no-op: %+v %v", r, err)
	}
	if _, err := selfAt("0.3.0", mk(15, "0.2.0", "0.2.0")); err == nil || !strings.Contains(err.Error(), "降级") {
		t.Fatalf("低于当前版本应被拒绝（不降级）: %v", err)
	}
}

// 撤销动作谎报成功（没有真的切回 current）：回滚结果复核必须发现，按「回滚失败」处理（12），journal 保留。
func TestRollbackVerificationCatchesLyingUndo(t *testing.T) {
	w := newUpdW(t)
	b2 := bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0"))
	w.SetCtl("fail-health-node-0.2.0", true)
	o := doUpdate(t, w, "node", b2, []func(*installer.Deps){func(d *installer.Deps) {
		d.WrapUndoer = func(u installer.UndoApplier) installer.UndoApplier {
			return undoFn(func(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
				if a.Action == installer.ActUpdateSwitchCurrent {
					return service.UndoResult{Outcome: service.OutcomeDone}, nil // 谎报：什么都没做
				}
				return u.Apply(ctx, a)
			})
		}
	}})
	ie := instErr(t, o.err)
	if ie.Kind != installer.KindRollbackFailed || !strings.Contains(o.err.Error(), "复核") {
		t.Fatalf("复核应发现 current 没切回: %v", o.err)
	}
	if len(activeJournals(t, w)) != 1 {
		t.Fatal("复核失败时 journal 必须保留")
	}
}

// 在提交点（第 13 步）begin 之后、state.json 写入之前被杀：事务「已 begin 第 13 步但未提交」——recover 必须回滚（不能因为第 13 步已 begin 就当作已提交）；
// 在 state.json 写入之后被杀则只补写收尾（TestRecoverFinalizesCommittedUpdate）。
func TestRecoverStep13BegunButNotCommittedRollsBack(t *testing.T) {
	w := newUpdW(t)
	b2 := bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0"))
	before := updSnap(t, w)
	var j installer.Journal
	done := make(chan struct{})
	go func() {
		defer close(done)
		in := w.NewInstaller("run-crash", recDeps(w), wrapJournal(func(x installer.Journal) installer.Journal { j = x; return x }),
			func(d *installer.Deps) {
				d.Hook = func(p string) {
					if p == "begun:13" {
						j.Close() // 等价 kill -9：不写任何收尾记录，flock 释放
						runtime.Goexit()
					}
				}
			})
		task, err := in.Deps().Reg.Begin(t.Context(), registry.TaskRequest{Components: []string{"node"}})
		if err != nil {
			return
		}
		defer task.Release()
		_, _ = in.Update(t.Context(), task, installer.UpdateRequest{Component: "node", Channel: "stable", Offline: b2, NonInteractive: true})
	}()
	<-done
	if len(activeJournals(t, w)) != 1 {
		t.Fatalf("崩溃后应留有 1 个未完成 journal: %v", activeJournals(t, w))
	}
	if compState(t, w, "node").Version != "0.1.0" {
		t.Fatal("state.json 尚未写入，版本应仍是 0.1.0")
	}
	in := w.NewInstaller("run-rec", recDeps(w))
	r, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback})
	if err != nil || len(r.Transactions) != 1 || r.Transactions[0].Action != "rolled_back" {
		t.Fatalf("第 13 步已 begin 但未提交：recover 必须回滚: %+v %v", r, err)
	}
	if d := diffSnap(before, updSnap(t, w)); d != "" {
		t.Fatalf("回滚后应回到升级前:\n%s", d)
	}
}
