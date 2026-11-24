package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/pkg/version"
)

func toolsInfo() map[string]any {
	return map[string]any{
		"version": version.Version, "commit": version.Commit,
		"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
	}
}

func notYet(_ *invocation, flag, story string) *Error {
	return &Error{
		Exit: ExitGeneral, Code: "cli.not_implemented",
		Message: fmt.Sprintf("--%s 尚未实现（%s 交付）", flag, story),
		Hint:    "该选项当前不可用，未做任何修改",
		Details: map[string]any{"flag": flag, "story": story},
	}
}

// version：Tools 版本 + 已装组件（来自 state.json；无文件则为空）。
func runVersion(inv *invocation) (*Result, error) {
	if inv.flag("check") {
		return nil, notYet(inv, "check", "Story 1.43（update check 的清单比对）")
	}
	comps, err := inv.r.env.State.Components(inv.r.ctx)
	if err != nil {
		return nil, err
	}
	type vc struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	list := make([]vc, 0, len(comps))
	for _, c := range comps {
		list = append(list, vc{c.Name, c.Version})
	}
	return &Result{
		Data: map[string]any{"tools": toolsInfo(), "components": list},
		Human: func(w io.Writer) {
			wln(w, version.Info("lingedge"))
			if len(list) == 0 {
				wln(w, "已装组件：（无）")
				return
			}
			wln(w, "已装组件：")
			for _, c := range list {
				wf(w, "  %-12s %s\n", c.Name, c.Version)
			}
		},
	}, nil
}

// list：已装组件（tools-design §6 data.components[]）。Story 1.42 起每项带进程归属（process：端口上的真实监听者与 systemd 主进程）与静态前端的站点信息（site）；
// --available 另附清单里的可装版本（经验签的清单，只读）。
func runList(inv *invocation) (*Result, error) {
	comps, err := inv.r.env.State.Components(inv.r.ctx)
	if err != nil {
		return nil, err
	}
	var avail []AvailableInfo
	var mfMeta map[string]any
	if inv.flag("available") {
		channel := "stable"
		if inv.changed("channel") {
			channel = inv.str("channel")
		}
		m, ferr := inv.r.fetchManifest(inv.r.ctx, channel)
		if ferr != nil {
			return nil, ferr
		}
		avail = availableFrom(m, comps)
		mfMeta = map[string]any{"channel": m.Channel, "sequence": m.Sequence, "generated_at": m.GeneratedAt.UTC().Format(time.RFC3339), "valid_until": m.ValidUntil.UTC().Format(time.RFC3339)}
	}
	inv.r.enrichList(inv.r.ctx, comps)
	data := map[string]any{"components": comps}
	if inv.flag("available") {
		if avail == nil {
			avail = []AvailableInfo{}
		}
		data["available"] = avail
		data["manifest"] = mfMeta
	}
	return &Result{
		Data: data,
		Human: func(w io.Writer) {
			if len(comps) == 0 {
				wln(w, "未安装任何组件")
			} else {
				tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
				wln(tw, "组件\t版本\t状态\t端口\t配置目录\t运行用户\t进程归属")
				for _, c := range comps {
					ports := make([]string, len(c.Ports))
					for i, p := range c.Ports {
						ports[i] = fmt.Sprint(p)
					}
					state := c.State
					if c.Setup != nil {
						state += "（" + c.Setup.Detail + "）"
					}
					wf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, c.Version, state, strings.Join(ports, ","), c.ConfigDir, c.RunUser, ownerSummary(c))
				}
				_ = tw.Flush()
			}
			if inv.flag("available") {
				wln(w, "\n清单中的可装组件：")
				tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
				wln(tw, "组件\t状态\t最新稳定版\t已装")
				for _, a := range avail {
					wf(tw, "%s\t%s\t%s\t%s\n", a.Component, a.Status, orDash(a.Latest), orDash(a.Installed))
				}
				_ = tw.Flush()
			}
		},
	}, nil
}

// ownerSummary 把进程归属压成一格：静态前端显示站点域名；服务显示端口上的监听者（pid/用户），没有监听者则说明。
func ownerSummary(c ComponentInfo) string {
	if c.Site != nil {
		h := c.Site.Host
		if h == "" {
			h = "（站点域名未知）"
		}
		return "nginx 站点 " + h
	}
	if c.Process == nil {
		return "-"
	}
	var parts []string
	for _, l := range c.Process.Listeners {
		who := fmt.Sprintf("%s(pid %d,%s)", l.Process, l.PID, l.User)
		if l.Process == "" {
			who = fmt.Sprintf("pid %d", l.PID)
		}
		if !l.Ours {
			who += "【非本组件】"
		}
		parts = append(parts, fmt.Sprintf("%d→%s", l.Port, who))
	}
	if len(parts) == 0 {
		if c.Process.MainPID > 0 {
			return fmt.Sprintf("主进程 pid %d，端口无监听", c.Process.MainPID)
		}
		return "无监听进程"
	}
	return strings.Join(parts, " ")
}

