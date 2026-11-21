package downloader_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/cli"
	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

// 校验链（§9.5 步 5–7 与步 1–4 的接线）：同一张用例表分别经「在线 HTTPS」与「离线 bundle」运行，两条路径必须给出同样的失败；
// 每个失败都断言：退出码 5（或表中指明的码）、staging 目录树前后一致、信任库（sequence/keys.d）未被改动。

type chainCase struct {
	name  string
	sc    func(t *testing.T, w *world) scenario
	pre   func(t *testing.T, w *world)
	check func(t *testing.T, err error)
	exit  cli.ExitCode
}

func dKind(k downloader.Kind) func(*testing.T, error) {
	return func(t *testing.T, err error) { wantKind(t, err, k) }
}
func mKind(k manifest.Kind) func(*testing.T, error) {
	return func(t *testing.T, err error) { wantManifestKind(t, err, k) }
}
func extractIsAny(sentinels ...error) func(*testing.T, error) {
	return func(t *testing.T, err error) {
		wantKind(t, err, downloader.KindExtract)
		for _, s := range sentinels {
			if errors.Is(err, s) {
				return
			}
		}
		t.Fatalf("期望底层错误之一 %v，实际 %v", sentinels, err)
	}
}

func extractIs(sentinel error) func(*testing.T, error) {
	return func(t *testing.T, err error) {
		wantKind(t, err, downloader.KindExtract)
		if !errors.Is(err, sentinel) {
			t.Fatalf("期望底层错误 %v，实际 %v", sentinel, err)
		}
	}
}

func goodPkg(t *testing.T, w *world, mut func(*pkgSpec)) []byte {
	t.Helper()
	p := pkgSpec{signer: w.key}
	if mut != nil {
		mut(&p)
	}
	return p.build(t)
}

