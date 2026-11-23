package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// InstallEnv 是安装编排（install/recover）的外部依赖装配（Env.Install）。零值 = 生产。
// 它只是「世界」的接缝（根目录前缀、外部命令执行器、系统探测、组件规格、故障注入挂钩）：安装的业务逻辑（状态机、journal、撤销、冲突判定）永远是生产代码。
type InstallEnv struct {
	// Root 非空时 /etc /opt /var 路径前缀到该目录（测试用）；生产为空。
	Root string
	// Specs 是可用的组件规格；nil = installer.ProductionSpecs()（server 与静态前端；未交付规格的组件 install 明确失败，不会有假组件）。
	Specs installer.SpecSet
	// Collect 做系统探测；nil = 真实 probe.Collect。
	Collect func(ctx context.Context) (probe.Result, error)
	// Runner 执行 systemctl/useradd 等；nil = 真实的 service.SystemRunner。
	Runner service.Runner
	// WebRunner 执行 nginx/openresty（静态前端的 conf.d 软链：nginx -t 与 -s reload）；nil = service.SystemWebServerRunner()。
	WebRunner service.Runner
	Own       service.Ownership
	Booted    func() bool
	FS        baota.FS
	// NewClient 构造 HTTPS 客户端；nil = downloader.New。
	NewClient func(cfg downloader.Config) (*downloader.Client, error)
	// Hook/WrapJournal/WrapUndoer 是故障注入与 kill 挂钩（仅测试；生产为 nil）。
	Hook        func(point string)
	WrapJournal func(installer.Journal) installer.Journal
	WrapUndoer  func(installer.UndoApplier) installer.UndoApplier
}

func (r *Run) hostPath(p string) string {
	if r.env.Install.Root == "" {
		return p
	}
	return filepath.Join(r.env.Install.Root, p)
}

// newInstaller 装配 installer：tools.yaml（release.base_url/ca_file/proxy/resume_dir、install.health_*）、registry、验证器、探测、执行器。
func (r *Run) newInstaller(events chan<- installer.Event, req installer.Request) (*installer.Installer, error) {
	ie := r.env.Install
	cfgPath, explicit := r.opts.Config, r.opts.Config != ""
	if !explicit {
		cfgPath = r.hostPath(DefaultConfigPath)
	}
	cfg, err := installer.LoadToolsConfig(cfgPath, explicit)
	if err != nil {
		return nil, err
	}
	reg := r.registry()
	runner := ie.Runner
	if runner == nil {
		rr, err := service.SystemRunner()
		if err != nil {
			return nil, err
		}
		runner = rr
	}
	collect := ie.Collect
	if collect == nil {
		collect = func(ctx context.Context) (probe.Result, error) { return probe.Collect(ctx, probe.Options{}) }
	}
	own := ie.Own
	if own == nil {
		own = service.SystemOwnership()
	}
	specs := ie.Specs
	if specs == nil {
		specs = installer.ProductionSpecs()
	}
	return installer.New(installer.Deps{
		Root: ie.Root, Reg: reg, Verifier: r.env.NewVerifier, Config: cfg, Specs: specs, Collect: collect, FS: ie.FS,
		Runner: runner, WebRunner: ie.WebRunner, Own: own, Booted: ie.Booted, Geteuid: r.env.Geteuid, Now: r.env.Now, NewClient: ie.NewClient,
		Confirm: r.confirmPrompt, Events: events, Hook: ie.Hook, WrapJournal: ie.WrapJournal, WrapUndoer: ie.WrapUndoer,
	})
}

// confirmPrompt 实现 installer.Deps.Confirm：交互时向 stderr 提问、从 stdin 读 y/N（非交互由 installer 自行以 Request.Yes 判定，不会走到这里）。
func (r *Run) confirmPrompt(p installer.Prompt) (bool, error) {
	if !r.interactive() {
		return false, nil
	}
	wf(r.env.Stderr, "%s\n", redactText(p.Message))
	for _, it := range p.Items {
		wf(r.env.Stderr, "  - %s\n", redactText(it))
	}
	wf(r.env.Stderr, "继续吗？[y/N]: ")
	ans, err := r.readLine()
	if err != nil {
		return false, nil
	}
	return strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes"), nil
}

