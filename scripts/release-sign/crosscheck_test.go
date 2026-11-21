package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// §9.6 示例向量（tools-design §9.6）的权威数据由 Story 1.27 落盘在 internal/tools/manifest/testdata/vectors.json：
// 本测试读取同一份文件，用 release-sign 自己的（与 internal/tools/manifest 互为独立的）签名输入实现复算，
// 必须逐字节得到文档里的确定性签名——两个独立实现（签名侧 / Tools 验签侧）对同一向量一致。
const vectorsPath = "../../internal/tools/manifest/testdata/vectors.json"

type vectors struct {
	SeedPhrase   string `json:"seed_phrase"`
	PublicKeyB64 string `json:"public_key_b64"`
	KeyID        string `json:"key_id"`
	Manifest     struct {
		FileText string `json:"file_text"`
		SigB64   string `json:"sig_b64"`
	} `json:"manifest"`
	Package struct {
		Component string `json:"component"`
		Version   string `json:"version"`
		Platform  string `json:"platform"`
		SumsText  string `json:"sha256sums_text"`
		SigB64    string `json:"sig_b64"`
	} `json:"package"`
}

func loadVectors(t *testing.T) (vectors, *signer) {
	t.Helper()
	b, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	seed := sha256.Sum256([]byte(v.SeedPhrase))
	s := &signer{keyID: v.KeyID, priv: ed25519.NewKeyFromSeed(seed[:])}
	if base64.StdEncoding.EncodeToString(s.public()) != v.PublicKeyB64 {
		t.Fatal("向量公钥与种子不一致")
	}
	return v, s
}

func sigOf(t *testing.T, s *signer, input []byte) string {
	t.Helper()
	var d sigDoc
	if err := json.Unmarshal(s.sigFile(input), &d); err != nil {
		t.Fatal(err)
	}
	return d.Sig
}

func TestGolden96ReproducedBySigner(t *testing.T) {
	v, s := loadVectors(t)
	if got := sigOf(t, s, manifestSigInput([]byte(v.Manifest.FileText))); got != v.Manifest.SigB64 {
		t.Fatalf("清单签名与 §9.6 不符:\n got %s\nwant %s", got, v.Manifest.SigB64)
	}
	in, err := packageSigInput(v.Package.Component, v.Package.Version, v.Package.Platform, []byte(v.Package.SumsText))
	if err != nil {
		t.Fatal(err)
	}
	if got := sigOf(t, s, in); got != v.Package.SigB64 {
		t.Fatalf("包签名与 §9.6 不符:\n got %s\nwant %s", got, v.Package.SigB64)
	}
}

// --- Ruby OpenSSL 交叉验证 -----------------------------------------------------------------------------------

// findRuby 返回第一个「Ruby OpenSSL 支持 Ed25519」的 ruby；候选：$RUBY、PATH 上的 ruby、Homebrew。
func findRuby(t *testing.T) string {
	t.Helper()
	cands := []string{os.Getenv("RUBY")}
	if p, err := exec.LookPath("ruby"); err == nil {
		cands = append(cands, p)
	}
	cands = append(cands, "/opt/homebrew/bin/ruby", "/usr/local/bin/ruby", "/usr/bin/ruby")
	seen := map[string]bool{}
	for _, c := range cands {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if _, err := os.Stat(c); err != nil {
			continue
		}
		if _, err := rubyCall(c, map[string]any{"op": "probe"}); err == nil {
			return c
		}
	}
	if os.Getenv("LINGEDGE_REQUIRE_RUBY") == "1" {
		t.Fatal("LINGEDGE_REQUIRE_RUBY=1 但本机没有支持 Ed25519 的 Ruby OpenSSL（需要 OpenSSL>=1.1.1，如 Homebrew ruby）")
	}
	t.Skip("等待环境验证：本机没有支持 Ed25519 的 Ruby OpenSSL（macOS 系统 Ruby 2.6/LibreSSL 3.3 不支持）；跳过不代表通过。可设置 RUBY=/path/to/ruby")
	return ""
}

func rubyCall(ruby string, req map[string]any) (map[string]json.RawMessage, error) {
	in, _ := json.Marshal(req)
	cmd := exec.Command(ruby, "crosscheck/ed25519.rb")
	cmd.Stdin = bytes.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, &rubyErr{err, errb.String()}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		return nil, err
	}
	return m, nil
}

type rubyErr struct {
	err error
	msg string
}

func (e *rubyErr) Error() string { return e.err.Error() + ": " + e.msg }

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func rubyVerify(t *testing.T, ruby string, pub []byte, cases map[string][2][]byte) map[string]bool {
	t.Helper()
	var cs []map[string]string
	for name, c := range cases {
		cs = append(cs, map[string]string{"name": name, "input_b64": b64(c[0]), "sig_b64": b64(c[1])})
	}
	out, err := rubyCall(ruby, map[string]any{"op": "verify", "pub_b64": b64(pub), "cases": cs})
	if err != nil {
		t.Fatal(err)
	}
	res := map[string]bool{}
	for k, raw := range out {
		var ok bool
		if err := json.Unmarshal(raw, &ok); err != nil {
			t.Fatal(err)
		}
		res[k] = ok
	}
	return res
}

