package signing

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

type kp struct {
	id   string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newKP(t *testing.T, id string) kp {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return kp{id: id, pub: pub, priv: priv}
}

func (k kp) key(st KeyStatus) Key {
	return Key{KeyID: k.id, Algorithm: AlgEd25519, PublicKey: k.pub, Status: st}
}

var t0 = time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)

func root(keys ...Key) Keyset { return Keyset{Version: 1, Keys: keys, IssuedAt: t0} }

func TestSignAndVerifyRevision(t *testing.T) {
	k := newKP(t, "cfg-a")
	ks := root(k.key(StatusActive))
	manifest := []byte(`{"seq":7}`)
	sig, err := SignRevision(k.id, k.priv, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if sig.KeyID != k.id || sig.Algorithm != AlgEd25519 || len(sig.Signature) != 64 {
		t.Fatalf("签名元数据: %+v", sig)
	}
	if err := VerifyRevision(ks, manifest, sig); err != nil {
		t.Fatalf("合法签名应通过: %v", err)
	}
	// 签名输入就是 域前缀 ‖ 清单字节（与 node-protocol 一致，独立复算）。
	if !ed25519.Verify(k.pub, append([]byte("LINGEDGE-REVISION-V1\n"), manifest...), sig.Signature) {
		t.Fatal("签名输入不是 \"LINGEDGE-REVISION-V1\\n\" + manifest")
	}
}

// 域分隔：改前缀任意一个字节、改清单一个字节、换 key、换 keyset 里的状态都必须失败。
func TestVerifyRevisionNegative(t *testing.T) {
	k, other := newKP(t, "cfg-a"), newKP(t, "cfg-b")
	manifest := []byte(`{"seq":7,"blocks":[]}`)
	sig, _ := SignRevision(k.id, k.priv, manifest)
	ks := root(k.key(StatusActive))

	// 逐字节改前缀：用改了前缀的输入签名，必须无法通过验证。
	for i := 0; i < len(RevisionDomain); i++ {
		in := append([]byte(RevisionDomain), manifest...)
		in[i] ^= 0x01
		forged := RevisionSignature{KeyID: k.id, Algorithm: AlgEd25519, Signature: ed25519.Sign(k.priv, in)}
		if err := VerifyRevision(ks, manifest, forged); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("前缀第 %d 字节被改仍应失败: %v", i, err)
		}
	}
	// 逐字节改清单。
	for i := range manifest {
		m := bytes.Clone(manifest)
		m[i] ^= 0x01
		if err := VerifyRevision(ks, m, sig); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("清单第 %d 字节被改仍应失败: %v", i, err)
		}
	}
	// 没有前缀的裸签名不被接受。
	bare := RevisionSignature{KeyID: k.id, Algorithm: AlgEd25519, Signature: ed25519.Sign(k.priv, manifest)}
	if err := VerifyRevision(ks, manifest, bare); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("无域前缀的签名应失败: %v", err)
	}
	// keyset 域的签名不能当作修订签名（跨域误用）。
	cross := RevisionSignature{KeyID: k.id, Algorithm: AlgEd25519, Signature: ed25519.Sign(k.priv, append([]byte(KeysetDomain), manifest...))}
	if err := VerifyRevision(ks, manifest, cross); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("keyset 域前缀的签名不得被当作修订签名: %v", err)
	}
	// 另一把 key 的签名、未知 key、已吊销 key、非 ed25519 算法。
	other2, _ := SignRevision(k.id, other.priv, manifest)
	if err := VerifyRevision(ks, manifest, other2); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("别的私钥签名应失败: %v", err)
	}
	unk := sig
	unk.KeyID = "cfg-zzz"
	if err := VerifyRevision(ks, manifest, unk); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("未知 key: %v", err)
	}
	rev := root(k.key(StatusRevoked), other.key(StatusActive))
	if err := VerifyRevision(rev, manifest, sig); !errors.Is(err, ErrKeyRevoked) {
		t.Fatalf("已吊销 key: %v", err)
	}
	ret := root(k.key(StatusRetiring), other.key(StatusActive))
	if err := VerifyRevision(ret, manifest, sig); err != nil {
		t.Fatalf("RETIRING 仍可验证（重叠期）: %v", err)
	}
	alg := sig
	alg.Algorithm = "ecdsa_p256"
	if err := VerifyRevision(ks, manifest, alg); !errors.Is(err, ErrAlgorithm) {
		t.Fatalf("非 ed25519 算法: %v", err)
	}
	// keyset 里登记了 ECDSA（例如 CA 的）公钥：不能用来验证修订。
	ecKS := Keyset{Version: 1, IssuedAt: t0, Keys: []Key{{KeyID: "ca-x", Algorithm: "ecdsa_p256", PublicKey: make([]byte, 91), Status: StatusActive}}}
	if err := VerifyRevision(ecKS, manifest, RevisionSignature{KeyID: "ca-x", Algorithm: AlgEd25519, Signature: make([]byte, 64)}); !errors.Is(err, ErrAlgorithm) {
		t.Fatalf("CA（ECDSA）公钥不得用于验证修订: %v", err)
	}
	if _, err := SignRevision("", k.priv, manifest); !errors.Is(err, ErrBadKey) {
		t.Fatal("缺 key_id 应拒绝")
	}
	if _, err := SignRevision("x", k.priv[:10], manifest); !errors.Is(err, ErrBadKey) {
		t.Fatal("私钥长度非法应拒绝")
	}
}

