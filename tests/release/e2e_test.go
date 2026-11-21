package release_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

const (
	baseURL  = "https://releases.example.invalid/lingedge"
	relVer   = "0.1.0"
	relPlats = "linux-amd64,linux-arm64"
)

var (
	sharedDir  string
	sharedOnce sync.Once
	shared     *flow
	sharedErr  error
)

func TestMain(m *testing.M) {
	d, err := os.MkdirTemp("", "lingedge-release-e2e-")
	if err != nil {
		panic(err)
	}
	sharedDir = d
	code := m.Run()
	_ = os.RemoveAll(d)
	os.Exit(code)
}

// flow 是一次完整的发行演练：build → 离线签名第 1 轮 → finalize → 离线签名第 2 轮 → verify。
type flow struct {
	t                 testing.TB
	src, out          string
	keyPath, keysPath string
	keys              []byte
	keyID             string
	release, sign     string
	afterR1           string // 第 1 轮签名之后的快照（含 stage 与 signatures），供 finalize 的负向/轮换用例
	genAt             time.Time
}

func (f *flow) run(name string, args ...string) (string, error) {
	out, err := runIn(f.src, nil, name, args...)
	return string(out), err
}

func (f *flow) signRound(n int, key string, extra ...string) (string, error) {
	args := append([]string{"sign", "-key", key, "-to-sign", filepath.Join(f.out, fmt.Sprintf("to-sign-%d.json", n)), "-yes"}, extra...)
	return f.run(f.sign, args...)
}

func newFlow(t testing.TB, id string) *flow {
	t.Helper()
	return newFlowIn(t, id, t.TempDir(), t.TempDir())
}

func newFlowIn(t testing.TB, id, keyDir, srcDir string) *flow {
	t.Helper()
	rel, sign := tools(t)
	keyPath, keysPath, keys := keypairIn(t, id, keyDir)
	return &flow{t: t, release: rel, sign: sign, keyPath: keyPath, keysPath: keysPath, keys: keys, keyID: id, src: srcCopyIn(t, srcDir, keys)}
}

// 共享的完整发行演练（只做一次）。
func sharedFlow(t *testing.T) *flow {
	t.Helper()
	if testing.Short() {
		t.Skip("端到端流水线涉及真实多平台构建，-short 下跳过")
	}
	sharedOnce.Do(func() {
		f := newFlowIn(t, "rel-e2e-a", mkSub("keys"), mkSub("src"))
		f.out = filepath.Join(sharedDir, "out")
		out, err := f.run(f.release, "build", "-src", f.src, "-out", f.out, "-version", relVer, "-platforms", relPlats, "-repro-fresh-cache=false")
		if err != nil {
			sharedErr = fmt.Errorf("build: %v\n%s", err, out)
			return
		}
		if out, err := f.signRound(1, f.keyPath); err != nil {
			sharedErr = fmt.Errorf("sign 1: %v\n%s", err, out)
			return
		}
		f.afterR1 = filepath.Join(sharedDir, "after-r1")
		copyDir(t, f.out, f.afterR1)
		f.genAt = time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
		if out, err := f.run(f.release, "finalize", "-out", f.out, "-keys", f.keysPath, "-sequence", "5", "-base-url", baseURL, "-generated-at", f.genAt.Format(time.RFC3339)); err != nil {
			sharedErr = fmt.Errorf("finalize: %v\n%s", err, out)
			return
		}
		if out, err := f.signRound(2, f.keyPath); err != nil {
			sharedErr = fmt.Errorf("sign 2: %v\n%s", err, out)
			return
		}
		if out, err := f.run(f.release, "verify", "-out", f.out, "-keys", f.keysPath); err != nil {
			sharedErr = fmt.Errorf("verify: %v\n%s", err, out)
			return
		}
		shared = f
	})
	if sharedErr != nil {
		t.Fatal(sharedErr)
	}
	f := *shared
	f.t = t
	return &f
}

func mkSub(name string) string {
	d := filepath.Join(sharedDir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		panic(err)
	}
	return d
}

func (f *flow) pub(rel string) string {
	return filepath.Join(f.out, "publish", filepath.FromSlash(rel))
}

