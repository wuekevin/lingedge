package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// Access 是步骤 22 输出的访问/管理信息。Secret=true 的值（setup token）只在本次输出中出现一次，不落盘、不写日志、不进事件。
type Access struct {
	Component string `json:"component"`
	Kind      string `json:"kind"` // setup_url | ssh_tunnel | setup_token | master_key_fingerprint | master_key_warning | site_url | nginx_include | baota_guide | note
	Label     string `json:"label"`
	Value     string `json:"value"`
	// JSON 键刻意不叫 secret：信封脱敏按键名把名为 secret 的键的值替换成 "***"（布尔也不例外），会破坏 schema。
	Secret bool `json:"one_time,omitempty"`
}

// InstalledComponent 是一个已装组件的摘要。
type InstalledComponent struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	State      string `json:"state"`
	ReleaseDir string `json:"release_dir"`
}

// Result 是安装的结果。
type Result struct {
	Outcome    string               `json:"outcome"`
	Tx         string               `json:"tx,omitempty"`
	Components []InstalledComponent `json:"components"`
	Access     []Access             `json:"access"`
	Warnings   []Warning            `json:"warnings"`
	// Pre 是预检与计划。
	Pre *PreflightResult `json:"-"`
	// Resumed 为 true 表示这是 recover --resume 续跑完成的事务。
	Resumed bool `json:"resumed,omitempty"`
}

// Install 执行完整的 22 步安装。task 是写命令入口 registry.Begin 得到的任务（持有组件锁；Install 会为依赖闭包追加锁并创建 journal）。
// 返回错误时事务已按 journal 回滚（或按错误类型保留 journal，见 Kind）；调用方不需要（也不应）再 Abort。
func (in *Installer) Install(ctx context.Context, task *registry.Task, req Request) (*Result, error) {
	pre, err := in.Plan(ctx, req)
	if err != nil {
		return nil, err
	}
	if pre.Noop {
		return &Result{Outcome: OutcomeNoop, Pre: pre, Components: []InstalledComponent{}, Access: []Access{}, Warnings: pre.Warnings}, nil
	}
	if pre.acceptedBy == "conflicts" {
		pre.req.Accepted = true
	}
	var names []string
	for _, c := range pre.Plan.Components {
		if c.Action == "install" {
			names = append(names, c.Name)
		}
		if c.Wanted { // 续跑只固定用户直接选择的组件（内部依赖不能被直接选择，由求解器随之解出；包若变了，取包后的 SHA256 比对会以退出码 5 拒绝）
			pre.req.Pinned = append(pre.req.Pinned, resolver.Want{Component: c.Name, Version: c.Version, Variant: c.Variant})
		}
	}
	if err := task.Extend(names); err != nil {
		return nil, err
	}
	j, err := task.OpenTx(ctx)
	if err != nil {
		return nil, err
	}
	x := &execution{in: in, pre: pre, task: task, tx: task.TxID(), start: 1}
	x.j = Journal(j)
	if in.d.WrapJournal != nil {
		x.j = in.d.WrapJournal(x.j)
	}
	x.init()
	return x.run(ctx)
}

// execution 是一次事务（新装或续跑）的运行状态。
type execution struct {
	in   *Installer
	pre  *PreflightResult
	task *registry.Task
	j    Journal
	tx   string
	// start 是第一个要执行的步骤；更小的步骤视为已完成（续跑）。
	start   int
	resumed bool

	udo      UndoApplier
	acq      []*downloader.Acquired
	acqByC   map[string]*downloader.Acquired
	fetchCur int // 取包阶段当前在途的步骤（0 = 无）
	fetchMax int // 取包阶段已写过 begin 的最大步骤

	access      []Access
	warnings    []Warning
	tokenUsable bool
	healthErr   error // --no-rollback 下保留的健康检查失败
	comps       []InstalledComponent
	userCreated map[string]bool
	// nginxLinked：静态前端的 conf.d 软链已创建且 nginx -t 与 reload 通过（Story 1.40）。
	nginxLinked map[string]bool
	// committed：第 21 步已把 state.json 原子写入（提交点已过）。
	committed bool
	// masterKeyFP 是本次新建的 master.key 的短指纹（F-7；仅当本事务创建了它）。
	masterKeyFP string
}

