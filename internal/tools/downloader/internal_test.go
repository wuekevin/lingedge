package downloader

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// 包内部的单元测试：覆盖只有通过内部状态才能到达的防护层（对应 1.28 报告里的「未检出变异」）。

func shaHex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func tarBytes(t *testing.T, files map[string][]byte, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range order {
		if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[n]))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(files[n])
	}
	_ = tw.Close()
	return buf.Bytes()
}

// copyPackage 的「复制出的字节数必须等于清单 size」层：bundle 文件在被索引之后被截短（读取期间文件变化），
// 复制得到的字节变少——这一层给出专门的「被截断」（KindSize）分类，而不是等到后面的 SHA256 比较才以 KindSHA256 失败。
func TestCopyPackageDetectsTruncationAfterIndexing(t *testing.T) {
	pkg := bytes.Repeat([]byte("p"), 4000)
	raw := tarBytes(t, map[string][]byte{"manifest.json": []byte("{}"), "manifest.json.sig": []byte("{}"), "packages/a.tar.gz": pkg},
		[]string{"manifest.json", "manifest.json.sig", "packages/a.tar.gz"})
	path := filepath.Join(t.TempDir(), "b.tar")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBundle(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	exp := Expected{Size: int64(len(pkg)), SHA256: shaHex(pkg)}
	// 对照：未被改动时成功。
	if err := b.copyPackage("a.tar.gz", filepath.Join(t.TempDir(), "ok"), exp); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, int64(len(raw))-1500); err != nil { // 被索引后文件被截短（同一个 inode，已打开的句柄可见）
		t.Fatal(err)
	}
	err = b.copyPackage("a.tar.gz", filepath.Join(t.TempDir(), "cut"), exp)
	if !IsKind(err, KindSize) {
		t.Fatalf("期望 KindSize（专门的截断层）：%v", err)
	}
}

// 续传缓存落名的「复制 + 重新计算」路径（跨文件系统 rename 失败时使用）：成功与哈希不符。
func TestCopyFinalize(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte("z"), 10_000)
	exp := Expected{Size: int64(len(body)), SHA256: shaHex(body)}
	part, out := filepath.Join(dir, "cache.part"), filepath.Join(dir, "final")
	if err := os.WriteFile(part, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFinalize(part, out, exp); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, body) {
		t.Fatal("内容不符")
	}
	if _, err := os.Stat(part); err == nil {
		t.Fatal("成功后应删除缓存 partial")
	}
	if _, err := os.Stat(out + ".part"); err == nil {
		t.Fatal("临时文件应已改名")
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限 = %v", fi.Mode().Perm())
	}
	// 重新计算发现与清单不符（缓存在校验之后、落名之前被改）：失败，两端都不留。
	bad := append([]byte(nil), body...)
	bad[5] ^= 1
	if err := os.WriteFile(part, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	out2 := filepath.Join(dir, "final2")
	if err := copyFinalize(part, out2, exp); !IsKind(err, KindSHA256) {
		t.Fatalf("期望 KindSHA256：%v", err)
	}
	for _, p := range []string{part, out2, out2 + ".part"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("失败后不应遗留 %s", p)
		}
	}
	// 长度不符。
	if err := os.WriteFile(part, body[:100], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFinalize(part, filepath.Join(dir, "final3"), exp); !IsKind(err, KindSHA256) {
		t.Fatalf("期望失败：%v", err)
	}
	// partial 是符号链接：不跟随。
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, body, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.part")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := copyFinalize(link, filepath.Join(dir, "final4"), exp); err == nil {
		t.Fatal("符号链接 partial 必须被拒绝")
	}
}

// flock：同一个 key 的第二个持有者得到 busy 而不是共享；释放后可再取得；符号链接的锁文件被拒绝。
func TestLockFileExclusive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k.lock")
	rel, busy, err := lockFile(p)
	if err != nil || busy {
		t.Fatalf("首次加锁：%v busy=%v", err, busy)
	}
	if _, busy, err := lockFile(p); err != nil || !busy {
		t.Fatalf("已被持有时应报 busy：%v busy=%v", err, busy)
	}
	rel()
	rel2, busy, err := lockFile(p)
	if err != nil || busy {
		t.Fatalf("释放后应可再取得：%v busy=%v", err, busy)
	}
	rel2()
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ln := filepath.Join(dir, "l.lock")
	if err := os.Symlink(other, ln); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lockFile(ln); err == nil {
		t.Fatal("符号链接的锁文件必须被拒绝")
	}
}
