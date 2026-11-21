package downloader

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// 跨调用断点续传（Story 1.28b）。
//
// 设计：Config.ResumeDir 指向一个只属于当前用户的缓存目录（0700，不在 staging 内——staging/<tx> 失败即整体删除，AC4 不变）。
// 下载按「URL + 期望 SHA256 + 期望 size」命名 <key>.part，同一个包的上一次中断下载（网络错误、被取消、重试耗尽）会留下它，
// 下一次 Download/Acquire 从磁盘重新计算已有部分的哈希后用 Range 续传。
//
// 信任模型：缓存里的 partial **不被信任**。
//   - 目录必须是自己拥有、不对 group/other 开放的真实目录（符号链接/他人拥有/可被他人写入一律拒绝）；
//   - partial 与锁文件必须是自己拥有、只有一个硬链接、权限不含 group/other 的普通文件，且打开时不跟随符号链接，
//     不满足则拒绝复用（并删除该条目，下次干净地重新开始）；
//   - 续传前后都不依赖内存状态：整体长度与 SHA256 必须等于清单才会改名成最终文件，
//     校验不通过（前半段或后半段被篡改、缓存被破坏）→ 立即删除缓存里的 partial 并以 KindSHA256 失败（退出码 5），绝不复用；
//   - 只有传输类失败（网络/取消/超时/截断）才保留 partial；策略类失败（SHA256、超量、非法 Content-Range…）一律删除。
//
// 并发：同一个 key 由 flock 保护；拿不到锁（另一个进程/调用正在下载同一个包）时本次退回到调用方私有的 <dst>.part（不共享、不续传），
// 因此两个并发下载永远不会写同一个文件。命名用 SHA256(域前缀 + URL + sha + size)，调用方无法影响路径的其它部分。
const defaultResumeTTL = 7 * 24 * time.Hour

var reResumeName = regexp.MustCompile(`^([0-9a-f]{64})\.(part|lock)$`)

type resumeSlot struct {
	path    string
	release func()
}

// ResumeKey 返回缓存条目名（不含目录与后缀）。
func ResumeKey(raw string, exp Expected) string {
	h := sha256.Sum256([]byte("lingedge-resume-v1\n" + raw + "\n" + exp.SHA256 + "\n" + strconv.FormatInt(exp.Size, 10)))
	return hex.EncodeToString(h[:])
}

func checkResumeDir(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return newErr(KindOptions, "ResumeDir 必须是规范化的绝对路径")
	}
	return nil
}

// ensureResumeDir 创建（0700）并校验缓存目录。
func ensureResumeDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		if errors.Is(err, os.ErrNotExist) {
			if e := os.MkdirAll(filepath.Dir(dir), 0o700); e != nil {
				return wrapErr(KindStaging, e, "无法创建续传缓存目录的上级")
			}
			if e := os.Mkdir(dir, 0o700); e != nil && !errors.Is(e, os.ErrExist) {
				return wrapErr(KindStaging, e, "无法创建续传缓存目录")
			}
		} else {
			return wrapErr(KindStaging, err, "无法创建续传缓存目录")
		}
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return wrapErr(KindStaging, err, "无法检查续传缓存目录")
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return newErr(KindStaging, "续传缓存目录是符号链接，拒绝使用")
	case !fi.IsDir():
		return newErr(KindStaging, "续传缓存目录不是目录")
	case !dirOwnerOK(fi):
		return newErr(KindStaging, "续传缓存目录不属于当前用户，拒绝使用")
	case fi.Mode().Perm()&0o077 != 0:
		return newErr(KindStaging, "续传缓存目录对 group/other 开放（须为 0700），拒绝使用")
	}
	return nil
}

// checkPartial 校验已有的 partial 是否可复用；不可复用（符号链接/非普通文件/他人拥有/硬链接/权限过宽）则删除条目并报错。
func checkPartial(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return wrapErr(KindStaging, err, "无法检查续传缓存")
	}
	if !fi.Mode().IsRegular() || !fileOwnerOK(fi) || fi.Mode().Perm()&0o077 != 0 {
		_ = os.Remove(path) // 只摘掉条目本身（符号链接不会被跟随）
		return newErr(KindStaging, "续传缓存条目不安全（符号链接/非普通文件/他人拥有/硬链接/权限过宽），已拒绝复用并删除")
	}
	return nil
}

