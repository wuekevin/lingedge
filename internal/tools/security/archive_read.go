package security

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"
)

// 只读归档助手（Story 1.28）：下载器在「解压前」要读取 META/*（§9.5 第 6 步），并要为离线 bundle 建立索引。
// G-4 规定归档只能在本包内解析（非测试代码不得 import archive/tar），因此这两个入口放在这里，复用同一套路径/类型/限额规则。
// 它们都不向磁盘写任何东西。

// ReadMembersOptions 是 ReadMembers 的参数。
type ReadMembersOptions struct {
	// Want 是需要取出的文件：规范化路径 -> 单文件字节上限（声明与实际流都检查）。必须非空。
	Want map[string]int64
	// Before 非空时：在 Want 的全部文件取齐之前，出现路径以它开头的条目（如 "payload/"）即失败（ErrMissing）——
	// 即规定 META 必须先于 payload，使解压前的读取只需扫过归档的开头，不必为找 META 而解压整个包。
	Before string
	// Gzip 为 true 时输入是 tar.gz。
	Gzip bool
	// Limits 约束扫描阶段读过的条目数与字节总量（零值取默认）；条目数/总量按 SafeExtract 同一公式折算成流上限。
	Limits Limits
}

// ReadMembers 流式扫描 tar（或 tar.gz），取出 Want 中的全部文件内容；取齐后立即停止（不读完归档）。
// 与 SafeExtract 一致：只允许普通文件与目录，路径经 CleanEntryName，Want 中的路径重复出现即失败。
func ReadMembers(r io.Reader, opt ReadMembersOptions) (map[string][]byte, error) {
	if len(opt.Want) == 0 {
		return nil, extractErr(ErrOptions, "", "Want 不能为空")
	}
	lim := opt.Limits.normalized()
	want := make(map[string]int64, len(opt.Want))
	keyLim := Limits{MaxPathLen: AbsMaxPathLen, MaxDepth: AbsMaxDepth}
	for p, max := range opt.Want {
		c, err := CleanEntryName(p, false, keyLim)
		if err != nil {
			return nil, extractErr(ErrOptions, p, "Want 路径非法: %v", err)
		}
		if max < 0 {
			return nil, extractErr(ErrOptions, p, "单文件上限为负")
		}
		want[c] = max
	}
	src := r
	if opt.Gzip {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, extractErr(ErrArchive, "", "gzip 头无效: %v", err)
		}
		defer func() { _ = zr.Close() }()
		src = zr
	}
	streamCap := lim.MaxTotalBytes + int64(lim.MaxFiles)*perEntryOverhead + streamSlack
	tr := tar.NewReader(&capReader{r: src, left: streamCap})
	out := map[string][]byte{}
	entries := 0
	for len(out) < len(want) {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, ErrLimit) {
				return nil, extractErr(ErrLimit, "", "归档开头的扫描超过流上限 %d 字节", streamCap)
			}
			return nil, extractErr(ErrArchive, "", "%v", err)
		}
		entries++
		if entries > lim.MaxFiles {
			return nil, extractErr(ErrLimit, hdr.Name, "扫描的条目数超过上限 %d", lim.MaxFiles)
		}
		isDir := false
		switch hdr.Typeflag {
		case tar.TypeReg:
		case tar.TypeDir:
			isDir = true
		default:
			return nil, extractErr(ErrUnsupportedType, hdr.Name, "类型 %q 不被允许（仅普通文件与目录）", string(hdr.Typeflag))
		}
		if sparse(hdr) {
			return nil, extractErr(ErrUnsupportedType, hdr.Name, "稀疏文件不被允许")
		}
		name, err := CleanEntryName(hdr.Name, isDir, lim)
		if err != nil {
			return nil, err
		}
		if isDir {
			continue
		}
		max, wanted := want[name]
		if !wanted {
			if opt.Before != "" && strings.HasPrefix(name, opt.Before) {
				return nil, extractErr(ErrMissing, name, "在取齐 META 之前出现了 %s 条目：META 必须位于归档前部", opt.Before)
			}
			continue
		}
		if _, dup := out[name]; dup {
			return nil, extractErr(ErrUnsafePath, name, "重复条目")
		}
		if hdr.Size < 0 || hdr.Size > max {
			return nil, extractErr(ErrLimit, name, "声明大小 %d 超过上限 %d", hdr.Size, max)
		}
		// 实际流同样受上限约束（多读 1 字节用于发现「声明小、实际大」）。
		b, err := io.ReadAll(io.LimitReader(tr, max+1))
		if err != nil {
			if errors.Is(err, ErrLimit) {
				return nil, extractErr(ErrLimit, name, "归档流超过上限")
			}
			return nil, extractErr(ErrArchive, name, "%v", err)
		}
		if int64(len(b)) > max || int64(len(b)) != hdr.Size {
			return nil, extractErr(ErrLimit, name, "实际 %d 字节与声明 %d / 上限 %d 不符", len(b), hdr.Size, max)
		}
		out[name] = b
	}
	for p := range want {
		if _, ok := out[p]; !ok {
			return nil, extractErr(ErrMissing, p, "归档中不存在")
		}
	}
	return out, nil
}

