package security

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// 绝对上限（tools-design §18.1：限额取自清单并设绝对上限）：调用方给出的限额超过这些值时被收紧到这些值，零值取默认。
const (
	AbsMaxFiles      = 200_000
	AbsMaxTotalBytes = int64(8) << 30 // 8 GiB
	AbsMaxFileBytes  = int64(4) << 30 // 4 GiB
	AbsMaxPathLen    = 1024
	AbsMaxDepth      = 32
	maxComponentLen  = 255 // 单个路径段上限（多数文件系统的 NAME_MAX）

	// 每个条目允许的非数据开销（头、填充、PAX 扩展记录），用于推算原始 tar 流的上限。
	perEntryOverhead = 4 << 10
	streamSlack      = 1 << 20
)

// Limits 是解压限额。零值字段取默认（等于绝对上限的保守子集），超过绝对上限的值被收紧。
type Limits struct {
	MaxFiles      int   // 条目总数（含目录）
	MaxTotalBytes int64 // 所有文件内容累计字节
	MaxFileBytes  int64 // 单文件字节
	MaxPathLen    int   // 整条路径长度
	MaxDepth      int   // 路径段数
}

func (l Limits) normalized() Limits {
	clampI := func(v, def, abs int) int {
		if v <= 0 {
			return def
		}
		return min(v, abs)
	}
	clampL := func(v, def, abs int64) int64 {
		if v <= 0 {
			return def
		}
		return min(v, abs)
	}
	return Limits{
		MaxFiles:      clampI(l.MaxFiles, 50_000, AbsMaxFiles),
		MaxTotalBytes: clampL(l.MaxTotalBytes, int64(2)<<30, AbsMaxTotalBytes),
		MaxFileBytes:  clampL(l.MaxFileBytes, int64(1)<<30, AbsMaxFileBytes),
		MaxPathLen:    clampI(l.MaxPathLen, 512, AbsMaxPathLen),
		MaxDepth:      clampI(l.MaxDepth, 16, AbsMaxDepth),
	}
}

// ExtractOptions 是 SafeExtract 的参数。
type ExtractOptions struct {
	Limits Limits
	// Gzip 为 true 时输入是 tar.gz；解压后的 tar 流同样受总量上限约束（防压缩炸弹）。
	Gzip bool
	// Checksums 是已验签的 META/SHA256SUMS 的内容：规范化相对路径 -> 小写十六进制 SHA256。必须非 nil。
	// 归档中的每个普通文件都必须在此声明（否则 ErrExtra），此处声明的每个路径都必须出现在归档中（否则 ErrMissing）。
	Checksums map[string]string
	// Unchecked 是允许出现在归档中但不在 Checksums 里的路径（如 META/SHA256SUMS 及其签名自身）。
	Unchecked []string
	// Executable 是 META 声明的可执行文件路径集合：只有它们得到 0755，其余文件一律 0644（包内的 mode 位被完全忽略）。
	Executable []string
}

// ExtractResult 是成功解压的统计。
type ExtractResult struct {
	Files int   // 普通文件数
	Dirs  int   // 目录条目数（不含隐式创建）
	Bytes int64 // 文件内容累计字节
}

// ParseSHA256SUMS 解析 `sha256sum` 格式（`<64 位十六进制>  <路径>` 或 `<hex> *<路径>`）为规范化路径到哈希的映射。
// 路径按解压同一规则校验；重复路径、坏格式一律报错。
func ParseSHA256SUMS(data []byte, lim Limits) (map[string]string, error) {
	lim = lim.normalized()
	out := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) < 66 || (line[64] != ' ') {
			return nil, fmt.Errorf("SHA256SUMS 第 %d 行格式错误", i+1)
		}
		sum := strings.ToLower(line[:64])
		if !isHex(sum) {
			return nil, fmt.Errorf("SHA256SUMS 第 %d 行哈希不是十六进制", i+1)
		}
		name := line[65:]
		if strings.HasPrefix(name, " ") || strings.HasPrefix(name, "*") {
			name = name[1:]
		}
		clean, err := CleanEntryName(name, false, lim)
		if err != nil {
			return nil, fmt.Errorf("SHA256SUMS 第 %d 行路径不安全: %w", i+1, err)
		}
		if _, dup := out[clean]; dup {
			return nil, fmt.Errorf("SHA256SUMS 第 %d 行路径重复: %q", i+1, clean)
		}
		out[clean] = sum
	}
	return out, nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// CleanEntryName 校验并规范化归档条目名（tools-design §18.1 第 2 条「路径」）。