// progress 启动进度消费者：把 installer.Event 渲染到 stderr（--json 下为 NDJSON，--progress=none 关闭）。
// 返回事件通道与 wait（关闭通道并等待消费者排空）。UI 不影响业务：installer 对该通道的发送是非阻塞的。
func (r *Run) progress() (chan installer.Event, func()) {
	ch := make(chan installer.Event, 1024)
	done := make(chan struct{})
	mode := r.opts.Progress
	if mode == "" {
		mode = "text"
		if r.jsonMode() {
			mode = "json"
		}
	}
	go func() {
		defer close(done)
		w := bufio.NewWriter(r.env.Stderr)
		defer func() { _ = w.Flush() }()
		for ev := range ch {
			r.log.log("info", ev.Step, "install.event", map[string]any{"type": string(ev.Type), "name": ev.Name, "code": ev.Code, "message": redactText(ev.Message)})
			switch mode {
			case "none":
			case "json":
				ev.Message = redactText(ev.Message)
				b, _ := json.Marshal(map[string]any{"type": "progress", "event": ev})
				wln(w, string(b))
			default:
				switch ev.Type {
				case installer.EventStepStart:
					wf(w, "[%2d/22] %s…\n", ev.Step, redactText(ev.Message))
				case installer.EventStepFailed:
					wf(w, "[%2d/22] 失败：%s\n", ev.Step, redactText(ev.Message))
				case installer.EventRollback:
					wf(w, "回滚：撤销第 %d 步 %s\n", ev.Step, ev.Name)
				case installer.EventWarning:
					wf(w, "警告：%s\n", redactText(ev.Message))
				case installer.EventPhase, installer.EventInfo:
					if ev.Message != "" {
						wf(w, "== %s\n", redactText(ev.Message))
					}
				}
			}
		}
	}()
	return ch, func() { close(ch); <-done }
}

func toWarnings(ws []installer.Warning) []Warning {
	out := make([]Warning, 0, len(ws))
	for _, w := range ws {
		out = append(out, Warning{Code: w.Code, Message: w.Message})
	}
	return out
}

// installData 是 `install` 的 data（schema/data.install.schema.json；追加式）：--dry-run 与真实安装同一形状。
type installData struct {
	Component      string                         `json:"component"`
	DryRun         bool                           `json:"dry_run"`
	Outcome        string                         `json:"outcome"`
	Tx             string                         `json:"tx,omitempty"`
	Plan           *resolver.Plan                 `json:"plan"`
	Preflight      []installer.StepVerdict        `json:"preflight"`
	StepMap        []installer.PlanStepMapping    `json:"step_map"`
	Conflicts      []installer.Conflict           `json:"conflicts"`
	Components     []installer.InstalledComponent `json:"components"`
	Access         []installer.Access             `json:"access"`
	CredentialMode string                         `json:"credential_mode,omitempty"`
	ListenersKnown bool                           `json:"listeners_known"`
}

func newInstallData(comp string, dry bool, pre *installer.PreflightResult, res *installer.Result) installData {
	d := installData{Component: comp, DryRun: dry, Outcome: installer.OutcomePlanned, Plan: pre.Plan, Preflight: pre.Steps, StepMap: pre.StepMap,
		Conflicts: pre.Conflicts, Components: []installer.InstalledComponent{}, Access: []installer.Access{}, CredentialMode: pre.CredentialMode, ListenersKnown: pre.ListenersKnown}
	if d.Conflicts == nil {
		d.Conflicts = []installer.Conflict{}
	}
	if d.Preflight == nil {
		d.Preflight = []installer.StepVerdict{}
	}
	if d.StepMap == nil {
		d.StepMap = []installer.PlanStepMapping{}
	}
	if dry && pre.Noop {
		d.Outcome = installer.OutcomeNoop
	}
	if res != nil {
		d.Outcome, d.Tx = res.Outcome, res.Tx
		if res.Components != nil {
			d.Components = res.Components
		}
		if res.Access != nil {
			d.Access = res.Access
		}
	}
	return d
}

