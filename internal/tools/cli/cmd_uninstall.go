package cli

import (
	"io"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/installer"
)

// uninstallData 是 `uninstall` 的 data（schema/data.uninstall.schema.json）。
type uninstallData struct {
	Component string                    `json:"component"`
	Outcome   string                    `json:"outcome"`
	Tx        string                    `json:"tx,omitempty"`
	Purge     bool                      `json:"purge"`
	DryRun    bool                      `json:"dry_run"`
	Items     []installer.UninstallItem `json:"items"`
}

func runUninstall(inv *invocation) (*Result, error) {
	r := inv.r
	comp := inv.args[0]
	in, err := r.newInstaller(nil, installer.Request{})
	if err != nil {
		return nil, err
	}
	req := installer.UninstallRequest{Component: comp, Purge: inv.flag("purge"), DryRun: r.opts.DryRun}
	var res *installer.UninstallResult
	if req.DryRun {
		res, err = in.PlanUninstall(r.ctx, req)
	} else {
		if inv.task == nil {
			return nil, newErr(ExitGeneral, "general.unexpected", "uninstall 没有取得任务锁", "")
		}
		res, err = in.Uninstall(r.ctx, inv.task, req)
	}
	if err != nil {
		return nil, err
	}
	d := uninstallData{Component: comp, Outcome: res.Outcome, Tx: res.Tx, Purge: res.Purge, DryRun: res.DryRun, Items: res.Items}
	return &Result{Data: d, Warnings: toWarnings(res.Warnings), Human: func(w io.Writer) {
		switch {
		case res.DryRun:
			wf(w, "卸载计划（--dry-run：未做任何修改）：%s%s\n", comp, map[bool]string{true: "，--purge", false: ""}[res.Purge])
		default:
			wf(w, "已卸载 %s%s\n", comp, map[bool]string{true: "（--purge：配置、数据与备份已删除）", false: "（配置、数据与备份已保留）"}[res.Purge])
		}
		verb := map[string]string{"remove": "删除", "keep": "保留", "absent": "不存在", "skip": "未动"}
		for _, it := range res.Items {
			p := it.Path
			if p == "" {
				p = "-"
			}
			wf(w, "  %-4s %-13s %s", verb[it.Action], it.Kind, p)
			if it.Note != "" {
				wf(w, "  （%s）", strings.TrimSpace(it.Note))
			}
			wln(w)
		}
		if !res.DryRun && !res.Purge {
			wln(w, "\n重新安装：lingedge install "+comp+"；彻底删除数据：lingedge uninstall "+comp+" --purge --confirm-name <主机名>")
		}
	}}, nil
}
