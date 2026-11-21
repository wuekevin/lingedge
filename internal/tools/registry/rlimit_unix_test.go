//go:build linux || darwin

package registry

import (
	"os/signal"
	"syscall"
)

func signalIgnoreXFSZ() { signal.Ignore(syscall.SIGXFSZ) }

func setFsizeLimit(n uint64) error {
	return syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: n, Max: n})
}
