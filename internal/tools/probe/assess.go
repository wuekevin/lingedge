package probe

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

// PortWant 是我方要占用的端口。
type PortWant struct {
	Component string
	Proto     string // tcp | udp
	Port      int
	Bind      string // 空 = 0.0.0.0（最保守）
}

// PathWant 是我方要创建/写入的路径。
type PathWant struct {
	Component string
	Path      string
	// OwnedByUs：该路径已由 Tools 安装（registry 记录中的目录）——存在是正常的，不算冲突。
	OwnedByUs bool
}

// NameWant 是我方要使用的站点名（域名）。
type NameWant struct {
	Component  string
	ServerName string
}

// Intent 是 Assess 的输入：我方打算占用什么。
type Intent struct {
	Ports []PortWant
	Paths []PathWant
	Names []NameWant
}

// Conflict 是一个真实冲突。Kind：port | path | name。
type Conflict struct {
	Kind      string         `json:"kind"`
	Component string         `json:"component,omitempty"`
	Message   string         `json:"message"`
	Hint      string         `json:"hint"`
	Details   map[string]any `json:"details,omitempty"`
}

// Assess 在探测结果之上判定「真实冲突」（FR-7：宝塔存在本身不是冲突，只有端口/路径/同名才阻止）。
// 返回按 (kind, component, message) 排序的冲突；空 = 没有冲突。它不修改任何东西，也不建议自动停用——只列出占用者与处置选项。
//
// 注意：ListenersKnown=false 时端口冲突无法判定，Assess 会为每个想要的端口返回一条 kind=port、code=unknown 的冲突
// （无法证明无冲突 → 保守阻断，由调用方决定是否让用户确认）。
func Assess(res Result, fsys baota.FS, in Intent) []Conflict {
	var out []Conflict
	// 端口
	for _, w := range in.Ports {
		proto := resolver.NormProto(w.Proto)
		bind := w.Bind
		if bind == "" {
			bind = "0.0.0.0"
		}
		if !res.ListenersKnown {
			out = append(out, Conflict{Kind: "port", Component: w.Component,
				Message: fmt.Sprintf("无法检测 %s/%d 是否被占用（ss 与 /proc 均不可用）", proto, w.Port),
				Hint:    "请用 root 运行，或确认该端口空闲后再继续", Details: map[string]any{"proto": proto, "port": w.Port, "code": "unknown"}})
			continue
		}
		var hits []Socket
		kinds := map[string]bool{}
		for _, s := range res.Sockets {
			l := s.Listener
			if resolver.NormProto(l.Proto) != proto || l.Port != w.Port {
				continue
			}
			if ok, kind := resolver.AddrConflict(bind, l.Addr); ok {
				hits = append(hits, s)
				kinds[kind] = true
			}
		}
		if len(hits) == 0 {
			continue
		}
		// 同一端口上可能有多个监听（0.0.0.0 与 ::、多个地址）：合并为一条冲突，主占用者取第一个可见进程的监听
		primary := hits[0]
		for _, h := range hits {
			if h.Listener.PID > 0 {
				primary = h
				break
			}
		}
		l := primary.Listener
		kind := "exact"
		if kinds["wildcard"] {
			kind = "wildcard"
		}
		who := l.Process
		if who == "" {
			who = "未知进程（无权限查看）"
		}
		hint := "换端口（--set）、释放占用者（Tools 不会自动停用任何服务），或取消"
		if res.Baota.Detected && (w.Port == 80 || w.Port == 443) {
			hint = "宝塔环境下 80/443 被占用（R-10）：(a) 迁移宝塔站点端口，(b) 节点与面板分机，(c) 取消；Tools 不修改宝塔站点"
		}
		occupants := make([]map[string]any, 0, len(hits))
		for _, h := range hits {
			procs := make([]map[string]any, 0, len(h.Procs))
			for _, p := range h.Procs {
				procs = append(procs, map[string]any{"pid": p.PID, "name": p.Name})
			}
			occupants = append(occupants, map[string]any{"process": h.Listener.Process, "pid": h.Listener.PID, "user": h.Listener.User, "addr": h.Listener.Addr, "unit": h.Listener.Unit, "procs": procs, "hidden": h.Hidden})
		}
		out = append(out, Conflict{Kind: "port", Component: w.Component,
			Message: fmt.Sprintf("端口冲突：%s 要占用 %s %s/%d，本机已被 %s（pid %d，属主 %s）监听 %s（%s冲突，共 %d 个监听）", w.Component, bind, proto, w.Port, who, l.PID, orDash(l.User), l.Addr, kindZh(kind), len(hits)),
			Hint:    hint,
			Details: map[string]any{"proto": proto, "port": w.Port, "match": kind, "bind": bind, "occupant": occupants[0], "occupants": occupants}})
	}
	// 路径
	for _, w := range in.Paths {
		p := path.Clean(w.Path)
		if !path.IsAbs(p) {
			continue
		}
		if res.Baota.Detected && (p == "/www" || strings.HasPrefix(p, "/www/")) {
			out = append(out, Conflict{Kind: "path", Component: w.Component,
				Message: fmt.Sprintf("路径冲突：%s 位于宝塔管理目录 /www 之下", p),
				Hint:    "LingEdge 使用独立前缀 /opt/lingedge、/etc/lingedge、/var/lib/lingedge；Tools 不写入宝塔目录", Details: map[string]any{"path": p}})
			continue
		}
		if w.OwnedByUs || fsys == nil {
			continue
		}
		if fi, err := fsys.Lstat(p); err == nil {
			out = append(out, Conflict{Kind: "path", Component: w.Component,
				Message: fmt.Sprintf("路径冲突：%s 已存在（%s）且不属于 Tools 的安装记录", p, fileKind(fi.IsDir(), fi.Mode().IsRegular())),
				Hint:    "移走或改用其他目录；Tools 不覆盖既有路径", Details: map[string]any{"path": p}})
		}
	}
	// 同名站点
	for _, n := range in.Names {
		want := strings.ToLower(strings.TrimSpace(n.ServerName))
		if want == "" {
			continue
		}
		for _, s := range res.Baota.Sites {
			for _, have := range s.ServerNames {
				if serverNameMatches(strings.ToLower(have), want) {
					out = append(out, Conflict{Kind: "name", Component: n.Component,
						Message: fmt.Sprintf("同名冲突：站点名 %s 已被 %s 站点配置 %s 使用（server_name %s）", want, s.Source, s.File, have),
						Hint:    "更换域名，或先在对应站点配置中调整；Tools 不修改宝塔/系统站点", Details: map[string]any{"server_name": have, "file": s.File, "source": s.Source}})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		return a.Message < b.Message
	})
	return out
}

func kindZh(k string) string {
	if k == "wildcard" {
		return "通配"
	}
	return "同地址"
}

func fileKind(dir, reg bool) string {
	switch {
	case dir:
		return "目录"
	case reg:
		return "文件"
	}
	return "其他"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// serverNameMatches 判断两个 nginx server_name 是否会匹配同一主机名：精确相等；`*.example.com` 匹配其子域；`.example.com` 匹配自身与子域。
// 正则（~ 开头）与 `_`（默认站点）不参与比较——无法静态判定，避免误报。
func serverNameMatches(a, b string) bool {
	if a == "" || b == "" || a[0] == '~' || b[0] == '~' || a == "_" || b == "_" {
		return false
	}
	if a == b {
		return true
	}
	return wildMatch(a, b) || wildMatch(b, a)
}

func wildMatch(pat, host string) bool {
	switch {
	case strings.HasPrefix(pat, "*."):
		return strings.HasSuffix(host, pat[1:]) && len(host) > len(pat)-1
	case strings.HasPrefix(pat, "."):
		return host == pat[1:] || strings.HasSuffix(host, pat)
	}
	return false
}
