package downloader_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/cli"
	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// 仅测试：测试密钥由「标签 → SHA-256 → 种子」确定性派生，只存在于 _test.go；与 manifest 包测试同一派生方案
// （其公钥已登记到 release-keys 门禁的指纹表，生产构建/内置 keys.json 不得出现）。
// 测试内 HTTPS 服务器只在 _test.go 中（G-11：testdouble/测试替身不进入生产构建）。

type tkey struct {
	id   string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func deriveKey(id, label string) tkey {
	s := sha256.Sum256([]byte("LingEdge unit test key v1 - TEST ONLY: " + label))
	priv := ed25519.NewKeyFromSeed(s[:])
	return tkey{id: id, priv: priv, pub: priv.Public().(ed25519.PublicKey)}
}

func sigDoc(id string, sig []byte) []byte {
	b, _ := json.Marshal(map[string]string{"key_id": id, "alg": "ed25519", "sig": base64.StdEncoding.EncodeToString(sig)})
	return b
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

const (
	comp     = "node"
	ver      = "0.1.0"
	variant  = "default"
	platform = "linux-amd64"
)

// ---- 包构造 ---------------------------------------------------------------------------------------

type ent struct {
	name string
	body []byte
	typ  byte
	link string
}

type pkgSpec struct {
	files   map[string]string // payload 相对路径 -> 内容
	signer  tkey
	sigComp string // 签名绑定的 component/version/platform（缺省与 META 一致）；用于重放攻击测试
	sigVer  string
	sigPlat string
	meta    func(m map[string]any)
	// 篡改钩子
	sumsInTar   []byte            // 非空：tar 中放这份 SHA256SUMS，但签名仍针对真正的 sums
	sigInTar    []byte            // 非空：tar 中放这份 .sig
	tamper      func([]ent) []ent // 最后对条目表改写（顺序、增删、类型）
	trailingPad int               // tar 结束后追加的零字节（压缩前）
	sumsExtra   string            // 签名前追加到 SHA256SUMS 的行（测试「sums 含 payload/ 之外路径」）
}

func defaultFiles() map[string]string {
	return map[string]string{"bin/lingedge-node": "#!node-binary-v1\n", "conf/default.yaml": "listen: 127.0.0.1\n", "README": "hello\n"}
}

func (p pkgSpec) sums() []byte {
	var names []string
	for n := range p.files {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s  payload/%s\n", sha([]byte(p.files[n])), n)
	}
	b.WriteString(p.sumsExtra)
	return []byte(b.String())
}

func (p pkgSpec) build(t testing.TB) []byte {
	t.Helper()
	if p.files == nil {
		p.files = defaultFiles()
	}
	sc, sv, sp := p.sigComp, p.sigVer, p.sigPlat
	if sc == "" {
		sc = comp
	}
	if sv == "" {
		sv = ver
	}
	if sp == "" {
		sp = platform
	}
	sums := p.sums()
	in, err := manifest.PackageSigInput(sc, sv, sp, sums)
	if err != nil {
		t.Fatal(err)
	}
	sig := sigDoc(p.signer.id, ed25519.Sign(p.signer.priv, in))
	var total int64
	for _, c := range p.files {
		total += int64(len(c))
	}
	meta := map[string]any{"component": comp, "version": ver, "variant": variant, "platform": platform,
		"built_at": "2026-11-15T00:00:00Z", "config_schema_version": 1, "files_count": len(p.files), "unpacked_size": total}
	if p.meta != nil {
		p.meta(meta)
	}
	mb, _ := json.Marshal(meta)
	if p.sumsInTar != nil {
		sumsT := p.sumsInTar
		sums = sumsT
	}
	if p.sigInTar != nil {
		sig = p.sigInTar
	}
	ents := []ent{{name: "META/package.json", body: mb}, {name: "META/SHA256SUMS", body: sums}, {name: "META/SHA256SUMS.sig", body: sig}}
	var names []string
	for n := range p.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ents = append(ents, ent{name: "payload/" + n, body: []byte(p.files[n])})
	}
	if p.tamper != nil {
		ents = p.tamper(ents)
	}
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range ents {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o755, Linkname: e.link, Format: tar.FormatPAX}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	raw.Write(make([]byte, p.trailingPad))
	var gzb bytes.Buffer
	zw := gzip.NewWriter(&gzb)
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()
	return gzb.Bytes()
}

// ---- 世界：密钥、验证器、HTTPS 服务器、staging/trust 目录 ---------------------------------------------

type world struct {
	t        *testing.T
	key      tkey
	trust    *manifest.TrustSet
	trustDir string
	staging  string
	keysDir  string
	now      time.Time
	srv      *httptest.Server
	caFile   string
	base     string // https://127.0.0.1:port

	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	hits   map[string]int
	ranges map[string][]string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, key: deriveKey("rel-dl-a", "a"), now: t0, routes: map[string]http.HandlerFunc{}, hits: map[string]int{}, ranges: map[string][]string{}}
	root := t.TempDir()
	w.trustDir = filepath.Join(root, "trust")
	w.keysDir = filepath.Join(w.trustDir, "keys.d")
	w.staging = filepath.Join(root, "staging")
	for _, d := range []string{w.trustDir, w.staging} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	w.trust = w.mkTrust(w.key)
	w.srv = httptest.NewUnstartedServer(http.HandlerFunc(w.handle))
	w.base = "https://" + w.srv.Listener.Addr().String()
	w.srv.StartTLS()
	t.Cleanup(w.srv.Close)
	// release.ca_file：把测试服务器的证书写成 PEM（0600）。
	w.caFile = filepath.Join(root, "ca.pem")
	pemb := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: w.srv.Certificate().Raw})
	if err := os.WriteFile(w.caFile, pemb, 0o600); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) mkTrust(keys ...tkey) *manifest.TrustSet {
	var ks []manifest.Key
	for _, k := range keys {
		ks = append(ks, manifest.Key{KeyID: k.id, Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(k.pub), Status: manifest.StatusActive,
			NotBefore: "2026-01-01T00:00:00Z", NotAfter: "2028-01-01T00:00:00Z"})
	}
	doc, _ := json.Marshal(manifest.KeysFile{SchemaVersion: 1, Keys: ks, RevokedKeyIDs: []string{}})
	kf, err := manifest.ParseKeysFile(doc)
	if err != nil {
		w.t.Fatal(err)
	}
	ts, err := manifest.NewTrustSet(kf)
	if err != nil {
		w.t.Fatal(err)
	}
	return ts
}

