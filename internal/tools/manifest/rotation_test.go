package manifest

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

type rotFixture struct {
	e       *env
	newKey  tkey
	keysDir string
}

func newRot(t *testing.T) *rotFixture {
	return &rotFixture{e: newEnv(t), newKey: deriveKey("rel-test-b", "b"), keysDir: filepath.Join(t.TempDir(), "trust", "keys.d")}
}

func (f *rotFixture) endorse(by tkey, nk Key) map[string]any {
	sig := ed25519.Sign(by.priv, KeyRotationInput(nk))
	return map[string]any{"key_id": nk.KeyID, "endorsed_by": by.id, "sig": base64.StdEncoding.EncodeToString(sig)}
}

func (f *rotFixture) manifest(t *testing.T, keys []Key, ends []any) []byte {
	return mf(t, func(m map[string]any) {
		ks := []any{}
		for _, k := range keys {
			ks = append(ks, map[string]any{"key_id": k.KeyID, "alg": k.Alg, "public_key": k.PublicKey, "not_before": k.NotBefore, "not_after": k.NotAfter, "status": k.Status})
		}
		m["key_rotation"] = map[string]any{"keys": ks, "endorsements": ends}
	})
}

func TestKeyRotationEndorsedByOldActiveKey(t *testing.T) {
	f := newRot(t)
	nk := f.newKey.entry(StatusActive, "2026-11-01T00:00:00Z", "2029-01-01T00:00:00Z")
	raw := f.manifest(t, []Key{nk}, []any{f.endorse(f.e.active, nk)})
	res, err := f.e.verify(raw, signManifest(f.e.active, raw), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rotation) != 1 || res.Rotation[0].KeyID != "rel-test-b" {
		t.Fatalf("应得到待落盘的新公钥: %+v", res.Rotation)
	}
	// 验证后才落盘：Verify 不写盘。
	if _, err := os.Stat(f.keysDir); err == nil {
		t.Fatal("Verify 不得落盘")
	}
	if _, err := f.e.trust.Lookup("rel-test-b"); err == nil {
		t.Fatal("Commit 之前新公钥不得被信任")
	}
	if err := f.e.v.Commit(res, f.keysDir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(f.keysDir, "rel-test-b.json")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("落盘文件权限 = %o", fi.Mode().Perm())
	}
	if di, _ := os.Stat(f.keysDir); di.Mode().Perm() != 0o700 {
		t.Fatalf("keys.d 目录权限 = %o", di.Mode().Perm())
	}
	// 重叠期：新旧并存。新公钥此后可验证清单，旧公钥仍可。
	raw2 := mf(t, func(m map[string]any) { m["sequence"] = 11 })
	if _, err := f.e.verify(raw2, signManifest(f.newKey, raw2), VerifyOptions{}); err != nil {
		t.Fatalf("新公钥应可验证: %v", err)
	}
	if _, err := f.e.verify(raw2, signManifest(f.e.active, raw2), VerifyOptions{}); err != nil {
		t.Fatalf("旧公钥在重叠期应仍可验证: %v", err)
	}
	// 重启后从内置 + keys.d 重新构建信任集仍包含新公钥。
	bf := keysDoc(t, []string{"rel-test-x"}, f.e.active.entry(StatusActive, "2026-01-01T00:00:00Z", "2028-01-01T00:00:00Z"))
	ts, err := LoadTrust(bf, f.keysDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Lookup("rel-test-b"); err != nil {
		t.Fatalf("落盘后重新加载应信任新公钥: %v", err)
	}
	// 幂等：同一清单重放（sequence 相等）不报错，不重复。
	res2, err := f.e.verify(raw, signManifest(f.e.active, raw), VerifyOptions{})
	if err != nil || len(res2.Rotation) != 0 {
		t.Fatalf("已落盘的公钥重放应幂等（无待落盘项）: %v %+v", err, res2)
	}
}

func TestKeyRotationRejections(t *testing.T) {
	f := newRot(t)
	nk := f.newKey.entry(StatusActive, "", "")
	retired := deriveKey("rel-test-old", "old")
	stranger := deriveKey("rel-test-unknown", "stranger")
	revokedKey := deriveKey("rel-test-x", "x")
	good := f.endorse(f.e.active, nk)
	check := func(name string, keys []Key, ends []any, kind Kind) {
		t.Helper()
		raw := f.manifest(t, keys, ends)
		_, err := f.e.verify(raw, signManifest(f.e.active, raw), VerifyOptions{})
		if err == nil {
			t.Fatalf("%s：应拒绝", name)
		}
		wantKind(t, err, kind)
	}
	check("无背书", []Key{nk}, []any{}, KindRotation)
	check("背书者是 retired key", []Key{nk}, []any{f.endorse(retired, nk)}, KindRotation)
	check("背书者未知", []Key{nk}, []any{f.endorse(stranger, nk)}, KindRotation)
	check("背书者已吊销", []Key{nk}, []any{f.endorse(revokedKey, nk)}, KindRotation)
	check("新 key 自己背书自己", []Key{nk}, []any{f.endorse(f.newKey, nk)}, KindRotation)
	// 背书的是另一把公钥（背书不能被移花接木）。
	other := deriveKey("rel-test-b", "evil").entry(StatusActive, "", "")
	check("背书对象被替换", []Key{other}, []any{good}, KindRotation)
	// 背书后改了有效期/状态（背书覆盖全部语义字段）。
	widened := nk
	widened.NotAfter = "2099-01-01T00:00:00Z"
	check("背书后放宽有效期", []Key{widened}, []any{good}, KindRotation)
	// 新公钥不是 active。
	ret := f.newKey.entry(StatusRetired, "", "")
	check("新 key 状态 retired", []Key{ret}, []any{f.endorse(f.e.active, ret)}, KindRotation)
	// 多背书 / 孤立背书 / 空 keys。
	check("重复背书", []Key{nk}, []any{good, good}, KindRotation)
	check("孤立背书", []Key{nk}, []any{good, f.endorse(f.e.active, deriveKey("rel-test-c", "c").entry(StatusActive, "", ""))}, KindRotation)
	check("空 keys", []Key{}, []any{}, KindRotation)
	// 新 key_id 与内置公钥同名但公钥不同（替换攻击）。
	replace := deriveKey("rel-test-a", "evil").entry(StatusActive, "", "")
	check("同名替换", []Key{replace}, []any{f.endorse(f.e.active, replace)}, KindRotation)
	// 新 key_id 已在吊销集。
	rv := revokedKey.entry(StatusActive, "", "")
	check("复活已吊销 key_id", []Key{rv}, []any{f.endorse(f.e.active, rv)}, KindRotation)
	// 背书签名损坏。
	bad := f.endorse(f.e.active, nk)
	bad["sig"] = base64.StdEncoding.EncodeToString(make([]byte, 64))
	check("背书签名损坏", []Key{nk}, []any{bad}, KindRotation)
	// 新公钥格式非法。
	broken := nk
	broken.PublicKey = "AAAA"
	check("公钥格式", []Key{broken}, []any{good}, KindRotation)
}

// 轮换段在「清单签名」之内：攻击者无法在不破坏清单签名的前提下注入新公钥；用未被信任的 key 签的清单带轮换段也无效。
func TestKeyRotationCannotBeInjectedWithoutManifestSignature(t *testing.T) {
	f := newRot(t)
	nk := f.newKey.entry(StatusActive, "", "")
	raw := f.manifest(t, []Key{nk}, []any{f.endorse(f.e.active, nk)})
	sig := signManifest(f.e.active, raw)
	plain := mf(t, nil) // 同一签名套在没有轮换段的清单上 → 签名不符
	_, err := f.e.verify(plain, sig, VerifyOptions{})
	wantKind(t, err, KindSignature)
	// 新 key 用自己的签名 + 自己的背书（不被信任的签名者）。
	_, err = f.e.verify(raw, signManifest(f.newKey, raw), VerifyOptions{})
	wantKind(t, err, KindUnknownKey)
}
