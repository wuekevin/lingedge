//go:build !linux

package safeexec

import "os/exec"

func applyPlatform(cmd *exec.Cmd) {
	// 非 Linux（仅开发机）无 Pdeathsig；Tools 的生产目标是 Linux。
	setProcessGroup(cmd)
}
