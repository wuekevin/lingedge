package security

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func extractInto(t *testing.T, raw []byte, opts ExtractOptions) (parent, dst string, res ExtractResult, err error) {
	t.Helper()
	parent = t.TempDir()
	dst = filepath.Join(parent, "release")
	res, err = SafeExtract(context.Background(), bytes.NewReader(raw), dst, opts)
	return
}

func TestSafeExtractHappyPath(t *testing.T) {
	ents := []tent{
		d("./"), d("bin/"), f("bin/tool", "#!bin\n"), f("conf/app.yaml", "a: 1\n"), f("lib/x.so", "ELF"), d("empty/"),
		{name: "META/SHA256SUMS", typ: tar.TypeReg, body: "sums"},
	}
	// setuid/可执行位的包内 mode 必须被忽略。
	ents[2].mode = 0o4777
	ents[3].mode = 0o777
	opts := ExtractOptions{Checksums: sumsFor(ents[:5]...), Unchecked: []string{"META/SHA256SUMS"}, Executable: []string{"bin/tool"}}
	parent, dst, res, err := extractInto(t, buildTar(t, ents...), opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 4 || res.Bytes != int64(len("#!bin\n")+len("a: 1\n")+len("ELF")+len("sums")) {
		t.Fatalf("统计不符: %+v", res)
	}
	assertMode := func(rel string, want os.FileMode) {
		st, err := os.Lstat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode() != want {
			t.Fatalf("%s 模式 %v，期望 %v", rel, st.Mode(), want)
		}
	}
	assertMode("bin/tool", 0o755)
	assertMode("conf/app.yaml", 0o644)
	assertMode("lib/x.so", 0o644)
	assertMode("bin", os.ModeDir|0o755)
	assertMode("conf", os.ModeDir|0o755)
	assertMode("empty", os.ModeDir|0o755)
	if got := mustRead(t, filepath.Join(dst, "conf/app.yaml")); got != "a: 1\n" {
		t.Fatalf("内容 %q", got)
	}
	// 成功后没有 .partial 残留，父目录只有 release。
	if k := keys(snapshot(t, parent)); strings.Contains(strings.Join(k, ","), ".partial") {
		t.Fatalf("残留 .partial: %v", k)
	}
}

func TestSafeExtractGzip(t *testing.T) {
	ents := []tent{f("a/b.txt", "hello")}
	raw := gz(t, buildTar(t, ents...))
	parent := t.TempDir()
	dst := filepath.Join(parent, "r")
	if _, err := SafeExtract(context.Background(), bytes.NewReader(raw), dst, ExtractOptions{Gzip: true, Checksums: sumsFor(ents...)}); err != nil {
		t.Fatal(err)
	}
	if mustRead(t, filepath.Join(dst, "a/b.txt")) != "hello" {
		t.Fatal("内容不符")
	}
	// 声明 gzip 但给了明文 → 归档损坏。
	_, err := SafeExtract(context.Background(), bytes.NewReader(buildTar(t, ents...)), filepath.Join(parent, "r2"), ExtractOptions{Gzip: true, Checksums: sumsFor(ents...)})
	if !errors.Is(err, ErrArchive) {
		t.Fatalf("应为 ErrArchive: %v", err)
	}
}

func TestSafeExtractRejectsTypes(t *testing.T) {
	cases := map[string]tent{
		"符号链接":  {name: "ln", typ: tar.TypeSymlink, link: "/etc/passwd"},
		"硬链接":   {name: "hl", typ: tar.TypeLink, link: "bin/tool"},
		"字符设备":  {name: "dev/null", typ: tar.TypeChar},
		"块设备":   {name: "dev/sda", typ: tar.TypeBlock},
		"FIFO":  {name: "pipe", typ: tar.TypeFifo},
		"全局PAX": {name: "pax_global_header", typ: tar.TypeXGlobalHeader, pax: true},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			parent, dst, _, err := extractInto(t, buildTar(t, f("bin/tool", "x"), e), ExtractOptions{Checksums: map[string]string{"bin/tool": sum("x")}})
			if !errors.Is(err, ErrUnsupportedType) {
				t.Fatalf("应为 ErrUnsupportedType: %v", err)
			}
			assertClean(t, parent, dst)
		})
	}
}

// assertClean：失败后目标与 .partial 都不存在，父目录为空。
func assertClean(t *testing.T, parent, dst string) {
	t.Helper()
	for _, p := range []string{dst, dst + ".partial"} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("失败后仍存在 %s", p)
		}
	}
	if k := keys(snapshot(t, parent)); len(k) != 1 { // 只有 "."
		t.Fatalf("父目录有残留: %v", k)
	}
}

