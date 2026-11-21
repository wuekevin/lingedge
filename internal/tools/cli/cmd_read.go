package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/service"
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
		if comps[i].Name == "server" {
			comps[i].Setup = inv.r.serverSetupInfo(comps[i])
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
				state := c.State
				if c.Setup != nil {
					state += "（" + c.Setup.Detail + "）"
				}
				wf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, c.Version, state, strings.Join(ports, ","), c.ReleaseDir, c.RunUser)
			}
			_ = tw.Flush()
		},
	}, nil
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