// 修订签名不含也不依赖时间：同一输入两次签名逐字节相同（Ed25519 确定性），验证函数没有时间参数（R-4）。
func TestRevisionSignatureHasNoTime(t *testing.T) {
	k := newKP(t, "cfg-a")
	a, _ := SignRevision(k.id, k.priv, []byte("m"))
	time.Sleep(5 * time.Millisecond)
	b, _ := SignRevision(k.id, k.priv, []byte("m"))
	if !bytes.Equal(a.Signature, b.Signature) {
		t.Fatal("签名不应随时间变化")
	}
}

func TestCanonicalBodyIsDeterministicAndSorted(t *testing.T) {
	k := newKP(t, "cfg-a")
	ks := root(k.key(StatusActive))
	ks.Keys[0].NotBefore = t0
	b1, err := ks.CanonicalBody()
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := ks.CanonicalBody()
	if !bytes.Equal(b1, b2) {
		t.Fatal("canonical 输出不确定")
	}
	want := `{"issued_at":"2026-11-21T08:00:00Z","keys":[{"algorithm":"ed25519","key_id":"cfg-a","not_before":"2026-11-21T08:00:00Z","public_key":"` +
		stdB64(k.pub) + `","status":"ACTIVE"}],"previous_version":0,"version":1}`
	if string(b1) != want {
		t.Fatalf("canonical 字节:\n got %s\nwant %s", b1, want)
	}
	// 不含 signed_by_key_id 与 signature；改签署者/签名不改变被签名字节。
	ks.SignedByKeyID, ks.Signature = "x", []byte("y")
	if b3, _ := ks.CanonicalBody(); !bytes.Equal(b1, b3) {
		t.Fatal("被签名字节不应包含签名字段")
	}
}

