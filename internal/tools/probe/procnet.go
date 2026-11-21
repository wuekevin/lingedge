package probe

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

// ProcNetEntry 是 /proc/net/{tcp,tcp6,udp,udp6} 的一行。
type ProcNetEntry struct {
	Proto string // tcp | udp（tcp6/udp6 归一，Addr 的 IPv6 形态保留）
	Addr  string
	Port  int
	UID   int
	Inode uint64
}

// ParseProcNet 解析一个 /proc/net/{tcp,tcp6,udp,udp6} 的内容。proto 为 "tcp" 或 "udp"，v6 表示该文件是 tcp6/udp6。
// littleEndianHost 指明内核所在主机的字节序：/proc/net 中的 IPv4 地址是「按主机字节序打印的 32 位整数」，IPv6 是四个这样的 32 位字——
// 小端主机上 127.0.0.1 写成 0100007F，::1 写成 00000000000000000000000001000000；端口始终是大端十六进制（0050 = 80）。
// 只返回监听项：TCP 状态 0A（LISTEN）；UDP 状态 07 且远端地址全零（已绑定未连接）。
func ParseProcNet(data []byte, proto string, v6, littleEndianHost bool) (entries []ProcNetEntry, skipped int) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	first := true
	for sc.Scan() {
		if first { // 表头
			first = false
			if strings.Contains(sc.Text(), "local_address") {
				continue
			}
		}
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		// sl local rem st tx:rx tr:tm->when retrnsmt uid timeout inode ...
		if len(f) < 10 {
			skipped++
			continue
		}
		addr, port, ok := decodeHexAddr(f[1], v6, littleEndianHost)
		if !ok {
			skipped++
			continue
		}
		st := strings.ToUpper(f[3])
		switch proto {
		case "tcp":
			if st != "0A" {
				continue
			}
		case "udp":
			if st != "07" {
				continue
			}
			if ra, rp, ok := decodeHexAddr(f[2], v6, littleEndianHost); !ok || rp != 0 || !ra.IsUnspecified() {
				continue
			}
		default:
			continue
		}
		uid, err1 := strconv.Atoi(f[7])
		inode, err2 := strconv.ParseUint(f[9], 10, 64)
		if err1 != nil || err2 != nil {
			skipped++
			continue
		}
		entries = append(entries, ProcNetEntry{Proto: proto, Addr: addr.String(), Port: port, UID: uid, Inode: inode})
	}
	return entries, skipped
}

func decodeHexAddr(s string, v6, le bool) (netip.Addr, int, bool) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return netip.Addr{}, 0, false
	}
	ph := s[i+1:]
	if len(ph) != 4 {
		return netip.Addr{}, 0, false
	}
	pn, err := strconv.ParseUint(ph, 16, 16)
	if err != nil {
		return netip.Addr{}, 0, false
	}
	ah := s[:i]
	want := 8
	if v6 {
		want = 32
	}
	if len(ah) != want {
		return netip.Addr{}, 0, false
	}
	raw, err := hex.DecodeString(ah)
	if err != nil {
		return netip.Addr{}, 0, false
	}
	var bo binary.ByteOrder = binary.BigEndian
	if le {
		bo = binary.LittleEndian
	}
	b := make([]byte, len(raw))
	for w := 0; w < len(raw); w += 4 {
		val := binary.BigEndian.Uint32(raw[w:]) // 文本即数值（十六进制数字从高位写起）；再按主机字节序展开成内存字节 = 网络序
		bo.PutUint32(b[w:], val)
	}
	var a netip.Addr
	if v6 {
		a = netip.AddrFrom16([16]byte(b))
	} else {
		a = netip.AddrFrom4([4]byte(b))
	}
	return a, int(pn), true
}

// ToSocket 把一行转换为 Socket（占用进程待补）。
func (e ProcNetEntry) ToSocket() Socket {
	return Socket{Source: "proc", UID: e.UID, Hidden: true,
		Listener: resolver.Listener{Proto: e.Proto, Addr: e.Addr, Port: e.Port}}
}
