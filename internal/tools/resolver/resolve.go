package resolver

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
)

type run struct {
	in       Input
	lay      Layout
	m        *manifest.Manifest
	plan     *Plan
	wants    map[string]Want
	instVer  map[string]manifest.Version
	platform string
	memo     map[string]*candSet

	assigned map[string]*cand
	intro    map[string]string
}

// Resolve 求解 Plan。纯函数：不做 IO、不读时钟/随机/环境，输出与输入切片顺序、map 迭代序无关。
// 失败不以 error 返回，而是写入 Plan.Blockers（--dry-run 同样展示）；用 Plan.Err() 得到带退出码分类的错误。
func Resolve(in Input) *Plan {
	r := &run{in: in, lay: in.Layout.withDefaults(), m: in.Manifest, wants: map[string]Want{}, instVer: map[string]manifest.Version{},
		memo: map[string]*candSet{}, plan: &Plan{}}
	r.execute()
	r.finish()
	return r.plan
}

func (r *run) block(b Blocker) { r.plan.Blockers = append(r.plan.Blockers, b) }
func (r *run) warn(code, comp, f string, a ...any) {
	r.plan.Warnings = append(r.plan.Warnings, Warning{Code: code, Component: comp, Message: fmt.Sprintf(f, a...)})
}

func (r *run) execute() {
	if r.m == nil {
		r.block(Blocker{Kind: KindUsage, Code: "manifest.missing", Message: "没有可用的清单（须先获取并验签清单）", Hint: "运行 lingedge update --check-only 或指定 --offline bundle"})
		return
	}
	if r.in.System.OS != "linux" {
		r.block(Blocker{Kind: KindSystem, Code: "system.os_unsupported", Message: fmt.Sprintf("不支持的操作系统 %q：Tools 仅支持 Linux", r.in.System.OS)})
	}
	if a := r.in.System.Arch; a != "amd64" && a != "arm64" {
		r.block(Blocker{Kind: KindSystem, Code: "system.arch_unsupported", Message: fmt.Sprintf("不支持的 CPU 架构 %q：仅支持 amd64 与 arm64", a)})
	}
	if len(r.plan.Blockers) > 0 {
		return
	}
	r.platform = r.in.System.OS + "-" + r.in.System.Arch
	for n, c := range r.in.Installed.Components {
		if v, err := manifest.ParseVersion(c.Version); err == nil {
			r.instVer[n] = v
		}
	}
	if !r.normalize() {
		return
	}
	if !r.solve() {
		return
	}
	r.assemble()
}

