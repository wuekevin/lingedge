package probe

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"sort"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

// Options 配置 Collect。零值即生产默认（真实根文件系统、/proc、固定目录解析、127.0.0.1:3306/6379）。
type Options struct {
	Runner     *safeexec.Runner // 默认 NewRunner(nil)
	FS         baota.FS         // 宝塔与既有环境的文件读取；默认 baota.OSFS{Root: "/"}
	ProcRoot   string           // 默认 /proc（Linux）；非 Linux 默认空 = 不可用
	UserDBPath string           // 默认 /etc/passwd
	MySQL      []Endpoint       // 默认 127.0.0.1:3306；可含 unix 端点（UnixEndpoint），声明了就一定探测
	Redis      []Endpoint       // 默认 127.0.0.1:6379；同上
	// MySQLSockets / RedisSockets 是 unix socket 候选路径。nil：所有 TCP 端点都不可达时，自动探测默认位置
	// （DefaultMySQLSockets/DefaultRedisSockets）与 /proc/net/unix 里发现的 socket，存在且是 socket 才探测；
	// 非 nil（含空切片）：恰好探测这些路径（空 = 完全不探测 socket），不存在的记为不可达，不做自动发现。
	MySQLSockets []string
	RedisSockets []string
	// NodeHomes 是扫描 nvm/fnm/volta 等的用户主目录；nil = /root、/home/* 与当前用户主目录；空切片 = 不扫描主目录。
	NodeHomes   []string
	Creds       Creds // 可选；见 Creds
	DialTimeout time.Duration
	Now         time.Time
	// ListenersOnly 为 true 时只采集内存/磁盘与监听（ss、/proc），不探测宝塔/服务/MySQL/Redis/Node.js：
	// list/status 只需要「谁占着哪个端口」，不应为此向 MySQL/Redis 发起连接（免凭据握手会被 MySQL 计入 Aborted_connects）。
	ListenersOnly bool
	// LittleEndian 指明 /proc/net 的主机字节序；nil = 运行主机的字节序。
	LittleEndian *bool
}

// Warning 是不阻断的提示（降级、不可得、不可达）。
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Result 是 Collect 的产出。
type Result struct {
	// System 直接就是 resolver.System（Story 1.30 的输入契约）。
	System resolver.System `json:"system"`
	// Baota 写入 state.json 的 host.baota；含站点、既有 Web/DB、PHP、防火墙/WAF、系统服务。
	Baota baota.Report `json:"baota"`
	// Sockets 是监听的完整信息（Listener 之外还有全部占用进程、是否被隐藏），doctor 复用。
	Sockets []Socket `json:"sockets"`
	// ListenersKnown 为 false 表示 ss 与 /proc 均不可用——System.Listeners 是空的但**并不代表没有占用**。
	// 调用方（1.33 的预检）必须把它当作「端口冲突无法检测」处理（阻断或要求确认），不能当作无冲突。
	ListenersKnown  bool        `json:"listeners_known"`
	ListenersSource string      `json:"listeners_source"` // ss | proc | none
	Deps            []DepStatus `json:"deps"`
	// NodeInstalls 是在非固定目录（nvm/fnm/volta/n/asdf/宝塔 Node 项目管理器）识别到的 Node.js（追加字段）。
	NodeInstalls []NodeInstall `json:"node_installs"`
	Warnings     []Warning     `json:"warnings"`
}

func hostLittleEndian() bool { return binary.NativeEndian.Uint16([]byte{1, 0}) == 1 }

