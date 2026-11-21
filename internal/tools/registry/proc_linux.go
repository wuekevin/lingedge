//go:build linux

package registry

import (
	"os"
	"strconv"
)

// procStartTicks 读取进程启动时刻（仅用于持有者诊断显示，使 pid 复用可被肉眼识别）；读不到返回 0。
func procStartTicks(pid int) uint64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	n, _ := parseProcStat(string(b))
	return n
}
