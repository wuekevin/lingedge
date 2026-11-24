package doctor

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/probe"
)

// ---- port.<comp>.<port> ----

func runPorts(ctx context.Context, w *World, f Filter) []Finding {
	var out []Finding
	comps := w.installed(f)
	res, perr := w.probe(ctx)
	for _, c := range comps {
		for _, p := range c.Ports {
			id := fmt.Sprintf("port.%s.%d", c.Name, p)
			x := Finding{ID: id, Component: c.Name}
			switch {
			case perr != nil:
				x.Level, x.Message = LevelSkip, "系统探测不可用："+perr.Error()
			case !res.ListenersKnown:
				x.Level, x.Message = LevelWarn, fmt.Sprintf("无法检测端口 %d 的占用情况（ss 与 /proc 均不可用）", p)
				x.Hint = "以 root 运行 lingedge doctor"
			default:
				x = portFinding(id, c, p, res.Sockets)
			}
			out = append(out, x)
		}
	}
	return out
}

func portFinding(id string, c Component, port int, socks []probe.Socket) Finding {
	x := Finding{ID: id, Component: c.Name}
	var ours, others []probe.Socket
	for _, s := range socks {
		if s.Listener.Port != port {
			continue
		}
		if isOurs(c, s) {
			ours = append(ours, s)
		} else {
			others = append(others, s)
		}
	}
	switch {
	case len(others) > 0:
		s := others[0]
		holder := describeHolder(s)
		x.Level = LevelFail
		x.Message = fmt.Sprintf("%s 端口 %d 被 %s 占用，不是 %s 自己的监听", c.Name, port, holder, c.Name)
		x.Hint = "停止占用者或让 " + c.Name + " 换用其他端口（--set port.… 见 tools-design §12.4）"
		if port == 53 {
			x.Hint = "53 端口常被 systemd-resolved（DNSStubListener）或 named 占用：先停用/改监听地址再启动 dns 组件（D-9）"
		}
		x.Details = map[string]any{"port": port, "proto": s.Listener.Proto, "addr": s.Listener.Addr, "holder": map[string]any{"pid": s.Listener.PID, "name": s.Listener.Process, "user": s.Listener.User, "unit": s.Listener.Unit}}
	case len(ours) > 0:
		x.Level, x.Message = LevelPass, fmt.Sprintf("%s 端口 %d 由 %s 自己监听", c.Name, port, c.Name)
	case c.State == "running":
		x.Level = LevelWarn
		x.Message = fmt.Sprintf("%s 记录为运行中，但端口 %d 没有被监听", c.Name, port)
		x.Hint = "lingedge service status " + c.Name + " 查看服务状态；lingedge logs " + c.Name + " 查看日志"
	default:
		x.Level, x.Message = LevelPass, fmt.Sprintf("端口 %d 未被占用（%s 当前未运行）", port, c.Name)
	}
	return x
}

func isOurs(c Component, s probe.Socket) bool {
	if c.Unit != "" && s.Listener.Unit == c.Unit {
		return true
	}
	return c.RunUser != "" && s.Listener.User == c.RunUser
}

func describeHolder(s probe.Socket) string {
	switch {
	case s.Hidden || (s.Listener.PID == 0 && s.Listener.Process == ""):
		return "未知进程（无权查看占用者，请以 root 运行）"
	case s.Listener.Process == "":
		return fmt.Sprintf("pid %d", s.Listener.PID)
	}
	h := fmt.Sprintf("%s(pid %d", s.Listener.Process, s.Listener.PID)
	if s.Listener.User != "" {
		h += "，用户 " + s.Listener.User
	}
	return h + ")"
}

// ---- baota.* ----

func hasInstalled(w *World, names ...string) bool {
	for _, n := range names {
		if _, ok := w.component(n); ok {
			return true
		}
	}
	return false
}

func runBaota(ctx context.Context, w *World, f Filter) []Finding {
	res, perr := w.probe(ctx)
	var out []Finding
	if perr != nil {
		return []Finding{fnd("baota.nginx_conflict", LevelSkip, "系统探测不可用："+perr.Error(), ""), fnd("baota.firewall", LevelSkip, "系统探测不可用："+perr.Error(), "")}
	}
	out = append(out, baotaNginx(w, res))
	out = append(out, baotaFirewall(w, res))
	return out
}

