package registry

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureFileIdempotentAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "server.yaml")
	data := []byte("listen: 127.0.0.1:8080\n")
	created, err := EnsureFile(p, data, 0o640)
	if err != nil || !created {
		t.Fatalf("首次应创建: %v %v", created, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Fatalf("权限 %v", fi.Mode().Perm())
	}
	// 重复执行：一致 → 已完成，created=false（所以不会被记成「本事务创建」而在撤销时误删）。
	if created, err := EnsureFile(p, data, 0o640); err != nil || created {
		t.Fatalf("重复执行应视为完成: %v %v", created, err)
	}
	// 内容不同 → 冲突，不覆盖。
	user := []byte("listen: 0.0.0.0:80\n# 用户手工修改\n")
	if err := os.WriteFile(p, user, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureFile(p, data, 0o640); kindOfErr(t, err) != KindConflict {
		t.Fatalf("内容不同应冲突: %v", err)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, user) {
		t.Fatal("冲突时用户文件被覆盖")
	}
	// 权限不同 → 冲突（不悄悄 chmod）。
	if err := os.WriteFile(p, data, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureFile(p, data, 0o640); kindOfErr(t, err) != KindConflict {
		t.Fatalf("权限不同应冲突: %v", err)
	}
	// 目标是符号链接 / 目录 → 冲突，且不跟随。
	victim := filepath.Join(dir, "victim")
	_ = os.WriteFile(victim, []byte("v"), 0o640)
	link := filepath.Join(dir, "link.yaml")
	_ = os.Symlink(victim, link)
	if _, err := EnsureFile(link, data, 0o640); kindOfErr(t, err) != KindConflict {
		t.Fatalf("符号链接应冲突: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "v" {
		t.Fatal("符号链接目标被改写")
	}
	if _, err := EnsureFile(dir, data, 0o640); kindOfErr(t, err) != KindConflict {
		t.Fatalf("目录应冲突: %v", err)
	}
	// 不遗留临时文件。
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if len(e.Name()) > 1 && e.Name()[0] == '.' {
			t.Errorf("遗留临时文件 %s", e.Name())
		}
	}
}

// 并发创建同一文件：恰好一个 created=true，其余视为完成；内容完整。
func TestEnsureFileConcurrentExactlyOneCreator(t *testing.T) {
	p := filepath.Join(t.TempDir(), "master.key.new")
	data := bytes.Repeat([]byte("k"), 4096)
	res := make(chan bool, 16)
	for i := 0; i < 16; i++ {
		go func() {
			c, err := EnsureFile(p, data, 0o600)
			if err != nil {
				t.Error(err)
			}
			res <- c
		}()
	}
	n := 0
	for i := 0; i < 16; i++ {
		if <-res {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("创建者个数 %d，期望恰好 1", n)
	}
	if b, _ := os.ReadFile(p); !bytes.Equal(b, data) {
		t.Fatal("内容不完整")
	}
}

func TestEnsureDirAllAndSymlink(t *testing.T) {
	base := t.TempDir()
	deep := filepath.Join(base, "a", "b", "c")
	created, err := EnsureDirAll(deep, 0o750)
	if err != nil || len(created) != 3 || created[0] != filepath.Join(base, "a") || created[2] != deep {
		t.Fatalf("created=%v err=%v（应自上而下列出本次新建的目录）", created, err)
	}
	if created, err := EnsureDirAll(deep, 0o750); err != nil || len(created) != 0 {
		t.Fatalf("重复执行不应再创建: %v %v", created, err)
	}
	f := filepath.Join(base, "file")
	_ = os.WriteFile(f, nil, 0o644)
	if _, err := EnsureDirAll(filepath.Join(f, "x"), 0o750); kindOfErr(t, err) != KindConflict {
		t.Fatalf("路径上有文件应冲突: %v", err)
	}
	ln := filepath.Join(base, "ln")
	_ = os.Symlink(deep, ln)
	if _, err := EnsureDirAll(ln, 0o750); kindOfErr(t, err) != KindConflict {
		t.Fatalf("符号链接不是真实目录: %v", err)
	}

	cur := filepath.Join(base, "current")
	if c, err := EnsureSymlink(cur, "releases/1.0.0"); err != nil || !c {
		t.Fatalf("%v %v", c, err)
	}
	if c, err := EnsureSymlink(cur, "releases/1.0.0"); err != nil || c {
		t.Fatalf("同目标应视为完成: %v %v", c, err)
	}
	if _, err := EnsureSymlink(cur, "releases/2.0.0"); kindOfErr(t, err) != KindConflict {
		t.Fatalf("指向别处应冲突（不替换）: %v", err)
	}
	if got, _ := os.Readlink(cur); got != "releases/1.0.0" {
		t.Fatalf("链接被改动: %s", got)
	}
	if _, err := EnsureSymlink(f, "x"); kindOfErr(t, err) != KindConflict {
		t.Fatalf("已有普通文件应冲突: %v", err)
	}
}
