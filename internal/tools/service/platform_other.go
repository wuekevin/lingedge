//go:build !linux

package service

// systemdBooted 在非 Linux 平台恒为 false：Tools 仅支持 Linux（tools-design §22 T-1）。
// Systemctl 因此会明确返回 KindNotSystemd，而不是尝试去执行一个不存在的 systemctl。
func systemdBooted() bool { return false }
