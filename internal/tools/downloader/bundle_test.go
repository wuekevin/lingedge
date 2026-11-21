package downloader_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/cli"
	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

func okBundle(t *testing.T, w *world) []ent {
	t.Helper()
	s := scenario{pkg: goodPkg(t, w, nil)}
	mf, sig := s.build(w)
	return []ent{{name: "manifest.json", body: mf}, {name: "manifest.json.sig", body: sig}, {name: "packages/" + pkgFile, body: s.pkg}}
}

func bundleFile(t *testing.T, raw []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "b.tar")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func acquireBundle(t *testing.T, w *world, path string) error {
	t.Helper()
	p := w.params()
	p.BundlePath = path
	before := tree(t, w.staging)
	_, err := downloader.Acquire(t.Context(), p)
	if err != nil {
		equalTree(t, "staging", before, tree(t, w.staging)) // 失败：staging 目录树前后一致
		return err
	}
	ents, _ := os.ReadDir(w.staging) // 成功：留下 <tx>/pkg，清掉以便下一个用例比较
	for _, e := range ents {
		_ = os.RemoveAll(filepath.Join(w.staging, e.Name()))
	}
	return nil
}

func TestBundlePathMustBeRegularFile(t *testing.T) {
	w := newWorld(t)
	good := bundleFile(t, bundleBytes(t, okBundle(t, w)...))
	dir := t.TempDir()

	link := filepath.Join(dir, "link.tar")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(dir, "escape.tar") // 指向目录之外的普通文件（符号链接逃逸）
	if err := os.Symlink("/etc/hosts", escape); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(dir, "dirlink")
	if err := os.Symlink(dir, dirLink); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"指向普通文件的符号链接": link,
		"符号链接逃逸到目录外":  escape,
		"目录":          dir,
		"目录符号链接":      dirLink,
		"字符设备":        "/dev/null",
		"含 NUL":       "a\x00b",
	} {
		err := acquireBundle(t, w, p)
		wantKind(t, err, downloader.KindBundle)
		if exitOf(err) != 5 {
			t.Errorf("%s: 退出码 = %d", name, exitOf(err))
		}
	}
	for p, want := range map[string]string{dir: "目录", "/dev/null": "设备"} {
		if _, err := downloader.ResolveBundlePath(p); err == nil || !strings.Contains(err.Error(), "不是普通文件（"+want+"）") {
			t.Errorf("ResolveBundlePath(%s) 应在路径校验阶段报「不是普通文件（%s）」：%v", p, want, err)
		}
	}
	if _, err := downloader.ResolveBundlePath(""); !downloader.IsKind(err, downloader.KindBundle) {
		t.Errorf("空路径应被拒绝：%v", err)
	}
	// 不存在：用法错误（2）。
	err := acquireBundle(t, w, filepath.Join(dir, "missing.tar"))
	wantKind(t, err, downloader.KindBundleAbsent)
	if exitOf(err) != cli.ExitUsage {
		t.Errorf("不存在的 bundle 退出码 = %d", exitOf(err))
	}
	// 路径中间含符号链接（如 /var → /private/var）是正常的：realpath 之后是普通文件即可。
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "b.tar"), bundleBytes(t, okBundle(t, w)...), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	if err := acquireBundle(t, w, filepath.Join(alias, "b.tar")); err != nil {
		t.Fatalf("父目录经符号链接、末级为普通文件应被接受：%v", err)
	}
}

