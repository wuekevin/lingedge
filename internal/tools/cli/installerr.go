package cli

import (
	"errors"

	"github.com/wuekevin/lingedge/internal/tools/installer"
)

// InstallError 把 internal/tools/installer 的失败映射为 CLI 结构化错误（Story 1.33）。它是退出码 9（冲突需确认）、10（激活后失败已回滚）、
// 11（健康检查失败且 --no-rollback）、12（撤销失败，journal 保留）、130（SIGINT 后已回滚）的真实生产者：
//
//	spec_missing       → 1   install.component_spec_missing   组件的安装规格尚未交付（生产构建当前没有任何组件规格，直到 1.35 等 Story）
//	settings           → 2   usage.invalid_set                --set 被组件的 schema 拒绝
//	config             → 2   usage.invalid_config             tools.yaml 不合法
//	upgrade_required   → 3   precondition.upgrade_required    计划含已装组件的升级，须走 lingedge update（Story 1.43）
//	precondition       → 3   precondition.install             systemd 缺失/过旧、release.base_url 缺失、目标 release 已存在且内容不同…
//	port_unverified    → 4   port.unverified                  ss 与 /proc 均不可用且未确认
//	confirm_required   → 9   confirm.required                 既有配置/单元/站点内容不同等需要显式确认（--yes / 交互 y）
//	rolled_back        → 10  install.rolled_back              激活后失败，已逆序撤销并恢复
//	health_failed      → 11  install.health_failed            健康检查失败且 --no-rollback
//	rollback_failed    → 12  install.rollback_failed          撤销失败，journal 保留（lingedge recover --rollback 重试）
//	interrupted        → 130 interrupt.sigint                 SIGINT：当前步骤完成后已回滚
//	recover            → 3   recover.not_possible             recover 的前置不满足
//	step_failed        → 沿 Cause 的映射（校验失败 5、传输失败 1、systemd 3…），并补上失败步骤与系统状态（rolled_back）
//	internal           → 1   install.internal
//
// 不是 installer 的错误返回 nil。
func InstallError(err error) *Error {
	var ie *installer.Error
	if !errors.As(err, &ie) {
		return nil
	}
	e := &Error{Exit: ExitGeneral, Message: ie.Msg, Hint: ie.Hint, Step: ie.Step, SystemState: ie.SystemState, Cause: ie.Cause}
	if len(ie.Details) > 0 {
		e.Details = map[string]any{}
		for k, v := range ie.Details {
			if k == "plan" {
				e.Details[k] = toAny(v)
				continue
			}
			e.Details[k] = v
		}
	}
	if len(ie.Conflicts) > 0 {
		if e.Details == nil {
			e.Details = map[string]any{}
		}
		e.Details["conflicts"] = toAny(ie.Conflicts)
	}
	switch ie.Kind {
	case installer.KindSpecMissing:
		e.Exit, e.Code = ExitGeneral, "install.component_spec_missing"
	case installer.KindSettings:
		e.Exit, e.Code = ExitUsage, "usage.invalid_set"
	case installer.KindConfig:
		e.Exit, e.Code = ExitUsage, "usage.invalid_config"
	case installer.KindUpgradeRequired:
		e.Exit, e.Code = ExitPrecondition, "precondition.upgrade_required"
	case installer.KindPrecondition:
		e.Exit, e.Code = ExitPrecondition, "precondition.install"
	case installer.KindPortUnverified:
		e.Exit, e.Code = ExitPortConflict, "port.unverified"
	case installer.KindConfirmRequired:
		e.Exit, e.Code = ExitConfirmRequired, "confirm.required"
	case installer.KindRolledBack:
		e.Exit, e.Code = ExitRolledBack, "install.rolled_back"
	case installer.KindHealthFailed:
		e.Exit, e.Code = ExitHealthFailed, "install.health_failed"
	case installer.KindRollbackFailed:
		e.Exit, e.Code = ExitRollbackFailed, "install.rollback_failed"
	case installer.KindInterrupted:
		e.Exit, e.Code = ExitSigint, "interrupt.sigint"
	case installer.KindRecover:
		e.Exit, e.Code = ExitPrecondition, "recover.not_possible"
	case installer.KindStepFailed:
		inner := ie.Cause
		if c, ok := classify(inner); ok && c != nil {
			if ie.Step > 0 && c.Step == 0 {
				c.Step = ie.Step
			}
			if ie.SystemState != "" {
				c.SystemState = ie.SystemState
			}
			if c.Details == nil {
				c.Details = map[string]any{}
			}
			for k, v := range ie.Details {
				if _, dup := c.Details[k]; !dup {
					c.Details[k] = v
				}
			}
			return c
		}
		e.Exit, e.Code = ExitGeneral, "install.step_failed"
		if e.Hint == "" {
			e.Hint = "详见日志；失败已按 journal 回滚"
		}
	default:
		e.Exit, e.Code = ExitGeneral, "install.internal"
	}
	if e.Hint == "" {
		e.Hint = "详见日志与 details"
	}
	return e
}
