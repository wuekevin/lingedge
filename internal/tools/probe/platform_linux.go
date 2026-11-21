//go:build linux

package probe

import (
	"fmt"
	"os"
	"syscall"

	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

const (
	defaultProcRoot = "/proc"
	supportsProc    = true
)

// osMemory 读取 /proc/meminfo（MemTotal/MemAvailable）。
func osMemory() (totalMB, availMB int64, notes []string, err error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, nil, err
	}
	t, a, approx, ok := ParseMeminfo(b)
	if !ok {
		return 0, 0, nil, fmt.Errorf("probe: /proc/meminfo 缺少 MemTotal")
	}
	if approx {
		notes = append(notes, "内核没有 MemAvailable，可用内存按 MemFree+Buffers+Cached 近似")
	}
	return t, a, notes, nil
}

// osDisks 读取 /proc/self/mountinfo 并对每个挂载点 statfs（f_bavail × f_frsize，即非特权进程可用空间）。
func osDisks() ([]resolver.Disk, []string, error) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, nil, err
	}
	mounts, skipped := ParseMountinfo(b)
	var notes []string
	if skipped > 0 {
		notes = append(notes, fmt.Sprintf("mountinfo 有 %d 行无法解析，已跳过", skipped))
	}
	var out []resolver.Disk
	for _, m := range mounts {
		var st syscall.Statfs_t
		if err := syscall.Statfs(m.Mount, &st); err != nil {
			notes = append(notes, fmt.Sprintf("statfs %s 失败：%v（该挂载点不计入）", m.Mount, err))
			continue
		}
		bs := int64(st.Frsize) //nolint:unconvert // 字段宽度随架构不同
		if bs <= 0 {
			bs = int64(st.Bsize) //nolint:unconvert
		}
		out = append(out, resolver.Disk{
			Mount: m.Mount, FSType: m.FSType, ReadOnly: m.ReadOnly || st.Flags&1 != 0, // ST_RDONLY=1
			TotalMB: int64(st.Blocks) * bs >> 20, FreeMB: int64(st.Bavail) * bs >> 20, //nolint:unconvert
		})
	}
	return out, notes, nil
}