func TestGolden96VerifiedByRubyOpenSSL(t *testing.T) {
	ruby := findRuby(t)
	v, s := loadVectors(t)
	pub := s.public()
	msig, _ := base64.StdEncoding.DecodeString(v.Manifest.SigB64)
	psig, _ := base64.StdEncoding.DecodeString(v.Package.SigB64)
	minput := manifestSigInput([]byte(v.Manifest.FileText))
	pinput, _ := packageSigInput(v.Package.Component, v.Package.Version, v.Package.Platform, []byte(v.Package.SumsText))

	flip := func(b []byte, i int) []byte { c := append([]byte(nil), b...); c[i] ^= 1; return c }
	cases := map[string][2][]byte{
		"manifest":            {minput, msig},
		"package":             {pinput, psig},
		"manifest-tamper-in":  {flip(minput, len(minput)-3), msig},
		"manifest-tamper-sig": {minput, flip(msig, 5)},
		"package-tamper-in":   {flip(pinput, len(pinput)-3), psig},
		"package-tamper-sig":  {pinput, flip(psig, 5)},
		"domain-confusion":    {pinput, msig}, // 清单签名不能当包签名
		"no-domain":           {[]byte(v.Manifest.FileText), msig},
	}
	got := rubyVerify(t, ruby, pub, cases)
	for name, ok := range got {
		want := name == "manifest" || name == "package"
		if ok != want {
			t.Errorf("Ruby OpenSSL 对 %s 验签结果 %v，期望 %v", name, ok, want)
		}
	}
	// Go 侧对同一批输入的结论必须与 Ruby 一致（两个独立实现的判定对账）。
	for name, c := range cases {
		if goOK := ed25519.Verify(pub, c[0], c[1]); goOK != got[name] {
			t.Errorf("%s: Go=%v Ruby=%v 两实现不一致", name, goOK, got[name])
		}
	}
}

// 反向：Ruby 用同一种子签名，确定性签名必须与 Go/§9.6 完全相同；再用运行时随机密钥做 Go 签 → Ruby 验、Ruby 签 → Go 验。
func TestRubyOpenSSLSignsSameAsGo(t *testing.T) {
	ruby := findRuby(t)
	v, s := loadVectors(t)
	minput := manifestSigInput([]byte(v.Manifest.FileText))
	out, err := rubyCall(ruby, map[string]any{"op": "sign", "seed_b64": b64(s.priv.Seed()), "cases": []map[string]string{{"name": "m", "input_b64": b64(minput)}}})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	_ = json.Unmarshal(out["m"], &got)
	if got != v.Manifest.SigB64 {
		t.Fatalf("Ruby 对 §9.6 清单的确定性签名与文档不符:\n got %s\nwant %s", got, v.Manifest.SigB64)
	}

	// 运行时随机密钥（密钥不落盘、不入库）。
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	rs := &signer{keyID: "rel-runtime", priv: ed25519.NewKeyFromSeed(seed)}
	msgs := map[string][]byte{"a": []byte("LINGEDGE-MANIFEST-V1\n{}\n"), "b": bytes.Repeat([]byte{0xff, 0}, 4096)}
	var cs []map[string]string
	goSigs := map[string][2][]byte{}
	for n, m := range msgs {
		cs = append(cs, map[string]string{"name": n, "input_b64": b64(m)})
		var d sigDoc
		_ = json.Unmarshal(rs.sigFile(m), &d)
		raw, _ := base64.StdEncoding.DecodeString(d.Sig)
		goSigs[n] = [2][]byte{m, raw}
	}
	for n, ok := range rubyVerify(t, ruby, rs.public(), goSigs) {
		if !ok {
			t.Errorf("Go 签名 %s 未通过 Ruby 验签", n)
		}
	}
	out, err = rubyCall(ruby, map[string]any{"op": "sign", "seed_b64": b64(seed), "cases": cs})
	if err != nil {
		t.Fatal(err)
	}
	for n, m := range msgs {
		var sb string
		_ = json.Unmarshal(out[n], &sb)
		raw, _ := base64.StdEncoding.DecodeString(sb)
		if !ed25519.Verify(rs.public(), m, raw) {
			t.Errorf("Ruby 签名 %s 未通过 Go 验签", n)
		}
	}
}

func TestCrosscheckScriptFilesExist(t *testing.T) {
	if _, err := os.Stat(filepath.Join("crosscheck", "ed25519.rb")); err != nil {
		t.Fatal(err)
	}
}
