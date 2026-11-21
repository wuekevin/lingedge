package cli

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/wuekevin/lingedge/pkg/version"
)

func toolsInfo() map[string]any {
	return map[string]any{
		"version": version.Version, "commit": version.Commit,
		"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
	}
}

func notYet(inv *invocation, flag, story string) *Error {
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
		return nil, notYet(inv, "check", "Story 1.27/1.28（清单与下载）")
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

// list：已装组件（tools-design §6 data.components[]）。
func runList(inv *invocation) (*Result, error) {
	if inv.flag("available") {
		return nil, notYet(inv, "available", "Story 1.27/1.28（清单与下载）")
	}
	comps, err := inv.r.env.State.Components(inv.r.ctx)
	if err != nil {
		return nil, err
	}
	for i := range comps {
		if comps[i].Ports == nil {
			comps[i].Ports = []int{}
		}
	}
	return &Result{
		Data: map[string]any{"components": comps},
		Human: func(w io.Writer) {
			if len(comps) == 0 {
				wln(w, "未安装任何组件")
				return
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			wln(tw, "组件\t版本\t状态\t端口\t目录\t用户")
			for _, c := range comps {
				ports := make([]string, len(c.Ports))
				for i, p := range c.Ports {
					ports[i] = fmt.Sprint(p)
				}
				wf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, c.Version, c.State, strings.Join(ports, ","), c.ReleaseDir, c.RunUser)
			}
			_ = tw.Flush()
		},
	}, nil
}

// status：本 Story 只汇总 registry 记录的安装状态；systemd 状态与健康探测随 Story 1.42 补全（data 字段只增）。
func runStatus(inv *invocation) (*Result, error) {
	comps, err := inv.r.env.State.Components(inv.r.ctx)
	if err != nil {
		return nil, err
	}
	type sc struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		State   string `json:"state"`
	}
	list := make([]sc, 0, len(comps))
	for _, c := range comps {
		list = append(list, sc{c.Name, c.Version, c.State})
	}
	return &Result{
		Data: map[string]any{"components": list},
		Human: func(w io.Writer) {
			if len(list) == 0 {
				wln(w, "未安装任何组件")
				return
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			wln(tw, "组件\t版本\t状态")
			for _, c := range list {
				wf(tw, "%s\t%s\t%s\n", c.Name, c.Version, c.State)
			}
			_ = tw.Flush()
		},
	}, nil
}
