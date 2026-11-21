//go:build !linux

package registry

// procStartTicks 在非 Linux 平台没有 /proc，返回 0（诊断字段缺省）。
func procStartTicks(int) uint64 { return 0 }
