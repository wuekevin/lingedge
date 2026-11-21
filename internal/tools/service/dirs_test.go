package service

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

type wantDir struct {
	mode         fs.FileMode
	owner, group string
}

// 期望表手写自 tools-design §8 与 ADR-020 §2 权威总表（不引用 NodeDirs 的数据，避免自证）。
func nodeWant() map[string]wantDir {
	return map[string]wantDir{
		"/etc/lingedge":                      {0o755, "root", "root"},
		"/etc/lingedge/node":                 {0o750, "root", "lingedge-node"},
		"/var/lib/lingedge-node":             {0o750, "lingedge-node", "lingedge-node"},
		"/var/lib/lingedge-node/identity":    {0o700, "lingedge-node", "lingedge-node"},
		"/var/lib/lingedge-node/generations": {0o750, "lingedge-node", "lingedge-node"},
		"/var/lib/lingedge-node/certs":       {0o700, "lingedge-node", "lingedge-node"},
		"/var/lib/lingedge-node/cache":       {0o750, "lingedge-node", "lingedge-node"},
		"/var/lib/lingedge-node/runtime":     {0o750, "lingedge-node", "lingedge-node"},
		"/var/lib/lingedge-node/staging":     {0o750, "lingedge-node", "lingedge-node"},
		"/var/lib/lingedge-node/logs-spool":  {0o750, "lingedge-node", "lingedge-node"},
		"/var/log/lingedge-node":             {0o750, "lingedge-node", "lingedge-node"},
		"/var/log/lingedge-node/access":      {0o750, "lingedge-node", "lingedge-node"},
	}
}

func serverWant() map[string]wantDir {
	return map[string]wantDir{
		"/etc/lingedge":            {0o755, "root", "root"},
		"/var/lib/lingedge":        {0o755, "root", "root"},
		"/etc/lingedge/server":     {0o750, "root", "lingedge"},
		"/var/lib/lingedge/server": {0o750, "lingedge", "lingedge"},
		"/var/log/lingedge":        {0o750, "lingedge", "lingedge"},
	}
}

func TestDirPlansMatchAuthoritativeTables(t *testing.T) {
	for name, c := range map[string]struct {
		specs []DirSpec
		want  map[string]wantDir
	}{
		"node":   {NodeDirs("lingedge-node", "lingedge-node"), nodeWant()},
		"server": {ServerDirs("lingedge", "lingedge"), serverWant()},
	} {
		got := map[string]DirSpec{}
		for _, sp := range c.specs {
			got[sp.Path] = sp
			if sp.Why == "" {
				t.Errorf("%s %s：缺少出处说明", name, sp.Path)
			}
		}
		if len(got) != len(c.want) || len(c.specs) != len(c.want) {
			t.Errorf("%s：目录数 %d（唯一 %d），期望 %d", name, len(c.specs), len(got), len(c.want))
		}
		for p, w := range c.want {
			sp, ok := got[p]
			if !ok {
				t.Errorf("%s：缺少 %s", name, p)
				continue
			}
			if sp.Mode != w.mode || sp.Owner != w.owner || sp.Group != w.group {
				t.Errorf("%s：%s = %04o %s:%s，期望 %04o %s:%s", name, p, sp.Mode, sp.Owner, sp.Group, w.mode, w.owner, w.group)
			}
		}
	}
	// 父目录必须排在子目录之前。
	seen := map[string]bool{}
	for _, sp := range NodeDirs("lingedge-node", "lingedge-node") {
		if parent := filepath.Dir(sp.Path); parent != "/" && nodeWant()[parent] != (wantDir{}) && !seen[parent] {
			t.Errorf("%s 排在其父目录 %s 之前", sp.Path, parent)
		}
		seen[sp.Path] = true
	}
}

func TestUpgradeResultDirIsRootOwnedAndNotAgentWritable(t *testing.T) {
	sp := UpgradeResultDir()
	if sp.Path != "/var/lib/lingedge-node-upgrade" || sp.Owner != "root" || sp.Group != "root" || sp.Mode != 0o755 {
		t.Fatalf("%+v", sp)
	}
	if sp.Mode&0o022 != 0 {
		t.Error("升级结果目录不得对 group/other 可写（BLK-07：Agent 无写权限）")
	}
	// 不得出现在 Node 的 Agent 属主目录计划里，也不得位于 /var/lib/lingedge-node 之下。
	for _, d := range NodeDirs("lingedge-node", "lingedge-node") {
		if d.Path == sp.Path {
			t.Error("结果目录不应在 Agent 目录计划中")
		}
	}
	if under(sp.Path, NodeDataDir) {
		t.Error("结果目录不得位于 Agent 属主的数据目录之下")
	}
}

