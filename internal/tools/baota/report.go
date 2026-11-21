package baota

import (
	"fmt"
	"path"
	"regexp"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// Report 是只读探测的结果，也是 state.json 的 host.baota 的形状。它不含任何凭据字段（类型层面就没有地方放）。
type Report struct {
	ProbedAt string `json:"probed_at,omitempty"` // RFC3339（UTC）

	// 宝塔面板判定：三类证据中任两类成立才 Detected（high=三类，medium=两类）；只有一类不判定。
	Detected      bool       `json:"detected"`
	Confidence    string     `json:"confidence,omitempty"`
	Evidence      []Evidence `json:"evidence"`
	Version       string     `json:"version,omitempty"`
	VersionSource string     `json:"version_source,omitempty"`
	PanelPort     int        `json:"panel_port,omitempty"`

	WebServers []WebServer `json:"web_servers"`
	Databases  []DBInstall `json:"databases"`
	PHP        []PHP       `json:"php"` // 仅识别并记录，不做任何处理
	Sites      []Site      `json:"sites"`
	Firewalls  []Firewall  `json:"firewalls"`
	WAF        []WAF       `json:"waf"`
	Plugins    []string    `json:"plugins"` // /www/server/panel/plugin 下的目录名
	Services   []Service   `json:"services"`
	Notes      []string    `json:"notes"`
}

// Evidence 是一条证据。Kind：panel_dir | bt_command | port_file（宝塔判定）；其余证据（web/db 等）记在各自的结构里。
type Evidence struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Detail string `json:"detail,omitempty"`
}

// WebServer 是一个 Nginx/OpenResty 安装。Variant：baota | system。
type WebServer struct {
	Variant  string `json:"variant"`
	Kind     string `json:"kind"` // nginx | openresty（以 -v 输出为准；无法执行时按路径）
	Binary   string `json:"binary,omitempty"`
	Conf     string `json:"conf,omitempty"`
	VhostDir string `json:"vhost_dir,omitempty"`
	Version  string `json:"version,omitempty"`
}

// DBInstall 是一个 MySQL/MariaDB/Redis 安装（仅识别；凭据由 Server 向导获得）。Kind：mysql | mariadb | redis。
type DBInstall struct {
	Variant string `json:"variant"`
	Kind    string `json:"kind"`
	Path    string `json:"path"`               // 安装目录或可执行文件
	DataDir string `json:"data_dir,omitempty"` // 仅路径，不读取内容
	Version string `json:"version,omitempty"`
}

// PHP 是一个 PHP 版本目录。
type PHP struct {
	Version string `json:"version"` // 如 7.4
	Path    string `json:"path"`
}

// Firewall 是系统防火墙。Kind：firewalld | ufw | iptables | nftables；Active：yes | no | unknown。
type Firewall struct {
	Kind   string `json:"kind"`
	Active string `json:"active"`
	Source string `json:"source,omitempty"`
}

// WAF 是宝塔防火墙/WAF 插件目录。
type WAF struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Service 是识别到的系统服务（systemctl list-units 的结果）。Role：dns | resolver | web | db | cache | firewall。
type Service struct {
	Name   string `json:"name"` // 规范名：named、systemd-resolved、nginx、mysqld、redis…
	Unit   string `json:"unit"`
	Active string `json:"active"` // active | inactive | failed ...（systemd 原文）
	Role   string `json:"role"`
}

const (
	maxEvidence  = 32
	maxList      = 1000
	maxPlugins   = 200
	maxNotes     = 64
	maxStringLen = 1024
)

var (
	reEvKind  = regexp.MustCompile(`^(panel_dir|bt_command|port_file)$`)
	reVariant = regexp.MustCompile(`^(baota|system)$`)
	reActive  = regexp.MustCompile(`^(yes|no|unknown)$`)
	reToken   = regexp.MustCompile(`^[A-Za-z0-9._@:+-]{1,128}$`)
)

