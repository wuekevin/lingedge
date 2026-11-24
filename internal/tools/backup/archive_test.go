package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type fixture struct {
	m     Manifest
	files []StagedFile
}

// newFixture 生成一个 server 清单（不含数据库条目）与两个暂存文件。
func newFixture(t *testing.T, encrypted bool) fixture {
	t.Helper()
	dir := t.TempDir()
	mk := func(name string, content []byte) (string, string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
		return p, sum(content)
	}
	a, b := []byte("listen: 127.0.0.1:8080\n"), bytes.Repeat([]byte("data"), 5000)
	pa, sa := mk("a", a)
	pb, sb := mk("b", b)
	m := Manifest{FormatVersion: FormatVersion, Purpose: PurposeManual, Component: "server", Version: "1.0.0", Host: "edge-1", CreatedAt: time.Now().UTC().Format(time.RFC3339),
		ToolsVersion: "t", KEKFingerprints: map[string]string{}, KEKIDsRequired: []string{}, Encrypted: encrypted,
		Files: []FileEntry{
			{Archive: "files/000001", Dest: "/etc/lingedge/server/server.yaml", Kind: KindConfig, Size: int64(len(a)), SHA256: sa, Mode: 0o640},
			{Archive: "files/000002", Dest: "/var/lib/lingedge/server/blob", Kind: KindData, Size: int64(len(b)), SHA256: sb, Mode: 0o600},
		}}
	return fixture{m: m, files: []StagedFile{{Entry: m.Files[0], Path: pa}, {Entry: m.Files[1], Path: pb}}}
}

func writeFixture(t *testing.T, f fixture, pass string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "x.tar.gz")
	o := WriteOptions{KDF: testKDF, Chunk: 2048}
	if pass != "" {
		o.Passphrase = []byte(pass)
	}
	if err := WriteArchive(out, f.m, f.files, o); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestArchiveRoundTripPlainAndEncrypted(t *testing.T) {
	for _, pass := range []string{"", testPass} {
		f := newFixture(t, pass != "")
		out := writeFixture(t, f, pass)
		if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
			t.Fatalf("备份文件应为 0600，实际 %v", fi.Mode().Perm())
		}
		got := map[string][]byte{}
		ar, err := ReadArchive(out, []byte(pass), func(e FileEntry) (io.WriteCloser, error) { return &memSink{m: got, k: e.Archive}, nil })
		if err != nil {
			t.Fatalf("pass=%q: %v", pass, err)
		}
		if ar.Encrypted != (pass != "") || len(got) != 2 || sum(got["files/000001"]) != f.m.Files[0].SHA256 {
			t.Fatalf("pass=%q: 读回内容不符 %v", pass, ar)
		}
		if _, err := ReadManifest(out, []byte(pass)); err != nil {
			t.Fatal(err)
		}
		if pass != "" {
			if _, err := ReadArchive(out, nil, nil); !errors.Is(err, ErrPassphraseRequired) {
				t.Fatalf("加密备份无口令应为 ErrPassphraseRequired: %v", err)
			}
			if _, err := ReadArchive(out, []byte("some other pass!!"), nil); !errors.Is(err, ErrBadPassphrase) {
				t.Fatalf("错误口令: %v", err)
			}
		}
	}
}

type memSink struct {
	m map[string][]byte
	k string
	b bytes.Buffer
}

func (s *memSink) Write(p []byte) (int, error) { return s.b.Write(p) }
func (s *memSink) Close() error                { s.m[s.k] = s.b.Bytes(); return nil }

// 篡改未加密归档：压缩流里任意位置被翻转 → 必须报错（gzip CRC / tar / 哈希三层之一）。
func TestPlainArchiveTamperDetected(t *testing.T) {
	f := newFixture(t, false)
	out := writeFixture(t, f, "")
	raw, _ := os.ReadFile(out)
	step := len(raw) / 97
	if step < 1 {
		step = 1
	}
	for i := 0; i < len(raw); i += step {
		if i >= 4 && i <= 9 {
			continue // gzip 头的 MTIME/XFL/OS 字段不在完整性保护范围内（对内容无影响）
		}
		bad := append([]byte(nil), raw...)
		bad[i] ^= 0xff
		p := filepath.Join(t.TempDir(), "bad.tar.gz")
		_ = os.WriteFile(p, bad, 0o600)
		if _, err := ReadArchive(p, nil, nil); err == nil {
			t.Fatalf("翻转第 %d 字节未被发现", i)
		}
	}
	if _, err := ReadArchive(out, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// 手工构造 tar.gz：用于「清单之外的条目」「符号链接」「大小/哈希与清单不符」「缺少条目」等结构攻击。
func craft(t *testing.T, m Manifest, mutate func(tw *tar.Writer, m Manifest)) string {
	t.Helper()
	mb, err := MarshalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: ManifestName, Size: int64(len(mb)), Mode: 0o600})
	_, _ = tw.Write(mb)
	mutate(tw, m)
	_ = tw.Close()
	_ = gz.Close()
	p := filepath.Join(t.TempDir(), "c.tar.gz")
	_ = os.WriteFile(p, buf.Bytes(), 0o600)
	return p
}

