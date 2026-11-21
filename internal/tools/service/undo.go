package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

// ErrUnknownAction 表示 Action 不属于本包（调用方可交给其他步骤的实现者）。
var ErrUnknownAction = errors.New("service: 不是本包的撤销动作")

// UndoOutcome 是一次撤销的结果。
type UndoOutcome string

// UndoOutcome 取值。
const (
	OutcomeDone    UndoOutcome = "done"    // 已撤销
	OutcomeAbsent  UndoOutcome = "absent"  // 目标已不存在（撤销幂等）
	OutcomeNoop    UndoOutcome = "noop"    // Created=false：不是本事务创建的，不动
	OutcomeSkipped UndoOutcome = "skipped" // 有意不动（内容已被改过 / 目录非空），Detail 说明
)

// UndoResult 是 Undoer.Apply 的返回。
type UndoResult struct {
	Outcome UndoOutcome
	Detail  string
}

// Undoer 执行本包产出的 registry.UndoAction（Action ∈ userdel / remove_dir_if_empty / remove_path / unlink_unit /
// systemctl_disable / systemctl_stop）。原则：只撤销本事务创建的东西（Created=true）；journal 内容在恢复时不被盲信——
// 路径必须落在 Tools 管理的目录内、单元名与用户名必须是已知集合、删除文件前复核内容哈希。
// 一批单元被撤销后，调用方需再执行一次 Systemctl.DaemonReload。
type Undoer struct {
	Root string // 非空时路径前缀到该目录（与创建时一致）
	Prov *Provisioner
	Sys  *Systemctl
}

// Handles 报告 action 是否由本包执行。
func Handles(action string) bool {
	switch action {
	case ActUserdel, ActRemoveDirIfEmpty, ActRemovePath, ActUnlinkUnit, ActSystemctlDisable, ActSystemctlStop:
		return true
	}
	return false
}

// managedPrefixes 是 remove_path / remove_dir_if_empty 允许触及的目录（含自身）。Tools 自己的状态目录
// /var/lib/lingedge/tools 被显式排除：那里的内容归 registry 管。
var managedPrefixes = []string{
	"/etc/lingedge", "/var/lib/lingedge", "/var/lib/lingedge-node", NodeUpgradeDir, "/var/log/lingedge", NodeLogDir,
}

func under(p, prefix string) bool { return p == prefix || strings.HasPrefix(p, prefix+"/") }

func managedPath(p string) error {
	if err := security.ValidateUnitPath(p); err != nil {
		return newErr(KindUndo, "撤销路径不合法：%v", err)
	}
	if under(p, "/var/lib/lingedge/tools") {
		return newErr(KindUndo, "撤销路径 %s 属于 Tools 状态目录，拒绝", p)
	}
	for _, m := range managedPrefixes {
		if under(p, m) {
			return nil
		}
	}
	return newErr(KindUndo, "撤销路径 %s 不在 Tools 管理的目录内，拒绝", p)
}

func unitFilePath(p string) error {
	dir, base := filepath.Split(p)
	if strings.TrimSuffix(dir, "/") != UnitDir {
		return newErr(KindUndo, "撤销路径 %s 不在单元目录内，拒绝", p)
	}
	if !UnitName(strings.TrimSuffix(base, ".new")).Valid() {
		return newErr(KindUndo, "撤销路径 %s 不是 LingEdge 单元，拒绝", p)
	}
	return nil
}

// Apply 执行一条撤销。不属于本包的 Action 返回 ErrUnknownAction。
func (u *Undoer) Apply(ctx context.Context, a registry.UndoAction) (UndoResult, error) {
	switch a.Action {
	case ActUserdel:
		return u.undoUser(ctx, a)
	case ActRemoveDirIfEmpty:
		return u.undoDir(a)
	case ActRemovePath:
		if strings.HasPrefix(a.Path, UnitDir+"/") {
			if err := unitFilePath(a.Path); err != nil {
				return UndoResult{}, err
			}
		} else if err := managedPath(a.Path); err != nil {
			return UndoResult{}, err
		}
		return u.undoFile(a)
	case ActUnlinkUnit:
		if err := unitFilePath(a.Path); err != nil {
			return UndoResult{}, err
		}
		return u.undoFile(a)
	case ActSystemctlDisable, ActSystemctlStop:
		return u.undoSystemctl(ctx, a)
	}
	return UndoResult{}, ErrUnknownAction
}

