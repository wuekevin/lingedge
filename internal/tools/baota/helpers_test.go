package baota

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureRoot = "../../../tests/fixtures/hostprobe"

// copyTree 把夹具树复制到临时目录（保留文件模式），返回新根。
func copyTree(t *testing.T, rel string) string {
	t.Helper()
	src := filepath.Join(fixtureRoot, rel)
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, r), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		return os.WriteFile(filepath.Join(dst, r), b, fi.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// recordingFS 包装真实的 OSFS，记录每一次访问（读/列目录/stat）的路径——不改变行为，只用于证明「没有打开过某些文件」。
type recordingFS struct {
	inner FS
	mu    sync.Mutex
	reads []string
	lists []string
	stats []string
}

func (r *recordingFS) ReadFile(name string, max int64) ([]byte, error) {
	r.mu.Lock()
	r.reads = append(r.reads, name)
	r.mu.Unlock()
	return r.inner.ReadFile(name, max)
}
func (r *recordingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	r.mu.Lock()
	r.lists = append(r.lists, name)
	r.mu.Unlock()
	return r.inner.ReadDir(name)
}
func (r *recordingFS) Lstat(name string) (fs.FileInfo, error) {
	r.mu.Lock()
	r.stats = append(r.stats, name)
	r.mu.Unlock()
	return r.inner.Lstat(name)
}

func (r *recordingFS) Readlink(name string) (string, error) {
	r.mu.Lock()
	r.stats = append(r.stats, name)
	r.mu.Unlock()
	return r.inner.Readlink(name)
}

func probeCase(t *testing.T, rel string) Report {
	t.Helper()
	return Probe(context.Background(), Options{FS: OSFS{Root: filepath.Join(fixtureRoot, "baota", rel)}, Now: time.Date(2026, 11, 21, 1, 2, 3, 0, time.UTC)})
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func evidenceKinds(r Report) []string {
	var ks []string
	for _, e := range r.Evidence {
		ks = append(ks, e.Kind)
	}
	sort.Strings(ks)
	return ks
}

func hasNote(r Report, sub string) bool {
	for _, n := range r.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
