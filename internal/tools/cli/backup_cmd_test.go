package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.41 的命令层行为（无数据库的 node 路径；真实 MySQL 的 server 路径见 tests/integration/toolsinstall/backup_test.go）。
// 真实命令 + 真实 backup/installer/registry；只有「操作系统」（伪 systemctl/useradd/chown）与组件（toolsworld 的假规格）是假的。

const passText = "a sufficiently long passphrase"

func writeAt(t *testing.T, w *toolsworld.World, rel, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(w.Root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readAt(w *toolsworld.World, rel string) string {
	b, err := os.ReadFile(filepath.Join(w.Root, rel))
	if err != nil {
		return "(missing)"
	}
	return string(b)
}

func passFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(p, []byte(content+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// nodeBackupHarness 安装（假）node 并播种用户数据。
func nodeBackupHarness(t *testing.T) (*harness, *toolsworld.World) {
	t.Helper()
	h, w := installHarness(t)
	h.env.Backup.KDF = backup.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}
	if code := h.run("install", "node", "--offline", w.Bundle, "--json", "--non-interactive"); code != 0 {
		t.Fatalf("install node: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	writeAt(t, w, "etc/lingedge/node/agent.yaml", "server: https://edge.example.test\n", 0o640)
	writeAt(t, w, "var/lib/lingedge-node/generations/1/openresty.conf", "worker_processes auto;\n", 0o644)
	writeAt(t, w, "var/lib/lingedge-node/identity/node.key", "-----PRIVATE NODE KEY MATERIAL-----", 0o600)
	writeAt(t, w, "var/lib/lingedge-node/staging/download.part", "staging junk", 0o644)
	if err := os.Symlink("generations/1", filepath.Join(w.Root, "var/lib/lingedge-node/current")); err != nil {
		t.Fatal(err)
	}
	return h, w
}

func TestBackupCreateListVerifyNode(t *testing.T) {
	h, w := nodeBackupHarness(t)
	if code := h.run("backup", "create", "--component", "node", "--json", "--non-interactive"); code != 0 {
		t.Fatalf("create: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.backup-create.schema.json", env["data"]); err != nil {
		t.Fatalf("data 不符合 schema: %v\n%s", err, h.stdout)
	}
	d := env["data"].(map[string]any)
	path := d["path"].(string)
	if d["notice"] != nil || d["database"] != nil {
		t.Errorf("node 备份没有数据库，也没有主密钥提示: %v", d)
	}
	var dests []string
	for _, f := range d["files"].([]any) {
		dests = append(dests, f.(map[string]any)["dest"].(string))
	}
	joined := strings.Join(dests, "\n")
	for _, want := range []string{"/etc/lingedge/node/agent.yaml", "/var/lib/lingedge-node/generations/1/openresty.conf"} {
		if !strings.Contains(joined, want) {
			t.Errorf("备份应含 %s:\n%s", want, joined)
		}
	}
	for _, not := range []string{"identity", "staging", "current"} {
		if strings.Contains(joined, not) {
			t.Errorf("默认备份不得含 %s（node 身份默认不含私钥，staging 是临时目录，符号链接不进备份）:\n%s", not, joined)
		}
	}
	warned := false
	for _, wn := range env["warnings"].([]any) {
		warned = warned || wn.(map[string]any)["code"] == "backup.symlink_skipped"
	}
	if !warned {
		t.Errorf("跳过符号链接应有警告: %v", env["warnings"])
	}

	// list：不需要口令，列出所需 kek_id（node 为空）。
	if code := h.run("backup", "list", "--json"); code != 0 {
		t.Fatalf("list: %d %s", code, h.stderr)
	}
	env = h.envelope()
	if err := validateAgainst(t, "data.backup-list.schema.json", env["data"]); err != nil {
		t.Fatalf("list data: %v\n%s", err, h.stdout)
	}
	if bs := env["data"].(map[string]any)["backups"].([]any); len(bs) != 1 || bs[0].(map[string]any)["status"] != "ok" {
		t.Fatalf("list: %v", bs)
	}
	// verify。
	if code := h.run("backup", "verify", path, "--json"); code != 0 {
		t.Fatalf("verify: %d %s", code, h.stdout)
	}
	env = h.envelope()
	if err := validateAgainst(t, "data.backup-verify.schema.json", env["data"]); err != nil {
		t.Fatalf("verify data: %v\n%s", err, h.stdout)
	}
	// 人类输出。
	if code := h.run("backup", "verify", path); code != 0 || !strings.Contains(h.stdout.String(), "只读校验") {
		t.Errorf("人类输出: %d %s", code, h.stdout)
	}
	// 篡改后 verify → 5。
	raw, _ := os.ReadFile(path)
	raw[len(raw)/2] ^= 0xff
	bad := filepath.Join(t.TempDir(), filepath.Base(path))
	_ = os.WriteFile(bad, raw, 0o600)
	if code := h.run("backup", "verify", bad, "--json"); code != 5 || errCode(h.envelope()) != "verify.backup_invalid" {
		t.Errorf("篡改应退出码 5: %d %s", code, h.stdout)
	}
	if code := h.run("backup", "verify", filepath.Join(t.TempDir(), "none"), "--json"); code != 2 {
		t.Errorf("文件不存在应为用法错误: %d", code)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("备份文件 0600: %v %v", fi, err)
	}
	_ = w
}

func TestBackupEncryptedNodeIdentityAndPassphraseSources(t *testing.T) {
	h, w := nodeBackupHarness(t)
	pf := passFile(t, passText, 0o400)
	// --include-identity 必须同时 --encrypt。
	if code := h.run("backup", "create", "--component", "node", "--include-identity", "--json", "--non-interactive"); code != 2 || errCode(h.envelope()) != "usage.flag_requires" {
		t.Fatalf("--include-identity 无 --encrypt 应退出码 2: %d %s", code, h.stdout)
	}
	// 非交互 + --encrypt + 没有口令文件 → 3，不写任何文件。
	if code := h.run("backup", "create", "--component", "node", "--encrypt", "--json", "--non-interactive"); code != 3 || errCode(h.envelope()) != "precondition.passphrase_required" {
		t.Fatalf("缺口令应退出码 3: %d %s", code, h.stdout)
	}
	if ents, _ := os.ReadDir(filepath.Join(w.Root, "var/lib/lingedge/backups")); len(ents) != 0 {
		t.Errorf("失败不得留下文件: %v", ents)
	}
	// 口令文件权限过宽 / 过短口令 → 拒绝。
	if code := h.run("backup", "create", "--component", "node", "--encrypt", "--passphrase-file", passFile(t, passText, 0o644), "--json", "--non-interactive"); code != 3 {
		t.Fatalf("口令文件 0644 应被拒绝: %d %s", code, h.stdout)
	}
	if code := h.run("backup", "create", "--component", "node", "--encrypt", "--passphrase-file", passFile(t, "short", 0o400), "--json", "--non-interactive"); code != 2 || errCode(h.envelope()) != "usage.weak_passphrase" {
		t.Fatalf("弱口令: %d %s", code, h.stdout)
	}
	// 加密 + 身份私钥。
	if code := h.run("backup", "create", "--component", "node", "--encrypt", "--include-identity", "--passphrase-file", pf, "--json", "--non-interactive"); code != 0 {
		t.Fatalf("create: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	path := h.envelope()["data"].(map[string]any)["path"].(string)
	if !strings.HasSuffix(path, ".tar.gz.enc") {
		t.Errorf("加密备份后缀: %s", path)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("PRIVATE NODE KEY MATERIAL")) || bytes.Contains(raw, []byte("edge.example.test")) {
		t.Error("加密备份里不得出现明文")
	}
	// 经环境变量通道提供口令文件（recover 等没有标志的路径用它）。
	h.env.Getenv = func(k string) string {
		if k == EnvBackupPassFile {
			return pf
		}
		return ""
	}
	if code := h.run("backup", "verify", path, "--json"); code != 0 {
		t.Fatalf("环境变量通道: %d %s", code, h.stdout)
	}
	if d := h.envelope()["data"].(map[string]any); d["encrypted"] != true {
		t.Errorf("%v", d)
	}
	h.env.Getenv = func(string) string { return "" }
	// 交互：终端无回显读取（创建时输入两次），不一致 → 3。
	h2, _ := nodeBackupHarness(t)
	h2.env.StdinTTY, h2.env.StderrTTY = true, true
	answers := []string{passText, "different passphrase!!"}
	h2.env.Backup.ReadSecret = func(string) (string, error) { a := answers[0]; answers = answers[1:]; return a, nil }
	// 注意：--json 隐含非交互，交互提示只在人类输出模式下可用。
	if code := h2.run("backup", "create", "--component", "node", "--encrypt"); code != 3 || !strings.Contains(h2.stderr.String(), "两次输入的口令不一致") {
		t.Fatalf("两次输入不一致应退出码 3: %d %s", code, h2.stderr)
	}
	answers = []string{passText, passText}
	if code := h2.run("backup", "create", "--component", "node", "--encrypt"); code != 0 {
		t.Fatalf("交互口令: %d\n%s\n%s", code, h2.stdout, h2.stderr)
	}
}

func TestBackupRestoreNode(t *testing.T) {
	h, w := nodeBackupHarness(t)
	pf := passFile(t, passText, 0o400)
	if code := h.run("backup", "create", "--component", "node", "--encrypt", "--include-identity", "--passphrase-file", pf, "--json", "--non-interactive"); code != 0 {
		t.Fatalf("create: %d %s", code, h.stdout)
	}
	path := h.envelope()["data"].(map[string]any)["path"].(string)

	// 备份之后：改配置、删数据、加新文件、身份被换。
	writeAt(t, w, "etc/lingedge/node/agent.yaml", "server: https://changed.example.test\n", 0o600)
	_ = os.Remove(filepath.Join(w.Root, "var/lib/lingedge-node/generations/1/openresty.conf"))
	writeAt(t, w, "var/lib/lingedge-node/generations/2/openresty.conf", "newer generation", 0o644)
	writeAt(t, w, "var/lib/lingedge-node/identity/node.key", "ROTATED", 0o600)

	// --dry-run：完整校验备份并给出计划，零改动。
	before := readAt(w, "etc/lingedge/node/agent.yaml")
	if code := h.run("backup", "restore", path, "--passphrase-file", pf, "--dry-run", "--json", "--non-interactive"); code != 0 {
		t.Fatalf("dry-run: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.backup-restore.schema.json", env["data"]); err != nil {
		t.Fatalf("data 不符合 schema: %v\n%s", err, h.stdout)
	}
	if d := env["data"].(map[string]any); d["outcome"] != "planned" || d["dry_run"] != true || readAt(w, "etc/lingedge/node/agent.yaml") != before {
		t.Fatalf("dry-run 应只给计划: %v", d)
	}
	// 缺确认（非交互无 --yes）→ 9。
	if code := h.run("backup", "restore", path, "--passphrase-file", pf, "--json", "--non-interactive"); code != 9 {
		t.Fatalf("缺确认应退出码 9: %d %s", code, h.stdout)
	}
	// 组件不一致 → 2；口令错 → 5；缺口令 → 3；全程零改动。
	if code := h.run("backup", "restore", path, "--passphrase-file", pf, "--component", "server", "--yes", "--json", "--non-interactive"); code != 2 {
		t.Fatalf("组件不一致应为用法错误: %d %s", code, h.stdout)
	}
	if code := h.run("backup", "restore", path, "--passphrase-file", passFile(t, "wrong wrong wrong wrong", 0o400), "--yes", "--json", "--non-interactive"); code != 5 || errCode(h.envelope()) != "verify.bad_passphrase" {
		t.Fatalf("口令错: %d %s", code, h.stdout)
	}
	if code := h.run("backup", "restore", path, "--yes", "--json", "--non-interactive"); code != 3 || errCode(h.envelope()) != "precondition.passphrase_required" {
		t.Fatalf("缺口令: %d %s", code, h.stdout)
	}
	if readAt(w, "etc/lingedge/node/agent.yaml") != before {
		t.Fatal("拒绝路径不得改动系统")
	}
	// 恢复。
	if code := h.run("backup", "restore", path, "--passphrase-file", pf, "--yes", "--json", "--non-interactive"); code != 0 {
		t.Fatalf("restore: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	env = h.envelope()
	if err := validateAgainst(t, "data.backup-restore.schema.json", env["data"]); err != nil {
		t.Fatalf("data 不符合 schema: %v\n%s", err, h.stdout)
	}
	d := env["data"].(map[string]any)
	if d["outcome"] != "restored" || d["key_check"] != "not_applicable" {
		t.Fatalf("%v", d)
	}
	if got := readAt(w, "etc/lingedge/node/agent.yaml"); got != "server: https://edge.example.test\n" {
		t.Errorf("配置应回到备份时: %q", got)
	}
	if got := readAt(w, "var/lib/lingedge-node/generations/1/openresty.conf"); got != "worker_processes auto;\n" {
		t.Errorf("被删的数据应恢复: %q", got)
	}
	if got := readAt(w, "var/lib/lingedge-node/identity/node.key"); !strings.Contains(got, "PRIVATE NODE KEY MATERIAL") {
		t.Errorf("身份应随 --include-identity 的备份恢复: %q", got)
	}
	if got := readAt(w, "var/lib/lingedge-node/generations/2/openresty.conf"); got != "newer generation" {
		t.Errorf("备份里没有的文件不被删除（恢复只覆盖备份里有的文件）: %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(w.Root, "etc/lingedge/node/agent.yaml")); fi.Mode().Perm() != 0o640 {
		t.Errorf("权限应随备份恢复: %v", fi.Mode().Perm())
	}
	if !w.FakeHas("active-lingedge-node.service") {
		t.Error("node 单元应被重新启动")
	}
	// 安全备份保留且用途为 pre-restore；journal 已归档。
	safety := d["safety_backup"].(string)
	if code := h.run("backup", "verify", safety, "--passphrase-file", pf, "--json"); code != 0 || h.envelope()["data"].(map[string]any)["purpose"] != "pre-restore" {
		t.Errorf("安全备份: %d %s", code, h.stdout)
	}
	if un, _ := w.Registry("x").Unfinished(t.Context()); len(un) != 0 {
		t.Errorf("事务应已提交: %v", un)
	}
	if ents, _ := os.ReadDir(w.Layout.StagingDir()); len(ents) != 0 {
		t.Errorf("暂存区应已清空: %v", ents)
	}
	// node 未安装时：退出码 14。
	h3, w3 := installHarness(t)
	_ = w3
	if code := h3.run("backup", "restore", path, "--passphrase-file", pf, "--yes", "--json", "--non-interactive"); code != 14 {
		t.Errorf("目标组件未安装应退出码 14: %d %s", code, h3.stdout)
	}
}

// 清单被改写成越界的恢复目标（/etc/passwd）：哪怕哈希与结构都对，恢复也在碰系统之前被拒绝。
func TestRestoreRefusesManifestWithEscapingDestination(t *testing.T) {
	h, w := nodeBackupHarness(t)
	f, err := backupFixtureWithDest(t, "/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	before := readAt(w, "etc/lingedge/node/agent.yaml")
	if code := h.run("backup", "restore", f, "--yes", "--json", "--non-interactive"); code != 5 {
		t.Fatalf("越界目标应被拒绝（5）: %d\n%s", code, h.stdout)
	}
	if readAt(w, "etc/lingedge/node/agent.yaml") != before {
		t.Error("不得改动系统")
	}
}

func TestBackupCommandValidation(t *testing.T) {
	h := newHarness(t, asRoot())
	for _, c := range []struct {
		args []string
		want int
	}{
		{[]string{"backup", "create", "--include-master-key"}, 2},
		{[]string{"backup", "create", "--include-migrate-credential"}, 2},
		{[]string{"backup", "create", "--passphrase-file", "/x"}, 2},
		{[]string{"backup", "create", "--component", "node", "--encrypt", "--include-master-key"}, 2},
		{[]string{"backup", "create", "--component", "server", "--encrypt", "--include-identity"}, 2},
		{[]string{"backup", "create", "--db-socket", "/s", "--db-host", "h"}, 2},
		{[]string{"backup", "restore", "f", "--confirm-name", "x"}, 2},
		{[]string{"backup", "restore", "f", "--master-key-file", "/k", "--use-backup-master-key"}, 2},
		{[]string{"backup", "restore", "f", "--master-key-file", "relative"}, 2},
		{[]string{"backup", "restore", "f", "--component", "dns"}, 2},
	} {
		if code := h.run(append(c.args, "--json", "--non-interactive")...); code != c.want {
			t.Errorf("%v: 退出码 %d，期望 %d\n%s", c.args, code, c.want, h.stdout)
		}
	}
	// 本机什么都没装：create 退出码 14。
	if code := h.run("backup", "create", "--json", "--non-interactive"); code != 14 {
		t.Errorf("没有可备份的组件: %d %s", code, h.stdout)
	}
}

func TestUninstallCommand(t *testing.T) {
	h, w := nodeBackupHarness(t)
	// 未安装 → 14（--yes 通过确认之后）。
	if code := h.run("uninstall", "server", "--yes", "--json", "--non-interactive"); code != 14 || errCode(h.envelope()) != "not_installed.component" {
		t.Fatalf("未安装: %d %s", code, h.stdout)
	}
	// --dry-run：计划（保留项与删除项），零改动，不需要确认。
	if code := h.run("uninstall", "node", "--dry-run", "--json", "--non-interactive"); code != 0 {
		t.Fatalf("dry-run: %d %s %s", code, h.stdout, h.stderr)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.uninstall.schema.json", env["data"]); err != nil {
		t.Fatalf("data 不符合 schema: %v\n%s", err, h.stdout)
	}
	if d := env["data"].(map[string]any); d["outcome"] != "planned" || d["dry_run"] != true {
		t.Fatalf("%v", d)
	}
	if !w.FakeHas("active-lingedge-node.service") || readAt(w, "etc/lingedge/node/agent.yaml") == "(missing)" {
		t.Fatal("dry-run 不得改动系统")
	}
	// 缺确认 → 9。
	if code := h.run("uninstall", "node", "--json", "--non-interactive"); code != 9 {
		t.Fatalf("缺确认: %d", code)
	}
	// 默认卸载：保留配置/数据；人类输出说明保留与重装方式。
	if code := h.run("uninstall", "node", "--yes", "--non-interactive"); code != 0 {
		t.Fatalf("uninstall: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "配置、数据与备份已保留") || !strings.Contains(out, "lingedge install node") {
		t.Errorf("人类输出: %s", out)
	}
	if readAt(w, "etc/lingedge/node/agent.yaml") == "(missing)" || readAt(w, "var/lib/lingedge-node/identity/node.key") == "(missing)" {
		t.Error("默认卸载必须保留配置与数据（含身份私钥）")
	}
	if w.FakeHas("active-lingedge-node.service") || w.Has("opt/lingedge/node") {
		t.Error("运行时应已摘除")
	}
	// list 不再有 node。
	if code := h.run("list", "--json"); code != 0 || strings.Contains(h.stdout.String(), `"name":"node"`) {
		t.Errorf("list 不应再含 node: %s", h.stdout)
	}
	// 再次卸载 → 14。
	if code := h.run("uninstall", "node", "--yes", "--json", "--non-interactive"); code != 14 {
		t.Errorf("已卸载: %d", code)
	}
	// 重新安装后 --purge：主机名确认（--yes + 正确主机名）→ 配置、数据与备份全部删除。
	if code := h.run("install", "node", "--offline", w.Bundle, "--json", "--non-interactive", "--yes"); code != 0 {
		t.Fatalf("重装: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	if code := h.run("uninstall", "node", "--purge", "--yes", "--confirm-name", "edge-host-1", "--json", "--non-interactive"); code != 0 {
		t.Fatalf("purge: %d\n%s\n%s", code, h.stdout, h.stderr)
	}
	d := h.envelope()["data"].(map[string]any)
	if err := validateAgainst(t, "data.uninstall.schema.json", d); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"etc/lingedge/node", "var/lib/lingedge-node"} {
		if w.Has(rel) {
			t.Errorf("--purge 应删除 %s", rel)
		}
	}
}
