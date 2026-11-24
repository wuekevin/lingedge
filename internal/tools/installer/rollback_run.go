package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/rollback"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// 手动回滚事务的步骤号（journal）。
const (
	RbStepRequest = 1 // 请求记录
	RbStepSafety  = 2 // 回滚前安全备份（会改动数据库的路径）
	RbStepStop    = 3 // 停服务
	RbStepSwitch  = 4 // 原子切换 current
	RbStepDB      = 5 // 回退数据库（migrate down / 恢复备份 + lineage bump）
	RbStepStart   = 6 // 启动
	RbStepHealth  = 7 // 健康检查
	RbStepCommit  = 8 // 保存状态（提交点）
	RbTotalSteps  = 8
)

var rollbackStepNames = [RbTotalSteps + 1]string{"", "rollback_request", "safety_backup", "stop_service", "switch_current", "revert_database", "start_service", "health_check", "confirm_commit"}
var rollbackStepTitles = [RbTotalSteps + 1]string{"", "记录回滚请求", "回滚前安全备份", "停止服务", "原子切换 current", "回退数据库", "启动服务", "健康检查", "保存状态（提交点）"}

// RollbackStepName 返回手动回滚事务某一步的稳定名称。
func RollbackStepName(n int) string {
	if n < 1 || n > RbTotalSteps {
		return ""
	}
	return rollbackStepNames[n]
}

// RollbackRequest 是 `lingedge rollback <组件> [--to V] [--from-backup F]` 的请求。
type RollbackRequest struct {
	Component  string
	To         string
	FromBackup string
	DryRun     bool
}

// RollbackPlan 是手动回滚的计划（--dry-run 与真实回滚同源）。
type RollbackPlan struct {
	Component string `json:"component"`
	From      string `json:"from"`
	To        string `json:"to"`
	// Path：binary | migrate_down | binary_expand | restore_backup（registry.Path*）。
	Path string `json:"path"`
	// Source：upgrade_record（上一次升级的记录）| explicit_to（--to）| backup（--from-backup）。
	Source       string    `json:"source"`
	Backup       string    `json:"backup,omitempty"`
	SchemaFrom   int64     `json:"schema_from,omitempty"`
	SchemaTo     int64     `json:"schema_to,omitempty"`
	Irreversible bool      `json:"irreversible"`
	Promise      string    `json:"promise"`
	SafetyBackup bool      `json:"safety_backup"`
	Destructive  bool      `json:"destructive"`
	Warnings     []Warning `json:"warnings"`
	Steps        []string  `json:"steps"`

	hasDB   bool
	units   []service.UnitName
	rec     registry.Component
	passArg []byte
}

// RollbackResult 是手动回滚的结果。
type RollbackResult struct {
	Plan     *RollbackPlan  `json:"plan"`
	Outcome  string         `json:"outcome"` // planned | rolled_back
	Tx       string         `json:"tx,omitempty"`
	Safety   string         `json:"safety_backup,omitempty"`
	Report   RollbackReport `json:"report"`
	Warnings []Warning      `json:"warnings"`
}

func rbSteps() []string {
	out := make([]string, 0, RbTotalSteps)
	for n := 1; n <= RbTotalSteps; n++ {
		out = append(out, rollbackStepNames[n])
	}
	return out
}

func preErr(code, msg, hint string) *Error {
	return &Error{Kind: KindPrecondition, Msg: msg, Hint: hint, SystemState: "unchanged", Details: map[string]any{"code": code}}
}