// 离线不联网：用「若发生任何网络活动就计数」的拦截证明零调用。
// 拦截面：①测试 HTTPS 服务器（包 URL/清单 URL 指向它，并在运行前关闭）；②http.DefaultTransport/DefaultClient 被换成计数的 RoundTripper；
// ③net.DefaultResolver 的 Dial；④非回环主机名 + HTTPS_PROXY 指向计数监听器（回环主机不走代理，故另用非回环主机名的包 URL）。
// 负向对照：同样的拦截器在「在线」尝试时确实计到调用，证明拦截有效。
func TestOfflineMakesNoNetworkCalls(t *testing.T) {
	var rt, dial atomic.Int64
	oldT, oldR := http.DefaultTransport, net.DefaultResolver
	http.DefaultTransport = countingRT{&rt}
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		dial.Add(1)
		return nil, net.ErrClosed
	}}
	t.Cleanup(func() { http.DefaultTransport, net.DefaultResolver = oldT, oldR })
	px := newConnectProxy(t)
	t.Setenv("HTTPS_PROXY", "http://"+px.ln.Addr().String())
	t.Setenv("https_proxy", "http://"+px.ln.Addr().String())
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	w := newWorld(t)
	s := scenario{pkg: goodPkg(t, w, nil), urlBase: "https://releases.example.invalid"}
	bundle := w.writeBundle(s)
	hitsBefore := func() int {
		return w.hitCount("/manifest.json") + w.hitCount("/manifest.json.sig") + w.hitCount("/dl/"+pkgFile)
	}

	p := w.params()
	p.BundlePath = bundle
	p.TxID = "off-1"
	if _, err := downloader.Acquire(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if hitsBefore() != 0 || rt.Load() != 0 || dial.Load() != 0 {
		t.Fatalf("离线路径发生了网络活动：server=%d defaultRT=%d dns=%d", hitsBefore(), rt.Load(), dial.Load())
	}
	if cons, _ := px.seen(); len(cons) != 0 {
		t.Fatalf("离线路径连接了代理：%v", cons)
	}
	// 离线路径连 Client 都不需要：nil 也不影响（Params 校验要求只给一个来源）。
	// 失败路径同样零网络：篡改 bundle 内的包。
	bad := scenario{pkg: flip(goodPkg(t, w, nil), 50), manifestPkg: goodPkg(t, w, nil), urlBase: "https://releases.example.invalid"}
	p.BundlePath = w.writeBundle(bad)
	p.TxID = "off-2"
	if _, err := downloader.Acquire(t.Context(), p); err == nil {
		t.Fatal("期望失败")
	}
	if rt.Load() != 0 || dial.Load() != 0 {
		t.Fatal("离线失败路径发生了网络活动")
	}
	if cons, _ := px.seen(); len(cons) != 0 {
		t.Fatalf("离线失败路径连接了代理：%v", cons)
	}

	// 负向对照：同一个清单走在线路径，拦截器能看到活动（解析主机名触发 net.DefaultResolver 的 Dial，或经代理 CONNECT 到 releases.example.invalid:443）。
	mf, sig := s.build(w)
	w.serveBytes("/manifest.json", mf)
	w.serveBytes("/manifest.json.sig", sig)
	po := w.params()
	po.Online = &downloader.OnlineSource{Client: w.client(downloader.Config{MaxAttempts: 1}), ManifestURL: "https://releases.example.invalid/manifest.json"}
	if _, err := downloader.Acquire(t.Context(), po); err == nil {
		t.Fatal("对照组：在线路径此处应失败（代理会返回 502）")
	}
	if cons, _ := px.seen(); len(cons) == 0 && dial.Load() == 0 {
		t.Fatal("对照组：拦截器没有观测到在线尝试，零调用结论无效")
	}
}

type countingRT struct{ n *atomic.Int64 }

func (c countingRT) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, net.ErrClosed
}

func TestBundleLayoutRejections(t *testing.T) {
	w := newWorld(t)
	base := okBundle(t, w)
	mf, sig, pkg := base[0], base[1], base[2]
	with := func(extra ...ent) []ent { return append(append([]ent(nil), base...), extra...) }
	cases := map[string][]ent{
		"根目录多出文件":               with(ent{name: "extra.txt", body: []byte("x")}),
		"packages 下嵌套目录":        with(ent{name: "packages/sub/x.tar.gz", body: []byte("x")}),
		"包文件名含空格":               with(ent{name: "packages/a b.tar.gz", body: []byte("x")}),
		"包文件名以点开头":              with(ent{name: "packages/.hidden", body: []byte("x")}),
		"路径穿越":                  with(ent{name: "../evil", body: []byte("x")}),
		"绝对路径":                  with(ent{name: "/etc/passwd", body: []byte("x")}),
		"符号链接条目":                with(ent{name: "packages/ln", typ: '2', link: "/etc/passwd"}),
		"硬链接条目":                 with(ent{name: "packages/hl", typ: '1', link: "manifest.json"}),
		"重复 manifest.json":      with(ent{name: "manifest.json", body: mf.body}),
		"仅大小写不同的重复":             with(ent{name: "Manifest.json", body: mf.body}),
		"缺 manifest.json":       {sig, pkg},
		"缺 manifest.json.sig":   {mf, pkg},
		"非法目录条目":                with(ent{name: "evil/", typ: '5'}),
		"offline-bundle 前缀与根混用": {{name: "offline-bundle/manifest.json", body: mf.body}, {name: "offline-bundle/manifest.json.sig", body: sig.body}, {name: "manifest.json", body: mf.body}},
		"空的 manifest.json":      {{name: "manifest.json"}, sig, pkg},
		"manifest.json.sig 过大":  {mf, {name: "manifest.json.sig", body: bytes.Repeat([]byte("a"), 5000)}, pkg},
		"manifest.json 超过 8MiB": {{name: "manifest.json", body: make([]byte, 8<<20+1)}, sig, pkg},
	}
	for name, ents := range cases {
		t.Run(name, func(t *testing.T) {
			err := acquireBundle(t, w, bundleFile(t, bundleBytes(t, ents...)))
			wantKind(t, err, downloader.KindBundle)
			if exitOf(err) != 5 {
				t.Fatalf("退出码 = %d", exitOf(err))
			}
		})
	}
	// 非 tar / 被截断 / gzip 过的 tar / 声明巨大条目：拒绝。
	raw := bundleBytes(t, base...)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	for name, b := range map[string][]byte{
		"不是 tar":  []byte(strings.Repeat("not a tar file ", 100)),
		"被截断":     raw[:len(raw)-3000],
		"gzip 包装": gz.Bytes(),
		"空文件":     {},
		"仅尾部零块":   make([]byte, 1024),
	} {
		t.Run(name, func(t *testing.T) {
			err := acquireBundle(t, w, bundleFile(t, b))
			wantKind(t, err, downloader.KindBundle)
		})
	}
	// 同样合法的布局带 offline-bundle/ 前缀与目录条目：成功。
	pref := []ent{{name: "offline-bundle/", typ: '5'}, {name: "offline-bundle/packages/", typ: '5'},
		{name: "offline-bundle/manifest.json", body: mf.body}, {name: "offline-bundle/manifest.json.sig", body: sig.body}, {name: "offline-bundle/packages/" + pkgFile, body: pkg.body}}
	if err := acquireBundle(t, w, bundleFile(t, bundleBytes(t, pref...))); err != nil {
		t.Fatalf("带 offline-bundle/ 前缀的合法布局应成功：%v", err)
	}
}

