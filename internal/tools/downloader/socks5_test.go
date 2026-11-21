package downloader_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
)

// 测试内实现的真实 SOCKS5 服务器（RFC 1928 / 1929）：真实 TCP 监听，真正把流量隧道到目标。
// 标准库 http.Transport 对 socks5/socks5h 代理已原生支持，下载器直接使用它，因此这里测的是「真实经过代理」。
type socks5Server struct {
	ln       net.Listener
	user, pw string            // 非空：要求用户名/口令认证（0x02）；空：无认证（0x00）
	resolve  map[string]string // 目标主机名 -> 真实地址（模拟「由代理解析域名」）
	denyAll  bool              // 认证后拒绝所有 CONNECT（REP=0x02 not allowed）

	mu       sync.Mutex
	dests    []string // 收到的 CONNECT 目标（host:port，域名原样）
	authOK   int
	authBad  int
	methods  [][]byte
	finished sync.WaitGroup
}

func newSocks5(t *testing.T, user, pw string) *socks5Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &socks5Server{ln: ln, user: user, pw: pw, resolve: map[string]string{}}
	t.Cleanup(func() { _ = ln.Close(); s.finished.Wait() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.finished.Add(1)
			go func() { defer s.finished.Done(); s.serve(c) }()
		}
	}()
	return s
}

func (s *socks5Server) addr() string { return s.ln.Addr().String() }

func (s *socks5Server) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return
	}
	ms := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, ms); err != nil {
		return
	}
	s.mu.Lock()
	s.methods = append(s.methods, ms)
	s.mu.Unlock()
	want := byte(0x00)
	if s.user != "" {
		want = 0x02
	}
	if !bytes.Contains(ms, []byte{want}) {
		_, _ = c.Write([]byte{5, 0xff})
		return
	}
	_, _ = c.Write([]byte{5, want})
	if want == 0x02 {
		h := make([]byte, 2)
		if _, err := io.ReadFull(c, h); err != nil || h[0] != 1 {
			return
		}
		u := make([]byte, h[1])
		if _, err := io.ReadFull(c, u); err != nil {
			return
		}
		pl := make([]byte, 1)
		if _, err := io.ReadFull(c, pl); err != nil {
			return
		}
		p := make([]byte, pl[0])
		if _, err := io.ReadFull(c, p); err != nil {
			return
		}
		ok := string(u) == s.user && string(p) == s.pw
		s.mu.Lock()
		if ok {
			s.authOK++
		} else {
			s.authBad++
		}
		s.mu.Unlock()
		if !ok {
			_, _ = c.Write([]byte{1, 1})
			return
		}
		_, _ = c.Write([]byte{1, 0})
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[0] != 5 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = string(b)
	case 4:
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		_, _ = c.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return
	}
	port := strconv.Itoa(int(binary.BigEndian.Uint16(pb)))
	dest := net.JoinHostPort(host, port)
	s.mu.Lock()
	s.dests = append(s.dests, dest)
	s.mu.Unlock()
	if req[1] != 1 { // 只支持 CONNECT
		_, _ = c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	if s.denyAll {
		_, _ = c.Write([]byte{5, 2, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	target := dest
	if real, ok := s.resolve[host]; ok {
		target = real
	}
	up, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = c.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0}) // host unreachable
		return
	}
	defer func() { _ = up.Close() }()
	_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func (s *socks5Server) seen() (dests []string, authOK, authBad int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.dests...), s.authOK, s.authBad
}

func socksURL(scheme, user, pw, host string) string {
	u := &url.URL{Scheme: scheme, Host: host}
	if user != "" {
		u.User = url.UserPassword(user, pw)
	}
	return u.String()
}

func TestSocks5NoAuthDownloadGoesThroughProxy(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 50_000)
	w.serveBytes("/blob", b)
	sp := newSocks5(t, "", "")
	for _, scheme := range []string{"socks5", "socks5h"} {
		d := dst(t)
		c := w.client(downloader.Config{ProxyURL: socksURL(scheme, "", "", sp.addr())})
		if err := c.Download(t.Context(), w.base+"/blob", d, exp); err != nil {
			t.Fatalf("%s：%v", scheme, err)
		}
		if !bytes.Equal(readFile(t, d), b) {
			t.Fatalf("%s：内容不符", scheme)
		}
	}
	dests, _, _ := sp.seen()
	if len(dests) < 2 || dests[0] != strings.TrimPrefix(w.base, "https://") {
		t.Fatalf("下载没有经过 socks5 代理：%v", dests)
	}
	// 小对象（清单）同样走代理。
	w.serveBytes("/m", []byte("{}"))
	before, _, _ := sp.seen()
	if _, err := w.client(downloader.Config{ProxyURL: socksURL("socks5", "", "", sp.addr())}).FetchBytes(t.Context(), w.base+"/m", 100); err != nil {
		t.Fatal(err)
	}
	if after, _, _ := sp.seen(); len(after) != len(before)+1 {
		t.Fatalf("FetchBytes 没有经过代理：%v -> %v", before, after)
	}
}

// 代理解析域名：目标主机名以域名（ATYP=3）交给代理，本机无法解析它——能成功只可能是经过了代理。
func TestSocks5ProxyResolvesHostname(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 20_000)
	w.serveBytes("/blob", b)
	sp := newSocks5(t, "", "")
	real := strings.TrimPrefix(w.base, "https://")
	_, port, _ := net.SplitHostPort(real)
	sp.resolve["example.com"] = real // httptest 证书包含 example.com
	c := w.client(downloader.Config{ProxyURL: socksURL("socks5h", "", "", sp.addr())})
	if err := c.Download(t.Context(), "https://example.com:"+port+"/blob", dst(t), exp); err != nil {
		t.Fatal(err)
	}
	if dests, _, _ := sp.seen(); len(dests) != 1 || dests[0] != "example.com:"+port {
		t.Fatalf("代理应收到域名目标：%v", dests)
	}
}

