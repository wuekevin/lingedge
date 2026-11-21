package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// globalOpts 是全局标志（tools-design §3）。
type globalOpts struct {
	JSON           bool
	Yes            bool
	NonInteractive bool
	DryRun         bool
	LogLevel       string
	LogFile        string
	NoColor        bool
	Config         string
	Progress       string
}

// Result 是命令成功时的产出：data 进 envelope，Human 渲染人类输出（stdout）。
type Result struct {
	Data     any
	Warnings []Warning
	Human    func(w io.Writer)
}

// Run 是一次 lingedge 调用的上下文。
type Run struct {
	env     Env
	ctx     context.Context
	args    []string
	started time.Time
	runID   string

	opts     globalOpts
	jsonPre  bool // 标志解析失败后按 pflag 语义重扫得到的 --json（见 jsonAfterParseFailure）
	nonInter bool
	log      *fileLogger
	stdin    *bufio.Reader
	result   *Result
	helpDone bool
	warnings []Warning
	cmdPath  string
}

func (r *Run) jsonMode() bool { return r.opts.JSON || r.jsonPre }

// lenientValue 接收任意取值：重扫只关心 --json，其他标志的取值是否合法留给 Cobra 报告。
type lenientValue struct{ typ string }

func (v *lenientValue) String() string   { return "" }
func (v *lenientValue) Set(string) error { return nil }
func (v *lenientValue) Type() string     { return v.typ }

// jsonAfterParseFailure 在 Cobra 标志解析失败后判定 --json 是否生效。pflag 一出错就停止，
// 出错点之后的 --json 不会被记到 opts.JSON，但调用方仍期望 envelope，所以用与真实解析同一套
// pflag 语义重扫一遍：同一个命令的完整标志集（含继承的全局标志）、同样的 `--` 终止、同样的
// 取值消耗（--config --json 中的 --json 是取值而非标志）、bool 经 strconv.ParseBool
// （--json=TRUE/t/1 为真，--json=maybe 为非法而不是真）、后者覆盖前者。除 json 外其他标志用
// 宽松取值，使其取值非法不会中断重扫；未知标志按 pflag 自己的规则跳过。
func jsonAfterParseFailure(cmd *cobra.Command, args []string) bool {
	if cmd == nil {
		return false
	}
	fs := pflag.NewFlagSet("json-rescan", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.ParseErrorsAllowlist.UnknownFlags = true
	var asJSON bool
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "json" {
			fs.BoolVar(&asJSON, "json", false, "")
			return
		}
		nf := &pflag.Flag{Name: f.Name, Shorthand: f.Shorthand, NoOptDefVal: f.NoOptDefVal, Value: &lenientValue{typ: f.Value.Type()}}
		fs.AddFlag(nf)
	})
	_ = fs.Parse(args) // 出错（如 --json=maybe）即停止，已解析到的结果保留，与 pflag 一致
	return asJSON
}

// isParseStageError 判定错误是否来自 Cobra 的标志/命令解析阶段（解析失败的 FlagErrorFunc 包装，
// 或 Cobra 自己的未知命令/参数错误），而不是命令执行期的结构化错误。
func isParseStageError(err error) bool {
	var ce *Error
	var me *manifest.Error
	switch {
	case errors.As(err, &ce):
		return ce.Code == "usage.invalid_flag"
	case errors.As(err, &me), errors.Is(err, context.Canceled):
		return false
	}
	return true
}

// Execute 运行 lingedge 并返回进程退出码。stdout/stderr 的分工见 tools-design §5：
// --json 时 stdout 仅一个 envelope，其余（进度、提示）走 stderr。
func Execute(ctx context.Context, env Env, args []string) int {
	env = env.withDefaults()
	now := env.Now()
	r := &Run{
		env: env, ctx: ctx, args: append([]string(nil), args...),
		started: now, runID: env.NewRunID(now),
		stdin: bufio.NewReader(env.Stdin),
	}
	root := buildRoot(r)
	root.SetArgs(rewriteHelpTopic(args))
	root.SetContext(ctx)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	cmd, err := root.ExecuteC()
	return r.finish(cmd, err)
}

