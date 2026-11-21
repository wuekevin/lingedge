package cli

import (
	"errors"
	"strconv"

	"github.com/wuekevin/lingedge/internal/tools/registry"
)

// RegistryError 把 internal/tools/registry 的失败映射为 CLI 结构化错误（Story 1.29）。
//
//	*registry.LockedError     → 退出码 6（LOCKED）：同目标已有任务，输出持有者 pid/run_id/command
//	*registry.UnfinishedError → 退出码 13（INTERRUPTED）：存在未完成事务，提示 lingedge recover
//	state.json 的 schema 版本高于本程序 → 3（升级 lingedge；本程序不改写它）
//	state.json 损坏/内容不合法/journal 损坏 → 1（详情见 code；不要手工编辑）
//	目录/文件权限或属主不安全、I/O 失败 → 3（precondition，与 1.25 的 state.unreadable 一致）
//
// 已是 *Error 的原样返回；不认识的返回 nil。
func RegistryError(err error) *Error {
	var ce *Error
	if errors.As(err, &ce) {
		return ce
	}
	var le *registry.LockedError
	if errors.As(err, &le) {
		e := &Error{Exit: ExitLocked, Code: "locked.task", SystemState: "unchanged", Cause: err,
			Message: "同目标已有任务在运行（" + le.Name + " 被占用）",
			Hint:    "等该任务结束后重试；锁由内核管理，持有进程退出（含 kill -9）即自动释放，请勿手动删除锁文件",
			Details: map[string]any{"lock": le.Name}}
		if le.Name == "global" {
			e.Code = "locked.global"
		}
		if h := le.Holder; h != nil {
			e.Details["holder"] = map[string]any{"pid": h.PID, "run_id": h.RunID, "command": h.Command, "started_at": h.StartedAt, "host": h.Host}
			e.Why = "持有者：pid " + strconv.Itoa(h.PID) + "，run_id " + h.RunID + "，命令 " + h.Command
		}
		return e
	}
	var ue *registry.UnfinishedError
	if errors.As(err, &ue) {
		txs := make([]any, 0, len(ue.Txs)) // 须是 []any/map[string]any：envelope 的脱敏只遍历这两种动态类型
		for _, t := range ue.Txs {
			txs = append(txs, map[string]any{"tx": t.ID, "status": t.Status, "command": t.Command, "components": anyStrings(t.Components), "steps": t.Steps, "problem": t.Problem})
		}
		return &Error{Exit: ExitInterrupted, Code: "interrupted.unfinished_tx", SystemState: "needs_manual", Cause: err,
			Message: "存在未完成的事务（上一次安装/升级被中断）",
			Hint:    "先运行 lingedge recover（默认回滚；可选 --resume）处理被中断的事务，再重试本命令",
			Details: map[string]any{"transactions": txs}}
	}
	var re *registry.Error
	if !errors.As(err, &re) {
		return nil
	}
	e := &Error{Exit: ExitGeneral, Cause: err, Message: re.Msg, SystemState: "unchanged"}
	if re.Path != "" {
		e.Details = map[string]any{"path": re.Path}
	}
	switch re.Kind {
	case registry.KindStateUnsupported:
		e.Exit, e.Code = ExitPrecondition, "state.unsupported_schema"
		e.Hint = "state.json 由更新版本的 lingedge 写入，请升级 lingedge；本程序不会改写它"
	case registry.KindStateCorrupt:
		e.Code = "state.corrupt"
		e.Hint = "state.json 已损坏或被手工改动：不要继续写入；从备份恢复，或运行 lingedge doctor 查看详情"
	case registry.KindStateInvalid:
		e.Code = "state.invalid"
		e.Hint = "state.json 的内容不满足约束（组件名/版本/路径/端口）；不要手工编辑，运行 lingedge doctor"
	case registry.KindSecret:
		e.Code = "state.secret_refused"
		e.Hint = "状态与日志只记录路径、版本与哈希，不记录密钥；这是程序缺陷或输入异常，请反馈"
	case registry.KindPermission:
		e.Exit, e.Code = ExitPrecondition, "state.unsafe_permissions"
		e.Hint = "状态目录/文件必须属于 root（或当前用户）且不对 group/other 可写；运行 lingedge doctor --fix-perms"
	case registry.KindIO:
		e.Exit, e.Code = ExitPrecondition, "state.unreadable"
		e.Message = "无法读取或写入 Tools 状态：" + re.Msg
		e.Hint = "检查状态目录（/var/lib/lingedge/tools）及其父目录的权限与磁盘空间（state.json 应为 0644，普通用户可读）"
	case registry.KindUnsupported:
		e.Exit, e.Code = ExitPrecondition, "precondition.platform"
		e.Hint = "任务锁需要 flock，本平台不支持；Tools 仅支持 Linux"
	case registry.KindConflict:
		e.Code = "registry.conflict"
		e.Hint = "目标已存在且与期望不一致，已拒绝覆盖；请人工核对后再试"
	case registry.KindJournalCorrupt, registry.KindJournalUnsupported, registry.KindJournalProtocol, registry.KindJournalClosed, registry.KindFinished, registry.KindNotFound:
		e.Code = "journal." + string(re.Kind)
		e.Hint = "事务日志异常，运行 lingedge recover 查看；不要手工编辑 journal/"
	case registry.KindInvalid:
		e.Exit, e.Code = ExitUsage, "usage.invalid_value"
	default:
		e.Code = "registry.unknown"
	}
	return e
}

func anyStrings(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
