package manifest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"
)

type goldenVectors struct {
	SeedPhrase   string `json:"seed_phrase"`
	SeedHex      string `json:"seed_hex"`
	PublicKeyB64 string `json:"public_key_b64"`
	PublicKeyHex string `json:"public_key_hex"`
	KeyID        string `json:"key_id"`
	Manifest     struct {
		FileText     string `json:"file_text"`
		SHA256       string `json:"sha256"`
		DomainPrefix string `json:"domain_prefix"`
		SigB64       string `json:"sig_b64"`
	} `json:"manifest"`
	Package struct {
		Component, Version, Platform string
		PayloadPath                  string `json:"payload_path"`
		PayloadText                  string `json:"payload_text"`
		SumsText                     string `json:"sha256sums_text"`
		SigInputPrefix               string `json:"sig_input_prefix"`
		SigB64                       string `json:"sig_b64"`
	} `json:"package"`
}

func loadGolden(t *testing.T) goldenVectors {
	t.Helper()
	b, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldenVectors
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func goldenVerifier(t *testing.T, g goldenVectors, now time.Time) (*Verifier, memSeq) {
	t.Helper()
	pub, _ := base64.StdEncoding.DecodeString(g.PublicKeyB64)
	k := Key{KeyID: g.KeyID, Alg: "ed25519", PublicKey: base64.StdEncoding.EncodeToString(pub), Status: StatusActive}
	bf, err := ParseKeysFile(keysDoc(t, nil, k))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := NewTrustSet(bf)
	if err != nil {
		t.Fatal(err)
	}
	seen := memSeq{}
	return &Verifier{Trust: ts, Seen: seen, Now: func() time.Time { return now }}, seen
}

// §9.6 的真实示例向量：种子/公钥/哈希自洽，Go crypto/ed25519 复算出文档里的确定性签名（Ed25519 签名是确定性的）。
func TestGoldenVectorsSelfConsistent(t *testing.T) {
	g := loadGolden(t)
	sum := sha256.Sum256([]byte(g.SeedPhrase))
	if hex.EncodeToString(sum[:]) != g.SeedHex {
		t.Fatal("seed_hex 不是 seed_phrase 的 SHA-256")
	}
	priv := ed25519.NewKeyFromSeed(sum[:])
	pub := priv.Public().(ed25519.PublicKey)
	if base64.StdEncoding.EncodeToString(pub) != g.PublicKeyB64 || hex.EncodeToString(pub) != g.PublicKeyHex {
		t.Fatal("公钥与种子不一致")
	}
	mh := sha256.Sum256([]byte(g.Manifest.FileText))
	if hex.EncodeToString(mh[:]) != g.Manifest.SHA256 {
		t.Fatal("manifest.json 的 SHA-256 与文档不符")
	}
	if g.Manifest.DomainPrefix != ManifestDomain || g.Manifest.FileText[len(g.Manifest.FileText)-1] != '\n' {
		t.Fatal("域前缀/文件末尾换行与文档不符")
	}
	sig := ed25519.Sign(priv, []byte(ManifestDomain+g.Manifest.FileText))
	if base64.StdEncoding.EncodeToString(sig) != g.Manifest.SigB64 {
		t.Fatal("清单签名复算结果与文档不符")
	}
	psum := sha256.Sum256([]byte(g.Package.PayloadText))
	if hex.EncodeToString(psum[:])+"  "+g.Package.PayloadPath+"\n" != g.Package.SumsText {
		t.Fatal("SHA256SUMS 与 payload 不符")
	}
	in, err := PackageSigInput(g.Package.Component, g.Package.Version, g.Package.Platform, []byte(g.Package.SumsText))
	if err != nil {
		t.Fatal(err)
	}
	if string(in[:len(g.Package.SigInputPrefix)]) != g.Package.SigInputPrefix {
		t.Fatal("包签名输入前缀与文档不符")
	}
	psig := ed25519.Sign(priv, in)
	if base64.StdEncoding.EncodeToString(psig) != g.Package.SigB64 {
		t.Fatal("包签名复算结果与文档不符")
	}
}

// 生产验签路径对 golden 向量通过；清单是 §9.6 的示例，valid_until=2026-04-01，故取 now=2026-02-01。
func TestGoldenManifestVerifiesViaVerifier(t *testing.T) {
	g := loadGolden(t)
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	v, _ := goldenVerifier(t, g, now)
	sigRaw, _ := base64.StdEncoding.DecodeString(g.Manifest.SigB64)
	sigFile := sigDoc(g.KeyID, sigRaw)
	res, err := v.Verify([]byte(g.Manifest.FileText), sigFile, VerifyOptions{Channel: "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Sequence != 1 || res.Manifest.Channel != "stable" || len(res.Manifest.Components) != 0 {
		t.Fatalf("解析结果异常: %+v", res.Manifest)
	}
	// 过期（2026-04-01 之后）默认拒绝；golden 清单是 §9.6 的过期测试素材。
	v2, _ := goldenVerifier(t, g, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	_, err = v2.Verify([]byte(g.Manifest.FileText), sigFile, VerifyOptions{Channel: "stable"})
	wantKind(t, err, KindExpired)
	// 任一输入字节翻转 → 失败（清单 + 签名各翻每一位）。
	raw := []byte(g.Manifest.FileText)
	for i := range raw {
		bad := append([]byte(nil), raw...)
		bad[i] ^= 0x80
		if _, err := v.Verify(bad, sigFile, VerifyOptions{Channel: "stable"}); err == nil {
			t.Fatalf("清单第 %d 字节翻转后仍通过", i)
		}
	}
	for i := range sigRaw {
		bad := append([]byte(nil), sigRaw...)
		bad[i] ^= 1
		if _, err := v.Verify(raw, sigDoc(g.KeyID, bad), VerifyOptions{Channel: "stable"}); err == nil {
			t.Fatalf("签名第 %d 字节翻转后仍通过", i)
		}
	}
	// 清单签名不能被当作包签名复用（域分隔）：用清单签名充当 SHA256SUMS 签名必须失败。
	_, err = v.VerifyPackageSums(sigFile, []byte(g.Package.SumsText), g.Package.Component, g.Package.Version, g.Package.Platform, now)
	wantKind(t, err, KindSignature)
	// key_id 不在公钥集 / 在吊销集 → 失败；sequence 小于已见 → 失败（§9.6 测试断言）。
	_, err = v.Verify(raw, sigDoc("someone-else", sigRaw), VerifyOptions{Channel: "stable"})
	wantKind(t, err, KindUnknownKey)
	v3, seen := goldenVerifier(t, g, now)
	seen["stable"] = 2
	_, err = v3.Verify(raw, sigFile, VerifyOptions{Channel: "stable"})
	wantKind(t, err, KindRollback)
	v3.Trust.revoked[g.KeyID] = struct{}{}
	_, err = v3.Verify(raw, sigFile, VerifyOptions{Channel: "stable"})
	wantKind(t, err, KindKeyRevoked)
}

func TestGoldenPackageSignature(t *testing.T) {
	g := loadGolden(t)
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	v, _ := goldenVerifier(t, g, now)
	sigRaw, _ := base64.StdEncoding.DecodeString(g.Package.SigB64)
	sigFile := sigDoc(g.KeyID, sigRaw)
	sums := []byte(g.Package.SumsText)
	id, err := v.VerifyPackageSums(sigFile, sums, "node", "0.1.0", "linux-amd64", now)
	if err != nil || id != g.KeyID {
		t.Fatalf("golden 包签名应通过: %v %q", err, id)
	}
	// SHA256SUMS 任一字节翻转、签名翻转、component/version/platform 任一改动 → 失败。
	for i := range sums {
		bad := append([]byte(nil), sums...)
		bad[i] ^= 1
		if _, err := v.VerifyPackageSums(sigFile, bad, "node", "0.1.0", "linux-amd64", now); err == nil {
			t.Fatalf("SHA256SUMS 第 %d 字节翻转后仍通过", i)
		}
	}
	for name, args := range map[string][3]string{
		"component": {"server", "0.1.0", "linux-amd64"},
		"version":   {"node", "0.1.1", "linux-amd64"},
		"platform":  {"node", "0.1.0", "linux-arm64"},
		"any":       {"node", "0.1.0", "any"},
	} {
		_, err := v.VerifyPackageSums(sigFile, sums, args[0], args[1], args[2], now)
		if err == nil {
			t.Fatalf("%s 改动后包签名仍通过（签名未绑定该字段）", name)
		}
		wantKind(t, err, KindSignature)
	}
	// 注入：component 含换行会让签名输入有歧义，必须在构造输入前被拒绝。
	_, err = PackageSigInput("node\n0.1.0", "0.1.0", "linux-amd64", sums)
	wantKind(t, err, KindPackage)
	_, err = PackageSigInput("node", "0.1.0\nlinux-amd64", "linux-amd64", sums)
	wantKind(t, err, KindSchema)
}
