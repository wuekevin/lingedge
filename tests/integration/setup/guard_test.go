package setupit

import (
	"net/http"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/setup"
)

// AC2（抢占防护）：全部写接口（以及要求 token 的 status）缺少/错误 X-Setup-Token → 401 setup.token_invalid；
// 失败计数达到上限后 429（含此后携带正确 token 的请求，直到窗口结束），按来源 IP 隔离。
func TestTokenGuardAndThrottle(t *testing.T) {
	for _, mode := range []string{"local", "redis", "redis-down"} {
		t.Run(mode, func(t *testing.T) {
			w := newWiz(t, func(o *setup.Options) {
				o.FailMax = 3
				switch mode {
				case "redis":
					rc, err := redisx.New(ctxT(t), redisx.Config{Addr: redisAddr(t), Env: "test"}, redisx.Options{Clock: clock.System()})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = rc.Close() })
					o.Redis = rc
				case "redis-down": // Redis 不可达：redisx 降级，向导仍然节流（本地计数）
					rc, err := redisx.New(ctxT(t), redisx.Config{Addr: "127.0.0.1:1", Env: "test"}, redisx.Options{Clock: clock.System()})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = rc.Close() })
					o.Redis = rc
				}
			})
			ops := w.allOps()
			// 缺少 token、错误 token：全部 9 个 operation 都是 401，且没有任何副作用（库与凭据文件都不存在）。
			for _, op := range ops[:2] { // 两次失败（上限 3）
				rr := w.do(op.method, api+op.path, "", op.body)
				if rr.Status != 401 || rr.errCode() != "setup.token_invalid" {
					t.Errorf("%s 缺少 token 应 401 setup.token_invalid: %d %s", op.path, rr.Status, rr.Raw)
				}
			}
			rr := w.do("GET", api+"status", "wrong-"+w.token, nil)
			if rr.Status != 401 {
				t.Errorf("错误 token 应 401: %d", rr.Status)
			}
			// 已有 3 次失败：来源被节流，此后携带**正确** token 也是 429，并带 Retry-After。
			rr = w.do("GET", api+"status", w.token, nil)
			if rr.Status != 429 || rr.errCode() != "rate_limit.exceeded" || rr.Header.Get("Retry-After") == "" {
				t.Errorf("达到失败上限后应 429 并带 Retry-After: %d %s hdr=%v", rr.Status, rr.Raw, rr.Header)
			}
			// 另一来源（可信反代的 XFF）不受影响：用正确 token 通过。
			req, _ := http.NewRequest("GET", w.srv.URL+api+"status", nil)
			req.Header.Set("X-Setup-Token", w.token)
			req.Header.Set("X-Forwarded-For", "198.51.100.7")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != 200 {
				t.Errorf("另一来源应不受节流影响: %d", res.StatusCode)
			}
			// 没有副作用：库、账号、凭据文件都不存在。
			if n := w.dbInt("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", w.dbName); n != 0 {
				t.Error("被拒绝的请求不得创建库")
			}
			if n := w.dbInt("SELECT COUNT(*) FROM mysql.user WHERE User = 'lingedge_app'"); n != 0 {
				t.Error("被拒绝的请求不得创建账号")
			}
		})
	}
}

// 每个 operation 都受保护：缺少 token → 401（逐个、各用独立来源，避免被节流打断）。
func TestEveryOperationRequiresToken(t *testing.T) {
	w := newWiz(t)
	for i, op := range w.allOps() {
		for _, tok := range []string{"", "bad"} {
			req, _ := http.NewRequest(op.method, w.srv.URL+api+op.path, strings.NewReader("{}"))
			if tok != "" {
				req.Header.Set("X-Setup-Token", tok)
			}
			req.Header.Set("X-Forwarded-For", "192.0.2."+string(rune('1'+i)))
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != 401 {
				t.Errorf("%s %s（token=%q）应 401，实际 %d", op.method, op.path, tok, res.StatusCode)
			}
		}
	}
	// 正确 token 的 status 可用，且响应 no-store。
	r := w.get("status")
	if r.Status != 200 || !strings.Contains(r.Header.Get("Cache-Control"), "no-store") {
		t.Errorf("正确 token 的 status: %d %v", r.Status, r.Header)
	}
}

