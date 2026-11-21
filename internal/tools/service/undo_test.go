package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/registry"
)

func TestUndoUserOnlyWhenCreated(t *testing.T) {
	e := newFakeEnv(t)
	e.set(t, "users/lingedge-node", "998\n")
	e.set(t, "users/lingedge", "431\n")
	u := &Undoer{Prov: e.provisioner(t, 0)}
	ctx := context.Background()

	// 事务前就存在 → Created=false → 不动。
	res, err := u.Apply(ctx, registry.UndoAction{Action: ActUserdel, Created: false, Extra: map[string]string{"user": "lingedge"}})
	if err != nil || res.Outcome != OutcomeNoop || !e.has("users/lingedge") {
		t.Fatalf("%+v %v", res, err)
	}
	if len(e.calls(t)) != 0 {
		t.Error("noop 不应执行任何进程")
	}
	// 本事务创建 → userdel（参数数组，且先复核是系统用户）。
	res, err = u.Apply(ctx, registry.UndoAction{Action: ActUserdel, Created: true, Extra: map[string]string{"user": "lingedge-node"}})
	if err != nil || res.Outcome != OutcomeDone || e.has("users/lingedge-node") {
		t.Fatalf("%+v %v", res, err)
	}
	got := e.calls(t)
	if len(got) != 2 || got[0] != "id [-u] [lingedge-node]" || got[1] != "userdel [lingedge-node]" {
		t.Errorf("argv：%q", got)
	}
	// 幂等：已不存在。
	if res, err = u.Apply(ctx, registry.UndoAction{Action: ActUserdel, Created: true, Extra: map[string]string{"user": "lingedge-node"}}); err != nil || res.Outcome != OutcomeDone {
		t.Errorf("已不存在应幂等成功：%+v %v", res, err)
	}
}

func TestUndoUserGuards(t *testing.T) {
	e := newFakeEnv(t)
	u := &Undoer{Prov: e.provisioner(t, 0)}
	ctx := context.Background()
	for _, name := range []string{"root", "daemon", "lingedge; id", "", "../x"} {
		if _, err := u.Apply(ctx, registry.UndoAction{Action: ActUserdel, Created: true, Extra: map[string]string{"user": name}}); KindOf(err) != KindUndo {
			t.Errorf("用户名 %q 应被拒绝：%v", name, err)
		}
	}
	e.set(t, "users/lingedge", "1500\n") // 之后变成了普通用户：拒绝删除
	if _, err := u.Apply(ctx, registry.UndoAction{Action: ActUserdel, Created: true, Extra: map[string]string{"user": "lingedge"}}); KindOf(err) != KindUndo || !e.has("users/lingedge") {
		t.Errorf("普通用户范围不得 userdel：%v", err)
	}
	e.set(t, "users/lingedge-dns", "0\n")
	if _, err := u.Apply(ctx, registry.UndoAction{Action: ActUserdel, Created: true, Extra: map[string]string{"user": "lingedge-dns"}}); KindOf(err) != KindUndo {
		t.Errorf("uid 0 不得 userdel：%v", err)
	}
	e.set(t, "users/lingedge-web", "400\n")
	nonroot := &Undoer{Prov: e.provisioner(t, 1000)}
	if _, err := nonroot.Apply(ctx, registry.UndoAction{Action: ActUserdel, Created: true, Extra: map[string]string{"user": "lingedge-web"}}); KindOf(err) != KindPermission {
		t.Errorf("非 root：%v", err)
	}
	if _, err := (&Undoer{}).Apply(ctx, registry.UndoAction{Action: ActUserdel, Created: true, Extra: map[string]string{"user": "lingedge"}}); KindOf(err) != KindUndo {
		t.Errorf("缺 Provisioner：%v", err)
	}
}

