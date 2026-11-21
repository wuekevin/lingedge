package resolver

import (
	"strconv"
	"strings"
)

// Kind 是阻断/失败的稳定分类（cli.ResolveError 据此映射退出码）。
type Kind string

// Kind 取值。
const (
	KindUsage         Kind = "usage"            // 用法：未知组件、固定版本不存在、重复/冲突的选择、内部组件被直接选择、非法覆盖（2）
	KindNotReleased   Kind = "not_released"     // status: unreleased（7）
	KindBuiltin       Kind = "builtin"          // scheduler|worker 或 status: builtin（8，D-16）
	KindRevoked       Kind = "revoked"          // 固定版本命中 revoked_versions（5，与 manifest.KindVersionRevoked 一致）
	KindUnsatisfiable Kind = "unsatisfiable"    // 依赖/版本约束无解（3，带冲突链）
	KindCycle         Kind = "cycle"            // 依赖成环（3）
	KindUpgradePath   Kind = "upgrade_path"     // 违反 min_upgrade_from 或降级（3，带逐级升级路径）
	KindSystem        Kind = "system"           // OS/arch/外部依赖（3）
	KindResource      Kind = "resource"         // 内存/磁盘不足、挂载点未知或只读（3）
	KindPort          Kind = "port_conflict"    // 端口冲突（4）
	KindConfirm       Kind = "confirm_required" // 被动升级未确认（9）
)

// Kinds 返回全部分类（cli 映射表的穷尽性测试用）。
func Kinds() []Kind {
	return []Kind{KindUsage, KindNotReleased, KindBuiltin, KindRevoked, KindUnsatisfiable, KindCycle, KindUpgradePath, KindSystem, KindResource, KindPort, KindConfirm}
}

// kindPriority 决定多个阻断并存时 *Error.Kind（进而退出码）取哪一个：越靠前越优先。
var kindPriority = []Kind{KindUsage, KindNotReleased, KindBuiltin, KindRevoked, KindUnsatisfiable, KindCycle, KindUpgradePath, KindSystem, KindResource, KindPort, KindConfirm}

// Error 是 Plan 不可执行时的错误：Kind 取阻断中优先级最高者，Blockers 是全部阻断。
type Error struct {
	Kind     Kind
	Msg      string
	Blockers []Blocker
	Plan     *Plan
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("resolver: " + string(e.Kind) + ": " + e.Msg)
	if n := len(e.Blockers); n > 1 {
		b.WriteString("（共 " + strconv.Itoa(n) + " 项阻断）")
	}
	return b.String()
}

// Err 在 Plan 可执行时返回 nil，否则返回 *Error。--dry-run 同样返回它，调用方同时展示 Plan。
func (p *Plan) Err() error {
	if len(p.Blockers) == 0 {
		return nil
	}
	var primary *Blocker
	for _, k := range kindPriority {
		for i := range p.Blockers {
			if p.Blockers[i].Kind == k {
				primary = &p.Blockers[i]
				break
			}
		}
		if primary != nil {
			break
		}
	}
	if primary == nil {
		primary = &p.Blockers[0]
	}
	return &Error{Kind: primary.Kind, Msg: primary.Message, Blockers: p.Blockers, Plan: p}
}
