package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.43 的命令层：update check / update <组件> / update --all / update self / rollback / version --check。
// 真实 cli.Execute → installer → registry/downloader/resolver/backup；伪的只有「操作系统」（伪 systemctl）与「组件」（toolsworld 假规格，仅 _test.go 引用）。

func nodeBundle(t *testing.T, w *toolsworld.World, seq int64, ver string, mut ...func(*toolsworld.BundleComp)) string {
	t.Helper()
	comps := []toolsworld.BundleComp{
		{Name: "node", Version: ver, Requires: map[string]any{"components": map[string]string{"openresty": ">=0.1.0"}},
			Ports: []map[string]any{{"proto": "tcp", "port": 9100, "bind": "127.0.0.1", "purpose": "agent"}},
			Files: []toolsworld.PkgFile{{Name: "bin/lingedge-node", Body: "#!node-" + ver + "\n", Exec: true}}},
		{Name: "openresty", Version: "0.1.0", Internal: true, Files: []toolsworld.PkgFile{{Name: "bin/openresty", Body: "#!openresty-v1\n", Exec: true}}},
	}
	for _, m := range mut {
		m(&comps[0])
	}
	p := filepath.Join(t.TempDir(), "b-"+ver+".tar")
	if err := w.BuildBundle(p, seq, comps, nil); err != nil {
		t.Fatal(err)
	}
	return p
}

