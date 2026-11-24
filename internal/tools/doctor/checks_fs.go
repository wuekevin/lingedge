package doctor

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/wuekevin/lingedge/internal/tools/service"
)

// permSpec 是对一个路径的权限期望：Max 是允许的最宽权限位（宽于它 = 偏差），Owner/Group 是期望属主。
type permSpec struct {
	Path     string
	Dir      bool
	Max      fs.FileMode
	Owner    string
	Group    string // 空 = 不核对属组
	Optional bool   // 不存在时不算问题
}

// permIssue 是一个偏差。Hard=false 的是「窄于规格」之类的软问题（告警，不修复）。
type permIssue struct {
	Path    string
	Desc    string
	Hard    bool
	FixMode fs.FileMode // 非 0 = 可修复的权限偏差（chmod 到该值）
	FixOwn  bool        // 可修复的属主偏差
	uid     int
	gid     int
}

func fmtMode(m fs.FileMode) string { return fmt.Sprintf("%04o", uint32(m&0o7777)) }

// evalPerm 只读评估 sp。
func (w *World) evalPerm(sp permSpec) []permIssue {
	st, err := os.Lstat(w.path(sp.Path))
	if err != nil {
		if os.IsNotExist(err) {
			if sp.Optional {
				return nil
			}
			return []permIssue{{Path: sp.Path, Desc: sp.Path + " 不存在", Hard: true}}
		}
		return []permIssue{{Path: sp.Path, Desc: sp.Path + " 无法检查（" + errText(err) + "）", Hard: true}}
	}
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		return []permIssue{{Path: sp.Path, Desc: sp.Path + " 是符号链接（Tools 不跟随，也不会修复）", Hard: true}}
	case sp.Dir && !st.IsDir():
		return []permIssue{{Path: sp.Path, Desc: sp.Path + " 不是目录", Hard: true}}
	case !sp.Dir && !st.Mode().IsRegular():
		return []permIssue{{Path: sp.Path, Desc: sp.Path + " 不是普通文件", Hard: true}}
	}
	var out []permIssue
	perm := st.Mode().Perm() | st.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)
	cur := st.Mode().Perm()
	if perm&^sp.Max != 0 {
		out = append(out, permIssue{Path: sp.Path, Hard: true, FixMode: cur & sp.Max,
			Desc: fmt.Sprintf("%s 权限 %s 宽于 %s", sp.Path, fmtMode(st.Mode()&0o7777), fmtMode(sp.Max))})
	} else if cur != sp.Max && sp.Max&^cur != 0 && !sp.Dir && sp.Max == 0o640 {
		out = append(out, permIssue{Path: sp.Path, Desc: fmt.Sprintf("%s 权限 %s 窄于规格 %s（运行用户可能读不到）", sp.Path, fmtMode(cur), fmtMode(sp.Max))})
	}
	if uid, gid, ok := ownerOf(st); ok && w.Own != nil && sp.Owner != "" {
		group := sp.Group
		if group == "" {
			group = sp.Owner
		}
		if wu, wg, rerr := w.Own.Resolve(sp.Owner, group); rerr == nil {
			bad := uid != wu || (sp.Group != "" && gid != wg)
			if bad {
				g := sp.Group
				if g == "" {
					g = "（不限）"
				}
				iss := permIssue{Path: sp.Path, Hard: true, FixOwn: true, uid: wu, gid: wg,
					Desc: fmt.Sprintf("%s 属主 %d:%d 应为 %s:%s", sp.Path, uid, gid, sp.Owner, g)}
				if sp.Group == "" {
					iss.gid = gid // 不限属组：只改 uid，gid 保持
				}
				out = append(out, iss)
			}
		}
	}
	return out
}

