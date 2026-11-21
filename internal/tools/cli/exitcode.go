package cli

import "fmt"

// ExitCode 是 lingedge 的进程退出码（tools-design §4，公开契约 FR-4）。
// 表项变更须走 ADR；本表与 tools-design §4、ux/cli-tui §4.2 的一致性由 exitcode_test.go 解析文档逐项断言。
type ExitCode int

// 退出码常量。
const (
	ExitOK              ExitCode = 0
	ExitGeneral         ExitCode = 1
	ExitUsage           ExitCode = 2
	ExitPrecondition    ExitCode = 3
	ExitPortConflict    ExitCode = 4
	ExitVerifyFailed    ExitCode = 5
	ExitLocked          ExitCode = 6
	ExitNotReleased     ExitCode = 7
	ExitBuiltin         ExitCode = 8
	ExitConfirmRequired ExitCode = 9
	ExitRolledBack      ExitCode = 10
	ExitHealthFailed    ExitCode = 11
	ExitRollbackFailed  ExitCode = 12
	ExitInterrupted     ExitCode = 13
	ExitNotInstalled    ExitCode = 14
	ExitSigint          ExitCode = 130
)

// ExitInfo 描述一个退出码。
type ExitInfo struct {
	Code    ExitCode `json:"code"`
	Name    string   `json:"name"`
	Meaning string   `json:"meaning"`
	Typical string   `json:"typical"`
}

// exitTable 的顺序即 `lingedge --help exit-codes` 的输出顺序。
var exitTable = []ExitInfo{
	{ExitOK, "OK", "成功（含 --dry-run 计划成功、doctor 无失败项）", "—"},
	{ExitGeneral, "GENERAL", "通用/未分类失败", "未预期错误；详见日志"},
	{ExitUsage, "USAGE", "参数/用法错误", "未知标志、非法 --set、组件名错误"},
	{ExitPrecondition, "PRECONDITION", "前置条件/预检失败", "非 root、内存/磁盘不足、OS/架构不支持、依赖缺失、doctor 存在失败项"},
	{ExitPortConflict, "PORT_CONFLICT", "端口冲突", "80/443/53/9443 被占用"},
	{ExitVerifyFailed, "VERIFY_FAILED", "校验/签名失败", "清单验签失败、SHA256 不符、包内签名失败、sequence 回退、清单过期"},
	{ExitLocked, "LOCKED", "任务锁被占用", "同目标已有安装/升级任务"},
	{ExitNotReleased, "NOT_RELEASED", "组件尚未发布", "install dns（MVP）"},
	{ExitBuiltin, "BUILTIN", "组件已内置于 Server", "install scheduler|worker（D-16）"},
	{ExitConfirmRequired, "CONFIRM_REQUIRED", "需要显式确认但处于非交互", "含不可逆迁移而未传 --accept-irreversible；--purge 未带 --confirm-name"},
	{ExitRolledBack, "ROLLED_BACK", "升级/安装失败，已自动回滚且服务恢复", "健康检查失败后回滚成功"},
	{ExitHealthFailed, "HEALTH_FAILED", "健康检查失败（且未回滚或无需回滚）", "service status/status 发现组件不健康"},
	{ExitRollbackFailed, "ROLLBACK_FAILED", "回滚本身失败，需人工恢复", "journal 保留，输出恢复指引"},
	{ExitInterrupted, "INTERRUPTED", "存在未完成事务，需要 recover", "新命令启动时发现未完成 journal"},
	{ExitNotInstalled, "NOT_INSTALLED", "目标组件未安装", "service start node 而未装"},
	{ExitSigint, "SIGINT", "用户中断（SIGINT），已按 journal 处理", "Ctrl-C"},
}

// ExitTable 返回退出码表的副本。
func ExitTable() []ExitInfo { return append([]ExitInfo(nil), exitTable...) }

// Name 返回退出码的稳定名称；表外的值返回 "UNKNOWN"。
func (c ExitCode) Name() string {
	for _, e := range exitTable {
		if e.Code == c {
			return e.Name
		}
	}
	return "UNKNOWN"
}

// Error 是命令执行中的结构化失败：携带退出码与 envelope 的 errors[] 内容。
// 命令实现返回它（或包装它）即可得到确定的退出码，不必自己写输出。
type Error struct {
	Exit        ExitCode
	Code        string         // 稳定小写点分串，如 "usage.invalid_component"
	Message     string         // 发生了什么
	Why         string         // 为什么（人类输出；可空）
	Hint        string         // 怎么办
	Step        int            // 安装步骤号（可空）
	Details     map[string]any // 结构化细节（脱敏后输出）
	SystemState string         // unchanged|rolled_back|partial|needs_manual；空视为 unchanged
	Cause       error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap 支持 errors.Is/As。
func (e *Error) Unwrap() error { return e.Cause }

func newErr(exit ExitCode, code, msg, hint string) *Error {
	return &Error{Exit: exit, Code: code, Message: msg, Hint: hint}
}

func usageErr(code, msg, hint string) *Error { return newErr(ExitUsage, code, msg, hint) }
