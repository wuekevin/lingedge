package downloader_test

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
)

// 下载器传输策略的集成测试：全部走真实的 TLS 服务器与真实的 TCP 连接（httptest.NewUnstartedServer + StartTLS），无 Mock。

func blob(t testing.TB, n int) ([]byte, downloader.Expected) {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b, downloader.Expected{Size: int64(n), SHA256: sha(b)}
}

// withUser 构造带用户信息的 URL（运行时拼装，源码里不出现内嵌口令的连接串字面量）。
func withUser(scheme, user, pw, host string) string {
	return (&url.URL{Scheme: scheme, User: url.UserPassword(user, pw), Host: host}).String()
}

func randHex(t testing.TB) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func dst(t testing.TB) string { return filepath.Join(t.TempDir(), "pkg.bin") }

func assertNoLeftovers(t testing.TB, d string) {
	t.Helper()
	for _, p := range []string{d, d + ".part"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("失败后不应遗留 %s", p)
		}
	}
}

func TestDownloadHappyPath(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 300<<10)
	w.serveBytes("/blob", b)
	d := dst(t)
	if err := w.client(downloader.Config{}).Download(t.Context(), w.base+"/blob", d, exp); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, d), b) {
		t.Fatal("下载内容不符")
	}
	if _, err := os.Stat(d + ".part"); err == nil {
		t.Fatal(".part 应在成功后被改名")
	}
}

func TestOnlyHTTPS(t *testing.T) {
	w := newWorld(t)
	_, exp := blob(t, 10)
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("不应有任何明文 HTTP 请求") }))
	defer plain.Close()
	c := w.client(downloader.Config{})
	d := dst(t)
	for _, u := range []string{plain.URL + "/x", "ftp://example.test/x", "file:///etc/passwd", "//example.test/x"} {
		err := c.Download(t.Context(), u, d, exp)
		if err == nil || (!downloader.IsKind(err, downloader.KindInsecureURL) && !downloader.IsKind(err, downloader.KindOptions)) {
			t.Fatalf("%s 应被拒绝：%v", u, err)
		}
		if _, err := c.FetchBytes(t.Context(), u, 100); err == nil {
			t.Fatalf("%s FetchBytes 应被拒绝", u)
		}
	}
	err := c.Download(t.Context(), plain.URL+"/x", d, exp)
	wantKind(t, err, downloader.KindInsecureURL)
	if exitOf(err) != 5 {
		t.Fatalf("退出码 = %d", exitOf(err))
	}
	// Acquire 的清单 URL 同样只允许 https，且不会发起任何请求。
	p := w.params()
	p.Online = &downloader.OnlineSource{Client: c, ManifestURL: plain.URL + "/manifest.json"}
	_, err = downloader.Acquire(t.Context(), p)
	wantKind(t, err, downloader.KindInsecureURL)
	// URL 带凭据拒绝。
	if err := downloader.CheckURL(withUser("https", "user", "pw-"+randHex(t), "example.test")); err == nil {
		t.Fatal("URL 不得含凭据")
	}
}

func TestNoInsecureSwitchExists(t *testing.T) {
	// 没有任何关闭证书校验的开关：Config 的字段名里没有 insecure/skip/verify 字样（编译期结构 + 此处守卫，防止日后加入）。
	typ := reflect.TypeOf(downloader.Config{})
	for i := 0; i < typ.NumField(); i++ {
		n := strings.ToLower(typ.Field(i).Name)
		for _, bad := range []string{"insecure", "skip", "verify", "plain", "http"} {
			if strings.Contains(n, bad) {
				t.Errorf("Config.%s 疑似关闭校验/放宽传输的开关", typ.Field(i).Name)
			}
		}
	}
}

