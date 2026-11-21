package downloader_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

// 外部可控的解析入口 fuzz：离线 bundle 的 tar 索引/布局、以及完整的离线校验链（清单、签名、包全部来自 bundle）。

func fuzzWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	w := &world{t: t, key: deriveKey("rel-dl-a", "a"), now: t0, base: "https://releases.example.invalid",
		trustDir: filepath.Join(root, "trust"), staging: filepath.Join(root, "staging")}
	w.keysDir = filepath.Join(w.trustDir, "keys.d")
	for _, d := range []string{w.trustDir, w.staging} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	w.trust = w.mkTrust(w.key)
	return w
}

func seedBundles(f *testing.F) [][]byte {
	w := &world{key: deriveKey("rel-dl-a", "a"), base: "https://releases.example.invalid"}
	good := pkgSpec{signer: w.key}.build(f)
	s := scenario{pkg: good}
	mf, sig := s.build(w)
	out := [][]byte{
		bundleBytes(f, ent{name: "manifest.json", body: mf}, ent{name: "manifest.json.sig", body: sig}, ent{name: "packages/" + pkgFile, body: good}),
		bundleBytes(f, ent{name: "offline-bundle/", typ: tar.TypeDir}, ent{name: "offline-bundle/manifest.json", body: mf}, ent{name: "offline-bundle/manifest.json.sig", body: sig},
			ent{name: "offline-bundle/packages/" + pkgFile, body: good}),
		bundleBytes(f, ent{name: "manifest.json", body: mf}),
		bundleBytes(f, ent{name: "../x", body: []byte("x")}),
		bundleBytes(f, ent{name: "packages/ln", typ: tar.TypeSymlink, link: "/etc/passwd"}),
		[]byte("not a tar"),
		bytes.Repeat([]byte{0}, 2048),
	}
	return out
}

// FuzzOpenBundle：任意字节作为 bundle 文件。不变量：不 panic；失败必为 KindBundle；成功时索引的每个条目都落在文件范围内、
// 名称都是布局允许的形态，且 Manifest() 读出的字节长度等于索引声明的大小。
func FuzzOpenBundle(f *testing.F) {
	for _, s := range seedBundles(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		p := filepath.Join(t.TempDir(), "b.tar")
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		b, err := downloader.OpenBundle(p)
		if err != nil {
			var de *downloader.Error
			if !errors.As(err, &de) || de.Kind != downloader.KindBundle {
				t.Fatalf("失败必须是 KindBundle：%T %v", err, err)
			}
			return
		}
		defer func() { _ = b.Close() }()
		check := func(e security.TarEntry) {
			if e.Dir || e.Offset < 0 || e.Size < 0 || e.Offset+e.Size > int64(len(raw)) {
				t.Fatalf("条目 %v 越出文件（%d 字节）", e, len(raw))
			}
		}
		check(b.Layout.Manifest)
		check(b.Layout.Sig)
		for n, e := range b.Layout.Packages {
			check(e)
			if strings.ContainsAny(n, "/\\") || n == "" || n[0] == '.' {
				t.Fatalf("包文件名 %q 不合法", n)
			}
		}
		mf, sig, err := b.Manifest()
		if err != nil || int64(len(mf)) != b.Layout.Manifest.Size || int64(len(sig)) != b.Layout.Sig.Size {
			t.Fatalf("Manifest() 与索引不一致：%v", err)
		}
	})
}

// FuzzParseBundleLayout：任意索引（由字节切分出的条目名与标志）解析布局；失败必为 KindBundle；成功时 Packages 键合法。
func FuzzParseBundleLayout(f *testing.F) {
	f.Add("manifest.json\nmanifest.json.sig\npackages/a.tar.gz", uint8(0))
	f.Add("offline-bundle/manifest.json\noffline-bundle/manifest.json.sig\noffline-bundle/packages/x", uint8(1))
	f.Add("a/b\nmanifest.json\n../manifest.json.sig", uint8(0xff))
	f.Fuzz(func(t *testing.T, names string, dirMask uint8) {
		var es []security.TarEntry
		for i, n := range strings.Split(names, "\n") {
			if i >= 16 {
				break
			}
			es = append(es, security.TarEntry{Name: n, Dir: dirMask&(1<<uint(i%8)) != 0, Size: int64(len(n)) + 1})
		}
		l, err := downloader.ParseBundleLayout(es)
		if err != nil {
			var de *downloader.Error
			if !errors.As(err, &de) || de.Kind != downloader.KindBundle {
				t.Fatalf("失败必须是 KindBundle：%T %v", err, err)
			}
			return
		}
		if l.Manifest.Name == "" || l.Sig.Name == "" || l.Manifest.Dir || l.Sig.Dir {
			t.Fatalf("成功的布局必须有 manifest 与 sig：%+v", l)
		}
		for n := range l.Packages {
			if _, e := downloader.ArtifactFileName("https://h/dl/" + n); e != nil {
				t.Fatalf("Packages 键 %q 不是合法的包文件名", n)
			}
		}
	})
}

func FuzzArtifactFileName(f *testing.F) {
	for _, s := range []string{"https://h/dl/a-1.0.tar.gz", "https://h/dl/", "https://h/%2e%2e/x", "https://h/a/../b", "https://h/.x", "", "https://h/a b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		n, err := downloader.ArtifactFileName(raw)
		if err != nil {
			return
		}
		if n == "" || strings.ContainsAny(n, "/\\%") || strings.Contains(n, "..") || n[0] == '.' || len(n) > 200 {
			t.Fatalf("%q -> %q 不合法", raw, n)
		}
	})
}

// FuzzAcquireOffline：整条离线校验链（清单、签名、包都来自被变异的 bundle）。不变量：不 panic；
// 失败必为 *manifest.Error 或 *downloader.Error；失败后 staging 为空、信任库未被改动；成功时 sequence 已推进且解压结果只含 META/payload。
func FuzzAcquireOffline(f *testing.F) {
	for _, s := range seedBundles(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		w := fuzzWorld(t)
		p := w.params()
		p.BundlePath = filepath.Join(t.TempDir(), "b.tar")
		if err := os.WriteFile(p.BundlePath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		before := tree(t, w.trustDir)
		a, err := downloader.Acquire(t.Context(), p)
		if err != nil {
			var me *manifest.Error
			var de *downloader.Error
			if !errors.As(err, &me) && !errors.As(err, &de) {
				t.Fatalf("未分类的错误类型 %T：%v", err, err)
			}
			if len(tree(t, w.staging)) != 1 { // 只有 staging 根本身
				t.Fatalf("失败后 staging 非空：%v", tree(t, w.staging))
			}
			equalTree(t, "trust", before, tree(t, w.trustDir))
			return
		}
		if a.Result.Manifest.Sequence < 1 || readState(w) == "" {
			t.Fatal("成功后 sequence 必须已推进")
		}
		for k := range tree(t, a.PkgDir) {
			if k != "." && k != "META" && k != "payload" && !strings.HasPrefix(k, "META/") && !strings.HasPrefix(k, "payload/") {
				t.Fatalf("解压结果含意外路径 %s", k)
			}
		}
	})
}
