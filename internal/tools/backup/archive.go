package backup

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// 归档格式：tar.gz；包内固定形态（BACKUP.json 在最前，其后是 files/NNNNNN 与 db/dump.sql），加密时整体（gzip 流）套 AES-256-GCM 分块容器。
// 条目名与恢复目标路径解耦（目标只在清单里，经 validDest 约束），因此归档本身不可能通过条目名做路径穿越。

// 归档错误。
var (
	ErrBadArchive         = errors.New("backup: 备份结构不合法")
	ErrHashMismatch       = errors.New("backup: 备份内容哈希与清单不符")
	ErrPassphraseRequired = errors.New("backup: 这是加密备份，需要口令（--passphrase-file 或交互输入）")
)

// WriteOptions 是写归档的参数。
type WriteOptions struct {
	// Passphrase 非空则加密（KDF 取 KDF）。
	Passphrase []byte
	KDF        KDFParams
	Chunk      uint32
}

// StagedFile 是已暂存、已哈希的文件：Path 是暂存区里的文件（内容不会再变），Entry.SHA256/Size 与之一致。
type StagedFile struct {
	Entry FileEntry
	Path  string
}

// WriteArchive 创建 out（O_EXCL，0600）并写入归档。files 须与 m.Files 一一对应（按 Archive 名）。任何失败都删除 out。
func WriteArchive(out string, m Manifest, files []StagedFile, o WriteOptions) (err error) {
	m.Encrypted = len(o.Passphrase) > 0
	m.FormatVersion = FormatVersion
	mb, err := MarshalManifest(m)
	if err != nil {
		return err
	}
	// 再解析一次：写出去的清单必须通过与读取方相同的校验（生产者与消费者不会各说各话）。
	if _, err := ParseManifest(mb); err != nil {
		return err
	}
	byName := map[string]StagedFile{}
	for _, f := range files {
		byName[f.Entry.Archive] = f
	}
	if len(byName) != len(m.Files) {
		return fmt.Errorf("%w: 暂存文件与清单不一致", ErrBadArchive)
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(out)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	var sink io.Writer = bw
	var ew *EncryptWriter
	if m.Encrypted {
		meta, merr := json.Marshal(m.Meta())
		if merr != nil {
			return merr
		}
		kdf := o.KDF
		if kdf == (KDFParams{}) {
			kdf = DefaultKDF
		}
		if ew, err = NewEncryptWriter(bw, o.Passphrase, kdf, meta, o.Chunk); err != nil {
			return err
		}
		sink = ew
	}
	tw := security.NewTarGzWriter(sink, time.Now())
	if err := tw.WriteFile(ManifestName, int64(len(mb)), bytes.NewReader(mb)); err != nil {
		return err
	}
	for _, e := range m.Files {
		sf := byName[e.Archive]
		if sf.Path == "" {
			return fmt.Errorf("%w: 缺少暂存文件 %s", ErrBadArchive, e.Archive)
		}
		src, err := os.Open(sf.Path)
		if err != nil {
			return err
		}
		h := sha256.New()
		werr := tw.WriteFile(e.Archive, e.Size, io.TeeReader(src, h))
		_ = src.Close()
		if werr != nil {
			if errors.Is(werr, security.ErrArchive) {
				return fmt.Errorf("%w: 暂存文件 %s 在写入期间被改动", ErrHashMismatch, e.Archive)
			}
			return werr
		}
		if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
			return fmt.Errorf("%w: 暂存文件 %s 在写入期间被改动", ErrHashMismatch, e.Archive)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if ew != nil {
		if err := ew.Close(); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// Archive 是读取结果：清单与容器形态。
type Archive struct {
	Manifest  Manifest
	Encrypted bool
}

// Sink 为归档里的每个条目提供内容的去处；返回 nil writer 表示丢弃（只校验）。
type Sink func(e FileEntry) (io.WriteCloser, error)

// openStream 打开文件并剥掉加密层，返回明文（gzip 字节）流。
func openStream(path string, pass []byte) (io.Reader, io.Closer, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	magic, _ := br.Peek(len(encMagic))
	if string(magic) == encMagic {
		if len(pass) == 0 {
			_ = f.Close()
			return nil, nil, true, ErrPassphraseRequired
		}
		dr, _, derr := NewDecryptReader(br, pass)
		if derr != nil {
			_ = f.Close()
			return nil, nil, true, derr
		}
		return dr, f, true, nil
	}
	return br, f, false, nil
}

func readManifestEntry(tr *security.TarGzReader) (Manifest, error) {
	mem, err := tr.Next()
	if err != nil {
		return Manifest{}, mapStreamErr(err, fmt.Sprintf("读不到 %s", ManifestName))
	}
	if mem.Name != ManifestName || mem.Size <= 0 || mem.Size > maxManifestBytes {
		return Manifest{}, fmt.Errorf("%w: 第一个条目必须是 %s", ErrBadArchive, ManifestName)
	}
	b, err := io.ReadAll(io.LimitReader(tr, mem.Size+1))
	if err != nil || int64(len(b)) != mem.Size {
		return Manifest{}, fmt.Errorf("%w: 清单读取失败", ErrBadArchive)
	}
	return ParseManifest(b)
}

// ReadManifest 只读取清单（不校验内容）：未加密备份直接解析；加密备份需要口令。
func ReadManifest(path string, pass []byte) (Archive, error) {
	r, c, enc, err := openStream(path, pass)
	if err != nil {
		return Archive{Encrypted: enc}, err
	}
	defer func() { _ = c.Close() }()
	tr, err := security.NewTarGzReader(r)
	if err != nil {
		return Archive{}, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	m, err := readManifestEntry(tr)
	if err != nil {
		return Archive{}, err
	}
	if m.Encrypted != enc {
		return Archive{}, fmt.Errorf("%w: 清单的 encrypted 标志与容器形态不符", ErrBadArchive)
	}
	return Archive{Manifest: m, Encrypted: enc}, nil
}

// ReadArchive 完整读取并校验归档：结构（只允许清单声明的条目、普通文件、大小一致）、每个条目的 SHA256；条目内容流给 sink。
// 任何一处不符返回错误（sink 已写出的内容由调用方丢弃：先写暂存区、校验通过后才使用）。
// tar 的解析与条目类型/名称规则在 internal/tools/security（G-4）。
func ReadArchive(path string, pass []byte, sink Sink) (Archive, error) {
	r, c, enc, err := openStream(path, pass)
	if err != nil {
		return Archive{Encrypted: enc}, err
	}
	defer func() { _ = c.Close() }()
	tr, err := security.NewTarGzReader(r)
	if err != nil {
		return Archive{}, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	m, err := readManifestEntry(tr)
	if err != nil {
		return Archive{}, err
	}
	if m.Encrypted != enc {
		return Archive{}, fmt.Errorf("%w: 清单的 encrypted 标志与容器形态不符", ErrBadArchive)
	}
	want := map[string]FileEntry{}
	for _, e := range m.Files {
		want[e.Archive] = e
	}
	seen := map[string]bool{}
	for {
		mem, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Archive{}, mapStreamErr(err, "读取条目失败")
		}
		e, ok := want[mem.Name]
		switch {
		case !ok:
			return Archive{}, fmt.Errorf("%w: 清单之外的条目 %q", ErrBadArchive, truncName(mem.Name))
		case seen[mem.Name]:
			return Archive{}, fmt.Errorf("%w: 重复条目 %q", ErrBadArchive, mem.Name)
		case mem.Size != e.Size:
			return Archive{}, fmt.Errorf("%w: 条目 %q 大小与清单不符", ErrHashMismatch, mem.Name)
		}
		seen[mem.Name] = true
		var dst io.WriteCloser
		if sink != nil {
			if dst, err = sink(e); err != nil {
				return Archive{}, err
			}
		}
		h := sha256.New()
		var w io.Writer = h
		if dst != nil {
			w = io.MultiWriter(h, dst)
		}
		n, cerr := io.Copy(w, io.LimitReader(tr, e.Size+1))
		if dst != nil {
			if clerr := dst.Close(); cerr == nil {
				cerr = clerr
			}
		}
		if cerr != nil {
			return Archive{}, mapStreamErr(cerr, "读取条目内容失败")
		}
		if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
			return Archive{}, fmt.Errorf("%w: %s", ErrHashMismatch, e.Archive)
		}
	}
	for name := range want {
		if !seen[name] {
			return Archive{}, fmt.Errorf("%w: 缺少条目 %s", ErrBadArchive, name)
		}
	}
	// 排空尾部（tar 结束块、gzip 校验和；加密容器的 final 块与「无多余数据」在此被检查）。
	if err := tr.DrainTail(1 << 20); err != nil {
		return Archive{}, mapStreamErr(err, "归档尾部不合法")
	}
	return Archive{Manifest: m, Encrypted: enc}, nil
}

// mapStreamErr 把底层流错误映射到本包的错误类别：加密层的认证/截断错误原样保留；归档结构错误（含 security 的类型/路径/限额）统一为 ErrBadArchive。
func mapStreamErr(err error, what string) error {
	switch {
	case errors.Is(err, ErrCorrupt), errors.Is(err, ErrBadPassphrase), errors.Is(err, ErrHashMismatch), errors.Is(err, ErrBadArchive):
		return err
	}
	return fmt.Errorf("%w: %s: %v", ErrBadArchive, what, err)
}

func truncName(s string) string {
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	return fmt.Sprintf("%.64s", s)
}
