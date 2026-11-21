package resolver

import (
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// AC1/AC5：server + admin-web + website(static) 同机可解，合并成一个 Plan，Plan 全字段都有实质内容。
func TestMergedPlanServerAdminWebWebsite(t *testing.T) {
	m := stdManifest(t)
	p := plan(t, Input{Manifest: m, Want: []Want{{Component: "website", Variant: "static"}, {Component: "admin-web"}, {Component: "server"}}})
	mustOK(t, p)
	if got, want := versions(p), "admin-web@0.1.0(install),server@0.2.0(install),website@0.1.0(install)"; got != want {
		t.Fatalf("组件 = %s，期望 %s", got, want)
	}
	if !p.Executable() {
		t.Fatal("无阻断应可执行")
	}
	// order：依赖在前（admin-web 依赖 server）；其余按字典序稳定
	if strings.Join(p.Order, ",") != "server,admin-web,website" {
		t.Fatalf("order = %v", p.Order)
	}
	for i, c := range p.Components {
		if c.Name != p.Order[i] {
			t.Fatalf("components 应按 order 排列：%v vs %v", p.Components, p.Order)
		}
	}
	// ports：server 的 4 个（含条件端口 9444），website static 没有 3000
	var got []string
	for _, q := range p.Ports {
		got = append(got, q.Component+":"+q.Proto+"/"+itoa(q.Port)+"@"+q.Bind)
	}
	if strings.Join(got, " ") != "server:tcp/8080@127.0.0.1 server:tcp/9443@0.0.0.0 server:tcp/9444@0.0.0.0 server:tcp/9180@127.0.0.1" {
		t.Fatalf("ports = %v", got)
	}
	if !p.Ports[2].Conditional {
		t.Fatal("9444 应标为条件端口")
	}
	// units / files
	if len(p.Units) != 1 || p.Units[0].Name != "lingedge-server.service" || p.Units[0].Path != "/etc/systemd/system/lingedge-server.service" || p.Units[0].RunUser != "lingedge" {
		t.Fatalf("units = %+v", p.Units)
	}
	paths := map[string]File{}
	for _, f := range p.Files {
		paths[f.Path] = f
	}
	for path, pol := range map[string]string{
		"/etc/lingedge/server/master.key":     PolicyCreateIfAbsent,
		"/etc/lingedge/nginx/admin-web.conf":  PolicyManaged,
		"/etc/lingedge/nginx/website.conf":    PolicyManaged,
		"/opt/lingedge/server/releases/0.2.0": PolicyManaged,
		"/opt/lingedge/server/current":        PolicyManaged,
		"/var/lib/lingedge/server":            PolicyCreateIfAbsent,
	} {
		if f, ok := paths[path]; !ok || f.Policy != pol {
			t.Errorf("files 缺少 %s（policy %s）：%+v", path, pol, f)
		}
	}
	if f := paths["/etc/lingedge/server/master.key"]; f.Mode != "0400" || f.Owner != "root:root" {
		t.Errorf("master.key 权限 = %+v", f)
	}
	if _, ok := paths["/etc/lingedge/website/revalidate.secret"]; ok {
		t.Error("static 变体不应有 revalidate.secret")
	}
	// steps：seq 连续，依赖在前
	if len(p.Steps) == 0 {
		t.Fatal("steps 为空")
	}
	for i, s := range p.Steps {
		if s.Seq != i+1 {
			t.Fatalf("steps seq 不连续: %+v", p.Steps)
		}
	}
	if p.Steps[0].Component != "server" || p.Steps[0].Kind != "download" {
		t.Fatalf("首步应是 server 下载: %+v", p.Steps[0])
	}
	// 资源：内存 1024；磁盘 2048+100（min_disk）+30（3 个包各 10 MiB 的 staging）
	if p.Resources.MemoryNeededMB != 1024 || len(p.Resources.Disks) != 1 || p.Resources.Disks[0].NeededMB != 2178 {
		t.Fatalf("resources = %+v", p.Resources)
	}
	if c := p.comp("server"); c.URL == "" || c.SHA256 == "" || c.Size == 0 || c.Platform != "any" || c.RunUser != "lingedge" {
		t.Fatalf("server 组件信息 = %+v", c)
	}
	// 外部依赖都探测到 → 没有 remote 提示
	if p.hasWarning("external.remote_assumed") {
		t.Fatal("不应有 external.remote_assumed")
	}
}

func TestWebsiteSSRPortsFilesAndRuntime(t *testing.T) {
	m := stdManifest(t)
	p := plan(t, Input{Manifest: m, Want: []Want{{Component: "website", Variant: "ssr"}}})
	mustOK(t, p)
	if len(p.Ports) != 1 || p.Ports[0].Port != 3000 || p.Ports[0].Bind != "127.0.0.1" {
		t.Fatalf("ssr 应占用 3000/127.0.0.1: %+v", p.Ports)
	}
	if len(p.Units) != 1 || p.Units[0].Name != "lingedge-website.service" {
		t.Fatalf("units = %+v", p.Units)
	}
	var secret bool
	for _, f := range p.Files {
		if f.Path == "/etc/lingedge/website/revalidate.secret" && f.Mode == "0440" {
			secret = true
		}
	}
	if !secret {
		t.Fatal("ssr 应有 revalidate.secret")
	}
	// Node.js 版本范围（探测结果作为输入）：18 不满足 >=20 <23；23 也不满足；缺失 → 失败（§10.3 步骤 5：对 website ssr 则失败）
	for name, dep := range map[string]Dep{"18": {Detected: true, Version: "v18.19.0"}, "23": {Detected: true, Version: "23.0.0"}, "missing": {}} {
		sys := okSystem()
		sys.Deps["nodejs"] = dep
		q := plan(t, Input{Manifest: m, System: sys, Want: []Want{{Component: "website", Variant: "ssr"}}})
		if b := q.hasBlocker("resolve.unsatisfiable"); b == nil || !strings.Contains(strings.Join(b.Chain, "\n"), "runtime node >=20 <23") {
			t.Fatalf("node %s 应无解并说明原因: %+v", name, q.Blockers)
		}
		if exitOf(t, q) != 3 {
			t.Fatalf("退出码应为 3")
		}
	}
	// static 不需要 node
	sys := okSystem()
	sys.Deps = nil
	mustOK(t, plan(t, Input{Manifest: m, System: sys, Want: []Want{{Component: "website", Variant: "static"}}}))
}

func TestVariantRequiredAndUnknown(t *testing.T) {
	m := stdManifest(t)
	p := plan(t, Input{Manifest: m, Want: want("website")})
	if b := p.hasBlocker("variant.required"); b == nil || !strings.Contains(b.Message, "ssr|static") {
		t.Fatalf("未指定变体应提示: %+v", p.Blockers)
	}
	p = plan(t, Input{Manifest: m, Want: []Want{{Component: "website", Variant: "xx"}}})
	if p.hasBlocker("variant.not_found") == nil {
		t.Fatalf("%+v", p.Blockers)
	}
	p = plan(t, Input{Manifest: m, Want: []Want{{Component: "website", Variant: "xx", Version: "0.1.0"}}})
	if p.hasBlocker("variant.not_found") == nil {
		t.Fatalf("%+v", p.Blockers)
	}
	p = plan(t, Input{Manifest: m, Want: want("website", "website")})
	if p.hasBlocker("variant.required") == nil || p.hasBlocker("want.conflict") != nil {
		t.Fatalf("完全相同的重复选择应合并：%+v", p.Blockers)
	}
	p = plan(t, Input{Manifest: m, Want: []Want{{Component: "server"}, {Component: "server", Version: "0.1.0"}}})
	if p.hasBlocker("want.conflict") == nil {
		t.Fatalf("不一致的重复选择应拒绝：%+v", p.Blockers)
	}
}

// AC2：BUILTIN（8）与 NOT_RELEASED（7）。
func TestBuiltinAndNotReleased(t *testing.T) {
	m := stdManifest(t)
	for _, c := range []struct {
		name  string
		want  []string
		kind  Kind
		exit  int
		code  string
		other string
	}{
		{"scheduler（清单 builtin）", []string{"scheduler"}, KindBuiltin, 8, "component.builtin", ""},
		{"worker（清单里没有也是 builtin）", []string{"worker"}, KindBuiltin, 8, "component.builtin", ""},
		{"dns unreleased", []string{"dns"}, KindNotReleased, 7, "component.not_released", ""},
		{"server+dns：不伪造部分成功", []string{"server", "dns"}, KindNotReleased, 7, "component.not_released", ""},
		{"dns+scheduler 并存时 7 优先", []string{"scheduler", "dns"}, KindNotReleased, 7, "component.not_released", "component.builtin"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := plan(t, Input{Manifest: m, Want: want(c.want...)})
			if p.Executable() || p.Err() == nil {
				t.Fatal("必须有阻断")
			}
			if len(p.Components) != 0 || len(p.Steps) != 0 {
				t.Fatalf("有阻断时不得给出可执行内容: %+v", p)
			}
			e := p.Err().(*Error)
			if e.Kind != c.kind || p.hasBlocker(c.code) == nil {
				t.Fatalf("kind=%s blockers=%+v", e.Kind, p.Blockers)
			}
			if c.other != "" && p.hasBlocker(c.other) == nil {
				t.Fatalf("两个阻断都应展示: %+v", p.Blockers)
			}
		})
	}
	if e := plan(t, Input{Manifest: m, Want: want("scheduler")}).Err().(*Error); !strings.Contains(e.Msg, "内置于 server") {
		t.Fatal(e.Msg)
	}
}

