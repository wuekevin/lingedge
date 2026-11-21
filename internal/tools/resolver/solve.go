package resolver

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

type edgeKind int

const (
	edgeRequires edgeKind = iota // requires.components：同机已装或本次一并安装的组件
	edgePeer                     // requires.peer：本机存在该组件时强制（否则只在 enroll 时比对并警告）
	edgeConflict                 // conflicts[]
)

type edge struct {
	kind edgeKind
	to   string
	cs   string // 约束原文；conflict 为空 = 任意版本
	c    manifest.Constraint
	src  string // component@version
}

// cand 是组件的一个候选（entry 为 nil 表示合成的「保持已装版本」候选：清单中已没有该组件/版本）。
type cand struct {
	comp    string
	ver     manifest.Version
	vs      string
	entry   *manifest.VersionEntry
	variant string
	edges   []edge
	keep    bool // 即已装版本
	minMem  int64
	minDisk int64
	art     manifest.Artifact
	platKey string
}

type candSet struct {
	cands   []*cand
	rejects []string
}

var reDepVersion = regexp.MustCompile(`^v?(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// normalizeDepVersion 取外部依赖原始版本串的数字前缀并补全为 x.y.z（8.0.36-0ubuntu0.22.04.1 → 8.0.36；v20.11.0 → 20.11.0）。
// 发行版后缀必须丢弃：SemVer 把它当预发布段，约束 `>=8.0.16` 不会接纳预发布。
func normalizeDepVersion(s string) (manifest.Version, bool) {
	m := reDepVersion.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return manifest.Version{}, false
	}
	p := func(i int) string {
		if m[i] == "" {
			return "0"
		}
		return strings.TrimLeft(m[i], "0") + zeroIfEmpty(m[i])
	}
	v, err := manifest.ParseVersion(p(1) + "." + p(2) + "." + p(3))
	return v, err == nil
}

func zeroIfEmpty(s string) string {
	if strings.TrimLeft(s, "0") == "" {
		return "0"
	}
	return ""
}

func depName(n string) string {
	switch strings.ToLower(n) {
	case "node", "nodejs", "node.js":
		return "nodejs"
	}
	return strings.ToLower(n)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func pickVariant(e *manifest.VersionEntry, req string) (string, string) {
	if req != "" {
		if _, ok := e.Variants[req]; !ok {
			return "", fmt.Sprintf("没有变体 %q", req)
		}
		return req, ""
	}
	if len(e.Variants) == 1 {
		for k := range e.Variants {
			return k, ""
		}
	}
	if _, ok := e.Variants["default"]; ok {
		return "default", ""
	}
	names := make([]string, 0, len(e.Variants))
	for k := range e.Variants {
		names = append(names, k)
	}
	sort.Strings(names)
	return "", "有多个变体（" + strings.Join(names, "|") + "）而未指定；请显式选择该组件及变体"
}

func (r *run) cands(name string) *candSet {
	if cs, ok := r.memo[name]; ok {
		return cs
	}
	cs := r.buildCands(name)
	r.memo[name] = cs
	return cs
}

func (r *run) buildCands(name string) *candSet {
	cs := &candSet{}
	comp, inM := r.m.Components[name]
	w, wanted := r.wants[name]
	inst, has := r.in.Installed.Components[name]
	iv, ivOK := r.instVer[name]
	reject := func(f string, a ...any) { cs.rejects = append(cs.rejects, fmt.Sprintf(f, a...)) }

	synthetic := func() *cand {
		c := &cand{comp: name, keep: true, vs: inst.Version}
		if ivOK {
			c.ver, c.vs = iv, iv.String()
		}
		return c
	}
	if !inM || comp.Status != manifest.CompAvailable {
		if has && ivOK {
			cs.cands = append(cs.cands, synthetic())
			return cs
		}
		reject("清单中没有可安装的组件 %s", name)
		return cs
	}

	var keepCand *cand
	var rest []*cand
	pinned := wanted && w.Version != ""
	var pw manifest.Version
	if pinned {
		pw, _ = manifest.ParseVersion(w.Version)
	}
	type ev struct {
		e  *manifest.VersionEntry
		pv manifest.Version
	}
	var evs []ev
	for i := range comp.Versions {
		pv, err := manifest.ParseVersion(comp.Versions[i].Version)
		if err != nil {
			reject("版本 %q 不是严格 SemVer", comp.Versions[i].Version)
			continue
		}
		evs = append(evs, ev{&comp.Versions[i], pv})
	}
	sort.SliceStable(evs, func(i, j int) bool {
		if c := evs[i].pv.Compare(evs[j].pv); c != 0 {
			return c > 0
		}
		return evs[i].pv.String() > evs[j].pv.String()
	})

	for _, x := range evs {
		e, pv := x.e, x.pv
		vs := pv.String()
		isInst := has && ivOK && pv.Compare(iv) == 0
		if pinned && pv.Compare(pw) != 0 {
			continue
		}
		if has && ivOK && pv.Compare(iv) < 0 {
			continue // 不降级（降级走 rollback）
		}
		if pv.Prerelease() && !pinned && !isInst {
			continue // 默认只选稳定版
		}
		if _, rev := r.m.RevokedVersion(name, pv); rev && !isInst {
			reject("%s@%s 已被发行方吊销", name, vs)
			continue
		}
		req := ""
		if wanted {
			req = w.Variant
		} else if has {
			req = r.in.InstalledVariants[name]
		}
		vname, why := pickVariant(e, req)
		if why != "" && (!isInst || wanted) {
			reject("%s@%s：%s", name, vs, why)
			continue
		}
		c := &cand{comp: name, ver: pv, vs: vs, entry: e, variant: vname, keep: isInst}
		lists := reqLists(e, vname)
		if !isInst {
			art, plat, ok := r.artifactFor(e, vname)
			if !ok {
				reject("%s@%s（%s）没有适用于 %s 的包", name, vs, vname, r.platform)
				continue
			}
			c.art, c.platKey = art, plat
			if has && ivOK && e.MinUpgradeFrom != "" {
				if mv, err := manifest.ParseVersion(e.MinUpgradeFrom); err == nil && iv.Compare(mv) < 0 {
					reject("%s@%s 要求已装版本 ≥ %s（当前 %s），不能直接升级，需逐级升级", name, vs, mv.String(), iv.String())
					continue
				}
			}
			if why := r.systemReject(name, lists, c); why != "" {
				reject("%s@%s：%s", name, vs, why)
				continue
			}
		}
		edges, why := buildEdges(name, vs, e, lists)
		if why != "" {
			reject("%s@%s：%s", name, vs, why)
			continue
		}
		c.edges = edges
		if isInst {
			keepCand = c
		} else {
			rest = append(rest, c)
		}
	}
	if !wanted && has && ivOK {
		if keepCand == nil {
			keepCand = synthetic()
		}
		cs.cands = append(cs.cands, keepCand)
		cs.cands = append(cs.cands, rest...)
		return cs
	}
	if keepCand != nil {
		// wanted + 已装：保持已装版本按版本序落在 rest 之中（最高者优先）
		rest = append(rest, keepCand)
		sort.SliceStable(rest, func(i, j int) bool { return rest[i].ver.Compare(rest[j].ver) > 0 })
	}
	cs.cands = rest
	return cs
}

func reqLists(e *manifest.VersionEntry, variant string) []*manifest.Requires {
	var out []*manifest.Requires
	if e.Requires != nil {
		out = append(out, e.Requires)
	}
	if v, ok := e.Variants[variant]; ok && v.Requires != nil {
		out = append(out, v.Requires)
	}
	return out
}

func (r *run) artifactFor(e *manifest.VersionEntry, variant string) (manifest.Artifact, string, bool) {
	v, ok := e.Variants[variant]
	if !ok {
		return manifest.Artifact{}, "", false
	}
	if a, ok := v.Artifacts[r.platform]; ok {
		return a, r.platform, true
	}
	if a, ok := v.Artifacts["any"]; ok {
		return a, "any", true
	}
	return manifest.Artifact{}, "", false
}

// systemReject 检查与候选版本自身有关的系统要求：os/arch，及外部依赖/运行时（版本不满足或必需而缺失）。
func (r *run) systemReject(name string, lists []*manifest.Requires, c *cand) string {
	for _, rq := range lists {
		if s := rq.System; s != nil {
			if len(s.OS) > 0 && !contains(s.OS, r.in.System.OS) {
				return fmt.Sprintf("要求 OS ∈ %v，本机为 %s", s.OS, r.in.System.OS)
			}
			if len(s.Arch) > 0 && !contains(s.Arch, r.in.System.Arch) {
				return fmt.Sprintf("要求 arch ∈ %v，本机为 %s", s.Arch, r.in.System.Arch)
			}
			c.minMem = max64(c.minMem, s.MinMemoryMB)
			c.minDisk = max64(c.minDisk, s.MinDiskMB)
		}
		for _, k := range sortedKeys(rq.External) {
			if why := r.depReject("external", name, k, rq.External[k], false); why != "" {
				return why
			}
		}
		for _, k := range sortedKeys(rq.Runtime) {
			if why := r.depReject("runtime", name, k, rq.Runtime[k], true); why != "" {
				return why
			}
		}
	}
	return ""
}

// depReject：已探测到则版本必须满足；未探测到：runtime 一律拒绝；external 仅当该组件允许远端（server 的 mysql/redis）时放行。
func (r *run) depReject(kind, comp, key, cs string, mustExist bool) string {
	n := depName(key)
	d, ok := r.in.System.Deps[n]
	if !ok || !d.Detected {
		if !mustExist && inList(profileFor(comp).remoteOK, n) {
			return ""
		}
		return fmt.Sprintf("要求 %s %s %s，但本机未探测到", kind, key, cs)
	}
	c, err := manifest.ParseConstraint(cs)
	if err != nil {
		return fmt.Sprintf("清单中的约束 %q 非法", cs)
	}
	v, ok := normalizeDepVersion(d.Version)
	if !ok {
		return fmt.Sprintf("要求 %s %s %s，但探测到的版本串 %q 无法解析", kind, key, cs, d.Version)
	}
	if !c.Check(v) {
		return fmt.Sprintf("要求 %s %s %s，本机探测到 %s", kind, key, cs, d.Version)
	}
	return ""
}

func buildEdges(name, vs string, e *manifest.VersionEntry, lists []*manifest.Requires) ([]edge, string) {
	src := name + "@" + vs
	var out []edge
	add := func(k edgeKind, to, cs string) string {
		var c manifest.Constraint
		if cs != "" {
			var err error
			if c, err = manifest.ParseConstraint(cs); err != nil {
				return fmt.Sprintf("清单中的约束 %q 非法", cs)
			}
		}
		out = append(out, edge{kind: k, to: to, cs: cs, c: c, src: src})
		return ""
	}
	for _, rq := range lists {
		for _, k := range sortedKeys(rq.Components) {
			if w := add(edgeRequires, k, rq.Components[k]); w != "" {
				return nil, w
			}
		}
		for _, k := range sortedKeys(rq.Peer) {
			if w := add(edgePeer, k, rq.Peer[k]); w != "" {
				return nil, w
			}
		}
	}
	for _, cf := range e.Conflicts {
		if w := add(edgeConflict, cf.Component, cf.Version); w != "" {
			return nil, w
		}
	}
	return out, ""
}

func (e edge) violatedBy(to *cand) string {
	switch e.kind {
	case edgeConflict:
		if e.cs == "" || e.c.Check(to.ver) {
			return fmt.Sprintf("%s 与 %s@%s 冲突", e.src, to.comp, to.vs)
		}
	default:
		word := "要求"
		if e.kind == edgePeer {
			word = "（peer）要求"
		}
		if !e.c.Check(to.ver) {
			return fmt.Sprintf("%s %s %s %s，而 %s 的版本是 %s", e.src, word, e.to, e.cs, e.to, to.vs)
		}
	}
	return ""
}

type failure struct {
	depth int
	comp  string
	lines []string
}

const searchLimit = 200000

type solver struct {
	r        *run
	assigned map[string]*cand
	intro    map[string]string // 组件 → 引入者：want | installed | 组件名（被其依赖）
	fail     *failure
	steps    int
	exceeded bool
}

func (s *solver) names() []string {
	out := make([]string, 0, len(s.intro))
	for n := range s.intro {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *solver) pathTo(comp string) []string {
	var rev []string
	seen := map[string]bool{}
	for cur := comp; cur != "" && !seen[cur]; {
		seen[cur] = true
		by := s.intro[cur]
		switch by {
		case "want":
			rev = append(rev, cur+"（用户选择）")
			cur = ""
		case "installed":
			rev = append(rev, cur+"（已安装）")
			cur = ""
		default:
			if a := s.assigned[by]; a != nil {
				rev = append(rev, fmt.Sprintf("%s（被 %s@%s 依赖）", cur, by, a.vs))
			} else {
				rev = append(rev, cur+"（被 "+by+" 依赖）")
			}
			cur = by
		}
	}
	out := make([]string, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out
}

func (s *solver) record(comp string, reasons []string) {
	if s.fail != nil && s.fail.depth >= len(s.assigned) {
		return
	}
	lines := s.pathTo(comp)
	for i := range lines {
		if i > 0 {
			lines[i] = "→ " + lines[i]
		}
	}
	lines = append(lines, "✗ "+comp+" 没有满足全部约束的版本：")
	const capN = 12
	for i, x := range reasons {
		if i == capN {
			lines = append(lines, fmt.Sprintf("  - …另有 %d 项", len(reasons)-capN))
			break
		}
		lines = append(lines, "  - "+x)
	}
	s.fail = &failure{depth: len(s.assigned), comp: comp, lines: lines}
}

func (s *solver) compatible(c *cand) []string {
	var why []string
	for _, e := range c.edges {
		if e.to == c.comp {
			continue
		}
		if a, ok := s.assigned[e.to]; ok {
			if m := e.violatedBy(a); m != "" {
				why = append(why, fmt.Sprintf("%s@%s：%s", c.comp, c.vs, m))
			}
		} else if e.kind == edgeRequires && s.r.internalDep(e.to) == depMissing {
			why = append(why, fmt.Sprintf("%s@%s：被依赖的组件 %s 不在清单中", c.comp, c.vs, e.to))
		}
	}
	for _, a := range s.assigned {
		for _, e := range a.edges {
			if e.to == c.comp {
				if m := e.violatedBy(c); m != "" {
					why = append(why, fmt.Sprintf("%s@%s：%s", c.comp, c.vs, m))
				}
			}
		}
	}
	sort.Strings(why)
	return why
}

type depState int

const (
	depInert    depState = iota // 非内部组件：不在 Plan/已装则不强制（远端部署）
	depInternal                 // 内部组件：自动加入闭包
	depMissing                  // 清单中没有、也未安装：无法满足
)

func (r *run) internalDep(to string) depState {
	comp, ok := r.m.Components[to]
	if !ok {
		if _, has := r.in.Installed.Components[to]; has {
			return depInert
		}
		if _, w := r.wants[to]; w {
			return depInert
		}
		return depMissing
	}
	if comp.Internal {
		return depInternal
	}
	return depInert
}

func (s *solver) search() bool {
	s.steps++
	if s.steps > searchLimit {
		s.exceeded = true
		return false
	}
	var next string
	for _, n := range s.names() {
		if _, ok := s.assigned[n]; !ok {
			next = n
			break
		}
	}
	if next == "" {
		return true
	}
	cs := s.r.cands(next)
	reasons := append([]string(nil), cs.rejects...)
	for _, c := range cs.cands {
		if why := s.compatible(c); len(why) > 0 {
			reasons = append(reasons, why...)
			continue
		}
		var added []string
		s.assigned[next] = c
		for _, e := range c.edges {
			if e.kind == edgeRequires && s.r.internalDep(e.to) == depInternal {
				if _, ok := s.intro[e.to]; !ok {
					s.intro[e.to] = next
					added = append(added, e.to)
				}
			}
		}
		if s.search() {
			return true
		}
		delete(s.assigned, next)
		for _, a := range added {
			delete(s.intro, a)
		}
		if s.exceeded {
			return false
		}
	}
	if len(reasons) == 0 {
		reasons = []string{"没有候选版本"}
	}
	s.record(next, reasons)
	return false
}

func contains(list []string, v string) bool { return inList(list, v) }

func inList(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