func TestTLSTrust(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 1000)
	w.serveBytes("/blob", b)

	// 不给 CA：测试服务器的自签证书不在系统根里 → 证书失败（退出码 5），且不重试。
	var retries int
	c, err := downloader.New(downloader.Config{BackoffBase: time.Millisecond, OnRetry: func(int, error) { retries++ }})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = c.Download(t.Context(), w.base+"/blob", dst(t), exp)
	wantKind(t, err, downloader.KindTLS)
	if exitOf(err) != 5 || retries != 0 {
		t.Fatalf("证书失败应退出码 5 且不重试：exit=%d retries=%d", exitOf(err), retries)
	}

	// 给了「别的」CA：同样失败。
	c2, _ := downloader.New(downloader.Config{CAFile: selfSignedCAFile(t), BackoffBase: time.Millisecond})
	defer c2.Close()
	wantKind(t, c2.Download(t.Context(), w.base+"/blob", dst(t), exp), downloader.KindTLS)

	// 正确的 release.ca_file：成功。
	if err := w.client(downloader.Config{}).Download(t.Context(), w.base+"/blob", dst(t), exp); err != nil {
		t.Fatal(err)
	}
}

func TestCAFileValidation(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	write := func(name, content string, perm os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, perm); err != nil {
			t.Fatal(err)
		}
		return p
	}
	pemOK := string(readFile(t, w.caFile))
	for name, p := range map[string]string{
		"不存在":       filepath.Join(dir, "nope.pem"),
		"目录":        dir,
		"空文件":       write("empty.pem", "", 0o600),
		"不是 PEM":    write("junk.pem", "not a certificate", 0o600),
		"group 可写":  write("gw.pem", pemOK, 0o620),
		"other 可写":  write("ow.pem", pemOK, 0o602),
		"PEM 但无证书块": write("key.pem", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", 0o600),
	} {
		_, err := downloader.New(downloader.Config{CAFile: p})
		if err == nil || !downloader.IsKind(err, downloader.KindOptions) {
			t.Errorf("%s: release.ca_file 应被拒绝：%v", name, err)
		}
	}
	if _, err := downloader.New(downloader.Config{CAFile: write("ok.pem", pemOK, 0o644)}); err != nil {
		t.Errorf("合法 CA 文件应被接受：%v", err)
	}
}

func TestRedirects(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 2000)
	w.serveBytes("/blob", b)
	w.route("/r-https", func(rw http.ResponseWriter, r *http.Request) { http.Redirect(rw, r, "/blob", http.StatusFound) })
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { plainHits.Add(1); _, _ = rw.Write(b) }))
	defer plain.Close()
	w.route("/r-http", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, plain.URL+"/blob", http.StatusFound)
	})
	w.route("/loop", func(rw http.ResponseWriter, r *http.Request) { http.Redirect(rw, r, "/loop", http.StatusFound) })
	c := w.client(downloader.Config{MaxAttempts: 2})

	// https → https：允许。
	if err := c.Download(t.Context(), w.base+"/r-https", dst(t), exp); err != nil {
		t.Fatal(err)
	}
	// https → http：拒绝，明文服务器没有收到任何请求，退出码 5，不重试。
	d := dst(t)
	err := c.Download(t.Context(), w.base+"/r-http", d, exp)
	wantKind(t, err, downloader.KindInsecureURL)
	if exitOf(err) != 5 || plainHits.Load() != 0 {
		t.Fatalf("重定向降级必须拒绝且不访问明文端：exit=%d plainHits=%d", exitOf(err), plainHits.Load())
	}
	if w.hitCount("/r-http") != 1 {
		t.Fatalf("策略失败不应重试：%d", w.hitCount("/r-http"))
	}
	assertNoLeftovers(t, d)
	// 同样适用于小对象（清单）。
	_, err = c.FetchBytes(t.Context(), w.base+"/r-http", 1<<20)
	wantKind(t, err, downloader.KindInsecureURL)
	// 重定向环：超过上限即失败。
	err = c.Download(t.Context(), w.base+"/loop", dst(t), exp)
	wantKind(t, err, downloader.KindNetwork)
}