// Validate 校验 Report 的全部约束（写入 state.json 前与读取后调用）：路径为规范绝对路径、端口范围、枚举取值、数量上限、
// 以及泄漏检查——任何字符串经脱敏后若有变化，说明疑似密钥，拒绝写入公开可读的 state.json。
func (r Report) Validate() error {
	bad := func(f, why string, a ...any) error {
		return fmt.Errorf("baota: 报告字段 %s %s", f, fmt.Sprintf(why, a...))
	}
	if len(r.Evidence) > maxEvidence || len(r.Sites) > maxList || len(r.Plugins) > maxPlugins || len(r.Notes) > maxNotes ||
		len(r.WebServers) > maxList || len(r.Databases) > maxList || len(r.PHP) > maxList || len(r.Services) > maxList || len(r.WAF) > maxList || len(r.Firewalls) > maxList {
		return bad("*", "超过数量上限")
	}
	if r.Detected && r.Confidence != "high" && r.Confidence != "medium" {
		return bad("confidence", "在 detected=true 时必须是 high|medium: %q", r.Confidence)
	}
	if !r.Detected && r.Confidence != "" {
		return bad("confidence", "在 detected=false 时必须为空: %q", r.Confidence)
	}
	if r.PanelPort < 0 || r.PanelPort > 65535 {
		return bad("panel_port", "越界: %d", r.PanelPort)
	}
	absOK := func(f, p string) error {
		if p != "" && (!path.IsAbs(p) || path.Clean(p) != p) {
			return bad(f, "必须是规范的绝对路径: %q", p)
		}
		return nil
	}
	for _, e := range r.Evidence {
		if !reEvKind.MatchString(e.Kind) {
			return bad("evidence.kind", "非法: %q", e.Kind)
		}
		if err := absOK("evidence.path", e.Path); err != nil {
			return err
		}
	}
	for _, w := range r.WebServers {
		if !reVariant.MatchString(w.Variant) || (w.Kind != "nginx" && w.Kind != "openresty") {
			return bad("web_servers", "variant/kind 非法")
		}
		for _, p := range []string{w.Binary, w.Conf, w.VhostDir} {
			if err := absOK("web_servers.path", p); err != nil {
				return err
			}
		}
	}
	for _, d := range r.Databases {
		if !reVariant.MatchString(d.Variant) || (d.Kind != "mysql" && d.Kind != "mariadb" && d.Kind != "redis") {
			return bad("databases", "variant/kind 非法")
		}
		for _, p := range []string{d.Path, d.DataDir} {
			if err := absOK("databases.path", p); err != nil {
				return err
			}
		}
	}
	for _, p := range r.PHP {
		if err := absOK("php.path", p.Path); err != nil {
			return err
		}
	}
	for _, w := range r.WAF {
		if err := absOK("waf.path", w.Path); err != nil {
			return err
		}
	}
	for _, f := range r.Firewalls {
		if !reActive.MatchString(f.Active) || !reToken.MatchString(f.Kind) {
			return bad("firewalls", "非法")
		}
	}
	for _, s := range r.Sites {
		if !reVariant.MatchString(s.Source) {
			return bad("sites.source", "非法: %q", s.Source)
		}
		if err := absOK("sites.file", s.File); err != nil {
			return err
		}
		for _, l := range s.Listens {
			if l.Port < 0 || l.Port > 65535 {
				return bad("sites.listens.port", "越界: %d", l.Port)
			}
		}
	}
	for _, s := range r.Services {
		if !reToken.MatchString(s.Name) || !reToken.MatchString(s.Unit) {
			return bad("services", "非法")
		}
	}
	var leak error
	r.eachString(func(f, v string) {
		if leak != nil {
			return
		}
		if len(v) > maxStringLen {
			leak = bad(f, "过长")
		} else if security.RedactString(v) != v {
			leak = bad(f, "疑似含密钥，拒绝写入")
		}
	})
	return leak
}

// Scrub 返回把所有「脱敏后会变化」的字符串替换为脱敏文本、并截断过长字符串的副本，使 Validate 的泄漏检查不会因为
// 恰好形似令牌的域名/路径而使整个 state.json 写入失败（该值本身被遮蔽，其余信息保留）。
func (r Report) Scrub() Report {
	r.mapStrings(func(v string) string {
		if len(v) > maxStringLen {
			v = v[:maxStringLen]
		}
		return security.RedactString(v)
	})
	return r
}
