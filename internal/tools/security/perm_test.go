package security

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestCreateFileFinalPermRegardlessOfUmask(t *testing.T) {
	for _, mask := range []int{0o000, 0o022, 0o077, 0o277} {
		t.Run(os.FileMode(mask).String(), func(t *testing.T) {
			dir := t.TempDir()
			for _, perm := range []fs.FileMode{0o600, 0o644, 0o640, 0o755, 0o400} {
				p := filepath.Join(dir, perm.String())
				err := withUmask(mask, func() error {
					f, err := CreateFile(p, perm)
					if err != nil {
						return err
					}
					return f.Close()
				})
				if err != nil {
					t.Fatal(err)
				}
				st, _ := os.Stat(p)
				if st.Mode().Perm() != perm {
					t.Fatalf("umask %04o 下 %v 创建后为 %v", mask, perm, st.Mode().Perm())
				}
			}
		})
	}
}

func TestCreateFileRefusals(t *testing.T) {
	dir := t.TempDir()
	if _, err := CreateFile(filepath.Join(dir, "x"), fs.ModeSetuid|0o755); err == nil {
		t.Error("setuid 应被拒绝")
	}
	if _, err := CreateFile(filepath.Join(dir, "y"), fs.ModeSticky|0o755); err == nil {
		t.Error("sticky 应被拒绝")
	}
	p := filepath.Join(dir, "exists")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateFile(p, 0o600); !errors.Is(err, fs.ErrExist) {
		t.Errorf("已存在应 ErrExist: %v", err)
	}
	// 不跟随符号链接（O_EXCL 同样拒绝悬空链接）。
	target := filepath.Join(dir, "victim")
	ln := filepath.Join(dir, "ln")
	if err := os.Symlink(target, ln); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateFile(ln, 0o600); err == nil {
		t.Error("符号链接应被拒绝")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Error("符号链接目标不应被创建")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret")
	if err := WriteFileAtomic(p, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("v2"), 0o640); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o640 || mustRead(t, p) != "v2" {
		t.Fatalf("%v %q", st.Mode(), mustRead(t, p))
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("不应留下临时文件: %v", ents)
	}
	// 目标目录不存在 → 失败且无残留。
	if err := WriteFileAtomic(filepath.Join(dir, "no", "x"), []byte("x"), 0o600); err == nil {
		t.Fatal("目录不存在应失败")
	}
	if err := WriteFileAtomic(p, nil, fs.ModeSetgid|0o644); err == nil {
		t.Fatal("setgid 应被拒绝")
	}
}

func TestMkdirAllPerm(t *testing.T) {
	dir := t.TempDir()
	for _, mask := range []int{0o000, 0o077} {
		p := filepath.Join(dir, os.FileMode(mask).String(), "a", "b", "c")
		if err := withUmask(mask, func() error { return MkdirAllPerm(p, 0o750) }); err != nil {
			t.Fatal(err)
		}
		for q := p; q != filepath.Join(dir); q = filepath.Dir(q) {
			if q == filepath.Join(dir, os.FileMode(mask).String()) {
				break
			}
			if st, _ := os.Stat(q); st.Mode().Perm() != 0o750 {
				t.Fatalf("%s 为 %v", q, st.Mode().Perm())
			}
		}
	}
	// 已存在的目录不改权限；路径上是文件则失败。
	ex := filepath.Join(dir, "exist")
	if err := os.Mkdir(ex, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ex, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAllPerm(ex, 0o700); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(ex); st.Mode().Perm() != 0o711 {
		t.Fatalf("不应改已有目录权限: %v", st.Mode().Perm())
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAllPerm(filepath.Join(file, "sub"), 0o700); err == nil {
		t.Fatal("文件下建目录应失败")
	}
}

func TestWithUmask077Restores(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	var inside int
	if err := WithUmask077(func() error {
		inside = syscall.Umask(0o077)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if inside != 0o077 {
		t.Fatalf("fn 内 umask=%04o", inside)
	}
	if now := syscall.Umask(0o022); now != 0o022 {
		t.Fatalf("结束后应恢复为 022，实际 %04o", now)
	}
}
