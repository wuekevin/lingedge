package installer_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

func doUninstall(t testing.TB, w *toolsworld.World, comp string, purge, dry bool, mutDeps ...func(*installer.Deps)) (*installer.UninstallResult, error) {
	t.Helper()
	in := w.NewInstaller("run-un", mutDeps...)
	var task *registry.Task
	if !dry {
		var err error
		task, err = in.Deps().Reg.Begin(t.Context(), registry.TaskRequest{Components: []string{comp}})
		if err != nil {
			return nil, err
		}
		defer task.Release()
	}
	return in.Uninstall(t.Context(), task, installer.UninstallRequest{Component: comp, Purge: purge, DryRun: dry})
}

func stateHas(t testing.TB, w *toolsworld.World, comp string) bool {
	t.Helper()
	st, _, err := w.Registry("x").LoadState()
	if err != nil {
		t.Fatal(err)
	}
	_, ok := st.Components[comp]
	return ok
}

func put(t testing.TB, w *toolsworld.World, rel, content string) {
	t.Helper()
	p := filepath.Join(w.Root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedServerInstall(t testing.TB) *toolsworld.World {
	t.Helper()
	w := newW(t)
	must(t, doInstall(t, w, "server", nil))
	// 用户数据：配置、数据库之外的服务数据、两个备份（server 的与 node 的）。
	if !w.Has("etc/lingedge/server/master.key") {
		t.Fatal("前置：安装应已生成 master.key")
	}
	put(t, w, "var/lib/lingedge/server/secrets/runtime-credentials.json", "{}")
	put(t, w, "var/lib/lingedge/server/install.lock", "x")
	put(t, w, "var/lib/lingedge/backups/lingedge-backup-h-server-20260101T000000Z.tar.gz", "b1")
	put(t, w, "var/lib/lingedge/backups/lingedge-backup-h-server-20260102T000000Z.tar.gz.enc", "b2")
	put(t, w, "var/lib/lingedge/backups/lingedge-backup-h-node-20260101T000000Z.tar.gz", "b3")
	return w
}

// AC5：默认保留配置/数据/备份；只摘除运行时（单元、release、state）。
func TestUninstallKeepsDataByDefault(t *testing.T) {
	w := seedServerInstall(t)
	if !w.FakeHas("active-lingedge-server.service") || !w.Has("opt/lingedge/server/current") || !stateHas(t, w, "server") {
		t.Fatal("前置：server 应已安装并运行")
	}
	res, err := doUninstall(t, w, "server", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != installer.OutcomeUninstalled || res.Tx == "" {
		t.Fatalf("结果: %+v", res)
	}
	for _, gone := range []string{"etc/systemd/system/lingedge-server.service", "opt/lingedge/server"} {
		if w.Has(gone) {
			t.Errorf("%s 应已被删除", gone)
		}
	}
	for _, f := range []string{"active-lingedge-server.service", "enabled-lingedge-server.service"} {
		if w.FakeHas(f) {
			t.Errorf("单元应已停止并停用（%s 仍在）", f)
		}
	}
	if stateHas(t, w, "server") {
		t.Error("state.json 里应已移除 server")
	}
	for _, kept := range []string{"etc/lingedge/server/server.yaml", "etc/lingedge/server/master.key", "var/lib/lingedge/server/secrets/runtime-credentials.json", "var/lib/lingedge/server/install.lock",
		"var/lib/lingedge/backups/lingedge-backup-h-server-20260101T000000Z.tar.gz", "var/lib/lingedge/backups/lingedge-backup-h-node-20260101T000000Z.tar.gz"} {
		if !w.Has(kept) {
			t.Errorf("%s 默认必须保留", kept)
		}
	}
	if !w.FakeHas("users/lingedge") {
		t.Error("运行用户默认保留")
	}
	if un, _ := w.Registry("x").Unfinished(t.Context()); len(un) != 0 {
		t.Errorf("事务应已提交归档: %v", un)
	}
	// 计划与结果一致：保留项明确列出，数据库明确「不删除」。
	kinds := map[string]string{}
	for _, it := range res.Items {
		kinds[it.Kind+":"+it.Action] = it.Path
	}
	for _, k := range []string{"config:keep", "data:keep", "backup:keep", "database:keep", "user:keep", "release:remove", "state:remove"} {
		if _, ok := kinds[k]; !ok {
			t.Errorf("结果缺少 %s: %+v", k, res.Items)
		}
	}
	// 之后可以重新安装（卸载后状态一致）。
	must(t, doInstall(t, w, "server", nil, func(r *installer.Request) {}))
}

func TestUninstallPurgeRemovesOnlyThisComponentsData(t *testing.T) {
	w := seedServerInstall(t)
	put(t, w, "etc/lingedge/node/agent.yaml", "keep me")
	put(t, w, "var/lib/lingedge-node/identity/key", "keep me")
	if _, err := doUninstall(t, w, "server", true, false); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"etc/lingedge/server", "var/lib/lingedge/server", "var/lib/lingedge/backups/lingedge-backup-h-server-20260101T000000Z.tar.gz",
		"var/lib/lingedge/backups/lingedge-backup-h-server-20260102T000000Z.tar.gz.enc"} {
		if w.Has(gone) {
			t.Errorf("--purge 应删除 %s", gone)
		}
	}
	for _, kept := range []string{"etc/lingedge/node/agent.yaml", "var/lib/lingedge-node/identity/key", "var/lib/lingedge/backups/lingedge-backup-h-node-20260101T000000Z.tar.gz", "var/lib/lingedge/tools"} {
		if !w.Has(kept) {
			t.Errorf("--purge 不得动 %s", kept)
		}
	}
	if !w.FakeHas("users/lingedge") {
		t.Error("purge 也不删除运行用户")
	}
	if stateHas(t, w, "server") {
		t.Error("state 应已移除")
	}
}

func TestUninstallPurgeWarnsAboutMasterKey(t *testing.T) {
	w := seedServerInstall(t)
	res, err := doUninstall(t, w, "server", true, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, wn := range res.Warnings {
		found = found || (wn.Code == "uninstall.purge_master_key" && strings.Contains(wn.Message, "master.key"))
	}
	if !found {
		t.Errorf("purge 计划应醒目警告 master.key 将被删除: %+v", res.Warnings)
	}
}

func TestUninstallNotInstalledAndDryRun(t *testing.T) {
	w := newW(t)
	if _, err := doUninstall(t, w, "server", false, false); kindOf(t, err) != installer.KindNotInstalled {
		t.Fatalf("未安装应为 KindNotInstalled: %v", err)
	}
	if _, err := doUninstall(t, w, "../etc", false, true); err == nil {
		t.Fatal("非法组件名应被拒绝")
	}
	w = seedServerInstall(t)
	before := sysSnap(t, w)
	res, err := doUninstall(t, w, "server", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "planned" || !res.DryRun || len(res.Items) == 0 {
		t.Fatalf("dry-run 结果: %+v", res)
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" {
		t.Errorf("--dry-run 不得改动系统:\n%s", d)
	}
	if !stateHas(t, w, "server") || !w.FakeHas("active-lingedge-server.service") {
		t.Error("dry-run 后组件应原样")
	}
}

// 静态前端：没有数据目录；摘除 conf.d 软链并 reload；片段默认保留，purge 才删且只删本组件的两个文件（目录共用，绝不删）。
func TestUninstallStaticFrontend(t *testing.T) {
	w := staticWorld(t)
	if err := w.SetNativeNginx(); err != nil {
		t.Fatal(err)
	}
	must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com", "nginx-link", "yes"))...))
	must(t, doInstall(t, w, "console", nil, setReq(hostSet("console.example.com"))...))
	if _, err := os.Readlink(linkPath(w)); err != nil {
		t.Fatalf("前置：软链应已创建: %v", err)
	}
	callsBefore := len(w.NginxCalls())
	res, err := doUninstall(t, w, "admin-web", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(linkPath(w)); err == nil {
		t.Error("软链应已摘除")
	}
	if calls := w.NginxCalls(); len(calls) != callsBefore+2 || calls[len(calls)-2] != "-t" || calls[len(calls)-1] != "-s reload" {
		t.Errorf("摘除软链后应 nginx -t 再 reload: %v", calls)
	}
	if w.Has("opt/lingedge/admin-web") {
		t.Error("release 树应已删除")
	}
	for _, kept := range []string{"etc/lingedge/nginx/admin-web.conf", "etc/lingedge/nginx/admin-web-security-headers.conf", "etc/lingedge/nginx/console.conf", "opt/lingedge/console/current"} {
		if !w.Has(kept) {
			t.Errorf("%s 应保留", kept)
		}
	}
	if stateHas(t, w, "admin-web") || !stateHas(t, w, "console") {
		t.Error("只应移除 admin-web 的记录")
	}
	noData := false
	for _, it := range res.Items {
		noData = noData || (it.Kind == "data" && it.Action == "keep" && strings.Contains(it.Note, "没有数据目录"))
	}
	if !noData {
		t.Errorf("结果应说明静态前端没有数据目录: %+v", res.Items)
	}
	// purge console：只删它自己的两个片段文件，目录与 admin-web 的片段仍在。
	if _, err := doUninstall(t, w, "console", true, false); err != nil {
		t.Fatal(err)
	}
	if w.Has("etc/lingedge/nginx/console.conf") || w.Has("etc/lingedge/nginx/console-security-headers.conf") {
		t.Error("purge 应删除 console 的片段")
	}
	if !w.Has("etc/lingedge/nginx/admin-web.conf") {
		t.Error("purge 不得动其他前端的片段")
	}
}

func TestUninstallForeignNginxLinkIsLeftAlone(t *testing.T) {
	w := staticWorld(t)
	if err := w.SetNativeNginx(); err != nil {
		t.Fatal(err)
	}
	must(t, doInstall(t, w, "admin-web", nil, setReq(hostSet("admin.example.com"))...))
	theirs := "# 别人的文件\n"
	_ = os.WriteFile(linkPath(w), []byte(theirs), 0o644)
	res, err := doUninstall(t, w, "admin-web", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(linkPath(w)); string(b) != theirs {
		t.Fatal("不是 Tools 软链的同名文件不得被删除")
	}
	if len(w.NginxCalls()) != 0 {
		t.Errorf("没有摘除软链就不该 reload: %v", w.NginxCalls())
	}
	ok := false
	for _, wn := range res.Warnings {
		ok = ok || wn.Code == "uninstall.nginx_link_foreign"
	}
	if !ok {
		t.Errorf("应警告: %+v", res.Warnings)
	}
}

// 卸载中途失败：事务以 abort 结束并报告 partial；组件仍在 state.json 里，重跑即可完成（各步幂等）。
func TestUninstallFailureIsPartialAndRerunCompletes(t *testing.T) {
	w := seedServerInstall(t)
	failRelease := func(d *installer.Deps) {
		d.Hook = func(point string) {
			if point == "before:5" {
				w.SetCtl("fail-all-stop", false)
				// 让删除 release 失败：把 current 目录换成不可删的内容（父目录去写权限）。
				_ = os.Chmod(filepath.Join(w.Root, "opt/lingedge/server"), 0o500)
			}
		}
	}
	_, err := doUninstall(t, w, "server", false, false, failRelease)
	if err == nil {
		_ = os.Chmod(filepath.Join(w.Root, "opt/lingedge/server"), 0o755)
		t.Skip("当前用户可无视目录权限（如 root），跳过")
	}
	var ie *installer.Error
	if !errors.As(err, &ie) || ie.Kind != installer.KindStepFailed || ie.SystemState != "partial" || !strings.Contains(ie.Hint, "重新运行") {
		t.Fatalf("应报告 partial 与重跑指引: %+v", err)
	}
	if !stateHas(t, w, "server") {
		t.Fatal("失败时组件记录必须仍在 state.json（表示尚未完成）")
	}
	if un, _ := w.Registry("x").Unfinished(t.Context()); len(un) != 0 {
		t.Errorf("失败的卸载事务应以 abort 结束: %v", un)
	}
	_ = os.Chmod(filepath.Join(w.Root, "opt/lingedge/server"), 0o755)
	// 部分状态：单元已停并删除，release 仍在 → 重跑完成。
	if w.Has("etc/systemd/system/lingedge-server.service") {
		t.Error("失败发生在第 5 步，单元文件此前已删除")
	}
	if _, err := doUninstall(t, w, "server", false, false); err != nil {
		t.Fatalf("重跑应完成: %v", err)
	}
	if stateHas(t, w, "server") || w.Has("opt/lingedge/server") {
		t.Error("重跑后应彻底卸载")
	}
	if !w.Has("etc/lingedge/server/server.yaml") {
		t.Error("配置仍应保留")
	}
}

// 崩溃（journal 未结束）后 recover：卸载不可撤销，--rollback 只结束事务记录并说明；--resume 明确拒绝并给出指引。
func TestRecoverOfInterruptedUninstall(t *testing.T) {
	w := seedServerInstall(t)
	in := w.NewInstaller("run-crash")
	task, err := in.Deps().Reg.Begin(t.Context(), registry.TaskRequest{Components: []string{"server"}})
	if err != nil {
		t.Fatal(err)
	}
	j, err := task.OpenTx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Begin(installer.UStepRequest, "uninstall_request", registry.Undo{Actions: []registry.UndoAction{{Action: installer.ActUninstallRequest, Extra: map[string]string{"component": "server"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := j.Done(installer.UStepRequest); err != nil {
		t.Fatal(err)
	}
	if err := j.Begin(installer.UStepStop, "stop_disable_units", registry.Undo{}); err != nil {
		t.Fatal(err)
	}
	task.Release() // 等同进程死亡：journal 未 commit/abort

	// 新命令被 13 拦住（有未完成事务）。
	if _, err := w.Registry("y").Begin(t.Context(), registry.TaskRequest{Components: []string{"server"}}); err == nil {
		t.Fatal("存在未完成事务时应拒绝新的写命令")
	}
	in2 := w.NewInstaller("run-recover")
	if _, err := in2.Recover(context.Background(), installer.RecoverOptions{Mode: installer.RecoverResume}); err == nil || !strings.Contains(err.Error(), "uninstall") {
		t.Fatalf("--resume 应明确拒绝 uninstall 事务: %v", err)
	}
	res, err := in2.Recover(context.Background(), installer.RecoverOptions{Mode: installer.RecoverRollback})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Transactions) != 1 || !strings.Contains(res.Transactions[0].Message, "卸载不可撤销") {
		t.Fatalf("rollback 的说明应指出卸载不可撤销: %+v", res.Transactions)
	}
	if !stateHas(t, w, "server") {
		t.Error("recover 不得改动 state.json")
	}
	if _, err := doUninstall(t, w, "server", false, false); err != nil {
		t.Fatalf("recover 之后重跑 uninstall 应完成: %v", err)
	}
}
