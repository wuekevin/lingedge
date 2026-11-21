//go:build linux || darwin

package service

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
