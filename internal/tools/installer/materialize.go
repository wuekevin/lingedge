package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/security"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

func removeAll(p string) error { return os.RemoveAll(p) }

func (x *execution) hp(virtual string) string { return x.in.d.hostPath(virtual) }

func releaseDir(c resolver.PlanComponent) string {
	return optRoot + "/" + c.Name + "/releases/" + c.Version
}

// ---- 步骤 14：安全解压（staging → releases/<v>）----

func (x *execution) planExtract() (registry.Undo, error) {
	// 撤销顺序（一步内的动作按记录顺序执行）：后装的组件先撤；每个组件先摘 current 链接、再删 release，最后（由深到浅）删本事务创建的空目录。
	var u registry.Undo
	comps := x.installComps()
	var dirs []registry.UndoAction
	seen := map[string]bool{}
	for i := len(comps) - 1; i >= 0; i-- {
		c := comps[i]
		cur := optRoot + "/" + c.Name + "/current"
		_, curErr := os.Lstat(x.hp(cur))
		rel := releaseDir(c)
		_, relErr := os.Lstat(x.hp(rel))
		u.Actions = append(u.Actions,
			registry.UndoAction{Action: actUnlinkCurrent, Path: cur, Created: curErr != nil, Extra: map[string]string{targetKey: "releases/" + c.Version}},
			registry.UndoAction{Action: actRemoveRelease, Path: rel + ".partial", Created: true},
			registry.UndoAction{Action: actRemoveRelease, Path: rel, Created: relErr != nil})
		for _, d := range []string{optRoot + "/" + c.Name + "/releases", optRoot + "/" + c.Name} {
			if !seen[d] {
				seen[d] = true
				if _, err := os.Lstat(x.hp(d)); err != nil {
					dirs = append(dirs, registry.UndoAction{Action: actRemoveOptDir, Path: d, Created: true})
				}
			}
		}
	}
	if _, err := os.Lstat(x.hp(optRoot)); err != nil {
		dirs = append(dirs, registry.UndoAction{Action: actRemoveOptDir, Path: optRoot, Created: true})
	}
	u.Actions = append(u.Actions, dirs...)
	return u, nil
}

func (x *execution) doExtract(ctx context.Context) error {
	for _, c := range x.installComps() {
		a := x.acqByC[c.Name]
		if a == nil {
			return fmt.Errorf("组件 %s 没有已校验的包（内部错误）", c.Name)
		}
		for _, d := range []string{optRoot, optRoot + "/" + c.Name, optRoot + "/" + c.Name + "/releases"} {
			if _, err := registry.EnsureDirAll(x.hp(d), 0o755); err != nil {
				return err
			}
		}
		rel := releaseDir(c)
		sums, err := loadSums(a.PkgDir)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(x.hp(rel)); err == nil {
			// 目标 release 已存在：只有与已验证的包逐文件一致才算幂等完成，否则绝不覆盖。
			if err := verifyTree(x.hp(rel), filepath.Join(a.PkgDir, "payload"), sums); err != nil {
				return &Error{Kind: KindPrecondition, Step: StepExtract, Msg: "目标 release 目录 " + rel + " 已存在且内容与已验证的包不一致，不会覆盖：" + err.Error(),
					Hint: "确认它是遗留物后手工删除，或安装其他版本", SystemState: "unchanged"}
			}
		} else {
			partial := x.hp(rel + ".partial")
			if err := os.RemoveAll(partial); err != nil {
				return err
			}
			if err := copyVerified(filepath.Join(a.PkgDir, "payload"), partial, sums); err != nil {
				return err
			}
			if err := os.Rename(partial, x.hp(rel)); err != nil {
				return err
			}
			syncDir(filepath.Dir(x.hp(rel)))
		}
		if _, err := registry.EnsureSymlink(x.hp(optRoot+"/"+c.Name+"/current"), "releases/"+c.Version); err != nil {
			return err
		}
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// loadSums 读取已验签的 META/SHA256SUMS（Acquire 已核对它与验签时的字节一致）。
func loadSums(pkgDir string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(pkgDir, "META", "SHA256SUMS"))
	if err != nil {
		return nil, err
	}
	return security.ParseSHA256SUMS(b, security.Limits{})
}

// copyVerified 把已解压的 payload 复制到 dst（跨文件系统也可用）：只接受目录与普通文件（符号链接/特殊文件一律拒绝），
// 逐文件重新计算 SHA256 并与已验签的 sums 比对（staging 到落地之间的任何改动都会被发现），权限：目录 0755，文件保持 0644/0755。
func copyVerified(src, dst string, sums map[string]string) error {
	if err := security.MkdirAllPerm(dst, 0o755); err != nil {
		return err
	}
	seen := map[string]bool{}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return security.MkdirAllPerm(target, 0o755)
		case d.Type().IsRegular():
			want, ok := sums["payload/"+filepath.ToSlash(rel)]
			if !ok {
				return fmt.Errorf("包内文件 %s 不在已验签的 SHA256SUMS 中", rel)
			}
			seen[filepath.ToSlash(rel)] = true
			return copyOne(p, target, want)
		}
		return fmt.Errorf("包内含不允许的文件类型：%s", rel)
	})
	if err != nil {
		return err
	}
	for k := range sums {
		if rest, ok := strings.CutPrefix(k, "payload/"); ok && !seen[rest] {
			return fmt.Errorf("已验签的 SHA256SUMS 声明了 %s，但 staging 中缺失", rest)
		}
	}
	syncDir(dst)
	return nil
}