func (x *execution) init() {
	svc := &service.Undoer{Root: x.in.d.Root, Prov: x.in.prov, Sys: x.in.sys}
	var u UndoApplier = &Undoer{Root: x.in.d.Root, Reg: x.in.d.Reg, Svc: svc, Web: x.in.webRunnerOrNil(), Safety: x.in.d.RestoreSafety,
		Notice: func(code, msg string) { x.warn(Warning{Code: code, Step: StepStart, Message: msg}) }}
	if x.in.d.WrapUndoer != nil {
		u = x.in.d.WrapUndoer(u)
	}
	x.udo = u
	x.acqByC = map[string]*downloader.Acquired{}
	x.nginxLinked = map[string]bool{}
	x.warnings = append([]Warning(nil), x.pre.Warnings...)
}

func (x *execution) hook(point string, n int) {
	if x.in.d.Hook != nil {
		x.in.d.Hook(fmt.Sprintf("%s:%d", point, n))
	}
}

func (x *execution) installComps() []resolver.PlanComponent {
	var out []resolver.PlanComponent
	for _, name := range x.pre.Plan.Order {
		for _, c := range x.pre.Plan.Components {
			if c.Name == name && c.Action == "install" {
				out = append(out, c)
			}
		}
	}
	return out
}

func (x *execution) emitStep(t EventType, n int, msg string) {
	x.in.ev.emit(Event{Type: t, Step: n, Name: StepName(n), Phase: PhaseOf(n), Message: msg})
}

func isCancel(err error) bool { return errors.Is(err, context.Canceled) }

// run 按 22 步执行；失败时逆序撤销。
func (x *execution) run(ctx context.Context) (*Result, error) {
	err := x.sequence(ctx)
	if err != nil {
		if x.committed {
			// 第 21 步（提交点）之后的失败只可能出在 journal 收尾（done/commit）：状态已原子提交，安装实际已完成，绝不能为此回滚。
			// journal 保持未完成，下次命令提示 recover，recover --resume 只补写收尾记录。
			x.warnings = append(x.warnings, Warning{Code: "journal.finish_failed", Message: "安装已提交，但事务日志未能收尾：" + err.Error() + "；请运行 lingedge recover --resume"})
			x.in.ev.emit(Event{Type: EventWarning, Code: "journal.finish_failed", Message: "安装已提交，但事务日志未能收尾"})
			x.j.Close()
			if len(x.access) == 0 {
				_ = x.doAccess(ctx) // 第 22 步无副作用：访问信息（含只显示一次的 setup token）照常交付
			}
			return x.finishUnjournaled(), nil
		}
		return nil, x.fail(ctx, err)
	}
	return x.finish(ctx)
}

// stepErr 记录失败发生的步骤。
type stepErr struct {
	step int
	err  error
}

func (e *stepErr) Error() string { return fmt.Sprintf("第 %d 步失败: %v", e.step, e.err) }
func (e *stepErr) Unwrap() error { return e.err }

