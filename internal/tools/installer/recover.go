package installer

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// RecoverMode 是 recover 的处理方式。
type RecoverMode string

// RecoverMode 取值。
const (
	RecoverRollback RecoverMode = "rollback" // 按 journal 逆序撤销（默认建议）
	RecoverResume   RecoverMode = "resume"   // 从第一个未完成的步骤续跑（仅当尚未进入激活阶段；已进入提交阶段则只做收尾）
)

// RecoverOptions 是 recover 的参数。
type RecoverOptions struct {
	Mode           RecoverMode
	Yes            bool
	NonInteractive bool
}

// TxRecovery 是一个事务的处理结果。
type TxRecovery struct {
	Tx         string   `json:"tx"`
	Command    string   `json:"command,omitempty"`
	Components []string `json:"components"`
	Action     string   `json:"action"` // rolled_back | resumed | finalized | discarded_empty
	Steps      int      `json:"steps_undone,omitempty"`
	Message    string   `json:"message,omitempty"`
	Result     *Result  `json:"-"`
}

// RecoverResult 是 recover 的结果。
type RecoverResult struct {
	Transactions []TxRecovery `json:"transactions"`
	// StaleTemps 是顺手回收的 state.json 临时文件数。
	StaleTemps int `json:"stale_temps_removed"`
}

// Unfinished 列出已死亡且未结束的事务（recover 的处理对象）。
func (in *Installer) Unfinished(ctx context.Context) ([]registry.TxInfo, error) {
	return in.d.Reg.Unfinished(ctx)
}

func (in *Installer) newUndoer() UndoApplier {
	svc := &service.Undoer{Root: in.d.Root, Prov: in.prov, Sys: in.sys}
	var u UndoApplier = &Undoer{Root: in.d.Root, Reg: in.d.Reg, Svc: svc, Web: in.webRunnerOrNil(), Safety: in.d.RestoreSafety, Update: in.d.Update, Sys: in.sys,
		Notice: func(code, msg string) { in.ev.emit(Event{Type: EventWarning, Code: code, Message: msg}) }}
	if in.d.WrapUndoer != nil {
		u = in.d.WrapUndoer(u)
	}
	return u
}

