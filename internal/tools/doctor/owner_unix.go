//go:build unix

package doctor

import (
	"io/fs"
	"syscall"
)

// ownerOf 取 FileInfo 的属主 uid/gid；ok=false 表示本平台取不到。
func ownerOf(st fs.FileInfo) (uid, gid int, ok bool) {
	sys, k := st.Sys().(*syscall.Stat_t)
	if !k {
		return 0, 0, false
	}
	return int(sys.Uid), int(sys.Gid), true
}

// nofileLimit 返回 RLIMIT_NOFILE 的软限制。
func nofileLimit() (uint64, error) {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0, err
	}
	return uint64(r.Cur), nil //nolint:unconvert // Rlimit.Cur 在部分平台是 int64
}
