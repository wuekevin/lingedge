package baota

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Exec 取可执行文件的版本（`nginx -v`、`mysqld --version` 等）。实现必须经 safeexec（见 probe.VersionExec）；nil 表示不执行任何命令
// （此时版本只取自文件证据，缺失则为空）。kind：nginx | mysqld | redis-server。
type Exec interface {
	Version(ctx context.Context, kind, exe string) (version, flavor string, err error)
}

// Options 配置 Probe。
type Options struct {
	FS   FS        // 默认 OSFS{Root: "/"}
	Exec Exec      // 可选
	Now  time.Time // 零值取 time.Now()
}

// 宝塔默认布局（tools-design §11；真实机器上的核对属 ENV-BT）。
const (
	PanelDir     = "/www/server/panel"
	panelPortPL  = "/www/server/panel/data/port.pl"
	btInitScript = "/etc/init.d/bt"
	pluginDir    = "/www/server/panel/plugin"
	btVhostNginx = "/www/server/panel/vhost/nginx"
	maxSmall     = 4096
	maxVersion   = 64 << 10
)

var btCommands = []string{"/usr/bin/bt", "/usr/local/bin/bt", btInitScript}

// 版本来源候选（按序取第一个匹配）。路径与正则按宝塔常见布局给出，真机核对属 ENV-BT；VersionSource 会如实写出实际取自哪个文件。
var panelVersionSources = []struct {
	path string
	re   *regexp.Regexp
}{
	{"/www/server/panel/class/common.py", regexp.MustCompile(`(?m)\bversion\s*=\s*['"](\d+(?:\.\d+){1,3})['"]`)},
	{"/www/server/panel/config/version.pl", regexp.MustCompile(`^\s*(\d+(?:\.\d+){1,3})\s*$`)},
}

// Probe 只读探测宝塔与既有 Nginx/OpenResty/MySQL/Redis/PHP/站点/防火墙/WAF。永不返回 error：单项失败记入 Notes，不影响其余项。
func Probe(ctx context.Context, o Options) Report {
	if o.FS == nil {
		o.FS = OSFS{Root: "/"}
	}
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	p := &prober{o: o, ctx: ctx}
	r := Report{ProbedAt: now.UTC().Format("2006-01-02T15:04:05.000Z"),
		Evidence: []Evidence{}, WebServers: []WebServer{}, Databases: []DBInstall{}, PHP: []PHP{}, Sites: []Site{},
		Firewalls: []Firewall{}, WAF: []WAF{}, Plugins: []string{}, Services: []Service{}, Notes: []string{}}
	p.r = &r
	p.panel()
	p.web()
	p.databases()
	p.php()
	p.waf()
	p.firewalls()
	p.sites()
	sort.Slice(r.Evidence, func(i, j int) bool {
		if r.Evidence[i].Kind != r.Evidence[j].Kind {
			return r.Evidence[i].Kind < r.Evidence[j].Kind
		}
		return r.Evidence[i].Path < r.Evidence[j].Path
	})
	return r.Scrub()
}

type prober struct {
	o   Options
	ctx context.Context
	r   *Report
}

func (p *prober) note(f string, a ...any) {
	if len(p.r.Notes) < maxNotes {
		p.r.Notes = append(p.r.Notes, sprintf(f, a...))
	}
}

func (p *prober) isDir(name string) bool {
	fi, err := p.o.FS.Lstat(name)
	return err == nil && fi.IsDir()
}

func (p *prober) isFile(name string) bool {
	fi, err := p.o.FS.Lstat(name)
	return err == nil && (fi.Mode().IsRegular() || fi.Mode()&fs.ModeSymlink != 0)
}

func (p *prober) read(name string, max int64) ([]byte, bool) {
	b, err := p.o.FS.ReadFile(name, max)
	if err != nil {
		return nil, false
	}
	return b, true
}