func fastCfg(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tools.yaml")
	if err := os.WriteFile(p, []byte("install:\n  health_timeout: 80ms\n  health_interval: 10ms\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func installedNodeHarness(t *testing.T, opts ...hopt) (*harness, *toolsworld.World, string) {
	t.Helper()
	h, w := installHarness(t, opts...)
	cfg := fastCfg(t)
	if code := h.run("install", "node", "--offline", w.Bundle, "--json", "--config", cfg); code != 0 {
		t.Fatalf("install: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	return h, w, cfg
}

func (h *harness) reset() { h.stdout.Reset(); h.stderr.Reset() }

func TestUpdateCommandEndToEnd(t *testing.T) {
	h, w, cfg := installedNodeHarness(t)
	b2 := nodeBundle(t, w, 11, "0.2.0")
	// --dry-run / --plan-only 同源、零变化、符合 schema。
	for _, flag := range []string{"--dry-run", "--plan-only"} {
		h.reset()
		if code := h.run("update", "node", "--offline", b2, flag, "--json", "--config", cfg); code != 0 {
			t.Fatalf("%s: %d\n%s\n%s", flag, code, h.stdout, h.stderr)
		}
		env := h.envelope()
		if err := validateAgainst(t, "data.update.schema.json", env["data"]); err != nil {
			t.Fatalf("data 不符合 schema: %v\n%s", err, h.stdout)
		}
		it := env["data"].(map[string]any)["components"].([]any)[0].(map[string]any)
		if it["outcome"] != "planned" || it["to"] != "0.2.0" || it["path"] != "binary" || env["dry_run"] != true {
			t.Fatalf("%v", it)
		}
		if tg, _ := os.Readlink(filepath.Join(w.Root, "opt/lingedge/node/current")); tg != "releases/0.1.0" {
			t.Fatal("dry-run 不得切换 current")
		}
	}
	// 真实升级：进度是 13 步的 NDJSON（total=13）；data 符合 schema。
	h.reset()
	if code := h.run("update", "node", "--offline", b2, "--json", "--config", cfg); code != 0 {
		t.Fatalf("update: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.update.schema.json", env["data"]); err != nil {
		t.Fatalf("data 不符合 schema: %v\n%s", err, h.stdout)
	}
	it := env["data"].(map[string]any)["components"].([]any)[0].(map[string]any)
	if it["outcome"] != "updated" || it["from"] != "0.1.0" || it["backup"] == "" || it["backup_verified"] != true {
		t.Fatalf("%v", it)
	}
	seen := map[int]bool{}
	sc := bufio.NewScanner(strings.NewReader(h.stderr.String()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("进度行不是 JSON: %q", sc.Text())
		}
		if ev, ok := m["event"].(map[string]any); ok && ev["type"] == "step_start" && ev["op"] == "update" {
			if ev["total"] != float64(13) {
				t.Fatalf("事件 total 应为 13: %v", ev)
			}
			seen[int(ev["step"].(float64))] = true
		}
	}
	for _, n := range []int{5, 6, 7, 8, 9, 10, 11, 12, 13} {
		if !seen[n] && n != 7 && n != 9 {
			t.Errorf("进度缺少第 %d 步", n)
		}
	}
	// 已是最新 → no-op，退出码 0。
	h.reset()
	if code := h.run("update", "node", "--offline", b2, "--json", "--config", cfg); code != 0 {
		t.Fatal(code)
	}
	if h.envelope()["data"].(map[string]any)["components"].([]any)[0].(map[string]any)["outcome"] != "already_up_to_date" {
		t.Fatalf("%s", h.stdout)
	}
	// 人类输出包含回滚路径与承诺。
	h.reset()
	h.run("update", "node", "--offline", nodeBundle(t, w, 12, "0.3.0"), "--config", cfg, "--yes")
	if !strings.Contains(h.stdout.String(), "失败回滚路径") || !strings.Contains(h.stdout.String(), "二进制回退即恢复") {
		t.Errorf("人类输出应展示回滚路径与承诺:\n%s", h.stdout)
	}
}

// 退出码：健康检查失败的新版本 → 自动回滚、退出码 10；回滚也失败 → 12 并保留 journal，recover --rollback 收尾。
func TestUpdateExitCodes10And12(t *testing.T) {
	h, w, cfg := installedNodeHarness(t)
	b2 := nodeBundle(t, w, 11, "0.2.0")
	w.SetCtl("fail-health-node-0.2.0", true)
	if code := h.run("update", "node", "--offline", b2, "--json", "--config", cfg); code != 10 {
		t.Fatalf("退出码 %d，期望 10\n%s\n%s", code, h.stdout, h.stderr)
	}
	env := h.envelope()
	e := env["errors"].([]any)[0].(map[string]any)
	if e["system_state"] != "rolled_back" || env["exit_name"] != "ROLLED_BACK" {
		t.Fatalf("%v", env)
	}
	if tg, _ := os.Readlink(filepath.Join(w.Root, "opt/lingedge/node/current")); tg != "releases/0.1.0" {
		t.Fatal("回滚后 current 应在旧版本")
	}
	// 12：回滚本身失败（让 fail-start 在回滚的重启时触发 → 旧版本起不来）。
	h.reset()
	w.SetCtl("fail-health-node-0.2.0", false)
	w.SetCtl("fail-health-node-0.1.0", true) // 回滚后复核旧版本健康失败 → 撤销复核失败
	w.SetCtl("fail-health-node-0.2.0", true)
	if code := h.run("update", "node", "--offline", b2, "--json", "--config", cfg); code != 12 {
		t.Fatalf("退出码 %d，期望 12\n%s\n%s", code, h.stdout, h.stderr)
	}
	env = h.envelope()
	e = env["errors"].([]any)[0].(map[string]any)
	if e["system_state"] != "needs_manual" || env["exit_name"] != "ROLLBACK_FAILED" {
		t.Fatalf("%v", env)
	}
	// 未完成事务 → 其他写命令得 13；recover --rollback 收尾。
	w.SetCtl("fail-health-node-0.1.0", false)
	w.SetCtl("fail-health-node-0.2.0", false)
	h.reset()
	if code := h.run("update", "node", "--offline", b2, "--json", "--config", cfg); code != 13 {
		t.Fatalf("存在未完成事务应得 13: %d", code)
	}
	h.reset()
	if code := h.run("recover", "--rollback", "--json", "--config", cfg); code != 0 {
		t.Fatalf("recover: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	d := h.envelope()["data"].(map[string]any)
	if tx := d["transactions"].([]any)[0].(map[string]any); tx["action"] != "rolled_back" {
		t.Fatalf("%v", tx)
	}
}

func TestRollbackCommand(t *testing.T) {
	h, w, cfg := installedNodeHarness(t)
	// 没有可回滚的版本 → 3；未安装 → 14；非 root → 3；非交互缺 --yes → 9。
	if code := h.run("rollback", "node", "--yes", "--json", "--config", cfg); code != 3 || errCode(h.envelope()) != "precondition.no_previous" {
		t.Fatalf("无上一版本: %d %s", code, h.stdout)
	}
	h.reset()
	if code := h.run("update", "node", "--offline", nodeBundle(t, w, 11, "0.2.0"), "--json", "--config", cfg); code != 0 {
		t.Fatalf("%d %s", code, h.stderr)
	}
	h.reset()
	if code := h.run("rollback", "node", "--json", "--config", cfg); code != 9 {
		t.Fatalf("缺确认应 9: %d", code)
	}
	// --dry-run：同源计划、schema、零变化。
	h.reset()
	if code := h.run("rollback", "node", "--dry-run", "--json", "--config", cfg); code != 0 {
		t.Fatalf("%d %s", code, h.stdout)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.rollback.schema.json", env["data"]); err != nil {
		t.Fatalf("%v\n%s", err, h.stdout)
	}
	if env["data"].(map[string]any)["plan"].(map[string]any)["to"] != "0.1.0" {
		t.Fatalf("%s", h.stdout)
	}
	h.reset()
	if code := h.run("rollback", "node", "--yes", "--json", "--config", cfg); code != 0 {
		t.Fatalf("rollback: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	env = h.envelope()
	if err := validateAgainst(t, "data.rollback.schema.json", env["data"]); err != nil {
		t.Fatalf("%v\n%s", err, h.stdout)
	}
	rep := env["data"].(map[string]any)["report"].(map[string]any)
	if rep["version"] != "0.1.0" || rep["health"] != "healthy" {
		t.Fatalf("%v", rep)
	}
	// 含不可逆迁移的上一次升级：拒绝简单回退并提示 --from-backup（退出码 3）。
	h.reset()
	h.run("update", "node", "--offline", nodeBundle(t, w, 12, "0.3.0"), "--json", "--config", cfg)
	if err := w.Registry("x").UpdateState(t.Context(), func(s *registry.State) error {
		c := s.Components["node"]
		c.Upgrade.Irreversible, c.Upgrade.Path, c.Upgrade.Classes = true, registry.PathRestoreBackup, []string{"irreversible"}
		s.Components["node"] = c
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.reset()
	if code := h.run("rollback", "node", "--yes", "--json", "--config", cfg); code != 3 || errCode(h.envelope()) != "precondition.irreversible_rollback" {
		t.Fatalf("不可逆应拒绝: %d %s", code, h.stdout)
	}
	if !strings.Contains(h.envelope()["errors"].([]any)[0].(map[string]any)["hint"].(string), "--from-backup") {
		t.Error("应提示 --from-backup")
	}
	// --from-backup 覆盖数据：--yes 不足，须主机名确认（9）；主机名正确则通过。
	bk := compStateBackup(t, w)
	h.reset()
	if code := h.run("rollback", "node", "--from-backup", bk, "--yes", "--json", "--config", cfg); code != 9 {
		t.Fatalf("from-backup 缺主机名确认应 9: %d %s", code, h.stdout)
	}
	h.reset()
	if code := h.run("rollback", "node", "--from-backup", bk, "--yes", "--confirm-name", "edge-host-1", "--json", "--config", cfg); code != 0 {
		t.Fatalf("from-backup: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
}

func compStateBackup(t *testing.T, w *toolsworld.World) string {
	t.Helper()
	st, _, err := w.Registry("x").LoadState()
	if err != nil {
		t.Fatal(err)
	}
	return st.Components["node"].Upgrade.Backup
}

func TestUpdateAllAndCheckAndVersionCheck(t *testing.T) {
	h, w, cfg := installedNodeHarness(t)
	b2 := nodeBundle(t, w, 11, "0.2.0")
	// update check / version --check：经签名清单（真实验签）列出可升级组件与迁移预告。
	mfv, err := w.Verifier()
	if err != nil {
		t.Fatal(err)
	}
	bodies := bundleParts(t, b2)
	h.env.Ops.FetchManifest = func(ctx context.Context, channel string) (*manifest.Manifest, error) {
		res, err := mfv.Verify(bodies["manifest.json"], bodies["manifest.json.sig"], manifest.VerifyOptions{Channel: channel, Offline: true})
		if err != nil {
			return nil, err
		}
		return res.Manifest, nil
	}
	h.reset()
	if code := h.run("update", "check", "--json", "--config", cfg); code != 0 {
		t.Fatalf("update check: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.update-check.schema.json", env["data"]); err != nil {
		t.Fatalf("%v\n%s", err, h.stdout)
	}
	d := env["data"].(map[string]any)
	if d["upgradable"] != float64(1) {
		t.Fatalf("%s", h.stdout)
	}
	var node map[string]any
	for _, c := range d["components"].([]any) {
		if c.(map[string]any)["component"] == "node" {
			node = c.(map[string]any)
		}
	}
	if node["status"] != "upgrade_available" || node["latest"] != "0.2.0" || node["installed"] != "0.1.0" {
		t.Fatalf("%v", node)
	}
	h.reset()
	if code := h.run("version", "--check", "--json", "--config", cfg); code != 0 {
		t.Fatalf("version --check: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	env = h.envelope()
	if err := validateAgainst(t, "data.version.schema.json", env["data"]); err != nil {
		t.Fatalf("%v\n%s", err, h.stdout)
	}
	if env["data"].(map[string]any)["check"] == nil {
		t.Fatal("version --check 应带 check")
	}
	// --channel 只能与 --check 联用。
	h.reset()
	if code := h.run("version", "--channel", "beta", "--json"); code != 2 {
		t.Fatalf("%d", code)
	}
	// update --all：逐组件升级（openresty 是内部组件，不直接选择；node 升级）。
	h.reset()
	if code := h.run("update", "--all", "--offline", b2, "--json", "--config", cfg); code != 0 {
		t.Fatalf("update --all: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	if err := validateAgainst(t, "data.update.schema.json", h.envelope()["data"]); err != nil {
		t.Fatalf("update --all 的 data 不符合 schema: %v\n%s", err, h.stdout)
	}
	comps := h.envelope()["data"].(map[string]any)["components"].([]any)
	byName := map[string]string{}
	for _, c := range comps {
		byName[c.(map[string]any)["component"].(string)] = c.(map[string]any)["outcome"].(string)
	}
	if byName["node"] != "updated" || byName["openresty"] != "skipped_internal" {
		t.Fatalf("%v", byName)
	}
	_ = service.UnitNode
}

// bundleParts 读出离线 bundle 里的文件（manifest.json 与 .sig）。
func bundleParts(t *testing.T, path string) map[string][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out[h.Name] = body
	}
}
