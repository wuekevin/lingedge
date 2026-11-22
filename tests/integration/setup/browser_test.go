package setupit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// Story 1.37 的浏览器端到端（Playwright，真实 Chromium）：被测对象是**真实的 lingedge-server 二进制（setup mode）**、本测试自拉起的独立临时 mysqld
// 与 redis-server；向导 SPA 是内嵌在二进制里的真实生产构建（go:embed）。没有任何 Mock：前端 spec 只驱动页面，不拦截任何请求。
//
// 可用性与开关（与 mysqltest 的约定一致：缺环境时必须干净地跳过，且不得假装通过）：
//   - 找不到 pnpm/node、Playwright 或其 Chromium 浏览器 → t.Skip，消息以「等待环境验证」开头；
//   - LINGEDGE_E2E_BROWSER=off（或 0/false）→ 显式跳过；testing.Short() 也跳过；
//   - LINGEDGE_REQUIRE_BROWSER=1 → 浏览器环境缺失视为失败（make e2e 设置它）；
//   - LINGEDGE_E2E_SHOT_DIR=<目录> → 截图落到该目录（缺省落在用后即删的临时目录）。
const (
	envBrowserTests   = "LINGEDGE_E2E_BROWSER"
	envBrowserRequire = "LINGEDGE_REQUIRE_BROWSER"
	envShotDir        = "LINGEDGE_E2E_SHOT_DIR"
	adminURLForE2E    = "https://admin.e2e.example.test"
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

type browserEnv struct {
	pnpm   string
	setup  string // frontend/setup
	shotTo string
}

// needBrowser 检查浏览器端到端的前置条件，缺失时按约定跳过/失败。
func needBrowser(t *testing.T) browserEnv {
	t.Helper()
	skip := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		if os.Getenv(envBrowserRequire) == "1" {
			t.Fatalf("%s（%s=1：浏览器环境缺失视为失败）", msg, envBrowserRequire)
		}
		t.Skip("等待环境验证: " + msg)
	}
	switch strings.ToLower(os.Getenv(envBrowserTests)) {
	case "off", "0", "false":
		t.Skip("等待环境验证: " + envBrowserTests + "=off，浏览器端到端被显式关闭")
	}
	if testing.Short() {
		t.Skip("-short：跳过浏览器端到端")
	}
	pnpm, err := exec.LookPath("pnpm")
	if err != nil {
		skip("找不到 pnpm（ENV-BROWSER）")
	}
	setupDir := filepath.Join(repoRoot(), "frontend", "setup")
	if _, err := os.Stat(filepath.Join(setupDir, "node_modules", "@playwright", "test")); err != nil {
		skip("前端依赖未安装（先运行 pnpm -C frontend install --frozen-lockfile）")
	}
	cmd := exec.Command("node", "-e", "console.log(require('@playwright/test').chromium.executablePath())")
	cmd.Dir = setupDir
	out, err := cmd.Output()
	if err != nil {
		skip("无法解析 Playwright 的 Chromium 路径: %v", err)
	}
	exe := strings.TrimSpace(string(out))
	if _, err := os.Stat(exe); err != nil {
		skip("Playwright 的 Chromium 未安装（%s；运行 pnpm -C frontend/setup exec playwright install chromium）", exe)
	}
	shot := os.Getenv(envShotDir)
	if shot == "" {
		shot = t.TempDir()
	}
	return browserEnv{pnpm: pnpm, setup: setupDir, shotTo: shot}
}

// browserRun 是一个场景：独立的 wiz 环境（临时 MySQL/Redis/数据目录/随机口令）+ 真实 server 进程。
type browserRun struct {
	t       *testing.T
	b       browserEnv
	w       *wiz
	p       *proc
	cfgPath string
	addr    string
	out     string
	name    string
}

func newBrowserRun(t *testing.T, b browserEnv, name string) *browserRun {
	t.Helper()
	w := newWiz(t)
	w.stop() // 进程内向导服务不用；只用 wiz 的环境
	cfgPath, addr := w.processConfig(t)
	r := &browserRun{t: t, b: b, w: w, cfgPath: cfgPath, addr: addr, name: name, out: filepath.Join(w.dir, "e2e-out.json")}
	r.p = w.startServer(t, cfgPath, addr)
	w.baseURL = r.p.url
	return r
}

func (r *browserRun) restart() {
	r.t.Helper()
	r.p.kill() // SIGKILL：进程内存全部丢失，模拟中断
	r.p = r.w.startServer(r.t, r.cfgPath, r.addr)
}