func baotaNginx(w *World, res probe.Result) Finding {
	id := "baota.nginx_conflict"
	if _, ok := w.component("node"); !ok {
		msg := "本机未安装 node，80/443 不由 Tools 的 OpenResty 占用"
		if res.Baota.Detected {
			msg += "（检测到宝塔面板：静态前端经站点片段/conf.d include 接入宝塔 Nginx，不抢占 80/443）"
		}
		return fnd(id, LevelSkip, msg, "")
	}
	if !res.ListenersKnown {
		return fnd(id, LevelWarn, "无法检测 80/443 的占用情况（ss 与 /proc 均不可用）", "以 root 运行 lingedge doctor")
	}
	c, _ := w.component("node")
	var bad []string
	for _, p := range []int{80, 443} {
		for _, s := range res.Sockets {
			if s.Listener.Port == p && !isOurs(c, s) && !strings.HasPrefix(s.Listener.Unit, "lingedge-") {
				bad = append(bad, fmt.Sprintf("%d 被 %s 占用", p, describeHolder(s)))
				break
			}
		}
	}
	if len(bad) == 0 {
		return fnd(id, LevelPass, "80/443 没有被其他 Web 服务占用", "")
	}
	x := fnd(id, LevelFail, "节点数据面需要的 80/443 被占用："+strings.Join(bad, "；"), "停止占用者，或把 node 与宝塔分机部署（FR-7：宝塔存在本身不是冲突，真实占用才是）")
	if res.Baota.Detected {
		x.Message += "（检测到宝塔面板，占用者通常是宝塔 Nginx）"
		x.Hint = "在宝塔里停用占用 80/443 的站点/Nginx，或把 node 部署到另一台机器；Tools 不会替你停用它"
	}
	return x
}

// 防火墙放行的只读判定：只读取配置文件（ufw 的 user.rules、firewalld 的 zones/*.xml），不执行任何命令。
var (
	reUfwRule  = regexp.MustCompile(`-A ufw-user-input -p (tcp|udp)(?: -m (?:tcp|udp))? --dport (\d+) -j ACCEPT`)
	reFwdPort  = regexp.MustCompile(`<port\s+[^>]*?port="(\d+)(?:-(\d+))?"[^>]*?/?>`)
	reFwdProto = regexp.MustCompile(`protocol="(tcp|udp)"`)
	reFwdSvc   = regexp.MustCompile(`<service\s+name="([a-z0-9-]+)"`)
)

var fwdServicePorts = map[string][]int{"http": {80}, "https": {443}}

// firewallOpen 报告 kind 防火墙是否放行 tcp port：yes | no | unknown。
func firewallOpen(w *World, kind string, port int) string {
	switch kind {
	case "ufw":
		b, err := os.ReadFile(w.path("/etc/ufw/user.rules"))
		if err != nil {
			return "unknown"
		}
		for _, m := range reUfwRule.FindAllStringSubmatch(string(b), -1) {
			if n, _ := strconv.Atoi(m[2]); m[1] == "tcp" && n == port {
				return "yes"
			}
		}
		return "no"
	case "firewalld":
		files, _ := filepath.Glob(w.path("/etc/firewalld/zones/*.xml"))
		if len(files) == 0 {
			return "unknown"
		}
		for _, fl := range files {
			b, err := os.ReadFile(fl)
			if err != nil {
				return "unknown"
			}
			content := string(b)
			for _, m := range reFwdPort.FindAllStringSubmatch(content, -1) {
				lo, _ := strconv.Atoi(m[1])
				hi := lo
				if m[2] != "" {
					hi, _ = strconv.Atoi(m[2])
				}
				pm := reFwdProto.FindStringSubmatch(m[0])
				if (pm == nil || pm[1] == "tcp") && port >= lo && port <= hi {
					return "yes"
				}
			}
			for _, m := range reFwdSvc.FindAllStringSubmatch(content, -1) {
				for _, p := range fwdServicePorts[m[1]] {
					if p == port {
						return "yes"
					}
				}
			}
		}
		return "no"
	}
	return "unknown"
}

