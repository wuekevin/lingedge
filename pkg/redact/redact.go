// Package redact 是 LingEdge 的脱敏引擎（NFR-12、ADR-011 §7、G-5）：只依赖标准库，Server、Tools、Node 共用。
//
// 引擎在三个层面工作，任何一处命中即把敏感部分替换为 Mask：
//   - 键名：键名含 password|secret|token|private_key|authorization|cookie 等词（不区分大小写、驼峰/连字符/点分均归一）时，整个值被替换；
//   - 值格式：与键名无关，自由文本里出现 API Token（le_<前缀>_…）、节点注册令牌（lenr1.…）、PEM 私钥块、
//     DSN/URL 内嵌口令、Bearer 凭据、`password=…`/JSON 片段、Cookie 头时，只替换敏感部分、保留上下文；
//   - 结构与载体：嵌套 map/slice、错误链（Error 包装保留 Unwrap，errors.Is/As 仍可用）、panic 值。
//
// 设计取向：宁可多遮（键名按子串匹配），也不漏；对良性内容（ULID、主机名、普通句子）保持原样，由测试断言不误伤。
package redact

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Mask 是敏感内容的替换文本。
const Mask = "***"

// sensitiveWords 是键名判定词（归一为小写下划线形态后做子串匹配）。前六项来自 Story 1.4 AC2，其余为同类凭据的常见写法。
var sensitiveWords = []string{
	"password", "passwd", "passphrase", "secret", "token", "private_key", "privatekey",
	"authorization", "cookie", "credential", "api_key", "apikey", "dsn",
	"master_key", "encryption_key", "signing_key", "access_key", "secret_key", "session_id", "sessionid", "recovery_code",
}

// sensitiveSegments 是按下划线分段后需整段相等才命中的短词（避免 side/footprint 之类被子串误伤）。
var sensitiveSegments = map[string]bool{"pwd": true, "sid": true, "otp": true, "totp": true, "csrf": true, "hmac": true, "signature": true}

// Key 报告键名是否指向敏感值。归一规则：驼峰拆为下划线（apiKey→api_key），连字符/点/空白统一为下划线，再转小写。
func Key(key string) bool {
	n := normalizeKey(key)
	if n == "" {
		return false
	}
	for _, w := range sensitiveWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	for _, seg := range strings.Split(n, "_") {
		if sensitiveSegments[seg] {
			return true
		}
	}
	return false
}

func normalizeKey(key string) string {
	var b strings.Builder
	b.Grow(len(key) + 4)
	var prev rune
	for i, r := range key {
		switch {
		case r == '-' || r == '.' || r == ' ' || r == '\t':
			b.WriteByte('_')
		case r >= 'A' && r <= 'Z':
			// 驼峰边界：小写/数字后接大写（apiKey、PrivateKey 的 K 前）。连续大写（API_KEY、APIKey）按小写直接拼接。
			if i > 0 && (prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9') {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune(r)
		}
		prev = r
	}
	return b.String()
}

var (
	// PEM 私钥块：成对（含 RSA/EC/OPENSSH/ENCRYPTED/PGP 变体，兼容 JSON 里被转义的换行）；以及只有头没有尾（被截断）的情形。
	rePEMBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----.*?-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----`)
	rePEMOpen  = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----.*`)

	// API Token：le_<前缀>_<随机>（threat-model §：前缀 8 位，此处放宽到 1–16 位字母数字）。
	reAPIToken = regexp.MustCompile(`\ble_[A-Za-z0-9]{1,16}_[A-Za-z0-9_-]{8,}`)
	// 节点注册令牌：lenr1.<base64url(…)>。
	reEnrollToken = regexp.MustCompile(`\blenr1\.[A-Za-z0-9_\-+/=.]+`)

	// URL 内嵌口令：scheme://<用户>:<口令>@host（口令中允许出现 @，取到 token 内最后一个 @）。
	reURLPassword = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://[^\s:/@]*:)([^\s/]+)(@)`)
	// go-sql-driver/mysql DSN：<用户>:<口令>@tcp(host:port)/db。
	reMySQLDSN = regexp.MustCompile(`(?i)([A-Za-z0-9_.\-]+:)(\S+)(@(?:(?:tcp[46]?|unix|udp|pipe|memory)?\(|/))`)
	// URL 用户信息里直接放令牌：scheme://<令牌>@host（用户名 ≥8 位视为令牌；git@host 之类短用户名不受影响）。
	reURLTokenUser = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)([^\s/:@]{8,})(@)`)
	// 命令行标志：「--<口令类名称> <值>」空格分隔形态（等号形态已由键值规则处理）。
	reCLIFlag = regexp.MustCompile(`(?i)(--(?:password|passwd|passphrase|secret|token|api-key|private-key)[ \t]+)[^\s-][^\s]*`)

	// Bearer 凭据。
	reBearer = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9\-._~+/]{8,}=*`)

	// Cookie / Set-Cookie 头：整行（值里常含空格与分号）。
	reCookieHeader = regexp.MustCompile(`(?i)\b((?:set-)?cookie[ \t]*[:=][ \t]*)[^\r\n"'\]]+`)
	// 会话 cookie 名=值（le_admin_sid / le_console_sid）。
	reSessionCookie = regexp.MustCompile(`(?i)\b(le_(?:admin|console)_sid[ \t]*=[ \t]*)[^\s;,"']+`)

	// 自由文本里的 key=value / "key":"value" / key: value 的「键」部分：键名含敏感词，其后（同一行内）跟 : 或 =。
	// 值部分由 maskKeyValues 手工扫描（支持带转义的引号串、嵌套的 {…}/[…]、Bearer 等前缀词），正则无法表达嵌套。
	reKeyHead = regexp.MustCompile(`(?i)\b[\w.\-]*(?:password|passwd|passphrase|secret|token|private[_-]?key|authorization|cookie|credential|api[_-]?key|dsn)[\w.\-]*(?:\\?["'])?[ \t]*[:=][ \t]*`)
	// 值前缀词（Authorization: Bearer xxx 这类「方案 + 凭据」）。
	reSchemeWord = regexp.MustCompile(`(?i)^(?:bearer|basic|digest|token)[ \t]+`)
)