func copyOne(src, dst, wantSum string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("%s 不是普通文件", src)
	}
	perm := fs.FileMode(0o644)
	if st.Mode().Perm()&0o100 != 0 {
		perm = 0o755
	}
	out, err := security.CreateFile(dst, perm)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSum {
		return fmt.Errorf("%s 的 SHA256 与已验签清单不符", filepath.Base(src))
	}
	return nil
}

// verifyTree 校验已存在的 release 目录与 payload 逐文件一致（路径集合与 SHA256 都相同，且只含普通文件/目录）。
func verifyTree(existing, payload string, sums map[string]string) error {
	seen := map[string]bool{}
	err := filepath.WalkDir(existing, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(existing, p)
		if rel == "." || d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("含非普通文件 %s", rel)
		}
		want, ok := sums["payload/"+filepath.ToSlash(rel)]
		if !ok {
			return fmt.Errorf("多出文件 %s", rel)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, cerr := io.Copy(h, f)
		_ = f.Close()
		if cerr != nil {
			return cerr
		}
		if hex.EncodeToString(h.Sum(nil)) != want {
			return fmt.Errorf("文件 %s 内容不同", rel)
		}
		seen[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		return err
	}
	for k := range sums {
		if rest, ok := strings.CutPrefix(k, "payload/"); ok && !seen[rest] {
			return fmt.Errorf("缺少文件 %s", rest)
		}
	}
	return nil
}

// ---- 步骤 15：运行用户 ----

func (x *execution) users() []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range x.installComps() {
		if c.RunUser != "" && !seen[c.RunUser] {
			seen[c.RunUser] = true
			out = append(out, c.RunUser)
		}
	}
	return out
}

func (x *execution) planUsers() (registry.Undo, error) {
	var u registry.Undo
	ctx := context.Background()
	x.userCreated = map[string]bool{}
	for _, name := range x.users() {
		exists, err := x.in.prov.Exists(ctx, name)
		if err != nil {
			return u, err
		}
		x.userCreated[name] = !exists
	}
	return registry.Undo{Actions: x.userUndo()}, nil
}

