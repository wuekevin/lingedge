package resolver

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
)

// 测试用清单经真实的 manifest.ParseManifest 生成（不绕过生产解析），而不是手工拼结构体。

type vs struct {
	ver        string
	min        string
	comps      map[string]string
	peer       map[string]string
	external   map[string]string
	runtime    map[string]string
	memMB      int64
	diskMB     int64
	osList     []string
	archList   []string
	conflicts  [][2]string // component, version constraint
	ports      []pt
	variants   []string                  // 默认 ["default"]
	variantReq map[string]map[string]any // 变体 → requires 片段
	size       int64
	platform   string // 默认 any
}

type pt struct {
	proto string
	port  int
	bind  string
	cond  bool
}

type cs struct {
	name      string
	status    string // 默认 available
	internal  bool
	builtinIn string
	vers      []vs
}

func buildManifest(t testing.TB, revoked [][2]string, comps ...cs) *manifest.Manifest {
	t.Helper()
	m, err := manifest.ParseManifest(manifestJSON(revoked, comps...))
	if err != nil {
		t.Fatalf("测试清单未通过生产解析: %v", err)
	}
	return m
}

func manifestJSON(revoked [][2]string, comps ...cs) []byte {
	rv := []any{}
	for _, r := range revoked {
		rv = append(rv, map[string]any{"component": r[0], "version": r[1], "reason": "test"})
	}
	cm := map[string]any{}
	for _, c := range comps {
		st := c.status
		if st == "" {
			st = "available"
		}
		o := map[string]any{"status": st}
		if c.internal {
			o["internal"] = true
		}
		switch st {
		case "available":
			vers := []any{}
			for _, v := range c.vers {
				vers = append(vers, versionObj(c.name, v))
			}
			o["versions"] = vers
		case "builtin":
			o["builtin_in"] = c.builtinIn
		}
		cm[c.name] = o
	}
	doc := map[string]any{"schema_version": 1, "channel": "stable", "sequence": 1, "generated_at": "2026-01-01T00:00:00Z",
		"valid_until": "2026-04-01T00:00:00Z", "revoked_versions": rv, "components": cm}
	b, _ := json.Marshal(doc)
	return b
}

func versionObj(name string, v vs) map[string]any {
	req := map[string]any{}
	if len(v.comps) > 0 {
		req["components"] = v.comps
	}
	if len(v.peer) > 0 {
		req["peer"] = v.peer
	}
	if len(v.external) > 0 {
		req["external"] = v.external
	}
	if len(v.runtime) > 0 {
		req["runtime"] = v.runtime
	}
	sys := map[string]any{}
	if v.memMB > 0 {
		sys["min_memory_mb"] = v.memMB
	}
	if v.diskMB > 0 {
		sys["min_disk_mb"] = v.diskMB
	}
	if len(v.osList) > 0 {
		sys["os"] = v.osList
	}
	if len(v.archList) > 0 {
		sys["arch"] = v.archList
	}
	if len(sys) > 0 {
		req["system"] = sys
	}
	o := map[string]any{"version": v.ver}
	if len(req) > 0 {
		o["requires"] = req
	}
	if v.min != "" {
		o["min_upgrade_from"] = v.min
	}
	if len(v.conflicts) > 0 {
		var cf []any
		for _, c := range v.conflicts {
			x := map[string]any{"component": c[0]}
			if c[1] != "" {
				x["version"] = c[1]
			}
			cf = append(cf, x)
		}
		o["conflicts"] = cf
	}
	if len(v.ports) > 0 {
		var ps []any
		for _, p := range v.ports {
			x := map[string]any{"proto": p.proto, "port": p.port}
			if p.bind != "" {
				x["bind"] = p.bind
			}
			if p.cond {
				x["conditional"] = true
			}
			ps = append(ps, x)
		}
		o["ports"] = ps
	}
	names := v.variants
	if len(names) == 0 {
		names = []string{"default"}
	}
	size := v.size
	if size == 0 {
		size = 10 << 20
	}
	plat := v.platform
	if plat == "" {
		plat = "any"
	}
	vars := map[string]any{}
	for _, n := range names {
		vo := map[string]any{"artifacts": map[string]any{plat: map[string]any{
			"url": "https://releases.example.invalid/" + name + "/" + v.ver + "/" + n + ".tar.gz", "sha256": strings.Repeat("a", 64), "size": size}}}
		if r, ok := v.variantReq[n]; ok {
			vo["requires"] = r
		}
		vars[n] = vo
	}
	o["variants"] = vars
	return o
}

func okSystem() System {
	return System{OS: "linux", Arch: "amd64", MemoryTotalMB: 16384, MemoryAvailableMB: 8192,
		Disks: []Disk{{Mount: "/", FreeMB: 500000}},
		Deps: map[string]Dep{
			"mysql":  {Detected: true, Version: "8.0.36-0ubuntu0.22.04.1"},
			"redis":  {Detected: true, Version: "8.0.1"},
			"nodejs": {Detected: true, Version: "v20.11.0"},
		}}
}