// String 清洗自由文本中的敏感值，保留其余上下文。对正常输入幂等（String(String(s)) == String(s)，语料测试断言）；
// 对畸形输入重复清洗只会更保守（文本不增长），因此重复包装（如 Wrap 套 Wrap）不会放出任何内容。
func String(s string) string {
	if s == "" {
		return s
	}
	// 先处理跨行/长块，避免后续规则在块内部产生碎片。
	s = rePEMBlock.ReplaceAllString(s, Mask)
	s = rePEMOpen.ReplaceAllString(s, Mask)
	s = reCookieHeader.ReplaceAllString(s, "${1}"+Mask)
	s = reSessionCookie.ReplaceAllString(s, "${1}"+Mask)
	s = maskKeyValues(s)
	s = reURLPassword.ReplaceAllString(s, "${1}"+Mask+"${3}")
	s = reMySQLDSN.ReplaceAllString(s, "${1}"+Mask+"${3}")
	s = reURLTokenUser.ReplaceAllString(s, "${1}"+Mask+"${3}")
	s = reCLIFlag.ReplaceAllString(s, "${1}"+Mask)
	s = reBearer.ReplaceAllString(s, "${1}"+Mask)
	s = reAPIToken.ReplaceAllString(s, Mask)
	s = reEnrollToken.ReplaceAllString(s, Mask)
	return s
}

// maskKeyValues 把「敏感键名 + 分隔符」之后的值替换为 Mask，保留键与引号形态。
func maskKeyValues(s string) string {
	var out strings.Builder
	pos := 0
	for pos < len(s) {
		loc := reKeyHead.FindStringIndex(s[pos:])
		if loc == nil {
			break
		}
		headEnd := pos + loc[1]
		end, repl := scanValue(s, headEnd)
		if end == headEnd { // 空值：不替换，从键之后继续
			out.WriteString(s[pos:headEnd])
			pos = headEnd
			continue
		}
		out.WriteString(s[pos:headEnd])
		out.WriteString(repl)
		pos = end
	}
	if pos == 0 {
		return s
	}
	out.WriteString(s[pos:])
	return out.String()
}

// scanValue 从 s[i:] 起解析一个值，返回值的结束位置与替换文本；空值返回 (i, "")。
func scanValue(s string, i int) (int, string) {
	if i >= len(s) {
		return i, ""
	}
	if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\'') { // JSON 套 JSON：\"…\"
		q := s[i+1 : i+2]
		if k := strings.Index(s[i+2:], "\\"+q); k >= 0 {
			return i + 2 + k + 2, "\\" + q + Mask + "\\" + q
		}
		return len(s), Mask
	}
	switch c := s[i]; c {
	case '"', '\'':
		end := skipQuoted(s, i)
		return end, string(c) + Mask + string(c)
	case '{', '[':
		return skipBalanced(s, i), Mask
	}
	j := i
	if m := reSchemeWord.FindString(s[i:]); m != "" {
		j += len(m)
	}
	start := j
	for j < len(s) && !isValueDelim(s[j]) {
		j++
	}
	if j == start { // 只有前缀词、没有凭据本体：把前缀词之后的内容视为空
		return i, ""
	}
	return j, Mask
}

func isValueDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '"', '\'', ',', ';', '&', '}', ']', ')':
		return true
	}
	return false
}

// skipQuoted 返回带引号字符串（含转义）之后的位置；未闭合则到本行末（不跨行，避免吞掉后续日志内容）。
func skipQuoted(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case q:
			return j + 1
		case '\n':
			return j
		}
	}
	return len(s)
}

// skipBalanced 返回与 s[i]（{ 或 [）配对的闭合符之后的位置，识别字符串内的括号；不配对则到结尾。
func skipBalanced(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '"', '\'':
			j = skipQuoted(s, j) - 1
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(s)
}

// Bytes 是 String 的字节切片版本（返回新切片）。
func Bytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	return []byte(String(string(b)))
}

// Value 递归清洗由 JSON 解码得到的通用结构（map[string]any、[]any、string 及标量）。
// map 键敏感时整个值替换为 Mask（无论其类型）；其余字符串按 String 清洗；标量原样返回。
// 其他具体类型（如 map[string]string、[]string）同样处理；无法识别的类型转为其 %v 文本后清洗（宁可降级为字符串也不原样透传）。
func Value(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return stringOrEncoded(x)
	case []byte:
		return String(string(x))
	case json.Number:
		return x
	case bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if Key(k) {
				out[k] = Mask
				continue
			}
			out[k] = Value(val)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if Key(k) {
				out[k] = Mask
				continue
			}
			out[k] = String(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = Value(val)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = String(val)
		}
		return out
	default:
		return String(fmt.Sprint(x))
	}
}

// stringOrEncoded 清洗字符串；若本身无敏感形态但整体是 base64（[]byte 经 JSON 往返的典型形态），再解码检查，
// 解码结果含敏感内容则整体替换为 Mask。
func stringOrEncoded(s string) string {
	if out := String(s); out != s {
		return out
	}
	if len(s) < 16 || len(s) > 1<<16 {
		return s
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if txt := string(b); utf8.ValidString(txt) && String(txt) != txt {
				return Mask
			}
			break
		}
	}
	return s
}
