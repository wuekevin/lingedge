package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wuekevin/lingedge/pkg/version"
)

const rootLong = `lingedge —— LingEdge Tools：平台组件的安装、升级、回滚、备份、诊断与服务管理入口。

退出码是公开契约：lingedge --help exit-codes
--json 时 stdout 仅输出一个 lingedge.cli/v1 JSON 文档，进度与提示走 stderr（NDJSON）。
无 TTY、TERM=dumb 或 CI=true 时自动等价于 --non-interactive。`

// buildRoot 构造完整命令树并绑定全局标志。
func buildRoot(r *Run) *cobra.Command {
	root := &cobra.Command{
		Use:           "lingedge",
		Short:         "LingEdge Tools：安装、升级、回滚、备份与诊断",
		Long:          rootLong,
		SilenceUsage:  true,
		SilenceErrors: true,
		// 全局：解析完标志后统一做校验、非交互判定、日志初始化（所有子命令共用；子命令不得自定义 PersistentPreRun）。
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error { return r.prepare(cmd, args) },
	}
	root.CompletionOptions.DisableDefaultCmd = true

	f := root.PersistentFlags()
	f.BoolVar(&r.opts.JSON, "json", false, "stdout 仅输出一个 lingedge.cli/v1 JSON 文档（进度走 stderr NDJSON）")
	f.BoolVarP(&r.opts.Yes, "yes", "y", false, "对所有可确认的提示回答是（不覆盖不可逆迁移与 --purge）")
	f.BoolVar(&r.opts.NonInteractive, "non-interactive", false, "禁止任何提示与 TUI；需要确认而未给显式标志则以退出码 9 失败")
	f.BoolVar(&r.opts.DryRun, "dry-run", false, "只做预检与计划，不写任何持久状态")
	f.StringVar(&r.opts.LogLevel, "log-level", "info", "日志级别："+strings.Join(logLevels, "|"))
	f.StringVar(&r.opts.LogFile, "log-file", "", "日志文件（JSON Lines，0640；缺省 "+DefaultLogPath+"）")
	f.BoolVar(&r.opts.NoColor, "no-color", false, "关闭彩色输出（也可设置 NO_COLOR）")
	f.StringVar(&r.opts.Config, "config", "", "Tools 配置文件（缺省 "+DefaultConfigPath+"）")
	f.StringVar(&r.opts.Progress, "progress", "", "进度输出：json|none|text（缺省：--json 时为 json，否则 text）")

	// 解析失败（未知标志、缺参数值）统一落为用法错误（退出码 2）。
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageErr("usage.invalid_flag", err.Error(), "运行 lingedge --help 或 lingedge <命令> --help 查看可用标志")
	})
	root.SetHelpFunc(func(c *cobra.Command, args []string) { r.help(c, args) })

	var showVersion bool
	root.Flags().BoolVar(&showVersion, "version", false, "显示 Tools 版本并退出")
	root.RunE = func(cmd *cobra.Command, args []string) error {
		if showVersion {
			r.result = versionToolsResult()
			return nil
		}
		// 无参数：交互菜单属 Story 1.44（TUI）；此前打印帮助（非 TTY 本来就是打印帮助，退出码 0）。
		if r.interactive() {
			r.warn("tui.not_delivered", "交互菜单尚未交付（Story 1.44），已显示帮助；请直接使用子命令")
		}
		r.help(cmd, nil)
		return nil
	}

	// help 子命令：help [命令|exit-codes]。
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [命令|exit-codes]",
		Short: "显示帮助；help exit-codes 输出退出码表",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 1 && args[0] == "exit-codes" {
				r.help(root, args)
				return nil
			}
			target, _, err := root.Find(args)
			if err != nil || target == nil {
				return usageErr("usage.unknown_command", fmt.Sprintf("未知命令 %q", strings.Join(args, " ")), "运行 lingedge --help 查看可用命令")
			}
			r.help(target, nil)
			return nil
		},
	})

	for _, s := range commandSpecs() {
		root.AddCommand(s.build(r))
	}
	localizeHelpFlags(root)
	return root
}

