package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// 快照与活目录逐字节一致，且位于 <out>/work/frontend/<组件>（此后校验与打包只读它）。
func TestSnapshotFrontendCopiesExactly(t *testing.T) {
	live, out := t.TempDir(), t.TempDir()
	writeTree(t, live, map[string]string{"index.html": "<html></html>", "assets/app-1.js": "console.log(1)", "assets/app-1.css": "a{}"})
	snap, err := snapshotFrontend(out, "admin-web", live)
	if err != nil {
		t.Fatal(err)
	}
	if snap != filepath.Join(out, "work", "frontend", "admin-web") {
		t.Errorf("快照路径：%s", snap)
	}
	a, na, _ := treeDigest(live)
	b, nb, _ := treeDigest(snap)
	if a != b || na != 3 || nb != 3 {
		t.Errorf("快照与活目录应一致：%s/%d vs %s/%d", a, na, b, nb)
	}
	// 重复快照会先清掉上一次的残留（不混入旧文件）。
	writeTree(t, snap, map[string]string{"stale.txt": "x"})
	snap, err = snapshotFrontend(out, "admin-web", live)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(snap, "stale.txt")); err == nil {
		t.Error("上一次快照的残留文件不应保留")
	}
	// 快照之后改动活目录，不影响快照（打包用的是快照）。
	_ = os.Remove(filepath.Join(live, "index.html"))
	if _, err := os.Stat(filepath.Join(snap, "index.html")); err != nil {
		t.Error("快照不受活目录之后的改动影响")
	}
}

// 复制期间活目录被改动（并发的 vite build）：以明确错误失败，而不是采用状态不明的拷贝。
func TestSnapshotFrontendDetectsMutationDuringCopy(t *testing.T) {
	for name, mutate := range map[string]func(live string){
		"清空 index.html": func(live string) { _ = os.Remove(filepath.Join(live, "index.html")) },
		"改写资源内容": func(live string) {
			_ = os.WriteFile(filepath.Join(live, "assets", "app-1.js"), []byte("changed"), 0o644)
		},
		"新增文件":    func(live string) { _ = os.WriteFile(filepath.Join(live, "assets", "new.js"), []byte("n"), 0o644) },
		"整个目录被删除": func(live string) { _ = os.RemoveAll(live) },
	} {
		live, out := t.TempDir(), t.TempDir()
		writeTree(t, live, map[string]string{"index.html": "<html></html>", "assets/app-1.js": "console.log(1)"})
		snapshotHook = func() { mutate(live) }
		_, err := snapshotFrontend(out, "admin-web", live)
		snapshotHook = nil
		if err == nil {
			t.Errorf("%s：应检测到复制期间的改动", name)
			continue
		}
		if !strings.Contains(err.Error(), "admin-web") {
			t.Errorf("%s：错误应指出组件：%v", name, err)
		}
	}
}

func TestSnapshotFrontendRefusesSymlinksAndMissingDir(t *testing.T) {
	live, out := t.TempDir(), t.TempDir()
	writeTree(t, live, map[string]string{"index.html": "<html></html>", "real.js": "x"})
	if err := os.Symlink(filepath.Join(live, "real.js"), filepath.Join(live, "link.js")); err != nil {
		t.Skip(err)
	}
	if _, err := snapshotFrontend(out, "admin-web", live); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Errorf("含符号链接的构建输出应被拒绝：%v", err)
	}
	if _, err := snapshotFrontend(out, "admin-web", filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("目录不存在应失败")
	}
}

// 校验针对快照：缺 index.html 的报错文案仍引用用户给出的目录。
func TestCheckFrontendDistReportsOriginalDir(t *testing.T) {
	snap := t.TempDir()
	err := checkFrontendDist(t.TempDir(), "admin-web", "frontend/dist/admin", snap)
	if err == nil || !strings.Contains(err.Error(), "frontend/dist/admin") || !strings.Contains(err.Error(), "缺少 index.html") {
		t.Errorf("%v", err)
	}
}

// 纵深防御：被打包的字节里没有 index.html（前端目录在校验之后被改动）→ stagePackage 失败，而不是发出残缺的包。
// 正常路径上快照保证它不会发生，所以只有直接驱动 stagePackage 才能证明这道防线真的存在。
func TestStagePackageRefusesFrontendWithoutIndexHTML(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"没有 index.html": {"assets/app-1.js": "x"},
		"index.html 为空": {"index.html": "", "assets/app-1.js": "x"},
	} {
		dist := t.TempDir()
		writeTree(t, dist, files)
		o := &buildOpts{out: t.TempDir(), src: t.TempDir(), version: "0.1.0", frontendDirs: treeFlags{"admin-web": dist}}
		_, _, _, err := o.stagePackage(compDef{Name: "admin-web", Status: "available", Variant: "default"}, "any", "", time.Now(), &buildRecord{})
		if err == nil || !strings.Contains(err.Error(), "index.html") {
			t.Errorf("%s：应拒绝：%v", name, err)
		}
	}
}
