package cli

import (
	"encoding/json"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

// ResolveError 把 internal/tools/resolver 的失败映射为 CLI 结构化错误（Story 1.30）。
// 它是退出码 3（无解/资源/系统）、4（端口冲突）、7（NOT_RELEASED）、8（BUILTIN）、9（被动升级需确认）的真实生产者，
// 并与 VerifyError 一致地把「固定版本命中 revoked_versions」映射为 5：
//
//	usage         → 2  usage.invalid_value        未知组件/固定版本不存在/内部组件被直接选择/重复选择/非法 --set
//	not_released  → 7  component.not_released     status: unreleased（如 MVP 的 install dns）：不伪造成功
//	builtin       → 8  component.builtin          scheduler|worker 已内置于 Server（D-16）
//	revoked       → 5  verify.version_revoked
//	unsatisfiable → 3  resolve.unsatisfiable      依赖/版本约束无解，冲突链在 why/details.chain
//	cycle         → 3  resolve.dependency_cycle
//	upgrade_path  → 3  resolve.upgrade_path       min_upgrade_from 不满足或降级（details 含逐级升级路径）
//	system        → 3  precondition.system
//	resource      → 3  precondition.resources     内存/磁盘不足（按挂载点）、挂载点未知/只读
//	port_conflict → 4  port.conflict              端口冲突（组件间/与本机监听），含占用者
//	confirm       → 9  confirm.passive_upgrade    已装组件被动升级需确认
//
// 多个阻断并存时按 resolver.Error.Kind（优先级最高者）取退出码，全部阻断与完整 Plan 放进 details（--dry-run 同样展示）。
// 命令实现（1.33）直接返回 plan.Err() 即可；本函数不执行任何安装。
func ResolveError(re *resolver.Error) *Error {
	e := &Error{Exit: ExitGeneral, Message: re.Msg, SystemState: "unchanged", Cause: re}
	details := map[string]any{}
	if len(re.Blockers) > 0 {
		var bl []any
		for _, b := range re.Blockers {
			bl = append(bl, toAny(b))
		}
		details["blockers"] = bl
	}
	if re.Plan != nil {
		details["plan"] = toAny(re.Plan)
	}
	var primary *resolver.Blocker
	for i := range re.Blockers {
		if re.Blockers[i].Kind == re.Kind {
			primary = &re.Blockers[i]
			break
		}
	}
	if primary != nil {
		e.Hint = primary.Hint
		if len(primary.Chain) > 0 {
			e.Why = strings.Join(primary.Chain, "\n")
		}
		e.Message = primary.Message
	}
	if len(details) > 0 {
		e.Details = details
	}
	switch re.Kind {
	case resolver.KindUsage:
		e.Exit, e.Code = ExitUsage, "usage.invalid_value"
	case resolver.KindNotReleased:
		e.Exit, e.Code = ExitNotReleased, "component.not_released"
	case resolver.KindBuiltin:
		e.Exit, e.Code = ExitBuiltin, "component.builtin"
	case resolver.KindRevoked:
		e.Exit, e.Code = ExitVerifyFailed, "verify.version_revoked"
	case resolver.KindUnsatisfiable:
		e.Exit, e.Code = ExitPrecondition, "resolve.unsatisfiable"
	case resolver.KindCycle:
		e.Exit, e.Code = ExitPrecondition, "resolve.dependency_cycle"
	case resolver.KindUpgradePath:
		e.Exit, e.Code = ExitPrecondition, "resolve.upgrade_path"
	case resolver.KindSystem:
		e.Exit, e.Code = ExitPrecondition, "precondition.system"
	case resolver.KindResource:
		e.Exit, e.Code = ExitPrecondition, "precondition.resources"
	case resolver.KindPort:
		e.Exit, e.Code = ExitPortConflict, "port.conflict"
	case resolver.KindConfirm:
		e.Exit, e.Code = ExitConfirmRequired, "confirm.passive_upgrade"
	default:
		e.Exit, e.Code = ExitGeneral, "resolve.unknown"
	}
	if e.Hint == "" {
		e.Hint = "详见 details.blockers"
	}
	return e
}

// toAny 经 JSON 往返得到 []any/map[string]any 动态类型：envelope 的脱敏只遍历这两种类型。
func toAny(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}
