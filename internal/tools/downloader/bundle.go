package downloader

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// 离线 bundle（tools-design §9.4）：外层 tar 含 manifest.json、manifest.json.sig、packages/<artifact-filename>…
// bundle 本身不被信任：这里只负责安全地「定位」其中的字节；真正的信任来自对这些字节走的同一条验证链（Acquire）。

const (
	bundleManifestMax = 8 << 20 // 与 manifest 包的 maxManifestBytes 一致
	bundleSigMax      = 4 << 10 // 与 manifest.ParseSig 的上限一致
	bundlePrefixDir   = "offline-bundle"
)

var reArtifactFile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,199}$`)

// ArtifactFileName 返回清单 URL 的末段文件名（bundle 里 packages/ 下的文件名）。字符集受限，拒绝 `..`/编码/空名。
func ArtifactFileName(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" || strings.HasSuffix(u.Path, "/") {
		return "", newErr(KindBundle, "无法从 URL 得到包文件名")
	}
	name := path.Base(u.Path)
	if name == "." || name == ".." || !reArtifactFile.MatchString(name) || strings.Contains(name, "..") {
		return "", newErr(KindBundle, "包文件名 %q 不合法", trim(name, 64))
	}
	return name, nil
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ResolveBundlePath 校验 --offline 给出的路径：经 realpath 解析后必须是普通文件；
// 路径末级本身是符号链接一律拒绝（符号链接逃逸）；目录、设备、FIFO、套接字拒绝；文件不存在是用法错误（KindBundleAbsent）。
// 返回 realpath。注意校验与随后的打开之间存在竞态，OpenBundle 在打开后用 fstat 再次核对。
func ResolveBundlePath(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", newErr(KindBundle, "bundle 路径为空或含 NUL")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", wrapErr(KindBundle, err, "bundle 路径无法解析")
	}
	lst, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", wrapErr(KindBundleAbsent, err, "bundle 文件不存在")
		}
		return "", wrapErr(KindBundle, err, "无法读取 bundle")
	}
	if lst.Mode()&fs.ModeSymlink != 0 {
		return "", newErr(KindBundle, "bundle 路径是符号链接：拒绝（请直接传入真实文件）")
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", wrapErr(KindBundle, err, "bundle 路径的 realpath 解析失败")
	}
	st, err := os.Lstat(real)
	if err != nil {
		return "", wrapErr(KindBundle, err, "无法读取 bundle")
	}
	if !st.Mode().IsRegular() {
		return "", newErr(KindBundle, "bundle 不是普通文件（%s）", kindOf(st.Mode()))
	}
	return real, nil
}

func kindOf(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "目录"
	case m&fs.ModeDevice != 0:
		return "设备"
	case m&fs.ModeNamedPipe != 0:
		return "FIFO"
	case m&fs.ModeSocket != 0:
		return "套接字"
	case m&fs.ModeSymlink != 0:
		return "符号链接"
	}
	return "其他"
}

// Layout 是 bundle 索引按 §9.4 布局解析的结果。
type Layout struct {
	Manifest security.TarEntry
	Sig      security.TarEntry
	Packages map[string]security.TarEntry // 文件名 -> 条目
}

// ParseBundleLayout 把 tar 索引按 §9.4 布局解析：条目只能是 manifest.json、manifest.json.sig、packages/<文件名>
// （整体可选地位于单一前缀目录 offline-bundle/ 下），及这些路径的目录条目。其他任何条目、重复、缺失清单/签名都拒绝。
// 这是外部可控输入的解析入口（有 fuzz）。
func ParseBundleLayout(entries []security.TarEntry) (*Layout, error) {
	prefix := ""
	for _, e := range entries {
		if !e.Dir && (e.Name == bundlePrefixDir+"/manifest.json" || e.Name == bundlePrefixDir+"/manifest.json.sig") {
			prefix = bundlePrefixDir + "/"
		}
	}
	l := &Layout{Packages: map[string]security.TarEntry{}}
	var haveM, haveS bool
	for _, e := range entries {
		rel := e.Name
		if e.Dir {
			if rel == bundlePrefixDir && prefix != "" || rel == strings.TrimSuffix(prefix, "/")+"/packages" && prefix != "" || rel == "packages" && prefix == "" {
				continue
			}
			return nil, newErr(KindBundle, "bundle 含非法目录条目 %q", trim(rel, 64))
		}
		if prefix != "" {
			if !strings.HasPrefix(rel, prefix) {
				return nil, newErr(KindBundle, "bundle 含布局之外的条目 %q", trim(rel, 64))
			}
			rel = strings.TrimPrefix(rel, prefix)
		}
		switch {
		case rel == "manifest.json":
			if e.Size < 1 || e.Size > bundleManifestMax {
				return nil, newErr(KindBundle, "bundle 内 manifest.json 大小非法（%d）", e.Size)
			}
			l.Manifest, haveM = e, true
		case rel == "manifest.json.sig":
			if e.Size < 1 || e.Size > bundleSigMax {
				return nil, newErr(KindBundle, "bundle 内 manifest.json.sig 大小非法（%d）", e.Size)
			}
			l.Sig, haveS = e, true
		case strings.HasPrefix(rel, "packages/") && !strings.Contains(rel[len("packages/"):], "/"):
			name := rel[len("packages/"):]
			if !reArtifactFile.MatchString(name) || strings.Contains(name, "..") {
				return nil, newErr(KindBundle, "bundle 内包文件名 %q 不合法", trim(name, 64))
			}
			l.Packages[name] = e
		default:
			return nil, newErr(KindBundle, "bundle 含布局之外的条目 %q", trim(e.Name, 64))
		}
	}
	if !haveM || !haveS {
		return nil, newErr(KindBundle, "bundle 缺少 manifest.json 或 manifest.json.sig")
	}
	return l, nil
}

// Bundle 是已打开并建立索引的离线 bundle。
type Bundle struct {
	f      *os.File
	Path   string // realpath
	Layout *Layout
}

// OpenBundle 校验路径（ResolveBundlePath）、以 O_NOFOLLOW|O_NONBLOCK 打开并 fstat 复核仍是同一个普通文件，随后建立受限索引。
// 该函数不联网。
func OpenBundle(p string) (*Bundle, error) {
	real, err := ResolveBundlePath(p)
	if err != nil {
		return nil, err
	}
	lst, err := os.Lstat(real)
	if err != nil {
		return nil, wrapErr(KindBundle, err, "无法读取 bundle")
	}
	f, err := os.OpenFile(real, bundleOpenFlags, 0)
	if err != nil {
		return nil, wrapErr(KindBundle, err, "无法打开 bundle")
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(lst, fi) {
		_ = f.Close()
		return nil, newErr(KindBundle, "bundle 在校验与打开之间发生变化或不是普通文件")
	}
	entries, err := security.IndexTar(io.NewSectionReader(f, 0, fi.Size()), security.IndexLimits{MaxArchiveSize: fi.Size() + 4096})
	if err != nil {
		_ = f.Close()
		return nil, wrapErr(KindBundle, err, "bundle 不是合法的 tar 或超出限额")
	}
	layout, err := ParseBundleLayout(entries)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Bundle{f: f, Path: real, Layout: layout}, nil
}

// Close 关闭 bundle。
func (b *Bundle) Close() error { return b.f.Close() }

func (b *Bundle) readEntry(e security.TarEntry) ([]byte, error) {
	buf := make([]byte, e.Size)
	if _, err := io.ReadFull(io.NewSectionReader(b.f, e.Offset, e.Size), buf); err != nil {
		return nil, wrapErr(KindBundle, err, "读取 bundle 条目 %q 失败（文件在读取期间变化？）", trim(e.Name, 64))
	}
	return buf, nil
}

// Manifest 返回 bundle 内的 manifest.json 与 manifest.json.sig 字节（未经信任，调用方须交给 Verifier）。
func (b *Bundle) Manifest() (mf, sig []byte, err error) {
	if mf, err = b.readEntry(b.Layout.Manifest); err != nil {
		return nil, nil, err
	}
	if sig, err = b.readEntry(b.Layout.Sig); err != nil {
		return nil, nil, err
	}
	return mf, sig, nil
}

// copyPackage 把 packages/<name> 复制到 dst：条目大小必须等于清单 size，流式 SHA256 必须等于清单 sha256（与在线下载同一口径）。
func (b *Bundle) copyPackage(name, dst string, exp Expected) error {
	e, ok := b.Layout.Packages[name]
	if !ok {
		return newErr(KindBundle, "bundle 缺少清单引用的包 %q", trim(name, 64))
	}
	if e.Size != exp.Size {
		return newErr(KindSize, "bundle 内包 %q 的大小 %d 与清单 size %d 不符", trim(name, 64), e.Size, exp.Size)
	}
	out, err := security.CreateFile(dst, 0o600)
	if err != nil {
		return wrapErr(KindStaging, err, "无法创建 staging 文件")
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(out, h), io.NewSectionReader(b.f, e.Offset, e.Size))
	if serr := out.Sync(); cerr == nil {
		cerr = serr
	}
	if clerr := out.Close(); cerr == nil {
		cerr = clerr
	}
	if cerr != nil {
		return wrapErr(KindBundle, cerr, "读取 bundle 内的包失败")
	}
	if n != exp.Size {
		return newErr(KindSize, "bundle 内包被截断：%d / %d 字节", n, exp.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != exp.SHA256 {
		return newErr(KindSHA256, "bundle 内包的 SHA256 与清单不符").with("expected", exp.SHA256).with("actual", got)
	}
	return nil
}
