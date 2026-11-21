package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func mk(t *testing.T, root, rel, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func ln(t *testing.T, root, target, rel string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// OpenResty 风格的真实目录树：相对/绝对文件链接、库版本链、目录链接、链中链。
func openrestyLikeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk(t, root, "nginx/sbin/nginx", "NGINX-BINARY", 0o755)
	mk(t, root, "lualib/resty/core.lua", "return {}", 0o644)
	mk(t, root, "luajit/lib/libluajit-5.1.so.2.1.0", "LUAJIT", 0o644)
	ln(t, root, "../nginx/sbin/nginx", "bin/openresty")                                  // 相对文件链接
	ln(t, root, "libluajit-5.1.so.2.1.0", "luajit/lib/libluajit-5.1.so.2")               // 库版本链
	ln(t, root, "libluajit-5.1.so.2", "luajit/lib/libluajit-5.1.so")                     // 链中链
	ln(t, root, "../lualib", "site/lualib")                                              // 目录链接
	ln(t, filepath.Join(root), filepath.Join(root, "nginx", "sbin", "nginx"), "bin/ng2") // 绝对路径链接（目标在树内）
	return root
}

func TestCopyTreeEliminatesSymlinks(t *testing.T) {
	src := openrestyLikeTree(t)
	dst := filepath.Join(t.TempDir(), "payload")
	links, err := copyTree(src, dst, "")
	if err != nil {
		t.Fatal(err)
	}
	files, err := listPayload(dst) // 任何符号链接残留都会使它报错
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Rel)
	}
	for _, want := range []string{"bin/openresty", "bin/ng2", "luajit/lib/libluajit-5.1.so", "luajit/lib/libluajit-5.1.so.2", "site/lualib/resty/core.lua", "nginx/sbin/nginx"} {
		found := false
		for _, n := range names {
			found = found || n == want
		}
		if !found {
			t.Errorf("缺少 %s（链接未被物化为普通文件）: %v", want, names)
		}
	}
	if read(t, filepath.Join(dst, "bin/openresty")) != "NGINX-BINARY" || read(t, filepath.Join(dst, "luajit/lib/libluajit-5.1.so")) != "LUAJIT" {
		t.Error("链接物化后的内容必须等于目标内容")
	}
	// 权限位归一化：可执行目标 → 0755（经链接也一样），其余 0644。
	if fi, _ := os.Stat(filepath.Join(dst, "bin/openresty")); fi.Mode().Perm() != 0o755 {
		t.Errorf("经链接的可执行文件应为 0755: %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(dst, "site/lualib/resty/core.lua")); fi.Mode().Perm() != 0o644 {
		t.Errorf("普通文件应为 0644: %v", fi.Mode())
	}
	if len(links) != 5 {
		t.Errorf("应记录 5 条被消除的链接，得到 %d: %+v", len(links), links)
	}
	for _, l := range links {
		if l.Path == "" || l.Target == "" || (l.Kind != "file" && l.Kind != "dir") {
			t.Errorf("链接记录不完整: %+v", l)
		}
	}
}

func TestCopyTreeRefusesUnsafe(t *testing.T) {
	cases := map[string]func(root string){
		"逃逸到源树之外": func(root string) {
			outside := t.TempDir()
			mk(t, outside, "secret", "x", 0o644)
			ln(t, root, filepath.Join(outside, "secret"), "bin/evil")
		},
		"相对路径逃逸": func(root string) { ln(t, root, "../../../../etc/hosts", "bin/evil") },
		"悬空链接":   func(root string) { ln(t, root, "nowhere", "bin/dangling") },
		"链接环":    func(root string) { ln(t, root, "b", "a"); ln(t, root, "a", "b") },
		"目录环（指向祖先）": func(root string) {
			mk(t, root, "d/x", "1", 0o644)
			ln(t, root, "..", "d/up")
		},
		"目录环（指向根）": func(root string) { mk(t, root, "d/x", "1", 0o644); ln(t, root, ".", "d/root") },
		"FIFO": func(root string) {
			if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
				t.Skip("无法创建 FIFO")
			}
		},
		"仅大小写不同": func(root string) { mk(t, root, "Lua/a", "1", 0o644); mk(t, root, "lua/a", "2", 0o644) },
		"百分号编码名": func(root string) { mk(t, root, "a%2e%2e/x", "1", 0o644) },
		"过深": func(root string) {
			p := "a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r"
			mk(t, root, p, "1", 0o644)
		},
	}
	for name, setup := range cases {
		root := t.TempDir()
		setup(root)
		// 大小写不敏感文件系统（macOS 缺省）上无法创建仅大小写不同的两个目录：此时该用例本身不成立。
		if name == "仅大小写不同" {
			if _, err := os.Stat(filepath.Join(root, "lua", "a")); err == nil && read(t, filepath.Join(root, "Lua", "a")) == "2" {
				t.Log("文件系统大小写不敏感，用例不成立，跳过")
				continue
			}
		}
		if _, err := copyTree(root, filepath.Join(t.TempDir(), "payload"), ""); err == nil {
			t.Errorf("%s: 必须被拒绝", name)
		}
	}
}

func TestCopyTreeHardlinksAndSiblingDirLink(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "a/file", "SAME", 0o644)
	if err := os.Link(filepath.Join(root, "a/file"), filepath.Join(root, "a/hard")); err != nil {
		t.Fatal(err)
	}
	ln(t, root, "a", "b") // 指向兄弟目录（非祖先）合法：复制两份
	dst := filepath.Join(t.TempDir(), "p")
	if _, err := copyTree(root, dst, ""); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a/file", "a/hard", "b/file", "b/hard"} {
		if read(t, filepath.Join(dst, p)) != "SAME" {
			t.Errorf("%s 内容不符", p)
		}
	}
	files, err := listPayload(dst)
	if err != nil || len(files) != 4 {
		t.Fatalf("应有 4 个独立普通文件: %v %d", err, len(files))
	}
	// 目标目录里不会出现硬链接关系（独立 inode）。
	st1, _ := os.Stat(filepath.Join(dst, "a/file"))
	st2, _ := os.Stat(filepath.Join(dst, "a/hard"))
	if os.SameFile(st1, st2) {
		t.Error("硬链接必须被复制为独立文件")
	}
}

func TestListPayloadRejectsResidualSymlink(t *testing.T) {
	d := t.TempDir()
	mk(t, d, "f", "x", 0o644)
	ln(t, d, "f", "l")
	if _, err := listPayload(d); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("残留符号链接必须使 listPayload 失败: %v", err)
	}
}
