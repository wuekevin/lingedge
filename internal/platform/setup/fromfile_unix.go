//go:build !windows

package setup

import (
	"os"
	"syscall"
)

// openFromFile 以 O_NOFOLLOW 打开并对同一句柄 fstat：普通文件、属主 = 调用者、权限恰为 0600，否则 ErrFromFilePermission。
func openFromFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrFromFilePermission
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		_ = f.Close()
		return nil, ErrFromFilePermission
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != uint32(os.Geteuid()) {
		_ = f.Close()
		return nil, ErrFromFilePermission
	}
	return f, nil
}