func (x *execution) sequence(ctx context.Context) error {
	// 预检 1–9：已在 Plan 里只读求值；这里按 22 步在 journal 里补记（无副作用，undo 为空；第 1 步携带请求供 recover --resume 使用）。
	for n := StepOSDetect; n <= StepVersion; n++ {
		n := n
		if err := x.step(ctx, n, true, func() (registry.Undo, error) {
			if n != StepOSDetect {
				return registry.Undo{}, nil
			}
			ra, err := encodeRequest(x.pre.req)
			if err != nil {
				return registry.Undo{}, err
			}
			return registry.Undo{Actions: []registry.UndoAction{ra}}, nil
		}, func(context.Context) error { return nil }); err != nil {
			return err
		}
	}
	x.in.ev.emit(Event{Type: EventPhase, Phase: PhaseFetch, Message: "取包与校验"})
	if err := x.fetch(ctx); err != nil {
		return err
	}
	x.in.ev.emit(Event{Type: EventPhase, Phase: PhaseMaterialize, Message: "物化"})
	// 14–20 包在 Acquired.Finish 里：全部成功后才提交信任状态（sequence 推进/轮换公钥落盘）；失败则 Discard，信任库不变（1.28b）。
	err := finishAll(x.acq, func() error {
		for _, s := range []struct {
			n    int
			plan func() (registry.Undo, error)
			do   func(context.Context) error
		}{
			{StepExtract, x.planExtract, x.doExtract},
			{StepCreateUser, x.planUsers, x.doUsers},
			{StepCreateDirs, x.planDirs, x.doDirs},
			{StepInstallUnits, x.planUnits, x.doUnits},
			{StepInitConfig, x.planConfig, x.doConfig},
		} {
			if err := x.step(ctx, s.n, false, s.plan, s.do); err != nil {
				return err
			}
		}
		x.in.ev.emit(Event{Type: EventPhase, Phase: PhaseActivate, Message: "激活"})
		if err := x.step(ctx, StepStart, false, x.planStart, x.doStart); err != nil {
			return err
		}
		return x.step(ctx, StepHealth, false, func() (registry.Undo, error) { return registry.Undo{}, nil }, x.doHealth)
	})
	if err != nil {
		var se *stepErr
		if !errors.As(err, &se) { // Finish 自身的 Commit 失败
			return &stepErr{step: StepHealth, err: err}
		}
		return err
	}
	x.in.ev.emit(Event{Type: EventPhase, Phase: PhaseCommit, Message: "提交"})
	if err := x.step(ctx, StepSaveState, false, x.planState, x.doState); err != nil {
		return err
	}
	return x.step(ctx, StepAccessInfo, false, func() (registry.Undo, error) { return registry.Undo{}, nil }, x.doAccess)
}

// finishAll 把 fn 嵌套进每个 Acquired 的 Finish：fn 成功则按内到外依次提交，失败则全部 Discard。
func finishAll(acqs []*downloader.Acquired, fn func() error) error {
	if len(acqs) == 0 {
		return fn()
	}
	return acqs[0].Finish(func(*downloader.Acquired) error { return finishAll(acqs[1:], fn) })
}

