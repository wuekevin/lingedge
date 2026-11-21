package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func unitRoot(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "etc/systemd/system")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, dir
}

func TestInstallFreshIdempotentAndNewOnConflict(t *testing.T) {
	root, dir := unitRoot(t)
	own := &recOwn{}
	in := &Installer{Root: root, Own: own}
	u := mustRender(t, UnitNode)

	r, err := in.Install(u)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "lingedge-node.service")
	if r.Status != StatusInstalled || r.Path != "/etc/systemd/system/lingedge-node.service" || r.Warning != "" {
		t.Fatalf("%+v", r)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 || readFile(t, p) != string(u.Content) {
		t.Errorf("新建单元应为 0644 且内容一致：%v", st.Mode())
	}
	if c, ok := own.callFor(p); !ok || c.UID != 0 || c.GID != 0 {
		t.Errorf("应 chown root:root：%+v", c)
	}
	if len(r.Undo) != 1 || r.Undo[0].Action != ActUnlinkUnit || !r.Undo[0].Created || r.Undo[0].SHA256 != sha256Hex(u.Content) {
		t.Errorf("undo：%+v", r.Undo)
	}

	// 内容一致 → 幂等完成，不产生 undo、不写 .new。
	r, err = in.Install(u)
	if err != nil || r.Status != StatusUnchanged || len(r.Undo) != 0 || r.Warning != "" {
		t.Fatalf("幂等：%+v %v", r, err)
	}
	if _, err := os.Stat(p + ".new"); err == nil {
		t.Error("内容一致不应写 .new")
	}

	// 管理员改过单元 → 不覆盖，写 .new 并警告。
	if err := os.WriteFile(p, []byte("[Unit]\nDescription=admin edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err = in.Install(u)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusNewWritten || r.NewPath != "/etc/systemd/system/lingedge-node.service.new" || !strings.Contains(r.Warning, "未覆盖") || !strings.Contains(r.Warning, ".new") {
		t.Fatalf("%+v", r)
	}
	if readFile(t, p) != "[Unit]\nDescription=admin edited\n" {
		t.Error("既有单元被覆盖（FR-6 禁止）")
	}
	if readFile(t, p+".new") != string(u.Content) {
		t.Error(".new 内容不符")
	}
	if st, _ := os.Stat(p + ".new"); st.Mode().Perm() != 0o644 {
		t.Errorf(".new 权限 %v", st.Mode().Perm())
	}
	if len(r.Undo) != 1 || r.Undo[0].Action != ActRemovePath || r.Undo[0].Path != r.NewPath || !r.Undo[0].Created {
		t.Errorf(".new 的 undo：%+v", r.Undo)
	}

	// 再装一次：.new 已是同内容 → 不再写、无 undo；单元仍不被覆盖。
	r, err = in.Install(u)
	if err != nil || r.Status != StatusNewWritten || len(r.Undo) != 0 {
		t.Fatalf("重复：%+v %v", r, err)
	}
	// 模板更新后 .new 被原子替换为新内容（.new 是 Tools 自己的暂存产物），但不算本事务新建。
	p2 := DefaultNodeParams()
	p2.AgentNOFILE = 4096
	u2, err := RenderNode(UnitNode, p2)
	if err != nil {
		t.Fatal(err)
	}
	r, err = in.Install(u2)
	if err != nil || len(r.Undo) != 0 || readFile(t, p+".new") != string(u2.Content) {
		t.Fatalf("替换 .new：%+v %v", r, err)
	}
	if readFile(t, p) != "[Unit]\nDescription=admin edited\n" {
		t.Error("单元仍不得被覆盖")
	}
	if fs := PendingNewUnits(root); len(fs) != 1 || fs[0].Level != LevelWarn || fs[0].ID != "service.unit_new_pending" || !strings.Contains(fs[0].Message, "lingedge-node.service.new") {
		t.Errorf("doctor 应告警待合并的 .new：%+v", fs)
	}
}

func TestInstallDoesNotFollowOrOverwriteSymlinkUnit(t *testing.T) {
	root, dir := unitRoot(t)
	// 典型：管理员 mask 了单元（链接到 /dev/null）。
	victim := filepath.Join(root, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "lingedge-node.service")); err != nil {
		t.Fatal(err)
	}
	in := &Installer{Root: root, Own: &recOwn{}}
	r, err := in.Install(mustRender(t, UnitNode))
	if err != nil || r.Status != StatusNewWritten {
		t.Fatalf("%+v %v", r, err)
	}
	if readFile(t, victim) != "keep" {
		t.Error("不得经符号链接写入目标")
	}
	if st, _ := os.Lstat(filepath.Join(dir, "lingedge-node.service")); st.Mode()&os.ModeSymlink == 0 {
		t.Error("符号链接被替换")
	}
}

func TestInstallErrors(t *testing.T) {
	in := &Installer{Root: t.TempDir(), Own: &recOwn{}} // 没有 etc/systemd/system
	if _, err := in.Install(mustRender(t, UnitNode)); KindOf(err) != KindIO {
		t.Errorf("单元目录不存在：%v", err)
	}
	root, _ := unitRoot(t)
	in = &Installer{Root: root, Own: &recOwn{}}
	for _, u := range []Unit{{Name: "evil.service", Content: []byte("x")}, {Name: UnitNode}, {Name: UnitNode, Content: make([]byte, maxUnitFile+1)}} {
		if _, err := in.Install(u); KindOf(err) != KindInvalidParam {
			t.Errorf("%q/%d 字节：%v", u.Name, len(u.Content), err)
		}
	}
	in = &Installer{Root: root, Own: &recOwn{Fail: os.ErrPermission}}
	if _, err := in.Install(mustRender(t, UnitNode)); KindOf(err) != KindPermission {
		t.Errorf("chown 失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/systemd/system/lingedge-node.service")); err == nil {
		t.Error("chown 失败不应留下单元文件")
	}
}

func TestInstallAllNodeUnitsThenUndo(t *testing.T) {
	root, dir := unitRoot(t)
	in := &Installer{Root: root, Own: &recOwn{}}
	us, err := NodeUnits(DefaultNodeParams())
	if err != nil {
		t.Fatal(err)
	}
	var undo []InstallResult
	for _, u := range us {
		r, err := in.Install(u)
		if err != nil {
			t.Fatal(err)
		}
		undo = append(undo, r)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 5 {
		t.Fatalf("应有 5 个单元文件，得到 %d", len(ents))
	}
	ud := &Undoer{Root: root}
	for i := len(undo) - 1; i >= 0; i-- {
		for _, a := range undo[i].Undo {
			if res, err := ud.Apply(t.Context(), a); err != nil || res.Outcome != OutcomeDone {
				t.Fatalf("%+v %+v %v", a, res, err)
			}
		}
	}
	if ents, _ = os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("撤销后不应残留单元：%v", ents)
	}
}