func TestUsageErrors(t *testing.T) {
	m := stdManifest(t)
	for name, c := range map[string]struct {
		in   Input
		code string
	}{
		"空选择":      {Input{Manifest: m}, "want.empty"},
		"未知组件":     {Input{Manifest: m, Want: want("nosuch")}, "component.unknown"},
		"非法组件名":    {Input{Manifest: m, Want: want("Bad Name")}, "component.invalid"},
		"内部组件不可直选": {Input{Manifest: m, Want: want("openresty")}, "component.internal"},
		"固定版本不存在":  {Input{Manifest: m, Want: []Want{{Component: "server", Version: "9.9.9"}}}, "version.not_found"},
		"固定版本非法":   {Input{Manifest: m, Want: []Want{{Component: "server", Version: "v1"}}}, "version.invalid"},
		"没有清单":     {Input{Want: want("server")}, "manifest.missing"},
	} {
		p := plan(t, c.in)
		if p.hasBlocker(c.code) == nil || exitOf(t, p) != 2 {
			t.Errorf("%s: %+v exit=%d", name, p.Blockers, exitOf(t, p))
		}
	}
	for name, sys := range map[string]System{
		"非 linux": {OS: "darwin", Arch: "arm64"},
		"未知架构":    {OS: "linux", Arch: "riscv64"},
	} {
		p := plan(t, Input{Manifest: m, Want: want("server"), System: sys})
		code := map[string]string{"非 linux": "system.os_unsupported", "未知架构": "system.arch_unsupported"}[name]
		if exitOf(t, p) != 3 || p.Executable() || p.hasBlocker(code) == nil || len(p.Blockers) != 1 {
			t.Errorf("%s: %+v", name, p.Blockers)
		}
	}
}