func TestSizeCapDeclaredAndActual(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 50_000)
	c := w.client(downloader.Config{MaxAttempts: 3})

	// 声明（Content-Length）超过清单 size：在读取任何内容前即中止，不重试。
	w.route("/declared-big", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Length", fmt.Sprint(len(b)+1000))
		rw.WriteHeader(200)
		_, _ = rw.Write(b)
	})
	d := dst(t)
	err := c.Download(t.Context(), w.base+"/declared-big", d, exp)
	wantKind(t, err, downloader.KindSize)
	if exitOf(err) != 5 || w.hitCount("/declared-big") != 1 {
		t.Fatalf("exit=%d hits=%d", exitOf(err), w.hitCount("/declared-big"))
	}
	assertNoLeftovers(t, d)

	// 声明被省略（分块传输），实际流超过清单 size：写入量封顶在 size，随后中止。
	var sent atomic.Int64
	w.route("/actual-big", func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(200)
		chunk := make([]byte, 4096)
		for i := 0; i < 2000; i++ { // 最多 8 MB，远大于清单 size（50 KB）
			n, err := rw.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
			rw.(http.Flusher).Flush()
		}
	})
	d = dst(t)
	err = c.Download(t.Context(), w.base+"/actual-big", d, exp)
	wantKind(t, err, downloader.KindSize)
	assertNoLeftovers(t, d)
	if s := sent.Load(); s > 4<<20 {
		t.Errorf("服务器写出了 %d 字节：客户端应在超过清单 size 后很快断开", s)
	}

	// 声明小于清单 size（截断声明）：失败。
	w.route("/declared-small", func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write(b[:len(b)-5]) })
	d = dst(t)
	wantKind(t, c.Download(t.Context(), w.base+"/declared-small", d, exp), downloader.KindSize)
	assertNoLeftovers(t, d)

	// 小对象（清单）同样受上限约束：声明与实际。
	w.route("/mf-big", func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write(make([]byte, 5000)) })
	_, err = c.FetchBytes(t.Context(), w.base+"/mf-big", 1000)
	wantKind(t, err, downloader.KindSize)
	w.route("/mf-chunked", func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(200)
		for i := 0; i < 100; i++ {
			if _, err := rw.Write(make([]byte, 1000)); err != nil {
				return
			}
			rw.(http.Flusher).Flush()
		}
	})
	_, err = c.FetchBytes(t.Context(), w.base+"/mf-chunked", 1000)
	wantKind(t, err, downloader.KindSize)
}

func TestStreamingSHA256Mismatch(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 100_000)
	w.serveBytes("/blob", flip(b, 77)) // 等长篡改
	d := dst(t)
	c := w.client(downloader.Config{MaxAttempts: 3})
	err := c.Download(t.Context(), w.base+"/blob", d, exp)
	wantKind(t, err, downloader.KindSHA256)
	if exitOf(err) != 5 || w.hitCount("/blob") != 1 {
		t.Fatalf("SHA256 不符不得重试：exit=%d hits=%d", exitOf(err), w.hitCount("/blob"))
	}
	assertNoLeftovers(t, d)
}

// hijackCut 先发出 body[:n]（声明完整 Content-Length）再硬关闭连接。
func hijackCut(rw http.ResponseWriter, body []byte, n int) {
	rw.Header().Set("Content-Length", fmt.Sprint(len(body)))
	rw.WriteHeader(200)
	_, _ = rw.Write(body[:n])
	rw.(http.Flusher).Flush()
	conn, _, err := rw.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func TestTruncatedAlwaysExhaustsRetries(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 80_000)
	w.route("/blob", func(rw http.ResponseWriter, r *http.Request) { hijackCut(rw, b, 1000) })
	var retries int
	c := w.client(downloader.Config{MaxAttempts: 3, OnRetry: func(int, error) { retries++ }})
	d := dst(t)
	err := c.Download(t.Context(), w.base+"/blob", d, exp)
	wantKind(t, err, downloader.KindNetwork) // 连接被掐断：传输类失败（Content-Length 已声明为完整长度）
	if w.hitCount("/blob") != 3 || retries != 2 {
		t.Fatalf("应尝试 3 次、重试 2 次：hits=%d retries=%d", w.hitCount("/blob"), retries)
	}
	assertNoLeftovers(t, d)
}

