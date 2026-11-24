package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/dbmigrate"
	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/rollback"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// updateExec 是一次升级事务的运行状态。
type updateExec struct {
	in   *Installer
	env  *UpdateEnv
	plan *UpdatePlan
	task *registry.Task
	j    Journal
	tx   string
	udo  UndoApplier
	conn *backup.DBConn // 仅内存；用后 Close，绝不落盘
	acq  *downloader.Acquired

	hasDB     bool
	decision  rollback.Decision
	accepted  bool
	mplan     *dbmigrate.Plan
	pre, tgt  int64
	backupPth string
	backupEnc bool
	verified  bool
	wasActive bool
	started   bool

	healthErr error
	committed bool
	health    string
	warnings  []Warning
	pruned    []string
	checks    *PostCheckReport
	details   []string
}

func (x *updateExec) hook(point string, n int) {
	if x.in.d.Hook != nil {
		x.in.d.Hook(fmt.Sprintf("%s:%d", point, n))
	}
}

func (x *updateExec) emit(t EventType, n int, msg string) {
	x.in.ev.emit(Event{Type: t, Op: "update", Total: UpTotalSteps, Step: n, Name: UpdateStepName(n), Component: x.plan.Component, Message: msg})
}

func (x *updateExec) warn(w Warning) {
	x.warnings = append(x.warnings, w)
	x.in.ev.emit(Event{Type: EventWarning, Op: "update", Total: UpTotalSteps, Step: w.Step, Code: w.Code, Component: w.Component, Message: w.Message})
}

