package service

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

// DirSpec 是一个目录的最终属主与权限。
type DirSpec struct {
	Path  string
	Mode  fs.FileMode
	Owner string
	Group string
	Why   string // 出处（§8 / ADR-020 §2），写进计划输出与测试断言信息
	// Loose 用于多组件共用的父目录：既有目录仅在「权限宽于规格」或属主不符时才算不符（窄于规格不动它）。
	Loose bool
}

// 撤销动作名（registry.UndoAction.Action；执行见 Undoer）。
const (
	ActRemoveDirIfEmpty = "remove_dir_if_empty"
	ActRemovePath       = "remove_path"
	ActUnlinkUnit       = "unlink_unit"
	ActUserdel          = "userdel"
)

func ds(path string, mode fs.FileMode, owner, group, why string) DirSpec {
	return DirSpec{Path: path, Mode: mode, Owner: owner, Group: group, Why: why}
}

func etcRoot() DirSpec {
	return DirSpec{Path: "/etc/lingedge", Mode: 0o755, Owner: "root", Group: "root", Why: "§8 配置根", Loose: true}
}

func libRoot() DirSpec {
	return DirSpec{Path: "/var/lib/lingedge", Mode: 0o755, Owner: "root", Group: "root", Why: "§8 数据根（0755：使 Nginx 运行用户可读公开媒体目录，MIN-08）", Loose: true}
}

// ServerDirs 返回 Server 的目录计划（tools-design §8）：配置 root:<组> 0750；数据/日志 <运行用户> 0750。
func ServerDirs(user, group string) []DirSpec {
	return append([]DirSpec{etcRoot(), libRoot()},
		ds(ServerConfigDir, 0o750, "root", group, "§8 配置：root:<run-user> 0750（文件 0640，master.key 另见 SpecMasterKey）"),
		ds(ServerDataDir, 0o750, user, group, "§8 数据：<run-user> 0750"),
		ds(ServerLogDir, 0o750, user, group, "§8 日志：<run-user> 0750"),
	)
}

// NodeDirs 返回 Node 的目录计划（tools-design §8 + ADR-020 §2 权威总表）。
// 不创建的（由运行期产生）：generations/g<N>/ 的内容、current 符号链接、state.json、certs/<certificate_uid>/（Agent 渲染/落盘）、
// /run/lingedge-node（systemd RuntimeDirectory=）。升级结果目录 NodeUpgradeDir 见 UpgradeResultDir。
func NodeDirs(user, group string) []DirSpec {
	return append([]DirSpec{etcRoot()},
		ds(NodeConfigDir, 0o750, "root", group, "§8 配置：root:<run-user> 0750"),
		ds(NodeDataDir, 0o750, user, group, "§8 数据：<run-user> 0750"),
		ds(NodeDataDir+"/identity", 0o700, user, group, "ADR-020 §2：节点私钥/证书/CA 链，目录 0700、文件 0600"),
		ds(NodeDataDir+"/generations", 0o750, user, group, "ADR-020 §2：g<N>/ 由 Agent 渲染后只读"),
		ds(NodeDataDir+"/certs", 0o700, user, group, "ADR-020 §2：certs/<certificate_uid>/ 私钥 0600，不属于任何代"),
		ds(NodeDataDir+"/cache", 0o750, user, group, "ADR-020 §2：proxy_cache_path（绝对路径，不在代目录内）"),
		ds(NodeDataDir+"/runtime", 0o750, user, group, "ADR-020 §2：nginx 临时目录"),
		ds(NodeStagingDir, 0o750, user, group, "ADR-020 §2/ADR-024 §7：升级暂存，不可信域（Agent 只写、root 侧经 dirfd 读）"),
		ds(NodeDataDir+"/logs-spool", 0o750, user, group, "ADR-020 §2：上传前访问日志段与 rollup"),
		ds(NodeLogDir, 0o750, user, group, "§8 日志：<run-user> 0750"),
		ds(NodeLogDir+"/access", 0o750, user, group, "ADR-020 §2：OpenResty 访问日志（JSONL）"),
	)
}

// UpgradeResultDir 返回升级结果目录（BLK-07、ADR-024 §7.6）：root:root 0755，Agent 无写权限；Tools 安装 node 时创建。
func UpgradeResultDir() DirSpec {
	return ds(NodeUpgradeDir, 0o755, "root", "root", "BLK-07：root 属主，Agent 只读；结果文件 root:root 0644")
}

