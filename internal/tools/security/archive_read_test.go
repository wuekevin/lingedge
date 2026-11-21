package security

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestReadMembersBasics(t *testing.T) {
	raw := buildTar(t, d("META/"), f("META/package.json", `{"a":1}`), f("META/SHA256SUMS", "sums"), f("META/SHA256SUMS.sig", "sig"), d("payload/"), f("payload/x", strings.Repeat("p", 5000)))
	want := map[string]int64{"META/package.json": 100, "META/SHA256SUMS": 100, "META/SHA256SUMS.sig": 100}
	for _, z := range []bool{false, true} {
		in := raw
		if z {
			in = gz(t, raw)
		}
		got, err := ReadMembers(bytes.NewReader(in), ReadMembersOptions{Want: want, Before: "payload/", Gzip: z})
		if err != nil {
			t.Fatalf("gzip=%v: %v", z, err)
		}
		if string(got["META/SHA256SUMS"]) != "sums" || string(got["META/package.json"]) != `{"a":1}` || len(got) != 3 {
			t.Fatalf("gzip=%v: %v", z, got)
		}
	}
}

func TestReadMembersRejections(t *testing.T) {
	want := map[string]int64{"META/a": 10, "META/b": 10}
	run := func(raw []byte, opt ReadMembersOptions) error {
		opt.Want = want
		_, err := ReadMembers(bytes.NewReader(raw), opt)
		return err
	}
	cases := []struct {
		name string
		raw  []byte
		opt  ReadMembersOptions
		is   error
	}{
		{"缺少一个", buildTar(t, f("META/a", "1")), ReadMembersOptions{}, ErrMissing},
		{"payload 先于 META", buildTar(t, f("META/a", "1"), f("payload/x", "x"), f("META/b", "2")), ReadMembersOptions{Before: "payload/"}, ErrMissing},
		{"重复", buildTar(t, f("META/a", "1"), f("META/a", "1"), f("META/b", "2")), ReadMembersOptions{}, ErrUnsafePath},
		{"超过单文件上限", buildTar(t, f("META/a", strings.Repeat("x", 11)), f("META/b", "2")), ReadMembersOptions{}, ErrLimit},
		{"符号链接", buildTar(t, tent{name: "META/ln", typ: tar.TypeSymlink, link: "x"}, f("META/a", "1"), f("META/b", "2")), ReadMembersOptions{}, ErrUnsupportedType},
		{"路径穿越", buildTar(t, f("../META/a", "1"), f("META/b", "2")), ReadMembersOptions{}, ErrUnsafePath},
		{"不是 tar", []byte(strings.Repeat("junk", 500)), ReadMembersOptions{}, ErrArchive},
		{"gzip 头无效", []byte("not gzip"), ReadMembersOptions{Gzip: true}, ErrArchive},
		{"条目过多", buildTar(t, f("n1", "x"), f("n2", "x"), f("n3", "x"), f("META/a", "1"), f("META/b", "2")), ReadMembersOptions{Limits: Limits{MaxFiles: 2}}, ErrLimit},
	}
	for _, c := range cases {
		if err := run(c.raw, c.opt); !errors.Is(err, c.is) {
			t.Errorf("%s: 期望 %v，实际 %v", c.name, c.is, err)
		}
	}
	if _, err := ReadMembers(bytes.NewReader(nil), ReadMembersOptions{}); !errors.Is(err, ErrOptions) {
		t.Errorf("空 Want 应为 ErrOptions：%v", err)
	}
	if _, err := ReadMembers(bytes.NewReader(nil), ReadMembersOptions{Want: map[string]int64{"../x": 1}}); !errors.Is(err, ErrOptions) {
		t.Errorf("非法 Want 路径应为 ErrOptions：%v", err)
	}
}

// 取齐即停：META 之后的内容（哪怕是损坏的数据）不会被读到。
func TestReadMembersStopsEarly(t *testing.T) {
	raw := buildTar(t, f("META/a", "1"), f("META/b", "2"))
	raw = append(raw[:2048], bytes.Repeat([]byte("garbage!"), 1000)...) // 去掉结束块，后面全是垃圾
	if _, err := ReadMembers(bytes.NewReader(raw), ReadMembersOptions{Want: map[string]int64{"META/a": 8, "META/b": 8}}); err != nil {
		t.Fatalf("取齐后应立即返回：%v", err)
	}
}

