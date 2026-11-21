package resolver

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// 属性测试：随机依赖图 + 约束。
//   soundness   ：Resolve 给出可执行结果时，结果满足全部约束（requires/peer/conflicts，闭包恰为用户选择 ∪ 内部依赖），order 是拓扑序；
//   completeness：与独立的暴力枚举对照——存在解当且仅当 Resolve 给出解（环依赖单独以 cycle 阻断，不算无解）；
//   determinism ：同一输入多次求解、打乱选择顺序，JSON 完全一致。
// 暴力枚举器是另一套实现（只用 manifest.ParseConstraint 的 Check，不共用 solver 代码）。

var constraintPool = []string{">=1.0.0", ">=1.1.0", "<1.2.0", ">=1.0.0 <1.2.0", "^1.1.0", "=1.0.0", "~1.1.0", ">=1.0.0 <1.1.0 || >=1.2.0"}

type gcomp struct {
	name     string
	internal bool
	vers     []gver
}

type gver struct {
	v         string
	requires  map[string]string
	peer      map[string]string
	conflicts [][2]string
}

type graph struct {
	comps []gcomp
	wants []string
}

func genGraph(rng *rand.Rand) graph {
	n := 2 + rng.Intn(4)
	g := graph{}
	for i := 0; i < n; i++ {
		c := gcomp{name: fmt.Sprintf("c%d", i), internal: rng.Intn(10) < 3}
		nv := 1 + rng.Intn(3)
		for k := 0; k < nv; k++ {
			gv := gver{v: fmt.Sprintf("1.%d.0", k), requires: map[string]string{}, peer: map[string]string{}}
			for j := 0; j < n; j++ {
				if j == i {
					continue
				}
				switch r := rng.Intn(12); {
				case r < 3:
					gv.requires[fmt.Sprintf("c%d", j)] = constraintPool[rng.Intn(len(constraintPool))]
				case r == 3:
					gv.peer[fmt.Sprintf("c%d", j)] = constraintPool[rng.Intn(len(constraintPool))]
				case r == 4:
					cs := ""
					if rng.Intn(2) == 0 {
						cs = constraintPool[rng.Intn(len(constraintPool))]
					}
					gv.conflicts = append(gv.conflicts, [2]string{fmt.Sprintf("c%d", j), cs})
				}
			}
			c.vers = append(c.vers, gv)
		}
		g.comps = append(g.comps, c)
	}
	for _, c := range g.comps {
		if !c.internal && rng.Intn(2) == 0 {
			g.wants = append(g.wants, c.name)
		}
	}
	if len(g.wants) == 0 {
		g.comps[0].internal = false
		g.wants = []string{g.comps[0].name}
	}
	return g
}

func (g graph) input(t testing.TB) Input {
	var list []cs
	for _, c := range g.comps {
		x := cs{name: c.name, internal: c.internal}
		for _, v := range c.vers {
			x.vers = append(x.vers, vs{ver: v.v, comps: v.requires, peer: v.peer, conflicts: v.conflicts})
		}
		list = append(list, x)
	}
	return Input{Manifest: buildManifest(t, nil, list...), System: okSystem(), Want: want(g.wants...)}
}

func (g graph) comp(name string) gcomp {
	for _, c := range g.comps {
		if c.name == name {
			return c
		}
	}
	panic("no comp " + name)
}

func (g graph) ver(name, v string) gver {
	for _, x := range g.comp(name).vers {
		if x.v == v {
			return x
		}
	}
	panic("no ver")
}

func mustCons(s string) manifest.Constraint {
	c, err := manifest.ParseConstraint(s)
	if err != nil {
		panic(err)
	}
	return c
}

