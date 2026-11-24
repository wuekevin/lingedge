package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/doctor"
)

var categoryOrder = []string{"system", "port", "baota", "dependency", "service", "health", "files", "frontend", "certificate", "config", "install", "backup", "version"}

func categoryLabel(c string) string {
	return map[string]string{"system": "系统", "port": "端口", "baota": "宝塔/共存", "dependency": "依赖", "service": "服务", "health": "健康", "files": "文件与权限",
		"frontend": "静态前端", "certificate": "证书与有效期", "config": "配置", "install": "安装一致性", "backup": "备份", "version": "版本与信任"}[c]
}

// doctor [--all|<组件>] [--strict] [--fix-perms]（tools-design §19）。有 fail → 退出码 3；--strict 把 warn 视同 fail。
func runDoctor(inv *invocation) (*Result, error) {
	r := inv.r
	filter := doctor.Filter{}
	if len(inv.args) == 1 {
		comps, e := r.installedComponents(inv.args)
		if e != nil {
			return nil, e
		}
		filter.Component = comps[0].Name
	}
	fix := inv.flag("fix-perms")
	w, _ := r.doctorWorld(r.ctx, fix)
	reg, err := doctor.Default(r.ops().ExtraChecks...)
	if err != nil {
		return nil, newErr(ExitGeneral, "doctor.registry_invalid", "检查项注册表不合法："+err.Error(), "这是实现缺陷，请报告")
	}
	rep := reg.Run(r.ctx, w, filter)
	strict := inv.flag("strict")
	res := &Result{
		Data: map[string]any{"checks": rep.Findings, "summary": rep.Summary, "strict": strict, "fix_perms": fix},
		Human: func(out io.Writer) {
			by := map[string][]doctor.Finding{}
			for _, f := range rep.Findings {
				by[f.Category] = append(by[f.Category], f)
			}
			cats := append([]string{}, categoryOrder...)
			for c := range by {
				known := false
				for _, k := range categoryOrder {
					known = known || k == c
				}
				if !known {
					cats = append(cats, c)
				}
			}
			for _, c := range cats {
				fs := by[c]
				if len(fs) == 0 {
					continue
				}
				label := categoryLabel(c)
				if label == "" {
					label = c
				}
				wf(out, "【%s】\n", label)
				for _, f := range fs {
					sym := map[doctor.Level]string{doctor.LevelPass: "✓", doctor.LevelWarn: "!", doctor.LevelFail: "✗", doctor.LevelSkip: "-"}[f.Level]
					fixed := ""
					if f.Fixed {
						fixed = "（已修复）"
					}
					wf(out, "  %s %-34s %s%s\n", sym, f.ID, f.Message, fixed)
					if f.Hint != "" && f.Level != doctor.LevelPass {
						wf(out, "      → %s\n", f.Hint)
					}
				}
			}
			wf(out, "\n通过 %d　警告 %d　失败 %d　跳过 %d\n", rep.Summary.Pass, rep.Summary.Warn, rep.Summary.Fail, rep.Summary.Skip)
		},
	}
	if rep.Failed(strict) {
		var ids []string
		for _, f := range rep.Findings {
			if f.Level == doctor.LevelFail || (strict && f.Level == doctor.LevelWarn) {
				ids = append(ids, f.ID)
			}
		}
		shown := ids
		if len(shown) > 5 {
			shown = shown[:5]
		}
		msg := fmt.Sprintf("%d 项失败", rep.Summary.Fail)
		if strict && rep.Summary.Warn > 0 {
			msg += fmt.Sprintf("、%d 项警告（--strict 视同失败）", rep.Summary.Warn)
		}
		return res, &Error{Exit: ExitPrecondition, Code: "doctor.failed", Message: "doctor：" + msg + "：" + strings.Join(shown, "、"),
			Hint: "按各项 → 后的提示处理；lingedge doctor --json 可机读全部结果", Details: map[string]any{"failed": ids}}
	}
	return res, nil
}