func readFile(t testing.TB, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newVerifier(t testing.TB, keys []byte, keysDir, state string, now time.Time) *manifest.Verifier {
	t.Helper()
	ts, err := manifest.LoadTrust(keys, keysDir)
	if err != nil {
		t.Fatal(err)
	}
	return &manifest.Verifier{Trust: ts, Seen: manifest.FileSequenceStore{Path: state}, Now: func() time.Time { return now }}
}

func acquire(t testing.TB, v *manifest.Verifier, bundle, comp, plat, keysDir string, exec ...string) (*downloader.Acquired, error) {
	t.Helper()
	root := t.TempDir()
	if keysDir == "" {
		keysDir = filepath.Join(root, "keys.d")
	}
	return downloader.Acquire(context.Background(), downloader.Params{Verifier: v, KeysDir: keysDir, BundlePath: bundle, Channel: "stable",
		Component: comp, Version: relVer, Variant: "default", Platform: plat, StagingRoot: filepath.Join(root, "staging"), Executable: exec, DeferCommit: true})
}

func wantManifestKind(t testing.TB, err error, kind manifest.Kind) {
	t.Helper()
	var me *manifest.Error
	if !errors.As(err, &me) || me.Kind != kind {
		t.Fatalf("期望 manifest 错误 %s，得到 %v", kind, err)
	}
}

// ---------------------------------------------------------------------------------------------------------------

func TestPublishLayoutAndManifestContent(t *testing.T) {
	f := sharedFlow(t)
	for _, rel := range []string{"stable/manifest.json", "stable/manifest.json.sig", "server/" + relVer + "/lingedge-server-" + relVer + "-linux-amd64.tar.gz",
		"server/" + relVer + "/lingedge-server-" + relVer + "-linux-arm64.tar.gz", "tools/" + relVer + "/lingedge-tools-" + relVer + "-linux-amd64.tar.gz",
		"lingedge-offline-" + relVer + ".tar", "SBOM.cdx.json", "RELEASE-RECORD.json", "repro.json", "build-record.json"} {
		if _, err := os.Stat(f.pub(rel)); err != nil {
			t.Errorf("发行目录缺少 %s", rel)
		}
	}
	mb := readFile(t, f.pub("stable/manifest.json"))
	m, err := manifest.ParseManifest(mb)
	if err != nil {
		t.Fatal(err)
	}
	if m.Sequence != 5 || m.Channel != "stable" || !m.ValidUntil.After(m.GeneratedAt.Add(80*24*time.Hour)) {
		t.Errorf("清单 sequence/channel/valid_until 不符: %+v", m)
	}
	if st := m.Components["server"].Status; st != manifest.CompAvailable {
		t.Errorf("server 应 available: %s", st)
	}
	for _, c := range []string{"node", "dns", "reporter", "admin-web", "console", "website", "docs", "status", "openresty"} {
		if m.Components[c].Status != manifest.CompUnreleased {
			t.Errorf("%s 尚无真实交付物，必须如实标 unreleased，得到 %q", c, m.Components[c].Status)
		}
	}
	if m.Components["scheduler"].Status != manifest.CompBuiltin || m.Components["worker"].BuiltinIn != "server" {
		t.Error("scheduler/worker 应为 builtin（内置于 server）")
	}
	sv := m.Components["server"].Versions[0]
	if len(sv.Ports) != 4 || sv.Requires == nil || sv.Requires.External["mysql"] == "" || sv.ConfigSchemaVersion != 1 {
		t.Errorf("server 的 ports/requires/config_schema_version 缺失: %+v", sv)
	}
	// 迁移预告取自真实的 lingedge-server migrate plan：与独立执行的结果一致。
	out := mustRun(t, repoRoot(t), "go", "run", "./cmd/lingedge-server", "migrate", "plan", "--offline")
	var plan struct {
		Latest int64 `json:"latest_version"`
		Count  int   `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatal(err)
	}
	if sv.Migrations == nil || sv.Migrations.SchemaFrom != "0000" || sv.Migrations.SchemaTo != fmt.Sprintf("%04d", plan.Latest) || plan.Count < 13 {
		t.Errorf("迁移预告与 migrate plan 不符: %+v plan=%+v", sv.Migrations, plan)
	}
	// URL 在 release.base_url 之下；清单 URL 的末段 = 包文件名（离线 bundle 约定）。
	for _, v := range sv.Variants {
		for plat, a := range v.Artifacts {
			if !strings.HasPrefix(a.URL, baseURL+"/server/"+relVer+"/") || !strings.HasSuffix(a.URL, plat+".tar.gz") {
				t.Errorf("artifact URL 不在发行源下: %s", a.URL)
			}
			fi, err := os.Stat(f.pub("server/" + relVer + "/" + a.URL[strings.LastIndex(a.URL, "/")+1:]))
			if err != nil || fi.Size() != a.Size {
				t.Errorf("artifact 大小与文件不符: %v", err)
			}
		}
	}
	// Tools 侧配置：release.base_url 决定清单地址，与发行目录布局 <通道>/manifest.json 对得上（T-3）。
	cfg, err := installer.ParseToolsConfig([]byte("release:\n  base_url: " + baseURL + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := cfg.ManifestURL("stable")
	if err != nil || u != baseURL+"/stable/manifest.json" {
		t.Fatalf("ManifestURL=%q %v", u, err)
	}
	if _, err := os.Stat(f.pub(strings.TrimPrefix(u, baseURL+"/"))); err != nil {
		t.Errorf("发行目录布局与 Tools 的清单地址约定不符: %v", err)
	}
	// SBOM：CycloneDX，含 Go 依赖；发行记录登记了每个文件的 sha256。
	var bom struct {
		BOMFormat  string `json:"bomFormat"`
		Components []struct {
			Type string `json:"type"`
			PURL string `json:"purl"`
		} `json:"components"`
	}
	if err := json.Unmarshal(readFile(t, f.pub("SBOM.cdx.json")), &bom); err != nil || bom.BOMFormat != "CycloneDX" || len(bom.Components) < 20 {
		t.Errorf("汇总 SBOM 异常: %v %d", err, len(bom.Components))
	}
	var rec struct {
		Files map[string]string `json:"files"`
	}
	_ = json.Unmarshal(readFile(t, f.pub("RELEASE-RECORD.json")), &rec)
	for rel, want := range rec.Files {
		sum := sha256.Sum256(readFile(t, f.pub(rel)))
		if hex.EncodeToString(sum[:]) != want {
			t.Errorf("RELEASE-RECORD 中 %s 的 sha256 不符", rel)
		}
	}
	if len(rec.Files) < 8 {
		t.Errorf("RELEASE-RECORD 文件数异常: %d", len(rec.Files))
	}
}

// AC：测试密钥签名 → Tools 验签通过。
func TestToolsVerifiesPublishedRelease(t *testing.T) {
	f := sharedFlow(t)
	now := time.Now().UTC()
	state := filepath.Join(t.TempDir(), "trust", "state")
	v := newVerifier(t, f.keys, "", state, now)
	res, err := v.Verify(readFile(t, f.pub("stable/manifest.json")), readFile(t, f.pub("stable/manifest.json.sig")), manifest.VerifyOptions{Channel: "stable"})
	if err != nil {
		t.Fatalf("Tools 验签应通过: %v", err)
	}
	if res.KeyID != f.keyID || res.Manifest.Sequence != 5 {
		t.Errorf("验签结果: %+v", res)
	}
	bundle := f.pub("lingedge-offline-" + relVer + ".tar")
	for _, c := range []struct{ comp, plat string }{{"server", "linux-amd64"}, {"server", "linux-arm64"}, {"tools", "linux-amd64"}, {"tools", "linux-arm64"}} {
		bin := "payload/bin/lingedge-server"
		if c.comp == "tools" {
			bin = "payload/bin/lingedge"
		}
		acq, err := acquire(t, newVerifier(t, f.keys, "", filepath.Join(t.TempDir(), "s"), now), bundle, c.comp, c.plat, "", bin)
		if err != nil {
			t.Fatalf("%s %s 应通过 Tools 的完整校验链: %v", c.comp, c.plat, err)
		}
		if acq.Meta.Component != c.comp || acq.Meta.Version != relVer || acq.Meta.Platform != c.plat || acq.Extract.Files != acq.Meta.FilesCount+3 /* META 三个文件 */ {
			t.Errorf("META 与解压结果不符: %+v %+v", acq.Meta, acq.Extract)
		}
		// 解压结果：二进制可执行、版本信息已注入、SBOM 随包且是合法 CycloneDX。
		fi, err := os.Stat(filepath.Join(acq.PkgDir, filepath.FromSlash(bin)))
		if err != nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("二进制缺失或不可执行: %v", err)
		}
		binBytes := readFile(t, filepath.Join(acq.PkgDir, filepath.FromSlash(bin)))
		var rec struct {
			Commit string `json:"commit"`
		}
		_ = json.Unmarshal(readFile(t, f.pub("build-record.json")), &rec)
		if !bytes.Contains(binBytes, []byte(rec.Commit)) || !bytes.Contains(binBytes, []byte(relVer)) {
			t.Errorf("二进制里没有注入发行版本/提交（ldflags）")
		}
		var bom map[string]any
		if err := json.Unmarshal(readFile(t, filepath.Join(acq.PkgDir, "payload/share/lingedge-sbom.cdx.json")), &bom); err != nil || bom["bomFormat"] != "CycloneDX" {
			t.Errorf("包内 SBOM 缺失或非法: %v", err)
		}
		if c.comp == "server" {
			if _, err := os.Stat(filepath.Join(acq.PkgDir, "payload/share/config/server.example.yaml")); err != nil {
				t.Error("server 包应含示例配置")
			}
		}
		_ = acq.Discard()
	}
	// 验证通过且提交后 sequence 才推进（与 downloader 约定一致）；重放同一清单（相等）仍允许。
	if err := v.Commit(res, filepath.Join(t.TempDir(), "keys.d")); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(readFile(t, f.pub("stable/manifest.json")), readFile(t, f.pub("stable/manifest.json.sig")), manifest.VerifyOptions{Channel: "stable"}); err != nil {
		t.Errorf("同 sequence 重放应允许（幂等重取）: %v", err)
	}
}

// AC：篡改 → 拒绝（Tools 侧）。
func TestTamperingIsRejectedByTools(t *testing.T) {
	f := sharedFlow(t)
	now := time.Now().UTC()
	mb := readFile(t, f.pub("stable/manifest.json"))
	sb := readFile(t, f.pub("stable/manifest.json.sig"))
	verify := func(keys, m, s []byte, n time.Time, opt manifest.VerifyOptions, state string) error {
		if state == "" {
			state = filepath.Join(t.TempDir(), "state")
		}
		_, err := newVerifier(t, keys, "", state, n).Verify(m, s, opt)
		return err
	}
	stable := manifest.VerifyOptions{Channel: "stable"}

	flip := append([]byte(nil), mb...)
	flip[len(flip)/2] ^= 1
	wantManifestKind(t, verify(f.keys, flip, sb, now, stable, ""), manifest.KindSignature)

	// 用另一把（不在公钥集里的）密钥签名 → unknown_key_id。
	keyB, _, _ := keypair(t, "rel-e2e-evil")
	evil := filepath.Join(t.TempDir(), "evil")
	for _, n := range []string{"manifest.json", "to-sign-2.json"} {
		copyFile(t, filepath.Join(f.out, n), filepath.Join(evil, n), 0o644)
	}
	if out, err := runIn(f.src, nil, f.sign, "sign", "-key", keyB, "-to-sign", filepath.Join(evil, "to-sign-2.json"), "-yes"); err != nil {
		t.Fatalf("签名失败: %v\n%s", err, out)
	}
	evilSig := readFile(t, filepath.Join(evil, "manifest.json.sig"))
	wantManifestKind(t, verify(f.keys, mb, evilSig, now, stable, ""), manifest.KindUnknownKey)
	// 把 key_id 改成合法的 A，但签名仍是 B 的 → signature_invalid。
	var sd map[string]string
	_ = json.Unmarshal(evilSig, &sd)
	sd["key_id"] = f.keyID
	forged, _ := json.Marshal(sd)
	wantManifestKind(t, verify(f.keys, mb, forged, now, stable, ""), manifest.KindSignature)

	// 回滚：本地已见更大的 sequence。
	state := filepath.Join(t.TempDir(), "trust", "state")
	if err := (manifest.FileSequenceStore{Path: state}).Advance("stable", 99); err != nil {
		t.Fatal(err)
	}
	wantManifestKind(t, verify(f.keys, mb, sb, now, stable, state), manifest.KindRollback)
	// 过期（valid_until 之后）；在线路径永不放行。
	m, _ := manifest.ParseManifest(mb)
	wantManifestKind(t, verify(f.keys, mb, sb, m.ValidUntil.Add(time.Second), stable, ""), manifest.KindExpired)
	// 通道不符。
	wantManifestKind(t, verify(f.keys, mb, sb, now, manifest.VerifyOptions{Channel: "beta"}, ""), manifest.KindChannel)
	// 密钥被吊销（keys.json 的 revoked_key_ids）。
	var kd map[string]any
	_ = json.Unmarshal(f.keys, &kd)
	_, _, otherKeys := keypair(t, "rel-e2e-spare")
	var od struct {
		Keys []any `json:"keys"`
	}
	_ = json.Unmarshal(otherKeys, &od)
	kd["keys"] = append(kd["keys"].([]any), od.Keys...) // 另有一把在用的密钥，才不会因「没有任何可用公钥」而走另一条失败路径
	kd["revoked_key_ids"] = []string{f.keyID}
	revoked, _ := json.Marshal(kd)
	wantManifestKind(t, verify(revoked, mb, sb, now, stable, ""), manifest.KindKeyRevoked)

	// 包体被篡改（离线 bundle 里翻转一个字节）→ 外层 sha256 不符。
	bundle := f.pub("lingedge-offline-" + relVer + ".tar")
	bad := filepath.Join(t.TempDir(), "bad.tar")
	rewriteBundle(t, bundle, bad, func(name string, body []byte) []byte {
		if strings.HasSuffix(name, "lingedge-server-"+relVer+"-linux-amd64.tar.gz") {
			b := append([]byte(nil), body...)
			b[len(b)/2] ^= 1
			return b
		}
		return body
	})
	_, err := acquire(t, newVerifier(t, f.keys, "", filepath.Join(t.TempDir(), "s"), now), bad, "server", "linux-amd64", "")
	var de *downloader.Error
	if !errors.As(err, &de) || (de.Kind != downloader.KindSHA256 && de.Kind != downloader.KindSize) {
		t.Fatalf("包体被篡改应以 sha256/size 校验失败拒绝，得到 %v", err)
	}
	// 清单被换成 B 签名的另一份（bundle 里替换 .sig）→ 拒绝。
	bad2 := filepath.Join(t.TempDir(), "bad2.tar")
	rewriteBundle(t, bundle, bad2, func(name string, body []byte) []byte {
		if strings.HasSuffix(name, "manifest.json.sig") {
			return evilSig
		}
		return body
	})
	_, err = acquire(t, newVerifier(t, f.keys, "", filepath.Join(t.TempDir(), "s"), now), bad2, "server", "linux-amd64", "")
	wantManifestKind(t, err, manifest.KindUnknownKey)
	// 包错位：tools 的包文件内容放在 server 的文件名下 → 清单 sha256 不符。
	bad3 := filepath.Join(t.TempDir(), "bad3.tar")
	var toolsPkg []byte
	rewriteBundle(t, bundle, filepath.Join(t.TempDir(), "x.tar"), func(name string, body []byte) []byte {
		if strings.HasSuffix(name, "lingedge-tools-"+relVer+"-linux-amd64.tar.gz") {
			toolsPkg = body
		}
		return body
	})
	rewriteBundle(t, bundle, bad3, func(name string, body []byte) []byte {
		if strings.HasSuffix(name, "lingedge-server-"+relVer+"-linux-amd64.tar.gz") {
			return toolsPkg
		}
		return body
	})
	if _, err := acquire(t, newVerifier(t, f.keys, "", filepath.Join(t.TempDir(), "s"), now), bad3, "server", "linux-amd64", ""); err == nil {
		t.Fatal("包错位必须被拒绝")
	}
}

// 流水线自己的 verify 对篡改同样失败，且失败时不产出 publish/。
func TestPipelineVerifyRejectsTampering(t *testing.T) {
	f := sharedFlow(t)
	prep := func() string {
		d := filepath.Join(t.TempDir(), "out")
		for _, sub := range []string{"packages", "signatures"} {
			copyDir(t, filepath.Join(f.out, sub), filepath.Join(d, sub))
		}
		for _, n := range []string{"plan.json", "manifest.json", "manifest.json.sig", "to-sign-2.json", "SBOM.cdx.json", "build-record.json", "repro.json"} {
			copyFile(t, filepath.Join(f.out, n), filepath.Join(d, n), 0o644)
		}
		return d
	}
	verify := func(d string, keys string) (string, error) {
		return f.run(f.release, "verify", "-out", d, "-keys", keys)
	}
	if out, err := verify(prep(), f.keysPath); err != nil {
		t.Fatalf("未篡改的副本应通过: %v\n%s", err, out)
	}
	flipFile := func(p string) {
		b := readFile(t, p)
		b[len(b)/2] ^= 1
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(d string){
		"包体字节翻转": func(d string) {
			flipFile(filepath.Join(d, "packages/server", relVer, "lingedge-server-"+relVer+"-linux-amd64.tar.gz"))
		},
		"清单字节翻转": func(d string) { flipFile(filepath.Join(d, "manifest.json")) },
		"缺少清单签名": func(d string) { _ = os.Remove(filepath.Join(d, "manifest.json.sig")) },
		"清单签名被翻转": func(d string) {
			b := readFile(t, filepath.Join(d, "manifest.json.sig"))
			b[len(b)-8] ^= 1
			_ = os.WriteFile(filepath.Join(d, "manifest.json.sig"), b, 0o644)
		},
		"包缺失": func(d string) {
			_ = os.Remove(filepath.Join(d, "packages/tools", relVer, "lingedge-tools-"+relVer+"-linux-arm64.tar.gz"))
		},
	}
	for name, mut := range cases {
		d := prep()
		mut(d)
		if out, err := verify(d, f.keysPath); err == nil {
			t.Errorf("%s: verify 必须失败\n%s", name, out)
		}
		if _, err := os.Stat(filepath.Join(d, "publish")); err == nil {
			t.Errorf("%s: 失败时不得产出 publish/", name)
		}
	}
	// 公钥集不含签名者 → 失败。
	_, otherKeys, _ := keypair(t, "rel-e2e-other")
	if _, err := verify(prep(), otherKeys); err == nil {
		t.Error("用不含签名者的公钥集验证必须失败")
	}
}

// finalize 对第 1 轮签名同样把关。
func TestFinalizeRejectsBadFirstRoundSignatures(t *testing.T) {
	f := sharedFlow(t)
	fin := func(d string, keys string, extra ...string) (string, error) {
		args := append([]string{"finalize", "-out", d, "-keys", keys, "-sequence", "9", "-base-url", baseURL}, extra...)
		return f.run(f.release, args...)
	}
	// 缺少签名。
	d1 := filepath.Join(t.TempDir(), "o1")
	copyDir(t, f.afterR1, d1)
	_ = os.Remove(filepath.Join(d1, "signatures", "tools-"+relVer+"-linux-arm64.sig"))
	if _, err := fin(d1, f.keysPath); err == nil {
		t.Error("缺少某个包的 .sig 必须失败")
	}
	// 非签名者（公钥集里没有）。
	d2 := filepath.Join(t.TempDir(), "o2")
	copyDir(t, f.afterR1, d2)
	_, otherKeys, _ := keypair(t, "rel-e2e-other2")
	if _, err := fin(d2, otherKeys); err == nil {
		t.Error("签名者不在 -keys 中必须失败")
	}
	// 签名之后 stage 被改动（调包）。
	d3 := filepath.Join(t.TempDir(), "o3")
	copyDir(t, f.afterR1, d3)
	p := filepath.Join(d3, "stage", "server-"+relVer+"-linux-amd64", "payload", "bin", "lingedge-server")
	b := readFile(t, p)
	b[len(b)/2] ^= 1
	if err := os.WriteFile(p, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := fin(d3, f.keysPath); err == nil || !strings.Contains(out, "不一致") {
		t.Errorf("stage 在签名后被改动必须失败: %v\n%s", err, out)
	}
	// sequence 回退（-prev-manifest）：必须严格大于上一份清单的 sequence（5）。
	d5 := filepath.Join(t.TempDir(), "o5")
	copyDir(t, f.afterR1, d5)
	out, err := f.run(f.release, "finalize", "-out", d5, "-keys", f.keysPath, "-sequence", "5", "-base-url", baseURL, "-prev-manifest", f.pub("stable/manifest.json"))
	if err == nil || !strings.Contains(out, "严格大于") {
		t.Errorf("sequence 不大于上一份必须失败: %v\n%s", err, out)
	}
	// 非 https / 带凭据的发行源。
	for _, bad := range []string{"http://r.example.invalid", "https://u:" + "p@r.example.invalid"} {
		d := filepath.Join(t.TempDir(), "ob")
		copyDir(t, f.afterR1, d)
		if _, err := f.run(f.release, "finalize", "-out", d, "-keys", f.keysPath, "-sequence", "9", "-base-url", bad); err == nil {
			t.Errorf("发行源 %q 必须被拒绝", bad)
		}
	}
}

// AC3：可复现性——同源两次构建产物哈希一致的验证记录。
func TestReproducibilityRecord(t *testing.T) {
	f := sharedFlow(t)
	var repro struct {
		AllEqual bool `json:"all_equal"`
		Binaries []struct {
			Name, Platform string
			A              string `json:"sha256_build_a"`
			B              string `json:"sha256_build_b"`
			Equal          bool
		} `json:"binaries"`
		SrcDiffers bool `json:"second_build_source_dir_differs"`
	}
	if err := json.Unmarshal(readFile(t, f.pub("repro.json")), &repro); err != nil {
		t.Fatal(err)
	}
	if !repro.AllEqual || !repro.SrcDiffers || len(repro.Binaries) != 4 {
		t.Fatalf("repro.json 异常: %+v", repro)
	}
	for _, b := range repro.Binaries {
		if !b.Equal || b.A != b.B || len(b.A) != 64 {
			t.Errorf("%s/%s 两次构建哈希不一致: %s %s", b.Platform, b.Name, b.A, b.B)
		}
	}
	// 记录里的哈希就是包里实际的二进制的哈希。
	for _, b := range repro.Binaries {
		var stage string
		switch b.Name {
		case "lingedge-server":
			stage = "server"
		case "lingedge":
			stage = "tools"
		}
		sum := sha256.Sum256(readFile(t, filepath.Join(f.afterR1, "stage", stage+"-"+relVer+"-"+b.Platform, "payload", "bin", b.Name)))
		if hex.EncodeToString(sum[:]) != b.A {
			t.Errorf("repro.json 的哈希与 stage 里的二进制不符: %s %s", b.Platform, b.Name)
		}
	}
}

// AC1/AC3：生产构建参数——-trimpath、无 VCS 戳、CGO 关闭、无 lingedge_testdouble 标签、二进制里没有测试替身与测试公钥。
func TestProductionBuildFlags(t *testing.T) {
	f := sharedFlow(t)
	bin := filepath.Join(f.afterR1, "stage", "server-"+relVer+"-linux-amd64", "payload", "bin", "lingedge-server")
	out := mustRun(t, t.TempDir(), "go", "version", "-m", bin)
	for _, want := range []string{"-trimpath=true", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "path\tgithub.com/wuekevin/lingedge/cmd/lingedge-server"} {
		if !strings.Contains(out, want) {
			t.Errorf("go version -m 缺少 %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"-tags", "lingedge_testdouble", "vcs."} {
		if strings.Contains(out, bad) {
			t.Errorf("生产构建不应出现 %q:\n%s", bad, out)
		}
	}
	data := readFile(t, bin)
	if bytes.Contains(data, []byte("lingedge/internal/testdouble")) {
		t.Error("生产二进制不得包含测试替身包")
	}
	// 二进制里没有测试公钥（raw/base64/hex）。
	for _, line := range strings.Split(string(readFile(t, filepath.Join(repoRoot(t), "scripts/gates/data/test-release-pubkeys.txt"))), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pub := strings.Fields(line)[0]
		if bytes.Contains(data, []byte(pub)) {
			t.Errorf("生产二进制含测试公钥 %s", pub)
		}
	}
	// 注意：cmd/lingedge 目前只有 --version 脚手架、尚未链接 internal/tools/manifest，所以发行二进制里还没有 go:embed 的 keys.json；
	// 内置公钥对「真实产物」的覆盖（发行公钥确实被内置、测试公钥不在其中）随 Tools 命令接线（Story 1.35）生效，
	// 守卫逻辑本身（gates release-keys、tests/security/releasekeys 的真实 go build 扫描、上面对产物的测试公钥扫描）已就绪。
	var rec struct {
		Dirty bool   `json:"dirty"`
		Tags  string `json:"build_tags"`
	}
	_ = json.Unmarshal(readFile(t, f.pub("build-record.json")), &rec)
	if rec.Dirty || rec.Tags != "(none)" {
		t.Errorf("构建记录应为干净提交、无标签: %+v", rec)
	}
}

// AC5（与 1.27 联动）：含测试公钥/测试 key_id/缺失的 keys.json → 发行构建失败，且在构建任何产物之前失败。
func TestReleaseBuildRejectsBadBuiltinKeys(t *testing.T) {
	rel, _ := tools(t)
	_, _, good := keypair(t, "rel-e2e-g")
	var gd struct {
		Keys []map[string]any `json:"keys"`
	}
	_ = json.Unmarshal(good, &gd)
	mk := func(mut func(k map[string]any)) []byte {
		k := map[string]any{}
		for a, b := range gd.Keys[0] {
			k[a] = b
		}
		mut(k)
		b, _ := json.Marshal(map[string]any{"schema_version": 1, "keys": []any{k}, "revoked_key_ids": []string{}})
		return b
	}
	cases := map[string]struct {
		keys []byte
		want string
	}{
		"golden 测试公钥":    {mk(func(k map[string]any) { k["public_key"] = "P3pE/lvUPOLOFWJtF/1sxj4LU923fmJQU9Utz1K5ghc=" }), "release-keys-test-pubkey"},
		"test 字样 key_id": {mk(func(k map[string]any) { k["key_id"] = "test-2026" }), "release-keys-test-key-id"},
		"含私钥字段":          {mk(func(k map[string]any) { k["seed"] = "AAAA" }), "release-keys-private-material"},
		"缺失 keys.json":   {nil, "release-keys-missing"},
	}
	for name, c := range cases {
		src := srcCopy(t, c.keys)
		out := filepath.Join(t.TempDir(), "out")
		cmd, err := runIn(src, nil, rel, "build", "-src", src, "-out", out, "-version", relVer, "-platforms", "linux-amd64")
		if err == nil {
			t.Errorf("%s: 发行构建必须失败", name)
			continue
		}
		if !strings.Contains(string(cmd), c.want) {
			t.Errorf("%s: 期望命中 %s:\n%s", name, c.want, cmd)
		}
		if _, err := os.Stat(filepath.Join(out, "stage")); err == nil {
			t.Errorf("%s: 守卫失败时不得已经产出 stage", name)
		}
	}
	// 脏工作树：发行必须来自干净提交（演练需显式 -allow-dirty）。
	src := srcCopy(t, good)
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runIn(src, nil, rel, "build", "-src", src, "-out", filepath.Join(t.TempDir(), "o"), "-version", relVer, "-platforms", "linux-amd64"); err == nil || !strings.Contains(string(out), "未提交") {
		t.Errorf("脏工作树必须被拒绝: %v\n%s", err, out)
	}
}

// 公钥轮换端到端：旧 active key 背书新公钥 → Tools 验证并落盘 → 之后由新密钥签名的清单可被验证。
func TestKeyRotationEndToEnd(t *testing.T) {
	f := sharedFlow(t)
	now := time.Now().UTC()
	d := filepath.Join(t.TempDir(), "o")
	copyDir(t, f.afterR1, d)
	keyB, keysBPath, _ := keypair(t, "rel-e2e-b")
	rot := filepath.Join(t.TempDir(), "rot.json")
	if out, err := runIn(f.src, nil, f.sign, "endorse", "-key", f.keyPath, "-new-keys", keysBPath, "-out", rot); err != nil {
		t.Fatalf("endorse: %v\n%s", err, out)
	}
	finalize := func(seq string, extra ...string) {
		t.Helper()
		args := append([]string{"finalize", "-out", d, "-keys", f.keysPath, "-sequence", seq, "-base-url", baseURL, "-generated-at", f.genAt.Format(time.RFC3339)}, extra...)
		if out, err := f.run(f.release, args...); err != nil {
			t.Fatalf("finalize: %v\n%s", err, out)
		}
	}
	sign := func(key string) {
		t.Helper()
		_ = os.Remove(filepath.Join(d, "manifest.json.sig"))
		if out, err := runIn(f.src, nil, f.sign, "sign", "-key", key, "-to-sign", filepath.Join(d, "to-sign-2.json"), "-yes"); err != nil {
			t.Fatalf("sign: %v\n%s", err, out)
		}
	}
	// 发行 N：携带 key_rotation，由旧 key A 签名。
	finalize("6", "-key-rotation", rot)
	sign(f.keyPath)
	if out, err := f.run(f.release, "verify", "-out", d, "-keys", f.keysPath, "-now", now.Format(time.RFC3339)); err != nil {
		t.Fatalf("带 key_rotation 的发行应通过 Tools 校验链: %v\n%s", err, out)
	}
	mb, sb := readFile(t, filepath.Join(d, "manifest.json")), readFile(t, filepath.Join(d, "manifest.json.sig"))
	keysDir := filepath.Join(t.TempDir(), "trust", "keys.d")
	v := newVerifier(t, f.keys, keysDir, filepath.Join(t.TempDir(), "state"), now)
	res, err := v.Verify(mb, sb, manifest.VerifyOptions{Channel: "stable"})
	if err != nil || len(res.Rotation) != 1 || res.Rotation[0].KeyID != "rel-e2e-b" {
		t.Fatalf("应验证出一把待落盘的新公钥: %v %+v", err, res)
	}
	if err := v.Commit(res, keysDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(keysDir, "rel-e2e-b.json")); err != nil {
		t.Fatal("新公钥应已落盘到 trust/keys.d")
	}
	// 发行 N+1：只由新密钥 B 签清单（内置公钥集仍只有 A，B 来自 keys.d）。包内签名仍是 A（仍在可信集）。
	finalize("7")
	sign(keyB)
	mb, sb = readFile(t, filepath.Join(d, "manifest.json")), readFile(t, filepath.Join(d, "manifest.json.sig"))
	if _, err := newVerifier(t, f.keys, keysDir, filepath.Join(t.TempDir(), "s2"), now).Verify(mb, sb, manifest.VerifyOptions{Channel: "stable"}); err != nil {
		t.Fatalf("轮换落盘后，新密钥签名的清单应通过: %v", err)
	}
	_, err = newVerifier(t, f.keys, "", filepath.Join(t.TempDir(), "s3"), now).Verify(mb, sb, manifest.VerifyOptions{Channel: "stable"})
	wantManifestKind(t, err, manifest.KindUnknownKey) // 没有 keys.d 时（只有内置 A）不认识 B
	// 背书者不可信（未知密钥 C 背书 B）→ 整份清单被拒绝（退出码 5 的 rotation_invalid）。
	keyC, _, _ := keypair(t, "rel-e2e-c")
	rot2 := filepath.Join(t.TempDir(), "rot2.json")
	if out, err := runIn(f.src, nil, f.sign, "endorse", "-key", keyC, "-new-keys", keysBPath, "-out", rot2); err != nil {
		t.Fatalf("endorse: %v\n%s", err, out)
	}
	finalize("8", "-key-rotation", rot2)
	sign(f.keyPath)
	mb, sb = readFile(t, filepath.Join(d, "manifest.json")), readFile(t, filepath.Join(d, "manifest.json.sig"))
	_, err = newVerifier(t, f.keys, "", filepath.Join(t.TempDir(), "s4"), now).Verify(mb, sb, manifest.VerifyOptions{Channel: "stable"})
	wantManifestKind(t, err, manifest.KindRotation)
	if out, err := f.run(f.release, "verify", "-out", d, "-keys", f.keysPath); err == nil {
		t.Errorf("背书者不可信的发行必须无法通过流水线 verify\n%s", out)
	}
}

// AC6：OpenResty 发行目录里的符号链接在构建期消除；SafeExtract 不放宽，包里没有任何链接。
func TestOpenRestyTreeSymlinksEliminated(t *testing.T) {
	if testing.Short() {
		t.Skip("涉及真实构建，-short 下跳过")
	}
	f := newFlow(t, "rel-e2e-or")
	tree := t.TempDir()
	write := func(rel, body string, mode os.FileMode) {
		p := filepath.Join(tree, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, rel string) {
		p := filepath.Join(tree, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	write("nginx/sbin/nginx", "NGINX-ELF", 0o755)
	write("luajit/lib/libluajit-5.1.so.2.1.0", "LUAJIT", 0o644)
	write("lualib/resty/core.lua", "return {}", 0o644)
	link("../nginx/sbin/nginx", "bin/openresty")
	link("libluajit-5.1.so.2.1.0", "luajit/lib/libluajit-5.1.so.2")
	link("libluajit-5.1.so.2", "luajit/lib/libluajit-5.1.so")
	link("../lualib", "site/lualib")

	out := filepath.Join(t.TempDir(), "out")
	args := []string{"build", "-src", f.src, "-out", out, "-version", relVer, "-platforms", "linux-amd64", "-repro=false", "-openresty-version", "1.27.1", "-openresty-tree", "linux-amd64=" + tree}
	if o, err := f.run(f.release, args...); err != nil {
		t.Fatalf("build: %v\n%s", err, o)
	}
	f.out = out
	if o, err := f.signRound(1, f.keyPath); err != nil {
		t.Fatalf("sign1: %v\n%s", err, o)
	}
	for _, step := range [][]string{
		{"finalize", "-out", out, "-keys", f.keysPath, "-sequence", "1", "-base-url", baseURL},
	} {
		if o, err := f.run(f.release, step...); err != nil {
			t.Fatalf("%v: %v\n%s", step[0], err, o)
		}
	}
	if o, err := f.signRound(2, f.keyPath); err != nil {
		t.Fatalf("sign2: %v\n%s", err, o)
	}
	if o, err := f.run(f.release, "verify", "-out", out, "-keys", f.keysPath); err != nil {
		t.Fatalf("verify: %v\n%s", err, o)
	}
	// 清单：openresty 为 available + internal。
	m, err := manifest.ParseManifest(readFile(t, filepath.Join(out, "manifest.json")))
	if err != nil {
		t.Fatal(err)
	}
	if c := m.Components["openresty"]; c.Status != manifest.CompAvailable || !c.Internal || len(c.Versions) != 1 {
		t.Fatalf("openresty 应 available+internal: %+v", c)
	}
	// 包内只有普通文件与目录（逐条目检查 tar 头）。
	pkg := readFile(t, filepath.Join(out, "publish", "openresty", relVer, "lingedge-openresty-"+relVer+"-linux-amd64.tar.gz"))
	zr, err := gzip.NewReader(bytes.NewReader(pkg))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			t.Errorf("包内出现非普通文件条目 %s（类型 %c）", h.Name, h.Typeflag)
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	for name, want := range map[string]string{"payload/bin/openresty": "NGINX-ELF", "payload/luajit/lib/libluajit-5.1.so": "LUAJIT", "payload/site/lualib/resty/core.lua": "return {}", "payload/nginx/sbin/nginx": "NGINX-ELF"} {
		if got[name] != want {
			t.Errorf("%s 内容应为目标内容 %q，得到 %q", name, want, got[name])
		}
	}
	// 经 Tools 校验链取出，解压结果里没有符号链接。
	acq, err := acquire(t, newVerifier(t, f.keys, "", filepath.Join(t.TempDir(), "s"), time.Now().UTC()), filepath.Join(out, "publish", "lingedge-offline-"+relVer+".tar"), "openresty", "linux-amd64", "", "payload/bin/openresty", "payload/nginx/sbin/nginx")
	if err != nil {
		t.Fatalf("openresty 包应通过 Tools 校验链: %v", err)
	}
	_ = filepath.WalkDir(acq.PkgDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink != 0 {
			t.Errorf("解压结果含符号链接 %s", p)
		}
		return nil
	})
	_ = acq.Discard()
	// 构建记录登记了被消除的链接。
	var rec struct {
		Links []struct{ Path, Target, Kind string } `json:"links_resolved"`
	}
	_ = json.Unmarshal(readFile(t, filepath.Join(out, "publish", "build-record.json")), &rec)
	if len(rec.Links) != 4 {
		t.Errorf("应记录 4 条被消除的链接，得到 %+v", rec.Links)
	}

	// 无法安全消除的链接 → 构建失败（逃逸/悬空）。
	for name, mut := range map[string]func(){
		"逃逸":  func() { link("../../../../../../etc/hosts", "bin/escape") },
		"悬空":  func() { link("nowhere", "bin/dangling") },
		"目录环": func() { link("..", "site/up") },
	} {
		mut()
		o := filepath.Join(t.TempDir(), "o")
		args := []string{"build", "-src", f.src, "-out", o, "-version", relVer, "-platforms", "linux-amd64", "-repro=false", "-openresty-version", "1.27.1", "-openresty-tree", "linux-amd64=" + tree}
		if out, err := f.run(f.release, args...); err == nil {
			t.Errorf("%s: 无法安全消除的链接必须使构建失败\n%s", name, out)
		}
		_ = os.Remove(filepath.Join(tree, "bin", "escape"))
		_ = os.Remove(filepath.Join(tree, "bin", "dangling"))
		_ = os.Remove(filepath.Join(tree, "site", "up"))
	}
}

// SafeExtract 不放宽：含符号链接条目的归档被拒绝（Tools 侧的最后防线，与流水线的构建期消除相互独立）。
func TestSafeExtractStillRejectsLinks(t *testing.T) {
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	body := []byte("x")
	_ = tw.WriteHeader(&tar.Header{Name: "payload/a", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	_, _ = tw.Write(body)
	_ = tw.WriteHeader(&tar.Header{Name: "payload/l", Typeflag: tar.TypeSymlink, Linkname: "a", Mode: 0o777})
	_ = tw.Close()
	_ = zw.Close()
	sum := sha256.Sum256(body)
	dst := filepath.Join(t.TempDir(), "x")
	_, err := security.SafeExtract(context.Background(), &raw, dst, security.ExtractOptions{Gzip: true, Checksums: map[string]string{"payload/a": hex.EncodeToString(sum[:])}})
	if !errors.Is(err, security.ErrUnsupportedType) {
		t.Fatalf("符号链接条目必须被 SafeExtract 拒绝: %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("失败后不得留下解压目标")
	}
}

// 仓库密钥扫描（G-7）：真实仓库通过；负向样本见 scripts/gates/secrets_test.go。
func TestRepoSecretScanPasses(t *testing.T) {
	out, err := runIn(repoRoot(t), nil, "go", "run", "./scripts/gates", "secrets")
	if err != nil {
		t.Fatalf("gates secrets 失败:\n%s", out)
	}
}