// AvailableInfo 是 list --available 的一项（清单里的组件）。
type AvailableInfo struct {
	Component string   `json:"component"`
	Status    string   `json:"status"`
	Latest    string   `json:"latest,omitempty"`
	Versions  []string `json:"versions"`
	Installed string   `json:"installed,omitempty"`
	Internal  bool     `json:"internal,omitempty"`
	BuiltinIn string   `json:"builtin_in,omitempty"`
}

func availableFrom(m *manifest.Manifest, comps []ComponentInfo) []AvailableInfo {
	names := make([]string, 0, len(m.Components))
	for n := range m.Components {
		names = append(names, n)
	}
	sort.Strings(names)
	inst := map[string]string{}
	for _, c := range comps {
		inst[c.Name] = c.Version
	}
	var out []AvailableInfo
	for _, n := range names {
		mc := m.Components[n]
		ai := AvailableInfo{Component: n, Status: mc.Status, Versions: []string{}, Installed: inst[n], Internal: mc.Internal, BuiltinIn: mc.BuiltinIn}
		var best *manifest.Version
		for _, v := range mc.Versions {
			ai.Versions = append(ai.Versions, v.Version)
			pv, err := manifest.ParseVersion(v.Version)
			if err != nil || pv.Prerelease() {
				continue
			}
			if best == nil || pv.Compare(*best) > 0 {
				p := pv
				best = &p
			}
		}
		if best != nil {
			ai.Latest = best.String()
		}
		out = append(out, ai)
	}
	return out
}

// enrichList 填充 list 的进程归属与站点信息（只读；采集失败如实标注，不影响其余字段）。
func (r *Run) enrichList(ctx context.Context, comps []ComponentInfo) {
	var res *probe.Result
	needProbe := false
	for _, c := range comps {
		if len(c.Ports) > 0 {
			needProbe = true
		}
	}
	if needProbe {
		if pr, err := r.collect(ctx, true); err == nil {
			res = &pr
		}
	}
	sc := r.systemctl()
	for i := range comps {
		c := &comps[i]
		if comps[i].Ports == nil {
			comps[i].Ports = []int{}
		}
		if c.Name == "server" {
			c.Setup = r.serverSetupInfo(*c)
		}
		if site := r.siteInfo(*c); site != nil {
			c.Site = site
			continue
		}
		pid := 0
		if units := service.StatusUnits(c.Name); len(units) > 0 && sc != nil {
			if p, err := sc.Show(ctx, units[0]); err == nil {
				pid = p.MainPID
			}
		}
		pi := r.processInfo(*c, res, pid)
		c.Process = &pi
	}
}

// serverSetupInfo 只读检查 <data_dir>/install.lock（Server 向导完成时写下）得出向导进展。非 root 对 0750 的数据目录没有搜索权限时报告 unknown，而不是猜测。
func (r *Run) serverSetupInfo(c ComponentInfo) *SetupInfo {
	dir := c.DataDir
	if dir == "" {
		dir = service.ServerDataDir
	}
	_, err := os.Lstat(r.hostPath(filepath.ToSlash(filepath.Join(dir, installer.InstallLockName))))
	switch {
	case err == nil:
		return &SetupInfo{Phase: "initialized", InstallLock: true, Detail: "已初始化：install.lock 存在"}
	case errors.Is(err, fs.ErrNotExist):
		return &SetupInfo{Phase: "wizard_pending", Detail: "setup mode：安装向导未完成（install.lock 不存在）"}
	default:
		return &SetupInfo{Phase: "unknown", Detail: "无法读取 " + dir + "（需要 root 或 lingedge 用户权限）"}
	}
}

// status：每个已装组件的真实 systemd 状态 + 健康探测结果（Story 1.42；data 字段只增）。有不健康的组件 → 退出码 11。
func runStatus(inv *invocation) (*Result, error) {
	comps, err := inv.r.env.State.Components(inv.r.ctx)
	if err != nil {
		return nil, err
	}
	items, warns := inv.r.inspectComponents(inv.r.ctx, comps)
	res := statusResult(items, warns)
	if he := healthFailedError(items); he != nil {
		return res, he
	}
	return res, nil
}
