package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/dbmigrate"
	"github.com/wuekevin/lingedge/internal/tools/doctor"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/rollback"
	"github.com/wuekevin/lingedge/internal/tools/updater"
	"github.com/wuekevin/lingedge/pkg/version"
)

// 升级/回滚命令（Story 1.43）：update check | update <组件>|--all | update self | rollback | version --check。

// dbMemo 在一次命令调用内缓存已解析的迁移凭据（含交互输入的口令）：同一次升级/回滚的多个步骤与撤销动作不会重复提示。口令只在内存里，命令结束时清零。
type dbMemo struct {
	conn *backup.DBConn
}

func (r *Run) resolveDBMemo(inv *invocation, interactive bool) (*backup.DBConn, error) {
	if r.dbm != nil && r.dbm.conn != nil {
		c := *r.dbm.conn
		c.Password = append([]byte(nil), r.dbm.conn.Password...)
		return &c, nil
	}
	conn, err := r.resolveDBRaw(inv, interactive)
	if err != nil {
		return nil, err
	}
	r.dbm = &dbMemo{conn: &conn}
	c := conn
	c.Password = append([]byte(nil), conn.Password...)
	return &c, nil
}

func (r *Run) clearDBMemo() {
	if r.dbm != nil && r.dbm.conn != nil {
		r.dbm.conn.Close()
	}
	r.dbm = nil
}

// backupPassphraseOpt 返回升级前备份的口令：--passphrase-file ＞ LINGEDGE_BACKUP_PASSPHRASE_FILE；都没有 = nil（不加密）。口令永远不经命令行参数。
func (r *Run) backupPassphraseOpt(inv *invocation) ([]byte, error) {
	path := ""
	if inv != nil && inv.cmd.Flags().Lookup("passphrase-file") != nil {
		path = inv.str("passphrase-file")
	}
	if path == "" {
		path = r.env.Getenv(EnvBackupPassFile)
	}
	if path == "" {
		return nil, nil
	}
	return backup.ReadPassphraseFile(path, r.env.Backup.AllowedUIDs)
}

// updateEnv 装配升级/回滚的外部依赖（installer.UpdateEnv）。inv 可为 nil（recover 撤销升级事务时）。
func (r *Run) updateEnv(inv *invocation) (*installer.UpdateEnv, *Error) {
	svc, e := r.backupService(inv)
	if e != nil {
		return nil, e
	}
	be, ie := r.env.Backup, r.env.Install
	srun := be.ServerRun
	if srun == nil {
		rr, err := backup.NewServerRunner()
		if err != nil {
			return nil, newErr(ExitGeneral, "general.unexpected", "无法构造 lingedge-server 执行器："+err.Error(), "")
		}
		srun = rr
	}
	mig := &dbmigrate.Migrator{Run: srun, Env: be.ServerEnv}
	ex := &rollback.Executor{Root: ie.Root, Backup: svc, Mig: mig,
		DB: func(ctx context.Context) (*backup.DBConn, error) { return r.resolveDBMemo(inv, false) },
		Passphrase: func(ctx context.Context) ([]byte, error) {
			p, err := r.backupPassphraseOpt(inv)
			if err == nil && len(p) == 0 {
				return nil, fmt.Errorf("需要备份口令：设置环境变量 %s 指向口令文件", EnvBackupPassFile)
			}
			return p, err
		}}
	tc := r.toolsConfigLenient()
	env := &installer.UpdateEnv{Backup: svc, Rollback: ex, Mig: mig,
		DB: func(ctx context.Context, interactive bool) (*backup.DBConn, error) {
			return r.resolveDBMemo(inv, interactive)
		},
		BackupPassphrase: func(ctx context.Context) ([]byte, error) { return r.backupPassphraseOpt(inv) },
		ToolsVersion:     ie.ToolsVersion,
		PostChecks:       r.postChecks,
		TypedConfirm:     r.typedConfirm}
	if tc.Backup.Dir != "" {
		env.BackupDir = r.hostPath(tc.Backup.Dir)
	}
	return env, nil
}

