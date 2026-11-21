package baota

import (
	"errors"
	"strconv"
	"strings"
)

// Nginx 配置只读解析（用于预警同名域名/端口冲突，tools-design §11「站点配置」）。
// 只做词法 + 块结构：注释、引号、转义、多 listen、嵌套块；畸形输入不 panic，返回已解析部分与 error。
// 不处理 include（不跟随文件引用，避免读到范围外的文件），也不求值变量。

const (
	maxConfBytes   = 1 << 20
	maxConfDepth   = 64
	maxConfTokens  = 200000
	maxServers     = 1000
	maxNamesPerSrv = 256
)

// Listen 是一条 listen 指令的解析结果。
type Listen struct {
	Addr    string `json:"addr"` // IP 字面量；通配为 "*"（nginx 的 `listen 80;` 与 `listen *:80;`）；IPv6 不带方括号
	Port    int    `json:"port"`
	SSL     bool   `json:"ssl,omitempty"`
	Default bool   `json:"default_server,omitempty"`
	Unix    string `json:"unix,omitempty"` // listen unix:/path 时的套接字路径（此时 Port=0）
	UDP     bool   `json:"udp,omitempty"`  // quic / udp 参数（listen 443 quic）
}

// Site 是一个 server 块。
type Site struct {
	File        string   `json:"file"`
	Source      string   `json:"source"` // baota | system
	ServerNames []string `json:"server_names"`
	Listens     []Listen `json:"listens"`
	SSL         bool     `json:"ssl,omitempty"` // 任一 listen 带 ssl，或有 ssl_certificate 指令（只记有无，不读证书）
	Malformed   bool     `json:"malformed,omitempty"`
}

type directive struct {
	name  string
	args  []string
	block []directive
	hasB  bool
}

type tok struct {
	kind byte // w 词，{ } ;
	s    string
}

var errUnterminatedQuote = errors.New("baota: nginx 配置引号未闭合")

// tokenize 词法分析。遇到词法错误时返回已得到的 token 与错误。
func tokenize(b []byte) ([]tok, error) {
	var out []tok
	i, n := 0, len(b)
	for i < n {
		if len(out) >= maxConfTokens {
			return out, errors.New("baota: nginx 配置 token 过多")
		}
		c := b[i]
		switch c {
		case ' ', '\t', '\r', '\n':
			i++
		case '#':
			for i < n && b[i] != '\n' {
				i++
			}
		case '{', '}', ';':
			out = append(out, tok{kind: c})
			i++
		case '"', '\'':
			q := c
			i++
			var sb strings.Builder
			closed := false
			for i < n {
				d := b[i]
				if d == '\\' && i+1 < n {
					sb.WriteByte(b[i+1])
					i += 2
					continue
				}
				if d == q {
					closed = true
					i++
					break
				}
				sb.WriteByte(d)
				i++
			}
			if !closed {
				return out, errUnterminatedQuote
			}
			out = append(out, tok{kind: 'w', s: sb.String()})
		default:
			j := i
			for j < n {
				d := b[j]
				if d == ' ' || d == '\t' || d == '\r' || d == '\n' || d == ';' || d == '{' || d == '}' || d == '#' {
					break
				}
				j++
			}
			out = append(out, tok{kind: 'w', s: string(b[i:j])})
			i = j
		}
	}
	return out, nil
}

// parseBlock 把 token 解析成指令树。畸形（缺 ;、括号不配对、过深）返回已解析部分与 error。
func parseTree(toks []tok) ([]directive, error) {
	pos := 0
	var soft error // 缺少 ; 之类：指令仍被保留，但整体视为畸形
	var parse func(depth int) ([]directive, error)
	parse = func(depth int) ([]directive, error) {
		if depth > maxConfDepth {
			return nil, errors.New("baota: nginx 配置嵌套过深")
		}
		var ds []directive
		var cur []string
		for pos < len(toks) {
			t := toks[pos]
			pos++
			switch t.kind {
			case 'w':
				cur = append(cur, t.s)
			case ';':
				if len(cur) > 0 {
					ds = append(ds, directive{name: cur[0], args: cur[1:]})
				}
				cur = nil
			case '{':
				if len(cur) == 0 {
					return ds, errors.New("baota: nginx 配置出现没有名字的块")
				}
				blk, err := parse(depth + 1)
				ds = append(ds, directive{name: cur[0], args: cur[1:], block: blk, hasB: true})
				cur = nil
				if err != nil {
					return ds, err
				}
			case '}':
				if depth == 0 {
					return ds, errors.New("baota: nginx 配置多余的 }")
				}
				if len(cur) > 0 { // 缺少 ;，仍保留这条指令
					ds = append(ds, directive{name: cur[0], args: cur[1:]})
					soft = errors.New("baota: nginx 配置在 } 之前缺少 ;")
				}
				return ds, nil
			}
		}
		if depth > 0 {
			return ds, errors.New("baota: nginx 配置缺少 }")
		}
		if len(cur) > 0 {
			ds = append(ds, directive{name: cur[0], args: cur[1:]})
			return ds, errors.New("baota: nginx 配置末尾缺少 ;")
		}
		return ds, nil
	}
	ds, err := parse(0)
	if err == nil {
		err = soft
	}
	return ds, err
}

