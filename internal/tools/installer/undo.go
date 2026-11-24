package installer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// installer 自己的撤销动作（registry.UndoAction.Action）。一步内的动作按记录顺序执行，记录者（materialize.go 的 plan*）负责排成「先依赖者、后被依赖者」的撤销顺序。service 包的动作（userdel/remove_dir_if_empty/remove_path/unlink_unit/
// systemctl_disable/systemctl_stop）由 service.Undoer 执行。
const (
	actRemoveStaging = "remove_staging" // staging/<tx>-<组件>：整棵删除
	actRemoveRelease = "remove_release" // /opt/lingedge/<c>/releases/<v>[.partial]：仅 Created=true，整棵删除
	actUnlinkCurrent = "unlink_current" // /opt/lingedge/<c>/current：仅 Created=true 且仍指向 Extra[target]
	actRemoveOptDir  = "remove_opt_dir" // /opt/lingedge、/opt/lingedge/<c>、/opt/lingedge/<c>/releases：仅 Created=true 且为空
	actStateForget   = "state_forget"   // 从 state.json 删除本事务写入的组件记录（LastTx == tx）
	actNoop          = actRequest       // 请求记录：撤销为空操作
	optRoot          = "/opt/lingedge"  // 与 resolver.Layout 默认值一致
	stateForgetKey   = "component"      // Extra 键
	stateForgetTx    = "tx"             // Extra 键
	targetKey        = "target"         // Extra 键
)

