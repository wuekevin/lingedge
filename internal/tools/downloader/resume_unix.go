//go:build unix

package downloader

import (
	"errors"
	"os"
	"syscall"
)

// noFollow 使 open 不跟随末级符号链接。
const noFollow = syscall.O_NOFOLLOW

// fileOwnerOK 报告文件属于当前进程的有效用户，且只有一个硬链接（缓存里不接受他人创建/被硬链接出去的文件）。
func fileOwnerOK(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == os.Geteuid() && uint64(st.Nlink) == 1 //nolint:unconvert // Nlink 在 darwin 为 uint16、linux 为 uint64
}

func dirOwnerOK(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// lockFile 在 path 上取得独占的非阻塞咨询锁（flock）。busy=true 表示别的持有者正在使用（不是错误）。
// 打开时不跟随符号链接、要求是自己拥有的普通文件；加锁后复核路径仍指向同一个 inode（防「被清理线程 unlink 后重建」的竞态）。
func lockFile(path string) (release func(), busy bool, err error) {
	for range 3 {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|noFollow, 0o600)
		if err != nil {
			return nil, false, err
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() || !fileOwnerOK(fi) {
			_ = f.Close()
			return nil, false, errors.New("锁文件不是自己拥有的普通文件")
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd 值域安全
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, true, nil
			}
			return nil, false, err
		}
		if li, lerr := os.Lstat(path); lerr != nil || !os.SameFile(fi, li) {
			_ = f.Close() // 加锁期间路径被换掉：重来
			continue
		}
		return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, false, nil //nolint:gosec // fd 值域安全
	}
	return nil, false, errors.New("锁文件持续被替换")
}