func (w *world) verifier() *manifest.Verifier {
	return &manifest.Verifier{Trust: w.trust, Seen: manifest.FileSequenceStore{Path: filepath.Join(w.trustDir, "state")}, Now: func() time.Time { return w.now }}
}

func (w *world) handle(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	w.hits[r.URL.Path]++
	w.ranges[r.URL.Path] = append(w.ranges[r.URL.Path], r.Header.Get("Range"))
	h := w.routes[r.URL.Path]
	w.mu.Unlock()
	if h == nil {
		http.NotFound(rw, r)
		return
	}
	h(rw, r)
}

func (w *world) route(path string, h http.HandlerFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.routes[path] = h
}

func (w *world) serveBytes(path string, b []byte) {
	w.route(path, func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write(b) })
}

func (w *world) hitCount(path string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hits[path]
}

func (w *world) rangeHeaders(path string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.ranges[path]...)
}

// ---- 清单构造 ------------------------------------------------------------------------------------

type scenario struct {
	pkg         []byte // 实际提供/打包的包字节
	manifestPkg []byte // 用来计算清单 sha256/size 的包字节（缺省 = pkg；篡改包体时用原始字节）
	seq         int
	channel     string
	generated   string
	validUntil  string
	status      string // 组件状态，缺省 available
	revoked     []any
	mfEdit      func(m map[string]any)
	tamperMf    func([]byte) []byte // 签名之后改写清单字节
	tamperSig   func([]byte) []byte
	signer      *tkey
	rawMf       []byte // 非空：直接用作清单字节（调用方自己签）
	pkgName     string
	urlBase     string // 清单里包 URL 的前缀（缺省 w.base）
}