// AC3：内部依赖闭包（node → openresty）。
func TestInternalClosureNodeOpenresty(t *testing.T) {
	m := stdManifest(t)
	p := plan(t, Input{Manifest: m, Want: want("node")})
	mustOK(t, p)
	if versions(p) != "node@0.2.0(install),openresty@1.27.1(install)" {
		t.Fatalf("versions = %s", versions(p))
	}
	if strings.Join(p.Order, ",") != "openresty,node" {
		t.Fatalf("openresty 必须先于 node: %v", p.Order)
	}
	oc := p.comp("openresty")
	if !oc.Internal || oc.RequiredBy != "node" || oc.Wanted {
		t.Fatalf("openresty = %+v", oc)
	}
	names := []string{}
	for _, u := range p.Units {
		names = append(names, u.Name)
	}
	if strings.Join(names, ",") != "lingedge-node.service,lingedge-openresty.path,lingedge-openresty.service" {
		t.Fatalf("units = %v", names)
	}
	// 清单里缺少被依赖的内部组件 → 无解（而不是装 node 而没有 openresty）
	m2 := buildManifest(t, nil, cs{name: "node", vers: []vs{{ver: "0.1.0", comps: map[string]string{"openresty": ">=1 <2"}}}})
	q := plan(t, Input{Manifest: m2, Want: want("node")})
	if b := q.hasBlocker("resolve.unsatisfiable"); b == nil || !strings.Contains(strings.Join(b.Chain, "\n"), "不在清单中") {
		t.Fatalf("%+v", q.Blockers)
	}
}