func TestKeysetChain(t *testing.T) {
	a, b, c := newKP(t, "cfg-a"), newKP(t, "cfg-b"), newKP(t, "cfg-c")
	v1 := root(a.key(StatusActive))
	if err := VerifyRoot(v1); err != nil {
		t.Fatalf("信任起点: %v", err)
	}
	// v2：新 key 先以 ACTIVE 加入、旧 key 降为 RETIRING，由 v1 的 ACTIVE key 签名。
	v2, err := SignKeyset(v1, Keyset{Version: 2, PreviousVersion: 1, IssuedAt: t0.Add(time.Hour), Keys: []Key{a.key(StatusRetiring), b.key(StatusActive)}}, a.id, a.priv)
	if err != nil {
		t.Fatal(err)
	}
	// v3：旧 key 吊销，由 v2 里仍有效的 b（也可由 RETIRING 的 a）签名。
	v3, err := SignKeyset(v2, Keyset{Version: 3, PreviousVersion: 2, IssuedAt: t0.Add(2 * time.Hour), Keys: []Key{a.key(StatusRevoked), b.key(StatusActive), c.key(StatusActive)}}, b.id, b.priv)
	if err != nil {
		t.Fatal(err)
	}
	tip, err := VerifyChain(v1, []Keyset{v2, v3})
	if err != nil || tip.Version != 3 {
		t.Fatalf("合法链应通过: %v tip=%d", err, tip.Version)
	}
	if got, err := VerifyChain(v3, nil); err != nil || got.Version != 3 {
		t.Fatalf("空链返回已信任版本: %v", err)
	}
	// 重叠期：v2 之下，旧 key（RETIRING）与新 key 签发的修订都可验证；v3 之后旧 key 的被拒。
	m := []byte("{}")
	sa, _ := SignRevision(a.id, a.priv, m)
	sb, _ := SignRevision(b.id, b.priv, m)
	if VerifyRevision(v2, m, sa) != nil || VerifyRevision(v2, m, sb) != nil {
		t.Fatal("重叠期新旧 key 签发的修订均应可验证")
	}
	if err := VerifyRevision(v3, m, sa); !errors.Is(err, ErrKeyRevoked) {
		t.Fatalf("吊销后旧 key 应被拒: %v", err)
	}

	t.Run("防回滚", func(t *testing.T) {
		if _, err := VerifyChain(v3, []Keyset{v2}); !errors.Is(err, ErrKeysetRollback) {
			t.Fatalf("以旧版本替换新版本应判回滚: %v", err)
		}
		if _, err := VerifyChain(v2, []Keyset{v2}); !errors.Is(err, ErrKeysetRollback) {
			t.Fatalf("重放同版本应判回滚: %v", err)
		}
	})
	t.Run("断链", func(t *testing.T) {
		if _, err := VerifyChain(v1, []Keyset{v3}); !errors.Is(err, ErrKeysetBroken) {
			t.Fatalf("跳过 v2 应判断链: %v", err)
		}
		bad := v2
		bad.PreviousVersion = 0
		if _, err := VerifyChain(v1, []Keyset{bad}); err == nil {
			t.Fatal("previous_version 不符应失败")
		}
		bad = v2
		bad.PreviousVersion = 5
		if _, err := VerifyChain(v1, []Keyset{bad}); err == nil {
			t.Fatal("previous_version 不符应失败")
		}
	})
	t.Run("篡改", func(t *testing.T) {
		// 改一个公钥字节 / 改状态 / 改 issued_at / 改签名一个字节。
		mut := func(f func(k *Keyset)) Keyset {
			x := v2
			x.Keys = append([]Key(nil), v2.Keys...)
			for i := range x.Keys {
				x.Keys[i].PublicKey = bytes.Clone(x.Keys[i].PublicKey)
			}
			x.Signature = bytes.Clone(v2.Signature)
			f(&x)
			return x
		}
		cases := map[string]Keyset{
			"公钥":   mut(func(k *Keyset) { k.Keys[1].PublicKey[0] ^= 1 }),
			"状态":   mut(func(k *Keyset) { k.Keys[0].Status = StatusActive }),
			"签发时间": mut(func(k *Keyset) { k.IssuedAt = k.IssuedAt.Add(time.Second) }),
			"签名":   mut(func(k *Keyset) { k.Signature[0] ^= 1 }),
			"追加 key": mut(func(k *Keyset) {
				ev := newKP(t, "evil")
				k.Keys = append(k.Keys, ev.key(StatusActive))
			}),
		}
		for name, c := range cases {
			if _, err := VerifyChain(v1, []Keyset{c}); !errors.Is(err, ErrKeysetSignature) {
				t.Errorf("篡改%s应判签名失败: %v", name, err)
			}
		}
		// 攻击者用自己的 key 签名（不在上一版中）。
		ev := newKP(t, "evil")
		forged, _ := Keyset{Version: 2, PreviousVersion: 1, IssuedAt: t0, Keys: []Key{ev.key(StatusActive)}, SignedByKeyID: ev.id}, 0
		in, _ := forged.keysetInput()
		forged.Signature = ed25519.Sign(ev.priv, in)
		if _, err := VerifyChain(v1, []Keyset{forged}); !errors.Is(err, ErrKeysetSigner) {
			t.Errorf("签署者不在上一版应拒绝: %v", err)
		}
		// 用上一版里的 key 的 id 但别的私钥签名。
		forged.SignedByKeyID = a.id
		in, _ = forged.keysetInput()
		forged.Signature = ed25519.Sign(ev.priv, in)
		if _, err := VerifyChain(v1, []Keyset{forged}); !errors.Is(err, ErrKeysetSignature) {
			t.Errorf("冒用 key_id 应签名失败: %v", err)
		}
	})
	t.Run("吊销的 key 不能签署", func(t *testing.T) {
		if _, err := SignKeyset(v3, Keyset{Version: 4, PreviousVersion: 3, IssuedAt: t0, Keys: []Key{b.key(StatusActive)}}, a.id, a.priv); !errors.Is(err, ErrKeysetSigner) {
			t.Fatalf("REVOKED 的 key 不得签署新 keyset: %v", err)
		}
	})
	t.Run("改写已有 key 与复活", func(t *testing.T) {
		evil := newKP(t, "cfg-b") // 同 key_id、不同公钥
		if _, err := SignKeyset(v2, Keyset{Version: 3, PreviousVersion: 2, IssuedAt: t0, Keys: []Key{evil.key(StatusActive), a.key(StatusRetiring)}}, b.id, b.priv); !errors.Is(err, ErrKeyTampered) {
			t.Fatalf("同 key_id 换公钥应拒绝: %v", err)
		}
		if _, err := SignKeyset(v3, Keyset{Version: 4, PreviousVersion: 3, IssuedAt: t0, Keys: []Key{a.key(StatusActive), b.key(StatusActive)}}, b.id, b.priv); !errors.Is(err, ErrKeyResurrected) {
			t.Fatalf("REVOKED 不得复活: %v", err)
		}
	})
	t.Run("结构非法", func(t *testing.T) {
		for name, ks := range map[string]Keyset{
			"无 ACTIVE":  {Version: 1, IssuedAt: t0, Keys: []Key{a.key(StatusRetiring)}},
			"重复 key_id": {Version: 1, IssuedAt: t0, Keys: []Key{a.key(StatusActive), a.key(StatusRetiring)}},
			"非法 key_id": {Version: 1, IssuedAt: t0, Keys: []Key{{KeyID: "a\"b", Algorithm: AlgEd25519, PublicKey: a.pub, Status: StatusActive}}},
			"短公钥":       {Version: 1, IssuedAt: t0, Keys: []Key{{KeyID: "x", Algorithm: AlgEd25519, PublicKey: a.pub[:5], Status: StatusActive}}},
			"空 keys":    {Version: 1, IssuedAt: t0},
			"version 0": {Version: 0, IssuedAt: t0, Keys: []Key{a.key(StatusActive)}},
		} {
			if err := VerifyRoot(ks); err == nil {
				t.Errorf("%s 应被拒绝", name)
			}
		}
		if err := VerifyRoot(v2); !errors.Is(err, ErrNotRoot) {
			t.Errorf("带签名的 keyset 不是信任起点: %v", err)
		}
	})
}