// PlanRollback 只读求出回滚计划。含不可逆迁移的上一次升级 → 拒绝简单回退并提示 --from-backup（退出码 3，未改动任何东西）。
func (in *Installer) PlanRollback(ctx context.Context, req RollbackRequest) (*RollbackPlan, error) {
	env, err := in.updateEnv()
	if err != nil {
		return nil, err
	}
	if !registry.ValidComponent(req.Component) {
		return nil, newErr(KindSettings, "组件名不合法")
	}
	st, _, err := in.d.Reg.LoadState()
	if err != nil {
		return nil, err
	}
	rec, ok := st.Components[req.Component]
	if !ok {
		return nil, &Error{Kind: KindNotInstalled, Msg: "组件 " + req.Component + " 未安装", Hint: "lingedge list 查看已安装的组件", SystemState: "unchanged"}
	}
	if _, ok := in.d.Specs[req.Component]; !ok {
		return nil, &Error{Kind: KindSpecMissing, Msg: "组件 " + req.Component + " 的规格尚未交付，无法回滚", SystemState: "unchanged"}
	}
	cur, err := in.curVersion(req.Component)
	if err != nil || cur != rec.Version {
		return nil, preErr("precondition.state_mismatch", "current 指向的版本（"+cur+"）与 state.json 记录的 "+rec.Version+" 不一致，拒绝回滚", "lingedge doctor 查看详情")
	}
	hasDB := req.Component == "server" && in.serverInitialized()
	p := &RollbackPlan{Component: req.Component, From: rec.Version, Warnings: []Warning{}, Steps: rbSteps(), hasDB: hasDB, units: service.RunUnits(req.Component), rec: rec}
	u := rec.Upgrade
	switch {
	case req.FromBackup != "":
		p.Source, p.Backup, p.Path, p.Irreversible = "backup", req.FromBackup, registry.PathRestoreBackup, true
		var pass []byte
		if enc, eerr := backup.IsEncrypted(req.FromBackup); eerr != nil {
			return nil, preErr("precondition.backup_unreadable", "无法读取备份文件 "+req.FromBackup+"："+eerr.Error(), "")
		} else if enc {
			if env.BackupPassphrase == nil {
				return nil, preErr("precondition.passphrase_required", "备份是加密的，需要口令", "设置环境变量 LINGEDGE_BACKUP_PASSPHRASE_FILE 指向口令文件")
			}
			if pass, err = env.BackupPassphrase(ctx); err != nil || len(pass) == 0 {
				return nil, preErr("precondition.passphrase_required", "备份是加密的，需要口令", "设置环境变量 LINGEDGE_BACKUP_PASSPHRASE_FILE 指向口令文件")
			}
			p.passArg = pass
		}
		ar, err := backup.ReadManifest(req.FromBackup, pass)
		if err != nil {
			return nil, &Error{Kind: KindStepFailed, Msg: "备份校验失败", Cause: err, SystemState: "unchanged"}
		}
		if ar.Manifest.Component != req.Component {
			return nil, preErr("precondition.backup_component", "备份的组件是 "+ar.Manifest.Component+"，与 "+req.Component+" 不一致", "")
		}
		p.To, p.SchemaFrom = ar.Manifest.Version, ar.Manifest.SchemaVersion
		if req.To != "" {
			p.To = req.To
		}
		if u != nil {
			p.SchemaTo = u.SchemaTo
		}
		p.Promise = rollback.PromiseIrreversible
		p.Destructive = true
	default:
		p.To = req.To
		if p.To == "" {
			p.To = rec.PrevVersion
		}
		if p.To == "" {
			return nil, preErr("precondition.no_previous", "没有可回滚的上一版本（state.json 里没有 prev_version）", "用 --to <版本> 指定仍保留的 release，或 --from-backup <备份文件>")
		}
		p.Source = "upgrade_record"
		if req.To != "" {
			p.Source = "explicit_to"
		}
		if refuse, hint := rollback.RefusesPlainRollback(u); refuse && (p.To == u.From || req.To == "") {
			msg := "上一次升级含不可逆迁移，拒绝简单回退二进制（旧二进制与升级后的数据库结构不兼容）。回滚路径是「恢复升级前备份 + 旧版本二进制」，备份点之后产生的数据将丢失"
			h := "使用 lingedge rollback " + req.Component + " --from-backup <升级前备份文件>"
			if u.Backup != "" {
				h = "使用 lingedge rollback " + req.Component + " --from-backup " + u.Backup
			}
			_ = hint
			return nil, preErr("precondition.irreversible_rollback", msg, h)
		}
		switch {
		case u != nil && p.To == u.From:
			p.Path, p.SchemaFrom, p.SchemaTo = u.Path, u.SchemaFrom, u.SchemaTo
			p.Promise = map[string]string{registry.PathBinary: rollback.PromiseBinary, registry.PathMigrateDown: rollback.PromiseReversible, registry.PathBinaryExpand: rollback.PromiseExpand}[p.Path]
			if p.Path == registry.PathBinaryExpand {
				p.Warnings = append(p.Warnings, Warning{Code: "rollback.expand_unpromised", Component: req.Component, Step: RbStepSwitch,
					Message: "上一次升级含 expand 迁移：只有「升级失败的即时回滚」才被承诺；新版本已运行并写入新列/新枚举值之后回退二进制依赖 N-1 兼容性（ADR-016 §10）。如需确保一致请改用 --from-backup"})
			}
		case hasDB:
			return nil, preErr("precondition.rollback_target", "无法确认 "+p.To+" 与当前数据库 schema 兼容（它不是上一次升级的来源版本）", "用 --from-backup <备份文件> 恢复到该版本对应的备份")
		default:
			p.Path, p.Promise = registry.PathBinary, rollback.PromiseBinary
		}
	}
	if v, perr := manifest.ParseVersion(p.To); perr != nil || v.String() != p.To {
		return nil, preErr("precondition.version", "目标版本 "+p.To+" 不是严格 SemVer", "")
	}
	if p.To == p.From && p.Path != registry.PathRestoreBackup {
		return nil, preErr("precondition.same_version", "已经是版本 "+p.To, "")
	}
	if _, serr := os.Stat(in.d.hostPath(rollback.ReleaseDir(req.Component, p.To))); serr != nil {
		return nil, preErr("precondition.release_missing", "版本 "+p.To+" 的 release 目录已不存在（可能已被保留策略清理）", "重新 lingedge update/install 该版本，或确认 /opt/lingedge/"+req.Component+"/releases/ 下仍保留的版本后用 --to 指定")
	}
	p.SafetyBackup = hasDB && (p.Path == registry.PathMigrateDown || p.Path == registry.PathRestoreBackup)
	if p.Path == registry.PathRestoreBackup && !hasDB && req.Component == "server" {
		p.Warnings = append(p.Warnings, Warning{Code: "rollback.setup_mode", Component: "server", Message: "server 处于 setup mode（没有数据库）：只恢复备份里的文件并切换二进制"})
	}
	return p, nil
}