// 拒绝：空名、非法 UTF-8、NUL/反斜杠/控制字符、绝对路径、Windows 驱动器前缀、百分号编码（`%2e%2e` 之类）、
// 含 `..`/`.`/空段、单段或整条过长、过深。目录名允许尾部 `/`；开头的 `./` 被去掉。
// 返回规范化的相对路径（斜杠分隔）；仅当 name 表示归档根（`.`、`./`）且 isDir 时返回 "."。
func CleanEntryName(name string, isDir bool, lim Limits) (string, error) {
	lim = lim.normalized()
	bad := func(format string, a ...any) (string, error) {
		return "", extractErr(ErrUnsafePath, name, format, a...)
	}
	if name == "" {
		return bad("空路径")
	}
	if !utf8.ValidString(name) {
		return bad("非法 UTF-8")
	}
	if len(name) > lim.MaxPathLen {
		return bad("路径过长（%d > %d）", len(name), lim.MaxPathLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c < 0x20 || c == 0x7f:
			return bad("含控制字符/NUL")
		case c == '\\':
			return bad("含反斜杠")
		case c == '%' && i+2 < len(name) && isHexByte(name[i+1]) && isHexByte(name[i+2]):
			return bad("含百分号编码")
		}
	}
	if name[0] == '/' {
		return bad("绝对路径")
	}
	if len(name) >= 2 && name[1] == ':' && (name[0] >= 'a' && name[0] <= 'z' || name[0] >= 'A' && name[0] <= 'Z') {
		return bad("含驱动器前缀")
	}
	rest := name
	for strings.HasPrefix(rest, "./") {
		rest = rest[2:]
	}
	if rest == "" || rest == "." {
		if !isDir {
			return bad("文件名不能是根")
		}
		return ".", nil
	}
	if strings.HasSuffix(rest, "/") {
		if !isDir {
			return bad("文件名以斜杠结尾")
		}
		rest = rest[:len(rest)-1]
	}
	segs := strings.Split(rest, "/")
	if len(segs) > lim.MaxDepth {
		return bad("路径过深（%d > %d）", len(segs), lim.MaxDepth)
	}
	for _, s := range segs {
		switch {
		case s == "":
			return bad("含空路径段（重复斜杠）")
		case s == "." || s == "..":
			return bad("含 %q 段", s)
		case len(s) > maxComponentLen:
			return bad("路径段过长（%d > %d）", len(s), maxComponentLen)
		}
	}
	return rest, nil
}

func isHexByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// SafeExtract 把 tar（或 tar.gz）流安全解压到 dst（必须尚不存在，其父目录必须存在）。
//
// 过程在同级 dst+".partial" 内完成，全部成功后才 rename 为 dst；任何失败都会整体删除 .partial（调用方映射为退出码 5）。
// 写入使用 os.Root（逐级不跟随符号链接、无法越出根），并叠加词法与 realpath 双重检查（见 tools-design §18.1）。
// 仅允许普通文件与目录；属主交给调用方（Tools 之后按需 chown），权限为 0755/0644，包内 mode 位全部忽略。
func SafeExtract(ctx context.Context, r io.Reader, dst string, opts ExtractOptions) (res ExtractResult, err error) {
	if opts.Checksums == nil {
		return res, extractErr(ErrOptions, "", "必须提供 SHA256SUMS 内容（Checksums 不能为 nil）")
	}
	lim := opts.Limits.normalized()
	// 清单/声明里的路径只需「安全」，深度与长度的收紧只作用于归档条目（过深的清单项自然对不上任何条目，报 ErrMissing）。
	keyLim := Limits{MaxPathLen: AbsMaxPathLen, MaxDepth: AbsMaxDepth}
	unchecked := map[string]bool{}
	for _, p := range opts.Unchecked {
		c, e := CleanEntryName(p, false, keyLim)
		if e != nil {
			return res, extractErr(ErrOptions, p, "Unchecked 路径非法: %v", e)
		}
		unchecked[c] = true
	}
	exec := map[string]bool{}
	for _, p := range opts.Executable {
		c, e := CleanEntryName(p, false, keyLim)
		if e != nil {
			return res, extractErr(ErrOptions, p, "Executable 路径非法: %v", e)
		}
		exec[c] = true
	}
	sums := make(map[string]string, len(opts.Checksums))
	for p, h := range opts.Checksums {
		c, e := CleanEntryName(p, false, keyLim)
		if e != nil {
			return res, extractErr(ErrOptions, p, "Checksums 路径非法: %v", e)
		}
		h = strings.ToLower(h)
		if len(h) != sha256.Size*2 || !isHex(h) {
			return res, extractErr(ErrOptions, p, "Checksums 哈希不是 64 位十六进制")
		}
		sums[c] = h
	}

	dst, err = filepath.Abs(dst)
	if err != nil {
		return res, extractErr(ErrDestination, dst, "%v", err)
	}
	partial := dst + ".partial"
	if st, e := os.Stat(filepath.Dir(dst)); e != nil || !st.IsDir() {
		return res, extractErr(ErrDestination, dst, "父目录不存在")
	}
	for _, p := range []string{dst, partial} {
		if _, e := os.Lstat(p); e == nil {
			return res, extractErr(ErrDestination, p, "已存在")
		} else if !errors.Is(e, fs.ErrNotExist) {
			return res, extractErr(ErrDestination, p, "%v", e)
		}
	}
	if err := os.Mkdir(partial, 0o700); err != nil {
		return res, extractErr(ErrDestination, partial, "%v", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(partial)
			res = ExtractResult{}
		}
	}()
	if err := os.Chmod(partial, 0o755); err != nil {
		return res, extractErr(ErrDestination, partial, "%v", err)
	}
	realPartial, err := filepath.EvalSymlinks(partial)
	if err != nil {
		return res, extractErr(ErrDestination, partial, "%v", err)
	}
	root, err := os.OpenRoot(partial)
	if err != nil {
		return res, extractErr(ErrDestination, partial, "%v", err)
	}
	defer func() { _ = root.Close() }()

	x := &extractor{
		ctx: ctx, lim: lim, root: root, partial: partial, realPartial: realPartial,
		sums: sums, unchecked: unchecked, exec: exec,
		names: map[string]string{}, files: map[string]bool{}, dirsMade: map[string]bool{},
	}
	if err := x.run(r, opts.Gzip); err != nil {
		return res, err
	}
	// 清单声明的内容必须齐全。
	for p := range sums {
		if !x.seen[p] {
			return res, extractErr(ErrMissing, p, "清单声明但归档中不存在")
		}
	}
	for p := range exec {
		if !x.files[strings.ToLower(p)] {
			return res, extractErr(ErrMissing, p, "声明为可执行但归档中不存在")
		}
	}
	if err := root.Close(); err != nil {
		return res, extractErr(ErrDestination, partial, "%v", err)
	}
	if err := os.Rename(partial, dst); err != nil {
		return res, extractErr(ErrDestination, dst, "%v", err)
	}
	cleanup = false
	return x.res, nil
}

type extractor struct {
	ctx         context.Context
	lim         Limits
	root        *os.Root
	partial     string
	realPartial string
	sums        map[string]string
	unchecked   map[string]bool
	exec        map[string]bool

	names    map[string]string // 小写路径 -> 首次出现的原样路径（大小写冲突与重复检测）
	files    map[string]bool   // 小写路径 -> 是普通文件
	dirsMade map[string]bool
	seen     map[string]bool // 已见到的文件（规范化路径）
	res      ExtractResult
	total    int64
}