func chainCases() []chainCase {
	stranger := deriveKey("rel-dl-x", "x")
	sc := func(mut func(*pkgSpec)) func(*testing.T, *world) scenario {
		return func(t *testing.T, w *world) scenario { return scenario{pkg: goodPkg(t, w, mut)} }
	}
	meta := func(f func(m map[string]any)) func(*testing.T, *world) scenario {
		return sc(func(p *pkgSpec) { p.meta = f })
	}
	tamp := func(f func([]ent) []ent) func(*testing.T, *world) scenario {
		return sc(func(p *pkgSpec) { p.tamper = f })
	}
	dropEnt := func(name string) func([]ent) []ent {
		return func(es []ent) []ent {
			var out []ent
			for _, e := range es {
				if e.name != name {
					out = append(out, e)
				}
			}
			return out
		}
	}
	return []chainCase{
		// ---- 外层（步骤 5）
		{name: "包体被篡改（等长翻转一字节）", exit: 5, check: dKind(downloader.KindSHA256), sc: func(t *testing.T, w *world) scenario {
			g := goodPkg(t, w, nil)
			return scenario{pkg: flip(g, len(g)/2), manifestPkg: g}
		}},
		{name: "包体被截断（比清单 size 短）", exit: 5, check: dKind(downloader.KindSize), sc: func(t *testing.T, w *world) scenario {
			g := goodPkg(t, w, nil)
			return scenario{pkg: g[:len(g)-10], manifestPkg: g}
		}},
		{name: "包体被追加（比清单 size 长）", exit: 5, check: dKind(downloader.KindSize), sc: func(t *testing.T, w *world) scenario {
			g := goodPkg(t, w, nil)
			return scenario{pkg: append(append([]byte(nil), g...), "tail"...), manifestPkg: g}
		}},
		{name: "包体不是 gzip（SHA256 与清单一致）", exit: 5, check: dKind(downloader.KindExtract), sc: func(t *testing.T, w *world) scenario {
			return scenario{pkg: []byte("this is not a tar.gz at all, but its hash matches the manifest")}
		}},
		// ---- 清单（步骤 2–4）
		{name: "清单被篡改", exit: 5, check: mKind(manifest.KindSignature), sc: func(t *testing.T, w *world) scenario {
			return scenario{pkg: goodPkg(t, w, nil), tamperMf: func(b []byte) []byte { return flip(b, 40) }}
		}},
		{name: "清单签名被篡改", exit: 5, check: mKind(manifest.KindSignature), sc: func(t *testing.T, w *world) scenario {
			return scenario{pkg: goodPkg(t, w, nil), tamperSig: func(b []byte) []byte { return bytes.Replace(b, []byte(`"sig":"`), []byte(`"sig":"A`), 1) }}
		}},
		{name: "清单由不在可信集的密钥签名", exit: 5, check: mKind(manifest.KindUnknownKey), sc: func(t *testing.T, w *world) scenario {
			return scenario{pkg: goodPkg(t, w, nil), signer: &stranger}
		}},
		{name: "清单已过期", exit: 5, check: mKind(manifest.KindExpired), sc: func(t *testing.T, w *world) scenario {
			return scenario{pkg: goodPkg(t, w, nil), validUntil: "2026-11-02T00:00:00Z"}
		}},
		{name: "sequence 回滚", exit: 5, check: mKind(manifest.KindRollback), sc: func(t *testing.T, w *world) scenario {
			return scenario{pkg: goodPkg(t, w, nil), seq: 5}
		}, pre: func(t *testing.T, w *world) {
			// 预置「已见 sequence=50」的真实 trust/state（经生产的 FileSequenceStore）。
			if err := (manifest.FileSequenceStore{Path: filepath.Join(w.trustDir, "state")}).Advance("stable", 50); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "通道不符", exit: 5, check: mKind(manifest.KindChannel), sc: func(t *testing.T, w *world) scenario {
			return scenario{pkg: goodPkg(t, w, nil), channel: "beta"}
		}},
		{name: "目标版本在 revoked_versions", exit: 5, check: mKind(manifest.KindVersionRevoked), sc: func(t *testing.T, w *world) scenario {
			return scenario{pkg: goodPkg(t, w, nil), revoked: []any{map[string]any{"component": comp, "version": ver, "reason": "测试吊销"}}}
		}},
		{name: "组件 unreleased", exit: cli.ExitNotReleased, check: dKind(downloader.KindUnreleased), sc: func(t *testing.T, w *world) scenario {
			return scenario{pkg: goodPkg(t, w, nil), status: "unreleased"}
		}},
		{name: "组件 builtin", exit: cli.ExitBuiltin, check: dKind(downloader.KindBuiltin), sc: func(t *testing.T, w *world) scenario {
			return scenario{pkg: goodPkg(t, w, nil), status: "builtin"}
		}},
		// ---- 包内签名（步骤 6）
		{name: "SHA256SUMS 被篡改（签名不再匹配）", exit: 5, check: mKind(manifest.KindSignature), sc: sc(func(p *pkgSpec) {
			p.sumsInTar = []byte("0000000000000000000000000000000000000000000000000000000000000000  payload/README\n")
		})},
		{name: "SHA256SUMS.sig 被篡改", exit: 5, check: mKind(manifest.KindSignature), sc: sc(func(p *pkgSpec) {
			p.sigInTar = sigDoc("rel-dl-a", bytes.Repeat([]byte{7}, 64))
		})},
		{name: "SHA256SUMS.sig 由不在可信集的密钥签名", exit: 5, check: mKind(manifest.KindUnknownKey), sc: sc(func(p *pkgSpec) { p.signer = stranger })},
		{name: "包内签名重放：绑定了别的 component", exit: 5, check: mKind(manifest.KindSignature), sc: sc(func(p *pkgSpec) { p.sigComp = "server" })},
		{name: "包内签名重放：绑定了别的 version", exit: 5, check: mKind(manifest.KindSignature), sc: sc(func(p *pkgSpec) { p.sigVer = "0.2.0" })},
		{name: "包内签名重放：绑定了别的 platform", exit: 5, check: mKind(manifest.KindSignature), sc: sc(func(p *pkgSpec) { p.sigPlat = "linux-arm64" })},
		{name: "SHA256SUMS 含 payload/ 之外的路径", exit: 5, check: dKind(downloader.KindExtract), sc: sc(func(p *pkgSpec) {
			p.sumsExtra = "1111111111111111111111111111111111111111111111111111111111111111  META/package.json\n"
		})},
		{name: "META 缺失（无 SHA256SUMS.sig）", exit: 5, check: extractIs(security.ErrMissing), sc: tamp(dropEnt("META/SHA256SUMS.sig"))},
		{name: "META 在 payload 之后", exit: 5, check: extractIs(security.ErrMissing), sc: tamp(func(es []ent) []ent { return append(es[3:], es[:3]...) })},
		// ---- 解压逐文件校验（步骤 6）
		{name: "包内文件被篡改（内容与已签名 SHA256SUMS 不符）", exit: 5, check: extractIs(security.ErrChecksum), sc: tamp(func(es []ent) []ent {
			for i := range es {
				if es[i].name == "payload/README" {
					es[i].body = []byte("HELLO\n")
				}
			}
			return es
		})},
		{name: "包内多出未声明的文件", exit: 5, check: extractIsAny(security.ErrExtra, security.ErrLimit), sc: tamp(func(es []ent) []ent { return append(es, ent{name: "payload/evil.sh", body: []byte("x")}) })},
		{name: "包内缺少声明的文件", exit: 5, check: extractIs(security.ErrMissing), sc: tamp(dropEnt("payload/README"))},
		{name: "包内符号链接", exit: 5, check: extractIs(security.ErrUnsupportedType), sc: tamp(func(es []ent) []ent {
			return append(es, ent{name: "payload/ln", typ: 50 /* '2' */, link: "/etc/passwd"})
		})},
		{name: "包内硬链接", exit: 5, check: extractIs(security.ErrUnsupportedType), sc: tamp(func(es []ent) []ent {
			return append(es, ent{name: "payload/hl", typ: '1', link: "payload/README"})
		})},
		{name: "包内路径穿越", exit: 5, check: extractIs(security.ErrUnsafePath), sc: tamp(func(es []ent) []ent {
			es[3].name = "payload/../../evil"
			return es
		})},
		{name: "包内重复条目", exit: 5, check: extractIsAny(security.ErrUnsafePath, security.ErrLimit), sc: tamp(func(es []ent) []ent { return append(es, es[len(es)-1]) })},
		// ---- 炸弹包
		{name: "炸弹包：tar 结束后追加超量零字节", exit: 5, check: extractIs(security.ErrLimit), sc: sc(func(p *pkgSpec) { p.trailingPad = 8 << 20 })},
		{name: "炸弹包：实际内容超过 META 声明的 unpacked_size", exit: 5, check: extractIs(security.ErrLimit), sc: meta(func(m map[string]any) { m["unpacked_size"] = 3 })},
		// ---- META/package.json 与清单一致（步骤 7）
		{name: "package.json component 与清单不符", exit: 5, check: mKind(manifest.KindPackage), sc: meta(func(m map[string]any) { m["component"] = "server" })},
		{name: "package.json version 与清单不符", exit: 5, check: mKind(manifest.KindPackage), sc: meta(func(m map[string]any) { m["version"] = "0.1.1" })},
		{name: "package.json variant 与清单不符", exit: 5, check: mKind(manifest.KindPackage), sc: meta(func(m map[string]any) { m["variant"] = "static" })},
		{name: "package.json platform 与清单不符", exit: 5, check: mKind(manifest.KindPackage), sc: meta(func(m map[string]any) { m["platform"] = "linux-arm64" })},
		{name: "package.json files_count 与已签名 SHA256SUMS 不符", exit: 5, check: mKind(manifest.KindPackage), sc: meta(func(m map[string]any) { m["files_count"] = 99 })},
		{name: "package.json unpacked_size 大于实际", exit: 5, check: mKind(manifest.KindPackage), sc: meta(func(m map[string]any) { m["unpacked_size"] = 10000 })},
		{name: "package.json 缺必需字段", exit: 5, check: mKind(manifest.KindPackage), sc: meta(func(m map[string]any) { delete(m, "built_at") })},
	}
}

func TestChainFailuresOnlineAndOffline(t *testing.T) {
	for _, mode := range []string{"online", "offline"} {
		for _, c := range chainCases() {
			t.Run(mode+"/"+c.name, func(t *testing.T) {
				w := newWorld(t)
				if c.pre != nil {
					c.pre(t, w)
				}
				s := c.sc(t, w)
				stagingBefore, trustBefore := tree(t, w.staging), tree(t, w.trustDir)
				var err error
				if mode == "online" {
					_, err = w.runOnline(s, downloader.Config{})
				} else {
					_, err = w.runOffline(s, false)
				}
				if err == nil {
					t.Fatal("期望失败，实际成功")
				}
				c.check(t, err)
				if got := exitOf(err); got != c.exit {
					t.Errorf("退出码 = %d，期望 %d（%v）", got, c.exit, err)
				}
				equalTree(t, "staging", stagingBefore, tree(t, w.staging))
				equalTree(t, "trust", trustBefore, tree(t, w.trustDir))
			})
		}
	}
}

// 清单里没有所请求的版本/变体/平台/组件：退出码 2（用法），同样清理 staging。
func TestChainNoSuchTarget(t *testing.T) {
	for name, edit := range map[string]func(*downloader.Params){
		"版本":   func(p *downloader.Params) { p.Version = "9.9.9" },
		"变体":   func(p *downloader.Params) { p.Variant = "nope" },
		"平台":   func(p *downloader.Params) { p.Platform = "linux-arm64" },
		"组件":   func(p *downloader.Params) { p.Component = "dns" },
		"版本非法": func(p *downloader.Params) { p.Version = "not-a-version" },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			s := scenario{pkg: goodPkg(t, w, nil)}
			w.publish(s)
			p := w.params()
			edit(&p)
			p.BundlePath = w.writeBundle(s)
			before := tree(t, w.staging)
			_, err := downloader.Acquire(t.Context(), p)
			if err == nil {
				t.Fatal("期望失败")
			}
			if got := exitOf(err); got != cli.ExitUsage {
				t.Errorf("退出码 = %d，期望 2（%v）", got, err)
			}
			equalTree(t, "staging", before, tree(t, w.staging))
		})
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 成功路径：在线与离线得到完全相同的解压结果；sequence 在整条链成功后才推进；staging 只留 <tx>/pkg。
func TestChainSuccessOnlineEqualsOffline(t *testing.T) {
	results := map[string]map[string]string{}
	for _, mode := range []string{"online", "offline"} {
		t.Run(mode, func(t *testing.T) {
			w := newWorld(t)
			s := scenario{pkg: goodPkg(t, w, nil), seq: 17}
			if st := readState(w); st != "" {
				t.Fatalf("初始不应有 trust/state：%s", st)
			}
			var a *downloader.Acquired
			var err error
			p := w.params()
			p.TxID = "tx-1"
			p.Executable = []string{"payload/bin/lingedge-node"}
			if mode == "online" {
				w.publish(s)
				p.Online = &downloader.OnlineSource{Client: w.client(downloader.Config{}), ManifestURL: w.base + "/manifest.json"}
			} else {
				p.BundlePath = w.writeBundle(s)
			}
			a, err = downloader.Acquire(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			if a.Version != ver || a.Platform != platform || a.Meta.Component != comp || a.Result.Manifest.Sequence != 17 {
				t.Fatalf("结果字段不符：%+v", a)
			}
			if want := filepath.Join(w.staging, "tx-1", "pkg"); a.PkgDir != want {
				t.Fatalf("PkgDir = %s，期望 %s", a.PkgDir, want)
			}
			if b := readFile(t, filepath.Join(a.PkgDir, "payload", "README")); string(b) != "hello\n" {
				t.Fatalf("payload 内容不符：%q", b)
			}
			// 权限：只有 META 声明的可执行文件得到 0755，其余 0644，不论包内 mode（包内一律写 0755）。
			for rel, want := range map[string]os.FileMode{"payload/bin/lingedge-node": 0o755, "payload/README": 0o644, "META/SHA256SUMS": 0o644} {
				fi, err := os.Stat(filepath.Join(a.PkgDir, filepath.FromSlash(rel)))
				if err != nil || fi.Mode().Perm() != want {
					t.Fatalf("%s 权限 = %v，期望 %v（%v）", rel, fi.Mode().Perm(), want, err)
				}
			}
			// staging：只有 tx-1/pkg，中间文件（下载的 tar.gz、.part）已清理。
			got := tree(t, w.staging)
			pkgPrefix := filepath.Join("tx-1", "pkg")
			for k := range got {
				if k != "." && k != "tx-1" && k != pkgPrefix && !strings.HasPrefix(k, pkgPrefix+string(filepath.Separator)) {
					t.Errorf("staging 中存在不应保留的文件 %s", k)
				}
			}
			// Commit：sequence 已推进到 17，状态文件 0600。
			if st := readState(w); !bytes.Contains([]byte(st), []byte(`"stable": 17`)) {
				t.Fatalf("sequence 未推进：%s", st)
			}
			pk := tree(t, a.PkgDir)
			results[mode] = pk
		})
	}
	if on, off := results["online"], results["offline"]; on != nil && off != nil {
		equalTree(t, "在线与离线的解压结果", on, off)
	}
}

// 同一份清单重放（sequence 相等）允许：幂等重取；更低的 sequence 拒绝（已在表中）。
func TestChainReplaySameSequenceIdempotent(t *testing.T) {
	w := newWorld(t)
	s := scenario{pkg: goodPkg(t, w, nil), seq: 20}
	for i := 0; i < 2; i++ {
		p := w.params()
		p.BundlePath = w.writeBundle(s)
		p.TxID = "tx-" + string(rune('a'+i))
		if _, err := downloader.Acquire(t.Context(), p); err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
	}
}

// --allow-expired-manifest 只与 --offline 一起：离线放行过期清单并告警，在线（Params.AllowExpired 不带 bundle）是用法错误。
func TestAllowExpiredOnlyOffline(t *testing.T) {
	w := newWorld(t)
	s := scenario{pkg: goodPkg(t, w, nil), validUntil: "2026-11-02T00:00:00Z"}
	a, err := w.runOffline(s, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Warnings) == 0 {
		t.Fatal("放行过期清单必须带告警")
	}
	// 在线 + AllowExpired
	w2 := newWorld(t)
	w2.publish(s)
	p := w2.params()
	p.Online = &downloader.OnlineSource{Client: w2.client(downloader.Config{}), ManifestURL: w2.base + "/manifest.json"}
	p.AllowExpired = true
	before := tree(t, w2.staging)
	_, err = downloader.Acquire(t.Context(), p)
	wantKind(t, err, downloader.KindOptions)
	if exitOf(err) != cli.ExitUsage {
		t.Fatalf("退出码 = %d", exitOf(err))
	}
	equalTree(t, "staging", before, tree(t, w2.staging))
	if w2.hitCount("/manifest.json") != 0 {
		t.Fatal("参数非法时不应发起任何请求")
	}
	// 放行过期不放行回滚：离线 + AllowExpired + sequence 回退仍失败。
	w3 := newWorld(t)
	_ = (manifest.FileSequenceStore{Path: filepath.Join(w3.trustDir, "state")}).Advance("stable", 50)
	_, err = w3.runOffline(scenario{pkg: goodPkg(t, w3, nil), validUntil: "2026-11-02T00:00:00Z", seq: 5}, true)
	wantManifestKind(t, err, manifest.KindRollback)
}

// key_rotation：落盘发生在整条链成功之后；链在包阶段失败时，keys.d 不被写入。
func TestRotationCommittedOnlyAfterFullChain(t *testing.T) {
	newKey := deriveKey("rel-dl-b", "b")
	rot := func(m map[string]any) {
		nk := manifest.Key{KeyID: newKey.id, Alg: "ed25519", PublicKey: b64(newKey.pub), Status: manifest.StatusActive, NotBefore: "2026-01-01T00:00:00Z", NotAfter: "2028-01-01T00:00:00Z"}
		m["key_rotation"] = map[string]any{
			"keys":         []any{map[string]any{"key_id": nk.KeyID, "alg": nk.Alg, "public_key": nk.PublicKey, "status": nk.Status, "not_before": nk.NotBefore, "not_after": nk.NotAfter}},
			"endorsements": []any{map[string]any{"key_id": nk.KeyID, "endorsed_by": "rel-dl-a", "sig": b64(signRotation(t, deriveKey("rel-dl-a", "a"), nk))}},
		}
	}
	// 包阶段失败（sums 被篡改）：keys.d 与 state 均不出现。
	w := newWorld(t)
	bad := goodPkg(t, w, func(p *pkgSpec) { p.sumsInTar = []byte("x") })
	before := tree(t, w.trustDir)
	if _, err := w.runOffline(scenario{pkg: bad, mfEdit: rot}, false); err == nil {
		t.Fatal("期望失败")
	}
	equalTree(t, "trust", before, tree(t, w.trustDir))
	if _, err := os.Stat(filepath.Join(w.keysDir, "rel-dl-b.json")); err == nil {
		t.Fatal("链失败时不得落盘轮换公钥")
	}
	// 成功：落盘。
	a, err := w.runOffline(scenario{pkg: goodPkg(t, w, nil), mfEdit: rot}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Result.Rotation) != 1 {
		t.Fatalf("Rotation = %v", a.Result.Rotation)
	}
	fi, err := os.Stat(filepath.Join(w.keysDir, "rel-dl-b.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("轮换公钥未按 0600 落盘：%v %v", fi, err)
	}
}
