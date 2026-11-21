//go:build !unix

package safeexec

import "os/exec"

func setProcessGroup(*exec.Cmd) {}
