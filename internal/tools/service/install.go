package service

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

// InstallStatus 是单元安装的结果类别。
type InstallStatus string

// InstallStatus 取值。
const (
	// StatusInstalled：目标不存在，已按 root:root 0644 创建。
	StatusInstalled InstallStatus = "installed"
	// StatusUnchanged：目标已存在且内容一致，视为幂等完成（FR-6）。
	StatusUnchanged InstallStatus = "unchanged"
	// StatusNewWritten：目标已存在且内容不同——不覆盖，新内容写入 <unit>.new 并给出警告（FR-6）。
	StatusNewWritten InstallStatus = "new_written"
)

const maxUnitFile = 1 << 20

// Installer 把渲染好的单元写入单元目录。Root 非空时所有路径前缀到该目录（测试/离线根）。
type Installer struct {
	Root string
	Own  Ownership
}

// InstallResult 是一个单元的安装结果。
type InstallResult struct {
	Unit    UnitName
	Status  InstallStatus
	Path    string // 目标单元文件路径
	NewPath string // StatusNewWritten 时的 .new 路径
	Warning string // 面向用户的警告（可空）
	// Undo 可直接写入 journal 的 undo：只有本次新建的文件才带 Created=true（撤销时只删除本事务创建的东西）。
	Undo []registry.UndoAction
}

// Install 安装一个单元：不存在 → 创建；已存在且内容一致 → 幂等完成；已存在且内容不同 → 不覆盖、写 .new 并警告。
// 创建经 registry.EnsureFile（不可能覆盖并发创建的同名文件）；.new 是 Tools 自己的暂存产物，原子替换。
// 调用方在一批单元装完后再执行 Systemctl.DaemonReload。
func (in *Installer) Install(u Unit) (InstallResult, error) {
	if !u.Name.Valid() {
		return InstallResult{}, &Error{Kind: KindInvalidParam, Field: "unit", Msg: "不是 LingEdge 已知单元"}
	}
	if len(u.Content) == 0 || len(u.Content) > maxUnitFile {
		return InstallResult{}, newErr(KindInvalidParam, "单元内容为空或过大：%s", u.Name)
	}
	dir := hostPath(in.Root, UnitDir)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return InstallResult{}, &Error{Kind: KindIO, Path: UnitDir, Msg: "systemd 单元目录不存在（本机不是 systemd 系统？）", Err: err}
	}
	virt := UnitDir + "/" + string(u.Name)
	target := filepath.Join(dir, string(u.Name))
	res := InstallResult{Unit: u.Name, Path: virt}

	st, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		created, err := registry.EnsureFile(target, u.Content, SpecUnit.Mode)
		if err != nil {
			var re *registry.Error
			if errors.As(err, &re) && re.Kind == registry.KindConflict { // 并发新建了不同内容：按「已存在且不同」处理
				return in.writeNew(u, target, virt, res)
			}
			return InstallResult{}, wrapErr(KindIO, err, "无法创建 %s", virt)
		}
		if created {
			if err := in.chownRoot(target); err != nil {
				_ = os.Remove(target)
				return InstallResult{}, err
			}
			res.Status = StatusInstalled
			res.Undo = []registry.UndoAction{{Action: ActUnlinkUnit, Path: virt, Created: true, SHA256: sha256Hex(u.Content)}}
		} else {
			res.Status = StatusUnchanged
		}
		return res, nil
	case err != nil:
		return InstallResult{}, wrapErr(KindIO, err, "无法检查 %s", virt)
	}

	if st.Mode().IsRegular() {
		same, err := sameContent(target, u.Content)
		if err != nil {
			return InstallResult{}, wrapErr(KindIO, err, "无法读取 %s", virt)
		}
		if same {
			res.Status = StatusUnchanged
			return res, nil
		}
	}
	return in.writeNew(u, target, virt, res) // 内容不同，或目标是符号链接/其他类型（不跟随、不覆盖）
}

func (in *Installer) writeNew(u Unit, target, virt string, res InstallResult) (InstallResult, error) {
	newTarget := target + ".new"
	res.NewPath = virt + ".new"
	res.Status = StatusNewWritten
	res.Warning = "单元 " + string(u.Name) + " 已存在且内容不同：未覆盖；新内容已写入 " + res.NewPath + "，请人工比对合并后执行 systemctl daemon-reload（FR-6）"
	st, err := os.Lstat(newTarget)
	existed := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return InstallResult{}, wrapErr(KindIO, err, "无法检查 %s", res.NewPath)
	}
	if existed && st.Mode().IsRegular() {
		if same, err := sameContent(newTarget, u.Content); err == nil && same {
			return res, nil // 上次已写过同样的 .new
		}
	}
	if err := security.WriteFileAtomic(newTarget, u.Content, SpecUnit.Mode); err != nil {
		return InstallResult{}, wrapErr(KindIO, err, "无法写入 %s", res.NewPath)
	}
	if err := in.chownRoot(newTarget); err != nil {
		_ = os.Remove(newTarget)
		return InstallResult{}, err
	}
	if !existed {
		res.Undo = []registry.UndoAction{{Action: ActRemovePath, Path: res.NewPath, Created: true, SHA256: sha256Hex(u.Content)}}
	}
	return res, nil
}

func (in *Installer) chownRoot(path string) error {
	uid, gid, err := in.Own.Resolve(SpecUnit.Owner, SpecUnit.Group)
	if err != nil {
		return err
	}
	if err := in.Own.Chown(path, uid, gid); err != nil {
		return wrapErr(KindPermission, err, "无法设置 %s 的属主为 root:root", path)
	}
	return nil
}

func sameContent(path string, want []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	got, err := io.ReadAll(io.LimitReader(f, maxUnitFile+1))
	if err != nil {
		return false, err
	}
	return bytes.Equal(got, want), nil
}

// PendingNewUnits 列出单元目录里遗留的 `<lingedge-unit>.new`（FR-6 的「待人工合并」），供 doctor 告警（1.42）。
func PendingNewUnits(root string) []Finding {
	var out []Finding
	for _, u := range AllUnits {
		p := filepath.Join(hostPath(root, UnitDir), string(u)+".new")
		if _, err := os.Lstat(p); err == nil {
			out = append(out, Finding{ID: "service.unit_new_pending", Category: "service", Level: LevelWarn,
				Message: "存在待合并的单元文件 " + UnitDir + "/" + string(u) + ".new",
				Hint:    "比对 " + string(u) + " 与 .new 后合并（或删除 .new），再执行 systemctl daemon-reload"})
		}
	}
	return out
}
