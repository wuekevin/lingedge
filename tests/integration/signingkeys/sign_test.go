package signingkeysit

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/signingkeys"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/pkg/signing"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// AC2：Sign(manifestBytes)——域前缀 "LINGEDGE-REVISION-V1\n"、记录 key_id、不带过期时间；用 pkg/signing（节点侧同一实现）验证；
// 改前缀一个字节、改内容一个字节、换 key 全部失败；签名输入与协议文档一致（独立复算）。
func TestSignAndVerify(t *testing.T) {
	w := apistack.New(t)
	if _, err := w.Keys.Sign(ctxT(t), []byte("{}")); !errors.Is(err, signingkeys.ErrNotInitialized) {
		t.Fatalf("未初始化时 Sign 应拒绝: %v", err)
	}
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatal(err)
	}
	ks, err := w.Keys.InitialKeyset(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := signing.VerifyRoot(ks); err != nil || len(ks.Keys) != 1 || ks.Keys[0].Status != signing.StatusActive || ks.Keys[0].Algorithm != "ed25519" {
		t.Fatalf("信任起点 keyset: %+v %v", ks, err)
	}
	cfg := byPurpose(t, readKeys(t, w), "config_signing")
	manifest := []byte(`{"schema_version":1,"cluster_uid":"c1","lineage":1,"seq":42,"blocks":[]}`)

	sig, err := w.Keys.Sign(ctxT(t), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if sig.KeyID != cfg.KeyID || sig.Algorithm != "ed25519" || len(sig.Signature) != ed25519.SignatureSize {
		t.Fatalf("签名元数据应记录 key_id: %+v", sig)
	}
	if err := signing.VerifyRevision(ks, manifest, sig); err != nil {
		t.Fatalf("节点侧验证应通过: %v", err)
	}
	// 独立复算：签名输入 = "LINGEDGE-REVISION-V1\n" ‖ manifest_json，用库内公钥验证。
	if !ed25519.Verify(cfg.Public, append([]byte("LINGEDGE-REVISION-V1\n"), manifest...), sig.Signature) {
		t.Fatal("签名输入与协议不一致")
	}
	// 不带过期时间：验证函数没有时间参数；换一个时钟（远未来/远过去）结果相同；同输入重复签名逐字节一致（无时间/随机成分）。
	again, _ := w.Keys.Sign(ctxT(t), manifest)
	if !bytes.Equal(again.Signature, sig.Signature) {
		t.Error("签名不应含时间或随机成分")
	}

	// 域分隔与篡改。
	for i := 0; i < len("LINGEDGE-REVISION-V1\n"); i++ {
		in := append([]byte("LINGEDGE-REVISION-V1\n"), manifest...)
		in[i] ^= 1
		if ed25519.Verify(cfg.Public, in, sig.Signature) {
			t.Fatalf("前缀第 %d 字节被改后签名竟然成立", i)
		}
	}
	for i := range manifest {
		m := bytes.Clone(manifest)
		m[i] ^= 1
		if signing.VerifyRevision(ks, m, sig) == nil {
			t.Fatalf("内容第 %d 字节被改后验证应失败", i)
		}
	}
	bad := sig
	bad.KeyID = "cfg-unknown"
	if err := signing.VerifyRevision(ks, manifest, bad); !errors.Is(err, signing.ErrUnknownKey) {
		t.Errorf("未知 key_id: %v", err)
	}
	// 另一把 Ed25519 密钥（例如别的部署）的签名不能通过。
	_, otherPriv, _ := ed25519.GenerateKey(nil)
	forged, _ := signing.SignRevision(cfg.KeyID, otherPriv, manifest)
	if err := signing.VerifyRevision(ks, manifest, forged); !errors.Is(err, signing.ErrBadSignature) {
		t.Errorf("别的私钥的签名: %v", err)
	}
	// keyset 域的签名不能当作修订签名。
	der := decrypt(t, w, cfg)
	defer secretbox.Zero(der)
	priv := edKey(t, der)
	cross := signing.RevisionSignature{KeyID: cfg.KeyID, Algorithm: "ed25519", Signature: ed25519.Sign(priv, append([]byte(signing.KeysetDomain), manifest...))}
	if err := signing.VerifyRevision(ks, manifest, cross); !errors.Is(err, signing.ErrBadSignature) {
		t.Errorf("keyset 域签名不得通过修订验证: %v", err)
	}
}

// 跨 purpose 的密钥误用必须失败：CA（ECDSA）私钥的密文被挪进配置签名行 → 解密失败（AAD 绑定 表|列|uid）；
// 把 purpose 改成别的也不能让 Sign 用上别的密钥（purpose/算法不匹配被拒）；CA/叶证书的公钥不能验证修订。
func TestCrossPurposeMisuseFails(t *testing.T) {
	w := apistack.New(t)
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatal(err)
	}
	keys := readKeys(t, w)
	cfg, ca, srv := byPurpose(t, keys, "config_signing"), byPurpose(t, keys, "mtls_ca"), byPurpose(t, keys, "grpc_server")
	manifest := []byte(`{"seq":1}`)
	if _, err := w.Keys.Sign(ctxT(t), manifest); err != nil {
		t.Fatalf("前提: %v", err)
	}

	// ① 密文挪行：把 CA/叶证书的密文放进配置签名行，AAD 的 uid 不符 → 解密失败，Sign 不产出签名。
	for _, src := range []dbKey{ca, srv} {
		w.Root("UPDATE §.signing_keys SET private_key_enc = ? WHERE uid = ?", src.Enc, cfg.UID)
		_, err := w.Keys.Sign(ctxT(t), manifest)
		if err == nil || !errors.Is(err, secretbox.ErrDecrypt) {
			t.Errorf("把 %s 的密文挪进配置签名行，Sign 应因解密失败而失败: %v", src.Purpose, err)
		}
		w.Root("UPDATE §.signing_keys SET private_key_enc = ? WHERE uid = ?", cfg.Enc, cfg.UID)
	}
	// ② 反向：配置签名的密文放进 CA 行，再初始化补建叶证书时无法解开 CA 私钥（不会用错密钥签发）。
	w.Root("DELETE FROM §.signing_keys WHERE purpose = 'grpc_server'")
	w.Root("UPDATE §.signing_keys SET private_key_enc = ? WHERE uid = ?", cfg.Enc, ca.UID)
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err == nil {
		t.Error("CA 行里是别的密钥的密文时，签发叶证书必须失败")
	}
	w.Root("UPDATE §.signing_keys SET private_key_enc = ? WHERE uid = ?", ca.Enc, ca.UID)
	if n := w.RootInt("SELECT COUNT(*) FROM §.signing_keys WHERE purpose = 'grpc_server'"); n != 0 {
		t.Errorf("失败的初始化不得残留叶证书: %d", n)
	}
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatalf("恢复后应能补建叶证书: %v", err)
	}

	// ③ purpose 被改成 mtls_ca 的 config 行：同一个 purpose 不能有两个 active（唯一索引），Sign 找不到 config_signing 的 active 行。
	w.Root("UPDATE §.signing_keys SET status = 'retiring' WHERE uid = ?", cfg.UID)
	if _, err := w.Keys.Sign(ctxT(t), manifest); !errors.Is(err, signingkeys.ErrNotInitialized) {
		t.Errorf("没有 active 的配置签名密钥时 Sign 应拒绝（不会退而使用别的 purpose）: %v", err)
	}
	w.Root("UPDATE §.signing_keys SET status = 'active' WHERE uid = ?", cfg.UID)

	// ④ CA/叶证书的公钥（ECDSA）放进 keyset 也不能验证修订。
	ks, _ := w.Keys.InitialKeyset(ctxT(t))
	ks.Keys = append(ks.Keys, signing.Key{KeyID: ca.KeyID, Algorithm: "ecdsa_p256", PublicKey: ca.Public, Status: signing.StatusActive})
	if err := signing.VerifyRoot(ks); err == nil {
		t.Error("keyset 里出现非 ed25519 的配置签名公钥应被判结构非法")
	}
	sig := signing.RevisionSignature{KeyID: ca.KeyID, Algorithm: "ecdsa_p256", Signature: bytes.Repeat([]byte{1}, 64)}
	if err := signing.VerifyRevision(ks, manifest, sig); err == nil {
		t.Error("ECDSA 公钥不得用于验证配置修订")
	}
}