func (p *prober) panel() {
	r := p.r
	kinds := map[string]bool{}
	add := func(kind, pth, detail string) {
		if len(r.Evidence) < maxEvidence {
			r.Evidence = append(r.Evidence, Evidence{Kind: kind, Path: pth, Detail: detail})
		}
		kinds[kind] = true
	}
	if p.isDir(PanelDir) {
		add("panel_dir", PanelDir, "")
	}
	for _, c := range btCommands {
		if p.isFile(c) {
			add("bt_command", c, "")
		}
	}
	if b, ok := p.read(panelPortPL, maxSmall); ok {
		s := strings.TrimSpace(string(b))
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= 65535 {
			add("port_file", panelPortPL, "port="+s)
			r.PanelPort = n
		} else {
			p.note("%s 内容不是合法端口，不作为证据", panelPortPL)
		}
	}
	switch len(kinds) {
	case 3:
		r.Detected, r.Confidence = true, "high"
	case 2:
		r.Detected, r.Confidence = true, "medium"
	case 1:
		p.note("只有一类宝塔证据（%s），不判定为宝塔环境", onlyKey(kinds))
	}
	if !r.Detected {
		r.PanelPort = 0 // 未判定时不输出面板端口
		return
	}
	for _, src := range panelVersionSources {
		b, ok := p.read(src.path, maxVersion)
		if !ok {
			continue
		}
		if m := src.re.FindSubmatch(b); m != nil {
			r.Version, r.VersionSource = string(m[1]), src.path
			break
		}
	}
	if r.Version == "" {
		p.note("未能从面板文件取得宝塔版本（版本来源文件待 ENV-BT 核对）")
	}
}

func onlyKey(m map[string]bool) string {
	for k := range m {
		return k
	}
	return ""
}

type webCand struct {
	variant, kind, binary, conf, vhost, versionFile string
	vhostDirs                                       []string
}

var webCands = []webCand{
	{variant: "baota", kind: "nginx", binary: "/www/server/nginx/sbin/nginx", conf: "/www/server/nginx/conf/nginx.conf", vhost: btVhostNginx, versionFile: "/www/server/nginx/version.pl"},
	{variant: "system", kind: "nginx", binary: "/usr/sbin/nginx", conf: "/etc/nginx/nginx.conf", vhostDirs: []string{"/etc/nginx/conf.d", "/etc/nginx/sites-enabled"}},
	{variant: "system", kind: "nginx", binary: "/usr/local/sbin/nginx", conf: "/usr/local/etc/nginx/nginx.conf"},
	{variant: "system", kind: "openresty", binary: "/usr/local/openresty/nginx/sbin/openresty", conf: "/usr/local/openresty/nginx/conf/nginx.conf"},
	{variant: "system", kind: "openresty", binary: "/usr/bin/openresty", conf: "/usr/local/openresty/nginx/conf/nginx.conf"},
}

func (p *prober) web() {
	for _, c := range webCands {
		hasBin := p.isFile(c.binary)
		hasConf := p.isFile(c.conf)
		if !hasBin && !hasConf {
			continue
		}
		// 同一变体+类型只记一次（/usr/bin/openresty 与 /usr/local/openresty/... 并存时取先到者）
		dup := false
		for _, w := range p.r.WebServers {
			if w.Variant == c.variant && w.Kind == c.kind {
				dup = true
			}
		}
		if dup {
			continue
		}
		w := WebServer{Variant: c.variant, Kind: c.kind}
		if hasBin {
			w.Binary = c.binary
		}
		if hasConf {
			w.Conf = c.conf
		}
		if c.vhost != "" && p.isDir(c.vhost) {
			w.VhostDir = c.vhost
		}
		if hasBin && p.o.Exec != nil {
			if v, flavor, err := p.o.Exec.Version(p.ctx, "nginx", c.binary); err == nil {
				w.Version = v
				if flavor == "openresty" || flavor == "nginx" {
					w.Kind = flavor // 宝塔的 /www/server/nginx 可能是 OpenResty，以 -v 输出为准
				}
			} else {
				p.note("无法取得 %s 的版本：%v", c.binary, err)
			}
		}
		if w.Version == "" && c.versionFile != "" {
			if b, ok := p.read(c.versionFile, maxSmall); ok {
				w.Version = cleanVersion(string(b))
			}
		}
		p.r.WebServers = append(p.r.WebServers, w)
	}
}