type rbExec struct {
	in   *Installer
	env  *UpdateEnv
	plan *RollbackPlan
	task *registry.Task
	j    Journal
	tx   string
	udo  UndoApplier
	conn *backup.DBConn

	wasActive bool
	safety    string
	safetyEnc bool
	pre       int64 // 回滚前数据库 schema（撤销时的复核目标）
	rep       RollbackReport
	warnings  []Warning
	committed bool
}

func (x *rbExec) emit(t EventType, n int, msg string) {
	x.in.ev.emit(Event{Type: t, Op: "rollback", Total: RbTotalSteps, Step: n, Name: RollbackStepName(n), Component: x.plan.Component, Message: msg})
}

func (x *rbExec) ustep(ctx context.Context, n int, quiet bool, plan func() (registry.Undo, error), do func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return &stepErr{step: n, err: err}
	}
	if x.in.d.Hook != nil {
		x.in.d.Hook(fmt.Sprintf("before:%d", n))
	}
	var undo registry.Undo
	if plan != nil {
		u, err := plan()
		if err != nil {
			return &stepErr{step: n, err: err}
		}
		undo = u
	}
	if err := x.j.Begin(n, RollbackStepName(n), undo); err != nil {
		return &stepErr{step: n, err: err}
	}
	if x.in.d.Hook != nil {
		x.in.d.Hook(fmt.Sprintf("begun:%d", n))
	}
	if !quiet {
		x.emit(EventStepStart, n, rollbackStepTitles[n])
	}
	bctx := ctx
	if n >= RbStepStop && n != RbStepHealth {
		bctx = context.WithoutCancel(ctx)
	}
	if do != nil {
		if err := do(bctx); err != nil {
			x.emit(EventStepFailed, n, err.Error())
			return &stepErr{step: n, err: err}
		}
	}
	if x.in.d.Hook != nil {
		x.in.d.Hook(fmt.Sprintf("effect:%d", n))
	}
	if err := x.j.Done(n); err != nil {
		return &stepErr{step: n, err: err}
	}
	if x.in.d.Hook != nil {
		x.in.d.Hook(fmt.Sprintf("done:%d", n))
	}
	if !quiet {
		x.emit(EventStepDone, n, "")
	}
	return nil
}