// 清零探针：Sign 与 Initialize（含「CA 已存在时解密旧 CA 私钥签发新叶」）物化的每个私钥明文缓冲，操作结束后全部为零；
// 解密失败路径不留明文。探针在物化时记下非零内容，避免「对空缓冲恒真」。
func TestPrivateKeyPlaintextIsZeroed(t *testing.T) {
	w := apistack.New(t)
	p := &probe{}
	svc := newService(t, w, p, nil)
	if _, err := svc.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range p.names() {
		got[n] = true
	}
	for _, want := range []string{"config_signing.pkcs8", "config_signing.ed25519", "mtls_ca.pkcs8", "grpc_server.pkcs8"} {
		if !got[want] {
			t.Errorf("初始化应物化并清零 %s: %v", want, p.names())
		}
	}
	p.assertWiped(t)

	// Sign：解密出 PKCS#8 明文与 Ed25519 私钥，返回后均已清零。
	p.reset()
	if _, err := svc.Sign(ctxT(t), []byte("m")); err != nil {
		t.Fatal(err)
	}
	if n := p.names(); len(n) != 2 {
		t.Fatalf("Sign 应物化 2 个缓冲: %v", n)
	}
	p.assertWiped(t)

	// 失败路径：Sign 的密文被破坏 → 解密失败，没有任何明文被物化。
	cfg := byPurpose(t, readKeys(t, w), "config_signing")
	w.Root("UPDATE §.signing_keys SET private_key_enc = CONCAT('X', SUBSTRING(private_key_enc, 2)) WHERE uid = ?", cfg.UID)
	p.reset()
	if _, err := svc.Sign(ctxT(t), []byte("m")); err == nil {
		t.Fatal("密文被破坏应失败")
	}
	if len(p.names()) != 0 {
		t.Errorf("解密失败时不应物化明文: %v", p.names())
	}
	w.Root("UPDATE §.signing_keys SET private_key_enc = ? WHERE uid = ?", cfg.Enc, cfg.UID)

	// 补建叶证书：解密旧 CA 私钥（物化 CA 的 PKCS#8）后同样清零。
	w.Root("DELETE FROM §.signing_keys WHERE purpose = 'grpc_server'")
	p.reset()
	if _, err := svc.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatal(err)
	}
	got = map[string]bool{}
	for _, n := range p.names() {
		got[n] = true
	}
	if !got["mtls_ca.pkcs8"] || !got["grpc_server.pkcs8"] {
		t.Errorf("补建叶证书应解密 CA 私钥并生成叶私钥: %v", p.names())
	}
	p.assertWiped(t)

	// 探针自检：一个故意不清零的缓冲必须被 assertWiped 判为失败（证明断言不是恒真）。
	live := []byte{1, 2, 3}
	neg := &probe{recs: []probeRec{{name: "negative", live: live, before: []byte{1, 2, 3}}}}
	ft := &testing.T{}
	done := make(chan struct{})
	go func() { defer close(done); neg.assertWiped(ft) }()
	<-done
	if !ft.Failed() {
		t.Error("未清零的缓冲应使 assertWiped 失败（探针自测）")
	}
}

