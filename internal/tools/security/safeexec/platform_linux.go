//go:build linux

package safeexec

import (
	"os/exec"
	"syscall"
)

// applyPlatform（Linux）：子进程独立进程组，并在父进程（Tools）死亡时由内核发 SIGKILL，避免遗留孤儿进程。
// 该语义（PR_SET_PDEATHSIG）只在 Linux 存在，需在 ENV-LNX 验收机复验。
func applyPlatform(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