func runInstall(inv *invocation) (*Result, error) {
	r := inv.r
	if len(inv.args) == 0 {
		return nil, notYet(inv, "install（无组件名）", "Story 1.44（交互式多选安装界面）")
	}
	comp := inv.args[0]
	set, err := installer.ParseSet(inv.list("set"))
	if err != nil {
		var se *installer.SetError
		if errors.As(err, &se) {
			return nil, &Error{Exit: ExitUsage, Code: "usage.invalid_set", Message: se.Error(), Hint: "端口：--set port.<组件>.<tcp|udp>.<原端口>=<新端口>[@<绑定地址>]；其他键见组件文档",
				Details: map[string]any{"flag": "set", "key": se.Key}}
		}
		return nil, err
	}
	req := installer.Request{
		Component: comp, Version: inv.str("version"), Channel: inv.str("channel"), Offline: inv.str("offline"), AllowExpired: inv.flag("allow-expired-manifest"),
		Settings: set.Settings, PortOverrides: set.Ports, SkipStart: inv.flag("skip-start"), NoRollback: inv.flag("no-rollback"),
		SetupListen: inv.str("setup-listen"), Variant: variantFor(comp, inv.str("mode")),
		DryRun: r.opts.DryRun, NonInteractive: !r.interactive(), Yes: r.opts.Yes,
	}
	if len(req.Settings) == 0 {
		req.Settings = nil
	}
	ch, wait := r.progress()
	defer wait()
	in, err := r.newInstaller(ch, req)
	if err != nil {
		return nil, err
	}
	if req.DryRun {
		pre, err := in.Plan(r.ctx, req)
		if err != nil {
			return nil, err
		}
		return &Result{Data: newInstallData(comp, true, pre, nil), Warnings: toWarnings(pre.Warnings), Human: func(w io.Writer) { renderPlan(w, pre, nil, true) }}, nil
	}
	res, err := in.Install(r.ctx, inv.task, req)
	if res != nil && res.Pre != nil && err != nil {
		// 带结果的错误（--no-rollback 的健康失败）：仍把访问信息打印给用户。
		r.printAccessOnError(res)
	}
	if err != nil {
		return nil, err
	}
	return &Result{Data: newInstallData(comp, false, res.Pre, res), Warnings: toWarnings(res.Warnings), Human: func(w io.Writer) { renderPlan(w, res.Pre, res, false) }}, nil
}

func (r *Run) printAccessOnError(res *installer.Result) {
	if r.jsonMode() {
		return
	}
	for _, a := range res.Access {
		wf(r.env.Stderr, "%s：%s\n", a.Label, a.Value)
	}
}

// variantFor：--mode 只属于 website（校验已保证），映射为 resolver 的变体名。
func variantFor(comp, mode string) string {
	if comp == "website" && mode != "" {
		return mode
	}
	return ""
}

