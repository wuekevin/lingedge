package installer

import (
	"context"
	"fmt"
	"sort"

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
	var u UndoApplier = &Undoer{Root: in.d.Root, Reg: in.d.Reg, Svc: svc}
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

	if opt.Mode == RecoverRollback {
		n := len(tx.UndoPlan())
		if err := in.rollbackJournal(rctx, j, info.ID, udo, "recover --rollback", func(c context.Context) error { return in.sys.DaemonReload(c) }); err != nil {
			return out, &Error{Kind: KindRollbackFailed, Msg: "回滚未完成，事务日志已保留：" + err.Error(), Cause: err, SystemState: "needs_manual",
				Hint: "排查原因后重新运行 lingedge recover --rollback（撤销是幂等的，会从未完成处继续）", Details: map[string]any{"tx": info.ID}}
		}
		removeStaging(in, info.ID, info.Components)
		out.Action, out.Steps, out.Message = "rolled_back", n, "已按 journal 逆序撤销，系统恢复到事务开始前"
		return out, nil
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
