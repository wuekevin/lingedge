package security

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzSafeExtract：任意字节作为归档，不论成功与否都必须满足不变量——
//   - 失败：父目录里不留任何东西（无 .partial、无目标）；
//   - 成功：父目录里只有目标目录，其下只有目录与 0644/0755 的普通文件，无符号链接/特殊位；
//   - 任何情况下都不能在父目录之外写入（哨兵文件内容不变，兄弟目录不增不减）。
func FuzzSafeExtract(f *testing.F) {
	seed := [][]tent{
		{f2("a", "x")},
		{d2("d/"), f2("d/b", "yy")},
		{f2("../evil", "x")},
		{f2("/abs", "x")},
		{{name: "ln", typ: tar.TypeSymlink, link: "../../etc/passwd"}},
		{{name: "hl", typ: tar.TypeLink, link: "a"}},
		{{name: "dev", typ: tar.TypeChar}},
		{f2("%2e%2e/x", "x")},
		{f2("a\\..\\b", "x")},
		{f2("A", "1"), f2("a", "2")},
		{f2("a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r", "x")},
	}
	for _, s := range seed {
		raw := buildTar(f, s...)
		f.Add(raw, false)
		f.Add(gz(f, raw), true)
	}
	f.Add([]byte("not a tar"), false)
	f.Add([]byte{}, false)
	f.Add(bytes.Repeat([]byte{0}, 4096), false)

	f.Fuzz(func(t *testing.T, raw []byte, gzip bool) {
		base, err := os.MkdirTemp("", "fuzz-extract-")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.RemoveAll(base) }()
		parent := filepath.Join(base, "parent")
		if err := os.Mkdir(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(base, "sentinel")
		if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, base)

		// 用宽松解析构造校验表，使「合法」归档有机会成功。
		sums := map[string]string{}
		if !gzip {
			tr := tar.NewReader(bytes.NewReader(raw))
			for {
				h, err := tr.Next()
				if err != nil {
					break
				}
				if h.Typeflag != tar.TypeReg {
					continue
				}
				b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
				if err != nil {
					break
				}
				if c, e := CleanEntryName(h.Name, false, Limits{}); e == nil {
					sums[c] = sum(string(b))
				}
			}
		}
		dst := filepath.Join(parent, "release")
		_, err = SafeExtract(context.Background(), bytes.NewReader(raw), dst, ExtractOptions{
			Gzip: gzip, Checksums: sums, Limits: Limits{MaxFiles: 64, MaxTotalBytes: 1 << 20, MaxFileBytes: 1 << 20, MaxDepth: 8, MaxPathLen: 128},
		})

		after := snapshot(t, base)
		if sb := after["sentinel"]; sb != before["sentinel"] {
			t.Fatalf("哨兵被改动")
		}
		for rel, st := range after {
			if rel == "." || rel == "sentinel" || rel == "parent" {
				continue
			}
			if rel != "parent/release" && !strings.HasPrefix(rel, "parent/release/") {
				t.Fatalf("越界写入: %s", rel)
			}
			_ = st
		}
		if err != nil {
			for rel := range after {
				if strings.HasPrefix(rel, "parent/") {
					t.Fatalf("失败后残留 %s（err=%v）", rel, err)
				}
			}
			return
		}
		walkErr := filepath.WalkDir(dst, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := de.Info()
			if err != nil {
				return err
			}
			m := info.Mode()
			switch {
			case m.IsDir():
				if m != fs.ModeDir|0o755 {
					t.Errorf("目录 %s 模式 %v", p, m)
				}
			case m.IsRegular():
				if m != 0o644 && m != 0o755 {
					t.Errorf("文件 %s 模式 %v", p, m)
				}
			default:
				t.Errorf("%s 是特殊文件 %v", p, m)
			}
			return nil
		})
		if walkErr != nil {
			t.Fatal(walkErr)
		}
	})
}

func f2(name, body string) tent { return f(name, body) }
func d2(name string) tent       { return d(name) }

// FuzzCleanEntryName：通过校验的名字必须满足「规范、相对、无 .. 、无控制字符、无百分号编码、在限额内」。
func FuzzCleanEntryName(f *testing.F) {
	for _, s := range []string{"a/b", "./a", "../a", "/a", "a\\b", "%2e%2e", "a\x00", "a//b", ".", "./", "a/", "é"} {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, name string, isDir bool) {
		got, err := CleanEntryName(name, isDir, Limits{MaxPathLen: 128, MaxDepth: 6})
		if err != nil {
			return
		}
		if got == "." {
			if !isDir {
				t.Fatalf("文件不能是根: %q", name)
			}
			return
		}
		if got == "" || strings.HasPrefix(got, "/") || strings.HasSuffix(got, "/") || filepath.IsAbs(got) {
			t.Fatalf("%q -> %q 不规范", name, got)
		}
		for _, seg := range strings.Split(got, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("%q -> %q 含非法段", name, got)
			}
		}
		if strings.ContainsAny(got, "\\\x00") || strings.Count(got, "/")+1 > 6 || len(got) > 128 {
			t.Fatalf("%q -> %q 违反限制", name, got)
		}
		for i := 0; i < len(got); i++ {
			if got[i] < 0x20 || got[i] == 0x7f {
				t.Fatalf("%q -> %q 含控制字符", name, got)
			}
			if got[i] == '%' && i+2 < len(got) && isHexByte(got[i+1]) && isHexByte(got[i+2]) {
				t.Fatalf("%q -> %q 含百分号编码", name, got)
			}
		}
		// 拼到根上不得越出。
		root := "/r"
		if j := filepath.Join(root, got); !strings.HasPrefix(j, root+"/") {
			t.Fatalf("%q -> %q 越出根", name, got)
		}
	})
}