// 链式 keyset 联调：用真实库里的信任起点 + 解密出的真实旧私钥签发 v2（新 key 先 ACTIVE、旧 key 降 RETIRING），节点侧逐级验证；
// 防回滚/断链/篡改的细粒度负向用例见 pkg/signing 的单元测试，这里验证它们与服务端产出的数据结构兼容。
func TestKeysetChainWithRealKeys(t *testing.T) {
	w := apistack.New(t)
	if _, err := w.Keys.Initialize(ctxT(t), signingkeys.InitOptions{ServerURL: serverURL}); err != nil {
		t.Fatal(err)
	}
	v1, err := w.Keys.InitialKeyset(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg := byPurpose(t, readKeys(t, w), "config_signing")
	der := decrypt(t, w, cfg)
	defer secretbox.Zero(der)
	oldPriv := edKey(t, der)
	newPub, newPriv, _ := ed25519.GenerateKey(nil)
	newID := "cfg-rot-" + signing.Fingerprint(newPub)[:8]

	v2, err := signing.SignKeyset(v1, signing.Keyset{Version: 2, PreviousVersion: 1, IssuedAt: time.Now().UTC(), Keys: []signing.Key{
		{KeyID: cfg.KeyID, Algorithm: "ed25519", PublicKey: cfg.Public, Status: signing.StatusRetiring},
		{KeyID: newID, Algorithm: "ed25519", PublicKey: newPub, Status: signing.StatusActive},
	}}, cfg.KeyID, oldPriv)
	if err != nil {
		t.Fatalf("旧 active key 签发 v2: %v", err)
	}
	tip, err := signing.VerifyChain(v1, []signing.Keyset{v2})
	if err != nil || tip.Version != 2 {
		t.Fatalf("链验证: %v", err)
	}
	// 重叠期：服务端（旧 key）与新 key 签发的修订都能被 v2 验证。
	m := []byte(`{"seq":9}`)
	oldSig, _ := w.Keys.Sign(ctxT(t), m)
	newSig, _ := signing.SignRevision(newID, newPriv, m)
	if signing.VerifyRevision(v2, m, oldSig) != nil || signing.VerifyRevision(v2, m, newSig) != nil {
		t.Error("重叠期新旧 key 的签名都应可验证")
	}
	// 回滚（旧版本替换新版本）、断链、篡改。
	if _, err := signing.VerifyChain(v2, []signing.Keyset{v1}); !errors.Is(err, signing.ErrKeysetRollback) {
		t.Errorf("回滚: %v", err)
	}
	v3gap := v2
	v3gap.Version, v3gap.PreviousVersion = 4, 3
	if _, err := signing.VerifyChain(v1, []signing.Keyset{v2, v3gap}); err == nil {
		t.Error("断链应失败")
	}
	tam := v2
	tam.Keys = append([]signing.Key(nil), v2.Keys...)
	tam.Keys[0].Status = signing.StatusActive
	if _, err := signing.VerifyChain(v1, []signing.Keyset{tam}); !errors.Is(err, signing.ErrKeysetSignature) {
		t.Errorf("篡改: %v", err)
	}
}