func cleanVersion(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if !regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_~-]{0,63}$`).MatchString(s) {
		return ""
	}
	return s
}

type dbCand struct {
	variant, kind, bin, dir, data, versionFile, execKind string
}

var dbCands = []dbCand{
	{variant: "baota", kind: "mysql", bin: "/www/server/mysql/bin/mysqld", dir: "/www/server/mysql", data: "/www/server/data", versionFile: "/www/server/mysql/version.pl", execKind: "mysqld"},
	{variant: "system", kind: "mysql", bin: "/usr/sbin/mysqld", dir: "/etc/mysql", execKind: "mysqld"},
	{variant: "system", kind: "mysql", bin: "/usr/libexec/mysqld", execKind: "mysqld"},
	{variant: "system", kind: "mariadb", bin: "/usr/sbin/mariadbd", dir: "/etc/mysql", execKind: "mysqld"},
	{variant: "baota", kind: "redis", bin: "/www/server/redis/src/redis-server", dir: "/www/server/redis", versionFile: "/www/server/redis/version.pl", execKind: "redis-server"},
	{variant: "system", kind: "redis", bin: "/usr/bin/redis-server", execKind: "redis-server"},
	{variant: "system", kind: "redis", bin: "/usr/local/bin/redis-server", execKind: "redis-server"},
}

func (p *prober) databases() {
	for _, c := range dbCands {
		hasBin := p.isFile(c.bin)
		hasDir := c.variant == "baota" && c.dir != "" && p.isDir(c.dir)
		if !hasBin && !hasDir {
			continue
		}
		dup := false
		for _, d := range p.r.Databases {
			if d.Variant == c.variant && (d.Kind == c.kind || (c.kind != "redis" && d.Kind != "redis")) {
				dup = true
			}
		}
		if dup {
			continue
		}
		d := DBInstall{Variant: c.variant, Kind: c.kind, Path: c.bin}
		if !hasBin {
			d.Path = c.dir
		}
		if c.data != "" && p.isDir(c.data) {
			d.DataDir = c.data // 只记路径，不读取其中任何内容
		}
		if hasBin && p.o.Exec != nil {
			if v, flavor, err := p.o.Exec.Version(p.ctx, c.execKind, c.bin); err == nil {
				d.Version = v
				if flavor == "mariadb" && c.kind == "mysql" {
					d.Kind = "mariadb"
				}
			} else {
				p.note("无法取得 %s 的版本：%v", c.bin, err)
			}
		}
		if d.Version == "" && c.versionFile != "" {
			if b, ok := p.read(c.versionFile, maxSmall); ok {
				d.Version = cleanVersion(string(b))
			}
		}
		p.r.Databases = append(p.r.Databases, d)
	}
}

var rePHPDir = regexp.MustCompile(`^(\d)(\d{1,2})$`)

func (p *prober) php() {
	es, err := p.o.FS.ReadDir("/www/server/php")
	if err != nil {
		return
	}
	for _, e := range es {
		m := rePHPDir.FindStringSubmatch(e.Name())
		if m == nil || !e.IsDir() {
			continue
		}
		if len(p.r.PHP) >= maxList {
			break
		}
		p.r.PHP = append(p.r.PHP, PHP{Version: m[1] + "." + m[2], Path: "/www/server/php/" + e.Name()})
	}
	sort.Slice(p.r.PHP, func(i, j int) bool { return p.r.PHP[i].Path < p.r.PHP[j].Path })
}

var wafDirs = []struct{ name, path string }{
	{"btwaf", "/www/server/btwaf"},
	{"btwaf-plugin", "/www/server/panel/plugin/btwaf"},
	{"btwaf-httpd-plugin", "/www/server/panel/plugin/btwaf_httpd"},
	{"firewall-plugin", "/www/server/panel/plugin/firewall"},
	{"syssafe-plugin", "/www/server/panel/plugin/syssafe"},
}

var rePluginName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func (p *prober) waf() {
	for _, w := range wafDirs {
		if p.isDir(w.path) {
			p.r.WAF = append(p.r.WAF, WAF{Name: w.name, Path: w.path})
		}
	}
	es, err := p.o.FS.ReadDir(pluginDir)
	if err != nil {
		return
	}
	for _, e := range es {
		if e.IsDir() && rePluginName.MatchString(e.Name()) && len(p.r.Plugins) < maxPlugins {
			p.r.Plugins = append(p.r.Plugins, e.Name())
		}
	}
	sort.Strings(p.r.Plugins)
}

var reUFWEnabled = regexp.MustCompile(`(?mi)^\s*ENABLED\s*=\s*(yes|no)\s*$`)

func (p *prober) firewalls() {
	if b, ok := p.read("/etc/ufw/ufw.conf", maxVersion); ok {
		act := "unknown"
		if m := reUFWEnabled.FindSubmatch(b); m != nil {
			act = strings.ToLower(string(m[1]))
		}
		p.r.Firewalls = append(p.r.Firewalls, Firewall{Kind: "ufw", Active: act, Source: "/etc/ufw/ufw.conf"})
	}
	if p.isFile("/etc/firewalld/firewalld.conf") {
		p.r.Firewalls = append(p.r.Firewalls, Firewall{Kind: "firewalld", Active: "unknown", Source: "/etc/firewalld/firewalld.conf"})
	}
}

const maxVhostFiles = 2000

func (p *prober) sites() {
	type dirSrc struct {
		dir, source string
		anyName     bool // sites-enabled 的条目通常没有 .conf 后缀
	}
	var dirs []dirSrc
	for _, w := range p.r.WebServers {
		if w.VhostDir != "" {
			dirs = append(dirs, dirSrc{dir: w.VhostDir, source: w.Variant})
		}
	}
	for _, c := range webCands {
		for _, d := range c.vhostDirs {
			if p.isDir(d) {
				dirs = append(dirs, dirSrc{dir: d, source: "system", anyName: strings.HasSuffix(d, "sites-enabled")})
			}
		}
	}
	seen := map[string]bool{}
	files := 0
	for _, d := range dirs {
		if seen[d.dir] {
			continue
		}
		seen[d.dir] = true
		es, err := p.o.FS.ReadDir(d.dir)
		if err != nil {
			p.note("无法列出 vhost 目录 %s", d.dir)
			continue
		}
		names := make([]string, 0, len(es))
		for _, e := range es {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if strings.HasSuffix(e.Name(), ".conf") || d.anyName {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			if files++; files > maxVhostFiles {
				p.note("vhost 文件超过 %d 个，其余忽略", maxVhostFiles)
				return
			}
			file := path.Join(d.dir, n)
			b, err := p.readVhost(file)
			if err != nil {
				p.note("无法读取 vhost 文件 %s", file)
				continue
			}
			ss, perr := ParseVhost(file, d.source, b)
			if perr != nil {
				p.note("vhost 文件 %s 解析不完整：%v", file, perr)
				if len(ss) == 0 {
					ss = append(ss, Site{File: file, Source: d.source, Malformed: true, ServerNames: []string{}, Listens: []Listen{}})
				}
			}
			for _, s := range ss {
				if len(p.r.Sites) < maxList {
					p.r.Sites = append(p.r.Sites, s)
				}
			}
		}
	}
}

// readVhost 读取 vhost 文件。符号链接（Debian 的 sites-enabled → sites-available）仅当目标落在 /etc/nginx/ 之下时才读取目标——
// 不跟随指向其他位置的链接。
func (p *prober) readVhost(file string) ([]byte, error) {
	if fi, err := p.o.FS.Lstat(file); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		tgt, err := p.o.FS.Readlink(file)
		if err != nil {
			return nil, err
		}
		if !path.IsAbs(tgt) {
			tgt = path.Join(path.Dir(file), tgt)
		}
		tgt = path.Clean(tgt)
		if !strings.HasPrefix(tgt, "/etc/nginx/") {
			return nil, errors.New("符号链接目标不在 /etc/nginx/ 之下，不跟随")
		}
		return p.o.FS.ReadFile(tgt, maxConfBytes+1)
	}
	return p.o.FS.ReadFile(file, maxConfBytes+1)
}