// confirmIrreversible：含不可逆迁移须确认（AC4）。交互键入 confirm-irreversible；非交互必须 --accept-irreversible，否则退出码 9（此时尚未改动任何东西）。
func (in *Installer) confirmIrreversible(env *UpdateEnv, d rollback.Decision, req UpdateRequest, comp, from, to string, items []string) (bool, error) {
	if !d.NeedConfirm {
		return false, nil
	}
	if req.AcceptIrreversible {
		return true, nil
	}
	msg := fmt.Sprintf("升级 %s %s → %s 含不可逆迁移。%s", comp, from, to, d.Promise)
	if !req.NonInteractive && env.TypedConfirm != nil {
		ok, err := env.TypedConfirm(msg, items, "confirm-irreversible")
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, &Error{Kind: KindConfirmRequired, Msg: "含不可逆迁移，需要明确确认：" + msg, SystemState: "unchanged",
		Hint:    "交互运行并键入 confirm-irreversible，或加 --accept-irreversible（非交互）；确认前已强制备份并校验，此时尚未改动任何东西",
		Details: map[string]any{"code": "confirm.irreversible", "classes": d.Classes}}
}

// Update 执行升级（tools-design §17.3 的 13 步）。task 是写命令入口 registry.Begin 得到的任务（持有组件锁）。
// 失败时事务已按 journal 回滚并复核（成功回滚 → KindRolledBack 退出码 10；回滚失败 → KindRollbackFailed 退出码 12，journal 保留）；
// 在改动系统之前失败（取清单/校验/凭据/确认）→ 系统未改动，沿原因分类。调用方不需要再 Abort。
func (in *Installer) Update(ctx context.Context, task *registry.Task, req UpdateRequest) (*UpdateResult, error) {
	env, err := in.updateEnv()
	if err != nil {
		return nil, err
	}
	plan, err := in.PlanUpdate(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &UpdateResult{Component: plan.Component, From: plan.From, To: plan.To, Path: plan.Decision.Path, Decision: plan.Decision, Warnings: plan.Warnings, Pruned: []string{}, Plan: plan}
	if plan.Action == "keep" {
		res.Outcome = OutcomeAlreadyCurrent
		return res, nil
	}
	if cur, cerr := in.curVersion(plan.Component); cerr != nil || cur != plan.From {
		return nil, &Error{Kind: KindPrecondition, Msg: "current 指向的版本（" + cur + "）与 state.json 记录的 " + plan.From + " 不一致，拒绝升级", SystemState: "unchanged",
			Hint: "lingedge doctor 查看详情；确认现状后修复 current 链接或 state.json", Details: map[string]any{"code": "precondition.state_mismatch"}}
	}
	// 清单预告已经表明含不可逆迁移：在做任何事之前就要求确认（AC4）。
	accepted, err := in.confirmIrreversible(env, plan.Decision, req, plan.Component, plan.From, plan.To, nil)
	if err != nil {
		return nil, err
	}
	x := &updateExec{in: in, env: env, plan: plan, task: task, decision: plan.Decision, accepted: accepted, hasDB: plan.Component == "server" && plan.Migrations.DBInitialized,
		warnings: append([]Warning(nil), plan.Warnings...)}
	// 迁移凭据（AC9）：非交互且无凭据 → 退出码 3，此时尚未改动任何东西（journal 都还没开）。
	if x.hasDB {
		if env.DB == nil {
			return nil, newErr(KindInternal, "本次调用没有装配迁移凭据通道")
		}
		conn, cerr := env.DB(ctx, !req.NonInteractive)
		if cerr != nil {
			return nil, credentialsError(cerr)
		}
		x.conn = conn
		defer conn.Close()
	}
	j, err := task.OpenTx(ctx)
	if err != nil {
		return nil, err
	}
	x.tx = task.TxID()
	x.j = Journal(j)
	if in.d.WrapJournal != nil {
		x.j = in.d.WrapJournal(x.j)
	}
	x.udo = &detailUndoer{inner: in.newUndoer(), sink: func(s string) { x.details = append(x.details, s) }}
	if err := x.sequence(ctx); err != nil {
		if x.committed {
			x.warnings = append(x.warnings, Warning{Code: "journal.finish_failed", Message: "升级已提交，但事务日志未能收尾：" + err.Error() + "；请运行 lingedge recover --resume"})
			x.j.Close()
			return x.result(res), nil
		}
		return nil, x.fail(ctx, err)
	}
	return x.result(res), nil
}

func credentialsError(err error) error {
	if errors.Is(err, backup.ErrNoCredentials) {
		return &Error{Kind: KindPrecondition, Msg: "缺少数据库迁移账号凭据，且当前不是交互终端", SystemState: "unchanged",
			Hint: "任选其一：环境变量 " + backup.EnvDBPass + " 或 " + backup.EnvPassFile + "；先执行 `sudo lingedge-server migrate credential init`（写入 " + backup.DefaultMigrateYAML +
				"，root:root 0400）；在终端交互运行。此时尚未改动任何东西", Details: map[string]any{"code": "precondition.db_credentials"}}
	}
	return &Error{Kind: KindPrecondition, Msg: "无法确定数据库连接：" + err.Error(), SystemState: "unchanged", Details: map[string]any{"code": "precondition.db_credentials"}}
}

func (in *Installer) curVersion(comp string) (string, error) {
	env, err := in.updateEnv()
	if err != nil || env.Rollback == nil {
		return "", errors.New("未装配回滚执行器")
	}
	return env.Rollback.CurrentVersion(comp)
}

func (x *updateExec) result(res *UpdateResult) *UpdateResult {
	res.Outcome, res.Tx, res.Backup, res.BackupVerified = OutcomeUpdated, x.tx, x.backupPth, x.verified
	res.Decision, res.Path = x.decision, x.decision.Path
	res.SchemaFrom, res.SchemaTo, res.Health, res.Pruned, res.Checks = x.pre, x.tgt, x.health, x.pruned, x.checks
	if res.Pruned == nil {
		res.Pruned = []string{}
	}
	res.Warnings = x.warnings
	return res
}

func (x *updateExec) newBinVirtual() string { return releaseBin(x.plan.To) }

func (x *updateExec) requestAction() registry.UndoAction {
	return registry.UndoAction{Action: ActUpdateRequest, Extra: map[string]string{"v": "1", "component": x.plan.Component, "from": x.plan.From, "to": x.plan.To,
		"path": x.decision.Path, "classes": csv(x.decision.Classes), "db": strconv.FormatBool(x.hasDB), "units": unitsCSV(x.plan.units)}}
}

func unitsCSV(us []service.UnitName) string {
	ss := make([]string, len(us))
	for i, u := range us {
		ss[i] = string(u)
	}
	return strings.Join(ss, ",")
}

// ustep 是每一步的统一外壳：先在 journal 写 begin（含 undo，fsync），再执行副作用，最后写 done。8–11、13 是不可被 SIGINT 打断的原子段（服务已停/已切换时不能半途退出）。
func (x *updateExec) ustep(ctx context.Context, n int, quiet bool, plan func() (registry.Undo, error), do func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return &stepErr{step: n, err: err}
	}
	x.hook("before", n)
	var undo registry.Undo
	if plan != nil {
		u, err := plan()
		if err != nil {
			return &stepErr{step: n, err: err}
		}
		undo = u
	}
	if err := x.j.Begin(n, UpdateStepName(n), undo); err != nil {
		return &stepErr{step: n, err: err}
	}
	x.hook("begun", n)
	if !quiet {
		x.emit(EventStepStart, n, UpdateStepTitle(n))
	}
	bctx := ctx
	if (n >= UpStepStop && n <= UpStepStart) || n == UpStepCommit {
		bctx = context.WithoutCancel(ctx)
	}
	if do != nil {
		if err := do(bctx); err != nil {
			if !quiet {
				x.emit(EventStepFailed, n, err.Error())
			}
			return &stepErr{step: n, err: err}
		}
	}
	x.hook("effect", n)
	if err := x.j.Done(n); err != nil {
		return &stepErr{step: n, err: err}
	}
	x.hook("done", n)
	if !quiet {
		x.emit(EventStepDone, n, "")
	}
	return nil
}

func (x *updateExec) sequence(ctx context.Context) error {
	p := x.plan
	if err := x.ustep(ctx, UpStepLock, true, func() (registry.Undo, error) {
		return registry.Undo{Actions: []registry.UndoAction{x.requestAction()}}, nil
	}, nil); err != nil {
		return err
	}
	for _, n := range []int{UpStepManifest, UpStepCompat, UpStepPlan} { // 只读，规划阶段已完成：在 journal 里补记（无副作用）
		if err := x.ustep(ctx, n, true, nil, nil); err != nil {
			return err
		}
	}
	x.in.ev.emit(Event{Type: EventPhase, Op: "update", Total: UpTotalSteps, Phase: PhaseFetch, Message: "取包与校验"})
	if err := x.ustep(ctx, UpStepDownload, false, x.planDownload, x.doDownload); err != nil {
		return err
	}
	finish := func() error {
		if err := x.ustep(ctx, UpStepBackup, false, nil, x.doBackup); err != nil {
			return err
		}
		if err := x.ustep(ctx, UpStepMigratePlan, false, nil, x.doMigratePlan); err != nil {
			return err
		}
		x.in.ev.emit(Event{Type: EventPhase, Op: "update", Total: UpTotalSteps, Phase: PhaseActivate, Message: "切换"})
		if err := x.ustep(ctx, UpStepStop, false, x.planStop, x.doStop); err != nil {
			return err
		}
		if err := x.ustep(ctx, UpStepMigrateUp, false, x.planMigrateUp, x.doMigrateUp); err != nil {
			return err
		}
		if err := x.ustep(ctx, UpStepSwitch, false, x.planSwitch, x.doSwitch); err != nil {
			return err
		}
		if err := x.ustep(ctx, UpStepStart, false, x.planStart, x.doStart); err != nil {
			return err
		}
		return x.ustep(ctx, UpStepHealth, false, nil, x.doHealth)
	}
	if err := x.acq.Finish(func(*downloader.Acquired) error { return finish() }); err != nil {
		var se *stepErr
		if !errors.As(err, &se) { // Finish 自身的 Commit（推进 sequence）失败
			return &stepErr{step: UpStepHealth, err: err}
		}
		return err
	}
	x.in.ev.emit(Event{Type: EventPhase, Op: "update", Total: UpTotalSteps, Phase: PhaseCommit, Message: "提交"})
	if err := x.ustep(ctx, UpStepCommit, false, nil, x.doCommit); err != nil {
		return err
	}
	_ = p
	return x.finishTx()
}

// ---- 步骤 5：取包、验证、落地新 release ----

func (x *updateExec) stagingDir() string {
	return filepath.Join(x.in.d.Reg.L.StagingDir(), x.tx+"-"+x.plan.Component)
}

func (x *updateExec) releaseVirtual() string { return rollback.ReleaseDir(x.plan.Component, x.plan.To) }

func (x *updateExec) planDownload() (registry.Undo, error) {
	rel := x.releaseVirtual()
	_, relErr := os.Lstat(x.in.d.hostPath(rel))
	return registry.Undo{Actions: []registry.UndoAction{
		{Action: actRemoveStaging, Path: x.stagingDir(), Created: true},
		{Action: actRemoveRelease, Path: rel + ".partial", Created: true},
		{Action: actRemoveRelease, Path: rel, Created: relErr != nil},
	}}, nil
}

func (x *updateExec) doDownload(ctx context.Context) error {
	in, p := x.in, x.plan
	v, err := in.verifier(true)
	if err != nil {
		return err
	}
	var src downloader.Params
	if p.req.Offline != "" {
		src.BundlePath = p.req.Offline
	} else {
		url, err := in.d.Config.ManifestURL(p.Channel)
		if err != nil {
			return err
		}
		cl, err := in.d.NewClient(in.d.Config.DownloaderConfig(filepath.Join(in.d.Reg.L.Root, "downloads"), true))
		if err != nil {
			return err
		}
		defer cl.Close()
		src.Online = &downloader.OnlineSource{Client: cl, ManifestURL: url}
	}
	dp := src
	dp.Verifier, dp.KeysDir = v, filepath.Join(in.d.Reg.L.TrustDir(), "keys.d")
	dp.Channel, dp.AllowExpired = p.Channel, p.req.AllowExpired
	dp.Component, dp.Version, dp.Variant, dp.Platform = p.Component, p.pc.Version, p.pc.Variant, p.probe.System.OS+"-"+p.probe.System.Arch
	dp.StagingRoot, dp.TxID = in.d.Reg.L.StagingDir(), x.tx+"-"+p.Component
	dp.DeferCommit = true
	dp.Executable = p.spec.Executables(p.pc.Version, p.pc.Variant)
	a, err := downloader.Acquire(ctx, dp)
	if err != nil {
		return err
	}
	if a.Artifact.SHA256 != p.pc.SHA256 || a.Version != p.pc.Version {
		_ = a.Discard()
		return manifestChanged(p.Component)
	}
	x.acq = a
	if err := in.extractRelease(p.Component, p.pc.Version, a); err != nil {
		_ = a.Discard()
		x.acq = nil
		return err
	}
	return nil
}

// ---- 步骤 6：备份 ----

func (x *updateExec) doBackup(ctx context.Context) error {
	p, env := x.plan, x.env
	if !p.Backup.Planned {
		x.emit(EventStepSkipped, UpStepBackup, p.Backup.Reason)
		return nil
	}
	if env.Backup == nil {
		return errors.New("未装配备份服务")
	}
	var pass []byte
	if env.BackupPassphrase != nil {
		b, err := env.BackupPassphrase(ctx)
		if err != nil {
			return fmt.Errorf("无法取得备份口令：%w", err)
		}
		pass = b
	}
	co := backup.CreateOptions{Component: p.Component, Version: p.From, OutputDir: p.Backup.Dir, Encrypt: len(pass) > 0, Passphrase: pass, Purpose: backup.PurposePreUpgrade}
	if p.Component == "server" {
		co.DB = x.conn
	}
	r, err := env.Backup.Create(ctx, co)
	if err != nil {
		return fmt.Errorf("升级前备份失败（升级中止，系统未改动）：%w", err)
	}
	// 写完立即回读校验（Create 已做一次）；这里再做一次独立的完整可读校验——含不可逆迁移时这是硬性要求，其余场景同样做（廉价且让「备份可恢复」成为已知事实）。
	if _, err := env.Backup.Verify(r.Path, pass); err != nil {
		return fmt.Errorf("升级前备份回读校验失败（升级中止，系统未改动）：%w", err)
	}
	x.backupPth, x.backupEnc, x.verified = r.Path, len(pass) > 0, true
	for _, w := range r.Warnings {
		x.warn(Warning{Code: w.Code, Component: p.Component, Step: UpStepBackup, Message: w.Message})
	}
	return nil
}

// ---- 步骤 7：migrate plan ----

func (x *updateExec) doMigratePlan(ctx context.Context) error {
	if !x.hasDB {
		x.emit(EventStepSkipped, UpStepMigratePlan, "该组件没有数据库迁移")
		return nil
	}
	mp, err := x.env.Mig.Plan(ctx, x.in.d.hostPath(x.newBinVirtual()), x.conn)
	if err != nil {
		return err
	}
	x.mplan = mp
	if mp.DBVersion != nil {
		x.pre = *mp.DBVersion
	}
	x.tgt = x.pre
	if mp.Count > 0 {
		x.tgt = mp.LatestKnown
	}
	// 与清单预告取更严格者（§17.2）。
	x.decision = rollback.Decide(rollback.MergeClasses(x.plan.Migrations.Classes, mp.Classes()), true)
	x.plan.Migrations.Source, x.plan.Migrations.Items = "migrate_plan", mp.Migrations
	x.plan.Migrations.Classes, x.plan.Migrations.HasIrreversible = x.decision.Classes, x.decision.Irreversible
	x.plan.Migrations.SchemaFrom, x.plan.Migrations.SchemaTo = x.pre, x.tgt
	x.plan.Decision = x.decision
	if x.decision.NeedConfirm && !x.accepted {
		items := make([]string, 0, len(mp.Migrations))
		for _, m := range mp.Migrations {
			items = append(items, fmt.Sprintf("%06d_%s（%s）", m.Version, m.Name, m.Class))
		}
		ok, err := x.in.confirmIrreversible(x.env, x.decision, x.plan.req, x.plan.Component, x.plan.From, x.plan.To, items)
		if err != nil {
			return err
		}
		x.accepted = ok
	}
	return nil
}

// ---- 步骤 8：停服务 ----

func (x *updateExec) planStop() (registry.Undo, error) {
	ctx := context.Background()
	for _, u := range x.plan.units {
		st, err := x.in.sys.IsActive(ctx, u)
		if err != nil {
			return registry.Undo{}, err
		}
		if st == "active" || st == "activating" {
			x.wasActive = true
		}
	}
	if len(x.plan.units) == 0 {
		return registry.Undo{}, nil
	}
	return registry.Undo{Actions: []registry.UndoAction{{Action: ActUpdateStartUnits, Extra: map[string]string{"units": unitsCSV(x.plan.units), "was_active": strconv.FormatBool(x.wasActive)}}}}, nil
}

func (x *updateExec) doStop(ctx context.Context) error {
	if len(x.plan.units) == 0 || !x.wasActive {
		x.emit(EventStepSkipped, UpStepStop, "没有需要停止的服务")
		return nil
	}
	return x.in.sys.Stop(ctx, x.plan.units...)
}

// ---- 步骤 9：migrate up ----

func (x *updateExec) planMigrateUp() (registry.Undo, error) {
	if !x.hasDB || x.mplan == nil || x.mplan.Count == 0 {
		return registry.Undo{}, nil
	}
	return registry.Undo{Actions: []registry.UndoAction{{Action: ActUpdateDBRevert, Extra: map[string]string{
		"component": x.plan.Component, "path": x.decision.Path, "new_bin": x.newBinVirtual(), "pre": strconv.FormatInt(x.pre, 10), "target": strconv.FormatInt(x.tgt, 10),
		"backup": x.backupPth, "encrypted": strconv.FormatBool(x.backupEnc)}}}}, nil
}

func (x *updateExec) doMigrateUp(ctx context.Context) error {
	if !x.hasDB || x.mplan == nil || x.mplan.Count == 0 {
		x.emit(EventStepSkipped, UpStepMigrateUp, "没有待执行的迁移")
		return nil
	}
	r, err := x.env.Mig.Up(ctx, x.in.d.hostPath(x.newBinVirtual()), x.conn, x.accepted && x.decision.Irreversible)
	if err != nil {
		return err
	}
	if r.ToVersion != x.tgt {
		return fmt.Errorf("migrate up 之后数据库版本为 %d，与计划的 %d 不一致", r.ToVersion, x.tgt)
	}
	return nil
}

// ---- 步骤 10：切换 current ----

func (x *updateExec) planSwitch() (registry.Undo, error) {
	return registry.Undo{Actions: []registry.UndoAction{{Action: ActUpdateSwitchCurrent, Extra: map[string]string{"component": x.plan.Component, "to": x.plan.From}}}}, nil
}

func (x *updateExec) doSwitch(context.Context) error {
	_, err := x.env.Rollback.SwitchCurrent(x.plan.Component, x.plan.To)
	return err
}

// ---- 步骤 11/12：启动与健康检查 ----

func (x *updateExec) runtime() *Runtime {
	p := x.plan
	rt := &Runtime{Component: p.Component, Version: p.To, Variant: p.pc.Variant, Units: p.units, Root: x.in.d.Root, SetupListen: x.in.serverHTTPListen(),
		start: func(ctx context.Context, units ...service.UnitName) error {
			_, err := x.in.sys.Start(ctx, units...)
			return err
		}}
	for _, pp := range p.Resolved.Ports {
		if pp.Component == p.Component {
			rt.Ports = append(rt.Ports, pp)
		}
	}
	if d := p.probe.System.Deps["nodejs"]; d.Detected {
		n := NodeRuntime{Detected: true, Version: d.Version, Bin: "node", InPath: true}
		if filepath.IsAbs(d.Source) {
			n.Bin, n.InPath = d.Source, false
		}
		rt.Node = n
	}
	return rt
}

func (x *updateExec) planStart() (registry.Undo, error) {
	var u registry.Undo
	if !x.wasActive {
		return u, nil
	}
	for i := len(x.plan.units) - 1; i >= 0; i-- {
		u.Actions = append(u.Actions, registry.UndoAction{Action: "systemctl_stop", Path: string(x.plan.units[i]), Created: true})
	}
	return u, nil
}

func (x *updateExec) doStart(ctx context.Context) error {
	if !x.wasActive {
		x.emit(EventStepSkipped, UpStepStart, "升级前服务未运行：不启动")
		return nil
	}
	if err := x.plan.spec.Start(ctx, x.runtime()); err != nil {
		return fmt.Errorf("启动 %s 失败: %w", x.plan.Component, err)
	}
	x.started = true
	return nil
}

func (x *updateExec) doHealth(ctx context.Context) error {
	if !x.started {
		x.emit(EventStepSkipped, UpStepHealth, "服务未启动，跳过健康检查")
		return nil
	}
	in := x.in
	timeout, err := in.d.Config.HealthTimeout()
	if err != nil {
		return err
	}
	interval, err := in.d.Config.HealthInterval()
	if err != nil {
		return err
	}
	rt := x.runtime()
	herr := waitHealth(ctx, timeout, interval, func(cx context.Context) error {
		return in.healthOnce(cx, x.plan.spec, rt, x.hasDB, x.conn, x.in.d.hostPath(x.newBinVirtual()), x.tgt)
	})
	if herr == nil {
		x.health = "healthy"
		if x.hasDB {
			x.health = "healthz_ready+schema"
		}
		return nil
	}
	if isCancel(herr) || !x.plan.req.NoRollback {
		return fmt.Errorf("%s 健康检查失败: %w", x.plan.Component, herr)
	}
	x.healthErr = fmt.Errorf("%s 健康检查失败: %w", x.plan.Component, herr)
	x.health = "failed"
	x.warn(Warning{Code: "health.failed_kept", Component: x.plan.Component, Step: UpStepHealth, Message: x.healthErr.Error() + "（--no-rollback：保留现场；可用 lingedge rollback 回退）"})
	return nil
}

// healthOnce 做一次健康探测：组件自己的 Health；server 且数据库已初始化时再要求 /healthz/ready 与「数据库迁移版本 = 期望」（§17.3 步骤 12）。
func (in *Installer) healthOnce(ctx context.Context, spec ComponentSpec, rt *Runtime, hasDB bool, conn *backup.DBConn, bin string, wantSchema int64) error {
	if err := spec.Health(ctx, rt); err != nil {
		return err
	}
	if !hasDB {
		return nil
	}
	if err := HTTPOK(ctx, strings.TrimSuffix(ServerHealthURL(rt.SetupListen), "/healthz")+"/healthz/ready", 3e9); err != nil {
		return err
	}
	env := in.d.Update
	if env == nil || env.Mig == nil || conn == nil || wantSchema <= 0 {
		return nil
	}
	st, err := env.Mig.Status(ctx, bin, conn)
	if err != nil {
		return err
	}
	if st.Problem != "" || st.DBVersion != wantSchema {
		return fmt.Errorf("数据库迁移版本为 %d，期望 %d%s", st.DBVersion, wantSchema, map[bool]string{true: "（" + st.Problem + "）", false: ""}[st.Problem != ""])
	}
	return nil
}

// ---- 步骤 13：提交 ----

func (x *updateExec) doCommit(ctx context.Context) error {
	p := x.plan
	now := x.in.d.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	rec := &registry.UpgradeRecord{From: p.From, At: now, Tx: x.tx, Classes: x.decision.Classes, Irreversible: x.decision.Irreversible, Path: x.decision.Path,
		SchemaFrom: x.pre, SchemaTo: x.tgt, Backup: x.backupPth, BackupEncrypted: x.backupEnc}
	if rec.Classes == nil {
		rec.Classes = []string{}
	}
	err := x.in.d.Reg.UpdateState(ctx, func(s *registry.State) error {
		c, ok := s.Components[p.Component]
		if !ok {
			return fmt.Errorf("state.json 里已没有组件 %s", p.Component)
		}
		c.PrevVersion, c.Version, c.ReleaseDir, c.LastTx, c.Upgrade = p.From, p.To, rollback.ReleaseDir(p.Component, p.To), x.tx, rec
		if x.healthErr != nil {
			c.State = "failed"
		}
		s.Components[p.Component] = c
		return nil
	})
	if err != nil {
		return err
	}
	x.committed = true
	return nil
}

// finishTx 是提交点之后的收尾：journal commit/archive、清理超出保留数的旧 release（只在升级确认成功之后，AC7）、doctor 子集（只读报告）。
func (x *updateExec) finishTx() error {
	if err := x.j.Commit(); err != nil {
		x.warn(Warning{Code: "journal.commit_failed", Message: "升级已完成，但事务日志未能收尾；请运行 lingedge recover --resume：" + err.Error()})
	} else if err := x.j.Archive(); err != nil {
		x.warn(Warning{Code: "journal.archive_failed", Message: "事务日志归档失败（已 commit，下次运行会补做）：" + err.Error()})
	}
	_ = removeAll(x.stagingDir())
	if x.healthErr == nil {
		x.pruneAfterSuccess()
	}
	if x.env.PostChecks != nil {
		r := x.env.PostChecks(context.Background(), x.plan.Component)
		x.checks = &r
		if r.Fail > 0 {
			x.warn(Warning{Code: "doctor.failed_after_update", Component: x.plan.Component, Message: fmt.Sprintf("升级完成，但 doctor 子集有 %d 项失败：%s", r.Fail, strings.Join(r.Problems, "；"))})
		}
	}
	return nil
}

func (x *updateExec) pruneAfterSuccess() {
	rm, err := x.env.Rollback.PruneReleases(x.plan.Component, x.in.d.Config.KeepReleases(), x.plan.To, x.plan.From)
	x.pruned = rm
	if err != nil {
		x.warn(Warning{Code: "update.prune_failed", Component: x.plan.Component, Message: "升级成功，但清理旧 release 失败（不影响运行）：" + err.Error()})
	}
}