// prepare 在每个（非帮助）命令执行前统一运行：校验全局标志、判定非交互、打开日志、记录命令开始。
func (r *Run) prepare(cmd *cobra.Command, args []string) error {
	if p := strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), "lingedge")); p != "" {
		r.cmdPath = p
		// 带组件参数的命令把（合法的）组件名并入 command，如 "install node"（tools-design §6）；非法输入不回显进 command。
		if strings.Contains(cmd.Use, "<组件>") || strings.Contains(cmd.Use, "[组件]") {
			if len(args) > 0 && (inList(components, args[0]) || inList(builtinComponents, args[0])) {
				r.cmdPath += " " + args[0]
			}
		}
	} else {
		r.cmdPath = "lingedge"
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if levelRank(r.opts.LogLevel) < 0 {
		return checkEnum("log-level", r.opts.LogLevel, logLevels...)
	}
	if e := checkEnum("progress", r.opts.Progress, "", "json", "none", "text"); e != nil {
		return e
	}
	if r.opts.Config != "" {
		if st, err := os.Stat(r.opts.Config); err != nil || !st.Mode().IsRegular() {
			// tools.yaml 的内容校验属 Story 1.42（config validate）；此处至少保证显式指定的路径真实存在，不静默忽略。
			return usageErr("usage.config_not_found", fmt.Sprintf("--config 指定的文件 %q 不存在或不是普通文件", r.opts.Config), "检查路径；缺省 "+DefaultConfigPath)
		}
	}

	reason := autoNonInteractiveReason(r.env)
	explicit := r.opts.NonInteractive || r.opts.JSON
	r.nonInter = explicit || reason != ""

	logPath, explicitLog := r.opts.LogFile, r.opts.LogFile != ""
	if !explicitLog {
		logPath = r.env.DefaultLogFile
	}
	lg, lerr := openLogger(logPath, explicitLog, r.opts.LogLevel, r.runID, r.env.Now)
	if lerr != nil {
		return lerr
	}
	r.log = lg

	r.log.log("info", 0, "command.start", map[string]any{
		"command": r.cmdPath, "argv": redactArgs(r.args), "non_interactive": r.nonInter,
		"dry_run": r.opts.DryRun, "euid": r.env.Geteuid(),
	})
	if !explicit && reason != "" {
		r.notice("cli.non_interactive_auto", "检测到无交互终端（"+reason+"），已自动启用 --non-interactive；需要确认的操作须显式传入 --yes 等标志")
	}
	return nil
}

// help 输出帮助（Cobra 的 --help、help 子命令与无参数共用）。--json 时帮助文本进入 envelope.data.help。
func (r *Run) help(c *cobra.Command, args []string) {
	r.helpDone = true
	if len(args) > 0 && args[0] == "exit-codes" {
		r.result = exitCodesResult()
		if !r.jsonMode() {
			r.result.Human(r.env.Stdout)
			r.result = nil
		}
		return
	}
	text := helpText(c)
	if r.jsonMode() {
		r.result = &Result{Data: map[string]any{"help": text}}
		return
	}
	_, _ = io.WriteString(r.env.Stdout, text)
}

func helpText(c *cobra.Command) string {
	var b strings.Builder
	if c.Long != "" {
		b.WriteString(strings.TrimSpace(c.Long))
	} else {
		b.WriteString(c.Short)
	}
	b.WriteString("\n\n")
	b.WriteString(c.UsageString())
	return b.String()
}

// exitCodesResult 是 `lingedge --help exit-codes` 的产出（人类表格或 envelope.data.exit_codes）。
func exitCodesResult() *Result {
	return &Result{
		Data: map[string]any{"exit_codes": ExitTable()},
		Human: func(w io.Writer) {
			wln(w, "lingedge 退出码（公开契约，随版本只增不改；tools-design §4）")
			wf(w, "\n%-5s %-17s %s\n", "码", "名称", "含义（典型场景）")
			for _, e := range exitTable {
				typ := ""
				if e.Typical != "—" {
					typ = "（" + e.Typical + "）"
				}
				wf(w, "%-5d %-17s %s%s\n", int(e.Code), e.Name, e.Meaning, typ)
			}
			wln(w, "\n约定：10 与 12 互斥；10 表示状态已恢复为操作前，脚本可据此继续。")
		},
	}
}

func versionToolsResult() *Result {
	return &Result{
		Data:  map[string]any{"tools": toolsInfo()},
		Human: func(w io.Writer) { wln(w, version.Info("lingedge")) },
	}
}

// Main 是 cmd/lingedge 的入口：绑定真实环境与 SIGINT，返回进程退出码。
func Main(args []string) int {
	ctx, stop := notifyInterrupt()
	defer stop()
	return Execute(ctx, DefaultEnv(), args)
}

// localizeHelpFlags 给每个命令补上中文的 -h/--help 说明（Cobra 默认是英文）。
func localizeHelpFlags(c *cobra.Command) {
	c.InitDefaultHelpFlag()
	if f := c.Flags().Lookup("help"); f != nil {
		f.Usage = "显示帮助"
	}
	for _, sub := range c.Commands() {
		localizeHelpFlags(sub)
	}
}
