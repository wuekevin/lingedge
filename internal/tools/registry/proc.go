package registry

import (
	"strconv"
	"strings"
)

// parseProcStat 解析 /proc/<pid>/stat 的 starttime（第 22 个字段，单位 clock tick）。
// comm 字段（第 2 个）被括号包起且可含空格与括号，因此以「最后一个 ')'」为界再按空格切分。平台无关，便于在任何系统上单元测试。
func parseProcStat(s string) (uint64, bool) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	// ')' 之后第一个字段是第 3 个（state），starttime 是第 22 个 → 下标 19。
	if len(f) < 20 {
		return 0, false
	}
	n, err := strconv.ParseUint(f[19], 10, 64)
	return n, err == nil
}
