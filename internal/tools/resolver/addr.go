package resolver

import (
	"net/netip"
	"strings"
)

// 地址与端口冲突语义（tools-design §10.4：同地址冲突与通配冲突）。
//
//	0.0.0.0 对任意 IPv4 地址冲突（含 127.0.0.1）；:: 在 Linux 默认双栈，对任意地址（IPv4 与 IPv6）冲突；
//	0.0.0.0 对 IPv6 具体地址（如 ::1）不冲突；具体地址之间仅「完全相同」才冲突（127.0.0.1 与 127.0.0.2 不冲突）。
//	IPv4 映射地址（::ffff:a.b.c.d）按 IPv4 处理。空串与 * 视为全通配（最保守）。
type bindAddr struct {
	any   bool // 全通配（"" "*" "::" "[::]"）：与一切冲突
	wild4 bool // 0.0.0.0：与一切 IPv4 冲突
	ip    netip.Addr
}

func parseBind(s string) (bindAddr, bool) {
	s = strings.TrimSpace(s)
	switch s {
	case "", "*", "::", "[::]":
		return bindAddr{any: true}, true
	case "0.0.0.0":
		return bindAddr{wild4: true}, true
	}
	ip, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"))
	if err != nil {
		return bindAddr{}, false
	}
	ip = ip.Unmap()
	if ip.IsUnspecified() {
		if ip.Is4() {
			return bindAddr{wild4: true}, true
		}
		return bindAddr{any: true}, true
	}
	return bindAddr{ip: ip}, true
}

// overlap 报告两个绑定地址（同协议同端口）是否冲突，并返回冲突类别：exact | wildcard。
func overlap(a, b bindAddr) (bool, string) {
	switch {
	case a.any && b.any, a.any && b.wild4, a.wild4 && b.any, a.wild4 && b.wild4:
		return true, "wildcard"
	case a.any || b.any:
		return true, "wildcard"
	case a.wild4:
		return b.ip.Is4(), "wildcard"
	case b.wild4:
		return a.ip.Is4(), "wildcard"
	}
	return a.ip == b.ip, "exact"
}

func normProto(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "tcp6":
		return "tcp"
	case "udp6":
		return "udp"
	}
	return p
}

// AddrConflict 报告两个绑定地址（同协议同端口的前提下）是否冲突，并给出类别 exact | wildcard。
// 它是 overlap 的导出封装，供 Story 1.31 的 probe.Assess 复用同一套语义（不另造一套）；无法解析的地址按全通配处理（最保守）。
func AddrConflict(a, b string) (conflict bool, kind string) {
	x, ok := parseBind(a)
	if !ok {
		x = bindAddr{any: true}
	}
	y, ok := parseBind(b)
	if !ok {
		y = bindAddr{any: true}
	}
	return overlap(x, y)
}

// NormProto 把 tcp6/udp6 归一为 tcp/udp。
func NormProto(p string) string { return normProto(p) }