func (x *extractor) run(r io.Reader, gz bool) error {
	x.seen = map[string]bool{}
	src := r
	if gz {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return extractErr(ErrArchive, "", "gzip 头无效: %v", err)
		}
		defer func() { _ = zr.Close() }()
		src = zr
	}
	// 解压后的 tar 流总量上限：内容总量 + 每个条目的头/填充开销 + 余量。无论头里声明什么，实际读出的字节都受此约束。
	streamCap := x.lim.MaxTotalBytes + int64(x.lim.MaxFiles)*perEntryOverhead + streamSlack
	capped := &capReader{r: src, left: streamCap}
	tr := tar.NewReader(capped)
	entries := 0
	for {
		if err := x.ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, ErrLimit) {
				return extractErr(ErrLimit, "", "解压后的归档流超过上限 %d 字节", streamCap)
			}
			return extractErr(ErrArchive, "", "%v", err)
		}
		entries++
		if entries > x.lim.MaxFiles {
			return extractErr(ErrLimit, hdr.Name, "条目数超过上限 %d", x.lim.MaxFiles)
		}
		if err := x.entry(tr, hdr); err != nil {
			return err
		}
	}
	// 读完剩余流（尾部填充）：既校验 gzip 的 CRC，也让「尾部塞垃圾」同样受上限约束。
	if _, err := io.Copy(io.Discard, capped); err != nil {
		if errors.Is(err, ErrLimit) {
			return extractErr(ErrLimit, "", "归档结束后仍有超量数据（流上限 %d 字节）", streamCap)
		}
		return extractErr(ErrArchive, "", "%v", err)
	}
	return nil
}

func (x *extractor) entry(tr *tar.Reader, hdr *tar.Header) error {
	var isDir bool
	switch hdr.Typeflag {
	case tar.TypeReg:
	case tar.TypeDir:
		isDir = true
	default:
		return extractErr(ErrUnsupportedType, hdr.Name, "类型 %q 不被允许（仅普通文件与目录）", string(hdr.Typeflag))
	}
	// 稀疏文件（PAX 0.0/0.1/1.0 的 GNU.sparse.* 记录）：标准库会把空洞还原成零字节，条目看上去像普通文件。
	// 发行包不允许稀疏（与 IndexTar/ReadMembers 一致，1.28b 补齐）。
	if sparse(hdr) {
		return extractErr(ErrUnsupportedType, hdr.Name, "稀疏文件不被允许")
	}
	name, err := CleanEntryName(hdr.Name, isDir, x.lim)
	if err != nil {
		return err
	}
	if name == "." {
		return nil // 归档根目录条目（tar -C dir .）：根已存在
	}
	if err := x.lexicalCheck(name); err != nil {
		return err
	}
	low := strings.ToLower(name)
	if isDir {
		if prev, ok := x.names[low]; ok {
			if prev != name {
				return extractErr(ErrUnsafePath, name, "与 %q 仅大小写不同", prev)
			}
			if x.files[low] {
				return extractErr(ErrUnsafePath, name, "同名条目既是文件又是目录")
			}
		}
		if err := x.mkdirs(name); err != nil {
			return err
		}
		x.res.Dirs++
		return nil
	}

	// 普通文件。
	if hdr.Size < 0 {
		return extractErr(ErrLimit, name, "声明大小为负")
	}
	if hdr.Size > x.lim.MaxFileBytes {
		return extractErr(ErrLimit, name, "声明大小 %d 超过单文件上限 %d", hdr.Size, x.lim.MaxFileBytes)
	}
	if x.total+hdr.Size > x.lim.MaxTotalBytes {
		return extractErr(ErrLimit, name, "声明大小使累计超过上限 %d", x.lim.MaxTotalBytes)
	}
	want, declared := x.sums[name]
	if !declared && !x.unchecked[name] {
		return extractErr(ErrExtra, name, "清单未声明")
	}
	if prev, ok := x.names[low]; ok {
		if prev != name {
			return extractErr(ErrUnsafePath, name, "与 %q 仅大小写不同", prev)
		}
		return extractErr(ErrUnsafePath, name, "重复条目或与目录同名")
	}
	// 任一祖先已作为文件出现过 → 非法（文件当目录用）。
	for p := parentOf(low); p != ""; p = parentOf(p) {
		if x.files[p] {
			return extractErr(ErrUnsafePath, name, "祖先 %q 是文件", p)
		}
	}
	if dir := parentOf(name); dir != "" {
		if err := x.mkdirs(dir); err != nil {
			return err
		}
	}
	mode := fs.FileMode(0o644)
	if x.exec[name] {
		mode = 0o755
	}
	f, err := x.root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return extractErr(ErrUnsafePath, name, "无法在根内创建: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return extractErr(ErrDestination, name, "chmod: %v", err)
	}
	h := sha256.New()
	lw := &limitWriter{w: io.MultiWriter(f, h), fileLeft: x.lim.MaxFileBytes, totalLeft: x.lim.MaxTotalBytes - x.total}
	n, err := io.Copy(lw, tr)
	if err != nil {
		switch {
		case errors.Is(err, errFileTooBig):
			return extractErr(ErrLimit, name, "实际写入超过单文件上限 %d", x.lim.MaxFileBytes)
		case errors.Is(err, errTotalTooBig):
			return extractErr(ErrLimit, name, "实际写入使累计超过上限 %d", x.lim.MaxTotalBytes)
		case errors.Is(err, ErrLimit):
			return extractErr(ErrLimit, name, "归档流超过上限")
		}
		return extractErr(ErrArchive, name, "%v", err)
	}
	if n != hdr.Size {
		return extractErr(ErrArchive, name, "实际 %d 字节与声明 %d 字节不符", n, hdr.Size)
	}
	if err := f.Close(); err != nil {
		closed = true
		return extractErr(ErrDestination, name, "%v", err)
	}
	closed = true
	if declared {
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			return extractErr(ErrChecksum, name, "期望 %s，实际 %s", want, got)
		}
	}
	// 写后复核：确实是单链接的普通文件（防御性；os.Root 之外的并发篡改在此暴露）。
	if fi, err := x.root.Lstat(filepath.FromSlash(name)); err != nil || !fi.Mode().IsRegular() {
		return extractErr(ErrUnsafePath, name, "写入后不是普通文件")
	} else if nl, ok := linkCount(fi); ok && nl != 1 {
		return extractErr(ErrUnsafePath, name, "写入后硬链接数为 %d", nl)
	}
	x.names[low] = name
	x.files[low] = true
	x.seen[name] = true
	x.total += n
	x.res.Files++
	x.res.Bytes += n
	return nil
}