// typedConfirm 要求键入 phrase（交互）：全屏醒目提示后读取一行，必须与 phrase 完全一致。
func (r *Run) typedConfirm(message string, items []string, phrase string) (bool, error) {
	if !r.interactive() {
		return false, nil
	}
	wf(r.env.Stderr, "\n%s\n  !! %s !!\n", r.symbol("==== 不可逆迁移 ====", "31"), redactText(message))
	for _, it := range items {
		wf(r.env.Stderr, "  - %s\n", redactText(it))
	}
	wf(r.env.Stderr, "继续请键入 %s（其他输入取消）: ", phrase)
	ans, err := r.readLine()
	return err == nil && ans == phrase, nil
}

// postChecks 运行 doctor 的组件子集（只读诊断；结果只作报告）。
func (r *Run) postChecks(ctx context.Context, comp string) installer.PostCheckReport {
	rep := installer.PostCheckReport{}
	w, _ := r.doctorWorld(ctx, false)
	reg, err := doctor.Default(r.ops().ExtraChecks...)
	if err != nil {
		rep.Problems = []string{"doctor 注册表不合法"}
		return rep
	}
	out := reg.Run(ctx, w, doctor.Filter{Component: comp})
	rep.Pass, rep.Warn, rep.Fail, rep.Skip = out.Summary.Pass, out.Summary.Warn, out.Summary.Fail, out.Summary.Skip
	for _, f := range out.Findings {
		if f.Level == doctor.LevelFail {
			rep.Problems = append(rep.Problems, f.ID)
		}
	}
	return rep
}

func (r *Run) installerFor(inv *invocation, events chan<- installer.Event) (*installer.Installer, error) {
	env, e := r.updateEnv(inv)
	if e != nil {
		return nil, e
	}
	return r.newInstallerWith(events, installer.Request{}, env)
}

// ---- update check / version --check ----

type checkManifestInfo struct {
	Channel     string `json:"channel"`
	Sequence    int64  `json:"sequence"`
	GeneratedAt string `json:"generated_at"`
	ValidUntil  string `json:"valid_until"`
}

type updateCheckData struct {
	Manifest   checkManifestInfo `json:"manifest"`
	Components []updater.Entry   `json:"components"`
	Upgradable int               `json:"upgradable"`
	// Tools 是 Tools 自身的检查结果（清单里的 tools 组件）。
	Tools *updater.Entry `json:"tools,omitempty"`
}

// updateCheck 取得并验证清单（只读，不推进 sequence）并算出各已装组件的升级项与迁移预告。
func (r *Run) updateCheck(channel string) (*updateCheckData, error) {
	m, err := r.fetchManifest(r.ctx, channel)
	if err != nil {
		return nil, err
	}
	st, _, err := r.registry().LoadState()
	if err != nil {
		return nil, err
	}
	d := &updateCheckData{Manifest: checkManifestInfo{Channel: m.Channel, Sequence: m.Sequence, GeneratedAt: m.GeneratedAt.UTC().Format(time.RFC3339), ValidUntil: m.ValidUntil.UTC().Format(time.RFC3339)},
		Components: updater.Check(m, st)}
	if d.Components == nil {
		d.Components = []updater.Entry{}
	}
	for _, e := range d.Components {
		if e.Status == updater.StatusUpgradeAvailable || e.Status == updater.StatusBlocked {
			d.Upgradable++
		}
	}
	if tv := r.toolsVersion(); tv != "" {
		ts := registry.NewState()
		ts.Components["tools"] = registry.Component{Version: tv, State: "installed"}
		if es := updater.Check(m, ts); len(es) == 1 && es[0].Status != updater.StatusNotInManifest {
			d.Tools = &es[0]
		}
	}
	return d, nil
}

func (r *Run) toolsVersion() string {
	if r.env.Install.ToolsVersion != "" {
		return r.env.Install.ToolsVersion
	}
	return version.Version
}

func renderCheck(w io.Writer, d *updateCheckData) {
	wf(w, "清单：通道 %s，sequence %d，生成于 %s，有效至 %s\n", d.Manifest.Channel, d.Manifest.Sequence, d.Manifest.GeneratedAt, d.Manifest.ValidUntil)
	if len(d.Components) == 0 {
		wln(w, "未安装任何组件")
	} else {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		wln(tw, "\n组件\t已装\t最新稳定版\t状态\t迁移预告")
		for _, e := range d.Components {
			wf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Component, e.Installed, orDash(e.Latest), statusLabel(e), migrationsLabel(e))
		}
		_ = tw.Flush()
		for _, e := range d.Components {
			if e.Message != "" {
				wf(w, "  %s：%s\n", e.Component, redactText(e.Message))
			}
		}
	}
	if d.Tools != nil {
		wf(w, "\nTools 自身：%s → %s（%s）；升级：lingedge update self\n", d.Tools.Installed, orDash(d.Tools.Latest), statusLabel(*d.Tools))
	}
	if d.Upgradable > 0 {
		wln(w, "\n升级：lingedge update <组件>（先看计划：--dry-run）；含「不可逆」的升级需要 --accept-irreversible 或交互键入 confirm-irreversible")
	}
}

