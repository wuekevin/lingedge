package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.40：静态前端从「发行流水线」到「Tools 安装」到「真实 Nginx 托管」的端到端证据。
//
//	真实：scripts/release 打包前端（admin-web 在存在 frontend/dist/admin 时是真实的 Admin 生产构建，否则夹具；website 为夹具）→ 离线签名 → Tools 的验签链与
//	      生产的静态前端 ComponentSpec 经 installer 安装 → 安装器生成的站点片段由本机真实 nginx 加载并提供 HTTP/HTTPS 服务，逐项断言响应。
//	伪的只有「操作系统」（toolsworld 的 useradd/chown/systemctl 伪二进制，静态前端不用）与 API 上游（一个 httptest 服务，用来观察 Nginx 转发了什么——它不是 lingedge-server）。
//	本机没有 nginx 时 Nginx 相关用例以「等待环境验证」跳过（LINGEDGE_REQUIRE_NGINX=1 则视为失败）；跳过不代表通过。

func installWorld(t *testing.T, f *flow) (*toolsworld.World, func(comp string, set map[string]string, mut ...func(*installer.Request)) (*installer.Result, error)) {
	t.Helper()
	w, err := toolsworld.NewWorld(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keysDir := filepath.Join(w.Root, "keys.d")
	deps := func(d *installer.Deps) {
		d.Specs = installer.ProductionSpecs()
		d.Verifier = func() (*manifest.Verifier, error) {
			return newVerifier(t, f.keys, keysDir, w.Layout.SequenceFile(), time.Now().UTC()), nil
		}
	}
	return w, func(comp string, set map[string]string, mut ...func(*installer.Request)) (*installer.Result, error) {
		in := w.NewInstaller("run-fe", deps)
		task, err := in.Deps().Reg.Begin(t.Context(), registry.TaskRequest{Components: []string{comp}})
		if err != nil {
			return nil, err
		}
		defer task.Release()
		req := installer.Request{Component: comp, Channel: "stable", Offline: f.pub("lingedge-offline-" + relVer + ".tar"), NonInteractive: true, Settings: set}
		if comp == "website" {
			req.Variant = "static"
		}
		for _, m := range mut {
			m(&req)
		}
		return in.Install(t.Context(), task, req)
	}
}

func TestFrontendPackagesInPublishedRelease(t *testing.T) {
	f := sharedFlow(t)
	if realAdminDist {
		t.Log("admin-web 包来自真实的 frontend/dist/admin")
	} else {
		t.Log("未发现 frontend/dist/admin：admin-web 包使用夹具（先运行 make verify-build 可使用真实 Admin 构建）")
	}
	m, err := manifest.ParseManifest(readFile(t, f.pub("stable/manifest.json")))
	if err != nil {
		t.Fatal(err)
	}
	for comp, variant := range map[string]string{"admin-web": "default", "website": "static"} {
		ve := m.Components[comp].Versions[0]
		v, ok := ve.Variants[variant]
		if !ok || len(v.Artifacts) != 1 {
			t.Fatalf("%s 应只有变体 %s 且只有一个 artifact: %+v", comp, variant, ve.Variants)
		}
		a, ok := v.Artifacts["any"]
		if !ok {
			t.Fatalf("%s 的平台应为 any: %+v", comp, v.Artifacts)
		}
		if !strings.HasPrefix(a.URL, baseURL+"/"+comp+"/"+relVer+"/") {
			t.Errorf("%s 的 URL 不在发行源下: %s", comp, a.URL)
		}
	}

	// 包内布局：payload/public/**（含预压缩 .gz）+ share/lingedge-sbom.cdx.json，且没有别的顶层目录。
	pkg := filepath.Join(f.out, "publish", "admin-web", relVer, "lingedge-admin-web-"+relVer+".tar.gz")
	files := readTarGz(t, pkg)
	tops := map[string]bool{}
	for name := range files {
		if strings.HasPrefix(name, "payload/") {
			tops[strings.Split(name, "/")[1]] = true
		}
	}
	if len(tops) != 2 || !tops["public"] || !tops["share"] {
		t.Errorf("包布局应只含 payload/public 与 payload/share: %v", tops)
	}
	if _, ok := files["payload/public/index.html"]; !ok {
		t.Fatal("包内缺少 payload/public/index.html")
	}
	gzN := 0
	for name, body := range files {
		if strings.HasSuffix(name, ".gz") && strings.HasPrefix(name, "payload/public/") {
			gzN++
			orig, ok := files[strings.TrimSuffix(name, ".gz")]
			if !ok {
				t.Errorf("%s 没有对应的原文件", name)
				continue
			}
			zr, err := gzip.NewReader(bytes.NewReader(body))
			if err != nil {
				t.Errorf("%s 不是合法 gzip: %v", name, err)
				continue
			}
			got, _ := io.ReadAll(zr)
			if !bytes.Equal(got, orig) {
				t.Errorf("%s 解压后与原文件不一致", name)
			}
			if len(body) >= len(orig) {
				t.Errorf("%s 没有比原文件更小，不应生成", name)
			}
		}
	}
	if gzN == 0 {
		t.Error("没有任何预压缩 .gz（gzip_static 将无资源可用）")
	}
	for name := range files {
		if strings.HasSuffix(name, ".png.gz") || strings.HasSuffix(name, ".woff2.gz") {
			t.Errorf("已压缩的资源不应再预压缩: %s", name)
		}
	}

	// SBOM 与 build-record 登记了锁文件哈希与前端目录摘要。
	var rec struct {
		NPM       string `json:"npm"`
		Frontends []struct {
			Component     string `json:"component"`
			Variant       string `json:"variant"`
			Files         int    `json:"files"`
			Precompressed int    `json:"precompressed"`
			TreeSHA256    string `json:"tree_sha256"`
			MockCheck     string `json:"mock_check"`
		} `json:"frontends"`
	}
	if err := json.Unmarshal(readFile(t, f.pub("build-record.json")), &rec); err != nil {
		t.Fatal(err)
	}
	lock := sha256Of(t, filepath.Join(f.src, "frontend", "pnpm-lock.yaml"))
	if !strings.Contains(rec.NPM, "sha256="+lock) {
		t.Errorf("build-record.npm 没有登记 pnpm-lock.yaml 的哈希 %s: %q", lock, rec.NPM)
	}
	if len(rec.Frontends) != 2 {
		t.Fatalf("build-record.frontends 应有 2 项: %+v", rec.Frontends)
	}
	for _, fr := range rec.Frontends {
		if fr.MockCheck != "pass" || fr.TreeSHA256 == "" || fr.Files == 0 || fr.Precompressed == 0 {
			t.Errorf("前端记录不完整: %+v", fr)
		}
	}
	if !bytes.Contains(files["payload/share/lingedge-sbom.cdx.json"], []byte("release:pnpm-lock-sha256")) ||
		!bytes.Contains(files["payload/share/lingedge-sbom.cdx.json"], []byte(lock)) {
		t.Error("包内 SBOM 没有登记 pnpm-lock.yaml 的哈希")
	}
}

func sha256Of(t testing.TB, p string) string {
	t.Helper()
	return sha256Hex(readFile(t, p))
}

func readTarGz(t testing.TB, p string) map[string][]byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(readFile(t, p)))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			out[h.Name] = b
		}
	}
	return out
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// 前端目录的流水线守卫：无 index.html、含 mock 入口都必须让 build 失败（用同一个二进制，不构建任何 Go 目标——守卫在编译之前）。
func TestFrontendDistGuards(t *testing.T) {
	if testing.Short() {
		t.Skip("需要完整源仓库副本与流水线二进制")
	}
	rel, _ := tools(t)
	_, _, keys := keypair(t, "guard") // 通过发行公钥守卫，才能走到前端目录检查
	src := srcCopy(t, keys)
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"没有 index.html", map[string]string{"assets/a.js": "x"}, "缺少 index.html"},
		{"含 mock 入口标记", map[string]string{"index.html": "<html></html>", "assets/m.js": "console.warn('LE_MOCK_ENTRY active')"}, "mock"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		for rel, body := range c.files {
			p := filepath.Join(dir, filepath.FromSlash(rel))
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		out, err := runIn(src, nil, rel, "build", "-src", src, "-out", filepath.Join(t.TempDir(), "o"), "-version", relVer, "-frontend-dist", "admin-web="+dir)
		if err == nil || !strings.Contains(string(out), c.want) {
			t.Errorf("%s：build 应失败并提到 %q，得 err=%v\n%s", c.name, c.want, err, out)
		}
	}
	// 非法的组件名/重复/格式。
	for _, arg := range []string{"nope=/tmp/x", "admin-web", "admin-web="} {
		out, err := runIn(src, nil, rel, "build", "-src", src, "-out", filepath.Join(t.TempDir(), "o"), "-version", relVer, "-frontend-dist", arg)
		if err == nil {
			t.Errorf("-frontend-dist %q 应被拒绝\n%s", arg, out)
		}
	}
}