// normalize 是 §10.3 步骤 1：规范化选择（内置/未发布/未知/固定版本检查）。任一阻断即停止（不进入求解）。
func (r *run) normalize() bool {
	ws := append([]Want(nil), r.in.Want...)
	sort.SliceStable(ws, func(i, j int) bool {
		a, b := ws[i], ws[j]
		if a.Component != b.Component {
			return a.Component < b.Component
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Variant < b.Variant
	})
	if len(ws) == 0 {
		r.block(Blocker{Kind: KindUsage, Code: "want.empty", Message: "没有选择任何组件", Hint: "运行 lingedge install <组件…>"})
		return false
	}
	for _, w := range ws {
		if prev, dup := r.wants[w.Component]; dup {
			if prev != w {
				r.block(Blocker{Kind: KindUsage, Code: "want.conflict", Component: w.Component,
					Message: fmt.Sprintf("组件 %s 被重复选择且版本/变体不一致", w.Component), Hint: "每个组件只能选择一次"})
			}
			continue
		}
		r.wants[w.Component] = w
	}
	names := make([]string, 0, len(r.wants))
	for n := range r.wants {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		r.checkWant(name, r.wants[name])
	}
	return len(r.plan.Blockers) == 0
}

func (r *run) checkWant(name string, w Want) {
	if !registry.ValidComponent(name) {
		r.block(Blocker{Kind: KindUsage, Code: "component.invalid", Component: name, Message: fmt.Sprintf("组件名 %q 不合法", trimName(name))})
		return
	}
	comp, inM := r.m.Components[name]
	if name == "scheduler" || name == "worker" || (inM && comp.Status == manifest.CompBuiltin) {
		in := comp.BuiltinIn
		if in == "" {
			in = "server"
		}
		r.block(Blocker{Kind: KindBuiltin, Code: "component.builtin", Component: name,
			Message: fmt.Sprintf("%s 已内置于 %s，无需单独安装（D-16）", name, in), Hint: "它随 Server 的 --roles 启用，不通过 lingedge install 安装"})
		return
	}
	if !inM {
		r.block(Blocker{Kind: KindUsage, Code: "component.unknown", Component: name, Message: fmt.Sprintf("清单中没有组件 %q", trimName(name)), Hint: "运行 lingedge install --help 查看可选组件"})
		return
	}
	if comp.Status == manifest.CompUnreleased {
		r.block(Blocker{Kind: KindNotReleased, Code: "component.not_released", Component: name,
			Message: fmt.Sprintf("%s 尚未发布（status: unreleased）", name), Hint: "该组件尚未发布；不会伪造安装成功"})
		return
	}
	if comp.Internal {
		r.block(Blocker{Kind: KindUsage, Code: "component.internal", Component: name,
			Message: fmt.Sprintf("%s 是内部组件，随依赖它的组件自动安装，不能直接选择", name)})
		return
	}
	if w.Version == "" {
		if w.Variant != "" && !r.anyVariant(comp, w.Variant) {
			r.block(Blocker{Kind: KindUsage, Code: "variant.not_found", Component: name, Message: fmt.Sprintf("%s 没有变体 %q", name, w.Variant)})
		} else if w.Variant == "" {
			r.variantRequired(name, comp)
		}
		return
	}
	pv, err := manifest.ParseVersion(w.Version)
	if err != nil {
		r.block(Blocker{Kind: KindUsage, Code: "version.invalid", Component: name, Message: fmt.Sprintf("固定版本 %q 不是严格 SemVer", trimName(w.Version))})
		return
	}
	var entry *manifest.VersionEntry
	for i := range comp.Versions {
		if v, err := manifest.ParseVersion(comp.Versions[i].Version); err == nil && v.Compare(pv) == 0 {
			entry = &comp.Versions[i]
		}
	}
	if entry == nil {
		r.block(Blocker{Kind: KindUsage, Code: "version.not_found", Component: name, Message: fmt.Sprintf("清单中没有 %s@%s", name, pv.String())})
		return
	}
	if err := r.m.CheckTarget(name, pv.String()); err != nil {
		msg := err.Error()
		var me *manifest.Error
		if errors.As(err, &me) {
			msg = me.Msg
		}
		r.block(Blocker{Kind: KindRevoked, Code: "version.revoked", Component: name, Message: msg, Hint: "该版本已被发行方吊销，请选择其他版本"})
		return
	}
	if w.Variant == "" {
		if _, why := pickVariant(entry, ""); why != "" {
			r.block(Blocker{Kind: KindUsage, Code: "variant.required", Component: name, Message: fmt.Sprintf("%s@%s %s", name, pv.String(), why)})
			return
		}
	}
	if w.Variant != "" {
		if _, ok := entry.Variants[w.Variant]; !ok {
			r.block(Blocker{Kind: KindUsage, Code: "variant.not_found", Component: name, Message: fmt.Sprintf("%s@%s 没有变体 %q", name, pv.String(), w.Variant)})
			return
		}
	}
	iv, has := r.instVer[name]
	if !has {
		return
	}
	if pv.Compare(iv) < 0 {
		r.block(Blocker{Kind: KindUpgradePath, Code: "upgrade.downgrade", Component: name,
			Message: fmt.Sprintf("%s 已装 %s，高于固定版本 %s：不支持降级", name, iv.String(), pv.String()), Hint: "回退请使用 lingedge rollback"})
		return
	}
	if entry.MinUpgradeFrom != "" && pv.Compare(iv) != 0 {
		if mv, err := manifest.ParseVersion(entry.MinUpgradeFrom); err == nil && iv.Compare(mv) < 0 {
			path := r.upgradePath(name, comp, iv, pv)
			b := Blocker{Kind: KindUpgradePath, Code: "upgrade.min_upgrade_from", Component: name,
				Message: fmt.Sprintf("%s@%s 要求已装版本 ≥ %s，当前为 %s，不能直接升级", name, pv.String(), mv.String(), iv.String()),
				Hint:    "须逐级升级", Details: map[string]any{"installed": iv.String(), "min_upgrade_from": mv.String(), "upgrade_path": path}}
			if len(path) > 0 {
				b.Hint = "须逐级升级：" + strings.Join(path, " → ")
			}
			r.block(b)
		}
	}
}

func trimName(s string) string {
	if len(s) > 48 {
		return s[:48] + "…"
	}
	return s
}

func (r *run) anyVariant(comp manifest.Component, v string) bool {
	for i := range comp.Versions {
		if _, ok := comp.Versions[i].Variants[v]; ok {
			return true
		}
	}
	return false
}

// upgradePath 给出从 from 逐级升到 target 的路径（每一步都满足 min_upgrade_from；贪心取每步可达的最高版本）；无路返回 nil。
func (r *run) upgradePath(name string, comp manifest.Component, from, target manifest.Version) []string {
	type ev struct {
		v   manifest.Version
		min *manifest.Version
	}
	var evs []ev
	for i := range comp.Versions {
		v, err := manifest.ParseVersion(comp.Versions[i].Version)
		if err != nil || v.Prerelease() {
			continue
		}
		if _, rev := r.m.RevokedVersion(name, v); rev {
			continue
		}
		x := ev{v: v}
		if s := comp.Versions[i].MinUpgradeFrom; s != "" {
			if mv, err := manifest.ParseVersion(s); err == nil {
				x.min = &mv
			}
		}
		evs = append(evs, x)
	}
	reach := func(e ev, cur manifest.Version) bool { return e.min == nil || cur.Compare(*e.min) >= 0 }
	cur := from
	var path []string
	for i := 0; i <= len(evs); i++ {
		for _, e := range evs {
			if e.v.Compare(target) == 0 && reach(e, cur) {
				return append(path, target.String())
			}
		}
		var best *ev
		for k := range evs {
			e := evs[k]
			if e.v.Compare(cur) > 0 && e.v.Compare(target) < 0 && reach(e, cur) && (best == nil || e.v.Compare(best.v) > 0) {
				best = &evs[k]
			}
		}
		if best == nil {
			return nil
		}
		path = append(path, best.v.String())
		cur = best.v
	}
	return nil
}

// solve 是 §10.3 步骤 2–3 与 5、7：候选版本、约束传播与回溯（含外部依赖与 conflicts）。
func (r *run) solve() bool {
	s := &solver{r: r, assigned: map[string]*cand{}, intro: map[string]string{}}
	for n := range r.in.Installed.Components {
		s.intro[n] = "installed"
	}
	for n := range r.wants {
		s.intro[n] = "want"
	}
	if s.search() {
		r.assigned, r.intro = s.assigned, s.intro
		return true
	}
	if s.exceeded {
		r.block(Blocker{Kind: KindUnsatisfiable, Code: "resolve.search_limit", Message: "依赖图过大或约束过于纠缠，求解超出规模上限", Hint: "减少同时选择的组件，或固定版本（--version）"})
		return false
	}
	f := s.fail
	b := Blocker{Kind: KindUnsatisfiable, Code: "resolve.unsatisfiable", Component: f.comp,
		Message: fmt.Sprintf("依赖/版本约束无解：%s 没有满足全部约束的版本", f.comp), Chain: f.lines,
		Hint: "放宽固定版本、先升级冲突的已装组件，或移除冲突的选择；详见冲突链"}
	r.block(b)
	return false
}

// variantRequired：未指定变体且最新可选版本有多个变体而无 default（如 website 的 static/ssr）→ 用法错误（2）。
func (r *run) variantRequired(name string, comp manifest.Component) {
	var best *manifest.VersionEntry
	var bv manifest.Version
	for i := range comp.Versions {
		v, err := manifest.ParseVersion(comp.Versions[i].Version)
		if err != nil || v.Prerelease() {
			continue
		}
		if best == nil || v.Compare(bv) > 0 {
			best, bv = &comp.Versions[i], v
		}
	}
	if best == nil {
		return
	}
	if _, why := pickVariant(best, ""); why != "" {
		r.block(Blocker{Kind: KindUsage, Code: "variant.required", Component: name, Message: fmt.Sprintf("%s@%s %s", name, bv.String(), why)})
	}
}
