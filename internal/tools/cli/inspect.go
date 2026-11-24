package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/doctor"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// UnitInfo 是一个 systemd 单元的真实状态（systemctl show）。
type UnitInfo struct {
	Unit          string `json:"unit"`
	ActiveState   string `json:"active_state"`
	SubState      string `json:"sub_state"`
	UnitFileState string `json:"unit_file_state"`
	MainPID       int    `json:"main_pid"`
	Restarts      int    `json:"restarts"`
	ActiveSince   string `json:"active_since,omitempty"`
}

// SystemdInfo 是组件的 systemd 视图。Managed=false：该组件没有 Tools 管理的服务单元（静态前端由 Nginx 提供）。
// Active：主单元是否 active（Managed=false 或读不到状态时为 null）。
type SystemdInfo struct {
	Managed bool       `json:"managed"`
	Active  *bool      `json:"active"`
	Units   []UnitInfo `json:"units"`
	Note    string     `json:"note,omitempty"`
}

// HealthInfo 是健康探测结果。Status：ok | degraded | unhealthy | unknown | n/a。
type HealthInfo struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
	Check  string `json:"check,omitempty"`
}

// ListenerInfo 是组件端口上的真实监听者（FR-58 的「进程归属」）。
type ListenerInfo struct {
	Proto   string `json:"proto"`
	Addr    string `json:"addr"`
	Port    int    `json:"port"`
	PID     int    `json:"pid"`
	Process string `json:"process"`
	User    string `json:"user"`
	// Ours：监听者属于该组件自己（单元/运行用户匹配）。
	Ours bool `json:"ours"`
}

// ProcessInfo 是组件的进程归属：期望的运行用户、systemd 主进程 pid、端口上的实际监听者。
type ProcessInfo struct {
	RunUser   string         `json:"run_user"`
	MainPID   int            `json:"main_pid"`
	Listeners []ListenerInfo `json:"listeners"`
	// Note 说明不可得的原因（如无权查看他人进程）。
	Note string `json:"note,omitempty"`
}

// SiteInfo 是静态前端的站点信息。
type SiteInfo struct {
	Host      string `json:"host,omitempty"`
	Root      string `json:"root"`
	NginxConf string `json:"nginx_conf"`
	ConfDLink string `json:"conf_d_link"` // ours | absent | foreign | unknown
}

// Inspected 是 status/service status 的一个组件项。
type Inspected struct {
	Name    string      `json:"name"`
	Version string      `json:"version"`
	State   string      `json:"state"`
	Healthy *bool       `json:"healthy"`
	Systemd SystemdInfo `json:"systemd"`
	Health  HealthInfo  `json:"health"`
}

func boolp(b bool) *bool { return &b }

// inspectSystemd 读取组件各单元的真实 systemd 状态。
func (r *Run) inspectSystemd(ctx context.Context, c ComponentInfo, sc *service.Systemctl) (SystemdInfo, string) {
	units := service.StatusUnits(c.Name)
	if len(units) == 0 {
		note := "没有 Tools 管理的服务单元"
		if installer.IsStaticComponent(c.Name) {
			note = "静态前端没有服务进程，由 Nginx 直接提供文件"
		}
		return SystemdInfo{Managed: false, Units: []UnitInfo{}, Note: note}, ""
	}
	out := SystemdInfo{Managed: true, Units: []UnitInfo{}}
	for _, u := range units {
		p, err := sc.Show(ctx, u)
		if err != nil {
			out.Note = "无法读取 systemd 状态：" + redactString(err.Error())
			return out, out.Note
		}
		out.Units = append(out.Units, UnitInfo{Unit: string(u), ActiveState: p.ActiveState, SubState: p.SubState, UnitFileState: p.UnitFileState,
			MainPID: p.MainPID, Restarts: p.NRestarts, ActiveSince: p.ActiveEnterTimestamp})
	}
	out.Active = boolp(out.Units[0].ActiveState == "active")
	return out, ""
}

func levelToHealth(l doctor.Level) string {
	switch l {
	case doctor.LevelPass:
		return "ok"
	case doctor.LevelWarn:
		return "degraded"
	case doctor.LevelFail:
		return "unhealthy"
	}
	return "unknown"
}

// inspectHealth 做组件的健康探测：server → /healthz（向导完成后加 /healthz/ready）；静态前端 → 发布物与站点片段的深度检查；其余没有探针的组件 n/a。
func (r *Run) inspectHealth(ctx context.Context, c ComponentInfo, w *doctor.World) HealthInfo {
	switch {
	case c.Name == "server":
		f := w.CheckServerHealth(ctx)
		return HealthInfo{Status: levelToHealth(f.Level), Detail: f.Message, Check: f.ID}
	case installer.IsStaticComponent(c.Name):
		worst := doctor.LevelPass
		var details []string
		for _, f := range w.CheckFrontend(toDoctorComponents([]ComponentInfo{c})[0]) {
			if f.Level == doctor.LevelFail || (f.Level == doctor.LevelWarn && worst == doctor.LevelPass) {
				worst = f.Level
			}
			if f.Level != doctor.LevelPass {
				details = append(details, f.Message)
			}
		}
		d := "发布物与站点片段正常"
		if len(details) > 0 {
			d = strings.Join(details, "；")
		}
		return HealthInfo{Status: levelToHealth(worst), Detail: d, Check: "fe." + c.Name}
	case c.Name == "node":
		return HealthInfo{Status: "n/a", Detail: "Node Agent 的连通/心跳探测随 Epic 3（Story 3.31）交付；此处只有 systemd 状态"}
	}
	return HealthInfo{Status: "n/a", Detail: "该组件尚无健康探针"}
}