func (x *execution) doUsers(ctx context.Context) error {
	for _, name := range x.users() {
		r, err := x.in.prov.EnsureUser(ctx, name)
		if err != nil {
			return err
		}
		if r.Warning != "" {
			x.warn(Warning{Code: "user.not_system", Step: StepCreateUser, Message: r.Warning})
		}
		if r.Created != x.userCreated[name] {
			// 预判与实际不符（预判到执行之间有人创建/删除了该用户）：以实际为准重写 undo（在途步骤可重新 begin），避免漏删或误删。
			x.userCreated[name] = r.Created
			if err := x.j.Begin(StepCreateUser, StepName(StepCreateUser), registry.Undo{Actions: x.userUndo()}); err != nil {
				return err
			}
		}
	}
	return nil
}

// userUndo 按撤销顺序（后创建的先删）给出各运行用户的撤销动作。
func (x *execution) userUndo() []registry.UndoAction {
	us := x.users()
	var out []registry.UndoAction
	for i := len(us) - 1; i >= 0; i-- {
		out = append(out, service.UserUndo(service.UserResult{Name: us[i], Created: x.userCreated[us[i]]}))
	}
	return out
}

func (x *execution) warn(w Warning) {
	x.warnings = append(x.warnings, w)
	x.in.ev.emit(Event{Type: EventWarning, Step: w.Step, Code: w.Code, Component: w.Component, Message: w.Message})
}

// ---- 步骤 16：目录 ----

func (x *execution) allDirSpecs() []service.DirSpec {
	var out []service.DirSpec
	for _, c := range x.installComps() {
		out = append(out, x.in.dirSpecs(x.pre, c)...)
	}
	return out
}

func (x *execution) ensurer() *service.Ensurer {
	return &service.Ensurer{Root: x.in.d.Root, Own: x.in.d.Own}
}

func (x *execution) planDirs() (registry.Undo, error) {
	plans, err := x.ensurer().Plan(x.allDirSpecs())
	if err != nil {
		return registry.Undo{}, err
	}
	// 受管目录之外被创建的祖先（真实系统上 /etc、/var/lib、/var/log 总是存在，所以通常没有）回滚时保留：
	// 为它们写 undo 只会让回滚被 service.Undoer 的路径白名单拒绝。
	var acts []registry.UndoAction
	for _, a := range service.PlanUndo(plans) {
		if service.IsManagedPath(a.Path) {
			acts = append(acts, a)
		}
	}
	return registry.Undo{Actions: acts}, nil
}

