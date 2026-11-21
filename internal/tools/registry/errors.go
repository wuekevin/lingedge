package registry

import (
	"fmt"
	"strings"
)

// Kind 是 registry 失败的稳定分类（cli 据此映射退出码与错误码，见 cli/registryerr.go）。
type Kind string

// Kind 取值。
const (
	KindInvalid            Kind = "invalid"             // 参数不合法（组件名、tx ID、step 编号、字段格式）
	KindStateCorrupt       Kind = "state_corrupt"       // state.json 无法解析/含未知字段/缺 schema_version
	KindStateUnsupported   Kind = "state_unsupported"   // state.json 的 schema_version 高于本程序所知
	KindStateInvalid       Kind = "state_invalid"       // 内容不满足约束（写入前与读取后都校验）
	KindSecret             Kind = "secret"              // 待写入的值疑似含密钥（拒绝写）
	KindPermission         Kind = "permission"          // 目录/文件的属主或权限不安全
	KindIO                 Kind = "io"                  // 其他 I/O 失败
	KindJournalCorrupt     Kind = "journal_corrupt"     // journal 中间行损坏（尾行撕裂不算）
	KindJournalUnsupported Kind = "journal_unsupported" // journal 的 v 高于本程序所知
	KindJournalProtocol    Kind = "journal_protocol"    // 违反 begin/done/commit/abort 的次序约束
	KindJournalClosed      Kind = "journal_closed"      // 事务已 commit/abort/Close，或之前的写入失败使其不可再写
	KindConflict           Kind = "conflict"            // 幂等步骤发现已存在且内容不同：不覆盖
	KindUnsupported        Kind = "unsupported"         // 本平台不支持（无 flock）
	KindFinished           Kind = "finished"            // 恢复的事务其实已结束
	KindNotFound           Kind = "not_found"           // 没有这个事务
)

// Error 是 registry 的结构化错误。
type Error struct {
	Kind    Kind
	Msg     string
	Path    string
	Cause   error
	Details map[string]any
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Msg)
	if e.Path != "" {
		b.WriteString("（" + e.Path + "）")
	}
	if e.Cause != nil {
		b.WriteString(": " + e.Cause.Error())
	}
	return b.String()
}

// Unwrap 暴露底层原因。
func (e *Error) Unwrap() error { return e.Cause }

func newErr(k Kind, format string, a ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...)}
}

func wrapErr(k Kind, cause error, format string, a ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...), Cause: cause}
}

// LockedError 表示锁被别的任务持有（退出码 6）。Holder 来自锁文件的诊断内容，可能为 nil（刚拿到锁还没来得及写，或文件被清空）。
type LockedError struct {
	Name   string // "global" 或组件名
	Path   string
	Holder *Holder
}

func (e *LockedError) Error() string {
	if e.Holder == nil {
		return fmt.Sprintf("%s 锁被占用（持有者信息暂不可得）", e.Name)
	}
	h := e.Holder
	return fmt.Sprintf("%s 锁被占用：pid=%d run_id=%s command=%q host=%s started_at=%s", e.Name, h.PID, h.RunID, h.Command, h.Host, h.StartedAt)
}

// TxInfo 是一个 journal 的摘要（供 13 的提示与 recover 使用）。
type TxInfo struct {
	ID         string   `json:"tx"`
	Path       string   `json:"path"`
	Command    string   `json:"command,omitempty"`
	RunID      string   `json:"run_id,omitempty"`
	Components []string `json:"components,omitempty"`
	Status     string   `json:"status"` // active|committed|aborted|corrupt|empty
	Live       bool     `json:"live"`   // 事务的持有进程仍在运行（journal 文件被 flock 占着）
	Steps      int      `json:"steps"`
	TornTail   bool     `json:"torn_tail,omitempty"`
	Problem    string   `json:"problem,omitempty"`
}

// UnfinishedError 表示存在已死亡且未 commit/abort 的事务（退出码 13，提示 lingedge recover）。
type UnfinishedError struct{ Txs []TxInfo }

func (e *UnfinishedError) Error() string {
	ids := make([]string, 0, len(e.Txs))
	for _, t := range e.Txs {
		ids = append(ids, t.ID)
	}
	return "存在未完成的事务 " + strings.Join(ids, ",") + "，请先运行 lingedge recover"
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error) //nolint:errorlint // 仅取最外层 registry 错误以补全 Path
	if ok {
		*target = e
	}
	return ok
}
