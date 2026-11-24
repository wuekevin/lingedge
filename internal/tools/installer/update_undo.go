package installer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/rollback"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// journal 内容在恢复时不被盲信：路径形状、版本号、单元名、数值都经校验后才执行。
var (
	reNewBin     = regexp.MustCompile(`^/opt/lingedge/server/releases/[0-9A-Za-z][0-9A-Za-z.+-]{0,63}/bin/lingedge-server$`)
	reBackupFile = regexp.MustCompile(`^lingedge-backup-.+-(server|node)-[0-9]{8}T[0-9]{6}Z(-[0-9]+)?\.tar\.gz(\.enc)?$`)
)

func (u *Undoer) updExec() (*rollback.Executor, error) {
	if u.Update == nil || u.Update.Rollback == nil {
		return nil, errors.New("本次调用没有装配回滚执行器，无法撤销升级事务（请用 lingedge recover --rollback 重试）")
	}
	return u.Update.Rollback, nil
}

// updStartUnits 撤销「停止服务」：升级前是 active 的单元重新启动（此时 current 已被前面的撤销切回旧版本、数据库也已回退）。幂等（已在运行则无事发生）。
func (u *Undoer) updStartUnits(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
	units, err := parseUnitNames(a.Extra["units"])
	if err != nil {
		return service.UndoResult{}, err
	}
	if a.Extra["was_active"] != "true" || len(units) == 0 {
		return service.UndoResult{Outcome: service.OutcomeNoop}, nil
	}
	if u.Sys == nil {
		return service.UndoResult{}, errors.New("未配置 systemd 控制")
	}
	if _, err := u.Sys.Start(ctx, units...); err != nil {
		return service.UndoResult{}, fmt.Errorf("重新启动旧版本服务失败：%w", err)
	}
	return service.UndoResult{Outcome: service.OutcomeDone, Detail: "已重新启动 " + a.Extra["units"]}, nil
}

// updSwitchCurrent 撤销「切换 current」：切回旧版本（原子；已经指向则幂等）。
func (u *Undoer) updSwitchCurrent(a registry.UndoAction) (service.UndoResult, error) {
	ex, err := u.updExec()
	if err != nil {
		return service.UndoResult{}, err
	}
	comp, to := a.Extra["component"], a.Extra["to"]
	if !registry.ValidComponent(comp) {
		return service.UndoResult{}, errors.New("update_switch_current 的组件名不合法")
	}
	if v, perr := manifest.ParseVersion(to); perr != nil || v.String() != to {
		return service.UndoResult{}, errors.New("update_switch_current 的目标版本不合法")
	}
	cur, err := ex.CurrentVersion(comp)
	if err != nil {
		return service.UndoResult{}, err
	}
	if cur == to {
		return service.UndoResult{Outcome: service.OutcomeAbsent, Detail: "current 已指向 " + to}, nil
	}
	if _, err := ex.SwitchCurrent(comp, to); err != nil {
		return service.UndoResult{}, err
	}
	return service.UndoResult{Outcome: service.OutcomeDone, Detail: "current 已切回 " + to}, nil
}

// updDBRevert 撤销「migrate up」：按迁移级别回退数据库，数据库现状自适应；需要迁移账号凭据（来源同升级：环境变量/口令文件/migrate.yaml，非交互——
// 取不到则明确失败并保留 journal，绝不假装成功）。
func (u *Undoer) updDBRevert(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
	ex, err := u.updExec()
	if err != nil {
		return service.UndoResult{}, err
	}
	x := a.Extra
	if x["component"] != "server" {
		return service.UndoResult{}, errors.New("update_db_revert 的组件不合法")
	}
	if !reNewBin.MatchString(x["new_bin"]) {
		return service.UndoResult{}, errors.New("update_db_revert 的二进制路径不合法，拒绝")
	}
	pre, e1 := strconv.ParseInt(x["pre"], 10, 64)
	tgt, e2 := strconv.ParseInt(x["target"], 10, 64)
	if e1 != nil || e2 != nil || pre < 0 || tgt < 0 {
		return service.UndoResult{}, errors.New("update_db_revert 的 schema 版本不合法")
	}
	switch x["path"] {
	case registry.PathMigrateDown, registry.PathBinaryExpand, registry.PathRestoreBackup:
	default:
		return service.UndoResult{}, errors.New("update_db_revert 的回滚路径不合法")
	}
	if b := x["backup"]; b != "" && (!filepath.IsAbs(b) || filepath.Clean(b) != b || !reBackupFile.MatchString(filepath.Base(b))) {
		return service.UndoResult{}, errors.New("update_db_revert 的备份路径不合法，拒绝")
	}
	if pre == 0 && x["path"] != registry.PathBinary {
		return service.UndoResult{Outcome: service.OutcomeNoop, Detail: "升级前数据库没有迁移版本记录，无需回退"}, nil
	}
	out, err := ex.RevertDB(ctx, rollback.RevertParams{Component: "server", Path: x["path"], NewBin: x["new_bin"], PreSchema: pre, TargetSchema: tgt, Backup: x["backup"],
		BackupEncrypted: x["encrypted"] == "true", AllowRestore: x["allow_restore"] != "false"})
	if err != nil {
		return service.UndoResult{}, err
	}
	oc := service.OutcomeDone
	if out.Action == "none" {
		oc = service.OutcomeAbsent
	}
	return service.UndoResult{Outcome: oc, Detail: out.Detail}, nil
}
