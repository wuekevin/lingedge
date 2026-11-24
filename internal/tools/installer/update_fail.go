package installer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// rollbackFacts 是复核「回滚结果」所需的事实：升级/回滚事务的请求记录与撤销动作里都有（recover 据此重建）。
type rollbackFacts struct {
	Component string
	// Version 是回滚之后应当在运行的版本（升级的旧版本；手动回滚的目标版本）。
	Version string
	// NewVersion 是被撤销掉的版本（其二进制用于 migrate status）。
	NewVersion string
	Units      []service.UnitName
	WasActive  bool
	DB         bool
	PreSchema  int64
	// AltSchema 是可接受的另一个 schema 版本（>0 时）：expand 路径的回滚只切回旧二进制、不执行 down，数据库保持在升级后的 schema（§17.2）。
	AltSchema int64
	// SkipState：不核对 state.json 的版本（手动回滚的前向复核发生在提交点之前，state 此时仍记着回滚前的版本）。
	SkipState bool
}

// RollbackReport 是回滚后复核的结果（报告给用户）。
type RollbackReport struct {
	Version string `json:"version"`
	Schema  int64  `json:"schema_version,omitempty"`
	Health  string `json:"health"`
	Path    string `json:"path,omitempty"`
	// Detail 是回滚各动作的说明（如「migrate down 到 …」「已恢复升级前备份 … lineage a → b」）。
	Detail []string         `json:"detail,omitempty"`
	Checks *PostCheckReport `json:"post_checks,omitempty"`
}

// verifyRolledBack 复核回滚结果（tools-design §17.3「回滚后检查」）：current 与 state.json 的版本号与升级前一致、数据库 schema 版本符合预期、服务恢复并通过健康检查；
// 随后运行 doctor 子集（只读报告）。任何一项失败返回错误——调用方按「回滚失败」处理（退出码 12，journal 保留）。
func (in *Installer) verifyRolledBack(ctx context.Context, f rollbackFacts, rep *RollbackReport) error {
	env, err := in.updateEnv()
	if err != nil {
		return err
	}
	cur, err := in.curVersion(f.Component)
	if err != nil {
		return err
	}
	if cur != f.Version {
		return fmt.Errorf("回滚后 current 指向 %q，期望 %q", cur, f.Version)
	}
	if !f.SkipState {
		st, _, err := in.d.Reg.LoadState()
		if err != nil {
			return err
		}
		if c, ok := st.Components[f.Component]; !ok || c.Version != f.Version {
			return fmt.Errorf("回滚后 state.json 记录的版本与期望的 %s 不一致", f.Version)
		}
	}
	rep.Version = f.Version
	var conn *backup.DBConn
	liveOnly := false
	if f.DB && f.PreSchema > 0 {
		if env.DB == nil || env.Mig == nil {
			return errors.New("无法复核数据库 schema 版本：未装配迁移凭据通道")
		}
		if conn, err = env.DB(ctx, false); err != nil {
			return fmt.Errorf("无法复核数据库 schema 版本：%w", err)
		}
		defer conn.Close()
		ver, serr := env.Rollback.SchemaVersion(ctx, conn)
		if serr != nil {
			return fmt.Errorf("无法读取数据库迁移版本：%w", serr)
		}
		if ver != f.PreSchema && (f.AltSchema == 0 || ver != f.AltSchema) {
			return fmt.Errorf("回滚后数据库 schema 版本为 %d，与升级前的 %d 不一致", ver, f.PreSchema)
		}
		rep.Schema = ver
		// expand 路径：库保持在升级后的 schema，旧二进制对「库里有它不认识的迁移」报告 /healthz/ready 不就绪（见 tools-design §17.7）——
		// 此时回滚后的健康只要求存活探针（/healthz）与 schema 符合预期，并在报告里如实标明。
		liveOnly = f.AltSchema != 0 && ver == f.AltSchema && ver != f.PreSchema
	}
	rep.Health = "not_running"
	if f.WasActive && len(f.Units) > 0 {
		spec, ok := in.d.Specs[f.Component]
		if !ok {
			return fmt.Errorf("组件 %s 没有规格，无法做回滚后的健康检查", f.Component)
		}
		timeout, err := in.d.Config.HealthTimeout()
		if err != nil {
			return err
		}
		interval, err := in.d.Config.HealthInterval()
		if err != nil {
			return err
		}
		rt := &Runtime{Component: f.Component, Version: f.Version, Units: f.Units, Root: in.d.Root, SetupListen: in.serverHTTPListen()}
		if herr := waitHealth(ctx, timeout, interval, func(cx context.Context) error {
			return in.healthOnce(cx, spec, rt, f.DB && !liveOnly, nil, "", 0)
		}); herr != nil {
			return fmt.Errorf("回滚后健康检查未通过：%w", herr)
		}
		rep.Health = "healthy"
		if liveOnly {
			rep.Health = "healthy_live_only"
		}
	}
	if env.PostChecks != nil {
		r := env.PostChecks(ctx, f.Component)
		rep.Checks = &r
	}
	return nil
}