var (
	reReleasePath = regexp.MustCompile(`^/opt/lingedge/[a-z][a-z0-9-]{0,31}/releases/[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$`)
	reCurrentPath = regexp.MustCompile(`^/opt/lingedge/[a-z][a-z0-9-]{0,31}/current$`)
	reOptDirPath  = regexp.MustCompile(`^/opt/lingedge(/[a-z][a-z0-9-]{0,31}(/releases)?)?$`)
	reStagingName = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_-]{7,63}$`)
)

// Undoer 执行 journal 里的撤销动作。journal 内容在恢复时不被盲信：每个动作都校验路径形状（只认 Tools 自己创建的位置）、
// 只处理 Created=true 的对象、删除前确认类型，撤销是幂等的（目标已不存在即视为完成）。
type Undoer struct {
	Root string
	Reg  *registry.Registry
	Svc  *service.Undoer
	// Web 执行 nginx/openresty（撤销 conf.d 软链后 reload）；nil = 只摘软链不 reload。Notice 接收撤销过程中的非致命提示。
	Web    service.Runner
	Notice func(code, msg string)
	// Safety 执行 backup.ActRestoreSafety（见 Deps.RestoreSafety）。
	Safety func(ctx context.Context, a registry.UndoAction) (service.UndoResult, error)
}

func (u *Undoer) host(virtual string) string {
	if u.Root == "" {
		return virtual
	}
	return filepath.Join(u.Root, virtual)
}

// Apply 实现 UndoApplier。
func (u *Undoer) Apply(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
	if service.Handles(a.Action) {
		return u.Svc.Apply(ctx, a)
	}
	switch a.Action {
	case actNoop:
		return service.UndoResult{Outcome: service.OutcomeNoop}, nil
	case actRemoveStaging:
		return u.removeStaging(a)
	case actRemoveRelease:
		return u.removeRelease(a)
	case actUnlinkCurrent:
		return u.unlinkCurrent(a)
	case actRemoveOptDir:
		return u.removeOptDir(a)
	case actStateForget:
		return u.stateForget(ctx, a)
	case actNginxLink:
		return u.unlinkNginx(ctx, a)
	case backup.ActRestoreRequest, ActUninstallRequest:
		return service.UndoResult{Outcome: service.OutcomeNoop}, nil
	case backup.ActRestoreMasterKey:
		if !strings.HasSuffix(a.Path, service.ServerConfigDir+"/master.key") || filepath.Clean(a.Path) != a.Path {
			return service.UndoResult{}, fmt.Errorf("撤销路径 %s 不是 server 的 master.key，拒绝", a.Path)
		}
		return backup.UndoMasterKey(a)
	case backup.ActRestoreSafety:
		if u.Safety == nil {
			return service.UndoResult{}, fmt.Errorf("无法自动回滚恢复：本次调用没有装配数据库凭据通道。请手工执行：lingedge backup restore %s --replace（恢复前安全备份，加密时附 --passphrase-file）", a.Path)
		}
		return u.Safety(ctx, a)
	}
	return service.UndoResult{}, fmt.Errorf("未知的撤销动作 %q（journal 来自更新版本或被改动）", a.Action)
}

func (u *Undoer) removeStaging(a registry.UndoAction) (service.UndoResult, error) {
	dir := u.Reg.L.StagingDir()
	if filepath.Dir(a.Path) != dir || !reStagingName.MatchString(filepath.Base(a.Path)) {
		return service.UndoResult{}, fmt.Errorf("撤销路径 %s 不是 staging 目录下的事务目录，拒绝", a.Path)
	}
	st, err := os.Lstat(a.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return service.UndoResult{Outcome: service.OutcomeAbsent}, nil
	}
	if err != nil {
		return service.UndoResult{}, err
	}
	if !st.IsDir() {
		return service.UndoResult{Outcome: service.OutcomeSkipped, Detail: a.Path + " 已不是目录，未动"}, nil
	}
	if err := os.RemoveAll(a.Path); err != nil {
		return service.UndoResult{}, err
	}
	return service.UndoResult{Outcome: service.OutcomeDone}, nil
}

func (u *Undoer) removeRelease(a registry.UndoAction) (service.UndoResult, error) {
	base := strings.TrimSuffix(a.Path, ".partial")
	if !reReleasePath.MatchString(base) {
		return service.UndoResult{}, fmt.Errorf("撤销路径 %s 不是 release 目录，拒绝", a.Path)
	}
	if !a.Created {
		return service.UndoResult{Outcome: service.OutcomeNoop}, nil
	}
	real := u.host(a.Path)
	st, err := os.Lstat(real)
	if errors.Is(err, fs.ErrNotExist) {
		return service.UndoResult{Outcome: service.OutcomeAbsent}, nil
	}
	if err != nil {
		return service.UndoResult{}, err
	}
	if !st.IsDir() {
		return service.UndoResult{Outcome: service.OutcomeSkipped, Detail: a.Path + " 已不是真实目录，未动"}, nil
	}
	if err := os.RemoveAll(real); err != nil {
		return service.UndoResult{}, err
	}
	return service.UndoResult{Outcome: service.OutcomeDone}, nil
}

func (u *Undoer) unlinkCurrent(a registry.UndoAction) (service.UndoResult, error) {
	if !reCurrentPath.MatchString(a.Path) {
		return service.UndoResult{}, fmt.Errorf("撤销路径 %s 不是 current 链接，拒绝", a.Path)
	}
	if !a.Created {
		return service.UndoResult{Outcome: service.OutcomeNoop}, nil
	}
	real := u.host(a.Path)
	st, err := os.Lstat(real)
	if errors.Is(err, fs.ErrNotExist) {
		return service.UndoResult{Outcome: service.OutcomeAbsent}, nil
	}
	if err != nil {
		return service.UndoResult{}, err
	}
	if st.Mode()&fs.ModeSymlink == 0 {
		return service.UndoResult{Outcome: service.OutcomeSkipped, Detail: a.Path + " 已不是符号链接，未动"}, nil
	}
	if cur, rerr := os.Readlink(real); rerr != nil || cur != a.Extra[targetKey] {
		return service.UndoResult{Outcome: service.OutcomeSkipped, Detail: a.Path + " 已指向别处，未动"}, nil
	}
	if err := os.Remove(real); err != nil {
		return service.UndoResult{}, err
	}
	return service.UndoResult{Outcome: service.OutcomeDone}, nil
}

func (u *Undoer) removeOptDir(a registry.UndoAction) (service.UndoResult, error) {
	if !reOptDirPath.MatchString(a.Path) {
		return service.UndoResult{}, fmt.Errorf("撤销路径 %s 不是 /opt/lingedge 下的受管目录，拒绝", a.Path)
	}
	if !a.Created {
		return service.UndoResult{Outcome: service.OutcomeNoop}, nil
	}
	real := u.host(a.Path)
	st, err := os.Lstat(real)
	if errors.Is(err, fs.ErrNotExist) {
		return service.UndoResult{Outcome: service.OutcomeAbsent}, nil
	}
	if err != nil {
		return service.UndoResult{}, err
	}
	if !st.IsDir() {
		return service.UndoResult{Outcome: service.OutcomeSkipped, Detail: a.Path + " 已不是目录，未动"}, nil
	}
	if err := os.Remove(real); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return service.UndoResult{Outcome: service.OutcomeSkipped, Detail: a.Path + " 非空，已保留"}, nil
		}
		return service.UndoResult{}, err
	}
	return service.UndoResult{Outcome: service.OutcomeDone}, nil
}

func (u *Undoer) stateForget(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
	comp, tx := a.Extra[stateForgetKey], a.Extra[stateForgetTx]
	if !registry.ValidComponent(comp) || !registry.ValidTxID(tx) {
		return service.UndoResult{}, errors.New("state_forget 的组件或事务 ID 不合法")
	}
	// 先只读检查：没有本事务写入的记录就不动 state.json（UpdateState 无论有无改动都会写文件，不能让回滚凭空创建它）。
	if st, _, err := u.Reg.LoadState(); err != nil {
		return service.UndoResult{}, err
	} else if c, ok := st.Components[comp]; !ok || c.LastTx != tx {
		return service.UndoResult{Outcome: service.OutcomeAbsent}, nil
	}
	changed := false
	err := u.Reg.UpdateState(ctx, func(s *registry.State) error {
		if c, ok := s.Components[comp]; ok && c.LastTx == tx {
			delete(s.Components, comp)
			changed = true
		}
		return nil
	})
	if err != nil {
		return service.UndoResult{}, err
	}
	if !changed {
		return service.UndoResult{Outcome: service.OutcomeAbsent}, nil
	}
	return service.UndoResult{Outcome: service.OutcomeDone}, nil
}

// touchesUnits 报告一条已执行的撤销是否改动了单元文件（之后需要 daemon-reload，service.Undoer 文档约定由调用方做）。
func touchesUnits(a registry.UndoAction) bool {
	return a.Action == "unlink_unit" || (a.Action == "remove_path" && strings.HasPrefix(a.Path, service.UnitDir+"/"))
}