func statusLabel(e updater.Entry) string {
	s := map[string]string{updater.StatusUpToDate: "已是最新", updater.StatusUpgradeAvailable: "可升级", updater.StatusBlocked: "需逐级升级", updater.StatusUnreleased: "未发布",
		updater.StatusBuiltin: "内置", updater.StatusNotInManifest: "清单中没有", updater.StatusNoCandidate: "无可用版本"}[e.Status]
	if e.InstalledRevoked {
		s += "（已装版本已被吊销）"
	}
	return s
}

func migrationsLabel(e updater.Entry) string {
	if e.Migrations == nil {
		return "-"
	}
	if len(e.Migrations.Classes) == 0 {
		return "无迁移"
	}
	s := strings.Join(e.Migrations.Classes, "+")
	if e.Migrations.HasIrreversible {
		s += "（含不可逆：需确认并强制备份）"
	}
	return s
}

func runUpdateCheck(inv *invocation) (*Result, error) {
	channel := "stable"
	if inv.changed("channel") {
		channel = inv.str("channel")
	}
	d, err := inv.r.updateCheck(channel)
	if err != nil {
		return nil, err
	}
	return &Result{Data: d, Human: func(w io.Writer) { renderCheck(w, d) }}, nil
}

// ---- update <组件> | --all ----

type updateItem struct {
	Component    string                     `json:"component"`
	From         string                     `json:"from"`
	To           string                     `json:"to"`
	Outcome      string                     `json:"outcome"`
	Tx           string                     `json:"tx,omitempty"`
	Path         string                     `json:"path"`
	Classes      []string                   `json:"classes"`
	Irreversible bool                       `json:"has_irreversible"`
	Promise      string                     `json:"promise"`
	Backup       string                     `json:"backup,omitempty"`
	BackupVerify bool                       `json:"backup_verified"`
	SchemaFrom   int64                      `json:"schema_from,omitempty"`
	SchemaTo     int64                      `json:"schema_to,omitempty"`
	Health       string                     `json:"health,omitempty"`
	Pruned       []string                   `json:"pruned_releases"`
	PostChecks   *installer.PostCheckReport `json:"post_checks,omitempty"`
	Plan         updatePlanView             `json:"plan"`
}

type updatePlanView struct {
	Migrations  installer.MigrationPlan    `json:"migrations"`
	Backup      installer.UpdateBackupPlan `json:"backup"`
	Units       []string                   `json:"units"`
	Steps       []installer.UpdateStepInfo `json:"steps"`
	Credentials string                     `json:"db_login"`
	Keep        int                        `json:"keep_releases"`
	NeedConfirm bool                       `json:"need_confirm"`
	BackupForce bool                       `json:"backup_forced"`
}

type updateData struct {
	DryRun     bool         `json:"dry_run"`
	All        bool         `json:"all"`
	Components []updateItem `json:"components"`
}

func viewOf(p *installer.UpdatePlan) updatePlanView {
	return updatePlanView{Migrations: p.Migrations, Backup: p.Backup, Units: p.Units, Steps: p.Steps, Credentials: p.Credentials, Keep: p.KeepReleases,
		NeedConfirm: p.Decision.NeedConfirm, BackupForce: p.Decision.BackupForced}
}

func itemFromPlan(p *installer.UpdatePlan) updateItem {
	out := "planned"
	if p.Action == "keep" {
		out = installer.OutcomeAlreadyCurrent
	}
	return updateItem{Component: p.Component, From: p.From, To: p.To, Outcome: out, Path: p.Decision.Path, Classes: p.Decision.Classes, Irreversible: p.Decision.Irreversible,
		Promise: p.Decision.Promise, Pruned: []string{}, Plan: viewOf(p)}
}