func TestTruncatedCleanEOFIsSizeFailure(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 80_000)
	// 分块传输，发了一半就正常结束（无 Content-Length，干净 EOF）：这是「截断」→ 退出码 5。
	w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(200)
		_, _ = rw.Write(b[:len(b)/2])
	})
	c := w.client(downloader.Config{MaxAttempts: 2})
	d := dst(t)
	err := c.Download(t.Context(), w.base+"/blob", d, exp)
	wantKind(t, err, downloader.KindSize)
	if exitOf(err) != 5 {
		t.Fatalf("exit=%d", exitOf(err))
	}
	// 第二次尝试是续传：已收到的前半段作为起点。
	hs := w.rangeHeaders("/blob")
	if len(hs) != 2 || hs[0] != "" || !strings.HasPrefix(hs[1], "bytes=40000-") {
		t.Fatalf("Range 头 = %q", hs)
	}
	assertNoLeftovers(t, d)
}

func TestResumeAfterCutThenVerify(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 120_000)
	var n atomic.Int32
	w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			hijackCut(rw, b, 50_000)
			return
		}
		http.ServeContent(rw, r, "blob", time.Time{}, bytes.NewReader(b)) // 标准库实现 Range → 206
	})
	d := dst(t)
	var retries int
	c := w.client(downloader.Config{OnRetry: func(int, error) { retries++ }})
	if err := c.Download(t.Context(), w.base+"/blob", d, exp); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, d), b) {
		t.Fatal("续传后的内容不符")
	}
	hs := w.rangeHeaders("/blob")
	if len(hs) != 2 || hs[1] != "bytes=50000-" || retries != 1 {
		t.Fatalf("应有一次续传 bytes=50000-：%q retries=%d", hs, retries)
	}
	assertNoLeftovers(t, d+".x")
}

func TestResumeTamperedSecondHalfFails(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 120_000)
	var n atomic.Int32
	w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			hijackCut(rw, b, 50_000)
			return
		}
		tam := flip(b, 90_000) // 后半段被篡改
		http.ServeContent(rw, r, "blob", time.Time{}, bytes.NewReader(tam))
	})
	d := dst(t)
	err := w.client(downloader.Config{}).Download(t.Context(), w.base+"/blob", d, exp)
	wantKind(t, err, downloader.KindSHA256)
	if exitOf(err) != 5 {
		t.Fatalf("exit=%d", exitOf(err))
	}
	assertNoLeftovers(t, d) // 篡改的续传不得被合并成最终文件
}

func TestResumeTamperedFirstHalfFails(t *testing.T) {
	// 已落盘的前半段被篡改（磁盘/中间人），续传后半段是诚实的：从磁盘重新计算哈希，所以整体 SHA256 仍不符。
	w := newWorld(t)
	b, exp := blob(t, 120_000)
	var n atomic.Int32
	w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			hijackCut(rw, flip(b, 10), 50_000) // 前 50000 字节里第 10 字节被翻转
			return
		}
		http.ServeContent(rw, r, "blob", time.Time{}, bytes.NewReader(b))
	})
	d := dst(t)
	err := w.client(downloader.Config{}).Download(t.Context(), w.base+"/blob", d, exp)
	wantKind(t, err, downloader.KindSHA256)
	assertNoLeftovers(t, d)
}