func (x *execution) doDirs(context.Context) error {
	for _, c := range x.installComps() {
		_, err := x.ensurer().Ensure(x.in.dirSpecs(x.pre, c))
		var me *service.MismatchError
		if errors.As(err, &me) {
			// 既有目录与规格不符：预检已列为冲突并获确认；目录保持原样，只告警。
			for _, it := range me.Items {
				x.warn(Warning{Code: "dir.mismatch", Component: c.Name, Step: StepCreateDirs, Message: it})
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- 步骤 17：systemd 单元 ----

func (x *execution) planUnits() (registry.Undo, error) {
	// 撤销顺序：后装的组件先撤；每个组件先 disable、再删除单元文件/.new（systemctl 在文件还在时才能 disable）。
	var u registry.Undo
	ctx := context.Background()
	comps := x.installComps()
	for i := len(comps) - 1; i >= 0; i-- {
		c := comps[i]
		for _, t := range service.EnableTargets(c.Name) {
			created := true
			if _, err := os.Lstat(x.hp(service.UnitDir + "/" + string(t))); err == nil {
				st, qerr := x.in.sys.IsEnabled(ctx, t)
				if qerr != nil {
					return u, qerr
				}
				created = !enabledState(st)
			}
			u.Actions = append(u.Actions, registry.UndoAction{Action: "systemctl_disable", Path: string(t), Created: created})
		}
		us := x.pre.units[c.Name]
		for j := len(us) - 1; j >= 0; j-- {
			un := us[j]
			virt := service.UnitDir + "/" + string(un.Name)
			sum := sha256Hex(un.Content)
			b, err := os.ReadFile(x.hp(virt))
			switch {
			case errors.Is(err, fs.ErrNotExist):
				u.Actions = append(u.Actions, registry.UndoAction{Action: "unlink_unit", Path: virt, Created: true, SHA256: sum})
			case err == nil && string(b) == string(un.Content):
			default: // 已存在且不同（或不可读）：不覆盖，写 .new
				if _, nerr := os.Lstat(x.hp(virt + ".new")); nerr != nil {
					u.Actions = append(u.Actions, registry.UndoAction{Action: "remove_path", Path: virt + ".new", Created: true, SHA256: sum})
				}
			}
		}
	}
	return u, nil
}

func enabledState(st string) bool {
	switch st {
	case "enabled", "enabled-runtime", "static", "alias", "indirect", "generated":
		return true
	}
	return false
}

func (x *execution) doUnits(ctx context.Context) error {
	ins := &service.Installer{Root: x.in.d.Root, Own: x.in.d.Own}
	for _, c := range x.installComps() {
		for _, un := range x.pre.units[c.Name] {
			r, err := ins.Install(un)
			if err != nil {
				return err
			}
			if r.Warning != "" {
				x.warn(Warning{Code: "unit.new_written", Component: c.Name, Step: StepInstallUnits, Message: r.Warning})
			}
		}
	}
	// 单元文件落盘后、enable 之前 daemon-reload（systemctl 才看得到新单元）。
	if err := x.in.sys.DaemonReload(ctx); err != nil {
		return err
	}
	for _, c := range x.installComps() {
		if ts := service.EnableTargets(c.Name); len(ts) > 0 {
			if _, err := x.in.sys.Enable(ctx, ts...); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- 步骤 18：初始配置 ----

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (x *execution) planConfig() (registry.Undo, error) {
	var u registry.Undo
	for i := len(x.pre.files) - 1; i >= 0; i-- { // 撤销顺序：后创建的先删
		pf := x.pre.files[i]
		b, err := os.ReadFile(x.hp(pf.Path))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			u.Actions = append(u.Actions, registry.UndoAction{Action: "remove_path", Path: pf.Path, Created: true, SHA256: sha256Hex(pf.Content)})
		case err == nil && (pf.Random || string(b) == string(pf.Content)):
		default:
			if _, nerr := os.Lstat(x.hp(pf.Path + ".new")); nerr != nil {
				u.Actions = append(u.Actions, registry.UndoAction{Action: "remove_path", Path: pf.Path + ".new", Created: true, SHA256: sha256Hex(pf.Content)})
			}
		}
	}
	return u, nil
}

func (x *execution) doConfig(context.Context) error {
	w := &service.FileWriter{Root: x.in.d.Root, Own: x.in.d.Own}
	for _, pf := range x.pre.files {
		spec := service.FileSpec{Mode: pf.Mode, Owner: pf.Owner, Group: pf.Group}
		_, err := w.Create(pf.Path, pf.Content, spec)
		if err == nil {
			switch pf.Kind {
			case "setup_hash":
				x.tokenUsable = true
			case "secret_key":
				x.masterKeyFP = sha256Hex(pf.Content)[:16]
			}
			continue
		}
		var se *service.Error
		if !errors.As(err, &se) || se.Kind != service.KindConflict {
			return err
		}
		// 目标已存在。
		if pf.Random {
			x.warn(Warning{Code: "config.kept_existing", Component: pf.Component, Step: StepInitConfig, Message: pf.Path + " 已存在，沿用（不会重新生成或覆盖）"})
			continue
		}
		if b, rerr := os.ReadFile(x.hp(pf.Path)); rerr == nil && string(b) == string(pf.Content) {
			continue // 幂等：内容一致
		}
		// 内容不同：绝不覆盖；新内容写入 .new（预检已列为冲突并获确认）。
		if err := x.writeNew(pf); err != nil {
			return err
		}
		x.warn(Warning{Code: "config.new_written", Component: pf.Component, Step: StepInitConfig,
			Message: pf.Path + " 已存在且内容不同：未覆盖；新内容已写入 " + pf.Path + ".new，请人工比对合并（FR-6）"})
	}
	return nil
}

func (x *execution) writeNew(pf plannedFile) error {
	real := x.hp(pf.Path + ".new")
	if err := security.WriteFileAtomic(real, pf.Content, pf.Mode); err != nil {
		return err
	}
	uid, gid, err := x.in.d.Own.Resolve(pf.Owner, pf.Group)
	if err != nil {
		_ = os.Remove(real)
		return err
	}
	if err := x.in.d.Own.Chown(real, uid, gid); err != nil {
		_ = os.Remove(real)
		return err
	}
	return nil
}

// ---- 步骤 19/20：启动与健康检查 ----

func (x *execution) planStart() (registry.Undo, error) {
	var u registry.Undo
	if x.pre.req.SkipStart {
		return u, nil
	}
	ctx := context.Background()
	comps := x.installComps()
	for i := len(comps) - 1; i >= 0; i-- { // 撤销顺序：后启动的先停
		c := comps[i]
		for _, t := range service.EnableTargets(c.Name) {
			st, err := x.in.sys.IsActive(ctx, t)
			if err != nil {
				return u, err
			}
			u.Actions = append(u.Actions, registry.UndoAction{Action: "systemctl_stop", Path: string(t), Created: st != "active" && st != "activating", Prev: st})
		}
	}
	return u, nil
}

func (x *execution) runtimeFor(c resolver.PlanComponent) *Runtime {
	var ports []resolver.PlannedPort
	for _, p := range x.pre.Plan.Ports {
		if p.Component == c.Name {
			ports = append(ports, p)
		}
	}
	rt := &Runtime{Component: c.Name, Version: c.Version, Variant: c.Variant, Ports: ports, Units: service.EnableTargets(c.Name),
		SetupListen: x.pre.req.SetupListen, Root: x.in.d.Root,
		start: func(ctx context.Context, units ...service.UnitName) error {
			_, err := x.in.sys.Start(ctx, units...)
			return err
		}}
	if x.pre.Node != nil {
		rt.Node = *x.pre.Node
	}
	return rt
}

func (x *execution) doStart(ctx context.Context) error {
	if x.pre.req.SkipStart {
		x.emitStep(EventStepSkipped, StepStart, "--skip-start：只物化，不启动")
		return nil
	}
	for _, c := range x.installComps() {
		if err := x.in.d.Specs[c.Name].Start(ctx, x.runtimeFor(c)); err != nil {
			return fmt.Errorf("启动 %s 失败: %w", c.Name, err)
		}
	}
	return nil
}

func (x *execution) doHealth(ctx context.Context) error {
	if x.pre.req.SkipStart {
		x.emitStep(EventStepSkipped, StepHealth, "--skip-start：未启动，跳过健康检查")
		return nil
	}
	timeout, err := x.in.d.Config.HealthTimeout()
	if err != nil {
		return err
	}
	interval, err := x.in.d.Config.HealthInterval()
	if err != nil {
		return err
	}
	for _, c := range x.installComps() {
		spec := x.in.d.Specs[c.Name]
		rt := x.runtimeFor(c)
		herr := waitHealth(ctx, timeout, interval, func(cx context.Context) error { return spec.Health(cx, rt) })
		if herr == nil {
			continue
		}
		if isCancel(herr) || !x.pre.req.NoRollback {
			return fmt.Errorf("%s 健康检查失败: %w", c.Name, herr)
		}
		x.healthErr = fmt.Errorf("%s 健康检查失败: %w", c.Name, herr)
		x.warn(Warning{Code: "health.failed_kept", Component: c.Name, Step: StepHealth, Message: x.healthErr.Error()})
		return nil
	}
	return nil
}

// ---- 步骤 21/22：提交点与访问信息 ----

func (x *execution) planState() (registry.Undo, error) {
	var u registry.Undo
	for _, c := range x.installComps() {
		u.Actions = append(u.Actions, registry.UndoAction{Action: actStateForget, Extra: map[string]string{stateForgetKey: c.Name, stateForgetTx: x.tx}})
	}
	return u, nil
}

func uniquePorts(ps []resolver.PlannedPort, comp string) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range ps {
		if p.Component == comp && !seen[p.Port] {
			seen[p.Port] = true
			out = append(out, p.Port)
		}
	}
	sort.Ints(out)
	return out
}

// doState 是提交点：state.json 原子写（registry.UpdateState：全局锁内读-改-校验-原子 rename）。提交前崩溃 = 未完成，由 journal 回滚或续跑。
func (x *execution) doState(ctx context.Context) error {
	now := x.in.d.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	state := "running"
	if x.pre.req.SkipStart {
		state = "installed"
	}
	if x.healthErr != nil {
		state = "failed"
	}
	comps := x.installComps()
	rep := x.pre.probe.Baota.Scrub()
	err := x.in.d.Reg.UpdateState(ctx, func(s *registry.State) error {
		for _, c := range comps {
			rec := registry.Component{
				Version: c.Version, State: state, RunUser: c.RunUser, Ports: uniquePorts(x.pre.Plan.Ports, c.Name),
				ReleaseDir: releaseDir(c), InstalledAt: now, LastTx: x.tx,
			}
			if ts := service.EnableTargets(c.Name); len(ts) > 0 {
				rec.Unit = string(ts[0])
			}
			for _, f := range x.pre.Plan.Files {
				if f.Component == c.Name && f.Kind == "file" && rec.ConfigDir == "" {
					rec.ConfigDir = path.Dir(f.Path)
				}
				if f.Component == c.Name && f.Kind == "dir" && f.Policy == resolver.PolicyCreateIfAbsent {
					rec.DataDir = f.Path
				}
			}
			if c.Name == "node" {
				rec.ConfigDir = service.NodeConfigDir
			}
			if rec.Ports == nil {
				rec.Ports = []int{}
			}
			s.Components[c.Name] = rec
		}
		// 宿主机只读探测报告（Story 1.31 的 RecordHostBaota 语义）与组件记录在同一次原子写里落盘。
		if s.Host == nil {
			s.Host = &registry.Host{}
		}
		s.Host.Baota = &rep
		return nil
	})
	if err != nil {
		return err
	}
	x.committed = true
	for _, c := range comps {
		x.comps = append(x.comps, InstalledComponent{Name: c.Name, Version: c.Version, State: state, ReleaseDir: releaseDir(c)})
	}
	return nil
}

func (x *execution) doAccess(context.Context) error {
	for _, c := range x.installComps() {
		if c.Name != "server" {
			continue
		}
		x.access = append(x.access, Access{Component: "server", Kind: "setup_url", Label: "安装向导地址", Value: "http://" + x.pre.req.SetupListen + "/"})
		if x.masterKeyFP != "" {
			// F-7：主密钥只在这里给出短指纹（不是密钥本身），并醒目提示与数据库备份分开保管——丢失即无法解密已加密的数据。
			x.access = append(x.access, Access{Component: "server", Kind: "master_key_fingerprint", Label: "主密钥指纹（sha256 前 8 字节）",
				Value: x.masterKeyFP + "（" + service.ServerConfigDir + "/master.key；请与数据库备份分开保管，丢失将无法解密已加密的数据）"})
		}
		switch {
		case x.pre.token != "" && x.tokenUsable:
			// setup token 只在这里显示一次；它不写盘、不写日志、不进事件（哈希已在步骤 18 写入）。
			x.access = append(x.access, Access{Component: "server", Kind: "setup_token", Label: "安装向导令牌（仅显示一次）", Value: x.pre.token, Secret: true})
		default:
			x.access = append(x.access, Access{Component: "server", Kind: "note", Label: "安装向导令牌",
				Value: "本次没有生成新令牌（哈希文件已存在或续跑中断）；如需新令牌运行 lingedge config setup-token --reset"})
		}
	}
	x.in.ev.emit(Event{Type: EventInfo, Step: StepAccessInfo, Message: "安装完成：" + joinNames(x.compNames())})
	return nil
}

func (x *execution) compNames() []string {
	var out []string
	for _, c := range x.installComps() {
		out = append(out, c.Name)
	}
	return out
}
