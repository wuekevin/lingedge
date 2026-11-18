package migrate

import (
	"strings"
	"unicode"
)

// 轻量 SQL 词法：只服务迁移 lint 与 DDL 预检（不是 SQL 解析器）。注释被丢弃，字符串与反引号标识符整体成词，
// 因此「字符串里的 DROP」「注释里的分号」都不会被误判。语句最终仍由 MySQL 执行，词法只用于静态规则。

type tokKind int

const (
	tWord   tokKind = iota // 未加引号的标识符/关键字/数字
	tQuoted                // `反引号标识符`
	tString                // '字符串' 或 "字符串"
	tPunct                 // 单个标点：( ) , ; . = 等
)

type token struct {
	kind tokKind
	text string // tWord：原文；tQuoted：去掉反引号；tString：去掉引号（不处理转义）；tPunct：该字符
	up   string // tWord 的大写形式（便于比较关键字）
}

func (t token) is(kw string) bool { return t.kind == tWord && t.up == kw }
func (t token) isPunct(p string) bool {
	return t.kind == tPunct && t.text == p
}

// ident 返回标识符的规范化形式（小写；表名/列名在迁移里按大小写不敏感比较）。
func (t token) ident() string { return strings.ToLower(t.text) }

func isWordRune(r rune) bool {
	return r == '_' || r == '$' || r == '@' || unicode.IsLetter(r) || unicode.IsDigit(r) || r > 127
}

// tokenize 把一段 SQL 切成词，丢弃 `-- `/`#`/`/* */` 注释。
func tokenize(s string) []token {
	rs := []rune(s)
	var out []token
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '#':
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
		case c == '-' && i+1 < len(rs) && rs[i+1] == '-' && (i+2 >= len(rs) || unicode.IsSpace(rs[i+2])):
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(rs) && rs[i+1] == '*':
			i += 2
			for i+1 < len(rs) && (rs[i] != '*' || rs[i+1] != '/') {
				i++
			}
			i += 2
		case c == '\'' || c == '"':
			j := i + 1
			var b strings.Builder
			for j < len(rs) {
				if rs[j] == '\\' && j+1 < len(rs) {
					b.WriteRune(rs[j+1])
					j += 2
					continue
				}
				if rs[j] == c {
					if j+1 < len(rs) && rs[j+1] == c {
						b.WriteRune(c)
						j += 2
						continue
					}
					break
				}
				b.WriteRune(rs[j])
				j++
			}
			out = append(out, token{kind: tString, text: b.String()})
			i = j + 1
		case c == '`':
			j := i + 1
			var b strings.Builder
			for j < len(rs) {
				if rs[j] == '`' {
					if j+1 < len(rs) && rs[j+1] == '`' {
						b.WriteRune('`')
						j += 2
						continue
					}
					break
				}
				b.WriteRune(rs[j])
				j++
			}
			out = append(out, token{kind: tQuoted, text: b.String()})
			i = j + 1
		case isWordRune(c):
			j := i
			for j < len(rs) && isWordRune(rs[j]) {
				j++
			}
			w := string(rs[i:j])
			out = append(out, token{kind: tWord, text: w, up: strings.ToUpper(w)})
			i = j
		default:
			out = append(out, token{kind: tPunct, text: string(c)})
			i++
		}
	}
	return out
}

