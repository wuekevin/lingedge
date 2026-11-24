package rollback

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/dbmigrate"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

// 决策表（tools-design §17.2）四类 × 组合：备份、确认、回滚路径、承诺。
func TestDecideTable(t *testing.T) {
	cases := []struct {
		name         string
		classes      []string
		server       bool
		path         string
		backupForced bool
		verified     bool
		confirm      bool
		promise      string
	}{
		{"无迁移 server", nil, true, registry.PathBinary, true, false, false, PromiseBinary},
		{"无迁移 非 server", nil, false, registry.PathBinary, false, false, false, PromiseBinary},
		{"全部 reversible", []string{"reversible", "reversible"}, true, registry.PathMigrateDown, true, false, false, PromiseReversible},
		{"含 expand", []string{"reversible", "expand"}, true, registry.PathBinaryExpand, true, false, false, PromiseExpand},
		{"含 irreversible", []string{"irreversible"}, true, registry.PathRestoreBackup, true, true, true, PromiseIrreversible},
		{"多类并存取最严格", []string{"expand", "reversible", "irreversible"}, true, registry.PathRestoreBackup, true, true, true, PromiseIrreversible},
		{"未知级别按不可逆处理（失败关闭）", []string{"quantum"}, true, registry.PathRestoreBackup, true, true, true, PromiseIrreversible},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(c.classes, c.server)
			if d.Path != c.path || d.BackupForced != c.backupForced || d.BackupVerified != c.verified || d.NeedConfirm != c.confirm || d.Promise != c.promise {
				t.Fatalf("%+v", d)
			}
		})
	}
	if got := Decide([]string{"irreversible", "reversible", "reversible"}, true).Classes; !reflect.DeepEqual(got, []string{"reversible", "irreversible"}) {
		t.Errorf("Classes 应去重并按固定顺序: %v", got)
	}
	if got := MergeClasses([]string{"reversible"}, []string{"irreversible", "reversible"}); !reflect.DeepEqual(got, []string{"irreversible", "reversible"}) {
		t.Errorf("MergeClasses 取并集（更严格者）: %v", got)
	}
	// 清单预告与 migrate plan 各有一类：并集必须同时包含二者（只取其一会漏掉更严格的那类）。
	if got := MergeClasses([]string{"irreversible"}, []string{"reversible"}); !reflect.DeepEqual(got, []string{"irreversible", "reversible"}) {
		t.Errorf("预告=irreversible、plan=reversible 的并集: %v", got)
	}
	if got := MergeClasses([]string{"reversible"}, []string{"irreversible"}); !reflect.DeepEqual(got, []string{"irreversible", "reversible"}) {
		t.Errorf("预告=reversible、plan=irreversible 的并集: %v", got)
	}
	if d := Decide(MergeClasses([]string{"reversible"}, []string{"irreversible"}), true); !d.NeedConfirm {
		t.Error("低估的清单预告 + 不可逆的 migrate plan 必须需要确认")
	}
}

func TestRefusesPlainRollback(t *testing.T) {
	if r, _ := RefusesPlainRollback(nil); r {
		t.Error("没有升级记录不拒绝")
	}
	if r, h := RefusesPlainRollback(&registry.UpgradeRecord{Irreversible: true, Path: registry.PathRestoreBackup}); !r || !h {
		t.Error("不可逆必须拒绝并提示备份")
	}
	if r, h := RefusesPlainRollback(&registry.UpgradeRecord{Path: registry.PathBinaryExpand}); r || !h {
		t.Error("expand 不拒绝但提示")
	}
	if r, h := RefusesPlainRollback(&registry.UpgradeRecord{Path: registry.PathMigrateDown}); r || h {
		t.Error("reversible 不拒绝")
	}
}