// 公开路由的防滥用：每来源每分钟请求上限（即使 token 正确）。
func TestRequestRateLimit(t *testing.T) {
	w := newWiz(t, func(o *setup.Options) { o.RateMax = 5 })
	var last resp
	for i := 0; i < 6; i++ {
		last = w.get("status")
	}
	if last.Status != 429 || last.errCode() != "rate_limit.exceeded" {
		t.Errorf("超过请求上限应 429: %d %s", last.Status, last.Raw)
	}
}

// setup token 哈希文件缺失/损坏：fail-closed（任何 token 都 401）。
func TestTokenHashFileMissingIsFailClosed(t *testing.T) {
	w := newWiz(t)
	if err := removeFile(w.hashFile); err != nil {
		t.Fatal(err)
	}
	if r := w.get("status"); r.Status != 401 {
		t.Errorf("哈希文件缺失应 401: %d", r.Status)
	}
}

// ADR-026 §10：监听非回环且请求非 TLS 时，携带口令的接口返回 400 setup.insecure_transport；回环监听、TLS、来自可信代理的 X-Forwarded-Proto: https 放行。
func TestInsecureTransport(t *testing.T) {
	sensitive := func(w *wiz) []opCase {
		var out []opCase
		for _, op := range w.allOps() {
			switch op.path {
			case "database", "redis", "admin", "complete":
				out = append(out, op)
			}
		}
		return out
	}
	t.Run("非回环监听且非TLS被拒绝", func(t *testing.T) {
		w := newWiz(t, func(o *setup.Options) { o.ListenAddr = "0.0.0.0:8080" })
		for _, op := range sensitive(w) {
			r := w.do(op.method, api+op.path, w.token, op.body)
			if r.Status != 400 || r.errCode() != "setup.insecure_transport" {
				t.Errorf("%s 应 400 setup.insecure_transport: %d %s", op.path, r.Status, r.Raw)
			}
			w.assertBodyHasNoSecrets(r)
		}
		// 不携带口令的接口不受限。
		if r := w.get("status"); r.Status != 200 {
			t.Errorf("status 不受传输限制: %d", r.Status)
		}
		// 不可信来源伪造 X-Forwarded-Proto 无效：把可信代理改成别的网段后，127.0.0.1 的头不被采信。
		w.cfg.Server.TrustedProxies = []string{"10.0.0.0/8"}
		w.start()
		req, _ := http.NewRequest("PUT", w.srv.URL+api+"database", strings.NewReader("{}"))
		req.Header.Set("X-Setup-Token", w.token)
		req.Header.Set("X-Forwarded-Proto", "https")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 400 {
			t.Errorf("不可信来源的 X-Forwarded-Proto 不应被采信: %d", res.StatusCode)
		}
	})
	t.Run("可信反代的X-Forwarded-Proto放行", func(t *testing.T) {
		w := newWiz(t, func(o *setup.Options) { o.ListenAddr = "0.0.0.0:8080" })
		req, _ := http.NewRequest("PUT", w.srv.URL+api+"redis", strings.NewReader(`{"address":""}`))
		req.Header.Set("X-Setup-Token", w.token)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode == 400 {
			// 400 也可能来自校验失败（address 为空）：区分错误码。
			r := w.do("PUT", api+"redis", w.token, map[string]any{"address": ""})
			if r.errCode() == "setup.insecure_transport" {
				t.Errorf("TLS 终止（可信代理）应放行")
			}
		}
	})
	t.Run("回环监听不受限", func(t *testing.T) {
		w := newWiz(t)
		r := w.put("redis", map[string]any{"address": "bad"})
		if r.errCode() == "setup.insecure_transport" {
			t.Errorf("回环监听不应触发传输限制")
		}
	})
}
