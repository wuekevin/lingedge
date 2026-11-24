package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// fakeOwn 是属主解析/chown 的测试替身：Resolve 缺省返回当前进程的 uid/gid（让真实文件的属主恰好「符合规格」），
// 需要制造属主偏差时用 resolve 覆盖；Chown 只记录（测试进程不是 root，不能真的 chown 给别人）。
type fakeOwn struct {
	uid, gid int
	resolve  map[string][2]int
	chowned  []string
}

func newFakeOwn() *fakeOwn {
	return &fakeOwn{uid: os.Getuid(), gid: os.Getgid(), resolve: map[string][2]int{}}
}

func (f *fakeOwn) Resolve(user, group string) (int, int, error) {
	if v, ok := f.resolve[user]; ok {
		return v[0], v[1], nil
	}
	return f.uid, f.gid, nil
}

func (f *fakeOwn) Chown(path string, uid, gid int) error {
	f.chowned = append(f.chowned, path)
	return nil
}

// newWorld 构造一棵真实的临时根目录与一个只含必要字段的 World；各用例按需覆盖/追加字段。
func newWorld(t *testing.T) *World {
	t.Helper()
	root := t.TempDir()
	w := &World{Root: root, Now: func() time.Time { return t0 }, Euid: os.Geteuid(), Own: newFakeOwn(), StateDir: filepath.Join(root, "var/lib/lingedge/tools")}
	return w
}

func put(t *testing.T, w *World, virtual string, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(w.Root, virtual)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func fakeProbe(sys resolver.System, socks ...probe.Socket) func(context.Context) (probe.Result, error) {
	return func(context.Context) (probe.Result, error) {
		return probe.Result{System: sys, Sockets: socks, ListenersKnown: true, ListenersSource: "ss"}, nil
	}
}

func goodSystem() resolver.System {
	return resolver.System{OS: "linux", Arch: "amd64", MemoryTotalMB: 8000, MemoryAvailableMB: 4000,
		Disks: []resolver.Disk{{Mount: "/", FreeMB: 100000}}, Deps: map[string]resolver.Dep{}}
}

func find(t *testing.T, rep Report, id string) Finding {
	t.Helper()
	for _, f := range rep.Findings {
		if f.ID == id {
			return f
		}
	}
	var ids []string
	for _, f := range rep.Findings {
		ids = append(ids, f.ID)
	}
	t.Fatalf("结果里没有 %s（有：%s）", id, strings.Join(ids, " "))
	return Finding{}
}

func runAll(t *testing.T, w *World, f Filter) Report {
	t.Helper()
	reg, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	return reg.Run(context.Background(), w, f)
}

func want(t *testing.T, f Finding, lv Level, substr string) {
	t.Helper()
	if f.Level != lv {
		t.Errorf("%s：级别 %s，期望 %s（%s）", f.ID, f.Level, lv, f.Message)
		return
	}
	if substr != "" && !strings.Contains(f.Message, substr) {
		t.Errorf("%s：消息 %q 不含 %q", f.ID, f.Message, substr)
	}
}