// matchParen 返回 toks[i]（必须是 "("）对应的 ")" 下标；找不到返回 -1。
func matchParen(toks []token, i int) int {
	depth := 0
	for j := i; j < len(toks); j++ {
		switch {
		case toks[j].isPunct("("):
			depth++
		case toks[j].isPunct(")"):
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// splitTopLevel 在括号深度为 0 处按逗号切分。
func splitTopLevel(toks []token) [][]token {
	var out [][]token
	depth, start := 0, 0
	for i, t := range toks {
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
		case t.isPunct(",") && depth == 0:
			out = append(out, toks[start:i])
			start = i + 1
		}
	}
	return append(out, toks[start:])
}

// ---- 语句切分（与 goose 的 SQL 解析语义一致：以分号结尾；StatementBegin/End 之间整体为一条语句）----

// splitStatements 把一段 SQL 文本按分号切成语句；返回的 rest 非空表示末尾有未以分号结束的内容（goose 会拒绝）。
// 每条语句保留其原文（含前导注释）与起始行号（startLine 为 text 第一行的行号），并标出 goose 与 MySQL 会「看到不同东西」的写法：
//   - SameLine：与上一条语句写在同一行（goose 只在行尾分号处切分，会把它们当成一条多语句发送而失败）；
//   - ExecComment：含 `/*!…*/`/`/*M!…*/` 可执行注释（MySQL 会执行其内容，而词法层把它当注释丢弃，规则看不见）；
//   - SplitRisk：引号内的某一行以分号结尾（goose 会在那里把字符串切断）。
func splitStatements(text string, startLine int) (stmts []Statement, unfinished string) {
	rs := []rune(text)
	line := startLine
	termLine := -1
	chunkStart, chunkLine := 0, startLine
	hasContent := false
	var sameLine, execComment, splitRisk bool
	begin := func() {
		if !hasContent {
			hasContent, chunkLine = true, line
			sameLine = termLine == line
		}
	}
	flush := func(end int, terminated bool) {
		raw := string(rs[chunkStart:end])
		if !hasContent {
			return
		}
		if terminated {
			st := newStatement(raw, chunkLine)
			st.SameLine, st.ExecComment, st.SplitRisk = sameLine, execComment, splitRisk
			stmts = append(stmts, st)
		} else {
			unfinished = strings.TrimSpace(raw)
		}
	}
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case c == '\n':
			line++
			i++
		case unicode.IsSpace(c):
			i++
		case c == '#' || (c == '-' && i+1 < len(rs) && rs[i+1] == '-' && (i+2 >= len(rs) || unicode.IsSpace(rs[i+2]))):
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(rs) && rs[i+1] == '*':
			if i+2 < len(rs) && (rs[i+2] == '!' || (rs[i+2] == 'M' && i+3 < len(rs) && rs[i+3] == '!')) {
				begin()
				execComment = true
			}
			i += 2
			for i+1 < len(rs) && (rs[i] != '*' || rs[i+1] != '/') {
				if rs[i] == '\n' {
					line++
				}
				i++
			}
			i += 2
		case c == '\'' || c == '"' || c == '`':
			begin()
			j := i + 1
			for j < len(rs) {
				if rs[j] == '\n' {
					line++
				}
				if rs[j] == ';' && c != '`' && (j+1 < len(rs) && (rs[j+1] == '\n' || (rs[j+1] == '\r' && j+2 < len(rs) && rs[j+2] == '\n'))) {
					splitRisk = true
				}
				if rs[j] == '\\' && c != '`' && j+1 < len(rs) {
					j += 2
					continue
				}
				if rs[j] == c {
					if j+1 < len(rs) && rs[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			i = j + 1
		case c == ';':
			if hasContent {
				flush(i, true)
			}
			i++
			termLine = line
			chunkStart, chunkLine, hasContent = i, line, false
			sameLine, execComment, splitRisk = false, false, false
		default:
			begin()
			i++
		}
	}
	if chunkStart < len(rs) {
		flush(len(rs), false)
	}
	return stmts, unfinished
}

// newStatement 由原文建立 Statement：Lead 为首个有效词之前的注释文本。
func newStatement(raw string, line int) Statement {
	s := Statement{Text: strings.TrimSpace(raw), Line: line}
	s.Lead = leadingComments(raw)
	return s
}

// leadingComments 返回 raw 中第一个非注释字符之前的全部注释文本。
func leadingComments(raw string) string {
	rs := []rune(raw)
	i := 0
	for i < len(rs) {
		switch {
		case unicode.IsSpace(rs[i]):
			i++
		case rs[i] == '#' || (rs[i] == '-' && i+1 < len(rs) && rs[i+1] == '-'):
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
		case rs[i] == '/' && i+1 < len(rs) && rs[i+1] == '*':
			i += 2
			for i+1 < len(rs) && (rs[i] != '*' || rs[i+1] != '/') {
				i++
			}
			i += 2
		default:
			return string(rs[:i])
		}
	}
	return string(rs)
}
