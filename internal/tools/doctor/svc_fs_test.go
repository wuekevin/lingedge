package doctor

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// fakeCtl 是 systemctl 的测试替身（经真实的 service.Systemctl 封装与参数构造）：按单元名给出 show 的属性。
type fakeCtl struct {
	state map[string]map[string]string // unit → 属性
	ver   string
}

func (f *fakeCtl) Run(_ context.Context, c safeexec.Cmd) (safeexec.Result, error) {
	switch c.Args[0] {
	case "--version":
		v := f.ver
		if v == "" {
			v = "systemd 252 (252.22-1~deb12u1)"
		}
		return safeexec.Result{Stdout: []byte(v + "\n")}, nil
	case "show":
		unit := c.Args[len(c.Args)-1]
		p := map[string]string{"LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "UnitFileState": "disabled", "Result": "success", "MainPID": "0", "NRestarts": "0"}
		for k, v := range f.state[unit] {
			p[k] = v
		}
		var b strings.Builder
		for _, k := range []string{"LoadState", "ActiveState", "SubState", "UnitFileState", "Result", "MainPID", "NRestarts"} {
			fmt.Fprintf(&b, "%s=%s\n", k, p[k])
		}
		return safeexec.Result{Stdout: []byte(b.String())}, nil
	}
	return safeexec.Result{}, errors.New("unexpected " + c.Args[0])
}

func ctl(state map[string]map[string]string) *service.Systemctl {
	return &service.Systemctl{Run: &fakeCtl{state: state}, Booted: func() bool { return true }}
}

func active(extra ...string) map[string]string {
	m := map[string]string{"ActiveState": "active", "SubState": "running", "UnitFileState": "enabled", "MainPID": "321"}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	return m
}

func TestSvcChecks(t *testing.T) {
	server := Component{Name: "server", Version: "1.0.0", State: "running", Unit: "lingedge-server.service"}
	cases := []struct {
		name                  string
		props                 map[string]string
		state                 string
		act, enabled, restart Level
	}{
		{"全部正常", active(), "running", LevelPass, LevelPass, LevelPass},
		{"未运行", map[string]string{"UnitFileState": "enabled"}, "running", LevelFail, LevelPass, LevelPass},
		{"人为停止（记录为 stopped）→ 告警", map[string]string{"UnitFileState": "enabled"}, "stopped", LevelWarn, LevelPass, LevelPass},
		{"failed", active("ActiveState", "failed", "SubState", "failed", "Result", "exit-code"), "running", LevelFail, LevelPass, LevelPass},
		{"没有开机自启", active("UnitFileState", "disabled"), "running", LevelPass, LevelWarn, LevelPass},
		{"重启 3 次告警", active("NRestarts", "3"), "running", LevelPass, LevelPass, LevelWarn},
		{"重启 2 次正常", active("NRestarts", "2"), "running", LevelPass, LevelPass, LevelPass},
		{"auto-restart 循环", active("ActiveState", "activating", "SubState", "auto-restart", "NRestarts", "9"), "running", LevelWarn, LevelPass, LevelFail},
		{"单元不存在", map[string]string{"LoadState": "not-found"}, "running", LevelFail, LevelWarn, LevelPass},
	}
	for _, c := range cases {
		w := newWorld(t)
		s := server
		s.State = c.state
		w.Components = []Component{s}
		w.Systemctl = ctl(map[string]map[string]string{"lingedge-server.service": c.props})
		rep := runAll(t, w, Filter{Component: "server"})
		want(t, find(t, rep, "svc.server.active"), c.act, "")
		want(t, find(t, rep, "svc.server.enabled"), c.enabled, "")
		want(t, find(t, rep, "svc.server.restarts"), c.restart, "")
	}
	// 阈值可配置。
	w := newWorld(t)
	w.Components = []Component{server}
	w.Cfg.RestartsWarn = 10
	w.Systemctl = ctl(map[string]map[string]string{"lingedge-server.service": active("NRestarts", "9")})
	want(t, find(t, runAll(t, w, Filter{Component: "server"}), "svc.server.restarts"), LevelPass, "")
}

func TestSvcNodeChecksAllUnits(t *testing.T) {
	w := newWorld(t)
	w.Components = []Component{{Name: "node", Version: "1.0.0", State: "running", Unit: "lingedge-node.service"}}
	w.Systemctl = ctl(map[string]map[string]string{
		"lingedge-node.service": active(), "lingedge-openresty.path": active(), "lingedge-node-upgrade.path": map[string]string{"UnitFileState": "disabled"},
	})
	rep := runAll(t, w, Filter{Component: "node"})
	want(t, find(t, rep, "svc.node.active"), LevelPass, "")
	f := find(t, rep, "svc.node.enabled")
	want(t, f, LevelWarn, "lingedge-node-upgrade.path")
}

func TestSvcWithoutSystemd(t *testing.T) {
	w := newWorld(t)
	w.Components = []Component{{Name: "server", Version: "1.0.0", Unit: "lingedge-server.service"}}
	w.Systemctl = &service.Systemctl{Run: &fakeCtl{}, Booted: func() bool { return false }}
	rep := runAll(t, w, Filter{Component: "server"})
	for _, id := range []string{"svc.server.active", "svc.server.enabled", "svc.server.restarts"} {
		want(t, find(t, rep, id), LevelSkip, "systemd")
	}
	w.Systemctl = nil
	rep = runAll(t, newWorldWith3(w), Filter{Component: "server"})
	want(t, find(t, rep, "svc.server.active"), LevelSkip, "")
}

func newWorldWith3(w *World) *World {
	n := newWorldWith(w)
	n.Systemctl = w.Systemctl
	return n
}

func TestSystemdVersionAndCredentialFindings(t *testing.T) {
	w := newWorld(t)
	w.Components = []Component{{Name: "server", Version: "1.0.0", Unit: "lingedge-server.service"}}
	fc := &fakeCtl{ver: "systemd 245 (245.4-4ubuntu3)"}
	w.Systemctl = &service.Systemctl{Run: fc, Booted: func() bool { return true }}
	put(t, w, "etc/systemd/system/lingedge-server.service", "[Service]\nLoadCredential=master.key:/etc/lingedge/server/master.key\n", 0o644)
	rep := runAll(t, w, Filter{})
	want(t, find(t, rep, "service.systemd_version"), LevelWarn, "245")
	want(t, find(t, rep, "service.credential_mode"), LevelFail, "LoadCredential")
	put(t, w, "etc/systemd/system/lingedge-node.service.new", "x", 0o644)
	rep = runAll(t, newWorldWith3(w), Filter{})
	want(t, find(t, rep, "service.unit_new_pending"), LevelWarn, "lingedge-node.service.new")
}

func TestHealthServer(t *testing.T) {
	srv := Component{Name: "server", Version: "1.0.0", State: "running", DataDir: "/var/lib/lingedge/server"}
	mk := func(init bool, get func(string) (int, error)) Finding {
		w := newWorld(t)
		w.Components = []Component{srv}
		w.ServerListens = func() (string, string) { return "0.0.0.0:8080", "" }
		_ = os.MkdirAll(filepath.Join(w.Root, "var/lib/lingedge/server"), 0o755)
		if init {
			put(t, w, "var/lib/lingedge/server/install.lock", "", 0o600)
		}
		w.HTTPGet = func(_ context.Context, u string) (int, error) {
			if !strings.HasPrefix(u, "http://127.0.0.1:8080/") {
				t.Errorf("通配监听必须换成回环地址：%s", u)
			}
			return get(u)
		}
		return find(t, runAll(t, w, Filter{Component: "server"}), "health.server")
	}
	ok := func(string) (int, error) { return 200, nil }
	want(t, mk(true, ok), LevelPass, "就绪")
	want(t, mk(false, ok), LevelPass, "setup mode")
	want(t, mk(true, func(u string) (int, error) {
		if strings.HasSuffix(u, "/ready") {
			return 503, nil
		}
		return 200, nil
	}), LevelFail, "未就绪")
	want(t, mk(true, func(string) (int, error) { return 0, errors.New("连接失败") }), LevelFail, "无法连接")
	want(t, mk(true, func(u string) (int, error) {
		if strings.HasSuffix(u, "/ready") {
			return 0, errors.New("超时")
		}
		return 200, nil
	}), LevelFail, "无响应")
	want(t, mk(true, func(string) (int, error) { return 500, nil }), LevelFail, "HTTP 500")
	// 向导阶段的 /healthz/ready 不被探测（必然 503）。
	var asked []string
	mk(false, func(u string) (int, error) { asked = append(asked, u); return 200, nil })
	for _, u := range asked {
		if strings.HasSuffix(u, "/ready") {
			t.Error("setup mode 不应探测 /healthz/ready")
		}
	}
	// node 的深度探测属 Epic 3：如实 skip，不假装健康。
	w := newWorld(t)
	w.Components = []Component{{Name: "node", Version: "1.0.0"}}
	want(t, find(t, runAll(t, w, Filter{Component: "node"}), "health.node"), LevelSkip, "3.31")
}

// ---- fs.* ----

func TestMasterKeyPerm(t *testing.T) {
	setup := func(mode os.FileMode, unit string, fix bool) (*World, Finding) {
		w := newWorld(t)
		w.Components = []Component{{Name: "server", Version: "1.0.0"}}
		put(t, w, "etc/lingedge/server/master.key", "k1:AAAA\n", mode)
		if unit != "" {
			put(t, w, "etc/systemd/system/lingedge-server.service", unit, 0o644)
		}
		w.FixPerms = fix
		return w, find(t, runAll(t, w, Filter{Component: "server"}), "fs.master_key_perm")
	}
	cred := "[Service]\nLoadCredential=master.key:/etc/lingedge/server/master.key\n"
	keyfile := "[Service]\nExecStart=/x\n"
	_, f := setup(0o400, cred, false)
	want(t, f, LevelPass, "LoadCredential")
	_, f = setup(0o440, keyfile, false)
	want(t, f, LevelPass, "降级")
	_, f = setup(0o440, cred, false)
	want(t, f, LevelFail, "宽于")
	_, f = setup(0o644, keyfile, false)
	want(t, f, LevelFail, "0644")
	_, f = setup(0o600, "", false) // 单元读不到：接受 0400/0440，0600 宽于 0440
	want(t, f, LevelFail, "0600")
	// 缺失：不修复、不重建。
	w := newWorld(t)
	w.Components = []Component{{Name: "server", Version: "1.0.0"}}
	w.FixPerms = true
	f = find(t, runAll(t, w, Filter{Component: "server"}), "fs.master_key_perm")
	want(t, f, LevelFail, "不存在")
	if _, err := os.Stat(filepath.Join(w.Root, "etc/lingedge/server/master.key")); err == nil {
		t.Error("--fix-perms 不得创建 master.key")
	}
}

func TestFixPermsFixesOnlyDefinedDeviationsAndNeverTouchesContent(t *testing.T) {
	w := newWorld(t)
	w.Components = []Component{{Name: "server", Version: "1.0.0"}}
	mk := put(t, w, "etc/lingedge/server/master.key", "k1:SECRET-KEY-MATERIAL\n", 0o666)
	cfg := put(t, w, "etc/lingedge/server/server.yaml", "server:\n  env: prod\n", 0o666)
	other := put(t, w, "etc/lingedge/server/unrelated.txt", "x", 0o666) // 不在修复范围
	put(t, w, "etc/systemd/system/lingedge-server.service", "[Service]\nLoadCredential=master.key:x\n", 0o644)
	if err := os.Chmod(filepath.Dir(mk), 0o777); err != nil {
		t.Fatal(err)
	}
	w.FixPerms, w.Euid = true, 0 // 生产里 --fix-perms 只在 root 下运行（cli 层强制）
	before, _ := os.ReadFile(mk)
	rep := runAll(t, w, Filter{Component: "server"})
	f := find(t, rep, "fs.master_key_perm")
	want(t, f, LevelPass, "已修复")
	if !f.Fixed {
		t.Error("应标记 Fixed")
	}
	if fi, _ := os.Stat(mk); fi.Mode().Perm() != 0o400 {
		t.Errorf("master.key 权限 %v，期望 0400", fi.Mode().Perm())
	}
	if after, _ := os.ReadFile(mk); !bytes.Equal(before, after) {
		t.Error("--fix-perms 不得改动 master.key 的内容")
	}
	c := find(t, rep, "fs.config_perm")
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o640 {
		t.Errorf("server.yaml 权限 %v，期望 0640", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(cfg)); fi.Mode().Perm() != 0o750 {
		t.Errorf("配置目录权限 %v，期望 0750", fi.Mode().Perm())
	}
	if c.Level != LevelPass || !c.Fixed {
		t.Errorf("配置修复后应复检通过：%+v", c)
	}
	if fi, _ := os.Stat(other); fi.Mode().Perm() != 0o666 {
		t.Errorf("范围之外的文件不得被改动：%v", fi.Mode().Perm())
	}
	// 复检：再跑一次（不带 fix）全部通过。
	w.FixPerms = false
	rep = runAll(t, newWorldWith4(w), Filter{Component: "server"})
	want(t, find(t, rep, "fs.master_key_perm"), LevelPass, "")
	want(t, find(t, rep, "fs.config_perm"), LevelPass, "")
}

func newWorldWith4(w *World) *World {
	n := newWorldWith(w)
	n.Components = w.Components
	return n
}

func TestFixPermsDryRunAndNonRoot(t *testing.T) {
	w := newWorld(t)
	w.Components = []Component{{Name: "server", Version: "1.0.0"}}
	mk := put(t, w, "etc/lingedge/server/master.key", "k", 0o644)
	w.FixPerms, w.DryRun, w.Euid = true, true, 0
	f := find(t, runAll(t, w, Filter{Component: "server"}), "fs.master_key_perm")
	want(t, f, LevelFail, "0644")
	if fi, _ := os.Stat(mk); fi.Mode().Perm() != 0o644 {
		t.Error("dry-run 不得修改")
	}
	if f.Details["would_fix"] == nil {
		t.Errorf("dry-run 应列出将修复的项：%+v", f.Details)
	}
	// 非 root 即便传了 FixPerms 也不改（cli 层本来就会拒绝，这里是纵深防御）。
	w2 := newWorld(t)
	w2.Euid = 1000
	w2.Components = []Component{{Name: "server", Version: "1.0.0"}}
	mk2 := put(t, w2, "etc/lingedge/server/master.key", "k", 0o644)
	w2.FixPerms = true
	runAll(t, w2, Filter{Component: "server"})
	if fi, _ := os.Stat(mk2); fi.Mode().Perm() != 0o644 {
		t.Error("非 root 不得修复")
	}
}

func TestFixPermsOwnerViaChownAndSymlinksRefused(t *testing.T) {
	w := newWorld(t)
	own := w.Own.(*fakeOwn)
	own.resolve["root"] = [2]int{own.uid + 1000, own.gid + 1000} // 期望属主与文件实际属主不同
	w.Components = []Component{{Name: "server", Version: "1.0.0"}}
	put(t, w, "etc/lingedge/server/master.key", "k", 0o400)
	put(t, w, "etc/systemd/system/lingedge-server.service", "LoadCredential=x\n", 0o644)
	f := find(t, runAll(t, w, Filter{Component: "server"}), "fs.master_key_perm")
	want(t, f, LevelFail, "属主")
	w.FixPerms = true
	w.Euid = 0
	f = find(t, runAll(t, newWorldWith5(w, own), Filter{Component: "server"}), "fs.master_key_perm")
	if len(own.chowned) == 0 {
		t.Errorf("属主偏差应经 Chown 修复：%+v", f)
	}
	// 符号链接：不跟随、不修复。
	w2 := newWorld(t)
	w2.Components = []Component{{Name: "server", Version: "1.0.0"}}
	target := filepath.Join(w2.Root, "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o666); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(w2.Root, "etc/lingedge/server")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.Symlink(target, filepath.Join(dir, "master.key")); err != nil {
		t.Fatal(err)
	}
	w2.FixPerms, w2.Euid = true, 0
	f = find(t, runAll(t, w2, Filter{Component: "server"}), "fs.master_key_perm")
	want(t, f, LevelFail, "符号链接")
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o666 {
		t.Error("符号链接目标不得被修复/改动")
	}
}

func newWorldWith5(w *World, own *fakeOwn) *World {
	n := newWorldWith(w)
	n.Own, n.Euid, n.FixPerms, n.Components = own, 0, true, w.Components
	return n
}

func TestIdentityPerm(t *testing.T) {
	w := newWorld(t)
	w.Components = []Component{{Name: "node", Version: "1.0.0"}}
	want(t, find(t, runAll(t, w, Filter{Component: "node"}), "fs.identity_perm"), LevelPass, "") // 尚未注册：目录可选
	dir := "var/lib/lingedge-node/identity"
	put(t, w, dir+"/node.key.pem", "x", 0o644)
	put(t, w, dir+"/node.crt.pem", "x", 0o600)
	if err := os.Chmod(filepath.Join(w.Root, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	f := find(t, runAll(t, w, Filter{Component: "node"}), "fs.identity_perm")
	want(t, f, LevelFail, "node.key.pem")
	f = find(t, runAll(t, newWorldWith5(w, w.Own.(*fakeOwn)), Filter{Component: "node"}), "fs.identity_perm")
	want(t, f, LevelPass, "已修复")
	if fi, _ := os.Stat(filepath.Join(w.Root, dir)); fi.Mode().Perm() != 0o700 {
		t.Errorf("identity 目录应为 0700：%v", fi.Mode().Perm())
	}
}

func TestReleaseOwner(t *testing.T) {
	w := newWorld(t)
	w.Components = []Component{{Name: "server", Version: "1.0.0", RunUser: "lingedge", ReleaseDir: "/opt/lingedge/server/releases/1.0.0"}}
	put(t, w, "opt/lingedge/server/releases/1.0.0/bin/lingedge-server", "bin", 0o755)
	own := w.Own.(*fakeOwn)
	own.resolve["root"] = [2]int{0, 0}
	own.resolve["lingedge"] = [2]int{own.uid + 1, own.gid + 1} // 运行用户不是文件属主
	f := find(t, runAll(t, w, Filter{Component: "server"}), "fs.release_owner.server")
	want(t, f, LevelPass, "")
	// 运行用户 == 文件属主 → 可写自己的二进制。
	own.resolve["lingedge"] = [2]int{own.uid, own.gid}
	w2 := newWorld(t)
	w2.Own = own
	w2.Components = w.Components
	put(t, w2, "opt/lingedge/server/releases/1.0.0/bin/lingedge-server", "bin", 0o755)
	f = find(t, runAll(t, w2, Filter{Component: "server"}), "fs.release_owner.server")
	if os.Geteuid() != 0 {
		want(t, f, LevelFail, "运行用户")
	}
	// other 可写。
	own.resolve["lingedge"] = [2]int{own.uid + 1, own.gid + 1}
	w3 := newWorld(t)
	w3.Own = own
	w3.Components = w.Components
	p := put(t, w3, "opt/lingedge/server/releases/1.0.0/bin/x", "bin", 0o755)
	if err := os.Chmod(p, 0o757); err != nil {
		t.Fatal(err)
	}
	want(t, find(t, runAll(t, w3, Filter{Component: "server"}), "fs.release_owner.server"), LevelFail, "other 可写")
}

func TestDirsAndStateDir(t *testing.T) {
	w := newWorld(t)
	w.Components = []Component{{Name: "server", Version: "1.0.0"}}
	rep := runAll(t, w, Filter{})
	want(t, find(t, rep, "fs.dirs"), LevelFail, "不存在")
	for _, sp := range service.ServerDirs("lingedge", "lingedge") {
		p := filepath.Join(w.Root, sp.Path)
		if err := os.MkdirAll(p, sp.Mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, sp.Mode)
	}
	want(t, find(t, runAll(t, newWorldWith4(w), Filter{}), "fs.dirs"), LevelPass, "")
	// 状态目录：网络文件系统告警、权限过宽失败。
	if err := os.MkdirAll(w.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	w2 := newWorldWith4(w)
	w2.FSKind = func(string) (registry.FSInfo, error) { return registry.FSInfo{Type: "nfs", Network: true}, nil }
	want(t, find(t, runAll(t, w2, Filter{}), "fs.state_dir"), LevelWarn, "nfs")
	if err := os.Chmod(w.StateDir, 0o777); err != nil {
		t.Fatal(err)
	}
	want(t, find(t, runAll(t, newWorldWith4(w), Filter{}), "fs.state_dir"), LevelFail, "group/other")
}

// ---- fe.* ----

func gzOf(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

func feWorld(t *testing.T) *World {
	t.Helper()
	w := newWorld(t)
	w.Components = []Component{{Name: "admin-web", Version: "1.0.0", State: "installed", ReleaseDir: "/opt/lingedge/admin-web/releases/1.0.0"}}
	rel := filepath.Join(w.Root, "opt/lingedge/admin-web/releases/1.0.0/public")
	if err := os.MkdirAll(filepath.Join(rel, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	index := `<!doctype html><link rel="stylesheet" href="/assets/app-1a2b.css"><script type="module" src="/assets/app-3c4d.js"></script><a href="https://example.org/x">x</a><a href="/api/v1/x">api</a>`
	js, css := "console.log(1);", "body{}"
	for p, c := range map[string]string{"index.html": index, "assets/app-3c4d.js": js, "assets/app-1a2b.css": css} {
		if err := os.WriteFile(filepath.Join(rel, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rel, p+".gz"), gzOf([]byte(c)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/1.0.0", filepath.Join(w.Root, "opt/lingedge/admin-web/current")); err != nil {
		t.Fatal(err)
	}
	conf, hdr, err := installer.RenderStaticSite(installer.SiteParams{Component: "admin-web", Host: "admin.example.com", APIUpstream: "http://127.0.0.1:8080", HTTPPort: 80, HTTPSPort: 443})
	if err != nil {
		t.Fatalf("渲染站点片段：%v", err)
	}
	put(t, w, installer.StaticConfPath("admin-web"), string(conf), 0o644)
	put(t, w, installer.StaticHeadersPath("admin-web"), string(hdr), 0o644)
	return w
}

func TestFrontendDeepChecks(t *testing.T) {
	w := feWorld(t)
	rep := runAll(t, w, Filter{Component: "admin-web"})
	f := find(t, rep, "fe.admin-web.release")
	want(t, f, LevelPass, "2 个资源齐全")
	if f.Details["gzip_verified"].(int) != 3 {
		t.Errorf("3 个预压缩副本都应被逐字节核对：%+v", f.Details)
	}
	want(t, find(t, rep, "fe.admin-web.nginx"), LevelPass, "admin.example.com")

	rel := func(w *World) string { return filepath.Join(w.Root, "opt/lingedge/admin-web/releases/1.0.0/public") }
	// 引用的哈希资源缺失。
	w = feWorld(t)
	_ = os.Remove(filepath.Join(rel(w), "assets/app-3c4d.js"))
	want(t, find(t, runAll(t, w, Filter{Component: "admin-web"}), "fe.admin-web.release"), LevelFail, "app-3c4d.js")
	// 预压缩副本与原文件不一致（nginx gzip_static 会下发错内容）。
	w = feWorld(t)
	_ = os.WriteFile(filepath.Join(rel(w), "assets/app-3c4d.js.gz"), gzOf([]byte("console.log('tampered');")), 0o644)
	want(t, find(t, runAll(t, w, Filter{Component: "admin-web"}), "fe.admin-web.release"), LevelFail, "app-3c4d.js.gz")
	// .gz 不是合法 gzip。
	w = feWorld(t)
	_ = os.WriteFile(filepath.Join(rel(w), "index.html.gz"), []byte("not gzip"), 0o644)
	want(t, find(t, runAll(t, w, Filter{Component: "admin-web"}), "fe.admin-web.release"), LevelFail, "gzip")
	// current 指向错误的版本。
	w = feWorld(t)
	cur := filepath.Join(w.Root, "opt/lingedge/admin-web/current")
	_ = os.Remove(cur)
	_ = os.Symlink("releases/0.9.0", cur)
	want(t, find(t, runAll(t, w, Filter{Component: "admin-web"}), "fe.admin-web.release"), LevelFail, "0.9.0")
	// index.html 引用越出站点根的路径。
	w = feWorld(t)
	_ = os.WriteFile(filepath.Join(rel(w), "index.html"), []byte(`<script src="/../../../../etc/passwd"></script>`), 0o644)
	_ = os.Remove(filepath.Join(rel(w), "index.html.gz"))
	want(t, find(t, runAll(t, w, Filter{Component: "admin-web"}), "fe.admin-web.release"), LevelFail, "缺失")
	// index.html 为空。
	w = feWorld(t)
	_ = os.WriteFile(filepath.Join(rel(w), "index.html"), nil, 0o644)
	want(t, find(t, runAll(t, w, Filter{Component: "admin-web"}), "fe.admin-web.release"), LevelFail, "index.html")
}

func TestFrontendNginxChecks(t *testing.T) {
	w := feWorld(t)
	// 站点片段被改坏：含 add_header 但没有显式 include 安全头文件。
	conf, _ := os.ReadFile(filepath.Join(w.Root, installer.StaticConfPath("admin-web")))
	bad := strings.Replace(string(conf), "proxy_http_version 1.1;", "proxy_http_version 1.1;\n        add_header X-Foo bar;", 1)
	_ = os.WriteFile(filepath.Join(w.Root, installer.StaticConfPath("admin-web")), []byte(bad), 0o644)
	f := find(t, runAll(t, w, Filter{Component: "admin-web"}), "fe.admin-web.nginx")
	want(t, f, LevelFail, "静态检查")
	// 缺文件。
	w = feWorld(t)
	_ = os.Remove(filepath.Join(w.Root, installer.StaticHeadersPath("admin-web")))
	want(t, find(t, runAll(t, w, Filter{Component: "admin-web"}), "fe.admin-web.nginx"), LevelFail, "缺失")
	// conf.d 软链：ours / foreign。
	w = feWorld(t)
	link := filepath.Join(w.Root, installer.StaticLinkPath("admin-web"))
	_ = os.MkdirAll(filepath.Dir(link), 0o755)
	_ = os.Symlink(installer.StaticConfPath("admin-web"), link)
	f = find(t, runAll(t, w, Filter{Component: "admin-web"}), "fe.admin-web.nginx")
	want(t, f, LevelPass, "软链指向 Tools 片段")
	_ = os.Remove(link)
	_ = os.WriteFile(link, []byte("# someone else"), 0o644)
	want(t, find(t, runAll(t, w, Filter{Component: "admin-web"}), "fe.admin-web.nginx"), LevelWarn, "不是指向 Tools")
}
