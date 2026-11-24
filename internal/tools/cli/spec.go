package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/wuekevin/lingedge/internal/tools/registry"
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
	// Locks 非 nil 表示这是写命令（install/update/rollback/uninstall/restore/backup create/service start|stop|restart，tools-design §16）：
	// 返回它要操作的组件，命令在 root 检查之后、确认之前取这些组件锁（字典序、非阻塞）并扫描 journal：
	// 被占用 → 退出码 6，存在已死亡且未结束的事务 → 退出码 13。--dry-run 不写任何持久状态，不取锁。
	// recover 不设 Locks（它就是处理未完成事务的命令）；读命令（list/status/doctor…）永远不取锁。
	Locks func(inv *invocation) ([]string, *Error)
	Run   func(inv *invocation) (*Result, error)
	Sub   []*cmdSpec
}

// invocation 是一次命令调用的参数视图。
type invocation struct {
	r    *Run
	cmd  *cobra.Command
	args []string
	// task 是写命令持有的任务锁与事务 ID（registry.Task；Run 为 nil 的未实现命令不持有，Check 后即释放）。
	// 命令的 Run 通过它取得 staging 根目录、TxID 与 OpenTx，不要再自己 Begin（同进程内会与自己冲突）。
	task *registry.Task
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
		// 校验顺序：参数/用法（2、8）→ root（3）→ 任务锁与未完成事务（6、13）→ 确认（9）→ 执行/尚未实现（1）。
		if s.Validate != nil {
			if e := s.Validate(inv); e != nil {
				return e
			}
		}
		if s.NeedsRoot && r.env.Geteuid() != 0 {
			return r.needRoot()
		}
		if s.Locks != nil && !r.opts.DryRun {
			release, e := r.gate(inv, s)
			if e != nil {
				return e
			}
			defer release()
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
		// 命令可以同时返回结果与错误（doctor 有失败项、status 发现不健康、config validate 有问题）：data 仍进入 envelope，
		// 人类输出仍然打印，退出码由错误决定。返回 (nil, err) 的命令行为不变。
		if res != nil {
			r.result = res
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			var ce *Error
			if !errors.As(err, &ce) && !isStructured(err) {
				return &Error{Exit: ExitGeneral, Code: "general.unexpected", Message: "未预期的错误", Cause: err, Hint: "详见日志"}
			}
			return err
		}
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

// registry 构造本次调用的 registry 入口：状态目录由 state.json 路径推出，锁文件与 journal 里的持有者信息用本次的 run_id 与（脱敏的）命令行。
func (r *Run) registry() *registry.Registry {
	return registry.New(registry.LayoutForStatePath(r.env.StatePath), registry.Options{
		Now: r.env.Now, Hostname: r.env.Hostname, RunID: r.runID, Command: "lingedge " + argvString(redactArgs(r.args)),
	})
}

// gate 是写命令的启动检查（AC3/AC4）。
//
// 命令已实现（Run != nil）：registry.Begin 取组件锁并扫描 journal，成功则任务锁保持到命令结束（release），命令从 inv.task 取用。
// 命令尚未实现（Run == nil）：只做 Check（取锁、扫描、立即释放），并且仅在状态目录已存在时才做——尚未安装过任何东西的机器上，
// 未实现的命令不应为了「假装检查」而创建 /var/lib/lingedge/tools；一旦状态目录存在，被占用/有未完成事务的真实结果照常以 6/13 报告。
func (r *Run) gate(inv *invocation, s *cmdSpec) (release func(), _ *Error) {
	none := func() {}
	comps, e := s.Locks(inv)
	if e != nil {
		return none, e
	}
	reg := r.registry()
	if s.Run == nil {
		if !reg.L.Exists() {
			return none, nil
		}
		if err := reg.Check(r.ctx, comps); err != nil {
			return none, asCLIError(err)
		}
		return none, nil
	}
	task, err := reg.Begin(r.ctx, registry.TaskRequest{Components: comps})
	if err != nil {
		return none, asCLIError(err)
	}
	inv.task = task
	return task.Release, nil
}

func asCLIError(err error) *Error {
	if e := RegistryError(err); e != nil {
		return e
	}
	return &Error{Exit: ExitGeneral, Code: "general.unexpected", Message: "未预期的错误", Cause: err, Hint: "详见日志"}
}
