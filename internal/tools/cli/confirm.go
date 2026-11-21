package cli

import (
	"strings"
)

// interactive 报告本次运行能否向用户提问：有 TTY、未请求 --non-interactive/--json、TERM!=dumb、CI!=true（cli-tui §8）。
func (r *Run) interactive() bool { return !r.nonInter }

// autoNonInteractive 返回导致「自动」进入非交互的原因（无则空）。显式 --non-interactive/--json 不算自动。
func autoNonInteractiveReason(env Env) string {
	switch {
	case !env.StdinTTY:
		return "stdin 不是终端"
	case env.Getenv("TERM") == "dumb":
		return "TERM=dumb"
	case env.Getenv("CI") == "true":
		return "CI=true"
	}
	return ""
}

func (r *Run) readLine() (string, error) {
	line, err := r.stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// confirmYes 处理「可确认」的提示（--yes 覆盖）：
//   - 已传 --yes → 通过；
//   - 非交互且未传 → 退出码 9（CONFIRM_REQUIRED），提示应传的显式标志；
//   - 交互 → 向 stderr 提问，读 stdin 一行，y/yes 才通过。
func (r *Run) confirmYes(question string) *Error {
	if r.opts.Yes {
		return nil
	}
	if !r.interactive() {
		return &Error{
			Exit: ExitConfirmRequired, Code: "confirm.required",
			Message: question + "——该操作需要确认，而当前为非交互模式",
			Hint:    "确认执行请显式传入 --yes（不可逆迁移须传 --accept-irreversible，--purge 须传 --yes --confirm-name <主机名>）",
		}
	}
	wf(r.env.Stderr, "%s [y/N]: ", question)
	ans, err := r.readLine()
	if err == nil && (strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes")) {
		return nil
	}
	return &Error{Exit: ExitConfirmRequired, Code: "confirm.declined", Message: "未确认，已取消", Hint: "重新运行并输入 y，或传入 --yes"}
}

// confirmHostname 处理 `uninstall --purge`：--yes 不足以确认，必须给出与本机主机名一致的 --confirm-name；
// 交互时也可手工键入主机名（FR-9「不默认删除用户数据」）。
func (r *Run) confirmHostname(given string) *Error {
	host, err := r.env.Hostname()
	if err != nil || host == "" {
		return &Error{Exit: ExitPrecondition, Code: "precondition.hostname", Message: "无法获取本机主机名，不能确认 --purge", Cause: err}
	}
	if given != "" {
		if given != host {
			return &Error{Exit: ExitConfirmRequired, Code: "confirm.name_mismatch", Message: "--confirm-name 与本机主机名不一致",
				Hint: "--purge 会删除用户数据；请确认主机名后重试（hostname 命令可查看）"}
		}
		if r.opts.Yes || r.interactive() {
			return nil
		}
	}
	if !r.interactive() {
		return &Error{
			Exit: ExitConfirmRequired, Code: "confirm.required",
			Message: "--purge 需要显式确认，而当前为非交互模式",
			Hint:    "传入 --yes --confirm-name <本机主机名>",
		}
	}
	wf(r.env.Stderr, "此操作将永久删除数据。请输入本机主机名 %q 以确认: ", host)
	ans, rerr := r.readLine()
	if rerr == nil && ans == host {
		return nil
	}
	return &Error{Exit: ExitConfirmRequired, Code: "confirm.name_mismatch", Message: "输入的主机名不一致，已取消"}
}