// AC3：约束回溯。openresty 清单里只有 1.25.x 时，node 0.2.0（要求 >=1.27）不可行，回溯到 node 0.1.0。
func TestBacktrackToOlderVersion(t *testing.T) {
	m := buildManifest(t, nil,
		cs{name: "node", vers: []vs{
			{ver: "0.1.0", comps: map[string]string{"openresty": ">=1.25 <1.26"}},
			{ver: "0.2.0", comps: map[string]string{"openresty": ">=1.27 <2"}}}},
		cs{name: "openresty", internal: true, vers: []vs{{ver: "1.25.3"}}})
	p := plan(t, Input{Manifest: m, Want: want("node")})
	mustOK(t, p)
	if versions(p) != "node@0.1.0(install),openresty@1.25.3(install)" {
		t.Fatalf("versions = %s", versions(p))
	}
}

// peer 约束：本机同时安装时强制并传播（node 的 peer server <0.2 迫使 server 取 0.1.0）；本机没有则只在 enroll 时比对并警告。
func TestPeerConstraintPropagation(t *testing.T) {
	m := buildManifest(t, nil,
		cs{name: "node", vers: []vs{{ver: "0.1.0", peer: map[string]string{"server": ">=0.1 <0.2"}}}},
		cs{name: "server", vers: []vs{{ver: "0.1.0"}, {ver: "0.2.0"}}})
	p := plan(t, Input{Manifest: m, Want: want("node", "server")})
	mustOK(t, p)
	if versions(p) != "node@0.1.0(install),server@0.1.0(install)" {
		t.Fatalf("versions = %s", versions(p))
	}
	p = plan(t, Input{Manifest: m, Want: want("node")})
	mustOK(t, p)
	if !p.hasWarning("peer.compare_at_enroll") {
		t.Fatalf("应提示 enroll 时比对: %+v", p.Warnings)
	}
	// 固定 server 0.2.0 而 node 只接受 <0.2 → 无解（退出码 3），冲突链点名双方
	p = plan(t, Input{Manifest: m, Want: []Want{{Component: "node"}, {Component: "server", Version: "0.2.0"}}})
	b := p.hasBlocker("resolve.unsatisfiable")
	if b == nil || exitOf(t, p) != 3 {
		t.Fatalf("%+v", p.Blockers)
	}
	txt := strings.Join(b.Chain, "\n")
	for _, s := range []string{"用户选择", "node@0.1.0", "peer", "server", "0.2.0"} {
		if !strings.Contains(txt, s) {
			t.Errorf("冲突链缺少 %q:\n%s", s, txt)
		}
	}
}

// requires.components 指向本机没有、也不会一并安装的非内部组件：按远端部署处理（警告而非阻断）。
func TestRequiresComponentsRemoteAssumed(t *testing.T) {
	p := plan(t, Input{Manifest: stdManifest(t), Want: want("admin-web")})
	mustOK(t, p)
	if !p.hasWarning("dependency.remote_assumed") {
		t.Fatalf("%+v", p.Warnings)
	}
	// 已装 server 满足约束 → 保持不动；已装版本不满足且无法升级 → 无解
	p = plan(t, Input{Manifest: stdManifest(t), Want: want("admin-web"), Installed: inst("server@0.1.0")})
	mustOK(t, p)
	if p.comp("server") != nil || p.hasWarning("dependency.remote_assumed") {
		t.Fatalf("已装 server 满足约束，不应进入 Plan/警告: %+v", p)
	}
	m := buildManifest(t, nil, fronts("admin-web", "0.1.0"), cs{name: "server", vers: []vs{{ver: "0.1.0"}, {ver: "2.0.0"}}})
	q := plan(t, Input{Manifest: m, Want: want("admin-web"), Installed: inst("server@2.0.0")}) // 约束 <1.0.0，而已装 2.0.0 不能降级
	if q.hasBlocker("resolve.unsatisfiable") == nil || exitOf(t, q) != 3 {
		t.Fatalf("%+v", q.Blockers)
	}
}

