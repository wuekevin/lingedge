package security

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"time"
)

// 顺序 tar.gz 读写（Story 1.41，备份归档）。
//
// G-4 规定 archive/tar 只能在本包内使用。备份归档是 Tools 自己生成、自己读回的格式（清单在最前、条目名固定形态），不是「发行包解压」，
// 所以不走 SafeExtract（那是把包展开到目录）；但同样的类型/路径规则在这里强制：只接受普通文件、拒绝稀疏/PAX 扩展类型、条目名经 CleanEntryName。
// 条目内容永远不按条目名落盘（备份的恢复目标只来自经校验的清单），本读取器也不写磁盘。

// TarGzWriter 顺序写 tar.gz。
type TarGzWriter struct {
	gz  *gzip.Writer
	tw  *tar.Writer
	now time.Time
}

// NewTarGzWriter 构造写入器；mtime 写入条目头（取调用方的时钟，便于确定性测试）。
func NewTarGzWriter(w io.Writer, mtime time.Time) *TarGzWriter {
	gz := gzip.NewWriter(w)
	return &TarGzWriter{gz: gz, tw: tar.NewWriter(gz), now: mtime.UTC().Truncate(time.Second)}
}

// WriteFile 写一个普通文件条目（0600）。name 须通过 CleanEntryName；size 必须与 r 实际提供的字节数一致，否则报错。
func (t *TarGzWriter) WriteFile(name string, size int64, r io.Reader) error {
	if _, err := CleanEntryName(name, false, Limits{}.normalized()); err != nil {
		return err
	}
	if size < 0 {
		return extractErr(ErrOptions, name, "size 为负")
	}
	if err := t.tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o600, ModTime: t.now, Format: tar.FormatPAX}); err != nil {
		return err
	}
	n, err := io.Copy(t.tw, io.LimitReader(r, size+1))
	if err != nil {
		return err
	}
	if n != size {
		return extractErr(ErrArchive, name, "实际字节数 %d 与声明的 %d 不一致", n, size)
	}
	return nil
}

// Close 写出 tar 与 gzip 的结尾（不关闭底层 writer）。
func (t *TarGzWriter) Close() error {
	if err := t.tw.Close(); err != nil {
		return err
	}
	return t.gz.Close()
}

// TarMember 是读取到的一个条目头。
type TarMember struct {
	Name string
	Size int64
}

// TarGzReader 顺序读 tar.gz：Next 前进到下一个条目，Read 读取当前条目内容。
type TarGzReader struct {
	gz *gzip.Reader
	tr *tar.Reader
}

// NewTarGzReader 读取 gzip 头并返回读取器。
func NewTarGzReader(r io.Reader) (*TarGzReader, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, extractErr(ErrArchive, "", "不是 gzip 流: %v", err)
	}
	return &TarGzReader{gz: gz, tr: tar.NewReader(gz)}, nil
}

// Next 前进到下一个条目；没有更多条目返回 io.EOF。只接受普通文件与合规名称；稀疏/其他类型返回 ErrUnsupportedType，坏名称返回 ErrUnsafePath。
func (t *TarGzReader) Next() (TarMember, error) {
	h, err := t.tr.Next()
	if errors.Is(err, io.EOF) {
		return TarMember{}, io.EOF
	}
	if err != nil {
		return TarMember{}, extractErr(ErrArchive, "", "读取条目头失败: %v", err)
	}
	if h.Typeflag != tar.TypeReg || sparse(h) {
		return TarMember{}, extractErr(ErrUnsupportedType, h.Name, "类型 %c 不允许", h.Typeflag)
	}
	name, err := CleanEntryName(h.Name, false, Limits{}.normalized())
	if err != nil {
		return TarMember{}, err
	}
	if name != h.Name {
		return TarMember{}, extractErr(ErrUnsafePath, h.Name, "条目名不是规范形式")
	}
	if h.Size < 0 {
		return TarMember{}, extractErr(ErrArchive, h.Name, "size 为负")
	}
	return TarMember{Name: h.Name, Size: h.Size}, nil
}

// Read 读取当前条目的内容。
func (t *TarGzReader) Read(p []byte) (int, error) {
	n, err := t.tr.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, extractErr(ErrArchive, "", "读取条目内容失败: %v", err)
	}
	return n, err
}

// DrainTail 读完 tar 的结束块与 gzip 的校验和，并确认之后没有任何非零尾部数据（最多检查 limit 字节）。
func (t *TarGzReader) DrainTail(limit int64) error {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, err := t.gz.Read(buf)
		for _, b := range buf[:n] {
			if b != 0 {
				return extractErr(ErrArchive, "", "尾部有多余数据")
			}
		}
		total += int64(n)
		if total > limit {
			return extractErr(ErrLimit, "", "尾部数据超过 %d 字节", limit)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrArchive, err)
		}
	}
}
