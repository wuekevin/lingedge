package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/dbmigrate"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/rollback"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

func updateSteps() []UpdateStepInfo {
	out := make([]UpdateStepInfo, 0, UpTotalSteps)
	for n := 1; n <= UpTotalSteps; n++ {
		out = append(out, UpdateStepInfo{N: n, Name: updateStepNames[n], Title: updateStepTitles[n]})
	}
	return out
}

func (in *Installer) updateEnv() (*UpdateEnv, error) {
	if in.d.Update == nil {
		return nil, newErr(KindInternal, "本次调用没有装配升级依赖（Deps.Update）")
	}
	return in.d.Update, nil
}

// serverInitialized 报告 server 的安装向导是否已完成（<数据目录>/install.lock）：未完成 = setup mode，没有数据库可备份/迁移。
func (in *Installer) serverInitialized() bool {
	_, err := os.Stat(in.d.hostPath(service.ServerDataDir + "/install.lock"))
	return err == nil
}

// serverHTTPListen 读取 server.yaml 的 http.listen（健康检查地址）；读不到取缺省 127.0.0.1:8080。
func (in *Installer) serverHTTPListen() string {
	b, err := os.ReadFile(in.d.hostPath(backup.ServerConfigFile))
	if err != nil {
		return "127.0.0.1:8080"
	}
	var shape struct {
		HTTP struct {
			Listen string `yaml:"listen"`
		} `yaml:"http"`
	}
	if yaml.Unmarshal(b, &shape) != nil || shape.HTTP.Listen == "" {
		return "127.0.0.1:8080"
	}
	return shape.HTTP.Listen
}

func (in *Installer) updateBackupDir() string {
	if in.d.Update != nil && in.d.Update.BackupDir != "" {
		return in.d.Update.BackupDir
	}
	return in.hostP(backup.DefaultBackupDir)
}

// supportsBackup：backup 包支持的组件（server/node）。其余组件（静态前端等）的「快照」就是保留的旧 release 目录。
func supportsBackup(c string) bool { return c == "server" || c == "node" }

