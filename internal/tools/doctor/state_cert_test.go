package doctor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/cfgcheck"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
)

// ---- 证书（真实 x509） ----

type certSpec struct {
	cn        string
	ca        bool
	eku       []x509.ExtKeyUsage
	nb, na    time.Time
	parent    *x509.Certificate
	parentKey *ecdsa.PrivateKey
}

func mkCert(t *testing.T, s certSpec) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: s.cn}, NotBefore: s.nb, NotAfter: s.na,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: s.eku, BasicConstraintsValid: true, IsCA: s.ca}
	if s.ca {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	parent, pkey := s.parent, s.parentKey
	if parent == nil {
		parent, pkey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, pkey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func day(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// 节点证书：90 天期限，剩余 < 1/6（15 天）才告警；剩余 30 天（落在 1/3 续期窗口内）不告警（F-2、L24）。
func TestNodeMTLSThresholdBoundaries(t *testing.T) {
	cases := []struct {
		name      string
		remaining time.Duration
		want      Level
	}{
		{"刚签发（剩余 90 天）", day(90), LevelPass},
		{"剩余 31 天（刚进入续期窗口）", day(31), LevelPass},
		{"剩余 30 天（正好 1/3）：续期窗口内不告警", day(30), LevelPass},
		{"剩余 16 天（> 15 天）不告警", day(16), LevelPass},
		{"剩余 15 天（正好 1/6）不告警", day(15), LevelPass},
		{"剩余 14 天（< 1/6）告警", day(14), LevelWarn},
		{"剩余 1 天告警", day(1), LevelWarn},
		{"已过期", -time.Hour, LevelFail},
	}
	for _, c := range cases {
		w := newWorld(t)
		w.Components = []Component{{Name: "node", Version: "1.0.0"}}
		na := t0.Add(c.remaining)
		_, _, pemb := mkCert(t, certSpec{cn: "node", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nb: na.Add(-day(90)), na: na})
		put(t, w, "var/lib/lingedge-node/identity/node.crt.pem", string(pemb), 0o600)
		f := find(t, runAll(t, w, Filter{Component: "node"}), "cert.node_mtls")
		want(t, f, c.want, "")
		if c.want == LevelFail && !strings.Contains(f.Hint, "force-reenroll") {
			t.Errorf("过期提示应指向 node enroll --force-reenroll：%q", f.Hint)
		}
	}
}

func TestNodeMTLSCustomLifetimeAndPicksLatestLeaf(t *testing.T) {
	w := newWorld(t)
	w.Components = []Component{{Name: "node", Version: "1.0.0"}}
	// 总期限 30 天（可配）：剩余 4 天 < 5 天（1/6）告警，6 天不告警——阈值按「总期限的 1/6」而不是固定 15 天。
	mk := func(rem time.Duration) Finding {
		w := newWorld(t)
		w.Components = []Component{{Name: "node", Version: "1.0.0"}}
		na := t0.Add(rem)
		_, _, p := mkCert(t, certSpec{cn: "n", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nb: na.Add(-day(30)), na: na})
		put(t, w, "var/lib/lingedge-node/identity/c.crt", string(p), 0o600)
		return find(t, runAll(t, w, Filter{Component: "node"}), "cert.node_mtls")
	}
	want(t, mk(day(4)), LevelWarn, "")
	want(t, mk(day(6)), LevelPass, "")
	// 同目录有新旧两张叶证书：以最晚到期的为准（续期两阶段：旧证书仍可能在目录里）。
	_, _, old := mkCert(t, certSpec{cn: "old", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nb: t0.Add(-day(80)), na: t0.Add(day(2))})
	_, _, fresh := mkCert(t, certSpec{cn: "new", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nb: t0.Add(-day(1)), na: t0.Add(day(89))})
	put(t, w, "var/lib/lingedge-node/identity/a-old.crt", string(old), 0o600)
	put(t, w, "var/lib/lingedge-node/identity/b-new.crt", string(fresh), 0o600)
	want(t, find(t, runAll(t, w, Filter{Component: "node"}), "cert.node_mtls"), LevelPass, "")
}

func TestNodeMTLSSkipsAndIgnoresPrivateKeys(t *testing.T) {
	w := newWorld(t)
	w.Components = []Component{{Name: "node", Version: "1.0.0"}}
	want(t, find(t, runAll(t, w, Filter{Component: "node"}), "cert.node_mtls"), LevelSkip, "尚未注册")
	put(t, w, "var/lib/lingedge-node/identity/node.key", "-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n", 0o600)
	f := find(t, runAll(t, newWorldWith4(w), Filter{Component: "node"}), "cert.node_mtls")
	want(t, f, LevelSkip, "没有客户端")
	if strings.Contains(f.Message+f.Hint, "AAAA") {
		t.Error("私钥内容不得出现在任何输出里")
	}
}

func TestPrivateCAThreshold(t *testing.T) {
	cases := []struct {
		rem  time.Duration
		want Level
	}{{day(3650), LevelPass}, {day(400), LevelPass}, {day(365), LevelPass}, {day(364), LevelWarn}, {day(30), LevelWarn}, {-day(1), LevelFail}}
	for _, c := range cases {
		w := newWorld(t)
		w.Components = []Component{{Name: "node", Version: "1.0.0"}}
		na := t0.Add(c.rem)
		_, _, ca := mkCert(t, certSpec{cn: "LingEdge CA", ca: true, nb: na.Add(-day(3650)), na: na})
		put(t, w, "var/lib/lingedge-node/identity/ca.pem", string(ca), 0o600)
		want(t, find(t, runAll(t, w, Filter{Component: "node"}), "cert.private_ca"), c.want, "")
	}
}

// Server gRPC 叶证书与握手链里的 CA：叶 < 1/6 告警、CA < 1 年告警；监听不可达/向导阶段 skip。
func TestServerLeafAndChainCA(t *testing.T) {
	ca, caKey, _ := mkCert(t, certSpec{cn: "CA", ca: true, nb: t0.Add(-day(100)), na: t0.Add(day(200))}) // CA 剩余 200 天 < 1 年
	mkLeaf := func(rem time.Duration) *x509.Certificate {
		na := t0.Add(rem)
		c, _, _ := mkCert(t, certSpec{cn: "server", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nb: na.Add(-day(90)), na: na, parent: ca, parentKey: caKey})
		return c
	}
	mk := func(chain []*x509.Certificate, err error, initialized bool) Report {
		w := newWorld(t)
		w.Components = []Component{{Name: "server", Version: "1.0.0", DataDir: "/var/lib/lingedge/server"}}
		w.ServerListens = func() (string, string) { return "127.0.0.1:8080", "0.0.0.0:9443" }
		_ = os.MkdirAll(filepath.Join(w.Root, "var/lib/lingedge/server"), 0o755)
		if initialized {
			put(t, w, "var/lib/lingedge/server/install.lock", "", 0o600)
		}
		var gotAddr string
		w.PeerCerts = func(_ context.Context, addr string) ([]*x509.Certificate, error) { gotAddr = addr; return chain, err }
		rep := runAll(t, w, Filter{Component: "server"})
		if initialized && gotAddr != "" && gotAddr != "127.0.0.1:9443" {
			t.Errorf("通配监听应换成回环地址：%s", gotAddr)
		}
		return rep
	}
	rep := mk([]*x509.Certificate{mkLeaf(day(60)), ca}, nil, true)
	want(t, find(t, rep, "cert.server_leaf"), LevelPass, "")
	want(t, find(t, rep, "cert.private_ca"), LevelWarn, "不足 1 年")
	rep = mk([]*x509.Certificate{mkLeaf(day(10))}, nil, true)
	want(t, find(t, rep, "cert.server_leaf"), LevelWarn, "1/6")
	want(t, find(t, rep, "cert.private_ca"), LevelSkip, "没有读到")
	rep = mk([]*x509.Certificate{mkLeaf(-time.Hour)}, nil, true)
	want(t, find(t, rep, "cert.server_leaf"), LevelFail, "过期")
	rep = mk(nil, errors.New("连接失败"), true)
	want(t, find(t, rep, "cert.server_leaf"), LevelSkip, "连接失败")
	rep = mk([]*x509.Certificate{mkLeaf(day(60))}, nil, false)
	want(t, find(t, rep, "cert.server_leaf"), LevelSkip, "setup mode")
}

// ---- cfg.validate ----

func TestCfgValidateAggregation(t *testing.T) {
	mk := func(vs map[string]ConfigVerdict, comps ...string) Finding {
		w := newWorld(t)
		for _, c := range comps {
			w.Components = append(w.Components, Component{Name: c, Version: "1.0.0"})
		}
		w.ConfigVerify = func(_ context.Context, target string) ConfigVerdict {
			v := vs[target]
			v.Target = target
			return v
		}
		return find(t, runAll(t, w, Filter{}), "cfg.validate")
	}
	bad := []cfgcheck.Problem{{Path: "grpc.listen", Line: 12, Column: 3, Reason: "必须是 host:port"}}
	want(t, mk(map[string]ConfigVerdict{"server": {Problems: bad}}, "server"), LevelFail, "第 12 行")
	want(t, mk(nil, "server"), LevelPass, "tools、server")
	f := mk(map[string]ConfigVerdict{"server": {Partial: "只做了结构校验"}}, "server")
	want(t, f, LevelWarn, "结构校验")
	want(t, mk(map[string]ConfigVerdict{"server": {Unavailable: "无权读取"}, "tools": {Unavailable: "无权读取"}}, "server"), LevelSkip, "无权读取")
	want(t, mk(map[string]ConfigVerdict{"server": {Unavailable: "无权读取"}}, "server"), LevelWarn, "无法校验")
	// 指定组件：只校验它自己（不含 tools.yaml）。
	w := newWorld(t)
	w.Components = []Component{{Name: "server", Version: "1.0.0"}, {Name: "node", Version: "1.0.0"}}
	var seen []string
	w.ConfigVerify = func(_ context.Context, target string) ConfigVerdict {
		seen = append(seen, target)
		return ConfigVerdict{}
	}
	runAll(t, w, Filter{Component: "node"})
	if len(seen) != 1 || seen[0] != "node" {
		t.Errorf("指定组件时只校验该组件：%v", seen)
	}
}

// ---- state.drift / journal.incomplete ----

func installedServer(t *testing.T, w *World) {
	t.Helper()
	w.Components = []Component{{Name: "server", Version: "1.0.0", State: "running", Unit: "lingedge-server.service", RunUser: "lingedge",
		ReleaseDir: "/opt/lingedge/server/releases/1.0.0", ConfigDir: "/etc/lingedge/server", DataDir: "/var/lib/lingedge/server"}}
	for _, d := range []string{"opt/lingedge/server/releases/1.0.0", "etc/lingedge/server", "var/lib/lingedge/server", "etc/systemd/system"} {
		if err := os.MkdirAll(filepath.Join(w.Root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("releases/1.0.0", filepath.Join(w.Root, "opt/lingedge/server/current")); err != nil {
		t.Fatal(err)
	}
	put(t, w, "etc/systemd/system/lingedge-server.service", "[Service]\n", 0o644)
}

func TestStateDrift(t *testing.T) {
	w := newWorld(t)
	installedServer(t, w)
	want(t, find(t, runAll(t, w, Filter{}), "state.drift"), LevelPass, "一致")
	// 各类偏差。
	cases := []struct {
		name string
		mut  func(w *World)
		sub  string
	}{
		{"release 目录被删", func(w *World) { _ = os.RemoveAll(filepath.Join(w.Root, "opt/lingedge/server/releases/1.0.0")) }, "release 目录"},
		{"current 指向别的版本", func(w *World) {
			p := filepath.Join(w.Root, "opt/lingedge/server/current")
			_ = os.Remove(p)
			_ = os.Symlink("releases/2.0.0", p)
		}, "state.json 记录版本"},
		{"current 缺失", func(w *World) { _ = os.Remove(filepath.Join(w.Root, "opt/lingedge/server/current")) }, "current 链接缺失"},
		{"单元文件被删", func(w *World) { _ = os.Remove(filepath.Join(w.Root, "etc/systemd/system/lingedge-server.service")) }, "单元文件"},
		{"配置目录被删", func(w *World) { _ = os.RemoveAll(filepath.Join(w.Root, "etc/lingedge/server")) }, "配置目录"},
		{"磁盘上有未记录的安装", func(w *World) {
			_ = os.MkdirAll(filepath.Join(w.Root, "opt/lingedge/docs/releases/1.0.0"), 0o755)
			_ = os.Symlink("releases/1.0.0", filepath.Join(w.Root, "opt/lingedge/docs/current"))
		}, "没有该组件的记录"},
		{"运行用户不存在", func(w *World) { w.Own = errOwn{} }, "运行用户"},
	}
	for _, c := range cases {
		w := newWorld(t)
		installedServer(t, w)
		c.mut(w)
		f := find(t, runAll(t, w, Filter{}), "state.drift")
		want(t, f, LevelFail, c.sub)
	}
	// state.json 读取失败。
	w = newWorld(t)
	w.StateErr = errors.New("损坏")
	want(t, find(t, runAll(t, w, Filter{}), "state.drift"), LevelFail, "损坏")
}

type errOwn struct{}

func (errOwn) Resolve(string, string) (int, int, error) { return 0, 0, errors.New("no such user") }
func (errOwn) Chown(string, int, int) error             { return nil }

func TestJournalIncomplete(t *testing.T) {
	mk := func(txs []registry.TxInfo, err error) Finding {
		w := newWorld(t)
		w.Journals = func(context.Context) ([]registry.TxInfo, error) { return txs, err }
		return find(t, runAll(t, w, Filter{}), "journal.incomplete")
	}
	want(t, mk(nil, nil), LevelPass, "没有未完成")
	want(t, mk([]registry.TxInfo{{ID: "tx1", Status: "committed"}, {ID: "tx2", Status: "aborted"}}, nil), LevelPass, "")
	f := mk([]registry.TxInfo{{ID: "01ABC", Status: "active", Command: "lingedge install server"}}, nil)
	want(t, f, LevelFail, "01ABC")
	if !strings.Contains(f.Hint, "recover") {
		t.Errorf("提示应指向 recover：%q", f.Hint)
	}
	want(t, mk([]registry.TxInfo{{ID: "x", Status: "corrupt"}}, nil), LevelFail, "")
	want(t, mk([]registry.TxInfo{{ID: "x", Status: "active", Live: true}}, nil), LevelPass, "正在进行")
	want(t, mk(nil, errors.New("permission denied")), LevelSkip, "permission denied")
}

// ---- backup.* ----

func backupEntry(comp string, created time.Time, withKey bool) backup.ListEntry {
	return backup.ListEntry{Name: "lingedge-backup-" + comp + ".tar.gz.enc", Status: "ok", Encrypted: true,
		Meta: &backup.Meta{Component: comp, CreatedAt: created.UTC().Format(time.RFC3339), MasterKeyIncluded: withKey, Purpose: "backup"}}
}

func TestBackupRecent(t *testing.T) {
	mk := func(entries []backup.ListEntry, err error, mut func(w *World)) Finding {
		w := newWorld(t)
		installedServer(t, w)
		put(t, w, "var/lib/lingedge/server/install.lock", "", 0o600)
		w.Backups = func() ([]backup.ListEntry, error) { return entries, err }
		if mut != nil {
			mut(w)
		}
		return find(t, runAll(t, w, Filter{}), "backup.recent")
	}
	want(t, mk([]backup.ListEntry{backupEntry("server", t0.Add(-day(2)), false)}, nil, nil), LevelPass, "")
	want(t, mk([]backup.ListEntry{backupEntry("server", t0.Add(-day(8)), false)}, nil, nil), LevelWarn, "已过期")
	want(t, mk(nil, nil, nil), LevelWarn, "从未备份")
	// 阈值可配置。
	want(t, mk([]backup.ListEntry{backupEntry("server", t0.Add(-day(8)), false)}, nil, func(w *World) { w.Cfg.BackupMaxAge = day(30) }), LevelPass, "")
	// 损坏/他人的备份不算数；恢复前安全备份也不算「最近备份」。
	bad := backupEntry("server", t0.Add(-time.Hour), false)
	bad.Status = "invalid"
	safety := backupEntry("server", t0.Add(-time.Hour), false)
	safety.Meta.Purpose = "restore_safety"
	want(t, mk([]backup.ListEntry{bad, safety, backupEntry("node", t0.Add(-time.Hour), false)}, nil, nil), LevelWarn, "从未备份")
	want(t, mk(nil, errors.New("permission denied"), nil), LevelSkip, "需要 root")
	// 向导未完成：没有数据，不要求备份。
	w := newWorld(t)
	installedServer(t, w)
	w.Backups = func() ([]backup.ListEntry, error) { return nil, nil }
	want(t, find(t, runAll(t, w, Filter{}), "backup.recent"), LevelSkip, "尚未完成")
}

func TestBackupMasterKeySeparate(t *testing.T) {
	const fp = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	mk := func(entries []backup.ListEntry, rec string, wizardDone bool) Finding {
		w := newWorld(t)
		installedServer(t, w)
		if wizardDone {
			put(t, w, "var/lib/lingedge/server/install.lock", "", 0o600)
		}
		w.MasterKeyFingerprint = func() (string, error) { return fp, nil }
		w.Backups = func() ([]backup.ListEntry, error) { return entries, nil }
		w.Cfg.CustodyFingerprint = rec
		return find(t, runAll(t, w, Filter{}), "backup.master_key_separate")
	}
	want(t, mk(nil, "", true), LevelWarn, "0123456789abcdef")
	want(t, mk(nil, "0123456789abcdef", true), LevelPass, "独立保管")
	want(t, mk(nil, fp, true), LevelPass, "")
	want(t, mk(nil, "fedcba9876543210", true), LevelWarn, "不一致")
	f := mk([]backup.ListEntry{backupEntry("server", t0, true)}, "0123456789abcdef", true)
	want(t, f, LevelWarn, "同一个文件")
	if f.Details["co_located_backups"] == nil {
		t.Errorf("应列出主密钥与数据同处的备份：%+v", f.Details)
	}
	want(t, mk(nil, "", false), LevelSkip, "向导")
	// 读不到主密钥：skip，不猜。
	w := newWorld(t)
	installedServer(t, w)
	put(t, w, "var/lib/lingedge/server/install.lock", "", 0o600)
	w.MasterKeyFingerprint = func() (string, error) { return "", errors.New("permission denied") }
	want(t, find(t, runAll(t, w, Filter{}), "backup.master_key_separate"), LevelSkip, "需要 root")
	// 指纹值不会出现在除前 16 位之外的任何输出里。
	f = mk(nil, "", true)
	if strings.Contains(f.Message+f.Hint, fp) {
		t.Error("不得输出完整指纹")
	}
}

// ---- ver.* / trust.* ----

func TestVerChecks(t *testing.T) {
	ve := func(v string) manifest.VersionEntry { return manifest.VersionEntry{Version: v} }
	mkManifest := func(rev []manifest.RevokedVersion, valid time.Time) *manifest.Manifest {
		return &manifest.Manifest{ValidUntil: valid, RevokedVersions: rev, Components: map[string]manifest.Component{
			"server": {Status: "available", Versions: []manifest.VersionEntry{ve("1.0.0"), ve("1.1.0"), ve("1.2.0-rc.1"), ve("1.3.0")}},
			"docs":   {Status: "available", Versions: []manifest.VersionEntry{ve("1.0.0")}},
		}}
	}
	mk := func(m *manifest.Manifest, err error, comps ...Component) Report {
		w := newWorld(t)
		w.Components = comps
		w.Manifest = func(context.Context) (*manifest.Manifest, error) { return m, err }
		return runAll(t, w, Filter{})
	}
	srv, docs := Component{Name: "server", Version: "1.0.0"}, Component{Name: "docs", Version: "1.0.0"}
	rep := mk(mkManifest(nil, t0.Add(day(30))), nil, srv, docs)
	want(t, find(t, rep, "ver.revoked"), LevelPass, "")
	f := find(t, rep, "ver.update_available")
	want(t, f, LevelPass, "server 1.0.0 → 1.3.0")
	if strings.Contains(f.Message, "rc") || strings.Contains(f.Message, "docs") {
		t.Errorf("预发布版本不算更新，docs 已是最新：%q", f.Message)
	}
	want(t, find(t, rep, "trust.manifest_valid"), LevelPass, "")
	// 命中吊销。
	rep = mk(mkManifest([]manifest.RevokedVersion{{Component: "server", Version: "1.0.0", Reason: "CVE-2026-0001"}}, t0.Add(day(30))), nil, srv)
	want(t, find(t, rep, "ver.revoked"), LevelFail, "CVE-2026-0001")
	rep = mk(mkManifest([]manifest.RevokedVersion{{Component: "server", Version: "1.1.0"}}, t0.Add(day(30))), nil, srv)
	want(t, find(t, rep, "ver.revoked"), LevelPass, "")
	// 清单即将过期。
	rep = mk(mkManifest(nil, t0.Add(day(1))), nil, srv)
	want(t, find(t, rep, "trust.manifest_valid"), LevelWarn, "即将过期")
	// 获取失败：网络问题是告警；验签类失败是 fail。
	rep = mk(nil, errors.New("连接超时"), srv)
	want(t, find(t, rep, "ver.revoked"), LevelSkip, "连接超时")
	want(t, find(t, rep, "trust.manifest_valid"), LevelWarn, "")
	sigErr := &manifest.Error{Kind: manifest.KindSignature, Msg: "清单签名校验失败"}
	rep = mk(nil, sigErr, srv)
	want(t, find(t, rep, "trust.manifest_valid"), LevelFail, "验证失败")
	expErr := &manifest.Error{Kind: manifest.KindExpired, Msg: "清单已于 2026-11-01 过期"}
	rep = mk(nil, expErr, srv)
	want(t, find(t, rep, "trust.manifest_valid"), LevelFail, "过期")
	// 离线：skip，且不调用清单来源。
	w := newWorld(t)
	w.Components = []Component{srv}
	w.Cfg.Offline = true
	called := false
	w.Manifest = func(context.Context) (*manifest.Manifest, error) { called = true; return nil, nil }
	rep = runAll(t, w, Filter{})
	want(t, find(t, rep, "ver.revoked"), LevelSkip, "离线")
	if called {
		t.Error("offline 不得获取清单")
	}
}