// 回归（Story 1.42 排查的偶发失败的根因）：前端目录在「校验通过」与「打包」之间被改动——典型场景是并发的 `vite build`（frontend/admin/vite.config.ts 的
// emptyOutDir: true 会先清空 frontend/dist/admin）。流水线在 build 的第 3 步校验目录（index.html、mock 入口），随后要编译 Go 目标（数十秒），
// 第 7 步才从同一个活目录打包；旧实现中间没有任何保护，于是包里缺少 payload/public/index.html，且流水线不报错。
// 本用例在 Go 编译期间把活目录清空、再恢复：流水线必须产出完整的包（实现：开始即把目录快照到 out/work/frontend 并核对复制前后目录摘要一致，之后只读快照）。
func TestFrontendDistMutatedDuringBuildStillYieldsIntactPackage(t *testing.T) {
	if testing.Short() {
		t.Skip("需要完整源仓库副本与流水线二进制")
	}
	f := newFlow(t, "fe-race")
	dist := frontendFixture(t.TempDir(), "admin-web")
	want := readFile(t, filepath.Join(dist, "index.html"))
	out := filepath.Join(t.TempDir(), "out")
	stop := make(chan struct{})
	done := make(chan struct{})
	removed := make(chan bool, 1)
	go func() {
		defer close(done)
		// 观察到 Go 编译开始（work/build-a 出现）后清空活目录；之后保持清空直到 stage 出现或测试结束，再恢复（模拟 vite build 先清空再重建）。
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := os.Stat(filepath.Join(out, "work", "build-a")); err == nil {
				_ = os.Remove(filepath.Join(dist, "index.html"))
				removed <- true
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := os.Stat(filepath.Join(out, "stage")); err == nil {
						time.Sleep(50 * time.Millisecond)
						return
					}
					time.Sleep(2 * time.Millisecond)
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	outStr, err := f.run(f.release, "build", "-src", f.src, "-out", out, "-version", relVer, "-platforms", "linux-amd64", "-repro-fresh-cache=false", "-frontend-dist", "admin-web="+dist)
	close(stop)
	<-done
	select {
	case <-removed:
	default:
		t.Fatalf("竞态未被制造出来（没有观察到 Go 编译开始）：测试无效\n%s", outStr)
	}
	if err != nil {
		t.Fatalf("流水线应基于开始时的快照产出完整的包：%v\n%s", err, outStr)
	}
	got, rerr := os.ReadFile(filepath.Join(out, "stage", "admin-web-"+relVer+"-any", "payload", "public", "index.html"))
	if rerr != nil || !bytes.Equal(got, want) {
		t.Fatalf("包内 payload/public/index.html 缺失或内容不符：%v", rerr)
	}
}

// ---- 安装（发行包 → Tools 验签链 → 生产静态前端规格）----

func TestInstallFrontendFromPublishedRelease(t *testing.T) {
	f := sharedFlow(t)
	w, install := installWorld(t, f)
	res, err := install("admin-web", map[string]string{"host": "admin.example.com"})
	if err != nil {
		t.Fatalf("install admin-web: %v", err)
	}
	if res.Outcome != installer.OutcomeInstalled || len(res.Components) != 1 || res.Components[0].State != "installed" {
		t.Fatalf("结果异常: %+v", res)
	}
	cur := filepath.Join(w.Root, "opt/lingedge/admin-web/current")
	if tg, err := os.Readlink(cur); err != nil || tg != "releases/"+relVer {
		t.Fatalf("current 应原子指向 releases/%s: %q %v", relVer, tg, err)
	}
	real, err := os.ReadFile(filepath.Join(cur, "public", "index.html"))
	if err != nil || len(real) == 0 {
		t.Fatalf("public/index.html 缺失: %v", err)
	}
	gz, err := os.ReadFile(filepath.Join(cur, "public", "index.html.gz"))
	if err != nil {
		t.Fatalf("预压缩 index.html.gz 缺失: %v", err)
	}
	if zr, err := gzip.NewReader(bytes.NewReader(gz)); err != nil {
		t.Fatal(err)
	} else if got, _ := io.ReadAll(zr); !bytes.Equal(got, real) {
		t.Error("index.html.gz 与 index.html 不一致")
	}
	conf := readFile(t, filepath.Join(w.Root, "etc/lingedge/nginx/admin-web.conf"))
	if comp, host, ok := installer.ParseSiteMarker(conf); !ok || comp != "admin-web" || host != "admin.example.com" {
		t.Errorf("站点片段缺少标记: %q", conf[:min(len(conf), 120)])
	}
	if bad := installer.LintNginxSnippet(string(conf), installer.StaticHeadersPath("admin-web")); len(bad) > 0 {
		t.Errorf("已安装片段未通过静态检查: %v", bad)
	}
	// 站点根权限：目录 0755、文件 0644（Nginx 工作进程可读）。
	if err := filepath.WalkDir(filepath.Join(cur, "public"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		if d.IsDir() && fi.Mode().Perm() != 0o755 || !d.IsDir() && fi.Mode().Perm() != 0o644 {
			t.Errorf("%s 权限 %v，期望目录 0755/文件 0644", p, fi.Mode().Perm())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// 独立主机名：同主机名装 website → 退出码 2 的错误类型，且没有留下任何 website 痕迹。
	_, err = install("website", map[string]string{"host": "Admin.Example.com"})
	if installer.KindOf(err) != installer.KindSettings {
		t.Fatalf("同主机名应被拒绝（KindSettings→退出码 2），得 %v", err)
	}
	if w.Has("opt/lingedge/website") || w.Has("etc/lingedge/nginx/website.conf") {
		t.Error("被拒绝的安装不应留下任何痕迹")
	}
	if _, err := install("website", map[string]string{"host": "www.example.com"}); err != nil {
		t.Fatalf("不同主机名应能装 website: %v", err)
	}
}

// ---- 真实 Nginx ----

func nginxBin(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("nginx")
	if err != nil {
		if os.Getenv("LINGEDGE_REQUIRE_NGINX") == "1" {
			t.Fatalf("LINGEDGE_REQUIRE_NGINX=1 但本机没有 nginx: %v", err)
		}
		t.Skip("本机没有 nginx：真实 Nginx 托管的断言等待环境验证（跳过不代表通过）")
	}
	return p
}

func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// startNginx 用安装器生成的片段（路径前缀改写到临时世界根；其余逐字不动）启动真实 nginx。
func startNginx(t *testing.T, w *toolsworld.World, comps ...string) (stop func()) {
	t.Helper()
	bin := nginxBin(t)
	prefix, err := os.MkdirTemp("", "lenginx")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"tmp/body", "tmp/proxy", "tmp/fcgi", "tmp/uwsgi", "tmp/scgi", "conf"} {
		if err := os.MkdirAll(filepath.Join(prefix, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rewrite := func(b []byte) []byte {
		s := strings.ReplaceAll(string(b), "/opt/lingedge/", w.Root+"/opt/lingedge/")
		return []byte(strings.ReplaceAll(s, "/etc/lingedge/nginx/", w.Root+"/etc/lingedge/nginx/"))
	}
	var includes strings.Builder
	for _, c := range comps {
		for _, name := range []string{c + ".conf", c + "-security-headers.conf"} {
			b := readFile(t, filepath.Join(w.Root, "etc/lingedge/nginx", name))
			if err := os.WriteFile(filepath.Join(prefix, "conf", name), rewrite(b), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Fprintf(&includes, "    include %s/conf/%s.conf;\n", prefix, c)
	}
	// 站点片段里的头文件 include 是改写后的 <世界根>/etc/lingedge/nginx/<c>-security-headers.conf：把改写后的头文件也放到那个位置的副本里不可行（目录属世界根，已存在即可）。
	// 直接让它指向世界根里的真实头文件（内容与改写无关：头文件不含路径）。
	main := fmt.Sprintf(`worker_processes 1;
pid %[1]s/nginx.pid;
error_log %[1]s/error.log notice;
events { worker_connections 64; }
http {
    access_log off;
    client_body_temp_path %[1]s/tmp/body;
    proxy_temp_path %[1]s/tmp/proxy;
    fastcgi_temp_path %[1]s/tmp/fcgi;
    uwsgi_temp_path %[1]s/tmp/uwsgi;
    scgi_temp_path %[1]s/tmp/scgi;
    types {
        text/html html;
        application/javascript js mjs;
        text/css css;
        image/png png;
        image/svg+xml svg;
        application/json json;
        text/plain txt;
    }
    default_type application/octet-stream;
%[2]s}
`, prefix, includes.String())
	mainPath := filepath.Join(prefix, "nginx.conf")
	if err := os.WriteFile(mainPath, []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "-t", "-p", prefix, "-c", mainPath).CombinedOutput(); err != nil {
		t.Fatalf("nginx -t 未通过安装器生成的片段：%v\n%s\n--- 站点片段 ---\n%s", err, out, readFile(t, filepath.Join(prefix, "conf", comps[0]+".conf")))
	}
	cmd := exec.Command(bin, "-p", prefix, "-c", mainPath, "-g", "daemon off;")
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			// SIGQUIT 是 nginx 的优雅停止：主进程带走工作进程。直接 SIGKILL 主进程会留下孤儿工作进程。
			_ = cmd.Process.Signal(syscall.SIGQUIT)
			done := make(chan struct{})
			go func() { _, _ = cmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			_ = os.RemoveAll(prefix)
		})
	}
	t.Cleanup(stop)
	return stop
}

func waitPort(t testing.TB, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nginx 没有在 %s 上监听", addr)
}

type upstreamSeen struct {
	Path, Host, XFF, Proto, RealIP, Cookie string
}

// startUpstream 起一个只用于观察转发行为的 API 上游（不是 lingedge-server）。
func startUpstream(t *testing.T) (addr string, seen func() []upstreamSeen) {
	t.Helper()
	var mu sync.Mutex
	var got []upstreamSeen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, upstreamSeen{Path: r.URL.RequestURI(), Host: r.Host, XFF: r.Header.Get("X-Forwarded-For"), Proto: r.Header.Get("X-Forwarded-Proto"),
			RealIP: r.Header.Get("X-Real-IP"), Cookie: r.Header.Get("Cookie")})
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "le_admin_sid", Value: "x", Path: "/", HttpOnly: true})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"upstream":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), func() []upstreamSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]upstreamSeen(nil), got...)
	}
}