// neededPorts 是已装组件需要从外部可达的端口（节点→Server 的 9443/9444，数据面的 80/443）。
func neededPorts(w *World) map[int]string {
	need := map[int]string{}
	if _, ok := w.component("server"); ok && w.ServerListens != nil {
		_, grpc := w.ServerListens()
		if h, p, err := net.SplitHostPort(grpc); err == nil {
			if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
				if n, _ := strconv.Atoi(p); n > 0 {
					need[n] = "server gRPC（节点连接）"
					need[n+1] = "server 节点注册"
				}
			}
		}
	}
	if c, ok := w.component("node"); ok {
		for _, p := range c.Ports {
			if p == 80 || p == 443 {
				need[p] = "node 数据面"
			}
		}
	}
	return need
}

func baotaFirewall(w *World, res probe.Result) Finding {
	id := "baota.firewall"
	need := neededPorts(w)
	if len(need) == 0 {
		return fnd(id, LevelSkip, "没有需要从外部可达的端口（未安装 node，Server gRPC 仅监听回环）", "")
	}
	var active []string
	var blocked, unknown []string
	ports := make([]int, 0, len(need))
	for p := range need {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	for _, fw := range res.Baota.Firewalls {
		if fw.Active != "yes" {
			continue
		}
		active = append(active, fw.Kind)
		for _, p := range ports {
			switch firewallOpen(w, fw.Kind, p) {
			case "no":
				blocked = append(blocked, fmt.Sprintf("%s 未放行 %d（%s）", fw.Kind, p, need[p]))
			case "unknown":
				unknown = append(unknown, fmt.Sprintf("%s/%d", fw.Kind, p))
			}
		}
	}
	d := map[string]any{"ports": ports, "active_firewalls": active}
	cloud := "；云厂商安全组与上游防火墙 Tools 无法读取，请另行确认"
	switch {
	case len(active) == 0:
		x := fnd(id, LevelPass, "未检测到启用的主机防火墙（ufw/firewalld/iptables/nftables）"+cloud, "")
		x.Details = d
		return x
	case len(blocked) > 0:
		x := fnd(id, LevelWarn, strings.Join(blocked, "；"), "放行相应端口（ufw allow <端口>/tcp 或 firewall-cmd --permanent --add-port=<端口>/tcp）；Tools 不会改动防火墙")
		x.Details = d
		return x
	case len(unknown) > 0:
		x := fnd(id, LevelWarn, "防火墙 "+strings.Join(active, "、")+" 已启用，但无法只读判定是否放行："+strings.Join(unknown, "、")+cloud, "手工核对规则（iptables -S / nft list ruleset）；Tools 只读取 ufw 与 firewalld 的配置文件")
		x.Details = d
		return x
	}
	x := fnd(id, LevelPass, "防火墙 "+strings.Join(active, "、")+" 已放行所需端口"+cloud, "")
	x.Details = d
	return x
}

// ---- dep.* ----

func runDeps(ctx context.Context, w *World, f Filter) []Finding {
	cfg := w.cfg()
	_ = cfg
	var out []Finding
	wantServer := f.Component == "" || f.Component == "server"
	res, perr := w.probe(ctx)
	if wantServer {
		if _, ok := w.component("server"); ok || f.Component == "" {
			out = append(out, depMySQL(ctx, w, res, perr)...)
			out = append(out, depRedis(ctx, w, res, perr))
		}
	}
	if (f.Component == "" || f.Component == "website") && hasInstalled(w, "website") {
		out = append(out, depNode(w, res, perr))
	}
	if (f.Component == "" || f.Component == "node") && hasInstalled(w, "node") {
		out = append(out, depOpenresty(ctx, w))
	}
	return out
}

func reachable(res probe.Result, kind string) *probe.DepStatus {
	for i := range res.Deps {
		if res.Deps[i].Kind == kind && res.Deps[i].Reachable {
			return &res.Deps[i]
		}
	}
	return nil
}

func depMySQL(ctx context.Context, w *World, res probe.Result, perr error) []Finding {
	if perr != nil {
		return []Finding{fnd("dep.mysql", LevelSkip, "系统探测不可用："+perr.Error(), "")}
	}
	_, serverInstalled := w.component("server")
	st := reachable(res, "mysql")
	if st == nil {
		x := fnd("dep.mysql", LevelWarn, "本机默认端点上没有可达的 MySQL/MariaDB", "Server 使用远端/自定义地址时这是正常的；否则检查 mysqld 是否运行")
		if !serverInstalled {
			x.Level, x.Message = LevelSkip, "未安装 server，不检查 MySQL"
			x.Hint = ""
			return []Finding{x}
		}
		if h := w.serverHealth(ctx); h.ready == 200 {
			x.Level, x.Message = LevelPass, "本机默认端点上没有 MySQL，但 Server 就绪检查通过（它使用的是远端/自定义实例）"
			x.Hint = ""
		} else if h.initialized == initNo {
			x.Message = "本机默认端点上没有可达的 MySQL/MariaDB（Server 仍在安装向导阶段，向导里会配置连接）"
		} else if h.ready != 0 {
			x.Level = LevelFail
			x.Message = "本机默认端点上没有 MySQL/MariaDB，且 Server 就绪检查未通过（HTTP " + strconv.Itoa(h.ready) + "）"
			x.Hint = "检查数据库是否运行、server.yaml/运行期凭据的连接目标是否正确；lingedge logs server"
		}
		return []Finding{x}
	}
	x := fnd("dep.mysql", LevelPass, fmt.Sprintf("%s %s 可达（%s）", orStr(st.Flavor, "mysql"), orStr(st.Version, "版本未知"), epLabel(st)), "")
	x.Details = map[string]any{"flavor": st.Flavor, "version": st.Version}
	out := []Finding{x}
	out = append(out, mysqlSettings(ctx, w), mysqlPmax(ctx, w))
	return out
}

func epLabel(st *probe.DepStatus) string {
	if st.Endpoint.IsUnix() {
		return "unix " + st.Endpoint.Socket
	}
	return net.JoinHostPort(st.Endpoint.Host, strconv.Itoa(st.Endpoint.Port))
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func mysqlSettings(ctx context.Context, w *World) Finding {
	id := "dep.mysql.settings"
	if w.MySQLSettings == nil {
		return fnd(id, LevelSkip, "没有可用的迁移账号凭据，无法查询 MySQL 服务端设置", "以 root 运行并提供迁移账号凭据（环境变量/口令文件/migrate.yaml，同 backup）")
	}
	s, err := w.MySQLSettings(ctx)
	if err != nil {
		return fnd(id, LevelSkip, "无法查询 MySQL 服务端设置："+err.Error(), "以 root 运行并确认迁移账号凭据可用（lingedge-server migrate credential init）")
	}
	var bad []string
	if !strings.HasPrefix(strings.ToLower(s.CharacterSet), "utf8mb4") {
		bad = append(bad, "character_set_server="+s.CharacterSet+"（应为 utf8mb4*）")
	}
	mode := strings.ToUpper(s.SQLMode)
	if !strings.Contains(mode, "STRICT_TRANS_TABLES") && !strings.Contains(mode, "STRICT_ALL_TABLES") {
		bad = append(bad, "sql_mode 不含 STRICT_TRANS_TABLES/STRICT_ALL_TABLES（严格模式）")
	}
	x := Finding{ID: id, Details: map[string]any{"character_set_server": s.CharacterSet, "collation_server": s.Collation, "sql_mode": s.SQLMode, "version": s.Version}}
	if len(bad) > 0 {
		x.Level, x.Message = LevelWarn, "MySQL 服务端设置不符合建议："+strings.Join(bad, "；")
		x.Hint = "在 my.cnf 设置 character-set-server=utf8mb4 与严格 sql_mode；LingEdge 的库表显式使用 utf8mb4，但服务端默认值不一致会让手工建表/连接出现意外"
		return x
	}
	x.Level, x.Message = LevelPass, "MySQL 服务端字符集 "+s.CharacterSet+"、严格 sql_mode"
	return x
}

// mysqlPmax：分区表的 pmax 兜底分区非空 = Worker 的 partition.maintain 没有按月预建新分区（maint 账号未配置/作业失败/日期已越过预建范围）。
// 写入不会失败（落入 pmax），但分区不再滚动，保留期清理也无法按分区丢弃（docs/database/README §5.1；Story 1.15/1.21 的「已知依赖」）。
func mysqlPmax(ctx context.Context, w *World) Finding {
	const id = "dep.mysql.pmax"
	if w.PmaxRows == nil {
		return fnd(id, LevelSkip, "没有可用的迁移账号凭据，无法检查分区表的 pmax 分区", "以 root 运行并提供迁移账号凭据（环境变量/口令文件/migrate.yaml，同 backup）")
	}
	rows, err := w.PmaxRows(ctx)
	if err != nil {
		return fnd(id, LevelSkip, "无法检查分区表的 pmax 分区："+err.Error(), "以 root 运行并确认迁移账号凭据可用")
	}
	if len(rows) == 0 {
		return fnd(id, LevelPass, "没有分区表（或库尚未迁移）", "")
	}
	var bad []string
	for _, r := range rows {
		if r.Rows > 0 {
			bad = append(bad, fmt.Sprintf("%s（%d 行）", r.Table, r.Rows))
		}
	}
	x := Finding{ID: id, Details: map[string]any{"partitioned_tables": len(rows)}}
	if len(bad) == 0 {
		x.Level, x.Message = LevelPass, fmt.Sprintf("%d 张分区表的 pmax 兜底分区均为空（预建分区仍在滚动）", len(rows))
		return x
	}
	x.Level, x.Message = LevelWarn, "分区表的 pmax 兜底分区非空，预建分区没有跟上写入："+strings.Join(bad, "、")
	x.Hint = "检查 database.maint.* 是否已配置、Worker 的 partition.maintain 作业日志（lingedge logs server）；写入不会失败，但分区不再滚动，保留期无法按分区清理"
	x.Details["tables_with_rows"] = bad
	return x
}

func depRedis(ctx context.Context, w *World, res probe.Result, perr error) Finding {
	if perr != nil {
		return fnd("dep.redis", LevelSkip, "系统探测不可用："+perr.Error(), "")
	}
	if _, ok := w.component("server"); !ok {
		return fnd("dep.redis", LevelSkip, "未安装 server，不检查 Redis", "")
	}
	st := reachable(res, "redis")
	if st != nil {
		x := fnd("dep.redis", LevelPass, fmt.Sprintf("Redis %s 可达（%s）", orStr(st.Version, "版本未知"), epLabel(st)), "")
		x.Details = map[string]any{"version": st.Version, "auth_required": st.AuthRequired}
		return x
	}
	// Redis 不是唯一真相源（ADR-025）：不可达时 Server 降级运行，所以是告警而不是失败。
	x := fnd("dep.redis", LevelWarn, "本机默认端点上没有可达的 Redis", "Server 在 Redis 不可用时以降级模式运行（ADR-025）；使用远端 Redis 时这是正常的，否则检查 redis-server")
	if w.serverHealth(ctx).ready == 200 {
		x.Level, x.Message, x.Hint = LevelPass, "本机默认端点上没有 Redis，但 Server 就绪检查通过（使用远端/自定义实例，或降级运行）", ""
	}
	return x
}

func depNode(w *World, res probe.Result, perr error) Finding {
	if perr != nil {
		return fnd("dep.nodejs", LevelSkip, "系统探测不可用："+perr.Error(), "")
	}
	c, _ := w.component("website")
	if c.Unit == "" {
		return fnd("dep.nodejs", LevelSkip, "website 是静态变体，不需要 Node.js", "")
	}
	d, ok := res.System.Deps["nodejs"]
	if !ok || !d.Detected {
		return fnd("dep.nodejs", LevelFail, "website（ssr 变体）需要 Node.js，但没有探测到", "安装 Node.js（Tools 不代装依赖）")
	}
	x := fnd("dep.nodejs", LevelPass, "Node.js "+orStr(d.Version, "版本未知")+" 可用", "")
	return x
}

func depOpenresty(ctx context.Context, w *World) Finding {
	id := "dep.openresty"
	if w.OpenrestyV == nil {
		return fnd(id, LevelSkip, "无法执行 openresty -V", "")
	}
	out, err := w.OpenrestyV(ctx)
	if err != nil {
		return fnd(id, LevelFail, "node 需要 OpenResty，但 openresty -V 不可用："+err.Error(), "安装 OpenResty（含 lua 模块）；Tools 不代装依赖")
	}
	low := strings.ToLower(out)
	if !strings.Contains(low, "lua") {
		return fnd(id, LevelFail, "OpenResty 缺少 lua 模块（openresty -V 输出里没有 lua）", "使用官方 OpenResty 构建（含 ngx_http_lua_module）")
	}
	return fnd(id, LevelPass, "OpenResty 可用且含 lua 模块", "")
}
