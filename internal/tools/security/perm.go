package security

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// 文件权限助手（tools-design §18.3：umask 077 + 显式 chmod，创建即最终权限）。
//
// 做法不依赖进程级 umask（它是全局状态，并发不安全）：
//   - 创建时一律用 0600（目录 0700），保证任何时刻都不比最终权限更宽（对 group/other 而言）；
//   - 随后立即用 Chmod 设为显式的最终权限，且在写入任何内容之前完成；
//   - 因此结果与调用时的 umask 无关，也不会出现「先宽后收」的窗口。

// CreateFile 以独占方式（O_EXCL，不跟随符号链接）创建新文件并设为最终权限 perm，返回可写句柄。
func CreateFile(path string, perm fs.FileMode) (*os.File, error) {
	if err := checkPerm(perm); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return f, nil
}

// WriteFileAtomic 原子写文件：同目录临时文件（0600→显式 chmod 为 perm，先于写入）→ 写入 → fsync → rename → fsync 目录。
// 目标已存在则被替换；任何失败都不留下临时文件，也不会让读者看到半个文件。
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return WriteFileAtomicPre(path, data, perm, nil)
}

// WriteFileAtomicPre 同 WriteFileAtomic，但在内容写入之前调用 pre(临时文件路径)（用于先 chown 再写内容、再 rename：
// 目标路径上的文件在任何时刻都不会出现「内容已就位而属主不对」的状态）。pre 失败则整体失败并清理临时文件，目标保持原样。
func WriteFileAtomicPre(path string, data []byte, perm fs.FileMode, pre func(tmp string) error) error {
	if err := checkPerm(perm); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	var tmp string
	var f *os.File
	for i := 0; i < 8; i++ {
		var rnd [6]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return err
		}
		tmp = filepath.Join(dir, "."+filepath.Base(path)+".tmp-"+hex.EncodeToString(rnd[:]))
		var err error
		f, err = CreateFile(tmp, perm)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		f = nil
	}
	if f == nil {
		return fmt.Errorf("无法创建临时文件: %s", path)
	}
	fail := func(err error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if pre != nil {
		if err := pre(tmp); err != nil {
			return fail(err)
		}
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil { // 目录 fsync 失败不致命（部分文件系统不支持）
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// MkdirAllPerm 逐级创建目录：新建的每一级都先以 0700 创建、立即 chmod 为 perm；已存在的目录不改权限。
// 路径上遇到非目录（含符号链接指向文件）则失败。
func MkdirAllPerm(path string, perm fs.FileMode) error {
	if err := checkPerm(perm); err != nil {
		return err
	}
	path = filepath.Clean(path)
	if st, err := os.Stat(path); err == nil {
		if !st.IsDir() {
			return &fs.PathError{Op: "mkdir", Path: path, Err: errors.New("不是目录")}
		}
		return nil
	}
	if parent := filepath.Dir(path); parent != path {
		if err := MkdirAllPerm(parent, perm); err != nil {
			return err
		}
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			if st, e := os.Stat(path); e == nil && st.IsDir() {
				return nil
			}
		}
		return err
	}
	return os.Chmod(path, perm)
}

// checkPerm 只接受普通权限位：拒绝 setuid/setgid/sticky 等特殊位（Tools 创建的文件从不需要它们）。
func checkPerm(perm fs.FileMode) error {
	if perm&^fs.ModePerm != 0 {
		return fmt.Errorf("权限 %v 含特殊位（setuid/setgid/sticky 不被允许）", perm)
	}
	return nil
}

// WithUmask077 在 fn 执行期间把进程 umask 设为 077，结束后恢复。umask 是进程级状态：
// 仅供 Tools 单线程的启动/安装阶段包裹第三方代码使用，并发代码请用 CreateFile/MkdirAllPerm（不依赖 umask）。
func WithUmask077(fn func() error) error { return withUmask(0o077, fn) }