func rawGet(t *testing.T, port int, host, path string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	tr := &http.Transport{DisableCompression: true}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s%s: %v", host, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func wantHeader(t *testing.T, what string, h http.Header, k, contains string) {
	t.Helper()
	if v := h.Get(k); !strings.Contains(v, contains) {
		t.Errorf("%s：响应头 %s=%q，应含 %q", what, k, v, contains)
	}
}

func wantSecurityHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	wantHeader(t, what, h, "X-Content-Type-Options", "nosniff")
	wantHeader(t, what, h, "X-Frame-Options", "DENY")
	wantHeader(t, what, h, "Content-Security-Policy", "frame-ancestors 'none'")
	wantHeader(t, what, h, "Referrer-Policy", "strict-origin-when-cross-origin")
}

func TestRealNginxServesInstalledFrontends(t *testing.T) {
	f := sharedFlow(t)
	nginxBin(t) // 先判定环境，缺失则跳过
	w, install := installWorld(t, f)
	upAddr, upSeen := startUpstream(t)
	upstream := "http://" + upAddr
	httpPort := freePort(t)
	if _, err := install("admin-web", map[string]string{"host": "admin.example.com", "http-port": fmt.Sprint(httpPort), "ipv6": "no", "api-upstream": upstream}); err != nil {
		t.Fatalf("install admin-web: %v", err)
	}
	if _, err := install("website", map[string]string{"host": "www.example.com", "http-port": fmt.Sprint(httpPort), "ipv6": "no", "api-upstream": upstream}); err != nil {
		t.Fatalf("install website: %v", err)
	}
	startNginx(t, w, "admin-web", "website")
	waitPort(t, fmt.Sprintf("127.0.0.1:%d", httpPort))

	admin, www := "admin.example.com", "www.example.com"
	adminIndex := readFile(t, filepath.Join(w.Root, "opt/lingedge/admin-web/current/public/index.html"))
	wwwIndex := readFile(t, filepath.Join(w.Root, "opt/lingedge/website/current/public/index.html"))

	t.Run("入口页与独立主机名", func(t *testing.T) {
		r, b := rawGet(t, httpPort, admin, "/", nil)
		if r.StatusCode != 200 || !bytes.Equal(b, adminIndex) {
			t.Fatalf("admin 入口页: %d", r.StatusCode)
		}
		wantSecurityHeaders(t, "admin /", r.Header)
		wantHeader(t, "admin /", r.Header, "Cache-Control", "no-store")
		r2, b2 := rawGet(t, httpPort, www, "/", nil)
		if r2.StatusCode != 200 || !bytes.Equal(b2, wwwIndex) || bytes.Equal(b2, adminIndex) {
			t.Fatalf("官网主机名应得到官网自己的入口页（与 Admin 不同）: %d", r2.StatusCode)
		}
		if r.Header.Get("Server") != "nginx" {
			t.Errorf("server_tokens off 应只暴露 nginx（不含版本）：%q", r.Header.Get("Server"))
		}
	})

	t.Run("SPA 历史路由回落入口页（Admin）", func(t *testing.T) {
		r, b := rawGet(t, httpPort, admin, "/system/users/42", nil)
		if r.StatusCode != 200 || !bytes.Equal(b, adminIndex) {
			t.Fatalf("SPA 回落失败: %d", r.StatusCode)
		}
		wantSecurityHeaders(t, "SPA 回落", r.Header)
		// 官网不是 SPA：未知路径 404。
		if r2, _ := rawGet(t, httpPort, www, "/no/such/page", nil); r2.StatusCode != 404 {
			t.Errorf("官网未知路径应 404，得 %d", r2.StatusCode)
		}
	})

	t.Run("哈希资源 immutable + gzip_static + 错误响应也带安全头", func(t *testing.T) {
		pub := filepath.Join(w.Root, "opt/lingedge/admin-web/current/public")
		var gzRel string
		_ = filepath.WalkDir(filepath.Join(pub, "assets"), func(p string, d fs.DirEntry, err error) error {
			if err == nil && gzRel == "" && strings.HasSuffix(p, ".js.gz") {
				gzRel, _ = filepath.Rel(pub, strings.TrimSuffix(p, ".gz"))
			}
			return nil
		})
		if gzRel == "" {
			t.Fatal("包内没有带预压缩的 js 资源")
		}
		url := "/" + filepath.ToSlash(gzRel)
		orig := readFile(t, filepath.Join(pub, gzRel))
		r, b := rawGet(t, httpPort, admin, url, map[string]string{"Accept-Encoding": "gzip"})
		if r.StatusCode != 200 || r.Header.Get("Content-Encoding") != "gzip" {
			t.Fatalf("gzip_static 未生效: %d %q", r.StatusCode, r.Header.Get("Content-Encoding"))
		}
		if zr, err := gzip.NewReader(bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		} else if got, _ := io.ReadAll(zr); !bytes.Equal(got, orig) {
			t.Error("gzip 响应解压后与原文件不一致")
		}
		wantHeader(t, "asset", r.Header, "Cache-Control", "immutable")
		wantHeader(t, "asset", r.Header, "Vary", "Accept-Encoding")
		wantSecurityHeaders(t, "asset", r.Header)
		r2, b2 := rawGet(t, httpPort, admin, url, nil)
		if r2.Header.Get("Content-Encoding") != "" || !bytes.Equal(b2, orig) {
			t.Error("不带 Accept-Encoding 应返回原文件")
		}
		r3, _ := rawGet(t, httpPort, admin, "/assets/does-not-exist.js", nil)
		if r3.StatusCode != 404 {
			t.Errorf("缺失资源应 404（不回落入口页），得 %d", r3.StatusCode)
		}
		wantSecurityHeaders(t, "404 响应（always）", r3.Header)
	})

	t.Run("点文件不对外", func(t *testing.T) {
		for _, p := range []string{"/.env", "/.git/config", "/assets/.secret"} {
			if r, _ := rawGet(t, httpPort, admin, p, nil); r.StatusCode != 404 {
				t.Errorf("%s 应 404，得 %d", p, r.StatusCode)
			}
		}
	})

	t.Run("Admin 的 /api/ 同源反代：覆盖 XFF、Host 原样、Cookie host-only", func(t *testing.T) {
		before := len(upSeen())
		r, b := rawGet(t, httpPort, admin, "/api/v1/auth/me?x=1", map[string]string{"X-Forwarded-For": "6.6.6.6", "Cookie": "le_admin_sid=abc"})
		if r.StatusCode != 200 || !strings.Contains(string(b), `"upstream":true`) {
			t.Fatalf("未反代到上游: %d %s", r.StatusCode, b)
		}
		seen := upSeen()
		if len(seen) != before+1 {
			t.Fatalf("上游应收到 1 次请求，实际 %d", len(seen)-before)
		}
		s := seen[len(seen)-1]
		if s.Path != "/api/v1/auth/me?x=1" || s.Host != admin || s.XFF != "127.0.0.1" || s.RealIP != "127.0.0.1" || s.Proto != "http" || s.Cookie != "le_admin_sid=abc" {
			t.Errorf("转发内容不符（XFF 必须被覆盖为 $remote_addr，不信任访客值）: %+v", s)
		}
		for _, c := range r.Cookies() {
			if c.Name == "le_admin_sid" && c.Domain != "" {
				t.Errorf("会话 Cookie 必须 host-only（不得带 Domain）: %+v", c)
			}
		}
		if strings.Contains(strings.ToLower(r.Header.Get("Set-Cookie")), "domain=") {
			t.Errorf("Set-Cookie 不得被改写出 Domain: %q", r.Header.Get("Set-Cookie"))
		}
	})

	t.Run("官网公网域：只放行 /api/v1/public/，其余与 setup 全部 404（IMP-17）", func(t *testing.T) {
		base := len(upSeen())
		for _, p := range []string{"/api/v1/auth/login", "/api/v1/admin/users", "/api/v1/public/setup/status", "/api/v1/public/setup", "/api/v1/public/setup/admin", "/api/other", "/api/"} {
			if r, _ := rawGet(t, httpPort, www, p, nil); r.StatusCode != 404 {
				t.Errorf("官网域 %s 应 404，得 %d", p, r.StatusCode)
			}
		}
		if n := len(upSeen()) - base; n != 0 {
			t.Fatalf("被禁止的路径不得到达上游，上游收到 %d 次", n)
		}
		r, _ := rawGet(t, httpPort, www, "/api/v1/public/stats", map[string]string{"X-Forwarded-For": "6.6.6.6"})
		if r.StatusCode != 200 {
			t.Fatalf("/api/v1/public/stats 应放行: %d", r.StatusCode)
		}
		seen := upSeen()
		if len(seen)-base != 1 || seen[len(seen)-1].XFF != "127.0.0.1" || seen[len(seen)-1].Path != "/api/v1/public/stats" {
			t.Errorf("官网公开接口转发不符: %+v", seen[base:])
		}
	})

	t.Run("官网 /_nuxt/ 永久缓存", func(t *testing.T) {
		pub := filepath.Join(w.Root, "opt/lingedge/website/current/public")
		_ = os.MkdirAll(filepath.Join(pub, "_nuxt"), 0o755) // 夹具官网没有 _nuxt：不改动安装器写出的文件，只验证 location（目录在测试世界里补一个文件）
		if err := os.WriteFile(filepath.Join(pub, "_nuxt", "entry.abc.js"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		r, _ := rawGet(t, httpPort, www, "/_nuxt/entry.abc.js", nil)
		if r.StatusCode != 200 {
			t.Fatalf("/_nuxt/ 应可读: %d", r.StatusCode)
		}
		wantHeader(t, "_nuxt", r.Header, "Cache-Control", "immutable")
		wantSecurityHeaders(t, "_nuxt", r.Header)
	})
}

// TLS：只允许 TLS 1.3、HSTS、HTTP 301 到 HTTPS（同一份生成器，真实 nginx + 运行时生成的自签证书）。
func TestRealNginxTLS(t *testing.T) {
	f := sharedFlow(t)
	nginxBin(t)
	w, install := installWorld(t, f)
	certDir, err := os.MkdirTemp("", "lecert")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(certDir) })
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "admin.example.com"}, DNSNames: []string{"admin.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(key)
	certPath, keyPath := filepath.Join(certDir, "admin_cert.pem"), filepath.Join(certDir, "admin_key.pem")
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600)

	httpPort, httpsPort := freePort(t), freePort(t)
	if _, err := install("admin-web", map[string]string{"host": "admin.example.com", "http-port": fmt.Sprint(httpPort), "https-port": fmt.Sprint(httpsPort), "ipv6": "no",
		"tls-cert": certPath, "tls-key": keyPath}); err != nil {
		t.Fatalf("install: %v", err)
	}
	startNginx(t, w, "admin-web")
	waitPort(t, fmt.Sprintf("127.0.0.1:%d", httpsPort))

	r, _ := rawGet(t, httpPort, "admin.example.com", "/system/users?a=1", nil)
	if r.StatusCode != 301 || r.Header.Get("Location") != fmt.Sprintf("https://admin.example.com:%d/system/users?a=1", httpsPort) {
		t.Errorf("HTTP 应 301 到 HTTPS: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	dial := func(maxVer uint16) (*http.Response, error) {
		tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "admin.example.com", MaxVersion: maxVer}, // 自签证书：只验证协议/头，不验证链
			DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, n, fmt.Sprintf("127.0.0.1:%d", httpsPort))
			}}
		defer tr.CloseIdleConnections()
		return (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get("https://admin.example.com/")
	}
	resp, err := dial(tls.VersionTLS13)
	if err != nil {
		t.Fatalf("TLS 1.3 访问失败: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
		t.Errorf("应以 TLS 1.3 提供服务: %d %+v", resp.StatusCode, resp.TLS)
	}
	wantHeader(t, "https", resp.Header, "Strict-Transport-Security", "max-age=31536000")
	wantSecurityHeaders(t, "https", resp.Header)
	if _, err := dial(tls.VersionTLS12); err == nil {
		t.Error("TLS 1.2 不应被接受（最低 TLS 1.3，threat-model §5）")
	}
}

