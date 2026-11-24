package toolsworld

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// PkgFile 是包内 payload 的一个文件。
type PkgFile struct {
	Name string
	Body string
	Exec bool
}

// BundleComp 是 BuildBundle 里的一个组件版本（真实签名的包；与 writeBundle 同一套包/清单/签名实现）。
type BundleComp struct {
	Name, Version  string
	Variant        string // 缺省 default
	Platform       string // 缺省 linux-amd64
	Files          []PkgFile
	Requires       map[string]any
	Ports          []map[string]any
	MinUpgradeFrom string
	// Migrations 是清单里的迁移预告（manifest.MigrationsNotice 的 JSON 形态）。
	Migrations map[string]any
	Internal   bool
	// Status 缺省 available；Unreleased 等不带版本。
	Status string
}

// RevokedVersion 是吊销项。
type RevokedVersion struct{ Component, Version, Reason string }

// BuildBundle 用测试发行密钥签出一个离线 bundle（清单 sequence = seq；每个组件一个版本一个变体一个平台），原子写到 path。
// 清单、包内 SHA256SUMS 与签名都走生产的格式/域分隔（manifest.ManifestDomain、PackageSigInput），不是占位。
func (w *World) BuildBundle(path string, seq int64, comps []BundleComp, revoked []RevokedVersion) error {
	var ents []struct {
		n string
		b []byte
	}
	cm := map[string]any{}
	for _, c := range comps {
		plat, variant := c.Platform, c.Variant
		if plat == "" {
			plat = "linux-amd64"
		}
		if variant == "" {
			variant = "default"
		}
		status := c.Status
		if status == "" {
			status = "available"
		}
		co := map[string]any{"status": status}
		if c.Internal {
			co["internal"] = true
		}
		if status == "available" {
			var files []pkgFileSpec
			for _, f := range c.Files {
				files = append(files, pkgFileSpec{name: f.Name, body: f.Body, exec: f.Exec})
			}
			fn := fmt.Sprintf("lingedge-%s-%s-%s.tar.gz", c.Name, c.Version, plat)
			if variant != "default" {
				fn = fmt.Sprintf("lingedge-%s-%s-%s-%s.tar.gz", c.Name, variant, c.Version, plat)
			}
			pkg, err := w.buildPkg(c.Name, c.Version, plat, variant, files)
			if err != nil {
				return err
			}
			ents = append(ents, struct {
				n string
				b []byte
			}{"packages/" + fn, pkg})
			ve := map[string]any{"version": c.Version, "variants": map[string]any{variant: map[string]any{"artifacts": map[string]any{
				plat: map[string]any{"url": w.PackageBase + "/dl/" + fn, "sha256": sha(pkg), "size": len(pkg)}}}}}
			if c.Requires != nil {
				ve["requires"] = c.Requires
			}
			if c.Ports != nil {
				ve["ports"] = c.Ports
			}
			if c.MinUpgradeFrom != "" {
				ve["min_upgrade_from"] = c.MinUpgradeFrom
			}
			if c.Migrations != nil {
				ve["migrations"] = c.Migrations
			}
			if prev, ok := cm[c.Name].(map[string]any); ok { // 同一组件的多个版本合并进 versions
				prev["versions"] = append(prev["versions"].([]any), ve)
				continue
			}
			co["versions"] = []any{ve}
		}
		cm[c.Name] = co
	}
	rv := []any{}
	for _, r := range revoked {
		rv = append(rv, map[string]any{"component": r.Component, "version": r.Version, "reason": r.Reason})
	}
	m := map[string]any{"schema_version": 1, "channel": "stable", "sequence": seq, "generated_at": "2026-11-01T00:00:00Z", "valid_until": "2027-02-01T00:00:00Z",
		"revoked_versions": rv, "components": cm}
	mb, err := json.Marshal(m)
	if err != nil {
		return err
	}
	mb = append(mb, '\n')
	id, priv := tkeyFor("a")
	sigb, _ := json.Marshal(map[string]string{"key_id": id, "alg": "ed25519", "sig": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(manifest.ManifestDomain), mb...)))})
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	all := append([]struct {
		n string
		b []byte
	}{{"manifest.json", mb}, {"manifest.json.sig", sigb}}, ents...)
	for _, e := range all {
		if err := tw.WriteHeader(&tar.Header{Name: e.n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e.b)), Format: tar.FormatPAX}); err != nil {
			return err
		}
		if _, err := tw.Write(e.b); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// StaticFiles 返回静态前端包的内容（与发行流水线布局一致；version 写进页面，使不同版本的内容不同）。
func StaticFiles(comp, version string) []PkgFile {
	idx := "<!doctype html><title>" + comp + " " + version + "</title><script type=\"module\" src=\"/assets/app-1a2b3c.js\"></script>\n"
	js := "console.log('" + comp + " " + version + "');\n"
	return []PkgFile{
		{"public/index.html", idx, false}, {"public/index.html.gz", gzString(idx), false},
		{"public/assets/app-1a2b3c.js", js, false}, {"public/assets/app-1a2b3c.js.gz", gzString(js), false},
	}
}
