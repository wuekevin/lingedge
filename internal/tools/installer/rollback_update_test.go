package installer_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

func doRollback(t *testing.T, w *toolsworld.World, comp string, mutDeps []func(*installer.Deps), mut ...func(*installer.RollbackRequest)) (*installer.RollbackResult, error) {
	t.Helper()
	in := w.NewInstaller("run-rb", append([]func(*installer.Deps){recDeps(w)}, mutDeps...)...)
	task, err := in.Deps().Reg.Begin(t.Context(), registry.TaskRequest{Components: []string{comp}})
	if err != nil {
		return nil, err
	}
	defer task.Release()
	req := installer.RollbackRequest{Component: comp}
	for _, m := range mut {
		m(&req)
	}
	return in.Rollback(t.Context(), task, req)
}

// 手动回滚（无迁移路径）：回到 prev_version；state 的 prev/upgrade 清除；服务在旧版本上运行；旧 release 保留。
func TestManualRollbackBinaryPath(t *testing.T) {
	w := newUpdW(t)
	before := updSnap(t, w)
	mustUpd(t, doUpdate(t, w, "node", bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0")), nil))
	res, err := doRollback(t, w, "node", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "rolled_back" || res.Plan.To != "0.1.0" || res.Plan.Path != registry.PathBinary || res.Report.Version != "0.1.0" || res.Report.Health != "healthy" {
		t.Fatalf("结果: %+v %+v", res.Plan, res.Report)
	}
	c := compState(t, w, "node")
	if c.Version != "0.1.0" || c.PrevVersion != "" || c.Upgrade != nil {
		t.Fatalf("state: %+v", c)
	}
	if curTarget(t, w, "node") != "releases/0.1.0" || !w.FakeHas("active-lingedge-node.service") {
		t.Fatal("回滚后 current/服务不对")
	}
	// 回滚后「系统状态」与升级前一致，除了 releases/0.2.0（保留供取证/再次升级）与 state 的 LastTx。
	after := updSnap(t, w)
	for k := range after {
		if strings.HasPrefix(k, "opt/lingedge/node/releases/0.2.0") {
			delete(after, k)
		}
	}
	if d := diffSnap(before, after); d != "" {
		t.Fatalf("回滚后与升级前不一致:\n%s", d)
	}
	if len(activeJournals(t, w)) != 0 {
		t.Fatal("不应有活动 journal")
	}
	// 再回滚一次：没有上一版本。
	if _, err := doRollback(t, w, "node", nil); err == nil || !strings.Contains(err.Error(), "没有可回滚的上一版本") {
		t.Fatalf("再次回滚应被拒绝: %v", err)
	}
}

// 含不可逆迁移的升级之后：拒绝简单回退并提示 --from-backup（退出码 3 的来源）；--dry-run 同源。
func TestManualRollbackRefusesIrreversible(t *testing.T) {
	w := newUpdW(t)
	mustUpd(t, doUpdate(t, w, "node", bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0")), nil))
	// 把升级记录改成「含不可逆迁移」（真实的不可逆升级路径见 tests/integration/toolsinstall）。
	if err := w.Registry("x").UpdateState(t.Context(), func(s *registry.State) error {
		c := s.Components["node"]
		c.Upgrade.Irreversible, c.Upgrade.Path, c.Upgrade.Classes = true, registry.PathRestoreBackup, []string{"irreversible"}
		s.Components["node"] = c
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := updSnap(t, w)
	_, err := doRollback(t, w, "node", nil)
	ie := instErr(t, err)
	if ie.Kind != installer.KindPrecondition || ie.Details["code"] != "precondition.irreversible_rollback" || !strings.Contains(ie.Hint, "--from-backup") || ie.SystemState != "unchanged" {
		t.Fatalf("应拒绝简单回退: %+v", ie)
	}
	if d := diffSnap(before, updSnap(t, w)); d != "" {
		t.Fatalf("被拒绝的回滚不得改动任何东西:\n%s", d)
	}
	// --from-backup：用升级前备份恢复（node：恢复配置文件），再切回旧二进制。
	c := compState(t, w, "node")
	res, err := doRollback(t, w, "node", nil, func(r *installer.RollbackRequest) { r.FromBackup = c.Upgrade.Backup })
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan.Path != registry.PathRestoreBackup || res.Plan.To != "0.1.0" || curTarget(t, w, "node") != "releases/0.1.0" || compState(t, w, "node").Version != "0.1.0" {
		t.Fatalf("from-backup 回滚: %+v", res.Plan)
	}
}

// 回滚失败（新旧切换中途健康检查失败）：撤销回到回滚之前（仍是新版本），退出分类 rolled_back。
func TestManualRollbackFailureUndoesToBeforeRollback(t *testing.T) {
	w := newUpdW(t)
	mustUpd(t, doUpdate(t, w, "node", bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0")), nil))
	before := updSnap(t, w)
	w.SetCtl("fail-health-node-0.1.0", true) // 旧版本「不健康」：手动回滚的复核失败 → 撤销回到新版本
	_, err := doRollback(t, w, "node", nil)
	ie := instErr(t, err)
	if ie.Kind != installer.KindRolledBack {
		t.Fatalf("应为 rolled_back: %v", err)
	}
	w.SetCtl("fail-health-node-0.1.0", false)
	if d := diffSnap(before, updSnap(t, w)); d != "" {
		t.Fatalf("撤销后应回到回滚之前:\n%s", d)
	}
	if c := compState(t, w, "node"); c.Version != "0.2.0" || c.Upgrade == nil {
		t.Fatalf("state 应保持升级后: %+v", c)
	}
}

// 保留策略（AC7）：成功确认后才清理超出保留数的旧 release；current 与 prev 永远保留；失败的升级不清理；默认 2、不少于 2。
func TestReleaseRetentionOnlyAfterSuccess(t *testing.T) {
	w := newUpdW(t)
	rel := func(v string) bool { return w.Has("opt/lingedge/node/releases/" + v) }
	mustUpd(t, doUpdate(t, w, "node", bundleFor(t, w, "v2", 11, nodeV(w, "0.2.0")), nil))
	if !rel("0.1.0") || !rel("0.2.0") {
		t.Fatal("两个 release 都应保留")
	}
	// 失败的升级（0.3.0 健康失败）不清理任何旧 release，且新的 0.3.0 release 被撤销删除。
	w.SetCtl("fail-health-node-0.3.0", true)
	if o := doUpdate(t, w, "node", bundleFor(t, w, "v3", 12, nodeV(w, "0.3.0")), nil); o.err == nil {
		t.Fatal("应失败")
	}
	if !rel("0.1.0") || !rel("0.2.0") || rel("0.3.0") {
		t.Fatal("失败的升级不得清理旧 release，也不得残留新 release")
	}
	w.SetCtl("fail-health-node-0.3.0", false)
	res := mustUpd(t, doUpdate(t, w, "node", bundleFor(t, w, "v3b", 13, nodeV(w, "0.3.0")), nil))
	if len(res.Pruned) != 1 || res.Pruned[0] != "0.1.0" || rel("0.1.0") || !rel("0.2.0") || !rel("0.3.0") {
		t.Fatalf("保留 2 个（最新两个 + current/prev）：pruned=%v", res.Pruned)
	}
	// keep_releases=3 时不清理；配置小于 2 被 tools.yaml 校验拒绝。
	res = mustUpd(t, doUpdate(t, w, "node", bundleFor(t, w, "v4", 14, nodeV(w, "0.4.0")), []func(*installer.Deps){func(d *installer.Deps) { d.Config.Update.KeepReleases = 3 }}))
	if len(res.Pruned) != 0 || !rel("0.2.0") || !rel("0.3.0") || !rel("0.4.0") {
		t.Fatalf("keep=3: pruned=%v", res.Pruned)
	}
	if _, err := installer.ParseToolsConfig([]byte("update:\n  keep_releases: 1\n")); err == nil {
		t.Fatal("keep_releases<2 必须被拒绝")
	}
	if _, err := installer.ParseToolsConfig([]byte("update:\n  keep_releases: 5\n")); err != nil {
		t.Fatal(err)
	}
}

// 静态前端（1.40 留下）：升级与回滚复用原子切换 current；无数据库、无进程；页面内容随版本变化；回滚后恢复。
func TestStaticFrontendUpdateAndRollback(t *testing.T) {
	w, err := toolsworld.NewWorldWith(t.TempDir(), toolsworld.Options{Static: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetNativeNginx(); err != nil {
		t.Fatal(err)
	}
	must(t, doInstall(t, w, "docs", nil, func(r *installer.Request) {
		r.Settings = map[string]string{"host": "docs.example.com"}
		r.NginxLink = true
	}))
	page := func() string {
		b, _ := os.ReadFile(filepath.Join(w.Root, "opt/lingedge/docs/current/public/index.html"))
		return string(b)
	}
	if !strings.Contains(page(), "docs") {
		t.Fatalf("安装后页面: %q", page())
	}
	v2 := bundleFor(t, w, "docs2", 11, []toolsworld.BundleComp{{Name: "docs", Version: "0.2.0", Platform: "any", Files: toolsworld.StaticFiles("docs", "0.2.0")}})
	before := updSnap(t, w)
	res := mustUpd(t, doUpdate(t, w, "docs", v2, nil))
	if res.Path != registry.PathBinary || res.Backup != "" || !strings.Contains(page(), "0.2.0") || curTarget(t, w, "docs") != "releases/0.2.0" {
		t.Fatalf("静态前端升级: %+v %q", res, page())
	}
	if _, err := doRollback(t, w, "docs", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page(), "0.2.0") || curTarget(t, w, "docs") != "releases/0.1.0" || compState(t, w, "docs").Version != "0.1.0" {
		t.Fatalf("回滚后应是旧版本: %q", page())
	}
	_ = before
	// 失败的升级（落地前包布局不合格）：系统不变。
	bad := bundleFor(t, w, "docsbad", 12, []toolsworld.BundleComp{{Name: "docs", Version: "0.3.0", Platform: "any", Files: []toolsworld.PkgFile{{Name: "evil.sh", Body: "#!/bin/sh\n", Exec: true}}}})
	b4 := updSnap(t, w)
	if o := doUpdate(t, w, "docs", bad, nil); o.err == nil {
		t.Fatal("布局不合格的包应被拒绝")
	}
	if d := diffSnap(b4, updSnap(t, w)); d != "" {
		t.Fatalf("被拒绝的升级不得改动系统:\n%s", d)
	}
}
