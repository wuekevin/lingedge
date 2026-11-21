package security

import (
	"errors"
	"fmt"
)

// 解压失败的分类（errors.Is 可判定）。调用方（Tools）一律映射为退出码 5（tools-design §18.1 第 3 条）。
var (
	// ErrUnsafePath 条目路径不安全：绝对路径、`..`、NUL/反斜杠/控制字符、百分号编码、过长/过深、驱动器前缀、大小写冲突、重复条目、越出目标根。
	ErrUnsafePath = errors.New("security: 不安全的归档路径")
	// ErrUnsupportedType 条目类型不在白名单（仅 regular file / directory）：符号链接、硬链接、设备、FIFO、套接字、稀疏/全局 PAX 头等。
	ErrUnsupportedType = errors.New("security: 不允许的归档条目类型")
	// ErrLimit 条目数、单文件大小、累计大小或原始流大小超限（含声明与实际两种口径）。
	ErrLimit = errors.New("security: 归档超出限额")
	// ErrChecksum 文件内容的 SHA256 与清单不符。
	ErrChecksum = errors.New("security: SHA256 校验失败")
	// ErrMissing 清单中声明的条目在归档中不存在。
	ErrMissing = errors.New("security: 归档缺少清单声明的条目")
	// ErrExtra 归档中存在清单未声明的条目。
	ErrExtra = errors.New("security: 归档含清单未声明的条目")
	// ErrArchive 归档本身损坏或被截断。
	ErrArchive = errors.New("security: 归档格式损坏")
	// ErrDestination 目标位置不满足前置条件（已存在、.partial 残留、父目录不存在）。
	ErrDestination = errors.New("security: 解压目标不可用")
	// ErrOptions 调用参数不合法（如缺少校验和表）。
	ErrOptions = errors.New("security: 解压参数不合法")
)

// ExtractError 携带失败类别与条目名（条目名已按需转义，不含控制字符）。
type ExtractError struct {
	Kind  error  // 上述哨兵之一
	Entry string // 相关条目名，可为空
	Msg   string
}

func (e *ExtractError) Error() string {
	if e.Entry != "" {
		return fmt.Sprintf("%v: %s: %s", e.Kind, e.Entry, e.Msg)
	}
	return fmt.Sprintf("%v: %s", e.Kind, e.Msg)
}

func (e *ExtractError) Unwrap() error { return e.Kind }

func extractErr(kind error, entry, format string, a ...any) error {
	return &ExtractError{Kind: kind, Entry: printable(entry), Msg: fmt.Sprintf(format, a...)}
}

// printable 把条目名转为可安全写入日志/终端的形式（控制字符与非法 UTF-8 用 %q 转义），并截断。
func printable(s string) string {
	if s == "" {
		return ""
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return fmt.Sprintf("%q", s)
}
