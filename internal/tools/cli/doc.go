// Package cli 是 LingEdge Tools（lingedge）的 Cobra 命令树、输出层与退出码映射（Story 1.25，tools-design §3–§6、ux/cli-tui §4/§7）。
//
// 本包只负责「命令外壳」：全局标志、退出码契约（CF-5）、`lingedge.cli/v1` JSON envelope、非交互/确认约定、
// 日志脱敏与 root 检查。业务能力（安装/升级/回滚/备份/诊断…）由后续 Story 在各自的包里实现并挂接到本命令树；
// 尚未实现的命令保持 Hidden，被调用时返回明确的「尚未实现」错误（退出码 1），绝不返回假成功（AC1）。
//
// 依赖：只依赖 pkg/redact（脱敏引擎，Server/Tools/Node 共用）、pkg/version 与标准库/Cobra，
// 与 Server 业务模块无编译依赖（architecture §5.3 规则 5）。
package cli
