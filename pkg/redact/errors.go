package redact

import (
	"fmt"
	"runtime/debug"
)

// Err 是已清洗的错误：Error() 返回脱敏文本，Unwrap() 保留原错误，因此 errors.Is/As 仍然可用。
type Err struct {
	msg string
	err error
}

// Error 返回脱敏后的消息。
func (e *Err) Error() string { return e.msg }

// Unwrap 返回原始错误（供 errors.Is/As；调用方不得再直接把它格式化进日志）。
func (e *Err) Unwrap() error { return e.err }

// Error 把 err 包装为 Error() 已脱敏的错误；nil 返回 nil；已是 *Err 则原样返回（幂等）。
func Error(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*Err); ok {
		return err
	}
	return &Err{msg: String(errText(err)), err: err}
}

// errText 调用 err.Error()；实现自身 panic（如 nil 指针接收者）时降级为占位文本，保证日志路径不会因错误值崩溃。
func errText(err error) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = "<panic in Error()>"
		}
	}()
	return err.Error()
}

// Chain 返回错误链上每一层（含 errors.Join/多重 %w 的分支）的脱敏文本，自外向内，去重后不含与外层完全相同的层。
// 用于日志里补充记录「被包装的原因」，且每一层都已清洗。
func Chain(err error) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(error, int)
	walk = func(e error, depth int) {
		if e == nil || depth > maxChainDepth {
			return
		}
		if msg := String(errText(e)); !seen[msg] {
			seen[msg] = true
			out = append(out, msg)
		}
		for _, c := range safeUnwrap(e) {
			walk(c, depth+1)
		}
	}
	walk(err, 0)
	return out
}

// maxChainDepth 限制错误链遍历深度，防止自引用/环形 Unwrap 造成无界递归（栈溢出是不可恢复的致命错误）。
const maxChainDepth = 32

// safeUnwrap 取一层被包装的错误（含 errors.Join/多重 %w 的分支）；实现自身 panic 时视为没有下层。
func safeUnwrap(e error) (out []error) {
	defer func() {
		if r := recover(); r != nil {
			out = nil
		}
	}()
	switch u := e.(type) {
	case interface{ Unwrap() []error }:
		return u.Unwrap()
	case interface{ Unwrap() error }:
		if c := u.Unwrap(); c != nil {
			return []error{c}
		}
	}
	return nil
}

// Panic 把 recover() 得到的值格式化为脱敏文本：error 取其 Error()，其余按 %v；值本身可能含凭据，因此一律清洗。
func Panic(recovered any) string {
	switch v := recovered.(type) {
	case nil:
		return "<nil>"
	case error:
		return String(errText(v))
	case string:
		return String(v)
	case fmt.Stringer:
		return safeString(v)
	default:
		return String(fmt.Sprintf("%v", v))
	}
}

func safeString(s fmt.Stringer) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = "<panic in String()>"
		}
	}()
	return String(s.String())
}

// Stack 返回当前 goroutine 的脱敏调用栈（用于 recover 中间件；栈里可能含函数参数形态的字符串）。
func Stack() string { return String(string(debug.Stack())) }
