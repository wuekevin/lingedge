package probe

import (
	"bufio"
	"bytes"
	"net/netip"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

// ProcRef 是占用一个套接字的进程（ss 的 users:(("nginx",pid=1,fd=6),…) 中的一项）。
type ProcRef struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
}

// Socket 是一个本机监听及其全部占用进程。Listener 是 resolver 使用的形状（占用者取 pid 最小的一项，通常是 master 进程）。
type Socket struct {
	Listener resolver.Listener `json:"listener"`
	Procs    []ProcRef         `json:"procs,omitempty"`
	// Hidden：占用进程信息不可见（ss 无 users 列：无权限看别人的进程，或 /proc/<pid>/fd 不可读）。
	Hidden bool   `json:"hidden,omitempty"`
	Iface  string `json:"iface,omitempty"` // 地址的 %lo 之类的接口后缀
	UID    int    `json:"uid,omitempty"`   // /proc/net/* 才有的属主 uid；-1 未知
	Source string `json:"source"`          // ss | proc
}

const maxSockets = 100000

// ParseSS 解析 `ss -H -lntup` 的输出（-H 无表头；列：Netid State Recv-Q Send-Q Local:Port Peer:Port [users:(...)]）。
// 无法解析的行被跳过并计入 skipped（不会 panic，也不会因一行畸形丢掉其余行）。
func ParseSS(out []byte) (socks []Socket, skipped int) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if len(socks) >= maxSockets {
			skipped++
			continue
		}
		s, ok := parseSSLine(line)
		if !ok {
			skipped++
			continue
		}
		socks = append(socks, s)
	}
	return socks, skipped
}

func parseSSLine(line string) (Socket, bool) {
	f := strings.Fields(line)
	// 最少：Netid State Recv-Q Send-Q Local Peer
	if len(f) < 6 {
		return Socket{}, false
	}
	netid := strings.ToLower(f[0])
	if netid != "tcp" && netid != "udp" {
		return Socket{}, false
	}
	state := strings.ToUpper(f[1])
	if (netid == "tcp" && state != "LISTEN") || (netid == "udp" && state != "UNCONN") {
		return Socket{}, false // -l 只列监听；UDP 的「监听」是 UNCONN
	}
	if _, err := strconv.ParseUint(f[2], 10, 64); err != nil {
		return Socket{}, false
	}
	if _, err := strconv.ParseUint(f[3], 10, 64); err != nil {
		return Socket{}, false
	}
	addr, iface, port, ok := splitHostPort(f[4])
	if !ok {
		return Socket{}, false
	}
	s := Socket{Source: "ss", UID: -1, Iface: iface,
		Listener: resolver.Listener{Proto: netid, Addr: addr, Port: port}}
	// users:(...) 可能含空格（进程名带空格时被拆成多个字段），因此从原行切出
	if i := strings.Index(line, "users:("); i >= 0 {
		s.Procs = parseUsers(line[i+len("users:"):])
	}
	if len(s.Procs) == 0 {
		s.Hidden = true
	} else {
		best := s.Procs[0]
		for _, p := range s.Procs {
			if p.PID < best.PID {
				best = p
			}
		}
		s.Listener.PID, s.Listener.Process = best.PID, best.Name
	}
	return s, true
}

// splitHostPort 解析 ss 的本地地址列：`0.0.0.0:80`、`*:8080`、`[::]:22`、`[::1]%lo:323`、`127.0.0.53%lo:53`。
// 返回的 addr：通配归一为 0.0.0.0（`*` 在 ss 里表示该协议族的任意地址，双栈 socket 上是 IPv4+IPv6 全部，保守按 `::` 处理）。
func splitHostPort(s string) (addr, iface string, port int, ok bool) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return "", "", 0, false
	}
	host, ps := s[:i], s[i+1:]
	p, err := strconv.Atoi(ps)
	if err != nil || p < 0 || p > 65535 {
		return "", "", 0, false
	}
	if strings.HasPrefix(host, "[") { // [::1]、[fe80::1]%eth0（接口后缀在括号外）、[fe80::1%eth0]
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return "", "", 0, false
		}
		rest := host[end+1:]
		host = host[1:end]
		if strings.HasPrefix(rest, "%") {
			iface = rest[1:]
		} else if rest != "" {
			return "", "", 0, false
		}
	}
	if j := strings.IndexByte(host, '%'); j >= 0 {
		host, iface = host[:j], host[j+1:]
	}
	if host == "*" {
		return "*", iface, p, true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", "", 0, false
	}
	return ip.String(), iface, p, true
}

// parseUsers 解析 `(("nginx",pid=1234,fd=6),("nginx",pid=1235,fd=6))`。
// 进程名可含空格、括号、逗号；ss 把不可打印字符写成 \xNN，并把 " 与 \ 写成 \" 与 \\。
func parseUsers(s string) []ProcRef {
	var out []ProcRef
	i := strings.IndexByte(s, '(')
	if i < 0 {
		return nil
	}
	i++
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if i >= len(s) || s[i] == ')' {
			break
		}
		if s[i] != '(' || i+1 >= len(s) || s[i+1] != '"' {
			break
		}
		i += 2
		var name strings.Builder
		closed := false
		for i < len(s) {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				if s[i+1] == 'x' && i+3 < len(s) {
					if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
						name.WriteByte(byte(v))
						i += 4
						continue
					}
				}
				name.WriteByte(s[i+1])
				i += 2
				continue
			}
			if c == '"' {
				closed = true
				i++
				break
			}
			name.WriteByte(c)
			i++
		}
		if !closed {
			break
		}
		// 之后是 ,pid=N,fd=M[,…]) ——以匹配的 ')' 结束（名字已经在引号里读完，此处不会再有括号歧义）
		end := strings.IndexByte(s[i:], ')')
		if end < 0 {
			break
		}
		attrs := s[i : i+end]
		i += end + 1
		pid := -1
		for _, a := range strings.Split(attrs, ",") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(a), "pid="); ok {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					pid = n
				}
			}
		}
		if pid > 0 {
			out = append(out, ProcRef{PID: pid, Name: name.String()})
		}
	}
	return out
}
