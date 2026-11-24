package security

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func buildStreamTar(t *testing.T, hdrs []tar.Header, bodies []string, trailer []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := range hdrs {
		h := hdrs[i]
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(bodies[i]))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(bodies[i]))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return append(buf.Bytes(), trailer...)
}

func TestTarGzRoundTripAndTail(t *testing.T) {
	var buf bytes.Buffer
	w := NewTarGzWriter(&buf, time.Now())
	if err := w.WriteFile("BACKUP.json", 2, strings.NewReader("{}")); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFile("files/000001", 5, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewTarGzReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		name, body string
	}{{"BACKUP.json", "{}"}, {"files/000001", "hello"}} {
		m, err := r.Next()
		if err != nil || m.Name != want.name || m.Size != int64(len(want.body)) {
			t.Fatalf("Next: %+v %v", m, err)
		}
		b, _ := io.ReadAll(r)
		if string(b) != want.body {
			t.Fatalf("内容 %q", b)
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("应结束: %v", err)
	}
	if err := r.DrainTail(1 << 20); err != nil {
		t.Fatalf("干净的尾部: %v", err)
	}
	// 写入器：size 与实际不符、非法名称。
	if err := NewTarGzWriter(io.Discard, time.Now()).WriteFile("files/000001", 10, strings.NewReader("short")); !errors.Is(err, ErrArchive) {
		t.Errorf("size 不符: %v", err)
	}
	if err := NewTarGzWriter(io.Discard, time.Now()).WriteFile("../x", 1, strings.NewReader("x")); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("路径穿越: %v", err)
	}
}

func TestTarGzReaderRejectsUnsafeMembers(t *testing.T) {
	cases := map[string]struct {
		h    tar.Header
		want error
	}{
		"符号链接":  {tar.Header{Typeflag: tar.TypeSymlink, Name: "files/000001", Linkname: "/etc/shadow"}, ErrUnsupportedType},
		"硬链接":   {tar.Header{Typeflag: tar.TypeLink, Name: "files/000001", Linkname: "x"}, ErrUnsupportedType},
		"目录":    {tar.Header{Typeflag: tar.TypeDir, Name: "files/"}, ErrUnsupportedType},
		"设备":    {tar.Header{Typeflag: tar.TypeChar, Name: "files/000001"}, ErrUnsupportedType},
		"路径穿越":  {tar.Header{Typeflag: tar.TypeReg, Name: "../../etc/passwd"}, ErrUnsafePath},
		"绝对路径":  {tar.Header{Typeflag: tar.TypeReg, Name: "/etc/passwd"}, ErrUnsafePath},
		"反斜杠":   {tar.Header{Typeflag: tar.TypeReg, Name: `files\000001`}, ErrUnsafePath},
		"非规范形式": {tar.Header{Typeflag: tar.TypeReg, Name: "files//000001"}, ErrUnsafePath},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r, err := NewTarGzReader(bytes.NewReader(buildStreamTar(t, []tar.Header{c.h}, []string{"x"}, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Next(); !errors.Is(err, c.want) {
				t.Fatalf("期望 %v，得到 %v", c.want, err)
			}
		})
	}
	if _, err := NewTarGzReader(strings.NewReader("not gzip")); !errors.Is(err, ErrArchive) {
		t.Errorf("非 gzip: %v", err)
	}
	// 尾部多余数据（非零）与超限。
	r, _ := NewTarGzReader(bytes.NewReader(buildStreamTar(t, []tar.Header{{Typeflag: tar.TypeReg, Name: "a"}}, []string{"x"}, []byte("garbage"))))
	_, _ = r.Next()
	_, _ = io.ReadAll(r)
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("%v", err)
	}
	if err := r.DrainTail(1 << 20); err == nil {
		t.Error("尾部垃圾应被发现")
	}
}
