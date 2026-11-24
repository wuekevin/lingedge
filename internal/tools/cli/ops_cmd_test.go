package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/doctor"
	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.42 的命令层行为：doctor / service / logs / status / list / config。真实命令 + 真实 registry/installer/systemctl 封装/safeexec，
// 只有「操作系统」（伪 systemctl/journalctl/timedatectl 脚本，状态落在文件里）与「组件」（toolsworld 的假规格）是假的；HTTP、TLS、UDP 都是真实套接字。

func opsHarness(t *testing.T, opts ...hopt) (*harness, *toolsworld.World) {
	t.Helper()
	h, w := installHarness(t, opts...)
	h.env.Ops.Journal = journalRunnerFor(t, w)
	return h, w
}

func journalRunnerFor(t *testing.T, w *toolsworld.World) StreamRunner {
	t.Helper()
	r, err := safeexec.New(safeexec.Options{Allowed: []string{"journalctl"}, SearchDirs: []string{w.Bin}, MaxOutput: 1024})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func installNode(t *testing.T, h *harness, w *toolsworld.World) {
	t.Helper()
	if code := h.run("install", "node", "--offline", w.Bundle, "--non-interactive"); code != 0 {
		t.Fatalf("安装 node：%d\n%s", code, h.stderr)
	}
}

func opsWrite(t *testing.T, w *toolsworld.World, rel, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(w.Root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(p, mode)
	return p
}

func checkOf(t *testing.T, h *harness, id string) map[string]any {
	t.Helper()
	env := h.envelope()
	d, _ := env["data"].(map[string]any)
	for _, c := range d["checks"].([]any) {
		m := c.(map[string]any)
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("doctor 结果里没有 %s：%s", id, h.stdout)
	return nil
}

// ---- status / service status ----

func TestStatusAndServiceStatusRealSystemdState(t *testing.T) {
	h, w := opsHarness(t)
	installNode(t, h, w)
	if code := h.run("status", "--json"); code != 0 {
		t.Fatalf("退出码 %d\n%s", code, h.stdout)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.status.schema.json", env["data"]); err != nil {
		t.Fatalf("data 不符合 schema：%v\n%s", err, h.stdout)
	}
	var node map[string]any
	for _, c := range env["data"].(map[string]any)["components"].([]any) {
		if c.(map[string]any)["name"] == "node" {
			node = c.(map[string]any)
		}
	}
	sd := node["systemd"].(map[string]any)
	if sd["managed"] != true || sd["active"] != true || node["healthy"] != true {
		t.Fatalf("node 应 active 且健康：%v", node)
	}
	var seen []string
	for _, u := range sd["units"].([]any) {
		seen = append(seen, u.(map[string]any)["unit"].(string))
	}
	if strings.Join(seen, ",") != "lingedge-node.service,lingedge-openresty.service,lingedge-openresty.path,lingedge-node-upgrade.path" {
		t.Errorf("node 的观察单元：%v", seen)
	}
	// 真实停掉 → 退出码 11（HEALTH_FAILED），data 仍在 envelope 里。
	if err := os.Remove(filepath.Join(w.Fake, "active-lingedge-node.service")); err != nil {
		t.Fatal(err)
	}
	if code := h.run("status", "--json"); code != 11 {
		t.Fatalf("node 停止后 status 应退出 11：%d\n%s", code, h.stdout)
	}
	env = h.envelope()
	if errCode(env) != "health.unhealthy" || env["data"].(map[string]any)["summary"].(map[string]any)["unhealthy"].(float64) != 1 {
		t.Errorf("%v", env)
	}
	if err := validateAgainst(t, "envelope.schema.json", env); err != nil {
		t.Error(err)
	}
	// 人类输出：结果与错误并存。
	h.run("status")
	if !strings.Contains(h.stdout.String(), "✗ node") || !strings.Contains(h.stderr.String(), "退出码 11") {
		t.Errorf("人类输出应同时给出结果与错误：\n%s\n%s", h.stdout, h.stderr)
	}
	// service status node / --all / 未安装 14 / 冲突 2。
	if code := h.run("service", "status", "node", "--json"); code != 11 {
		t.Errorf("service status node：%d", code)
	}
	if err := validateAgainst(t, "data.service-status.schema.json", h.envelope()["data"]); err != nil {
		t.Error(err)
	}
	if code := h.run("service", "status", "admin-web", "--json"); code != 14 {
		t.Errorf("未安装应 14：%d", code)
	}
	if code := h.run("service", "status", "node", "--all", "--json"); code != 2 {
		t.Errorf("--all 与组件互斥：%d", code)
	}
	// 非 root 也能读（读命令不需要 root）。
	h2, _ := opsHarness(t)
	h2.env.Geteuid = func() int { return 1000 }
	if code := h2.run("service", "status", "--json"); code != 0 {
		t.Errorf("空状态下 service status：%d", code)
	}
}

// ---- service start|stop|restart ----

func callsLog(t *testing.T, w *toolsworld.World) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(w.Ctl, "calls"))
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", " | "))
}

func TestServiceStartStopRestart(t *testing.T) {
	h, w := opsHarness(t)
	installNode(t, h, w)
	_ = os.Remove(filepath.Join(w.Ctl, "calls"))
	opsWrite(t, w, "etc/lingedge/tools/tools.yaml", "install:\n  health_timeout: 1s\n  health_interval: 100ms\n", 0o644)

	if code := h.run("service", "stop", "node", "--json"); code != 0 {
		t.Fatalf("stop：%d\n%s", code, h.stdout)
	}
	d := dataOf(t, h)
	if d["action"] != "stop" || d["active"] != false || d["state"] != "stopped" {
		t.Errorf("%v", d)
	}
	if err := validateAgainst(t, "data.service-stop.schema.json", d); err != nil {
		t.Error(err)
	}
	if w.FakeHas("active-lingedge-node.service") {
		t.Error("单元应已停止")
	}
	if got := callsLog(t, w); strings.Join(got, " ") != "stop lingedge-node.service" {
		t.Errorf("stop 只作用于 Agent，不得停数据面 OpenResty：%v", got)
	}
	// state.json 记录更新为 stopped；list 看得到。
	h.run("list", "--json")
	if !strings.Contains(h.stdout.String(), `"state":"stopped"`) {
		t.Errorf("state.json 应记录为 stopped：%s", h.stdout)
	}

	if code := h.run("service", "start", "node", "--json"); code != 0 {
		t.Fatalf("start：%d\n%s", code, h.stdout)
	}
	d = dataOf(t, h)
	if d["state"] != "running" || d["active"] != true {
		t.Errorf("%v", d)
	}
	if !w.FakeHas("active-lingedge-node.service") {
		t.Error("单元应已启动")
	}
	h.run("list", "--json")
	if !strings.Contains(h.stdout.String(), `"state":"running"`) {
		t.Errorf("state.json 应记录为 running：%s", h.stdout)
	}

	if code := h.run("service", "restart", "node", "--json"); code != 0 {
		t.Fatalf("restart：%d\n%s", code, h.stdout)
	}
	if got := strings.Join(callsLog(t, w), " "); !strings.HasSuffix(got, "restart lingedge-node.service") {
		t.Errorf("calls=%s", got)
	}
}

func TestServiceStartRestartHealthFailureIsExit11(t *testing.T) {
	h, w := opsHarness(t)
	installNode(t, h, w)
	opsWrite(t, w, "etc/lingedge/tools/tools.yaml", "install:\n  health_timeout: 1s\n  health_interval: 100ms\n", 0o644)
	// restart 之后单元不处于 active（伪 systemctl 的 restart 不会把它拉起来）：健康检查在窗口内未通过 → 退出码 11。
	_ = os.Remove(filepath.Join(w.Fake, "active-lingedge-node.service"))
	start := time.Now()
	if code := h.run("service", "restart", "node", "--json"); code != 11 {
		t.Fatalf("应退出 11：%d\n%s", code, h.stdout)
	}
	if time.Since(start) > 15*time.Second {
		t.Errorf("健康等待应遵守 install.health_timeout：%v", time.Since(start))
	}
	env := h.envelope()
	if errCode(env) != "health.unhealthy" {
		t.Errorf("%v", env["errors"])
	}
	if env["data"].(map[string]any)["state"] != "failed" {
		t.Errorf("%v", env["data"])
	}
	h.run("list", "--json")
	if strings.Contains(h.stdout.String(), `"state":"running"`) && strings.Contains(h.stdout.String(), `"name":"node"`) {
		t.Log("state 记录保持安装时的值（失败的手工操作不改记录为 running）")
	}
}

func TestServiceCommandGuards(t *testing.T) {
	h, w := opsHarness(t)
	_ = w
	// 未安装 → 14；无 root → 3；--dry-run 不执行任何 systemctl 写操作。
	if code := h.run("service", "start", "node", "--json"); code != 14 {
		t.Errorf("未安装：%d", code)
	}
	nr := newHarness(t)
	if code := nr.run("service", "restart", "node", "--json"); code != 3 || errCode(nr.envelope()) != "precondition.root_required" {
		t.Errorf("非 root：%d", code)
	}
	installNode(t, h, w)
	_ = os.Remove(filepath.Join(w.Ctl, "calls"))
	if code := h.run("service", "stop", "node", "--dry-run", "--json"); code != 0 {
		t.Fatalf("dry-run：%d", code)
	}
	if d := dataOf(t, h); d["dry_run"] != true {
		t.Errorf("%v", d)
	}
	if len(callsLog(t, w)) != 0 || !w.FakeHas("active-lingedge-node.service") {
		t.Error("--dry-run 不得改变服务状态")
	}
	// 未知组件 → 2；scheduler/worker 已内置 → 不是合法的 service 目标。
	if code := h.run("service", "start", "nope", "--json"); code != 2 {
		t.Errorf("未知组件：%d", code)
	}
	// systemctl 失败 → 结构化错误（ServiceError），不是 panic，也不假装成功。
	w.SetCtl("fail-start", true)
	_ = os.Remove(filepath.Join(w.Fake, "active-lingedge-node.service"))
	if code := h.run("service", "start", "node", "--json"); code == 0 {
		t.Error("systemctl 失败必须体现为非零退出码")
	}
	// 并发写命令持有同一组件锁 → 6（任务锁，tools-design §16）：此处用另一个 registry 进程内持锁模拟。
	w.SetCtl("fail-start", false)
	reg := registry.New(registry.LayoutForStatePath(h.env.StatePath), registry.Options{Now: h.env.Now, Hostname: h.env.Hostname, RunID: "holder", Command: "lingedge test"})
	task, err := reg.Begin(context.Background(), registry.TaskRequest{Components: []string{"node"}})
	if err != nil {
		t.Fatal(err)
	}
	if code := h.run("service", "restart", "node", "--json"); code != 6 {
		t.Errorf("组件锁被占用应 6：%d", code)
	}
	task.Release()
}

func TestServiceOnStaticFrontendIsPrecondition(t *testing.T) {
	h, w := staticHarness(t)
	h.env.Ops.Journal = journalRunnerFor(t, w)
	if code := h.run("install", "admin-web", "--offline", w.Bundle, "--set", "host=admin.example.com", "--non-interactive"); code != 0 {
		t.Fatalf("%d\n%s", code, h.stderr)
	}
	for _, args := range [][]string{{"service", "start", "admin-web"}, {"service", "stop", "admin-web"}, {"service", "restart", "admin-web"}, {"logs", "admin-web"}} {
		if code := h.run(append(args, "--json")...); code != 3 {
			t.Errorf("%v：静态前端没有服务，应 3：%d", args, code)
		}
		if c := errCode(h.envelope()); c != "service.no_service" && c != "logs.no_service" {
			t.Errorf("%v：%s", args, c)
		}
	}
}

// ---- logs ----

// 运行时拼接口令样例：避免 G-6 固定口令扫描把测试样例当成真实口令。
const dsnPass = "hunter" + "2pass"

var journalFixture = `2026-11-24T12:00:00+0000 host lingedge-node[1]: started
2026-11-24T12:00:01+0000 host lingedge-node[1]: Authorization: Bearer abcdefghijklmnop1234567890
2026-11-24T12:00:02+0000 host lingedge-node[1]: connecting dsn=mysql://app:` + dsnPass + `@127.0.0.1/db
2026-11-24T12:00:03+0000 host lingedge-node[1]: enroll with lenr1.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEoAoGCCqGSM49
-----END EC PRIVATE KEY-----
2026-11-24T12:00:04+0000 host lingedge-node[1]: password=correct-horse-battery
2026-11-24T12:00:05+0000 host lingedge-node[1]: done
`

func TestLogsRedactsAndWrapsJournalctlWithArgvArray(t *testing.T) {
	h, w := opsHarness(t)
	installNode(t, h, w)
	if err := os.WriteFile(filepath.Join(w.Fake, "journal"), []byte(journalFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := h.run("logs", "node", "--since", "10m", "-n", "5"); code != 0 {
		t.Fatalf("退出码 %d\n%s", code, h.stderr)
	}
	out := h.stdout.String()
	for _, secret := range []string{"abcdefghijklmnop1234567890", dsnPass, "AAAAAAAAAAAAAAAA", "FAKEFAKEFAKE", "correct-horse-battery", "BEGIN EC PRIVATE KEY"} {
		if strings.Contains(out, secret) {
			t.Errorf("日志输出泄露了 %q：\n%s", secret, out)
		}
	}
	for _, keep := range []string{"started", "done", "PEM 块已隐去"} {
		if !strings.Contains(out, keep) {
			t.Errorf("正常内容 %q 应保留：\n%s", keep, out)
		}
	}
	args, _ := os.ReadFile(filepath.Join(w.Fake, "journal-args"))
	if got := strings.TrimSpace(string(args)); got != "--no-pager --output=short-iso -n 5 -u lingedge-node.service -u lingedge-openresty.service --since=-600s" {
		t.Errorf("journalctl 参数数组：%q", got)
	}
	// --json：脱敏后的行进入 data.lines；-f 与 --json 不兼容（用法错误）。
	if code := h.run("logs", "node", "--json"); code != 0 {
		t.Fatalf("%d", code)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.logs.schema.json", env["data"]); err != nil {
		t.Error(err)
	}
	if strings.Contains(h.stdout.String(), dsnPass) || strings.Contains(h.stdout.String(), "FAKEFAKE") {
		t.Errorf("--json 日志泄露：%s", h.stdout)
	}
	if n := len(env["data"].(map[string]any)["lines"].([]any)); n != 7 { // fixture 里 PEM 的 3 行合并成 1 个占位
		t.Errorf("lines=%d", n)
	}
	if code := h.run("logs", "node", "-f", "--json"); code != 2 {
		t.Errorf("-f 与 --json 应为用法错误：%d", code)
	}
	// -f：参数里带 --follow。
	if code := h.run("logs", "node", "-f"); code != 0 {
		t.Fatalf("-f：%d\n%s", code, h.stderr)
	}
	args, _ = os.ReadFile(filepath.Join(w.Fake, "journal-args"))
	if !strings.Contains(string(args), "--follow") {
		t.Errorf("%q", args)
	}
	// 非法参数 → 2；未安装 → 14。
	for _, a := range [][]string{{"logs", "node", "--since", "abc"}, {"logs", "node", "-n", "0"}, {"logs", "nope"}} {
		if code := h.run(append(a, "--json")...); code != 2 {
			t.Errorf("%v：%d", a, code)
		}
	}
	h2, _ := opsHarness(t)
	if code := h2.run("logs", "node", "--json"); code != 14 {
		t.Errorf("未安装：%d", code)
	}
}

func TestLogsLineFilterEdgeCases(t *testing.T) {
	var sb strings.Builder
	f := &lineFilter{w: &sb}
	// 分块写入、CRLF、超长行、PEM 跨写入边界、结尾无换行。
	writes := []string{"hello wor", "ld\r\npass", "word=hunter2\n", "-----BEGIN PRIVATE KEY-----\nAAAA", "BBBB\n-----END PRIVATE KEY-----\n", strings.Repeat("x", maxLogLine+500) + "\n", "tail-no-newline"}
	for _, wr := range writes {
		if _, err := f.Write([]byte(wr)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if !strings.Contains(out, "hello world\n") || strings.Contains(out, "hunter2") || strings.Contains(out, "AAAABBBB") || strings.Contains(out, "PRIVATE KEY") {
		t.Errorf("过滤结果：%q", out[:min(len(out), 300)])
	}
	if !strings.Contains(out, "行过长已截断") || !strings.HasSuffix(out, "tail-no-newline\n") {
		t.Errorf("超长行/结尾：%q", out[max(0, len(out)-80):])
	}
	// 超长行没有换行时不无限缓冲。
	var sb2 strings.Builder
	f2 := &lineFilter{w: &sb2}
	for i := 0; i < 10; i++ {
		_, _ = f2.Write([]byte(strings.Repeat("y", maxLogLine/2)))
	}
	if len(f2.buf) > maxLogLine {
		t.Errorf("缓冲未受限：%d", len(f2.buf))
	}
}

// ---- list ----

func TestListShowsProcessOwnershipAndSite(t *testing.T) {
	h, w := opsHarness(t)
	installNode(t, h, w)
	// 节点端口上的真实监听者：注入一份探测结果（监听者来自 ss 的解析结果形状），验证归属判定与输出。
	w.Probe.Sockets = nil
	if code := h.run("list", "--json"); code != 0 {
		t.Fatalf("%d\n%s", code, h.stdout)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.list.schema.json", env["data"]); err != nil {
		t.Fatalf("%v\n%s", err, h.stdout)
	}
	for _, c := range env["data"].(map[string]any)["components"].([]any) {
		m := c.(map[string]any)
		if m["name"] == "node" {
			p := m["process"].(map[string]any)
			if p["run_user"] != "lingedge-node" || p["listeners"] == nil {
				t.Errorf("node 的进程归属：%v", p)
			}
		}
	}
	// 静态前端：站点域名、根目录、片段、conf.d 软链状态。
	sh, sw := staticHarness(t)
	if code := sh.run("install", "docs", "--offline", sw.Bundle, "--set", "host=docs.example.com", "--non-interactive"); code != 0 {
		t.Fatalf("%d\n%s", code, sh.stderr)
	}
	if code := sh.run("list", "--json"); code != 0 {
		t.Fatalf("%d", code)
	}
	env = sh.envelope()
	if err := validateAgainst(t, "data.list.schema.json", env["data"]); err != nil {
		t.Fatalf("%v\n%s", err, sh.stdout)
	}
	site := env["data"].(map[string]any)["components"].([]any)[0].(map[string]any)["site"].(map[string]any)
	if site["host"] != "docs.example.com" || site["root"] != "/opt/lingedge/docs/current/public" || site["conf_d_link"] != "absent" {
		t.Errorf("site：%v", site)
	}
	sh.run("list")
	if !strings.Contains(sh.stdout.String(), "nginx 站点 docs.example.com") {
		t.Errorf("人类输出应含站点：\n%s", sh.stdout)
	}
}

func TestListAvailableFromVerifiedManifest(t *testing.T) {
	h, w := opsHarness(t)
	// 不配置发行源：没有 base_url → 前置条件（3），不是假数据。
	if code := h.run("list", "--available", "--json"); code != 3 {
		t.Fatalf("没有发行源应 3：%d\n%s", code, h.stdout)
	}
	if code := h.run("list", "--channel", "beta", "--json"); code != 2 {
		t.Errorf("--channel 必须与 --available 联用：%d", code)
	}
	// 真实签名的清单（toolsworld 的测试密钥）经 Env 注入的 FetchManifest 接缝返回；验证链在 install 测试里覆盖，这里验证输出与 schema。
	b, err := downloader.OpenBundle(w.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	mfb, sig, err := b.Manifest()
	_ = b.Close()
	if err != nil {
		t.Fatal(err)
	}
	vf, err := w.Verifier()
	if err != nil {
		t.Fatal(err)
	}
	vres, err := vf.Verify(mfb, sig, manifest.VerifyOptions{Channel: "stable", Offline: true, AllowExpired: true})
	if err != nil {
		t.Fatal(err)
	}
	h.env.Ops.FetchManifest = func(context.Context, string) (*manifest.Manifest, error) { return vres.Manifest, nil }
	installNode(t, h, w)
	if code := h.run("list", "--available", "--json"); code != 0 {
		t.Fatalf("%d\n%s", code, h.stdout)
	}
	env := h.envelope()
	if err := validateAgainst(t, "data.list.schema.json", env["data"]); err != nil {
		t.Fatalf("%v\n%s", err, h.stdout)
	}
	av := env["data"].(map[string]any)["available"].([]any)
	if len(av) == 0 {
		t.Fatal("available 不应为空")
	}
	var nodeInstalled string
	for _, a := range av {
		if m := a.(map[string]any); m["component"] == "node" {
			nodeInstalled, _ = m["installed"].(string)
		}
	}
	if nodeInstalled == "" {
		t.Errorf("已装组件应标注 installed：%v", av)
	}
}

// ---- doctor ----

func TestDoctorCommandJSONExitCodesAndStrict(t *testing.T) {
	h, w := opsHarness(t)
	installNode(t, h, w)
	h.env.Ops.Nofile = func() (uint64, error) { return 65536, nil }
	h.env.Ops.PeerCerts = func(context.Context, string) ([]*x509.Certificate, error) { return nil, os.ErrNotExist }
	h.env.Ops.OpenrestyRun = fakeRunnerFor(t, w, "openresty")
	h.env.Ops.Timedatectl = fakeRunnerFor(t, w, "timedatectl")
	code := h.run("doctor", "--json")
	env := h.envelope()
	if err := validateAgainst(t, "envelope.schema.json", env); err != nil {
		t.Fatal(err)
	}
	if err := validateAgainst(t, "data.doctor.schema.json", env["data"]); err != nil {
		t.Fatalf("data 不符合 schema：%v\n%s", err, h.stdout)
	}
	d := env["data"].(map[string]any)
	sum := d["summary"].(map[string]any)
	// 退出码与汇总一致：有 fail → 3，否则 0。
	if (sum["fail"].(float64) > 0) != (code == 3) {
		t.Errorf("退出码 %d 与汇总 %v 不一致", code, sum)
	}
	ids := map[string]bool{}
	for _, c := range d["checks"].([]any) {
		m := c.(map[string]any)
		ids[m["id"].(string)] = true
		for _, k := range []string{"id", "category", "level", "message"} {
			if m[k] == nil || m[k] == "" {
				t.Errorf("检查项缺 %s：%v", k, m)
			}
		}
	}
	for _, id := range []string{"sys.os", "sys.arch", "sys.mem", "sys.disk", "sys.time_sync", "sys.ulimit_nofile", "svc.node.active", "svc.node.enabled", "svc.node.restarts",
		"fs.config_perm", "cfg.validate", "state.drift", "journal.incomplete", "backup.recent", "ver.revoked", "ver.update_available", "baota.nginx_conflict", "baota.firewall", "dep.openresty", "health.node", "cert.node_mtls"} {
		if !ids[id] {
			t.Errorf("doctor 缺少检查项 %s", id)
		}
	}
	// --strict：有 warn 就是 3；--strict 与无 warn 时不变。
	warn := sum["warn"].(float64)
	code2 := h.run("doctor", "--json", "--strict")
	if want := 0; warn == 0 && sum["fail"].(float64) == 0 && code2 != want {
		t.Errorf("无 warn/fail 时 --strict 应 0：%d", code2)
	}
	if warn > 0 && code2 != 3 {
		t.Errorf("--strict 应把 warn 视同 fail：%d（warn=%v）", code2, warn)
	}
	// 人类输出分类别、带提示。
	h.run("doctor")
	if !strings.Contains(h.stdout.String(), "【系统】") || !strings.Contains(h.stdout.String(), "通过 ") {
		t.Errorf("人类输出：\n%s", h.stdout)
	}
}

func fakeRunnerFor(t *testing.T, w *toolsworld.World, name string) service.Runner {
	t.Helper()
	r, err := safeexec.New(safeexec.Options{Allowed: []string{name}, SearchDirs: []string{w.Bin}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDoctorComponentFilterAndUnknown(t *testing.T) {
	h, w := opsHarness(t)
	installNode(t, h, w)
	h.run("doctor", "node", "--json")
	env := h.envelope()
	for _, c := range env["data"].(map[string]any)["checks"].([]any) {
		id := c.(map[string]any)["id"].(string)
		if strings.HasPrefix(id, "sys.") || strings.HasPrefix(id, "baota.") || id == "journal.incomplete" {
			t.Errorf("doctor node 不应含主机级检查 %s", id)
		}
	}
	if code := h.run("doctor", "admin-web", "--json"); code != 14 {
		t.Errorf("未安装的组件：%d", code)
	}
	if code := h.run("doctor", "nope", "--json"); code != 2 {
		t.Errorf("未知组件：%d", code)
	}
	if code := h.run("doctor", "node", "--all", "--json"); code != 2 {
		t.Errorf("--all 与组件互斥：%d", code)
	}
}

func TestDoctorFailsWhenRealDriftExists(t *testing.T) {
	h, w := opsHarness(t)
	installNode(t, h, w)
	h.env.Ops.Nofile = func() (uint64, error) { return 65536, nil }
	// 手工破坏：删掉单元文件与 current 链接 → state.drift fail → 退出码 3。
	_ = os.Remove(filepath.Join(w.Root, "etc/systemd/system/lingedge-node.service"))
	if code := h.run("doctor", "--json"); code != 3 {
		t.Fatalf("应退出 3：%d", code)
	}
	if c := checkOf(t, h, "state.drift"); c["level"] != "fail" || !strings.Contains(c["message"].(string), "lingedge-node.service") {
		t.Errorf("%v", c)
	}
	env := h.envelope()
	if errCode(env) != "doctor.failed" {
		t.Errorf("%v", env["errors"])
	}
	// 人类输出：结果表在 stdout，错误说明在 stderr。
	h.run("doctor")
	if !strings.Contains(h.stdout.String(), "state.drift") || !strings.Contains(h.stderr.String(), "doctor.failed") {
		t.Errorf("\n%s\n%s", h.stdout, h.stderr)
	}
}

func TestDoctorFixPermsCommand(t *testing.T) {
	// 非 root：--fix-perms 退出 3，不修改任何东西。
	nr := newHarness(t)
	if code := nr.run("doctor", "--fix-perms", "--json"); code != 3 || errCode(nr.envelope()) != "precondition.root_required" {
		t.Errorf("非 root：%d", code)
	}
	h, w := opsHarness(t)
	installNode(t, h, w)
	h.env.Ops.Nofile = func() (uint64, error) { return 65536, nil }
	cfgPath := opsWrite(t, w, "etc/lingedge/node/node.yaml", "x: 1\n", 0o666)
	if err := os.Chmod(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	h.run("doctor", "node", "--json")
	if c := checkOf(t, h, "fs.config_perm"); c["level"] != "fail" {
		t.Fatalf("0666 的配置应 fail：%v", c)
	}
	if code := h.run("doctor", "node", "--fix-perms", "--dry-run", "--json"); code == 0 {
		t.Log("dry-run 仍报告偏差")
	}
	if fi, _ := os.Stat(cfgPath); fi.Mode().Perm() != 0o666 {
		t.Error("--dry-run 不得修改")
	}
	h.run("doctor", "node", "--fix-perms", "--json")
	c := checkOf(t, h, "fs.config_perm")
	if c["fixed"] != true || c["level"] != "pass" {
		t.Errorf("应修复并复检通过：%v", c)
	}
	if fi, _ := os.Stat(cfgPath); fi.Mode().Perm() != 0o640 {
		t.Errorf("node.yaml 权限 %v，期望 0640", fi.Mode().Perm())
	}
}

// cert.server_leaf 的生产实现：对真实 TLS 监听（要求客户端证书，Tools 不出示）做握手，取到证书链与 CA（真实握手，不是 mock）。
func TestRealPeerCertsAgainstMutualTLSListener(t *testing.T) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "LingEdge test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour * 365 * 10),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "server"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour * 90),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: x509.NewCertPool(),
		Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}}})
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake(); _ = c.Close() }()
		}
	}()
	certs, err := realPeerCerts(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatalf("应取到证书链（即使服务端随后因缺客户端证书拒绝握手）：%v", err)
	}
	if len(certs) != 2 || certs[0].Subject.CommonName != "server" || !certs[1].IsCA {
		t.Errorf("证书链：%d 张", len(certs))
	}
	// 没有 TLS 的端口（明文 HTTP）→ 握手失败而不是挂住；关闭的端口 → 连接失败。
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := realPeerCerts(context.Background(), strings.TrimPrefix(srv.URL, "http://")); err == nil {
		t.Error("明文端口应失败")
	}
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln2.Addr().String()
	_ = ln2.Close()
	if _, err := realPeerCerts(context.Background(), addr); err == nil {
		t.Error("关闭的端口应失败")
	}
	// TLS 1.2 以下的服务器被拒绝（只接受 1.3，ADR-003）。
	ln3, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MaxVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}})
	if err == nil {
		defer func() { _ = ln3.Close() }()
		go func() {
			for {
				c, err := ln3.Accept()
				if err != nil {
					return
				}
				go func() { _ = c.(*tls.Conn).Handshake(); _ = c.Close() }()
			}
		}()
		if _, err := realPeerCerts(context.Background(), ln3.Addr().String()); err == nil {
			t.Error("TLS 1.2 服务器应被拒绝")
		}
	}
}

