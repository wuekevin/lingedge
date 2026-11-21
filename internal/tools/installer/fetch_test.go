package installer_test

import (
	"archive/tar"
	"bytes"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

func bundleEntries(t testing.TB, path string) (names []string, bodies map[string][]byte) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	bodies = map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		bodies[h.Name] = body
	}
}

func writeBundleFrom(t testing.TB, path string, names []string, bodies map[string][]byte) {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(bodies[n])), Format: tar.FormatPAX}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(bodies[n])
	}
	_ = tw.Close()
	if err := os.WriteFile(path, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// 校验失败（外层 SHA256 不符）→ 取包阶段失败：返回 *downloader.Error（cli 映射为 5），已回滚，信任库 sequence 不推进，staging 清理，state 不变。
func TestTamperedPackageFailsVerificationWithoutSideEffects(t *testing.T) {
	w := newW(t)
	names, bodies := bundleEntries(t, w.Bundle)
	for _, n := range names {
		if strings.HasPrefix(n, "packages/lingedge-openresty") {
			b := append([]byte(nil), bodies[n]...)
			b[len(b)/2] ^= 0x01
			bodies[n] = b
		}
	}
	bad := filepath.Join(w.Root, "bad.tar")
	writeBundleFrom(t, bad, names, bodies)
	before := sysSnap(t, w)
	o := doInstall(t, w, "node", nil, func(r *installer.Request) { r.Offline = bad })
	var de *downloader.Error
	if !errors.As(o.err, &de) || de.Kind != downloader.KindSHA256 {
		t.Fatalf("应为 sha256_mismatch: %v", o.err)
	}
	if kindOf(t, o.err) != installer.KindStepFailed {
		t.Errorf("kind=%s", kindOf(t, o.err))
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" {
		t.Fatalf("校验失败后系统应不变:\n%s", d)
	}
	if _, ok, _ := (manifest.FileSequenceStore{Path: w.Layout.SequenceFile()}).Max("stable"); ok {
		t.Error("校验失败不得推进 sequence")
	}
	if ents, _ := os.ReadDir(w.Layout.StagingDir()); len(ents) != 0 {
		t.Errorf("staging 应已清理: %v", ents)
	}
}

// 清单过期：在线路径永不放行；离线 + --allow-expired-manifest 放行并给出警告；通道不符 → 校验失败。
func TestManifestExpiryAndChannel(t *testing.T) {
	w := newW(t)
	w.Now = time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC) // 清单 valid_until = 2027-02-01
	_, err := plan(t, w, "node", nil)
	var me *manifest.Error
	if !errors.As(err, &me) || me.Kind != manifest.KindExpired {
		t.Fatalf("应为 manifest_expired: %v", err)
	}
	pre, err := plan(t, w, "node", nil, func(r *installer.Request) { r.AllowExpired = true })
	if err != nil {
		t.Fatalf("--offline + --allow-expired-manifest 应放行: %v", err)
	}
	warned := false
	for _, x := range pre.Warnings {
		warned = warned || x.Code == "manifest.warning"
	}
	if !warned {
		t.Error("放行过期清单必须告警")
	}
	w2 := newW(t)
	_, err = plan(t, w2, "node", nil, func(r *installer.Request) { r.Channel = "beta" })
	if !errors.As(err, &me) || me.Kind != manifest.KindChannel {
		t.Fatalf("通道不符应为 channel_mismatch: %v", err)
	}
}