// Rollback 执行手动回滚（tools-design §17.4）。task 是写命令入口的任务。失败时事务按 journal 撤销并复核（回到回滚之前的状态）：成功撤销 → KindRolledBack（退出码 10）；撤销失败 → KindRollbackFailed（12，journal 保留）。
func (in *Installer) Rollback(ctx context.Context, task *registry.Task, req RollbackRequest) (*RollbackResult, error) {
	env, err := in.updateEnv()
	if err != nil {
		return nil, err
	}
	plan, err := in.PlanRollback(ctx, req)
	if err != nil {
		return nil, err
	}
	x := &rbExec{in: in, env: env, plan: plan, task: task, warnings: append([]Warning(nil), plan.Warnings...)}
	if plan.hasDB {
		if env.DB == nil {
			return nil, newErr(KindInternal, "本次调用没有装配迁移凭据通道")
		}
		c, cerr := env.DB(ctx, true)
		if cerr != nil {
			return nil, credentialsError(cerr)
		}
		x.conn = c
		defer c.Close()
		ms, merr := env.Mig.Status(ctx, in.d.hostPath(releaseBin(plan.From)), c)
		if merr != nil || ms.Problem != "" {
			return nil, preErr("precondition.db_state", fmt.Sprintf("无法确认数据库迁移状态（%v）", merr), "lingedge-server migrate status 查看详情")
		}
		x.pre = ms.DBVersion
	}
	j, err := task.OpenTx(ctx)
	if err != nil {
		return nil, err
	}
	x.tx, x.j = task.TxID(), Journal(j)
	if in.d.WrapJournal != nil {
		x.j = in.d.WrapJournal(x.j)
	}
	x.udo = in.newUndoer()
	if err := x.sequence(ctx); err != nil {
		if x.committed {
			x.warnings = append(x.warnings, Warning{Code: "journal.finish_failed", Message: "回滚已提交，但事务日志未能收尾：" + err.Error() + "；请运行 lingedge recover --resume"})
			x.j.Close()
			return x.result(), nil
		}
		return nil, x.fail(ctx, err)
	}
	return x.result(), nil
}

func (x *rbExec) result() *RollbackResult {
	return &RollbackResult{Plan: x.plan, Outcome: "rolled_back", Tx: x.tx, Safety: x.safety, Report: x.rep, Warnings: x.warnings}
}

func (x *rbExec) requestAction() registry.UndoAction {
	p := x.plan
	return registry.UndoAction{Action: ActRollbackRequest, Extra: map[string]string{"v": "1", "component": p.Component, "restore_version": p.From, "other_version": p.To, "path": p.Path,
		"db": strconv.FormatBool(p.hasDB), "units": unitsCSV(p.units), "pre_schema": strconv.FormatInt(x.pre, 10)}}
}

func (x *rbExec) sequence(ctx context.Context) error {
	p, in := x.plan, x.in
	if err := x.ustep(ctx, RbStepRequest, true, func() (registry.Undo, error) {
		return registry.Undo{Actions: []registry.UndoAction{x.requestAction()}}, nil
	}, nil); err != nil {
		return err
	}
	if err := x.ustep(ctx, RbStepSafety, false, nil, x.doSafety); err != nil {
		return err
	}
	if err := x.ustep(ctx, RbStepStop, false, x.planStop, x.doStop); err != nil {
		return err
	}
	if err := x.ustep(ctx, RbStepSwitch, false, func() (registry.Undo, error) {
		return registry.Undo{Actions: []registry.UndoAction{{Action: ActUpdateSwitchCurrent, Extra: map[string]string{"component": p.Component, "to": p.From}}}}, nil
	}, func(context.Context) error { _, err := x.env.Rollback.SwitchCurrent(p.Component, p.To); return err }); err != nil {
		return err
	}
	if err := x.ustep(ctx, RbStepDB, false, nil, x.doDB); err != nil {
		return err
	}
	if err := x.ustep(ctx, RbStepStart, false, func() (registry.Undo, error) {
		var u registry.Undo
		if x.wasActive {
			for i := len(p.units) - 1; i >= 0; i-- {
				u.Actions = append(u.Actions, registry.UndoAction{Action: "systemctl_stop", Path: string(p.units[i]), Created: true})
			}
		}
		return u, nil
	}, x.doStart); err != nil {
		return err
	}
	if err := x.ustep(ctx, RbStepHealth, false, nil, x.doVerify); err != nil {
		return err
	}
	if err := x.ustep(ctx, RbStepCommit, false, nil, x.doCommit); err != nil {
		return err
	}
	if err := x.j.Commit(); err != nil {
		x.warnings = append(x.warnings, Warning{Code: "journal.commit_failed", Message: "回滚已完成，但事务日志未能收尾；请运行 lingedge recover --resume：" + err.Error()})
	} else if err := x.j.Archive(); err != nil {
		x.warnings = append(x.warnings, Warning{Code: "journal.archive_failed", Message: "事务日志归档失败（已 commit，下次运行会补做）：" + err.Error()})
	}
	_ = in
	return nil
}