func TestResumeServerQuirks(t *testing.T) {
	b, exp := blob(t, 60_000)
	for name, second := range map[string]func(rw http.ResponseWriter, r *http.Request){
		"服务器无视 Range 返回 200 整包": func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write(b) },
		"416 后整包重来": func(rw http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Range") != "" {
				rw.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			_, _ = rw.Write(b)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			var n atomic.Int32
			w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
				if n.Add(1) == 1 {
					hijackCut(rw, b, 20_000)
					return
				}
				second(rw, r)
			})
			d := dst(t)
			if err := w.client(downloader.Config{MaxAttempts: 4}).Download(t.Context(), w.base+"/blob", d, exp); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFile(t, d), b) {
				t.Fatal("内容不符")
			}
		})
	}
	for name, cr := range map[string]string{
		"起点不符":            "bytes 0-59999/60000",
		"总长不符":            "bytes 20000-59999/99999",
		"终点不符":            "bytes 20000-30000/60000",
		"缺 Content-Range": "",
	} {
		t.Run("坏 206："+name, func(t *testing.T) {
			w := newWorld(t)
			var n atomic.Int32
			w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
				if n.Add(1) == 1 {
					hijackCut(rw, b, 20_000)
					return
				}
				if cr != "" {
					rw.Header().Set("Content-Range", cr)
				}
				rw.WriteHeader(http.StatusPartialContent)
				_, _ = rw.Write(b[20_000:])
			})
			d := dst(t)
			err := w.client(downloader.Config{}).Download(t.Context(), w.base+"/blob", d, exp)
			wantKind(t, err, downloader.KindSize)
			assertNoLeftovers(t, d)
		})
	}
}

func TestStatusAndRetry(t *testing.T) {
	b, exp := blob(t, 5000)
	t.Run("503 后恢复", func(t *testing.T) {
		w := newWorld(t)
		var n atomic.Int32
		w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
			if n.Add(1) <= 2 {
				rw.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = rw.Write(b)
		})
		var retries int
		if err := w.client(downloader.Config{OnRetry: func(int, error) { retries++ }}).Download(t.Context(), w.base+"/blob", dst(t), exp); err != nil {
			t.Fatal(err)
		}
		if retries != 2 {
			t.Fatalf("retries=%d", retries)
		}
	})
	t.Run("429 可重试", func(t *testing.T) {
		w := newWorld(t)
		var n atomic.Int32
		w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				rw.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = rw.Write(b)
		})
		if err := w.client(downloader.Config{}).Download(t.Context(), w.base+"/blob", dst(t), exp); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("404 不重试", func(t *testing.T) {
		w := newWorld(t)
		err := w.client(downloader.Config{MaxAttempts: 5}).Download(t.Context(), w.base+"/missing", dst(t), exp)
		wantKind(t, err, downloader.KindNetwork)
		if w.hitCount("/missing") != 1 || exitOf(err) != 1 {
			t.Fatalf("hits=%d exit=%d", w.hitCount("/missing"), exitOf(err))
		}
	})
	t.Run("500 重试耗尽", func(t *testing.T) {
		w := newWorld(t)
		w.route("/blob", func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(500) })
		err := w.client(downloader.Config{MaxAttempts: 3}).Download(t.Context(), w.base+"/blob", dst(t), exp)
		wantKind(t, err, downloader.KindNetwork)
		var de *downloader.Error
		_ = errors.As(err, &de)
		if w.hitCount("/blob") != 3 || de.Details["attempts"] != 3 || de.Details["status"] != 500 {
			t.Fatalf("hits=%d details=%v", w.hitCount("/blob"), de.Details)
		}
	})
	t.Run("退避期间取消", func(t *testing.T) {
		w := newWorld(t)
		w.route("/blob", func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(500) })
		ctx, cancel := contextWithCancel(t)
		c := w.client(downloader.Config{MaxAttempts: 5, BackoffBase: time.Hour, BackoffMax: time.Hour})
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		err := c.Download(ctx, w.base+"/blob", dst(t), exp)
		if !errors.Is(err, errCanceled) {
			t.Fatalf("取消应原样返回 context.Canceled：%v", err)
		}
	})
}

