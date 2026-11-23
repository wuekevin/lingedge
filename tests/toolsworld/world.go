// Package toolsworld 是 installer/cli 集成测试共用的「世界」：一棵真实的临时目录树 + 伪二进制（systemctl/id/getent/useradd/userdel，
// 经真实 safeexec.Runner 执行，状态落在文件里，因此跨进程可见）+ 真实签名的离线 bundle + 假组件规格。
//
// 仅测试使用（G-11：测试替身只允许位于 _test.go、tests/、internal/**/testdouble/）：生产代码不得 import 本包，
// tests/security 的构建符号检查证明 lingedge 二进制不含这里的任何符号。被测的状态机、journal、撤销、冲突判定、registry、downloader、resolver 都是生产代码，
// 假的只有「操作系统」和「组件」。
package toolsworld

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/baota"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/probe"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// 仅测试：这里的「世界」是一棵真实的临时目录树 + 伪二进制（systemctl/id/getent/useradd/userdel，经真实 safeexec.Runner 执行，
// 状态落在文件里，因此跨进程可见）+ 真实签名的离线 bundle。被测的状态机、journal、撤销、冲突判定、registry、downloader、resolver 都是生产代码；
// 假的只有「操作系统」和「组件」。假组件（fakeSpec）只存在于 _test.go（G-11）。

var T0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

type World struct {
	Root   string
	Layout registry.Layout
	Bin    string
	Fake   string // 伪 systemd/用户状态目录
	Bundle string
	Ctl    string // 控制文件目录（注入故障/阻塞）
	Specs  installer.SpecSet
	Own    *recOwn
	Probe  probe.Result
	// PackageBase 是清单里包 URL 的前缀。
	PackageBase string
	// Now 是 Verifier 使用的时钟（缺省 T0）。
	Now time.Time
	// Variant 追加到 openresty 包内容里：改变它再 RewriteBundle，得到另一份同样合法签名但内容不同的 bundle（测试「预检与取包之间清单变了」）。
	Variant string

	opts Options
}