// Recover 处理全部未完成事务（lingedge recover [--resume|--rollback]）。没有未完成事务时返回空结果（并顺手归档已结束的 journal、回收临时文件）。
// 任一事务失败即停止并返回错误（其 journal 保留，其余事务未动）。
func (in *Installer) Recover(ctx context.Context, opt RecoverOptions) (*RecoverResult, error) {
	if opt.Mode != RecoverRollback && opt.Mode != RecoverResume {
		return nil, newErr(KindInternal, "recover 需要明确的模式")
	}
	res := &RecoverResult{Transactions: []TxRecovery{}}
	infos, err := in.d.Reg.Unfinished(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	for _, info := range infos {
		r, err := in.recoverOne(ctx, info, opt)
		if err != nil {
			return res, err
		}
		res.Transactions = append(res.Transactions, r)
	}
	if _, err := in.d.Reg.ArchiveFinished(ctx); err != nil {
		in.ev.emit(Event{Type: EventWarning, Code: "recover.archive", Message: "归档已结束的 journal 失败：" + err.Error()})
	}
	n, _ := in.d.Reg.CleanStaleTemps(ctx)
	res.StaleTemps = n
	return res, nil
}

func (in *Installer) recoverOne(ctx context.Context, info registry.TxInfo, opt RecoverOptions) (TxRecovery, error) {
	out := TxRecovery{Tx: info.ID, Command: info.Command, Components: info.Components}
	if out.Components == nil {
		out.Components = []string{}
	}
	switch info.Status {
	case "empty":
		if err := in.d.Reg.DiscardEmpty(ctx, info.ID); err != nil {
			return out, err
		}
		out.Action, out.Message = "discarded_empty", "事务在写入 open 记录前被中断，没有任何副作用，已丢弃"
		return out, nil
	case "corrupt":
		return out, &Error{Kind: KindRecover, Msg: "事务日志 " + info.ID + " 中间行损坏（" + info.Problem + "），无法自动恢复",
			Hint: "不要删除 journal：保留它并联系支持；journal 路径 " + info.Path, SystemState: "needs_manual", Details: map[string]any{"tx": info.ID}}
	}
	task, tx, err := in.d.Reg.BeginRecovery(ctx, info.ID)
	if err != nil {
		return out, err
	}
	defer task.Release()
	jr := task.Journal()
	var j Journal = jr
	if in.d.WrapJournal != nil {
		j = in.d.WrapJournal(j)
	}
	udo := in.newUndoer()
	rctx := context.WithoutCancel(ctx)

	if k := txKind(tx); k == "update" || k == "rollback" {
		return in.recoverUpdate(rctx, j, tx, udo, opt, out, k)
	}
	if opt.Mode == RecoverRollback {
		n := len(tx.UndoPlan())
		if err := in.rollbackJournal(rctx, j, info.ID, udo, "recover --rollback", func(c context.Context) error { return in.sys.DaemonReload(c) }); err != nil {
			return out, &Error{Kind: KindRollbackFailed, Msg: "回滚未完成，事务日志已保留：" + err.Error(), Cause: err, SystemState: "needs_manual",
				Hint: "排查原因后重新运行 lingedge recover --rollback（撤销是幂等的，会从未完成处继续）", Details: map[string]any{"tx": info.ID}}
		}
		removeStaging(in, info.ID, info.Components)
		out.Action, out.Steps, out.Message = "rolled_back", n, "已按 journal 逆序撤销，系统恢复到事务开始前"
		switch txKind(tx) {
		case "uninstall":
			out.Message = "卸载不可撤销：已结束这条事务记录，系统可能处于部分卸载状态；重新运行 lingedge uninstall <组件> 即可完成（各步骤幂等，组件仍在 state.json 里说明尚未完成）"
		case "restore":
			out.Message = "已按 journal 把系统回滚到恢复前安全备份的状态（文件、数据库、master.key），并按需重启了服务"
		}
		return out, nil
	}
	if k := txKind(tx); k != "" {
		return out, &Error{Kind: KindRecover, Msg: "这是一条 " + k + " 事务，不能续跑", SystemState: "partial", Details: map[string]any{"tx": tx.ID},
			Hint: map[string]string{
				"uninstall": "卸载的步骤都是幂等的：先 lingedge recover --rollback 结束这条事务记录，再重新运行 lingedge uninstall <组件>",
				"restore":   "恢复不能从中途续跑：lingedge recover --rollback 回到恢复前安全备份的状态，排查原因后重新运行 lingedge backup restore",
			}[k]}
	}
	return in.resume(ctx, task, j, tx, udo, opt, out)
}

func removeStaging(in *Installer, tx string, comps []string) {
	for _, c := range comps {
		_ = removeAll(in.d.Reg.L.StagingDir() + "/" + tx + "-" + c)
	}
}

// stepSet 返回 journal 里 begin 过的步骤集合与最大步骤号。
func stepSet(tx *registry.Tx) (map[int]registry.StepState, int) {
	m := map[int]registry.StepState{}
	maxN := 0
	for _, s := range tx.Steps {
		m[s.Step] = s
		if s.Step > maxN {
			maxN = s.Step
		}
	}
	return m, maxN
}

func (in *Installer) resume(ctx context.Context, task *registry.Task, j Journal, tx *registry.Tx, udo UndoApplier, opt RecoverOptions, out TxRecovery) (TxRecovery, error) {
	steps, maxN := stepSet(tx)
	refuse := func(msg string) (TxRecovery, error) {
		return out, &Error{Kind: KindRecover, Msg: msg, Hint: "改用 lingedge recover --rollback（撤销回到事务开始前，随后可重新安装）", SystemState: "partial",
			Details: map[string]any{"tx": tx.ID, "next_step": maxN}}
	}
	// 提交阶段：state.json 已写入则只做收尾，否则不能续跑。
	if _, begun := steps[StepSaveState]; begun {
		if !in.stateRecorded(tx, steps[StepSaveState]) {
			return refuse("提交点（第 21 步）尚未完成：state.json 里没有本事务的记录，无法续跑")
		}
		if err := finalizeCommit(j, tx, steps); err != nil {
			return out, wrap(KindStepFailed, err, "收尾失败")
		}
		removeStaging(in, tx.ID, out.Components)
		out.Action, out.Message = "finalized", "状态早已原子提交，仅补写 journal 的收尾记录（done/commit）并归档"
		return out, nil
	}
	if maxN >= StepStart {
		return refuse(fmt.Sprintf("事务已进入激活阶段（第 %d 步），服务状态不确定，不能续跑", maxN))
	}

	// 取出请求：第 1 步的 undo 携带它。
	var req Request
	found := false
	if s, ok := steps[StepOSDetect]; ok && s.Undo != nil {
		for _, a := range s.Undo.Actions {
			r, isReq, err := DecodeRequest(a)
			if err != nil {
				return out, err
			}
			if isReq {
				req, found = r, true
			}
		}
	}
	if !found {
		return refuse("journal 里没有安装请求（事务在记录请求之前被中断），无法续跑")
	}

	// 在途步骤可能只执行了一半：先撤销它，再从它重做（撤销幂等，重做时 Created 重新判定）。
	start := maxN + 1
	if fl := tx.InFlight(); fl != nil {
		start = fl.Step
		rctx := context.WithoutCancel(ctx)
		if fl.Undo != nil {
			for _, a := range fl.Undo.Actions {
				if _, err := udo.Apply(rctx, a); err != nil {
					return out, &Error{Kind: KindRollbackFailed, Step: fl.Step, Msg: "撤销在途步骤失败，无法续跑：" + err.Error(), Cause: err, SystemState: "needs_manual",
						Hint: "排查后重试，或 lingedge recover --rollback", Details: map[string]any{"tx": tx.ID}}
				}
			}
		}
		if err := j.Undone(fl.Step); err != nil {
			return out, err
		}
	}
	if start < StepOSDetect {
		start = StepOSDetect
	}
	req.DryRun, req.NonInteractive = false, opt.NonInteractive
	req.Yes = opt.Yes || req.Accepted
	pre, err := in.Plan(ctx, req)
	if err != nil {
		return out, err // 续跑的预检失败：事务保持未完成，可再次 recover（或 --rollback）
	}
	if pre.Noop {
		return refuse("预检显示计划里已没有需要安装的组件（状态已改变），无法续跑")
	}
	x := &execution{in: in, pre: pre, task: task, tx: tx.ID, start: start, resumed: true, j: j}
	x.init()
	x.udo = udo
	r, err := x.run(ctx)
	if err != nil {
		return out, err
	}
	out.Action, out.Result, out.Message = "resumed", r, fmt.Sprintf("已从第 %d 步续跑并完成安装", start)
	return out, nil
}

// stateRecorded 报告 state.json 是否已记录该事务写入的全部组件（第 21 步的 state_forget 动作列出了它们）。
func (in *Installer) stateRecorded(tx *registry.Tx, s registry.StepState) bool {
	st, _, err := in.d.Reg.LoadState()
	if err != nil || s.Undo == nil {
		return false
	}
	n := 0
	for _, a := range s.Undo.Actions {
		if a.Action != actStateForget {
			continue
		}
		n++
		if c, ok := st.Components[a.Extra[stateForgetKey]]; !ok || c.LastTx != tx.ID {
			return false
		}
	}
	return n > 0
}

// finalizeCommit 补写第 21/22 步的 done 与 commit，并归档。
func finalizeCommit(j Journal, tx *registry.Tx, steps map[int]registry.StepState) error {
	if s, ok := steps[StepSaveState]; ok && s.Phase != registry.PhaseDone {
		if err := j.Done(StepSaveState); err != nil {
			return err
		}
	}
	if s, ok := steps[StepAccessInfo]; !ok {
		if err := j.Begin(StepAccessInfo, StepName(StepAccessInfo), registry.Undo{}); err != nil {
			return err
		}
		if err := j.Done(StepAccessInfo); err != nil {
			return err
		}
	} else if s.Phase != registry.PhaseDone {
		if err := j.Done(StepAccessInfo); err != nil {
			return err
		}
	}
	if err := j.Commit(); err != nil {
		return err
	}
	return j.Archive()
}

// txKind 判断事务是否是 uninstall 或 restore（第一步 undo 里的请求记录）；install/update 返回空串。
func txKind(tx *registry.Tx) string {
	for _, s := range tx.Steps {
		if s.Undo == nil {
			continue
		}
		for _, a := range s.Undo.Actions {
			switch a.Action {
			case ActUninstallRequest:
				return "uninstall"
			case backup.ActRestoreRequest:
				return "restore"
			case ActUpdateRequest:
				return "update"
			case ActRollbackRequest:
				return "rollback"
			}
		}
	}
	return ""
}

// recoverUpdate 处理被中断的升级/手动回滚事务（Story 1.43）：
//   - 提交点（state.json 已写入本事务的记录）之后被中断：升级/回滚实际已完成，两种模式都只补写 journal 的收尾（done/commit）并归档——绝不回滚已提交的升级；
//   - 提交点之前：--rollback 按 journal 逆序撤销（切回旧版本、回退数据库、重启旧版本）并复核；--resume 不可行（服务/数据库状态不确定），拒绝并指引 --rollback。
func (in *Installer) recoverUpdate(ctx context.Context, j Journal, tx *registry.Tx, udo UndoApplier, opt RecoverOptions, out TxRecovery, kind string) (TxRecovery, error) {
	f, ok := factsFromTx(tx)
	if !ok {
		return out, &Error{Kind: KindRecover, Msg: "事务日志里没有完整的" + kind + "请求记录，无法自动恢复", SystemState: "needs_manual", Details: map[string]any{"tx": tx.ID},
			Hint: "不要删除 journal：保留它并联系支持；journal 路径 " + in.journalPath(tx.ID)}
	}
	steps, _ := stepSet(tx)
	commitStep := UpStepCommit
	if kind == "rollback" {
		commitStep = RbStepCommit
	}
	if _, begun := steps[commitStep]; begun && in.stateRecordedTx(f.Component, tx.ID) {
		if s := steps[commitStep]; s.Phase != registry.PhaseDone {
			if err := j.Done(commitStep); err != nil {
				return out, wrap(KindStepFailed, err, "收尾失败")
			}
		}
		if err := j.Commit(); err != nil {
			return out, wrap(KindStepFailed, err, "收尾失败")
		}
		if err := j.Archive(); err != nil {
			return out, wrap(KindStepFailed, err, "归档失败")
		}
		removeStaging(in, tx.ID, out.Components)
		out.Action, out.Message = "finalized", "状态早已原子提交（"+map[string]string{"update": "升级", "rollback": "回滚"}[kind]+"已完成），仅补写 journal 的收尾记录并归档；不会回滚已提交的结果"
		return out, nil
	}
	if opt.Mode == RecoverResume {
		return out, &Error{Kind: KindRecover, Msg: "这是一条" + map[string]string{"update": "升级", "rollback": "回滚"}[kind] + "事务，提交点之前不能续跑（服务与数据库状态不确定）", SystemState: "partial",
			Hint: "lingedge recover --rollback 回到事务开始前的状态（切回旧版本、回退数据库、重启服务），排查原因后重新运行", Details: map[string]any{"tx": tx.ID}}
	}
	n := len(tx.UndoPlan())
	var rep RollbackReport
	verify := func(context.Context) error { return nil }
	if in.d.Update != nil {
		verify = func(vc context.Context) error {
			// 撤销动作可能在上一次失败的回滚里已全部标记为 undone（只是复核没通过）：这里向「升级前的状态」收敛——升级前在运行的服务必须在运行（幂等）。
			if f.WasActive && len(f.Units) > 0 {
				if _, err := in.sys.Start(vc, f.Units...); err != nil {
					return fmt.Errorf("重新启动旧版本服务失败：%w", err)
				}
			}
			return in.verifyRolledBack(vc, f, &rep)
		}
	}
	if err := in.rollbackJournalV(ctx, j, tx.ID, udo, "recover --rollback", func(c context.Context) error { return in.sys.DaemonReload(c) }, verify); err != nil {
		return out, &Error{Kind: KindRollbackFailed, Msg: "回滚未完成，事务日志已保留：" + err.Error(), Cause: err, SystemState: "needs_manual",
			Hint: "排查原因后重新运行 lingedge recover --rollback（撤销是幂等的，会从未完成处继续；数据库回退需要迁移账号凭据：环境变量 " + backup.EnvDBPass + " 或 " + backup.EnvPassFile + "、migrate.yaml）", Details: map[string]any{"tx": tx.ID}}
	}
	removeStaging(in, tx.ID, out.Components)
	out.Action, out.Steps = "rolled_back", n
	out.Message = "已按 journal 逆序撤销并复核（版本 " + rep.Version + "，健康：" + rep.Health + "），系统恢复到事务开始前"
	return out, nil
}

// stateRecordedTx 报告 state.json 里该组件的最近事务是否就是 tx（升级/回滚的提交点已写入）。
func (in *Installer) stateRecordedTx(comp, tx string) bool {
	st, _, err := in.d.Reg.LoadState()
	if err != nil {
		return false
	}
	c, ok := st.Components[comp]
	return ok && c.LastTx == tx
}

// factsFromTx 从 journal 还原复核回滚结果所需的事实。
func factsFromTx(tx *registry.Tx) (rollbackFacts, bool) {
	var f rollbackFacts
	var found bool
	for _, s := range tx.Steps {
		if s.Undo == nil {
			continue
		}
		for _, a := range s.Undo.Actions {
			x := a.Extra
			switch a.Action {
			case ActUpdateRequest:
				units, err := parseUnitNames(x["units"])
				if err != nil {
					return f, false
				}
				f.Component, f.Version, f.NewVersion, f.Units, f.DB, found = x["component"], x["from"], x["to"], units, x["db"] == "true", true
			case ActRollbackRequest:
				units, err := parseUnitNames(x["units"])
				if err != nil {
					return f, false
				}
				f.Component, f.Version, f.NewVersion, f.Units, f.DB, found = x["component"], x["restore_version"], x["other_version"], units, x["db"] == "true", true
				f.PreSchema, _ = strconv.ParseInt(x["pre_schema"], 10, 64)
			case ActUpdateStartUnits:
				f.WasActive = x["was_active"] == "true"
			case backup.ActRestoreSafety:
				f.WasActive = x[backup.ExtraWasActive] == "true"
			case ActUpdateDBRevert:
				f.PreSchema, _ = strconv.ParseInt(x["pre"], 10, 64)
				if x["path"] == registry.PathBinaryExpand {
					f.AltSchema, _ = strconv.ParseInt(x["target"], 10, 64)
				}
			}
		}
	}
	return f, found && registry.ValidComponent(f.Component) && f.Version != ""
}
