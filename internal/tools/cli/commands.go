package cli

import (
	"strconv"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// commandSpecs 是 tools-design §3 命令树的完整声明。已交付：version/list/status；其余保持 Hidden 直至其 Story 交付。
func commandSpecs() []*cmdSpec {
	compArg := func(allowBuiltin bool) func(inv *invocation) *Error {
		return func(inv *invocation) *Error {
			for _, a := range inv.args {
				if e := checkComponent(a, allowBuiltin); e != nil {
					return e
				}
			}
			return nil
		}
	}
	chain := func(fns ...func(inv *invocation) *Error) func(inv *invocation) *Error {
		return func(inv *invocation) *Error {
			for _, f := range fns {
				if f == nil {
					continue
				}
				if e := f(inv); e != nil {
					return e
				}
			}
			return nil
		}
	}
	flagChecks := func(inv *invocation) *Error {
		if inv.cmd.Flags().Lookup("channel") != nil && inv.changed("channel") {
			if e := checkEnum("channel", inv.str("channel"), "stable", "beta"); e != nil {
				return e
			}
		}
		for _, n := range []string{"version", "to"} {
			if inv.cmd.Flags().Lookup(n) != nil && inv.changed(n) {
				if e := checkSemver(n, inv.str(n)); e != nil {
					return e
				}
			}
		}
		if inv.cmd.Flags().Lookup("set") != nil {
			if e := checkSet(inv.list("set")); e != nil {
				return e
			}
		}
		return nil
	}

	str := func(fs *pflag.FlagSet, name, def, usage string) { fs.String(name, def, usage) }

	versionSpec := &cmdSpec{
		Use: "version", Short: "显示 Tools 与各已装组件的版本",
		Args:  cobra.NoArgs,
		Flags: func(fs *pflag.FlagSet) { hidden(fs.Bool("check", false, "同时联网比对清单"), fs, "check") },
		Run:   runVersion,
	}
	listSpec := &cmdSpec{
		Use: "list", Short: "列出已装组件",
		Args: cobra.NoArgs,
		Flags: func(fs *pflag.FlagSet) {
			hidden(fs.Bool("available", false, "列出清单中可装版本"), fs, "available")
		},
		Run: runList,
	}
	statusSpec := &cmdSpec{Use: "status", Short: "汇总已装组件状态", Args: cobra.NoArgs, Run: runStatus}

	install := &cmdSpec{
		Use: "install [组件]", Short: "安装组件", Story: "Story 1.35（install server）/1.40（静态前端）/1.33（编排）",
		Args: cobra.MaximumNArgs(1), NeedsRoot: true,
		Flags: func(fs *pflag.FlagSet) {
			str(fs, "version", "", "组件版本（semver）；缺省为通道内满足约束的最高稳定版")
			str(fs, "channel", "stable", "发行通道 stable|beta")
			str(fs, "offline", "", "从离线可信发行包安装（同样验签）")
			fs.StringArray("set", nil, "组件初始配置项 key=value（可重复，经 schema 校验）")
			str(fs, "mode", "", "仅 website：static|ssr")
			fs.Bool("skip-start", false, "只物化，不启动（调试）")
			str(fs, "setup-listen", "127.0.0.1:8080", "仅 server：安装向导监听地址")
			fs.Bool("no-rollback", false, "安装后检查失败时不自动回滚")
		},
		Validate: chain(func(inv *invocation) *Error {
			if len(inv.args) == 0 {
				if inv.r.interactive() {
					return nil // 多选安装界面属 Story 1.44；校验后落为「尚未实现」
				}
				return missing("组件名", "用法：lingedge install <"+join(components, "|")+">")
			}
			return checkComponent(inv.args[0], true)
		}, flagChecks, func(inv *invocation) *Error {
			comp := ""
			if len(inv.args) > 0 {
				comp = inv.args[0]
			}
			if inv.changed("mode") {
				if e := checkEnum("mode", inv.str("mode"), "static", "ssr"); e != nil {
					return e
				}
				if comp != "website" {
					return usageErr("usage.flag_not_applicable", "--mode 仅适用于 website", "")
				}
			}
			if inv.changed("setup-listen") {
				if comp != "server" {
					return usageErr("usage.flag_not_applicable", "--setup-listen 仅适用于 server", "")
				}
				return checkHostPort("setup-listen", inv.str("setup-listen"))
			}
			if inv.changed("offline") && inv.str("offline") == "" {
				return usageErr("usage.invalid_value", "--offline 需要 bundle 路径", "")
			}
			return nil
		}),
	}

	update := &cmdSpec{
		Use: "update [组件]", Short: "升级组件", Story: "Story 1.43",
		Args: cobra.MaximumNArgs(1), NeedsRoot: true,
		Flags: func(fs *pflag.FlagSet) {
			fs.Bool("all", false, "升级全部已装组件")
			str(fs, "version", "", "目标版本（semver）")
			str(fs, "channel", "stable", "发行通道 stable|beta")
			str(fs, "offline", "", "离线 bundle")
			fs.Bool("accept-irreversible", false, "接受不可逆迁移（非交互必须显式传入）")
			fs.Bool("no-backup", false, "跳过升级前备份（仅非 server 组件，且需 --yes）")
			fs.Bool("plan-only", false, "等价 --dry-run：只输出升级计划")
			fs.Bool("no-rollback", false, "健康检查失败时不自动回滚")
			hidden(fs.Bool("apply-staged", false, "内部入口：应用 staging 中的 upgrade.request（仅 node，仅 root）"), fs, "apply-staged")
		},
		Validate: chain(func(inv *invocation) *Error {
			all := inv.flag("all")
			if inv.flag("plan-only") {
				inv.r.opts.DryRun = true
			}
			switch {
			case all && len(inv.args) > 0:
				return conflict("all", "<组件>")
			case !all && len(inv.args) == 0:
				return missing("组件名", "用法：lingedge update <组件> 或 lingedge update --all")
			case len(inv.args) == 1:
				if e := checkComponent(inv.args[0], true); e != nil {
					return e
				}
				if inv.flag("no-backup") && inv.args[0] == "server" {
					return usageErr("usage.flag_not_applicable", "--no-backup 不适用于 server（server 升级前必须备份）", "")
				}
				if inv.flag("apply-staged") && inv.args[0] != "node" {
					return usageErr("usage.flag_not_applicable", "--apply-staged 仅适用于 node", "")
				}
			}
			return nil
		}, flagChecks),
		Confirm: func(inv *invocation) *Error {
			if inv.flag("no-backup") {
				return inv.r.confirmYes("跳过升级前备份将无法按备份回滚")
			}
			return nil
		},
		Sub: []*cmdSpec{
			{Use: "check", Short: "获取签名清单，列出可升级组件", Story: "Story 1.43", Args: cobra.NoArgs,
				Flags:    func(fs *pflag.FlagSet) { str(fs, "channel", "stable", "发行通道 stable|beta") },
				Validate: flagChecks},
			{Use: "self", Short: "升级 lingedge 自身", Story: "Story 1.43", Args: cobra.NoArgs, NeedsRoot: true},
		},
	}

	service := &cmdSpec{
		Use: "service", Short: "服务管理",
		Sub: []*cmdSpec{
			{Use: "status [组件]", Short: "查看服务状态", Story: "Story 1.42", Args: cobra.MaximumNArgs(1),
				Flags: func(fs *pflag.FlagSet) { fs.Bool("all", false, "全部组件") },
				Validate: chain(compArg(false), func(inv *invocation) *Error {
					if inv.flag("all") && len(inv.args) > 0 {
						return conflict("all", "<组件>")
					}
					return nil
				})},
			{Use: "start <组件>", Short: "启动服务", Story: "Story 1.42", Args: cobra.ExactArgs(1), NeedsRoot: true, Validate: compArg(false)},
			{Use: "stop <组件>", Short: "停止服务", Story: "Story 1.42", Args: cobra.ExactArgs(1), NeedsRoot: true, Validate: compArg(false)},
			{Use: "restart <组件>", Short: "重启服务", Story: "Story 1.42", Args: cobra.ExactArgs(1), NeedsRoot: true, Validate: compArg(false)},
		},
	}

	logs := &cmdSpec{
		Use: "logs <组件>", Short: "查看组件日志（脱敏）", Story: "Story 1.42", Args: cobra.ExactArgs(1),
		Flags: func(fs *pflag.FlagSet) {
			fs.BoolP("follow", "f", false, "持续输出")
			str(fs, "since", "", "起始时间窗口，如 10m")
			fs.IntP("lines", "n", 100, "输出行数")
		},
		Validate: chain(compArg(false), func(inv *invocation) *Error {
			if inv.changed("since") {
				if e := checkDuration("since", inv.str("since")); e != nil {
					return e
				}
			}
			if n, _ := inv.cmd.Flags().GetInt("lines"); n <= 0 {
				return usageErr("usage.invalid_value", "-n 必须是正整数，收到 "+strconv.Itoa(n), "")
			}
			return nil
		}),
	}

	doctor := &cmdSpec{
		Use: "doctor [组件]", Short: "环境与安装诊断", Story: "Story 1.42", Args: cobra.MaximumNArgs(1),
		Flags: func(fs *pflag.FlagSet) {
			fs.Bool("all", false, "检查全部")
			fs.Bool("strict", false, "警告升级为失败")
			fs.Bool("fix-perms", false, "修复权限问题")
		},
		Validate: chain(compArg(false), func(inv *invocation) *Error {
			if inv.flag("all") && len(inv.args) > 0 {
				return conflict("all", "<组件>")
			}
			return nil
		}),
	}

	config := &cmdSpec{
		Use: "config", Short: "配置管理",
		Sub: []*cmdSpec{
			{Use: "validate [组件]", Short: "校验配置", Story: "Story 1.42", Args: cobra.MaximumNArgs(1),
				Flags:    func(fs *pflag.FlagSet) { str(fs, "file", "", "待校验的配置文件") },
				Validate: compArg(false)},
			{Use: "show <组件>", Short: "脱敏显示配置", Story: "Story 1.42", Args: cobra.ExactArgs(1), Validate: compArg(false)},
			{Use: "edit <组件>", Short: "编辑配置（校验后原子替换）", Story: "Story 1.42", Args: cobra.ExactArgs(1), NeedsRoot: true, Validate: compArg(false)},
			{Use: "setup-token", Short: "重新生成 Server 安装向导的一次性令牌", Story: "Story 1.35", Args: cobra.NoArgs, NeedsRoot: true,
				Flags: func(fs *pflag.FlagSet) { fs.Bool("reset", false, "重新生成令牌") },
				Confirm: func(inv *invocation) *Error {
					if inv.flag("reset") {
						return inv.r.confirmYes("重新生成令牌会使旧令牌立即失效")
					}
					return nil
				}},
		},
	}

	node := &cmdSpec{
		Use: "node", Short: "节点相关",
		Sub: []*cmdSpec{
			{Use: "enroll", Short: "把本机注册为节点", Story: "Story 1.4x（节点注册，见 epics Epic 3）", Args: cobra.NoArgs, NeedsRoot: true,
				Flags: func(fs *pflag.FlagSet) {
					str(fs, "token-file", "", "从文件读取注册令牌")
					str(fs, "token", "", "注册令牌（不推荐：会进入 ps 与 shell 历史）")
					str(fs, "server", "", "Server 地址")
					fs.Bool("force-reenroll", false, "强制重新注册")
				},
				Validate: func(inv *invocation) *Error {
					if inv.changed("token") && inv.changed("token-file") {
						return conflict("token", "token-file")
					}
					if inv.changed("server") {
						return checkServerURL(inv.str("server"))
					}
					return nil
				},
				Confirm: func(inv *invocation) *Error {
					if inv.flag("force-reenroll") {
						return inv.r.confirmYes("强制重新注册会使本机现有节点身份失效")
					}
					return nil
				}},
		},
	}

	backup := &cmdSpec{
		Use: "backup", Short: "备份与恢复",
		Sub: []*cmdSpec{
			{Use: "create", Short: "创建备份", Story: "Story 1.41", Args: cobra.NoArgs, NeedsRoot: true,
				Flags: func(fs *pflag.FlagSet) {
					str(fs, "component", "", "server|node")
					str(fs, "output", "", "输出目录")
					fs.Bool("encrypt", false, "加密备份")
					fs.Bool("include-logs", false, "包含日志")
					fs.Bool("include-master-key", false, "包含主密钥（需加密）")
				},
				Validate: func(inv *invocation) *Error {
					if inv.changed("component") {
						return checkEnum("component", inv.str("component"), "server", "node")
					}
					return nil
				}},
			{Use: "list", Short: "列出备份", Story: "Story 1.41", Args: cobra.NoArgs},
			{Use: "verify <文件>", Short: "校验备份", Story: "Story 1.41", Args: cobra.ExactArgs(1)},
			{Use: "restore <文件>", Short: "从备份恢复", Story: "Story 1.41", Args: cobra.ExactArgs(1), NeedsRoot: true,
				Flags: func(fs *pflag.FlagSet) {
					str(fs, "component", "", "server|node")
					fs.Bool("replace", false, "替换现有数据")
				},
				Validate: func(inv *invocation) *Error {
					if inv.changed("component") {
						return checkEnum("component", inv.str("component"), "server", "node")
					}
					return nil
				},
				Confirm: func(inv *invocation) *Error {
					return inv.r.confirmYes("恢复会覆盖当前数据")
				}},
		},
	}

	rollback := &cmdSpec{
		Use: "rollback <组件>", Short: "回滚组件", Story: "Story 1.43", Args: cobra.ExactArgs(1), NeedsRoot: true,
		Flags: func(fs *pflag.FlagSet) {
			str(fs, "to", "", "回到指定版本（semver）")
			str(fs, "from-backup", "", "从备份恢复（含不可逆迁移时必需）")
		},
		Validate: chain(compArg(false), flagChecks),
		Confirm:  func(inv *invocation) *Error { return inv.r.confirmYes("回滚将切换到旧版本") },
	}

	uninstall := &cmdSpec{
		Use: "uninstall <组件>", Short: "卸载组件（默认保留配置与数据）", Story: "Story 1.41", Args: cobra.ExactArgs(1), NeedsRoot: true,
		Flags: func(fs *pflag.FlagSet) {
			fs.Bool("purge", false, "同时删除配置、数据与备份（需 --confirm-name）")
			str(fs, "confirm-name", "", "本机主机名（--purge 的非交互确认）")
		},
		Validate: func(inv *invocation) *Error {
			if e := checkComponent(inv.args[0], false); e != nil {
				return e
			}
			if inv.changed("confirm-name") && !inv.flag("purge") {
				return usageErr("usage.flag_not_applicable", "--confirm-name 仅与 --purge 一起使用", "")
			}
			return nil
		},
		Confirm: func(inv *invocation) *Error {
			if inv.flag("purge") {
				return inv.r.confirmHostname(inv.str("confirm-name"))
			}
			return inv.r.confirmYes("卸载 " + inv.args[0])
		},
	}

	recoverSpec := &cmdSpec{
		Use: "recover", Short: "处理被中断的事务", Story: "Story 1.33", Args: cobra.NoArgs, NeedsRoot: true,
		Flags: func(fs *pflag.FlagSet) {
			fs.Bool("resume", false, "继续被中断的事务")
			fs.Bool("rollback", false, "回滚被中断的事务")
		},
		Validate: func(inv *invocation) *Error {
			if inv.flag("resume") && inv.flag("rollback") {
				return conflict("resume", "rollback")
			}
			return nil
		},
	}

	return []*cmdSpec{versionSpec, listSpec, statusSpec, install, update, service, logs, doctor, config, node, backup, rollback, uninstall, recoverSpec}
}

func hidden(_ *bool, fs *pflag.FlagSet, name string) { _ = fs.MarkHidden(name) }