func TestEnsureCreatesNodeTreeWithFinalPermsAndOwners(t *testing.T) {
	root := t.TempDir()
	own := &recOwn{}
	e := &Ensurer{Root: root, Own: own}
	specs := append(NodeDirs("lingedge-node", "lingedge-node"), UpgradeResultDir())
	res, err := e.Ensure(specs)
	if err != nil {
		t.Fatal(err)
	}
	want := nodeWant()
	want["/var/lib/lingedge-node-upgrade"] = wantDir{0o755, "root", "root"}
	created := map[string]bool{}
	for _, r := range res {
		created[r.Path] = true
		if !r.Created {
			t.Errorf("%s 应为新建", r.Path)
		}
	}
	for p, w := range want {
		st, err := os.Lstat(filepath.Join(root, p))
		if err != nil {
			t.Errorf("%s 未创建：%v", p, err)
			continue
		}
		if !st.IsDir() || st.Mode().Perm() != w.mode {
			t.Errorf("%s：模式 %v，期望 %04o", p, st.Mode(), w.mode)
		}
		c, ok := own.callFor(filepath.Join(root, p))
		if !ok {
			t.Errorf("%s 没有 chown 调用", p)
			continue
		}
		wu, wg, _ := own.Resolve(w.owner, w.group)
		if c.UID != wu || c.GID != wg {
			t.Errorf("%s chown 到 %d:%d，期望 %s:%s（%d:%d）", p, c.UID, c.GID, w.owner, w.group, wu, wg)
		}
		if !created[p] {
			t.Errorf("%s 未出现在结果中", p)
		}
	}
	// 不创建运行期产物：current/state.json/certs 子目录/generations/g<N>/ 与 /run/lingedge-node。
	for _, p := range []string{"/var/lib/lingedge-node/current", "/var/lib/lingedge-node/state.json", "/run/lingedge-node", "/var/lib/lingedge-node/generations/g1"} {
		if _, err := os.Lstat(filepath.Join(root, p)); err == nil {
			t.Errorf("%s 不应由 Tools 创建", p)
		}
	}
}

func TestEnsureIsIdempotentAndNeverTouchesExisting(t *testing.T) {
	root := t.TempDir()
	own := &recOwn{Real: true}
	e := &Ensurer{Root: root, Own: own}
	specs := ServerDirs("lingedge", "lingedge")
	if _, err := e.Ensure(specs); err != nil {
		t.Fatal(err)
	}
	// 第二次：属主按当前用户解析（让既有目录「属主正确」），不得再有任何创建或 chown。
	own2 := &recOwn{Same: true}
	e2 := &Ensurer{Root: root, Own: own2}
	// 先把权限校正到规格（第一次创建时 chown 只在 Real 下对自己生效，权限已是规格）。
	res, err := e2.Ensure(specs)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Created || len(r.Mismatch) != 0 {
			t.Errorf("既有目录不应被改动或报不符：%+v", r)
		}
	}
	if len(own2.Calls) != 0 {
		t.Errorf("既有且一致的目录不得 chown：%v", own2.Calls)
	}
	if len(DirUndo(res)) != 0 {
		t.Error("没有新建，undo 应为空")
	}
}

func TestEnsureReportsWideExistingPermsAndRepairs(t *testing.T) {
	root := t.TempDir()
	e := &Ensurer{Root: root, Own: &recOwn{Same: true}}
	specs := []DirSpec{ds("/var/lib/lingedge-node", 0o750, "lingedge-node", "lingedge-node", "x"), ds("/var/lib/lingedge-node/identity", 0o700, "lingedge-node", "lingedge-node", "x")}
	if _, err := e.Ensure(specs); err != nil {
		t.Fatal(err)
	}
	id := filepath.Join(root, "var/lib/lingedge-node/identity")
	if err := os.Chmod(id, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := e.Ensure(specs)
	var me *MismatchError
	if !errors.As(err, &me) || len(me.Items) != 1 {
		t.Fatalf("宽权限应以 MismatchError 报告而不是静默放行：%v", err)
	}
	if st, _ := os.Stat(id); st.Mode().Perm() != 0o777 {
		t.Error("未开 Repair 时不得改既有目录权限")
	}
	e.Repair = true
	res, err := e.Ensure(specs)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(id); st.Mode().Perm() != 0o700 {
		t.Errorf("Repair 后权限 %v", st.Mode().Perm())
	}
	if !res[1].Repaired || res[1].Created {
		t.Errorf("%+v", res[1])
	}
}

func TestEnsureOwnerMismatchDetected(t *testing.T) {
	root := t.TempDir()
	e := &Ensurer{Root: root, Own: &recOwn{Same: true}}
	sp := ds("/var/lib/lingedge-node", 0o750, "lingedge-node", "lingedge-node", "x")
	if _, err := e.Ensure([]DirSpec{sp}); err != nil {
		t.Fatal(err)
	}
	// 现在期望属主是 lingedge-node(902)，而目录属于当前用户：应报不符。
	e2 := &Ensurer{Root: root, Own: &recOwn{}}
	_, err := e2.Ensure([]DirSpec{sp})
	var me *MismatchError
	if os.Getuid() == 902 {
		t.Skip("当前用户恰为测试 uid")
	}
	if !errors.As(err, &me) {
		t.Fatalf("属主不符应报告：%v", err)
	}
}

func TestEnsureRefusesSymlinkAndFileTargets(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "var/lib/lingedge-node")); err != nil {
		t.Fatal(err)
	}
	e := &Ensurer{Root: root, Own: &recOwn{}}
	_, err := e.Ensure([]DirSpec{ds("/var/lib/lingedge-node", 0o750, "lingedge-node", "lingedge-node", "x")})
	if KindOf(err) != KindConflict {
		t.Fatalf("符号链接目标应冲突：%v", err)
	}
	if st, _ := os.Stat(outside); st.Mode().Perm() == 0o750 {
		t.Error("不得对符号链接指向的目录 chmod")
	}
	if err := os.WriteFile(filepath.Join(root, "var/lib/file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Ensure([]DirSpec{ds("/var/lib/file", 0o750, "root", "root", "x")}); KindOf(err) != KindConflict {
		t.Fatalf("文件目标应冲突：%v", err)
	}
}