func (s scenario) base(w *world) string {
	if s.urlBase != "" {
		return s.urlBase
	}
	return w.base
}

const pkgFile = "lingedge-node-0.1.0-linux-amd64.tar.gz"

func (s scenario) build(w *world) (mf, sig []byte) {
	if w.t != nil {
		w.t.Helper()
	}
	if s.manifestPkg == nil {
		s.manifestPkg = s.pkg
	}
	if s.seq == 0 {
		s.seq = 10
	}
	if s.channel == "" {
		s.channel = "stable"
	}
	if s.generated == "" {
		s.generated = "2026-11-01T00:00:00Z"
	}
	if s.validUntil == "" {
		s.validUntil = "2027-02-01T00:00:00Z"
	}
	if s.status == "" {
		s.status = "available"
	}
	if s.revoked == nil {
		s.revoked = []any{}
	}
	if s.pkgName == "" {
		s.pkgName = pkgFile
	}
	compDoc := map[string]any{"status": s.status}
	if s.status == "available" {
		compDoc["versions"] = []any{map[string]any{
			"version": ver,
			"variants": map[string]any{variant: map[string]any{"artifacts": map[string]any{
				platform: map[string]any{"url": s.base(w) + "/dl/" + s.pkgName, "sha256": sha(s.manifestPkg), "size": len(s.manifestPkg)},
			}}},
		}}
	}
	if s.status == "builtin" {
		compDoc["builtin_in"] = "server"
	}
	m := map[string]any{"schema_version": 1, "channel": s.channel, "sequence": s.seq, "generated_at": s.generated, "valid_until": s.validUntil,
		"revoked_versions": s.revoked, "components": map[string]any{comp: compDoc}}
	if s.mfEdit != nil {
		s.mfEdit(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		w.t.Fatal(err)
	}
	mf = append(b, '\n')
	if s.rawMf != nil {
		mf = s.rawMf
	}
	k := w.key
	if s.signer != nil {
		k = *s.signer
	}
	sig = sigDoc(k.id, ed25519.Sign(k.priv, append([]byte(manifest.ManifestDomain), mf...)))
	if s.tamperMf != nil {
		mf = s.tamperMf(mf)
	}
	if s.tamperSig != nil {
		sig = s.tamperSig(sig)
	}
	return mf, sig
}

func flip(b []byte, i int) []byte {
	c := append([]byte(nil), b...)
	c[i%len(c)] ^= 0x01
	return c
}

// ---- 运行 ------------------------------------------------------------------------------------------

func (w *world) client(cfg downloader.Config) *downloader.Client {
	w.t.Helper()
	if cfg.CAFile == "" {
		cfg.CAFile = w.caFile
	}
	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = time.Millisecond
		cfg.BackoffMax = 5 * time.Millisecond
	}
	c, err := downloader.New(cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(c.Close)
	return c
}

func (w *world) params() downloader.Params {
	return downloader.Params{Verifier: w.verifier(), KeysDir: w.keysDir, Channel: "stable", Component: comp, Version: ver, Variant: variant,
		Platform: platform, StagingRoot: w.staging}
}

// publish 把场景发布到 HTTPS 服务器（清单、签名、包）。
func (w *world) publish(s scenario) {
	mf, sig := s.build(w)
	w.serveBytes("/manifest.json", mf)
	w.serveBytes("/manifest.json.sig", sig)
	name := s.pkgName
	if name == "" {
		name = pkgFile
	}
	w.serveBytes("/dl/"+name, s.pkg)
}

func (w *world) runOnline(s scenario, cfg downloader.Config) (*downloader.Acquired, error) {
	w.publish(s)
	p := w.params()
	p.Online = &downloader.OnlineSource{Client: w.client(cfg), ManifestURL: w.base + "/manifest.json"}
	return downloader.Acquire(w.t.Context(), p)
}

// bundleBytes 构造离线 bundle（tar）：布局 §9.4。
func bundleBytes(t testing.TB, ents ...ent) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range ents {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Linkname: e.link, Format: tar.FormatPAX}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			_, _ = tw.Write(e.body)
		}
	}
	_ = tw.Close()
	return raw.Bytes()
}