// AC3：无解 → 退出码 3 并列出冲突链（外部依赖版本不满足也是无解的一种）。
func TestUnsatisfiableChains(t *testing.T) {
	m := buildManifest(t, nil,
		cs{name: "node", vers: []vs{{ver: "0.1.0", comps: map[string]string{"openresty": ">=2 <3"}}}},
		cs{name: "openresty", internal: true, vers: []vs{{ver: "1.25.3"}, {ver: "1.27.1"}}})
	p := plan(t, Input{Manifest: m, Want: want("node")})
	b := p.hasBlocker("resolve.unsatisfiable")
	if b == nil || exitOf(t, p) != 3 {
		t.Fatalf("%+v", p.Blockers)
	}
	txt := strings.Join(b.Chain, "\n")
	for _, s := range []string{"node（用户选择）", "→ openresty（被 node@0.1.0 依赖）", "✗ openresty", "openresty >=2 <3", "1.27.1"} {
		if !strings.Contains(txt, s) {
			t.Errorf("冲突链缺少 %q:\n%s", s, txt)
		}
	}
	// mysql 7.x 不满足 server 的 >=8.0.16 <9：每个候选版本的拒绝原因都在链里
	sys := okSystem()
	sys.Deps["mysql"] = Dep{Detected: true, Version: "7.0.1"}
	p = plan(t, Input{Manifest: stdManifest(t), System: sys, Want: want("server")})
	b = p.hasBlocker("resolve.unsatisfiable")
	if b == nil || !strings.Contains(strings.Join(b.Chain, "\n"), "external mysql >=8.0.16 <9，本机探测到 7.0.1") {
		t.Fatalf("%+v", p.Blockers)
	}
	// 外部依赖未探测到：server 允许继续（向导里配置远端实例），并给提示
	sys = okSystem()
	delete(sys.Deps, "mysql")
	sys.Deps["redis"] = Dep{}
	p = plan(t, Input{Manifest: stdManifest(t), System: sys, Want: want("server")})
	mustOK(t, p)
	if !p.hasWarning("external.remote_assumed") {
		t.Fatalf("%+v", p.Warnings)
	}
	// 版本串无法解析 → 不放行
	sys = okSystem()
	sys.Deps["mysql"] = Dep{Detected: true, Version: "unknown"}
	p = plan(t, Input{Manifest: stdManifest(t), System: sys, Want: want("server")})
	if p.Executable() {
		t.Fatal("无法解析的外部依赖版本不得放行")
	}
	// 回溯到更老的 server 版本：0.2.0 要求 redis >=9，探测到 8 → 选 0.1.0
	m = buildManifest(t, nil, cs{name: "server", vers: []vs{{ver: "0.1.0", external: map[string]string{"redis": ">=8"}}, {ver: "0.2.0", external: map[string]string{"redis": ">=9"}}}})
	p = plan(t, Input{Manifest: m, Want: want("server")})
	mustOK(t, p)
	if p.comp("server").Version != "0.1.0" {
		t.Fatalf("应回退到 0.1.0: %s", versions(p))
	}
}

func TestExternalVersionNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"8.0.36-0ubuntu0.22.04.1": "8.0.36", "v20.11.0": "20.11.0", "8": "8.0.0", "8.0": "8.0.0", "10.11.6-MariaDB": "10.11.6",
		"8.0.036": "8.0.36", "7.2.4 (jemalloc)": "7.2.4",
	} {
		v, ok := normalizeDepVersion(in)
		if !ok || v.String() != want {
			t.Errorf("%q → %v %v，期望 %s", in, v, ok, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1"} {
		if _, ok := normalizeDepVersion(bad); ok {
			t.Errorf("%q 不应解析成功", bad)
		}
	}
}

func TestConflictsDeclaration(t *testing.T) {
	m := buildManifest(t, nil,
		cs{name: "a", vers: []vs{{ver: "1.0.0", conflicts: [][2]string{{"b", ">=2.0.0"}}}}},
		cs{name: "b", vers: []vs{{ver: "1.0.0"}, {ver: "2.0.0"}}})
	p := plan(t, Input{Manifest: m, Want: want("a", "b")})
	mustOK(t, p)
	if p.comp("b").Version != "1.0.0" {
		t.Fatalf("a 与 b>=2 冲突，b 应取 1.0.0: %s", versions(p))
	}
	m = buildManifest(t, nil,
		cs{name: "a", vers: []vs{{ver: "1.0.0", conflicts: [][2]string{{"b", ""}}}}},
		cs{name: "b", vers: []vs{{ver: "1.0.0"}}})
	p = plan(t, Input{Manifest: m, Want: want("a", "b")})
	if b := p.hasBlocker("resolve.unsatisfiable"); b == nil || !strings.Contains(strings.Join(b.Chain, "\n"), "冲突") {
		t.Fatalf("%+v", p.Blockers)
	}
	// 与已装组件冲突
	p = plan(t, Input{Manifest: m, Want: want("a"), Installed: inst("b@1.0.0")})
	if p.hasBlocker("resolve.unsatisfiable") == nil {
		t.Fatalf("%+v", p.Blockers)
	}
}

// AC3：min_upgrade_from。
func TestMinUpgradeFrom(t *testing.T) {
	m := buildManifest(t, nil, cs{name: "server", vers: []vs{{ver: "0.1.0"}, {ver: "0.2.0", min: "0.1.0"}, {ver: "0.3.0", min: "0.2.0"}, {ver: "0.4.0", min: "0.3.0"}}})
	// 不固定版本：选可直接到达的最高版本，并提示逐级升级路径
	p := plan(t, Input{Manifest: m, Want: want("server"), Installed: inst("server@0.1.0")})
	mustOK(t, p)
	c := p.comp("server")
	if c.Version != "0.2.0" || c.Action != "upgrade" || c.From != "0.1.0" || c.PassiveUpgrade {
		t.Fatalf("%+v", c)
	}
	var w *Warning
	for i := range p.Warnings {
		if p.Warnings[i].Code == "upgrade.stepwise" {
			w = &p.Warnings[i]
		}
	}
	if w == nil || !strings.Contains(w.Message, "0.2.0 → 0.3.0 → 0.4.0") {
		t.Fatalf("%+v", p.Warnings)
	}
	// 固定到不可直达的版本 → 退出码 3，带路径
	p = plan(t, Input{Manifest: m, Want: []Want{{Component: "server", Version: "0.4.0"}}, Installed: inst("server@0.1.0")})
	b := p.hasBlocker("upgrade.min_upgrade_from")
	if b == nil || exitOf(t, p) != 3 || !strings.Contains(b.Hint, "0.2.0 → 0.3.0 → 0.4.0") {
		t.Fatalf("%+v", p.Blockers)
	}
	// 起点够新 → 直接升
	p = plan(t, Input{Manifest: m, Want: []Want{{Component: "server", Version: "0.4.0"}}, Installed: inst("server@0.3.0")})
	mustOK(t, p)
	if p.hasWarning("upgrade.stepwise") {
		t.Fatal("可直达不应提示逐级升级")
	}
	// 降级不支持
	p = plan(t, Input{Manifest: m, Want: []Want{{Component: "server", Version: "0.1.0"}}, Installed: inst("server@0.3.0")})
	if p.hasBlocker("upgrade.downgrade") == nil || exitOf(t, p) != 3 {
		t.Fatalf("%+v", p.Blockers)
	}
	// 全新安装不受 min_upgrade_from 限制
	p = plan(t, Input{Manifest: m, Want: want("server")})
	mustOK(t, p)
	if p.comp("server").Version != "0.4.0" {
		t.Fatal(versions(p))
	}
}

// 已装且已是目标版本：keep，无步骤。
func TestAlreadyInstalledIsKeep(t *testing.T) {
	p := plan(t, Input{Manifest: stdManifest(t), Want: want("server"), Installed: inst("server@0.2.0")})
	mustOK(t, p)
	if c := p.comp("server"); c.Action != "keep" || len(p.Steps) != 0 || len(p.Units) != 0 || !p.hasWarning("already.installed") {
		t.Fatalf("%+v %+v", c, p)
	}
	if p.Resources.MemoryNeededMB != 0 {
		t.Fatal("keep 不计资源")
	}
}

// AC3：revoked_versions 命中不可作目标。
func TestRevokedVersions(t *testing.T) {
	m := buildManifest(t, [][2]string{{"server", "0.2.0"}}, cs{name: "server", vers: []vs{serverVer("0.1.0"), serverVer("0.2.0")}})
	// 固定吊销版本 → 退出码 5（与 manifest.CheckTarget 一致）
	p := plan(t, Input{Manifest: m, Want: []Want{{Component: "server", Version: "0.2.0"}}})
	if p.hasBlocker("version.revoked") == nil || exitOf(t, p) != 5 {
		t.Fatalf("%+v", p.Blockers)
	}
	// 不固定：跳过吊销版本，取次高
	p = plan(t, Input{Manifest: m, Want: want("server")})
	mustOK(t, p)
	if p.comp("server").Version != "0.1.0" {
		t.Fatal(versions(p))
	}
	// 已装的版本被吊销：可保持但警告；升级不得以它为目标
	p = plan(t, Input{Manifest: m, Want: want("server"), Installed: inst("server@0.2.0")})
	mustOK(t, p)
	if !p.hasWarning("installed.revoked") || p.comp("server").Action != "keep" {
		t.Fatalf("%+v", p)
	}
	// 被依赖的内部组件的最高版本被吊销：回退到次高
	m = buildManifest(t, [][2]string{{"openresty", "1.27.1"}},
		cs{name: "node", vers: []vs{{ver: "0.1.0", comps: map[string]string{"openresty": ">=1 <2"}}}},
		cs{name: "openresty", internal: true, vers: []vs{{ver: "1.25.3"}, {ver: "1.27.1"}}})
	p = plan(t, Input{Manifest: m, Want: want("node")})
	mustOK(t, p)
	if p.comp("openresty").Version != "1.25.3" {
		t.Fatal(versions(p))
	}
	// 全部版本被吊销 → 无解
	m = buildManifest(t, [][2]string{{"server", "0.1.0"}}, cs{name: "server", vers: []vs{serverVer("0.1.0")}})
	p = plan(t, Input{Manifest: m, Want: want("server")})
	if b := p.hasBlocker("resolve.unsatisfiable"); b == nil || !strings.Contains(strings.Join(b.Chain, "\n"), "已被发行方吊销") {
		t.Fatalf("%+v", p.Blockers)
	}
}

// 预发布版本默认不入选，固定版本才可。
func TestPrereleaseOnlyWhenPinned(t *testing.T) {
	m := buildManifest(t, nil, cs{name: "server", vers: []vs{{ver: "0.1.0"}, {ver: "0.2.0-rc.1"}}})
	p := plan(t, Input{Manifest: m, Want: want("server")})
	mustOK(t, p)
	if p.comp("server").Version != "0.1.0" {
		t.Fatal(versions(p))
	}
	p = plan(t, Input{Manifest: m, Want: []Want{{Component: "server", Version: "0.2.0-rc.1"}}})
	mustOK(t, p)
	if p.comp("server").Version != "0.2.0-rc.1" {
		t.Fatal(versions(p))
	}
}

// AC3：已装组件被动升级需标记并确认。
func TestPassiveUpgradeFlagAndConfirm(t *testing.T) {
	m := buildManifest(t, nil,
		cs{name: "server", vers: []vs{{ver: "0.1.0"}, {ver: "0.2.0"}}},
		cs{name: "website", vers: []vs{
			{ver: "0.1.0", comps: map[string]string{"server": ">=0.1.0 <0.2.0"}, variants: []string{"static", "ssr"}},
			{ver: "0.2.0", comps: map[string]string{"server": ">=0.2.0 <0.3.0"}, variants: []string{"static", "ssr"}}}})
	in := Input{Manifest: m, Want: []Want{{Component: "server", Version: "0.2.0"}}, Installed: inst("server@0.1.0", "website@0.1.0"), InstalledVariants: map[string]string{"website": "static"}}
	p := plan(t, in)
	w := p.comp("website")
	if w == nil || !w.PassiveUpgrade || w.Action != "upgrade" || w.From != "0.1.0" || w.Version != "0.2.0" || w.Wanted || w.Variant != "static" {
		t.Fatalf("website = %+v", w)
	}
	if s := p.comp("server"); s.PassiveUpgrade || s.Action != "upgrade" || !s.Wanted {
		t.Fatalf("server = %+v", s)
	}
	b := p.hasBlocker("confirm.passive_upgrade")
	if b == nil || exitOf(t, p) != 9 || !strings.Contains(b.Message, "website 0.1.0 → 0.2.0") {
		t.Fatalf("未确认应以 9 阻断: %+v", p.Blockers)
	}
	if strings.Join(p.Order, ",") != "server,website" {
		t.Fatalf("server 先于依赖它的 website: %v", p.Order)
	}
	// 确认后放行并记警告
	in.AcceptPassiveUpgrade = true
	p = plan(t, in)
	mustOK(t, p)
	if !p.hasWarning("passive.upgrade_accepted") {
		t.Fatal("应记录已确认的被动升级")
	}
	// 变体未知且有多个变体（无 default）：被动升级候选被拒，而不是猜一个
	in.InstalledVariants = nil
	p = plan(t, in)
	if b := p.hasBlocker("resolve.unsatisfiable"); b == nil || !strings.Contains(strings.Join(b.Chain, "\n"), "请显式选择该组件及变体") {
		t.Fatalf("%+v", p.Blockers)
	}
	// 用户要升级 server（不固定版本）而已装 website 0.1.0 要求 server <0.2：取最高的 server 0.2.0，并把 website 标为被动升级（需确认）
	in2 := Input{Manifest: m, Want: want("server"), Installed: inst("server@0.1.0", "website@0.1.0"), InstalledVariants: map[string]string{"website": "static"}}
	p = plan(t, in2)
	if c := p.comp("website"); c == nil || !c.PassiveUpgrade || p.comp("server").Version != "0.2.0" || p.hasBlocker("confirm.passive_upgrade") == nil {
		t.Fatalf("%s %+v", versions(p), p.Blockers)
	}
	// 反向约束：website 没有更新的版本可升时，已装 website 0.1.0 的约束不能被破坏 → server 保持 0.1.0
	m2 := buildManifest(t, nil,
		cs{name: "server", vers: []vs{{ver: "0.1.0"}, {ver: "0.2.0"}}},
		cs{name: "website", vers: []vs{{ver: "0.1.0", comps: map[string]string{"server": ">=0.1.0 <0.2.0"}, variants: []string{"static", "ssr"}}}})
	p = plan(t, Input{Manifest: m2, Want: want("server"), Installed: inst("server@0.1.0", "website@0.1.0"), InstalledVariants: map[string]string{"website": "static"}})
	mustOK(t, p)
	if c := p.comp("server"); c.Action != "keep" || c.Version != "0.1.0" || p.comp("website") != nil {
		t.Fatalf("%s", versions(p))
	}
}

// 解析不修改输入（纯函数）。
func TestResolveDoesNotMutateInput(t *testing.T) {
	m := stdManifest(t)
	in := Input{Manifest: m, Want: []Want{{Component: "website", Variant: "static"}, {Component: "node"}, {Component: "server"}}, Installed: inst("openresty@1.25.3"),
		System: okSystem(), PortOverrides: []PortOverride{{Component: "server", Proto: "tcp", Port: 8080, To: 18080}}}
	before := jsonOf(t, struct {
		W  []Want
		I  any
		S  System
		O  []PortOverride
		Mf []byte
	}{in.Want, in.Installed, in.System, in.PortOverrides, manifestJSON(nil)})
	mj := jsonOf(t, m.Components)
	Resolve(in)
	after := jsonOf(t, struct {
		W  []Want
		I  any
		S  System
		O  []PortOverride
		Mf []byte
	}{in.Want, in.Installed, in.System, in.PortOverrides, manifestJSON(nil)})
	if before != after || mj != jsonOf(t, m.Components) {
		t.Fatal("Resolve 修改了输入")
	}
}

var _ = manifest.SchemaVersion