// inspectComponents 汇总每个组件的真实 systemd 状态与健康探测结果。返回 warnings（如 systemd 不可用）。
func (r *Run) inspectComponents(ctx context.Context, comps []ComponentInfo) ([]Inspected, []string) {
	sc := r.systemctl()
	w := &doctor.World{Root: r.env.Install.Root, Now: r.env.Now, Components: toDoctorComponents(comps), HTTPGet: r.httpGet, ServerListens: r.serverListens}
	var warns []string
	out := make([]Inspected, 0, len(comps))
	for _, c := range comps {
		it := Inspected{Name: c.Name, Version: c.Version, State: c.State}
		sd, warn := r.inspectSystemd(ctx, c, sc)
		if warn != "" {
			warns = append(warns, c.Name+"："+warn)
		}
		it.Systemd = sd
		it.Health = r.inspectHealth(ctx, c, w)
		it.Healthy = healthyOf(it)
		out = append(out, it)
	}
	return out, warns
}

// healthyOf 综合判定：systemd 主单元不是 active，或健康探测 unhealthy → false；两者都确认正常 → true；信息不足 → nil（未知）。
func healthyOf(it Inspected) *bool {
	if it.Health.Status == "unhealthy" {
		return boolp(false)
	}
	if it.Systemd.Managed {
		if it.Systemd.Active == nil {
			return nil
		}
		if !*it.Systemd.Active {
			return boolp(false)
		}
	}
	switch it.Health.Status {
	case "ok", "degraded":
		return boolp(true)
	case "n/a":
		if it.Systemd.Managed {
			return boolp(true)
		}
		return nil
	}
	return nil
}

// StatusSummary 是 status 的汇总计数。
type StatusSummary struct {
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
	Unknown   int `json:"unknown"`
}

func summarize(items []Inspected) StatusSummary {
	var s StatusSummary
	for _, it := range items {
		switch {
		case it.Healthy == nil:
			s.Unknown++
		case *it.Healthy:
			s.Healthy++
		default:
			s.Unhealthy++
		}
	}
	return s
}

func unhealthyNames(items []Inspected) []string {
	var out []string
	for _, it := range items {
		if it.Healthy != nil && !*it.Healthy {
			out = append(out, it.Name)
		}
	}
	return out
}

// healthFailedError 在存在不健康组件时返回退出码 11（HEALTH_FAILED）。
func healthFailedError(items []Inspected) *Error {
	names := unhealthyNames(items)
	if len(names) == 0 {
		return nil
	}
	var why []string
	for _, it := range items {
		if it.Healthy != nil && !*it.Healthy {
			d := it.Health.Detail
			if it.Systemd.Active != nil && !*it.Systemd.Active && len(it.Systemd.Units) > 0 {
				d = it.Systemd.Units[0].Unit + " 状态 " + it.Systemd.Units[0].ActiveState
			}
			why = append(why, it.Name+"："+d)
		}
	}
	return &Error{Exit: ExitHealthFailed, Code: "health.unhealthy", Message: fmt.Sprintf("%d 个组件不健康：%s", len(names), strings.Join(why, "；")),
		Hint: "lingedge doctor 查看逐项诊断；lingedge logs <组件> 查看日志；修复后 lingedge service restart <组件>", Details: map[string]any{"components": names}}
}

// processInfo 汇总组件端口上的真实监听者与主进程（list 的「进程归属」，FR-58）。
func (r *Run) processInfo(c ComponentInfo, res *probe.Result, mainPID int) ProcessInfo {
	pi := ProcessInfo{RunUser: c.RunUser, MainPID: mainPID, Listeners: []ListenerInfo{}}
	if res == nil {
		pi.Note = "无法采集监听信息"
		return pi
	}
	if !res.ListenersKnown {
		pi.Note = "无法采集监听信息（ss 与 /proc 均不可用）"
		return pi
	}
	hidden := false
	ports := map[int]bool{}
	for _, p := range c.Ports {
		ports[p] = true
	}
	for _, s := range res.Sockets {
		if !ports[s.Listener.Port] {
			continue
		}
		if s.Hidden {
			hidden = true
		}
		ours := (c.Unit != "" && s.Listener.Unit == c.Unit) || (c.RunUser != "" && s.Listener.User == c.RunUser)
		pi.Listeners = append(pi.Listeners, ListenerInfo{Proto: s.Listener.Proto, Addr: s.Listener.Addr, Port: s.Listener.Port, PID: s.Listener.PID,
			Process: s.Listener.Process, User: s.Listener.User, Ours: ours})
	}
	if hidden {
		pi.Note = "部分监听者的进程信息不可见（需要 root 才能查看他人进程）"
	}
	return pi
}

// siteInfo 是静态前端的站点信息（只读）。
func (r *Run) siteInfo(c ComponentInfo) *SiteInfo {
	if !installer.IsStaticComponent(c.Name) {
		return nil
	}
	si := &SiteInfo{Root: installer.StaticRoot(c.Name), NginxConf: installer.StaticConfPath(c.Name), ConfDLink: "unknown"}
	if b, _, err := readRegular(r.hostPath(si.NginxConf), 1<<20, false); err == nil {
		_, si.Host, _ = installer.ParseSiteMarker(b)
	}
	link := r.hostPath(installer.StaticLinkPath(c.Name))
	st, err := os.Lstat(link)
	switch {
	case err != nil && os.IsNotExist(err):
		si.ConfDLink = "absent"
	case err != nil:
	case st.Mode()&os.ModeSymlink != 0:
		if t, rerr := os.Readlink(link); rerr == nil && t == installer.StaticConfPath(c.Name) {
			si.ConfDLink = "ours"
		} else {
			si.ConfDLink = "foreign"
		}
	default:
		si.ConfDLink = "foreign"
	}
	return si
}