func TestTimeoutsAndSlowServers(t *testing.T) {
	b, exp := blob(t, 40_000)
	t.Run("响应头迟迟不来", func(t *testing.T) {
		w := newWorld(t)
		w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		})
		start := time.Now()
		err := w.client(downloader.Config{HeaderTimeout: 60 * time.Millisecond, MaxAttempts: 2}).Download(t.Context(), w.base+"/blob", dst(t), exp)
		wantKind(t, err, downloader.KindNetwork)
		if time.Since(start) > 1500*time.Millisecond {
			t.Fatalf("响应头超时没有生效：%s", time.Since(start))
		}
	})
	t.Run("空闲超时后续传成功", func(t *testing.T) {
		w := newWorld(t)
		var n atomic.Int32
		w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 { // 发一半后卡住
				rw.Header().Set("Content-Length", fmt.Sprint(len(b)))
				rw.WriteHeader(200)
				_, _ = rw.Write(b[:15_000])
				rw.(http.Flusher).Flush()
				select {
				case <-time.After(3 * time.Second):
				case <-r.Context().Done():
				}
				return
			}
			http.ServeContent(rw, r, "blob", time.Time{}, bytes.NewReader(b))
		})
		d := dst(t)
		if err := w.client(downloader.Config{IdleTimeout: 100 * time.Millisecond}).Download(t.Context(), w.base+"/blob", d, exp); err != nil {
			t.Fatal(err)
		}
		if hs := w.rangeHeaders("/blob"); len(hs) != 2 || hs[1] != "bytes=15000-" {
			t.Fatalf("Range = %q", hs)
		}
	})
	t.Run("一直卡住：重试耗尽", func(t *testing.T) {
		w := newWorld(t)
		w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Length", fmt.Sprint(len(b)))
			rw.WriteHeader(200)
			_, _ = rw.Write(b[:100])
			rw.(http.Flusher).Flush()
			select {
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
			}
		})
		d := dst(t)
		err := w.client(downloader.Config{IdleTimeout: 80 * time.Millisecond, MaxAttempts: 2}).Download(t.Context(), w.base+"/blob", d, exp)
		wantKind(t, err, downloader.KindNetwork)
		assertNoLeftovers(t, d)
	})
	t.Run("慢速滴灌：单次尝试超时", func(t *testing.T) {
		w := newWorld(t)
		w.route("/blob", func(rw http.ResponseWriter, r *http.Request) { // 每 10ms 一个字节，永不触发空闲超时
			rw.Header().Set("Content-Length", fmt.Sprint(len(b)))
			rw.WriteHeader(200)
			for i := 0; i < len(b); i++ {
				if _, err := rw.Write(b[i : i+1]); err != nil {
					return
				}
				rw.(http.Flusher).Flush()
				time.Sleep(10 * time.Millisecond)
			}
		})
		start := time.Now()
		err := w.client(downloader.Config{IdleTimeout: 2 * time.Second, AttemptTimeout: 200 * time.Millisecond, MaxAttempts: 1}).Download(t.Context(), w.base+"/blob", dst(t), exp)
		wantKind(t, err, downloader.KindNetwork)
		if time.Since(start) > 3*time.Second {
			t.Fatalf("单次尝试超时没有生效：%s", time.Since(start))
		}
	})
	t.Run("总时长上限", func(t *testing.T) {
		w := newWorld(t)
		w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Length", fmt.Sprint(len(b)))
			rw.WriteHeader(200)
			for i := 0; i < len(b); i++ {
				if _, err := rw.Write(b[i : i+1]); err != nil {
					return
				}
				rw.(http.Flusher).Flush()
				time.Sleep(5 * time.Millisecond)
			}
		})
		err := w.client(downloader.Config{IdleTimeout: 2 * time.Second, TotalTimeout: 250 * time.Millisecond, MaxAttempts: 10}).Download(t.Context(), w.base+"/blob", dst(t), exp)
		wantKind(t, err, downloader.KindNetwork)
	})
	t.Run("小对象（清单）同样有空闲超时", func(t *testing.T) {
		w := newWorld(t)
		w.route("/m", func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Length", "1000")
			rw.WriteHeader(200)
			_, _ = rw.Write([]byte("x"))
			rw.(http.Flusher).Flush()
			select {
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
			}
		})
		_, err := w.client(downloader.Config{IdleTimeout: 80 * time.Millisecond, MaxAttempts: 2}).FetchBytes(t.Context(), w.base+"/m", 4096)
		wantKind(t, err, downloader.KindNetwork)
	})
}