// lexicalCheck 是第二道防线：把条目名拼到 .partial 上，结果必须仍在其内（防 CleanEntryName 被改坏或绕过）。
func (x *extractor) lexicalCheck(name string) error {
	full := filepath.Join(x.partial, filepath.FromSlash(name))
	rel, err := filepath.Rel(x.partial, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) || rel == "." {
		return extractErr(ErrUnsafePath, name, "落点不在目标根内")
	}
	return nil
}

// mkdirs 逐级创建目录（0755）；每个新建目录都做 realpath 复核（第三道防线）。
func (x *extractor) mkdirs(dir string) error {
	segs := strings.Split(dir, "/")
	cur := ""
	for _, s := range segs {
		if cur == "" {
			cur = s
		} else {
			cur += "/" + s
		}
		if x.dirsMade[cur] {
			continue
		}
		low := strings.ToLower(cur)
		if prev, ok := x.names[low]; ok && prev != cur {
			return extractErr(ErrUnsafePath, cur, "与 %q 仅大小写不同", prev)
		}
		if x.files[low] {
			return extractErr(ErrUnsafePath, cur, "同名条目既是文件又是目录")
		}
		native := filepath.FromSlash(cur)
		if err := x.root.Mkdir(native, 0o700); err != nil {
			return extractErr(ErrUnsafePath, cur, "无法在根内创建目录: %v", err)
		}
		if err := x.root.Chmod(native, 0o755); err != nil {
			return extractErr(ErrDestination, cur, "chmod: %v", err)
		}
		real, err := filepath.EvalSymlinks(filepath.Join(x.partial, native))
		if err != nil || !within(x.realPartial, real) {
			return extractErr(ErrUnsafePath, cur, "目录的 realpath 不在目标根内")
		}
		x.dirsMade[cur] = true
		x.names[low] = cur
	}
	return nil
}

func within(root, p string) bool {
	return p != root && strings.HasPrefix(p, root+string(filepath.Separator))
}

func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// capReader 在读取总量超过 left 时返回 ErrLimit（流式总量上限）。
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, ErrLimit
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, ErrLimit
	}
	return n, err
}

var (
	errFileTooBig  = errors.New("file too big")
	errTotalTooBig = errors.New("total too big")
)

// limitWriter 在实际写入量超过单文件/累计上限时中止（与头里声明的大小无关）。
type limitWriter struct {
	w         io.Writer
	fileLeft  int64
	totalLeft int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.fileLeft {
		return 0, errFileTooBig
	}
	if int64(len(p)) > l.totalLeft {
		return 0, errTotalTooBig
	}
	n, err := l.w.Write(p)
	l.fileLeft -= int64(n)
	l.totalLeft -= int64(n)
	return n, err
}
