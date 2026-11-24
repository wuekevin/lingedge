//go:build unix

package backup

import (
	"os"
	"syscall"
)

// openNoFollow 以 O_NOFOLLOW 只读打开（拒绝符号链接）。
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// ownerIDs 返回文件属主 uid/gid；无法确定时 ok=false。
func ownerIDs(fi os.FileInfo) (uid, gid uint32, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Uid, st.Gid, true
}