// PlanUpdate 做升级的只读规划（步骤 2–4，--dry-run/--plan-only 与真实升级同源）：验签清单 → resolver 求解（含 min_upgrade_from/requires/conflicts/系统）→
// 迁移预告（清单）与决策表 → 备份计划。不取锁、不开 journal、不创建任何文件。
// 目标组件未安装 → KindNotInstalled；需要同时安装/升级别的组件 → KindPrecondition；清单/求解失败原样返回（cli 映射退出码 5/7/8/…）。
func (in *Installer) PlanUpdate(ctx context.Context, req UpdateRequest) (*UpdatePlan, error) {
	if _, err := in.updateEnv(); err != nil {
		return nil, err
	}
	if !registry.ValidComponent(req.Component) {
		return nil, newErr(KindSettings, "组件名不合法")
	}
	if req.Channel == "" {
		req.Channel = "stable"
	}
	if req.Channel != "stable" && req.Channel != "beta" {
		return nil, newErr(KindSettings, "通道必须是 stable 或 beta")
	}
	if req.AllowExpired && req.Offline == "" {
		return nil, newErr(KindSettings, "--allow-expired-manifest 只能与 --offline 一起使用")
	}
	in.ev.emit(Event{Type: EventPhase, Op: "update", Total: UpTotalSteps, Phase: PhasePreflight, Message: "升级计划（只读）"})
	state, _, err := in.d.Reg.LoadState()
	if err != nil {
		return nil, err
	}
	rec, ok := state.Components[req.Component]
	if !ok {
		return nil, &Error{Kind: KindNotInstalled, Msg: "组件 " + req.Component + " 未安装", Hint: "lingedge list 查看已安装的组件；安装用 lingedge install " + req.Component, SystemState: "unchanged"}
	}
	spec, ok := in.d.Specs[req.Component]
	if !ok {
		return nil, &Error{Kind: KindSpecMissing, Msg: "组件 " + req.Component + " 的规格（ComponentSpec）尚未交付，无法升级", SystemState: "unchanged",
			Hint: "该组件的实现由后续 Story 交付；本命令不会用占位实现假装升级", Details: map[string]any{"components": []string{req.Component}, "registered": in.d.Specs.Names()}}
	}
	// 步骤 2：获取并验证签名清单（只读，不推进 sequence）。
	mfRes, err := in.loadManifest(ctx, Request{Component: req.Component, Channel: req.Channel, Offline: req.Offline, AllowExpired: req.AllowExpired})
	if err != nil {
		return nil, err
	}
	pr, err := in.d.Collect(ctx)
	if err != nil {
		return nil, wrap(KindStepFailed, err, "系统探测失败")
	}
	// 步骤 3/4：兼容性与目标版本（resolver 同时给出 min_upgrade_from/requires/conflicts/系统/端口/资源的判定）。
	rplan := fixPlan(resolver.Resolve(resolver.Input{
		Want: []resolver.Want{{Component: req.Component, Version: req.Version, Variant: installedVariants(state)[req.Component]}}, Installed: state,
		InstalledVariants: installedVariants(state), Manifest: mfRes.Manifest, System: pr.System,
	}))
	if !rplan.Executable() {
		return nil, rplan.Err()
	}
	var pc *resolver.PlanComponent
	var others []string
	for i := range rplan.Components {
		c := rplan.Components[i]
		if c.Name == req.Component {
			pc = &rplan.Components[i]
			continue
		}
		if c.Action != "keep" {
			others = append(others, c.Name+"（"+c.Action+" "+c.Version+"）")
		}
	}
	if pc == nil {
		return nil, newErr(KindInternal, "求解结果里没有目标组件")
	}
	if len(others) > 0 && pc.Action != "keep" {
		return nil, &Error{Kind: KindPrecondition, Msg: "升级 " + req.Component + " 还需要同时变更：" + strings.Join(others, "、"), SystemState: "unchanged",
			Hint: "update 一次只升级一个组件的事务；请先 lingedge update 依赖组件（或 install 缺失的依赖），再升级 " + req.Component, Details: map[string]any{"needs": others}}
	}
	plan := &UpdatePlan{Component: req.Component, From: rec.Version, To: pc.Version, Variant: pc.Variant, Channel: req.Channel, Action: pc.Action,
		Warnings: []Warning{}, Steps: updateSteps(), Units: []string{}, KeepReleases: in.d.Config.KeepReleases(), Resolved: rplan, Credentials: "not_required",
		req: req, pc: *pc, rec: rec, mf: mfRes, spec: spec, probe: pr}
	for _, w := range rplan.Warnings {
		plan.Warnings = append(plan.Warnings, Warning{Code: w.Code, Component: w.Component, Message: w.Message})
	}
	for _, w := range mfRes.Warnings {
		plan.Warnings = append(plan.Warnings, Warning{Code: "manifest.warning", Message: w})
	}
	if pc.Action == "keep" {
		plan.Action = "keep"
		plan.Decision = rollback.Decide(nil, req.Component == "server")
		plan.Migrations = MigrationPlan{Source: "none", Classes: []string{}}
		return plan, nil
	}
	plan.units = service.RunUnits(req.Component)
	for _, u := range plan.units {
		plan.Units = append(plan.Units, string(u))
	}
	// 迁移预告（清单）与决策表。真正的依据是步骤 7 新二进制对真实数据库的 migrate plan，取更严格者。
	isServer := req.Component == "server"
	mp := MigrationPlan{Source: "none", Classes: []string{}, DBInitialized: !isServer || in.serverInitialized()}
	if isServer {
		mp.Source = "manifest"
		if n := pc.Migrations; n != nil {
			mp.Classes = rollback.MergeClasses(n.Classes)
			if n.IrreversibleCount > 0 {
				mp.Classes = rollback.MergeClasses(mp.Classes, []string{rollback.ClassIrreversible})
			}
		}
		if !mp.DBInitialized {
			mp.Source, mp.Classes = "none", []string{}
			plan.Warnings = append(plan.Warnings, Warning{Code: "update.setup_mode", Component: "server", Step: UpStepBackup,
				Message: "server 的安装向导尚未完成（setup mode，没有数据库）：本次只升级二进制，不备份数据库、不执行迁移；数据库迁移将在向导里由新二进制完成"})
		}
	}
	plan.Decision = rollback.Decide(mp.Classes, isServer)
	mp.HasIrreversible = plan.Decision.Irreversible
	plan.Migrations = mp
	// 备份计划。
	switch {
	case isServer && !mp.DBInitialized:
		plan.Backup = UpdateBackupPlan{Reason: "server 处于 setup mode（向导未完成）：没有数据库与业务数据，保留旧 release 即可回退"}
	case supportsBackup(req.Component):
		if req.NoBackup && isServer {
			return nil, &Error{Kind: KindSettings, Msg: "server 升级前必须备份，不能 --no-backup", SystemState: "unchanged"}
		}
		if req.NoBackup {
			plan.Backup = UpdateBackupPlan{Reason: "--no-backup：按用户要求跳过（将无法按备份回滚）"}
			plan.Warnings = append(plan.Warnings, Warning{Code: "update.no_backup", Component: req.Component, Step: UpStepBackup, Message: "已按 --no-backup 跳过升级前备份：失败时只能切回旧 release，无法按备份恢复"})
		} else {
			env, _ := in.updateEnv()
			enc := false
			if env.BackupPassphrase != nil {
				if p, perr := env.BackupPassphrase(ctx); perr == nil && len(p) > 0 {
					enc = true
				}
			}
			plan.Backup = UpdateBackupPlan{Planned: true, Dir: in.updateBackupDir(), Encrypted: enc, Database: isServer, Verify: plan.Decision.BackupVerified || isServer}
		}
	default:
		plan.Backup = UpdateBackupPlan{Reason: "该组件没有数据与备份支持：旧 release 目录保留作为快照（切回即回退）"}
	}
	// 迁移凭据可用性（不提示、不连库）：非交互且无任何来源 → 真实升级会在改动任何东西之前以退出码 3 拒绝。
	if isServer && mp.DBInitialized {
		plan.Credentials = in.credentialsStatus(ctx, req)
		if plan.Credentials == "missing_non_interactive" {
			plan.Warnings = append(plan.Warnings, Warning{Code: "update.db_credentials_missing", Component: "server", Step: UpStepMigratePlan,
				Message: "没有可用的迁移账号凭据且当前为非交互模式：真实升级将在改动任何东西之前以退出码 3 拒绝。请设置环境变量 " + backup.EnvDBPass + "、" + backup.EnvPassFile + "，或先执行 lingedge-server migrate credential init"})
		}
	}
	if plan.Decision.NeedConfirm {
		plan.Warnings = append(plan.Warnings, Warning{Code: "update.irreversible", Component: req.Component, Step: UpStepMigratePlan,
			Message: "本次升级含不可逆迁移：升级前强制备份并校验；交互需键入 confirm-irreversible，非交互需 --accept-irreversible；" + plan.Decision.Promise})
	}
	return plan, nil
}

// credentialsStatus 在不提示、不连库的前提下判断迁移凭据的可用性。
func (in *Installer) credentialsStatus(ctx context.Context, req UpdateRequest) string {
	env, err := in.updateEnv()
	if err != nil || env.DB == nil {
		return "missing_non_interactive"
	}
	conn, err := env.DB(ctx, false)
	if err == nil {
		conn.Close()
		return "available"
	}
	if errors.Is(err, backup.ErrNoCredentials) {
		if !req.NonInteractive {
			return "interactive"
		}
		return "missing_non_interactive"
	}
	return "unavailable"
}

// releaseBin 是（虚拟）release 里 server 二进制的路径。
func releaseBin(version string) string {
	return filepath.ToSlash(rollback.ReleaseDir("server", version)) + "/bin/lingedge-server"
}

var _ = dbmigrate.ClassExpand
var _ = manifest.SchemaVersion