func itemFromResult(res *installer.UpdateResult) updateItem {
	it := updateItem{Component: res.Component, From: res.From, To: res.To, Outcome: res.Outcome, Tx: res.Tx, Path: res.Path, Classes: res.Decision.Classes, Irreversible: res.Decision.Irreversible,
		Promise: res.Decision.Promise, Backup: res.Backup, BackupVerify: res.BackupVerified, SchemaFrom: res.SchemaFrom, SchemaTo: res.SchemaTo, Health: res.Health, Pruned: res.Pruned, PostChecks: res.Checks}
	if res.Plan != nil {
		it.Plan = viewOf(res.Plan)
	}
	if it.Classes == nil {
		it.Classes = []string{}
	}
	return it
}

// selectable 返回 `update --all` 要处理的组件：state 里已装、生产规格已登记、且非内部组件（内部组件随依赖它的组件一并处理，Epic 3）。
func (r *Run) updateAllTargets(st registry.State) []string {
	specs := r.env.Install.Specs
	if specs == nil {
		specs = installer.ProductionSpecs()
	}
	var out []string
	for _, n := range st.Names() {
		if _, ok := specs[n]; ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func runUpdate(inv *invocation) (*Result, error) {
	r := inv.r
	if inv.flag("apply-staged") {
		return nil, notYet(inv, "apply-staged", "Epic 3（节点升级路径 ADR-024：lingedge-node-upgrade.path/.service）")
	}
	defer r.clearDBMemo()
	ch, wait := r.progress()
	defer wait()
	in, err := r.installerFor(inv, ch)
	if err != nil {
		return nil, err
	}
	var comps []string
	all := inv.flag("all")
	if all {
		st, _, serr := r.registry().LoadState()
		if serr != nil {
			return nil, serr
		}
		if comps = r.updateAllTargets(st); len(comps) == 0 {
			return &Result{Data: updateData{DryRun: r.opts.DryRun, All: true, Components: []updateItem{}}, Human: func(w io.Writer) { wln(w, "没有可升级的已装组件") }}, nil
		}
	} else {
		comps = []string{inv.args[0]}
	}
	mk := func(c string) installer.UpdateRequest {
		return installer.UpdateRequest{Component: c, Version: inv.str("version"), Channel: inv.str("channel"), Offline: inv.str("offline"), AllowExpired: inv.flag("allow-expired-manifest"),
			AcceptIrreversible: inv.flag("accept-irreversible"), NoBackup: inv.flag("no-backup"), NoRollback: inv.flag("no-rollback"), DryRun: r.opts.DryRun,
			NonInteractive: !r.interactive(), Yes: r.opts.Yes}
	}
	data := updateData{DryRun: r.opts.DryRun, All: all, Components: []updateItem{}}
	var warns []Warning
	if r.opts.DryRun {
		for _, c := range comps {
			plan, perr := in.PlanUpdate(r.ctx, mk(c))
			if perr != nil {
				if all && isInternalBlocker(perr) {
					continue
				}
				return nil, perr
			}
			data.Components = append(data.Components, itemFromPlan(plan))
			warns = append(warns, toWarnings(plan.Warnings)...)
		}
		return &Result{Data: data, Warnings: warns, Human: func(w io.Writer) { renderUpdate(w, data, true) }}, nil
	}
	if inv.task == nil {
		return nil, newErr(ExitGeneral, "general.unexpected", "update 没有取得任务锁", "")
	}
	for i, c := range comps {
		task := inv.task
		if i > 0 { // 每个组件一个事务：释放上一个任务的锁，再为下一个组件取锁并扫描 journal
			inv.task.Release()
			t, berr := r.registry().Begin(r.ctx, registry.TaskRequest{Components: []string{c}})
			if berr != nil {
				return nil, berr
			}
			defer t.Release()
			task = t
		}
		res, uerr := in.Update(r.ctx, task, mk(c))
		if uerr != nil {
			if all && isInternalBlocker(uerr) { // 内部组件（如 openresty）不能直接选择：随依赖它的组件升级（Epic 3）
				data.Components = append(data.Components, updateItem{Component: c, Outcome: "skipped_internal", Path: "binary", Classes: []string{}, Pruned: []string{},
					Promise: "内部组件随依赖它的组件一并处理，不单独升级",
					Plan:    updatePlanView{Migrations: installer.MigrationPlan{Source: "none", Classes: []string{}}, Units: []string{}, Steps: []installer.UpdateStepInfo{}, Credentials: "not_required"}})
				continue
			}
			return nil, uerr
		}
		data.Components = append(data.Components, itemFromResult(res))
		warns = append(warns, toWarnings(res.Warnings)...)
	}
	return &Result{Data: data, Warnings: warns, Human: func(w io.Writer) { renderUpdate(w, data, false) }}, nil
}

// isInternalBlocker 报告求解失败是否因为目标是内部组件（resolver 的 component.internal 阻断）。
func isInternalBlocker(err error) bool {
	var re *resolver.Error
	if !errors.As(err, &re) {
		return false
	}
	for _, b := range re.Blockers {
		if b.Code == "component.internal" {
			return true
		}
	}
	return false
}

func renderUpdate(w io.Writer, d updateData, dry bool) {
	for _, it := range d.Components {
		switch {
		case it.Outcome == "skipped_internal":
			wf(w, "%s 是内部组件：随依赖它的组件一并处理，已跳过\n", it.Component)
			continue
		case it.Outcome == installer.OutcomeAlreadyCurrent:
			wf(w, "%s 已是 %s，无需升级\n", it.Component, it.From)
			continue
		case dry:
			wf(w, "升级计划（--dry-run：未做任何修改）：%s %s → %s\n", it.Component, it.From, it.To)
		default:
			wf(w, "已升级 %s %s → %s\n", it.Component, it.From, it.To)
		}
		wf(w, "  迁移：%s\n", map[bool]string{true: strings.Join(it.Classes, "+"), false: "无迁移"}[len(it.Classes) > 0])
		wf(w, "  失败回滚路径：%s\n  对用户的承诺：%s\n", it.Path, it.Promise)
		if it.Irreversible {
			wln(w, "  !! 含不可逆迁移：升级前强制备份并校验；回滚＝恢复升级前备份 + 旧版本二进制，备份点之后的数据将丢失 !!")
		}
		b := it.Plan.Backup
		switch {
		case dry && b.Planned:
			wf(w, "  备份：将写入 %s（加密：%v，含数据库：%v）\n", b.Dir, b.Encrypted, b.Database)
		case dry:
			wf(w, "  备份：不做（%s）\n", b.Reason)
		case it.Backup != "":
			wf(w, "  升级前备份：%s（已校验可读）\n", it.Backup)
		}
		if len(it.Plan.Units) > 0 {
			wf(w, "  将重启服务：%s\n", strings.Join(it.Plan.Units, "、"))
		}
		if dry {
			wf(w, "  迁移凭据：%s\n  步骤：", it.Plan.Credentials)
			for _, s := range it.Plan.Steps {
				wf(w, "%d.%s ", s.N, s.Name)
			}
			wln(w)
			continue
		}
		if it.SchemaTo > 0 {
			wf(w, "  数据库 schema：%d → %d\n", it.SchemaFrom, it.SchemaTo)
		}
		wf(w, "  健康检查：%s\n", orDash(it.Health))
		if len(it.Pruned) > 0 {
			wf(w, "  已清理旧 release：%s\n", strings.Join(it.Pruned, "、"))
		}
		if it.PostChecks != nil {
			wf(w, "  doctor 子集：通过 %d 警告 %d 失败 %d\n", it.PostChecks.Pass, it.PostChecks.Warn, it.PostChecks.Fail)
		}
		wf(w, "  回滚：lingedge rollback %s\n", it.Component)
	}
}

// ---- update self ----

type updateSelfData struct {
	DryRun bool                  `json:"dry_run"`
	Result *installer.SelfResult `json:"result"`
}

func runUpdateSelf(inv *invocation) (*Result, error) {
	r := inv.r
	ie := r.env.Install
	env, e := r.updateEnv(nil)
	if e != nil {
		return nil, e
	}
	env.ToolsVersion = r.toolsVersion()
	in, err := r.newInstallerWith(nil, installer.Request{}, env)
	if err != nil {
		return nil, err
	}
	_ = ie
	req := installer.SelfRequest{Version: inv.str("version"), Channel: inv.str("channel"), Offline: inv.str("offline"), AllowExpired: inv.flag("allow-expired-manifest"), DryRun: r.opts.DryRun}
	res, err := in.UpdateSelf(r.ctx, req)
	if err != nil {
		return nil, err
	}
	return &Result{Data: updateSelfData{DryRun: r.opts.DryRun, Result: res}, Warnings: toWarnings(res.Warnings), Human: func(w io.Writer) {
		switch res.Outcome {
		case "already_up_to_date":
			wf(w, "lingedge 已是最新（%s）\n", res.From)
		case "planned":
			wf(w, "升级计划（--dry-run）：lingedge %s → %s（替换 %s，旧版本保留为 %s.prev）\n", res.From, res.To, res.Target, res.Target)
		default:
			wf(w, "lingedge 已升级 %s → %s（%s；旧版本保留为 %s；自检：%s）\n", res.From, res.To, res.Target, res.Prev, res.SelfCheck)
		}
	}}, nil
}

// ---- rollback ----

type rollbackData struct {
	DryRun  bool                      `json:"dry_run"`
	Plan    *installer.RollbackPlan   `json:"plan"`
	Outcome string                    `json:"outcome"`
	Tx      string                    `json:"tx,omitempty"`
	Safety  string                    `json:"safety_backup,omitempty"`
	Report  *installer.RollbackReport `json:"report,omitempty"`
}

func runRollback(inv *invocation) (*Result, error) {
	r := inv.r
	defer r.clearDBMemo()
	ch, wait := r.progress()
	defer wait()
	in, err := r.installerFor(inv, ch)
	if err != nil {
		return nil, err
	}
	req := installer.RollbackRequest{Component: inv.args[0], To: inv.str("to"), FromBackup: inv.str("from-backup"), DryRun: r.opts.DryRun}
	if req.FromBackup != "" {
		abs, aerr := filepath.Abs(req.FromBackup)
		if aerr != nil {
			return nil, usageErr("usage.file_not_found", "无法解析备份路径", "")
		}
		if _, serr := os.Stat(abs); serr != nil {
			return nil, usageErr("usage.file_not_found", "找不到备份文件 "+req.FromBackup, "")
		}
		req.FromBackup = abs
	}
	if r.opts.DryRun {
		plan, perr := in.PlanRollback(r.ctx, req)
		if perr != nil {
			return nil, perr
		}
		return &Result{Data: rollbackData{DryRun: true, Plan: plan, Outcome: "planned"}, Warnings: toWarnings(plan.Warnings), Human: func(w io.Writer) { renderRollback(w, plan, nil, true) }}, nil
	}
	if inv.task == nil {
		return nil, newErr(ExitGeneral, "general.unexpected", "rollback 没有取得任务锁", "")
	}
	res, err := in.Rollback(r.ctx, inv.task, req)
	if err != nil {
		return nil, err
	}
	return &Result{Data: rollbackData{Plan: res.Plan, Outcome: res.Outcome, Tx: res.Tx, Safety: res.Safety, Report: &res.Report}, Warnings: toWarnings(res.Warnings),
		Human: func(w io.Writer) { renderRollback(w, res.Plan, res, false) }}, nil
}

func renderRollback(w io.Writer, p *installer.RollbackPlan, res *installer.RollbackResult, dry bool) {
	if dry {
		wf(w, "回滚计划（--dry-run：未做任何修改）：%s %s → %s\n", p.Component, p.From, p.To)
	} else {
		wf(w, "已回滚 %s %s → %s\n", p.Component, p.From, p.To)
	}
	wf(w, "  路径：%s（%s）\n  对用户的承诺：%s\n", p.Path, map[string]string{"upgrade_record": "上一次升级的记录", "explicit_to": "--to", "backup": "--from-backup"}[p.Source], p.Promise)
	if p.Destructive {
		wf(w, "  !! 将用备份 %s 覆盖当前数据库与配置：备份点之后产生的数据将丢失 !!\n", p.Backup)
	}
	if p.SafetyBackup {
		wln(w, "  回滚前会先做一份安全备份（失败时据此撤销）")
	}
	if res != nil {
		for _, d := range res.Report.Detail {
			wf(w, "  %s\n", redactText(d))
		}
		wf(w, "  复核：版本 %s，健康：%s", res.Report.Version, res.Report.Health)
		if res.Report.Schema > 0 {
			wf(w, "，数据库 schema %d", res.Report.Schema)
		}
		wln(w)
		if res.Report.Checks != nil {
			wf(w, "  doctor 子集：通过 %d 警告 %d 失败 %d\n", res.Report.Checks.Pass, res.Report.Checks.Warn, res.Report.Checks.Fail)
		}
		if res.Safety != "" {
			wf(w, "  回滚前安全备份：%s（保留）\n", res.Safety)
		}
	}
}