func TestArchiveStructuralAttacksRejected(t *testing.T) {
	f := newFixture(t, false)
	body := func(i int) []byte { b, _ := os.ReadFile(f.files[i].Path); return b }
	put := func(tw *tar.Writer, name string, b []byte) {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(b)), Mode: 0o600})
		_, _ = tw.Write(b)
	}
	cases := map[string]struct {
		mutate func(tw *tar.Writer, m Manifest)
		want   error
	}{
		"清单之外的条目": {func(tw *tar.Writer, m Manifest) {
			put(tw, "files/000001", body(0))
			put(tw, "files/000002", body(1))
			put(tw, "../../etc/passwd", []byte("x"))
		}, ErrBadArchive},
		"清单之外的条目（名称合规）": {func(tw *tar.Writer, m Manifest) {
			put(tw, "files/000001", body(0))
			put(tw, "files/000002", body(1))
			put(tw, "files/000009", []byte("smuggled"))
		}, ErrBadArchive},
		"缺少条目": {func(tw *tar.Writer, m Manifest) { put(tw, "files/000001", body(0)) }, ErrBadArchive},
		"重复条目": {func(tw *tar.Writer, m Manifest) {
			put(tw, "files/000001", body(0))
			put(tw, "files/000001", body(0))
			put(tw, "files/000002", body(1))
		}, ErrBadArchive},
		"内容被换（大小相同）": {func(tw *tar.Writer, m Manifest) {
			put(tw, "files/000001", bytes.Repeat([]byte("x"), len(body(0))))
			put(tw, "files/000002", body(1))
		}, ErrHashMismatch},
		"大小与清单不符": {func(tw *tar.Writer, m Manifest) {
			put(tw, "files/000001", append(body(0), 'x'))
			put(tw, "files/000002", body(1))
		}, ErrHashMismatch},
		"符号链接条目": {func(tw *tar.Writer, m Manifest) {
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "files/000001", Linkname: "/etc/shadow", Mode: 0o600})
			put(tw, "files/000002", body(1))
		}, ErrBadArchive},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := craft(t, f.m, c.mutate)
			if _, err := ReadArchive(p, nil, nil); !errors.Is(err, c.want) {
				t.Fatalf("期望 %v，得到 %v", c.want, err)
			}
		})
	}
	// 清单不是第一个条目。
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	put(tw, "files/000001", body(0))
	_ = tw.Close()
	_ = gz.Close()
	p := filepath.Join(t.TempDir(), "n.tar.gz")
	_ = os.WriteFile(p, buf.Bytes(), 0o600)
	if _, err := ReadArchive(p, nil, nil); !errors.Is(err, ErrBadArchive) {
		t.Fatalf("清单须是第一个条目: %v", err)
	}
	if _, err := ReadArchive(filepath.Join(t.TempDir(), "missing"), nil, nil); err == nil {
		t.Fatal("文件不存在应报错")
	}
}

