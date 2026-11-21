package security_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// G-4（路径穿越/越界写入）向量集（Story 1.26 AC2、tools-design §18.1）：对真实的 security.SafeExtract，在真实文件系统上
// 逐条投入恶意归档，要求：每条都被拒绝且错误类别正确；失败后目标与 .partial 都不存在；
// 沙箱里的「受害者」文件、兄弟目录、父目录之外的任何位置都没有被写入（以目录树哈希快照比对）。
// 向量通过内存构造的 tar 注入，不依赖外部样本文件。

type ent struct {
	name, link, body string
	typ              byte
}

func reg(name, body string) ent { return ent{name: name, body: body, typ: tar.TypeReg} }

func mkTar(t *testing.T, ents ...ent) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o777, Linkname: e.link, Format: tar.FormatPAX}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if e.typ == tar.TypeChar || e.typ == tar.TypeBlock {
			h.Devmajor, h.Devminor = 1, 3
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
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

func gzBytes(t *testing.T, raw []byte) []byte {
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

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// tree 返回沙箱（含父目录之外的兄弟目录）的完整快照：相对路径 -> 类型/模式/内容哈希/链接目标。
func tree(t *testing.T, root string) map[string]string {
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
		v := info.Mode().String()
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			v += "->" + l
		case info.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			v += ":" + sha(string(b))
		}
		out[rel] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diff(a, b map[string]string) string {
	var d []string
	for k, v := range a {
		if b[k] != v {
			d = append(d, "变更/删除 "+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d = append(d, "新增 "+k)
		}
	}
	sort.Strings(d)
	return strings.Join(d, "; ")
}

type sandbox struct {
	base, parent, dst, victim, outsideDir string
}

func newSandbox(t *testing.T) sandbox {
	t.Helper()
	base := t.TempDir()
	s := sandbox{base: base, parent: filepath.Join(base, "parent"), outsideDir: filepath.Join(base, "outside"), victim: filepath.Join(base, "victim.txt")}
	s.dst = filepath.Join(s.parent, "release")
	for _, d := range []string{s.parent, s.outsideDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(s.victim, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.outsideDir, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestG4PathTraversalVectors(t *testing.T) {
	big := strings.Repeat("\x00", 4<<20)
	deep := strings.Repeat("d/", 40) + "f"
	type vec struct {
		name string
		raw  func(t *testing.T, s sandbox) []byte
		gz   bool
		lim  security.Limits
		sums map[string]string
		kind error
	}
	plain := func(ents ...ent) func(*testing.T, sandbox) []byte {
		return func(t *testing.T, _ sandbox) []byte { return mkTar(t, ents...) }
	}
	vectors := []vec{
		{name: "../ 相对穿越", raw: plain(reg("../evil", "x")), kind: security.ErrUnsafePath},
		{name: "中段 ..", raw: plain(reg("a/../../evil", "x")), kind: security.ErrUnsafePath},
		{name: "纯 ..", raw: plain(ent{name: "..", typ: tar.TypeDir}), kind: security.ErrUnsafePath},
		{name: "多级 ../../../etc/passwd", raw: plain(reg("../../../etc/passwd", "x")), kind: security.ErrUnsafePath},
		{name: "绝对路径", raw: plain(reg("/tmp/evil", "x")), kind: security.ErrUnsafePath},
		{name: "绝对路径目录", raw: plain(ent{name: "/etc/", typ: tar.TypeDir}), kind: security.ErrUnsafePath},
		{name: "双斜杠开头", raw: plain(reg("//etc/evil", "x")), kind: security.ErrUnsafePath},
		{name: "反斜杠穿越", raw: plain(reg("..\\evil", "x")), kind: security.ErrUnsafePath},
		{name: "驱动器前缀", raw: plain(reg("C:/evil", "x")), kind: security.ErrUnsafePath},
		{name: "%2e%2e 编码穿越", raw: plain(reg("%2e%2e/evil", "x")), kind: security.ErrUnsafePath},
		{name: "%2E%2E%2F 编码", raw: plain(reg("%2E%2E%2Fevil", "x")), kind: security.ErrUnsafePath},
		{name: "双重编码 %252e", raw: plain(reg("a/%252e%252e/evil", "x")), kind: security.ErrUnsafePath},
		{name: "换行/控制字符", raw: plain(reg("a\nb", "x")), kind: security.ErrUnsafePath},
		{name: "过深路径", raw: plain(reg(deep, "x")), kind: security.ErrUnsafePath},
		{name: "过长路径", raw: plain(reg(strings.Repeat("a", 2000), "x")), kind: security.ErrUnsafePath},
		{name: "符号链接指向外部目录", raw: plain(ent{name: "link", typ: tar.TypeSymlink, link: "../../outside"}, reg("link/pwned", "x")), kind: security.ErrUnsupportedType},
		{name: "符号链接指向绝对路径", raw: plain(ent{name: "etc", typ: tar.TypeSymlink, link: "/etc"}, reg("etc/cron.d/x", "x")), kind: security.ErrUnsupportedType},
		{name: "符号链接逃逸（先目录后链接）", raw: plain(ent{name: "d/", typ: tar.TypeDir}, ent{name: "d/l", typ: tar.TypeSymlink, link: "../.."}, reg("d/l/victim.txt", "pwn")), kind: security.ErrUnsupportedType},
		{name: "指向包内的符号链接也拒绝", raw: plain(reg("a", "x"), ent{name: "b", typ: tar.TypeSymlink, link: "a"}), sums: map[string]string{"a": sha("x")}, kind: security.ErrUnsupportedType},
		{name: "硬链接到包内文件", raw: plain(reg("a", "x"), ent{name: "b", typ: tar.TypeLink, link: "a"}), sums: map[string]string{"a": sha("x")}, kind: security.ErrUnsupportedType},
		{name: "硬链接到系统文件", raw: plain(ent{name: "passwd", typ: tar.TypeLink, link: "/etc/passwd"}), kind: security.ErrUnsupportedType},
		{name: "字符设备", raw: plain(ent{name: "dev/null", typ: tar.TypeChar}), kind: security.ErrUnsupportedType},
		{name: "块设备", raw: plain(ent{name: "dev/sda", typ: tar.TypeBlock}), kind: security.ErrUnsupportedType},
		{name: "FIFO", raw: plain(ent{name: "fifo", typ: tar.TypeFifo}), kind: security.ErrUnsupportedType},
		{name: "超限条目数", raw: func(t *testing.T, _ sandbox) []byte {
			var ents []ent
			for i := 0; i < 50; i++ {
				ents = append(ents, ent{name: "d" + string(rune('A'+i%26)) + string(rune('a'+i/26)) + "/", typ: tar.TypeDir})
			}
			return mkTar(t, ents...)
		}, lim: security.Limits{MaxFiles: 10}, kind: security.ErrLimit},
		{name: "声明过大的单文件（压缩炸弹）", raw: plain(reg("zeros", big)), gz: true, lim: security.Limits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20}, sums: map[string]string{"zeros": sha(big)}, kind: security.ErrLimit},
		{name: "累计过大（压缩炸弹，多文件）", raw: func(t *testing.T, _ sandbox) []byte {
			return mkTar(t, reg("a", big[:900<<10]), reg("b", big[:900<<10]), reg("c", big[:900<<10]))
		}, gz: true, lim: security.Limits{MaxTotalBytes: 2 << 20}, sums: map[string]string{"a": sha(big[:900<<10]), "b": sha(big[:900<<10]), "c": sha(big[:900<<10])}, kind: security.ErrLimit},
		{name: "归档后追加海量垃圾（实际流式大小）", raw: func(t *testing.T, _ sandbox) []byte {
			return append(mkTar(t, reg("a", "x")), make([]byte, 64<<20)...)
		}, gz: true, lim: security.Limits{MaxFiles: 4, MaxTotalBytes: 1 << 20}, sums: map[string]string{"a": sha("x")}, kind: security.ErrLimit},
		{name: "清单未声明的多余文件", raw: plain(reg("a", "x"), reg("extra", "y")), sums: map[string]string{"a": sha("x")}, kind: security.ErrExtra},
		{name: "清单声明的文件缺失", raw: plain(reg("a", "x")), sums: map[string]string{"a": sha("x"), "b": sha("y")}, kind: security.ErrMissing},
		{name: "内容与哈希不符", raw: plain(reg("a", "tampered")), sums: map[string]string{"a": sha("x")}, kind: security.ErrChecksum},
		{name: "大小写冲突覆盖", raw: plain(reg("Bin", "1"), reg("bin", "2")), sums: map[string]string{"Bin": sha("1"), "bin": sha("2")}, kind: security.ErrUnsafePath},
		{name: "文件当目录使用", raw: plain(reg("a", "1"), reg("a/b", "2")), sums: map[string]string{"a": sha("1"), "a/b": sha("2")}, kind: security.ErrUnsafePath},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			s := newSandbox(t)
			before := tree(t, s.base)
			raw := v.raw(t, s)
			if v.gz {
				raw = gzBytes(t, raw)
			}
			sums := v.sums
			if sums == nil {
				sums = map[string]string{}
			}
			_, err := security.SafeExtract(context.Background(), bytes.NewReader(raw), s.dst, security.ExtractOptions{Gzip: v.gz, Limits: v.lim, Checksums: sums})
			if err == nil {
				t.Fatal("恶意归档被接受了")
			}
			if !errors.Is(err, v.kind) {
				t.Fatalf("错误类别应为 %v，实际 %v", v.kind, err)
			}
			if d := diff(before, tree(t, s.base)); d != "" {
				t.Fatalf("失败后沙箱发生变化（越界写入或残留）: %s", d)
			}
		})
	}
}

// 目标位置被预先布置了符号链接（攻击者控制的父目录）：不得跟随，不得写入链接指向处。
func TestG4PreplantedSymlinks(t *testing.T) {
	raw := mkTar(t, reg("a", "x"))
	opts := security.ExtractOptions{Checksums: map[string]string{"a": sha("x")}}
	for _, name := range []string{"release", "release.partial"} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			if err := os.Symlink(s.outsideDir, filepath.Join(s.parent, name)); err != nil {
				t.Fatal(err)
			}
			before := tree(t, s.base)
			_, err := security.SafeExtract(context.Background(), bytes.NewReader(raw), s.dst, opts)
			if !errors.Is(err, security.ErrDestination) {
				t.Fatalf("应 ErrDestination: %v", err)
			}
			if d := diff(before, tree(t, s.base)); d != "" {
				t.Fatalf("预置链接被跟随或沙箱变化: %s", d)
			}
		})
	}
}

// 对照组：合法归档在同一沙箱里成功，且只写在目标内（防止「全部拒绝」式的假通过）。
func TestG4ValidArchiveStaysInsideTarget(t *testing.T) {
	s := newSandbox(t)
	before := tree(t, s.base)
	raw := mkTar(t, ent{name: "bin/", typ: tar.TypeDir}, reg("bin/tool", "payload"), reg("conf/a.yaml", "k: v\n"))
	res, err := security.SafeExtract(context.Background(), bytes.NewReader(raw), s.dst, security.ExtractOptions{
		Checksums: map[string]string{"bin/tool": sha("payload"), "conf/a.yaml": sha("k: v\n")}, Executable: []string{"bin/tool"},
	})
	if err != nil || res.Files != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	after := tree(t, s.base)
	for k := range after {
		if _, existed := before[k]; existed {
			continue
		}
		if k != "parent/release" && !strings.HasPrefix(k, "parent/release/") {
			t.Fatalf("目标之外出现新文件: %s", k)
		}
	}
	if st, _ := os.Stat(filepath.Join(s.dst, "bin/tool")); st.Mode().Perm() != 0o755 {
		t.Fatalf("可执行位: %v", st.Mode())
	}
}