// sparse 报告条目是否为（PAX）稀疏文件：其数据流在文件里不连续，不允许进入发行包与 bundle。
func sparse(h *tar.Header) bool {
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// TarEntry 是 IndexTar 的一条结果：规范化名称与（普通文件的）数据在输入流中的起止位置。
type TarEntry struct {
	Name   string
	Dir    bool
	Offset int64 // 文件数据在输入流中的起始偏移（仅普通文件）
	Size   int64
}

// IndexLimits 是 IndexTar 的限额。
type IndexLimits struct {
	MaxEntries     int   // 条目数上限（缺省 4096）
	MaxFileBytes   int64 // 单个文件上限（缺省 AbsMaxTotalBytes）
	MaxArchiveSize int64 // 整个输入流上限（缺省 AbsMaxTotalBytes + 1 GiB）
	MaxPathLen     int
	MaxDepth       int
}

func (l IndexLimits) normalized() IndexLimits {
	if l.MaxEntries <= 0 {
		l.MaxEntries = 4096
	}
	if l.MaxFileBytes <= 0 || l.MaxFileBytes > AbsMaxTotalBytes {
		l.MaxFileBytes = AbsMaxTotalBytes
	}
	if l.MaxArchiveSize <= 0 {
		l.MaxArchiveSize = AbsMaxTotalBytes + int64(1)<<30
	}
	return l
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// IndexTar 顺序扫描一个（未压缩的）tar，返回每个条目的规范化名称、大小与数据偏移，供调用方随后按偏移读取——
// 它不信任输入：只允许普通文件与目录，拒绝链接/设备/FIFO/稀疏/全局 PAX 头，路径经 CleanEntryName，
// 重复与仅大小写不同的名称拒绝，条目数、单文件大小与整个流的大小都受限。
// 偏移基于本函数自己计数的已读字节数（不依赖 Seek），调用方应在读取数据时对内容另行校验（本包不对其内容作任何信任）。
func IndexTar(r io.Reader, lim IndexLimits) ([]TarEntry, error) {
	lim = lim.normalized()
	cr := &countingReader{r: &capReader{r: r, left: lim.MaxArchiveSize}}
	tr := tar.NewReader(cr)
	nl := Limits{MaxPathLen: lim.MaxPathLen, MaxDepth: lim.MaxDepth}
	var out []TarEntry
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, ErrLimit) {
				return nil, extractErr(ErrLimit, "", "归档超过上限 %d 字节", lim.MaxArchiveSize)
			}
			return nil, extractErr(ErrArchive, "", "%v", err)
		}
		if len(out) >= lim.MaxEntries {
			return nil, extractErr(ErrLimit, hdr.Name, "条目数超过上限 %d", lim.MaxEntries)
		}
		var isDir bool
		switch hdr.Typeflag {
		case tar.TypeReg:
		case tar.TypeDir:
			isDir = true
		default:
			return nil, extractErr(ErrUnsupportedType, hdr.Name, "类型 %q 不被允许（仅普通文件与目录）", string(hdr.Typeflag))
		}
		if sparse(hdr) {
			return nil, extractErr(ErrUnsupportedType, hdr.Name, "稀疏文件不被允许")
		}
		name, err := CleanEntryName(hdr.Name, isDir, nl)
		if err != nil {
			return nil, err
		}
		if name == "." {
			continue
		}
		low := strings.ToLower(name)
		if seen[low] {
			return nil, extractErr(ErrUnsafePath, name, "重复条目或仅大小写不同")
		}
		seen[low] = true
		e := TarEntry{Name: name, Dir: isDir}
		if !isDir {
			if hdr.Size < 0 || hdr.Size > lim.MaxFileBytes {
				return nil, extractErr(ErrLimit, name, "声明大小 %d 超过上限 %d", hdr.Size, lim.MaxFileBytes)
			}
			e.Offset, e.Size = cr.n, hdr.Size
			// 跳过数据（同时受流上限约束）；实际可读字节少于声明（截断）时 tar.Reader 报错。
			if n, err := io.Copy(io.Discard, tr); err != nil {
				if errors.Is(err, ErrLimit) {
					return nil, extractErr(ErrLimit, name, "归档超过上限 %d 字节", lim.MaxArchiveSize)
				}
				return nil, extractErr(ErrArchive, name, "%v", err)
			} else if n != hdr.Size {
				return nil, extractErr(ErrArchive, name, "数据 %d 字节与声明 %d 字节不符", n, hdr.Size)
			}
			// 普通文件读完数据后，底层恰好前进了 Size 字节；稀疏文件（数据流不连续，Size 是还原后的逻辑大小）不满足，
			// 它的偏移对调用方没有意义，拒绝。
			if cr.n != e.Offset+hdr.Size {
				return nil, extractErr(ErrUnsupportedType, name, "稀疏文件不被允许")
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// String 便于日志。
func (e TarEntry) String() string { return fmt.Sprintf("%s(%d@%d)", e.Name, e.Size, e.Offset) }