// finish 把执行结果映射为退出码并写出 envelope/人类输出。
func (r *Run) finish(cmd *cobra.Command, err error) int {
	defer r.log.close()
	exit := ExitOK
	var ce *Error
	var me *manifest.Error
	if err == nil && r.ctx.Err() != nil && r.result == nil && !r.helpDone {
		err = context.Canceled
	}
	if err != nil && !r.opts.JSON && isParseStageError(err) {
		// 解析在出错点停止：再用 pflag 语义重扫，保证出错点之后的 --json 同样得到 envelope。
		r.jsonPre = jsonAfterParseFailure(cmd, rewriteHelpTopic(r.args))
	}
	if err != nil {
		switch {
		case errors.As(err, &ce):
			exit = ce.Exit
		case errors.As(err, &me):
			// 发行信任域①的验证失败（验签/sequence/吊销/过期/轮换/包内签名）：退出码 5 的真实生产者。
			ce = VerifyError(me)
			exit = ce.Exit
		case errors.Is(err, context.Canceled):
			ce = newErr(ExitSigint, "interrupt.sigint", "已被用户中断（SIGINT）", "再次运行以继续；若有未完成事务，先运行 lingedge recover")
			exit = ExitSigint
		default:
			// 不是命令自己返回的结构化错误：来自 Cobra 的解析阶段（未知命令/标志/参数个数）→ 用法错误。
			ce = usageErr("usage.invalid", cobraMessage(err), "运行 lingedge --help 或 lingedge <命令> --help 查看用法")
			exit = ExitUsage
		}
	}

	name := r.commandName(cmd)
	dur := r.env.Now().Sub(r.started)
	if dur < 0 {
		dur = 0
	}
	r.log.log(levelFor(exit), 0, "command.finish", map[string]any{"command": name, "exit_code": int(exit), "exit_name": exit.Name(), "duration_ms": dur.Milliseconds()})

	if r.jsonMode() {
		env := &Envelope{
			Schema: SchemaID, OK: exit == ExitOK, Command: name, RunID: r.runID,
			ExitCode: int(exit), ExitName: exit.Name(),
			StartedAt: formatTime(r.started), DurationMS: dur.Milliseconds(), DryRun: r.opts.DryRun,
			Warnings: append([]Warning(nil), r.warnings...),
		}
		if r.result != nil {
			env.Data = r.result.Data
			env.Warnings = append(env.Warnings, r.result.Warnings...)
		}
		if ce != nil {
			env.Errors = []ErrorItem{errorItem(ce)}
		}
		if werr := env.write(r.env.Stdout); werr != nil && exit == ExitOK {
			return int(ExitGeneral)
		}
		return int(exit)
	}

	if ce != nil {
		r.renderError(ce, exit, name)
		return int(exit)
	}
	if r.result != nil {
		for _, w := range r.result.Warnings {
			wf(r.env.Stderr, "%s %s（%s）\n", r.symbol("⚠", "33"), redactText(w.Message), w.Code)
		}
		if r.result.Human != nil {
			r.result.Human(r.env.Stdout)
		}
	}
	return int(exit)
}

func levelFor(c ExitCode) string {
	if c == ExitOK {
		return "info"
	}
	return "error"
}

func errorItem(ce *Error) ErrorItem {
	state := ce.SystemState
	if state == "" {
		state = "unchanged"
	}
	return ErrorItem{Code: ce.Code, Message: ce.Message, Step: ce.Step, Hint: ce.Hint, SystemState: state, Details: ce.Details}
}

func cobraMessage(err error) string {
	return strings.TrimSpace(err.Error())
}

func (r *Run) commandName(cmd *cobra.Command) string {
	if r.cmdPath != "" {
		return r.cmdPath
	}
	if cmd == nil {
		return "lingedge"
	}
	p := strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), "lingedge"))
	if p == "" {
		return "lingedge"
	}
	return p
}