// 在线安装：release.base_url / release.ca_file / 代理配置接线、断点续传目录、HTTPS 取包、DeferCommit（sequence 在安装成功后才推进）。
func TestOnlineInstallWiresReleaseConfig(t *testing.T) {
	var routes map[string][]byte
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if b, ok := routes[r.URL.Path]; ok {
			_, _ = rw.Write(b)
			return
		}
		http.NotFound(rw, r)
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	base := "https://" + srv.Listener.Addr().String()
	w, err := toolsworld.NewWorldBase(t.TempDir(), base)
	if err != nil {
		t.Fatal(err)
	}
	names, bodies := bundleEntries(t, w.Bundle)
	routes = map[string][]byte{"/stable/manifest.json": bodies["manifest.json"], "/stable/manifest.json.sig": bodies["manifest.json.sig"]}
	for _, n := range names {
		if rest, ok := strings.CutPrefix(n, "packages/"); ok {
			routes["/dl/"+rest] = bodies[n]
		}
	}
	ca := filepath.Join(w.Root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	online := func(mut func(*installer.ToolsConfig)) []func(*installer.Deps) {
		return []func(*installer.Deps){func(d *installer.Deps) {
			d.Config.Release.BaseURL = base
			d.Config.Release.CAFile = ca
			if mut != nil {
				mut(&d.Config)
			}
		}}
	}
	noOffline := func(r *installer.Request) { r.Offline = "" }

	// 缺少 release.base_url：明确的前置条件失败，提示 UD-3 与 --offline。
	o := doInstall(t, w, "node", nil, noOffline)
	if kindOf(t, o.err) != installer.KindPrecondition || !strings.Contains(o.err.Error(), "release.base_url") {
		t.Fatalf("%v", o.err)
	}
	// 没有 ca_file：测试服务器证书不被系统根信任 → TLS 校验失败（无 --insecure）。
	o = doInstall(t, w, "node", []func(*installer.Deps){func(d *installer.Deps) { d.Config.Release.BaseURL = base }}, noOffline)
	var de *downloader.Error
	if !errors.As(o.err, &de) || de.Kind != downloader.KindTLS {
		t.Fatalf("应为 tls_untrusted: %v", o.err)
	}
	// 不可达的代理：配置被接线（失败为传输错误）。
	o = doInstall(t, w, "node", online(func(c *installer.ToolsConfig) { c.Release.Proxy = "http://127.0.0.1:1" }), noOffline)
	if !errors.As(o.err, &de) || de.Kind != downloader.KindNetwork {
		t.Fatalf("代理应被使用并导致传输失败: %v", o.err)
	}
	// 正常：在线安装成功；断点续传目录在状态目录下创建（0700）；sequence 在成功后推进。
	res := must(t, doInstall(t, w, "node", online(nil), noOffline))
	if res.Outcome != installer.OutcomeInstalled {
		t.Fatalf("%+v", res)
	}
	if fi, err := os.Stat(filepath.Join(w.Layout.Root, "downloads")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("续传目录: %v %v", fi, err)
	}
	if seq, ok, _ := (manifest.FileSequenceStore{Path: w.Layout.SequenceFile()}).Max("stable"); !ok || seq != 10 {
		t.Errorf("sequence=%d ok=%v", seq, ok)
	}
	// 失败的安装不推进 sequence（DeferCommit）：新世界里令启动失败，sequence 仍不存在。
	w2, _ := toolsworld.NewWorldBase(t.TempDir(), base)
	w2.SetCtl("fail-start", true)
	o = doInstall(t, w2, "node", online(nil), noOffline)
	if kindOf(t, o.err) != installer.KindRolledBack {
		t.Fatalf("%v", o.err)
	}
	if _, ok, _ := (manifest.FileSequenceStore{Path: w2.Layout.SequenceFile()}).Max("stable"); ok {
		t.Error("安装失败（回滚）后不得推进 sequence")
	}
	// 同一份清单可重试：排除故障后再装成功（提交前 sequence 未推进，清单可被再次获取）。
	w2.SetCtl("fail-start", false)
	must(t, doInstall(t, w2, "node", online(nil), noOffline))
}

// 预检与取包之间清单变了（同样合法签名、同版本，但包内容/SHA256 不同）：取包后的比对以校验失败（manifest.KindPackage → 退出码 5）中止并回滚，不安装预检时没见过的包。
func TestManifestChangedBetweenPreflightAndFetchIsRejected(t *testing.T) {
	w := newW(t)
	before := sysSnap(t, w)
	swapped := false
	o := doInstall(t, w, "node", []func(*installer.Deps){func(d *installer.Deps) {
		d.Hook = func(p string) {
			if p == "before:10" && !swapped {
				swapped = true
				if err := w.RewriteBundle("-v2"); err != nil {
					t.Error(err)
				}
			}
		}
	}})
	var me *manifest.Error
	if !errors.As(o.err, &me) || me.Kind != manifest.KindPackage {
		t.Fatalf("应为 package_invalid（5）: %v", o.err)
	}
	if d := diffSnap(before, sysSnap(t, w)); d != "" {
		t.Fatalf("应回到安装前:\n%s", d)
	}
	if _, ok, _ := (manifest.FileSequenceStore{Path: w.Layout.SequenceFile()}).Max("stable"); ok {
		t.Error("不得推进 sequence")
	}
}