// facts 是升级事务回滚复核用的事实。
func (x *updateExec) facts() rollbackFacts {
	f := rollbackFacts{Component: x.plan.Component, Version: x.plan.From, NewVersion: x.plan.To, Units: x.plan.units, WasActive: x.wasActive, DB: x.hasDB, PreSchema: x.pre}
	if x.decision.Path == registry.PathBinaryExpand {
		f.AltSchema = x.tgt
	}
	return f
}

// fail 处理升级失败：逆序执行 journal 的撤销计划（11 停止 → 10 切回 → 9 回退数据库 → 8 重启旧版本 …），复核回滚结果，之后 journal 以 abort 结束并归档。
//   - 在改动系统之前失败（第 8 步之前）：撤销只清理 staging/新 release，系统未改动，错误沿原因分类（校验失败 5、凭据/确认 3/9 …）；
//   - 第 8 步及之后失败：KindRolledBack（退出码 10）；收到 SIGINT：KindInterrupted（130）；
//   - 回滚（含复核）失败：KindRollbackFailed（退出码 12），journal 保留，`recover --rollback` 可重试（撤销幂等）。
func (x *updateExec) fail(ctx context.Context, cause error) error {
	in := x.in
	step, inner := 0, cause
	var se *stepErr
	if errors.As(cause, &se) {
		step, inner = se.step, se.err
	}
	rctx := context.WithoutCancel(ctx)
	touched := step >= UpStepStop
	in.ev.emit(Event{Type: EventInfo, Op: "update", Total: UpTotalSteps, Step: step, Message: "升级失败，开始回滚（逆序撤销）", Code: "rollback.start"})
	var rep RollbackReport
	rep.Path = x.decision.Path
	verify := func(vc context.Context) error { return nil }
	if touched {
		verify = func(vc context.Context) error { return in.verifyRolledBack(vc, x.facts(), &rep) }
	}
	if rerr := in.rollbackJournalV(rctx, x.j, x.tx, x.udo, "update failed at step "+strconv.Itoa(step), func(c context.Context) error { return in.sys.DaemonReload(c) }, verify); rerr != nil {
		e := &Error{Kind: KindRollbackFailed, Msg: "升级失败，且自动回滚未能完成，事务日志已保留：" + rerr.Error(), Cause: inner,
			Hint: "排查原因后运行 lingedge recover --rollback 重试（撤销是幂等的）；升级前备份：" + orDash(x.backupPth) + "；journal：" + in.journalPath(x.tx), SystemState: "needs_manual",
			Details: map[string]any{"tx": x.tx, "failed_step": step, "failed_step_name": UpdateStepName(step), "backup": x.backupPth}}
		x.j.Close()
		return e
	}
	rep.Detail = x.details
	det := map[string]any{"tx": x.tx, "failed_step": step, "failed_step_name": UpdateStepName(step), "backup": x.backupPth, "rollback": rep, "path": x.decision.Path}
	switch {
	case isCancel(inner):
		return &Error{Kind: KindInterrupted, Msg: "已收到 SIGINT：当前原子段完成后已回滚，系统恢复到升级前", Cause: inner, SystemState: "rolled_back", Details: det}
	case !touched:
		var ie *Error
		if errors.As(inner, &ie) { // 确认/凭据等自身带分类的错误：回滚了 staging 之后原样返回
			if ie.SystemState == "" {
				ie.SystemState = "unchanged"
			}
			return ie
		}
		return &Error{Kind: KindStepFailed, Msg: "升级在改动系统之前失败（第 " + strconv.Itoa(step) + " 步 " + UpdateStepName(step) + "），系统未改动", Cause: inner, SystemState: "unchanged", Details: det}
	}
	return &Error{Kind: KindRolledBack, Msg: fmt.Sprintf("升级失败（第 %d 步 %s），已自动回滚到 %s 并通过复核（版本、schema、健康检查）", step, UpdateStepName(step), x.plan.From), Cause: inner,
		Hint: "回滚路径：" + x.decision.Path + "；详见 cause 与 details.rollback；升级前备份保留在 " + orDash(x.backupPth), SystemState: "rolled_back", Details: det}
}

func orDash(s string) string {
	if s == "" {
		return "（无）"
	}
	return s
}

func parseUnitNames(csv string) ([]service.UnitName, error) {
	if csv == "" {
		return nil, nil
	}
	var out []service.UnitName
	for _, s := range strings.Split(csv, ",") {
		if !validUnit(s) {
			return nil, fmt.Errorf("journal 里的单元名 %q 不合法", s)
		}
		out = append(out, service.UnitName(s))
	}
	return out, nil
}

func validUnit(s string) bool {
	switch service.UnitName(s) {
	case service.UnitServer, service.UnitNode, service.UnitOpenresty, service.UnitOpenrestyPath, service.UnitUpgradePath, service.UnitUpgradeService:
		return true
	}
	return false
}

var _ = registry.PathBinary

// detailUndoer 包一层 UndoApplier：把每个撤销动作的说明收集起来放进回滚报告（RollbackReport.Detail）。
type detailUndoer struct {
	inner UndoApplier
	sink  func(string)
}

func (d *detailUndoer) Apply(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
	r, err := d.inner.Apply(ctx, a)
	if err == nil && r.Detail != "" && d.sink != nil {
		d.sink(r.Detail)
	}
	return r, err
}