func TestSocks5Authentication(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 20_000)
	w.serveBytes("/blob", b)
	pw := "pw-" + randHex(t)
	sp := newSocks5(t, "bob", pw)

	// 正确凭据：成功，代理收到一次通过的认证。
	c := w.client(downloader.Config{ProxyURL: socksURL("socks5", "bob", pw, sp.addr())})
	if err := c.Download(t.Context(), w.base+"/blob", dst(t), exp); err != nil {
		t.Fatal(err)
	}
	if _, ok, bad := sp.seen(); ok == 0 || bad != 0 {
		t.Fatalf("认证计数 ok=%d bad=%d", ok, bad)
	}

	// 口令错误：失败（传输类，退出码 1），目标服务器零请求（不得回退直连），错误文本不泄露口令。
	hitsBefore := w.hitCount("/blob")
	var retries int
	bad := w.client(downloader.Config{ProxyURL: socksURL("socks5", "bob", "wrong-"+pw, sp.addr()), MaxAttempts: 3, OnRetry: func(int, error) { retries++ }})
	err := bad.Download(t.Context(), w.base+"/blob", dst(t), exp)
	wantKind(t, err, downloader.KindNetwork)
	if exitOf(err) != 1 {
		t.Fatalf("exit=%d", exitOf(err))
	}
	if strings.Contains(err.Error(), pw) || strings.Contains(err.Error(), "wrong-") {
		t.Fatalf("错误文本泄露了口令：%v", err)
	}
	if w.hitCount("/blob") != hitsBefore {
		t.Fatal("代理认证失败后不得回退为直连")
	}
	if _, _, nbad := sp.seen(); nbad != 3 || retries != 2 {
		t.Fatalf("应有 3 次尝试、2 次重试：bad=%d retries=%d", nbad, retries)
	}

	// 无凭据而代理要求认证：失败（代理不接受无认证方法）。
	err = w.client(downloader.Config{ProxyURL: socksURL("socks5", "", "", sp.addr()), MaxAttempts: 1}).Download(t.Context(), w.base+"/blob", dst(t), exp)
	wantKind(t, err, downloader.KindNetwork)
	if w.hitCount("/blob") != hitsBefore {
		t.Fatal("不得回退直连")
	}
}

func TestSocks5ProxyFailureClassification(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 20_000)
	w.serveBytes("/blob", b)

	// 代理不可达。
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := dead.Addr().String()
	_ = dead.Close()
	d := dst(t)
	err := w.client(downloader.Config{ProxyURL: socksURL("socks5", "", "", addr), MaxAttempts: 2}).Download(t.Context(), w.base+"/blob", d, exp)
	wantKind(t, err, downloader.KindNetwork)
	assertNoLeftovers(t, d)

	// 代理拒绝 CONNECT。
	sp := newSocks5(t, "", "")
	sp.denyAll = true
	err = w.client(downloader.Config{ProxyURL: socksURL("socks5", "", "", sp.addr()), MaxAttempts: 2}).Download(t.Context(), w.base+"/blob", d, exp)
	wantKind(t, err, downloader.KindNetwork)
	if exitOf(err) != 1 {
		t.Fatalf("exit=%d", exitOf(err))
	}

	// 代理到不了目标（host unreachable）。
	sp2 := newSocks5(t, "", "")
	sp2.resolve["127.0.0.1"] = addr // 把目标改写到已关闭的端口
	err = w.client(downloader.Config{ProxyURL: socksURL("socks5", "", "", sp2.addr()), MaxAttempts: 1}).Download(t.Context(), w.base+"/blob", d, exp)
	wantKind(t, err, downloader.KindNetwork)
	if w.hitCount("/blob") != 0 {
		t.Fatalf("代理失败时不得出现直连请求：%d", w.hitCount("/blob"))
	}

	// 经代理时 TLS 仍然端到端校验：目标证书不被信任 -> KindTLS（不重试），代理只是个字节管道。
	sp3 := newSocks5(t, "", "")
	c, err := downloader.New(downloader.Config{ProxyURL: socksURL("socks5", "", "", sp3.addr()), MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Download(t.Context(), w.base+"/blob", d, exp) // 未配置 CAFile：测试服务器证书不被信任
	wantKind(t, err, downloader.KindTLS)
	if dests, _, _ := sp3.seen(); len(dests) != 1 {
		t.Fatalf("证书失败不应重试：%v", dests)
	}
}

// 经 socks5 的完整 Acquire（清单+包）：校验链与直连一致。
func TestSocks5AcquireEndToEnd(t *testing.T) {
	w := newWorld(t)
	w.publish(scenario{pkg: goodPkg(t, w, nil)})
	sp := newSocks5(t, "", "")
	p := w.params()
	p.Online = &downloader.OnlineSource{Client: w.client(downloader.Config{ProxyURL: socksURL("socks5", "", "", sp.addr())}), ManifestURL: w.base + "/manifest.json"}
	if _, err := downloader.Acquire(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if dests, _, _ := sp.seen(); len(dests) == 0 {
		t.Fatal("Acquire 没有经过代理")
	}
}