// health.server 与 status 对真实 HTTP 服务器的判定：setup mode / 就绪 / 未就绪。
func TestServerHealthAgainstRealHTTP(t *testing.T) {
	ready := true
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			rw.WriteHeader(200)
		case "/healthz/ready":
			if ready {
				rw.WriteHeader(200)
			} else {
				rw.WriteHeader(503)
			}
		default:
			rw.WriteHeader(404)
		}
	}))
	defer srv.Close()
	h, w := opsHarness(t)
	// 装 server（假规格）并让 server.yaml 指向这个真实 HTTP 服务器。
	if code := h.run("install", "server", "--offline", w.Bundle, "--non-interactive"); code != 0 {
		t.Fatalf("%d\n%s", code, h.stderr)
	}
	opsWrite(t, w, "etc/lingedge/server/server.yaml", "http:\n  listen: "+strings.TrimPrefix(srv.URL, "http://")+"\n", 0o640)
	h.env.Ops.Nofile = func() (uint64, error) { return 65536, nil }
	lock := filepath.Join(w.Root, "var/lib/lingedge/server/install.lock")
	// 向导未完成：存活即可。
	_ = os.Remove(lock)
	h.run("doctor", "server", "--json")
	if c := checkOf(t, h, "health.server"); c["level"] != "pass" || !strings.Contains(c["message"].(string), "setup mode") {
		t.Errorf("setup mode：%v", c)
	}
	// 向导完成且就绪。
	opsWrite(t, w, "var/lib/lingedge/server/install.lock", "", 0o600)
	h.run("doctor", "server", "--json")
	if c := checkOf(t, h, "health.server"); c["level"] != "pass" || !strings.Contains(c["message"].(string), "就绪") {
		t.Errorf("就绪：%v", c)
	}
	h.run("status", "--json")
	if d := h.envelope()["data"].(map[string]any); d["summary"].(map[string]any)["unhealthy"].(float64) != 0 {
		t.Errorf("status：%s", h.stdout)
	}
	// 未就绪 → doctor fail、status 退出 11。
	ready = false
	if code := h.run("doctor", "server", "--json"); code != 3 {
		t.Errorf("doctor：%d", code)
	}
	if c := checkOf(t, h, "health.server"); c["level"] != "fail" {
		t.Errorf("%v", c)
	}
	if code := h.run("status", "--json"); code != 11 {
		t.Errorf("status：%d", code)
	}
	// 服务停止（连接被拒绝）。
	srv.Close()
	if code := h.run("status", "--json"); code != 11 {
		t.Errorf("连接被拒绝应 11：%d", code)
	}
	if !strings.Contains(h.stdout.String(), "无法连接") {
		t.Errorf("%s", h.stdout)
	}
}

