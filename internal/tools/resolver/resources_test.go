package resolver

import (
	"strings"
	"testing"
)

func TestMountFor(t *testing.T) {
	disks := []Disk{{Mount: "/", FreeMB: 1}, {Mount: "/opt", FreeMB: 2}, {Mount: "/optional", FreeMB: 3}, {Mount: "/opt/lingedge", FreeMB: 4}, {Mount: "/var/", FreeMB: 5}}
	for path, want := range map[string]string{
		"/opt/lingedge/server": "/opt/lingedge", "/opt/lingedge": "/opt/lingedge", "/opt/other": "/opt", "/opt": "/opt", "/optx/y": "/",
		"/optional/a": "/optional", "/var/lib/x": "/var/", "/etc": "/", "/": "/",
	} {
		d, ok := mountFor(path, disks)
		if !ok || d.Mount != want {
			t.Errorf("mountFor(%s) = %q，期望 %q", path, d.Mount, want)
		}
	}
	if _, ok := mountFor("/x", []Disk{{Mount: "/opt"}}); ok {
		t.Error("没有覆盖的挂载点应返回 false")
	}
	// 输入顺序无关
	a, _ := mountFor("/opt/x", []Disk{{Mount: "/opt", FreeMB: 1}, {Mount: "/", FreeMB: 2}})
	b, _ := mountFor("/opt/x", []Disk{{Mount: "/", FreeMB: 2}, {Mount: "/opt", FreeMB: 1}})
	if a != b {
		t.Fatal("挂载点选择与顺序有关")
	}
}