func TestManifestValidation(t *testing.T) {
	base := newFixture(t, false).m
	bad := func(name string, mut func(m *Manifest), encrypted bool) {
		t.Helper()
		m := base
		m.Files = append([]FileEntry(nil), base.Files...)
		m.Encrypted = encrypted
		mut(&m)
		if err := m.Validate(); !errors.Is(err, ErrBadManifest) {
			t.Errorf("%s: 应被拒绝，得到 %v", name, err)
		}
	}
	bad("目标路径越界（/etc/passwd）", func(m *Manifest) { m.Files[0].Dest = "/etc/passwd" }, false)
	bad("目标路径含 ..", func(m *Manifest) { m.Files[0].Dest = "/etc/lingedge/server/../../shadow" }, false)
	bad("目标是 node 的目录却属 server", func(m *Manifest) { m.Files[0].Dest = "/etc/lingedge/node/agent.yaml" }, false)
	bad("相对路径", func(m *Manifest) { m.Files[0].Dest = "etc/lingedge/server/x" }, false)
	bad("控制字符", func(m *Manifest) { m.Files[0].Dest = "/etc/lingedge/server/x\ny" }, false)
	bad("包内路径不合规", func(m *Manifest) { m.Files[0].Archive = "../x" }, false)
	bad("包内路径重复", func(m *Manifest) { m.Files[1].Archive = m.Files[0].Archive }, false)
	bad("目标重复", func(m *Manifest) { m.Files[1].Dest = m.Files[0].Dest }, false)
	bad("sha 非法", func(m *Manifest) { m.Files[0].SHA256 = "xyz" }, false)
	bad("权限越界", func(m *Manifest) { m.Files[0].Mode = 0o4755 }, false)
	bad("未加密备份含主密钥", func(m *Manifest) {
		m.Files = append(m.Files, FileEntry{Archive: "files/000003", Dest: "/etc/lingedge/server/master.key", Kind: KindMasterKey, SHA256: sum(nil), Mode: 0o400})
		m.MasterKeyIncluded = true
	}, false)
	bad("include 标志与条目不一致", func(m *Manifest) { m.MasterKeyIncluded = true }, true)
	bad("未知类别", func(m *Manifest) { m.Files[0].Kind = "weird" }, false)
	bad("component 非法", func(m *Manifest) { m.Component = "dns" }, false)
	bad("purpose 非法", func(m *Manifest) { m.Purpose = "other" }, false)
	bad("kek_ids_required 未排序", func(m *Manifest) { m.KEKIDsRequired = []string{"k2", "k1"} }, false)
	bad("kek_id 非法", func(m *Manifest) { m.KEKIDsRequired = []string{"bad id!"} }, false)
	if err := base.Validate(); err != nil {
		t.Fatalf("基线清单应合法: %v", err)
	}
	if _, err := ParseManifest([]byte(`{"format_version":1,"bogus":true}`)); !errors.Is(err, ErrBadManifest) {
		t.Errorf("未知字段应被拒绝: %v", err)
	}
	if _, err := ParseManifest([]byte(strings.Repeat(" ", maxManifestBytes+1))); !errors.Is(err, ErrBadManifest) {
		t.Errorf("过大的清单应被拒绝: %v", err)
	}
	mb, _ := MarshalManifest(base)
	if _, err := ParseManifest(append(mb, []byte(" {}")...)); !errors.Is(err, ErrBadManifest) {
		t.Errorf("尾部数据应被拒绝: %v", err)
	}
}

// WriteArchive 对写出去的清单做与读取方相同的校验；暂存文件在写入期间被改动 → 失败并删除半成品。
func TestWriteArchiveRefusesInvalidAndChangedInput(t *testing.T) {
	f := newFixture(t, false)
	f.m.Files[0].Dest = "/etc/shadow"
	out := filepath.Join(t.TempDir(), "o.tar.gz")
	if err := WriteArchive(out, f.m, f.files, WriteOptions{}); !errors.Is(err, ErrBadManifest) {
		t.Fatalf("非法清单不应被写出: %v", err)
	}
	f = newFixture(t, false)
	_ = os.WriteFile(f.files[1].Path, []byte("changed while staging"), 0o600)
	if err := WriteArchive(out, f.m, f.files, WriteOptions{}); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("暂存文件被改动应失败: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("失败后不应留下半成品")
	}
	f = newFixture(t, false)
	if err := WriteArchive(out, f.m, f.files[:1], WriteOptions{}); !errors.Is(err, ErrBadArchive) {
		t.Fatalf("暂存文件缺失应失败: %v", err)
	}
	// 已存在的文件不被覆盖（O_EXCL）。
	f = newFixture(t, false)
	_ = os.WriteFile(out, []byte("existing"), 0o600)
	if err := WriteArchive(out, f.m, f.files, WriteOptions{}); err == nil {
		t.Fatal("不得覆盖已存在的文件")
	}
	if b, _ := os.ReadFile(out); string(b) != "existing" {
		t.Fatal("已存在的文件被改动")
	}
	_ = fmt.Sprint()
}