func TestBundlePackageProblems(t *testing.T) {
	w := newWorld(t)
	base := okBundle(t, w)
	mf, sig, pkg := base[0], base[1], base[2]
	t.Run("缺少清单引用的包", func(t *testing.T) {
		err := acquireBundle(t, w, bundleFile(t, bundleBytes(t, mf, sig, ent{name: "packages/other.tar.gz", body: pkg.body})))
		wantKind(t, err, downloader.KindBundle)
		if exitOf(err) != 5 {
			t.Fatalf("exit=%d", exitOf(err))
		}
	})
	t.Run("包条目大小与清单 size 不符", func(t *testing.T) {
		err := acquireBundle(t, w, bundleFile(t, bundleBytes(t, mf, sig, ent{name: pkg.name, body: append(append([]byte(nil), pkg.body...), 0)})))
		wantKind(t, err, downloader.KindSize)
		if !strings.Contains(err.Error(), "与清单 size ") {
			t.Fatalf("应在复制之前按条目大小拒绝：%v", err)
		}
	})
	t.Run("清单未引用的多余包被忽略（不读取）", func(t *testing.T) {
		if err := acquireBundle(t, w, bundleFile(t, bundleBytes(t, mf, sig, pkg, ent{name: "packages/unused.tar.gz", body: []byte("junk")}))); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("条目顺序任意（包在清单之前）", func(t *testing.T) {
		if err := acquireBundle(t, w, bundleFile(t, bundleBytes(t, pkg, sig, mf))); err != nil {
			t.Fatal(err)
		}
	})
}

// rawHeader 手工构造 ustar 头（含正确校验和），用于 tar.Writer 不允许写出的「声明大小远大于实际数据」的畸形条目。
func rawHeader(name string, size int64) []byte {
	h := make([]byte, 512)
	copy(h, name)
	copy(h[100:], "0000644\x00")
	copy(h[108:], "0000000\x00")
	copy(h[116:], "0000000\x00")
	copy(h[124:], fmt.Sprintf("%011o\x00", size))
	copy(h[136:], "00000000000\x00")
	h[156] = '0'
	copy(h[257:], "ustar\x0000")
	copy(h[148:], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
	return h
}

func TestBundleDeclaredHugeEntryRejected(t *testing.T) {
	// tar 头声明 4 GiB，实际文件只有几百字节：索引阶段即拒绝（数据不足），不分配 4 GiB。
	w := newWorld(t)
	raw := append(rawHeader("manifest.json", 4<<30), []byte("tiny")...)
	err := acquireBundle(t, w, bundleFile(t, raw))
	wantKind(t, err, downloader.KindBundle)
	if !errors.Is(err, security.ErrArchive) {
		t.Fatalf("期望 ErrArchive：%v", err)
	}
	// 声明在上限之内（1 MiB）但数据不够：被截断。
	raw = append(rawHeader("manifest.json", 1<<20), []byte("tiny")...)
	err = acquireBundle(t, w, bundleFile(t, raw))
	wantKind(t, err, downloader.KindBundle)
}

func TestIndexTarMatchesSectionReads(t *testing.T) {
	// 索引给出的偏移必须正好指向文件数据（PAX 长文件名、不同大小、512 对齐）。
	long := strings.Repeat("d", 90) + "/" + strings.Repeat("f", 90)
	raw := bundleBytes(t,
		ent{name: "a", body: []byte("hello")},
		ent{name: long, body: bytes.Repeat([]byte("x"), 1000)},
		ent{name: "empty", body: nil},
		ent{name: "z", body: bytes.Repeat([]byte("y"), 512)},
	)
	es, err := security.IndexTar(bytes.NewReader(raw), security.IndexLimits{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "hello", long: strings.Repeat("x", 1000), "empty": "", "z": strings.Repeat("y", 512)}
	if len(es) != len(want) {
		t.Fatalf("条目数 %d", len(es))
	}
	for _, e := range es {
		got := string(raw[e.Offset : e.Offset+e.Size])
		if got != want[e.Name] {
			t.Errorf("%s: 偏移处内容 %q 不符", e.Name, got[:min(len(got), 20)])
		}
	}
}
