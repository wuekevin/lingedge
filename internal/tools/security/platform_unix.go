//go:build unix

package security

import (
	"os"
	"syscall"
)

// noFollowFlag 使 open 不跟随末级符号链接（Linux/macOS 均支持 O_NOFOLLOW）。
const noFollowFlag = syscall.O_NOFOLLOW

func withUmask(mask int, fn func() error) error {
	old := syscall.Umask(mask)
	defer syscall.Umask(old)
	return fn()
}

// linkCount 返回文件的硬链接数；无法取得时 ok=false。
func linkCount(fi os.FileInfo) (n uint64, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, false
	}
	return uint64(st.Nlink), true //nolint:unconvert // Nlink 在 darwin 为 uint16、linux 为 uint64
}
