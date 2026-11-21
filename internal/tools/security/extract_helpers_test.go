package security

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type tent struct {
	name string
	typ  byte
	body string
	link string
	mode int64
	pax  bool // 全局 PAX 头
}

func f(name, body string) tent { return tent{name: name, typ: tar.TypeReg, body: body} }
func d(name string) tent       { return tent{name: name, typ: tar.TypeDir} }

func buildTar(t testing.TB, ents ...tent) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Linkname: e.link, Format: tar.FormatPAX}
		if e.mode != 0 {
			h.Mode = e.mode
		}
		if e.typ == tar.TypeReg || e.typ == 0 {
			h.Size = int64(len(e.body))
		}
		if e.pax {
			h.Format, h.PAXRecords = tar.FormatPAX, map[string]string{"comment": "x"}
			h.Name, h.Mode = "", 0
		}
		if e.typ == tar.TypeChar || e.typ == tar.TypeBlock {
			h.Devmajor, h.Devminor = 1, 3
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("WriteHeader(%q): %v", e.name, err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func gz(t testing.TB, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// sumsFor 为所有普通文件条目生成校验和表。
func sumsFor(ents ...tent) map[string]string {
	m := map[string]string{}
	for _, e := range ents {
		if e.typ == tar.TypeReg {
			m[e.name] = sum(e.body)
		}
	}
	return m
}

// snapshot 记录目录树（相对路径 -> 模式+大小），用于断言「没有越界写入」。
func snapshot(t testing.TB, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := de.Info()
		if err != nil {
			return err
		}
		out[rel] = info.Mode().String() + "/" + func() string {
			if info.Mode().IsRegular() {
				return sum(mustRead(t, p))
			}
			return ""
		}()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustRead(t testing.TB, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func keys(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