// fixIssue 修复一个偏差：chmod 经 O_NOFOLLOW 打开的句柄（不会因为路径被换成符号链接而改到别处），chown 用 Lchown（不跟随最后一级符号链接）。
func (w *World) fixIssue(is permIssue) error {
	p := w.path(is.Path)
	if is.FixMode != 0 {
		flags := os.O_RDONLY | syscall.O_NOFOLLOW
		f, err := os.OpenFile(p, flags, 0)
		if err != nil {
			return err
		}
		err = f.Chmod(is.FixMode)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	if is.FixOwn && w.Own != nil {
		if err := w.Own.Chown(p, is.uid, is.gid); err != nil {
			return err
		}
	}
	return nil
}

// permFinding 评估一组路径并（按需）修复。
func (w *World) permFinding(id, comp string, specs []permSpec, okMsg, hint string) Finding {
	x := Finding{ID: id, Component: comp}
	var issues []permIssue
	for _, sp := range specs {
		issues = append(issues, w.evalPerm(sp)...)
	}
	var fixed, failedFix []string
	if w.FixPerms && w.Euid == 0 {
		var planned []string
		for _, is := range issues {
			if !is.Hard || (is.FixMode == 0 && !is.FixOwn) {
				continue
			}
			if w.DryRun {
				planned = append(planned, is.Desc)
				continue
			}
			if err := w.fixIssue(is); err != nil {
				failedFix = append(failedFix, is.Desc+"（修复失败："+errText(err)+"）")
				continue
			}
			fixed = append(fixed, is.Desc)
		}
		if len(fixed) > 0 {
			// 复检：修复后重新评估，结果才是最终级别。
			issues = nil
			for _, sp := range specs {
				issues = append(issues, w.evalPerm(sp)...)
			}
			x.Fixed = true
		}
		if len(planned) > 0 {
			x.Details = map[string]any{"would_fix": planned}
		}
	}
	var hard, soft []string
	for _, is := range issues {
		if is.Hard {
			hard = append(hard, is.Desc)
		} else {
			soft = append(soft, is.Desc)
		}
	}
	hard = append(hard, failedFix...)
	d := x.Details
	if d == nil {
		d = map[string]any{}
	}
	if len(fixed) > 0 {
		d["fixed"] = fixed
	}
	if len(hard)+len(soft) > 0 {
		d["issues"] = append(append([]string{}, hard...), soft...)
	}
	if len(d) > 0 {
		x.Details = d
	}
	switch {
	case len(hard) > 0:
		x.Level, x.Message, x.Hint = LevelFail, strings.Join(hard, "；"), hint
		if len(fixed) > 0 {
			x.Message += "（已修复其余 " + fmt.Sprint(len(fixed)) + " 项）"
		}
	case len(soft) > 0:
		x.Level, x.Message, x.Hint = LevelWarn, strings.Join(soft, "；"), hint
	default:
		x.Level, x.Message = LevelPass, okMsg
		if len(fixed) > 0 {
			x.Message = "已修复 " + fmt.Sprint(len(fixed)) + " 项权限偏差：" + strings.Join(fixed, "；")
		}
	}
	return x
}

func runFS(ctx context.Context, w *World, f Filter) []Finding {
	var out []Finding
	want := func(names ...string) bool {
		if f.Component == "" {
			return true
		}
		for _, n := range names {
			if f.Component == n {
				return true
			}
		}
		return false
	}
	_, hasServer := w.component("server")
	_, hasNode := w.component("node")
	if hasServer && want("server") {
		out = append(out, w.masterKeyFinding())
	}
	if specs := w.configSpecs(f); len(specs) > 0 {
		comp := ""
		if f.Component != "" {
			comp = f.Component
		}
		out = append(out, w.permFinding("fs.config_perm", comp, specs, "配置文件与目录的权限、属主符合规格", "chmod/chown 到规格，或 lingedge doctor --fix-perms（只修复 master.key、配置与节点身份）"))
	}
	if hasNode && want("node") {
		out = append(out, w.identityFinding())
	}
	for _, c := range w.installed(f) {
		out = append(out, w.releaseOwner(c))
	}
	if f.Component == "" {
		if x, ok := w.dirsFinding(); ok {
			out = append(out, x)
		}
		out = append(out, w.stateDirFinding())
	}
	return out
}

func (w *World) masterKeyFinding() Finding {
	const id = "fs.master_key_perm"
	mk := service.ServerConfigDir + "/master.key"
	sp := permSpec{Path: mk, Max: 0o440, Owner: "root"}
	mode := "未知"
	if b, err := os.ReadFile(w.path(service.UnitDir + "/" + string(service.UnitServer))); err == nil {
		if strings.Contains(string(b), "\nLoadCredential=") || strings.HasPrefix(string(b), "LoadCredential=") {
			sp.Max, sp.Group, mode = 0o400, "root", "LoadCredential（root:root 0400）"
		} else {
			sp.Max, sp.Group, mode = 0o440, service.UserServer, "密钥文件降级（root:lingedge 0440）"
		}
	}
	x := w.permFinding(id, "server", []permSpec{sp}, "master.key 权限与属主符合规格（"+mode+"）",
		"master.key 必须 root 属主且不对 other 开放；lingedge doctor --fix-perms 只修正权限与属主，绝不读取、重建或删除它；丢失请用 backup restore --master-key-file 恢复")
	return x
}

func (w *World) configSpecs(f Filter) []permSpec {
	var out []permSpec
	add := func(comp string, specs ...permSpec) {
		if _, ok := w.component(comp); ok && (f.Component == "" || f.Component == comp) {
			out = append(out, specs...)
		}
	}
	add("server",
		permSpec{Path: service.ServerConfigDir, Dir: true, Max: 0o750, Owner: "root", Group: service.UserServer},
		permSpec{Path: service.ServerConfigDir + "/server.yaml", Max: 0o640, Owner: "root", Group: service.UserServer},
		permSpec{Path: service.ServerConfigDir + "/migrate.yaml", Max: 0o400, Owner: "root", Group: "root", Optional: true},
		permSpec{Path: service.ServerConfigDir + "/setup-token.hash", Max: 0o600, Owner: service.UserServer, Group: service.UserServer, Optional: true})
	add("node",
		permSpec{Path: service.NodeConfigDir, Dir: true, Max: 0o750, Owner: "root", Group: service.UserNode},
		permSpec{Path: service.NodeConfigDir + "/node.yaml", Max: 0o640, Owner: "root", Group: service.UserNode})
	add("dns", permSpec{Path: "/etc/lingedge/dns/dns.yaml", Max: 0o640, Owner: "root", Group: "lingedge-dns", Optional: true})
	add("reporter", permSpec{Path: "/etc/lingedge/reporter/reporter.yaml", Max: 0o640, Owner: "root", Group: service.UserReporter, Optional: true})
	if f.Component == "" {
		out = append(out, permSpec{Path: "/etc/lingedge/tools/tools.yaml", Max: 0o644, Owner: "root", Optional: true})
	}
	return out
}

func (w *World) identityFinding() Finding {
	dir := service.NodeDataDir + "/identity"
	specs := []permSpec{{Path: dir, Dir: true, Max: 0o700, Owner: service.UserNode, Group: service.UserNode, Optional: true}}
	ents, err := os.ReadDir(w.path(dir))
	if err == nil {
		sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
		for i, e := range ents {
			if i >= 200 {
				break
			}
			specs = append(specs, permSpec{Path: dir + "/" + e.Name(), Max: 0o600, Owner: service.UserNode, Group: service.UserNode})
		}
	} else if !os.IsNotExist(err) {
		x := fnd("fs.identity_perm", LevelSkip, "无法读取 "+dir+"（需要 root）："+errText(err), "以 root 运行 lingedge doctor")
		x.Component = "node"
		return x
	}
	return w.permFinding("fs.identity_perm", "node", specs, "节点身份目录 0700、文件 0600 且属主为 "+service.UserNode,
		"身份私钥只允许 "+service.UserNode+" 访问；lingedge doctor --fix-perms 只修正权限与属主，不读取私钥内容")
}

// releaseOwner 检查 release 目录不能被运行用户（或 other）写入：运行用户可写自己的二进制 = 被攻陷后可持久化提权（NFR-9）。
func (w *World) releaseOwner(c Component) Finding {
	id := "fs.release_owner." + c.Name
	if c.ReleaseDir == "" {
		x := fnd(id, LevelSkip, c.Name+" 没有 release 目录记录", "")
		x.Component = c.Name
		return x
	}
	x := Finding{ID: id, Component: c.Name}
	root := w.path(path.Dir(path.Dir(c.ReleaseDir))) // /opt/lingedge/<c>
	if _, err := os.Lstat(root); err != nil {
		x.Level, x.Message = LevelFail, root[len(w.Root):]+" 不存在（与 state.json 的记录不一致）"
		x.Hint = "重新安装该组件"
		return x
	}
	runUID, runGID := -1, -1
	if w.Own != nil && c.RunUser != "" {
		if u, g, err := w.Own.Resolve(c.RunUser, c.RunUser); err == nil {
			runUID, runGID = u, g
		}
	}
	var bad []string
	count := 0
	_ = filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if count++; count > 20000 || len(bad) >= 5 {
			return fs.SkipAll
		}
		st, err := de.Info()
		if err != nil || st.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		rel := strings.TrimPrefix(p, w.Root)
		perm := st.Mode().Perm()
		uid, gid, ok := ownerOf(st)
		switch {
		case perm&0o002 != 0:
			bad = append(bad, rel+"（other 可写 "+fmtMode(perm)+"）")
		case ok && runUID >= 0 && uid == runUID && runUID != 0:
			bad = append(bad, rel+"（属于运行用户 "+c.RunUser+"）")
		case ok && runGID >= 0 && gid == runGID && perm&0o020 != 0 && runGID != 0:
			bad = append(bad, rel+"（运行用户组可写 "+fmtMode(perm)+"）")
		}
		return nil
	})
	if len(bad) > 0 {
		x.Level, x.Message = LevelFail, c.Name+" 的 release 可被运行用户/other 写入："+strings.Join(bad, "；")
		x.Hint = "chown -R root:root " + path.Dir(path.Dir(c.ReleaseDir)) + " 并去掉 group/other 写权限；运行用户不得能改写自己的二进制"
		x.Details = map[string]any{"examples": bad}
		return x
	}
	x.Level, x.Message = LevelPass, c.Name+" 的 release 目录不可被运行用户写入"
	return x
}