func serverVer(v string) vs {
	return vs{ver: v, external: map[string]string{"mysql": ">=8.0.16 <9", "redis": ">=8"}, memMB: 1024, diskMB: 2048,
		ports: []pt{{"tcp", 8080, "127.0.0.1", false}, {"tcp", 9443, "0.0.0.0", false}, {"tcp", 9444, "0.0.0.0", true}, {"tcp", 9180, "127.0.0.1", false}}}
}

func fronts(name string, vers ...string) cs {
	c := cs{name: name}
	for _, v := range vers {
		c.vers = append(c.vers, vs{ver: v, comps: map[string]string{"server": ">=0.1.0 <1.0.0"}, diskMB: 100})
	}
	return c
}

// stdManifest 是贴近 tools-design 示例的清单：server/admin-web/console/website(static|ssr)/node/openresty(内部)/dns(unreleased)/scheduler(builtin)。
func stdManifest(t testing.TB) *manifest.Manifest {
	return buildManifest(t, nil,
		cs{name: "server", vers: []vs{serverVer("0.1.0"), serverVer("0.2.0")}},
		fronts("admin-web", "0.1.0"), fronts("console", "0.1.0"),
		cs{name: "website", vers: []vs{{ver: "0.1.0", variants: []string{"static", "ssr"}, ports: []pt{{"tcp", 3000, "127.0.0.1", false}},
			variantReq: map[string]map[string]any{"ssr": {"runtime": map[string]string{"node": ">=20 <23"}}}}}},
		cs{name: "node", vers: []vs{
			{ver: "0.1.0", comps: map[string]string{"openresty": ">=1.25 <2"}, ports: []pt{{"tcp", 80, "0.0.0.0", false}, {"tcp", 443, "0.0.0.0", false}}, memMB: 256, diskMB: 200},
			{ver: "0.2.0", comps: map[string]string{"openresty": ">=1.25 <2"}, ports: []pt{{"tcp", 80, "0.0.0.0", false}, {"tcp", 443, "0.0.0.0", false}}, memMB: 256, diskMB: 200}}},
		cs{name: "openresty", internal: true, vers: []vs{{ver: "1.25.3"}, {ver: "1.27.1"}}},
		cs{name: "dns", status: "unreleased"},
		cs{name: "scheduler", status: "builtin", builtinIn: "server"},
	)
}

func want(names ...string) []Want {
	var w []Want
	for _, n := range names {
		w = append(w, Want{Component: n})
	}
	return w
}

func inst(items ...string) registry.State {
	st := registry.NewState()
	for _, it := range items {
		p := strings.SplitN(it, "@", 2)
		st.Components[p[0]] = registry.Component{Version: p[1], State: "running", Unit: "lingedge-" + p[0] + ".service"}
	}
	return st
}

func plan(t testing.TB, in Input) *Plan {
	t.Helper()
	if in.System.OS == "" {
		in.System = okSystem()
	}
	return Resolve(in)
}

func (p *Plan) comp(name string) *PlanComponent {
	for i := range p.Components {
		if p.Components[i].Name == name {
			return &p.Components[i]
		}
	}
	return nil
}

func (p *Plan) hasBlocker(code string) *Blocker {
	for i := range p.Blockers {
		if p.Blockers[i].Code == code {
			return &p.Blockers[i]
		}
	}
	return nil
}

func (p *Plan) hasWarning(code string) bool {
	for _, w := range p.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func mustOK(t testing.TB, p *Plan) {
	t.Helper()
	if len(p.Blockers) != 0 {
		b, _ := json.MarshalIndent(p.Blockers, "", " ")
		t.Fatalf("不应有阻断: %s", b)
	}
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
}

func versions(p *Plan) string {
	var out []string
	for _, c := range p.Components {
		out = append(out, fmt.Sprintf("%s@%s(%s)", c.Name, c.Version, c.Action))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func jsonOf(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func itoa(n int) string { return fmt.Sprint(n) }

// exitOf 取 Plan.Err() 的 Kind 对应的退出码（与 cli.ResolveError 的映射一致，cli 侧另有测试断言同一张表）。
func exitOf(t testing.TB, p *Plan) int {
	t.Helper()
	err := p.Err()
	if err == nil {
		return 0
	}
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("Err() 类型 = %T", err)
	}
	return kindExit[e.Kind]
}

var kindExit = map[Kind]int{KindUsage: 2, KindNotReleased: 7, KindBuiltin: 8, KindRevoked: 5, KindUnsatisfiable: 3, KindCycle: 3,
	KindUpgradePath: 3, KindSystem: 3, KindResource: 3, KindPort: 4, KindConfirm: 9}
