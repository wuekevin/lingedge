package installer_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

func plan(t testing.TB, w *toolsworld.World, comp string, mutDeps []func(*installer.Deps), mut ...func(*installer.Request)) (*installer.PreflightResult, error) {
	t.Helper()
	in := w.NewInstaller("run-plan", mutDeps...)
	req := baseReq(w, comp, append([]func(*installer.Request){func(r *installer.Request) { r.DryRun = true }}, mut...)...)
	return in.Plan(t.Context(), req)
}

// AC2：--dry-run 只做预检与 Plan，文件系统与 state 零变化——对整棵临时根目录（含 journal/锁/staging/信任库/状态目录）前后快照逐项相等；
// 包括成功、被阻断、有冲突、规格缺失、已装 noop 等各条路径。
func TestDryRunHasZeroSideEffects(t *testing.T) {
	type tc struct {
		name  string
		setup func(w *toolsworld.World)
		deps  []func(*installer.Deps)
		comp  string
		check func(t *testing.T, pre *installer.PreflightResult, err error)
	}
	cases := []tc{
		{name: "成功的计划", comp: "node", check: func(t *testing.T, pre *installer.PreflightResult, err error) {
			if err != nil || pre.Noop || len(pre.Plan.Components) != 2 || len(pre.Steps) != 9 {
				t.Fatalf("%+v %v", pre, err)
			}
		}},
		{name: "server 计划（含内建三件套）", comp: "server", check: func(t *testing.T, pre *installer.PreflightResult, err error) {
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "端口被占 → resolver 阻断（4）", comp: "node", setup: func(w *toolsworld.World) {
			w.Probe.System.Listeners = []resolver.Listener{{Proto: "tcp", Addr: "127.0.0.1", Port: 9100, PID: 812, Process: "nginx", User: "root"}}
		}, check: func(t *testing.T, pre *installer.PreflightResult, err error) {
			var re *resolver.Error
			if !errors.As(err, &re) || re.Kind != resolver.KindPort || pre == nil || pre.Plan == nil {
				t.Fatalf("应为端口阻断且仍返回计划: %v", err)
			}
		}},
		{name: "既有配置冲突 → 9 且不写 .new", comp: "node", setup: func(w *toolsworld.World) {
			mustWrite(t, filepath.Join(w.Root, "etc/lingedge/node/node.yaml"), "listen: 0.0.0.0:1\n")
		}, check: func(t *testing.T, pre *installer.PreflightResult, err error) {
			if kindOf(t, err) != installer.KindConfirmRequired || len(pre.Conflicts) == 0 {
				t.Fatalf("%v", err)
			}
		}},
		{name: "规格缺失（生产形态：没有任何规格）", comp: "node", deps: []func(*installer.Deps){func(d *installer.Deps) { d.Specs = installer.SpecSet{} }}, check: func(t *testing.T, pre *installer.PreflightResult, err error) {
			if kindOf(t, err) != installer.KindSpecMissing {
				t.Fatalf("%v", err)
			}
		}},
		{name: "已装 → noop", comp: "node", setup: func(w *toolsworld.World) { must(t, doInstall(t, w, "node", nil)) }, check: func(t *testing.T, pre *installer.PreflightResult, err error) {
			if err != nil || !pre.Noop {
				t.Fatalf("%+v %v", pre, err)
			}
		}},
		{name: "内置组件（8）", comp: "scheduler", check: func(t *testing.T, pre *installer.PreflightResult, err error) {
			var re *resolver.Error
			if !errors.As(err, &re) || re.Kind != resolver.KindBuiltin {
				t.Fatalf("%v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newW(t)
			if c.setup != nil {
				c.setup(w)
			}
			before := fullSnap(t, w)
			pre, err := plan(t, w, c.comp, c.deps)
			c.check(t, pre, err)
			if d := diffSnap(before, fullSnap(t, w)); d != "" {
				t.Fatalf("dry-run 改动了文件系统:\n%s", d)
			}
		})
	}
	// 全新机器上的 dry-run 不得创建状态目录、锁、journal、staging、信任库。
	w := newW(t)
	if _, err := plan(t, w, "node", nil); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"var/lib/lingedge/tools", "etc/lingedge", "opt/lingedge", "var/lib/lingedge-node"} {
		if w.Has(rel) {
			t.Errorf("dry-run 不应创建 %s", rel)
		}
	}
}

func mustWrite(t testing.TB, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

// AC3（FR-6、SM-C3）：既有配置/单元内容不同 → 停止并要求显式确认（非交互 → 9，不留任何痕迹）；确认后既有文件逐字节原样保留，新内容写入 .new；
// 之后若失败回滚，既有文件仍原样，.new 被撤销。
func TestConflictsAreNeverOverwritten(t *testing.T) {
	existingCfg := "listen: 0.0.0.0:1   # 管理员改过\n"
	existingUnit := "[Unit]\nDescription=admin edited\n"
	setup := func(w *toolsworld.World) {
		mustWrite(t, filepath.Join(w.Root, "etc/lingedge/node/node.yaml"), existingCfg)
		mustWrite(t, filepath.Join(w.Root, "etc/systemd/system/lingedge-node.service"), existingUnit)
	}
	t.Run("非交互无 --yes：9，无任何改动", func(t *testing.T) {
		w := newW(t)
		setup(w)
		before := sysSnap(t, w)
		o := doInstall(t, w, "node", nil)
		var ie *installer.Error
		if !errors.As(o.err, &ie) || ie.Kind != installer.KindConfirmRequired || len(ie.Conflicts) < 2 {
			t.Fatalf("应为 confirm_required 且列出冲突: %v", o.err)
		}
		// 只比较「系统」部分：Tools 簿记（锁文件）因 Begin 取锁而出现是预期的。
		if d := diffSnap(before, sysSnap(t, w)); d != "" {
			t.Fatalf("冲突停止后不应有任何改动:\n%s", d)
		}
	})
	t.Run("--yes：保留既有、写 .new", func(t *testing.T) {
		w := newW(t)
		setup(w)
		must(t, doInstall(t, w, "node", nil, func(r *installer.Request) { r.Yes = true }))
		if b, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/node/node.yaml")); string(b) != existingCfg {
			t.Fatalf("既有配置被改动: %q", b)
		}
		if b, _ := os.ReadFile(filepath.Join(w.Root, "etc/systemd/system/lingedge-node.service")); string(b) != existingUnit {
			t.Fatalf("既有单元被改动: %q", b)
		}
		newCfg, err := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/node/node.yaml.new"))
		if err != nil || !strings.Contains(string(newCfg), "listen: 127.0.0.1:9100") {
			t.Fatalf("node.yaml.new: %q %v", newCfg, err)
		}
		if b, err := os.ReadFile(filepath.Join(w.Root, "etc/systemd/system/lingedge-node.service.new")); err != nil || !strings.Contains(string(b), "[Service]") {
			t.Fatalf("单元 .new: %v", err)
		}
	})
	t.Run("--yes 后失败回滚：既有文件原样，.new 被撤销", func(t *testing.T) {
		w := newW(t)
		setup(w)
		before := sysSnap(t, w)
		w.SetCtl("fail-start", true)
		o := doInstall(t, w, "node", nil, func(r *installer.Request) { r.Yes = true })
		if kindOf(t, o.err) != installer.KindRolledBack {
			t.Fatalf("%v", o.err)
		}
		if d := diffSnap(before, sysSnap(t, w)); d != "" {
			t.Fatalf("回滚后应与安装前逐项一致（既有文件原样、无 .new）:\n%s", d)
		}
	})
	t.Run("交互：回答 y 继续，回答 n 取消", func(t *testing.T) {
		for _, ans := range []bool{true, false} {
			w := newW(t)
			setup(w)
			asked := 0
			o := doInstall(t, w, "node", []func(*installer.Deps){func(d *installer.Deps) {
				d.Confirm = func(p installer.Prompt) (bool, error) {
					asked++
					if p.Kind != "conflicts" || len(p.Items) < 2 {
						t.Errorf("提示: %+v", p)
					}
					return ans, nil
				}
			}}, func(r *installer.Request) { r.NonInteractive = false })
			if asked != 1 {
				t.Fatalf("应只询问一次: %d", asked)
			}
			if ans && o.err != nil {
				t.Fatalf("确认后应成功: %v", o.err)
			}
			if !ans && kindOf(t, o.err) != installer.KindConfirmRequired {
				t.Fatalf("拒绝后应为 confirm_required: %v", o.err)
			}
		}
	})
	t.Run("既有 master.key 沿用（绝不覆盖、不算冲突）", func(t *testing.T) {
		w := newW(t)
		mustWrite(t, filepath.Join(w.Root, "etc/lingedge/server/master.key"), "# existing\n@current k1\nk1:AAAA\n")
		if err := os.Chmod(filepath.Join(w.Root, "etc/lingedge/server/master.key"), 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(w.Root, "etc/lingedge/server"), 0o750); err != nil { // 既有目录权限与规格一致才不算冲突
			t.Fatal(err)
		}
		res := must(t, doInstall(t, w, "server", nil))
		if b, _ := os.ReadFile(filepath.Join(w.Root, "etc/lingedge/server/master.key")); !strings.Contains(string(b), "k1:AAAA") {
			t.Fatalf("既有主密钥被覆盖: %q", b)
		}
		for _, a := range res.Access {
			if a.Kind == "setup_token" && a.Value != "" && !w.Has("etc/lingedge/server/setup-token.hash") {
				t.Error("token 与哈希文件不一致")
			}
		}
	})
}

// 被动升级：只有「确认」类阻断时，经确认后以 AcceptPassiveUpgrade 重新求解；安装流程本身不执行升级，明确指向 update（Story 1.43）。
func TestPassiveUpgradeConfirmThenRefusedToExecute(t *testing.T) {
	seed := func(w *toolsworld.World) {
		if err := w.Registry("seed").UpdateState(t.Context(), func(s *registry.State) error {
			s.Components["openresty"] = registry.Component{Version: "0.0.9", State: "running", InstalledAt: "2026-11-01T00:00:00.000Z"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("非交互无 --yes：计划阻断 → 9（resolver 的 confirm）", func(t *testing.T) {
		w := newW(t)
		seed(w)
		pre, err := plan(t, w, "node", nil)
		var re *resolver.Error
		if !errors.As(err, &re) || re.Kind != resolver.KindConfirm || pre == nil {
			t.Fatalf("%v", err)
		}
	})
	t.Run("确认（--yes）后重新求解：升级被明确拒绝执行", func(t *testing.T) {
		w := newW(t)
		seed(w)
		pre, err := plan(t, w, "node", nil, func(r *installer.Request) { r.Yes = true })
		if kindOf(t, err) != installer.KindUpgradeRequired {
			t.Fatalf("%v", err)
		}
		found := false
		for _, c := range pre.Plan.Components {
			found = found || (c.Name == "openresty" && c.Action == "upgrade" && c.PassiveUpgrade)
		}
		if !found {
			t.Fatalf("确认后的计划应含 openresty 的被动升级: %+v", pre.Plan.Components)
		}
	})
	t.Run("交互确认", func(t *testing.T) {
		w := newW(t)
		seed(w)
		asked := ""
		_, err := plan(t, w, "node", []func(*installer.Deps){func(d *installer.Deps) {
			d.Confirm = func(p installer.Prompt) (bool, error) { asked = p.Kind; return true, nil }
		}}, func(r *installer.Request) { r.NonInteractive, r.DryRun = false, false })
		if asked != "passive_upgrade" || kindOf(t, err) != installer.KindUpgradeRequired {
			t.Fatalf("asked=%q err=%v", asked, err)
		}
	})
}

// ListenersKnown=false：端口占用无法检测，不当作无冲突——非交互无 --yes → KindPortUnverified（4）；--yes 或确认后继续。
func TestUnknownListenersAreNotTreatedAsNoConflict(t *testing.T) {
	w := newW(t)
	w.Probe.ListenersKnown, w.Probe.ListenersSource = false, "none"
	_, err := plan(t, w, "node", nil)
	if kindOf(t, err) != installer.KindPortUnverified {
		t.Fatalf("%v", err)
	}
	pre, err := plan(t, w, "node", nil, func(r *installer.Request) { r.Yes = true })
	if err != nil || pre.ListenersKnown {
		t.Fatalf("--yes 应放行: %v", err)
	}
	if len(pre.Conflicts) != 1 || pre.Conflicts[0].Kind != "port_unverified" {
		t.Fatalf("%+v", pre.Conflicts)
	}
}

// 系统探测的 Node.js（dep.nodejs.not_in_path）：非固定目录安装的绝对路径经 ConfigInput.Node/Runtime.Node 交给组件（website 属后续 Story，接口先备好）。
func TestNodeRuntimeAbsolutePathIsExposed(t *testing.T) {
	w := newW(t)
	w.Probe.System.Deps["nodejs"] = resolver.Dep{Detected: true, Version: "v20.11.0", Source: "/root/.nvm/versions/node/v20.11.0/bin/node", Flavor: "nodejs"}
	pre, err := plan(t, w, "node", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pre.Node == nil || pre.Node.Bin != "/root/.nvm/versions/node/v20.11.0/bin/node" || pre.Node.InPath {
		t.Fatalf("Node 运行时: %+v", pre.Node)
	}
	w.Probe.System.Deps["nodejs"] = resolver.Dep{Detected: true, Version: "v20.11.0", Source: "node"}
	pre, _ = plan(t, w, "node", nil)
	if pre.Node == nil || pre.Node.Bin != "node" || !pre.Node.InPath {
		t.Fatalf("PATH 上的 node: %+v", pre.Node)
	}
}

// 步骤映射：resolver 的高层步骤映射到 §12.1 的 22 步；映射表覆盖 resolver 产出的全部 Kind。
func TestPlanStepMapCoversResolverKinds(t *testing.T) {
	w := newW(t)
	pre, err := plan(t, w, "node", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pre.StepMap) != len(pre.Plan.Steps) || len(pre.StepMap) == 0 {
		t.Fatalf("映射条数 %d vs %d", len(pre.StepMap), len(pre.Plan.Steps))
	}
	for _, m := range pre.StepMap {
		if len(m.Steps) == 0 {
			t.Errorf("resolver 步骤 %s 没有对应的 22 步映射", m.Kind)
		}
		for _, n := range m.Steps {
			if n < 1 || n > 22 {
				t.Errorf("映射越界: %+v", m)
			}
		}
	}
}

// 计划里补上 node 的升级 path/service 单元（resolver 画像只列出 lingedge-node.service），使计划如实。
func TestPlanListsAllRenderedUnits(t *testing.T) {
	w := newW(t)
	pre, err := plan(t, w, "node", nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, u := range pre.Plan.Units {
		names[u.Name] = true
	}
	for _, n := range []string{"lingedge-node.service", "lingedge-node-upgrade.path", "lingedge-node-upgrade.service", "lingedge-openresty.service", "lingedge-openresty.path"} {
		if !names[n] {
			t.Errorf("计划缺少单元 %s: %v", n, names)
		}
	}
}