func (w *World) dirsFinding() (Finding, bool) {
	if w.Own == nil {
		return Finding{}, false
	}
	_, hasServer := w.component("server")
	_, hasNode := w.component("node")
	var specs []service.DirSpec
	if hasServer {
		specs = append(specs, service.ServerDirs(service.UserServer, service.UserServer)...)
	}
	if hasNode {
		specs = append(specs, service.NodeDirs(service.UserNode, service.UserNode)...)
		specs = append(specs, service.UpgradeResultDir())
	}
	if len(specs) == 0 {
		return Finding{}, false
	}
	ens := &service.Ensurer{Root: w.Root, Own: w.Own}
	var fails, warns []string
	for _, sf := range ens.VerifyDirs(specs) {
		switch sf.Level {
		case service.LevelFail:
			fails = append(fails, sf.Message)
		case service.LevelWarn:
			warns = append(warns, sf.Message)
		}
	}
	x := Finding{ID: "fs.dirs"}
	switch {
	case len(fails) > 0:
		x.Level, x.Message = LevelFail, strings.Join(fails, "；")
		x.Hint = "按提示 chmod/chown，或重新运行安装；数据/日志目录的权限不在 --fix-perms 的修复范围内"
		x.Details = map[string]any{"issues": append(fails, warns...)}
	case len(warns) > 0:
		x.Level, x.Message = LevelWarn, strings.Join(warns, "；")
	default:
		x.Level, x.Message = LevelPass, fmt.Sprintf("Tools 管理的 %d 个目录权限与属主符合规格", len(specs))
	}
	return x, true
}