func mustVer(s string) manifest.Version {
	v, err := manifest.ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

// checkAssignment 独立地验证一个「组件→版本」赋值是否满足全部约束，并要求在场集合恰为用户选择经内部依赖闭包后的结果。
func (g graph) checkAssignment(assign map[string]string) error {
	closure := map[string]bool{}
	for _, w := range g.wants {
		closure[w] = true
	}
	for changed := true; changed; {
		changed = false
		for name, v := range assign {
			if !closure[name] {
				continue
			}
			for to := range g.ver(name, v).requires {
				if g.comp(to).internal && !closure[to] {
					closure[to] = true
					changed = true
				}
			}
		}
	}
	if len(closure) != len(assign) {
		return fmt.Errorf("在场集合 %v 不等于闭包 %v", keys(assign), keys(boolToStr(closure)))
	}
	for n := range closure {
		if _, ok := assign[n]; !ok {
			return fmt.Errorf("闭包成员 %s 没有版本", n)
		}
	}
	for name, v := range assign {
		gv := g.ver(name, v)
		for _, set := range []map[string]string{gv.requires, gv.peer} {
			for to, c := range set {
				if tv, ok := assign[to]; ok && !mustCons(c).Check(mustVer(tv)) {
					return fmt.Errorf("%s@%s 要求 %s %s，实际 %s", name, v, to, c, tv)
				}
			}
		}
		for _, cf := range gv.conflicts {
			if tv, ok := assign[cf[0]]; ok && (cf[1] == "" || mustCons(cf[1]).Check(mustVer(tv))) {
				return fmt.Errorf("%s@%s 与 %s@%s 冲突", name, v, cf[0], tv)
			}
		}
	}
	return nil
}

func boolToStr(m map[string]bool) map[string]string {
	o := map[string]string{}
	for k := range m {
		o[k] = ""
	}
	return o
}

func keys(m map[string]string) []string {
	var o []string
	for k := range m {
		o = append(o, k)
	}
	sort.Strings(o)
	return o
}

// bruteForce 枚举全部「缺席 | 某个版本」的赋值，返回是否存在合法解。
func (g graph) bruteForce() bool {
	var rec func(i int, assign map[string]string) bool
	rec = func(i int, assign map[string]string) bool {
		if i == len(g.comps) {
			return g.checkAssignment(assign) == nil
		}
		c := g.comps[i]
		if rec(i+1, assign) {
			return true
		}
		for _, v := range c.vers {
			assign[c.name] = v.v
			if rec(i+1, assign) {
				return true
			}
		}
		delete(assign, c.name)
		return false
	}
	return rec(0, map[string]string{})
}

func checkProperties(t *testing.T, seed int64) {
	t.Helper()
	g := genGraph(rand.New(rand.NewSource(seed)))
	in := g.input(t)
	p := Resolve(in)
	exists := g.bruteForce()
	cycle := p.hasBlocker("resolve.dependency_cycle") != nil
	switch {
	case len(p.Blockers) == 0 || cycle:
		if !exists {
			t.Fatalf("seed %d：Resolve 给出了解，但暴力枚举认为无解\n%s", seed, jsonOf(t, p))
		}
		assign := map[string]string{}
		for _, c := range p.Components {
			assign[c.Name] = c.Version
			if c.Action != "install" {
				t.Fatalf("seed %d：无已装组件，动作应为 install: %+v", seed, c)
			}
		}
		if err := g.checkAssignment(assign); err != nil {
			t.Fatalf("seed %d：结果不满足约束: %v\n%s", seed, err, jsonOf(t, p))
		}
		if !cycle {
			// order 是拓扑序：被依赖者（requires/peer，且在场）在前
			pos := map[string]int{}
			for i, n := range p.Order {
				pos[n] = i
			}
			if len(pos) != len(assign) {
				t.Fatalf("seed %d：order 不完整 %v", seed, p.Order)
			}
			for name, v := range assign {
				gv := g.ver(name, v)
				for _, set := range []map[string]string{gv.requires, gv.peer} {
					for to := range set {
						if _, ok := assign[to]; ok && pos[to] > pos[name] {
							t.Fatalf("seed %d：%s 依赖 %s，但 order 中靠后: %v", seed, name, to, p.Order)
						}
					}
				}
			}
		}
	default:
		if p.hasBlocker("resolve.unsatisfiable") == nil {
			t.Fatalf("seed %d：未预期的阻断 %+v", seed, p.Blockers)
		}
		if exists {
			t.Fatalf("seed %d：暴力枚举存在解，Resolve 却报无解\n%s", seed, jsonOf(t, p))
		}
		if len(p.Blockers[0].Chain) < 2 {
			t.Fatalf("seed %d：无解必须给出冲突链: %+v", seed, p.Blockers)
		}
	}
	// 确定性：重复求解 + 打乱选择顺序
	ref := jsonOf(t, p)
	rng := rand.New(rand.NewSource(seed + 1))
	for i := 0; i < 3; i++ {
		in2 := g.input(t)
		rng.Shuffle(len(in2.Want), func(a, b int) { in2.Want[a], in2.Want[b] = in2.Want[b], in2.Want[a] })
		if got := jsonOf(t, Resolve(in2)); got != ref {
			t.Fatalf("seed %d：不确定的输出\n%s\n%s", seed, got, ref)
		}
	}
}

func TestPropertyRandomGraphs(t *testing.T) {
	var sat, unsat int
	for seed := int64(1); seed <= 4000; seed++ {
		checkProperties(t, seed)
		g := genGraph(rand.New(rand.NewSource(seed)))
		if g.bruteForce() {
			sat++
		} else {
			unsat++
		}
	}
	// 生成器必须同时产生可解与无解的实例，否则属性测试是空转
	if sat < 500 || unsat < 200 {
		t.Fatalf("生成器分布失衡：可解 %d，无解 %d", sat, unsat)
	}
	t.Logf("可解 %d，无解 %d", sat, unsat)
}

// 原生 fuzz：字节流驱动同一个属性检查。
func FuzzResolveProperties(f *testing.F) {
	for _, s := range []int64{1, 2, 3, 42, 99, 1234} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, seed int64) { checkProperties(t, seed) })
}

// 求解结果里不应出现重复组件。
func TestNoDuplicateComponents(t *testing.T) {
	for seed := int64(1); seed <= 300; seed++ {
		g := genGraph(rand.New(rand.NewSource(seed)))
		p := Resolve(g.input(t))
		seen := map[string]bool{}
		for _, c := range p.Components {
			if seen[c.Name] {
				t.Fatalf("seed %d 重复 %s", seed, c.Name)
			}
			seen[c.Name] = true
		}
	}
}