// DirResult 是一个目录的处理结果。
type DirResult struct {
	Path     string
	Created  bool
	Mismatch []string // 既有目录与规格不符之处（权限/属主）
	Repaired bool
}

// MismatchError 在存在既有目录与规格不符且未开 Repair 时返回；Items 逐项列出。
type MismatchError struct{ Items []string }

func (e *MismatchError) Error() string {
	return "service: 既有目录与规格不符：" + strings.Join(e.Items, "；")
}

// Ensurer 按规格创建目录。创建即最终权限：目录先以 0700 创建（security.MkdirAllPerm）、显式 chmod 为规格权限，再 chown——
// 在 chown 之前它属 root（Tools 以 root 运行），不会有「先宽后收」或「属主错误时已可读」的窗口。
// 既有目录不改动，除非 Repair：不符之处作为 MismatchError 返回（不静默放行宽于规格的权限）。
type Ensurer struct {
	Root   string // 非空时所有路径前缀到该目录
	Own    Ownership
	Repair bool
}

// Ensure 按序处理 specs（父在前）。返回每个目录的结果；有不符且未 Repair 时同时返回 *MismatchError（已创建的目录仍记录在结果里，便于撤销）。
func (e *Ensurer) Ensure(specs []DirSpec) ([]DirResult, error) {
	var out []DirResult
	var mism []string
	for _, sp := range specs {
		if err := security.ValidateUnitPath(sp.Path); err != nil { // 复用路径规范：绝对、无 ..、无重复斜杠
			return out, newErr(KindInvalidParam, "目录路径不合法：%v", err)
		}
		real := hostPath(e.Root, sp.Path)
		st, err := os.Lstat(real)
		switch {
		case err == nil:
			r, items, err := e.inspectExisting(sp, real, st)
			if err != nil {
				return out, err
			}
			out = append(out, r)
			mism = append(mism, items...)
		case os.IsNotExist(err):
			created, err := e.create(sp, real)
			out = append(out, created...)
			if err != nil {
				return out, err
			}
		default:
			return out, wrapErr(KindIO, err, "无法检查 %s", sp.Path)
		}
	}
	if len(mism) > 0 {
		return out, &MismatchError{Items: mism}
	}
	return out, nil
}

func (e *Ensurer) inspectExisting(sp DirSpec, real string, st fs.FileInfo) (DirResult, []string, error) {
	if st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() {
		return DirResult{}, nil, &Error{Kind: KindConflict, Path: sp.Path, Msg: "已存在但不是目录（符号链接/文件），拒绝跟随或覆盖"}
	}
	res := DirResult{Path: sp.Path}
	var items []string
	if got := st.Mode().Perm(); got != sp.Mode && (!sp.Loose || got&^sp.Mode != 0) {
		items = append(items, sp.Path+" 权限 "+fmtMode(got)+" 应为 "+fmtMode(sp.Mode))
	}
	wantUID, wantGID, rerr := e.Own.Resolve(sp.Owner, sp.Group)
	if uid, gid, ok := ownerOf(st); ok && rerr == nil && (uid != wantUID || gid != wantGID) {
		items = append(items, sp.Path+" 属主 "+itoa(uid)+":"+itoa(gid)+" 应为 "+sp.Owner+":"+sp.Group)
	}
	if len(items) == 0 {
		return res, nil, nil
	}
	if !e.Repair {
		res.Mismatch = items
		return res, items, nil
	}
	if rerr != nil {
		return res, nil, rerr
	}
	if err := os.Chmod(real, sp.Mode); err != nil {
		return res, nil, wrapErr(KindPermission, err, "无法修正 %s 的权限", sp.Path)
	}
	if err := e.Own.Chown(real, wantUID, wantGID); err != nil {
		return res, nil, wrapErr(KindPermission, err, "无法修正 %s 的属主", sp.Path)
	}
	res.Mismatch, res.Repaired = items, true
	return res, nil, nil
}