func TestEnsureRejectsBadPaths(t *testing.T) {
	e := &Ensurer{Root: t.TempDir(), Own: &recOwn{}}
	for _, p := range []string{"relative", "/a/../b", "/a//b", "/a b", "/a/", ""} {
		if _, err := e.Ensure([]DirSpec{ds(p, 0o750, "root", "root", "x")}); KindOf(err) != KindInvalidParam {
			t.Errorf("路径 %q 应被拒绝：%v", p, err)
		}
	}
}

func TestLooseParentAcceptsNarrowerButNotWider(t *testing.T) {
	root := t.TempDir()
	e := &Ensurer{Root: root, Own: &recOwn{Same: true}}
	sp := etcRoot()
	if err := os.MkdirAll(filepath.Join(root, "etc/lingedge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "etc/lingedge"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Ensure([]DirSpec{sp}); err != nil {
		t.Errorf("共用父目录窄于规格应放行：%v", err)
	}
	if err := os.Chmod(filepath.Join(root, "etc/lingedge"), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Ensure([]DirSpec{sp}); err == nil {
		t.Error("共用父目录宽于规格必须报告")
	}
}

func TestDirUndoDeepestFirstOnlyCreated(t *testing.T) {
	res := []DirResult{{Path: "/etc/lingedge", Created: true}, {Path: "/etc/lingedge/node", Created: true}, {Path: "/var/lib/x", Created: false}, {Path: "/var/log/y", Created: true}}
	got := DirUndo(res)
	if len(got) != 3 || got[0].Path != "/var/log/y" || got[1].Path != "/etc/lingedge/node" || got[2].Path != "/etc/lingedge" {
		t.Fatalf("%+v", got)
	}
	for _, a := range got {
		if a.Action != ActRemoveDirIfEmpty || !a.Created {
			t.Errorf("%+v", a)
		}
	}
}

func TestEnsureChownFailureSurfaces(t *testing.T) {
	e := &Ensurer{Root: t.TempDir(), Own: &recOwn{Fail: os.ErrPermission}}
	_, err := e.Ensure([]DirSpec{ds("/var/lib/lingedge-node", 0o750, "lingedge-node", "lingedge-node", "x")})
	if KindOf(err) != KindPermission {
		t.Fatalf("%v", err)
	}
}

func TestVerifyDirsFindings(t *testing.T) {
	root := t.TempDir()
	e := &Ensurer{Root: root, Own: &recOwn{Same: true}}
	specs := []DirSpec{ds("/a-ok", 0o750, "x", "y", "t"), ds("/b-wide", 0o750, "x", "y", "t"), ds("/c-narrow", 0o750, "x", "y", "t"), ds("/d-missing", 0o750, "x", "y", "t")}
	for _, d := range []string{"a-ok", "b-wide", "c-narrow"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for d, m := range map[string]fs.FileMode{"a-ok": 0o750, "b-wide": 0o755, "c-narrow": 0o700} {
		if err := os.Chmod(filepath.Join(root, d), m); err != nil {
			t.Fatal(err)
		}
	}
	got := e.VerifyDirs(specs)
	levels := []string{LevelPass, LevelFail, LevelWarn, LevelFail}
	for i, f := range got {
		if f.Level != levels[i] || f.ID != "service.dir" || f.Message == "" {
			t.Errorf("%s：%+v，期望 %s", specs[i].Path, f, levels[i])
		}
		if f.Level == LevelFail && f.Hint == "" {
			t.Errorf("%s：fail 须带 hint", specs[i].Path)
		}
	}
}