// 渲染器的全部组合（5 个组件 × 默认/TLS/https 上游/关 IPv6）都必须被真实 nginx 的 `nginx -t` 接受（语法、指令、模块——含 gzip_static、http2 参数）。
func TestRealNginxAcceptsAllRenderedVariants(t *testing.T) {
	bin := nginxBin(t)
	dir, err := os.MkdirTemp("", "lent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, d := range []string{"tmp", "conf"} {
		_ = os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	cert, key, ca := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem"), filepath.Join(dir, "ca.pem")
	// nginx -t 会加载证书：用运行时生成的自签证书。
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x.example.com"}, DNSNames: []string{"x.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	kb, _ := x509.MarshalPKCS8PrivateKey(k)
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for p, b := range map[string][]byte{cert: pemCert, ca: pemCert, key: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	type variant struct {
		name string
		set  func(map[string]string)
	}
	variants := []variant{
		{"默认", func(map[string]string) {}},
		{"TLS", func(m map[string]string) { m["tls-cert"], m["tls-key"] = cert, key }},
		{"关 IPv6", func(m map[string]string) { m["ipv6"] = "no" }},
		{"https 上游", func(m map[string]string) { m["api-upstream"], m["api-ca"] = "https://localhost:8443", ca }},
		{"TLS + 自定义端口 + 远端 IPv6 上游", func(m map[string]string) {
			m["tls-cert"], m["tls-key"], m["http-port"], m["https-port"], m["api-upstream"] = cert, key, "8081", "8443", "http://[2001:db8::1]:8080"
		}},
	}
	for _, comp := range installer.StaticComponents() {
		for _, v := range variants {
			set := map[string]string{"host": "x.example.com"}
			v.set(set)
			if _, hasAPI := set["api-upstream"]; hasAPI && (comp == "docs" || comp == "status") {
				continue // docs/status 没有 API 反代，不认识这些键（另有用例断言被拒绝）
			}
			st, err := installer.ParseStaticSettings(comp, set)
			if err != nil {
				t.Fatalf("%s/%s: %v", comp, v.name, err)
			}
			conf, hdr, err := installer.RenderStaticSite(installer.SiteParams{Component: comp, Host: st.Host, APIUpstream: st.APIUpstream, APICA: st.APICA,
				TLSCert: st.TLSCert, TLSKey: st.TLSKey, HTTPPort: st.HTTPPort, HTTPSPort: st.HTTPSPort, IPv6: st.IPv6})
			if err != nil {
				t.Fatalf("%s/%s: %v", comp, v.name, err)
			}
			hp := filepath.Join(dir, "conf", comp+"-security-headers.conf")
			if err := os.WriteFile(hp, hdr, 0o644); err != nil {
				t.Fatal(err)
			}
			body := strings.ReplaceAll(string(conf), installer.StaticHeadersPath(comp), hp)
			main := fmt.Sprintf("worker_processes 1;\npid %[1]s/n.pid;\nerror_log %[1]s/e.log;\nevents {}\nhttp {\n  client_body_temp_path %[1]s/tmp; proxy_temp_path %[1]s/tmp; fastcgi_temp_path %[1]s/tmp; uwsgi_temp_path %[1]s/tmp; scgi_temp_path %[1]s/tmp;\n%[2]s\n}\n", dir, body)
			mp := filepath.Join(dir, "nginx.conf")
			if err := os.WriteFile(mp, []byte(main), 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(bin, "-t", "-p", dir, "-c", mp).CombinedOutput(); err != nil {
				t.Errorf("%s/%s：nginx -t 拒绝了生成的片段：%v\n%s\n%s", comp, v.name, err, out, conf)
			}
		}
	}
}