// AC4：内存与磁盘合计需求；磁盘按目标挂载点分别统计。
func TestResourcesMemoryAndDiskPerMount(t *testing.T) {
	m := stdManifest(t)
	// 内存：server 1024 + node 256 = 1280，合计才超过 1200
	sys := okSystem()
	sys.MemoryAvailableMB = 1200
	p := plan(t, Input{Manifest: m, System: sys, Want: want("server")})
	mustOK(t, p)
	p = plan(t, Input{Manifest: m, System: sys, Want: want("server", "node")})
	b := p.hasBlocker("memory.insufficient")
	if b == nil || exitOf(t, p) != 3 || !strings.Contains(b.Message, "1280") || !strings.Contains(b.Message, "1200") {
		t.Fatalf("%+v", p.Blockers)
	}
	if len(p.Components) != 3 || p.Resources.MemoryNeededMB != 1280 {
		t.Fatalf("阻断时仍展示 Plan 与资源统计: %+v", p.Resources)
	}
	// 内存未探测到而需要内存 → 阻断（而不是放行）
	sys = okSystem()
	sys.MemoryAvailableMB = 0
	if plan(t, Input{Manifest: m, System: sys, Want: want("server")}).hasBlocker("memory.unknown") == nil {
		t.Fatal("未探测到内存必须阻断")
	}

	// 磁盘：/opt 与 /var 是不同的挂载点。server 的 2048 落在 /opt（release 目录），各包的 staging 落在 /var（10 MiB/个）
	sys = okSystem()
	sys.Disks = []Disk{{Mount: "/", FreeMB: 1 << 20}, {Mount: "/opt", FreeMB: 2100}, {Mount: "/var", FreeMB: 100}}
	mustOK(t, plan(t, Input{Manifest: m, System: sys, Want: want("server")}))
	p = plan(t, Input{Manifest: m, System: sys, Want: want("server", "admin-web")}) // /opt：2048+100=2148 > 2100，单个都装得下
	b = p.hasBlocker("disk.insufficient")
	if b == nil || b.Details["mount"] != "/opt" || b.Details["needed_mb"] != int64(2148) || b.Details["free_mb"] != int64(2100) {
		t.Fatalf("应只在 /opt 上不足（合计）: %+v", p.Blockers)
	}
	n := 0
	for _, x := range p.Blockers {
		if x.Code == "disk.insufficient" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("只有 /opt 不足: %+v", p.Blockers)
	}
	var got []string
	for _, d := range p.Resources.Disks {
		got = append(got, d.Mount)
	}
	if strings.Join(got, ",") != "/opt,/var" {
		t.Fatalf("resources.disks = %+v", p.Resources.Disks)
	}
	// staging 所在的 /var 不足（需要 20，余 5）；/opt 充足
	sys.Disks = []Disk{{Mount: "/", FreeMB: 1 << 20}, {Mount: "/opt", FreeMB: 99999}, {Mount: "/var", FreeMB: 5}}
	p = plan(t, Input{Manifest: m, System: sys, Want: want("server", "admin-web")})
	b = p.hasBlocker("disk.insufficient")
	if b == nil || b.Details["mount"] != "/var" || b.Details["needed_mb"] != int64(20) {
		t.Fatalf("%+v", p.Blockers)
	}
	// 同一个机器、全在 / 上：合计 = 所有需求之和
	sys.Disks = []Disk{{Mount: "/", FreeMB: 2177}}
	p = plan(t, Input{Manifest: m, System: sys, Want: want("server", "admin-web", "console")}) // 2048+100+100+30 = 2278
	if b = p.hasBlocker("disk.insufficient"); b == nil || b.Details["needed_mb"] != int64(2278) {
		t.Fatalf("%+v", p.Blockers)
	}
	// 恰好够用 → 通过（边界）
	sys.Disks = []Disk{{Mount: "/", FreeMB: 2278}}
	mustOK(t, plan(t, Input{Manifest: m, System: sys, Want: want("server", "admin-web", "console")}))
	// 只读挂载点 / 没有覆盖的挂载点
	sys.Disks = []Disk{{Mount: "/", FreeMB: 1 << 20, ReadOnly: true}}
	if plan(t, Input{Manifest: m, System: sys, Want: want("server")}).hasBlocker("disk.readonly") == nil {
		t.Fatal("只读挂载点必须阻断")
	}
	sys.Disks = []Disk{{Mount: "/data", FreeMB: 1 << 20}}
	if plan(t, Input{Manifest: m, System: sys, Want: want("server")}).hasBlocker("disk.mount_unknown") == nil {
		t.Fatal("未覆盖的路径必须阻断")
	}
	// 目录布局可配置：改装到 /data/lingedge，则 server 的 2048 记在 /data
	sys.Disks = []Disk{{Mount: "/", FreeMB: 1 << 20}, {Mount: "/data", FreeMB: 100}}
	p = plan(t, Input{Manifest: m, System: sys, Want: want("server"), Layout: Layout{OptRoot: "/data/lingedge"}})
	if b = p.hasBlocker("disk.insufficient"); b == nil || b.Details["mount"] != "/data" {
		t.Fatalf("%+v", p.Blockers)
	}
	var onData bool
	for _, f := range p.Files {
		if f.Path == "/data/lingedge/server/current" {
			onData = true
		}
	}
	if !onData {
		t.Fatalf("%+v", p.Files)
	}
}

// AC4：资源不足/冲突在预检阶段被阻止并给出原因；预检阶段 = Resolve 本身（纯函数，没有任何副作用发生）。
func TestBlockersPreventExecutionAndAreShownInDryRun(t *testing.T) {
	sys := okSystem()
	sys.MemoryAvailableMB = 10
	sys.Listeners = []Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: 8080, Process: "x"}}
	p := plan(t, Input{Manifest: stdManifest(t), System: sys, Want: want("server")})
	if p.Executable() || p.Err() == nil {
		t.Fatal("有阻断不得可执行")
	}
	e := p.Err().(*Error)
	if len(e.Blockers) != 2 || e.Plan != p {
		t.Fatalf("错误应携带全部阻断与 Plan: %+v", e)
	}
	if e.Kind != KindResource { // 优先级：资源(3) 高于 端口(4)
		t.Fatalf("kind = %s", e.Kind)
	}
	for _, b := range p.Blockers {
		if b.Message == "" || b.Code == "" {
			t.Fatalf("阻断必须有原因: %+v", b)
		}
	}
	// dry-run 展示：同一个 Plan 仍含完整的 components/steps/ports/units/files
	if len(p.Components) == 0 || len(p.Steps) == 0 || len(p.Ports) == 0 || len(p.Units) == 0 || len(p.Files) == 0 {
		t.Fatalf("阻断时 Plan 仍应完整展示: %+v", p)
	}
	if !strings.Contains(e.Error(), "共 2 项阻断") {
		t.Fatal(e.Error())
	}
	nilErr := (&Plan{}).Err()
	if nilErr != nil {
		t.Fatal("无阻断应返回 nil")
	}
}

