//go:build unix

package cli

import (
	"io/fs"
	"syscall"
)

// fileOwner 取文件的属主 uid/gid；ok=false 表示本平台取不到。
func fileOwner(st fs.FileInfo) (uid, gid int, ok bool) {
	s, k := st.Sys().(*syscall.Stat_t)
	if !k {
		return 0, 0, false
	}
	return int(s.Uid), int(s.Gid), true
}
