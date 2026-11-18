package migrationstest

import (
	"strings"
)

// splitSQL 把脚本切成语句：按当前分隔符（缺省 ;）切分、引号/反引号内的分隔符不算、`-- ` 与 `#` 行注释丢弃、
// 支持 `DELIMITER x` 指令（触发器体）。仅供测试读取 ddl-rules.sql 与 m1-fk-skeleton.sql；迁移文件的解析在 internal/platform/migrate。
func splitSQL(text string) []string {
	var out []string
	delim := ";"
	var cur strings.Builder
	push := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	rs := []rune(text)
	for i := 0; i < len(rs); {
		// 行首的 DELIMITER 指令（仅当当前语句为空时有效）。
		if strings.TrimSpace(cur.String()) == "" && (i == 0 || rs[i-1] == '\n') {
			j := i
			for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
				j++
			}
			if j+9 <= len(rs) && strings.EqualFold(string(rs[j:j+9]), "DELIMITER") && j+9 < len(rs) && (rs[j+9] == ' ' || rs[j+9] == '\t') {
				k := j + 9
				for k < len(rs) && rs[k] != '\n' {
					k++
				}
				delim = strings.TrimSpace(string(rs[j+9 : k]))
				i = k
				continue
			}
		}
		c := rs[i]
		switch {
		case c == '#' || (c == '-' && i+1 < len(rs) && rs[i+1] == '-' && (i+2 >= len(rs) || rs[i+2] == ' ' || rs[i+2] == '\t' || rs[i+2] == '\n')):
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
		case c == '\'' || c == '"' || c == '`':
			cur.WriteRune(c)
			i++
			for i < len(rs) {
				cur.WriteRune(rs[i])
				if rs[i] == '\\' && c != '`' && i+1 < len(rs) {
					i++
					cur.WriteRune(rs[i])
				} else if rs[i] == c {
					i++
					break
				}
				i++
			}
		case strings.HasPrefix(string(rs[i:min(i+len(delim), len(rs))]), delim):
			push()
			i += len([]rune(delim))
		default:
			cur.WriteRune(c)
			i++
		}
	}
	push()
	return out
}
