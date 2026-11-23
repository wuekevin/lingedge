package installer

import (
	"fmt"
	"path"
	"strings"
)

// Nginx 片段的静态检查（Story 1.40 AC5、tools-design §11.3「安全头继承规则」）。
//
// nginx 只要某个 location 内出现任何一条 add_header，就不再继承上级（server/http）的 add_header。因此生成的片段必须：
//  1. 把全部安全头集中在 /etc/lingedge/nginx/<c>-security-headers.conf，且每条都带 always；
//  2. 在每一个含 add_header 的 location 内显式 include 该文件；
//  3. server 级不出现 add_header（不依赖继承）。
//
// 会话 Cookie 一律 host-only（ADR-018）：片段不得改写 Cookie 的 Domain（proxy_cookie_domain）或自行下发 Set-Cookie。
// LintNginxSnippet 对任何 nginx 片段做上述检查，既是生成时的自检（渲染器不得产出违规片段），也是 CI 的静态检查入口
// （tests/security/nginxsnippet 对全部组件 × 变体组合的生成结果调用它，并带负向样本）。

type nginxDirective struct {
	name  string
	args  []string
	block []nginxDirective
	hasB  bool
}

type nginxTok struct {
	kind byte // w 词，{ } ;
	s    string
}

func nginxTokenize(src string) ([]nginxTok, error) {
	var out []nginxTok
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		switch c {
		case ' ', '\t', '\r', '\n':
			i++
		case '#':
			for i < n && src[i] != '\n' {
				i++
			}
		case '{', '}', ';':
			out = append(out, nginxTok{kind: c})
			i++
		case '"', '\'':
			q := c
			i++
			var b strings.Builder
			for i < n && src[i] != q {
				if src[i] == '\\' && i+1 < n {
					i++
				}
				b.WriteByte(src[i])
				i++
			}
			if i >= n {
				return nil, fmt.Errorf("引号未闭合")
			}
			i++
			out = append(out, nginxTok{kind: 'w', s: b.String()})
		default:
			j := i
			for j < n && !strings.ContainsRune(" \t\r\n{};#", rune(src[j])) {
				j++
			}
			out = append(out, nginxTok{kind: 'w', s: src[i:j]})
			i = j
		}
	}
	return out, nil
}

func parseNginx(src string) ([]nginxDirective, error) {
	toks, err := nginxTokenize(src)
	if err != nil {
		return nil, err
	}
	pos := 0
	var parse func(depth int) ([]nginxDirective, error)
	parse = func(depth int) ([]nginxDirective, error) {
		var out []nginxDirective
		for pos < len(toks) {
			t := toks[pos]
			switch t.kind {
			case '}':
				if depth == 0 {
					return nil, fmt.Errorf("多余的 }")
				}
				pos++
				return out, nil
			case ';':
				pos++ // 空语句
			case '{':
				return nil, fmt.Errorf("{ 前缺少指令名")
			default:
				d := nginxDirective{name: t.s}
				pos++
				for pos < len(toks) && toks[pos].kind == 'w' {
					d.args = append(d.args, toks[pos].s)
					pos++
				}
				if pos >= len(toks) {
					return nil, fmt.Errorf("指令 %s 未以 ; 或 { 结束", d.name)
				}
				switch toks[pos].kind {
				case ';':
					pos++
				case '{':
					pos++
					d.hasB = true
					blk, err := parse(depth + 1)
					if err != nil {
						return nil, err
					}
					d.block = blk
				default:
					return nil, fmt.Errorf("指令 %s 后出现意外的 }", d.name)
				}
				out = append(out, d)
			}
		}
		if depth > 0 {
			return nil, fmt.Errorf("缺少 }")
		}
		return out, nil
	}
	return parse(0)
}

// LintNginxSnippet 检查生成的站点片段 conf（headersFile 是安全头文件的绝对路径）。返回全部违规（空 = 通过）。
func LintNginxSnippet(conf, headersFile string) []string {
	root, err := parseNginx(conf)
	if err != nil {
		return []string{"片段无法解析：" + err.Error()}
	}
	var bad []string
	var walk func(ds []nginxDirective, loc string, inServer bool)
	walk = func(ds []nginxDirective, loc string, inServer bool) {
		for _, d := range ds {
			switch d.name {
			case "server":
				if d.hasB {
					walk(d.block, "", true)
				}
			case "location":
				if !d.hasB {
					continue
				}
				name := "location " + strings.Join(d.args, " ")
				hasAdd, hasInc := false, false
				for _, c := range d.block {
					switch c.name {
					case "add_header", "add_trailer":
						hasAdd = true
						if len(c.args) > 0 && strings.EqualFold(c.args[0], "set-cookie") {
							bad = append(bad, name+"：不得自行下发 Set-Cookie（会话 Cookie 只由 Server 下发，host-only）")
						}
					case "include":
						if len(c.args) == 1 && path.Clean(c.args[0]) == headersFile {
							hasInc = true
						}
					}
				}
				if hasAdd && !hasInc {
					bad = append(bad, name+"：含 add_header 但没有显式 include "+headersFile+"（add_header 不继承，安全头会丢失）")
				}
				walk(d.block, name, inServer)
			case "add_header", "add_trailer":
				if loc == "" && inServer {
					bad = append(bad, "server 级出现 add_header（不得依赖继承，安全头应放在 include 的头文件中）")
				}
			case "proxy_cookie_domain":
				if len(d.args) != 1 || d.args[0] != "off" {
					bad = append(bad, "proxy_cookie_domain 改写了 Cookie 的 Domain（会话 Cookie 必须 host-only）")
				}
			default:
				if d.hasB {
					walk(d.block, loc, inServer)
				}
			}
		}
	}
	walk(root, "", false)
	return bad
}

// LintSecurityHeaders 检查安全头文件：只含 add_header，且每条都带 always。
func LintSecurityHeaders(conf string) []string {
	root, err := parseNginx(conf)
	if err != nil {
		return []string{"安全头文件无法解析：" + err.Error()}
	}
	var bad []string
	n := 0
	for _, d := range root {
		if d.name != "add_header" {
			bad = append(bad, "安全头文件只允许 add_header，发现 "+d.name)
			continue
		}
		n++
		if len(d.args) != 3 || d.args[2] != "always" {
			bad = append(bad, "add_header "+firstArg(d.args)+" 缺少 always（错误响应也必须带安全头）")
		}
	}
	if n == 0 {
		bad = append(bad, "安全头文件没有任何 add_header")
	}
	return bad
}

func firstArg(a []string) string {
	if len(a) == 0 {
		return "(空)"
	}
	return a[0]
}
