package main

import (
	"bytes"
	"regexp"
	"strings"
)

// 固定口令扫描（G-6、NFR-6）：代码、配置模板、迁移、文档中不得出现默认/固定口令模式。
// 唯一 allowlist：tests/fixtures/ 下且文件内含「仅测试」标注的文件（ADR-023 §A.3）。
// 本门禁自身的规则文本位于 scripts/gates（被扫描器跳过），测试用例的违规样例在运行时拼接生成。

const fixtureMarker = "仅测试"

var weakWords = `admin123|admin@123|password123|passw0rd|p@ssw0rd|changeme|change_me|letmein|qwerty123|root123|test123|12345678|123456|111111|000000|888888`

// 占位符/引用（不是固定口令）：环境变量、模板变量、掩码、空值、说明性文字。
var rePlaceholder = regexp.MustCompile(`(?i)^(\$\{?\w+\}?|<[^>]*>|\{\{.*\}\}|%[sdvq]|\*+|\.{3}|…|\[?redacted\]?|null|none|nil|true|false|env:\w+|file:/\S+|vault:\S+|\$\(.*\))$`)

type pwRule struct {
	name string
	re   *regexp.Regexp
	// literalGroup>0：该捕获组为口令字面量，需排除占位符；0：命中即违规。
	literalGroup int
	// 仅在 md 之外的文件生效（文档中的叙述性示例不含可执行语义）。
	nonDocOnly bool
	// 仅对配置/脚本/环境文件生效（YAML、.env、ini、systemd、shell 等），Go 代码另有引号规则。
	configOnly bool
}

var pwRules = []pwRule{
	{
		name: "固定口令赋值（口令/secret 键后跟非空字面量）",
		re:   regexp.MustCompile("(?i)(?:^|[^A-Za-z0-9])[\\w.-]*(?:password|passwd|passphrase|pwd|secret)[\\w.-]*[\"']?\\s*(?::=|=>|==|=|:)\\s*(?:\"([^\"\\n]*)\"|'([^'\\n]*)'|`([^`\\n]*)`)"),
		// 字面量可能在组 1/2/3 中，由 checkLiteral 处理
		literalGroup: -1,
		nonDocOnly:   true,
	},
	{
		// 无引号形态：.env / ini / YAML / systemd Environment= / shell export，如 `DB_PASSWORD=hunter2`、`password: hunter2`。
		name:         "固定口令赋值（无引号值，配置/脚本/环境文件）",
		re:           regexp.MustCompile(`(?im)^[ \t]*(?:export[ \t]+|Environment=)?"?[\w.-]*(?:password|passwd|passphrase|pwd|secret)[\w.-]*"?[ \t]*[:=][ \t]*([^\s"'` + "`" + `$<{#\[*][^\s#]*)`),
		literalGroup: -2,
		configOnly:   true,
	},
	{
		name: "已知弱口令/默认口令字面量（与口令类关键词同行）",
		re:   regexp.MustCompile(`(?i)(?:password|passwd|pwd|口令|密码|credential|login)[^\n]{0,80}?(?:^|[^A-Za-z0-9])(?:` + weakWords + `)(?:[^A-Za-z0-9]|$)`),
	},
	{
		name: "SQL 中的字面量口令（IDENTIFIED BY '…'）",
		re:   regexp.MustCompile(`(?i)identified\s+(?:with\s+\w+\s+)?by\s+(?:'([^'$%{<\n]+)'|"([^"$%{<\n]+)")`),
	},
	{
		name: "Redis requirepass 字面量",
		re:   regexp.MustCompile(`(?im)^\s*requirepass\s+(?:"[^"$<{%]+"|[^\s$<{%#"]+)`),
	},
	{
		name: "命令行内联口令（--password=… / mysql -p…）",
		re:   regexp.MustCompile(`(?i)(?:--password(?:=|\s+)(?:[^\s$<{%"'-][^\s]*)|\b(?:mysql|mysqladmin|mysqldump)\b[^\n]*\s(?-i:-p[A-Za-z0-9])[^\s]*|\bredis-cli\b[^\n]*\s(?-i:-a)\s+[^\s$<{%"'-][^\s]*|\bsshpass\b[^\n]*\s(?-i:-p)\s*[^\s$<{%"'-][^\s]*)`),
	},
	{
		name: "连接串内嵌口令（scheme://user:pass@ 或 user:pass@tcp(）",
		re:   regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]*://[^\s/:@'"]*:[^\s/@'"$<{%*]+@|[\w.-]+:[^\s@/'"$<{%*]+@tcp\()`),
	},
	{
		name: "默认账号口令对（admin/admin、root:root 等）",
		// root:root 是 chown 的属主:属组写法，不属于口令对，故 root 仅匹配 root/<口令> 与 root:<弱口令>。
		re: regexp.MustCompile(`(?i)(?:^|[^/\w-])(?:(?:admin|administrator)\s*[/:]\s*(?:admin|root|123456|password|admin123|pass)|root\s*/\s*(?:root|123456|password|root123|pass)|root\s*:\s*(?:123456|password|root123|pass))(?:[^\w-]|$)`),
	},
}

func runPasswords(root string, _ options) ([]Finding, error) {
	var out []Finding
	err := walkText(root, func(rel string, isDir bool) bool {
		if isDir {
			return false
		}
		base := rel[strings.LastIndex(rel, "/")+1:]
		return base == "go.sum" || base == "pnpm-lock.yaml" || base == "package-lock.json" || base == "yarn.lock"
	}, func(f textFile) {
		if strings.HasPrefix(f.Rel, "tests/fixtures/") && bytes.Contains(f.Content, []byte(fixtureMarker)) {
			return // 唯一 allowlist：tests/fixtures 且标注「仅测试」
		}
		isDoc := strings.HasSuffix(f.Rel, ".md") || strings.HasPrefix(f.Rel, "docs/")
		for _, r := range pwRules {
			if (r.nonDocOnly || r.configOnly) && isDoc {
				continue
			}
			if r.configOnly && !isConfigFile(f.Rel) {
				continue
			}
			for _, m := range r.re.FindAllSubmatchIndex(f.Content, -1) {
				if r.literalGroup != 0 && placeholderLiteral(f.Content, m) {
					continue
				}
				at := m[0]
				if f.Content[at] == '\n' { // 规则以「非字母数字」打头时可能吞掉前一行的换行
					at++
				}
				out = append(out, Finding{File: f.Rel, Line: lineOf(f.Content, at), Rule: "password",
					Msg: "疑似固定/默认口令：" + r.name + "（NFR-6、G-6；仅 tests/fixtures 且标注「仅测试」可豁免）"})
			}
		}
	})
	return out, err
}

func isConfigFile(rel string) bool {
	base := rel[strings.LastIndex(rel, "/")+1:]
	if base == ".env" || strings.HasPrefix(base, ".env.") || base == "Makefile" {
		return true
	}
	for _, ext := range []string{".yaml", ".yml", ".env", ".conf", ".cfg", ".ini", ".toml", ".sh", ".service", ".tmpl", ".properties", ".json"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return false
}

// placeholderLiteral 判断赋值的字面量是否为空/占位符（则不算固定口令）。
func placeholderLiteral(content []byte, m []int) bool {
	for g := 1; g*2+1 < len(m); g++ {
		if m[g*2] < 0 {
			continue
		}
		v := strings.TrimSpace(string(content[m[g*2]:m[g*2+1]]))
		return v == "" || rePlaceholder.MatchString(v)
	}
	return true
}