func TestCheckCAExpiry(t *testing.T) {
	now := t0
	for _, c := range []struct {
		name  string
		after time.Time
		want  CAExpiryLevel
	}{
		{"10 年", now.AddDate(10, 0, 0), CAExpiryOK},
		{"恰 1 年", now.Add(CAExpiryWarnWindow), CAExpiryOK},
		{"不足 1 年", now.Add(CAExpiryWarnWindow - time.Second), CAExpiryWarn},
		{"30 天", now.AddDate(0, 0, 30), CAExpiryWarn},
		{"已过期", now.Add(-time.Hour), CAExpiryExpired},
		{"恰过期", now, CAExpiryExpired},
	} {
		if got := CheckCAExpiry(c.after, now); got.Level != c.want {
			t.Errorf("%s: %s want %s", c.name, got.Level, c.want)
		}
	}
	if st := CheckCAExpiry(now.AddDate(0, 6, 0), now); st.Remaining <= 0 || !st.NotAfter.Equal(now.AddDate(0, 6, 0)) {
		t.Errorf("剩余时间: %+v", st)
	}
}

func TestFingerprint(t *testing.T) {
	// SHA-256("") 的已知值，核对实现就是小写十六进制 SHA-256。
	if got := Fingerprint(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("指纹: %s", got)
	}
	if CertFingerprint([]byte("a")) != Fingerprint([]byte("a")) {
		t.Fatal("证书指纹口径一致")
	}
}

func stdB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