// create 创建 sp（及缺失的祖先，祖先按 root:root 0755）。返回按创建顺序排列的结果。
func (e *Ensurer) create(sp DirSpec, real string) ([]DirResult, error) {
	var out []DirResult
	var missing []string // 由近到远
	for p := filepath.Dir(real); ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil || p == filepath.Dir(p) {
			break
		}
		missing = append(missing, p)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := security.MkdirAllPerm(missing[i], 0o755); err != nil {
			return out, wrapErr(KindIO, err, "无法创建 %s", e.virtual(missing[i]))
		}
		if err := e.chown(missing[i], "root", "root"); err != nil {
			return out, err
		}
		out = append(out, DirResult{Path: e.virtual(missing[i]), Created: true})
	}
	if err := security.MkdirAllPerm(real, sp.Mode); err != nil {
		return out, wrapErr(KindIO, err, "无法创建 %s", sp.Path)
	}
	if err := e.chown(real, sp.Owner, sp.Group); err != nil {
		return out, err
	}
	return append(out, DirResult{Path: sp.Path, Created: true}), nil
}

func (e *Ensurer) chown(real, owner, group string) error {
	uid, gid, err := e.Own.Resolve(owner, group)
	if err != nil {
		return err
	}
	if err := e.Own.Chown(real, uid, gid); err != nil {
		return wrapErr(KindPermission, err, "无法设置 %s 的属主为 %s:%s", e.virtual(real), owner, group)
	}
	return nil
}

func (e *Ensurer) virtual(real string) string {
	if e.Root == "" {
		return real
	}
	return "/" + strings.TrimPrefix(strings.TrimPrefix(real, filepath.Clean(e.Root)), "/")
}

// DirUndo 把结果转为撤销信息：只有本事务创建的目录才记 Created，顺序由深到浅（恢复时先删子目录）。
func DirUndo(results []DirResult) []registry.UndoAction {
	var out []registry.UndoAction
	for i := len(results) - 1; i >= 0; i-- {
		if results[i].Created {
			out = append(out, registry.UndoAction{Action: ActRemoveDirIfEmpty, Path: results[i].Path, Created: true})
		}
	}
	return out
}

// DirPlan 是一个目录规格的只读规划结果（不创建、不修改任何东西）。
type DirPlan struct {
	Path string
	// Exists：目标目录已存在（Create 为空）。
	Exists bool
	// Mismatch：既有目录与规格不符之处（权限宽于规格、属主不符）；Ensure 会把它作为 *MismatchError 报告，Repair 才会修正。
	Mismatch []string
	// Create：Ensure 将会创建的目录（缺失的祖先自上而下，最后是目标自身）。
	Create []string
}

// Plan 只读地预演 Ensure：返回每个规格将创建哪些目录、既有目录有哪些不符。installer 用它在「先写 undo 再动手」之前判定 Created、
// 并在预检阶段把不符作为需要确认的冲突报告（不静默放行宽于规格的权限）。同一批 specs 里前面创建的目录视为后面规格的既有祖先，与 Ensure 的顺序语义一致。
func (e *Ensurer) Plan(specs []DirSpec) ([]DirPlan, error) {
	willExist := map[string]bool{}
	probe := *e
	probe.Repair = false
	var out []DirPlan
	for _, sp := range specs {
		if err := security.ValidateUnitPath(sp.Path); err != nil {
			return out, newErr(KindInvalidParam, "目录路径不合法：%v", err)
		}
		real := hostPath(e.Root, sp.Path)
		dp := DirPlan{Path: sp.Path}
		st, err := os.Lstat(real)
		switch {
		case err == nil:
			dp.Exists = true
			_, items, ierr := probe.inspectExisting(sp, real, st)
			if ierr != nil {
				return out, ierr
			}
			dp.Mismatch = items
		case os.IsNotExist(err):
			if willExist[sp.Path] {
				dp.Exists = true
				break
			}
			var missing []string
			for p := filepath.Dir(sp.Path); p != filepath.Dir(p); p = filepath.Dir(p) {
				if willExist[p] {
					break
				}
				if _, serr := os.Lstat(hostPath(e.Root, p)); serr == nil {
					break
				}
				missing = append([]string{p}, missing...)
			}
			dp.Create = append(missing, sp.Path)
			for _, c := range dp.Create {
				willExist[c] = true
			}
		default:
			return out, wrapErr(KindIO, err, "无法检查 %s", sp.Path)
		}
		out = append(out, dp)
	}
	return out, nil
}

// PlanUndo 把 Plan 的结果转成撤销信息（与 DirUndo 同形：只有将被创建的目录 Created=true，由深到浅）。
func PlanUndo(plans []DirPlan) []registry.UndoAction {
	var all []string
	for _, p := range plans {
		all = append(all, p.Create...)
	}
	out := make([]registry.UndoAction, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		out = append(out, registry.UndoAction{Action: ActRemoveDirIfEmpty, Path: all[i], Created: true})
	}
	return out
}