func (w *World) stateDirFinding() Finding {
	const id = "fs.state_dir"
	dir := w.StateDir
	if dir == "" {
		return fnd(id, LevelSkip, "没有 Tools 状态目录", "")
	}
	st, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return fnd(id, LevelPass, "Tools 状态目录尚未创建（尚未安装过任何组件）", "")
		}
		return fnd(id, LevelFail, "无法检查 Tools 状态目录："+errText(err), "")
	}
	x := Finding{ID: id}
	var bad []string
	if !st.IsDir() {
		bad = append(bad, "状态目录不是目录")
	}
	if st.Mode().Perm()&0o022 != 0 {
		bad = append(bad, "状态目录 "+fmtMode(st.Mode().Perm())+" 允许 group/other 写入")
	}
	if uid, _, ok := ownerOf(st); ok && uid != 0 && uid != w.Euid {
		bad = append(bad, fmt.Sprintf("状态目录属于 uid %d（应为 root）", uid))
	}
	if w.FSKind != nil {
		if info, kerr := w.FSKind(dir); kerr == nil && info.Network {
			x.Level, x.Message = LevelWarn, "Tools 状态目录位于网络/FUSE 文件系统（"+info.Type+"）：flock 任务锁在其上不可靠"
			x.Hint = "把 /var/lib/lingedge/tools 放到本地文件系统（tools-design §16）"
			if len(bad) > 0 {
				x.Level, x.Message = LevelFail, strings.Join(bad, "；")+"；另外："+x.Message
			}
			return x
		}
	}
	if len(bad) > 0 {
		x.Level, x.Message, x.Hint = LevelFail, strings.Join(bad, "；"), "chown root:root 并 chmod 0700 该目录（lingedge doctor --fix-perms 不处理此项）"
		return x
	}
	x.Level, x.Message = LevelPass, "Tools 状态目录权限正常，位于本地文件系统"
	return x
}