func TestUndoDirsAndFiles(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	u := &Undoer{Root: root}
	mk := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	empty := mk("var/lib/lingedge-node/cache")
	full := mk("var/lib/lingedge-node/identity")
	if err := os.WriteFile(filepath.Join(full, "key.pem"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := u.Apply(ctx, registry.UndoAction{Action: ActRemoveDirIfEmpty, Path: "/var/lib/lingedge-node/cache", Created: true}); err != nil || res.Outcome != OutcomeDone {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(empty); err == nil {
		t.Error("空目录应被删除")
	}
	if res, err := u.Apply(ctx, registry.UndoAction{Action: ActRemoveDirIfEmpty, Path: "/var/lib/lingedge-node/identity", Created: true}); err != nil || res.Outcome != OutcomeSkipped {
		t.Fatalf("非空目录应保留：%+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(full, "key.pem")); err != nil {
		t.Error("撤销不得删除用户数据")
	}
	if res, _ := u.Apply(ctx, registry.UndoAction{Action: ActRemoveDirIfEmpty, Path: "/var/lib/lingedge-node/gone", Created: true}); res.Outcome != OutcomeAbsent {
		t.Errorf("不存在：%+v", res)
	}
	if res, _ := u.Apply(ctx, registry.UndoAction{Action: ActRemoveDirIfEmpty, Path: "/var/lib/lingedge-node/identity", Created: false}); res.Outcome != OutcomeNoop {
		t.Errorf("Created=false：%+v", res)
	}

	// 文件：哈希一致才删。
	f := filepath.Join(root, "etc/lingedge/server")
	mk("etc/lingedge/server")
	p := filepath.Join(f, "server.yaml")
	if err := os.WriteFile(p, []byte("a: 1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	act := registry.UndoAction{Action: ActRemovePath, Path: "/etc/lingedge/server/server.yaml", Created: true, SHA256: sha256Hex([]byte("a: 1\n"))}
	if err := os.WriteFile(p, []byte("a: 2 # 用户改过\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if res, err := u.Apply(ctx, act); err != nil || res.Outcome != OutcomeSkipped {
		t.Fatalf("被改过的文件不得删除：%+v %v", res, err)
	}
	if err := os.WriteFile(p, []byte("a: 1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if res, err := u.Apply(ctx, act); err != nil || res.Outcome != OutcomeDone {
		t.Fatalf("%+v %v", res, err)
	}
	if res, _ := u.Apply(ctx, act); res.Outcome != OutcomeAbsent {
		t.Errorf("幂等：%+v", res)
	}
	// 符号链接不跟随、不删除目标。
	victim := filepath.Join(root, "victim")
	if err := os.WriteFile(victim, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, p); err != nil {
		t.Fatal(err)
	}
	act.SHA256 = ""
	if res, _ := u.Apply(ctx, act); res.Outcome != OutcomeSkipped {
		t.Errorf("符号链接：%+v", res)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Error("不得删除链接目标")
	}
}

func TestUndoRefusesPathsOutsideManagedAreas(t *testing.T) {
	root := t.TempDir()
	u := &Undoer{Root: root}
	ctx := context.Background()
	for _, p := range []string{"/etc/passwd", "/var/lib/lingedge/tools/state.json", "/var/lib/lingedge/tools", "/var/lib/lingedge/tools/journal/x",
		"/etc/lingedge/../passwd", "relative/x", "/etc/lingedgeX/y", "/var/lib/lingedge-nodeX", "/", "", "/etc/systemd/system/sshd.service"} {
		for _, act := range []string{ActRemovePath, ActRemoveDirIfEmpty} {
			if _, err := u.Apply(ctx, registry.UndoAction{Action: act, Path: p, Created: true}); KindOf(err) != KindUndo {
				t.Errorf("%s %q 应被拒绝：%v", act, p, err)
			}
		}
	}
	for _, p := range []string{"/etc/systemd/system/sshd.service", "/etc/systemd/system/lingedge-node.service.new.new", "/etc/lingedge/x", "/etc/systemd/system/../x", "/etc/systemd/system/sub/lingedge-node.service"} {
		if _, err := u.Apply(ctx, registry.UndoAction{Action: ActUnlinkUnit, Path: p, Created: true}); KindOf(err) != KindUndo {
			t.Errorf("unlink_unit %q 应被拒绝：%v", p, err)
		}
	}
}

func TestUndoSystemctlGuards(t *testing.T) {
	e := newFakeEnv(t)
	u := &Undoer{Sys: e.systemctl(t)}
	ctx := context.Background()
	for _, p := range []string{"sshd.service", "lingedge-node.service --now", "", "--all"} {
		if _, err := u.Apply(ctx, registry.UndoAction{Action: ActSystemctlDisable, Path: p, Created: true}); KindOf(err) != KindUndo {
			t.Errorf("%q：%v", p, err)
		}
	}
	if _, err := (&Undoer{}).Apply(ctx, registry.UndoAction{Action: ActSystemctlStop, Path: "lingedge-node.service", Created: true}); KindOf(err) != KindUndo {
		t.Errorf("缺 Systemctl：%v", err)
	}
	if len(e.calls(t)) != 0 {
		t.Error("被拒绝的撤销不得执行进程")
	}
}

func TestUndoUnknownActionAndHandles(t *testing.T) {
	_, err := (&Undoer{}).Apply(context.Background(), registry.UndoAction{Action: "restore_symlink"})
	if !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("%v", err)
	}
	for _, a := range []string{ActUserdel, ActRemoveDirIfEmpty, ActRemovePath, ActUnlinkUnit, ActSystemctlDisable, ActSystemctlStop} {
		if !Handles(a) {
			t.Errorf("Handles(%q)", a)
		}
	}
	if Handles("restore_symlink") || Handles("") {
		t.Error("不属于本包的动作不应被认领")
	}
}

// journal 往返：本包产出的 undo 能原样写入 registry journal 并在重放后读回（供 1.33 编排与 recover 使用）。
func TestUndoActionsRoundTripThroughJournal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tools")
	reg := registry.New(registry.Layout{Root: root}, registry.Options{})
	if err := reg.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	j, err := reg.CreateJournal(ctx, registry.NewTxID(time.Now()), []string{"node"})
	if err != nil {
		t.Fatal(err)
	}
	e := newFakeEnv(t)
	ur, err := e.provisioner(t, 0).EnsureUser(ctx, UserNode)
	if err != nil {
		t.Fatal(err)
	}
	acts := []registry.UndoAction{UserUndo(ur)}
	acts = append(acts, DirUndo([]DirResult{{Path: "/var/lib/lingedge-node", Created: true}})...)
	acts = append(acts, registry.UndoAction{Action: ActUnlinkUnit, Path: "/etc/systemd/system/lingedge-node.service", Created: true, SHA256: sha256Hex([]byte("x"))})
	if err := j.Begin(1, "service", registry.Undo{Actions: acts}); err != nil {
		t.Fatal(err)
	}
	if err := j.Done(1); err != nil {
		t.Fatal(err)
	}
	tx, err := reg.ReadTx(j.ID())
	if err != nil {
		t.Fatal(err)
	}
	var got []registry.UndoAction
	for _, s := range tx.UndoPlan() {
		if s.Undo != nil {
			got = append(got, s.Undo.Actions...)
		}
	}
	if len(got) != 3 || got[0].Action != ActUserdel || !got[0].Created || got[0].Extra["user"] != "lingedge-node" || got[1].Action != ActRemoveDirIfEmpty {
		t.Fatalf("journal 往返后的 undo：%+v", got)
	}
	for _, a := range got {
		if !Handles(a.Action) {
			t.Errorf("本包产出的动作 %q 自己不认识", a.Action)
		}
	}
}
