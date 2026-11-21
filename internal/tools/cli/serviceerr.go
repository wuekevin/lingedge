package cli

import (
	"errors"

	"github.com/wuekevin/lingedge/internal/tools/service"
)

// ServiceError 把 internal/tools/service 的失败映射为 CLI 结构化错误（Story 1.32）。命令实现（1.33）直接返回 service 的错误即可。
//
//	not_systemd   → 3  precondition.systemd          本机不是 systemd 系统（Tools 仅支持 Linux + systemd）
//	unsupported   → 3  precondition.systemd_version  systemd 低于最低支持版本
//	permission    → 3  precondition.root             需要 root / 无法设置属主或权限
//	conflict      → 3  service.conflict              与现状冲突（既有用户 UID 为 0、目标是符号链接、文件已存在等），拒绝覆盖
//	io            → 3  service.io                    目录/文件操作失败
//	*MismatchError→ 3  service.dir_mismatch          既有目录权限/属主与规格不符
//	exec          → 1  service.exec_failed           systemctl/useradd/… 失败（命令与参数已脱敏）
//	undo          → 1  service.undo_refused          撤销信息不合法或被拒绝（journal 被篡改或程序缺陷）
//	invalid_param → 1  service.invalid_param         内部生成的单元参数未通过校验（程序缺陷，不接受用户自由文本）
//
// 已是 *Error 的原样返回；不认识的返回 nil。
func ServiceError(err error) *Error {
	var ce *Error
	if errors.As(err, &ce) {
		return ce
	}
	var me *service.MismatchError
	if errors.As(err, &me) {
		items := make([]any, 0, len(me.Items))
		for _, s := range me.Items {
			items = append(items, s)
		}
		return &Error{Exit: ExitPrecondition, Code: "service.dir_mismatch", SystemState: "unchanged", Cause: err,
			Message: "既有目录的权限或属主与规格不符",
			Hint:    "运行 lingedge doctor 查看逐项差异；确认无误后可修正权限（doctor --fix-perms），Tools 不会静默放行过宽的权限",
			Details: map[string]any{"items": items}}
	}
	var se *service.Error
	if !errors.As(err, &se) {
		return nil
	}
	e := &Error{Exit: ExitGeneral, Cause: err, Message: se.Msg, Why: se.Error(), SystemState: "unchanged"}
	if se.Path != "" {
		e.Details = map[string]any{"path": se.Path}
	}
	switch se.Kind {
	case service.KindNotSystemd:
		e.Exit, e.Code = ExitPrecondition, "precondition.systemd"
		e.Hint = "Tools 仅支持由 systemd 启动的 Linux；容器/WSL1/非 Linux 环境请改用受支持的主机"
	case service.KindUnsupported:
		e.Exit, e.Code = ExitPrecondition, "precondition.systemd_version"
		e.Hint = "升级到 systemd >= 235 的发行版（Debian 10+/Ubuntu 18.04+/RHEL 8+）；低于 247 时 Server 主密钥自动降级（ADR-027）"
	case service.KindPermission:
		e.Exit, e.Code = ExitPrecondition, "precondition.root"
		e.Hint = "创建运行用户、写 systemd 单元与设置属主需要 root：请以 root 运行（sudo lingedge …）"
	case service.KindConflict:
		e.Exit, e.Code = ExitPrecondition, "service.conflict"
		e.Hint = "目标已存在且与期望不一致，已拒绝覆盖或跟随；请人工核对后再试"
	case service.KindIO:
		e.Exit, e.Code = ExitPrecondition, "service.io"
		e.Hint = "检查目标目录是否存在、权限与磁盘空间"
	case service.KindExec:
		e.Code = "service.exec_failed"
		e.Hint = "外部命令执行失败；详情见 why，可运行 lingedge doctor"
	case service.KindUndo:
		e.Code = "service.undo_refused"
		e.Hint = "撤销信息被拒绝（路径/名称不在 Tools 管理范围内）；不要手工编辑 journal，运行 lingedge recover 查看"
	case service.KindInvalidParam:
		e.Code = "service.invalid_param"
		e.Hint = "内部生成的单元参数未通过安全校验；这是程序缺陷或输入异常，请反馈"
	default:
		e.Code = "service.unknown"
	}
	return e
}
