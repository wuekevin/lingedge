//go:build unix

package probe

import (
	"io/fs"
	"syscall"
)

func fileOwnerUID(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, false
	}
	return int(st.Uid), true
}
