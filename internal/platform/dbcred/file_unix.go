//go:build !windows

package dbcred

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// checkOwner：属主须是运行用户或 root。
func checkOwner(st fs.FileInfo) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return invalid("无法确定属主")
	}
	if uid := uint32(os.Geteuid()); sys.Uid != uid && sys.Uid != 0 {
		return invalid("属主既不是运行用户也不是 root")
	}
	return nil
}

// writeAtomic：同目录临时文件（O_EXCL、0600）+ fsync + rename + fsync 目录；目录 0700。
// 以 root 运行（headless setup / root 侧轮换命令）时，把新文件与目录的属主对齐到数据目录的属主（lingedge:lingedge，ADR-026 §3）。
func writeAtomic(path string, f *File) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("dbcred: 创建目录: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("dbcred: 目录权限: %w", err)
	}
	var uid, gid = -1, -1
	if os.Geteuid() == 0 {
		if st, err := os.Stat(filepath.Dir(dir)); err == nil {
			if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Uid != 0 {
				uid, gid = int(sys.Uid), int(sys.Gid)
				_ = os.Chown(dir, uid, gid)
			}
		}
	}
	b, err := marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".runtime-credentials-*")
	if err != nil {
		return fmt.Errorf("dbcred: 创建临时文件: %w", err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("dbcred: chmod: %w", err))
	}
	if uid >= 0 {
		if err := tmp.Chown(uid, gid); err != nil {
			return fail(fmt.Errorf("dbcred: chown: %w", err))
		}
	}
	if _, err := tmp.Write(b); err != nil {
		return fail(fmt.Errorf("dbcred: 写入: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("dbcred: fsync: %w", err))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("dbcred: close: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("dbcred: rename: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