func tkeyFor(label string) (string, ed25519.PrivateKey) {
	s := sha256.Sum256([]byte("LingEdge unit test key v1 - TEST ONLY: " + label))
	priv := ed25519.NewKeyFromSeed(s[:])
	return "rel-it-a", priv
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// NewWorld 在 root 下建立（或复用）世界；幂等，父进程与 kill -9 的子进程各自调用得到同一棵树。
func NewWorld(root string) (*World, error) { return NewWorldBase(root, "https://releases.example.org") }

// NewWorldBase 同 NewWorld，但清单里包的 URL 前缀为 base（在线安装测试把它指向本机 HTTPS 测试服务器）。
func NewWorldBase(root, base string) (*World, error) {
	return NewWorldWith(root, Options{PackageBase: base})
}

// Options 是世界的可选形态（零值 = NewWorld 的缺省）。
type Options struct {
	// PackageBase 是清单里包 URL 的前缀；空 = https://releases.example.org。
	PackageBase string
	// ServerBinary 非空：server 包携带该路径的真实二进制（payload/bin/lingedge-server，0755）；它与 ProductionServerSpec 任一生效，server 即使用发行流水线里 server 的真实端口表
	// （8080 rest / 9443 grpc-mtls / 9444 grpc-enroll 条件端口 / 9180 metrics，与 scripts/release/components.go 一致）。包的 platform 标签仍是 linux-amd64（见 world 的 Probe），
	// 二进制本身是本机构建的——用于「真实 lingedge-server 启动并通过 /healthz」的集成测试，不是真实发行包。
	ServerBinary string
	// ProductionServerSpec 为 true：Specs["server"] 使用 installer.ProductionSpecs()["server"]（生产规格），而不是假规格。
	ProductionServerSpec bool
	// Static 为 true：bundle 额外含静态前端（admin-web/console/docs/status/website 的 static 变体，平台 any，布局与发行流水线一致：payload/public/…，含预压缩的 .gz），
	// Specs 使用 installer.ProductionSpecs() 里**生产的**静态前端规格（不是假组件）；Deps 带伪 nginx 的 WebRunner（Story 1.40）。
	Static bool
	// StaticBrokenLayout 为 true（需要 Static）：docs 包的布局违反约定（没有 public/index.html，顶层多出文件）——用于「包布局在落地前被拒绝」的测试。
	StaticBrokenLayout bool
}

// NewWorldWith 按 Options 建立（或复用）世界。
func NewWorldWith(root string, o Options) (*World, error) {
	base := o.PackageBase
	if base == "" {
		base = "https://releases.example.org"
	}
	w := &World{opts: o, PackageBase: base, Root: root, Bin: filepath.Join(root, "bin"), Fake: filepath.Join(root, "fake"), Ctl: filepath.Join(root, "ctl"),
		Bundle: filepath.Join(root, "bundle.tar"), Layout: registry.Layout{Root: filepath.Join(root, "var/lib/lingedge/tools")}, Own: &recOwn{}}
	for _, d := range []string{w.Bin, w.Fake + "/users", w.Fake + "/groups", w.Ctl, filepath.Join(root, "run"), filepath.Join(root, "etc/systemd/system"),
		// 系统骨架：真实系统上这些目录总是存在（Tools 不创建、回滚也不会删除它们）。
		filepath.Join(root, "opt"), filepath.Join(root, "var/lib/lingedge"), filepath.Join(root, "var/log")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := w.writeFakeBins(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(w.Bundle); err != nil {
		if err := w.writeBundle(); err != nil {
			return nil, err
		}
	}
	w.Specs = installer.SpecSet{}
	for _, n := range []string{"server", "node", "openresty"} {
		w.Specs[n] = &fakeSpec{name: n, w: w}
	}
	if o.ProductionServerSpec {
		w.Specs["server"] = installer.ProductionSpecs()["server"]
	}
	if o.Static {
		for _, n := range installer.StaticComponents() {
			w.Specs[n] = installer.ProductionSpecs()[n]
		}
	}
	w.Probe = probe.Result{
		System: resolver.System{OS: "linux", Arch: "amd64", MemoryTotalMB: 16000, MemoryAvailableMB: 12000,
			Disks: []resolver.Disk{{Mount: "/", FreeMB: 500000}}, Listeners: []resolver.Listener{}, Deps: map[string]resolver.Dep{}},
		Baota: baota.Report{}, ListenersKnown: true, ListenersSource: "ss", Sockets: []probe.Socket{}, Deps: []probe.DepStatus{}, NodeInstalls: []probe.NodeInstall{}, Warnings: []probe.Warning{},
	}
	return w, nil
}

func (w *World) writeFakeBins() error {
	pre := "#!/bin/sh\nST='" + w.Fake + "'\n"
	scripts := map[string]string{
		"systemctl": pre + `
case "$1" in
  --version) if [ -e "$ST/version" ]; then cat "$ST/version"; else echo "systemd 252 (252.22-1~deb12u1)"; fi;;
  is-enabled) if [ -e "$ST/enabled-$2" ]; then echo enabled; exit 0; else echo disabled; exit 1; fi;;
  is-active) if [ -e "$ST/active-$2" ]; then echo active; exit 0; else echo inactive; exit 3; fi;;
  enable) [ -e "` + w.Ctl + `/fail-enable" ] && { echo boom >&2; exit 1; }; shift; for u in "$@"; do : > "$ST/enabled-$u"; done;;
  disable) shift; for u in "$@"; do rm -f "$ST/enabled-$u"; done;;
  start) [ -e "` + w.Ctl + `/fail-start" ] && { echo "Job failed" >&2; exit 1; }; shift; for u in "$@"; do : > "$ST/active-$u"; done;;
  stop) shift; for u in "$@"; do rm -f "$ST/active-$u"; done;;
  restart) ;;
  daemon-reload) echo reload >> "$ST/reloads";;
  *) echo "unknown verb $1" >&2; exit 2;;
esac
`,
		// 伪 nginx：-t 与 -s reload 的调用记录在状态目录；开关 fail-nginx-t / fail-nginx-reload 注入失败。
		"nginx": pre + `
echo "$@" >> "$ST/nginx-calls"
case "$1" in
  -t) [ -e "` + w.Ctl + `/fail-nginx-t" ] && { echo "nginx: [emerg] unknown directive in /etc/nginx/conf.d/lingedge.conf:3" >&2; echo "nginx: configuration file /etc/nginx/nginx.conf test failed" >&2; exit 1; }; echo "nginx: configuration file /etc/nginx/nginx.conf test is successful" >&2;;
  -s) [ "$2" = reload ] || exit 2; [ -e "` + w.Ctl + `/fail-nginx-reload" ] && { echo "nginx: [error] invalid PID number in /run/nginx.pid" >&2; exit 1; }; echo reload >> "$ST/nginx-reloads";;
  *) exit 2;;
esac
`,
		"id": pre + `
[ "$1" = "-u" ] || exit 9
if [ -e "$ST/users/$2" ]; then cat "$ST/users/$2"; exit 0; fi
echo "id: '$2': no such user" >&2; exit 1
`,
		"getent": pre + `
[ "$1" = "group" ] || exit 9
if [ -e "$ST/groups/$2" ]; then echo "$2:x:998:"; exit 0; fi
exit 2
`,
		"useradd": pre + `
for last; do :; done
[ -e "` + w.Ctl + `/fail-useradd" ] && { echo "useradd: cannot lock /etc/passwd" >&2; exit 1; }
echo 998 > "$ST/users/$last"
`,
		"userdel": pre + `
for last; do :; done
[ -e "` + w.Ctl + `/fail-userdel" ] && { echo "userdel: boom" >&2; exit 1; }
rm -f "$ST/users/$last"
`,
	}
	for n, body := range scripts {
		p := filepath.Join(w.Bin, n)
		if err := os.WriteFile(p+".tmp", []byte(body), 0o755); err != nil {
			return err
		}
		if err := os.Rename(p+".tmp", p); err != nil {
			return err
		}
	}
	return nil
}

// ---- 包与 bundle ----

type pkgFileSpec struct {
	name string
	body string
	exec bool
}

func (w *World) buildPkg(comp, ver, plat, variant string, files []pkgFileSpec) ([]byte, error) {
	var names []string
	by := map[string]pkgFileSpec{}
	var total int64
	for _, f := range files {
		names = append(names, f.name)
		by[f.name] = f
		total += int64(len(f.body))
	}
	sort.Strings(names)
	var sums strings.Builder
	for _, n := range names {
		fmt.Fprintf(&sums, "%s  payload/%s\n", sha([]byte(by[n].body)), n)
	}
	in, err := manifest.PackageSigInput(comp, ver, plat, []byte(sums.String()))
	if err != nil {
		return nil, err
	}
	id, priv := tkeyFor("a")
	sigb, _ := json.Marshal(map[string]string{"key_id": id, "alg": "ed25519", "sig": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, in))})
	meta, _ := json.Marshal(map[string]any{"component": comp, "version": ver, "variant": variant, "platform": plat,
		"built_at": "2026-11-15T00:00:00Z", "config_schema_version": 1, "files_count": len(files), "unpacked_size": total})
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	add := func(name string, body []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	for _, e := range []struct {
		n string
		b []byte
	}{{"META/package.json", meta}, {"META/SHA256SUMS", []byte(sums.String())}, {"META/SHA256SUMS.sig", sigb}} {
		if err := add(e.n, e.b); err != nil {
			return nil, err
		}
	}
	for _, n := range names {
		if err := add("payload/"+n, []byte(by[n].body)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()
	return gz.Bytes(), nil
}

func (w *World) serverComp() (c struct {
	name     string
	internal bool
	requires map[string]any
	ports    []map[string]any
	files    []pkgFileSpec
}) {
	c.name = "server"
	if w.opts.ServerBinary == "" && !w.opts.ProductionServerSpec {
		c.ports = []map[string]any{{"proto": "tcp", "port": 8080, "bind": "127.0.0.1", "purpose": "REST"}}
		c.files = []pkgFileSpec{{"bin/lingedge-server", "#!server-binary-v1\n", true}, {"conf/default.yaml", "x: 1\n", false}}
		return c
	}
	bin := []byte("#!server-binary-v1\n") // 生产规格但不给真实二进制：占位字节（测试只验证安装编排，不执行它）
	if w.opts.ServerBinary != "" {
		var err error
		if bin, err = os.ReadFile(w.opts.ServerBinary); err != nil {
			panic(err)
		}
	}
	c.ports = []map[string]any{
		{"proto": "tcp", "port": 8080, "bind": "127.0.0.1", "purpose": "rest"},
		{"proto": "tcp", "port": 9443, "bind": "0.0.0.0", "purpose": "grpc-mtls"},
		{"proto": "tcp", "port": 9444, "bind": "0.0.0.0", "purpose": "grpc-enroll", "conditional": true},
		{"proto": "tcp", "port": 9180, "bind": "127.0.0.1", "purpose": "metrics"},
	}
	c.requires = map[string]any{"external": map[string]string{"mysql": ">=8.0.16 <9", "redis": ">=8"}} // 与发行流水线一致（scripts/release/components.go）
	c.files = []pkgFileSpec{{"bin/lingedge-server", string(bin), true}}
	return c
}

func (w *World) writeBundle() error {
	type comp = struct {
		name     string
		internal bool
		requires map[string]any
		ports    []map[string]any
		files    []pkgFileSpec
	}
	statics := map[string]string{} // 静态前端：组件名 → 变体
	comps := []comp{
		w.serverComp(),
		{name: "openresty", internal: true, files: []pkgFileSpec{{"bin/openresty", "#!openresty-v1" + w.Variant + "\n", true}}},
		{name: "node", requires: map[string]any{"components": map[string]string{"openresty": ">=0.1.0"}},
			ports: []map[string]any{{"proto": "tcp", "port": 9100, "bind": "127.0.0.1", "purpose": "agent"}},
			files: []pkgFileSpec{{"bin/lingedge-node", "#!node-binary-v1\n", true}}},
	}
	if w.opts.Static {
		for _, n := range []string{"admin-web", "console", "docs", "status"} {
			statics[n] = "default"
			files := staticPkgFiles(n)
			if n == "docs" && w.opts.StaticBrokenLayout {
				files = []pkgFileSpec{{"public/readme.txt", "no index here\n", false}, {"evil.sh", "#!/bin/sh\n", true}}
			}
			comps = append(comps, comp{name: n, files: files})
		}
		statics["website"] = "static"
		comps = append(comps, comp{name: "website", files: staticPkgFiles("website")})
	}
	var tarEnts []struct {
		n string
		b []byte
	}
	cm := map[string]any{}
	for _, c := range comps {
		plat, variant := "linux-amd64", "default"
		fn := fmt.Sprintf("lingedge-%s-0.1.0-linux-amd64.tar.gz", c.name)
		if v, ok := statics[c.name]; ok {
			plat, variant = "any", v
			if v == "default" {
				fn = fmt.Sprintf("lingedge-%s-0.1.0.tar.gz", c.name)
			} else {
				fn = fmt.Sprintf("lingedge-%s-%s-0.1.0.tar.gz", c.name, v)
			}
		}
		pkg, err := w.buildPkg(c.name, "0.1.0", plat, variant, c.files)
		if err != nil {
			return err
		}
		tarEnts = append(tarEnts, struct {
			n string
			b []byte
		}{"packages/" + fn, pkg})
		ve := map[string]any{"version": "0.1.0", "variants": map[string]any{variant: map[string]any{"artifacts": map[string]any{
			plat: map[string]any{"url": w.PackageBase + "/dl/" + fn, "sha256": sha(pkg), "size": len(pkg)}}}}}
		if c.requires != nil {
			ve["requires"] = c.requires
		}
		if c.ports != nil {
			ve["ports"] = c.ports
		}
		co := map[string]any{"status": "available", "versions": []any{ve}}
		if c.internal {
			co["internal"] = true
		}
		cm[c.name] = co
	}
	m := map[string]any{"schema_version": 1, "channel": "stable", "sequence": 10, "generated_at": "2026-11-01T00:00:00Z", "valid_until": "2027-02-01T00:00:00Z",
		"revoked_versions": []any{}, "components": cm}
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
	}{{"manifest.json", mb}, {"manifest.json.sig", sigb}}, tarEnts...)
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
	tmp := w.Bundle + ".tmp"
	if err := os.WriteFile(tmp, raw.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.Bundle)
}

// ---- 依赖装配 ----

func (w *World) Registry(runID string) *registry.Registry {
	return registry.New(w.Layout, registry.Options{Now: func() time.Time { return T0 }, Hostname: func() (string, error) { return "edge-host-1", nil }, RunID: runID, Command: "lingedge install"})
}

// KeysJSON 返回世界的发行公钥集（keys.json 格式）：cli 测试经 Env.BuiltinKeys 注入，等价于「发行构建内置公钥」。
// 该公钥是登记在 scripts/gates/data/test-release-pubkeys.txt 的测试公钥（label "a"），任何构建的内置 keys.json 都不得含它（release-keys 门禁）。
func KeysJSON() []byte {
	id, priv := tkeyFor("a")
	pub := priv.Public().(ed25519.PublicKey)
	doc, _ := json.Marshal(manifest.KeysFile{SchemaVersion: 1, Keys: []manifest.Key{{KeyID: id, Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(pub), Status: manifest.StatusActive,
		NotBefore: "2026-01-01T00:00:00Z", NotAfter: "2028-01-01T00:00:00Z"}}, RevokedKeyIDs: []string{}})
	return doc
}

func (w *World) Verifier() (*manifest.Verifier, error) {
	doc := KeysJSON()
	kf, err := manifest.ParseKeysFile(doc)
	if err != nil {
		return nil, err
	}
	ts, err := manifest.NewTrustSet(kf)
	if err != nil {
		return nil, err
	}
	now := w.Now
	if now.IsZero() {
		now = T0
	}
	return &manifest.Verifier{Trust: ts, Seen: manifest.FileSequenceStore{Path: w.Layout.SequenceFile()}, Now: func() time.Time { return now }}, nil
}

func (w *World) Deps(runID string) installer.Deps {
	r, err := service.NewRunner([]string{w.Bin})
	if err != nil {
		panic(err)
	}
	wr, err := service.NewWebServerRunner([]string{w.Bin})
	if err != nil {
		panic(err)
	}
	return installer.Deps{
		Root: w.Root, Reg: w.Registry(runID), Verifier: w.Verifier, Specs: w.Specs,
		Collect: func(context.Context) (probe.Result, error) { return w.Probe, nil },
		Runner:  r, WebRunner: wr, Own: w.Own, Booted: func() bool { return true }, Geteuid: func() int { return 0 },
		Now: func() time.Time { return T0 }, FS: baota.OSFS{Root: w.Root},
	}
}

func (w *World) NewInstaller(runID string, mut ...func(*installer.Deps)) *installer.Installer {
	d := w.Deps(runID)
	for _, m := range mut {
		m(&d)
	}
	in, err := installer.New(d)
	if err != nil {
		panic(err)
	}
	return in
}

// ---- 伪世界的读取 ----

// Has 报告 root 下 rel 是否存在（不跟随符号链接）。
func (w *World) Has(rel string) bool {
	_, err := os.Lstat(filepath.Join(w.Root, rel))
	return err == nil
}

// FakeHas 报告伪 systemd/用户状态目录下 rel 是否存在。
func (w *World) FakeHas(rel string) bool {
	_, err := os.Lstat(filepath.Join(w.Fake, rel))
	return err == nil
}

// SetCtl 打开/关闭一个控制开关（注入故障、阻塞启动）。
func (w *World) SetCtl(name string, on bool) {
	p := filepath.Join(w.Ctl, name)
	if on {
		_ = os.WriteFile(p, nil, 0o644)
	} else {
		_ = os.Remove(p)
	}
}

// CtlOn 报告控制开关是否打开。
func (w *World) CtlOn(name string) bool {
	_, err := os.Stat(filepath.Join(w.Ctl, name))
	return err == nil
}

// ---- 假 Ownership ----

type recOwn struct {
	mu    sync.Mutex
	Calls []string
}

func (r *recOwn) Resolve(user, group string) (int, int, error) {
	// 非 root 的测试进程无法真实 chown：所有名字都映射为当前用户，使「既有目录属主正确」；调用本身被记录并断言。
	return os.Getuid(), os.Getgid(), nil
}

func (r *recOwn) Chown(path string, uid, gid int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls = append(r.Calls, fmt.Sprintf("%s %d:%d", path, uid, gid))
	return nil
}

// ---- 假组件（只存在于 _test.go）----

type fakeSpec struct {
	name string
	w    *World
}

func (s *fakeSpec) Name() string { return s.name }

func (s *fakeSpec) Executables(version, variant string) []string {
	switch s.name {
	case "server":
		return []string{"payload/bin/lingedge-server"}
	case "node":
		return []string{"payload/bin/lingedge-node"}
	}
	return []string{"payload/bin/openresty"}
}

func (s *fakeSpec) ValidateSettings(m map[string]string) error {
	for k := range m {
		if k != "greeting" {
			return fmt.Errorf("未知配置项 %q", k)
		}
	}
	return nil
}

func (s *fakeSpec) InitConfig(_ context.Context, in installer.ConfigInput) ([]installer.ConfigFile, error) {
	if s.name == "node" {
		return []installer.ConfigFile{{Path: "/etc/lingedge/node/node.yaml", Content: []byte("listen: 127.0.0.1:9100\ngreeting: " + in.Settings["greeting"] + "\n")}}, nil
	}
	return nil, nil
}

func (s *fakeSpec) Start(ctx context.Context, rt *installer.Runtime) error {
	if s.w.CtlOn("block-start-" + s.name) {
		_ = os.WriteFile(filepath.Join(s.w.Root, "run", s.name+".start-blocked"), nil, 0o644)
		for s.w.CtlOn("block-start-" + s.name) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	return rt.StartUnits(ctx)
}

func (s *fakeSpec) Health(context.Context, *installer.Runtime) error {
	if s.w.CtlOn("fail-health-" + s.name) {
		return errors.New("healthz 返回 503")
	}
	// 健康 = 组件的单元已被（伪）systemd 启动；没有单元的组件（openresty 由 node 的 path 单元拉起）以其 current 链接存在为准。
	for _, u := range service.EnableTargets(s.name) {
		if !s.w.FakeHas("active-" + string(u)) {
			return errors.New("单元 " + string(u) + " 尚未启动")
		}
	}
	if len(service.EnableTargets(s.name)) == 0 && !s.w.Has("opt/lingedge/"+s.name+"/current") {
		return errors.New("尚未安装")
	}
	return nil
}

// RewriteBundle 以当前 Variant 原子地重写 w.Bundle（与预检时的那份同样合法签名、版本相同，但 openresty 包的内容/SHA256 不同）。
func (w *World) RewriteBundle(variant string) error {
	w.Variant = variant
	return w.writeBundle()
}

// SetSystemdVersion 让伪 systemctl --version 输出给定的第一行（如 "systemd 245 (245.4-4ubuntu3)"）。
func (w *World) SetSystemdVersion(line string) error {
	return os.WriteFile(filepath.Join(w.Fake, "version"), []byte(line+"\n"), 0o644)
}

// staticPkgFiles 是静态前端包的内容：布局与发行流水线一致（payload/public/…，哈希命名的资源与预压缩 .gz）。
func staticPkgFiles(comp string) []pkgFileSpec {
	idx := "<!doctype html><title>" + comp + "</title><script type=\"module\" src=\"/assets/app-1a2b3c.js\"></script>\n"
	js := "console.log('" + comp + "');\n"
	return []pkgFileSpec{
		{"public/index.html", idx, false}, {"public/index.html.gz", gzString(idx), false},
		{"public/assets/app-1a2b3c.js", js, false}, {"public/assets/app-1a2b3c.js.gz", gzString(js), false},
	}
}

// SetNativeNginx 让探测结果呈现「原生 Nginx（非宝塔）+ conf.d 约定」：Probe 里有系统 Nginx，世界里有 /etc/nginx/nginx.conf（include conf.d/*.conf）与 /etc/nginx/conf.d。
func (w *World) SetNativeNginx() error {
	if err := os.MkdirAll(filepath.Join(w.Root, "etc/nginx/conf.d"), 0o755); err != nil {
		return err
	}
	conf := "user www-data;\nevents {}\nhttp {\n    include /etc/nginx/mime.types;\n    include /etc/nginx/conf.d/*.conf;\n}\n"
	if err := os.WriteFile(filepath.Join(w.Root, "etc/nginx/nginx.conf"), []byte(conf), 0o644); err != nil {
		return err
	}
	w.Probe.Baota.WebServers = []baota.WebServer{{Variant: "system", Kind: "nginx", Binary: "/usr/sbin/nginx", Conf: "/etc/nginx/nginx.conf"}}
	return nil
}

// NginxCalls 返回伪 nginx 被调用的参数行（每行一次调用）。
func (w *World) NginxCalls() []string {
	b, err := os.ReadFile(filepath.Join(w.Fake, "nginx-calls"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func gzString(v string) string {
	var b bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
	_, _ = zw.Write([]byte(v))
	_ = zw.Close()
	return b.String()
}
