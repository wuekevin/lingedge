package resolver

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

const (
	actInstall = "install"
	actUpgrade = "upgrade"
	actKeep    = "keep"
)

func (r *run) action(name string, c *cand) string {
	if _, has := r.in.Installed.Components[name]; !has {
		return actInstall
	}
	if c.keep {
		return actKeep
	}
	return actUpgrade
}

func (r *run) inPlan(name string, c *cand) bool {
	if _, w := r.wants[name]; w {
		return true
	}
	if r.action(name, c) != actKeep {
		return true
	}
	by := r.intro[name]
	return by != "want" && by != "installed"
}

func (r *run) sortedAssigned() []string {
	out := make([]string, 0, len(r.assigned))
	for n := range r.assigned {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// assemble 是 §10.3 步骤 4、6、8：系统检查（内存/磁盘按挂载点）、端口矩阵、产出 Plan。
func (r *run) assemble() {
	var plan []string // 进入 Plan 的组件（字典序）
	for _, n := range r.sortedAssigned() {
		if r.inPlan(n, r.assigned[n]) {
			plan = append(plan, n)
		}
	}
	inPlan := map[string]bool{}
	for _, n := range plan {
		inPlan[n] = true
	}

	var passive []map[string]any
	for _, n := range plan {
		c := r.assigned[n]
		act := r.action(n, c)
		inst := r.in.Installed.Components[n]
		_, wanted := r.wants[n]
		pc := PlanComponent{Name: n, Action: act, Version: c.vs, Variant: c.variant, From: inst.Version, Wanted: wanted,
			Internal: r.m.Components[n].Internal, RunUser: profileFor(n).runUser}
		if by := r.intro[n]; by != "want" && by != "installed" {
			pc.RequiredBy = by
		}
		if act == actUpgrade && !wanted {
			pc.PassiveUpgrade = true
			passive = append(passive, map[string]any{"component": n, "from": inst.Version, "to": c.vs})
		}
		if act != actKeep && c.entry != nil {
			pc.Platform, pc.URL, pc.SHA256, pc.Size = c.platKey, c.art.URL, c.art.SHA256, c.art.Size
			pc.Migrations = c.entry.Migrations
		}
		r.plan.Components = append(r.plan.Components, pc)
		if act == actKeep && wanted {
			r.warn("already.installed", n, "%s %s 已安装，无需操作", n, c.vs)
		}
	}
	r.revokedInstalledWarnings()
	r.stepwiseWarnings(plan)
	r.edgeWarnings(plan)
	r.externalWarnings(plan)
	if r.in.System.Baota.Detected {
		r.warn("baota.detected", "", "检测到宝塔面板（置信度 %s）：宝塔存在不阻止安装，只有真实的端口/路径/同名冲突才会阻止；Tools 不修改宝塔站点与防火墙", orDash(r.in.System.Baota.Confidence))
	}
	if len(passive) > 0 {
		if r.in.AcceptPassiveUpgrade {
			for _, p := range passive {
				r.warn("passive.upgrade_accepted", p["component"].(string), "已确认被动升级 %s：%s → %s", p["component"], p["from"], p["to"])
			}
		} else {
			parts := make([]string, 0, len(passive))
			for _, p := range passive {
				parts = append(parts, fmt.Sprintf("%s %s → %s", p["component"], p["from"], p["to"]))
			}
			r.block(Blocker{Kind: KindConfirm, Code: "confirm.passive_upgrade",
				Message: "以下已装组件并非你所选，但因版本约束必须升级：" + strings.Join(parts, "；"),
				Hint:    "确认后重试（交互式回答是；非交互加 --yes）", Details: map[string]any{"components": toAnySlice(passive)}})
		}
	}

	order, cycle := r.topo(plan, inPlan)
	if cycle != nil {
		r.block(Blocker{Kind: KindCycle, Code: "resolve.dependency_cycle", Message: "依赖成环：" + strings.Join(cycle, " → "), Chain: cycle,
			Hint: "清单的 requires.components 存在循环依赖，请联系发行方"})
		order = plan
	}
	r.plan.Order = order
	rank := map[string]int{}
	for i, n := range order {
		rank[n] = i
	}
	sort.SliceStable(r.plan.Components, func(i, j int) bool { return rank[r.plan.Components[i].Name] < rank[r.plan.Components[j].Name] })

	r.buildArtifacts(order)
	r.checkResources(order)
	r.checkPorts(plan)
}

func toAnySlice(in []map[string]any) []any {
	out := make([]any, len(in))
	for i, m := range in {
		out[i] = m
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (r *run) revokedInstalledWarnings() {
	for _, n := range r.sortedAssigned() {
		c := r.assigned[n]
		if !c.keep {
			continue
		}
		if _, rev := r.m.RevokedVersion(n, c.ver); rev {
			r.warn("installed.revoked", n, "已装的 %s %s 已被发行方吊销，建议尽快升级", n, c.vs)
		}
	}
}

// stepwiseWarnings：选中的版本不是通道内最高稳定版，且更高版本因 min_upgrade_from 不能直接到达 → 提示逐级升级路径。
func (r *run) stepwiseWarnings(plan []string) {
	for _, n := range plan {
		c := r.assigned[n]
		iv, has := r.instVer[n]
		if !has || c.keep || c.entry == nil {
			continue
		}
		comp := r.m.Components[n]
		var latest *manifest.Version
		for i := range comp.Versions {
			v, err := manifest.ParseVersion(comp.Versions[i].Version)
			if err != nil || v.Prerelease() {
				continue
			}
			if _, rev := r.m.RevokedVersion(n, v); rev {
				continue
			}
			if latest == nil || v.Compare(*latest) > 0 {
				x := v
				latest = &x
			}
		}
		if latest == nil || latest.Compare(c.ver) <= 0 {
			continue
		}
		if path := r.upgradePath(n, comp, iv, *latest); len(path) > 0 && path[0] != latest.String() {
			r.warn("upgrade.stepwise", n, "%s 的最新版 %s 不能从 %s 直接升级，本次升级到 %s；完整路径：%s（再次运行 update 继续）", n, latest.String(), iv.String(), c.vs, strings.Join(path, " → "))
		}
	}
}

func (r *run) edgeWarnings(plan []string) {
	for _, n := range plan {
		c := r.assigned[n]
		if c.keep {
			continue
		}
		for _, e := range c.edges {
			if _, ok := r.assigned[e.to]; ok {
				continue
			}
			switch e.kind {
			case edgeRequires:
				r.warn("dependency.remote_assumed", n, "%s 要求 %s %s：本机没有该组件也不一并安装，按远端部署处理，请自行确保其版本满足", e.src, e.to, e.cs)
			case edgePeer:
				r.warn("peer.compare_at_enroll", n, "%s 声明 peer %s %s：本机没有 %s，将在 enroll 时由对端返回版本比对（不满足仅警告）", e.src, e.to, e.cs, e.to)
			}
		}
	}
}

func (r *run) externalWarnings(plan []string) {
	for _, n := range plan {
		c := r.assigned[n]
		if c.keep || c.entry == nil {
			continue
		}
		for _, rq := range reqLists(c.entry, c.variant) {
			for _, k := range sortedKeys(rq.External) {
				if d, ok := r.in.System.Deps[depName(k)]; !ok || !d.Detected {
					r.warn("external.remote_assumed", n, "%s 要求外部依赖 %s %s：本机未探测到，向导中请配置远端实例", c.comp, k, rq.External[k])
				}
			}
		}
	}
}

// topo 返回依赖在前的稳定顺序（Kahn，同层按字典序）；有环时返回环路径。
func (r *run) topo(plan []string, inPlan map[string]bool) ([]string, []string) {
	deps := map[string][]string{}
	indeg := map[string]int{}
	rev := map[string][]string{}
	for _, n := range plan {
		indeg[n] += 0
		seen := map[string]bool{}
		for _, e := range r.assigned[n].edges {
			if e.kind == edgeConflict || !inPlan[e.to] || e.to == n || seen[e.to] {
				continue
			}
			seen[e.to] = true
			deps[n] = append(deps[n], e.to)
			rev[e.to] = append(rev[e.to], n)
			indeg[n]++
		}
	}
	for k := range deps {
		sort.Strings(deps[k])
	}
	return kahn(plan, indeg, rev, deps)
}

func kahn(nodes []string, indeg map[string]int, rev, deps map[string][]string) ([]string, []string) {
	var ready []string
	for _, n := range nodes {
		if indeg[n] == 0 {
			ready = append(ready, n)
		}
	}
	var out []string
	for len(ready) > 0 {
		sort.Strings(ready)
		n := ready[0]
		ready = ready[1:]
		out = append(out, n)
		for _, d := range rev[n] {
			indeg[d]--
			if indeg[d] == 0 {
				ready = append(ready, d)
			}
		}
	}
	if len(out) == len(nodes) {
		return out, nil
	}
	left := map[string]bool{}
	for _, n := range nodes {
		if indeg[n] > 0 {
			left[n] = true
		}
	}
	return nil, findCycle(nodes, left, deps)
}

// findCycle 在剩余节点中取字典序最小起点的第一个环（确定性）。
func findCycle(nodes []string, left map[string]bool, deps map[string][]string) []string {
	state := map[string]int{}
	var stack []string
	var res []string
	var dfs func(n string) bool
	dfs = func(n string) bool {
		state[n] = 1
		stack = append(stack, n)
		for _, d := range deps[n] {
			if !left[d] {
				continue
			}
			if state[d] == 1 {
				i := len(stack) - 1
				for stack[i] != d {
					i--
				}
				res = append(append([]string(nil), stack[i:]...), d)
				return true
			}
			if state[d] == 0 && dfs(d) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = 2
		return false
	}
	for _, n := range nodes {
		if left[n] && state[n] == 0 && dfs(n) {
			return res
		}
	}
	return nil
}

// buildArtifacts 产出 steps/units/files（只针对 install/upgrade 的组件；keep 无动作）。
func (r *run) buildArtifacts(order []string) {
	seq := 0
	step := func(comp, kind, f string, a ...any) {
		seq++
		r.plan.Steps = append(r.plan.Steps, Step{Seq: seq, Component: comp, Kind: kind, Summary: fmt.Sprintf(f, a...)})
	}
	l := r.lay
	for _, n := range order {
		c := r.assigned[n]
		act := r.action(n, c)
		if act == actKeep {
			continue
		}
		p := profileFor(n)
		verb := "安装"
		if act == actUpgrade {
			verb = "升级"
		}
		step(n, "download", "下载并校验 %s@%s（%s，%d 字节）", n, c.vs, c.platKey, c.art.Size)
		step(n, "extract", "解压到 %s/%s/releases/%s/", l.OptRoot, n, c.vs)
		if p.runUser != "" || p.dataDir != "" {
			step(n, "prepare", "创建运行用户与目录（%s）", orDash(p.runUser))
		}
		hasCfg := false
		for _, f := range p.files {
			if inVariants(f.variants, c.variant) {
				hasCfg = true
			}
		}
		if hasCfg {
			step(n, "configure", "生成配置与站点片段（已存在的配置/密钥绝不覆盖）")
		}
		var units []string
		for _, u := range p.units {
			if inVariants(u.variants, c.variant) {
				units = append(units, u.name)
				r.plan.Units = append(r.plan.Units, Unit{Component: n, Name: u.name, Kind: u.kind, Path: expandUnit(l, u.name), RunUser: p.runUser})
			}
		}
		if len(units) > 0 {
			step(n, "units", "安装 systemd 单元 %s", strings.Join(units, "、"))
		}
		step(n, "activate", "%s：原子切换 current → releases/%s", verb, c.vs)
		if len(units) > 0 {
			step(n, "start", "启动并启用服务")
			step(n, "health", "健康检查")
		}
		step(n, "record", "写入 state.json")

		rel := expand(l, "{opt}/{c}/releases/"+c.vs, n)
		r.plan.Files = append(r.plan.Files,
			File{Component: n, Path: rel, Kind: "dir", Mode: "0755", Owner: "root:root", Policy: PolicyManaged},
			File{Component: n, Path: expand(l, "{opt}/{c}/current", n), Kind: "symlink", Mode: "0777", Owner: "root:root", Policy: PolicyManaged, Note: "→ releases/" + c.vs})
		if p.dataDir != "" {
			r.plan.Files = append(r.plan.Files, File{Component: n, Path: expand(l, p.dataDir, n), Kind: "dir", Mode: "0750", Owner: p.runUser + ":" + p.runUser, Policy: PolicyCreateIfAbsent})
		}
		for _, f := range p.files {
			if inVariants(f.variants, c.variant) {
				r.plan.Files = append(r.plan.Files, File{Component: n, Path: expand(l, f.path, n), Kind: f.kind, Mode: f.mode, Owner: f.owner, Policy: f.policy, Note: f.note})
			}
		}
	}
	sort.SliceStable(r.plan.Units, func(i, j int) bool { return r.plan.Units[i].Name < r.plan.Units[j].Name })
	sort.SliceStable(r.plan.Files, func(i, j int) bool {
		a, b := r.plan.Files[i], r.plan.Files[j]
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		return a.Path < b.Path
	})
}

func expandUnit(l Layout, name string) string { return strings.TrimRight(l.UnitDir, "/") + "/" + name }

// ---- 资源 ----

func mountFor(path string, disks []Disk) (Disk, bool) {
	best, ok := Disk{}, false
	for _, d := range disks {
		m := strings.TrimRight(d.Mount, "/")
		if m == "" {
			m = "/"
		}
		if m != "/" && path != m && !strings.HasPrefix(path, m+"/") {
			continue
		}
		if !ok || len(m) > len(strings.TrimRight(best.Mount, "/")) || (len(m) == len(strings.TrimRight(best.Mount, "/")) && d.Mount < best.Mount) {
			best, ok = d, true
		}
	}
	return best, ok
}

func (r *run) checkResources(order []string) {
	var memNeed int64
	diskNeed := map[string]int64{}
	diskDisplay := map[string]Disk{}
	need := func(path string, mb int64) {
		if mb <= 0 {
			return
		}
		d, ok := mountFor(path, r.in.System.Disks)
		if !ok {
			r.block(Blocker{Kind: KindResource, Code: "disk.mount_unknown", Message: fmt.Sprintf("系统探测没有提供覆盖 %s 的挂载点，无法判断磁盘空间", path), Details: map[string]any{"path": path}})
			return
		}
		diskNeed[d.Mount] += mb
		diskDisplay[d.Mount] = d
	}
	for _, n := range order {
		c := r.assigned[n]
		if r.action(n, c) == actKeep || c.entry == nil {
			continue
		}
		memNeed += c.minMem
		need(expand(r.lay, "{opt}/{c}", n), c.minDisk)
		need(r.lay.StagingRoot, (c.art.Size+(1<<20)-1)>>20)
	}
	res := Resources{MemoryNeededMB: memNeed, MemoryAvailableMB: r.in.System.MemoryAvailableMB, Disks: []DiskUsage{}}
	if memNeed > 0 {
		switch {
		case r.in.System.MemoryAvailableMB <= 0:
			r.block(Blocker{Kind: KindResource, Code: "memory.unknown", Message: "系统探测没有提供可用内存，无法判断内存是否足够", Details: map[string]any{"needed_mb": memNeed}})
		case memNeed > r.in.System.MemoryAvailableMB:
			r.block(Blocker{Kind: KindResource, Code: "memory.insufficient",
				Message: fmt.Sprintf("可用内存不足：本次安装合计需要 %d MiB，本机可用 %d MiB", memNeed, r.in.System.MemoryAvailableMB),
				Hint:    "释放内存、增加内存，或减少本次选择的组件", Details: map[string]any{"needed_mb": memNeed, "available_mb": r.in.System.MemoryAvailableMB}})
		}
	}
	mounts := make([]string, 0, len(diskNeed))
	for m := range diskNeed {
		mounts = append(mounts, m)
	}
	sort.Strings(mounts)
	for _, m := range mounts {
		d := diskDisplay[m]
		res.Disks = append(res.Disks, DiskUsage{Mount: m, NeededMB: diskNeed[m], FreeMB: d.FreeMB})
		switch {
		case d.ReadOnly:
			r.block(Blocker{Kind: KindResource, Code: "disk.readonly", Message: fmt.Sprintf("挂载点 %s 是只读的，无法安装", m), Details: map[string]any{"mount": m}})
		case diskNeed[m] > d.FreeMB:
			r.block(Blocker{Kind: KindResource, Code: "disk.insufficient",
				Message: fmt.Sprintf("挂载点 %s 磁盘空间不足：本次安装在该挂载点合计需要 %d MiB，可用 %d MiB", m, diskNeed[m], d.FreeMB),
				Hint:    "清理空间、扩容该挂载点，或改装到空间更大的挂载点", Details: map[string]any{"mount": m, "needed_mb": diskNeed[m], "free_mb": d.FreeMB}})
		}
	}
	r.plan.Resources = res
}

// ---- 端口 ----

type portUse struct {
	comp string
	keep bool
	p    PlannedPort
	addr bindAddr
	bad  bool
}

func (r *run) plannedPorts(n string, c *cand) []PlannedPort {
	if c.entry == nil {
		return nil
	}
	prof := profileFor(n)
	if !inVariants(prof.portVariants, c.variant) {
		return nil
	}
	var out []PlannedPort
	for _, p := range c.entry.Ports {
		bind := p.Bind
		if strings.TrimSpace(bind) == "" {
			bind = "0.0.0.0"
		}
		out = append(out, PlannedPort{Component: n, Proto: normProto(p.Proto), Port: p.Port, Bind: bind, Purpose: p.Purpose, Conditional: p.Conditional})
	}
	return out
}

func (r *run) checkPorts(plan []string) {
	inPlan := map[string]bool{}
	for _, n := range plan {
		inPlan[n] = true
	}
	var uses []portUse
	byComp := map[string][]PlannedPort{}
	for _, n := range r.sortedAssigned() {
		c := r.assigned[n]
		ps := r.plannedPorts(n, c)
		if inPlan[n] {
			byComp[n] = ps
		}
		for _, p := range ps {
			uses = append(uses, portUse{comp: n, keep: r.action(n, c) == actKeep, p: p})
		}
	}
	// --set 换端口（只作用于进入 Plan 的组件）
	ovs := append([]PortOverride(nil), r.in.PortOverrides...)
	sort.SliceStable(ovs, func(i, j int) bool {
		a, b := ovs[i], ovs[j]
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		if a.Proto != b.Proto {
			return a.Proto < b.Proto
		}
		return a.Port < b.Port
	})
	for _, o := range ovs {
		hit := false
		if o.To < 1 || o.To > 65535 {
			r.block(Blocker{Kind: KindUsage, Code: "port.override_invalid", Component: o.Component, Message: fmt.Sprintf("--set 的目标端口 %d 不在 1–65535", o.To)})
			continue
		}
		if o.Bind != "" {
			if _, ok := parseBind(o.Bind); !ok {
				r.block(Blocker{Kind: KindUsage, Code: "port.override_invalid", Component: o.Component, Message: fmt.Sprintf("--set 的绑定地址 %q 不是合法 IP", trimName(o.Bind))})
				continue
			}
		}
		for i := range uses {
			u := &uses[i]
			if inPlan[u.comp] && u.comp == o.Component && u.p.Proto == normProto(o.Proto) && u.p.Port == o.Port && !u.p.Overridden {
				u.p.Port, u.p.Overridden = o.To, true
				if o.Bind != "" {
					u.p.Bind = o.Bind
				}
				hit = true
			}
		}
		if !hit {
			r.block(Blocker{Kind: KindUsage, Code: "port.override_unmatched", Component: o.Component, Message: fmt.Sprintf("--set 没有匹配的端口：%s %s/%d", o.Component, o.Proto, o.Port)})
		}
	}
	for i := range uses {
		a, ok := parseBind(uses[i].p.Bind)
		if !ok {
			r.block(Blocker{Kind: KindUsage, Code: "port.bind_invalid", Component: uses[i].comp, Message: fmt.Sprintf("%s 声明的绑定地址 %q 不是合法 IP", uses[i].comp, trimName(uses[i].p.Bind))})
			a, uses[i].bad = bindAddr{any: true}, true
		}
		uses[i].addr = a
	}
	for _, n := range plan {
		var ps []PlannedPort
		for _, u := range uses {
			if u.comp == n {
				ps = append(ps, u.p)
			}
		}
		r.plan.Ports = append(r.plan.Ports, ps...)
	}
	r.portsAmongComponents(uses)
	r.portsAgainstListeners(uses)
}

func (r *run) portsAmongComponents(uses []portUse) {
	for i := range uses {
		for j := i + 1; j < len(uses); j++ {
			a, b := uses[i], uses[j]
			if a.comp == b.comp || (a.keep && b.keep) || a.p.Proto != b.p.Proto || a.p.Port != b.p.Port {
				continue
			}
			if ok, kind := overlap(a.addr, b.addr); ok {
				x, y := a, b
				if y.comp < x.comp {
					x, y = y, x
				}
				r.block(Blocker{Kind: KindPort, Code: "port.conflict.component", Component: x.comp,
					Message: fmt.Sprintf("端口冲突：%s（%s %s/%d）与 %s（%s %s/%d）都要占用，且地址重叠（%s）", x.comp, x.p.Bind, x.p.Proto, x.p.Port, y.comp, y.p.Bind, y.p.Proto, y.p.Port, kind),
					Hint:    "换端口（--set）、取消其中一个组件，或把它们安装到不同主机", Details: map[string]any{"proto": x.p.Proto, "port": x.p.Port, "match": kind,
						"a": map[string]any{"component": x.comp, "bind": x.p.Bind}, "b": map[string]any{"component": y.comp, "bind": y.p.Bind}}})
			}
		}
	}
}

func (r *run) owns(comp string, l Listener) bool {
	inst, has := r.in.Installed.Components[comp]
	if !has {
		return false
	}
	return (l.Unit != "" && l.Unit == inst.Unit) || l.Process == "lingedge-"+comp
}

func (r *run) portsAgainstListeners(uses []portUse) {
	type lis struct {
		l    Listener
		addr bindAddr
	}
	var ls []lis
	for _, l := range r.in.System.Listeners {
		l.Proto = normProto(l.Proto)
		a, ok := parseBind(l.Addr)
		if !ok {
			r.warn("listener.addr_invalid", "", "本机监听 %s/%d 的地址 %q 无法解析，按通配处理（更保守）", l.Proto, l.Port, trimName(l.Addr))
			a = bindAddr{any: true}
		}
		ls = append(ls, lis{l, a})
	}
	sort.SliceStable(ls, func(i, j int) bool {
		a, b := ls[i].l, ls[j].l
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.Proto != b.Proto {
			return a.Proto < b.Proto
		}
		if a.Addr != b.Addr {
			return a.Addr < b.Addr
		}
		return a.PID < b.PID
	})
	for _, u := range uses {
		if u.keep {
			continue
		}
		for _, x := range ls {
			if x.l.Proto != u.p.Proto || x.l.Port != u.p.Port || r.owns(u.comp, x.l) {
				continue
			}
			ok, kind := overlap(u.addr, x.addr)
			if !ok {
				continue
			}
			who := x.l.Process
			if who == "" {
				who = "未知进程"
			}
			hint := "换端口（--set）、释放占用者（Tools 不会自动停用任何服务），或取消"
			code := "port.conflict.listener"
			if r.in.System.Baota.Detected && (u.p.Port == 80 || u.p.Port == 443) {
				hint = "宝塔环境下 80/443 被占用（R-10）：(a) 迁移宝塔站点端口，(b) 节点与面板分机，(c) 取消；Tools 不修改宝塔站点"
			}
			r.block(Blocker{Kind: KindPort, Code: code, Component: u.comp,
				Message: fmt.Sprintf("端口冲突：%s 要占用 %s %s/%d，本机已被 %s（pid %d，用户 %s）监听 %s（%s）", u.comp, u.p.Bind, u.p.Proto, u.p.Port, who, x.l.PID, orDash(x.l.User), x.l.Addr, kind),
				Hint:    hint, Details: map[string]any{"proto": u.p.Proto, "port": u.p.Port, "match": kind, "bind": u.p.Bind,
					"occupant": map[string]any{"process": x.l.Process, "pid": x.l.PID, "user": x.l.User, "addr": x.l.Addr, "unit": x.l.Unit}}})
		}
	}
}

// finish 规范化输出：nil 切片 → 空切片，警告/阻断稳定排序并去重。
func (r *run) finish() {
	p := r.plan
	sort.SliceStable(p.Warnings, func(i, j int) bool {
		return lessWB(p.Warnings[i].Code, p.Warnings[i].Component, p.Warnings[i].Message, p.Warnings[j].Code, p.Warnings[j].Component, p.Warnings[j].Message)
	})
	sort.SliceStable(p.Blockers, func(i, j int) bool {
		return lessWB(p.Blockers[i].Code, p.Blockers[i].Component, p.Blockers[i].Message, p.Blockers[j].Code, p.Blockers[j].Component, p.Blockers[j].Message)
	})
	p.Warnings = dedupW(p.Warnings)
	p.Blockers = dedupB(p.Blockers)
	if p.Components == nil {
		p.Components = []PlanComponent{}
	}
	if p.Order == nil {
		p.Order = []string{}
	}
	if p.Steps == nil {
		p.Steps = []Step{}
	}
	if p.Ports == nil {
		p.Ports = []PlannedPort{}
	}
	if p.Units == nil {
		p.Units = []Unit{}
	}
	if p.Files == nil {
		p.Files = []File{}
	}
	if p.Warnings == nil {
		p.Warnings = []Warning{}
	}
	if p.Blockers == nil {
		p.Blockers = []Blocker{}
	}
	if p.Resources.Disks == nil {
		p.Resources.Disks = []DiskUsage{}
	}
}

func lessWB(c1, p1, m1, c2, p2, m2 string) bool {
	if c1 != c2 {
		return c1 < c2
	}
	if p1 != p2 {
		return p1 < p2
	}
	return m1 < m2
}

func dedupW(in []Warning) []Warning {
	var out []Warning
	for i, w := range in {
		if i > 0 && w == in[i-1] {
			continue
		}
		out = append(out, w)
	}
	return out
}

func dedupB(in []Blocker) []Blocker {
	var out []Blocker
	for i, b := range in {
		if i > 0 && b.Code == in[i-1].Code && b.Component == in[i-1].Component && b.Message == in[i-1].Message {
			continue
		}
		out = append(out, b)
	}
	return out
}