func TestKindPriorityIsExhaustive(t *testing.T) {
	if len(kindPriority) != len(Kinds()) {
		t.Fatalf("kindPriority(%d) 与 Kinds()(%d) 不一致", len(kindPriority), len(Kinds()))
	}
	seen := map[Kind]bool{}
	for _, k := range kindPriority {
		seen[k] = true
	}
	for _, k := range Kinds() {
		if !seen[k] {
			t.Errorf("%s 不在优先级表中", k)
		}
		if _, ok := kindExit[k]; !ok {
			t.Errorf("测试表缺少 %s", k)
		}
	}
	// 每个优先级之间：高优先级赢
	for i := 0; i+1 < len(kindPriority); i++ {
		p := &Plan{Blockers: []Blocker{{Kind: kindPriority[i+1], Message: "b"}, {Kind: kindPriority[i], Message: "a"}}}
		if p.Err().(*Error).Kind != kindPriority[i] {
			t.Errorf("%s 应优先于 %s", kindPriority[i], kindPriority[i+1])
		}
	}
}

// 拓扑排序处理环：给出明确错误（环路径）。
func TestDependencyCycle(t *testing.T) {
	m := buildManifest(t, nil,
		cs{name: "alpha", vers: []vs{{ver: "1.0.0", comps: map[string]string{"beta": ">=1.0.0"}}}},
		cs{name: "beta", internal: true, vers: []vs{{ver: "1.0.0", comps: map[string]string{"gamma": ">=1.0.0"}}}},
		cs{name: "gamma", internal: true, vers: []vs{{ver: "1.0.0", comps: map[string]string{"alpha": ">=1.0.0"}}}})
	p := plan(t, Input{Manifest: m, Want: want("alpha")})
	b := p.hasBlocker("resolve.dependency_cycle")
	if b == nil || exitOf(t, p) != 3 {
		t.Fatalf("%+v", p.Blockers)
	}
	if strings.Join(b.Chain, ">") != "alpha>beta>gamma>alpha" || !strings.Contains(b.Message, "alpha → beta → gamma → alpha") {
		t.Fatalf("环路径 = %v", b.Chain)
	}
	// 不在环上的组件不受影响且顺序仍确定
	if len(p.Order) != 3 {
		t.Fatalf("order = %v", p.Order)
	}
	// 自依赖被忽略（不成环）
	m = buildManifest(t, nil, cs{name: "alpha", vers: []vs{{ver: "1.0.0", comps: map[string]string{"alpha": ">=1.0.0"}}}})
	mustOK(t, plan(t, Input{Manifest: m, Want: want("alpha")}))
}

// kahn / findCycle 直接测试：无环时依赖在前，同层字典序；环路径从字典序最小的环入口开始。
func TestKahnDirect(t *testing.T) {
	nodes := []string{"a", "b", "c", "d"}
	deps := map[string][]string{"a": {"c"}, "b": {"c"}, "c": {"d"}}
	indeg := map[string]int{"a": 1, "b": 1, "c": 1, "d": 0}
	rev := map[string][]string{"c": {"a", "b"}, "d": {"c"}}
	out, cyc := kahn(nodes, indeg, rev, deps)
	if cyc != nil || strings.Join(out, "") != "dcab" {
		t.Fatalf("%v %v", out, cyc)
	}
	deps = map[string][]string{"a": {"b"}, "b": {"a"}, "c": {"a"}}
	indeg = map[string]int{"a": 2, "b": 1, "c": 1}
	rev = map[string][]string{"a": {"b", "c"}, "b": {"a"}}
	out, cyc = kahn([]string{"a", "b", "c"}, indeg, rev, deps)
	if out != nil || strings.Join(cyc, ">") != "a>b>a" {
		t.Fatalf("%v %v", out, cyc)
	}
}