// Collect 只读采集系统信息。单项失败转为 Warnings，不使整体失败；除 ctx 取消外不返回 error。
func Collect(ctx context.Context, o Options) (Result, error) {
	if o.Runner == nil {
		r, err := NewRunner(nil)
		if err != nil {
			return Result{}, err
		}
		o.Runner = r
	}
	if o.FS == nil {
		o.FS = baota.OSFS{Root: "/"}
	}
	if o.ProcRoot == "" {
		o.ProcRoot = defaultProcRoot
	}
	if o.UserDBPath == "" {
		o.UserDBPath = "/etc/passwd"
	}
	if len(o.MySQL) == 0 {
		o.MySQL = []Endpoint{{Host: "127.0.0.1", Port: 3306}}
	}
	if len(o.Redis) == 0 {
		o.Redis = []Endpoint{{Host: "127.0.0.1", Port: 6379}}
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	le := hostLittleEndian()
	if o.LittleEndian != nil {
		le = *o.LittleEndian
	}
	c := &collector{o: o, le: le}
	c.res.System = resolver.System{OS: runtime.GOOS, Arch: runtime.GOARCH, Disks: []resolver.Disk{}, Listeners: []resolver.Listener{}, Deps: map[string]resolver.Dep{}}
	c.res.Sockets, c.res.Deps, c.res.Warnings, c.res.NodeInstalls = []Socket{}, []DepStatus{}, []Warning{}, []NodeInstall{}

	c.memoryAndDisks()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	c.listeners(ctx)
	if o.ListenersOnly {
		c.finish()
		return c.res, nil
	}
	c.baota(ctx)
	c.services(ctx)
	c.deps(ctx)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	c.finish()
	return c.res, nil
}

type collector struct {
	o   Options
	le  bool
	res Result
}

func (c *collector) warn(code, f string, a ...any) {
	c.res.Warnings = append(c.res.Warnings, Warning{Code: code, Message: fmt.Sprintf(f, a...)})
}

func (c *collector) memoryAndDisks() {
	t, a, notes, err := osMemory()
	if err != nil {
		c.warn("memory.unavailable", "无法采集内存：%v（内存记为 0，resolver 会对需要内存的组件阻断）", err)
	} else {
		c.res.System.MemoryTotalMB, c.res.System.MemoryAvailableMB = t, a
	}
	for _, n := range notes {
		c.warn("memory.note", "%s", n)
	}
	ds, dnotes, err := osDisks()
	if err != nil {
		c.warn("disks.unavailable", "无法采集磁盘：%v（resolver 对未覆盖的挂载点阻断）", err)
	} else {
		c.res.System.Disks = ds
	}
	for _, n := range dnotes {
		c.warn("disks.note", "%s", n)
	}
	if runtime.GOOS != "linux" {
		c.warn("platform.degraded", "当前平台 %s 不是 Linux：仅开发机可用，监听等采集项降级（生产目标是 Linux amd64/arm64）", runtime.GOOS)
	}
}

func (c *collector) procFS() ProcFS {
	return ProcFS{Root: c.o.ProcRoot, UserDB: c.o.UserDBPath, LE: c.le}
}

func (c *collector) listeners(ctx context.Context) {
	var socks []Socket
	source := "none"
	so, _, err := readOnlyRun(ctx, c.o.Runner, "ss", []string{"-H", "-lntup"})
	if err == nil {
		var skipped int
		socks, skipped = ParseSS(so)
		source = "ss"
		if skipped > 0 {
			c.warn("listeners.ss_skipped", "ss 输出有 %d 行无法解析，已跳过", skipped)
		}
		hidden := 0
		for _, s := range socks {
			if s.Hidden {
				hidden++
			}
		}
		if hidden > 0 {
			c.warn("listeners.hidden", "%d 个监听的占用进程不可见（ss 无权限查看他人进程，请用 root 运行以获得完整占用者信息）", hidden)
		}
		if c.o.ProcRoot != "" {
			c.procFS().Enrich(socks)
		}
	} else {
		c.warn("listeners.ss_unavailable", "ss 不可用（%v），回退 /proc/net", sanitize(err))
		if c.o.ProcRoot == "" {
			c.warn("listeners.unavailable", "本平台没有 /proc，端口占用无法检测（ListenersKnown=false；预检不得据此认为无冲突）")
		} else {
			ps, notes, perr := c.procFS().ListenersFromProc()
			for _, n := range notes {
				c.warn("listeners.proc_note", "%s", n)
			}
			if perr != nil {
				c.warn("listeners.unavailable", "/proc/net 也不可读：%v（ListenersKnown=false；预检不得据此认为无冲突）", perr)
			} else {
				socks, source = ps, "proc"
				c.procFS().Enrich(socks)
			}
		}
	}
	c.res.ListenersSource = source
	c.res.ListenersKnown = source != "none"
	c.res.Sockets = mergeSockets(socks)
	for _, s := range c.res.Sockets {
		c.res.System.Listeners = append(c.res.System.Listeners, s.Listener)
	}
}

// mergeSockets 把同一 (proto, addr, port) 的多条记录（SO_REUSEPORT、ss 与 /proc 的重复）合并为一条，占用进程取并集，Listener 取 pid 最小者。
func mergeSockets(in []Socket) []Socket {
	type key struct {
		proto, addr string
		port        int
	}
	idx := map[key]int{}
	var out []Socket
	for _, s := range in {
		k := key{s.Listener.Proto, s.Listener.Addr, s.Listener.Port}
		i, ok := idx[k]
		if !ok {
			idx[k] = len(out)
			out = append(out, s)
			continue
		}
		m := &out[i]
		have := map[int]bool{}
		for _, p := range m.Procs {
			have[p.PID] = true
		}
		for _, p := range s.Procs {
			if !have[p.PID] {
				m.Procs = append(m.Procs, p)
			}
		}
		if len(m.Procs) > 0 {
			sort.Slice(m.Procs, func(a, b int) bool { return m.Procs[a].PID < m.Procs[b].PID })
			m.Hidden = false
			if m.Listener.PID != m.Procs[0].PID {
				m.Listener.PID, m.Listener.Process = m.Procs[0].PID, m.Procs[0].Name
				if s.Listener.PID == m.Procs[0].PID {
					m.Listener.User, m.Listener.Unit = s.Listener.User, s.Listener.Unit
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Listener, out[j].Listener
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.Proto != b.Proto {
			return a.Proto < b.Proto
		}
		return a.Addr < b.Addr
	})
	return out
}

func (c *collector) baota(ctx context.Context) {
	rep := baota.Probe(ctx, baota.Options{FS: c.o.FS, Exec: VersionExec{Runner: c.o.Runner}, Now: c.o.Now})
	c.res.Baota = rep
	b := resolver.Baota{Detected: rep.Detected, Confidence: rep.Confidence, Version: rep.Version, PanelPort: rep.PanelPort}
	for _, w := range rep.WebServers {
		if w.Variant == "baota" && w.Binary != "" {
			b.NginxPath = w.Binary
			break
		}
	}
	c.res.System.Baota = b
}

func (c *collector) services(ctx context.Context) {
	so, _, err := readOnlyRun(ctx, c.o.Runner, "systemctl", []string{"list-units", "--type=service", "--all", "--no-legend", "--no-pager", "--plain"})
	if err != nil {
		c.warn("services.unavailable", "无法列出 systemd 服务（%v）：named/systemd-resolved 等系统服务未识别", sanitize(err))
		return
	}
	svcs := ParseSystemctlUnits(so)
	c.res.Baota.Services = svcs
	for i := range c.res.Baota.Firewalls {
		f := &c.res.Baota.Firewalls[i]
		for _, s := range svcs {
			if s.Role == "firewall" && s.Name == f.Kind {
				if s.Active == "active" {
					f.Active = "yes"
				} else {
					f.Active = "no"
				}
			}
		}
	}
	// 没有配置文件证据但服务存在的防火墙也记录
	for _, s := range svcs {
		if s.Role != "firewall" {
			continue
		}
		found := false
		for _, f := range c.res.Baota.Firewalls {
			if f.Kind == s.Name {
				found = true
			}
		}
		if !found {
			act := "no"
			if s.Active == "active" {
				act = "yes"
			}
			c.res.Baota.Firewalls = append(c.res.Baota.Firewalls, baota.Firewall{Kind: s.Name, Active: act, Source: "systemctl"})
		}
	}
}

func (c *collector) deps(ctx context.Context) {
	// MySQL / MariaDB
	var my *DepStatus
	probeMy := func(ep Endpoint) {
		st := ProbeMySQL(ctx, c.o.Runner, ep, c.o.Creds, c.o.DialTimeout)
		c.res.Deps = append(c.res.Deps, st)
		if st.Reachable && my == nil {
			cp := st
			my = &cp
		}
	}
	for _, ep := range c.o.MySQL {
		probeMy(ep)
	}
	for _, ep := range c.socketEndpoints(c.o.MySQLSockets, my == nil, DefaultMySQLSockets, reMySQLSockName) {
		probeMy(ep)
	}
	if my != nil {
		c.res.System.Deps["mysql"] = resolver.Dep{Detected: true, Version: my.Version, Source: my.Endpoint.source(), Flavor: my.Flavor}
	} else if d, ok := c.installedDB("mysql"); ok {
		c.res.System.Deps["mysql"] = d
		c.warn("dep.mysql.unreachable", "检测到本机已安装 MySQL/MariaDB（%s）但 %s 不可达：不会代装或改动；请在 Server 安装向导中提供连接信息%s", d.Source, c.o.MySQL[0].addr(), btHint(c.res.Baota))
	} else {
		c.warn("dep.mysql.absent", "未探测到 MySQL/MariaDB（%s 不可达，也未发现本机安装）：Tools 不代装依赖；Server 向导会引导配置外部/远端 MySQL", c.o.MySQL[0].addr())
	}
	// Redis
	var rd *DepStatus
	probeRd := func(ep Endpoint) {
		st := ProbeRedis(ctx, ep, c.o.Creds, c.o.DialTimeout)
		c.res.Deps = append(c.res.Deps, st)
		if st.Reachable && rd == nil {
			cp := st
			rd = &cp
		}
	}
	for _, ep := range c.o.Redis {
		probeRd(ep)
	}
	for _, ep := range c.socketEndpoints(c.o.RedisSockets, rd == nil, DefaultRedisSockets, reRedisSockName) {
		probeRd(ep)
	}
	switch {
	case rd != nil:
		ver := rd.Version
		if ver == "" { // 需要口令且未提供：版本取本机二进制
			if d, ok := c.installedDB("redis"); ok {
				ver = d.Version
			}
		}
		c.res.System.Deps["redis"] = resolver.Dep{Detected: true, Version: ver, Source: rd.Endpoint.source(), Flavor: orDefault(rd.Flavor, "redis")}
		if rd.AuthRequired && !rd.Authenticated {
			c.warn("dep.redis.auth_required", "Redis %s 需要口令：未提供凭据，版本取自本机二进制；连通性验证由 Server 向导完成", rd.Endpoint.addr())
		}
	default:
		if d, ok := c.installedDB("redis"); ok {
			c.res.System.Deps["redis"] = d
			c.warn("dep.redis.unreachable", "检测到本机已安装 Redis（%s）但 %s 不可达：不会代装或改动；请在 Server 安装向导中提供连接信息", d.Source, c.o.Redis[0].addr())
		} else {
			c.warn("dep.redis.absent", "未探测到 Redis（%s 不可达，也未发现本机安装）：Tools 不代装依赖；Server 向导会引导配置", c.o.Redis[0].addr())
		}
	}
	c.nodejs(ctx)
}

// nodejs 探测 Node.js（website ssr 的运行时）：PATH 上的 node（固定目录解析，经 safeexec）优先——那是实际会被执行的；
// 没有时从 nvm/fnm/volta/n/asdf/宝塔等目录里只读识别（不执行），取版本最高者并提示它不在固定目录。
func (c *collector) nodejs(ctx context.Context) {
	cur, _ := os.UserHomeDir()
	c.res.NodeInstalls = FindNodeInstalls(c.o.FS, nodeHomes(c.o.FS, c.o.NodeHomes, cur))
	if v, flavor, err := (VersionExec{Runner: c.o.Runner}).Version(ctx, "node", "node"); err == nil {
		c.res.System.Deps["nodejs"] = resolver.Dep{Detected: true, Version: v, Source: "node", Flavor: flavor}
		return
	}
	if len(c.res.NodeInstalls) == 0 {
		return
	}
	best := c.res.NodeInstalls[0]
	c.res.System.Deps["nodejs"] = resolver.Dep{Detected: true, Version: best.Version, Source: best.Bin, Flavor: "nodejs"}
	c.warn("dep.nodejs.not_in_path", "Node.js 不在固定目录/PATH 上，识别到 %d 个非固定目录安装（%s %s，来源 %s），取最高版本 %s；版本取自目录名/node_version.h，未执行；启动 website 时须使用该绝对路径", len(c.res.NodeInstalls), best.Manager, best.Bin, best.VersionSource, best.Version)
}

// socketEndpoints 给出要探测的 unix socket 端点。declared 非 nil：恰好这些；否则 auto 为真时发现候选并仅保留已存在的 socket。
func (c *collector) socketEndpoints(declared []string, auto bool, defaults []string, re *regexp.Regexp) []Endpoint {
	var eps []Endpoint
	if declared != nil {
		for _, p := range declared {
			eps = append(eps, UnixEndpoint(p))
		}
		return eps
	}
	if !auto {
		return nil
	}
	for _, p := range discoverSockets(defaults, c.o.ProcRoot, re) {
		if _, err := CheckSocket(p); err == nil {
			eps = append(eps, UnixEndpoint(p))
		}
	}
	return eps
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func btHint(r baota.Report) string {
	if r.Detected {
		return "（宝塔环境：Tools 不读取宝塔存储的 MySQL root 密码，请在向导中手工提供或新建专用账号）"
	}
	return ""
}

// installedDB 从 baota.Report 的既有安装里取 mysql/redis（Kind 为 mariadb 时按 mysql 处理）。
func (c *collector) installedDB(kind string) (resolver.Dep, bool) {
	for _, d := range c.res.Baota.Databases {
		k := d.Kind
		if k == "mariadb" {
			k = "mysql"
		}
		if k == kind {
			fl := d.Kind
			if kind == "redis" {
				fl = "redis"
			}
			return resolver.Dep{Detected: true, Version: d.Version, Source: d.Path, Flavor: fl}, true
		}
	}
	return resolver.Dep{}, false
}

// finish 规范化输出（确定顺序、空切片），并把 Baota 报告做脱敏收尾。
func (c *collector) finish() {
	sort.Slice(c.res.System.Disks, func(i, j int) bool { return c.res.System.Disks[i].Mount < c.res.System.Disks[j].Mount })
	c.res.Baota = c.res.Baota.Scrub()
	for i := range c.res.Warnings {
		c.res.Warnings[i].Message = sanitizeText(c.res.Warnings[i].Message)
	}
}