// ParseVhost 解析一个 nginx 配置文件内容，返回其中全部 server 块（任意嵌套层）。
// 畸形配置：返回能解析出的 server 块且 malformed=true，error 说明原因；永不 panic。
func ParseVhost(file, source string, data []byte) (sites []Site, err error) {
	defer func() {
		if r := recover(); r != nil { // 纵深防御：解析器不应 panic，但探测不能因配置畸形而崩溃
			sites, err = nil, errors.New("baota: nginx 配置解析异常")
		}
	}()
	if len(data) > maxConfBytes {
		data = data[:maxConfBytes]
		err = errors.New("baota: nginx 配置过大，已截断")
	}
	toks, lexErr := tokenize(data)
	tree, parseErr := parseTree(toks)
	if lexErr != nil {
		err = lexErr
	} else if parseErr != nil {
		err = parseErr
	}
	var walk func(ds []directive)
	walk = func(ds []directive) {
		for _, d := range ds {
			if len(sites) >= maxServers {
				return
			}
			if d.name == "server" && d.hasB {
				sites = append(sites, buildSite(file, source, d.block, err != nil))
				continue
			}
			if d.hasB {
				walk(d.block)
			}
		}
	}
	walk(tree)
	return sites, err
}

func buildSite(file, source string, blk []directive, malformed bool) Site {
	s := Site{File: file, Source: source, Malformed: malformed, ServerNames: []string{}, Listens: []Listen{}}
	for _, d := range blk {
		if d.hasB {
			continue // location 等子块里的 listen/server_name 无意义
		}
		switch d.name {
		case "server_name":
			for _, a := range d.args {
				if a == "" || len(s.ServerNames) >= maxNamesPerSrv {
					continue
				}
				s.ServerNames = append(s.ServerNames, a)
			}
		case "listen":
			if l, ok := parseListen(d.args); ok {
				s.Listens = append(s.Listens, l)
				if l.SSL {
					s.SSL = true
				}
			}
		case "ssl_certificate":
			s.SSL = true
		case "ssl":
			if len(d.args) == 1 && d.args[0] == "on" {
				s.SSL = true
			}
		}
	}
	return s
}

func parseListen(args []string) (Listen, bool) {
	if len(args) == 0 {
		return Listen{}, false
	}
	var l Listen
	spec := args[0]
	switch {
	case strings.HasPrefix(spec, "unix:"):
		l.Unix = strings.TrimPrefix(spec, "unix:")
	case strings.HasPrefix(spec, "["): // [::]:443 或 [::1]
		end := strings.IndexByte(spec, ']')
		if end < 0 {
			return Listen{}, false
		}
		l.Addr = spec[1:end]
		rest := spec[end+1:]
		if rest == "" {
			l.Port = 80
		} else if strings.HasPrefix(rest, ":") {
			p, ok := atoiPort(rest[1:])
			if !ok {
				return Listen{}, false
			}
			l.Port = p
		} else {
			return Listen{}, false
		}
	case strings.Contains(spec, ":"): // 1.2.3.4:80 或 *:80
		i := strings.LastIndexByte(spec, ':')
		l.Addr = spec[:i]
		p, ok := atoiPort(spec[i+1:])
		if !ok {
			return Listen{}, false
		}
		l.Port = p
	default:
		if p, ok := atoiPort(spec); ok { // 纯端口
			l.Addr, l.Port = "*", p
		} else if spec == "*" || looksLikeAddr(spec) { // 仅地址：端口缺省 80
			l.Addr, l.Port = spec, 80
		} else {
			return Listen{}, false
		}
	}
	if l.Addr == "" && l.Unix == "" {
		l.Addr = "*"
	}
	for _, a := range args[1:] {
		switch a {
		case "ssl":
			l.SSL = true
		case "default_server", "default":
			l.Default = true
		case "quic", "udp":
			l.UDP = true
		}
	}
	return l, true
}

func looksLikeAddr(s string) bool {
	return strings.Count(s, ".") == 3 && strings.Trim(s, "0123456789.") == ""
}

func atoiPort(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}