func mkRel(t *testing.T, root, comp string, versions ...string) {
	t.Helper()
	for _, v := range versions {
		if err := os.MkdirAll(filepath.Join(root, "opt/lingedge", comp, "releases", v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSwitchCurrentIsAtomicAndSafe(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Root: root}
	mkRel(t, root, "server", "1.0.0", "1.1.0")
	link := filepath.Join(root, "opt/lingedge/server/current")
	if err := os.Symlink("releases/1.0.0", link); err != nil {
		t.Fatal(err)
	}
	prev, err := e.SwitchCurrent("server", "1.1.0")
	if err != nil || prev != "releases/1.0.0" {
		t.Fatalf("%q %v", prev, err)
	}
	if tg, _ := os.Readlink(link); tg != "releases/1.1.0" {
		t.Fatal(tg)
	}
	if ents, _ := os.ReadDir(filepath.Dir(link)); len(ents) != 2 { // releases + current，没有残留的临时链接
		t.Fatalf("残留: %v", ents)
	}
	// 幂等；目标不存在 → 拒绝且 current 不变；版本号/组件名不合法 → 拒绝。
	if _, err := e.SwitchCurrent("server", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SwitchCurrent("server", "9.9.9"); err == nil {
		t.Fatal("不存在的 release 必须拒绝")
	}
	if tg, _ := os.Readlink(link); tg != "releases/1.1.0" {
		t.Fatal("拒绝后 current 不得改变")
	}
	for _, bad := range []string{"../etc", "1.0", "latest", ""} {
		if _, err := e.SwitchCurrent("server", bad); err == nil {
			t.Errorf("版本 %q 应被拒绝", bad)
		}
	}
	if _, err := e.SwitchCurrent("../x", "1.0.0"); err == nil {
		t.Error("组件名应被拒绝")
	}
	// current 不是符号链接：Tools 不替换它。
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SwitchCurrent("server", "1.0.0"); err == nil {
		t.Fatal("current 是目录时必须拒绝")
	}
}

func TestPruneReleases(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Root: root}
	mkRel(t, root, "docs", "0.1.0", "0.2.0", "0.3.0", "0.4.0", "0.5.0-rc.1", "junk")
	if err := os.WriteFile(filepath.Join(root, "opt/lingedge/docs/releases/0.9.0.partial"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/0.2.0", filepath.Join(root, "opt/lingedge/docs/current")); err != nil {
		t.Fatal(err)
	}
	rm, err := e.PruneReleases("docs", 1, "0.1.0") // keep<2 按 2；0.1.0 受保护；current(0.2.0) 受保护
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rm, []string{"0.3.0"}) {
		t.Fatalf("应只删 0.3.0（最新两个 0.5.0-rc.1/0.4.0 + current 0.2.0 + 受保护 0.1.0 保留）: %v", rm)
	}
	for _, keep := range []string{"0.1.0", "0.2.0", "0.4.0", "0.5.0-rc.1", "junk"} {
		if _, err := os.Stat(filepath.Join(root, "opt/lingedge/docs/releases", keep)); err != nil {
			t.Errorf("%s 不应被删除: %v", keep, err)
		}
	}
	if _, err := e.PruneReleases("nothing", 2); err != nil {
		t.Errorf("没有 releases 目录不是错误: %v", err)
	}
}

// 原子切换：并发读者在切换期间任何时刻都能读到 current（旧目标或新目标），绝不会读到「缺失」。
func TestSwitchCurrentNeverLeavesCurrentMissing(t *testing.T) {
	root := t.TempDir()
	e := &Executor{Root: root}
	mkRel(t, root, "server", "1.0.0", "1.1.0")
	link := filepath.Join(root, "opt/lingedge/server/current")
	if err := os.Symlink("releases/1.0.0", link); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var missing atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				// 以「路径始终能解析到一个 release 目录」为准（Readlink 在 macOS 上会在 rename 的瞬间对已被替换的旧 vnode 返回 EINVAL，那是平台行为，不是缺失）。
				if _, err := os.Stat(link); errors.Is(err, fs.ErrNotExist) { // 缺失（ENOENT）；macOS 在 rename 瞬间的 EINVAL 是平台行为，不算缺失
					missing.Add(1)
				}
			}
		}()
	}
	for i := 0; i < 300; i++ {
		v := "1.1.0"
		if i%2 == 1 {
			v = "1.0.0"
		}
		if _, err := e.SwitchCurrent("server", v); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	if n := missing.Load(); n != 0 {
		t.Fatalf("切换期间有 %d 次读到 current 缺失（必须原子替换）", n)
	}
}

type failRunner struct{ calls []string }

func (r *failRunner) Run(_ context.Context, c safeexec.Cmd) (safeexec.Result, error) {
	r.calls = append(r.calls, strings.Join(c.Args[:2], " "))
	if len(c.Args) > 1 && c.Args[1] == "status" {
		return safeexec.Result{Stdout: []byte(`{"db_version":19,"latest_version":19,"pending":0}`)}, nil
	}
	return safeexec.Result{ExitCode: 4}, &safeexec.ExitError{Exe: "lingedge-server", Code: 4, Stderr: "dirty"}
}

// 手动回滚（AllowRestore=false）里数据库回退失败时，绝不静默改走会丢数据的「恢复备份」；升级失败的自动回滚（AllowRestore=true）才允许。
func TestRevertDBNeverSilentlyEscalatesToRestoreUnlessAllowed(t *testing.T) {
	root := t.TempDir()
	mkRel(t, root, "server", "1.1.0")
	fr := &failRunner{}
	e := &Executor{Root: root, Mig: &dbmigrate.Migrator{Run: fr},
		DB: func(context.Context) (*backup.DBConn, error) {
			return &backup.DBConn{DBTarget: backup.DBTarget{Host: "127.0.0.1", Port: 3306, Name: "d", User: "u"}, Password: []byte("x")}, nil
		}}
	p := RevertParams{Component: "server", Path: registry.PathMigrateDown, NewBin: "/opt/lingedge/server/releases/1.1.0/bin/lingedge-server", PreSchema: 18, TargetSchema: 19,
		Backup: "/b/lingedge-backup-h-server-20261124T000000Z.tar.gz"}
	_, err := e.RevertDB(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "不允许自动改走") {
		t.Fatalf("down 失败且不允许恢复备份时必须明确失败: %v", err)
	}
	p.AllowRestore = true
	if _, err := e.RevertDB(context.Background(), p); err == nil || !strings.Contains(err.Error(), "未装配备份服务") {
		t.Fatalf("允许时应走到恢复备份（此处未装配 → 明确失败）: %v", err)
	}
	// 非 server / binary 路径：什么都不做。
	if out, err := e.RevertDB(context.Background(), RevertParams{Component: "node", Path: registry.PathMigrateDown}); err != nil || out.Action != "none" {
		t.Fatalf("%+v %v", out, err)
	}
	if out, err := e.RevertDB(context.Background(), RevertParams{Component: "server", Path: registry.PathBinary}); err != nil || out.Action != "none" {
		t.Fatalf("%+v %v", out, err)
	}
}