func TestSafeExtractLimits(t *testing.T) {
	big := strings.Repeat("A", 4096)
	mk := func(lim Limits, ents ...tent) error {
		sums := sumsFor(ents...)
		for k := range sums { // 过长/过深的名字本身进不了校验表：用 Unchecked 豁免，让条目走到路径校验
			if len(k) > 200 || strings.Count(k, "/") >= 3 {
				delete(sums, k)
			}
		}
		parent, dst, _, err := extractInto(t, buildTar(t, ents...), ExtractOptions{Limits: lim, Checksums: sums})
		if err != nil {
			assertClean(t, parent, dst)
		}
		return err
	}
	t.Run("条目数", func(t *testing.T) {
		var ents []tent
		for i := 0; i < 11; i++ {
			ents = append(ents, f("f"+string(rune('a'+i)), "x"))
		}
		if err := mk(Limits{MaxFiles: 10}, ents...); !errors.Is(err, ErrLimit) {
			t.Fatalf("%v", err)
		}
		if err := mk(Limits{MaxFiles: 11}, ents...); err != nil {
			t.Fatalf("恰好等于上限应通过: %v", err)
		}
	})
	t.Run("单文件", func(t *testing.T) {
		if err := mk(Limits{MaxFileBytes: 4095}, f("big", big)); !errors.Is(err, ErrLimit) {
			t.Fatalf("%v", err)
		}
		if err := mk(Limits{MaxFileBytes: 4096}, f("big", big)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("累计", func(t *testing.T) {
		if err := mk(Limits{MaxTotalBytes: 8191}, f("a", big), f("b", big)); !errors.Is(err, ErrLimit) {
			t.Fatalf("%v", err)
		}
		if err := mk(Limits{MaxTotalBytes: 8192}, f("a", big), f("b", big)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("深度与长度", func(t *testing.T) {
		if err := mk(Limits{MaxDepth: 3}, f("a/b/c/d", "x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("%v", err)
		}
		if err := mk(Limits{MaxPathLen: 8}, f("aaaa/bbbbbb", "x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("%v", err)
		}
		if err := mk(Limits{}, f(strings.Repeat("a", 256), "x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("单段 256 字节应被拒: %v", err)
		}
	})
	t.Run("限额被收紧到绝对上限", func(t *testing.T) {
		n := Limits{MaxFiles: 1 << 30, MaxTotalBytes: 1 << 62, MaxFileBytes: 1 << 62, MaxPathLen: 1 << 20, MaxDepth: 1 << 20}.normalized()
		if n.MaxFiles != AbsMaxFiles || n.MaxTotalBytes != AbsMaxTotalBytes || n.MaxFileBytes != AbsMaxFileBytes || n.MaxPathLen != AbsMaxPathLen || n.MaxDepth != AbsMaxDepth {
			t.Fatalf("%+v", n)
		}
		z := Limits{}.normalized()
		if z.MaxFiles <= 0 || z.MaxTotalBytes <= 0 || z.MaxDepth <= 0 {
			t.Fatalf("零值应取默认: %+v", z)
		}
	})
}

// 压缩炸弹：tar 头诚实但内容是海量零；以及归档结束后追加海量垃圾。流式总量必须被截断。
func TestSafeExtractCompressionBomb(t *testing.T) {
	t.Run("声明的大小超限（头诚实，解压前即拒绝）", func(t *testing.T) {
		body := strings.Repeat("\x00", 8<<20)
		raw := gz(t, buildTar(t, f("zeros", body)))
		if len(raw) > 64<<10 {
			t.Fatalf("样本应高度可压缩: %d", len(raw))
		}
		parent := t.TempDir()
		dst := filepath.Join(parent, "r")
		_, err := SafeExtract(context.Background(), bytes.NewReader(raw), dst, ExtractOptions{Gzip: true, Limits: Limits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20}, Checksums: map[string]string{"zeros": sum(body)}})
		if !errors.Is(err, ErrLimit) {
			t.Fatalf("%v", err)
		}
		assertClean(t, parent, dst)
	})
	t.Run("归档结束后的海量尾部数据（实际流式大小超限）", func(t *testing.T) {
		ents := []tent{f("a", "x")}
		raw := append(buildTar(t, ents...), bytes.Repeat([]byte{0}, 64<<20)...)
		parent := t.TempDir()
		dst := filepath.Join(parent, "r")
		_, err := SafeExtract(context.Background(), bytes.NewReader(gz(t, raw)), dst, ExtractOptions{Gzip: true, Limits: Limits{MaxFiles: 4, MaxTotalBytes: 1 << 20}, Checksums: sumsFor(ents...)})
		if !errors.Is(err, ErrLimit) {
			t.Fatalf("%v", err)
		}
		assertClean(t, parent, dst)
	})
	t.Run("声明很小但实际流被截断", func(t *testing.T) {
		raw := buildTar(t, f("a", strings.Repeat("y", 4096)))
		raw = raw[:700] // 头完整，内容被截断
		parent := t.TempDir()
		dst := filepath.Join(parent, "r")
		_, err := SafeExtract(context.Background(), bytes.NewReader(raw), dst, ExtractOptions{Checksums: map[string]string{"a": sum("y")}})
		if !errors.Is(err, ErrArchive) {
			t.Fatalf("%v", err)
		}
		assertClean(t, parent, dst)
	})
}

func TestSafeExtractChecksums(t *testing.T) {
	ents := []tent{f("a", "one"), f("b", "two")}
	t.Run("内容被篡改", func(t *testing.T) {
		sums := sumsFor(ents...)
		sums["b"] = sum("tampered")
		parent, dst, _, err := extractInto(t, buildTar(t, ents...), ExtractOptions{Checksums: sums})
		if !errors.Is(err, ErrChecksum) {
			t.Fatalf("%v", err)
		}
		assertClean(t, parent, dst)
	})
	t.Run("缺失条目", func(t *testing.T) {
		sums := sumsFor(ents...)
		sums["c"] = sum("three")
		parent, dst, _, err := extractInto(t, buildTar(t, ents...), ExtractOptions{Checksums: sums})
		if !errors.Is(err, ErrMissing) {
			t.Fatalf("%v", err)
		}
		assertClean(t, parent, dst)
	})
	t.Run("多余条目", func(t *testing.T) {
		parent, dst, _, err := extractInto(t, buildTar(t, ents...), ExtractOptions{Checksums: map[string]string{"a": sum("one")}})
		if !errors.Is(err, ErrExtra) {
			t.Fatalf("%v", err)
		}
		assertClean(t, parent, dst)
	})
	t.Run("Unchecked 豁免只对点名路径", func(t *testing.T) {
		_, _, _, err := extractInto(t, buildTar(t, ents...), ExtractOptions{Checksums: map[string]string{"a": sum("one")}, Unchecked: []string{"b"}})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("Executable 声明但不存在", func(t *testing.T) {
		parent, dst, _, err := extractInto(t, buildTar(t, ents...), ExtractOptions{Checksums: sumsFor(ents...), Executable: []string{"zzz"}})
		if !errors.Is(err, ErrMissing) {
			t.Fatalf("%v", err)
		}
		assertClean(t, parent, dst)
	})
	t.Run("缺少校验表即参数错误", func(t *testing.T) {
		_, _, _, err := extractInto(t, buildTar(t, ents...), ExtractOptions{})
		if !errors.Is(err, ErrOptions) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("坏的校验表", func(t *testing.T) {
		for _, m := range []map[string]string{{"a": "xyz"}, {"../a": sum("x")}, {"/a": sum("x")}} {
			if _, _, _, err := extractInto(t, buildTar(t, ents...), ExtractOptions{Checksums: m}); !errors.Is(err, ErrOptions) {
				t.Fatalf("%v: %v", m, err)
			}
		}
	})
}

func TestSafeExtractCollisionsAndConflicts(t *testing.T) {
	cases := map[string][]tent{
		"重复文件":      {f("a", "1"), f("a", "1")},
		"大小写冲突":     {f("Readme", "1"), f("README", "1")},
		"文件当目录":     {f("a", "1"), f("a/b", "1")},
		"目录与文件同名":   {d("a/"), f("a", "1")},
		"文件与目录同名":   {f("a", "1"), d("a/")},
		"目录大小写冲突":   {d("Dir/"), f("dir/x", "1")},
		"隐式目录大小写冲突": {f("Dir/x", "1"), f("dir/y", "1")},
	}
	for name, ents := range cases {
		t.Run(name, func(t *testing.T) {
			sums := map[string]string{}
			for _, e := range ents {
				if e.typ == tar.TypeReg {
					sums[e.name] = sum(e.body)
				}
			}
			parent, dst, _, err := extractInto(t, buildTar(t, ents...), ExtractOptions{Checksums: sums})
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("应为 ErrUnsafePath: %v", err)
			}
			// 大小写冲突必须由逻辑检出，不能依赖文件系统（macOS 默认不区分大小写会碰巧失败，Linux 则会静默并存）。
			if strings.Contains(name, "大小写") && !strings.Contains(err.Error(), "仅大小写不同") {
				t.Fatalf("应由大小写冲突检查拦截，而非文件系统偶然失败: %v", err)
			}
			assertClean(t, parent, dst)
		})
	}
}

func TestSafeExtractDestination(t *testing.T) {
	ents := []tent{f("a", "x")}
	opts := ExtractOptions{Checksums: sumsFor(ents...)}
	raw := buildTar(t, ents...)
	t.Run("目标已存在", func(t *testing.T) {
		p := t.TempDir()
		// 必须在解压之前就被拒绝（理由是「已存在」，而不是事后 rename 失败：Linux 上 rename 会静默替换空目录）。
		if _, err := SafeExtract(context.Background(), bytes.NewReader(raw), p, opts); !errors.Is(err, ErrDestination) || !strings.Contains(err.Error(), "已存在") {
			t.Fatalf("%v", err)
		}
	})
	t.Run(".partial 残留", func(t *testing.T) {
		p := t.TempDir()
		if err := os.Mkdir(filepath.Join(p, "r.partial"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := SafeExtract(context.Background(), bytes.NewReader(raw), filepath.Join(p, "r"), opts); !errors.Is(err, ErrDestination) {
			t.Fatalf("%v", err)
		}
		if _, err := os.Stat(filepath.Join(p, "r.partial")); err != nil {
			t.Fatal("不得删除调用方遗留的 .partial（可能是别人的数据）")
		}
	})
	t.Run("目标是悬空符号链接", func(t *testing.T) {
		p := t.TempDir()
		if err := os.Symlink(filepath.Join(p, "nowhere"), filepath.Join(p, "r")); err != nil {
			t.Fatal(err)
		}
		if _, err := SafeExtract(context.Background(), bytes.NewReader(raw), filepath.Join(p, "r"), opts); !errors.Is(err, ErrDestination) || !strings.Contains(err.Error(), "已存在") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("父目录不存在", func(t *testing.T) {
		if _, err := SafeExtract(context.Background(), bytes.NewReader(raw), filepath.Join(t.TempDir(), "no", "r"), opts); !errors.Is(err, ErrDestination) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("context 取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := t.TempDir()
		if _, err := SafeExtract(ctx, bytes.NewReader(raw), filepath.Join(p, "r"), opts); !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
		if k := keys(snapshot(t, p)); len(k) != 1 {
			t.Fatalf("取消后应清理: %v", k)
		}
	})
}

func TestCleanEntryName(t *testing.T) {
	ok := map[string]string{
		"a": "a", "a/b/c.txt": "a/b/c.txt", "./a": "a", "././a/b": "a/b", "a/": "a", "./": ".", ".": ".", "a b/c": "a b/c",
		"100%.txt": "100%.txt", "%zz": "%zz", "é/ü": "é/ü", "ab:c": "ab:c", "..a": "..a", "a..": "a..",
	}
	for in, want := range ok {
		isDir := strings.HasSuffix(in, "/") || in == "."
		got, err := CleanEntryName(in, isDir, Limits{})
		if err != nil || got != want {
			t.Errorf("%q -> %q, %v；期望 %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "/", "/etc/passwd", "//a", "../a", "a/../b", "a/..", "..", "a/./b", "a//b", "a/b//", ".//a",
		"a\\b", "..\\a", "a\x00b", "a\nb", "a\x1fb", "a\x7fb", "\tz", "C:/x", "c:\\x", "z:x",
		"%2e%2e/etc", "a/%2E%2E/b", "%2e", "a%2fb", "%5c", "%00", "\xff\xfe", "a\xc0\xafb",
		strings.Repeat("x/", 40) + "y", strings.Repeat("a", 300), "a/" + strings.Repeat("b", 256),
	}
	for _, in := range bad {
		if got, err := CleanEntryName(in, false, Limits{}); err == nil || !errors.Is(err, ErrUnsafePath) {
			t.Errorf("%q 应被拒绝，得到 %q, %v", in, got, err)
		}
	}
	if _, err := CleanEntryName("a/", false, Limits{}); err == nil {
		t.Error("非目录条目以斜杠结尾应被拒绝")
	}
	if _, err := CleanEntryName("./", false, Limits{}); err == nil {
		t.Error("根不能是文件")
	}
}

func TestParseSHA256SUMS(t *testing.T) {
	h := sum("x")
	m, err := ParseSHA256SUMS([]byte(h+"  bin/tool\n"+strings.ToUpper(h)+" *conf/a\n\n"), Limits{})
	if err != nil || m["bin/tool"] != h || m["conf/a"] != h || len(m) != 2 {
		t.Fatalf("%v %v", m, err)
	}
	for _, bad := range []string{h + " ../x\n", h + "  /abs\n", "zz  a\n", h + "  a\n" + h + "  a\n", h + "a\n", h + "  \n"} {
		if _, err := ParseSHA256SUMS([]byte(bad), Limits{}); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}

// 端到端：ParseSHA256SUMS 的产物直接喂给 SafeExtract。
func TestSafeExtractWithParsedSums(t *testing.T) {
	ents := []tent{f("bin/x", "payload")}
	m, err := ParseSHA256SUMS([]byte(sum("payload")+"  bin/x\n"), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := extractInto(t, buildTar(t, ents...), ExtractOptions{Checksums: m}); err != nil {
		t.Fatal(err)
	}
}
