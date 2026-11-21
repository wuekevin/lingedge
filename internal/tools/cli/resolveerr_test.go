package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
)

// ResolveError 的映射必须覆盖 resolver 的每一种分类（新增分类而忘记映射会使本测试失败）。
func TestResolveErrorMappingIsExhaustive(t *testing.T) {
	want := map[resolver.Kind]struct {
		exit ExitCode
		code string
	}{
		resolver.KindUsage:         {ExitUsage, "usage.invalid_value"},
		resolver.KindNotReleased:   {ExitNotReleased, "component.not_released"},
		resolver.KindBuiltin:       {ExitBuiltin, "component.builtin"},
		resolver.KindRevoked:       {ExitVerifyFailed, "verify.version_revoked"},
		resolver.KindUnsatisfiable: {ExitPrecondition, "resolve.unsatisfiable"},
		resolver.KindCycle:         {ExitPrecondition, "resolve.dependency_cycle"},
		resolver.KindUpgradePath:   {ExitPrecondition, "resolve.upgrade_path"},
		resolver.KindSystem:        {ExitPrecondition, "precondition.system"},
		resolver.KindResource:      {ExitPrecondition, "precondition.resources"},
		resolver.KindPort:          {ExitPortConflict, "port.conflict"},
		resolver.KindConfirm:       {ExitConfirmRequired, "confirm.passive_upgrade"},
	}
	for _, k := range resolver.Kinds() {
		w, ok := want[k]
		if !ok {
			t.Fatalf("分类 %s 没有在本测试中声明预期映射", k)
		}
		p := &resolver.Plan{Blockers: []resolver.Blocker{{Kind: k, Code: "x", Message: "m", Hint: "h", Chain: []string{"a", "b"}}}}
		e := ResolveError(p.Err().(*resolver.Error))
		if e.Exit != w.exit || e.Code != w.code || e.Hint != "h" || e.Why != "a\nb" || e.Details["blockers"] == nil || e.Details["plan"] == nil {
			t.Errorf("%s -> %+v，期望 exit=%d code=%s", k, e, w.exit, w.code)
		}
	}
	if len(want) != len(resolver.Kinds()) {
		t.Fatalf("本测试声明了 %d 个分类，resolver.Kinds() 有 %d 个", len(want), len(resolver.Kinds()))
	}
	if e := ResolveError(&resolver.Error{Kind: "nope", Msg: "m"}); e.Exit != ExitGeneral || e.Code != "resolve.unknown" {
		t.Fatalf("未知分类应兜底为通用失败：%+v", e)
	}
}

func cliResolveManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	ver := func(v string, extra string) string {
		return fmt.Sprintf(`{"version":%q,%s"variants":{"default":{"artifacts":{"any":{"url":"https://r.example.invalid/x.tgz","sha256":%q,"size":1048576}}}}}`, v, extra, strings.Repeat("b", 64))
	}
	doc := `{"schema_version":1,"channel":"stable","sequence":1,"generated_at":"2026-01-01T00:00:00Z","valid_until":"2026-04-01T00:00:00Z",
	"revoked_versions":[{"component":"server","version":"0.2.0","reason":"bad"}],
	"components":{
	 "server":{"status":"available","versions":[` + ver("0.1.0", `"ports":[{"proto":"tcp","port":8080,"bind":"127.0.0.1"}],"requires":{"system":{"min_memory_mb":1024}},`) + `,` + ver("0.2.0", ``) + `]},
	 "dns":{"status":"unreleased"},
	 "scheduler":{"status":"builtin","builtin_in":"server"},
	 "site":{"status":"available","versions":[` + ver("1.0.0", `"requires":{"components":{"server":">=0.1.0 <0.2.0"}},`) + `,` + ver("2.0.0", `"requires":{"components":{"server":">=0.5.0"}},`) + `]}
	}}`
	m, err := manifest.ParseManifest([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// 真实的 resolver 输出经真实的 Run.finish：得到约定的退出码、稳定错误码、合法 envelope，且 details 带 blockers 与 Plan（--dry-run 同样展示）。
func TestResolverFailuresThroughFinish(t *testing.T) {
	m := cliResolveManifest(t)
	sys := resolver.System{OS: "linux", Arch: "amd64", MemoryAvailableMB: 4096, Disks: []resolver.Disk{{Mount: "/", FreeMB: 1 << 20}}}
	st := registry.NewState()
	st.Components["server"] = registry.Component{Version: "0.1.0", State: "running", Unit: "lingedge-server.service"}
	st.Components["site"] = registry.Component{Version: "1.0.0", State: "running"}
	lowMem := sys
	lowMem.MemoryAvailableMB = 10
	busy := sys
	busy.Listeners = []resolver.Listener{{Proto: "tcp", Addr: "0.0.0.0", Port: 8080, PID: 77, Process: "nginx", User: "www"}}

	for _, c := range []struct {
		name string
		in   resolver.Input
		exit int
		code string
	}{
		{"install dns：NOT_RELEASED", resolver.Input{Manifest: m, System: sys, Want: []resolver.Want{{Component: "dns"}}}, 7, "component.not_released"},
		{"install scheduler：BUILTIN", resolver.Input{Manifest: m, System: sys, Want: []resolver.Want{{Component: "scheduler"}}}, 8, "component.builtin"},
		{"未知组件", resolver.Input{Manifest: m, System: sys, Want: []resolver.Want{{Component: "nosuch"}}}, 2, "usage.invalid_value"},
		{"固定吊销版本", resolver.Input{Manifest: m, System: sys, Want: []resolver.Want{{Component: "server", Version: "0.2.0"}}}, 5, "verify.version_revoked"},
		{"内存不足", resolver.Input{Manifest: m, System: lowMem, Want: []resolver.Want{{Component: "server"}}}, 3, "precondition.resources"},
		{"端口冲突", resolver.Input{Manifest: m, System: busy, Want: []resolver.Want{{Component: "server"}}}, 4, "port.conflict"},
		{"无解（已装 server 0.1.0 不能满足 site 2.0.0 的 >=0.5.0）", resolver.Input{Manifest: m, System: sys, Installed: st, Want: []resolver.Want{{Component: "site", Version: "2.0.0"}}}, 3, "resolve.unsatisfiable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := resolver.Resolve(c.in)
			perr := p.Err()
			if perr == nil {
				t.Fatal("应有阻断")
			}
			got, env := finishWith(t, fmt.Errorf("install: %w", perr)) // 被包装也能识别
			if got != c.exit {
				t.Fatalf("退出码 = %d，期望 %d：%+v", got, c.exit, env["errors"])
			}
			errs, _ := env["errors"].([]any)
			if len(errs) != 1 {
				t.Fatalf("errors = %v", env["errors"])
			}
			item := errs[0].(map[string]any)
			if item["code"] != c.code {
				t.Fatalf("code = %v，期望 %s", item["code"], c.code)
			}
			d, _ := item["details"].(map[string]any)
			if d["blockers"] == nil || d["plan"] == nil {
				t.Fatalf("details 应含 blockers 与 plan: %v", item)
			}
			var re *resolver.Error
			if !errors.As(perr, &re) {
				t.Fatal("Err() 应为 *resolver.Error")
			}
		})
	}
}

func TestPassiveUpgradeConfirmThroughFinish(t *testing.T) {
	doc := `{"schema_version":1,"channel":"stable","sequence":1,"generated_at":"2026-01-01T00:00:00Z","valid_until":"2026-04-01T00:00:00Z","revoked_versions":[],"components":{
	 "server":{"status":"available","versions":[` + oneVer("0.1.0", "") + `,` + oneVer("0.2.0", "") + `]},
	 "site":{"status":"available","versions":[` + oneVer("1.0.0", `"requires":{"components":{"server":">=0.1.0 <0.2.0"}},`) + `,` + oneVer("1.1.0", `"requires":{"components":{"server":">=0.2.0"}},`) + `]}}}`
	m, err := manifest.ParseManifest([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	st := registry.NewState()
	st.Components["server"] = registry.Component{Version: "0.1.0", State: "running"}
	st.Components["site"] = registry.Component{Version: "1.0.0", State: "running"}
	sys := resolver.System{OS: "linux", Arch: "amd64", MemoryAvailableMB: 4096, Disks: []resolver.Disk{{Mount: "/", FreeMB: 1 << 20}}}
	in := resolver.Input{Manifest: m, System: sys, Installed: st, Want: []resolver.Want{{Component: "server", Version: "0.2.0"}}}
	p := resolver.Resolve(in)
	got, env := finishWith(t, p.Err())
	if got != 9 {
		t.Fatalf("退出码 = %d：%+v", got, env["errors"])
	}
	item := env["errors"].([]any)[0].(map[string]any)
	if item["code"] != "confirm.passive_upgrade" || !strings.Contains(item["message"].(string), "site 1.0.0 → 1.1.0") {
		t.Fatalf("%v", item)
	}
	in.AcceptPassiveUpgrade = true
	if err := resolver.Resolve(in).Err(); err != nil {
		t.Fatalf("确认后应放行: %v", err)
	}
}

func oneVer(v, extra string) string {
	return fmt.Sprintf(`{"version":%q,%s"variants":{"default":{"artifacts":{"any":{"url":"https://r.example.invalid/x.tgz","sha256":%q,"size":1048576}}}}}`, v, extra, strings.Repeat("c", 64))
}
