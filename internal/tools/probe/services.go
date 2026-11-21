package probe

import (
	"bufio"
	"bytes"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/baota"
)

// serviceTable：systemd 服务名（去掉 .service）→ 规范名与角色。Role：dns | resolver | web | db | cache | firewall。
var serviceTable = map[string]struct{ name, role string }{
	"named": {"named", "dns"}, "named-chroot": {"named", "dns"}, "bind9": {"named", "dns"},
	"dnsmasq": {"dnsmasq", "dns"}, "unbound": {"unbound", "dns"}, "pdns": {"pdns", "dns"}, "knot": {"knot", "dns"}, "coredns": {"coredns", "dns"},
	"systemd-resolved": {"systemd-resolved", "resolver"},
	"nginx":            {"nginx", "web"}, "openresty": {"openresty", "web"}, "httpd": {"httpd", "web"}, "apache2": {"apache2", "web"}, "caddy": {"caddy", "web"},
	"mysqld": {"mysqld", "db"}, "mysql": {"mysqld", "db"}, "mariadb": {"mariadb", "db"}, "mariadbd": {"mariadb", "db"}, "postgresql": {"postgresql", "db"},
	"redis": {"redis", "cache"}, "redis-server": {"redis", "cache"}, "redis6": {"redis", "cache"}, "valkey": {"valkey", "cache"},
	"firewalld": {"firewalld", "firewall"}, "ufw": {"ufw", "firewall"}, "iptables": {"iptables", "firewall"}, "nftables": {"nftables", "firewall"},
}

// ParseSystemctlUnits 解析 `systemctl list-units --type=service --all --no-legend --no-pager --plain`：
//
//	nginx.service            loaded    active   running A high performance web server
//	named.service            loaded    inactive dead    BIND Domain Name Server
//	foo.service              not-found inactive dead    foo.service
//
// 只返回 serviceTable 中识别的服务；load 状态为 not-found 的悬空引用被忽略。同一规范名取「active 优先」。
func ParseSystemctlUnits(out []byte) []baota.Service {
	best := map[string]baota.Service{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimLeft(strings.TrimSpace(sc.Text()), "●*○× ") // 兼容未带 --plain 的失败标记
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		unit := f[0]
		if f[1] == "not-found" {
			continue
		}
		base := strings.TrimSuffix(unit, ".service")
		ent, ok := serviceTable[base]
		if !ok {
			continue
		}
		svc := baota.Service{Name: ent.name, Unit: unit, Active: f[2], Role: ent.role}
		if prev, dup := best[ent.name]; dup && prev.Active == "active" {
			continue
		}
		best[ent.name] = svc
	}
	out2 := make([]baota.Service, 0, len(best))
	for _, s := range best {
		out2 = append(out2, s)
	}
	sort.Slice(out2, func(i, j int) bool { return out2[i].Name < out2[j].Name })
	return out2
}
