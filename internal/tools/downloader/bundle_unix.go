//go:build unix

package downloader

import "syscall"

// bundleOpenFlags：只读、不跟随末级符号链接、不阻塞（防 FIFO/设备被换入时卡住）。
const bundleOpenFlags = syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
