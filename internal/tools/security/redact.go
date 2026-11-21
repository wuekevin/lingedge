package security

import (
	"strings"

	"github.com/wuekevin/lingedge/pkg/redact"
)

// 脱敏入口（NFR-12、G-5）。本文件只封装 pkg/redact（Server、Tools、Node 共用的唯一规则集），不另立任何规则：
// 规则的增删只在 pkg/redact 进行，这里新增的只有 Tools 特有的「命令行参数」形态（Args）。

// Mask 是敏感内容的替换文本。
const Mask = redact.Mask

// RedactString 对自由文本脱敏。
func RedactString(s string) string { return redact.String(s) }

// RedactValue 对任意值（map/slice/字符串/错误等）递归脱敏。
func RedactValue(v any) any { return redact.Value(v) }

// RedactError 返回脱敏后的错误（保留 errors.Is/As 链）。
func RedactError(err error) error { return redact.Error(err) }

// SensitiveKey 报告键名是否指向敏感值。
func SensitiveKey(key string) bool { return redact.Key(key) }

// RedactArgs 返回可安全记录的 argv 副本（tools-design §5、Story 1.25 AC6）：
//   - 敏感标志（--token、--password…，名称经 SensitiveKey 判定；*-file 是路径，不是机密）的值记为 `***`，
//     无论写成 `--token X` 还是 `--token=X`；
//   - `--set key=value` 在 key 敏感时值记为 `***`；
//   - 其余元素再经 RedactString 兜底（令牌形态/URL 口令等）。
func RedactArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out = append(out, args[i:]...)
			break
		}
		if !strings.HasPrefix(a, "--") {
			out = append(out, redact.String(a))
			continue
		}
		name, val, hasEq := strings.Cut(a[2:], "=")
		switch {
		case sensitiveFlag(name):
			if hasEq {
				out = append(out, "--"+name+"="+Mask)
			} else {
				out = append(out, a)
				if i+1 < len(args) {
					out = append(out, Mask)
					i++
				}
			}
		case name == "set":
			if !hasEq {
				out = append(out, a)
				if i+1 < len(args) {
					out = append(out, redactSet(args[i+1]))
					i++
				}
			} else {
				out = append(out, "--set="+redactSet(val))
			}
		default:
			out = append(out, redact.String(a))
		}
	}
	return out
}

func sensitiveFlag(name string) bool {
	if strings.HasSuffix(name, "-file") {
		return false
	}
	return redact.Key(name)
}

func redactSet(kv string) string {
	k, v, ok := strings.Cut(kv, "=")
	if !ok {
		return redact.String(kv)
	}
	if redact.Key(k) {
		return k + "=" + Mask
	}
	return k + "=" + redact.String(v)
}