func (u *Undoer) undoUser(ctx context.Context, a registry.UndoAction) (UndoResult, error) {
	name := a.Extra["user"]
	if !isRunUser(name) {
		return UndoResult{}, newErr(KindUndo, "撤销用户名不是 LingEdge 运行用户")
	}
	if !a.Created {
		return UndoResult{Outcome: OutcomeNoop}, nil
	}
	if u.Prov == nil {
		return UndoResult{}, newErr(KindUndo, "未提供 Provisioner")
	}
	if err := u.Prov.userdel(ctx, name); err != nil {
		return UndoResult{}, err
	}
	return UndoResult{Outcome: OutcomeDone}, nil
}

func (u *Undoer) undoDir(a registry.UndoAction) (UndoResult, error) {
	if err := managedPath(a.Path); err != nil {
		return UndoResult{}, err
	}
	if !a.Created {
		return UndoResult{Outcome: OutcomeNoop}, nil
	}
	real := hostPath(u.Root, a.Path)
	st, err := os.Lstat(real)
	if errors.Is(err, fs.ErrNotExist) {
		return UndoResult{Outcome: OutcomeAbsent}, nil
	}
	if err != nil {
		return UndoResult{}, wrapErr(KindIO, err, "无法检查 %s", a.Path)
	}
	if !st.IsDir() {
		return UndoResult{Outcome: OutcomeSkipped, Detail: a.Path + " 已不是目录，未动"}, nil
	}
	// os.Remove 对目录只在为空时成功；非空即有人写入了内容，留下它（撤销不删除用户数据）。
	if err := os.Remove(real); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return UndoResult{Outcome: OutcomeSkipped, Detail: a.Path + " 非空，已保留"}, nil
		}
		return UndoResult{}, wrapErr(KindIO, err, "无法删除 %s", a.Path)
	}
	return UndoResult{Outcome: OutcomeDone}, nil
}

func (u *Undoer) undoFile(a registry.UndoAction) (UndoResult, error) {
	if !a.Created {
		return UndoResult{Outcome: OutcomeNoop}, nil
	}
	real := hostPath(u.Root, a.Path)
	st, err := os.Lstat(real)
	if errors.Is(err, fs.ErrNotExist) {
		return UndoResult{Outcome: OutcomeAbsent}, nil
	}
	if err != nil {
		return UndoResult{}, wrapErr(KindIO, err, "无法检查 %s", a.Path)
	}
	if !st.Mode().IsRegular() {
		return UndoResult{Outcome: OutcomeSkipped, Detail: a.Path + " 已不是普通文件，未动"}, nil
	}
	if a.SHA256 != "" {
		b, err := os.ReadFile(real)
		if err != nil {
			return UndoResult{}, wrapErr(KindIO, err, "无法读取 %s", a.Path)
		}
		if sha256Hex(b) != a.SHA256 {
			return UndoResult{Outcome: OutcomeSkipped, Detail: a.Path + " 内容已被改动，未删除"}, nil
		}
	}
	if err := os.Remove(real); err != nil {
		return UndoResult{}, wrapErr(KindIO, err, "无法删除 %s", a.Path)
	}
	return UndoResult{Outcome: OutcomeDone}, nil
}

func (u *Undoer) undoSystemctl(ctx context.Context, a registry.UndoAction) (UndoResult, error) {
	unit := UnitName(a.Path)
	if !unit.Valid() {
		return UndoResult{}, newErr(KindUndo, "撤销单元名不是 LingEdge 单元")
	}
	if !a.Created {
		return UndoResult{Outcome: OutcomeNoop}, nil
	}
	if u.Sys == nil {
		return UndoResult{}, newErr(KindUndo, "未提供 Systemctl")
	}
	var err error
	if a.Action == ActSystemctlDisable {
		err = u.Sys.Disable(ctx, unit)
	} else {
		err = u.Sys.Stop(ctx, unit)
	}
	if err != nil {
		return UndoResult{}, err
	}
	return UndoResult{Outcome: OutcomeDone}, nil
}
