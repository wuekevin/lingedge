package installer

import (
	"errors"
	"fmt"
	"strings"
)

// Kind 是 installer 失败的稳定分类（cli.InstallError 据此映射退出码与错误码）。
type Kind string

// Kind 取值。
const (
	// KindSpecMissing：计划里有组件没有登记 ComponentSpec（生产构建当前只登记 server，其余由各自 Story 交付）。退出码 1。
	KindSpecMissing Kind = "spec_missing"
	// KindSettings：--set 的键/值不合法（组件的 schema 拒绝）。退出码 2。
	KindSettings Kind = "settings"
	// KindUpgradeRequired：计划含已装组件的升级（被动升级或版本变化），安装流程不执行升级，须走 lingedge update（Story 1.43）。退出码 3。
	KindUpgradeRequired Kind = "upgrade_required"
	// KindConfirmRequired：存在需要显式确认的冲突（既有配置/单元/站点内容不同、目录权限不符），非交互且未 --yes。退出码 9。
	KindConfirmRequired Kind = "confirm_required"
	// KindPortUnverified：ss 与 /proc 均不可用，端口占用无法检测，且未获确认。退出码 4。
	KindPortUnverified Kind = "port_unverified"
	// KindPrecondition：其他前置条件（systemd 缺失/过旧、release.base_url 缺失、目标 release 目录已存在且内容不同…）。退出码 3。
	KindPrecondition Kind = "precondition"
	// KindConfig：tools.yaml 不合法。退出码 2。
	KindConfig Kind = "config"
	// KindStepFailed：某一步失败（已按 journal 回滚，或事务尚未产生副作用）。Cause 是原始错误（manifest/downloader/registry/service/…），
	// cli 按 Cause 的类型映射退出码（校验失败 5 等）；没有结构化原因时退出码 1。
	KindStepFailed Kind = "step_failed"
	// KindRolledBack：激活后（19–20）失败，已逆序撤销并恢复到预检后状态。退出码 10。
	KindRolledBack Kind = "rolled_back"
	// KindHealthFailed：健康检查失败且 --no-rollback（保留现场）。退出码 11。
	KindHealthFailed Kind = "health_failed"
	// KindRollbackFailed：撤销本身失败（或 journal 无法收尾），journal 保留。退出码 12。
	KindRollbackFailed Kind = "rollback_failed"
	// KindInterrupted：收到 SIGINT，当前原子步骤完成后已回滚。退出码 130。
	KindInterrupted Kind = "interrupted"
	// KindRecover：recover 的前置不满足（没有未完成事务、不可续跑、事务已提交…）。退出码 3（无事务时 recover 返回成功结果而非错误）。
	KindRecover Kind = "recover"
	// KindInternal：不应发生的内部错误（程序缺陷）。退出码 1。
	KindInternal Kind = "internal"
)

// Conflict 是一个需要显式确认的冲突。
type Conflict struct {
	Kind      string `json:"kind"` // file | unit | dir | release | port_unverified
	Component string `json:"component,omitempty"`
	Path      string `json:"path,omitempty"`
	Message   string `json:"message"`
	Hint      string `json:"hint,omitempty"`
}

// Error 是 installer 的结构化错误。
type Error struct {
	Kind Kind
	Step int // 失败发生的步骤（0 = 不属于某一步）
	Msg  string
	Hint string
	// Cause 是底层错误（可被 errors.As 取到 *manifest.Error/*downloader.Error/*registry.Error/*service.Error/*resolver.Error）。
	Cause error
	// SystemState：unchanged | rolled_back | partial | needs_manual。
	SystemState string
	Conflicts   []Conflict
	Details     map[string]any
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("installer: " + string(e.Kind))
	if e.Step > 0 {
		fmt.Fprintf(&b, "（第 %d 步 %s）", e.Step, StepName(e.Step))
	}
	b.WriteString(": " + e.Msg)
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

func wrap(k Kind, cause error, format string, a ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...), Cause: cause}
}

// KindOf 返回错误链中最外层 *Error 的分类；不是 installer 错误返回空串。
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

// Kinds 返回全部分类（cli 映射表的穷尽性测试用）。
func Kinds() []Kind {
	return []Kind{KindSpecMissing, KindSettings, KindUpgradeRequired, KindConfirmRequired, KindPortUnverified, KindPrecondition, KindConfig,
		KindStepFailed, KindRolledBack, KindHealthFailed, KindRollbackFailed, KindInterrupted, KindRecover, KindInternal}
}