func TestIndexTarOffsetsAndLimits(t *testing.T) {
	long := strings.Repeat("d", 90) + "/" + strings.Repeat("f", 90)
	raw := buildTar(t, f("a", "hello"), d("dir/"), f(long, strings.Repeat("x", 1000)), f("e", ""), f("z", strings.Repeat("y", 512)))
	es, err := IndexTar(bytes.NewReader(raw), IndexLimits{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "hello", long: strings.Repeat("x", 1000), "e": "", "z": strings.Repeat("y", 512)}
	files := 0
	for _, e := range es {
		if e.Dir {
			continue
		}
		files++
		if got := string(raw[e.Offset : e.Offset+e.Size]); got != want[e.Name] {
			t.Errorf("%s: 偏移处内容不符", e.Name)
		}
	}
	if files != len(want) {
		t.Fatalf("文件数 %d", files)
	}
	for name, c := range map[string]struct {
		raw []byte
		lim IndexLimits
		is  error
	}{
		"单文件超限":  {raw, IndexLimits{MaxFileBytes: 100}, ErrLimit},
		"条目超限":   {raw, IndexLimits{MaxEntries: 2}, ErrLimit},
		"整体流超限":  {raw, IndexLimits{MaxArchiveSize: 600}, ErrLimit},
		"符号链接":   {buildTar(t, tent{name: "l", typ: tar.TypeSymlink, link: "x"}), IndexLimits{}, ErrUnsupportedType},
		"设备":     {buildTar(t, tent{name: "c", typ: tar.TypeChar}), IndexLimits{}, ErrUnsupportedType},
		"重复":     {buildTar(t, f("a", "1"), f("a", "2")), IndexLimits{}, ErrUnsafePath},
		"大小写冲突":  {buildTar(t, f("a", "1"), f("A", "2")), IndexLimits{}, ErrUnsafePath},
		"穿越":     {buildTar(t, f("../a", "1")), IndexLimits{}, ErrUnsafePath},
		"数据被截断":  {raw[:len(raw)-1500], IndexLimits{}, ErrArchive},
		"不是 tar": {[]byte(strings.Repeat("x", 2000)), IndexLimits{}, ErrArchive},
	} {
		if _, err := IndexTar(bytes.NewReader(c.raw), c.lim); !errors.Is(err, c.is) {
			t.Errorf("%s: 期望 %v，实际 %v", name, c.is, err)
		}
	}
}

// ustarHeader 手工构造带指定类型标志的头（含校验和）。
func ustarHeader(name string, typ byte, size int64) []byte {
	h := make([]byte, 512)
	copy(h, name)
	copy(h[100:], "0000644\x00")
	copy(h[108:], "0000000\x00")
	copy(h[116:], "0000000\x00")
	copy(h[124:], fmt.Sprintf("%011o\x00", size))
	copy(h[136:], "00000000000\x00")
	h[156] = typ
	copy(h[257:], "ustar\x0000")
	copy(h[148:], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
	return h
}

func pad512(b []byte) []byte {
	if r := len(b) % 512; r != 0 {
		b = append(b, make([]byte, 512-r)...)
	}
	return b
}

func paxRecord(k, v string) string {
	n := len(k) + len(v) + 3
	for {
		s := fmt.Sprintf("%d %s=%s\n", n, k, v)
		if len(s) == n {
			return s
		}
		n = len(s)
	}
}

// 稀疏文件的数据流在文件里不连续：IndexTar 的偏移对调用方没有意义，必须拒绝（PAX 1.0 稀疏与旧 GNU 稀疏两种）。
func TestIndexTarRejectsSparse(t *testing.T) {
	recs := paxRecord("GNU.sparse.major", "1") + paxRecord("GNU.sparse.minor", "0") + paxRecord("GNU.sparse.name", "s") + paxRecord("GNU.sparse.realsize", "10")
	var b bytes.Buffer
	b.Write(ustarHeader("PaxHeaders/s", 'x', int64(len(recs))))
	b.Write(pad512([]byte(recs)))
	body := append(pad512([]byte("1\n0\n3\n")), pad512([]byte("abc"))...)
	b.Write(ustarHeader("s", '0', int64(len(body))))
	b.Write(body)
	b.Write(make([]byte, 1024))
	if es, err := IndexTar(bytes.NewReader(b.Bytes()), IndexLimits{}); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("PAX 稀疏应被拒绝：%v %v", es, err)
	}
	var g bytes.Buffer
	g.Write(ustarHeader("old", 'S', 3))
	g.Write(pad512([]byte("abc")))
	g.Write(make([]byte, 1024))
	if _, err := IndexTar(bytes.NewReader(g.Bytes()), IndexLimits{}); err == nil {
		t.Fatalf("GNU 稀疏应被拒绝：%v", err)
	}
	if _, err := ReadMembers(bytes.NewReader(b.Bytes()), ReadMembersOptions{Want: map[string]int64{"x": 1}}); err == nil {
		t.Fatal("ReadMembers 遇到 PAX 稀疏也应失败/不取到内容")
	}
}

func FuzzIndexTar(f *testing.F) {
	long := strings.Repeat("d", 90) + "/" + strings.Repeat("f", 90)
	for _, s := range [][]tent{
		{f2("a", "hello")},
		{d2("dir/"), f2("dir/b", "yy"), f2(long, "z")},
		{f2("../x", "1")},
		{{name: "l", typ: tar.TypeSymlink, link: "x"}},
		{f2("a", "1"), f2("A", "2")},
	} {
		f.Add(buildTar(f, s...))
	}
	f.Add([]byte("junk"))
	f.Add(bytes.Repeat([]byte{0}, 2048))
	f.Fuzz(func(t *testing.T, raw []byte) {
		es, err := IndexTar(bytes.NewReader(raw), IndexLimits{MaxEntries: 64, MaxFileBytes: 1 << 20, MaxArchiveSize: 1 << 22, MaxDepth: 8, MaxPathLen: 128})
		if err != nil {
			return
		}
		seen := map[string]bool{}
		// 与标准库读取器逐条对照：成功的索引必须与 tar.Reader 看到的内容完全一致，偏移处的字节就是文件内容。
		tr := tar.NewReader(bytes.NewReader(raw))
		for _, e := range es {
			if c, err := CleanEntryName(e.Name, e.Dir, Limits{MaxDepth: 8, MaxPathLen: 128}); err != nil || c != e.Name {
				t.Fatalf("索引名 %q 不是规范化后的合法名称（%v）", e.Name, err)
			}
			low := strings.ToLower(e.Name)
			if seen[low] {
				t.Fatalf("索引含重复/仅大小写不同的名称 %q", e.Name)
			}
			seen[low] = true
			if e.Dir {
				continue
			}
			if e.Offset < 0 || e.Size < 0 || e.Offset+e.Size > int64(len(raw)) {
				t.Fatalf("条目 %v 越出输入（%d 字节）", e, len(raw))
			}
			for {
				h, err := tr.Next()
				if err != nil {
					t.Fatalf("索引成功但标准库读取失败：%v", err)
				}
				if h.Typeflag != tar.TypeReg {
					continue
				}
				if cn, e2 := CleanEntryName(h.Name, false, Limits{MaxDepth: 8, MaxPathLen: 128}); e2 != nil || cn != e.Name {
					continue // 可能是被索引跳过的 "." 根条目之类
				}
				b, _ := io.ReadAll(tr)
				if !bytes.Equal(b, raw[e.Offset:e.Offset+e.Size]) {
					t.Fatalf("%s: 偏移处内容与标准库读取到的不符", e.Name)
				}
				break
			}
		}
	})
}

func FuzzReadMembers(f *testing.F) {
	f.Add(buildTar(f, f2("META/a", "1"), f2("META/b", "2")), false)
	f.Add(gz(f, buildTar(f, f2("META/a", "1"), f2("payload/x", "x"), f2("META/b", "2"))), true)
	f.Add([]byte("junk"), false)
	f.Fuzz(func(t *testing.T, raw []byte, z bool) {
		got, err := ReadMembers(bytes.NewReader(raw), ReadMembersOptions{
			Want: map[string]int64{"META/a": 16, "META/b": 16}, Before: "payload/", Gzip: z, Limits: Limits{MaxFiles: 32, MaxTotalBytes: 1 << 16},
		})
		if err != nil {
			return
		}
		if len(got) != 2 {
			t.Fatalf("成功必须取齐：%v", got)
		}
		for k, v := range got {
			if len(v) > 16 || (k != "META/a" && k != "META/b") {
				t.Fatalf("越界结果 %q (%d)", k, len(v))
			}
		}
	})
}

func FuzzParseSHA256SUMS(f *testing.F) {
	h := strings.Repeat("a", 64)
	for _, s := range []string{h + "  payload/x\n", h + " *payload/y\r\n", h + "  ../x\n", "bad\n", h + "  a\n" + h + "  a\n", strings.ToUpper(h) + "  p/q\n", ""} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ParseSHA256SUMS(data, Limits{MaxDepth: 8, MaxPathLen: 128})
		if err != nil {
			return
		}
		for p, sum := range m {
			if c, e := CleanEntryName(p, false, Limits{MaxDepth: 8, MaxPathLen: 128}); e != nil || c != p {
				t.Fatalf("路径 %q 未规范化（%v）", p, e)
			}
			if len(sum) != 64 || !isHex(sum) || sum != strings.ToLower(sum) {
				t.Fatalf("哈希 %q 非法", sum)
			}
		}
	})
}
