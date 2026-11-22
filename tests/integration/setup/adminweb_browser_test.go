package setupit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/argon2"

	"github.com/wuekevin/lingedge/tests/apistack"
)

// Story 1.38 的 Admin 浏览器端到端（Playwright，真实 Chromium）：被测对象是**真实的 lingedge-server 二进制**（先经 setup 向导完成安装，
// 同一进程切换为正常模式）、本测试自拉起的独立临时 mysqld 与 redis-server；Admin SPA 是真实的 Vite 构建。没有 Mock：
//   - frontend/dist/admin：生产构建（登录/TOTP/恢复码/租户账号被拒/空菜单外壳/暗色对比度）；
//   - frontend/dist/admin-e2e：e2e 模式构建（多出夹具页面，用来验证菜单裁剪/403/404/标签/搜索/手机宽度；页面只是占位，权限与模块来自真实后端）。
// 两个构建由 Playwright 的 webServer（frontend/admin/e2e/serve.mjs）以「同源静态 + /api 反代（保留 Host）」提供，等价于生产 Nginx 的部署形态。
// 可用性与开关与 browser_test.go 一致（缺环境 → 「等待环境验证」跳过；LINGEDGE_REQUIRE_BROWSER=1 → 失败）。

type adminWorld struct {
	r            *browserRun
	totpSecret   string
	recovery     []string
	auditorEmail string
	auditorPW    string
	tenantEmail  string
	tenantPW     string
	mustEmail    string
	mustPW       string
	adminDir     string
}

func adminArgonHash(t *testing.T, pw string) string {
	t.Helper()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	const mem, iter, par = 19456, 2, 1
	key := argon2.IDKey([]byte(pw), salt, iter, mem, par, 32)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, mem, iter, par, b64.EncodeToString(salt), b64.EncodeToString(key))
}