// openResume 取得 key 的缓存槽位。(nil, nil) 表示别的调用正在使用同一个 key：调用方退回私有 .part。
func (c *Client) openResume(raw string, exp Expected) (*resumeSlot, error) {
	dir := c.cfg.ResumeDir
	if err := ensureResumeDir(dir); err != nil {
		return nil, err
	}
	key := ResumeKey(raw, exp)
	c.pruneResume(dir, key)
	release, busy, err := lockFile(filepath.Join(dir, key+".lock"))
	if err != nil {
		return nil, wrapErr(KindStaging, err, "无法锁定续传缓存")
	}
	if busy {
		return nil, nil
	}
	part := filepath.Join(dir, key+".part")
	if err := checkPartial(part); err != nil {
		release()
		return nil, err
	}
	return &resumeSlot{path: part, release: release}, nil
}

// pruneResume 尽力清理过期条目（超过 ResumeTTL 未被触碰）。只处理名称形如 <64 位十六进制>.part|.lock 的条目；
// 删除前先取得该 key 的锁，所以不会动正在被使用的条目。
func (c *Client) pruneResume(dir, keep string) {
	ttl := c.cfg.ResumeTTL
	if ttl <= 0 {
		ttl = defaultResumeTTL
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		m := reResumeName.FindStringSubmatch(e.Name())
		if m == nil || m[1] == keep {
			continue
		}
		fi, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil || time.Since(fi.ModTime()) < ttl {
			continue
		}
		release, busy, err := lockFile(filepath.Join(dir, m[1]+".lock"))
		if err != nil || busy {
			continue
		}
		_ = os.Remove(filepath.Join(dir, m[1]+".part"))
		if m[2] == "lock" || fi.Mode().IsRegular() {
			_ = os.Remove(filepath.Join(dir, m[1]+".lock")) // 持锁时 unlink；lockFile 的同一 inode 复核保证等待者会重来
		}
		release()
	}
}

// keepPartial 判断失败后是否保留缓存里的 partial：只有传输类失败（可重试的、被取消的、网络类）才保留。
func keepPartial(err error, lastRetryable bool) bool {
	if lastRetryable {
		return true
	}
	var de *Error
	if !errors.As(err, &de) {
		return true // 调用方取消（Ctrl-C）等：原样的 context 错误
	}
	return de.Kind == KindNetwork
}

// finalizeResume 把已校验的缓存 partial 变成最终文件：同一文件系统直接 rename；否则复制并在复制时重新计算哈希。
func finalizeResume(part, dst string, exp Expected) error {
	if err := os.Rename(part, dst); err == nil {
		return nil
	}
	return copyFinalize(part, dst, exp)
}

// copyFinalize 是跨文件系统时的落名路径：复制 + 复制时重新计算 SHA256，不匹配则删除两端并失败。
func copyFinalize(part, dst string, exp Expected) error {
	in, err := openNoFollow(part, os.O_RDONLY)
	if err != nil {
		return wrapErr(KindStaging, err, "无法读取续传缓存")
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".part"
	_ = os.Remove(tmp)
	out, err := security.CreateFile(tmp, 0o600)
	if err != nil {
		return wrapErr(KindStaging, err, "无法创建落名临时文件")
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, exp.Size+1))
	if serr := out.Sync(); cerr == nil {
		cerr = serr
	}
	if clerr := out.Close(); cerr == nil {
		cerr = clerr
	}
	if cerr == nil && (n != exp.Size || hex.EncodeToString(h.Sum(nil)) != exp.SHA256) {
		cerr = newErr(KindSHA256, "落名时重新校验失败：长度或 SHA256 与清单不符")
		_ = os.Remove(part)
	}
	if cerr == nil {
		if cerr = os.Rename(tmp, dst); cerr != nil {
			cerr = wrapErr(KindStaging, cerr, "无法落名下载结果")
		}
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	_ = os.Remove(part)
	return nil
}

func openNoFollow(path string, flag int) (*os.File, error) {
	return os.OpenFile(path, flag|noFollow, 0)
}