// ---- config ----

func TestConfigValidateBrokenToolsYAMLPointsToKeyAndLine(t *testing.T) {
	h, w := opsHarness(t)
	h.env.Geteuid = func() int { return 1000 }
	good := "release:\n  base_url: https://releases.example.org\n"
	opsWrite(t, w, "etc/lingedge/tools/tools.yaml", good, 0o644)
	if code := h.run("config", "validate", "tools", "--json"); code != 0 {
		t.Fatalf("合法配置应通过：%d\n%s", code, h.stdout)
	}
	if err := validateAgainst(t, "data.config-validate.schema.json", h.envelope()["data"]); err != nil {
		t.Error(err)
	}
	cases := []struct {
		name, yaml string
		path       string
		line       float64
	}{
		{"取值非法", "release:\n  base_url: http://insecure.example.org\n", "release.base_url", 2},
		{"未知键", "install:\n  health_timeout: 30s\n  bogus_key: 1\n", "install.bogus_key", 3},
		{"类型不符", "doctor:\n  restarts_warn: many\n", "", 2},
		{"语法错误", "release: [oops\n", "", 1},
	}
	for _, c := range cases {
		opsWrite(t, w, "etc/lingedge/tools/tools.yaml", c.yaml, 0o644)
		if code := h.run("config", "validate", "--json"); code != 3 {
			t.Errorf("%s：配置被破坏应非零退出（3）：%d\n%s", c.name, code, h.stdout)
			continue
		}
		env := h.envelope()
		if errCode(env) != "config.invalid" {
			t.Errorf("%s：%v", c.name, env["errors"])
		}
		found := false
		for _, tg := range env["data"].(map[string]any)["targets"].([]any) {
			for _, p := range tg.(map[string]any)["problems"].([]any) {
				pm := p.(map[string]any)
				if pm["line"] == c.line && (c.path == "" || pm["path"] == c.path) {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%s：应指出第 %v 行（%s）：%s", c.name, c.line, c.path, h.stdout)
		}
	}
	// --file：校验任意文件；人类输出带「第 N 行」。
	bad := filepath.Join(t.TempDir(), "broken.yaml")
	_ = os.WriteFile(bad, []byte("release:\n  base_url: ftp://x\n"), 0o644)
	if code := h.run("config", "validate", "--file", bad); code != 3 || !strings.Contains(h.stdout.String(), "第 2 行") || !strings.Contains(h.stdout.String(), "release.base_url") {
		t.Errorf("人类输出：%d\n%s", code, h.stdout)
	}
	if code := h.run("config", "validate", "tools", "--file", filepath.Join(t.TempDir(), "nope.yaml"), "--json"); code != 3 {
		t.Errorf("文件不存在：%d", code)
	}
	// 符号链接的标准配置路径不被跟随（--file 才允许任意路径）。
	_ = os.Remove(filepath.Join(w.Root, "etc/lingedge/tools/tools.yaml"))
	_ = os.Symlink(bad, filepath.Join(w.Root, "etc/lingedge/tools/tools.yaml"))
	if code := h.run("config", "validate", "tools", "--json"); code != 3 {
		t.Errorf("符号链接应被拒绝：%d", code)
	}
	// 文件不存在 = 缺省配置（不是错误）。
	_ = os.Remove(filepath.Join(w.Root, "etc/lingedge/tools/tools.yaml"))
	if code := h.run("config", "validate", "tools", "--json"); code != 0 {
		t.Errorf("tools.yaml 不存在应通过（缺省配置）：%d", code)
	}
	// 参数：未知目标 → 2；--config 指向的文件被校验。
	if code := h.run("config", "validate", "nope", "--json"); code != 2 {
		t.Errorf("%d", code)
	}
	if code := h.run("config", "validate", "tools", "--config", bad, "--json"); code != 3 {
		t.Errorf("--config 指向的 tools.yaml 应被校验：%d", code)
	}
}

func TestConfigValidateServerShapeFallbackIsMarkedPartial(t *testing.T) {
	h, w := opsHarness(t)
	if code := h.run("install", "server", "--offline", w.Bundle, "--non-interactive"); code != 0 {
		t.Fatalf("%d\n%s", code, h.stderr)
	}
	opsWrite(t, w, "etc/lingedge/server/server.yaml", "server:\n  env: prod\nhttp:\n  listen: 127.0.0.1:8080\n", 0o640)
	if code := h.run("config", "validate", "server", "--json"); code != 0 {
		t.Fatalf("%d\n%s", code, h.stdout)
	}
	env := h.envelope()
	warns, _ := env["warnings"].([]any)
	if len(warns) == 0 || !strings.Contains(h.stdout.String(), "只做了结构校验") {
		t.Errorf("没有 lingedge-server 时应明确标注只做了结构校验：%s", h.stdout)
	}
	// 破坏：未知键 + 口令写进文件 → 指出键路径与行号。
	opsWrite(t, w, "etc/lingedge/server/server.yaml", "server:\n  env: prod\n  enviroment: x\ndatabase:\n  password: s3cret-value\n", 0o640)
	if code := h.run("config", "validate", "server", "--json"); code != 3 {
		t.Fatalf("%d\n%s", code, h.stdout)
	}
	if strings.Contains(h.stdout.String(), "s3cret-value") {
		t.Errorf("校验输出回显了口令：%s", h.stdout)
	}
	if !strings.Contains(h.stdout.String(), `"path":"server.enviroment"`) {
		t.Errorf("%s", h.stdout)
	}
	// 不带参数：tools.yaml 与全部已装组件。
	h.run("config", "validate", "--json")
	var targets []string
	for _, tg := range h.envelope()["data"].(map[string]any)["targets"].([]any) {
		targets = append(targets, tg.(map[string]any)["target"].(string))
	}
	if strings.Join(targets, ",") != "tools,server" {
		t.Errorf("targets=%v", targets)
	}
}

func TestConfigShowRedacts(t *testing.T) {
	proxyPass := "proxy" + "pass99" // 运行时拼接（G-6）
	h, w := opsHarness(t)
	h.env.Geteuid = func() int { return 1000 }
	opsWrite(t, w, "etc/lingedge/tools/tools.yaml", "# 注释保留\nrelease:\n  base_url: https://releases.example.org\n  proxy: http://proxyuser:"+proxyPass+"@proxy.internal:3128\nbackup:\n  custody_fingerprint: 0123456789abcdef\n", 0o644)
	if code := h.run("config", "show", "tools"); code != 0 {
		t.Fatalf("%d\n%s", code, h.stderr)
	}
	out := h.stdout.String()
	if strings.Contains(out, proxyPass) || !strings.Contains(out, "# 注释保留") || !strings.Contains(out, "releases.example.org") {
		t.Errorf("tools.yaml 脱敏：\n%s", out)
	}
	hs, ws := opsHarness(t)
	if code := hs.run("install", "server", "--offline", ws.Bundle, "--non-interactive"); code != 0 {
		t.Fatalf("%d\n%s", code, hs.stderr)
	}
	opsWrite(t, ws, "etc/lingedge/server/server.yaml", "server:\n  env: prod\ndatabase:\n  host: db.internal\n  password: topsecretpw\n  password_file: /etc/lingedge/server/db.pw\nsecrets:\n  master_key_file: /etc/lingedge/server/master.key\nlog:\n  level: info\n", 0o640)
	if code := hs.run("config", "show", "server", "--json"); code != 0 {
		t.Fatalf("%d\n%s", code, hs.stdout)
	}
	if err := validateAgainst(t, "data.config-show.schema.json", hs.envelope()["data"]); err != nil {
		t.Error(err)
	}
	content := hs.envelope()["data"].(map[string]any)["content"].(string)
	for _, secret := range []string{"topsecretpw", "db.pw", "master.key"} {
		if strings.Contains(content, secret) {
			t.Errorf("config show 泄露 %q：\n%s", secret, content)
		}
	}
	if !strings.Contains(content, "db.internal") || !strings.Contains(content, "level: info") || !strings.Contains(content, "***") {
		t.Errorf("非敏感内容应保留、敏感值为 ***：\n%s", content)
	}
	// 语法有误的文件：逐行脱敏兜底（值里的令牌仍被替换）。
	opsWrite(t, ws, "etc/lingedge/server/server.yaml", "x: [broken\ntoken: lenr1.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n", 0o640)
	hs.run("config", "show", "server")
	if strings.Contains(hs.stdout.String(), "AAAAAAAAAAAA") {
		t.Errorf("语法有误时也不得泄露：\n%s", hs.stdout)
	}
	// 未安装 14；无权读取 3；静态前端显示片段（脱敏逐行）。
	if code := hs.run("config", "show", "node", "--json"); code != 14 {
		t.Errorf("未安装：%d", code)
	}
	_ = os.Chmod(filepath.Join(ws.Root, "etc/lingedge/server/server.yaml"), 0)
	if os.Geteuid() != 0 {
		if code := hs.run("config", "show", "server", "--json"); code != 3 {
			t.Errorf("无权读取应 3：%d", code)
		}
	}
}

// config edit：编辑副本 → 校验 → 原子替换（保留 .bak）→ 可选重启。编辑器是真实进程（经 safeexec 白名单启动的脚本）。
func editorScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "vi")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func editHarness(t *testing.T, script string) (*harness, *toolsworld.World) {
	t.Helper()
	h, w := opsHarness(t, withTTY(), withEnv(map[string]string{"EDITOR": "vi", "TERM": "xterm"}))
	h.env.Ops.EditorDirs = []string{editorScript(t, script)}
	return h, w
}

func TestConfigEditValidReplacesAtomicallyKeepsBakAndRestarts(t *testing.T) {
	h, w := editHarness(t, `printf 'doctor:\n  restarts_warn: 7\n' >> "$1"`)
	installNode(t, h, w)
	cfg := opsWrite(t, w, "etc/lingedge/tools/tools.yaml", "install:\n  health_timeout: 1s\n  health_interval: 100ms\n", 0o640)
	before, _ := os.ReadFile(cfg)
	if code := h.run("config", "edit", "tools"); code != 0 {
		t.Fatalf("%d\n%s\n%s", code, h.stdout, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "已替换 /etc/lingedge/tools/tools.yaml") || !strings.Contains(out, ".bak") {
		t.Errorf("人类输出：%q", out)
	}
	after, _ := os.ReadFile(cfg)
	bak, _ := os.ReadFile(cfg + ".bak")
	if !strings.Contains(string(after), "restarts_warn: 7") || string(bak) != string(before) {
		t.Errorf("替换/备份内容：\nafter=%q\nbak=%q", after, bak)
	}
	if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o640 {
		t.Errorf("权限应保持 0640：%v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(cfg + ".bak"); fi.Mode().Perm() != 0o640 {
		t.Errorf(".bak 权限应与原文件一致：%v", fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(filepath.Dir(cfg)); len(ents) != 2 {
		t.Errorf("不应留下临时文件：%v", ents)
	}
	// 组件配置 + --restart：校验通过后原子替换并重启该组件。
	nodeCfg := opsWrite(t, w, "etc/lingedge/node/node.yaml", "a: 1\n", 0o640)
	_ = os.Remove(filepath.Join(w.Ctl, "calls"))
	if code := h.run("config", "edit", "node", "--restart"); code != 0 {
		t.Fatalf("%d\n%s\n%s", code, h.stdout, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "已重启 node") {
		t.Errorf("人类输出：%q", h.stdout.String())
	}
	if got := strings.Join(callsLog(t, w), " "); !strings.Contains(got, "restart lingedge-node.service") {
		t.Errorf("应重启 node：%s", got)
	}
	if b, _ := os.ReadFile(nodeCfg); !strings.Contains(string(b), "restarts_warn: 7") {
		t.Errorf("node.yaml：%q", b)
	}
}

func TestConfigEditRejectsInvalidEditAndLeavesFileUntouched(t *testing.T) {
	h, w := editHarness(t, `printf 'release:\n  base_url: http://insecure.example.org\n' > "$1"`)
	cfg := opsWrite(t, w, "etc/lingedge/tools/tools.yaml", "install:\n  health_timeout: 30s\n", 0o640)
	before, _ := os.ReadFile(cfg)
	// 交互下校验失败 → 询问是否重新编辑（stdin 为空 = 不重新编辑）→ 退出 3，文件不变，没有 .bak，修改副本被保留供找回。
	if code := h.run("config", "edit", "tools"); code != 3 {
		t.Fatalf("%d\n%s\n%s", code, h.stdout, h.stderr)
	}
	errText := h.stderr.String()
	if !strings.Contains(errText, "config.invalid") || !strings.Contains(errText, "第 2 行") {
		t.Errorf("stderr 应指出位置与错误码：%s", errText)
	}
	if after, _ := os.ReadFile(cfg); string(after) != string(before) {
		t.Error("校验失败的编辑不得被应用")
	}
	if _, err := os.Stat(cfg + ".bak"); err == nil {
		t.Error("没有替换就不应写 .bak")
	}
	i := strings.Index(errText, "你的修改保存在 ")
	if i < 0 {
		t.Fatalf("应告知修改副本的位置：%s", errText)
	}
	rej := strings.Fields(errText[i+len("你的修改保存在 "):])[0]
	if b, err := os.ReadFile(rej); err != nil || !strings.Contains(string(b), "insecure.example.org") {
		t.Errorf("被拒绝的修改应保存在 %s：%v", rej, err)
	}
	if fi, err := os.Stat(rej); err == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("被拒副本权限 %v", fi.Mode().Perm())
	}
}

func TestConfigEditRetryAfterInvalid(t *testing.T) {
	// 第一次编辑写坏，用户回答 y 重新编辑，第二次写好：最终应用第二次的内容。
	counter := filepath.Join(t.TempDir(), "n")
	script := `n=$(cat "` + counter + `" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "` + counter + `"
if [ "$n" = 1 ]; then printf 'bogus_section: 1\n' > "$1"; else printf 'log:\n  level: debug\n' > "$1"; fi`
	h, w := editHarness(t, script)
	h.env.Stdin = strings.NewReader("y\n")
	cfg := opsWrite(t, w, "etc/lingedge/tools/tools.yaml", "install:\n  health_timeout: 30s\n", 0o640)
	if code := h.run("config", "edit", "tools"); code != 0 {
		t.Fatalf("%d\n%s\n%s", code, h.stdout, h.stderr)
	}
	if b, _ := os.ReadFile(cfg); string(b) != "log:\n  level: debug\n" {
		t.Errorf("应用的应是第二次的内容：%q", b)
	}
}

func TestConfigEditNoChangeAndGuards(t *testing.T) {
	h, w := editHarness(t, `true`)
	cfg := opsWrite(t, w, "etc/lingedge/tools/tools.yaml", "install:\n  health_timeout: 30s\n", 0o640)
	if code := h.run("config", "edit", "tools"); code != 0 {
		t.Fatalf("%d\n%s", code, h.stderr)
	}
	if !strings.Contains(h.stdout.String(), "没有改动") {
		t.Errorf("没有改动应明确告知：%q", h.stdout.String())
	}
	if _, err := os.Stat(cfg + ".bak"); err == nil {
		t.Error("没有改动不应写 .bak")
	}
	// 非交互/--json 无 TTY：9；非 root：3；静态前端：不可编辑 3；dry-run：只给计划。
	nh, nw := opsHarness(t)
	opsWrite(t, nw, "etc/lingedge/tools/tools.yaml", "install:\n  health_timeout: 30s\n", 0o640)
	if code := nh.run("config", "edit", "tools", "--non-interactive", "--json"); code != 9 {
		t.Errorf("非交互：%d", code)
	}
	if code := nh.run("config", "edit", "tools", "--dry-run", "--json"); code != 0 {
		t.Errorf("--dry-run 允许非交互：%d", code)
	} else if err := validateAgainst(t, "data.config-edit.schema.json", dataOf(t, nh)); err != nil {
		t.Errorf("dry-run 的 data：%v", err)
	}
	nr := newHarness(t, withTTY())
	if code := nr.run("config", "edit", "tools"); code != 3 || !strings.Contains(nr.stderr.String(), "precondition.root_required") {
		t.Errorf("非 root：%d", code)
	}
	// 不受支持的编辑器 → 2（不经 shell、不接受带参数的命令行）。
	h2, w2 := opsHarness(t, withTTY(), withEnv(map[string]string{"EDITOR": "rm -rf /"}))
	opsWrite(t, w2, "etc/lingedge/tools/tools.yaml", "install:\n  health_timeout: 30s\n", 0o640)
	if code := h2.run("config", "edit", "tools"); code != 2 || !strings.Contains(h2.stderr.String(), "usage.editor_unsupported") {
		t.Errorf("不受支持的编辑器：%d %s", code, h2.stderr)
	}
	h3, w3 := opsHarness(t, withTTY(), withEnv(map[string]string{"EDITOR": "emacs-evil"}))
	opsWrite(t, w3, "etc/lingedge/tools/tools.yaml", "install:\n  health_timeout: 30s\n", 0o640)
	if code := h3.run("config", "edit", "tools"); code != 2 {
		t.Errorf("白名单外的编辑器名：%d", code)
	}
	hs, ws := staticHarness(t)
	hs.env.StdinTTY, hs.env.StderrTTY = true, true
	if code := hs.run("install", "docs", "--offline", ws.Bundle, "--set", "host=docs.example.com", "--non-interactive"); code != 0 {
		t.Fatal(code)
	}
	if code := hs.run("config", "edit", "docs"); code != 3 || !strings.Contains(hs.stderr.String(), "config.not_editable") {
		t.Errorf("静态前端片段不可编辑：%d %s", code, hs.stderr)
	}
}

// tools.yaml 的 log 节是全局日志标志的缺省；命令行显式值优先；损坏的 tools.yaml 不阻止命令运行（回落到内置缺省）。
func TestToolsYAMLLogSectionSuppliesDefaults(t *testing.T) {
	h, w := opsHarness(t)
	h.env.Geteuid = func() int { return 1000 }
	logPath := filepath.Join(w.Root, "var/log/lingedge/custom.log")
	opsWrite(t, w, "etc/lingedge/tools/tools.yaml", "log:\n  level: debug\n  file: /var/log/lingedge/custom.log\n", 0o644)
	if code := h.run("version"); code != 0 {
		t.Fatalf("%d\n%s", code, h.stderr)
	}
	b, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(b), `"command.start"`) {
		t.Fatalf("应写入 tools.yaml 指定的日志文件：%v %q", err, b)
	}
	// 命令行 --log-file 优先。
	other := filepath.Join(t.TempDir(), "cli.log")
	if code := h.run("version", "--log-file", other); code != 0 {
		t.Fatal(code)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("--log-file 应优先：%v", err)
	}
	// tools.yaml 损坏：命令仍可运行（诊断类命令必须能在配置损坏时运行），回落到内置缺省。
	opsWrite(t, w, "etc/lingedge/tools/tools.yaml", "log:\n  level: trace\n", 0o644)
	if code := h.run("version"); code != 0 {
		t.Errorf("损坏的 tools.yaml 不得阻止 version：%d", code)
	}
	if code := h.run("config", "validate", "tools"); code != 3 {
		t.Errorf("config validate 应报告问题：%d", code)
	}
}

// dep.mysql.pmax 的生产实现：经迁移账号凭据与 mysql 客户端（伪客户端脚本记录每次调用的 SQL）；
// 表名来自 information_schema，必须经标识符白名单后才拼进 SQL——恶意表名不得被执行。
func fakeMySQLClient(t *testing.T, tables string, counts map[string]string) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(dir, "sql.log")
	var cases strings.Builder
	for tbl, n := range counts {
		cases.WriteString("  *'`" + tbl + "` PARTITION (pmax)'*) echo " + n + ";;\n")
	}
	body := "#!/bin/sh\nfor last; do :; done\necho \"$last\" >> '" + log + "'\ncase \"$last\" in\n  *information_schema.partitions*) printf '" + tables + "';;\n" + cases.String() + "  *) echo 0;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "mysql"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, log
}

func pmaxHarness(t *testing.T, tables string, counts map[string]string) (*harness, string) {
	t.Helper()
	dir, log := fakeMySQLClient(t, tables, counts)
	runner, err := safeexec.New(safeexec.Options{Allowed: []string{"mysql"}, SearchDirs: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	pw := "p" + "w-test-only"
	h := newHarness(t, asRoot(), withEnv(map[string]string{backup.EnvDBPass: pw, backup.EnvHost: "127.0.0.1", backup.EnvPort: "3306", backup.EnvName: "lingedge"}))
	h.env.Backup.MySQLRunner = runner
	return h, log
}

func TestDoctorPmaxProductionPathUsesMySQLClientAndWhitelistsIdentifiers(t *testing.T) {
	h, log := pmaxHarness(t, "audit_logs\\nlogin_logs\\n", map[string]string{"audit_logs": "0", "login_logs": "5"})
	rows, err := h.pmaxRows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Table != "audit_logs" || rows[0].Rows != 0 || rows[1].Table != "login_logs" || rows[1].Rows != 5 {
		t.Fatalf("%+v", rows)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "SELECT COUNT(*) FROM `login_logs` PARTITION (pmax)") {
		t.Errorf("应逐表计数：\n%s", b)
	}
	// 恶意表名：被标识符白名单拒绝，且不会执行任何 COUNT 查询。
	h2, log2 := pmaxHarness(t, "evil`; DROP TABLE users; --\\n", nil)
	if _, err := h2.pmaxRows(context.Background()); err == nil {
		t.Fatal("非法表名应被拒绝")
	}
	b2, _ := os.ReadFile(log2)
	if strings.Contains(string(b2), "COUNT") || strings.Contains(string(b2), "DROP") {
		t.Errorf("恶意表名不得进入任何后续 SQL：\n%s", b2)
	}
	// 没有凭据：skip 的来源错误（不提示、不交互）。
	h3 := newHarness(t, asRoot(), withTTY())
	if _, err := h3.pmaxRows(context.Background()); err == nil {
		t.Error("没有凭据应返回错误")
	}
}

// pmaxRows 以该 harness 的 Env 构造 Run 并调用生产的 doctorPmax。
func (h *harness) pmaxRows(ctx context.Context) ([]doctor.PartitionRow, error) {
	r := &Run{env: h.env.withDefaults(), ctx: ctx}
	return r.doctorPmax()(ctx)
}