// playwright 运行一个 spec 文件，extra 是附加的环境变量（KEY=VALUE）。失败时输出完整 Playwright 日志。
func (r *browserRun) playwright(spec string, extra ...string) {
	r.t.Helper()
	w := r.w
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.b.pnpm, "exec", "playwright", "test", "--config=e2e/playwright.config.ts", spec)
	cmd.Dir = r.b.setup
	env := []string{
		"LE_E2E_BASE_URL=" + r.p.url, "LE_E2E_TOKEN=" + w.token,
		"LE_E2E_DB_HOST=" + w.inst.Host(), fmt.Sprintf("LE_E2E_DB_PORT=%d", w.inst.Port), "LE_E2E_DB_SOCKET=" + w.inst.Socket, "LE_E2E_DB_NAME=" + w.dbName,
		"LE_E2E_DB_ADMIN_USER=" + w.adminUser, "LE_E2E_DB_ADMIN_PW=" + w.adminPW, "LE_E2E_REDIS_ADDR=" + w.rds.Addr(),
		"LE_E2E_ADMIN_EMAIL=" + w.admEmail, "LE_E2E_ADMIN_PW=" + w.admPW, "LE_E2E_ADMIN_URL=" + adminURLForE2E,
		"LE_E2E_OUT=" + r.out, "LE_E2E_SHOT_DIR=" + filepath.Join(r.b.shotTo, r.name), "LE_E2E_SCENARIO=" + r.name,
		"CI=1", "FORCE_COLOR=0",
	}
	cmd.Env = cleanEnv(append(env, extra...)...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	r.t.Logf("Playwright %s %v 输出:\n%s", spec, extra, tail(buf.String(), 6000))
	if err != nil {
		r.t.Fatalf("Playwright %s 失败: %v\n服务端日志（末尾）:\n%s", spec, err, tail(w.logs.String(), 4000))
	}
	// 页面里用过的所有机密都不得出现在测试输出里（Playwright 日志本身也扫描）。
	for _, s := range w.secrets {
		if s != "" && strings.Contains(buf.String(), s) {
			r.t.Errorf("Playwright 输出泄漏了机密（长度 %d）", len(s))
		}
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

type e2eOut struct {
	MigratePassword string   `json:"migratePassword"`
	TOTPSecret      string   `json:"totpSecret"`
	RecoveryCodes   []string `json:"recoveryCodes"`
	AdminURL        string   `json:"adminUrl"`
}

func (r *browserRun) readOut() e2eOut {
	r.t.Helper()
	var o e2eOut
	b, err := os.ReadFile(r.out)
	if err != nil {
		r.t.Fatalf("spec 没有写出结果文件: %v", err)
	}
	if err := json.Unmarshal(b, &o); err != nil {
		r.t.Fatal(err)
	}
	return o
}

// assertInstalled 校验一次成功安装留下的全部事实（与 API 驱动的 e2e 同口径），并对页面里「只展示一次」的机密做泄漏扫描。
func (r *browserRun) assertInstalled(o e2eOut, wantSocket bool) {
	t, w := r.t, r.w
	if _, err := os.Stat(w.lockFile); err != nil {
		t.Errorf("install.lock 应存在: %v", err)
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.system_installations WHERE install_locked_at IS NOT NULL"); n != 1 {
		t.Errorf("应恰有一条完成的安装记录，实际 %d", n)
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.user_roles ur JOIN §.roles r ON r.id = ur.role_id JOIN §.users u ON u.id = ur.user_id WHERE r.code = 'platform_admin' AND u.email = ? AND u.must_change_password = 0", w.admEmail); n != 1 {
		t.Errorf("向导创建的管理员应持有 platform_admin 且无需强制改密")
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.system_settings WHERE setting_key = 'general.base_url'"); n != 1 {
		t.Errorf("基础配置应已写入")
	}
	if _, err := os.Stat(w.hashFile); err == nil {
		if b, _ := os.ReadFile(w.hashFile); len(bytes.TrimSpace(b)) > 0 {
			t.Errorf("complete 之后 setup token 哈希应被删除或清空")
		}
	}
	st, err := os.Lstat(w.credFile)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("运行期凭据文件应存在且为 0600: %v %v", st, err)
	}
	cred := w.readCred(t)
	if wantSocket != strings.Contains(cred, `"socket"`) {
		t.Errorf("凭据文件的 socket 字段与场景不符（want socket=%v）", wantSocket)
	}
	if wantSocket && strings.Contains(cred, `"host"`) {
		t.Errorf("socket 场景的凭据文件不应含 host")
	}
	// 页面里展示过的一次性机密，之后任何地方都不得再出现。
	secrets := append([]string{o.MigratePassword, o.TOTPSecret}, o.RecoveryCodes...)
	w.secrets = append(w.secrets, secrets...)
	r.scan(secrets...)
	// 同一进程已切换为正常模式。
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := tryStatus(t, "GET", r.p.url+"/healthz/ready", nil); code == 200 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code, _ := httpStatus(t, "GET", r.p.url+"/healthz/ready", nil); code != 200 {
		t.Errorf("complete 后同一进程应切换为正常模式（/healthz/ready 200）: %d", code)
	}
	// 管理员可登录；启用了 TOTP 则进入第二步并可用页面里的密钥验证。
	code, m := httpStatus(t, "POST", r.p.url+"/api/v1/auth/login", map[string]string{"realm": "admin", "email": w.admEmail, "password": w.admPW}, "X-Forwarded-For", "203.0.113.50")
	if o.TOTPSecret == "" {
		if code != 200 || m["status"] == "totp_required" {
			t.Errorf("未启用 TOTP 的管理员应直接登录成功: %d %v", code, m)
		}
		return
	}
	if code != 200 || m["status"] != "totp_required" {
		t.Fatalf("启用 TOTP 的管理员应进入第二步: %d %v", code, m)
	}
	otp, err := totp.GenerateCode(o.TOTPSecret, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if code, m := httpStatus(t, "POST", r.p.url+"/api/v1/auth/totp/verify", map[string]string{"mfa_token": fmt.Sprint(m["mfa_token"]), "code": otp}, "X-Forwarded-For", "203.0.113.50"); code != 200 {
		t.Errorf("用页面里展示的 TOTP 密钥验证应登录成功: %d %v", code, m)
	}
}

// scan 扫描服务端日志、凭据文件、审计与状态行里是否出现已知机密。
func (r *browserRun) scan(extra ...string) {
	t, w := r.t, r.w
	all := append(append([]string(nil), w.secrets...), extra...)
	check := func(where, text string) {
		for _, s := range all {
			if s != "" && strings.Contains(text, s) {
				t.Errorf("%s 中出现了已知机密（长度 %d）", where, len(s))
			}
		}
	}
	if w.logs.String() == "" {
		t.Fatal("服务端日志为空：扫描会空转通过")
	}
	check("服务端日志", w.logs.String())
	if b, err := os.ReadFile(w.credFile); err == nil {
		check("runtime-credentials.json", string(b))
	}
	if w.dbInt("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'audit_logs'", w.dbName) > 0 {
		check("audit_logs", w.dumpAudit())
		check("system_installations.meta_json", w.stateJSON())
	}
}

// 全流程（TCP，管理员绑定 TOTP，中文界面）。
func TestBrowserWizardFullTCP(t *testing.T) {
	b := needBrowser(t)
	r := newBrowserRun(t, b, "full-tcp-zh")
	r.playwright("e2e/full.spec.ts", "LE_E2E_CONN=tcp", "LE_E2E_TOTP=1", "LE_E2E_LOCALE=zh-CN")
	r.assertInstalled(r.readOut(), false)
	if rc := r.p.term(); rc != 0 {
		t.Errorf("正常模式优雅停机退出码应为 0: %d", rc)
	}
}

// 全流程（Unix socket 连接 MySQL，无 TOTP，英文界面）——CC-13 的向导侧字段。
func TestBrowserWizardFullSocketEnglish(t *testing.T) {
	b := needBrowser(t)
	r := newBrowserRun(t, b, "full-socket-en")
	r.playwright("e2e/full.spec.ts", "LE_E2E_CONN=socket", "LE_E2E_TOTP=0", "LE_E2E_LOCALE=en")
	r.assertInstalled(r.readOut(), true)
}

// 暗色主题全流程（Story 1.37b）：Chromium 以 prefers-color-scheme: dark 打开，向导必须跟随系统切到暗色令牌；
// 每个检查点都做文字对比度 ≥4.5:1 审计（含品牌侧栏渐变的最坏色标）、主题底色核对，英文界面下真实 server 的全部消息必须命中词条（无一处回落到中文原文）。
func TestBrowserWizardFullDarkEnglish(t *testing.T) {
	b := needBrowser(t)
	r := newBrowserRun(t, b, "full-dark-en")
	r.playwright("e2e/full.spec.ts", "LE_E2E_CONN=tcp", "LE_E2E_TOTP=1", "LE_E2E_LOCALE=en", "LE_E2E_SCHEME=dark")
	r.assertInstalled(r.readOut(), false)
}

// 错 token / URL 里的 token / 限流（暗色 + 英文：错误码、Request ID 与词条化说明在暗色下同样可读）。
func TestBrowserWizardBadToken(t *testing.T) {
	b := needBrowser(t)
	r := newBrowserRun(t, b, "bad-token")
	r.playwright("e2e/token.spec.ts", "LE_E2E_LOCALE=en", "LE_E2E_SCHEME=dark")
	// 整个过程没有任何写入：无凭据文件、无 install.lock。
	if _, err := os.Stat(r.w.credFile); err == nil {
		t.Error("错 token 场景不应写出凭据文件")
	}
	if _, err := os.Stat(r.w.lockFile); err == nil {
		t.Error("错 token 场景不应写出 install.lock")
	}
	r.scan()
}

// 非空库。
func TestBrowserWizardNonEmptyDatabase(t *testing.T) {
	b := needBrowser(t)
	r := newBrowserRun(t, b, "non-empty-db")
	ne := "lingedge_ne_" + rnd(4)
	r.w.exec("CREATE DATABASE `" + ne + "`")
	r.w.exec("CREATE TABLE `" + ne + "`.precious (id INT PRIMARY KEY, v VARCHAR(20))")
	r.w.exec("INSERT INTO `" + ne + "`.precious VALUES (1, 'keep-me')")
	t.Cleanup(func() { _, _ = r.w.adm.Exec("DROP DATABASE IF EXISTS `" + ne + "`") })
	r.playwright("e2e/nonempty.spec.ts", "LE_E2E_NONEMPTY_DB="+ne, "LE_E2E_LOCALE=en")
	// 向导没有动过非空库：数据原样、没有任何迁移表。
	var v string
	if err := r.w.adm.QueryRow("SELECT v FROM `" + ne + "`.precious WHERE id = 1").Scan(&v); err != nil || v != "keep-me" {
		t.Errorf("非空库的数据应原样保留: %q %v", v, err)
	}
	if n := r.w.dbInt("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?", ne); n != 1 {
		t.Errorf("非空库里不应出现任何新表，实际 %d 张表", n)
	}
	r.scan()
}

// 中断续做（early）：做到 Redis 后 SIGKILL 重启；续做时初始化步骤要求重新提供迁移凭据（409），回到 MySQL 步骤重新提交后完成全部。
func TestBrowserWizardResumeEarly(t *testing.T) {
	b := needBrowser(t)
	r := newBrowserRun(t, b, "resume-early")
	r.playwright("e2e/resume.spec.ts", "LE_E2E_PHASE=a-early")
	if _, err := os.Stat(r.w.lockFile); err == nil {
		t.Fatal("阶段 A 不应完成安装")
	}
	r.restart()
	r.playwright("e2e/resume.spec.ts", "LE_E2E_PHASE=b-early")
	r.assertInstalled(r.readOut(), false)
}

// 中断续做（late）：做到「创建管理员」后 SIGKILL 重启，从「基础配置」继续并完成。
func TestBrowserWizardResumeLate(t *testing.T) {
	b := needBrowser(t)
	r := newBrowserRun(t, b, "resume-late")
	r.playwright("e2e/resume.spec.ts", "LE_E2E_PHASE=a-late")
	r.restart()
	r.playwright("e2e/resume.spec.ts", "LE_E2E_PHASE=b-late")
	r.assertInstalled(r.readOut(), false)
}

// 键盘可操作。
func TestBrowserWizardKeyboard(t *testing.T) {
	b := needBrowser(t)
	r := newBrowserRun(t, b, "keyboard")
	r.playwright("e2e/keyboard.spec.ts")
	r.scan()
}

// E2E 审计器的负向对照（对比度审计/CSP 监视/TOTP 夹具）：不需要 server，保证这些审计不会空转通过。
func TestBrowserAuditorsSelfTest(t *testing.T) {
	b := needBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.pnpm, "exec", "playwright", "test", "--config=e2e/playwright.config.ts", "e2e/selftest.spec.ts")
	cmd.Dir = b.setup
	cmd.Env = cleanEnv("LE_E2E_BASE_URL=http://127.0.0.1:1", "CI=1", "FORCE_COLOR=0")
	out, err := cmd.CombinedOutput()
	t.Logf("Playwright selftest 输出:\n%s", tail(string(out), 3000))
	if err != nil {
		t.Fatalf("E2E 审计器自测失败: %v", err)
	}
}
