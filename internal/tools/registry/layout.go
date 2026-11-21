package registry

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// DefaultRoot 是 Tools 状态目录（tools-design §8「Tools 状态」）。
const DefaultRoot = "/var/lib/lingedge/tools"

// Layout 是 Tools 状态目录的布局：
//
//	<Root>/state.json  journal/  journal/archive/  locks/  lock  staging/  trust/
//
// 权限（§8 的「root 0700 + state.json 0644 供普通用户 list/status」在同一棵目录树上自相矛盾——0700 的目录会挡住普通用户读 state.json——
// 因此这里取：Root 为 0711（只给「穿越」权限，不可列目录），state.json 0644；其余子目录一律 0700，只有属主可进入）。
type Layout struct{ Root string }

// LayoutForStatePath 由 state.json 的路径推出布局（state.json 位于 Root 下）。
func LayoutForStatePath(statePath string) Layout { return Layout{Root: filepath.Dir(statePath)} }

// StateFile 是 state.json 的路径。
func (l Layout) StateFile() string { return filepath.Join(l.Root, "state.json") }

// JournalDir 是事务日志目录。
func (l Layout) JournalDir() string { return filepath.Join(l.Root, "journal") }

// ArchiveDir 是已结束事务的归档目录。
func (l Layout) ArchiveDir() string { return filepath.Join(l.JournalDir(), "archive") }

// LocksDir 是组件锁目录。
func (l Layout) LocksDir() string { return filepath.Join(l.Root, "locks") }

// GlobalLockFile 是全局状态锁文件。
func (l Layout) GlobalLockFile() string { return filepath.Join(l.Root, "lock") }

// StagingDir 是 downloader 的 staging 根（StagingDir()/<tx>/）。
func (l Layout) StagingDir() string { return filepath.Join(l.Root, "staging") }

// TrustDir 是信任库（manifest.FileSequenceStore 的 trust/state 与 keys.d）。
func (l Layout) TrustDir() string { return filepath.Join(l.Root, "trust") }

// SequenceFile 是各通道已见最大 sequence 的文件（manifest.FileSequenceStore.Path）。
func (l Layout) SequenceFile() string { return filepath.Join(l.TrustDir(), "state") }

// ComponentLockFile 是组件锁文件（组件名须通过 ValidComponent）。
func (l Layout) ComponentLockFile(component string) string {
	return filepath.Join(l.LocksDir(), component+".lock")
}

// JournalFile 是事务日志文件（tx 须通过 ValidTxID）。
func (l Layout) JournalFile(tx string) string { return filepath.Join(l.JournalDir(), tx+".jsonl") }

var (
	componentRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	txRE        = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_-]{7,63}$`)
)

// ValidComponent 报告组件名是否可安全用作文件名与键（小写字母开头，字母数字与连字符，≤32）。
func ValidComponent(name string) bool { return componentRE.MatchString(name) }

// ValidTxID 报告事务 ID 是否可安全用作文件名（ULID 满足；8–64 个字母数字/下划线/连字符，首字符为字母数字）。
func ValidTxID(id string) bool { return txRE.MatchString(id) }

// Exists 报告状态目录是否已存在（只读命令与「尚未安装过任何东西」的判定用）。
func (l Layout) Exists() bool {
	fi, err := os.Stat(l.Root)
	return err == nil && fi.IsDir()
}

// Ensure 创建缺失的目录并校验已存在目录的安全性：Root 0711、其余 0700；
// 必须是真实目录（不是符号链接）、属于当前有效用户（或 root）、子目录不得对 group/other 开放、Root 不得对 group/other 可写。
// 已存在的目录不改权限（权限过宽是错误而不是被悄悄「修好」，修复属 doctor --fix-perms）。
func (l Layout) Ensure() error {
	if l.Root == "" || !filepath.IsAbs(l.Root) {
		return newErr(KindInvalid, "状态目录必须是绝对路径: %q", l.Root)
	}
	if err := security.MkdirAllPerm(filepath.Dir(l.Root), 0o755); err != nil {
		return wrapErr(KindIO, err, "无法创建 %s", filepath.Dir(l.Root))
	}
	if err := security.MkdirAllPerm(l.Root, 0o711); err != nil {
		return wrapErr(KindIO, err, "无法创建 %s", l.Root)
	}
	if err := checkDir(l.Root, 0o022); err != nil {
		return err
	}
	for _, d := range []string{l.JournalDir(), l.ArchiveDir(), l.LocksDir(), l.StagingDir(), l.TrustDir()} {
		if err := security.MkdirAllPerm(d, 0o700); err != nil {
			return wrapErr(KindIO, err, "无法创建 %s", d)
		}
		if err := checkDir(d, 0o077); err != nil {
			return err
		}
	}
	return nil
}

// checkDir 要求 path 是真实目录、属主为当前有效用户或 root，且 perm&forbidden==0。
func checkDir(path string, forbidden fs.FileMode) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return wrapErr(KindIO, err, "无法检查 %s", path)
	}
	if !fi.IsDir() { // Lstat：符号链接的 Mode 不含 ModeDir
		return &Error{Kind: KindPermission, Msg: "不是真实目录（符号链接或其他类型被拒绝）", Path: path}
	}
	if !ownerTrusted(fi) {
		return &Error{Kind: KindPermission, Msg: "目录属主既不是当前用户也不是 root", Path: path}
	}
	if fi.Mode().Perm()&forbidden != 0 {
		return &Error{Kind: KindPermission, Msg: "目录权限过宽 " + fi.Mode().Perm().String() + "（请 chmod 或运行 doctor --fix-perms）", Path: path}
	}
	return nil
}

// ownerTrusted 报告文件属主是当前有效用户或 root；取不到属主信息的平台上保守地返回 true（由权限位兜底）。
func ownerTrusted(fi os.FileInfo) bool {
	uid, ok := fileUID(fi)
	if !ok {
		return true
	}
	return uid == euid() || uid == 0
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