// connectProxy 是真实的 HTTP CONNECT 代理：隧道到目标并记录。
type connectProxy struct {
	ln       net.Listener
	mu       sync.Mutex
	connects []string
	auth     []string
}

func newConnectProxy(t *testing.T) *connectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &connectProxy{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return p
}

func (p *connectProxy) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		_, _ = c.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
		return
	}
	p.mu.Lock()
	p.connects = append(p.connects, req.Host)
	p.auth = append(p.auth, req.Header.Get("Proxy-Authorization"))
	p.mu.Unlock()
	up, err := net.Dial("tcp", req.Host)
	if err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer func() { _ = up.Close() }()
	_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func (p *connectProxy) seen() ([]string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.connects...), append([]string(nil), p.auth...)
}

func TestProxy(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 30_000)
	w.serveBytes("/blob", b)
	px := newConnectProxy(t)

	proxyPW := "pw-" + randHex(t) // 运行时随机生成
	// 显式代理（带凭据）：请求经 CONNECT 隧道到达，凭据以 Proxy-Authorization 发送。
	c := w.client(downloader.Config{ProxyURL: withUser("http", "alice", proxyPW, px.ln.Addr().String())})
	if err := c.Download(t.Context(), w.base+"/blob", dst(t), exp); err != nil {
		t.Fatal(err)
	}
	cons, auth := px.seen()
	if len(cons) == 0 || cons[0] != strings.TrimPrefix(w.base, "https://") || !strings.HasPrefix(auth[0], "Basic ") {
		t.Fatalf("代理未收到预期的 CONNECT：%v %v", cons, auth)
	}

	// 代理不可达：传输类失败；错误文本不得泄露代理口令。
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := dead.Addr().String()
	_ = dead.Close()
	c2 := w.client(downloader.Config{ProxyURL: withUser("http", "alice", proxyPW, addr), MaxAttempts: 2})
	err := c2.Download(t.Context(), w.base+"/blob", dst(t), exp)
	wantKind(t, err, downloader.KindNetwork)
	if strings.Contains(err.Error(), proxyPW) {
		t.Fatalf("错误文本泄露了代理口令：%v", err)
	}

	// 代理配置校验。
	for _, bad := range []string{"://x", "ftp://proxy:1", "http://", "proxy-without-scheme"} {
		if _, err := downloader.New(downloader.Config{ProxyURL: bad}); !downloader.IsKind(err, downloader.KindOptions) {
			t.Errorf("代理 %q 应被拒绝：%v", bad, err)
		}
	}
	if _, err := downloader.New(downloader.Config{ProxyURL: "socks5://127.0.0.1:1080"}); err != nil {
		t.Errorf("socks5 代理应被接受：%v", err)
	}
}

// 未显式配置代理时遵循 HTTPS_PROXY/NO_PROXY（Go 在进程内只读取一次环境变量，故在子进程里验证；
// 回环地址按 Go 约定永不走代理，所以用非回环主机名 releases.example.invalid）。
func TestProxyFromEnvironment(t *testing.T) {
	if addr := os.Getenv("LINGEDGE_TEST_PROXY_CHILD"); addr != "" {
		_, exp := blob(t, 100)
		c, err := downloader.New(downloader.Config{MaxAttempts: 1})
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Download(t.Context(), "https://releases.example.invalid/x", dst(t), exp) // 代理会尝试连接目标并返回 502
		return
	}
	px := newConnectProxy(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestProxyFromEnvironment$")
	cmd.Env = append(os.Environ(), "LINGEDGE_TEST_PROXY_CHILD="+px.ln.Addr().String(),
		"HTTPS_PROXY=http://"+px.ln.Addr().String(), "https_proxy=http://"+px.ln.Addr().String(), "NO_PROXY=", "no_proxy=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("子进程失败：%v\n%s", err, out)
	}
	cons, _ := px.seen()
	if len(cons) != 1 || cons[0] != "releases.example.invalid:443" {
		t.Fatalf("环境变量代理未被使用：%v", cons)
	}
}

// 离线路径零网络调用：见 TestOfflineMakesNoNetworkCalls（bundle_test.go）。