// renderPlan 输出人类可读的计划与结果。
func renderPlan(w io.Writer, pre *installer.PreflightResult, res *installer.Result, dry bool) {
	p := pre.Plan
	title := "安装计划"
	switch {
	case res != nil && res.Outcome == installer.OutcomeInstalled:
		title = "安装完成"
	case pre.Noop:
		title = "无需安装：计划里的组件都已是目标版本"
	case dry:
		title = "安装计划（--dry-run：未做任何修改）"
	}
	wln(w, title)
	if len(p.Components) > 0 {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		wln(tw, "\n组件\t版本\t动作\t变体\t运行用户")
		for _, c := range p.Components {
			wf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Name, c.Version, c.Action, orDash(c.Variant), orDash(c.RunUser))
		}
		_ = tw.Flush()
	}
	if len(pre.Steps) > 0 {
		wln(w, "\n预检（步骤 1–9）：")
		for _, s := range pre.Steps {
			wf(w, "  %2d %-18s %s", s.N, s.Name, s.Status)
			if s.Message != "" {
				wf(w, "  %s", redactText(s.Message))
			}
			wln(w)
		}
	}
	if len(p.Ports) > 0 {
		wln(w, "\n端口：")
		for _, pt := range p.Ports {
			wf(w, "  %s %s %s/%d\n", pt.Component, pt.Bind, pt.Proto, pt.Port)
		}
	}
	if len(p.Units) > 0 {
		wln(w, "\nsystemd 单元：")
		for _, u := range p.Units {
			wf(w, "  %s  %s\n", u.Name, u.Path)
		}
	}
	if len(p.Files) > 0 {
		wln(w, "\n将创建的文件/目录：")
		for _, f := range p.Files {
			wf(w, "  %-4s %s %s %s [%s]\n", f.Kind, f.Mode, f.Owner, f.Path, f.Policy)
		}
	}
	if len(pre.Conflicts) > 0 {
		wln(w, "\n需要确认的冲突（既有内容不会被覆盖）：")
		for _, c := range pre.Conflicts {
			wf(w, "  %s %s：%s\n", c.Kind, c.Path, redactText(c.Message))
		}
	}
	if res != nil {
		for _, a := range res.Access {
			if a.Secret {
				wf(w, "\n%s：%s\n（仅显示这一次，请立即保存；它不会被写入日志或磁盘）\n", a.Label, a.Value)
				continue
			}
			wf(w, "\n%s：%s\n", a.Label, a.Value)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// recoverData 是 `recover` 的 data（schema/data.recover.schema.json）。
type recoverData struct {
	Mode         string                  `json:"mode"`
	Unfinished   int                     `json:"unfinished"`
	Transactions []installer.TxRecovery  `json:"transactions"`
	StaleTemps   int                     `json:"stale_temps_removed"`
	Result       *recoverInstallResult   `json:"result,omitempty"`
	Pending      []recoverPendingSummary `json:"pending,omitempty"`
}

type recoverInstallResult struct {
	Outcome    string                         `json:"outcome"`
	Components []installer.InstalledComponent `json:"components"`
	Access     []installer.Access             `json:"access"`
}

type recoverPendingSummary struct {
	Tx         string   `json:"tx"`
	Command    string   `json:"command,omitempty"`
	Components []string `json:"components"`
	Status     string   `json:"status"`
	Steps      int      `json:"steps"`
}

func runRecover(inv *invocation) (*Result, error) {
	r := inv.r
	ch, wait := r.progress()
	defer wait()
	in, err := r.newInstaller(ch, installer.Request{})
	if err != nil {
		return nil, err
	}
	infos, err := in.Unfinished(r.ctx)
	if err != nil {
		return nil, err
	}
	pending := make([]recoverPendingSummary, 0, len(infos))
	for _, i := range infos {
		comps := i.Components
		if comps == nil {
			comps = []string{}
		}
		pending = append(pending, recoverPendingSummary{Tx: i.ID, Command: i.Command, Components: comps, Status: i.Status, Steps: i.Steps})
	}
	mode := ""
	switch {
	case inv.flag("resume"):
		mode = "resume"
	case inv.flag("rollback"):
		mode = "rollback"
	}
	if len(infos) == 0 {
		// 没有未完成事务：顺手归档已结束的 journal、回收临时文件。
		res, err := in.Recover(r.ctx, installer.RecoverOptions{Mode: installer.RecoverRollback})
		if err != nil {
			return nil, err
		}
		return &Result{Data: recoverData{Mode: mode, Transactions: res.Transactions, StaleTemps: res.StaleTemps},
			Human: func(w io.Writer) { wln(w, "没有未完成的事务") }}, nil
	}
	if mode == "" {
		// 默认建议回滚（tools-design §12.3）：交互时询问，非交互必须显式给出 --rollback 或 --resume（退出码 9）。
		if !r.interactive() {
			return nil, &Error{Exit: ExitConfirmRequired, Code: "confirm.required",
				Message: fmt.Sprintf("存在 %d 个未完成的事务，recover 需要明确的处理方式，而当前为非交互模式", len(infos)),
				Hint:    "回滚（推荐）：lingedge recover --rollback；从断点续跑（仅限尚未进入激活阶段）：lingedge recover --resume",
				Details: map[string]any{"transactions": toAny(pending)}}
		}
		for _, p := range pending {
			wf(r.env.Stderr, "未完成的事务 %s（%s），组件 %s，已记录 %d 步\n", p.Tx, orDash(p.Command), strings.Join(p.Components, ","), p.Steps)
		}
		ok, _ := r.confirmPrompt(installer.Prompt{Kind: "recover", Message: "将按 journal 逆序撤销这些事务（推荐）。要续跑请改用 --resume。"})
		if !ok {
			return nil, &Error{Exit: ExitConfirmRequired, Code: "confirm.declined", Message: "未确认，已取消", Hint: "重新运行并输入 y，或显式传入 --rollback / --resume"}
		}
		mode = "rollback"
	}
	m := installer.RecoverRollback
	if mode == "resume" {
		m = installer.RecoverResume
	}
	res, err := in.Recover(r.ctx, installer.RecoverOptions{Mode: m, Yes: r.opts.Yes, NonInteractive: !r.interactive()})
	if err != nil {
		return nil, err
	}
	data := recoverData{Mode: mode, Unfinished: len(infos), Transactions: res.Transactions, StaleTemps: res.StaleTemps}
	var warns []Warning
	for _, t := range res.Transactions {
		if t.Result != nil {
			data.Result = &recoverInstallResult{Outcome: t.Result.Outcome, Components: t.Result.Components, Access: t.Result.Access}
			warns = append(warns, toWarnings(t.Result.Warnings)...)
		}
	}
	return &Result{Data: data, Warnings: warns, Human: func(w io.Writer) {
		for _, t := range res.Transactions {
			wf(w, "事务 %s：%s（%s）\n", t.Tx, t.Action, t.Message)
		}
		if data.Result != nil {
			for _, a := range data.Result.Access {
				if a.Secret {
					wf(w, "%s：%s\n（仅显示这一次）\n", a.Label, a.Value)
				} else {
					wf(w, "%s：%s\n", a.Label, a.Value)
				}
			}
		}
	}}, nil
}

// setupTokenData 是 `config setup-token --reset` 的 data（schema/data.config-setup-token.schema.json）。令牌经 access[].value（secret:true）交付，
// 与 install 相同：只出现在这一次输出里，不写日志、不落盘（磁盘上只有它的 SHA-256）。
type setupTokenData struct {
	Component string             `json:"component"`
	Action    string             `json:"action"`
	HashFile  string             `json:"hash_file"`
	Replaced  bool               `json:"replaced"`
	Access    []installer.Access `json:"access"`
}

func runSetupToken(inv *invocation) (*Result, error) {
	r := inv.r
	in, err := r.newInstaller(nil, installer.Request{})
	if err != nil {
		return nil, err
	}
	res, err := in.ResetSetupToken(r.ctx)
	if err != nil {
		return nil, err
	}
	acc := installer.Access{Component: "server", Kind: "setup_token", Label: "新的安装向导令牌（仅显示一次）", Value: res.Token, Secret: true}
	return &Result{
		Data: setupTokenData{Component: "server", Action: "reset", HashFile: res.HashFile, Replaced: res.Replaced, Access: []installer.Access{acc}},
		Human: func(w io.Writer) {
			wf(w, "已重新生成安装向导令牌，旧令牌已失效（哈希写入 %s，属主 lingedge 0600；Server 无需重启）。\n\n%s：%s\n（仅显示这一次，请立即保存；它不会被写入日志或磁盘，磁盘上只有 SHA-256。）\n", res.HashFile, acc.Label, acc.Value)
		},
	}, nil
}