func newAdminWorld(t *testing.T, name string) *adminWorld {
	t.Helper()
	b := needBrowser(t)
	adminDir := filepath.Join(repoRoot(), "frontend", "admin")
	for _, d := range []string{"admin", "admin-e2e"} {
		if _, err := os.Stat(filepath.Join(repoRoot(), "frontend", "dist", d, "index.html")); err != nil {
			msg := fmt.Sprintf("Admin 构建产物缺失（frontend/dist/%s；先运行 make admin-e2e-build）", d)
			if os.Getenv(envBrowserRequire) == "1" {
				t.Fatalf("%s（%s=1：缺失视为失败）", msg, envBrowserRequire)
			}
			t.Skip("等待环境验证: " + msg)
		}
	}
	r := newBrowserRun(t, b, name)
	w := r.w
	aw := &adminWorld{r: r, adminDir: adminDir}

	// ---- 经真实向导 API 完成安装（管理员绑定 TOTP，拿到恢复码）----
	w.runThrough("init")
	ab := w.adminBody()
	ab["enable_totp"] = true
	res := w.mustOK(w.post("admin", ab, "__none__"), "admin")
	sec := res.secrets()
	if sec == nil || sec["totp_secret"] == "" {
		t.Fatalf("应返回 TOTP 密钥: %s", res.Raw)
	}
	aw.totpSecret = sec["totp_secret"].(string)
	w.secrets = append(w.secrets, aw.totpSecret)
	// 用上一个 time-step 的验证码确认启用：服务端对已用过的 time-step 做重放保护，当前步要留给浏览器里的真实登录。
	code, err := totp.GenerateCode(aw.totpSecret, time.Now().Add(-30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ab["totp_code"] = code
	res = w.mustOK(w.post("admin", ab), "admin(confirm)")
	codes, _ := res.secrets()["recovery_codes"].([]any)
	if len(codes) != 10 {
		t.Fatalf("应发放 10 个恢复码: %s", res.Raw)
	}
	for _, c := range codes {
		aw.recovery = append(aw.recovery, c.(string))
		w.secrets = append(w.secrets, c.(string))
	}
	w.mustOK(w.put("system", w.systemBody()), "system")
	w.mustOK(w.post("keys", nil), "keys")
	w.mustOK(w.post("complete", nil, w.migPW), "complete")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if c, _ := tryStatus(t, "GET", r.p.url+"/healthz/ready", nil); c == 200 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if c, _ := httpStatus(t, "GET", r.p.url+"/healthz/ready", nil); c != 200 {
		t.Fatalf("向导完成后同一进程应切换为正常模式: %d", c)
	}

	// ---- 额外账号（root 直接写库，口令运行期生成，G-6）：只读审计员 / 租户账号 / 须改口令的管理员 ----
	db := "`" + w.dbName + "`"
	seed := func(typ, label string, mustChange bool) (string, string, uint64) {
		email := strings.ToLower(label+rnd(3)) + "@example.test"
		pw := "Qx" + rnd(10) + "!y"
		w.secrets = append(w.secrets, pw)
		uidv := apistack.IDs.MustNew()
		must := 0
		if mustChange {
			must = 1
		}
		w.exec("INSERT INTO "+db+".users (uid, user_type, email, display_name, password_hash, status, must_change_password) VALUES (?, ?, ?, ?, ?, 'active', ?)", uidv, typ, email, "E2E "+label, adminArgonHash(t, pw), must)
		return email, pw, uint64(w.dbInt("SELECT id FROM §.users WHERE uid = ?", uidv))
	}
	var uidAuditor, uidMust, uidTenant uint64
	aw.auditorEmail, aw.auditorPW, uidAuditor = seed("platform", "auditor", false)
	aw.mustEmail, aw.mustPW, uidMust = seed("platform", "mustchange", true)
	aw.tenantEmail, aw.tenantPW, uidTenant = seed("tenant", "tenantuser", false)
	for _, u := range []uint64{uidAuditor, uidMust} {
		role := "platform_auditor"
		w.exec("INSERT INTO "+db+".user_roles (user_id, role_id, tenant_id) SELECT ?, id, NULL FROM "+db+".roles WHERE scope = 'platform' AND tenant_id IS NULL AND code = ?", u, role)
		w.exec("UPDATE "+db+".users SET permission_version = permission_version + 1 WHERE id = ?", u)
	}
	tenantUID := apistack.IDs.MustNew()
	w.exec("INSERT INTO "+db+".tenants (uid, name, slug, status, locale, contact_email) VALUES (?, 'E2E Tenant', ?, 'active', 'zh-CN', 'tenant@example.test')", tenantUID, "t-"+strings.ToLower(tenantUID))
	tid := uint64(w.dbInt("SELECT id FROM §.tenants WHERE uid = ?", tenantUID))
	w.exec("INSERT INTO "+db+".tenant_members (tenant_id, user_id, status, is_owner, joined_at) VALUES (?, ?, 'active', 0, NOW(3))", tid, uidTenant)
	w.exec("INSERT INTO "+db+".user_roles (user_id, role_id, tenant_id) SELECT ?, id, ? FROM "+db+".roles WHERE scope = 'tenant' AND tenant_id IS NULL AND code = 'tenant_owner'", uidTenant, tid)
	return aw
}

// playwrightAdmin 在 frontend/admin 下运行一个 spec（Playwright 的 webServer 负责拉起/回收静态服务）。
func (a *adminWorld) playwrightAdmin(spec string, extra ...string) {
	a.r.t.Helper()
	r, w := a.r, a.r.w
	t := r.t
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.b.pnpm, "exec", "playwright", "test", "--config=e2e/playwright.config.ts", spec)
	cmd.Dir = a.adminDir
	env := []string{
		"LE_E2E_API=" + r.p.url,
		"LE_E2E_ADMIN_EMAIL=" + w.admEmail, "LE_E2E_ADMIN_PW=" + w.admPW, "LE_E2E_TOTP_KEY=" + a.totpSecret, "LE_E2E_RECOVERY_CODES=" + strings.Join(a.recovery, ","),
		"LE_E2E_AUDITOR_EMAIL=" + a.auditorEmail, "LE_E2E_AUDITOR_PW=" + a.auditorPW,
		"LE_E2E_TENANT_EMAIL=" + a.tenantEmail, "LE_E2E_TENANT_PW=" + a.tenantPW,
		"LE_E2E_MUST_EMAIL=" + a.mustEmail, "LE_E2E_MUST_PW=" + a.mustPW,
		"LE_E2E_DIST_PROD=" + filepath.Join(repoRoot(), "frontend", "dist", "admin"), "LE_E2E_DIST_FX=" + filepath.Join(repoRoot(), "frontend", "dist", "admin-e2e"),
		"LE_E2E_PORT_PROD=" + fmt.Sprint(freePort(t)), "LE_E2E_PORT_FX=" + fmt.Sprint(freePort(t)),
		"LE_E2E_SHOT_DIR=" + filepath.Join(r.b.shotTo, r.name), "CI=1", "FORCE_COLOR=0",
	}
	cmd.Env = cleanEnv(append(env, extra...)...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	t.Logf("Playwright(admin) %s %v 输出:\n%s", spec, extra, tail(buf.String(), 8000))
	if err != nil {
		t.Fatalf("Playwright(admin) %s 失败: %v\n服务端日志（末尾）:\n%s", spec, err, tail(w.logs.String(), 4000))
	}
	for _, s := range w.secrets {
		if s != "" && strings.Contains(buf.String(), s) {
			t.Errorf("Playwright 输出泄漏了机密（长度 %d）", len(s))
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	var port int
	addr := freeAddr(t)
	if _, err := fmt.Sscanf(addr[strings.LastIndex(addr, ":")+1:], "%d", &port); err != nil {
		t.Fatal(err)
	}
	return port
}

// finish：服务端日志、凭据文件、审计里不得出现登录口令/TOTP 密钥/恢复码/会话相关机密；并核对审计里确有本次登录。
func (a *adminWorld) finish() {
	t, w := a.r.t, a.r.w
	a.r.scan(append([]string{a.totpSecret}, a.recovery...)...)
	if n := w.dbInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'auth.login'"); n < 1 {
		t.Errorf("审计里应有真实的登录记录，实际 %d", n)
	}
	// 不得残留：该 server 进程仍存活并可优雅停机。
	if rc := a.r.p.term(); rc != 0 {
		t.Errorf("正常模式优雅停机退出码应为 0: %d", rc)
	}
}

// 登录主流程（生产构建）：密码 + TOTP、恢复码（单次有效）、错口令、租户账号被拒、须改口令、CSRF 与 Cookie 属性、空菜单外壳、404、暗/亮主题对比度。
func TestBrowserAdminLoginFlows(t *testing.T) {
	a := newAdminWorld(t, "admin-login")
	a.playwrightAdmin("e2e/login.spec.ts")
	// 租户账号的登录尝试被记录为失败（登录日志里有 realm=admin 的失败）；且没有为它建立任何会话。
	if n := a.r.w.dbInt("SELECT COUNT(*) FROM §.sessions s JOIN §.users u ON u.id = s.user_id WHERE u.email = ?", a.tenantEmail); n != 0 {
		t.Errorf("租户账号不得在 Admin 入口建立任何会话，实际 %d", n)
	}
	a.finish()
}

// 外壳（e2e 构建）：菜单裁剪（模块已交付 ∧ 权限 ∧ 运行模式）、无权限菜单隐藏、直访 403/404、标签页、搜索、代管横幅、语言主题。
func TestBrowserAdminShell(t *testing.T) {
	a := newAdminWorld(t, "admin-shell")
	a.playwrightAdmin("e2e/shell.spec.ts")
	a.finish()
}

// 手机宽度（UX-23）：抽屉导航、面包屑 + 返回、状态/回滚/节点维护入口可用、复杂编辑如实标注、无横向溢出、触控目标。
func TestBrowserAdminMobile(t *testing.T) {
	a := newAdminWorld(t, "admin-mobile")
	a.playwrightAdmin("e2e/mobile.spec.ts")
	a.finish()
}

// 主题（亮/暗/跟随系统）与对比度审计：暗色与亮色各页面逐个审计 ≥4.5:1（含弹层、抽屉、搜索、403）。
func TestBrowserAdminThemeContrast(t *testing.T) {
	a := newAdminWorld(t, "admin-theme")
	a.playwrightAdmin("e2e/theme.spec.ts")
	a.finish()
}

// 审计器负向自测（对比度审计、控制台监视、TOTP 夹具、测试数据生成器），不需要 server。
func TestBrowserAdminAuditorsSelfTest(t *testing.T) {
	b := needBrowser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.pnpm, "exec", "playwright", "test", "--config=e2e/playwright.config.ts", "e2e/selftest.spec.ts")
	cmd.Dir = filepath.Join(repoRoot(), "frontend", "admin")
	cmd.Env = cleanEnv("LE_E2E_SELFTEST=1", "CI=1", "FORCE_COLOR=0")
	out, err := cmd.CombinedOutput()
	t.Logf("Playwright(admin) selftest 输出:\n%s", tail(string(out), 3000))
	if err != nil {
		t.Fatalf("Admin E2E 审计器自测失败: %v", err)
	}
}