func (x *rbExec) doSafety(ctx context.Context) error {
	if !x.plan.SafetyBackup {
		x.emit(EventStepSkipped, RbStepSafety, "不改动数据库：无需安全备份")
		return nil
	}
	var pass []byte
	if x.env.BackupPassphrase != nil {
		b, err := x.env.BackupPassphrase(ctx)
		if err != nil {
			return fmt.Errorf("无法取得备份口令：%w", err)
		}
		pass = b
	}
	r, err := x.env.Backup.Create(ctx, backup.CreateOptions{Component: x.plan.Component, Version: x.plan.From, OutputDir: x.plan.safetyDir(x.in), Encrypt: len(pass) > 0, Passphrase: pass,
		DB: x.conn, Purpose: backup.PurposePreRollback})
	if err != nil {
		return fmt.Errorf("无法创建回滚前安全备份（回滚中止，系统未改动）：%w", err)
	}
	if _, err := x.env.Backup.Verify(r.Path, pass); err != nil {
		return fmt.Errorf("回滚前安全备份回读校验失败（回滚中止，系统未改动）：%w", err)
	}
	x.safety, x.safetyEnc = r.Path, len(pass) > 0
	return nil
}

func (p *RollbackPlan) safetyDir(in *Installer) string { return in.updateBackupDir() }

func (x *rbExec) planStop() (registry.Undo, error) {
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
	if x.safety != "" {
		// 恢复到回滚前安全备份（文件、数据库、按需重启服务）：与 restore 的撤销是同一个动作与实现（Deps.RestoreSafety）。
		return registry.Undo{Actions: []registry.UndoAction{{Action: backup.ActRestoreSafety, Path: x.safety, Extra: map[string]string{
			backup.ExtraComponent: x.plan.Component, backup.ExtraUnits: unitsCSV(x.plan.units), backup.ExtraWasActive: strconv.FormatBool(x.wasActive), backup.ExtraEncrypted: strconv.FormatBool(x.safetyEnc)}}}}, nil
	}
	return registry.Undo{Actions: []registry.UndoAction{{Action: ActUpdateStartUnits, Extra: map[string]string{"units": unitsCSV(x.plan.units), "was_active": strconv.FormatBool(x.wasActive)}}}}, nil
}

func (x *rbExec) doStop(ctx context.Context) error {
	if len(x.plan.units) == 0 || !x.wasActive {
		return nil
	}
	return x.in.sys.Stop(ctx, x.plan.units...)
}

func (x *rbExec) doDB(ctx context.Context) error {
	p := x.plan
	if !p.hasDB || p.Path == registry.PathBinary || p.Path == registry.PathBinaryExpand {
		x.emit(EventStepSkipped, RbStepDB, "不改动数据库")
		return nil
	}
	out, err := x.env.Rollback.RevertDB(ctx, rollback.RevertParams{Component: p.Component, Path: p.Path, NewBin: releaseBin(p.From), PreSchema: p.SchemaFrom, TargetSchema: p.SchemaTo,
		Backup: p.Backup, BackupEncrypted: p.passArg != nil, AllowRestore: p.Path == registry.PathRestoreBackup, Force: p.Path == registry.PathRestoreBackup})
	if err != nil {
		return err
	}
	x.rep.Detail = append(x.rep.Detail, out.Detail)
	x.rep.Schema = out.SchemaAfter
	return nil
}

func (x *rbExec) doStart(ctx context.Context) error {
	if !x.wasActive {
		return nil
	}
	rt := &Runtime{Component: x.plan.Component, Version: x.plan.To, Units: x.plan.units, Root: x.in.d.Root, SetupListen: x.in.serverHTTPListen(),
		start: func(ctx context.Context, units ...service.UnitName) error {
			_, err := x.in.sys.Start(ctx, units...)
			return err
		}}
	if err := x.in.d.Specs[x.plan.Component].Start(ctx, rt); err != nil {
		return fmt.Errorf("启动 %s 失败: %w", x.plan.Component, err)
	}
	return nil
}