// warn 记录一条将进入 envelope warnings[] 的警告（人类模式下写 stderr）。
func (r *Run) warn(code, msg string) {
	r.warnings = append(r.warnings, Warning{Code: code, Message: msg})
	if !r.jsonMode() {
		wf(r.env.Stderr, "%s %s（%s）\n", r.symbol("⚠", "33"), redactText(msg), code)
	}
}

// notice 输出一次性提示到 stderr：--json 下为 NDJSON 事件（可用 --progress=none 关闭），否则为一行文本。
func (r *Run) notice(code, msg string) {
	r.log.log("info", 0, code, map[string]any{"message": msg})
	if r.jsonMode() {
		if r.opts.Progress == "none" {
			return
		}
		b, _ := json.Marshal(map[string]any{"type": "notice", "code": code, "message": redactText(msg), "ts": formatTime(r.env.Now())})
		wln(r.env.Stderr, string(b))
		return
	}
	wf(r.env.Stderr, "提示：%s\n", redactText(msg))
}

// color 判定：--no-color、NO_COLOR、TERM=dumb、--json、stderr 非 TTY 时一律不着色（cli-tui §4.1）。
func (r *Run) colorOn() bool {
	if r.opts.NoColor || r.jsonMode() || !r.env.StderrTTY || r.env.Getenv("NO_COLOR") != "" || r.env.Getenv("TERM") == "dumb" {
		return false
	}
	return true
}

func (r *Run) symbol(sym, ansi string) string {
	if r.colorOn() {
		return "\x1b[" + ansi + "m" + sym + "\x1b[0m"
	}
	return sym
}

// renderError 以 cli-tui §4.1 的固定版式输出人类可读错误（stderr）：发生了什么/为什么/怎么办/系统状态/退出码与日志。
func (r *Run) renderError(ce *Error, exit ExitCode, name string) {
	w := r.env.Stderr
	wf(w, " %s %s 失败\n", r.symbol("✗", "31"), name)
	row := func(label, text string) {
		if text != "" {
			// 标签均为全角汉字，按 2 列宽补齐到 12 列，使版式与 cli-tui §4.1 对齐。
			wf(w, "   %s%s%s\n", label, strings.Repeat(" ", max(2, 12-2*len([]rune(label)))), redactText(text))
		}
	}
	row("发生了什么", ce.Message)
	row("为什么", ce.Why)
	if ce.Cause != nil {
		row("原因", ce.Cause.Error())
	}
	row("怎么办", ce.Hint)
	switch ce.SystemState {
	case "", "unchanged":
		row("系统状态", "未做任何修改")
	case "rolled_back":
		row("系统状态", "已回滚到操作前的状态")
	case "partial":
		row("系统状态", "留有部分修改")
	case "needs_manual":
		row("系统状态", "需要人工恢复")
	}
	wf(w, "   退出码 %d   错误码 %s（%s）\n", int(exit), ce.Code, exit.Name())
	if r.log != nil {
		wf(w, "   日志  %s   运行 ID: %s\n", r.log.f.Name(), r.runID)
	} else {
		wf(w, "   运行 ID: %s\n", r.runID)
	}
}

func redactText(s string) string { return redactString(s) }

// rewriteHelpTopic 把 `--help exit-codes`（tools-design §3 / AC2）改写为 `help exit-codes`：
// Cobra 会把 --help 之后的 exit-codes 当作未知子命令而在帮助生效前报错。
func rewriteHelpTopic(args []string) []string {
	help, topic := -1, -1
	for i, a := range args {
		if a == "--" {
			break
		}
		if (a == "--help" || a == "-h") && help < 0 {
			help = i
		}
		if a == "exit-codes" && topic < 0 {
			topic = i
		}
	}
	if help < 0 || topic < 0 {
		return args
	}
	out := []string{"help", "exit-codes"}
	for i, a := range args {
		if i != help && i != topic {
			out = append(out, a)
		}
	}
	return out
}
