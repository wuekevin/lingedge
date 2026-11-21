//go:build darwin

package probe

import (
	"fmt"
	"syscall"

	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

// 非 Linux 仅用于开发机：没有 /proc，监听探测不可用（见 Collect 的降级说明）。
const (
	defaultProcRoot = ""
	supportsProc    = false
)

// osMemory：darwin 只给总内存（sysctl hw.memsize）；可用内存不可得 → 0（resolver 对「需要内存但为 0」阻断，而不是放行）。
func osMemory() (totalMB, availMB int64, notes []string, err error) {
	s, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0, 0, nil, err
	}
	b := []byte(s) // Sysctl 会去掉末尾一个 NUL，把 uint64 小端补齐
	for len(b) < 8 {
		b = append(b, 0)
	}
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return int64(v >> 20), 0, []string{"darwin 无 MemAvailable 等价物，可用内存记为 0（仅开发机）"}, nil
}

// osDisks：用 getfsstat 列出已挂载文件系统。
func osDisks() ([]resolver.Disk, []string, error) {
	n, err := syscall.Getfsstat(nil, 2) // MNT_NOWAIT
	if err != nil {
		return nil, nil, err
	}
	buf := make([]syscall.Statfs_t, n+8)
	n, err = syscall.Getfsstat(buf, 2)
	if err != nil {
		return nil, nil, err
	}
	var out []resolver.Disk
	for _, st := range buf[:n] {
		fs := cstr(st.Fstypename[:])
		if fs == "devfs" || fs == "autofs" {
			continue
		}
		bs := int64(st.Bsize)
		out = append(out, resolver.Disk{Mount: cstr(st.Mntonname[:]), FSType: fs, ReadOnly: st.Flags&1 != 0,
			TotalMB: int64(st.Blocks) * bs >> 20, FreeMB: int64(st.Bavail) * bs >> 20})
	}
	return out, []string{fmt.Sprintf("darwin：%d 个挂载点（仅开发机）", len(out))}, nil
}

func cstr(a []int8) string {
	b := make([]byte, 0, len(a))
	for _, c := range a {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