// doVerify 复核回滚结果（版本、schema、健康检查、doctor 子集）。
func (x *rbExec) doVerify(ctx context.Context) error {
	p := x.plan
	f := rollbackFacts{Component: p.Component, Version: p.To, NewVersion: p.From, Units: p.units, WasActive: x.wasActive, DB: p.hasDB, PreSchema: p.SchemaFrom, SkipState: true}
	rep := RollbackReport{Path: p.Path}
	if p.Path == registry.PathBinaryExpand {
		f.AltSchema = p.SchemaTo // expand：数据库保持在升级后的 schema（只切回旧二进制）；此时健康只要求存活探针（见 verifyRolledBack）
	}
	if err := x.in.verifyRolledBack(ctx, f, &rep); err != nil {
		return err
	}
	rep.Detail = x.rep.Detail
	x.rep = rep
	return nil
}

func (x *rbExec) doCommit(ctx context.Context) error {
	p := x.plan
	err := x.in.d.Reg.UpdateState(ctx, func(s *registry.State) error {
		c, ok := s.Components[p.Component]
		if !ok {
			return fmt.Errorf("state.json 里已没有组件 %s", p.Component)
		}
		c.Version, c.PrevVersion, c.Upgrade, c.ReleaseDir, c.LastTx = p.To, "", nil, rollback.ReleaseDir(p.Component, p.To), x.tx
		s.Components[p.Component] = c
		return nil
	})
	if err != nil {
		return err
	}
	x.committed = true
	return nil
}

// fail：撤销（回到回滚之前的状态：切回原版本、数据库恢复到回滚前安全备份、重启服务）并复核。
func (x *rbExec) fail(ctx context.Context, cause error) error {
	in, p := x.in, x.plan
	step, inner := 0, cause
	var se *stepErr
	if errors.As(cause, &se) {
		step, inner = se.step, se.err
	}
	rctx := context.WithoutCancel(ctx)
	in.ev.emit(Event{Type: EventInfo, Op: "rollback", Total: RbTotalSteps, Step: step, Message: "回滚失败，开始撤销（回到回滚之前的状态）", Code: "rollback.start"})
	touched := step >= RbStepStop
	var rep RollbackReport
	verify := func(vc context.Context) error { return nil }
	if touched {
		verify = func(vc context.Context) error {
			f := rollbackFacts{Component: p.Component, Version: p.From, NewVersion: p.To, Units: p.units, WasActive: x.wasActive, DB: p.hasDB, PreSchema: x.pre}
			return in.verifyRolledBack(vc, f, &rep)
		}
	}
	if rerr := in.rollbackJournalV(rctx, x.j, x.tx, x.udo, "rollback failed at step "+strconv.Itoa(step), func(c context.Context) error { return in.sys.DaemonReload(c) }, verify); rerr != nil {
		e := &Error{Kind: KindRollbackFailed, Msg: "回滚失败，且撤销未能完成，事务日志已保留：" + rerr.Error(), Cause: inner,
			Hint: "排查原因后运行 lingedge recover --rollback 重试；回滚前安全备份：" + orDash(x.safety) + "；journal：" + in.journalPath(x.tx), SystemState: "needs_manual",
			Details: map[string]any{"tx": x.tx, "failed_step": step, "failed_step_name": RollbackStepName(step), "safety_backup": x.safety}}
		x.j.Close()
		return e
	}
	det := map[string]any{"tx": x.tx, "failed_step": step, "failed_step_name": RollbackStepName(step), "safety_backup": x.safety, "restored": rep}
	switch {
	case isCancel(inner):
		return &Error{Kind: KindInterrupted, Msg: "已收到 SIGINT：已撤销回滚，系统恢复到回滚之前", Cause: inner, SystemState: "rolled_back", Details: det}
	case !touched:
		var ie *Error
		if errors.As(inner, &ie) {
			return ie
		}
		return &Error{Kind: KindStepFailed, Msg: "回滚在改动系统之前失败，系统未改动", Cause: inner, SystemState: "unchanged", Details: det}
	}
	return &Error{Kind: KindRolledBack, Msg: fmt.Sprintf("回滚失败（第 %d 步 %s），已撤销并恢复到回滚之前的状态（%s）", step, RollbackStepName(step), p.From), Cause: inner,
		Hint: "详见 cause；回滚前安全备份保留在 " + orDash(x.safety), SystemState: "rolled_back", Details: det}
}
