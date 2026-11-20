package serverintegration

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// Story 1.16：真实 lingedge-server 子进程（cmd 装配层的 assembly.buildHooks / register 真实接线，默认 argon2id 参数）——
// 登录 → 会话 → /auth/me → CSRF → 非自助路由被授权钩子拒绝 → 登出；Redis 被杀后登录仍然工作。
func TestServerProcessAuthLogin(t *testing.T) {
	e := newEnv(t, all)
	ns := newNotifySocket(t, e.dir)
	p := startProc(t, e, []string{"NOTIFY_SOCKET=" + ns.path}, "--config", e.cfgPath)
	eventually(t, "READY=1", 30*time.Second, func() bool { return ns.has("READY=1") || p.exited() })
	if p.exited() {
		t.Fatalf("进程提前退出: %d\n%s", p.exit, p.out.String())
	}
	api := "http://127.0.0.1:" + strconv.Itoa(e.ports.http) + "/api/v1"

	// 种子用户：测试内随机口令 + 真实 argon2id（与服务默认参数一致，登录时不会触发 rehash）。
	pw := mysqltest.RandomSecret(t) + "Aa1"
	d := config.Defaults().Auth.Argon2
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	key := argon2.IDKey([]byte(pw), salt, uint32(d.Iterations), uint32(d.MemoryKiB), uint8(d.Parallelism), 32)
	hash := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, d.MemoryKiB, d.Iterations, d.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
	ids := uid.NewGenerator(clock.System())
	email := strings.ToLower(ids.MustNew()) + "@example.test"
	e.root(t, "INSERT INTO §.users (uid, user_type, email, display_name, password_hash, status) VALUES (?, 'platform', ?, 'admin user', ?, 'active')", ids.MustNew(), email, hash)

	do := func(method, path string, body any, cookie, csrf string) (int, []byte, http.Header) {
		t.Helper()
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(method, api+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "le_admin_sid", Value: cookie})
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Bytes(), resp.Header
	}
	type loginBody struct {
		Realm    string `json:"realm"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	login := func() (string, string, int) {
		code, body, hdr := do("POST", "/auth/login", loginBody{Realm: "admin", Email: email, Password: pw}, "", "")
		if code != 200 {
			return "", "", code
		}
		var out struct {
			CSRF string `json:"csrf_token"`
		}
		_ = json.Unmarshal(body, &out)
		for _, c := range (&http.Response{Header: hdr}).Cookies() {
			if c.Name == "le_admin_sid" {
				if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
					t.Errorf("Cookie 属性不符: %+v", c)
				}
				return c.Value, out.CSRF, 200
			}
		}
		t.Fatalf("登录响应缺少会话 Cookie: %v", hdr)
		return "", "", 0
	}

	// 错误口令 → 401；正确口令 → 200 + Cookie。
	if code, _, _ := do("POST", "/auth/login", loginBody{Realm: "admin", Email: email, Password: pw + "x"}, "", ""); code != 401 {
		t.Fatalf("错误口令应 401: %d", code)
	}
	tok, csrf, code := login()
	if code != 200 || tok == "" || csrf == "" {
		t.Fatalf("登录应成功: %d\n%s", code, p.out.String())
	}
	if code, body, _ := do("GET", "/auth/me", nil, tok, ""); code != 200 || !strings.Contains(string(body), `"realm":"admin"`) {
		t.Fatalf("/auth/me: %d %s", code, body)
	}
	// 授权钩子尚未实现：认证通过的非自助路由（任务中心）仍被拒绝，不因「已登录」放行。
	if code, body, _ := do("GET", "/tasks", nil, tok, ""); code != 403 {
		t.Fatalf("任务中心在授权钩子实现前应 403: %d %s", code, body)
	}
	if code, _, _ := do("GET", "/tasks", nil, "", ""); code != 401 {
		t.Fatalf("未认证应 401: %d", code)
	}
	// Redis 被杀：登录与会话校验仍然工作（ADR-025）。
	e.redis.Kill()
	if tok2, _, code := login(); code != 200 || tok2 == "" {
		t.Fatalf("Redis 不可用时登录应仍成功: %d\n%s", code, p.out.String())
	}
	if code, _, _ := do("GET", "/auth/me", nil, tok, ""); code != 200 {
		t.Fatalf("Redis 不可用时会话校验应仍成功: %d", code)
	}
	// 登出（需要 CSRF）。
	if code, _, _ := do("POST", "/auth/logout", nil, tok, ""); code != 403 {
		t.Fatalf("缺 CSRF 的登出应 403: %d", code)
	}
	if code, _, _ := do("POST", "/auth/logout", nil, tok, csrf); code != 204 {
		t.Fatalf("登出应 204: %d", code)
	}
	if code, _, _ := do("GET", "/auth/me", nil, tok, ""); code != 401 {
		t.Fatalf("登出后应 401: %d", code)
	}
	// 进程日志：不含口令与令牌。
	logs := p.out.String()
	for _, s := range []string{pw, tok, csrf} {
		if strings.Contains(logs, s) {
			t.Errorf("进程日志泄露秘密值 %q…", s[:8])
		}
	}
}