// step 是每一步的统一外壳：先在 journal 里写 begin（含 undo，fsync），再执行副作用，最后写 done。
// quiet 用于预检（已在 Plan 里发过事件）。start 之前的步骤视为已完成（续跑）。
func (x *execution) step(ctx context.Context, n int, quiet bool, plan func() (registry.Undo, error), do func(context.Context) error) error {
	if n < x.start {
		if !quiet {
			x.emitStep(EventStepSkipped, n, "续跑：此前已完成")
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return &stepErr{step: n, err: err}
	}
	x.hook("before", n)
	undo, err := plan()
	if err != nil {
		return &stepErr{step: n, err: err}
	}
	if err := x.j.Begin(n, StepName(n), undo); err != nil {
		return &stepErr{step: n, err: err}
	}
	x.hook("begun", n)
	if !quiet {
		x.emitStep(EventStepStart, n, Steps[n-1].Title)
	}
	// 14–18、21 是原子的物化/提交步骤：收到 SIGINT 后也要完成当前步骤（不在半步中退出），回滚在步骤之间进行。
	bctx := ctx
	if PhaseOf(n) == PhaseMaterialize || n == StepSaveState {
		bctx = context.WithoutCancel(ctx)
	}
	if err := do(bctx); err != nil {
		if !quiet {
			x.emitStep(EventStepFailed, n, err.Error())
		}
		return &stepErr{step: n, err: err}
	}
	x.hook("effect", n)
	if err := x.j.Done(n); err != nil {
		return &stepErr{step: n, err: err}
	}
	x.hook("done", n)
	if !quiet {
		x.emitStep(EventStepDone, n, "")
	}
	return nil
}

// ---- 失败与回滚 ----

func (x *execution) fail(ctx context.Context, cause error) error {
	step := 0
	var se *stepErr
	inner := cause
	if errors.As(cause, &se) {
		step, inner = se.step, se.err
	}
	rctx := context.WithoutCancel(ctx) // 回滚本身不被 SIGINT 打断
	x.in.ev.emit(Event{Type: EventInfo, Phase: PhaseOf(step), Message: "开始回滚（逆序撤销）", Code: "rollback.start"})
	if rerr := x.in.rollbackJournal(rctx, x.j, x.tx, x.udo, "install failed at step "+fmt.Sprint(step), x.sysUnitsTouched); rerr != nil {
		e := &Error{Kind: KindRollbackFailed, Step: step, Msg: "回滚失败，事务日志已保留：" + rerr.Error(), Cause: inner,
			Hint: "排查原因后运行 lingedge recover --rollback 重试（撤销是幂等的）；journal：" + x.in.journalPath(x.tx), SystemState: "needs_manual",
			Details: map[string]any{"tx": x.tx, "failed_step": step}}
		x.j.Close()
		return e
	}
	switch {
	case isCancel(inner):
		return &Error{Kind: KindInterrupted, Step: step, Msg: "已收到 SIGINT：当前步骤完成后已回滚，系统恢复到安装前", Cause: inner, SystemState: "rolled_back",
			Details: map[string]any{"tx": x.tx}}
	case PhaseOf(step) == PhaseActivate || PhaseOf(step) == PhaseCommit:
		return &Error{Kind: KindRolledBack, Step: step, Msg: "激活后失败，已回滚并恢复到安装前的状态", Cause: inner, SystemState: "rolled_back",
			Hint: "详见 cause；修复后可重试安装", Details: map[string]any{"tx": x.tx}}
	}
	return &Error{Kind: KindStepFailed, Step: step, Msg: "安装失败，已回滚", Cause: inner, SystemState: "rolled_back", Details: map[string]any{"tx": x.tx}}
}

func (in *Installer) journalPath(tx string) string { return in.d.Reg.L.JournalFile(tx) }

// sysUnitsTouched 供回滚在撤销了单元文件之后做一次 daemon-reload。
func (x *execution) sysUnitsTouched(ctx context.Context) error { return x.in.sys.DaemonReload(ctx) }

// rollbackJournal 逆序执行 journal 的撤销计划：每撤销一步记 undone（撤销本身幂等，被中断后可续做），全部成功后以 abort 结束并归档。
// 撤销失败 → 立即返回错误（journal 保留）；journal 写入失败 → 其余步骤仍尽力撤销，最后返回错误（journal 保留，recover 会复核）。
func (in *Installer) rollbackJournal(ctx context.Context, j Journal, tx string, udo UndoApplier, reason string, reload func(context.Context) error) error {
	t, err := in.d.Reg.ReadTx(tx)
	if err != nil {
		return err
	}
	var jerr error
	reloadNeeded := false
	for _, st := range t.UndoPlan() {
		in.ev.emit(Event{Type: EventRollback, Step: st.Step, Name: st.Name, Phase: PhaseOf(st.Step), Message: "撤销"})
		if st.Undo != nil {
			// 一步内的撤销动作按记录顺序依次执行（与 registry/service 的约定一致：记录者负责把它们排成「先依赖者、后被依赖者」的撤销顺序）。
			for _, a := range st.Undo.Actions {
				if _, err := udo.Apply(ctx, a); err != nil {
					return fmt.Errorf("第 %d 步（%s）的撤销动作 %s 失败: %w", st.Step, st.Name, a.Action, err)
				}
				if touchesUnits(a) {
					reloadNeeded = true
				}
			}
		}
		if err := j.Undone(st.Step); err != nil && jerr == nil {
			jerr = err
		}
	}
	if reloadNeeded && reload != nil {
		if err := reload(ctx); err != nil {
			in.ev.emit(Event{Type: EventWarning, Code: "rollback.daemon_reload", Message: "撤销单元后 systemctl daemon-reload 失败：" + err.Error()})
		}
	}
	if jerr != nil {
		return fmt.Errorf("事务日志无法记录撤销进度: %w", jerr)
	}
	if err := j.Abort(reason); err != nil {
		return fmt.Errorf("事务日志无法以 abort 结束: %w", err)
	}
	if err := j.Archive(); err != nil {
		in.ev.emit(Event{Type: EventWarning, Code: "journal.archive", Message: "journal 归档失败（已 abort，下次 recover 会补做）：" + err.Error()})
	}
	return nil
}

// ---- 收尾 ----

func (x *execution) finish(ctx context.Context) (*Result, error) {
	if err := x.j.Commit(); err != nil {
		// 状态已原子写入（第 21 步），安装实际已完成；journal 未能收尾则下次命令会提示 recover（续跑只做收尾）。
		x.warnings = append(x.warnings, Warning{Code: "journal.commit_failed", Message: "安装已完成，但事务日志未能收尾；请运行 lingedge recover --resume：" + err.Error()})
	} else if err := x.j.Archive(); err != nil {
		x.warnings = append(x.warnings, Warning{Code: "journal.archive_failed", Message: "事务日志归档失败（已 commit，下次运行会补做）：" + err.Error()})
	}
	x.cleanStaging()
	res := &Result{Outcome: OutcomeInstalled, Tx: x.tx, Components: x.comps, Access: x.access, Warnings: x.warnings, Pre: x.pre, Resumed: x.resumed}
	if res.Components == nil {
		res.Components = []InstalledComponent{}
	}
	if res.Access == nil {
		res.Access = []Access{}
	}
	if x.healthErr != nil {
		return res, &Error{Kind: KindHealthFailed, Step: StepHealth, Msg: "健康检查失败，已按 --no-rollback 保留现场（状态记为 failed）", Cause: x.healthErr,
			Hint: "排查后 lingedge doctor；或 lingedge uninstall 后重装", SystemState: "partial", Details: map[string]any{"tx": x.tx}}
	}
	return res, nil
}

// finishUnjournaled 在 journal 无法收尾时构造成功结果（状态已提交；journal 留给 recover --resume 收尾）。
func (x *execution) finishUnjournaled() *Result {
	x.cleanStaging()
	res := &Result{Outcome: OutcomeInstalled, Tx: x.tx, Components: x.comps, Access: x.access, Warnings: x.warnings, Pre: x.pre, Resumed: x.resumed}
	if res.Components == nil {
		res.Components = []InstalledComponent{}
	}
	if res.Access == nil {
		res.Access = []Access{}
	}
	return res
}

func (x *execution) cleanStaging() {
	for _, c := range x.pre.Plan.Components {
		_ = removeAll(x.stagingDir(c.Name))
	}
}

func (x *execution) stagingDir(component string) string {
	return filepath.Join(x.in.d.Reg.L.StagingDir(), x.tx+"-"+component)
}

// ---- 取包（10–13）----

var stageStep = map[downloader.Stage]int{
	downloader.StageManifest: StepFetchManifest, downloader.StageVerify: StepVerifyManifest,
	downloader.StagePackage: StepDownload, downloader.StageCheck: StepVerifyPackage,
}

func (x *execution) fetchUndo() registry.Undo {
	var u registry.Undo
	for _, c := range x.installComps() {
		u.Actions = append(u.Actions, registry.UndoAction{Action: actRemoveStaging, Path: x.stagingDir(c.Name), Created: true})
	}
	return u
}

// onStage 是 downloader.Params.OnStage：把 Acquire 的四个阶段映射成 journal 的 10–13 步。
// 多个组件依次取包时，journal 的步骤号只前进不后退：后一个组件的同名阶段不再重复记（其副作用同样落在 staging/<tx>-<组件>，
// 已被这些步骤的 undo 覆盖）；续跑时已完成的阶段不再记录。
func (x *execution) onStage(ctx context.Context) func(downloader.Stage) error {
	return func(s downloader.Stage) error {
		n := stageStep[s]
		if n < x.start || n <= x.fetchMax {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return &stepErr{step: n, err: err}
		}
		if x.fetchCur != 0 {
			if err := x.closeFetchStep(); err != nil {
				return err
			}
		}
		x.hook("before", n)
		if err := x.j.Begin(n, StepName(n), x.fetchUndo()); err != nil {
			return &stepErr{step: n, err: err}
		}
		x.hook("begun", n)
		x.fetchCur, x.fetchMax = n, n
		x.emitStep(EventStepStart, n, Steps[n-1].Title)
		return nil
	}
}

func (x *execution) closeFetchStep() error {
	n := x.fetchCur
	x.hook("effect", n)
	if err := x.j.Done(n); err != nil {
		return &stepErr{step: n, err: err}
	}
	x.hook("done", n)
	x.emitStep(EventStepDone, n, "")
	x.fetchCur = 0
	return nil
}

// fetch 执行步骤 10–13：对计划里每个要安装的组件 Acquire（DeferCommit，提交由 finishAll 在安装成功后完成）。
func (x *execution) fetch(ctx context.Context) error {
	comps := x.installComps()
	if len(comps) == 0 {
		return nil
	}
	pre := x.pre
	v, err := x.in.verifier(true)
	if err != nil {
		return &stepErr{step: StepFetchManifest, err: err}
	}
	var src downloader.Params
	var closeClient func()
	if pre.req.Offline != "" {
		src.BundlePath = pre.req.Offline
	} else {
		url, err := x.in.d.Config.ManifestURL(pre.req.Channel)
		if err != nil {
			return &stepErr{step: StepFetchManifest, err: err}
		}
		cl, err := x.in.d.NewClient(x.in.d.Config.DownloaderConfig(filepath.Join(x.in.d.Reg.L.Root, "downloads"), true))
		if err != nil {
			return &stepErr{step: StepFetchManifest, err: err}
		}
		closeClient = cl.Close
		src.Online = &downloader.OnlineSource{Client: cl, ManifestURL: url}
	}
	if closeClient != nil {
		defer closeClient()
	}
	platform := pre.probe.System.OS + "-" + pre.probe.System.Arch
	for _, c := range comps {
		if x.resumed {
			// 续跑：上次中断留下的 staging/<tx>-<组件> 里的包不被信任，重新取得并完整重验（包仍在 staging 也不例外）。
			// 删除只限本事务自己的目录（名字由 tx 派生，且已被步骤 10 的 undo 覆盖）。
			if err := os.RemoveAll(x.stagingDir(c.Name)); err != nil {
				return &stepErr{step: StepFetchManifest, err: err}
			}
		}
		p := src
		p.Verifier, p.KeysDir = v, filepath.Join(x.in.d.Reg.L.TrustDir(), "keys.d")
		p.Channel, p.AllowExpired = pre.req.Channel, pre.req.AllowExpired
		p.Component, p.Version, p.Variant, p.Platform = c.Name, c.Version, c.Variant, platform
		p.StagingRoot, p.TxID = x.in.d.Reg.L.StagingDir(), x.tx+"-"+c.Name
		p.DeferCommit = true
		if spec := x.in.d.Specs[c.Name]; spec != nil {
			p.Executable = spec.Executables(c.Version, c.Variant)
		}
		p.OnStage = x.onStage(ctx)
		a, err := downloader.Acquire(ctx, p)
		if err != nil {
			var se *stepErr
			if errors.As(err, &se) {
				return err
			}
			n := x.fetchCur
			if n == 0 {
				n = StepFetchManifest
			}
			return &stepErr{step: n, err: err}
		}
		if a.Artifact.SHA256 != c.SHA256 || a.Version != c.Version {
			_ = a.Discard()
			return &stepErr{step: StepVerifyPackage, err: manifestChanged(c.Name)}
		}
		x.acq = append(x.acq, a)
		x.acqByC[c.Name] = a
		x.in.ev.emit(Event{Type: EventInfo, Step: StepVerifyPackage, Component: c.Name, Message: "已取得并校验 " + c.Name + " " + c.Version})
	}
	if x.fetchCur != 0 {
		if err := x.closeFetchStep(); err != nil {
			return err
		}
	}
	// 续跑到这里时，更早完成的取包步骤没有 begin（n < start），无需补记。
	return nil
}

// manifestChanged 是预检与取包之间清单变化的校验错误（manifest.KindPackage → 退出码 5）。
func manifestChanged(component string) error {
	return &manifest.Error{Kind: manifest.KindPackage, Msg: "清单在预检与取包之间发生了变化（" + component + " 的包与预检时选定的不一致），已中止",
		Details: map[string]any{"component": component}}
}

// waitHealth 在 timeout 窗口内按 interval 重复调用 probe，直到成功、窗口耗尽或 ctx 取消。窗口用真实定时器计时（不依赖可注入的 Now：
// 测试里时钟是固定的，用它算截止时间会永远等下去）。
func waitHealth(ctx context.Context, timeout, interval time.Duration, probe func(context.Context) error) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if last = probe(ctx); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("健康检查在 %s 内未通过：%w", timeout, last)
		case <-time.After(interval):
		}
	}
}

func joinNames(ns []string) string { return strings.Join(ns, "、") }
