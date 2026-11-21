package service

import (
	"errors"
	"fmt"
)

// Kind 是失败类别（errors.Is/As 可判定，cli.ServiceError 据此映射退出码）。
type Kind string

// Kind 取值。
const (
	KindInvalidParam Kind = "invalid_param" // 渲染变量/参数不合法（程序缺陷或内部生成值异常）
	KindNotSystemd   Kind = "not_systemd"   // 本机不是 systemd 启动的系统
	KindUnsupported  Kind = "unsupported"   // systemd 版本过低
	KindExec         Kind = "exec"          // 外部命令失败
	KindIO           Kind = "io"            // 文件/目录操作失败
	KindConflict     Kind = "conflict"      // 与现状冲突（如既有用户 UID 为 0、目标是符号链接）
	KindPermission   Kind = "permission"    // 权限/属主不符合要求，或需要 root
	KindUndo         Kind = "undo"          // 撤销信息不合法/被拒绝
)

// Error 是本包的结构化错误。
type Error struct {
	Kind  Kind
	Field string // KindInvalidParam：出错的字段
	Path  string // 相关路径（已是调用方给定的路径，不含秘密）
	Msg   string
	Err   error
}

func (e *Error) Error() string {
	s := "service: " + e.Msg
	if e.Field != "" {
		s = "service: 参数 " + e.Field + " 不合法：" + e.Msg
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap 暴露底层错误。
func (e *Error) Unwrap() error { return e.Err }

func newErr(k Kind, format string, a ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...)}
}

func wrapErr(k Kind, err error, format string, a ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...), Err: err}
}

func paramErr(field string, err error) *Error {
	return &Error{Kind: KindInvalidParam, Field: field, Msg: err.Error()}
}

// KindOf 返回错误链中第一个 *Error 的类别；没有则为空。
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

// Kinds 返回全部失败类别（cli.ServiceError 的映射必须覆盖每一种，测试据此保证）。
func Kinds() []Kind {
	return []Kind{KindInvalidParam, KindNotSystemd, KindUnsupported, KindExec, KindIO, KindConflict, KindPermission, KindUndo}
}