func (w *world) writeBundle(s scenario) string {
	mf, sig := s.build(w)
	name := s.pkgName
	if name == "" {
		name = pkgFile
	}
	p := filepath.Join(w.t.TempDir(), "bundle.tar")
	if err := os.WriteFile(p, bundleBytes(w.t, ent{name: "manifest.json", body: mf}, ent{name: "manifest.json.sig", body: sig}, ent{name: "packages/" + name, body: s.pkg}), 0o600); err != nil {
		w.t.Fatal(err)
	}
	return p
}

func (w *world) runOffline(s scenario, allowExpired bool) (*downloader.Acquired, error) {
	p := w.params()
	p.BundlePath = w.writeBundle(s)
	p.AllowExpired = allowExpired
	return downloader.Acquire(w.t.Context(), p)
}

// ---- 断言辅助 ---------------------------------------------------------------------------------------

// tree 返回目录树快照（相对路径 -> 类型/模式/内容哈希）。
func tree(t testing.TB, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		v := fi.Mode().String()
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += "/" + sha(b)
		}
		out[rel] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalTree(t testing.TB, what string, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Errorf("%s 目录树前后不一致：前 %v 后 %v", what, before, after)
		return
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s 目录树前后不一致：%s: %q -> %q", what, k, v, after[k])
		}
	}
}

// exitOf 复现 cli.Run.finish 的错误映射（manifest.Error → VerifyError，downloader.Error → DownloadError）。
func exitOf(err error) cli.ExitCode {
	var me *manifest.Error
	var de *downloader.Error
	switch {
	case errors.As(err, &me):
		return cli.VerifyError(me).Exit
	case errors.As(err, &de):
		return cli.DownloadError(de).Exit
	}
	return -1
}

func wantKind(t testing.TB, err error, k downloader.Kind) {
	t.Helper()
	var de *downloader.Error
	if !errors.As(err, &de) {
		t.Fatalf("期望 downloader 错误 %s，实际 %T %v", k, err, err)
	}
	if de.Kind != k {
		t.Fatalf("downloader 错误分类 = %s，期望 %s（%v）", de.Kind, k, err)
	}
}

func wantManifestKind(t testing.TB, err error, k manifest.Kind) {
	t.Helper()
	var me *manifest.Error
	if !errors.As(err, &me) {
		t.Fatalf("期望 manifest 错误 %s，实际 %T %v", k, err, err)
	}
	if me.Kind != k {
		t.Fatalf("manifest 错误分类 = %s，期望 %s（%v）", me.Kind, k, err)
	}
}

func readState(w *world) string {
	b, err := os.ReadFile(filepath.Join(w.trustDir, "state"))
	if err != nil {
		return ""
	}
	return string(b)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func signRotation(t testing.TB, by tkey, k manifest.Key) []byte {
	t.Helper()
	return ed25519.Sign(by.priv, manifest.KeyRotationInput(k))
}

var errCanceled = context.Canceled

func contextWithCancel(t testing.TB) (context.Context, context.CancelFunc) {
	return context.WithCancel(t.Context())
}

// selfSignedCAFile 生成一个与测试服务器无关的自签证书并写成 PEM（用于「CA 不匹配」的负向用例）。
func selfSignedCAFile(t testing.TB) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unrelated test CA"}, NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "other-ca.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
