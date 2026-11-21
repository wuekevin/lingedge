//go:build linux || darwin

package registry

import (
	"errors"
	"os"
	"syscall"
)

const noFollowFlag = syscall.O_NOFOLLOW

// flockTry 在 f 上取 LOCK_EX|LOCK_NB。busy=true 表示别的打开文件描述（含同进程内另一次 open）持有，不是错误。
func flockTry(f *os.File) (busy bool, err error) {
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // fd 值域安全
		switch {
		case err == nil:
			return false, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return true, nil
		default:
			return false, err
		}
	}
}

func funlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) } //nolint:gosec // fd 值域安全

// fileUID 返回文件属主；取不到时 ok=false。
func fileUID(fi os.FileInfo) (uid int, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, false
	}
	return int(st.Uid), true
}

func nlink(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink) //nolint:unconvert // Nlink 在 darwin 为 uint16、linux 为 uint64
	}
	return 1
}

func euid() int { return os.Geteuid() }

// platformSupported 报告本平台是否有真实的 flock 实现。
const platformSupported = true
