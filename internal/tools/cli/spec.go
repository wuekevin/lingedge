package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// cmdSpec 是命令树的声明式描述（tools-design §3）。
//
// Run 为 nil 表示命令尚未实现：该命令（及其全部子命令均未实现的父命令）保持 Hidden，不出现在 --help 中；
// 被调用时仍会先做参数校验（退出码 2/8）、root 检查（3）与确认（9），最后返回「尚未实现」（退出码 1），
// 绝不返回假成功（AC1）。Story 字段指出由哪个后续 Story 交付。
type cmdSpec struct {
	Use       string
	Short     string
	Story     string // 交付该命令的后续 Story（仅未实现时使用）
	NeedsRoot bool
	Args      cobra.PositionalArgs
	Flags     func(fs *pflag.FlagSet)
	Validate  func(inv *invocation) *Error
	Confirm   func(inv *invocation) *Error
	Run       func(inv *invocation) (*Result, error)
	Sub       []*cmdSpec
}

// invocation 是一次命令调用的参数视图。
type invocation struct {
	r    *Run
	cmd  *cobra.Command
	args []string
}

func (i *invocation) str(name string) string    { v, _ := i.cmd.Flags().GetString(name); return v }
func (i *invocation) flag(name string) bool     { v, _ := i.cmd.Flags().GetBool(name); return v }
func (i *invocation) list(name string) []string { v, _ := i.cmd.Flags().GetStringArray(name); return v }
func (i *invocation) changed(name string) bool  { return i.cmd.Flags().Changed(name) }

func (s *cmdSpec) implemented() bool {
	if s.Run != nil {
		return true
	}
	for _, c := range s.Sub {
		if c.implemented() {
			return true
		}
	}
	return false
}

func (s *cmdSpec) name() string { return strings.Fields(s.Use)[0] }

// build 把声明转换为 cobra 命令。
func (s *cmdSpec) build(r *Run) *cobra.Command {
	c := &cobra.Command{
		Use:    s.Use,
		Short:  s.Short,
		Hidden: !s.implemented(),
		Args:   s.Args,
	}
	if s.Flags != nil {
		s.Flags(c.Flags())
	}
	for _, sub := range s.Sub {
		c.AddCommand(sub.build(r))
	}
	if len(s.Sub) > 0 && s.Run == nil && s.Args == nil {
		// 纯分组命令：未给子命令或给了未知子命令都是用法错误（退出码 2）。
		c.Args = cobra.ArbitraryArgs
		c.RunE = func(cmd *cobra.Command, args []string) error {
			names := make([]string, 0, len(s.Sub))
			for _, sub := range s.Sub {
				names = append(names, sub.name())
			}
			if len(args) > 0 {
				return usageErr("usage.unknown_subcommand", fmt.Sprintf("%s 没有子命令 %q", cmd.CommandPath(), args[0]), "可用子命令："+strings.Join(names, "|"))
			}
			return usageErr("usage.missing_subcommand", cmd.CommandPath()+" 需要子命令", "可用子命令："+strings.Join(names, "|"))
		}
		return c
	}
	c.RunE = func(cmd *cobra.Command, args []string) error {
		inv := &invocation{r: r, cmd: cmd, args: args}
		if err := r.ctx.Err(); err != nil {
			return err
		}
		// 校验顺序：参数/用法（2、8）→ root（3）→ 确认（9）→ 执行/尚未实现（1）。
		if s.Validate != nil {
			if e := s.Validate(inv); e != nil {
				return e
			}
		}
		if s.NeedsRoot && r.env.Geteuid() != 0 {
			return r.needRoot()
		}
		if s.Confirm != nil && (s.Run != nil || !r.interactive()) {
			if e := s.Confirm(inv); e != nil {
				return e
			}
		}
		if s.Run == nil {
			story := s.Story
			if story == "" {
				story = "后续 Story"
			}
			return &Error{
				Exit: ExitGeneral, Code: "cli.not_implemented",
				Message: fmt.Sprintf("命令 %q 尚未实现（%s 交付）", r.cmdPath, story),
				Hint:    "该命令当前不可用，未做任何修改；进度见 docs/bmad/sprint-status.yaml",
				Details: map[string]any{"command": r.cmdPath, "story": story},
			}
		}
		res, err := s.Run(inv)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			var ce *Error
			if !errors.As(err, &ce) {
				return &Error{Exit: ExitGeneral, Code: "general.unexpected", Message: "未预期的错误", Cause: err, Hint: "详见日志"}
			}
			return err
		}
		r.result = res
		return nil
	}
	return c
}

// needRoot 构造「需要 root」的错误（退出码 3）并给出 sudo 提示。提示中的参数经脱敏（--token 等记为 ***）。
func (r *Run) needRoot() *Error {
	return &Error{
		Exit: ExitPrecondition, Code: "precondition.root_required",
		Message: "该命令需要 root 权限（lingedge 不会自行提权）",
		Hint:    "请运行：sudo lingedge " + argvString(redactArgs(r.args)),
		Details: map[string]any{"euid": r.env.Geteuid()},
	}
}
