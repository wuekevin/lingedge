package probe

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const fixtureRoot = "../../../tests/fixtures/hostprobe"

func fixture(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// materializeProc 把 tests/fixtures/hostprobe/proc 复制到临时目录，并按 fdmap.txt 实体化 /proc/<pid>/fd/<n> -> socket:[inode] 符号链接
// （悬空符号链接不适合放进 git）。返回临时 proc 根。
func materializeProc(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join(fixtureRoot, "proc")
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, r), 0o755)
		}
		if r == "fdmap.txt" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, r), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range strings.Split(string(fixture(t, "proc/fdmap.txt")), "\n") {
		f := strings.Fields(ln)
		if len(f) != 3 || strings.HasPrefix(ln, "#") {
			continue
		}
		if _, err := strconv.Atoi(f[1]); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(dst, f[0], "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f[2], filepath.Join(dir, f[1])); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func findSock(socks []Socket, proto, addr string, port int) (Socket, bool) {
	for _, s := range socks {
		if s.Listener.Proto == proto && s.Listener.Addr == addr && s.Listener.Port == port {
			return s, true
		}
	}
	return Socket{}, false
}

func bufioReader(s string) *bufio.Reader     { return bufio.NewReader(strings.NewReader(s)) }
func bufioReader2(r io.Reader) *bufio.Reader { return bufio.NewReader(r) }
